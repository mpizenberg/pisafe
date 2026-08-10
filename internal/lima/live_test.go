package lima_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/mpizenberg/pisafe/internal/hostnet"
	"github.com/mpizenberg/pisafe/internal/lima"
)

// TestLiveCreateAndStart provisions the persistent dedicated VM. It is gated
// because it downloads an image and changes Lima state on the host.
func TestLiveCreateAndStart(t *testing.T) {
	if os.Getenv("PISAFE_LIVE_LIMA") != "1" {
		t.Skip("set PISAFE_LIVE_LIMA=1 to create and start the dedicated VM")
	}

	ensureLiveVM(t)
}

func TestLiveContainerNetworkBoundary(t *testing.T) {
	if os.Getenv("PISAFE_LIVE_LIMA") != "1" {
		t.Skip("set PISAFE_LIVE_LIMA=1 to test the dedicated VM")
	}
	ensureLiveVM(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	runLive(t, ctx, "sh", "-ec", `
test ! -e /Users
test ! -S /run/user/"$(id -u)"/podman/podman.sock
test "$(sysctl -n net.ipv6.conf.all.disable_ipv6)" = 1
test "$(podman unshare cat /proc/self/uid_map | wc -l)" -ge 2
systemctl is-active --quiet pisafe-firewall.service
sudo /usr/local/sbin/pisafe-firewall-status >/dev/null
sudo /usr/local/sbin/pisafe-clock-step
if sudo -n true 2>/dev/null; then
  echo "Lima user still has unrestricted passwordless sudo" >&2
  exit 1
fi
test -r /etc/pisafe/security-profile
test "$(stat -c '%a:%U:%G' /etc/pisafe/security-profile)" = "444:root:root"
`)
	runLive(t, ctx, "podman", "run", "--rm", "docker.io/library/alpine:3.22", "sh", "-ec", `
wget -qO- https://example.com >/dev/null
for target in 10.0.0.1 169.254.169.254; do
  if nc -z -w 2 "${target}" 80; then
    echo "denied destination is reachable: ${target}" >&2
    exit 1
  fi
done
`)
}

func ensureLiveVM(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	prefixes, err := hostnet.OnLinkIPv4(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := lima.NewManager().Ensure(ctx, prefixes); err != nil {
		t.Fatal(err)
	}
}

func runLive(t *testing.T, ctx context.Context, arguments ...string) string {
	t.Helper()
	commandArguments := append([]string{"shell", lima.InstanceName}, arguments...)
	command := exec.CommandContext(ctx, "limactl", commandArguments...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf(
			"limactl %s: %v\n%s",
			strings.Join(commandArguments, " "),
			err,
			output,
		)
	}
	if testing.Verbose() && len(output) != 0 {
		fmt.Printf("%s", output)
	}
	return strings.TrimSpace(string(output))
}
