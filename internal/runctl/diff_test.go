package runctl

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mpizenberg/pisafe/internal/runstate"
)

// Diff is the one report a user can ask for while an agent is still working,
// so it must leave the run exactly as it found it.
func TestDiffReportsAnActiveRunWithoutStoppingIt(t *testing.T) {
	ctx := context.Background()
	_, workspace, snapshot := applyFixture(t, "diff-run")
	writeFile(t, filepath.Join(workspace, "tracked.txt"), "agent work\n")
	writeFile(t, filepath.Join(workspace, "scratch.log"), "noise\n")

	backend := &fakeBackend{applyWorkspace: workspace}
	store := runstate.NewStore(t.TempDir())
	controller := New(backend, store, &fakeSSHStore{}, testInference{})
	activeRun(t, store, snapshot)

	diff, err := controller.Diff(ctx, snapshot.RunID, testImage)
	if err != nil {
		t.Fatal(err)
	}
	if diff.RunID != snapshot.RunID || len(diff.Repositories) != 1 {
		t.Fatalf("diff = %#v", diff)
	}
	repository := diff.Repositories[0]
	if repository.FileTotal != 1 || repository.Files[0].Path != "tracked.txt" {
		t.Fatalf("files = %#v", repository.Files)
	}
	if repository.UntrackedTotal != 1 || repository.Untracked[0] != "scratch.log" {
		t.Fatalf("untracked = %#v", repository.Untracked)
	}

	manifest, err := store.Get(snapshot.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.State != runstate.StateActive {
		t.Fatalf("diff changed the run's state to %q", manifest.State)
	}
	joined := callsString(backend.calls)
	if !strings.Contains(joined, "pisafe-guest diff") {
		t.Fatalf("diff calls:\n%s", joined)
	}
	if strings.Contains(joined, "podman stop") {
		t.Fatalf("diff stopped the run:\n%s", joined)
	}
}

func TestDiffMountsRunStorageBeforeReadingIt(t *testing.T) {
	ctx := context.Background()
	_, workspace, snapshot := applyFixture(t, "unmounted-diff-run")

	backend := &fakeBackend{applyWorkspace: workspace, failAt: "verify-storage"}
	store := runstate.NewStore(t.TempDir())
	controller := New(backend, store, &fakeSSHStore{}, testInference{})
	activeRun(t, store, snapshot)

	if _, err := controller.Diff(ctx, snapshot.RunID, testImage); err == nil ||
		!strings.Contains(err.Error(), "verify storage") {
		t.Fatalf("error = %v", err)
	}
	if joined := callsString(backend.calls); strings.Contains(joined, "podman") {
		t.Fatalf("diff read a run whose storage was not mounted:\n%s", joined)
	}
}

func TestDiffRefusesARunWithNoWorkspaceLeft(t *testing.T) {
	ctx := context.Background()
	_, workspace, snapshot := applyFixture(t, "discarded-diff-run")

	backend := &fakeBackend{applyWorkspace: workspace}
	store := runstate.NewStore(t.TempDir())
	controller := New(backend, store, &fakeSSHStore{}, testInference{})
	stoppedRun(t, store, snapshot)
	if _, err := controller.Discard(ctx, snapshot.RunID); err != nil {
		t.Fatal(err)
	}

	if _, err := controller.Diff(ctx, snapshot.RunID, testImage); err == nil ||
		!strings.Contains(err.Error(), "no workspace to compare") {
		t.Fatalf("error = %v", err)
	}
}
