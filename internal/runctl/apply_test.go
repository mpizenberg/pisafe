package runctl

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mpizenberg/pisafe/internal/gitstage"
	"github.com/mpizenberg/pisafe/internal/runstate"
)

func TestApplyImportsAStoppedRunAndMarksItImported(t *testing.T) {
	ctx := context.Background()
	source, workspace, snapshot := applyFixture(t, "apply-run")
	writeFile(t, filepath.Join(workspace, "tracked.txt"), "agent result\n")

	backend := &fakeBackend{
		applyWorkspace: workspace,
		applyPackage:   filepath.Join(t.TempDir(), "package"),
	}
	store := runstate.NewStore(t.TempDir())
	controller := New(backend, store, &fakeSSHStore{}, testInference{})
	stoppedRun(t, store, snapshot)

	manifest, result, err := controller.Apply(ctx, snapshot.RunID, testImage, gitstage.KeepBaseline, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Branch != "pisafe/apply-run" || result.FinalCommit == "" {
		t.Fatalf("result = %#v", result)
	}
	if manifest.State != runstate.StateImported ||
		manifest.ImportedBranch != "pisafe/apply-run" ||
		manifest.Apply != nil {
		t.Fatalf("manifest = %#v", manifest)
	}
	if got := sourceGit(t, source, "show", "pisafe/apply-run:tracked.txt"); got != "agent result" {
		t.Fatalf("imported content = %q", got)
	}
	if got := sourceGit(t, source, "status", "--porcelain=v1"); got != "" {
		t.Fatalf("source checkout changed: %q", got)
	}

	joined := callsString(backend.calls)
	for _, expected := range []string{
		"pisafe-guest prepare-apply",
		"fetch-apply apply.bundle",
		"remove-apply",
	} {
		if !strings.Contains(joined, expected) {
			t.Errorf("apply calls lack %q:\n%s", expected, joined)
		}
	}
	if _, err := os.Stat(backend.applyPackage); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("apply package survived in the run: %v", err)
	}

	// An imported run is not applied twice.
	if _, _, err := controller.Apply(ctx, snapshot.RunID, testImage, gitstage.KeepBaseline, false); err == nil ||
		!strings.Contains(err.Error(), "already imported") {
		t.Fatalf("second apply error = %v", err)
	}
}

// An imported run still holds the fixed-capacity filesystem it ran on, so
// discard remains the way to reclaim it, exactly as apply's own guidance says.
func TestImportedRunStillReclaimsItsStorage(t *testing.T) {
	ctx := context.Background()
	_, workspace, snapshot := applyFixture(t, "reclaimed-run")
	writeFile(t, filepath.Join(workspace, "tracked.txt"), "agent result\n")

	backend := &fakeBackend{
		applyWorkspace: workspace,
		applyPackage:   filepath.Join(t.TempDir(), "package"),
	}
	store := runstate.NewStore(t.TempDir())
	controller := New(backend, store, &fakeSSHStore{}, testInference{})
	stoppedRun(t, store, snapshot)

	if _, _, err := controller.Apply(ctx, snapshot.RunID, testImage, gitstage.KeepBaseline, false); err != nil {
		t.Fatal(err)
	}
	if err := controller.Discard(ctx, snapshot.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(snapshot.RunID); err == nil {
		t.Fatal("a discarded run kept its record")
	}
	if joined := callsString(backend.calls); !strings.Contains(joined, "remove-storage") {
		t.Fatalf("discard did not reclaim run storage:\n%s", joined)
	}
}

func TestApplyMountsRunStorageBeforeCapturingIt(t *testing.T) {
	ctx := context.Background()
	_, workspace, snapshot := applyFixture(t, "unmounted-run")
	writeFile(t, filepath.Join(workspace, "tracked.txt"), "agent result\n")

	backend := &fakeBackend{failAt: "verify-storage"}
	store := runstate.NewStore(t.TempDir())
	controller := New(backend, store, &fakeSSHStore{}, testInference{})
	stoppedRun(t, store, snapshot)

	if _, _, err := controller.Apply(ctx, snapshot.RunID, testImage, gitstage.KeepBaseline, false); err == nil ||
		!strings.Contains(err.Error(), "verify storage") {
		t.Fatalf("error = %v", err)
	}
	if joined := callsString(backend.calls); strings.Contains(joined, "podman") {
		t.Fatalf("apply captured a run whose storage was not mounted:\n%s", joined)
	}
}

func TestApplyFinishesARecordedPlanWithoutRecapturingTheRun(t *testing.T) {
	ctx := context.Background()
	source, workspace, snapshot := applyFixture(t, "interrupted-run")
	writeFile(t, filepath.Join(workspace, "tracked.txt"), "agent result\n")

	store := runstate.NewStore(t.TempDir())
	stoppedRun(t, store, snapshot)

	// Stand where an interrupted apply left off: the plan is recorded and its
	// objects are imported, but no user-visible ref has moved.
	packageDir := t.TempDir()
	prepared, err := gitstage.PrepareApply(ctx, snapshot, workspace, packageDir, gitstage.KeepBaseline)
	if err != nil {
		t.Fatal(err)
	}
	planned, err := gitstage.ImportApply(ctx, snapshot, prepared, packageDir, gitstage.KeepBaseline)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginApply(snapshot.RunID, planned, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := sourceGitOutput(source, "rev-parse", "--verify", "refs/heads/pisafe/interrupted-run"); err == nil {
		t.Fatal("a recorded plan already moved a branch")
	}

	// The run is gone: any attempt to capture it again must fail the test.
	backend := &fakeBackend{failAt: "podman"}
	controller := New(backend, store, &fakeSSHStore{}, testInference{})
	manifest, result, err := controller.Apply(ctx, snapshot.RunID, testImage, gitstage.KeepBaseline, false)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.State != runstate.StateImported || manifest.Apply != nil {
		t.Fatalf("manifest = %#v", manifest)
	}
	if got := sourceGit(
		t,
		source,
		"rev-parse", "refs/heads/pisafe/interrupted-run",
	); got != result.Tip {
		t.Fatalf("branch = %s, want %s", got, result.Tip)
	}
	if len(backend.calls) != 0 {
		t.Fatalf("finishing a recorded plan touched the run: %#v", backend.calls)
	}
}

func TestApplyKeepsThePlanWhenARefIsContested(t *testing.T) {
	ctx := context.Background()
	source, workspace, snapshot := applyFixture(t, "contested-run")
	writeFile(t, filepath.Join(workspace, "tracked.txt"), "agent result\n")

	backend := &fakeBackend{
		applyWorkspace: workspace,
		applyPackage:   filepath.Join(t.TempDir(), "package"),
	}
	store := runstate.NewStore(t.TempDir())
	controller := New(backend, store, &fakeSSHStore{}, testInference{})
	stoppedRun(t, store, snapshot)
	sourceGit(t, source, "update-ref", "refs/heads/pisafe/contested-run", snapshot.SourceHead)

	_, _, err := controller.Apply(ctx, snapshot.RunID, testImage, gitstage.KeepBaseline, false)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("error = %v", err)
	}
	manifest, err := store.Get(snapshot.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.State != runstate.StateStopped || manifest.LastError == "" {
		t.Fatalf("manifest = %#v", manifest)
	}
	if got := sourceGit(
		t,
		source,
		"rev-parse", "refs/heads/pisafe/contested-run",
	); got != snapshot.SourceHead {
		t.Fatalf("contested ref was overwritten with %s", got)
	}
}

// applyFixture creates a real source repository and a staged run workspace,
// which is what apply reads on both sides of the boundary.
func applyFixture(t *testing.T, runID string) (string, string, gitstage.Snapshot) {
	t.Helper()
	source := t.TempDir()
	sourceGit(t, source, "init", "-q", "--initial-branch=main")
	sourceGit(t, source, "config", "user.name", "Test User")
	sourceGit(t, source, "config", "user.email", "test@example.invalid")
	sourceGit(t, source, "config", "commit.gpgsign", "false")
	writeFile(t, filepath.Join(source, "tracked.txt"), "initial\n")
	sourceGit(t, source, "add", "tracked.txt")
	sourceGit(t, source, "commit", "-qm", "initial")

	workspace := filepath.Join(t.TempDir(), "workspace")
	snapshot, err := gitstage.Stage(
		context.Background(),
		gitstage.PrepareRequest{SourcePath: source, RunID: runID},
		workspace,
	)
	if err != nil {
		t.Fatal(err)
	}
	return source, workspace, snapshot
}

// stoppedRun records the run apply expects to find: created, activated, then
// stopped, with the snapshot the workspace was staged from.
func stoppedRun(t *testing.T, store runstate.Store, snapshot gitstage.Snapshot) {
	t.Helper()
	activeRun(t, store, snapshot)
	if _, err := store.Stop(snapshot.RunID, nil); err != nil {
		t.Fatal(err)
	}
}

func creatingRun(t *testing.T, store runstate.Store, snapshot gitstage.Snapshot) {
	t.Helper()
	if _, err := store.Create(runstate.Manifest{
		RunID:              snapshot.RunID,
		Project:            "project",
		ProjectKey:         testProject.Key,
		Snapshot:           snapshot,
		Image:              testImage,
		ActiveLimitSeconds: 3600,
	}); err != nil {
		t.Fatal(err)
	}
}

func activeRun(t *testing.T, store runstate.Store, snapshot gitstage.Snapshot) {
	t.Helper()
	creatingRun(t, store, snapshot)
	capability, err := runstate.NewInferenceCapability()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Activate(snapshot.RunID, runstate.SSHConnection{
		Alias:              "pisafe-" + snapshot.RunID,
		IdentityFile:       "/state/ssh/" + snapshot.RunID + "/id_ed25519",
		KnownHostsFile:     "/state/ssh/" + snapshot.RunID + "/known_hosts",
		ConfigFile:         "/state/ssh/" + snapshot.RunID + "/ssh.config",
		HostKeyFingerprint: "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	}, gitstage.Snapshot{BaselineCommit: snapshot.BaselineCommit}, capability); err != nil {
		t.Fatal(err)
	}
}

func sourceGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	output, err := sourceGitOutput(dir, args...)
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return output
}

func sourceGitOutput(dir string, args ...string) (string, error) {
	command := exec.Command("git", append([]string{"-C", dir}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		return "", errors.New(strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The choice about the baseline is made on the Mac and carried out inside the
// run, so the controller has to hand it over and then check what came back.
func TestApplyDropsTheBaselineWhenTheRunIsAskedTo(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	sourceGit(t, source, "init", "-q", "--initial-branch=main")
	sourceGit(t, source, "config", "user.name", "Test User")
	sourceGit(t, source, "config", "user.email", "test@example.invalid")
	sourceGit(t, source, "config", "commit.gpgsign", "false")
	writeFile(t, filepath.Join(source, "tracked.txt"), "initial\n")
	sourceGit(t, source, "add", "tracked.txt")
	sourceGit(t, source, "commit", "-qm", "initial")
	writeFile(t, filepath.Join(source, "tracked.txt"), "carried-in edit\n")

	workspace := filepath.Join(t.TempDir(), "workspace")
	snapshot, err := gitstage.Stage(
		ctx,
		gitstage.PrepareRequest{SourcePath: source, RunID: "drop-run"},
		workspace,
	)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.BaselineCommit == "" {
		t.Fatal("dirty source produced no baseline commit")
	}
	writeFile(t, filepath.Join(workspace, "agent.txt"), "agent work\n")
	sourceGit(t, workspace, "add", "agent.txt")
	sourceGit(t, workspace, "commit", "-qm", "agent commit")

	backend := &fakeBackend{
		applyWorkspace: workspace,
		applyPackage:   filepath.Join(t.TempDir(), "package"),
	}
	store := runstate.NewStore(t.TempDir())
	controller := New(backend, store, &fakeSSHStore{}, testInference{})
	stoppedRun(t, store, snapshot)

	_, result, err := controller.Apply(ctx, snapshot.RunID, testImage, gitstage.DropBaseline, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(callsString(backend.calls), "prepare-apply drop") {
		t.Fatalf("the run was not told to drop the baseline:\n%s", callsString(backend.calls))
	}
	if got := sourceGit(t, source, "show", result.Branch+":tracked.txt"); got != "initial" {
		t.Fatalf("imported tracked.txt = %q", got)
	}
	if got := sourceGit(t, source, "log", "--format=%s", snapshot.SourceHead+".."+result.Branch); got != "agent commit" {
		t.Fatalf("imported commits = %q", got)
	}
}

// A replay the run could not finish is an answer, not a broken run: nothing is
// imported, nothing is recorded as a failure, and the run stays applicable.
func TestApplyReportsAReplayConflictWithoutMarkingTheRunBroken(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	sourceGit(t, source, "init", "-q", "--initial-branch=main")
	sourceGit(t, source, "config", "user.name", "Test User")
	sourceGit(t, source, "config", "user.email", "test@example.invalid")
	sourceGit(t, source, "config", "commit.gpgsign", "false")
	writeFile(t, filepath.Join(source, "tracked.txt"), "initial\n")
	sourceGit(t, source, "add", "tracked.txt")
	sourceGit(t, source, "commit", "-qm", "initial")
	writeFile(t, filepath.Join(source, "tracked.txt"), "carried-in edit\n")

	workspace := filepath.Join(t.TempDir(), "workspace")
	snapshot, err := gitstage.Stage(
		ctx,
		gitstage.PrepareRequest{SourcePath: source, RunID: "conflict-run"},
		workspace,
	)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(workspace, "tracked.txt"), "carried-in edit, refined\n")
	sourceGit(t, workspace, "commit", "-qam", "agent commit")

	backend := &fakeBackend{
		applyWorkspace: workspace,
		applyPackage:   filepath.Join(t.TempDir(), "package"),
	}
	store := runstate.NewStore(t.TempDir())
	controller := New(backend, store, &fakeSSHStore{}, testInference{})
	stoppedRun(t, store, snapshot)

	_, _, err = controller.Apply(ctx, snapshot.RunID, testImage, gitstage.DropBaseline, false)
	conflict := &gitstage.BaselineReplayConflict{}
	if !errors.As(err, &conflict) {
		t.Fatalf("error = %v", err)
	}
	manifest, err := store.Get(snapshot.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.State != runstate.StateStopped || manifest.LastError != "" ||
		manifest.Apply != nil {
		t.Fatalf("manifest = %#v", manifest)
	}
	if _, _, err := controller.Apply(ctx, snapshot.RunID, testImage, gitstage.KeepBaseline, false); err != nil {
		t.Fatalf("keeping the baseline after a conflict: %v", err)
	}
}
