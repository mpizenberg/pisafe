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
		unfinished.RunID:         "no workspace to copy through",
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

// A live run is reachable from the Mac in one direction already; this is the
// other one, and it lands where a run standing in its workspace would find it.
func TestCopyInPutsAFileFromTheMacIntoTheRunsWorkspace(t *testing.T) {
	ctx := context.Background()
	_, workspace, snapshot := applyFixture(t, "copy-in-run")
	backend := &fakeBackend{applyWorkspace: workspace}
	store := runstate.NewStore(t.TempDir())
	controller := New(backend, store, &fakeSSHStore{}, testInference{})
	activeRun(t, store, snapshot)

	source := filepath.Join(t.TempDir(), "cf-analytics.json")
	writeFile(t, source, "{\"requests\":12}\n")

	entries, err := controller.CopyIn(ctx, CopyIntoRequest{
		RunID:   snapshot.RunID,
		ImageID: testImage,
		Source:  source,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Path != "cf-analytics.json" {
		t.Fatalf("entries = %#v", entries)
	}
	content, err := os.ReadFile(filepath.Join(workspace, "cf-analytics.json"))
	if err != nil || string(content) != "{\"requests\":12}\n" {
		t.Fatalf("content = %q, err = %v", content, err)
	}
	if joined := callsString(backend.calls); !strings.Contains(joined, "pisafe-guest import") {
		t.Fatalf("copy calls:\n%s", joined)
	}

	// A second copy of the same name is the user overwriting their own work,
	// which costs a flag here exactly as it does coming out.
	if _, err := controller.CopyIn(ctx, CopyIntoRequest{
		RunID:   snapshot.RunID,
		ImageID: testImage,
		Source:  source,
	}); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("err = %v", err)
	}
	writeFile(t, source, "{\"requests\":34}\n")
	if _, err := controller.CopyIn(ctx, CopyIntoRequest{
		RunID:   snapshot.RunID,
		ImageID: testImage,
		Source:  source,
		Replace: true,
	}); err != nil {
		t.Fatal(err)
	}
	content, err = os.ReadFile(filepath.Join(workspace, "cf-analytics.json"))
	if err != nil || string(content) != "{\"requests\":34}\n" {
		t.Fatalf("replaced content = %q, err = %v", content, err)
	}
}

func TestCopyInRefusesWhatItCannotSend(t *testing.T) {
	ctx := context.Background()
	_, workspace, snapshot := applyFixture(t, "guarded-copy-in-run")
	backend := &fakeBackend{applyWorkspace: workspace}
	store := runstate.NewStore(t.TempDir())
	controller := New(backend, store, &fakeSSHStore{}, testInference{})
	activeRun(t, store, snapshot)

	source := filepath.Join(t.TempDir(), "note.md")
	writeFile(t, source, "hello\n")

	for name, request := range map[string]CopyIntoRequest{
		"absent source":        {Source: filepath.Join(t.TempDir(), "absent")},
		"absolute destination": {Source: source, Destination: "/etc/cron.d/pisafe"},
		"climbing destination": {Source: source, Destination: "../../elsewhere"},
	} {
		request.RunID, request.ImageID = snapshot.RunID, testImage
		if _, err := controller.CopyIn(ctx, request); err == nil {
			t.Errorf("%s was copied in", name)
		}
	}
	if len(backend.calls) != 0 {
		t.Fatalf("a refused copy reached the run: %#v", backend.calls)
	}

	unfinished := gitstage.Snapshot{RunID: "unfinished-in-run", WorkRef: "refs/heads/work/unfinished"}
	creatingRun(t, store, unfinished)
	if _, err := controller.CopyIn(ctx, CopyIntoRequest{
		RunID:   unfinished.RunID,
		ImageID: testImage,
		Source:  source,
	}); err == nil || !strings.Contains(err.Error(), "no workspace to copy through") {
		t.Fatalf("err = %v", err)
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
