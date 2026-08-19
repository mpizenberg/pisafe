package lima

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mpizenberg/pisafe/internal/gitstage"
	"github.com/mpizenberg/pisafe/internal/runcontainer"
	"github.com/mpizenberg/pisafe/internal/runssh"
)

const testWallSeconds = int64(8 * 60 * 60)

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

	vm := New()
	_, err = vm.CreateStage(ctx, prepared)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := vm.RemoveRun(context.Background(), runID); err != nil {
			t.Errorf("remove remote run: %v", err)
		}
	}()

	spec := runcontainer.DefaultSpec(runID, liveProjectKey, imageID)
	if err := vm.EnsureProjectStorage(ctx, liveProjectKey); err != nil {
		t.Fatal(err)
	}
	if err := vm.CreateRunStorage(ctx, runID); err != nil {
		t.Fatal(err)
	}
	if err := vm.PrepareRunLayout(ctx, runID, spec.Caches); err != nil {
		t.Fatal(err)
	}
	defer cleanupLiveContainer(t, vm, spec)
	if err := vm.ImportStage(ctx, runID); err != nil {
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
	hostPublicKey, err := vm.Execute(
		ctx,
		strings.NewReader(preparedSSH.PublicKey+"\n"),
		append([]string{"podman"}, configureSSHArgs...)...,
	)
	if err != nil {
		t.Fatal(err)
	}

	runArgs, err := spec.RunArgs(testWallSeconds)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := vm.Execute(ctx, nil, append([]string{"podman"}, runArgs...)...); err != nil {
		t.Fatal(err)
	}
	publishedPorts, err := vm.Execute(
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
	gateway, err := vm.SSHGateway(ctx)
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
	if _, err := vm.Execute(
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
	if _, err := vm.Execute(
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
	if _, err := vm.Execute(ctx, nil, execArgs...); err != nil {
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
		logs, _ := vm.Execute(
			ctx,
			nil,
			"podman", "logs", spec.ContainerName(),
		)
		state, _ := vm.Execute(
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
	if _, err := vm.Execute(
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
	assertLiveForward(t, ctx, vm, spec, endpoint)

	if _, err := vm.Execute(
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

// assertLiveForward proves the one way into a run from this Mac. Nothing is
// published in the VM or on macOS and the VM firewall drops inbound traffic
// that is not SSH, so a server the run hosts is reachable only as a channel on
// the run's own SSH connection — and only at the run's loopback, because a
// forward pointed anywhere else would make this Mac's loopback a way to ask the
// run to reach things on its behalf.
func assertLiveForward(
	t *testing.T,
	ctx context.Context,
	vm VM,
	spec runcontainer.Spec,
	endpoint runssh.Endpoint,
) {
	t.Helper()
	const remotePort = 8099
	const payload = "pisafe-forwarded"
	if _, err := vm.Execute(
		ctx,
		nil,
		"podman", "exec", "--detach", "--user", "1000:1000",
		spec.ContainerName(),
		"node", "-e", fmt.Sprintf(
			`require("http").createServer((_, r) => r.end(%q)).listen(%d, "127.0.0.1")`,
			payload,
			remotePort,
		),
	); err != nil {
		t.Fatal(err)
	}

	local := freeLocalPort(t)
	forward, stderr := startLiveForward(t, ctx, endpoint, fmt.Sprintf(
		"127.0.0.1:%d:127.0.0.1:%d",
		local,
		remotePort,
	))
	served := ""
	for attempt := 0; attempt < 20 && served != payload; attempt++ {
		if body, err := fetchLocal(ctx, local); err == nil {
			served = body
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
	if served != payload {
		t.Fatalf("forwarded port served %q, want %q\nssh: %s", served, payload, stderr)
	}
	forward()

	// The same run, the same key, one address over: sshd refuses to open it.
	denied := freeLocalPort(t)
	forward, stderr = startLiveForward(t, ctx, endpoint, fmt.Sprintf(
		"127.0.0.1:%d:127.0.0.2:%d",
		denied,
		remotePort,
	))
	for attempt := 0; attempt < 20; attempt++ {
		if _, err := fetchLocal(ctx, denied); err != nil &&
			strings.Contains(stderr.String(), "administratively prohibited") {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
	forward()
	if !strings.Contains(stderr.String(), "administratively prohibited") {
		t.Errorf("a forward off the run's loopback was not refused: %s", stderr)
	}
}

// startLiveForward runs the client that holds one forward and returns the call
// that ends it. Its standard error is what sshd's refusals come back on, so it
// is collected rather than discarded.
func startLiveForward(
	t *testing.T,
	ctx context.Context,
	endpoint runssh.Endpoint,
	forward string,
) (func(), *safeBuffer) {
	t.Helper()
	command := exec.CommandContext(
		ctx,
		"ssh",
		"-F", endpoint.ConfigFile,
		"-N", "-T",
		"-o", "ClearAllForwardings=no",
		"-o", "ExitOnForwardFailure=yes",
		"-L", forward,
		endpoint.Alias,
	)
	stderr := &safeBuffer{}
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		_ = command.Process.Kill()
		_ = command.Wait()
	}
	t.Cleanup(stop)
	return stop, stderr
}

// safeBuffer collects a running child's standard error while the test reads it.
type safeBuffer struct {
	mutex   sync.Mutex
	content strings.Builder
}

func (buffer *safeBuffer) Write(content []byte) (int, error) {
	buffer.mutex.Lock()
	defer buffer.mutex.Unlock()
	return buffer.content.Write(content)
}

func (buffer *safeBuffer) String() string {
	buffer.mutex.Lock()
	defer buffer.mutex.Unlock()
	return buffer.content.String()
}

func freeLocalPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func fetchLocal(ctx context.Context, port int) (string, error) {
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		fmt.Sprintf("http://127.0.0.1:%d/", port),
		nil,
	)
	if err != nil {
		return "", err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1024))
	if err != nil {
		return "", err
	}
	return string(body), nil
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

func cleanupLiveContainer(t *testing.T, vm VM, spec runcontainer.Spec) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, containerErr := vm.Execute(
		ctx,
		nil,
		"podman", "rm", "--force", spec.ContainerName(),
	)
	storageErr := vm.RemoveRunStorage(ctx, spec.RunID)
	if containerErr != nil {
		t.Errorf("remove live container: %v", containerErr)
	}
	if storageErr != nil {
		t.Errorf("remove live storage: %v", storageErr)
	}
}
