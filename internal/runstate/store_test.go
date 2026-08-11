package runstate

import (
	"fmt"
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
	active, err := store.Activate("run-one", testSSHConnection(root, "run-one"), gitstage.Snapshot{}, testCapability(), time.Time{})
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
		gitstage.Snapshot{},
		testCapability(),
		time.Time{},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginApply("run-one", testApplyPlan("run-one", root), nil); err == nil ||
		!strings.Contains(err.Error(), "not stopped") {
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
		fmt.Appendf(nil, `{"version":%d,"run_id":"other"}`, manifestVersion),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	// The listing stops, so it has to name the record that stopped it.
	_, err := store.List()
	if err == nil || !strings.Contains(err.Error(), "identity mismatch") ||
		!strings.Contains(err.Error(), `"bad"`) {
		t.Fatalf("error = %v", err)
	}
}

// Every run's workspace path is built from its project name, so what that name
// may contain is settled here rather than wherever the path is spliced into a
// command.
func TestStoreRefusesAProjectNameItWouldPutInAPath(t *testing.T) {
	store := NewStore(t.TempDir())
	manifest := testManifest("run-one")
	for _, project := range []string{"", "my project", "../escape", "a;b", "-lead"} {
		manifest.Project = project
		if _, err := store.Create(manifest); err == nil {
			t.Fatalf("project name %q was accepted", project)
		}
	}
	manifest.Project = "project"
	created, err := store.Create(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if created.Workspace() != "/work/project" {
		t.Fatalf("workspace = %q", created.Workspace())
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
		gitstage.Snapshot{},
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
		gitstage.Snapshot{BaselineCommit: strings.Repeat("a", 40)},
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
	}, gitstage.Snapshot{}, testCapability(), time.Time{}); err == nil {
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
		gitstage.Snapshot{BaselineCommit: "not-a-git-object"},
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
	active, err := store.Activate("run-one", testSSHConnection(root, "run-one"), gitstage.Snapshot{}, testCapability(), time.Time{})
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

func TestStoreAbandonsARunWithoutChargingForTheOutage(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	store := NewStore(root)
	store.now = func() time.Time { return now }
	if _, err := store.Create(testManifest("run-one")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Activate(
		"run-one",
		testSSHConnection(root, "run-one"),
		gitstage.Snapshot{},
		testCapability(),
		time.Time{},
	); err != nil {
		t.Fatal(err)
	}
	// Longer than the whole budget, which is what stopping by the wall clock
	// would charge and what the run must not be charged.
	now = now.Add(30 * time.Hour)
	abandoned, err := store.Abandon("run-one")
	if err != nil {
		t.Fatal(err)
	}
	if abandoned.State != StateStopped ||
		abandoned.ActiveElapsedSeconds != 0 ||
		abandoned.ActiveStartedAt != nil ||
		abandoned.ActiveDeadline != nil ||
		abandoned.InferenceCapability != "" {
		t.Fatalf("abandoned = %#v", abandoned)
	}
	if got := RemainingSeconds(abandoned, now); got != abandoned.ActiveLimitSeconds {
		t.Fatalf("remaining = %d of %d", got, abandoned.ActiveLimitSeconds)
	}
	if _, err := store.Abandon("run-one"); err == nil ||
		!strings.Contains(err.Error(), "invalid run transition") {
		t.Fatalf("error = %v", err)
	}
	if _, err := store.Resume("run-one", testCapability(), now); err != nil {
		t.Fatal(err)
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
		gitstage.Snapshot{},
		"not-a-capability",
		time.Time{},
	); err == nil {
		t.Fatal("invalid inference capability was accepted")
	}
	first := testCapability()
	active, err := store.Activate(
		"run-one",
		testSSHConnection(root, "run-one"),
		gitstage.Snapshot{},
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
	restopped, err := store.Stop("run-one", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if restopped.InferenceCapability != "" {
		t.Fatal("restopped run retained its inference capability")
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
	recorded, err := store.BeginApply("run-apply", planned, nil)
	if err != nil {
		t.Fatal(err)
	}
	if recorded.State != StateStopped || recorded.Apply == nil {
		t.Fatalf("recorded = %#v", recorded)
	}
	if _, err := store.BeginApply("run-apply", planned, nil); err == nil ||
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
		"invalid commit": func(plan *gitstage.PlannedApply) { plan.Journal.Steps[0].Commit = "HEAD" },
	} {
		plan := valid
		plan.Journal.Steps = append([]gitstage.ApplyStep(nil), valid.Journal.Steps...)
		corrupt(&plan)
		if _, err := store.BeginApply("run-plan", plan, nil); err == nil {
			t.Errorf("BeginApply accepted a plan with a %s", name)
		}
	}
}

func TestStoreForgetsARunWhateverItsRecordSays(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	stoppedTestRun(t, store, root, "run-gone")
	stoppedTestRun(t, store, root, "run-stale")

	if _, err := store.BeginApply("run-gone", testApplyPlan("run-gone", root), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteApply("run-gone"); err != nil {
		t.Fatal(err)
	}
	if err := store.Forget("run-gone"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "run-gone.json")); !os.IsNotExist(err) {
		t.Fatalf("forgotten manifest survived: %v", err)
	}
	if err := store.Forget("run-gone"); err == nil ||
		!strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("forgetting a run with no record = %v", err)
	}

	// A record written by a version that is gone is the one most worth
	// removing, so removing it may not depend on understanding it.
	stale := filepath.Join(root, "run-stale.json")
	if err := os.WriteFile(stale, []byte(`{"version":1,"run_id":"run-stale"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get("run-stale"); err == nil {
		t.Fatal("a superseded record decoded")
	}
	if err := store.Forget("run-stale"); err != nil {
		t.Fatalf("forgetting a superseded record = %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("superseded record survived: %v", err)
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
		gitstage.Snapshot{},
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
				Repository: repository,
				Commit:     tip,
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
		ProjectKey:         "project-3f9c2a1b",
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

// Only materialization knows which repositories needed a baseline commit, so
// activation is where the record stops describing an intention and starts
// describing the run.
func TestStoreRecordsMaterializedSubmoduleBaselines(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	manifest := testManifest("run-one")
	manifest.Snapshot.Submodules = []gitstage.SubmoduleStage{
		{Path: "dependency", Head: strings.Repeat("c", 40)},
	}
	if _, err := store.Create(manifest); err != nil {
		t.Fatal(err)
	}
	materialized := manifest.Snapshot
	materialized.BaselineCommit = strings.Repeat("a", 40)
	materialized.Submodules = []gitstage.SubmoduleStage{
		{
			Path:           "dependency",
			Head:           strings.Repeat("c", 40),
			BaselineCommit: strings.Repeat("d", 40),
		},
	}
	active, err := store.Activate(
		"run-one",
		testSSHConnection(root, "run-one"),
		materialized,
		testCapability(),
		time.Time{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if active.Snapshot.Submodules[0].BaselineCommit != strings.Repeat("d", 40) {
		t.Fatalf("submodules = %#v", active.Snapshot.Submodules)
	}

	// Materialization fills baselines in; it does not get to describe a
	// different set of submodules.
	for _, wrong := range [][]gitstage.SubmoduleStage{
		{},
		{{Path: "other", Head: strings.Repeat("c", 40)}},
		{{Path: "dependency", Head: strings.Repeat("e", 40)}},
		{{Path: "dependency", Head: strings.Repeat("c", 40), BaselineCommit: "not-a-git-object"}},
	} {
		store := NewStore(t.TempDir())
		if _, err := store.Create(manifest); err != nil {
			t.Fatal(err)
		}
		wrongSnapshot := manifest.Snapshot
		wrongSnapshot.Submodules = wrong
		if _, err := store.Activate(
			"run-one",
			testSSHConnection(root, "run-one"),
			wrongSnapshot,
			testCapability(),
			time.Time{},
		); err == nil {
			t.Fatalf("activation accepted submodules %#v", wrong)
		}
	}
}
