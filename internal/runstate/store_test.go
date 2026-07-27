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
	active, err := store.Activate("run-one", testSSHConnection(root, "run-one"), "", testCapability(), time.Time{})
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
		testCapability(),
		time.Time{},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Discard("run-one"); err == nil ||
		!strings.Contains(err.Error(), "invalid run transition") {
		t.Fatalf("error = %v", err)
	}
	if _, err := store.Resume("run-one", testCapability(), time.Now()); err == nil {
		t.Fatal("active run resumed")
	}
	if _, err := store.Resume("../escape", testCapability(), time.Now()); err == nil {
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
		[]byte(`{"version":4,"run_id":"other"}`),
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
		testCapability(),
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
		testCapability(),
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
	}, "", testCapability(), time.Time{}); err == nil {
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
		testCapability(),
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
	active, err := store.Activate("run-one", testSSHConnection(root, "run-one"), "", testCapability(), time.Time{})
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
	resumed, err := store.Resume("run-one", testCapability(), now)
	if err != nil {
		t.Fatal(err)
	}
	if got := RemainingSeconds(resumed, now); got != 28800-5401 {
		t.Fatalf("remaining = %d", got)
	}
}

func TestStoreIssuesRotatesAndRevokesInferenceCapability(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	if _, err := store.Create(testManifest("run-one")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Activate(
		"run-one",
		testSSHConnection(root, "run-one"),
		"",
		"not-a-capability",
		time.Time{},
	); err == nil {
		t.Fatal("invalid inference capability was accepted")
	}
	first := testCapability()
	active, err := store.Activate(
		"run-one",
		testSSHConnection(root, "run-one"),
		"",
		first,
		time.Time{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if active.InferenceCapability != first {
		t.Fatalf("active capability = %q", active.InferenceCapability)
	}
	stopped, err := store.Stop("run-one", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if stopped.InferenceCapability != "" {
		t.Fatal("stopped run retained its inference capability")
	}
	if _, err := store.Resume("run-one", "", time.Now()); err == nil {
		t.Fatal("resume without a fresh capability was accepted")
	}
	second := "pisafe-cap-" + strings.Repeat("cd", 32)
	resumed, err := store.Resume("run-one", second, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if resumed.InferenceCapability != second {
		t.Fatalf("resumed capability = %q", resumed.InferenceCapability)
	}
	if _, err := store.Stop("run-one", time.Time{}); err != nil {
		t.Fatal(err)
	}
	discarded, err := store.Discard("run-one")
	if err != nil {
		t.Fatal(err)
	}
	if discarded.InferenceCapability != "" {
		t.Fatal("discarded run retained its inference capability")
	}
}

func TestNewInferenceCapabilityIsValidAndUnique(t *testing.T) {
	first, err := NewInferenceCapability()
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewInferenceCapability()
	if err != nil {
		t.Fatal(err)
	}
	if !ValidInferenceCapability(first) || !ValidInferenceCapability(second) {
		t.Fatalf("generated capabilities are invalid: %q %q", first, second)
	}
	if first == second {
		t.Fatal("generated capabilities are not unique")
	}
	if ValidInferenceCapability("pisafe-cap-XYZ") {
		t.Fatal("malformed capability validated")
	}
}

func TestStoreRecordsApplyPlanUntilEveryRefIsImported(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	stopped := stoppedTestRun(t, store, root, "run-apply")
	planned := testApplyPlan("run-apply", root)

	// An apply plan only exists once the run is stopped.
	if _, err := store.CompleteApply("run-apply"); err == nil ||
		!strings.Contains(err.Error(), "no apply in progress") {
		t.Fatalf("premature completion error = %v", err)
	}
	recorded, err := store.BeginApply("run-apply", planned)
	if err != nil {
		t.Fatal(err)
	}
	if recorded.State != StateStopped || recorded.Apply == nil {
		t.Fatalf("recorded = %#v", recorded)
	}
	if _, err := store.BeginApply("run-apply", planned); err == nil ||
		!strings.Contains(err.Error(), "already has an apply in progress") {
		t.Fatalf("second plan error = %v", err)
	}

	// The recorded plan survives a fresh read, which is what makes an
	// interrupted apply replayable.
	reread, err := store.Get("run-apply")
	if err != nil {
		t.Fatal(err)
	}
	if reread.Apply == nil || len(reread.Apply.Journal.Steps) != 1 ||
		reread.Apply.Journal.Steps[0].Commit != planned.Journal.Steps[0].Commit {
		t.Fatalf("reread = %#v", reread.Apply)
	}

	imported, err := store.CompleteApply("run-apply")
	if err != nil {
		t.Fatal(err)
	}
	if imported.State != StateImported ||
		imported.ImportedBranch != "pisafe/run-apply" ||
		imported.ImportedAt == nil ||
		imported.Apply != nil {
		t.Fatalf("imported = %#v", imported)
	}
	if _, err := store.Resume("run-apply", testCapability(), time.Time{}); err == nil {
		t.Fatal("an imported run was resumed")
	}
	if stopped.State != StateStopped {
		t.Fatalf("fixture state = %q", stopped.State)
	}
}

func TestStoreRejectsApplyPlansItCannotReplaySafely(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	stoppedTestRun(t, store, root, "run-plan")

	valid := testApplyPlan("run-plan", root)
	for name, corrupt := range map[string]func(*gitstage.PlannedApply){
		"other run":      func(plan *gitstage.PlannedApply) { plan.Journal.RunID = "other" },
		"other branch":   func(plan *gitstage.PlannedApply) { plan.Result.Branch = "main" },
		"no steps":       func(plan *gitstage.PlannedApply) { plan.Journal.Steps = nil },
		"relative path":  func(plan *gitstage.PlannedApply) { plan.Journal.Steps[0].Repository = "project" },
		"foreign ref":    func(plan *gitstage.PlannedApply) { plan.Journal.Steps[0].Ref = "refs/heads/main" },
		"foreign temp":   func(plan *gitstage.PlannedApply) { plan.Journal.Steps[0].TemporaryRef = "refs/heads/main" },
		"invalid commit": func(plan *gitstage.PlannedApply) { plan.Journal.Steps[0].Commit = "HEAD" },
	} {
		plan := valid
		plan.Journal.Steps = append([]gitstage.ApplyStep(nil), valid.Journal.Steps...)
		corrupt(&plan)
		if _, err := store.BeginApply("run-plan", plan); err == nil {
			t.Errorf("BeginApply accepted a plan with a %s", name)
		}
	}
}

func TestStoreExpiresOnlyImportedRunsAndKeepsTheirAttribution(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	stoppedTestRun(t, store, root, "run-expire")

	// Work that was never imported cannot be expired at all.
	if _, err := store.Expire("run-expire"); err == nil ||
		!strings.Contains(err.Error(), "invalid run transition") {
		t.Fatalf("expiry of a stopped run = %v", err)
	}

	if _, err := store.BeginApply("run-expire", testApplyPlan("run-expire", root)); err != nil {
		t.Fatal(err)
	}
	imported, err := store.CompleteApply("run-expire")
	if err != nil {
		t.Fatal(err)
	}
	expired, err := store.Expire("run-expire")
	if err != nil {
		t.Fatal(err)
	}
	if expired.State != StateExpired {
		t.Fatalf("expired = %#v", expired)
	}
	// The workspace is gone but the branch must still name the run it came
	// from, so both import fields survive a fresh read.
	reread, err := store.Get("run-expire")
	if err != nil {
		t.Fatal(err)
	}
	if reread.ImportedBranch != "pisafe/run-expire" ||
		reread.ImportedAt == nil ||
		!reread.ImportedAt.Equal(*imported.ImportedAt) {
		t.Fatalf("reread = %#v", reread)
	}
	if _, err := store.Expire("run-expire"); err == nil {
		t.Fatal("a run expired twice")
	}
}

func TestStoreForgetsOnlyRecordsThatAttributeNothing(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	stoppedTestRun(t, store, root, "run-plain")
	stoppedTestRun(t, store, root, "run-branch")

	// A run still holding resources keeps its record whatever its age.
	if err := store.Forget("run-plain"); err == nil ||
		!strings.Contains(err.Error(), "still owns resources") {
		t.Fatalf("forgetting a stopped run = %v", err)
	}

	if _, err := store.BeginApply("run-branch", testApplyPlan("run-branch", root)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteApply("run-branch"); err != nil {
		t.Fatal(err)
	}
	for _, runID := range []string{"run-plain", "run-branch"} {
		if _, err := store.Discard(runID); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Forget("run-branch"); err == nil ||
		!strings.Contains(err.Error(), "pisafe/run-branch") {
		t.Fatalf("forgetting an imported run = %v", err)
	}
	if err := store.Forget("run-plain"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "run-plain.json")); !os.IsNotExist(err) {
		t.Fatalf("forgotten manifest survived: %v", err)
	}
	runs, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].RunID != "run-branch" {
		t.Fatalf("runs = %#v", runs)
	}
}

func stoppedTestRun(t *testing.T, store Store, root, runID string) Manifest {
	t.Helper()
	if _, err := store.Create(testManifest(runID)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Activate(
		runID,
		testSSHConnection(root, runID),
		"",
		testCapability(),
		time.Time{},
	); err != nil {
		t.Fatal(err)
	}
	stopped, err := store.Stop(runID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	return stopped
}

func testApplyPlan(runID, repository string) gitstage.PlannedApply {
	tip := strings.Repeat("c", 40)
	return gitstage.PlannedApply{
		Journal: gitstage.ApplyJournal{
			RunID: runID,
			Steps: []gitstage.ApplyStep{{
				Repository:   repository,
				Ref:          "refs/heads/pisafe/" + runID,
				Commit:       tip,
				TemporaryRef: "refs/pisafe/incoming/" + runID,
			}},
		},
		Result: gitstage.ApplyResult{Branch: "pisafe/" + runID, Tip: tip},
	}
}

func testCapability() string {
	return "pisafe-cap-" + strings.Repeat("ab", 32)
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
