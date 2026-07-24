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
	active, err := store.Activate("run-one", testSSHConnection(root, "run-one"), "", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if active.State != StateActive || !active.UpdatedAt.Equal(now) {
		t.Fatalf("active = %#v", active)
	}
	now = now.Add(time.Minute)
	stopped, err := store.Stop("run-one", now)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.StoppedAt == nil || !stopped.StoppedAt.Equal(now) {
		t.Fatalf("stopped = %#v", stopped)
	}
	if stopped.ActiveElapsedSeconds != 60 {
		t.Fatalf("active elapsed = %d", stopped.ActiveElapsedSeconds)
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
	root := t.TempDir()
	store := NewStore(root)
	if _, err := store.Create(testManifest("run-one")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Activate(
		"run-one",
		testSSHConnection(root, "run-one"),
		"",
		time.Time{},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Discard("run-one"); err == nil ||
		!strings.Contains(err.Error(), "invalid run transition") {
		t.Fatalf("error = %v", err)
	}
	if _, err := store.Resume("run-one", time.Now()); err == nil {
		t.Fatal("active run resumed")
	}
	if _, err := store.Resume("../escape", time.Now()); err == nil {
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
		[]byte(`{"version":3,"run_id":"other"}`),
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
	active, err := store.Activate(
		"run-one",
		testSSHConnection(store.root, "run-one"),
		"",
		time.Time{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if active.LastError != "" {
		t.Fatalf("active retained error %q", active.LastError)
	}
}

func TestStoreActivatesWithRunScopedSSHConnection(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	if _, err := store.Create(testManifest("run-one")); err != nil {
		t.Fatal(err)
	}
	connection := testSSHConnection(root, "run-one")
	active, err := store.Activate(
		"run-one",
		connection,
		strings.Repeat("a", 40),
		time.Time{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if active.State != StateActive || active.SSH == nil || *active.SSH != connection {
		t.Fatalf("active = %#v", active)
	}
	if active.Snapshot.BaselineCommit != strings.Repeat("a", 40) {
		t.Fatalf("baseline = %q", active.Snapshot.BaselineCommit)
	}
	stored, err := store.Get("run-one")
	if err != nil {
		t.Fatal(err)
	}
	if stored.SSH == nil || *stored.SSH != connection {
		t.Fatalf("stored = %#v", stored)
	}
}

func TestStoreRejectsMismatchedSSHConnection(t *testing.T) {
	store := NewStore(t.TempDir())
	if _, err := store.Create(testManifest("run-one")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Activate("run-one", SSHConnection{
		Alias: "pisafe-other",
	}, "", time.Time{}); err == nil {
		t.Fatal("mismatched SSH connection was accepted")
	}
}

func TestStoreRejectsInvalidMaterializedBaseline(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	if _, err := store.Create(testManifest("run-one")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Activate(
		"run-one",
		testSSHConnection(root, "run-one"),
		"not-a-git-object",
		time.Time{},
	); err == nil {
		t.Fatal("invalid baseline commit was accepted")
	}
}

func TestStoreAccountsCumulativeActiveWallClock(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	store := NewStore(root)
	store.now = func() time.Time { return now }
	if _, err := store.Create(testManifest("run-one")); err != nil {
		t.Fatal(err)
	}
	active, err := store.Activate("run-one", testSSHConnection(root, "run-one"), "", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if active.ActiveDeadline == nil ||
		!active.ActiveDeadline.Equal(now.Add(8*time.Hour)) {
		t.Fatalf("active deadline = %v", active.ActiveDeadline)
	}
	now = now.Add(90*time.Minute + 500*time.Millisecond)
	stopped, err := store.Stop("run-one", now)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.ActiveElapsedSeconds != 5401 {
		t.Fatalf("active elapsed = %d", stopped.ActiveElapsedSeconds)
	}
	now = now.Add(24 * time.Hour)
	resumed, err := store.Resume("run-one", now)
	if err != nil {
		t.Fatal(err)
	}
	if got := RemainingSeconds(resumed, now); got != 28800-5401 {
		t.Fatalf("remaining = %d", got)
	}
}

func testManifest(runID string) Manifest {
	return Manifest{
		RunID:              runID,
		Project:            "project",
		ActiveLimitSeconds: 8 * 60 * 60,
		Snapshot: gitstage.Snapshot{
			RunID:   runID,
			WorkRef: "refs/heads/work/" + runID,
		},
	}
}

func testSSHConnection(root string, runID string) SSHConnection {
	return SSHConnection{
		Alias:              "pisafe-" + runID,
		IdentityFile:       filepath.Join(root, "id_ed25519"),
		KnownHostsFile:     filepath.Join(root, "known_hosts"),
		ConfigFile:         filepath.Join(root, "ssh.config"),
		HostKeyFingerprint: "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	}
}
