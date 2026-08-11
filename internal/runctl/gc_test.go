package runctl

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mpizenberg/pisafe/internal/gitstage"
	"github.com/mpizenberg/pisafe/internal/runid"
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
	done, err := controller.Collect(ctx, plan, time.Now())
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
	if _, err := controller.Collect(ctx, plan, time.Now()); err != nil {
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

// TestASweepReclaimsAStoreOnlyAfterItsCheckoutStaysGone is the whole shape of
// the project sweep. A project filesystem holds transcripts nothing can
// reproduce, and the evidence that it is finished with — a path that no longer
// resolves — is exactly the evidence an unplugged disk produces, so one look is
// never enough to remove one.
func TestASweepReclaimsAStoreOnlyAfterItsCheckoutStaysGone(t *testing.T) {
	ctx := context.Background()
	store := runstate.NewStore(t.TempDir())
	backend := &fakeBackend{}
	controller := New(backend, store, &fakeSSHStore{}, testInference{})
	kept := registerCheckout(t, store, t.TempDir(), "kept")
	gone := registerCheckout(t, store, t.TempDir(), "gone")
	now := time.Now().UTC()

	// While both checkouts are there the sweep has nothing to say about either.
	plan, err := controller.Plan(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.MissingProjects) != 0 || len(plan.ReclaimedProjects) != 0 {
		t.Fatalf("plan = %#v", plan)
	}

	if err := os.RemoveAll(gone.Root); err != nil {
		t.Fatal(err)
	}
	plan, err = controller.Plan(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.MissingProjects) != 1 || plan.MissingProjects[0].Key != gone.Key {
		t.Fatalf("plan = %#v", plan)
	}
	if !plan.Empty() {
		t.Fatal("a checkout seen missing once was reclaimed")
	}
	done, err := controller.Collect(ctx, plan, now)
	if err != nil {
		t.Fatal(err)
	}
	if calls := callsString(backend.calls); strings.Contains(calls, "remove-project-storage") {
		t.Fatalf("the store was removed on first sight:\n%s", calls)
	}
	if len(done.MissingProjects) != 1 || done.MissingProjects[0].MissingSince == nil {
		t.Fatalf("done = %#v", done)
	}

	// The window is what the stamp starts, and it outlives the sweep that made
	// it: a later sweep inside it still leaves the store alone.
	plan, err = controller.Plan(now.Add(Retention - time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Empty() || len(plan.MissingProjects) != 1 {
		t.Fatalf("plan inside the window = %#v", plan)
	}

	plan, err = controller.Plan(now.Add(Retention + time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.ReclaimedProjects) != 1 || plan.ReclaimedProjects[0].Key != gone.Key {
		t.Fatalf("plan = %#v", plan)
	}
	if _, err := controller.Collect(ctx, plan, now.Add(Retention+time.Hour)); err != nil {
		t.Fatal(err)
	}
	last := backend.calls[len(backend.calls)-1]
	if last.kind != "remove-project-storage" || last.args[0] != gone.Key {
		t.Fatalf("last call = %#v", last)
	}
	records, err := store.ListProjects()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Key != kept.Key {
		t.Fatalf("records = %#v", records)
	}
}

// TestASweepLeavesAStoreWhoseCheckoutCameBack is the case the window exists
// for. The stamp is old enough to release the store, and the checkout being
// there again overrules it.
func TestASweepLeavesAStoreWhoseCheckoutCameBack(t *testing.T) {
	store := runstate.NewStore(t.TempDir())
	controller := New(&fakeBackend{}, store, &fakeSSHStore{}, testInference{})
	project := registerCheckout(t, store, t.TempDir(), "returning")
	long := time.Now().UTC().Add(-10 * Retention)
	if err := store.MarkProjectMissing(project.Key, long); err != nil {
		t.Fatal(err)
	}

	plan, err := controller.Plan(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Empty() || len(plan.MissingProjects) != 0 {
		t.Fatalf("plan = %#v", plan)
	}
}

// TestASweepLeavesAStoreARunStillRefersTo keeps the two sweeps from racing: a
// run's own storage stacks on the project's, and a run record is the only
// evidence that anything still needs it.
func TestASweepLeavesAStoreARunStillRefersTo(t *testing.T) {
	_, workspace, snapshot := applyFixture(t, "gc-project-held")
	store := runstate.NewStore(t.TempDir())
	controller := New(&fakeBackend{applyWorkspace: workspace}, store, &fakeSSHStore{}, testInference{})
	stoppedRun(t, store, snapshot)
	checkout := filepath.Join(t.TempDir(), testProject.Directory)
	if err := os.MkdirAll(checkout, 0o700); err != nil {
		t.Fatal(err)
	}
	// The run's manifest is filed under testProject.Key, so the record the
	// sweep reads has to be the one that key belongs to.
	if err := store.RegisterProject(testProject); err != nil {
		t.Fatal(err)
	}

	// testProject's checkout never existed, so the sweep would stamp it at once
	// were the run not there.
	plan, err := controller.Plan(time.Now().UTC().Add(10 * Retention))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.MissingProjects) != 0 || len(plan.ReclaimedProjects) != 0 {
		t.Fatalf("plan = %#v", plan)
	}
}

// TestARunNeverLeavesAStoreNothingCanAttribute is why registration comes first:
// a project filesystem whose checkout was never written down is one no sweep
// could ever recognise as unused.
func TestARunNeverLeavesAStoreNothingCanAttribute(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "projects"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	backend := &fakeBackend{}
	controller := New(backend, runstate.NewStore(root), &fakeSSHStore{}, testInference{})

	_, err := controller.StartPrepared(
		context.Background(),
		testPrepared(),
		testProject,
		testImage,
		testIdentity,
		nil,
	)
	if err == nil {
		t.Fatal("a run started without recording what its project store belongs to")
	}
	if calls := callsString(backend.calls); strings.Contains(calls, "ensure-project-storage") {
		t.Fatalf("the store was created anyway:\n%s", calls)
	}
}

// registerCheckout makes a checkout the sweep can find and records the project
// it keys, which is what one run of that project would have done.
func registerCheckout(
	t *testing.T,
	store runstate.Store,
	parent string,
	name string,
) runid.Project {
	t.Helper()
	checkout := filepath.Join(parent, name)
	if err := os.MkdirAll(checkout, 0o700); err != nil {
		t.Fatal(err)
	}
	project, err := runid.NewProject(checkout)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RegisterProject(project); err != nil {
		t.Fatal(err)
	}
	return project
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
	if _, _, err := controller.Apply(context.Background(), snapshot.RunID, testImage, gitstage.KeepBaseline, false); err != nil {
		t.Fatal(err)
	}
	return backend, store, controller, snapshot
}
