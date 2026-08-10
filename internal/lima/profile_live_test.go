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
func seedProfileExtension(t *testing.T, ctx context.Context, name, flag string) profile.Pin {
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
	return profile.Pin{
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
	path := runcontainer.ProfilePinsPath() + "/" + profile.RecordFile
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
	previous, err := lima.New().ReadProfileRecord(ctx)
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

// preserveProfileOffers puts back what the last check found when the test ends,
// so a test that asks npm a question does not leave its answer standing.
func preserveProfileOffers(t *testing.T, ctx context.Context) {
	t.Helper()
	previous, err := lima.New().ReadProfileOffers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if len(previous.Latest) == 0 {
			runLive(
				t, context.Background(), "podman", "unshare", "rm", "-f", "--",
				runcontainer.ProfilePinsPath()+"/"+profile.OffersFile,
			)
			return
		}
		if err := lima.New().WriteProfileOffers(context.Background(), previous); err != nil {
			t.Errorf("restore profile offers: %v", err)
		}
	})
}

// liveIsNumber is a published release that will never change: npm refuses to
// republish a version, so its integrity is a constant the test can assert
// against rather than merely record. liveIsNumberOld is an earlier release of
// the same package, which is what makes an update available without pisafe
// having to wait for one.
const (
	liveIsNumber          = "is-number@7.0.0"
	liveIsNumberOld       = "is-number@6.0.0"
	liveIsNumberIntegrity = "sha512-41Cifkg6e8TylSpdtTpeLVMqvSBEVzTttHvERD741" +
		"+pnZ8ANv0004MRL43QKPDlK9cGvNp6NZWZUBlbGXYxxng=="
)

// TestLiveAnAvailableUpdateIsOfferedAndNeverApplied is invariant 2's second
// half. Asking npm what an installed extension's name resolves to now must
// change nothing at all — not the pin, not the tree, not the mounted directory
// — and moving to what was offered must happen only when the user says so, and
// only through the same fetch-and-verify path an install takes.
func TestLiveAnAvailableUpdateIsOfferedAndNeverApplied(t *testing.T) {
	if os.Getenv("PISAFE_LIVE_LIMA") != "1" {
		t.Skip("set PISAFE_LIVE_LIMA=1 to test the dedicated VM")
	}
	imageID := liveImageID(t)
	ensureLiveVM(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	vm := lima.New()
	if err := vm.EnsureGlobalStorage(ctx); err != nil {
		t.Fatal(err)
	}
	seedProfileRecord(t, ctx, profile.Record{Version: profile.RecordVersion})
	preserveProfileOffers(t, ctx)

	superseded, err := vm.ResolvePackage(ctx, imageID, liveIsNumberOld)
	if err != nil {
		t.Fatal(err)
	}
	if superseded.Version != "6.0.0" {
		t.Fatalf("resolved %+v, want the exact version asked for", superseded)
	}
	if err := vm.InstallExtension(ctx, imageID, superseded); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := vm.RemoveExtension(context.Background(), superseded); err != nil {
			t.Errorf("remove live extension: %v", err)
		}
	})
	record := profile.Record{Version: profile.RecordVersion}.With(superseded)
	if err := vm.WriteProfileRecord(ctx, record); err != nil {
		t.Fatal(err)
	}

	checkedAt := time.Now()
	offers, err := vm.ResolveExtensionUpdates(ctx, imageID, record, checkedAt)
	if err != nil {
		t.Fatal(err)
	}
	if len(offers.Latest) != 1 || offers.Latest[0] != (profile.Offer{
		Name:    "is-number",
		Version: "7.0.0",
	}) {
		t.Fatalf("check found %+v", offers.Latest)
	}

	// The whole point: a check is a question, so the profile after it has to be
	// byte-for-byte the profile before it.
	installedManifest := runcontainer.ExtensionInstallRoot(superseded.Directory) +
		"/node_modules/is-number/package.json"
	if got := runLive(t, ctx, "podman", "unshare", "grep", "-c", `"version": "6.0.0"`, installedManifest); got != "1" {
		t.Errorf("the check changed the installed tree: %q", got)
	}
	unchanged, err := vm.ReadProfileRecord(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(unchanged.Extensions) != 1 || unchanged.Extensions[0] != superseded {
		t.Fatalf("the check changed the record: %+v", unchanged.Extensions)
	}
	if listed := runLive(
		t, ctx, "podman", "unshare", "ls", "-A", runcontainer.ProfileMount().Source,
	); listed != superseded.Directory {
		t.Errorf("the check left %q in the profile, want only %q", listed, superseded.Directory)
	}

	// What was found survives storage, because the offer is repeated to the user
	// long after the check that found it.
	if err := vm.WriteProfileOffers(ctx, offers); err != nil {
		t.Fatal(err)
	}
	stored, err := vm.ReadProfileOffers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.CheckedAt.Equal(checkedAt) {
		t.Errorf("checkedAt = %v, want %v", stored.CheckedAt, checkedAt)
	}
	pending := record.Pending(stored)
	if len(pending) != 1 || pending[0] != (profile.Update{
		Name:      "is-number",
		Installed: "6.0.0",
		Available: "7.0.0",
	}) {
		t.Fatalf("pending = %+v", pending)
	}

	// The end of a run speaks only when a check moved the answer, so the registry
	// answering the same thing twice has to leave nothing to say.
	again, err := vm.ResolveExtensionUpdates(ctx, imageID, record, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if spoke := record.PendingChange(stored, again); spoke != nil {
		t.Errorf("an unchanged check offered %+v again", spoke)
	}

	// Applying is an install: the offer names a version, and the bytes are still
	// checked against what the registry answers when they are fetched.
	resolved, err := vm.ResolvePackage(ctx, imageID, "is-number")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Version != "7.0.0" || resolved.Integrity != liveIsNumberIntegrity {
		t.Fatalf("resolved %+v", resolved)
	}
	if err := vm.InstallExtension(ctx, imageID, resolved); err != nil {
		t.Fatal(err)
	}
	updated := record.With(resolved)
	if err := vm.WriteProfileRecord(ctx, updated); err != nil {
		t.Fatal(err)
	}
	if got := runLive(t, ctx, "podman", "unshare", "grep", "-c", `"version": "7.0.0"`, installedManifest); got != "1" {
		t.Errorf("the applied update did not replace the tree: %q", got)
	}
	if pending := updated.Pending(stored); len(pending) != 0 {
		t.Errorf("an applied update is still offered: %+v", pending)
	}
}

// TestLiveAnInstalledExtensionIsPinnedToWhatWasFetched is what `pisafe
// extension install` is for. The version and hash pisafe records have to be the
// registry's own answer, the tree in the profile has to be that exact release,
// a hash that does not match has to stop the install, and a run has to find
// what was installed.
func TestLiveAnInstalledExtensionIsPinnedToWhatWasFetched(t *testing.T) {
	if os.Getenv("PISAFE_LIVE_LIMA") != "1" {
		t.Skip("set PISAFE_LIVE_LIMA=1 to test the dedicated VM")
	}
	imageID := liveImageID(t)
	ensureLiveVM(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	vm := lima.New()
	if err := vm.EnsureGlobalStorage(ctx); err != nil {
		t.Fatal(err)
	}
	seedProfileRecord(t, ctx, profile.Record{Version: profile.RecordVersion})

	extension, err := vm.ResolvePackage(ctx, imageID, liveIsNumber)
	if err != nil {
		t.Fatal(err)
	}
	if extension.Name != "is-number" || extension.Version != "7.0.0" ||
		extension.Integrity != liveIsNumberIntegrity {
		t.Fatalf("resolved %+v", extension)
	}

	// The pin is what the install is checked against, so bytes that hash to
	// anything else have to stay out of the profile.
	tampered := extension
	tampered.Integrity = "sha512-" + strings.Repeat("B", 86) + "=="
	if err := vm.InstallExtension(ctx, imageID, tampered); err == nil {
		t.Fatal("a package that did not match its pin was installed")
	}
	if listed := runLive(
		t, ctx, "podman", "unshare", "ls", "-A", runcontainer.ProfileMount().Source,
	); listed != "" {
		t.Fatalf("the profile holds %q after a refused install", listed)
	}

	if err := vm.InstallExtension(ctx, imageID, extension); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := vm.RemoveExtension(context.Background(), extension); err != nil {
			t.Errorf("remove live extension: %v", err)
		}
	})
	if err := vm.WriteProfileRecord(
		ctx,
		profile.Record{Version: profile.RecordVersion}.With(extension),
	); err != nil {
		t.Fatal(err)
	}
	installed := runcontainer.ExtensionInstallRoot(extension.Directory) +
		"/node_modules/is-number/package.json"
	if got := runLive(t, ctx, "podman", "unshare", "grep", "-c", `"version": "7.0.0"`, installed); got != "1" {
		t.Errorf("the installed tree is not the pinned release: %q", got)
	}

	// Installing again replaces rather than accumulates, and leaves no
	// half-installed directory behind for a run to mount.
	if err := vm.InstallExtension(ctx, imageID, extension); err != nil {
		t.Fatal(err)
	}
	if listed := runLive(
		t, ctx, "podman", "unshare", "ls", "-A", runcontainer.ProfileMount().Source,
	); listed != extension.Directory {
		t.Errorf("the profile holds %q, want only %q", listed, extension.Directory)
	}
	record, err := vm.ReadProfileRecord(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(record.Extensions) != 1 || record.Extensions[0] != extension {
		t.Fatalf("profile record = %+v", record.Extensions)
	}

	// A run started now finds the package where its settings say it is, which
	// is what makes an installed extension one Pi loads rather than one that
	// merely exists.
	stamp := time.Now().UTC().Format("20060102150405")
	projectKey := liveProject(t, vm, "liveinstall")
	spec := liveRun(t, ctx, vm, projectKey, imageID, "liveinstall-"+stamp)
	inContainer(t, ctx, vm, spec, "mkdir -p /work/project")
	configureRunProfile(t, ctx, vm, spec, record)
	listed := inContainer(t, ctx, vm, spec, "cd /work/project && pi list")
	path := runcontainer.ProfileMount().Destination + "/" +
		extension.Directory + "/node_modules/is-number"
	if strings.Count(listed, path) != 2 {
		t.Errorf("the run does not resolve the installed package:\n%s", listed)
	}
}

// configureRunProfile installs the run-side configuration the controller sends
// at every start.
func configureRunProfile(
	t *testing.T,
	ctx context.Context,
	vm lima.VM,
	spec runcontainer.Spec,
	record profile.Record,
) {
	t.Helper()
	configuration, err := json.Marshal(
		record.Configure(runcontainer.ProfileMount().Destination, "/work/project"),
	)
	if err != nil {
		t.Fatal(err)
	}
	args, err := spec.ConfigureProfileArgs()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := vm.Execute(
		ctx,
		bytes.NewReader(configuration),
		append([]string{"podman"}, args...)...,
	); err != nil {
		t.Fatal(err)
	}
}

// TestLiveTheProfileLoadsAndStaysReadOnlyToTheRun is invariant 1 end to end. An
// extension the user installed has to run in every run, and no code inside a
// run may change what the next one loads. A run installing a package for itself
// is not that: it writes its own home, and the profile is somewhere it has no
// writable handle on at all.
func TestLiveTheProfileLoadsAndStaysReadOnlyToTheRun(t *testing.T) {
	if os.Getenv("PISAFE_LIVE_LIMA") != "1" {
		t.Skip("set PISAFE_LIVE_LIMA=1 to test the dedicated VM")
	}
	imageID := liveImageID(t)
	ensureLiveVM(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	vm := lima.New()
	if err := vm.EnsureGlobalStorage(ctx); err != nil {
		t.Fatal(err)
	}
	installed := seedProfileExtension(t, ctx, "probe-ext", "probe-flag")
	seedProfileRecord(t, ctx, profile.Record{Version: profile.RecordVersion}.With(installed))

	record, err := vm.ReadProfileRecord(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(record.Extensions) != 1 || record.Extensions[0] != installed {
		t.Fatalf("profile record = %+v", record.Extensions)
	}

	stamp := time.Now().UTC().Format("20060102150405")
	projectKey := liveProject(t, vm, "liveprofile")
	spec := liveRun(t, ctx, vm, projectKey, imageID, "liveprofile-"+stamp)

	// A repository's own extension stands in for everything project trust
	// gates, and the file the run tries for one session stands in for pi -e.
	inContainer(t, ctx, vm, spec, `
mkdir -p /work/project/.pi/extensions
cat >/work/project/.pi/extensions/local.ts <<'TS'
export default function (pi) {
  pi.registerFlag("local-flag", { description: "from the repository", type: "boolean" });
}
TS
sed 's/local-flag/temporary-flag/' /work/project/.pi/extensions/local.ts >/tmp/try.ts
`)

	configureRunProfile(t, ctx, vm, spec, record)

	// A flag in the help is an extension module that was loaded and run, which
	// is what the profile is for. Pi needs no provider and no network to get
	// this far, so the assertion is about loading and nothing else.
	help := inContainer(t, ctx, vm, spec, "cd /work/project && pi --help")
	for _, expected := range []string{"--probe-flag", "--local-flag"} {
		if !strings.Contains(help, expected) {
			t.Errorf("pi --help lacks %s:\n%s", expected, help)
		}
	}
	tried := inContainer(t, ctx, vm, spec, "cd /work/project && pi -e /tmp/try.ts --help")
	if !strings.Contains(tried, "--temporary-flag") {
		t.Errorf("an extension tried for one run did not load:\n%s", tried)
	}

	// The profile itself refuses every write, from anywhere in the run.
	profileRoot := runcontainer.ProfileMount().Destination
	written := inContainer(t, ctx, vm, spec,
		"touch "+profileRoot+"/canary 2>&1 || true")
	if !strings.Contains(written, "Read-only file system") {
		t.Errorf("the profile accepted a write from the run: %q", written)
	}
	// Pi's own package store is the run's, so installing globally succeeds and
	// the package loads. An agent that cannot install globally installs into
	// the repository instead, which is the run's work rather than its tooling.
	installOutput := inContainer(t, ctx, vm, spec,
		"cd /work/project && pi install npm:is-number@7.0.0 2>&1")
	if strings.Contains(installOutput, "EROFS") ||
		strings.Contains(installOutput, "read-only") {
		t.Errorf("pi install was refused inside a run:\n%s", installOutput)
	}
	if listed := inContainer(
		t, ctx, vm, spec, "ls -A /home/node/.pi/agent/npm/node_modules",
	); !strings.Contains(listed, "is-number") {
		t.Errorf("a global install left nothing in the run's own store: %q", listed)
	}
	// What stopping reports: the run's own package, and not the profile's.
	selfInstalled := vm.ReadSelfInstalled(ctx, spec.RunID)
	if len(selfInstalled) != 1 || selfInstalled[0].Name != "is-number" {
		t.Errorf("the run's own packages read back as %#v", selfInstalled)
	}
	// Only the packages are mounted. What pisafe pinned them to is its own.
	if listed := inContainer(t, ctx, vm, spec, "ls -A "+profileRoot); listed != installed.Directory {
		t.Errorf("the run sees %q in the profile, want only %q", listed, installed.Directory)
	}
	// The point of all of it: what the run installed reached the run and
	// nothing else, so the next run's profile is the one the user installed.
	if listed := runLive(
		t, ctx, "podman", "unshare", "ls", "-A", runcontainer.ProfileMount().Source,
	); listed != installed.Directory {
		t.Errorf("the profile holds %q after a run, want only %q", listed, installed.Directory)
	}
}
