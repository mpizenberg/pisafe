package gitstage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/mpizenberg/pisafe/internal/runid"
)

// PreparedApply describes what one run produced. It crosses the boundary out
// of the run, so it names no filesystem path: every bundle is addressed by the
// artifact names formed here and verified against the hashes it carries.
type PreparedApply struct {
	RunID        string                   `json:"run_id"`
	Tip          string                   `json:"tip"`
	FinalCommit  string                   `json:"final_commit,omitempty"`
	Untracked    []string                 `json:"untracked,omitempty"`
	BundleSHA256 string                   `json:"bundle_sha256,omitempty"`
	Submodules   []PreparedApplySubmodule `json:"submodules,omitempty"`
	// Outputs is the work the run leaves under the paths the user included,
	// carried as files because that is how it arrived.
	Outputs       []SelectedInput `json:"outputs,omitempty"`
	OutputsSHA256 string          `json:"outputs_sha256,omitempty"`
	// ReplayConflicts names the paths that stopped a requested replay. It is
	// the whole answer when it is set: the run was left as the agent left it
	// and there is nothing to import.
	ReplayConflicts []string `json:"replay_conflicts,omitempty"`
}

// PreparedApplySubmodule carries one submodule's new history. Its bundle starts
// at the commit the Mac staged, which the Mac reads from its own snapshot: this
// record crosses the boundary out of the run and cannot be asked where the
// history it carries begins.
type PreparedApplySubmodule struct {
	Path         string `json:"path"`
	Tip          string `json:"tip"`
	FinalCommit  string `json:"final_commit,omitempty"`
	BundleSHA256 string `json:"bundle_sha256,omitempty"`
}

// ApplyArtifact names one bundle the run must hand back and the hash it must
// still have on arrival.
type ApplyArtifact struct {
	Name   string
	SHA256 string
}

// Artifacts lists the bundles that belong to a prepared apply, in the order
// they are imported.
func (prepared PreparedApply) Artifacts() []ApplyArtifact {
	artifacts := make([]ApplyArtifact, 0, len(prepared.Submodules)+1)
	for index, submodule := range prepared.Submodules {
		if submodule.BundleSHA256 == "" {
			continue
		}
		artifacts = append(artifacts, ApplyArtifact{
			Name:   applySubmoduleBundleName(index),
			SHA256: submodule.BundleSHA256,
		})
	}
	if prepared.BundleSHA256 != "" {
		artifacts = append(artifacts, ApplyArtifact{
			Name:   applyBundleName,
			SHA256: prepared.BundleSHA256,
		})
	}
	// The included work is written after the refs it accompanies, so it is
	// fetched last.
	if prepared.OutputsSHA256 != "" {
		artifacts = append(artifacts, ApplyArtifact{
			Name:   OutputsArtifactName,
			SHA256: prepared.OutputsSHA256,
		})
	}
	return artifacts
}

// ApplyJournal is the durable record of one in-flight apply. Every object it
// names is already imported and verified when the journal exists, so replaying
// it after an interruption only has to finish moving refs.
type ApplyJournal struct {
	RunID string      `json:"run_id"`
	Steps []ApplyStep `json:"steps"`
}

// Ref is the branch every step of a journal creates, and TemporaryRef the ref
// its bundles were imported into. Both follow from the run ID, so a stored
// journal has no way to name a ref its run never earned.
func (journal ApplyJournal) Ref() string {
	return "refs/heads/pisafe/" + journal.RunID
}

func (journal ApplyJournal) TemporaryRef() string {
	return "refs/pisafe/incoming/" + journal.RunID
}

// ApplyStep creates the journal's ref in one repository. Apply only ever
// creates that ref, so a step has no previous value to restore: rolling one
// back deletes the ref, and only while it still holds the recorded commit.
type ApplyStep struct {
	Repository string `json:"repository"`
	Commit     string `json:"commit"`
}

// PlannedApply pairs the journal to execute with what the user will be told
// once it succeeds.
type PlannedApply struct {
	Journal ApplyJournal `json:"journal"`
	Result  ApplyResult  `json:"result"`
	// Outputs is the included work the run handed back, kept with the plan so
	// it survives an interruption between the ref import and the copy.
	Outputs []SelectedInput `json:"outputs,omitempty"`
}

var ErrApplyNeedsReconciliation = errors.New(
	"a ref changed outside pisafe; apply stopped for manual reconciliation",
)

const applyBundleName = "apply.bundle"

func applySubmoduleBundleName(index int) string {
	return "apply-submodule-" + strconv.Itoa(index) + ".bundle"
}

// PrepareApply runs inside the isolated environment. It captures any work the
// agent left uncommitted, in the submodules first so the superproject records
// where they ended up, and creates the incremental bundles that will be
// streamed back to the controller.
func PrepareApply(
	ctx context.Context,
	snapshot Snapshot,
	workspace string,
	packageDir string,
	choice BaselineChoice,
) (PreparedApply, error) {
	if _, err := ParseBaselineChoice(string(choice)); err != nil {
		return PreparedApply{}, err
	}
	if err := runid.Validate(snapshot.RunID); err != nil {
		return PreparedApply{}, err
	}
	if snapshot.WorkRef != "refs/heads/work/"+snapshot.RunID {
		return PreparedApply{}, fmt.Errorf("work ref does not match run ID")
	}
	if packageDir == "" {
		return PreparedApply{}, fmt.Errorf("apply package directory is required")
	}
	if err := os.MkdirAll(packageDir, 0o700); err != nil {
		return PreparedApply{}, fmt.Errorf("create apply package: %w", err)
	}

	submodules, untracked, err := prepareApplySubmodules(ctx, snapshot, workspace, packageDir)
	if err != nil {
		return PreparedApply{}, err
	}
	finalCommit, superUntracked, err := FinalizeTracked(ctx, workspace)
	if err != nil {
		return PreparedApply{}, err
	}
	untracked = append(untracked, superUntracked...)

	outputs, outputsHash, err := captureIncludedOutputs(ctx, snapshot, workspace, packageDir)
	if err != nil {
		return PreparedApply{}, err
	}
	untracked = withoutReturned(untracked, outputs)

	tip, err := gitOutput(ctx, workspace, "rev-parse", "--verify", snapshot.WorkRef+"^{commit}")
	if err != nil {
		return PreparedApply{}, fmt.Errorf("resolve run tip: %w", err)
	}
	if err := requireAncestor(
		ctx,
		workspace,
		snapshot.SourceHead,
		tip,
		"run history is not based on captured source HEAD",
	); err != nil {
		return PreparedApply{}, err
	}

	bundleRef := applyBundleRef(snapshot, choice)
	if choice == DropBaseline {
		replayedTip, conflicts, err := replayWithoutBaseline(ctx, snapshot, workspace, packageDir)
		if err != nil {
			return PreparedApply{}, err
		}
		if len(conflicts) != 0 {
			return PreparedApply{RunID: snapshot.RunID, ReplayConflicts: conflicts}, nil
		}
		// The bundle is a file of its own, so the ref has nothing left to keep
		// reachable once it is written.
		defer func() { _ = gitRun(ctx, workspace, nil, nil, "update-ref", "-d", bundleRef) }()
		tip = replayedTip
	}

	prepared := PreparedApply{
		RunID:         snapshot.RunID,
		Tip:           tip,
		FinalCommit:   finalCommit,
		Untracked:     untracked,
		Submodules:    submodules,
		Outputs:       outputs,
		OutputsSHA256: outputsHash,
	}
	hash, err := captureIncremental(
		ctx,
		workspace,
		filepath.Join(packageDir, applyBundleName),
		bundleRef,
		snapshot.SourceHead,
		tip,
	)
	if err != nil {
		return PreparedApply{}, err
	}
	prepared.BundleSHA256 = hash
	return prepared, nil
}

func prepareApplySubmodules(
	ctx context.Context,
	snapshot Snapshot,
	workspace string,
	packageDir string,
) ([]PreparedApplySubmodule, []string, error) {
	prepared := make([]PreparedApplySubmodule, 0, len(snapshot.Submodules))
	untracked := []string{}
	for index, submodule := range snapshot.Submodules {
		if err := safePath("submodule", submodule.Path); err != nil {
			return nil, nil, err
		}
		target := filepath.Join(workspace, filepath.FromSlash(submodule.Path))
		finalCommit, names, err := FinalizeTracked(ctx, target)
		if err != nil {
			return nil, nil, fmt.Errorf("submodule %q: %w", submodule.Path, err)
		}
		for _, name := range names {
			untracked = append(untracked, submodule.Path+"/"+name)
		}
		tip, err := gitOutput(ctx, target, "rev-parse", "--verify", "HEAD")
		if err != nil {
			return nil, nil, fmt.Errorf("resolve submodule %q tip: %w", submodule.Path, err)
		}
		if err := requireAncestor(
			ctx,
			target,
			submodule.Base(),
			tip,
			"history is not based on its staged commit",
		); err != nil {
			return nil, nil, fmt.Errorf("submodule %q: %w", submodule.Path, err)
		}
		hash, err := captureIncremental(
			ctx,
			target,
			filepath.Join(packageDir, applySubmoduleBundleName(index)),
			"HEAD",
			submodule.Head,
			tip,
		)
		if err != nil {
			return nil, nil, fmt.Errorf("submodule %q: %w", submodule.Path, err)
		}
		prepared = append(prepared, PreparedApplySubmodule{
			Path:         submodule.Path,
			Tip:          tip,
			FinalCommit:  finalCommit,
			BundleSHA256: hash,
		})
	}
	return prepared, untracked, nil
}

// captureIncremental bundles the history one repository gained since the commit
// it was given, and reports no bundle when it gained none. That is the one case
// an apply carries no objects, so it is decided here rather than at each caller.
func captureIncremental(
	ctx context.Context,
	repository, bundlePath, ref, base, tip string,
) (string, error) {
	if tip == base {
		return "", nil
	}
	return createIncrementalBundle(ctx, repository, bundlePath, ref, base)
}

func createIncrementalBundle(
	ctx context.Context,
	repository, bundlePath, ref, base string,
) (string, error) {
	if err := requireAbsent(bundlePath, "apply bundle"); err != nil {
		return "", err
	}
	if err := gitRun(
		ctx,
		repository,
		nil,
		nil,
		"bundle", "create", bundlePath, ref, "^"+base,
	); err != nil {
		return "", fmt.Errorf("create incremental apply bundle: %w", err)
	}
	hash, err := fileSHA256(bundlePath)
	if err != nil {
		return "", fmt.Errorf("hash apply bundle: %w", err)
	}
	return hash, nil
}

// ImportApply runs on the Mac. It verifies and imports every object set into
// temporary refs before anything user-visible changes, and returns the journal
// that CommitApply then executes. The transferred bundles are read from
// packageDir under the names the run was told to use.
//
// An import that fails takes its temporary refs with it, so a refusal past the
// first fetch — a branch already taken, an unresolvable gitlink — leaves the
// repository as it found it and does not pin the objects of an apply that never
// happened.
func ImportApply(
	ctx context.Context,
	snapshot Snapshot,
	prepared PreparedApply,
	packageDir string,
	choice BaselineChoice,
) (planned PlannedApply, returnErr error) {
	if runid.Validate(snapshot.RunID) != nil || prepared.RunID != snapshot.RunID {
		return PlannedApply{}, fmt.Errorf("apply package does not match run")
	}
	if _, err := ParseBaselineChoice(string(choice)); err != nil {
		return PlannedApply{}, err
	}
	if len(prepared.ReplayConflicts) != 0 {
		if choice != DropBaseline {
			return PlannedApply{}, fmt.Errorf(
				"apply package reports a replay that was not asked for",
			)
		}
		return PlannedApply{}, &BaselineReplayConflict{Paths: prepared.ReplayConflicts}
	}
	if (len(prepared.Outputs) == 0) != (prepared.OutputsSHA256 == "") {
		return PlannedApply{}, errors.New("apply package describes its outputs inconsistently")
	}
	if err := requireOutputsUnderRoots(snapshot.IncludeRoots, prepared.Outputs); err != nil {
		return PlannedApply{}, err
	}
	sourceRoot, err := filepath.EvalSymlinks(snapshot.SourceRoot)
	if err != nil {
		return PlannedApply{}, fmt.Errorf("resolve source repository: %w", err)
	}
	currentHead, err := gitOutput(
		ctx,
		sourceRoot,
		"rev-parse", "--verify", snapshot.SourceHead+"^{commit}",
	)
	if err != nil || currentHead != snapshot.SourceHead {
		return PlannedApply{}, fmt.Errorf("captured source commit is unavailable")
	}
	if len(prepared.Submodules) != len(snapshot.Submodules) {
		return PlannedApply{}, fmt.Errorf("apply package does not match the staged submodules")
	}

	journal := ApplyJournal{RunID: snapshot.RunID}
	targetRef := journal.Ref()
	temporaryRef := journal.TemporaryRef()
	// A journal built only part-way is undone exactly as a committed one is:
	// nothing user-visible has moved yet, so every step rolls back to discarding
	// the temporary ref it imported into. The context is kept alive because a
	// cancelled apply is when there is most to clean up.
	defer func() {
		if returnErr != nil {
			_ = RollbackApply(context.WithoutCancel(ctx), journal)
		}
	}()
	result := ApplyResult{
		Branch:      strings.TrimPrefix(targetRef, "refs/heads/"),
		Tip:         prepared.Tip,
		FinalCommit: prepared.FinalCommit,
		Untracked:   prepared.Untracked,
	}

	// Submodule refs are created before the superproject ref, so an
	// interruption can only ever leave commits reachable, never a branch
	// pointing at gitlinks that are not.
	for index, submodule := range prepared.Submodules {
		staged := snapshot.Submodules[index]
		if submodule.Path != staged.Path {
			return PlannedApply{}, fmt.Errorf("apply package does not match the staged submodules")
		}
		if err := safePath("submodule", submodule.Path); err != nil {
			return PlannedApply{}, err
		}
		repository := filepath.Join(sourceRoot, filepath.FromSlash(submodule.Path))
		result.Submodules = append(result.Submodules, AppliedSubmodule{
			Path: submodule.Path,
			Tip:  submodule.Tip,
		})
		if submodule.Tip == staged.Head {
			continue
		}
		if err := importBundle(
			ctx,
			repository,
			temporaryRef,
			targetRef,
			"HEAD",
			filepath.Join(packageDir, applySubmoduleBundleName(index)),
			submodule.BundleSHA256,
			submodule.Tip,
		); err != nil {
			return PlannedApply{}, fmt.Errorf("submodule %q: %w", submodule.Path, err)
		}
		result.Submodules[len(result.Submodules)-1].Branch =
			strings.TrimPrefix(targetRef, "refs/heads/")
		journal.Steps = append(journal.Steps, ApplyStep{
			Repository: repository,
			Commit:     submodule.Tip,
		})
	}

	if prepared.Tip != snapshot.SourceHead {
		if err := importBundle(
			ctx,
			sourceRoot,
			temporaryRef,
			targetRef,
			applyBundleRef(snapshot, choice),
			filepath.Join(packageDir, applyBundleName),
			prepared.BundleSHA256,
			prepared.Tip,
		); err != nil {
			return PlannedApply{}, err
		}
		if choice == DropBaseline {
			if err := requireBaselineDropped(
				ctx,
				sourceRoot,
				temporaryRef,
				snapshot.BaselineCommit,
			); err != nil {
				return PlannedApply{}, err
			}
		}
	} else if prepared.BundleSHA256 != "" {
		return PlannedApply{}, fmt.Errorf("unchanged apply unexpectedly contains a bundle")
	} else if err := requireAbsentRef(ctx, sourceRoot, targetRef); err != nil {
		return PlannedApply{}, err
	}
	if err := requireSubmodulesPresent(ctx, sourceRoot, snapshot, prepared.Tip); err != nil {
		return PlannedApply{}, err
	}
	journal.Steps = append(journal.Steps, ApplyStep{
		Repository: sourceRoot,
		Commit:     prepared.Tip,
	})
	return PlannedApply{Journal: journal, Result: result, Outputs: prepared.Outputs}, nil
}

// importBundle verifies a transferred bundle and fetches it into a temporary
// ref, leaving the repository's branches untouched.
func importBundle(
	ctx context.Context,
	repository, temporaryRef, targetRef, bundleRef, bundlePath, expectedHash, expectedTip string,
) error {
	if expectedHash == "" {
		return fmt.Errorf("changed apply has no bundle")
	}
	if err := requireAbsentRef(ctx, repository, targetRef); err != nil {
		return err
	}
	hash, err := fileSHA256(bundlePath)
	if err != nil {
		return fmt.Errorf("hash transferred apply bundle: %w", err)
	}
	if hash != expectedHash {
		return fmt.Errorf("apply bundle hash mismatch")
	}
	if err := gitRun(ctx, repository, nil, nil, "bundle", "verify", bundlePath); err != nil {
		return fmt.Errorf("verify apply bundle: %w", err)
	}
	// The refspec is forced because the destination is pisafe's own scratch,
	// named by the run and written by nothing else. An import that died where no
	// cleanup could run — a crash, a kill — must not turn its leftover into a
	// precondition, refusing a later apply for a history that was rewritten
	// rather than for anything wrong with it.
	if err := gitRun(
		ctx,
		repository,
		nil,
		nil,
		"fetch", "--quiet", "--no-write-fetch-head",
		bundlePath, "+"+bundleRef+":"+temporaryRef,
	); err != nil {
		return fmt.Errorf("import apply bundle: %w", err)
	}
	imported, err := gitOutput(ctx, repository, "rev-parse", "--verify", temporaryRef+"^{commit}")
	if err != nil {
		return fmt.Errorf("resolve imported tip: %w", err)
	}
	if imported != expectedTip {
		// Nothing downstream records this repository, so the ref holding what the
		// bundle actually carried goes with the refusal rather than keeping those
		// objects reachable.
		discardTemporaryRef(ctx, repository, temporaryRef, imported)
		return fmt.Errorf("imported tip mismatch: wanted %s, got %s", expectedTip, imported)
	}
	return nil
}

// gitlink is one submodule pointer a commit records: where it sits, and the
// commit it names there.
type gitlink struct {
	path   string
	commit string
}

const gitlinkMode = "160000"

// requireSubmodulesPresent proves the branch about to land can be checked out
// with its submodules. What a run may hand back is fixed when it is staged, and
// a submodule it adds is outside that set: no bundle carries the new
// repository's objects, nothing on the Mac holds them, and every other check
// passes on a superproject whose tree points into nothing.
//
// A pointer to a staged submodule is checked too, though its commit is one the
// bundle beside it necessarily carries. That holds only because two things
// elsewhere are true — a submodule may not end below its staged base, and the
// final commit records where each one actually ended — and the property is the
// branch's to keep rather than theirs to imply.
//
// Only the pointers the run changed are examined, because one it left alone
// names what the source commit already named. Presence is the whole question:
// moving a pin to a commit the submodule already has is a change like any
// other, whichever direction it moves.
func requireSubmodulesPresent(
	ctx context.Context,
	sourceRoot string,
	snapshot Snapshot,
	tip string,
) error {
	moved, err := changedGitlinks(ctx, sourceRoot, snapshot.SourceHead, tip)
	if err != nil {
		return err
	}
	for _, link := range moved {
		if err := safePath("submodule", link.path); err != nil {
			return err
		}
		staged := slices.ContainsFunc(
			snapshot.Submodules,
			func(submodule SubmoduleStage) bool { return submodule.Path == link.path },
		)
		if !staged {
			return fmt.Errorf(
				"the run added a submodule at %q; pisafe staged no repository there, "+
					"so its history cannot be carried back",
				link.path,
			)
		}
		repository := filepath.Join(sourceRoot, filepath.FromSlash(link.path))
		// --quiet makes an object the submodule does not have exit 1 rather than
		// print, so success here means the commit is there to check out.
		_, err := gitOutput(
			ctx, repository, "rev-parse", "--verify", "--quiet", link.commit+"^{commit}",
		)
		if err == nil {
			continue
		}
		if isExitCode(err, 1) {
			return fmt.Errorf(
				"submodule %q: the run's history expects commit %s, which the "+
					"submodule does not have and the run handed back no way to obtain",
				link.path,
				link.commit,
			)
		}
		return fmt.Errorf("submodule %q: resolve expected commit: %w", link.path, err)
	}
	return nil
}

// changedGitlinks reports the submodule pointers one commit records differently
// from another, as the path and the commit now named there. A pointer the newer
// commit no longer carries is not among them: a removed submodule leaves
// nothing to resolve.
func changedGitlinks(ctx context.Context, repository, from, to string) ([]gitlink, error) {
	output, err := gitOutputBytes(
		ctx,
		repository,
		"diff-tree", "-r", "-z", "--no-commit-id", "--no-renames", from, to,
	)
	if err != nil {
		return nil, fmt.Errorf("compare recorded submodule pointers: %w", err)
	}
	// The raw format alternates one metadata record with the path it describes.
	records := splitNUL(output)
	links := make([]gitlink, 0, len(records)/2)
	for index := 0; index+1 < len(records); index += 2 {
		fields := strings.Fields(records[index])
		if len(fields) != 5 {
			return nil, fmt.Errorf("unreadable comparison of submodule pointers")
		}
		if fields[1] != gitlinkMode {
			continue
		}
		links = append(links, gitlink{path: records[index+1], commit: fields[3]})
	}
	return links, nil
}

func requireAbsentRef(ctx context.Context, repository, ref string) error {
	// --quiet makes a missing ref exit 1 rather than print nothing and succeed,
	// so success here means the ref is there.
	_, err := gitOutput(ctx, repository, "rev-parse", "--verify", "--quiet", ref)
	if err == nil {
		return ErrBranchExists
	}
	if !isExitCode(err, 1) {
		return fmt.Errorf("inspect target branch: %w", err)
	}
	return nil
}

// CommitApply moves refs one repository at a time. It is idempotent: a step
// whose ref already holds the recorded commit is complete, and a ref holding
// anything else stops the run for manual reconciliation instead of
// overwriting a change made meanwhile.
func CommitApply(ctx context.Context, journal ApplyJournal) error {
	if err := runid.Validate(journal.RunID); err != nil {
		return err
	}
	for _, step := range journal.Steps {
		done, err := stepIsComplete(ctx, journal, step)
		if err != nil {
			return err
		}
		if !done {
			if err := createRef(ctx, step.Repository, journal.Ref(), step.Commit); err != nil {
				return err
			}
		}
		discardTemporaryRef(ctx, step.Repository, journal.TemporaryRef(), step.Commit)
	}
	return nil
}

// discardTemporaryRef drops the ref a bundle was imported into, and only while
// it still holds the commit that was imported there. A repository that needed
// no bundle has no such ref, and one holding anything else was put there by
// something other than this import; both are left as they are.
func discardTemporaryRef(ctx context.Context, repository, ref, commit string) {
	_ = gitRun(ctx, repository, nil, nil, "update-ref", "-d", ref, commit)
}

// RollbackApply removes the refs a partial CommitApply created. A ref that no
// longer holds the recorded commit is left alone.
func RollbackApply(ctx context.Context, journal ApplyJournal) error {
	if err := runid.Validate(journal.RunID); err != nil {
		return err
	}
	for _, step := range journal.Steps {
		discardTemporaryRef(ctx, step.Repository, journal.TemporaryRef(), step.Commit)
		done, err := stepIsComplete(ctx, journal, step)
		if err != nil || !done {
			continue
		}
		if err := gitRun(
			ctx,
			step.Repository,
			nil,
			nil,
			"update-ref", "-d", journal.Ref(), step.Commit,
		); err != nil {
			return fmt.Errorf("roll back %s in %s: %w", journal.Ref(), step.Repository, err)
		}
	}
	return nil
}

func stepIsComplete(ctx context.Context, journal ApplyJournal, step ApplyStep) (bool, error) {
	ref := journal.Ref()
	current, err := gitOutput(ctx, step.Repository, "rev-parse", "--verify", "--quiet", ref)
	switch {
	case err == nil && current == step.Commit:
		return true, nil
	case err == nil:
		return false, fmt.Errorf(
			"%w: %s in %s holds %s",
			ErrApplyNeedsReconciliation,
			ref,
			step.Repository,
			current,
		)
	case isExitCode(err, 1):
		return false, nil
	default:
		return false, fmt.Errorf("inspect %s: %w", ref, err)
	}
}

// Apply composes the isolated and host halves locally for tests. The
// controller uses PrepareApply, ImportApply, and CommitApply separately with
// an SSH transfer between them.
func Apply(
	ctx context.Context,
	snapshot Snapshot,
	workspace string,
	choice BaselineChoice,
) (ApplyResult, error) {
	packageDir, err := os.MkdirTemp("", "pisafe-apply-*")
	if err != nil {
		return ApplyResult{}, fmt.Errorf("reserve apply package path: %w", err)
	}
	defer os.RemoveAll(packageDir)

	prepared, err := PrepareApply(ctx, snapshot, workspace, packageDir, choice)
	if err != nil {
		return ApplyResult{}, err
	}
	planned, err := ImportApply(ctx, snapshot, prepared, packageDir, choice)
	if err != nil {
		return ApplyResult{}, err
	}
	if err := CommitApply(ctx, planned.Journal); err != nil {
		return ApplyResult{}, errors.Join(err, RollbackApply(ctx, planned.Journal))
	}
	return planned.Result, nil
}

func createRef(ctx context.Context, repository, ref, commit string) error {
	zeroOID := strings.Repeat("0", len(commit))
	if err := gitRun(ctx, repository, nil, nil, "update-ref", ref, commit, zeroOID); err != nil {
		if _, checkErr := gitOutput(ctx, repository, "rev-parse", "--verify", ref); checkErr == nil {
			return ErrBranchExists
		}
		return fmt.Errorf("create import branch in %s: %w", repository, err)
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
