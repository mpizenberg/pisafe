package lima

import (
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestRenderConfigContainsSecurityBoundary(t *testing.T) {
	config, err := RenderConfig([]netip.Prefix{
		netip.MustParsePrefix("203.0.113.8/24"),
		netip.MustParsePrefix("192.168.4.2/24"),
		netip.MustParsePrefix("192.168.4.1/32"),
	})
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
		"PermitListen 192.0.2.1:18080",
		"ip daddr 192.0.2.1 tcp dport 18080 accept",
		"ip addr replace 192.0.2.1/32 dev pisafe-broker",
		"meta skuid 0 udp sport 68 udp dport 67 accept",
		"sudo /usr/local/sbin/pisafe-firewall-status",
		"usermod --add-subuids 100000-165535",
		"podman system migrate",
		"podman unshare cat /proc/self/uid_map",
		"/etc/pisafe/host-prefixes",
		"/etc/pisafe/security-profile",
		"sha256:",
		"pisafe-clock-step",
		"pisafe-storage",
		"mkfs.ext4 -q -F -m 0",
		"mount -o loop,nodev,nosuid",
		`if [[ -e "$run_root" || -L "$run_root" ]]`,
		`if [[ -e "$image" || -L "$image" ]]`,
		`storage_bytes=10737418240`,
		"directories=(workspace home overlay)",
		"directories=(cache sessions)",
		`storage_uid="$((subuid_start + 999))"`,
		"/var/lib/pisafe/runs",
		"/var/lib/pisafe/run-images",
		"/var/lib/pisafe/projects",
		"/var/lib/pisafe/project-images",
		`- name: "pisafe-state"`,
		"blkid --label pisafe-state",
		"mkfs.ext4 -q -F -L pisafe-state",
		`mount -o nodev,nosuid "$state_device" /var/lib/pisafe`,
		"mountpoint -q /var/lib/pisafe",
		`sed -i "\|^${pisafe_user} .*NOPASSWD:ALL|d"`,
		`gpasswd --delete "${pisafe_user}" wheel`,
	}
	for _, fragment := range required {
		if !strings.Contains(text, fragment) {
			t.Errorf("config does not contain %q", fragment)
		}
	}
	if count := strings.Count(text, "ct state established,related accept"); count != 3 {
		t.Errorf("%d stateful chains, want input, output, and forward", count)
	}
	if strings.Contains(text, "pisafe-firewall-refresh") {
		t.Error("config grants a runtime firewall mutation path")
	}
	if strings.Contains(text, "broker_ports") {
		t.Error("config retains a mutable broker port set")
	}
	if strings.Contains(text, "@@") {
		t.Error("config retains an unreplaced template value")
	}
	if strings.Contains(text, "/dev/vd") {
		t.Error("config names a disk by device path, which boot order decides")
	}
	if !strings.Contains(text, `[[ "${#candidates[@]}" -eq 1 ]]`) {
		t.Error("config formats a disk without proving exactly one is unclaimed")
	}
	mountIndex := strings.Index(text, `mount -o nodev,nosuid "$state_device" /var/lib/pisafe`)
	rootsIndex := strings.Index(text, "/var/lib/pisafe/runs /var/lib/pisafe/projects")
	if mountIndex < 0 || rootsIndex < 0 || mountIndex > rootsIndex {
		t.Error("storage roots are created before the state disk is mounted")
	}
	trapIndex := strings.Index(text, "trap 'cleanup_partial || true' ERR")
	truncateIndex := strings.Index(text, `truncate -s "$storage_bytes" "$image"`)
	if trapIndex < 0 || truncateIndex < 0 || trapIndex > truncateIndex {
		t.Error("storage cleanup trap is not installed before image allocation")
	}
}

func TestSecurityProfileChangesWithTemplateOrNetworks(t *testing.T) {
	first := securityProfileDigest([]string{"192.168.4.0/24"})
	equivalent := securityProfileDigest([]string{"192.168.4.0/24"})
	different := securityProfileDigest([]string{"10.20.30.0/24"})
	if first != equivalent {
		t.Fatal("equivalent security profiles have different digests")
	}
	if first == different {
		t.Fatal("network change did not change security profile digest")
	}
}

// What a Mac reports about itself may be unmasked, duplicated, unordered, and
// full of host routes inside subnets it already named. The boundary is what
// canonicalizes, so none of that reaches the digest: an instance is stale only
// when the networks themselves changed.
func TestSecurityProfileDependsOnTheCanonicalSetAlone(t *testing.T) {
	observed, err := CanonicalIPv4Prefixes([]netip.Prefix{
		netip.MustParsePrefix("192.168.7.23/24"),
		netip.MustParsePrefix("100.64.4.2/30"),
		netip.MustParsePrefix("192.168.7.1/32"),
		netip.MustParsePrefix("192.168.7.99/24"),
	})
	if err != nil {
		t.Fatal(err)
	}
	collapsed, err := CanonicalIPv4Prefixes([]netip.Prefix{
		netip.MustParsePrefix("100.64.4.0/30"),
		netip.MustParsePrefix("192.168.7.0/24"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(observed, collapsed) {
		t.Fatalf("canonical sets differ: %v and %v", observed, collapsed)
	}
	if securityProfileDigest(observed) != securityProfileDigest(collapsed) {
		t.Fatal("equivalent host networks produced different security profiles")
	}
}

func TestRenderConfigRequiresHostNetworks(t *testing.T) {
	_, err := RenderConfig(nil)
	if err == nil {
		t.Fatal("RenderConfig unexpectedly accepted no host networks")
	}
}

func TestGeneratedConfigValidatesWithInstalledLima(t *testing.T) {
	limactl, err := exec.LookPath("limactl")
	if err != nil {
		t.Skip("limactl is not installed")
	}
	config, err := RenderConfig([]netip.Prefix{
		netip.MustParsePrefix("192.168.4.0/24"),
	})
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
