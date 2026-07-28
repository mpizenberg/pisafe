package lima_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mpizenberg/pisafe/internal/lima"
	"github.com/mpizenberg/pisafe/internal/runcontainer"
)

// TestLiveProjectCacheIsSharedToReadAndPrivateToWrite runs two containers of
// one project at once. Both must start from the same dependency cache, and
// neither may see what the other does to it or change what the next run
// starts from.
func TestLiveProjectCacheIsSharedToReadAndPrivateToWrite(t *testing.T) {
	if os.Getenv("PISAFE_LIVE_LIMA") != "1" {
		t.Skip("set PISAFE_LIVE_LIMA=1 to test the dedicated VM")
	}
	imageID := os.Getenv("PISAFE_LIVE_RUN_IMAGE")
	if imageID == "" {
		t.Skip("set PISAFE_LIVE_RUN_IMAGE to an immutable sha256 image ID")
	}
	ensureLiveVM(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	stamp := time.Now().UTC().Format("20060102150405")
	projectKey := "livecache-" + stamp
	transport := lima.NewTransport()

	if err := transport.EnsureProjectStorage(ctx, projectKey); err != nil {
		t.Fatal(err)
	}
	defer runLive(
		t,
		context.Background(),
		"sudo", "/usr/local/sbin/pisafe-storage", "remove", "project", projectKey,
	)

	// Seeding stands in for promotion, which does not exist yet: the point
	// under test is what a run sees when the project already has a cache.
	cache := runcontainer.DefaultSpec("seed", projectKey, imageID).ProjectCachePath()
	runLive(t, ctx, "podman", "unshare", "sh", "-ec",
		"printf promoted > "+cache+"/shared.txt && chown -R 1000:1000 "+cache)

	specs := map[string]runcontainer.Spec{}
	for _, name := range []string{"a", "b"} {
		runID := "livecache-" + name + "-" + stamp
		spec := runcontainer.DefaultSpec(runID, projectKey, imageID)
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
		inCache(t, ctx, transport, spec, "printf %s > /cache/"+name+".txt", name)
	}
	for name, spec := range specs {
		if got := inCache(t, ctx, transport, spec, "cat /cache/shared.txt"); got != "promoted" {
			t.Errorf("run %s reads the project cache as %q", name, got)
		}
		listed := inCache(t, ctx, transport, spec, "ls /cache")
		if want := name + ".txt shared.txt"; strings.Join(strings.Fields(listed), " ") != want {
			t.Errorf("run %s sees %q, want %q", name, listed, want)
		}
	}

	// A run may not change what the next run of the project starts from.
	if got := runLive(t, ctx, "podman", "unshare", "ls", cache); got != "shared.txt" {
		t.Errorf("project cache = %q after two runs wrote to it", got)
	}
}

func inCache(
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
