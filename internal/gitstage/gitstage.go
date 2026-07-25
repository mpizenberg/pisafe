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
	"path/filepath"
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

type Snapshot struct {
	RunID          string           `json:"run_id"`
	SourceRoot     string           `json:"source_root"`
	SourceHead     string           `json:"source_head"`
	WorkRef        string           `json:"work_ref"`
	BaselineCommit string           `json:"baseline_commit,omitempty"`
	Inputs         []string         `json:"inputs,omitempty"`
	Submodules     []SubmoduleStage `json:"submodules,omitempty"`
	CreatedAt      time.Time        `json:"created_at"`
}

type ApplyResult struct {
	Branch       string
	Tip          string
	FinalCommit  string
	Untracked    []string
	BundleSHA256 string
}

type PreparedStage struct {
	Snapshot   Snapshot
	BundlePath string
	PatchPath  string
	InputsPath string
	Submodules []PreparedSubmodule
}

// PrepareRequest describes one staging operation. Selected inputs are the only
// source content that is copied rather than derived from Git history.
type PrepareRequest struct {
	SourcePath string
	PackageDir string
	RunID      string
	Inputs     InputSelection
}

type ExcludedInputs struct {
	Untracked []string
	Ignored   []string
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
		"ls-files", "-z", "--others", "--exclude-standard",
	)
	if err != nil {
		return ExcludedInputs{}, fmt.Errorf("list untracked inputs: %w", err)
	}
	ignored, err := gitOutputBytes(
		ctx,
		root,
		"ls-files", "-z", "--others", "--ignored", "--exclude-standard",
	)
	if err != nil {
		return ExcludedInputs{}, fmt.Errorf("list ignored inputs: %w", err)
	}
	return ExcludedInputs{
		Untracked: splitNUL(untracked),
		Ignored:   splitNUL(ignored),
	}, nil
}

// Prepare creates the source artifacts that cross the VM boundary: a Git
// bundle rooted at HEAD, a binary patch of the final tracked state, and an
// archive of any explicitly selected untracked inputs. PackageDir must not
// already exist.
func Prepare(ctx context.Context, request PrepareRequest) (prepared PreparedStage, err error) {
	runID, packageDir := request.RunID, request.PackageDir
	if err := runid.Validate(runID); err != nil {
		return PreparedStage{}, err
	}
	if _, err := os.Stat(packageDir); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return PreparedStage{}, fmt.Errorf("stage package already exists: %s", packageDir)
		}
		return PreparedStage{}, fmt.Errorf("inspect stage package: %w", err)
	}

	root, err := RepositoryRoot(ctx, request.SourcePath)
	if err != nil {
		return PreparedStage{}, err
	}
	inputs, err := selectInputEntries(ctx, root, request.Inputs)
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

	bundlePath := filepath.Join(packageDir, "source.bundle")
	patchPath := filepath.Join(packageDir, "tracked.patch")
	if err := captureRepository(ctx, root, bundlePath, patchPath); err != nil {
		return PreparedStage{}, err
	}
	preparedSubmodules := make([]PreparedSubmodule, 0, len(submodules))
	for index, submodule := range submodules {
		working := filepath.Join(root, filepath.FromSlash(submodule.Path))
		artifacts := PreparedSubmodule{
			Path:       submodule.Path,
			BundlePath: filepath.Join(packageDir, submoduleBundleName(index)),
			PatchPath:  filepath.Join(packageDir, submodulePatchName(index)),
		}
		if err := captureRepository(
			ctx,
			working,
			artifacts.BundlePath,
			artifacts.PatchPath,
		); err != nil {
			return PreparedStage{}, fmt.Errorf("submodule %q: %w", submodule.Path, err)
		}
		preparedSubmodules = append(preparedSubmodules, artifacts)
	}
	if err := recheckHeads(ctx, root, head, submodules); err != nil {
		return PreparedStage{}, err
	}

	inputsPath := ""
	names := make([]string, 0, len(inputs))
	if len(inputs) != 0 {
		inputsPath = filepath.Join(packageDir, inputsArchiveName)
		if err := writeInputsArchive(root, inputsPath, inputs); err != nil {
			return PreparedStage{}, err
		}
		for _, entry := range inputs {
			names = append(names, entry.path)
		}
	}

	snapshot := Snapshot{
		RunID:      runID,
		SourceRoot: root,
		SourceHead: head,
		WorkRef:    "refs/heads/work/" + runID,
		Inputs:     names,
		Submodules: submodules,
		CreatedAt:  time.Now().UTC(),
	}
	complete = true
	return PreparedStage{
		Snapshot:   snapshot,
		BundlePath: bundlePath,
		PatchPath:  patchPath,
		InputsPath: inputsPath,
		Submodules: preparedSubmodules,
	}, nil
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
	if _, err := os.Stat(workspace); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return Snapshot{}, fmt.Errorf("workspace already exists: %s", workspace)
		}
		return Snapshot{}, fmt.Errorf("inspect workspace: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(workspace), 0o700); err != nil {
		return Snapshot{}, fmt.Errorf("create workspace parent: %w", err)
	}
	if err := gitRun(ctx, "", nil, nil, "clone", "--quiet", prepared.BundlePath, workspace); err != nil {
		return Snapshot{}, fmt.Errorf("clone staged repository: %w", err)
	}

	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(workspace)
		}
	}()

	clonedHead, err := gitOutput(ctx, workspace, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return Snapshot{}, fmt.Errorf("resolve staged HEAD: %w", err)
	}
	if clonedHead != prepared.Snapshot.SourceHead {
		return Snapshot{}, fmt.Errorf(
			"staged HEAD mismatch: wanted %s, got %s",
			prepared.Snapshot.SourceHead,
			clonedHead,
		)
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
	if err := restoreInputs(ctx, prepared, workspace); err != nil {
		return Snapshot{}, err
	}
	submodules, err := restoreSubmodules(ctx, prepared, workspace)
	if err != nil {
		return Snapshot{}, err
	}

	// The tracked patch, the selected inputs, and every submodule gitlink are
	// all staged by now, so one question decides whether a baseline is needed.
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
		return false, fmt.Errorf("inspect staged baseline: %w", err)
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

	quietErr := gitRun(ctx, workspace, nil, nil, "diff", "--cached", "--quiet", "--")
	switch {
	case quietErr == nil:
		return "", untracked, nil
	case !isExitCode(quietErr, 1):
		return "", nil, fmt.Errorf("inspect final tracked changes: %w", quietErr)
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

// restoreInputs unpacks and stages the selected inputs so they join the
// baseline commit. The snapshot, not the archive, decides what belongs in the
// run: an archive naming anything else is refused.
func restoreInputs(ctx context.Context, prepared PreparedStage, workspace string) error {
	expected := prepared.Snapshot.Inputs
	if len(expected) == 0 {
		return nil
	}
	if prepared.InputsPath == "" {
		return errors.New("staged snapshot lists inputs but no archive was transferred")
	}
	extracted, err := extractInputs(prepared.InputsPath, workspace)
	if err != nil {
		return err
	}
	if !sameNames(expected, extracted) {
		return errors.New("input archive does not match the staged snapshot")
	}
	// Selected inputs are untracked, and may be ignored, so staging them needs
	// an explicit override. The literal prefix keeps a file name that happens
	// to look like pathspec magic from changing what is staged.
	pathspecs := make([]string, 0, len(extracted))
	for _, name := range extracted {
		pathspecs = append(pathspecs, ":(literal)"+name)
	}
	if err := gitRun(
		ctx,
		workspace,
		strings.NewReader(strings.Join(pathspecs, "\x00")),
		nil,
		"add", "--force", "--pathspec-from-file=-", "--pathspec-file-nul",
	); err != nil {
		return fmt.Errorf("stage selected inputs: %w", err)
	}
	return nil
}

func sameNames(first, second []string) bool {
	left := append([]string(nil), first...)
	right := append([]string(nil), second...)
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

func commit(ctx context.Context, dir, message string) error {
	return gitRun(
		ctx,
		dir,
		nil,
		nil,
		"-c", "user.name=pisafe",
		"-c", "user.email=pisafe@localhost.invalid",
		"-c", "commit.gpgsign=false",
		"commit", "--quiet", "--no-verify", "-m", message,
	)
}

func gitOutputBytes(ctx context.Context, dir string, args ...string) ([]byte, error) {
	var stdout bytes.Buffer
	if err := gitRun(ctx, dir, nil, &stdout, args...); err != nil {
		return nil, err
	}
	return stdout.Bytes(), nil
}
