package lima_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mpizenberg/pisafe/internal/lima"
	"github.com/mpizenberg/pisafe/internal/profile"
	"github.com/mpizenberg/pisafe/internal/runcontainer"
)

// liveCommitlint is a published release that will never change, chosen because
// its dependency tree carries commands of its own: npm links a binary for every
// package in the tree, and only one of them is the tool the user asked for.
const liveCommitlint = "@commitlint/cli@20.1.0"

// seedProfileTools puts back what was installed before the test when it ends.
func seedProfileTools(t *testing.T, ctx context.Context, tools profile.Tools) {
	t.Helper()
	transport := lima.NewTransport()
	previous, err := transport.ReadProfileTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	write := func(ctx context.Context, tools profile.Tools) {
		if err := transport.LinkToolBinaries(ctx, tools); err != nil {
			t.Errorf("link the profile's commands: %v", err)
			return
		}
		if err := transport.WriteProfileTools(ctx, tools); err != nil {
			t.Errorf("write the profile's tool record: %v", err)
		}
	}
	write(ctx, tools)
	t.Cleanup(func() { write(context.Background(), previous) })
}

// TestLiveAnInstalledToolIsOnEveryRunsPathAndNeverWritable is what `pisafe tool
// install` is for. A run cannot obtain a binary for itself at all, so what the
// user installed has to be reachable by name; and because it is reachable by
// name in every later run, no run may be able to change it. The third property
// is the one a package chooses rather than the user: installing one command
// must not quietly put its dependencies' commands on the PATH too.
func TestLiveAnInstalledToolIsOnEveryRunsPathAndNeverWritable(t *testing.T) {
	if os.Getenv("PISAFE_LIVE_LIMA") != "1" {
		t.Skip("set PISAFE_LIVE_LIMA=1 to test the dedicated VM")
	}
	imageID := liveImageID(t)
	ensureLiveVM(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	transport := lima.NewTransport()
	if err := transport.EnsureGlobalStorage(ctx); err != nil {
		t.Fatal(err)
	}
	seedProfileTools(t, ctx, profile.Tools{Version: profile.ToolsVersion})

	pin, err := transport.ResolvePackage(ctx, imageID, liveCommitlint)
	if err != nil {
		t.Fatal(err)
	}
	if pin.Name != "@commitlint/cli" || pin.Version != "20.1.0" {
		t.Fatalf("resolved %+v", pin)
	}
	if err := transport.InstallTool(ctx, imageID, pin); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := transport.RemoveTool(context.Background(), pin); err != nil {
			t.Errorf("remove live tool: %v", err)
		}
	})

	// The tree holds a link for every package in it that ships a command. Only
	// the one the user named is the tool's to claim.
	binaries, err := transport.ToolBinaries(ctx, pin)
	if err != nil {
		t.Fatal(err)
	}
	if len(binaries) != 1 || binaries[0] != "commitlint" {
		t.Fatalf("the tool claims %v", binaries)
	}
	installed := profile.Tools{Version: profile.ToolsVersion}.
		With(profile.Tool{Pin: pin, Binaries: binaries})
	if err := transport.LinkToolBinaries(ctx, installed); err != nil {
		t.Fatal(err)
	}
	if err := transport.WriteProfileTools(ctx, installed); err != nil {
		t.Fatal(err)
	}
	if listed := runLive(
		t, ctx, "podman", "unshare", "ls", "-A", runcontainer.ToolBinPath(),
	); listed != "commitlint" {
		t.Errorf("the searched directory holds %q, want only commitlint", listed)
	}

	stamp := time.Now().UTC().Format("20060102150405")
	projectKey := liveProject(t, transport, "livetool")
	spec := liveRun(t, ctx, transport, projectKey, imageID, "livetool-"+stamp)

	resolved := inContainer(t, ctx, transport, spec, "command -v commitlint")
	if resolved != runcontainer.ToolsMount().Destination+"/bin/commitlint" {
		t.Errorf("commitlint resolves to %q", resolved)
	}
	if version := inContainer(
		t, ctx, transport, spec, "commitlint --version",
	); !strings.Contains(version, "20.1.0") {
		t.Errorf("the installed command did not run: %q", version)
	}
	// A dependency's command was linked in the tool's own module root and must
	// not have reached the directory a run searches.
	for _, dependency := range []string{"js-yaml", "semver", "jiti"} {
		found := inContainer(t, ctx, transport, spec,
			"command -v "+dependency+" || echo MISSING")
		if !strings.Contains(found, "MISSING") {
			t.Errorf("a dependency's command is on the run's PATH: %s -> %q", dependency, found)
		}
	}

	// Invariant 1 for tools: what a run executes is not something a run can
	// change, whether by replacing a link or by adding one.
	toolRoot := runcontainer.ToolsMount().Destination
	for _, attempt := range []string{
		"touch " + toolRoot + "/canary",
		"touch " + toolRoot + "/bin/canary",
		"rm -f " + toolRoot + "/bin/commitlint",
		"ln -sf /bin/true " + toolRoot + "/bin/commitlint",
	} {
		refused := inContainer(t, ctx, transport, spec, attempt+" 2>&1 || true")
		if !strings.Contains(refused, "Read-only file system") {
			t.Errorf("the run was allowed to %s: %q", attempt, refused)
		}
	}

	// Removing rebuilds the directory rather than editing it, so what a tool
	// claimed stops being reachable even in a run that is already live.
	empty := profile.Tools{Version: profile.ToolsVersion}
	if err := transport.LinkToolBinaries(ctx, empty); err != nil {
		t.Fatal(err)
	}
	if err := transport.WriteProfileTools(ctx, empty); err != nil {
		t.Fatal(err)
	}
	if found := inContainer(
		t, ctx, transport, spec, "command -v commitlint || echo GONE",
	); !strings.Contains(found, "GONE") {
		t.Errorf("a removed tool is still reachable: %q", found)
	}
	if listed := runLive(
		t, ctx, "podman", "unshare", "ls", "-A", runcontainer.ToolBinPath(),
	); listed != "" {
		t.Errorf("removing a tool left %q behind", listed)
	}
}
