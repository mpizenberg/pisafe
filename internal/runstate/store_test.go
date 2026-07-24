package runstate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mpizenberg/pisafe/internal/gitstage"
)

func TestStoreLifecycleAndList(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	store := NewStore(root)
	store.now = func() time.Time { return now }

	created, err := store.Create(testManifest("run-one"))
	if err != nil {
		t.Fatal(err)
	}
	if created.State != StateCreating || !created.CreatedAt.Equal(now) {
		t.Fatalf("created = %#v", created)
	}
	now = now.Add(time.Minute)
	active, err := store.Transition("run-one", StateActive)
	if err != nil {
		t.Fatal(err)
	}
	if active.State != StateActive || !active.UpdatedAt.Equal(now) {
		t.Fatalf("active = %#v", active)
	}
	now = now.Add(time.Minute)
	stopped, err := store.Transition("run-one", StateStopped)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.StoppedAt == nil || !stopped.StoppedAt.Equal(now) {
		t.Fatalf("stopped = %#v", stopped)
	}

	now = now.Add(time.Minute)
	if _, err := store.Create(testManifest("run-two")); err != nil {
		t.Fatal(err)
	}
	runs, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].RunID != "run-two" || runs[1].RunID != "run-one" {
		t.Fatalf("runs = %#v", runs)
	}

	info, err := os.Stat(filepath.Join(root, "run-one.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("manifest mode = %#o", got)
	}
}

func TestStoreRejectsInvalidTransitions(t *testing.T) {
	store := NewStore(t.TempDir())
	if _, err := store.Create(testManifest("run-one")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Transition("run-one", StateDiscarded); err == nil ||
		!strings.Contains(err.Error(), "invalid run transition") {
		t.Fatalf("error = %v", err)
	}
	if _, err := store.Transition("../escape", StateActive); err == nil {
		t.Fatal("unsafe run ID was accepted")
	}
}

func TestStoreRejectsDuplicateAndCorruptManifest(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	if _, err := store.Create(testManifest("run-one")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(testManifest("run-one")); err == nil {
		t.Fatal("duplicate run was accepted")
	}
	if err := os.WriteFile(
		filepath.Join(root, "bad.json"),
		[]byte(`{"version":1,"run_id":"other"}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.List(); err == nil || !strings.Contains(err.Error(), "identity mismatch") {
		t.Fatalf("error = %v", err)
	}
}

func TestStoreRecordsAndClearsOperationError(t *testing.T) {
	store := NewStore(t.TempDir())
	if _, err := store.Create(testManifest("run-one")); err != nil {
		t.Fatal(err)
	}
	failed, err := store.RecordError("run-one", os.ErrPermission)
	if err != nil {
		t.Fatal(err)
	}
	if failed.State != StateCreating || failed.LastError == "" {
		t.Fatalf("failed = %#v", failed)
	}
	active, err := store.Transition("run-one", StateActive)
	if err != nil {
		t.Fatal(err)
	}
	if active.LastError != "" {
		t.Fatalf("active retained error %q", active.LastError)
	}
}

func testManifest(runID string) Manifest {
	return Manifest{
		RunID:   runID,
		Project: "project",
		Snapshot: gitstage.Snapshot{
			RunID:   runID,
			WorkRef: "refs/heads/work/" + runID,
		},
	}
}
