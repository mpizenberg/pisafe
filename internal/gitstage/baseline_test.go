package gitstage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stageDirtyRun prepares a run from a repository with one uncommitted tracked
// change, so its history starts with a baseline commit.
func stageDirtyRun(t *testing.T, runID, dirty string) (string, string, Snapshot) {
	t.Helper()
	source := newRepository(t)
	mustWrite(t, filepath.Join(source, "tracked.txt"), dirty)
	workspace := filepath.Join(t.TempDir(), "workspace")
	snapshot, err := Stage(
		context.Background(),
		PrepareRequest{SourcePath: source, RunID: runID},
		workspace,
	)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.BaselineCommit == "" {
		t.Fatal("dirty repository produced no baseline commit")
	}
	return source, workspace, snapshot
}

func TestDroppedBaselineImportsOnlyTheRunsOwnCommits(t *testing.T) {
	ctx := context.Background()
	source, workspace, snapshot := stageDirtyRun(t, "drop-run", "carried-in edit\n")

	// The agent works on a file of its own, so nothing depends on the
	// carried-in change.
	mustWrite(t, filepath.Join(workspace, "agent.txt"), "agent work\n")
	runGit(t, workspace, "add", "agent.txt")
	runGit(t, workspace, "commit", "-qm", "agent commit")
	mustWrite(t, filepath.Join(workspace, "agent.txt"), "agent work, uncommitted\n")

	result, err := Apply(ctx, snapshot, workspace, DropBaseline)
	if err != nil {
		t.Fatal(err)
	}
	commits := strings.Split(
		runGit(t, source, "log", "--format=%s", snapshot.SourceHead+".."+result.Branch),
		"\n",
	)
	expected := []string{finalMessage, "agent commit"}
	if strings.Join(commits, "|") != strings.Join(expected, "|") {
		t.Fatalf("imported commits = %#v, want %#v", commits, expected)
	}
	// The imported branch is the run's work applied to the checkout as it
	// stood, so the carried-in edit is simply absent from it.
	if content := runGit(t, source, "show", result.Tip+":tracked.txt"); content != "initial" {
		t.Fatalf("imported tracked.txt = %q", content)
	}
	if content := runGit(t, source, "show", result.Tip+":agent.txt"); content != "agent work, uncommitted" {
		t.Fatalf("imported agent.txt = %q", content)
	}
	// The run keeps its own history: the branch, the working tree, and the
	// baseline commit are all where the agent left them.
	if head := runGit(t, workspace, "symbolic-ref", "HEAD"); head != snapshot.WorkRef {
		t.Fatalf("run HEAD = %q", head)
	}
	runGit(t, workspace, "merge-base", "--is-ancestor", snapshot.BaselineCommit, snapshot.WorkRef)
	assertFile(t, filepath.Join(workspace, "tracked.txt"), "carried-in edit\n")
}

func TestDroppedBaselineStopsOnConflictWithoutTouchingAnything(t *testing.T) {
	ctx := context.Background()
	source, workspace, snapshot := stageDirtyRun(t, "conflict-run", "carried-in edit\n")

	// The agent builds on exactly the line the carried-in work changed, so its
	// commit has no meaning without the baseline.
	mustWrite(t, filepath.Join(workspace, "tracked.txt"), "carried-in edit, refined\n")
	runGit(t, workspace, "add", "tracked.txt")
	runGit(t, workspace, "commit", "-qm", "agent commit")
	runTip := runGit(t, workspace, "rev-parse", snapshot.WorkRef)

	_, err := Apply(ctx, snapshot, workspace, DropBaseline)
	conflict := &BaselineReplayConflict{}
	if !errors.As(err, &conflict) {
		t.Fatalf("error = %v", err)
	}
	if len(conflict.Paths) != 1 || conflict.Paths[0] != "tracked.txt" {
		t.Fatalf("conflicting paths = %#v", conflict.Paths)
	}
	if _, err := gitOutput(
		ctx,
		source,
		"rev-parse", "--verify", "refs/heads/pisafe/conflict-run",
	); err == nil {
		t.Fatal("a conflicted replay created an import branch")
	}
	// The run is left ready for the other answer: same branch, same tip, no
	// rebase in progress.
	if runGit(t, workspace, "rev-parse", snapshot.WorkRef) != runTip {
		t.Fatal("the run's branch moved")
	}
	if head := runGit(t, workspace, "symbolic-ref", "HEAD"); head != snapshot.WorkRef {
		t.Fatalf("run HEAD = %q", head)
	}
	if status := runGit(t, workspace, "status", "--porcelain"); status != "" {
		t.Fatalf("run workspace = %q", status)
	}
	if _, err := Apply(ctx, snapshot, workspace, KeepBaseline); err != nil {
		t.Fatalf("keeping the baseline after a conflict: %v", err)
	}
}

func TestDroppedBaselineRefusesRunsThatHaveNoneToDrop(t *testing.T) {
	ctx := context.Background()
	source := newRepository(t)
	workspace := filepath.Join(t.TempDir(), "workspace")
	snapshot, err := Stage(
		ctx,
		PrepareRequest{SourcePath: source, RunID: "clean-run"},
		workspace,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(ctx, snapshot, workspace, DropBaseline); err == nil ||
		!strings.Contains(err.Error(), "no baseline commit") {
		t.Fatalf("error = %v", err)
	}
}

// A submodule that carried uncommitted work of its own has a baseline commit
// the superproject's commits point at, so the two cannot be separated.
func TestDroppedBaselineRefusesSubmodulesCarryingTheirOwnWork(t *testing.T) {
	ctx := context.Background()
	source := newRepository(t)
	dependency := newRepository(t)
	addSubmodule(t, source, dependency, "dependency")
	mustWrite(t, filepath.Join(source, "tracked.txt"), "superproject edit\n")
	mustWrite(t, filepath.Join(source, "dependency", "tracked.txt"), "submodule edit\n")
	workspace := filepath.Join(t.TempDir(), "workspace")
	snapshot, err := Stage(
		ctx,
		PrepareRequest{SourcePath: source, RunID: "submodule-baseline-run"},
		workspace,
	)
	if err != nil {
		t.Fatal(err)
	}
	if paths := snapshot.SubmoduleBaselines(); len(paths) != 1 || paths[0] != "dependency" {
		t.Fatalf("submodule baselines = %#v", paths)
	}
	if _, err := Apply(ctx, snapshot, workspace, DropBaseline); err == nil ||
		!strings.Contains(err.Error(), "uncommitted work") {
		t.Fatalf("error = %v", err)
	}
}

// The Mac never takes the run's word for it: an import that asked for the
// baseline to go is refused if the history still contains it.
func TestImportApplyRefusesAHistoryThatKeptTheBaseline(t *testing.T) {
	ctx := context.Background()
	_, workspace, snapshot := stageDirtyRun(t, "dishonest-run", "carried-in edit\n")
	mustWrite(t, filepath.Join(workspace, "agent.txt"), "agent work\n")
	runGit(t, workspace, "add", "agent.txt")
	runGit(t, workspace, "commit", "-qm", "agent commit")

	packageDir := t.TempDir()
	prepared, err := PrepareApply(ctx, snapshot, workspace, packageDir, KeepBaseline)
	if err != nil {
		t.Fatal(err)
	}
	// Rebuild the kept history under the name a replayed one would carry, which
	// is all a run would have to do to pass the drop off as done.
	if err := os.Remove(filepath.Join(packageDir, applyBundleName)); err != nil {
		t.Fatal(err)
	}
	runGit(t, workspace, "update-ref", applyBundleRef(snapshot, DropBaseline), snapshot.WorkRef)
	hash, err := createIncrementalBundle(
		ctx,
		workspace,
		filepath.Join(packageDir, applyBundleName),
		applyBundleRef(snapshot, DropBaseline),
		snapshot.SourceHead,
	)
	if err != nil {
		t.Fatal(err)
	}
	prepared.BundleSHA256 = hash

	if _, err := ImportApply(ctx, snapshot, prepared, packageDir, DropBaseline); err == nil ||
		!strings.Contains(err.Error(), "still contains the baseline commit") {
		t.Fatalf("error = %v", err)
	}
}

func TestParseBaselineChoice(t *testing.T) {
	for _, value := range []string{"keep", "drop"} {
		if choice, err := ParseBaselineChoice(value); err != nil || string(choice) != value {
			t.Fatalf("%q parsed as %q, %v", value, choice, err)
		}
	}
	for _, value := range []string{"", "Keep", "replay", "--drop-baseline"} {
		if _, err := ParseBaselineChoice(value); err == nil {
			t.Fatalf("%q was accepted", value)
		}
	}
}
