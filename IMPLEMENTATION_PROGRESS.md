# Implementation progress

Last updated: 2026-07-24

This file is the durable handoff for continuing `pisafe` from a fresh session.
The design authority remains [`pisafe-design.md`](pisafe-design.md); this file
records what is implemented, what has been verified, and what should happen
next.

## Current milestone

Phase 1 is in progress. Three implementation slices now exist:

1. The Go controller skeleton and single-repository Git isolation core.
2. The dedicated Lima VM backend, host-network discovery, and initial static
   firewall boundary.
3. The SSH artifact transport, durable run records, pinned run image, hardened
   Podman launch contract, Linux materialization helper, and internal run
   creation transaction.

There is still no user-facing `pisafe run`. Do not add a local-workspace
fallback: the tested path streams through the mountless VM and materializes
inside a private rootless container volume.

## Implemented

### Controller and Git boundary

- Dependency-free Go module and `cmd/pisafe` executable.
- Compact CLI help and `pisafe doctor`.
- Git staging split at the VM boundary:
  - `gitstage.Prepare` runs on the Mac and produces a Git bundle plus a binary
    patch of the final tracked state.
  - `gitstage.Materialize` consumes those artifacts in the isolated
    environment and never accesses the recorded Mac source path.
- Dirty tracked state is flattened into a clearly labelled baseline commit.
- Untracked files remain excluded and are reported at finalization.
- Apply is split in the opposite direction:
  - `gitstage.PrepareApply` creates an incremental bundle in the isolated
    environment.
  - `gitstage.ImportApply` verifies and imports it on the Mac.
- Apply creates only `refs/heads/pisafe/<run>` with a compare-and-swap
  `git update-ref`; it does not change the current branch, index, or working
  tree.
- Git LFS and submodules currently fail closed rather than staging incomplete
  repositories.
- External diff and text-conversion drivers are disabled during host capture.
- NUL-delimited parsing preserves unusual Git filenames.

### Host-network discovery

- `internal/hostnet` gathers IPv4 prefixes from every active, non-loopback Mac
  interface.
- It also records the default IPv4 gateway.
- Prefixes are canonicalized, deduplicated, and collapsed so nftables interval
  sets never receive overlapping networks.
- Discovery fails closed if the host network cannot be determined.

### Lima backend

- Dedicated instance name: `pisafe`.
- Minimum Lima version: 2.2.0.
- Lima `plain: true`, which disables mounts, dynamic forwarding, containerd,
  the guest agent, and SSH-agent forwarding.
- Pinned Fedora 44 ARM64 cloud image:

  ```text
  sha256:55c60a3b80d3616a08705afd0459e75fe9f03c54aba7a46e4002a41a72fa0d5b
  ```

- Default VM resources: 4 CPUs, 8 GiB memory, 64 GiB sparse disk.
- Explicitly empty mounts.
- No forwarded SSH agent, X11, host proxy environment, Lima host resolver, or
  Podman socket.
- Public DNS configured directly.
- Rootless Podman, Git, nftables, and OpenSSH installed during provisioning.
- A 65,536-ID subordinate UID/GID range is assigned to the Lima user.
- Provisioning runs `podman system migrate` after assigning those ranges.
- Chrony is enabled, and every start/resume calls a narrowly privileged clock
  step because plain mode has no guest-agent time correction.
- IPv6 is disabled.
- A dedicated `192.0.2.1/32` dummy address is reserved for the future
  inference-broker reverse relay.
- SSH remote forwarding is restricted to that address; the dynamic
  `broker_ports` nftables set is empty by default.
- The generated configuration removes the cloud image's unrestricted
  passwordless sudo and removes the Lima user from `wheel`. It grants only
  two exact no-argument helpers: clock synchronization and firewall status.

### Firewall

- A boot-persistent `pisafe-firewall.service` owns the nftables ruleset.
- `firewalld` is disabled in the dedicated VM.
- Input defaults to drop, with established traffic, DHCP replies, control SSH,
  and future allowed broker ports admitted.
- Both output and forward hooks deny:
  - IPv4 loopback;
  - RFC1918;
  - CGNAT;
  - link-local and metadata;
  - TEST-NET broker space except an explicitly enabled broker port;
  - multicast and reserved/broadcast ranges; and
  - the Mac's current on-link networks.
- Root DHCP and root loopback exceptions keep VM infrastructure functional.
- `lima.Manager.Start` requires a non-empty host-prefix set and compares it
  with a root-owned record of the set baked into the firewall. A network
  change fails closed and requires VM recreation.
- There is deliberately no runtime firewall-mutation privilege for the Lima
  user. A merely syntax-valid refresh helper would still let an escaped
  process replace the real LAN set with an unrelated valid prefix.

### SSH transport and guest materialization

- `lima.Transport` executes argv-style commands through Lima's control SSH
  connection.
- It allocates a private VM-side run directory and uploads `source.bundle`,
  `tracked.patch`, and `snapshot.json` independently.
- Every upload is streamed as binary data, checked for exact byte count and
  SHA-256 in the VM, and atomically renamed.
- The guest snapshot omits the Mac source path.
- Stage files are imported into a new private Podman volume with
  `podman volume import`; no Mac mount, VM bind mount, Podman socket, or
  unsupported `podman cp --chown` path is used.
- `cmd/pisafe-guest` invokes the same tested `gitstage.Materialize`
  implementation inside Linux and rejects snapshots containing a Mac path.

### Run image and container contract

- Root image is pinned to the ARM64 Node manifest:

  ```text
  docker.io/library/node@sha256:af01d58b748ec92b1d6e8e11429aad424fd1e68c848185399dca0596a1ab8f5c
  ```

- Pi is pinned to `@earendil-works/pi-coding-agent@0.82.0`; the downloaded
  package tarball is checked against its registry SHA-512 integrity before
  installation.
- The image contains Git, OpenSSH, Tini, Pi, and the static ARM64 guest helper.
- Build-time SSH host keys are removed. Per-run keys/configuration are not
  implemented yet.
- Run commands require an immutable `sha256:` image ID and use:
  - UID/GID 1000;
  - read-only root;
  - all capabilities dropped;
  - `no-new-privileges`;
  - rootless pasta networking with explicit public DNS;
  - 2 CPUs, 4 GiB memory with no extra swap, and 512 PIDs;
  - bounded `/tmp` and `/run` tmpfs mounts; and
  - unique workspace and home volumes, chowned for the non-root user.
- No Podman/Docker socket, forwarded SSH agent, or credential environment is
  added.

### Run records and internal controller

- `internal/runstate` writes versioned, mode-0600 JSON manifests atomically
  under the user config directory (or `PISAFE_STATE_DIR`).
- It enforces `creating → active → stopped → active|imported|discarded|expired`.
- Failed creation remains visibly `creating` with `last_error`; it is not
  silently deleted or mislabeled active.
- `internal/runctl.StartPrepared` composes stage upload, private-volume import,
  hardened container start, in-container materialization, transfer cleanup,
  manifest activation, and bounded rollback.
- `pisafe list` displays these durable records. Run creation remains internal.

## Tests and verification

Normal checks:

```sh
go test -race -cover ./...
go vet ./...
go build -trimpath ./cmd/pisafe
git diff --check
```

Current package coverage at this milestone:

```text
pisafe-guest  64.3%
cli           26.0%
gitstage      68.7%
hostnet       50.0%
lima          68.5%
runcontainer  72.7%
runctl        73.6%
runid        100.0%
runstate      68.3%
```

The generated YAML is checked by the installed Lima validator in the normal
test suite.

The live suite is intentionally gated because it creates/reuses a persistent
VM and may download images:

```sh
PISAFE_LIVE_LIMA=1 go test -v ./internal/lima
```

Verified against a real ARM64 VM:

- Lima reaches READY.
- `/Users` is absent.
- No Podman socket is forwarded.
- The Podman user namespace has the expected direct UID plus subordinate
  65,536-ID mapping.
- The firewall service and nftables table are active.
- IPv6 is disabled.
- Public HTTPS works from a rootless Alpine container.
- RFC1918 and metadata TCP destinations fail from that container.
- `pisafe doctor` validates the current host networks and generated Lima
  configuration.
- The final tracked `Containerfile` builds on ARM64.
- The final local image has:

  ```text
  image ID:        sha256:cfd331bd6b1884869620885efa08c60eca6a522e1c6eeef07bb821be22039220
  manifest digest: sha256:127564ea34633ba52f838631afcb17988f3b98f7a32cf1f0e1f09c368ff6d1b8
  ```

- A direct runtime check verified UID 1000, zero effective/bounding
  capabilities, `NoNewPrivs=1`, read-only root, 4 GiB memory, 512 PIDs,
  Pi 0.82.0, and public HTTPS.
- `TestLiveSSHStageAndContainerMaterialize` passed against that exact image:
  dirty tracked state crossed Lima SSH, entered a private volume, materialized
  inside the hardened container, and could be deleted there without altering
  the Mac checkout.

Run that end-to-end test with:

```sh
PISAFE_LIVE_LIMA=1 \
PISAFE_LIVE_RUN_IMAGE=sha256:cfd331bd6b1884869620885efa08c60eca6a522e1c6eeef07bb821be22039220 \
go test -v -run TestLiveSSHStageAndContainerMaterialize ./internal/lima
```

## Live VM state

A persistent Lima instance named `pisafe` was left running. It contains no
project runs or user data. It now contains cached base/test images, the local
`localhost/pisafe-run:0.1-dev` image above, and a temporary image build
directory.

The VM was recreated while fixing live-only provisioning issues. Those issues
were:

1. Overlapping `/24` and gateway `/32` nftables interval elements.
2. A readiness probe that enumerated nftables without `sudo`.
3. Missing rootless Podman subordinate IDs.
4. Podman's existing pause namespace needing migration after the ID assignment.
5. Pasta's default link-local DNS forwarder being denied by the intended
   firewall; run/build commands now specify public DNS explicitly.
6. Plain-mode VM time drifting by roughly 6.5 hours after host sleep; manager
   start/resume now invokes the restricted chrony step helper.
7. Fedora cloud-init granting the Lima user unrestricted passwordless sudo,
   which would let a VM-user escape remove the firewall. The newly generated
   config revokes it and exposes only two exact, non-firewall-mutating
   controller helpers.
8. Fedora Podman not supporting `podman cp --chown`; stage transfer now uses
   the documented rootless local-volume import path.

Important: the current running VM predates the new restricted-sudo and
clock-helper configuration. It still has the cloud image's unrestricted
passwordless sudo, lacks the immutable host-prefix record, and received its
clock step manually. Do not use it for an untrusted run. Recreate it from the
generated configuration (or validate a separate fresh instance) before
exposing `pisafe run`.

The current VM also received `podman system migrate` manually. The exact
command in the generated config was verified, but neither that line nor the
new sudo/clock helpers have been exercised together during a completely fresh
VM creation. Fresh provisioning is the next required live gate.

When Codex runs inside a restricted filesystem sandbox, it may be unable to
connect to `~/.lima/pisafe/ha.sock`; `limactl list` then falsely labels the
instance `Broken`. Run the diagnostic with the needed host permission before
acting on that status. Do not delete or recreate the VM based only on the
sandboxed result.

## Known gaps

- No automated run-image build/install/cache validation exists. The verified
  image was built manually by streaming the Containerfile and ARM64 helper.
- No user-facing `run`, `connect`, `diff`, `apply`, `cp`, `discard`, or `gc`
  implementation exists. `list` is the only lifecycle command exposed.
- Per-run SSH host/client keys, sshd configuration, a Lima `ProxyCommand`, and
  Zed Remote are missing.
- Persistent workspace disk quota and wall-clock enforcement are missing.
  CPU, memory, PID, and tmpfs limits are implemented and live-verified.
- Selected untracked/ignored input archive handling is missing.
- Submodule staging and journaled multi-repository apply are missing.
- The broker port set and reverse SSH inference relay are not implemented.
- No inference broker or OAuth/Keychain integration exists.
- Firewall behavioral coverage still needs DNS-to-private answers, redirects,
  raw UDP, `host.containers.internal`, VM loopback attempts, and the exact
  broker exception.
- Existing Lima configuration drift is not detected or reconciled. This is
  currently security-relevant because the existing VM predates sudo hardening.
- Pi is installed, but inference intentionally remains unusable until the
  broker/relay exists. No raw provider credential may be added as a shortcut.
- Pi's top-level tarball is integrity-pinned, but a reproducible published
  image/digest workflow is still needed to freeze transitive npm resolution.

## Next implementation slice

Finish the first run lifecycle without weakening the boundary:

1. Exercise the complete updated Lima configuration on a fresh instance.
   Verify clean Podman provisioning, automatic clock correction, restricted
   helper commands, and that `sudo -n true` fails.
2. Add run-image build/install automation that streams only the Containerfile
   and companion helper, validates labels, and records the resulting immutable
   ID. Detect/reconcile security-relevant VM configuration drift.
3. Generate per-run sandbox SSH host/client keys, configure non-root sshd, and
   connect through a Lima control-SSH `ProxyCommand`; then prove Zed and Pi see
   the same workspace.
4. Wire `pisafe run` around host-network verification, `gitstage.Prepare`, and
   `runctl.StartPrepared`; keep Pi inference unavailable rather than injecting
   a credential.
5. Implement stop/resume and exact-confirmation discard, including reliable
   cleanup recovery and wall-clock enforcement. Choose and live-test a
   persistent workspace disk-quota mechanism before claiming the disk limit.
6. Implement the reverse inference relay and run-scoped capability.
7. Then add selected untracked inputs and submodule-aware journaled apply
   before exposing `pisafe apply`.

## Useful references

- Lima plain mode: <https://lima-vm.io/docs/config/plain/>
- Lima SSH: <https://lima-vm.io/docs/usage/ssh/>
- Lima Podman example: <https://lima-vm.io/docs/examples/containers/podman/>
- Podman rootless networking:
  <https://docs.podman.io/en/stable/markdown/podman-network.1.html>
