package lima

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mpizenberg/pisafe/internal/gitstage"
	"github.com/mpizenberg/pisafe/internal/runcontainer"
	"github.com/mpizenberg/pisafe/internal/runssh"
)

// liveProjectKey is the project every live run in this package belongs to. It
// is shaped like a real key so the shared filesystem it names is the same kind
// of object a run creates.
const liveProjectKey = "live-0000live"

func TestLiveSSHStageAndContainerMaterialize(t *testing.T) {
	if os.Getenv("PISAFE_LIVE_LIMA") != "1" {
		t.Skip("set PISAFE_LIVE_LIMA=1 to run persistent Lima integration tests")
	}
	imageID := os.Getenv("PISAFE_LIVE_RUN_IMAGE")
	if imageID == "" {
		t.Skip("set PISAFE_LIVE_RUN_IMAGE to an immutable sha256 image ID")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	runID := "live-stage-" + time.Now().UTC().Format("20060102-150405")
	source := initLiveRepository(t)
	packageDirectory := filepath.Join(t.TempDir(), "stage")
	prepared, err := gitstage.Prepare(ctx, gitstage.PrepareRequest{
		SourcePath: source,
		PackageDir: packageDirectory,
		RunID:      runID,
	})
	if err != nil {
		t.Fatal(err)
	}

	transport := NewTransport()
	_, err = transport.CreateStage(ctx, prepared)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := transport.RemoveRun(context.Background(), runID); err != nil {
			t.Errorf("remove remote run: %v", err)
		}
	}()

	spec := runcontainer.DefaultSpec(runID, liveProjectKey, imageID)
	if err := transport.EnsureProjectStorage(ctx, liveProjectKey); err != nil {
		t.Fatal(err)
	}
	if err := transport.CreateRunStorage(ctx, runID); err != nil {
		t.Fatal(err)
	}
	if err := transport.PrepareRunOverlays(ctx, runID, spec.Caches); err != nil {
		t.Fatal(err)
	}
	defer cleanupLiveContainer(t, transport, spec)
	if err := transport.ImportStage(ctx, runID); err != nil {
		t.Fatal(err)
	}

	sshStore := runssh.NewStore(filepath.Join(t.TempDir(), "ssh"))
	preparedSSH, err := sshStore.Prepare(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := sshStore.Remove(runID); err != nil {
			t.Errorf("remove live SSH state: %v", err)
		}
	}()
	configureSSHArgs, err := spec.ConfigureSSHArgs()
	if err != nil {
		t.Fatal(err)
	}
	hostPublicKey, err := transport.Execute(
		ctx,
		strings.NewReader(preparedSSH.PublicKey+"\n"),
		append([]string{"podman"}, configureSSHArgs...)...,
	)
	if err != nil {
		t.Fatal(err)
	}

	runArgs, err := spec.RunArgs()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transport.Execute(ctx, nil, append([]string{"podman"}, runArgs...)...); err != nil {
		t.Fatal(err)
	}
	publishedPorts, err := transport.Execute(
		ctx,
		nil,
		"podman", "port", spec.ContainerName(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(publishedPorts)) != "" {
		t.Fatalf("run container exposed VM ports: %s", publishedPorts)
	}
	gateway, err := transport.SSHGateway(ctx)
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := sshStore.Finalize(
		preparedSSH,
		string(hostPublicKey),
		gateway,
		spec.ContainerName(),
	)
	if err != nil {
		t.Fatal(err)
	}
	materializeArgs, err := spec.MaterializeArgs("project")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transport.Execute(
		ctx,
		nil,
		append([]string{"podman"}, materializeArgs...)...,
	); err != nil {
		t.Fatal(err)
	}
	cleanupStageArgs, err := spec.CleanupStageArgs()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transport.Execute(
		ctx,
		nil,
		append([]string{"podman"}, cleanupStageArgs...)...,
	); err != nil {
		t.Fatal(err)
	}

	assertions := []string{
		"test ! -e /Users",
		"test ! -e /run/podman/podman.sock",
		`test "$(id -u)" = 1000`,
		`test "$(awk '/^CapEff:/ {print $2}' /proc/self/status)" = 0000000000000000`,
		`test "$(awk '/^NoNewPrivs:/ {print $2}' /proc/self/status)" = 1`,
		`test "$(cat /sys/fs/cgroup/memory.max)" = 4294967296`,
		`test "$(cat /sys/fs/cgroup/pids.max)" = 512`,
		`test "$(git -C /work/project status --short)" = ""`,
		`test "$(cat /work/project/tracked.txt)" = changed`,
		"test ! -e /work/stage",
	}
	execArgs := []string{
		"podman", "exec",
		"--user", "1000:1000",
		spec.ContainerName(),
		"sh", "-ceu", strings.Join(assertions, "\n"),
	}
	if _, err := transport.Execute(ctx, nil, execArgs...); err != nil {
		t.Fatal(err)
	}

	sshOutput, err := waitForLiveSSH(ctx, endpoint, `set -eu
test "$(id -u)" = 1000
test -z "${SSH_AUTH_SOCK-}"
test ! -e /Users
test "$(cat /work/project/tracked.txt)" = changed
printf 'from-zed-compatible-ssh\n' > /work/project/zed-visible.txt
printf '%s\n' "$HOME"
`)
	if err != nil {
		logs, _ := transport.Execute(
			ctx,
			nil,
			"podman", "logs", spec.ContainerName(),
		)
		state, _ := transport.Execute(
			ctx,
			nil,
			"podman", "inspect",
			"--format", "{{.State.Status}} {{.State.ExitCode}}",
			spec.ContainerName(),
		)
		t.Fatalf("SSH connection: %v\ncontainer: %s\nlogs:\n%s", err, state, logs)
	}
	if strings.TrimSpace(sshOutput) != "/home/node" {
		t.Fatalf("SSH HOME = %q", sshOutput)
	}
	if _, err := transport.Execute(
		ctx,
		nil,
		"podman", "exec",
		"--user", "1000:1000",
		spec.ContainerName(),
		"sh", "-ceu",
		`test "$(cat /work/project/zed-visible.txt)" = from-zed-compatible-ssh
test -x "$(command -v pi)"
pi --version >/dev/null`,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := transport.Execute(
		ctx,
		nil,
		"podman", "exec",
		"--user", "1000:1000",
		spec.ContainerName(),
		"rm", "-rf", "/work/project",
	); err != nil {
		t.Fatal(err)
	}

	hostContent, err := os.ReadFile(filepath.Join(source, "tracked.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(hostContent) != "changed\n" {
		t.Fatalf("original checkout changed: %q", hostContent)
	}
}

func waitForLiveSSH(
	ctx context.Context,
	endpoint runssh.Endpoint,
	script string,
) (string, error) {
	var lastOutput []byte
	var lastErr error
	for attempt := 0; attempt < 10; attempt++ {
		command := exec.CommandContext(
			ctx,
			"ssh",
			"-F", endpoint.ConfigFile,
			"-o", "ConnectTimeout=2",
			endpoint.Alias,
			"sh", "-s",
		)
		command.Stdin = strings.NewReader(script)
		lastOutput, lastErr = command.CombinedOutput()
		if lastErr == nil {
			return string(lastOutput), nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return "", errors.New(string(lastOutput) + ": " + lastErr.Error())
}

func initLiveRepository(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	liveGit(t, root, "init", "--quiet")
	liveGit(t, root, "config", "user.name", "pisafe live test")
	liveGit(t, root, "config", "user.email", "pisafe@localhost.invalid")
	liveGit(t, root, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("original\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	liveGit(t, root, "add", "tracked.txt")
	liveGit(t, root, "commit", "--quiet", "-m", "initial")
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func liveGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", directory}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func cleanupLiveContainer(t *testing.T, transport Transport, spec runcontainer.Spec) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, containerErr := transport.Execute(
		ctx,
		nil,
		"podman", "rm", "--force", spec.ContainerName(),
	)
	storageErr := transport.RemoveRunStorage(ctx, spec.RunID)
	if containerErr != nil {
		t.Errorf("remove live container: %v", containerErr)
	}
	if storageErr != nil {
		t.Errorf("remove live storage: %v", storageErr)
	}
}
