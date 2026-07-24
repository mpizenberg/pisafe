package runctl

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/mpizenberg/pisafe/internal/gitstage"
	"github.com/mpizenberg/pisafe/internal/runstate"
)

const testImage = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type backendCall struct {
	kind string
	args []string
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
	_ io.Reader,
	args ...string,
) ([]byte, error) {
	backend.calls = append(backend.calls, backendCall{
		kind: "execute",
		args: append([]string(nil), args...),
	})
	if backend.failAt != "" && strings.Contains(strings.Join(args, " "), backend.failAt) {
		return nil, errors.New("execute failed")
	}
	return nil, nil
}

func (backend *fakeBackend) RemoveRun(_ context.Context, _ string) error {
	backend.calls = append(backend.calls, backendCall{kind: "remove-stage"})
	return nil
}

func TestStartPreparedActivatesOnlyAfterMaterialization(t *testing.T) {
	backend := &fakeBackend{}
	store := runstate.NewStore(t.TempDir())
	controller := New(backend, store)

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
		"podman run",
		"pisafe-guest materialize",
		"rm -rf /work/stage",
		"remove-stage",
	} {
		if !strings.Contains(joined, expected) {
			t.Errorf("calls lack %q:\n%s", expected, joined)
		}
	}
}

func TestStartPreparedRollsBackAndRecordsFailure(t *testing.T) {
	backend := &fakeBackend{failAt: "pisafe-guest materialize"}
	store := runstate.NewStore(t.TempDir())
	controller := New(backend, store)

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
}

func TestStartPreparedDoesNotDeleteVolumeItDidNotCreate(t *testing.T) {
	backend := &fakeBackend{failAt: "pisafe-home-run-123"}
	store := runstate.NewStore(t.TempDir())
	controller := New(backend, store)

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
