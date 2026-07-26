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
}

// PreparedApplySubmodule carries one submodule's new history. Base is the
// commit the Mac already has, which is where the incremental bundle starts.
type PreparedApplySubmodule struct {
	Path         string `json:"path"`
	Base         string `json:"base"`
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
	return artifacts
}

// ApplyJournal is the durable record of one in-flight apply. Every object it
// names is already imported and verified when the journal exists, so replaying
// it after an interruption only has to finish moving refs.
type ApplyJournal struct {
	RunID string      `json:"run_id"`
	Steps []ApplyStep `json:"steps"`
}

// ApplyStep creates one ref in one repository. Apply only ever creates a new
// pisafe/<run> ref, so a step has no previous value to restore: rolling one
// back deletes the ref, and only while it still holds the recorded commit.
type ApplyStep struct {
	Repository   string `json:"repository"`
	Ref          string `json:"ref"`
	Commit       string `json:"commit"`
	TemporaryRef string `json:"temporary_ref"`
}

// PlannedApply pairs the journal to execute with what the user will be told
// once it succeeds.
type PlannedApply struct {
	Journal ApplyJournal `json:"journal"`
	Result  ApplyResult  `json:"result"`
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
) (PreparedApply, error) {
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

	tip, err := gitOutput(ctx, workspace, "rev-parse", "--verify", snapshot.WorkRef+"^{commit}")
	if err != nil {
		return PreparedApply{}, fmt.Errorf("resolve run tip: %w", err)
	}
	if err := gitRun(
		ctx,
		workspace,
		nil,
		nil,
		"merge-base", "--is-ancestor", snapshot.SourceHead, tip,
	); err != nil {
		return PreparedApply{}, fmt.Errorf("run history is not based on captured source HEAD")
	}

	prepared := PreparedApply{
		RunID:       snapshot.RunID,
		Tip:         tip,
		FinalCommit: finalCommit,
		Untracked:   untracked,
		Submodules:  submodules,
	}
	if tip == snapshot.SourceHead {
		return prepared, nil
	}
	hash, err := createIncrementalBundle(
		ctx,
		workspace,
		filepath.Join(packageDir, applyBundleName),
		snapshot.WorkRef,
		snapshot.SourceHead,
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
		if err := safeSubmodulePath(submodule.Path); err != nil {
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
		base := submodule.BaselineCommit
		if base == "" {
			base = submodule.Head
		}
		if err := gitRun(
			ctx,
			target,
			nil,
			nil,
			"merge-base", "--is-ancestor", base, tip,
		); err != nil {
			return nil, nil, fmt.Errorf(
				"submodule %q history is not based on its staged commit",
				submodule.Path,
			)
		}
		entry := PreparedApplySubmodule{
			Path:        submodule.Path,
			Base:        submodule.Head,
			Tip:         tip,
			FinalCommit: finalCommit,
		}
		if tip != submodule.Head {
			hash, err := createIncrementalBundle(
				ctx,
				target,
				filepath.Join(packageDir, applySubmoduleBundleName(index)),
				"HEAD",
				submodule.Head,
			)
			if err != nil {
				return nil, nil, fmt.Errorf("submodule %q: %w", submodule.Path, err)
			}
			entry.BundleSHA256 = hash
		}
		prepared = append(prepared, entry)
	}
	return prepared, untracked, nil
}

func createIncrementalBundle(
	ctx context.Context,
	repository, bundlePath, ref, base string,
) (string, error) {
	if _, err := os.Stat(bundlePath); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return "", fmt.Errorf("apply bundle already exists: %s", bundlePath)
		}
		return "", fmt.Errorf("inspect apply bundle path: %w", err)
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
func ImportApply(
	ctx context.Context,
	snapshot Snapshot,
	prepared PreparedApply,
	packageDir string,
) (PlannedApply, error) {
	if runid.Validate(snapshot.RunID) != nil || prepared.RunID != snapshot.RunID {
		return PlannedApply{}, fmt.Errorf("apply package does not match run")
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

	targetRef := "refs/heads/pisafe/" + snapshot.RunID
	temporaryRef := "refs/pisafe/incoming/" + snapshot.RunID
	journal := ApplyJournal{RunID: snapshot.RunID}
	result := ApplyResult{
		Branch:       strings.TrimPrefix(targetRef, "refs/heads/"),
		Tip:          prepared.Tip,
		FinalCommit:  prepared.FinalCommit,
		Untracked:    prepared.Untracked,
		BundleSHA256: prepared.BundleSHA256,
	}

	// Submodule refs are created before the superproject ref, so an
	// interruption can only ever leave commits reachable, never a branch
	// pointing at gitlinks that are not.
	for index, submodule := range prepared.Submodules {
		if submodule.Path != snapshot.Submodules[index].Path {
			return PlannedApply{}, fmt.Errorf("apply package does not match the staged submodules")
		}
		if err := safeSubmodulePath(submodule.Path); err != nil {
			return PlannedApply{}, err
		}
		repository := filepath.Join(sourceRoot, filepath.FromSlash(submodule.Path))
		result.Submodules = append(result.Submodules, AppliedSubmodule{
			Path: submodule.Path,
			Tip:  submodule.Tip,
		})
		if submodule.Tip == submodule.Base {
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
			Repository:   repository,
			Ref:          targetRef,
			Commit:       submodule.Tip,
			TemporaryRef: temporaryRef,
		})
	}

	if prepared.Tip != snapshot.SourceHead {
		if err := importBundle(
			ctx,
			sourceRoot,
			temporaryRef,
			targetRef,
			snapshot.WorkRef,
			filepath.Join(packageDir, applyBundleName),
			prepared.BundleSHA256,
			prepared.Tip,
		); err != nil {
			return PlannedApply{}, err
		}
	} else if prepared.BundleSHA256 != "" {
		return PlannedApply{}, fmt.Errorf("unchanged apply unexpectedly contains a bundle")
	} else if err := requireAbsentRef(ctx, sourceRoot, targetRef); err != nil {
		return PlannedApply{}, err
	}
	journal.Steps = append(journal.Steps, ApplyStep{
		Repository: sourceRoot,
		Ref:        targetRef,
		Commit:     prepared.Tip,
	})
	if prepared.Tip != snapshot.SourceHead {
		journal.Steps[len(journal.Steps)-1].TemporaryRef = temporaryRef
	}
	return PlannedApply{Journal: journal, Result: result}, nil
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
	if err := gitRun(
		ctx,
		repository,
		nil,
		nil,
		"fetch", "--quiet", "--no-write-fetch-head",
		bundlePath, bundleRef+":"+temporaryRef,
	); err != nil {
		return fmt.Errorf("import apply bundle: %w", err)
	}
	imported, err := gitOutput(ctx, repository, "rev-parse", "--verify", temporaryRef+"^{commit}")
	if err != nil {
		return fmt.Errorf("resolve imported tip: %w", err)
	}
	if imported != expectedTip {
		return fmt.Errorf("imported tip mismatch: wanted %s, got %s", expectedTip, imported)
	}
	return nil
}

func requireAbsentRef(ctx context.Context, repository, ref string) error {
	existing, err := gitOutput(ctx, repository, "rev-parse", "--verify", "--quiet", ref)
	if err == nil || existing != "" {
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
		done, err := stepIsComplete(ctx, step)
		if err != nil {
			return err
		}
		if !done {
			if err := createRef(ctx, step.Repository, step.Ref, step.Commit); err != nil {
				return err
			}
		}
		if step.TemporaryRef != "" {
			_ = gitRun(
				ctx,
				step.Repository,
				nil,
				nil,
				"update-ref", "-d", step.TemporaryRef, step.Commit,
			)
		}
	}
	return nil
}

// RollbackApply removes the refs a partial CommitApply created. A ref that no
// longer holds the recorded commit is left alone.
func RollbackApply(ctx context.Context, journal ApplyJournal) error {
	if err := runid.Validate(journal.RunID); err != nil {
		return err
	}
	for _, step := range journal.Steps {
		if step.TemporaryRef != "" {
			_ = gitRun(
				ctx,
				step.Repository,
				nil,
				nil,
				"update-ref", "-d", step.TemporaryRef,
			)
		}
		done, err := stepIsComplete(ctx, step)
		if err != nil || !done {
			continue
		}
		if err := gitRun(
			ctx,
			step.Repository,
			nil,
			nil,
			"update-ref", "-d", step.Ref, step.Commit,
		); err != nil {
			return fmt.Errorf("roll back %s in %s: %w", step.Ref, step.Repository, err)
		}
	}
	return nil
}

func stepIsComplete(ctx context.Context, step ApplyStep) (bool, error) {
	current, err := gitOutput(ctx, step.Repository, "rev-parse", "--verify", "--quiet", step.Ref)
	switch {
	case err == nil && current == step.Commit:
		return true, nil
	case err == nil:
		return false, fmt.Errorf(
			"%w: %s in %s holds %s",
			ErrApplyNeedsReconciliation,
			step.Ref,
			step.Repository,
			current,
		)
	case isExitCode(err, 1):
		return false, nil
	default:
		return false, fmt.Errorf("inspect %s: %w", step.Ref, err)
	}
}

// Apply composes the isolated and host halves locally for tests. The
// controller uses PrepareApply, ImportApply, and CommitApply separately with
// an SSH transfer between them.
func Apply(ctx context.Context, snapshot Snapshot, workspace string) (ApplyResult, error) {
	packageDir, err := os.MkdirTemp("", "pisafe-apply-*")
	if err != nil {
		return ApplyResult{}, fmt.Errorf("reserve apply package path: %w", err)
	}
	defer os.RemoveAll(packageDir)

	prepared, err := PrepareApply(ctx, snapshot, workspace, packageDir)
	if err != nil {
		return ApplyResult{}, err
	}
	planned, err := ImportApply(ctx, snapshot, prepared, packageDir)
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
