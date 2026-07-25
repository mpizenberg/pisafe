package runstart

import (
	"context"
	"errors"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mpizenberg/pisafe/internal/gitstage"
	"github.com/mpizenberg/pisafe/internal/runimage"
	"github.com/mpizenberg/pisafe/internal/runstate"
)

const testImageID = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

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
) (runimage.Result, error) {
	installer.artifacts = artifacts
	installer.called = true
	return runimage.Result{ImageID: testImageID}, nil
}

type fakeController struct {
	prepared gitstage.PreparedStage
	project  string
	imageID  string
}

func (controller *fakeController) StartPrepared(
	_ context.Context,
	prepared gitstage.PreparedStage,
	project string,
	imageID string,
) (runstate.Manifest, error) {
	controller.prepared = prepared
	controller.project = project
	controller.imageID = imageID
	return runstate.Manifest{
		RunID:     prepared.Snapshot.RunID,
		Project:   project,
		State:     runstate.StateActive,
		Workspace: "/work/" + project,
		SSH: &runstate.SSHConnection{
			Alias:      "pisafe-" + prepared.Snapshot.RunID,
			ConfigFile: "/state/ssh.config",
		},
	}, nil
}

func TestStartComposesBoundaryImageStageAndController(t *testing.T) {
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
		return "/Users/alice/My Project", nil
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
		if request.SourcePath != "/Users/alice/My Project" {
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
		controller.project != "my-project" ||
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
}

func TestStartReportsSelectedInputsAndDropsThemFromExclusions(t *testing.T) {
	boundary := &fakeBoundary{}
	installer := &fakeInstaller{}
	controller := &fakeController{}
	service := New(boundary, installer, controller, runimage.Artifacts{})
	service.newRunID = func(project string, _ time.Time) (string, error) {
		return project + "-run", nil
	}
	service.repositoryRoot = func(context.Context, string) (string, error) {
		return "/Users/alice/project", nil
	}
	service.listExcluded = func(context.Context, string) (gitstage.ExcludedInputs, error) {
		return gitstage.ExcludedInputs{
			Untracked: []string{"notes.txt", "keep-out.txt"},
			Ignored:   []string{"build/artifact.bin"},
		}, nil
	}
	var selected gitstage.InputSelection
	service.selectInputs = func(
		_ context.Context,
		_ string,
		selection gitstage.InputSelection,
	) ([]string, error) {
		selected = selection
		return []string{"build/artifact.bin", "notes.txt"}, nil
	}
	service.prepare = func(
		_ context.Context,
		request gitstage.PrepareRequest,
	) (gitstage.PreparedStage, error) {
		if len(request.Inputs.Include) != 1 {
			t.Fatalf("prepare inputs = %#v", request.Inputs)
		}
		return gitstage.PreparedStage{
			Snapshot: gitstage.Snapshot{RunID: request.RunID},
		}, nil
	}

	result, err := service.Start(
		context.Background(),
		".",
		nil,
		gitstage.InputSelection{Include: []string{"notes.txt"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(selected.Include, ",") != "notes.txt" {
		t.Fatalf("selection = %#v", selected)
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
	boundary := &fakeBoundary{}
	installer := &fakeInstaller{}
	service := New(boundary, installer, &fakeController{}, runimage.Artifacts{})
	service.newRunID = func(project string, _ time.Time) (string, error) {
		return project + "-run", nil
	}
	service.repositoryRoot = func(context.Context, string) (string, error) {
		return "/Users/alice/project", nil
	}
	service.listExcluded = func(context.Context, string) (gitstage.ExcludedInputs, error) {
		return gitstage.ExcludedInputs{}, nil
	}
	service.selectInputs = func(
		context.Context,
		string,
		gitstage.InputSelection,
	) ([]string, error) {
		return nil, errors.New("looks like a credential")
	}

	_, err := service.Start(
		context.Background(),
		".",
		nil,
		gitstage.InputSelection{Include: []string{".env"}},
	)
	if err == nil || !strings.Contains(err.Error(), "credential") {
		t.Fatalf("err = %v", err)
	}
	if boundary.called || installer.called {
		t.Fatal("a rejected selection still prepared the VM")
	}
}
