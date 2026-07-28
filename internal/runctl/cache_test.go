package runctl

import (
	"context"
	"strings"
	"testing"

	"github.com/mpizenberg/pisafe/internal/runcontainer"
	"github.com/mpizenberg/pisafe/internal/runstate"
)

var (
	testNPMCache = runcontainer.CacheMount{
		Name:     "npm",
		Env:      []string{"npm_config_cache"},
		Key:      "0123456789abcdef",
		Snapshot: "fedcba9876543210",
	}
	testCargoCache = runcontainer.CacheMount{
		Name: "cargo",
		Env:  []string{"CARGO_HOME"},
		Key:  "1111111111111111",
	}
)

// stoppedWithCaches drives one run of a project all the way to stopped, which
// is where a run's caches are published.
func stoppedWithCaches(
	t *testing.T,
	store runstate.Store,
	backend *fakeBackend,
	caches ...runcontainer.CacheMount,
) runstate.Manifest {
	t.Helper()
	manifest := activeManifest(t, store, caches...)
	spec := specForManifest(manifest, manifest.Image)
	runArgs, err := spec.RunArgs()
	if err != nil {
		t.Fatal(err)
	}
	backend.container = inspectionFromRunArgs(
		append([]string{"podman"}, runArgs...),
		spec.ContainerName(),
	)
	stopped, err := New(backend, store, &fakeSSHStore{}, testInference{}).
		Stop(context.Background(), manifest.RunID)
	if err != nil {
		t.Fatal(err)
	}
	return stopped
}

// TestStoppingPublishesAndTrimsEveryDeclaredCache is the whole point of the
// restore path built before it: until a run's work becomes a generation, every
// namespace stays empty and the overlays are per-run directories.
func TestStoppingPublishesAndTrimsEveryDeclaredCache(t *testing.T) {
	store := runstate.NewStore(t.TempDir())
	backend := &fakeBackend{}
	stopped := stoppedWithCaches(t, store, backend, testNPMCache, testCargoCache)
	if stopped.LastError != "" {
		t.Fatalf("stop recorded %q", stopped.LastError)
	}

	published := map[string]string{}
	evicted := map[string]string{}
	for _, call := range backend.calls {
		switch call.kind {
		case "publish-snapshot":
			published[call.args[1]] = call.args[2]
		case "evict-snapshots":
			evicted[call.args[1]] = strings.Join(call.args[2:], " ")
		}
	}
	// A generation is named by what this run's inputs hashed to, never by what
	// it happened to restore, or a changed lockfile would overwrite the state
	// it was restored from.
	if published["npm"] != testNPMCache.Key || published["cargo"] != testCargoCache.Key {
		t.Fatalf("published = %#v", published)
	}
	// The run is stopped, not gone, so it may still resume onto the generation
	// it recorded, and eviction has to leave that one alone on top of the
	// newest it keeps anyway.
	if evicted["npm"] != "1 fedcba9876543210" {
		t.Errorf("npm eviction = %q", evicted["npm"])
	}
	// A run that restored nothing holds no generation, so nothing is protected.
	if evicted["cargo"] != "1" {
		t.Errorf("cargo eviction = %q", evicted["cargo"])
	}
}

func TestARunDeclaringNoCachePublishesNothing(t *testing.T) {
	store := runstate.NewStore(t.TempDir())
	backend := &fakeBackend{}
	stoppedWithCaches(t, store, backend)
	if joined := callsString(backend.calls); strings.Contains(joined, "publish-snapshot") ||
		strings.Contains(joined, "evict-snapshots") {
		t.Fatalf("a run sharing nothing still touched the project store:\n%s", joined)
	}
}

// TestAFailedPublishIsRecordedRatherThanFailingTheStop keeps the cache
// disposable in the code as well as in the design: the run stopped, its
// workspace is intact, and the only cost is that a later run fetches again.
func TestAFailedPublishIsRecordedRatherThanFailingTheStop(t *testing.T) {
	store := runstate.NewStore(t.TempDir())
	backend := &fakeBackend{failAt: "publish-snapshot"}
	stopped := stoppedWithCaches(t, store, backend, testNPMCache)
	if stopped.State != runstate.StateStopped {
		t.Fatalf("stopped = %#v", stopped)
	}
	if !strings.Contains(stopped.LastError, "publish run caches") {
		t.Fatalf("last error = %q", stopped.LastError)
	}
	if strings.Contains(callsString(backend.calls), "evict-snapshots") {
		t.Error("a namespace was trimmed after nothing was published to it")
	}
}

// TestResetRefusesWhileARunCouldStillMountAGeneration is the same constraint
// eviction obeys: overlayfs leaves behaviour undefined when a mounted lower
// disappears, and a stopped run remounts its lower when it resumes.
func TestResetRefusesWhileARunCouldStillMountAGeneration(t *testing.T) {
	store := runstate.NewStore(t.TempDir())
	backend := &fakeBackend{}
	stoppedWithCaches(t, store, backend, testNPMCache)
	controller := New(backend, store, &fakeSSHStore{}, testInference{})

	err := controller.ResetProjectCache(context.Background(), testProject)
	if err == nil || !strings.Contains(err.Error(), "discard it first") {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(callsString(backend.calls), "reset-project-cache") {
		t.Fatal("the cache was emptied under a run that can still mount it")
	}
}

func TestResetEmptiesACacheNoRunIsHolding(t *testing.T) {
	store := runstate.NewStore(t.TempDir())
	backend := &fakeBackend{}
	// A run that restored nothing never mounted a published generation, so it
	// is no reason to refuse.
	stoppedWithCaches(t, store, backend, testCargoCache)
	controller := New(backend, store, &fakeSSHStore{}, testInference{})

	if err := controller.ResetProjectCache(context.Background(), testProject); err != nil {
		t.Fatal(err)
	}
	last := backend.calls[len(backend.calls)-1]
	if last.kind != "reset-project-cache" || last.args[0] != testProject.Key {
		t.Fatalf("last call = %#v", last)
	}
}
