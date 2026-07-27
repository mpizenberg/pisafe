package gitstage

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// stageWithSubmodule prepares a run whose workspace holds a superproject and
// one initialized submodule, and returns the source, the workspace, and the
// materialized snapshot.
func stageWithSubmodule(t *testing.T, runID string) (string, string, Snapshot) {
	t.Helper()
	source := newRepository(t)
	dependency := newRepository(t)
	addSubmodule(t, source, dependency, "dependency")
	workspace := filepath.Join(t.TempDir(), "workspace")
	snapshot, err := Stage(
		context.Background(),
		PrepareRequest{SourcePath: source, RunID: runID},
		workspace,
	)
	if err != nil {
		t.Fatal(err)
	}
	return source, workspace, snapshot
}

func TestApplyImportsSubmoduleHistoryAndReportsExpectedCommits(t *testing.T) {
	ctx := context.Background()
	source, workspace, snapshot := stageWithSubmodule(t, "apply-submodule-run")
	submoduleWorkspace := filepath.Join(workspace, "dependency")

	// The agent commits in the submodule and leaves the superproject dirty.
	mustWrite(t, filepath.Join(submoduleWorkspace, "tracked.txt"), "submodule work\n")
	runGit(t, submoduleWorkspace, "add", "tracked.txt")
	runGit(t, submoduleWorkspace, "commit", "-qm", "submodule change")
	submoduleTip := runGit(t, submoduleWorkspace, "rev-parse", "HEAD")
	mustWrite(t, filepath.Join(workspace, "tracked.txt"), "superproject work\n")

	sourceBefore := runGit(t, source, "rev-parse", "HEAD")
	result, err := Apply(ctx, snapshot, workspace, KeepBaseline)
	if err != nil {
		t.Fatal(err)
	}
	if result.Branch != "pisafe/apply-submodule-run" {
		t.Fatalf("branch = %q", result.Branch)
	}
	if len(result.Submodules) != 1 {
		t.Fatalf("submodules = %#v", result.Submodules)
	}
	reported := result.Submodules[0]
	if reported.Path != "dependency" || reported.Tip != submoduleTip ||
		reported.Branch != "pisafe/apply-submodule-run" {
		t.Fatalf("reported submodule = %#v", reported)
	}

	// Both repositories keep the imported commits reachable.
	dependency := filepath.Join(source, "dependency")
	if got := runGit(
		t,
		dependency,
		"rev-parse", "refs/heads/pisafe/apply-submodule-run",
	); got != submoduleTip {
		t.Fatalf("submodule branch = %s, want %s", got, submoduleTip)
	}
	if got := runGit(
		t,
		source,
		"rev-parse", "refs/heads/pisafe/apply-submodule-run",
	); got != result.Tip {
		t.Fatalf("superproject branch = %s, want %s", got, result.Tip)
	}
	// The imported branch expects exactly the submodule commit that was
	// imported beside it.
	expected := runGit(t, source, "rev-parse", result.Tip+":dependency")
	if expected != submoduleTip {
		t.Fatalf("imported gitlink = %s, want %s", expected, submoduleTip)
	}

	// Neither checkout moved.
	if runGit(t, source, "rev-parse", "HEAD") != sourceBefore {
		t.Fatal("source HEAD moved")
	}
	assertFile(t, filepath.Join(source, "tracked.txt"), "initial\n")
	assertFile(t, filepath.Join(dependency, "tracked.txt"), "initial\n")
	for _, repository := range []string{source, dependency} {
		if _, err := gitOutput(
			ctx,
			repository,
			"rev-parse", "--verify", "refs/pisafe/incoming/apply-submodule-run",
		); err == nil {
			t.Fatalf("%s kept its temporary import ref", repository)
		}
	}
}

func TestApplyLeavesUnchangedSubmodulesWithoutABranch(t *testing.T) {
	ctx := context.Background()
	source, workspace, snapshot := stageWithSubmodule(t, "quiet-submodule-run")
	mustWrite(t, filepath.Join(workspace, "tracked.txt"), "superproject only\n")

	result, err := Apply(ctx, snapshot, workspace, KeepBaseline)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Submodules) != 1 || result.Submodules[0].Branch != "" {
		t.Fatalf("submodules = %#v", result.Submodules)
	}
	if _, err := gitOutput(
		ctx,
		filepath.Join(source, "dependency"),
		"rev-parse", "--verify", "refs/heads/pisafe/quiet-submodule-run",
	); err == nil {
		t.Fatal("an unchanged submodule got an import branch")
	}
}

func TestCommitApplyIsIdempotent(t *testing.T) {
	ctx := context.Background()
	source, workspace, snapshot := stageWithSubmodule(t, "idempotent-run")
	mustWrite(t, filepath.Join(workspace, "tracked.txt"), "work\n")

	planned := planApply(t, snapshot, workspace)
	if err := CommitApply(ctx, planned.Journal); err != nil {
		t.Fatal(err)
	}
	tip := runGit(t, source, "rev-parse", "refs/heads/pisafe/idempotent-run")

	// Replaying the journal after an interruption must be a no-op.
	if err := CommitApply(ctx, planned.Journal); err != nil {
		t.Fatalf("replay failed: %v", err)
	}
	if got := runGit(t, source, "rev-parse", "refs/heads/pisafe/idempotent-run"); got != tip {
		t.Fatalf("branch moved to %s", got)
	}
}

func TestCommitApplyStopsWhenARefChangedMeanwhile(t *testing.T) {
	ctx := context.Background()
	source, workspace, snapshot := stageWithSubmodule(t, "contested-run")
	mustWrite(t, filepath.Join(workspace, "tracked.txt"), "work\n")

	planned := planApply(t, snapshot, workspace)
	// Someone creates the same branch pointing elsewhere before the journal
	// runs.
	runGit(t, source, "update-ref", "refs/heads/pisafe/contested-run", snapshot.SourceHead)

	err := CommitApply(ctx, planned.Journal)
	if !errors.Is(err, ErrApplyNeedsReconciliation) {
		t.Fatalf("err = %v, want ErrApplyNeedsReconciliation", err)
	}
	if got := runGit(
		t,
		source,
		"rev-parse", "refs/heads/pisafe/contested-run",
	); got != snapshot.SourceHead {
		t.Fatalf("contested ref was overwritten with %s", got)
	}
}

func TestRollbackRemovesOnlyTheRefsApplyCreated(t *testing.T) {
	ctx := context.Background()
	source, workspace, snapshot := stageWithSubmodule(t, "rollback-run")
	submoduleWorkspace := filepath.Join(workspace, "dependency")
	mustWrite(t, filepath.Join(submoduleWorkspace, "tracked.txt"), "submodule work\n")
	runGit(t, submoduleWorkspace, "add", "tracked.txt")
	runGit(t, submoduleWorkspace, "commit", "-qm", "submodule change")
	mustWrite(t, filepath.Join(workspace, "tracked.txt"), "superproject work\n")

	planned := planApply(t, snapshot, workspace)
	if err := CommitApply(ctx, planned.Journal); err != nil {
		t.Fatal(err)
	}
	// A ref the user moved after the fact is left for them to reconcile.
	runGit(t, source, "update-ref", "refs/heads/pisafe/rollback-run", snapshot.SourceHead)

	if err := RollbackApply(ctx, planned.Journal); err != nil {
		t.Fatal(err)
	}
	if got := runGit(
		t,
		source,
		"rev-parse", "refs/heads/pisafe/rollback-run",
	); got != snapshot.SourceHead {
		t.Fatalf("moved ref was deleted or changed: %s", got)
	}
	if _, err := gitOutput(
		ctx,
		filepath.Join(source, "dependency"),
		"rev-parse", "--verify", "refs/heads/pisafe/rollback-run",
	); err == nil {
		t.Fatal("submodule ref survived rollback")
	}
}

func TestImportApplyRefusesAPackageThatDisagreesWithTheRun(t *testing.T) {
	ctx := context.Background()
	_, workspace, snapshot := stageWithSubmodule(t, "disagreeing-run")
	mustWrite(t, filepath.Join(workspace, "tracked.txt"), "work\n")

	packageDir := t.TempDir()
	prepared, err := PrepareApply(ctx, snapshot, workspace, packageDir, KeepBaseline)
	if err != nil {
		t.Fatal(err)
	}
	dropped := prepared
	dropped.Submodules = nil
	if _, err := ImportApply(ctx, snapshot, dropped, packageDir, KeepBaseline); err == nil ||
		!strings.Contains(err.Error(), "staged submodules") {
		t.Fatalf("dropped submodule err = %v", err)
	}

	renamed := prepared
	renamed.Submodules = []PreparedApplySubmodule{{Path: "elsewhere"}}
	if _, err := ImportApply(ctx, snapshot, renamed, packageDir, KeepBaseline); err == nil ||
		!strings.Contains(err.Error(), "staged submodules") {
		t.Fatalf("renamed submodule err = %v", err)
	}
}

func TestPreparedApplyNamesOnlyTheBundlesItProduced(t *testing.T) {
	_, workspace, snapshot := stageWithSubmodule(t, "artifact-run")
	submoduleWorkspace := filepath.Join(workspace, "dependency")
	mustWrite(t, filepath.Join(submoduleWorkspace, "tracked.txt"), "submodule work\n")
	runGit(t, submoduleWorkspace, "add", "tracked.txt")
	runGit(t, submoduleWorkspace, "commit", "-qm", "submodule change")

	packageDir := t.TempDir()
	prepared, err := PrepareApply(context.Background(), snapshot, workspace, packageDir, KeepBaseline)
	if err != nil {
		t.Fatal(err)
	}
	artifacts := prepared.Artifacts()
	if len(artifacts) != 2 ||
		artifacts[0].Name != "apply-submodule-0.bundle" ||
		artifacts[1].Name != "apply.bundle" {
		t.Fatalf("artifacts = %#v", artifacts)
	}
	for _, artifact := range artifacts {
		hash, err := fileSHA256(filepath.Join(packageDir, artifact.Name))
		if err != nil {
			t.Fatal(err)
		}
		if hash != artifact.SHA256 {
			t.Fatalf("%s hash = %s, want %s", artifact.Name, hash, artifact.SHA256)
		}
	}

	// An unchanged repository contributes no bundle to fetch.
	_, quietWorkspace, quietSnapshot := stageWithSubmodule(t, "quiet-artifact-run")
	quiet, err := PrepareApply(
		context.Background(),
		quietSnapshot,
		quietWorkspace,
		t.TempDir(),
		KeepBaseline,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(quiet.Artifacts()) != 0 {
		t.Fatalf("unchanged artifacts = %#v", quiet.Artifacts())
	}
}

func planApply(t *testing.T, snapshot Snapshot, workspace string) PlannedApply {
	t.Helper()
	packageDir := t.TempDir()
	prepared, err := PrepareApply(context.Background(), snapshot, workspace, packageDir, KeepBaseline)
	if err != nil {
		t.Fatal(err)
	}
	planned, err := ImportApply(context.Background(), snapshot, prepared, packageDir, KeepBaseline)
	if err != nil {
		t.Fatal(err)
	}
	return planned
}
