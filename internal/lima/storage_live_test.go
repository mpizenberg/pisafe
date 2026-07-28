package lima_test

import (
	"context"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/mpizenberg/pisafe/internal/lima"
	"github.com/mpizenberg/pisafe/internal/runcontainer"
)

// liveCacheKeys are what a declared cache's inputs would hash to. They are
// fixed here because this test seeds the project store directly rather than
// running the tool that would fill it.
const (
	liveOlderKey   = "1111111111111111"
	liveNewerKey   = "2222222222222222"
	liveMissingKey = "3333333333333333"
)

func liveCache(key string) runcontainer.CacheMount {
	return runcontainer.CacheMount{
		Name: "npm",
		Env:  []string{"npm_config_cache"},
		Key:  key,
	}
}

func liveProject(t *testing.T, transport lima.Transport, name string) string {
	t.Helper()
	projectKey := name + "-" + time.Now().UTC().Format("20060102150405")
	if err := transport.EnsureProjectStorage(context.Background(), projectKey); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		runLive(
			t,
			context.Background(),
			"sudo", "/usr/local/sbin/pisafe-storage", "remove", "project", projectKey,
		)
	})
	return projectKey
}

// seedSnapshot publishes one generation the way slice 4 will, with a fixed
// modification time so recency is decided by the test rather than by how fast
// it runs.
func seedSnapshot(t *testing.T, ctx context.Context, namespace, key, content, stamp string) {
	t.Helper()
	directory := namespace + "/" + key
	runLive(t, ctx, "podman", "unshare", "sh", "-ec",
		"install -d -m 0700 -o 1000 -g 1000 "+namespace+" "+directory+
			" && printf %s "+content+" > "+directory+"/shared.txt"+
			" && chown 1000:1000 "+directory+"/shared.txt"+
			" && touch -d "+stamp+" "+directory)
}

// TestLiveCacheSnapshotsAreSelectedByKeyThenRecency covers the whole point of
// keying a namespace: an exact match restores what this run's inputs produced
// before, and a miss restores the previous generation rather than nothing, so
// a changed lockfile costs a delta instead of a cold fetch.
func TestLiveCacheSnapshotsAreSelectedByKeyThenRecency(t *testing.T) {
	if os.Getenv("PISAFE_LIVE_LIMA") != "1" {
		t.Skip("set PISAFE_LIVE_LIMA=1 to test the dedicated VM")
	}
	ensureLiveVM(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	transport := lima.NewTransport()
	projectKey := liveProject(t, transport, "liveselect")
	namespace := runcontainer.
		DefaultSpec("seed", projectKey, liveImageID(t)).
		CacheNamespacePath("npm")

	// An empty namespace is the first run of a project, and must select
	// nothing rather than fail.
	selected, err := transport.SelectCacheSnapshots(ctx, projectKey, []runcontainer.CacheMount{
		liveCache(liveMissingKey),
	})
	if err != nil {
		t.Fatal(err)
	}
	if selected[0].Snapshot != "" {
		t.Errorf("an unpublished namespace selected %q", selected[0].Snapshot)
	}

	seedSnapshot(t, ctx, namespace, liveOlderKey, "older", "@1000000000")
	seedSnapshot(t, ctx, namespace, liveNewerKey, "newer", "@2000000000")

	for name, request := range map[string]struct{ key, want string }{
		"exact match on the older generation": {liveOlderKey, liveOlderKey},
		"exact match on the newer generation": {liveNewerKey, liveNewerKey},
		"no match falls back to the newest":   {liveMissingKey, liveNewerKey},
	} {
		selected, err := transport.SelectCacheSnapshots(ctx, projectKey, []runcontainer.CacheMount{
			liveCache(request.key),
		})
		if err != nil {
			t.Fatal(err)
		}
		if selected[0].Snapshot != request.want {
			t.Errorf("%s: selected %q, want %q", name, selected[0].Snapshot, request.want)
		}
	}
}

// TestLiveProjectLayersAreSharedToReadAndPrivateToWrite runs two containers of
// one project at once. Both must start from the same shared state, and neither
// may see what the other does to it or change what the next run starts from.
func TestLiveProjectLayersAreSharedToReadAndPrivateToWrite(t *testing.T) {
	if os.Getenv("PISAFE_LIVE_LIMA") != "1" {
		t.Skip("set PISAFE_LIVE_LIMA=1 to test the dedicated VM")
	}
	imageID := liveImageID(t)
	ensureLiveVM(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	stamp := time.Now().UTC().Format("20060102150405")
	transport := lima.NewTransport()
	projectKey := liveProject(t, transport, "livelayers")

	// Seeding stands in for publishing, which does not exist yet: the point
	// under test is what a run sees when the project already has state.
	seed := runcontainer.DefaultSpec("seed", projectKey, imageID)
	seedSnapshot(t, ctx, seed.CacheNamespacePath("npm"), liveNewerKey, "promoted", "@2000000000")
	runLive(t, ctx, "podman", "unshare", "sh", "-ec",
		"printf promoted > "+seed.ProjectPath()+"/sessions/shared.txt"+
			" && chown 1000:1000 "+seed.ProjectPath()+"/sessions/shared.txt")

	specs := map[string]runcontainer.Spec{}
	for _, name := range []string{"a", "b"} {
		runID := "livelayers-" + name + "-" + stamp
		spec := runcontainer.DefaultSpec(runID, projectKey, imageID)
		selected, err := transport.SelectCacheSnapshots(
			ctx,
			projectKey,
			[]runcontainer.CacheMount{liveCache(liveNewerKey)},
		)
		if err != nil {
			t.Fatal(err)
		}
		spec.Caches = selected
		specs[name] = spec
		if err := transport.CreateRunStorage(ctx, runID); err != nil {
			t.Fatal(err)
		}
		defer func() {
			runLive(t, context.Background(), "podman", "rm", "--force", "--ignore", spec.ContainerName())
			if err := transport.RemoveRunStorage(context.Background(), runID); err != nil {
				t.Errorf("remove live run storage: %v", err)
			}
		}()
		if err := transport.PrepareRunOverlays(ctx, runID, spec.Caches); err != nil {
			t.Fatal(err)
		}
		runArgs, err := spec.RunArgs()
		if err != nil {
			t.Fatal(err)
		}
		// Every mount and limit of a real run is kept; only the SSH server it
		// would start is replaced, because this test drives the container
		// directly rather than over SSH.
		runArgs = append(runArgs[:len(runArgs)-2:len(runArgs)-2], "sleep", "infinity")
		if _, err := transport.Execute(ctx, nil, append([]string{"podman"}, runArgs...)...); err != nil {
			t.Fatal(err)
		}
	}

	// Both runs are live throughout: each writes before either reads, so a
	// leak between them would be visible rather than merely possible.
	for name, spec := range specs {
		for _, overlay := range spec.ProjectOverlays() {
			inContainer(t, ctx, transport, spec,
				"printf %s > "+path.Join(overlay.Destination, name+".txt"), name)
		}
	}
	for name, spec := range specs {
		for _, overlay := range spec.ProjectOverlays() {
			shared := inContainer(t, ctx, transport, spec,
				"cat "+path.Join(overlay.Destination, "shared.txt"))
			if shared != "promoted" {
				t.Errorf("run %s reads %s as %q", name, overlay.Destination, shared)
			}
			listed := inContainer(t, ctx, transport, spec, "ls "+overlay.Destination)
			if want := name + ".txt shared.txt"; strings.Join(strings.Fields(listed), " ") != want {
				t.Errorf("run %s sees %s as %q, want %q", name, overlay.Destination, listed, want)
			}
		}
	}

	// A snapshot is immutable and the session store is shared, so neither may
	// carry a live run's writes into the next run of the project.
	for _, lower := range []string{
		seed.CacheNamespacePath("npm") + "/" + liveNewerKey,
		seed.ProjectPath() + "/sessions",
	} {
		if got := runLive(t, ctx, "podman", "unshare", "ls", lower); got != "shared.txt" {
			t.Errorf("project layer %s = %q after two runs wrote to it", lower, got)
		}
	}
}

func liveImageID(t *testing.T) string {
	t.Helper()
	imageID := os.Getenv("PISAFE_LIVE_RUN_IMAGE")
	if imageID == "" {
		t.Skip("set PISAFE_LIVE_RUN_IMAGE to an immutable sha256 image ID")
	}
	return imageID
}

func inContainer(
	t *testing.T,
	ctx context.Context,
	transport lima.Transport,
	spec runcontainer.Spec,
	script string,
	arguments ...string,
) string {
	t.Helper()
	command := append(
		[]string{"podman", "exec", "--user", "1000:1000", spec.ContainerName(), "sh", "-ec", script},
		arguments...,
	)
	output, err := transport.Execute(ctx, nil, command...)
	if err != nil {
		t.Fatalf("%s: %v\n%s", strings.Join(command, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}
