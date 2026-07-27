package runctl

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mpizenberg/pisafe/internal/gitstage"
	"github.com/mpizenberg/pisafe/internal/runstate"
)

func TestCopyOutBringsADirectoryToTheMac(t *testing.T) {
	ctx := context.Background()
	_, workspace, snapshot := applyFixture(t, "copy-run")
	if err := os.MkdirAll(filepath.Join(workspace, "dist"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(workspace, "dist", "index.html"), "<html>\n")

	backend := &fakeBackend{applyWorkspace: workspace}
	store := runstate.NewStore(t.TempDir())
	controller := New(backend, store, &fakeSSHStore{}, testInference{})
	activeRun(t, store, snapshot)

	destination := filepath.Join(t.TempDir(), "out")
	entries, err := controller.CopyOut(ctx, CopyRequest{
		RunID:       snapshot.RunID,
		ImageID:     testImage,
		Path:        "dist",
		Destination: destination,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %#v", entries)
	}
	content, err := os.ReadFile(filepath.Join(destination, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "<html>\n" {
		t.Fatalf("content = %q", content)
	}

	manifest, err := store.Get(snapshot.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.State != runstate.StateActive {
		t.Fatalf("copying changed the run's state to %q", manifest.State)
	}
	if joined := callsString(backend.calls); !strings.Contains(joined, "pisafe-guest export") {
		t.Fatalf("copy calls:\n%s", joined)
	}
}

// A copy the run cannot serve must fail loudly and leave nothing behind, not
// deliver a truncated directory.
func TestCopyOutLeavesNothingBehindWhenTheRunFails(t *testing.T) {
	ctx := context.Background()
	_, workspace, snapshot := applyFixture(t, "failed-copy-run")

	backend := &fakeBackend{applyWorkspace: workspace, failAt: "export"}
	store := runstate.NewStore(t.TempDir())
	controller := New(backend, store, &fakeSSHStore{}, testInference{})
	activeRun(t, store, snapshot)

	destination := filepath.Join(t.TempDir(), "out")
	if _, err := controller.CopyOut(ctx, CopyRequest{
		RunID:       snapshot.RunID,
		ImageID:     testImage,
		Path:        "missing",
		Destination: destination,
	}); err == nil || !strings.Contains(err.Error(), "copy out of run") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a failed copy left a destination behind")
	}
}

func TestCopyOutRefusesEscapingPathsAndRunsWithNoWorkspace(t *testing.T) {
	ctx := context.Background()
	_, workspace, snapshot := applyFixture(t, "guarded-copy-run")
	backend := &fakeBackend{applyWorkspace: workspace}
	store := runstate.NewStore(t.TempDir())
	controller := New(backend, store, &fakeSSHStore{}, testInference{})
	activeRun(t, store, snapshot)

	for name, requested := range map[string]string{
		"absolute": "/etc/passwd",
		"climbing": "../../../etc/passwd",
	} {
		if _, err := controller.CopyOut(ctx, CopyRequest{
			RunID:       snapshot.RunID,
			ImageID:     testImage,
			Path:        requested,
			Destination: filepath.Join(t.TempDir(), "out"),
		}); err == nil {
			t.Errorf("%s path was copied", name)
		}
	}
	if len(backend.calls) != 0 {
		t.Fatalf("an unsafe copy reached the run: %#v", backend.calls)
	}

	// A run whose creation never finished has nothing to copy out of, and a
	// reclaimed one has no record to name at all.
	unfinished := gitstage.Snapshot{RunID: "unfinished-copy-run", WorkRef: "refs/heads/work/unfinished"}
	creatingRun(t, store, unfinished)
	for runID, expected := range map[string]string{
		unfinished.RunID:         "no workspace to copy from",
		snapshot.RunID + "-gone": "does not exist",
	} {
		if _, err := controller.CopyOut(ctx, CopyRequest{
			RunID:       runID,
			ImageID:     testImage,
			Path:        "dist",
			Destination: filepath.Join(t.TempDir(), "out"),
		}); err == nil || !strings.Contains(err.Error(), expected) {
			t.Fatalf("copy out of %s = %v", runID, err)
		}
	}
}

// An occupied destination is the user's own mistake, so it is reported as such
// rather than as whatever the run's stream did when the copy stopped.
func TestCopyOutRefusesAnOccupiedDestinationBeforeStartingTheRun(t *testing.T) {
	ctx := context.Background()
	_, workspace, snapshot := applyFixture(t, "occupied-copy-run")

	backend := &fakeBackend{applyWorkspace: workspace}
	store := runstate.NewStore(t.TempDir())
	controller := New(backend, store, &fakeSSHStore{}, testInference{})
	activeRun(t, store, snapshot)

	destination := filepath.Join(t.TempDir(), "out")
	writeFile(t, destination, "mine\n")
	_, err := controller.CopyOut(ctx, CopyRequest{
		RunID:       snapshot.RunID,
		ImageID:     testImage,
		Path:        "dist",
		Destination: destination,
	})
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("err = %v", err)
	}
	if len(backend.calls) != 0 {
		t.Fatalf("a refused copy still started the run: %#v", backend.calls)
	}
	content, err := os.ReadFile(destination)
	if err != nil || string(content) != "mine\n" {
		t.Fatalf("destination = %q, err = %v", content, err)
	}
}
