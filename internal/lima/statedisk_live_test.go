package lima_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mpizenberg/pisafe/internal/hostnet"
	"github.com/mpizenberg/pisafe/internal/lima"
)

const (
	stateProbeInstance = "pisafeprobe"
	stateProbeDisk     = "pisafe-probe"
	stateProbeRun      = "probestate0001"
	stateProbeMissing  = "probestate0002"
)

// TestLiveStateDiskOutlivesTheInstance proves what the state disk is for.
// Recreating the VM is the cure every boundary check prescribes, and it has to
// leave a run's storage where it is for the next VM to mount back. The test
// drives a throwaway instance and a throwaway disk, because proving the
// property means destroying the VM that holds them.
func TestLiveStateDiskOutlivesTheInstance(t *testing.T) {
	if os.Getenv("PISAFE_LIVE_STATE_DISK") != "1" {
		t.Skip("set PISAFE_LIVE_STATE_DISK=1 to create and destroy a throwaway VM")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	configPath := writeStateProbeConfig(t, ctx)
	t.Cleanup(removeStateProbe)
	limactlProbe(t, ctx, "disk", "create", stateProbeDisk, "--size", lima.StateDiskSize)

	startStateProbe(t, ctx, configPath)
	assertStateProbeMount(t, ctx)
	shellStateProbe(t, ctx, "sudo", "/usr/local/sbin/pisafe-storage", "create", "run", stateProbeRun)

	limactlProbe(t, ctx, "--tty=false", "stop", stateProbeInstance)
	limactlProbe(t, ctx, "--tty=false", "delete", stateProbeInstance)
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".lima", stateProbeInstance)); !os.IsNotExist(err) {
		t.Fatalf("instance directory outlived the delete: %v", err)
	}

	// A second instance has never seen the disk, so what it finds there is what
	// the disk kept rather than what this boot created.
	startStateProbe(t, ctx, configPath)
	assertStateProbeMount(t, ctx)
	shellStateProbe(t, ctx, "sudo", "/usr/local/sbin/pisafe-storage", "verify", "run", stateProbeRun)

	// The same check against storage that was never created has to fail, or
	// the one above proves only that the helper is lenient.
	command := exec.CommandContext(
		ctx,
		"limactl", "shell", stateProbeInstance,
		"sudo", "/usr/local/sbin/pisafe-storage", "verify", "run", stateProbeMissing,
	)
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("storage that was never created verified:\n%s", output)
	}
}

// writeStateProbeConfig renames the disk the generated configuration asks for.
// One placeholder produces the Lima disk name, the filesystem label, and the
// device lookup, so renaming it here keeps every one of them off the disk the
// dedicated VM uses — which this test deletes an instance to test.
func writeStateProbeConfig(t *testing.T, ctx context.Context) string {
	t.Helper()
	prefixes, err := hostnet.OnLinkIPv4(ctx)
	if err != nil {
		t.Fatal(err)
	}
	config, err := lima.RenderConfig(prefixes)
	if err != nil {
		t.Fatal(err)
	}
	renamed := strings.ReplaceAll(string(config), lima.StateDiskName, stateProbeDisk)
	if strings.Contains(renamed, lima.StateDiskName) {
		t.Fatalf("probe configuration still names the dedicated state disk")
	}
	path := filepath.Join(t.TempDir(), "pisafe-probe.yaml")
	if err := lima.WriteConfig(path, []byte(renamed)); err != nil {
		t.Fatal(err)
	}
	return path
}

func startStateProbe(t *testing.T, ctx context.Context, configPath string) {
	t.Helper()
	limactlProbe(t, ctx, "--tty=false", "create", "--name="+stateProbeInstance, configPath)
	limactlProbe(t, ctx, "--tty=false", "start", stateProbeInstance)
}

// assertStateProbeMount reads the mount the way an unprivileged process can,
// and insists it is the labelled disk rather than a directory the instance's
// own filesystem happened to provide.
func assertStateProbeMount(t *testing.T, ctx context.Context) {
	t.Helper()
	label := shellStateProbe(t, ctx, "sh", "-ec",
		`lsblk --noheadings --output LABEL "$(findmnt -n -o SOURCE --target /var/lib/pisafe)"`)
	if label != stateProbeDisk {
		t.Fatalf("/var/lib/pisafe is backed by %q, want the %q disk", label, stateProbeDisk)
	}
}

func limactlProbe(t *testing.T, ctx context.Context, arguments ...string) string {
	t.Helper()
	command := exec.CommandContext(ctx, "limactl", arguments...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("limactl %s: %v\n%s", strings.Join(arguments, " "), err, output)
	}
	if testing.Verbose() && len(output) != 0 {
		fmt.Printf("%s", output)
	}
	return strings.TrimSpace(string(output))
}

func shellStateProbe(t *testing.T, ctx context.Context, arguments ...string) string {
	t.Helper()
	return limactlProbe(t, ctx, append([]string{"shell", stateProbeInstance}, arguments...)...)
}

// removeStateProbe runs whatever the test reached, so a failure partway leaves
// no instance and no disk behind. Lima reports an already-absent one as an
// error, which is the state this wants.
func removeStateProbe() {
	for _, arguments := range [][]string{
		{"--tty=false", "stop", "--force", stateProbeInstance},
		{"--tty=false", "delete", "--force", stateProbeInstance},
		{"disk", "unlock", stateProbeDisk},
		{"disk", "delete", "--force", stateProbeDisk},
	} {
		_ = exec.Command("limactl", arguments...).Run()
	}
}
