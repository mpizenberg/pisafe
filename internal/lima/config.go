package lima

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"

	"github.com/mpizenberg/pisafe/internal/runcontainer"
)

const (
	InstanceName       = "pisafe"
	BrokerAddress      = "192.0.2.1"
	BrokerPort         = 18080
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
	namespaces := strings.Join(runcontainer.ProjectNamespaces(), " ")
	globalNamespaces := strings.Join(runcontainer.GlobalNamespaces(), " ")

	replacements := strings.NewReplacer(
		"@@CPUS@@", fmt.Sprintf("%d", options.CPUs),
		"@@MEMORY@@", fmt.Sprintf("%dGiB", options.MemoryGiB),
		"@@DISK@@", fmt.Sprintf("%dGiB", options.DiskGiB),
		"@@HOST_PREFIXES@@", strings.Join(prefixes, ", "),
		"@@HOST_PREFIX_LINES@@", strings.Join(prefixes, "\n    "),
		"@@SECURITY_PROFILE_DIGEST@@", securityProfile,
		"@@RUN_STORAGE_BYTES@@", strconv.FormatInt(runcontainer.DefaultPersistent, 10),
		"@@PROJECT_STORAGE_BYTES@@", strconv.FormatInt(runcontainer.DefaultProject, 10),
		"@@PROJECT_NAMESPACES@@", namespaces,
		"@@GLOBAL_STORAGE_BYTES@@", strconv.FormatInt(runcontainer.DefaultGlobal, 10),
		"@@GLOBAL_NAMESPACES@@", globalNamespaces,
		"@@BROKER_ADDRESS@@", BrokerAddress,
		"@@BROKER_PORT@@", strconv.Itoa(BrokerPort),
	)
	return []byte(replacements.Replace(configTemplate)), nil
}

// securityProfileDigest changes whenever the generated VM definition, its
// immutable host-network deny set, its persistent storage quotas, or the sets
// of namespaces runs share change. The template is hashed before substitution, so
// every value substituted into it has to be hashed here too. VM sizing is
// deliberately excluded because it does not weaken a run boundary.
func securityProfileDigest(prefixes []string) string {
	digest := sha256.New()
	_, _ = digest.Write([]byte("pisafe-lima-security-profile-v3\x00"))
	_, _ = digest.Write([]byte(configTemplate))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write([]byte(strings.Join(prefixes, "\n")))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write([]byte(strconv.FormatInt(runcontainer.DefaultPersistent, 10)))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write([]byte(strconv.FormatInt(runcontainer.DefaultProject, 10)))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write([]byte(strings.Join(runcontainer.ProjectNamespaces(), "\n")))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write([]byte(strconv.FormatInt(runcontainer.DefaultGlobal, 10)))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write([]byte(strings.Join(runcontainer.GlobalNamespaces(), "\n")))
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
      chrony e2fsprogs git nftables openssh-server podman
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
    install -d -m 0711 -o root -g root \
      /var/lib/pisafe/runs /var/lib/pisafe/projects /var/lib/pisafe/global
    install -d -m 0700 -o root -g root \
      /var/lib/pisafe/run-images /var/lib/pisafe/project-images \
      /var/lib/pisafe/global-images

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
    PermitListen @@BROKER_ADDRESS@@:@@BROKER_PORT@@
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

      chain input {
        type filter hook input priority filter
        policy drop
        ct state invalid drop
        ct state established,related accept
        udp sport 67 udp dport 68 accept
        iifname "lo" accept
        tcp dport 22 accept
        ip daddr @@BROKER_ADDRESS@@ tcp dport @@BROKER_PORT@@ accept
      }

      chain output {
        type filter hook output priority filter
        policy accept
        ct state established,related accept
        meta skuid 0 ip daddr 127.0.0.0/8 accept
        meta skuid 0 udp sport 68 udp dport 67 accept
        ip daddr @@BROKER_ADDRESS@@ tcp dport @@BROKER_PORT@@ accept
        ip daddr @fixed_denied_v4 reject
        ip daddr @host_onlink_v4 reject
      }

      chain forward {
        type filter hook forward priority filter
        policy accept
        ct state established,related accept
        ip daddr @@BROKER_ADDRESS@@ tcp dport @@BROKER_PORT@@ accept
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
    ip addr replace @@BROKER_ADDRESS@@/32 dev pisafe-broker
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

    tee /usr/local/sbin/pisafe-storage >/dev/null <<'PISAFE_STORAGE'
    #!/bin/bash
    set -euo pipefail
    [[ "$#" -eq 3 ]]
    action="$1"
    scope="$2"
    storage_id="$3"
    [[ "${#storage_id}" -le 64 ]]
    [[ "$storage_id" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]]

    # A run filesystem holds the container's two bind mounts plus the root of
    # the private upper layers it stacks over shared project state. A project
    # filesystem holds only the shared namespaces, and the global one only the
    # profile every run mounts read-only. What lives below any of those roots
    # depends on the repository or on what the user installed, which this helper
    # never learns: the unprivileged user owns them and builds them itself.
    case "$scope" in
      run)
        storage_root=/var/lib/pisafe/runs
        image_root=/var/lib/pisafe/run-images
        storage_bytes=@@RUN_STORAGE_BYTES@@
        directories=(workspace home overlay)
        ;;
      project)
        storage_root=/var/lib/pisafe/projects
        image_root=/var/lib/pisafe/project-images
        storage_bytes=@@PROJECT_STORAGE_BYTES@@
        directories=(@@PROJECT_NAMESPACES@@)
        ;;
      global)
        storage_root=/var/lib/pisafe/global
        image_root=/var/lib/pisafe/global-images
        storage_bytes=@@GLOBAL_STORAGE_BYTES@@
        directories=(@@GLOBAL_NAMESPACES@@)
        ;;
      *) exit 64 ;;
    esac
    run_root="$storage_root/$storage_id"
    image="$image_root/$storage_id.ext4"
    pisafe_user="$(
      getent passwd |
        awk -F: '$3 >= 500 && $3 < 65534 && $6 ~ /^\/home\// { print $1; exit }'
    )"
    [[ -n "$pisafe_user" ]]
    subuid_start="$(awk -F: -v user="$pisafe_user" '$1 == user { print $2; exit }' /etc/subuid)"
    subgid_start="$(awk -F: -v user="$pisafe_user" '$1 == user { print $2; exit }' /etc/subgid)"
    [[ "$subuid_start" =~ ^[0-9]+$ && "$subgid_start" =~ ^[0-9]+$ ]]
    storage_uid="$((subuid_start + 999))"
    storage_gid="$((subgid_start + 999))"
    [[ "$(stat -c '%U:%G:%a' "$storage_root")" = "root:root:711" ]]
    [[ "$(stat -c '%U:%G:%a' "$image_root")" = "root:root:700" ]]

    cleanup_partial() {
      if mountpoint -q "$run_root"; then
        umount "$run_root"
      fi
      if [[ -d "$run_root" && ! -L "$run_root" ]]; then
        rmdir "$run_root"
      fi
      rm -f -- "$image"
    }

    create() {
      [[ ! -e "$run_root" && ! -L "$run_root" ]]
      [[ ! -e "$image" && ! -L "$image" ]]
      trap 'cleanup_partial || true' ERR
      truncate -s "$storage_bytes" "$image"
      chmod 0600 "$image"
      mkfs.ext4 -q -F -m 0 "$image"
      mkdir "$run_root"
      mount -o loop,nodev,nosuid "$image" "$run_root"
      for directory in "${directories[@]}"; do
        install -d -m 0700 -o "$storage_uid" -g "$storage_gid" \
          "$run_root/$directory"
      done
      chown root:root "$run_root"
      chmod 0711 "$run_root"
      chcon -t container_file_t "$run_root" "${directories[@]/#/$run_root/}"
      trap - ERR
    }

    verify() {
      [[ -f "$image" && ! -L "$image" ]]
      [[ "$(stat -c '%u:%g:%a:%s' "$image")" = "0:0:600:$storage_bytes" ]]
      [[ -d "$run_root" && ! -L "$run_root" ]]
      if ! mountpoint -q "$run_root"; then
        mount -o loop,nodev,nosuid "$image" "$run_root"
      fi
      source="$(findmnt -n -o SOURCE --target "$run_root")"
      [[ "$source" = /dev/loop* ]]
      read -r backing < <(losetup -n -O BACK-FILE "$source")
      [[ "$backing" = "$image" ]]
      [[ "$(findmnt -n -o FSTYPE --target "$run_root")" = "ext4" ]]
      findmnt -n -o OPTIONS --target "$run_root" |
        tr ',' '\n' |
        grep -qx nodev
      findmnt -n -o OPTIONS --target "$run_root" |
        tr ',' '\n' |
        grep -qx nosuid
      [[ "$(stat -c '%u:%g:%a' "$run_root")" = "0:0:711" ]]
      for directory in "${directories[@]}"; do
        path="$run_root/$directory"
        [[ -d "$path" && ! -L "$path" ]]
        chown "$storage_uid:$storage_gid" "$path"
        chmod 0700 "$path"
      done
      chcon -t container_file_t "$run_root" "${directories[@]/#/$run_root/}"
    }

    # ensure is how a project filesystem is reached: many runs of one project
    # need it present, and none of them may assume it is the first.
    ensure() {
      if [[ -e "$run_root" || -L "$run_root" || -e "$image" || -L "$image" ]]; then
        verify
      else
        create
      fi
    }

    remove() {
      if [[ ! -e "$run_root" && ! -L "$run_root" &&
            ! -e "$image" && ! -L "$image" ]]; then
        return
      fi
      if mountpoint -q "$run_root"; then
        umount "$run_root"
      fi
      if [[ -e "$run_root" || -L "$run_root" ]]; then
        [[ -d "$run_root" && ! -L "$run_root" ]]
        rmdir "$run_root"
      fi
      if [[ -e "$image" || -L "$image" ]]; then
        [[ -f "$image" && ! -L "$image" ]]
        rm -f -- "$image"
      fi
    }

    case "$action" in
      create) create ;;
      ensure) ensure ;;
      verify) verify ;;
      remove) remove ;;
      *) exit 64 ;;
    esac
    PISAFE_STORAGE
    chmod 0755 /usr/local/sbin/pisafe-storage

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
    ${pisafe_user} ALL=(root) NOPASSWD: /usr/local/sbin/pisafe-storage *
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
