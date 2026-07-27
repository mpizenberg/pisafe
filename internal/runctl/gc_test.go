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

func TestCollectExpiresAnImportedRunAndKeepsItsAttribution(t *testing.T) {
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
	if len(plan.Expired) != 1 || plan.Expired[0] != snapshot.RunID {
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
	if len(done.Expired) != 1 || done.Expired[0] != snapshot.RunID {
		t.Fatalf("done = %#v", done)
	}
	reclaimed := callsString(backend.calls[before:])
	for _, expected := range []string{"remove-storage", "remove-stage"} {
		if !strings.Contains(reclaimed, expected) {
			t.Errorf("expiry did not %s:\n%s", expected, reclaimed)
		}
	}

	manifest, err := store.Get(snapshot.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.State != runstate.StateExpired {
		t.Fatalf("state = %q", manifest.State)
	}
	if manifest.ImportedBranch != "pisafe/gc-imported" || manifest.ImportedAt == nil {
		t.Fatalf("expired run lost its attribution: %#v", manifest)
	}
	// The workspace is gone, so reading it is refused rather than attempted.
	if _, err := controller.Diff(ctx, snapshot.RunID, testImage); err == nil ||
		!strings.Contains(err.Error(), "expired") {
		t.Fatalf("diff of an expired run = %v", err)
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

func TestCollectForgetsOldRecordsThatAttributeNothing(t *testing.T) {
	ctx := context.Background()
	_, store, controller, snapshot := importedRun(t, "gc-attributed")
	if _, err := controller.Discard(ctx, snapshot.RunID); err != nil {
		t.Fatal(err)
	}

	plain := gitstage.Snapshot{
		RunID:   "gc-plain",
		WorkRef: "refs/heads/work/gc-plain",
	}
	activeRun(t, store, plain)
	if _, err := controller.Discard(ctx, plain.RunID); err != nil {
		t.Fatal(err)
	}

	plan, err := controller.Plan(time.Now().UTC().Add(Retention + time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Forgotten) != 1 || plan.Forgotten[0] != plain.RunID {
		t.Fatalf("plan = %#v", plan)
	}
	done, err := controller.Collect(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(done.Forgotten) != 1 {
		t.Fatalf("done = %#v", done)
	}
	if _, err := store.Get(plain.RunID); err == nil {
		t.Fatal("a forgotten record survived")
	}
	// The branch this run produced still names it, a week after its
	// workspace was reclaimed.
	attributed, err := store.Get(snapshot.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if attributed.ImportedBranch != "pisafe/gc-attributed" {
		t.Fatalf("attributed = %#v", attributed)
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
	if _, _, err := controller.Apply(context.Background(), snapshot.RunID, testImage); err != nil {
		t.Fatal(err)
	}
	return backend, store, controller, snapshot
}
