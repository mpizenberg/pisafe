package lima

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strings"
)

const (
	InstanceName       = "pisafe"
	BrokerAddress      = "192.0.2.1"
	MinimumLimaVersion = "2.2.0"
)

type ConfigOptions struct {
	CPUs             int
	MemoryGiB        int
	DiskGiB          int
	HostIPv4Prefixes []netip.Prefix
}

func DefaultConfigOptions(prefixes []netip.Prefix) ConfigOptions {
	return ConfigOptions{
		CPUs:             4,
		MemoryGiB:        8,
		DiskGiB:          64,
		HostIPv4Prefixes: prefixes,
	}
}

func RenderConfig(options ConfigOptions) ([]byte, error) {
	if options.CPUs < 1 || options.MemoryGiB < 1 || options.DiskGiB < 1 {
		return nil, errors.New("VM CPU, memory, and disk limits must be positive")
	}
	prefixes, err := canonicalIPv4Prefixes(options.HostIPv4Prefixes)
	if err != nil {
		return nil, err
	}
	securityProfile := securityProfileDigest(prefixes)

	replacements := strings.NewReplacer(
		"@@CPUS@@", fmt.Sprintf("%d", options.CPUs),
		"@@MEMORY@@", fmt.Sprintf("%dGiB", options.MemoryGiB),
		"@@DISK@@", fmt.Sprintf("%dGiB", options.DiskGiB),
		"@@HOST_PREFIXES@@", strings.Join(prefixes, ", "),
		"@@HOST_PREFIX_LINES@@", strings.Join(prefixes, "\n    "),
		"@@SECURITY_PROFILE_DIGEST@@", securityProfile,
	)
	return []byte(replacements.Replace(configTemplate)), nil
}

// securityProfileDigest changes whenever the generated VM definition or its
// immutable host-network deny set changes. Resource values are deliberately
// excluded so supported CPU/memory/disk tuning does not look like security
// drift.
func securityProfileDigest(prefixes []string) string {
	digest := sha256.New()
	_, _ = digest.Write([]byte("pisafe-lima-security-profile-v1\x00"))
	_, _ = digest.Write([]byte(configTemplate))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write([]byte(strings.Join(prefixes, "\n")))
	return "sha256:" + hex.EncodeToString(digest.Sum(nil))
}

func canonicalIPv4Prefixes(prefixes []netip.Prefix) ([]string, error) {
	if len(prefixes) == 0 {
		return nil, errors.New("host IPv4 prefixes are required; refusing an incomplete firewall")
	}
	unique := make(map[netip.Prefix]struct{})
	for _, prefix := range prefixes {
		if !prefix.IsValid() || !prefix.Addr().Is4() {
			return nil, fmt.Errorf("invalid host IPv4 prefix %q", prefix)
		}
		unique[prefix.Masked()] = struct{}{}
	}
	candidates := make([]netip.Prefix, 0, len(unique))
	for prefix := range unique {
		candidates = append(candidates, prefix)
	}
	sort.Slice(candidates, func(left, right int) bool {
		if candidates[left].Bits() != candidates[right].Bits() {
			return candidates[left].Bits() < candidates[right].Bits()
		}
		return candidates[left].String() < candidates[right].String()
	})
	collapsed := make([]netip.Prefix, 0, len(candidates))
	for _, candidate := range candidates {
		covered := false
		for _, existing := range collapsed {
			if existing.Contains(candidate.Addr()) {
				covered = true
				break
			}
		}
		if !covered {
			collapsed = append(collapsed, candidate)
		}
	}

	result := make([]string, 0, len(collapsed))
	for _, prefix := range collapsed {
		result = append(result, prefix.String())
	}
	sort.Strings(result)
	return result, nil
}

const configTemplate = `minimumLimaVersion: 2.2.0
plain: true
vmType: vz
arch: aarch64

images:
- location: https://download.fedoraproject.org/pub/fedora/linux/releases/44/Cloud/aarch64/images/Fedora-Cloud-Base-Generic-44-1.7.aarch64.qcow2
  arch: aarch64
  digest: sha256:55c60a3b80d3616a08705afd0459e75fe9f03c54aba7a46e4002a41a72fa0d5b

cpus: @@CPUS@@
memory: @@MEMORY@@
disk: @@DISK@@

mounts: []
containerd:
  system: false
  user: false
ssh:
  loadDotSSHPubKeys: false
  forwardAgent: false
  forwardX11: false
  forwardX11Trusted: false
hostResolver:
  enabled: false
  ipv6: false
dns:
- 1.1.1.1
- 9.9.9.9
propagateProxyEnv: false
upgradePackages: false

provision:
- mode: system
  script: |
    #!/bin/bash
    set -eux -o pipefail

    dnf -y install --best --setopt=install_weak_deps=False \
      chrony git nftables openssh-server podman
    systemctl disable --now firewalld 2>/dev/null || true
    systemctl enable --now chronyd.service

    pisafe_user="$(
      getent passwd |
        awk -F: '$3 >= 500 && $3 < 65534 && $6 ~ /^\/home\// { print $1; exit }'
    )"
    test -n "${pisafe_user}"
    grep -q "^${pisafe_user}:" /etc/subuid ||
      usermod --add-subuids 100000-165535 "${pisafe_user}"
    grep -q "^${pisafe_user}:" /etc/subgid ||
      usermod --add-subgids 100000-165535 "${pisafe_user}"
    runuser --login "${pisafe_user}" --command 'podman system migrate'

    install -d -m 0755 /etc/pisafe /etc/ssh/sshd_config.d
    tee /etc/sysctl.d/90-pisafe.conf >/dev/null <<'PISAFE_SYSCTL'
    net.ipv6.conf.all.disable_ipv6 = 1
    net.ipv6.conf.default.disable_ipv6 = 1
    net.ipv6.conf.lo.disable_ipv6 = 1
    PISAFE_SYSCTL

    tee /etc/ssh/sshd_config.d/10-pisafe.conf >/dev/null <<'PISAFE_SSHD'
    AllowAgentForwarding no
    AllowTcpForwarding remote
    GatewayPorts clientspecified
    PermitListen 192.0.2.1:*
    PermitTunnel no
    X11Forwarding no
    PISAFE_SSHD

    tee /etc/pisafe/firewall.nft >/dev/null <<'PISAFE_NFT'
    flush ruleset

    table inet pisafe {
      set fixed_denied_v4 {
        type ipv4_addr
        flags interval
        elements = {
          0.0.0.0/8,
          10.0.0.0/8,
          100.64.0.0/10,
          127.0.0.0/8,
          169.254.0.0/16,
          172.16.0.0/12,
          192.0.2.0/24,
          192.168.0.0/16,
          224.0.0.0/4,
          240.0.0.0/4
        }
      }

      set host_onlink_v4 {
        type ipv4_addr
        flags interval
        elements = { @@HOST_PREFIXES@@ }
      }

      set broker_ports {
        type inet_service
      }

      chain input {
        type filter hook input priority filter
        policy drop
        ct state invalid drop
        ct state established,related accept
        udp sport 67 udp dport 68 accept
        iifname "lo" accept
        tcp dport 22 accept
        ip daddr 192.0.2.1 tcp dport @broker_ports accept
      }

      chain output {
        type filter hook output priority filter
        policy accept
        meta skuid 0 ip daddr 127.0.0.0/8 accept
        meta skuid 0 udp sport 68 udp dport 67 accept
        ip daddr 192.0.2.1 tcp dport @broker_ports accept
        ip daddr @fixed_denied_v4 reject
        ip daddr @host_onlink_v4 reject
      }

      chain forward {
        type filter hook forward priority filter
        policy accept
        ip daddr 192.0.2.1 tcp dport @broker_ports accept
        ip daddr @fixed_denied_v4 reject
        ip daddr @host_onlink_v4 reject
      }
    }
    PISAFE_NFT

    tee /etc/pisafe/host-prefixes >/dev/null <<'PISAFE_PREFIXES'
    @@HOST_PREFIX_LINES@@
    PISAFE_PREFIXES
    chmod 0444 /etc/pisafe/host-prefixes

    tee /etc/pisafe/security-profile >/dev/null <<'PISAFE_SECURITY_PROFILE'
    @@SECURITY_PROFILE_DIGEST@@
    PISAFE_SECURITY_PROFILE
    chmod 0444 /etc/pisafe/security-profile

    tee /usr/local/sbin/pisafe-firewall >/dev/null <<'PISAFE_FIREWALL'
    #!/bin/bash
    set -eux -o pipefail
    sysctl --system
    ip link show pisafe-broker >/dev/null 2>&1 ||
      ip link add pisafe-broker type dummy
    ip addr replace 192.0.2.1/32 dev pisafe-broker
    ip link set pisafe-broker up
    nft --check --file /etc/pisafe/firewall.nft
    nft --file /etc/pisafe/firewall.nft
    PISAFE_FIREWALL
    chmod 0755 /usr/local/sbin/pisafe-firewall

    tee /usr/local/sbin/pisafe-firewall-status >/dev/null <<'PISAFE_STATUS'
    #!/bin/bash
    set -euo pipefail
    exec nft list table inet pisafe
    PISAFE_STATUS
    chmod 0755 /usr/local/sbin/pisafe-firewall-status

    tee /usr/local/sbin/pisafe-clock-step >/dev/null <<'PISAFE_CLOCK'
    #!/bin/bash
    set -euo pipefail
    chronyc -a 'burst 4/4'
    chronyc -a makestep
    exec chronyc waitsync 30 0.5 1000 1
    PISAFE_CLOCK
    chmod 0755 /usr/local/sbin/pisafe-clock-step

    tee /etc/systemd/system/pisafe-firewall.service >/dev/null <<'PISAFE_SERVICE'
    [Unit]
    Description=pisafe static network boundary
    Wants=network-pre.target
    Before=network.target sshd.service

    [Service]
    Type=oneshot
    ExecStart=/usr/local/sbin/pisafe-firewall
    RemainAfterExit=yes

    [Install]
    WantedBy=multi-user.target
    PISAFE_SERVICE

    systemctl daemon-reload
    systemctl enable --now pisafe-firewall.service
    sshd -t
    systemctl restart sshd.service

    tee /etc/sudoers.d/90-pisafe-controller >/dev/null <<PISAFE_SUDOERS
    ${pisafe_user} ALL=(root) NOPASSWD: /usr/local/sbin/pisafe-clock-step ""
    ${pisafe_user} ALL=(root) NOPASSWD: /usr/local/sbin/pisafe-firewall-status ""
    PISAFE_SUDOERS
    chmod 0440 /etc/sudoers.d/90-pisafe-controller
    if [[ -f /etc/sudoers.d/90-cloud-init-users ]]; then
      sed -i "\|^${pisafe_user} .*NOPASSWD:ALL|d" \
        /etc/sudoers.d/90-cloud-init-users
    fi
    gpasswd --delete "${pisafe_user}" wheel 2>/dev/null || true
    visudo --check --file=/etc/sudoers.d/90-pisafe-controller

probes:
- description: pisafe VM security boundary
  script: |
    #!/bin/bash
    set -eux -o pipefail
    command -v podman >/dev/null
    podman info >/dev/null
    test "$(podman unshare cat /proc/self/uid_map | wc -l)" -ge 2
    systemctl is-active --quiet pisafe-firewall.service
    sudo /usr/local/sbin/pisafe-firewall-status >/dev/null
    test "$(sysctl -n net.ipv6.conf.all.disable_ipv6)" = 1
`
