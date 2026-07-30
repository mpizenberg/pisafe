package lima_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mpizenberg/pisafe/internal/lima"
	"github.com/mpizenberg/pisafe/internal/profile"
	"github.com/mpizenberg/pisafe/internal/runcontainer"
	"github.com/mpizenberg/pisafe/internal/runid"
)

// seedProfileExtension installs one package the way the installer will, from
// the VM side and with nothing inside a container able to write it. The
// integrity is a placeholder: what a run loads follows from the record and the
// tree, and nothing re-derives the hash at run start.
func seedProfileExtension(t *testing.T, ctx context.Context, name, flag string) profile.Extension {
	t.Helper()
	directory, err := runid.NewPackageDirectory(name)
	if err != nil {
		t.Fatal(err)
	}
	root := runcontainer.ProfileMount().Source + "/" + directory
	module := root + "/node_modules/" + name
	runLive(t, ctx, "podman", "unshare", "sh", "-ec", `
set -eu
install -d "`+module+`/extensions"
cat >"`+module+`/package.json" <<'JSON'
{
  "name": "`+name+`",
  "version": "1.0.0",
  "keywords": ["pi-package"],
  "pi": { "extensions": ["./extensions"] }
}
JSON
cat >"`+module+`/extensions/flag.ts" <<'TS'
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

export default function (pi: ExtensionAPI) {
  pi.registerFlag("`+flag+`", {
    description: "registered by `+name+`",
    type: "boolean",
    default: false,
  });
}
TS
chown -R 1000:1000 "`+root+`"
chmod -R go-w "`+root+`"
`)
	t.Cleanup(func() {
		runLive(t, context.Background(), "podman", "unshare", "rm", "-rf", "--", root)
	})
	return profile.Extension{
		Name:      name,
		Version:   "1.0.0",
		Integrity: "sha512-" + strings.Repeat("A", 86) + "==",
		Directory: directory,
	}
}

// seedProfileRecord writes what the installer will record, and puts back what
// was installed before the test when it ends.
func seedProfileRecord(t *testing.T, ctx context.Context, record profile.Record) {
	t.Helper()
	path := runcontainer.ProfilePinsPath() + "/extensions.json"
	write := func(t *testing.T, ctx context.Context, record profile.Record) {
		content, err := record.Encode()
		if err != nil {
			t.Fatal(err)
		}
		runLive(t, ctx, "podman", "unshare", "sh", "-ec", `
set -eu
cat >"`+path+`" <<'JSON'
`+string(content)+`JSON
chown 1000:1000 "`+path+`"
`)
	}
	previous, err := lima.NewTransport().ReadProfileRecord(ctx)
	if err != nil {
		t.Fatal(err)
	}
	write(t, ctx, record)
	t.Cleanup(func() {
		if len(previous.Extensions) == 0 {
			runLive(t, context.Background(), "podman", "unshare", "rm", "-f", "--", path)
			return
		}
		write(t, context.Background(), previous)
	})
}

// TestLiveTheProfileLoadsAndStaysReadOnlyToTheRun is invariant 1 end to end. An
// extension the user installed has to run in every run, and no code inside a
// run may change what the next one loads — which is one property, because the
// profile is mounted exactly where Pi would have installed a package itself.
func TestLiveTheProfileLoadsAndStaysReadOnlyToTheRun(t *testing.T) {
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
	installed := seedProfileExtension(t, ctx, "probe-ext", "probe-flag")
	seedProfileRecord(t, ctx, profile.Record{Version: profile.RecordVersion}.With(installed))

	record, err := transport.ReadProfileRecord(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(record.Extensions) != 1 || record.Extensions[0] != installed {
		t.Fatalf("profile record = %+v", record.Extensions)
	}

	stamp := time.Now().UTC().Format("20060102150405")
	projectKey := liveProject(t, transport, "liveprofile")
	spec := liveRun(t, ctx, transport, projectKey, imageID, "liveprofile-"+stamp)

	// A repository's own extension stands in for everything project trust
	// gates, and the file the run tries for one session stands in for pi -e.
	inContainer(t, ctx, transport, spec, `
mkdir -p /work/project/.pi/extensions
cat >/work/project/.pi/extensions/local.ts <<'TS'
export default function (pi) {
  pi.registerFlag("local-flag", { description: "from the repository", type: "boolean" });
}
TS
sed 's/local-flag/temporary-flag/' /work/project/.pi/extensions/local.ts >/tmp/try.ts
`)

	configuration, err := json.Marshal(record.Configure("/work/project"))
	if err != nil {
		t.Fatal(err)
	}
	configureArgs, err := spec.ConfigureProfileArgs()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transport.Execute(
		ctx,
		bytes.NewReader(configuration),
		append([]string{"podman"}, configureArgs...)...,
	); err != nil {
		t.Fatal(err)
	}

	// A flag in the help is an extension module that was loaded and run, which
	// is what the profile is for. Pi needs no provider and no network to get
	// this far, so the assertion is about loading and nothing else.
	help := inContainer(t, ctx, transport, spec, "cd /work/project && pi --help")
	for _, expected := range []string{"--probe-flag", "--local-flag"} {
		if !strings.Contains(help, expected) {
			t.Errorf("pi --help lacks %s:\n%s", expected, help)
		}
	}
	tried := inContainer(t, ctx, transport, spec, "cd /work/project && pi -e /tmp/try.ts --help")
	if !strings.Contains(tried, "--temporary-flag") {
		t.Errorf("an extension tried for one run did not load:\n%s", tried)
	}

	// The profile arrives where Pi installs a global package, so refusing the
	// write and refusing the install are the same refusal.
	packageRoot := runcontainer.ProfileMount().Destination
	written := inContainer(t, ctx, transport, spec,
		"touch "+packageRoot+"/canary 2>&1 || true")
	if !strings.Contains(written, "Read-only file system") {
		t.Errorf("the profile accepted a write from the run: %q", written)
	}
	installOutput := inContainer(t, ctx, transport, spec,
		"pi install npm:is-number@7.0.0 2>&1 || echo REFUSED")
	if !strings.Contains(installOutput, "REFUSED") {
		t.Errorf("pi install succeeded inside a run:\n%s", installOutput)
	}
	// Only the packages are mounted. What pisafe pinned them to is its own.
	if listed := inContainer(t, ctx, transport, spec, "ls -A "+packageRoot); listed != installed.Directory {
		t.Errorf("the run sees %q in the profile, want only %q", listed, installed.Directory)
	}
	if listed := runLive(
		t, ctx, "podman", "unshare", "ls", "-A", runcontainer.ProfileMount().Source,
	); listed != installed.Directory {
		t.Errorf("the profile holds %q after a run, want only %q", listed, installed.Directory)
	}
}
