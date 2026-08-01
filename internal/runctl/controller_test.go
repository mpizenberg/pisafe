package runctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mpizenberg/pisafe/internal/gitstage"
	"github.com/mpizenberg/pisafe/internal/profile"
	"github.com/mpizenberg/pisafe/internal/runcontainer"
	"github.com/mpizenberg/pisafe/internal/runcopy"
	"github.com/mpizenberg/pisafe/internal/runid"
	"github.com/mpizenberg/pisafe/internal/runssh"
	"github.com/mpizenberg/pisafe/internal/runstate"
)

// testProject is derived rather than written out, because a project record
// carries the checkout its key was made from and refuses to describe any other.
var testProject = mustProject("/tmp/pisafe-test/project")

func mustProject(root string) runid.Project {
	project, err := runid.NewProject(root)
	if err != nil {
		panic(err)
	}
	return project
}

const testImage = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

var testIdentity = gitstage.Identity{Name: "Test User", Email: "test@example.invalid"}

type backendCall struct {
	kind  string
	args  []string
	stdin string
}

type fakeBackend struct {
	calls       []backendCall
	failAt      string
	failAfterAt string
	snapshot    string
	profile     profile.Record
	container   *containerInspection
	// applyWorkspace and applyPackage stand in for the run's storage: the
	// capture runs against a real workspace and leaves real bundles behind.
	applyWorkspace string
	applyPackage   string
}

func (backend *fakeBackend) CreateStage(
	_ context.Context,
	prepared gitstage.PreparedStage,
) (string, error) {
	backend.calls = append(backend.calls, backendCall{kind: "stage"})
	if backend.failAt == "stage" {
		return "", errors.New("stage failed")
	}
	return "/home/user/.local/share/pisafe/runs/" +
		prepared.Snapshot.RunID + "/stage", nil
}

func (backend *fakeBackend) ImportStage(
	_ context.Context,
	_ string,
) error {
	backend.calls = append(backend.calls, backendCall{kind: "import"})
	if backend.failAt == "import" {
		return errors.New("import failed")
	}
	return nil
}

func (backend *fakeBackend) CreateRunStorage(_ context.Context, _ string) error {
	backend.calls = append(backend.calls, backendCall{kind: "create-storage"})
	if backend.failAt == "create-storage" {
		return errors.New("storage failed")
	}
	return nil
}

func (backend *fakeBackend) VerifyRunStorage(_ context.Context, _ string) error {
	backend.calls = append(backend.calls, backendCall{kind: "verify-storage"})
	if backend.failAt == "verify-storage" {
		return errors.New("verify storage failed")
	}
	return nil
}

func (backend *fakeBackend) RemoveRunStorage(_ context.Context, _ string) error {
	backend.calls = append(backend.calls, backendCall{kind: "remove-storage"})
	if backend.failAt == "remove-storage" {
		return errors.New("remove storage failed")
	}
	return nil
}

func (backend *fakeBackend) EnsureProjectStorage(_ context.Context, _ string) error {
	backend.calls = append(backend.calls, backendCall{kind: "ensure-project-storage"})
	if backend.failAt == "ensure-project-storage" {
		return errors.New("ensure project storage failed")
	}
	return nil
}

func (backend *fakeBackend) RemoveProjectStorage(_ context.Context, projectKey string) error {
	backend.calls = append(backend.calls, backendCall{
		kind: "remove-project-storage",
		args: []string{projectKey},
	})
	if backend.failAt == "remove-project-storage" {
		return errors.New("remove project storage failed")
	}
	return nil
}

func (backend *fakeBackend) EnsureGlobalStorage(_ context.Context) error {
	backend.calls = append(backend.calls, backendCall{kind: "ensure-global-storage"})
	if backend.failAt == "ensure-global-storage" {
		return errors.New("ensure global storage failed")
	}
	return nil
}

func (backend *fakeBackend) ReadProfileRecord(_ context.Context) (profile.Record, error) {
	backend.calls = append(backend.calls, backendCall{kind: "read-profile"})
	if backend.failAt == "read-profile" {
		return profile.Record{}, errors.New("read profile failed")
	}
	return backend.profile, nil
}

func (backend *fakeBackend) SelectCacheSnapshots(
	_ context.Context,
	_ string,
	caches []runcontainer.CacheMount,
) ([]runcontainer.CacheMount, error) {
	backend.calls = append(backend.calls, backendCall{kind: "select-snapshots"})
	if backend.failAt == "select-snapshots" {
		return nil, errors.New("select snapshots failed")
	}
	selected := make([]runcontainer.CacheMount, 0, len(caches))
	for _, cache := range caches {
		cache.Snapshot = backend.snapshot
		selected = append(selected, cache)
	}
	return selected, nil
}

func (backend *fakeBackend) PrepareRunLayout(
	_ context.Context,
	_ string,
	_ []runcontainer.CacheMount,
) error {
	backend.calls = append(backend.calls, backendCall{kind: "prepare-layout"})
	if backend.failAt == "prepare-layout" {
		return errors.New("prepare layout failed")
	}
	return nil
}

func (backend *fakeBackend) PublishCacheSnapshot(
	_ context.Context,
	spec runcontainer.Spec,
	cache runcontainer.CacheMount,
) error {
	backend.calls = append(backend.calls, backendCall{
		kind: "publish-snapshot",
		args: []string{spec.ProjectKey, cache.Name, cache.Key, cache.Snapshot},
	})
	if backend.failAt == "publish-snapshot" {
		return errors.New("publish snapshot failed")
	}
	return nil
}

func (backend *fakeBackend) EvictCacheSnapshots(
	_ context.Context,
	projectKey string,
	name string,
	keep int,
	held []string,
) error {
	backend.calls = append(backend.calls, backendCall{
		kind: "evict-snapshots",
		args: append([]string{projectKey, name, strconv.Itoa(keep)}, held...),
	})
	if backend.failAt == "evict-snapshots" {
		return errors.New("evict snapshots failed")
	}
	return nil
}

func (backend *fakeBackend) PromoteSessions(
	_ context.Context,
	projectKey string,
	runID string,
) error {
	backend.calls = append(backend.calls, backendCall{
		kind: "promote-sessions",
		args: []string{projectKey, runID},
	})
	if backend.failAt == "promote-sessions" {
		return errors.New("promote sessions failed")
	}
	return nil
}

func (backend *fakeBackend) RestoreSessions(
	_ context.Context,
	projectKey string,
	archive io.Reader,
) error {
	carried, err := io.ReadAll(archive)
	if err != nil {
		return err
	}
	backend.calls = append(backend.calls, backendCall{
		kind: "restore-sessions",
		args: []string{projectKey, string(carried)},
	})
	if backend.failAt == "restore-sessions" {
		return errors.New("restore sessions failed")
	}
	return nil
}

func (backend *fakeBackend) AdoptSessions(
	_ context.Context,
	projectKey string,
	fromKey string,
) error {
	backend.calls = append(backend.calls, backendCall{
		kind: "adopt-sessions",
		args: []string{projectKey, fromKey},
	})
	if backend.failAt == "adopt-sessions" {
		return errors.New("adopt sessions failed")
	}
	return nil
}

func (backend *fakeBackend) ResetProjectCache(_ context.Context, projectKey string) error {
	backend.calls = append(backend.calls, backendCall{
		kind: "reset-project-cache",
		args: []string{projectKey},
	})
	if backend.failAt == "reset-project-cache" {
		return errors.New("reset project cache failed")
	}
	return nil
}

// StreamExecute stands in for the run's export container: it archives the same
// workspace the other fakes read, so the controller sees a real tar arrive.
func (backend *fakeBackend) StreamExecute(
	_ context.Context,
	stdout io.Writer,
	args ...string,
) error {
	backend.calls = append(backend.calls, backendCall{
		kind: "stream",
		args: append([]string(nil), args...),
	})
	joined := strings.Join(args, " ")
	if backend.failAt != "" && strings.Contains(joined, backend.failAt) {
		return errors.New("stream failed")
	}
	index := slices.Index(args, "export")
	if index < 0 || index+2 >= len(args) {
		return errors.New("unexpected stream command: " + joined)
	}
	return runcopy.Archive(backend.applyWorkspace, args[index+2], stdout)
}

func (backend *fakeBackend) Execute(
	ctx context.Context,
	stdin io.Reader,
	args ...string,
) ([]byte, error) {
	var input string
	if stdin != nil {
		content, err := io.ReadAll(stdin)
		if err != nil {
			return nil, err
		}
		input = string(content)
	}
	backend.calls = append(backend.calls, backendCall{
		kind:  "execute",
		args:  append([]string(nil), args...),
		stdin: input,
	})
	if backend.failAt != "" && strings.Contains(strings.Join(args, " "), backend.failAt) {
		return nil, errors.New("execute failed")
	}
	if strings.Contains(strings.Join(args, " "), "pisafe-inspect-container") {
		if backend.container == nil {
			return []byte("null\n"), nil
		}
		output, err := json.Marshal([]containerInspection{*backend.container})
		return append(output, '\n'), err
	}
	if strings.Contains(strings.Join(args, " "), "pisafe-guest configure-ssh") {
		return []byte("ssh-ed25519 host-key\n"), nil
	}
	if strings.Contains(strings.Join(args, " "), "pisafe-guest prepare-apply") {
		var snapshot gitstage.Snapshot
		if err := json.Unmarshal([]byte(input), &snapshot); err != nil {
			return nil, err
		}
		if snapshot.SourceRoot != "" {
			return nil, errors.New("apply request disclosed the Mac path")
		}
		choice, err := gitstage.ParseBaselineChoice(args[slices.Index(args, "prepare-apply")+1])
		if err != nil {
			return nil, err
		}
		prepared, err := gitstage.PrepareApply(
			ctx,
			snapshot,
			backend.applyWorkspace,
			backend.applyPackage,
			choice,
		)
		if err != nil {
			return nil, err
		}
		output, err := json.Marshal(prepared)
		return append(output, '\n'), err
	}
	if strings.Contains(strings.Join(args, " "), "pisafe-guest diff") {
		var snapshot gitstage.Snapshot
		if err := json.Unmarshal([]byte(input), &snapshot); err != nil {
			return nil, err
		}
		if snapshot.SourceRoot != "" {
			return nil, errors.New("diff request disclosed the Mac path")
		}
		diff, err := gitstage.DiffRun(ctx, snapshot, backend.applyWorkspace)
		if err != nil {
			return nil, err
		}
		output, err := json.Marshal(diff)
		return append(output, '\n'), err
	}
	if strings.Contains(strings.Join(args, " "), "pisafe-guest materialize") {
		materialized := testPrepared().Snapshot
		materialized.SourceRoot = ""
		materialized.BaselineCommit = strings.Repeat("b", 40)
		output, err := json.Marshal(materialized)
		return append(output, '\n'), err
	}
	if len(args) >= 2 && args[0] == "podman" && args[1] == "run" {
		if name := flagValue(args, "--name"); name != "" {
			backend.container = inspectionFromRunArgs(args, name)
		}
	}
	if len(args) >= 2 && args[0] == "podman" && args[1] == "stop" &&
		backend.container != nil {
		backend.container.State.Status = "exited"
		backend.container.State.FinishedAt = time.Now().UTC()
	}
	if len(args) >= 2 && args[0] == "podman" && args[1] == "rm" {
		backend.container = nil
	}
	if backend.failAfterAt != "" &&
		strings.Contains(strings.Join(args, " "), backend.failAfterAt) {
		return nil, errors.New("execute failed after remote success")
	}
	return nil, nil
}

// FetchApplyArtifact copies from the directory the fake run wrote, standing in
// for the streamed and verified transfer out of the VM.
func (backend *fakeBackend) FetchApplyArtifact(
	_ context.Context,
	_ string,
	artifact gitstage.ApplyArtifact,
	destination string,
) error {
	backend.calls = append(backend.calls, backendCall{
		kind: "fetch-apply",
		args: []string{artifact.Name},
	})
	content, err := os.ReadFile(filepath.Join(backend.applyPackage, artifact.Name))
	if err != nil {
		return err
	}
	return os.WriteFile(destination, content, 0o600)
}

func (backend *fakeBackend) RemoveApplyPackage(_ context.Context, _ string) error {
	backend.calls = append(backend.calls, backendCall{kind: "remove-apply"})
	if backend.failAt == "remove-apply" {
		return errors.New("remove apply package failed")
	}
	return os.RemoveAll(backend.applyPackage)
}

func (backend *fakeBackend) RemoveRun(_ context.Context, _ string) error {
	backend.calls = append(backend.calls, backendCall{kind: "remove-stage"})
	return nil
}

func (backend *fakeBackend) SSHGateway(_ context.Context) (runssh.Gateway, error) {
	backend.calls = append(backend.calls, backendCall{kind: "ssh-gateway"})
	return runssh.Gateway{
		ConfigFile: "/Users/alice/.lima/pisafe/ssh.config",
		Alias:      "lima-pisafe",
	}, nil
}

type fakeSSHStore struct {
	removed bool
}

func (store *fakeSSHStore) Prepare(
	_ context.Context,
	runID string,
) (runssh.Prepared, error) {
	return runssh.Prepared{
		RunID:        runID,
		PublicKey:    "ssh-ed25519 client-key",
		IdentityFile: "/state/ssh/" + runID + "/id_ed25519",
	}, nil
}

func (store *fakeSSHStore) Finalize(
	prepared runssh.Prepared,
	_ string,
	_ runssh.Gateway,
	_ string,
) (runssh.Endpoint, error) {
	root := filepath.Dir(prepared.IdentityFile)
	return runssh.Endpoint{
		Alias:              "pisafe-" + prepared.RunID,
		IdentityFile:       prepared.IdentityFile,
		KnownHostsFile:     filepath.Join(root, "known_hosts"),
		ConfigFile:         filepath.Join(root, "ssh.config"),
		HostKeyFingerprint: "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	}, nil
}

func (store *fakeSSHStore) Remove(_ string) error {
	store.removed = true
	return nil
}

func TestStartPreparedActivatesOnlyAfterMaterialization(t *testing.T) {
	backend := &fakeBackend{}
	store := runstate.NewStore(t.TempDir())
	ssh := &fakeSSHStore{}
	controller := New(backend, store, ssh, testInference{})

	manifest, err := controller.StartPrepared(
		context.Background(),
		testPrepared(),
		testProject,
		testImage,
		testIdentity,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.State != runstate.StateActive {
		t.Fatalf("manifest = %#v", manifest)
	}
	joined := callsString(backend.calls)
	for _, expected := range []string{
		"stage",
		"ensure-project-storage",
		"create-storage",
		"import",
		"pisafe-guest configure-ssh",
		"podman run",
		"ssh-gateway",
		"pisafe-guest materialize",
		"rm -rf /work/stage",
		"remove-stage",
		"pisafe-guest configure-identity",
		"pisafe-guest configure-inference",
	} {
		if !strings.Contains(joined, expected) {
			t.Errorf("calls lack %q:\n%s", expected, joined)
		}
	}
	if stdin := stdinFor(backend.calls, "configure-identity"); !strings.Contains(
		stdin,
		testIdentity.Email,
	) {
		t.Fatalf("run did not receive the user's Git identity: %q", stdin)
	}
	if manifest.SSH == nil ||
		manifest.SSH.Alias != "pisafe-run-123" {
		t.Fatalf("manifest SSH = %#v", manifest.SSH)
	}
	if !runstate.ValidInferenceCapability(manifest.InferenceCapability) {
		t.Fatalf("manifest capability = %q", manifest.InferenceCapability)
	}
	if stdin := stdinFor(backend.calls, "configure-inference"); !strings.Contains(
		stdin,
		manifest.InferenceCapability,
	) {
		t.Fatalf("inference configuration lacks the run capability: %q", stdin)
	}
	if manifest.Snapshot.BaselineCommit != strings.Repeat("b", 40) {
		t.Fatalf("manifest baseline = %q", manifest.Snapshot.BaselineCommit)
	}
	if ssh.removed {
		t.Fatal("active run SSH credentials were removed")
	}
}

func TestStartPreparedRollsBackAndRecordsFailure(t *testing.T) {
	backend := &fakeBackend{failAt: "pisafe-guest materialize"}
	store := runstate.NewStore(t.TempDir())
	ssh := &fakeSSHStore{}
	controller := New(backend, store, ssh, testInference{})

	_, err := controller.StartPrepared(
		context.Background(),
		testPrepared(),
		testProject,
		testImage,
		testIdentity,
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "materialize staged repository") {
		t.Fatalf("error = %v", err)
	}
	manifest, getErr := store.Get("run-123")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if manifest.State != runstate.StateCreating ||
		!strings.Contains(manifest.LastError, "materialize staged repository") {
		t.Fatalf("manifest = %#v", manifest)
	}
	joined := callsString(backend.calls)
	for _, expected := range []string{
		"podman rm --force --ignore pisafe-run-run-123",
		"remove-storage",
		"remove-stage",
	} {
		if !strings.Contains(joined, expected) {
			t.Errorf("rollback calls lack %q:\n%s", expected, joined)
		}
	}
	if !ssh.removed {
		t.Fatal("failed run SSH credentials were not removed")
	}
}

func TestStartPreparedCleansStorageAfterAmbiguousCreateFailure(t *testing.T) {
	backend := &fakeBackend{failAt: "create-storage"}
	store := runstate.NewStore(t.TempDir())
	controller := New(backend, store, &fakeSSHStore{}, testInference{})

	if _, err := controller.StartPrepared(
		context.Background(),
		testPrepared(),
		testProject,
		testImage,
		testIdentity,
		nil,
	); err == nil {
		t.Fatal("StartPrepared unexpectedly succeeded")
	}
	joined := callsString(backend.calls)
	if !strings.Contains(joined, "remove-storage") {
		t.Fatalf("controller did not clean potentially created storage:\n%s", joined)
	}
}

func TestLifecycleStopsAndResumesWithRemainingBudget(t *testing.T) {
	store := runstate.NewStore(t.TempDir())
	manifest := activeManifest(t, store)
	spec := specForManifest(manifest, manifest.Image)
	runArgs, err := spec.RunArgs()
	if err != nil {
		t.Fatal(err)
	}
	backend := &fakeBackend{
		container: inspectionFromRunArgs(
			append([]string{"podman"}, runArgs...),
			spec.ContainerName(),
		),
	}
	controller := New(backend, store, &fakeSSHStore{}, testInference{})

	stopped, err := controller.Stop(context.Background(), manifest.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.State != runstate.StateStopped || backend.container != nil {
		t.Fatalf("stopped = %#v, container = %#v", stopped, backend.container)
	}
	remaining := runstate.RemainingSeconds(stopped, time.Now())
	if remaining <= 0 || remaining > manifest.ActiveLimitSeconds {
		t.Fatalf("remaining = %d", remaining)
	}

	resumed, err := controller.Resume(context.Background(), manifest.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.State != runstate.StateActive ||
		backend.container == nil ||
		backend.container.State.Status != "running" {
		t.Fatalf("resumed = %#v, container = %#v", resumed, backend.container)
	}
	if !strings.Contains(callsString(backend.calls), "--timeout "+fmt.Sprint(remaining)) {
		t.Fatalf("resume did not apply remaining timeout:\n%s", callsString(backend.calls))
	}
	if !runstate.ValidInferenceCapability(resumed.InferenceCapability) ||
		resumed.InferenceCapability == manifest.InferenceCapability {
		t.Fatalf("resume did not rotate the capability: %q", resumed.InferenceCapability)
	}
	if stdin := stdinFor(backend.calls, "configure-inference"); !strings.Contains(
		stdin,
		resumed.InferenceCapability,
	) {
		t.Fatalf("resumed inference configuration lacks the fresh capability: %q", stdin)
	}
}

func TestResumeCleansContainerAfterAmbiguousStartFailure(t *testing.T) {
	store := runstate.NewStore(t.TempDir())
	manifest := activeManifest(t, store)
	spec := specForManifest(manifest, manifest.Image)
	runArgs, err := spec.RunArgs()
	if err != nil {
		t.Fatal(err)
	}
	backend := &fakeBackend{
		container: inspectionFromRunArgs(
			append([]string{"podman"}, runArgs...),
			spec.ContainerName(),
		),
	}
	controller := New(backend, store, &fakeSSHStore{}, testInference{})
	if _, err := controller.Stop(context.Background(), manifest.RunID); err != nil {
		t.Fatal(err)
	}

	backend.failAfterAt = "podman run --detach"
	if _, err := controller.Resume(context.Background(), manifest.RunID); err == nil ||
		!strings.Contains(err.Error(), "start run container") {
		t.Fatalf("error = %v", err)
	}
	if backend.container != nil {
		t.Fatal("ambiguously started container was not removed")
	}
	stopped, err := store.Get(manifest.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.State != runstate.StateStopped ||
		!strings.Contains(stopped.LastError, "start run container") {
		t.Fatalf("manifest = %#v", stopped)
	}
}

func TestDiscardCleansActiveAndFailedCreatingRuns(t *testing.T) {
	for _, initial := range []runstate.State{
		runstate.StateActive,
		runstate.StateCreating,
	} {
		t.Run(string(initial), func(t *testing.T) {
			store := runstate.NewStore(t.TempDir())
			var manifest runstate.Manifest
			if initial == runstate.StateActive {
				manifest = activeManifest(t, store)
			} else {
				prepared := testPrepared()
				spec := runcontainer.DefaultSpec(prepared.Snapshot.RunID, testProject.Key, testImage)
				var err error
				manifest, err = store.Create(runstate.Manifest{
					RunID:              spec.RunID,
					Project:            "project",
					ProjectKey:         testProject.Key,
					Snapshot:           prepared.Snapshot,
					Image:              spec.ImageID,
					Container:          spec.ContainerName(),
					Workspace:          "/work/project",
					ActiveLimitSeconds: spec.WallSeconds,
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			backend := &fakeBackend{}
			if initial == runstate.StateActive {
				spec := specForManifest(manifest, manifest.Image)
				args, err := spec.RunArgs()
				if err != nil {
					t.Fatal(err)
				}
				backend.container = inspectionFromRunArgs(
					append([]string{"podman"}, args...),
					spec.ContainerName(),
				)
			}
			ssh := &fakeSSHStore{}
			controller := New(backend, store, ssh, testInference{})
			if err := controller.Discard(context.Background(), manifest.RunID); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Get(manifest.RunID); err == nil {
				t.Fatal("a discarded run kept its record")
			}
			if !ssh.removed {
				t.Fatal("discard left the SSH key behind")
			}
			joined := callsString(backend.calls)
			for _, expected := range []string{"remove-storage", "remove-stage"} {
				if !strings.Contains(joined, expected) {
					t.Errorf("cleanup lacks %q:\n%s", expected, joined)
				}
			}
		})
	}
}

func TestLifecycleRefusesMismatchedContainerBeforeDeletion(t *testing.T) {
	store := runstate.NewStore(t.TempDir())
	manifest := activeManifest(t, store)
	spec := specForManifest(manifest, manifest.Image)
	args, err := spec.RunArgs()
	if err != nil {
		t.Fatal(err)
	}
	inspection := inspectionFromRunArgs(
		append([]string{"podman"}, args...),
		spec.ContainerName(),
	)
	inspection.Config.Labels["io.pisafe.run"] = "other-run"
	backend := &fakeBackend{container: inspection}
	controller := New(backend, store, &fakeSSHStore{}, testInference{})

	if _, err := controller.Stop(context.Background(), manifest.RunID); err == nil ||
		!strings.Contains(err.Error(), "label does not match") {
		t.Fatalf("error = %v", err)
	}
	if backend.container == nil {
		t.Fatal("mismatched container was deleted")
	}
}

// testInference stands in for a configured provider; the rendered content
// only needs to be recognizable in the recorded podman exec stdin.
type testInference struct{}

func (testInference) ModelsJSON(capability string) ([]byte, error) {
	return []byte(`{"providers":{"pisafe":{"apiKey":"` + capability + `"}}}`), nil
}

func testPrepared() gitstage.PreparedStage {
	return gitstage.PreparedStage{
		Snapshot: gitstage.Snapshot{
			RunID:      "run-123",
			SourceRoot: "/Users/alice/project",
			SourceHead: strings.Repeat("a", 40),
			WorkRef:    "refs/heads/work/run-123",
		},
		BundlePath: "/tmp/source.bundle",
		PatchPath:  "/tmp/tracked.patch",
	}
}

func callsString(calls []backendCall) string {
	var lines []string
	for _, call := range calls {
		lines = append(lines, strings.TrimSpace(call.kind+" "+strings.Join(call.args, " ")))
	}
	return strings.Join(lines, "\n")
}

// stdinFor returns the stdin recorded for the most recent call whose argv
// mentions marker.
func stdinFor(calls []backendCall, marker string) string {
	stdin := ""
	for _, call := range calls {
		if strings.Contains(strings.Join(call.args, " "), marker) {
			stdin = call.stdin
		}
	}
	return stdin
}

func inspectionFromRunArgs(args []string, name string) *containerInspection {
	inspection := &containerInspection{
		ID:    strings.Repeat("c", 64),
		Name:  name,
		Image: testImage,
	}
	inspection.Config.Labels = map[string]string{
		"io.pisafe.run": strings.TrimPrefix(name, "pisafe-run-"),
	}
	inspection.State.Status = "running"
	inspection.State.StartedAt = time.Now().UTC()
	for index, arg := range args {
		if index+1 >= len(args) {
			continue
		}
		switch arg {
		case "--mount":
			inspection.Mounts = append(inspection.Mounts, bindMount(args[index+1]))
		case "--volume":
			inspection.Mounts = append(inspection.Mounts, overlayMount(args[index+1]))
		}
	}
	return inspection
}

// bindMount mirrors what Podman reports for type=bind,src=…,dst=…,options.
// Read-only is reported as a field of its own and never as an option, so a
// mount is writable here unless its specification says otherwise.
func bindMount(specification string) containerMount {
	mount := containerMount{Writable: true}
	for _, part := range strings.Split(specification, ",") {
		key, value, found := strings.Cut(part, "=")
		if !found {
			if part == "ro" {
				mount.Writable = false
			}
			continue
		}
		switch key {
		case "type":
			mount.Type = value
		case "src":
			mount.Source = value
		case "dst":
			mount.Destination = value
		}
	}
	return mount
}

// overlayMount mirrors what Podman reports for lower:destination:O,options —
// a bind onto a merged directory it names itself, with the real layers left
// in the options.
func overlayMount(specification string) containerMount {
	lower, rest, _ := strings.Cut(specification, ":")
	destination, options, _ := strings.Cut(rest, ":")
	mount := containerMount{
		Type:        "bind",
		Source:      "/containers/storage/overlay/merged",
		Destination: destination,
		Options:     []string{"lowerdir=" + lower},
		Writable:    true,
	}
	for _, option := range strings.Split(options, ",") {
		if option != "O" {
			mount.Options = append(mount.Options, option)
		}
	}
	return mount
}

func flagValue(args []string, flag string) string {
	for index := range args {
		if args[index] == flag && index+1 < len(args) {
			return args[index+1]
		}
	}
	return ""
}

func activeManifest(
	t *testing.T,
	store runstate.Store,
	caches ...runcontainer.CacheMount,
) runstate.Manifest {
	t.Helper()
	prepared := testPrepared()
	spec := runcontainer.DefaultSpec(prepared.Snapshot.RunID, testProject.Key, testImage)
	spec.Caches = caches
	if _, err := store.Create(runstate.Manifest{
		RunID:              spec.RunID,
		Project:            "project",
		ProjectKey:         testProject.Key,
		Snapshot:           prepared.Snapshot,
		Image:              spec.ImageID,
		Container:          spec.ContainerName(),
		Workspace:          "/work/project",
		Caches:             spec.Caches,
		ActiveLimitSeconds: spec.WallSeconds,
	}); err != nil {
		t.Fatal(err)
	}
	manifest, err := store.Activate(
		spec.RunID,
		runstate.SSHConnection{
			Alias:              "pisafe-" + spec.RunID,
			IdentityFile:       "/state/ssh/" + spec.RunID + "/id_ed25519",
			KnownHostsFile:     "/state/ssh/" + spec.RunID + "/known_hosts",
			ConfigFile:         "/state/ssh/" + spec.RunID + "/ssh.config",
			HostKeyFingerprint: "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		},
		gitstage.Snapshot{BaselineCommit: strings.Repeat("b", 40)},
		"pisafe-cap-"+strings.Repeat("ab", 32),
		time.Time{},
	)
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}
