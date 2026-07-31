package runctl

import (
	"context"
	"strings"
	"testing"

	"github.com/mpizenberg/pisafe/internal/profile"
	"github.com/mpizenberg/pisafe/internal/runcontainer"
	"github.com/mpizenberg/pisafe/internal/runstate"
)

func testProfile() profile.Record {
	return profile.Record{Version: profile.RecordVersion}.With(profile.Pin{
		Name:      "@earendil-works/plan-mode",
		Version:   "1.2.3",
		Integrity: "sha512-" + strings.Repeat("A", 86) + "==",
		Directory: "earendil-works-plan-mode-bf0f2759",
	})
}

// TestARunIsToldWhatTheProfileHoldsWhenItStarts covers both starts a run has.
// The profile is shared and changes between them, and what a run was told about
// it does not survive one, so it is read and installed at each.
func TestARunIsToldWhatTheProfileHoldsWhenItStarts(t *testing.T) {
	store := runstate.NewStore(t.TempDir())
	backend := &fakeBackend{profile: testProfile()}
	controller := New(backend, store, &fakeSSHStore{}, testInference{})

	manifest, err := controller.StartPrepared(
		context.Background(),
		testPrepared(),
		testProject,
		testImage,
		testIdentity,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	wantPackage := "/home/node/.pi/agent/npm/earendil-works-plan-mode-bf0f2759" +
		"/node_modules/@earendil-works/plan-mode"
	stdin := stdinFor(backend.calls, "configure-profile")
	if !strings.Contains(stdin, wantPackage) {
		t.Fatalf("run was not told about the installed extension: %q", stdin)
	}
	if !strings.Contains(stdin, `"workspace":"/work/project"`) {
		t.Fatalf("run was not told which workspace is its own: %q", stdin)
	}
	if !strings.Contains(callsString(backend.calls), "ensure-global-storage") {
		t.Fatalf("the profile filesystem was not ensured:\n%s", callsString(backend.calls))
	}

	if _, err := controller.Stop(context.Background(), manifest.RunID); err != nil {
		t.Fatal(err)
	}
	backend.calls = nil
	backend.profile = profile.Record{Version: profile.RecordVersion}
	if _, err := controller.Resume(context.Background(), manifest.RunID); err != nil {
		t.Fatal(err)
	}
	resumedStdin := stdinFor(backend.calls, "configure-profile")
	if resumedStdin == "" || strings.Contains(resumedStdin, wantPackage) {
		t.Fatalf("resumed run still loads an uninstalled extension: %q", resumedStdin)
	}
}

// TestAProfileTheControllerCannotVouchForStopsTheRun is what keeps a corrupt
// record from silently becoming a run with no extensions: the profile is what
// the user installed deliberately, so a run that cannot read it fails.
func TestAProfileTheControllerCannotVouchForStopsTheRun(t *testing.T) {
	store := runstate.NewStore(t.TempDir())
	backend := &fakeBackend{failAt: "read-profile"}
	controller := New(backend, store, &fakeSSHStore{}, testInference{})

	if _, err := controller.StartPrepared(
		context.Background(),
		testPrepared(),
		testProject,
		testImage,
		testIdentity,
		nil,
	); err == nil {
		t.Fatal("a run started without knowing what the profile holds")
	}
	record, err := store.Get("run-123")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(record.LastError, "read profile failed") {
		t.Fatalf("last error = %q", record.LastError)
	}
}

// TestAWritableProfileIsNotAcceptedAsARunsProfile is invariant 1 checked
// against the running container rather than assumed from the arguments pisafe
// passed: a writable profile is agent code able to change what every later run
// of every project loads.
func TestAWritableProfileIsNotAcceptedAsARunsProfile(t *testing.T) {
	spec := runcontainer.DefaultSpec("run-123", testProject.Key, testImage)
	runArgs, err := spec.RunArgs()
	if err != nil {
		t.Fatal(err)
	}
	inspection := inspectionFromRunArgs(
		append([]string{"podman"}, runArgs...),
		spec.ContainerName(),
	)
	if err := validateContainerInspection(spec, *inspection); err != nil {
		t.Fatalf("a correctly mounted run was refused: %v", err)
	}
	for index := range inspection.Mounts {
		if inspection.Mounts[index].Destination != runcontainer.ProfileMount().Destination {
			continue
		}
		inspection.Mounts[index].Writable = true
	}
	if err := validateContainerInspection(spec, *inspection); err == nil {
		t.Fatal("a writable profile was accepted")
	}
}
