package runstart

import (
	"context"
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
}

func (boundary *fakeBoundary) Ensure(_ context.Context, prefixes []netip.Prefix) error {
	boundary.prefixes = append([]netip.Prefix(nil), prefixes...)
	return nil
}

type fakeInstaller struct {
	artifacts runimage.Artifacts
}

func (installer *fakeInstaller) Ensure(
	_ context.Context,
	artifacts runimage.Artifacts,
) (runimage.Result, error) {
	installer.artifacts = artifacts
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
		root string,
		packagePath string,
		runID string,
	) (gitstage.PreparedStage, error) {
		if root != "/Users/alice/My Project" {
			t.Fatalf("root = %q", root)
		}
		if filepath.Base(packagePath) != "package" {
			t.Fatalf("package path = %q", packagePath)
		}
		return gitstage.PreparedStage{
			Snapshot: gitstage.Snapshot{
				RunID:      runID,
				SourceRoot: root,
			},
		}, nil
	}
	prefixes := []netip.Prefix{netip.MustParsePrefix("192.168.1.0/24")}

	result, err := service.Start(context.Background(), ".", prefixes)
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
