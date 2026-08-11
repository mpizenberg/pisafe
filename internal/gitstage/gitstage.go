// Package gitstage implements the host-side Git boundary for a pisafe run.
//
// It deliberately knows nothing about Lima, SSH, or containers. A caller can
// stream the bundle into a mountless VM and invoke the same staging operations
// there without granting the VM access to the source checkout.
package gitstage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/mpizenberg/pisafe/internal/runid"
)

const (
	baselineMessage = "pisafe: imported working-tree baseline"
	finalMessage    = "pisafe: captured final working-tree state"
)

var (
	ErrBranchExists    = errors.New("pisafe import branch already exists")
	ErrLFSNotSupported = errors.New("Git LFS repositories are not supported yet")
)

// Snapshot describes one staged run. Inputs is what was carried in, with the
// content each file had at that moment; IncludeRoots is what the user named,
// which is what the run re-expands to find the work it hands back. A run staged
// before included paths round-tripped has inputs but no roots, and so hands
// nothing back.
type Snapshot struct {
	RunID          string           `json:"run_id"`
	SourceRoot     string           `json:"source_root"`
	SourceHead     string           `json:"source_head"`
	WorkRef        string           `json:"work_ref"`
	BaselineCommit string           `json:"baseline_commit,omitempty"`
	Inputs         []SelectedInput  `json:"inputs,omitempty"`
	IncludeRoots   []string         `json:"include_roots,omitempty"`
	Submodules     []SubmoduleStage `json:"submodules,omitempty"`
	CreatedAt      time.Time        `json:"created_at"`
}

type ApplyResult struct {
	Branch      string             `json:"branch"`
	Tip         string             `json:"tip"`
	FinalCommit string             `json:"final_commit,omitempty"`
	Untracked   []string           `json:"untracked,omitempty"`
	Submodules  []AppliedSubmodule `json:"submodules,omitempty"`
	// Included reports what the run's work under the paths the user chose did
	// to the working tree, which is the one part of an apply that writes files.
	Included IncludedResult `json:"included,omitempty"`
}

// AppliedSubmodule reports which commit the imported superproject branch
// expects in one submodule, and the branch that keeps it reachable. Branch is
// empty when the submodule did not change.
type AppliedSubmodule struct {
	Path   string `json:"path"`
	Branch string `json:"branch,omitempty"`
	Tip    string `json:"tip"`
}

type PreparedStage struct {
	Snapshot   Snapshot
	BundlePath string
	PatchPath  string
	InputsPath string
	Submodules []PreparedSubmodule
}

const (
	sourceBundleName = "source.bundle"
	trackedPatchName = "tracked.patch"
	// StageSnapshotName holds the one artifact a stage package does not
	// describe, because it is what describes the rest: the receiving side reads
	// it and locates everything else from what it says.
	StageSnapshotName = "snapshot.json"
)

// stageSubmoduleArtifact bounds the names a submodule contributes to a stage
// package, so an artifact name can never become a path.
var stageSubmoduleArtifact = regexp.MustCompile(`^submodule-[0-9]{1,4}\.(bundle|patch)$`)

// StageArtifact is one file of a stage package: the name both sides address it
// by, and the path it has on whichever side currently holds it.
type StageArtifact struct {
	Name string
	Path string
}

// Artifacts lists the files a prepared stage consists of, other than the
// snapshot that names them.
func (prepared PreparedStage) Artifacts() []StageArtifact {
	artifacts := []StageArtifact{
		{Name: sourceBundleName, Path: prepared.BundlePath},
		{Name: trackedPatchName, Path: prepared.PatchPath},
	}
	if prepared.InputsPath != "" {
		artifacts = append(artifacts, StageArtifact{
			Name: inputsArchiveName,
			Path: prepared.InputsPath,
		})
	}
	for index, submodule := range prepared.Submodules {
		artifacts = append(
			artifacts,
			StageArtifact{Name: submoduleBundleName(index), Path: submodule.BundlePath},
			StageArtifact{Name: submodulePatchName(index), Path: submodule.PatchPath},
		)
	}
	return artifacts
}

// StagePackage locates the artifacts of a stage package in one directory. It is
// what the receiving side has in place of the record the sending side built,
// and it is the layout Prepare writes, so neither side spells the other's file
// names.
func StagePackage(directory string, snapshot Snapshot) PreparedStage {
	prepared := PreparedStage{
		Snapshot:   snapshot,
		BundlePath: filepath.Join(directory, sourceBundleName),
		PatchPath:  filepath.Join(directory, trackedPatchName),
	}
	if len(snapshot.Inputs) != 0 {
		prepared.InputsPath = filepath.Join(directory, inputsArchiveName)
	}
	for index, submodule := range snapshot.Submodules {
		prepared.Submodules = append(prepared.Submodules, PreparedSubmodule{
			Path:       submodule.Path,
			BundlePath: filepath.Join(directory, submoduleBundleName(index)),
			PatchPath:  filepath.Join(directory, submodulePatchName(index)),
		})
	}
	return prepared
}

// ValidStageArtifactName reports whether a name is one a stage package holds.
// Whoever writes into the package asks here, because the names are formed here.
func ValidStageArtifactName(name string) bool {
	switch name {
	case sourceBundleName, trackedPatchName, StageSnapshotName, inputsArchiveName:
		return true
	}
	return stageSubmoduleArtifact.MatchString(name)
}

// PrepareRequest describes one staging operation. Selected inputs are the only
// source content that is copied rather than derived from Git history, and they
// arrive already resolved: what a run reports having taken and what it actually
// stages are then one list, decided once.
type PrepareRequest struct {
	SourcePath string
	PackageDir string
	RunID      string
	Inputs     InputPlan
}

// ExcludedInputs is everything in the repository a run does not receive, as Git
// reports it: a directory whose whole content is excluded stands for what is
// under it, so a checkout with a hundred thousand ignored files is a few dozen
// names rather than a walk of all of them.
type ExcludedInputs struct {
	Root      string
	Untracked []string
	Ignored   []string
}

// Base is the commit a run's work starts from: the baseline commit carrying the
// uncommitted work it was given, and otherwise the source HEAD it was staged at.
func (snapshot Snapshot) Base() string {
	if snapshot.BaselineCommit != "" {
		return snapshot.BaselineCommit
	}
	return snapshot.SourceHead
}

// requireAbsent refuses to write where something already is. A path that cannot
// be inspected is refused too: pisafe creates these only when it knows there is
// nothing there to lose.
func requireAbsent(path, noun string) error {
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return fmt.Errorf("%s already exists: %s", noun, path)
		}
		return fmt.Errorf("inspect %s: %w", noun, err)
	}
	return nil
}

// safePath bounds a slash-separated path chosen outside the Mac that pisafe
// then writes to inside a workspace: it has to stay under the workspace and out
// of any Git directory. Both spellings of "clean" must agree, so a name that is
// one path to Git and another to the filesystem is refused rather than resolved.
func safePath(subject, name string) error {
	if name == "" || name == ".." || strings.HasPrefix(name, "../") ||
		path.Clean(name) != name || path.IsAbs(name) ||
		filepath.ToSlash(filepath.Clean(filepath.FromSlash(name))) != name ||
		filepath.IsAbs(filepath.FromSlash(name)) {
		return fmt.Errorf("unsafe %s path %q", subject, name)
	}
	for _, segment := range strings.Split(name, "/") {
		if segment == ".git" {
			return fmt.Errorf("unsafe %s path %q: names a Git directory", subject, name)
		}
	}
	return nil
}

// cloneStaged restores one bundled repository and holds it to the commit the
// snapshot says it was captured at, so a bundle that is not the one prepared
// never becomes a workspace.
func cloneStaged(ctx context.Context, bundlePath, target, head string) error {
	if err := gitRun(ctx, "", nil, nil, "clone", "--quiet", bundlePath, target); err != nil {
		return fmt.Errorf("clone staged repository: %w", err)
	}
	cloned, err := gitOutput(ctx, target, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return fmt.Errorf("resolve staged HEAD: %w", err)
	}
	if cloned != head {
		return fmt.Errorf("staged HEAD mismatch: wanted %s, got %s", head, cloned)
	}
	return nil
}

func RepositoryRoot(ctx context.Context, sourcePath string) (string, error) {
	root, err := gitOutput(ctx, sourcePath, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("find repository: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve repository path: %w", err)
	}
	return root, nil
}

func ListExcludedInputs(ctx context.Context, sourcePath string) (ExcludedInputs, error) {
	root, err := RepositoryRoot(ctx, sourcePath)
	if err != nil {
		return ExcludedInputs{}, err
	}
	untracked, err := gitOutputBytes(
		ctx,
		root,
		"ls-files", "-z", "--others", "--exclude-standard", "--directory",
	)
	if err != nil {
		return ExcludedInputs{}, fmt.Errorf("list untracked inputs: %w", err)
	}
	ignored, err := gitOutputBytes(
		ctx,
		root,
		"ls-files", "-z", "--others", "--ignored", "--exclude-standard", "--directory",
	)
	if err != nil {
		return ExcludedInputs{}, fmt.Errorf("list ignored inputs: %w", err)
	}
	ignoredNames := splitNUL(ignored)
	return ExcludedInputs{
		Root:      root,
		Untracked: withoutCovered(splitNUL(untracked), ignoredNames),
		Ignored:   withoutCovered(ignoredNames, nil),
	}, nil
}

// withoutCovered drops a path another entry already stands for. A directory
// whose whole content is ignored is untracked and ignored at once, and an
// ignored file inside a collapsed directory is named twice as well; either way
// one entry already covers the path, and a run reports it once.
func withoutCovered(names, others []string) []string {
	directories := []string{}
	for _, name := range slices.Concat(names, others) {
		if strings.HasSuffix(name, "/") {
			directories = append(directories, name)
		}
	}
	kept := make([]string, 0, len(names))
	for _, name := range names {
		covered := slices.Contains(others, name)
		for _, directory := range directories {
			covered = covered || (directory != name && strings.HasPrefix(name, directory))
		}
		if !covered {
			kept = append(kept, name)
		}
	}
	return kept
}

// Prepare creates the source artifacts that cross the VM boundary: a Git
// bundle rooted at HEAD, a binary patch of the final tracked state, and an
// archive of any explicitly selected untracked inputs. PackageDir must not
// already exist.
func Prepare(ctx context.Context, request PrepareRequest) (PreparedStage, error) {
	runID, packageDir := request.RunID, request.PackageDir
	if err := runid.Validate(runID); err != nil {
		return PreparedStage{}, err
	}
	if err := requireAbsent(packageDir, "stage package"); err != nil {
		return PreparedStage{}, err
	}

	root, err := RepositoryRoot(ctx, request.SourcePath)
	if err != nil {
		return PreparedStage{}, err
	}
	head, err := gitOutput(ctx, root, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return PreparedStage{}, fmt.Errorf("resolve HEAD: %w", err)
	}
	if err := rejectLFS(ctx, root); err != nil {
		return PreparedStage{}, err
	}
	submodules, err := listSubmodules(ctx, root)
	if err != nil {
		return PreparedStage{}, err
	}

	if err := os.Mkdir(packageDir, 0o700); err != nil {
		return PreparedStage{}, fmt.Errorf("create stage package: %w", err)
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(packageDir)
		}
	}()

	prepared := StagePackage(packageDir, Snapshot{
		RunID:        runID,
		SourceRoot:   root,
		SourceHead:   head,
		WorkRef:      "refs/heads/work/" + runID,
		Inputs:       request.Inputs.Files,
		IncludeRoots: request.Inputs.Roots,
		Submodules:   submodules,
		CreatedAt:    time.Now().UTC(),
	})
	if err := captureRepository(ctx, root, prepared.BundlePath, prepared.PatchPath); err != nil {
		return PreparedStage{}, err
	}
	for _, artifacts := range prepared.Submodules {
		working := filepath.Join(root, filepath.FromSlash(artifacts.Path))
		if err := captureRepository(
			ctx,
			working,
			artifacts.BundlePath,
			artifacts.PatchPath,
		); err != nil {
			return PreparedStage{}, fmt.Errorf("submodule %q: %w", artifacts.Path, err)
		}
	}
	if err := recheckHeads(ctx, root, head, submodules); err != nil {
		return PreparedStage{}, err
	}
	if prepared.InputsPath != "" {
		if err := writeFileArchive(root, prepared.InputsPath, prepared.Snapshot.Inputs); err != nil {
			return PreparedStage{}, err
		}
	}
	complete = true
	return prepared, nil
}

// recheckHeads fails the run if the source moved while its artifacts were
// being captured, so a stage is never assembled from two different states.
func recheckHeads(
	ctx context.Context,
	root, head string,
	submodules []SubmoduleStage,
) error {
	current, err := gitOutput(ctx, root, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return fmt.Errorf("recheck HEAD: %w", err)
	}
	if current != head {
		return errors.New("repository HEAD changed while preparing the run")
	}
	for _, submodule := range submodules {
		working := filepath.Join(root, filepath.FromSlash(submodule.Path))
		current, err := gitOutput(ctx, working, "rev-parse", "--verify", "HEAD")
		if err != nil {
			return fmt.Errorf("recheck submodule %q HEAD: %w", submodule.Path, err)
		}
		if current != submodule.Head {
			return fmt.Errorf(
				"submodule %q HEAD changed while preparing the run",
				submodule.Path,
			)
		}
	}
	return nil
}

// Materialize consumes a transferred stage package inside the isolated
// environment. It does not access Snapshot.SourceRoot.
func Materialize(ctx context.Context, prepared PreparedStage, workspace string) (snapshot Snapshot, err error) {
	if err := runid.Validate(prepared.Snapshot.RunID); err != nil {
		return Snapshot{}, err
	}
	if prepared.Snapshot.WorkRef != "refs/heads/work/"+prepared.Snapshot.RunID {
		return Snapshot{}, fmt.Errorf("work ref does not match run ID")
	}
	if err := requireAbsent(workspace, "workspace"); err != nil {
		return Snapshot{}, err
	}
	if err := os.MkdirAll(filepath.Dir(workspace), 0o700); err != nil {
		return Snapshot{}, fmt.Errorf("create workspace parent: %w", err)
	}

	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(workspace)
		}
	}()

	if err := cloneStaged(
		ctx,
		prepared.BundlePath,
		workspace,
		prepared.Snapshot.SourceHead,
	); err != nil {
		return Snapshot{}, err
	}
	if err := gitRun(
		ctx,
		workspace,
		nil,
		nil,
		"switch", "--quiet", "-c", "work/"+prepared.Snapshot.RunID,
	); err != nil {
		return Snapshot{}, fmt.Errorf("create work branch: %w", err)
	}

	patch, err := os.ReadFile(prepared.PatchPath)
	if err != nil {
		return Snapshot{}, fmt.Errorf("read tracked patch: %w", err)
	}

	if len(patch) != 0 {
		if err := gitRun(ctx, workspace, bytes.NewReader(patch), nil, "apply", "--index", "--binary", "-"); err != nil {
			return Snapshot{}, fmt.Errorf("apply tracked baseline: %w", err)
		}
	}
	submodules, err := restoreSubmodules(ctx, prepared, workspace)
	if err != nil {
		return Snapshot{}, err
	}

	// The tracked patch and every submodule gitlink are staged by now, so one
	// question decides whether a baseline is needed.
	baseline := ""
	staged, err := indexDiffersFromHead(ctx, workspace)
	if err != nil {
		return Snapshot{}, err
	}
	if staged {
		if err := commit(ctx, workspace, baselineMessage); err != nil {
			return Snapshot{}, fmt.Errorf("commit tracked baseline: %w", err)
		}
		baseline, err = gitOutput(ctx, workspace, "rev-parse", "HEAD")
		if err != nil {
			return Snapshot{}, fmt.Errorf("resolve baseline commit: %w", err)
		}
	}

	// Inputs land once the run's history is settled, because they are not part
	// of it. They sit beside it as files, exactly as they do on the host.
	if err := restoreInputs(prepared, workspace); err != nil {
		return Snapshot{}, err
	}

	cleanup = false
	snapshot = prepared.Snapshot
	snapshot.BaselineCommit = baseline
	snapshot.Submodules = submodules
	return snapshot, nil
}

func indexDiffersFromHead(ctx context.Context, workspace string) (bool, error) {
	err := gitRun(ctx, workspace, nil, nil, "diff", "--cached", "--quiet", "--")
	switch {
	case err == nil:
		return false, nil
	case isExitCode(err, 1):
		return true, nil
	default:
		return false, fmt.Errorf("inspect staged changes: %w", err)
	}
}

// Stage is a local composition of Prepare and Materialize, primarily useful
// in tests. The controller uses the two operations separately with an SSH
// transfer between them.
func Stage(ctx context.Context, request PrepareRequest, workspace string) (Snapshot, error) {
	packageDir, err := os.MkdirTemp(filepath.Dir(workspace), ".pisafe-stage-package-*")
	if err != nil {
		return Snapshot{}, fmt.Errorf("reserve stage package path: %w", err)
	}
	if err := os.Remove(packageDir); err != nil {
		return Snapshot{}, fmt.Errorf("prepare stage package path: %w", err)
	}
	defer os.RemoveAll(packageDir)

	request.PackageDir = packageDir
	prepared, err := Prepare(ctx, request)
	if err != nil {
		return Snapshot{}, err
	}
	return Materialize(ctx, prepared, workspace)
}

func FinalizeTracked(ctx context.Context, workspace string) (commitID string, untracked []string, err error) {
	if err := gitRun(ctx, workspace, nil, nil, "add", "-u", "--"); err != nil {
		return "", nil, fmt.Errorf("stage final tracked changes: %w", err)
	}

	untrackedOutput, err := gitOutputBytes(
		ctx,
		workspace,
		"ls-files", "-z", "--others", "--exclude-standard",
	)
	if err != nil {
		return "", nil, fmt.Errorf("list final untracked files: %w", err)
	}
	untracked = splitNUL(untrackedOutput)

	staged, err := indexDiffersFromHead(ctx, workspace)
	if err != nil {
		return "", nil, err
	}
	if !staged {
		return "", untracked, nil
	}

	if err := commit(ctx, workspace, finalMessage); err != nil {
		return "", nil, fmt.Errorf("commit final tracked state: %w", err)
	}
	commitID, err = gitOutput(ctx, workspace, "rev-parse", "HEAD")
	if err != nil {
		return "", nil, fmt.Errorf("resolve final commit: %w", err)
	}
	return commitID, untracked, nil
}

// restoreInputs unpacks the selected inputs into the workspace and leaves them
// there, untracked. An included path crosses as files in both directions, so
// the run's history never contains one and the agent cannot commit it by
// accident. The snapshot, not the archive, decides what belongs in the run: an
// archive naming anything else is refused.
func restoreInputs(prepared PreparedStage, workspace string) error {
	expected := prepared.Snapshot.Inputs
	if len(expected) == 0 {
		return nil
	}
	if prepared.InputsPath == "" {
		return errors.New("staged snapshot lists inputs but no archive was transferred")
	}
	extracted, err := extractFileArchive(prepared.InputsPath, workspace)
	if err != nil {
		return err
	}
	if !sameNames(expected, extracted) {
		return errors.New("input archive does not match the staged snapshot")
	}
	return nil
}

func sameNames(expected []SelectedInput, extracted []string) bool {
	left := make([]string, 0, len(expected))
	for _, input := range expected {
		left = append(left, input.Path)
	}
	right := append([]string(nil), extracted...)
	sort.Strings(left)
	sort.Strings(right)
	return slices.Equal(left, right)
}

func rejectLFS(ctx context.Context, root string) error {
	files, err := gitOutputBytes(ctx, root, "ls-files", "-z")
	if err != nil {
		return fmt.Errorf("list files for LFS check: %w", err)
	}
	var attributes bytes.Buffer
	if err := gitRun(
		ctx,
		root,
		bytes.NewReader(files),
		&attributes,
		"check-attr", "--cached", "-z", "--stdin", "filter",
	); err != nil {
		return fmt.Errorf("inspect LFS attributes: %w", err)
	}

	fields := splitNUL(attributes.Bytes())
	for index := 2; index < len(fields); index += 3 {
		if fields[index] == "lfs" {
			return ErrLFSNotSupported
		}
	}
	return nil
}

func splitNUL(data []byte) []string {
	parts := splitNULBytes(data)
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		result = append(result, string(part))
	}
	return result
}

func splitNULBytes(data []byte) [][]byte {
	parts := bytes.Split(data, []byte{0})
	if len(parts) > 0 && len(parts[len(parts)-1]) == 0 {
		parts = parts[:len(parts)-1]
	}
	return parts
}

// pisafeCommitConfig makes the commits pisafe itself writes independent of the
// ambient Git configuration, which differs between the Mac and a run.
var pisafeCommitConfig = []string{
	"-c", "user.name=pisafe",
	"-c", "user.email=pisafe@localhost.invalid",
	"-c", "commit.gpgsign=false",
}

func commit(ctx context.Context, dir, message string) error {
	args := append(append([]string{}, pisafeCommitConfig...),
		"commit", "--quiet", "--no-verify", "-m", message,
	)
	return gitRun(ctx, dir, nil, nil, args...)
}

func gitOutputBytes(ctx context.Context, dir string, args ...string) ([]byte, error) {
	var stdout bytes.Buffer
	if err := gitRun(ctx, dir, nil, &stdout, args...); err != nil {
		return nil, err
	}
	return stdout.Bytes(), nil
}
