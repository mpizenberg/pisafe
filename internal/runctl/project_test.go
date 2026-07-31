package runctl

import (
	"context"
	"strings"
	"testing"

	"github.com/mpizenberg/pisafe/internal/runid"
	"github.com/mpizenberg/pisafe/internal/runstate"
)

var movedProject = mustProject("/tmp/pisafe-test/project-renamed")

// recordedProject puts a store on the map without a run, which is the state
// every command in this file is about: the checkout has been worked in before
// and nothing is holding the store now.
func recordedProject(t *testing.T, store runstate.Store, project runid.Project) {
	t.Helper()
	if err := store.RegisterProject(project); err != nil {
		t.Fatal(err)
	}
}

// TestDroppingAStoreRefusesWhileARunStillBelongsToIt is the constraint eviction
// and cache reset already obey, widened to the whole store: overlayfs leaves
// behaviour undefined when a mounted lower goes away, and a stopped run
// remounts its lowers when it resumes.
func TestDroppingAStoreRefusesWhileARunStillBelongsToIt(t *testing.T) {
	store := runstate.NewStore(t.TempDir())
	backend := &fakeBackend{}
	recordedProject(t, store, testProject)
	activeManifest(t, store)

	err := New(backend, store, &fakeSSHStore{}, testInference{}).
		DropProject(context.Background(), testProject)
	if err == nil || !strings.Contains(err.Error(), "discard it first") {
		t.Fatalf("error = %v", err)
	}
	if calls := callsString(backend.calls); calls != "" {
		t.Fatalf("a store was disturbed under a live run:\n%s", calls)
	}
}

// TestDroppingAStoreTakesItsFilesystemBeforeItsRecord keeps every store
// attributable: the record is what says which checkout a one-way key came
// from, so a drop interrupted between the two leaves a record naming a
// filesystem that is gone — which the next attempt simply finds again — rather
// than a filesystem nothing can name.
func TestDroppingAStoreTakesItsFilesystemBeforeItsRecord(t *testing.T) {
	store := runstate.NewStore(t.TempDir())
	backend := &fakeBackend{}
	recordedProject(t, store, testProject)

	if err := New(backend, store, &fakeSSHStore{}, testInference{}).
		DropProject(context.Background(), testProject); err != nil {
		t.Fatal(err)
	}
	if calls := callsString(backend.calls); calls != "remove-project-storage "+testProject.Key {
		t.Fatalf("calls =\n%s", calls)
	}
	projects, err := store.ListProjects()
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 0 {
		t.Fatalf("projects = %#v", projects)
	}

	interrupted := runstate.NewStore(t.TempDir())
	recordedProject(t, interrupted, testProject)
	if err := New(&fakeBackend{failAt: "remove-project-storage"}, interrupted, &fakeSSHStore{}, testInference{}).
		DropProject(context.Background(), testProject); err == nil {
		t.Fatal("a filesystem that survived was reported as dropped")
	}
	stillNamed, err := interrupted.HasProject(testProject.Key)
	if err != nil {
		t.Fatal(err)
	}
	if !stillNamed {
		t.Fatal("the record went first, leaving a filesystem nothing can name")
	}
}

// TestDroppingRefusesAStoreNothingRecords stops a mistyped path from being
// reported as a removal that happened.
func TestDroppingRefusesAStoreNothingRecords(t *testing.T) {
	store := runstate.NewStore(t.TempDir())
	backend := &fakeBackend{}

	err := New(backend, store, &fakeSSHStore{}, testInference{}).
		DropProject(context.Background(), testProject)
	if err == nil || !strings.Contains(err.Error(), "no project store is recorded") {
		t.Fatalf("error = %v", err)
	}
	if calls := callsString(backend.calls); calls != "" {
		t.Fatalf("calls =\n%s", calls)
	}
}

// TestRebindingCarriesTheTranscriptsAndLeavesTheCaches is the whole of what a
// moved repository gets back. The caches are a full copy of whatever it last
// fetched and losing one costs time only, which is the assumption every other
// decision about them rests on; a transcript is the part nothing reproduces.
func TestRebindingCarriesTheTranscriptsAndLeavesTheCaches(t *testing.T) {
	store := runstate.NewStore(t.TempDir())
	backend := &fakeBackend{}
	recordedProject(t, store, testProject)

	if err := New(backend, store, &fakeSSHStore{}, testInference{}).
		RebindProject(context.Background(), testProject, movedProject); err != nil {
		t.Fatal(err)
	}
	calls := callsString(backend.calls)
	// The destination filesystem exists before anything is copied into it, and
	// the source is released only once the copy is done.
	want := "ensure-project-storage\n" +
		"adopt-sessions " + movedProject.Key + " " + testProject.Key + "\n" +
		"remove-project-storage " + testProject.Key
	if calls != want {
		t.Fatalf("calls =\n%s\nwant:\n%s", calls, want)
	}
	if strings.Contains(calls, "cache") {
		t.Fatalf("a cache was carried over a rebind:\n%s", calls)
	}

	projects, err := store.ListProjects()
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 || projects[0].Root != movedProject.Root {
		t.Fatalf("projects = %#v", projects)
	}
}

// TestRebindingRecordsTheNewCheckoutBeforeAllocatingItsStore keeps the
// invariant a project key's one-wayness depends on: a filesystem that exists
// before anything says where it came from could never afterwards be recognised
// as unused, so a rebind that dies at the allocation leaves a nameable store.
func TestRebindingRecordsTheNewCheckoutBeforeAllocatingItsStore(t *testing.T) {
	store := runstate.NewStore(t.TempDir())
	backend := &fakeBackend{failAt: "ensure-project-storage"}
	recordedProject(t, store, testProject)

	err := New(backend, store, &fakeSSHStore{}, testInference{}).
		RebindProject(context.Background(), testProject, movedProject)
	if err == nil {
		t.Fatal("a failed allocation was reported as a rebind")
	}
	recorded, err := store.HasProject(movedProject.Key)
	if err != nil {
		t.Fatal(err)
	}
	if !recorded {
		t.Fatal("the destination store was left unattributable")
	}
	// The source is untouched, so its transcripts are still reachable.
	held, err := store.HasProject(testProject.Key)
	if err != nil {
		t.Fatal(err)
	}
	if !held {
		t.Fatal("the source store was released before its transcripts were carried")
	}
}

// TestRebindingRefusesADestinationThatAlreadyHasAStore keeps two projects'
// histories apart. An interrupted rebind and two genuine checkouts are
// indistinguishable from here, and merging them silently is the worse mistake
// of the two, so the refusal names the way out.
func TestRebindingRefusesADestinationThatAlreadyHasAStore(t *testing.T) {
	store := runstate.NewStore(t.TempDir())
	backend := &fakeBackend{}
	recordedProject(t, store, testProject)
	recordedProject(t, store, movedProject)

	err := New(backend, store, &fakeSSHStore{}, testInference{}).
		RebindProject(context.Background(), testProject, movedProject)
	if err == nil || !strings.Contains(err.Error(), "drop it first") {
		t.Fatalf("error = %v", err)
	}
	if calls := callsString(backend.calls); calls != "" {
		t.Fatalf("two histories were merged:\n%s", calls)
	}
}

// TestRebindingRefusesWhileEitherEndHasARun covers the destination as well as
// the source: a run of the checkout being rebound onto has the destination
// store's session layer mounted, and rebinding writes into it.
func TestRebindingRefusesWhileEitherEndHasARun(t *testing.T) {
	store := runstate.NewStore(t.TempDir())
	backend := &fakeBackend{}
	recordedProject(t, store, testProject)
	activeManifest(t, store)

	// activeManifest belongs to testProject, so this is the destination-side
	// case with the two ends swapped.
	err := New(backend, store, &fakeSSHStore{}, testInference{}).
		RebindProject(context.Background(), movedProject, testProject)
	if err == nil || !strings.Contains(err.Error(), "discard it first") {
		t.Fatalf("error = %v", err)
	}
	if calls := callsString(backend.calls); calls != "" {
		t.Fatalf("a store was disturbed under a live run:\n%s", calls)
	}
}
