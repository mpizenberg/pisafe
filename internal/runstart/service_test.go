package runstart

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mpizenberg/pisafe/internal/gitstage"
	"github.com/mpizenberg/pisafe/internal/projectconfig"
	"github.com/mpizenberg/pisafe/internal/runcontainer"
	"github.com/mpizenberg/pisafe/internal/runid"
	"github.com/mpizenberg/pisafe/internal/runimage"
	"github.com/mpizenberg/pisafe/internal/runstate"
)

const testImageID = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

var testIdentity = gitstage.Identity{Name: "Alice", Email: "alice@example.invalid"}

type fakeBoundary struct {
	prefixes []netip.Prefix
	called   bool
}

func (boundary *fakeBoundary) Ensure(_ context.Context, prefixes []netip.Prefix) error {
	boundary.prefixes = append([]netip.Prefix(nil), prefixes...)
	boundary.called = true
	return nil
}

type fakeInstaller struct {
	artifacts runimage.Artifacts
	called    bool
}

func (installer *fakeInstaller) Ensure(
	_ context.Context,
	artifacts runimage.Artifacts,
) (string, error) {
	installer.artifacts = artifacts
	installer.called = true
	return testImageID, nil
}

type fakeController struct {
	prepared gitstage.PreparedStage
	project  runid.Project
	imageID  string
	identity gitstage.Identity
	caches   []runcontainer.CacheMount
}

func (controller *fakeController) StartPrepared(
	_ context.Context,
	prepared gitstage.PreparedStage,
	project runid.Project,
	imageID string,
	identity gitstage.Identity,
	caches []runcontainer.CacheMount,
) (runstate.Manifest, error) {
	controller.prepared = prepared
	controller.project = project
	controller.imageID = imageID
	controller.identity = identity
	controller.caches = caches
	return runstate.Manifest{
		RunID:      prepared.Snapshot.RunID,
		Project:    project.Directory,
		ProjectKey: project.Key,
		State:      runstate.StateActive,
		SSH: &runstate.SSHConnection{
			Alias:      "pisafe-" + prepared.Snapshot.RunID,
			ConfigFile: "/state/ssh.config",
		},
	}, nil
}

// testRoot creates a real repository root, because the configuration a
// repository declares for itself is read from the checkout. The path is
// resolved as gitstage resolves one, so a selected input under it is recognized
// as being inside the repository.
func testRoot(t *testing.T, name string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func TestStartComposesBoundaryImageStageAndController(t *testing.T) {
	root := testRoot(t, "My Project")
	boundary := &fakeBoundary{}
	installer := &fakeInstaller{}
	controller := &fakeController{}
	artifacts := runimage.Artifacts{
		Containerfile: []byte("container"),
		Guest:         []byte("guest"),
	}
	service := New(boundary, installer, controller, artifacts)
	service.now = func() time.Time {
		return time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	}
	service.newRunID = func(project string, _ time.Time) (string, error) {
		return project + "-run", nil
	}
	service.repositoryRoot = func(context.Context, string) (string, error) {
		return root, nil
	}
	service.resolveIdentity = func(context.Context, string) (gitstage.Identity, error) {
		return testIdentity, nil
	}
	service.listExcluded = func(context.Context, string) (gitstage.ExcludedInputs, error) {
		return gitstage.ExcludedInputs{
			Untracked: []string{"local.txt"},
			Ignored:   []string{"build/output"},
		}, nil
	}
	service.prepare = func(
		_ context.Context,
		request gitstage.PrepareRequest,
	) (gitstage.PreparedStage, error) {
		if request.SourcePath != root {
			t.Fatalf("root = %q", request.SourcePath)
		}
		if filepath.Base(request.PackageDir) != "package" {
			t.Fatalf("package path = %q", request.PackageDir)
		}
		return gitstage.PreparedStage{
			Snapshot: gitstage.Snapshot{
				RunID:      request.RunID,
				SourceRoot: request.SourcePath,
			},
		}, nil
	}
	prefixes := []netip.Prefix{netip.MustParsePrefix("192.168.1.0/24")}

	result, err := service.Start(
		context.Background(),
		".",
		prefixes,
		gitstage.InputSelection{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Manifest.RunID != "my-project-run" ||
		controller.project.Directory != "my-project" ||
		!strings.HasPrefix(controller.project.Key, "my-project-") ||
		controller.imageID != testImageID {
		t.Fatalf("result = %#v, controller = %#v", result, controller)
	}
	if len(boundary.prefixes) != 1 || boundary.prefixes[0] != prefixes[0] {
		t.Fatalf("prefixes = %#v", boundary.prefixes)
	}
	if string(installer.artifacts.Guest) != "guest" {
		t.Fatalf("artifacts = %#v", installer.artifacts)
	}
	if controller.prepared.Snapshot.RunID != "my-project-run" {
		t.Fatalf("prepared = %#v", controller.prepared)
	}
	if strings.Join(result.Excluded.Untracked, ",") != "local.txt" ||
		strings.Join(result.Excluded.Ignored, ",") != "build/output" {
		t.Fatalf("excluded = %#v", result.Excluded)
	}
	if controller.identity != testIdentity {
		t.Fatalf("identity = %#v", controller.identity)
	}
}

func TestStartRefusesAnUnattributableRunBeforeTouchingTheBoundary(t *testing.T) {
	boundary := &fakeBoundary{}
	installer := &fakeInstaller{}
	service := New(boundary, installer, &fakeController{}, runimage.Artifacts{})
	service.repositoryRoot = func(context.Context, string) (string, error) {
		return testRoot(t, "project"), nil
	}
	service.resolveIdentity = func(context.Context, string) (gitstage.Identity, error) {
		return gitstage.Identity{}, gitstage.ErrNoIdentity
	}

	_, err := service.Start(context.Background(), ".", nil, gitstage.InputSelection{})
	if !errors.Is(err, gitstage.ErrNoIdentity) {
		t.Fatalf("err = %v", err)
	}
	if boundary.called || installer.called {
		t.Fatal("a run nobody could commit in still prepared the VM")
	}
}

func TestStartReportsSelectedInputsAndDropsThemFromExclusions(t *testing.T) {
	root := testRoot(t, "project")
	if err := os.MkdirAll(filepath.Join(root, "build"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"notes.txt", "keep-out.txt", "build/artifact.bin"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("content\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	boundary := &fakeBoundary{}
	installer := &fakeInstaller{}
	controller := &fakeController{}
	service := New(boundary, installer, controller, runimage.Artifacts{})
	service.newRunID = func(project string, _ time.Time) (string, error) {
		return project + "-run", nil
	}
	service.repositoryRoot = func(context.Context, string) (string, error) {
		return root, nil
	}
	service.resolveIdentity = func(context.Context, string) (gitstage.Identity, error) {
		return testIdentity, nil
	}
	service.listExcluded = func(context.Context, string) (gitstage.ExcludedInputs, error) {
		return gitstage.ExcludedInputs{
			Root:      root,
			Untracked: []string{"notes.txt", "keep-out.txt"},
			Ignored:   []string{"build/"},
		}, nil
	}
	// The stage archives what the report names, so the fake echoes back what it
	// was handed rather than inventing a second list.
	service.prepare = func(
		_ context.Context,
		request gitstage.PrepareRequest,
	) (gitstage.PreparedStage, error) {
		snapshot := gitstage.Snapshot{RunID: request.RunID}
		for _, input := range request.Inputs {
			snapshot.Inputs = append(snapshot.Inputs, input.Path)
		}
		return gitstage.PreparedStage{Snapshot: snapshot}, nil
	}

	result, err := service.Start(
		context.Background(),
		".",
		nil,
		gitstage.InputSelection{Include: []string{
			filepath.Join(root, "notes.txt"),
			filepath.Join(root, "build"),
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(result.Included, ",") != "build/artifact.bin,notes.txt" {
		t.Fatalf("included = %#v", result.Included)
	}
	if strings.Join(result.Excluded.Untracked, ",") != "keep-out.txt" ||
		len(result.Excluded.Ignored) != 0 {
		t.Fatalf("excluded = %#v", result.Excluded)
	}
}

func TestStartRejectsBadSelectionBeforeTouchingTheBoundary(t *testing.T) {
	root := testRoot(t, "project")
	boundary := &fakeBoundary{}
	installer := &fakeInstaller{}
	service := New(boundary, installer, &fakeController{}, runimage.Artifacts{})
	service.newRunID = func(project string, _ time.Time) (string, error) {
		return project + "-run", nil
	}
	service.repositoryRoot = func(context.Context, string) (string, error) {
		return root, nil
	}
	service.resolveIdentity = func(context.Context, string) (gitstage.Identity, error) {
		return testIdentity, nil
	}
	service.listExcluded = func(context.Context, string) (gitstage.ExcludedInputs, error) {
		return gitstage.ExcludedInputs{Root: root, Untracked: []string{".env"}}, nil
	}

	_, err := service.Start(
		context.Background(),
		".",
		nil,
		gitstage.InputSelection{Include: []string{filepath.Join(root, ".env")}},
	)
	if err == nil || !strings.Contains(err.Error(), "credential") {
		t.Fatalf("err = %v", err)
	}
	if boundary.called || installer.called {
		t.Fatal("a rejected selection still prepared the VM")
	}
}

// TestStartCarriesDeclaredCachesToTheController checks the one path by which a
// repository can influence what a run mounts, end to end on the Mac side.
func TestStartCarriesDeclaredCachesToTheController(t *testing.T) {
	root := testRoot(t, "project")
	if err := os.MkdirAll(filepath.Join(root, ".config"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, projectconfig.RelativePath),
		[]byte(`{"caches":[{"name":"npm","env":["npm_config_cache"],"key":["package-lock.json"]}]}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "package-lock.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	controller := &fakeController{}
	service := New(&fakeBoundary{}, &fakeInstaller{}, controller, runimage.Artifacts{})
	service.repositoryRoot = func(context.Context, string) (string, error) { return root, nil }
	service.resolveIdentity = func(context.Context, string) (gitstage.Identity, error) {
		return testIdentity, nil
	}
	service.listExcluded = func(context.Context, string) (gitstage.ExcludedInputs, error) {
		return gitstage.ExcludedInputs{}, nil
	}
	service.prepare = func(
		_ context.Context,
		request gitstage.PrepareRequest,
	) (gitstage.PreparedStage, error) {
		return gitstage.PreparedStage{
			Snapshot: gitstage.Snapshot{RunID: request.RunID},
		}, nil
	}

	if _, err := service.Start(
		context.Background(),
		".",
		nil,
		gitstage.InputSelection{},
	); err != nil {
		t.Fatal(err)
	}
	if len(controller.caches) != 1 {
		t.Fatalf("caches = %#v", controller.caches)
	}
	cache := controller.caches[0]
	if cache.Name != "npm" || strings.Join(cache.Env, ",") != "npm_config_cache" {
		t.Fatalf("cache = %#v", cache)
	}
	// The selection is still the VM's to make; what reaches the controller is
	// the key this run's inputs hash to and nothing resolved yet.
	if cache.Snapshot != "" {
		t.Fatalf("snapshot was resolved on the Mac: %#v", cache)
	}
	if err := cache.Validate(); err != nil {
		t.Fatal(err)
	}
}

// TestStartRefusesAMalformedDeclarationBeforeTouchingTheBoundary keeps a
// hostile or broken config from costing VM and image work.
func TestStartRefusesAMalformedDeclarationBeforeTouchingTheBoundary(t *testing.T) {
	root := testRoot(t, "project")
	if err := os.MkdirAll(filepath.Join(root, ".config"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, projectconfig.RelativePath),
		[]byte(`{"caches":[{"name":"npm","env":["HOME"],"key":[]}]}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	boundary := &fakeBoundary{}
	installer := &fakeInstaller{}
	service := New(boundary, installer, &fakeController{}, runimage.Artifacts{})
	service.repositoryRoot = func(context.Context, string) (string, error) { return root, nil }
	service.resolveIdentity = func(context.Context, string) (gitstage.Identity, error) {
		return testIdentity, nil
	}

	if _, err := service.Start(context.Background(), ".", nil, gitstage.InputSelection{}); err == nil {
		t.Fatal("a cache rebinding HOME was accepted")
	}
	if boundary.called || installer.called {
		t.Fatal("a malformed declaration still prepared the VM")
	}
}
