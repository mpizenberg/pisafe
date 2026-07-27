package gitstage

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDiffRunReportsWhatTheRunChangedSinceItsBaseline(t *testing.T) {
	workspace, snapshot := stagedRun(t)

	// Work an agent would do: a commit, an uncommitted edit, and a file it
	// never told Git about.
	mustWrite(t, filepath.Join(workspace, "tracked.txt"), "one\ntwo\nthree\n")
	runGit(t, workspace, "commit", "-qam", "extend tracked")
	mustWrite(t, filepath.Join(workspace, "tracked.txt"), "one\ntwo\nthree\nfour\n")
	mustWrite(t, filepath.Join(workspace, "notes.md"), "scratch\n")

	diff, err := DiffRun(context.Background(), snapshot, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if diff.RunID != snapshot.RunID || len(diff.Repositories) != 1 {
		t.Fatalf("diff = %#v", diff)
	}
	repository := diff.Repositories[0]
	if repository.Path != "" || repository.Base != snapshot.SourceHead {
		t.Fatalf("repository = %#v", repository)
	}
	if repository.CommitTotal != 1 || len(repository.Commits) != 1 ||
		repository.Commits[0].Subject != "extend tracked" {
		t.Fatalf("commits = %#v", repository.Commits)
	}
	if len(repository.Files) != 1 || repository.Files[0].Path != "tracked.txt" ||
		repository.Files[0].Insertions != 4 || repository.Files[0].Deletions != 1 {
		t.Fatalf("files = %#v", repository.Files)
	}
	if repository.UntrackedTotal != 1 || repository.Untracked[0] != "notes.md" {
		t.Fatalf("untracked = %#v", repository.Untracked)
	}
}

// The dirty state a user carries into a run is theirs, not the run's work, so
// the diff starts at the baseline commit that captured it.
func TestDiffRunStartsAtTheBaselineCommit(t *testing.T) {
	workspace, snapshot := stagedRun(t)
	mustWrite(t, filepath.Join(workspace, "carried.txt"), "user work\n")
	runGit(t, workspace, "add", "carried.txt")
	runGit(t, workspace, "commit", "-qm", "pisafe: imported working-tree baseline")
	snapshot.BaselineCommit = runGit(t, workspace, "rev-parse", "HEAD")

	diff, err := DiffRun(context.Background(), snapshot, workspace)
	if err != nil {
		t.Fatal(err)
	}
	repository := diff.Repositories[0]
	if repository.Base != snapshot.BaselineCommit {
		t.Fatalf("base = %q", repository.Base)
	}
	if repository.CommitTotal != 0 || len(repository.Files) != 0 {
		t.Fatalf("carried-in work was reported as the run's: %#v", repository)
	}
}

func TestDiffRunReportsBinaryAndDeletedPaths(t *testing.T) {
	workspace, snapshot := stagedRun(t)
	if err := os.Remove(filepath.Join(workspace, "tracked.txt")); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(workspace, "blob.bin"), "\x00\x01\x02binary\x00")
	runGit(t, workspace, "add", "-A")

	diff, err := DiffRun(context.Background(), snapshot, workspace)
	if err != nil {
		t.Fatal(err)
	}
	changes := map[string]DiffFile{}
	for _, file := range diff.Repositories[0].Files {
		changes[file.Path] = file
	}
	if removed := changes["tracked.txt"]; removed.Insertions != 0 || removed.Deletions != 1 {
		t.Fatalf("deleted file = %#v", removed)
	}
	if binary := changes["blob.bin"]; binary.Insertions != -1 || binary.Deletions != -1 {
		t.Fatalf("binary file = %#v", binary)
	}
}

// A run that touches more than the reply can hold must still report honest
// totals rather than a truncated list presented as the whole story.
func TestDiffRunCapsListsButKeepsTotalsExact(t *testing.T) {
	workspace, snapshot := stagedRun(t)
	const created = diffListLimit + 5
	for index := range created {
		mustWrite(t, filepath.Join(workspace, "file-"+strconv.Itoa(index)+".txt"), "content\n")
	}
	runGit(t, workspace, "add", "-A")

	diff, err := DiffRun(context.Background(), snapshot, workspace)
	if err != nil {
		t.Fatal(err)
	}
	repository := diff.Repositories[0]
	if len(repository.Files) != diffListLimit || repository.FileTotal != created {
		t.Fatalf("files = %d, total = %d", len(repository.Files), repository.FileTotal)
	}
}

func TestDiffRunKeepsSubjectsBounded(t *testing.T) {
	workspace, snapshot := stagedRun(t)
	mustWrite(t, filepath.Join(workspace, "tracked.txt"), "changed\n")
	runGit(t, workspace, "commit", "-qam", strings.Repeat("s", diffSubjectLimit*2))

	diff, err := DiffRun(context.Background(), snapshot, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if subject := diff.Repositories[0].Commits[0].Subject; len(subject) != diffSubjectLimit {
		t.Fatalf("subject length = %d", len(subject))
	}
}

// A submodule reports its own work, and the superproject does not repeat it as
// a changed gitlink.
func TestDiffRunReportsSubmoduleWorkSeparately(t *testing.T) {
	ctx := context.Background()
	source := newRepository(t)
	dependency := newRepository(t)
	addSubmodule(t, source, dependency, "dependency")
	workspace := filepath.Join(t.TempDir(), "workspace")
	snapshot, err := Stage(
		ctx,
		PrepareRequest{SourcePath: source, RunID: "diff-20260101-000000-0123456789ab"},
		workspace,
	)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(workspace, "dependency")
	mustWrite(t, filepath.Join(target, "tracked.txt"), "submodule work\n")
	runGit(t, target, "commit", "-qam", "submodule work")

	diff, err := DiffRun(ctx, snapshot, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(diff.Repositories) != 2 {
		t.Fatalf("repositories = %#v", diff.Repositories)
	}
	if superproject := diff.Repositories[0]; len(superproject.Files) != 0 {
		t.Fatalf("superproject repeated submodule work: %#v", superproject.Files)
	}
	submodule := diff.Repositories[1]
	if submodule.Path != "dependency" || submodule.CommitTotal != 1 ||
		len(submodule.Files) != 1 || submodule.Files[0].Path != "tracked.txt" {
		t.Fatalf("submodule = %#v", submodule)
	}
}

func TestDiffRunRefusesAnEscapingSubmodulePath(t *testing.T) {
	workspace, snapshot := stagedRun(t)
	snapshot.Submodules = []SubmoduleStage{{Path: "../escape", Head: snapshot.SourceHead}}

	if _, err := DiffRun(context.Background(), snapshot, workspace); err == nil {
		t.Fatal("an escaping submodule path was diffed")
	}
}

// stagedRun builds a workspace shaped like a materialized run: existing history
// with the run's work ref checked out.
func stagedRun(t *testing.T) (string, Snapshot) {
	t.Helper()
	workspace := newRepository(t)
	head := runGit(t, workspace, "rev-parse", "HEAD")
	runID := "diff-20260101-000000-0123456789ab"
	runGit(t, workspace, "checkout", "-q", "-b", "work/"+runID)
	return workspace, Snapshot{
		RunID:      runID,
		SourceHead: head,
		WorkRef:    "refs/heads/work/" + runID,
		CreatedAt:  time.Now().UTC(),
	}
}
