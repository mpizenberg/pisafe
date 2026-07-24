package runctl

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mpizenberg/pisafe/internal/gitstage"
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
	calls  []backendCall
	failAt string
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
	_ string,
) error {
	backend.calls = append(backend.calls, backendCall{kind: "import"})
	if backend.failAt == "import" {
		return errors.New("import failed")
	}
	return nil
}

func (backend *fakeBackend) Execute(
	_ context.Context,
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
	if strings.Contains(strings.Join(args, " "), "pisafe-guest configure-ssh") {
		return []byte("ssh-ed25519 host-key\n"), nil
	}
	if strings.Contains(strings.Join(args, " "), "pisafe-guest materialize") {
		materialized := testPrepared().Snapshot
		materialized.SourceRoot = ""
		materialized.BaselineCommit = strings.Repeat("b", 40)
		output, err := json.Marshal(materialized)
		return append(output, '\n'), err
	}
	return nil, nil
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
	controller := New(backend, store, ssh)

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
		"podman volume create",
		"import",
		"pisafe-guest configure-ssh",
		"podman run",
		"ssh-gateway",
		"pisafe-guest materialize",
		"rm -rf /work/stage",
		"remove-stage",
	} {
		if !strings.Contains(joined, expected) {
			t.Errorf("calls lack %q:\n%s", expected, joined)
		}
	}
	if manifest.SSH == nil ||
		manifest.SSH.Alias != "pisafe-run-123" {
		t.Fatalf("manifest SSH = %#v", manifest.SSH)
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
	controller := New(backend, store, ssh)

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
		"podman rm --force pisafe-run-run-123",
		"podman volume rm --force pisafe-work-run-123",
		"podman volume rm --force pisafe-home-run-123",
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

func TestStartPreparedDoesNotDeleteVolumeItDidNotCreate(t *testing.T) {
	backend := &fakeBackend{failAt: "pisafe-home-run-123"}
	store := runstate.NewStore(t.TempDir())
	controller := New(backend, store, &fakeSSHStore{})

	if _, err := controller.StartPrepared(
		context.Background(),
		testPrepared(),
		"project",
		testImage,
	); err == nil {
		t.Fatal("StartPrepared unexpectedly succeeded")
	}
	joined := callsString(backend.calls)
	if !strings.Contains(joined, "podman volume rm --force pisafe-work-run-123") {
		t.Fatalf("created workspace volume was not removed:\n%s", joined)
	}
	if strings.Contains(joined, "podman volume rm --force pisafe-home-run-123") {
		t.Fatalf("controller tried to remove a volume it did not create:\n%s", joined)
	}
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
