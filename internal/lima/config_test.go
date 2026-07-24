package lima

import (
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderConfigContainsSecurityBoundary(t *testing.T) {
	config, err := RenderConfig(DefaultConfigOptions([]netip.Prefix{
		netip.MustParsePrefix("203.0.113.8/24"),
		netip.MustParsePrefix("192.168.4.2/24"),
		netip.MustParsePrefix("192.168.4.1/32"),
	}))
	if err != nil {
		t.Fatal(err)
	}
	text := string(config)

	required := []string{
		"plain: true",
		"mounts: []",
		"forwardAgent: false",
		"propagateProxyEnv: false",
		"digest: sha256:",
		"ip daddr @fixed_denied_v4 reject",
		"ip daddr @host_onlink_v4 reject",
		"type filter hook output",
		"type filter hook forward",
		"elements = { 192.168.4.0/24, 203.0.113.0/24 }",
		"net.ipv6.conf.all.disable_ipv6 = 1",
		"PermitListen 192.0.2.1:*",
		"meta skuid 0 udp sport 68 udp dport 67 accept",
		"sudo /usr/local/sbin/pisafe-firewall-status",
		"usermod --add-subuids 100000-165535",
		"podman system migrate",
		"podman unshare cat /proc/self/uid_map",
		"/etc/pisafe/host-prefixes",
		"pisafe-clock-step",
		`sed -i "\|^${pisafe_user} .*NOPASSWD:ALL|d"`,
		`gpasswd --delete "${pisafe_user}" wheel`,
	}
	for _, fragment := range required {
		if !strings.Contains(text, fragment) {
			t.Errorf("config does not contain %q", fragment)
		}
	}
	if strings.Contains(text, "pisafe-firewall-refresh") {
		t.Error("config grants a runtime firewall mutation path")
	}
}

func TestRenderConfigRequiresHostNetworks(t *testing.T) {
	_, err := RenderConfig(DefaultConfigOptions(nil))
	if err == nil {
		t.Fatal("RenderConfig unexpectedly accepted no host networks")
	}
}

func TestGeneratedConfigValidatesWithInstalledLima(t *testing.T) {
	limactl, err := exec.LookPath("limactl")
	if err != nil {
		t.Skip("limactl is not installed")
	}
	config, err := RenderConfig(DefaultConfigOptions([]netip.Prefix{
		netip.MustParsePrefix("192.168.4.0/24"),
	}))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "pisafe.yaml")
	if err := os.WriteFile(path, config, 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(limactl, "template", "validate", path)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("limactl template validate: %v\n%s", err, output)
	}
}
