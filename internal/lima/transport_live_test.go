package lima

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mpizenberg/pisafe/internal/gitstage"
	"github.com/mpizenberg/pisafe/internal/runcontainer"
)

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
	prepared, err := gitstage.Prepare(ctx, source, packageDirectory, runID)
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

	spec := runcontainer.DefaultSpec(runID, imageID)
	volumeCommands, err := spec.CreateVolumeArgs()
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range volumeCommands {
		if _, err := transport.Execute(ctx, nil, append([]string{"podman"}, command...)...); err != nil {
			t.Fatal(err)
		}
	}
	defer cleanupLiveContainer(t, transport, spec)
	if err := transport.ImportStage(ctx, runID, spec.WorkspaceVolume()); err != nil {
		t.Fatal(err)
	}

	runArgs, err := spec.RunArgs()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transport.Execute(ctx, nil, append([]string{"podman"}, runArgs...)...); err != nil {
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
		"rm -rf /work/project",
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

	hostContent, err := os.ReadFile(filepath.Join(source, "tracked.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(hostContent) != "changed\n" {
		t.Fatalf("original checkout changed: %q", hostContent)
	}
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
	_, volumeErr := transport.Execute(
		ctx,
		nil,
		"podman", "volume", "rm", spec.WorkspaceVolume(), spec.HomeVolume(),
	)
	if containerErr != nil {
		t.Errorf("remove live container: %v", containerErr)
	}
	if volumeErr != nil {
		t.Errorf("remove live volumes: %v", volumeErr)
	}
}
