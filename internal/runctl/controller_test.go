package runctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mpizenberg/pisafe/internal/gitstage"
	"github.com/mpizenberg/pisafe/internal/runcontainer"
	"github.com/mpizenberg/pisafe/internal/runssh"
	"github.com/mpizenberg/pisafe/internal/runstate"
)

const testImage = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type backendCall struct {
	kind  string
	args  []string
	stdin string
}

type fakeBackend struct {
	calls       []backendCall
	failAt      string
	failAfterAt string
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

func (backend *fakeBackend) CreateStorage(_ context.Context, _ string) error {
	backend.calls = append(backend.calls, backendCall{kind: "create-storage"})
	if backend.failAt == "create-storage" {
		return errors.New("storage failed")
	}
	return nil
}

func (backend *fakeBackend) VerifyStorage(_ context.Context, _ string) error {
	backend.calls = append(backend.calls, backendCall{kind: "verify-storage"})
	if backend.failAt == "verify-storage" {
		return errors.New("verify storage failed")
	}
	return nil
}

func (backend *fakeBackend) RemoveStorage(_ context.Context, _ string) error {
	backend.calls = append(backend.calls, backendCall{kind: "remove-storage"})
	if backend.failAt == "remove-storage" {
		return errors.New("remove storage failed")
	}
	return nil
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
		prepared, err := gitstage.PrepareApply(
			ctx,
			snapshot,
			backend.applyWorkspace,
			backend.applyPackage,
		)
		if err != nil {
			return nil, err
		}
		output, err := json.Marshal(prepared)
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
		"project",
		testImage,
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
		"create-storage",
		"import",
		"pisafe-guest configure-ssh",
		"podman run",
		"ssh-gateway",
		"pisafe-guest materialize",
		"rm -rf /work/stage",
		"remove-stage",
		"pisafe-guest configure-inference",
	} {
		if !strings.Contains(joined, expected) {
			t.Errorf("calls lack %q:\n%s", expected, joined)
		}
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
		"project",
		testImage,
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
		"project",
		testImage,
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
	spec := specForManifest(manifest)
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
	spec := specForManifest(manifest)
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
				spec := runcontainer.DefaultSpec(prepared.Snapshot.RunID, testImage)
				var err error
				manifest, err = store.Create(runstate.Manifest{
					RunID:              spec.RunID,
					Project:            "project",
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
				spec := specForManifest(manifest)
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
			discarded, err := controller.Discard(context.Background(), manifest.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if discarded.State != runstate.StateDiscarded || !ssh.removed {
				t.Fatalf("discarded = %#v, SSH removed = %t", discarded, ssh.removed)
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
	spec := specForManifest(manifest)
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
		if arg != "--mount" || index+1 >= len(args) {
			continue
		}
		parts := strings.Split(args[index+1], ",")
		mount := struct {
			Type        string `json:"Type"`
			Source      string `json:"Source"`
			Destination string `json:"Destination"`
		}{}
		for _, part := range parts {
			key, value, found := strings.Cut(part, "=")
			if !found {
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
		inspection.Mounts = append(inspection.Mounts, mount)
	}
	return inspection
}

func flagValue(args []string, flag string) string {
	for index := range args {
		if args[index] == flag && index+1 < len(args) {
			return args[index+1]
		}
	}
	return ""
}

func activeManifest(t *testing.T, store runstate.Store) runstate.Manifest {
	t.Helper()
	prepared := testPrepared()
	spec := runcontainer.DefaultSpec(prepared.Snapshot.RunID, testImage)
	if _, err := store.Create(runstate.Manifest{
		RunID:              spec.RunID,
		Project:            "project",
		Snapshot:           prepared.Snapshot,
		Image:              spec.ImageID,
		Container:          spec.ContainerName(),
		Workspace:          "/work/project",
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
		strings.Repeat("b", 40),
		"pisafe-cap-"+strings.Repeat("ab", 32),
		time.Time{},
	)
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}
