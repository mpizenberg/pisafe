package runctl

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mpizenberg/pisafe/internal/gitstage"
	"github.com/mpizenberg/pisafe/internal/runstate"
)

func TestCollectReclaimsAnImportedRunRecordAndAll(t *testing.T) {
	ctx := context.Background()
	backend, store, controller, snapshot := importedRun(t, "gc-imported")
	now := time.Now().UTC()

	// Inside the window an imported run keeps everything it owns.
	fresh, err := controller.Plan(now)
	if err != nil {
		t.Fatal(err)
	}
	if !fresh.Empty() {
		t.Fatalf("fresh plan = %#v", fresh)
	}

	plan, err := controller.Plan(now.Add(Retention + time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Reclaimed) != 1 || plan.Reclaimed[0] != snapshot.RunID {
		t.Fatalf("plan = %#v", plan)
	}
	// An imported run cannot start a container, so it pins no image.
	if len(plan.KeepImages) != 0 {
		t.Fatalf("keep images = %#v", plan.KeepImages)
	}

	before := len(backend.calls)
	done, err := controller.Collect(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(done.Reclaimed) != 1 || done.Reclaimed[0] != snapshot.RunID {
		t.Fatalf("done = %#v", done)
	}
	reclaimed := callsString(backend.calls[before:])
	for _, expected := range []string{"remove-storage", "remove-stage"} {
		if !strings.Contains(reclaimed, expected) {
			t.Errorf("collection did not %s:\n%s", expected, reclaimed)
		}
	}

	// Nothing of the run is left to name, so every command refuses it before
	// reaching the VM.
	if _, err := store.Get(snapshot.RunID); err == nil {
		t.Fatal("a reclaimed run kept its record")
	}
	if _, err := controller.Diff(ctx, snapshot.RunID, testImage); err == nil ||
		!strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("diff of a reclaimed run = %v", err)
	}
}

func TestCollectNeverRemovesWorkThatWasNeverImported(t *testing.T) {
	ctx := context.Background()
	_, workspace, snapshot := applyFixture(t, "gc-stopped")
	backend := &fakeBackend{applyWorkspace: workspace}
	store := runstate.NewStore(t.TempDir())
	controller := New(backend, store, &fakeSSHStore{}, testInference{})
	stoppedRun(t, store, snapshot)

	plan, err := controller.Plan(time.Now().UTC().Add(10 * Retention))
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Empty() {
		t.Fatalf("an unimported run was collected: %#v", plan)
	}
	if len(plan.Kept) != 1 || plan.Kept[0].RunID != snapshot.RunID ||
		!strings.Contains(plan.Kept[0].Reason, "never imported") {
		t.Fatalf("kept = %#v", plan.Kept)
	}
	// A run that can still start a container keeps the image it starts from.
	if len(plan.KeepImages) != 1 || plan.KeepImages[0] != testImage {
		t.Fatalf("keep images = %#v", plan.KeepImages)
	}

	before := len(backend.calls)
	if _, err := controller.Collect(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if len(backend.calls) != before {
		t.Fatalf("collection touched the run: %s", callsString(backend.calls[before:]))
	}
	manifest, err := store.Get(snapshot.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.State != runstate.StateStopped {
		t.Fatalf("state = %q", manifest.State)
	}
}

// A discarded run leaves nothing behind, so no later sweep has anything to
// find: collection sees only what discard did not already release.
func TestCollectHasNothingLeftToDoAfterDiscard(t *testing.T) {
	ctx := context.Background()
	_, store, controller, snapshot := importedRun(t, "gc-discarded")
	if err := controller.Discard(ctx, snapshot.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(snapshot.RunID); err == nil {
		t.Fatal("a discarded run kept its record")
	}
	plan, err := controller.Plan(time.Now().UTC().Add(10 * Retention))
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Empty() || len(plan.Kept) != 0 {
		t.Fatalf("plan = %#v", plan)
	}
}

// importedRun drives the real apply path, so the manifest collection reads is
// the one an import actually leaves behind.
func importedRun(t *testing.T, runID string) (
	*fakeBackend,
	runstate.Store,
	Controller,
	gitstage.Snapshot,
) {
	t.Helper()
	_, workspace, snapshot := applyFixture(t, runID)
	writeFile(t, filepath.Join(workspace, "tracked.txt"), "agent result\n")
	backend := &fakeBackend{
		applyWorkspace: workspace,
		applyPackage:   filepath.Join(t.TempDir(), "package"),
	}
	store := runstate.NewStore(t.TempDir())
	controller := New(backend, store, &fakeSSHStore{}, testInference{})
	stoppedRun(t, store, snapshot)
	if _, _, err := controller.Apply(context.Background(), snapshot.RunID, testImage, gitstage.KeepBaseline); err != nil {
		t.Fatal(err)
	}
	return backend, store, controller, snapshot
}
