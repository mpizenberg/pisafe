# Implementation progress

Last updated: 2026-07-24

This file is the durable handoff for continuing `pisafe` from a fresh session.
The design authority remains [`pisafe-design.md`](pisafe-design.md); this file
records what is implemented, what has been verified, and what should happen
next.

## Current milestone

Phase 1 is in progress. Four implementation slices now exist:

1. The Go controller skeleton and single-repository Git isolation core.
2. The dedicated Lima VM backend, host-network discovery, and initial static
   firewall boundary.
3. The SSH artifact transport, durable run records, pinned run image, hardened
   Podman launch contract, Linux materialization helper, and internal run
   creation transaction.
4. Per-run SSH credentials, non-root container `sshd`, strict host-key
   pinning, and a portless Lima control-SSH ProxyCommand.

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
- Every provisioned VM records a root-owned SHA-256 fingerprint derived from
  the complete generated Lima definition and immutable host-network set.
  `Manager.Start` checks it before clock or firewall verification, so an older
  or locally modified security definition fails closed.
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
- `runimage.Installer` derives a recipe digest from the exact Containerfile and
  static guest-helper bytes, streams a tar context containing only those two
  files, and passes that digest into an image label.
- A mutable, recipe-derived local tag is used only as a cache key. Reuse
  requires matching recipe, base, and Pi labels, Linux/ARM64 platform, and a
  valid immutable SHA-256 image ID. Run containers continue to receive only
  that immutable ID.
- Artifact loading rejects symlinks, file swaps, oversize inputs, non-ELF or
  non-ARM64 helpers, dynamic interpreters, and imported shared libraries.
- The image contains Git, OpenSSH, Tini, Pi, and the static ARM64 guest helper.
- Build-time SSH host keys are removed. A network-disabled one-shot container
  generates each run's host key and installs only its client public key into
  the private home volume.
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

### Per-run SSH boundary

- `internal/runssh` creates a unique Ed25519 client key under mode-0700
  run-local Mac state; private and public key files are restricted to 0600.
- Both client and host public keys are decoded and validated as exact
  32-byte Ed25519 SSH wire values rather than accepted as arbitrary text.
- The container generates its own Ed25519 host key. The Mac stores a strict
  per-run `known_hosts` file and SHA-256 fingerprint before activation.
- `sshd` runs as UID/GID 1000 and is the container's main process. It listens
  only on container-local `127.0.0.1:2222`, permits public-key authentication
  only, and disables password, keyboard-interactive, root, agent, X11, tunnel,
  user-RC, and TCP-forwarding paths.
- No SSH port is published into the VM or onto macOS.
- The generated per-run OpenSSH config uses Lima's generated SSH config and a
  `ProxyCommand` that executes:

  ```text
  podman exec --interactive <run-container> pisafe-guest proxy-ssh
  ```

  The static guest helper relays binary stdio only to container loopback.
- The client private key never enters Lima or the run. The authorized public
  key is further restricted against agent, TCP, X11, and user-RC forwarding.
- Run manifests record the alias, identity/config/known-hosts paths, and pinned
  host-key fingerprint. A creating run can become active only through the
  activation operation that supplies this connection record.

### Run records and internal controller

- `internal/runstate` writes version-2, mode-0600 JSON manifests atomically
  under the user config directory (or `PISAFE_STATE_DIR`).
- It enforces `creating → active → stopped → active|imported|discarded|expired`.
- Failed creation remains visibly `creating` with `last_error`; it is not
  silently deleted or mislabeled active.
- `internal/runctl.StartPrepared` composes stage upload, private-volume import,
  per-run key generation, network-disabled SSH initialization, hardened
  container start, host-key pinning, in-container materialization, transfer
  cleanup, manifest activation, and bounded rollback.
- Creation rollback removes partial SSH client state along with the container,
  volumes, and VM-side transfer directory.
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
pisafe-guest  52.8%
cli           26.0%
gitstage      68.7%
hostnet       50.0%
lima          70.6%
runcontainer  72.2%
runctl        72.8%
runid        100.0%
runimage      74.4%
runssh        65.7%
runstate      69.7%
```

The generated YAML is checked by the installed Lima validator in the normal
test suite.

The live suite is intentionally gated because it creates/reuses a persistent
VM and may download images:

```sh
PISAFE_LIVE_LIMA=1 go test -v ./internal/lima
PISAFE_LIVE_LIMA=1 go test -v ./internal/runimage
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
  recipe digest:   sha256:c4c10720d220f2ebcd2f3fa67997d7cf964939ea5bcd874604fdf9399b26b02d
  image ID:        sha256:d0acf9f290d4bf49792a03fc2ed9080ccb18631ba2bfea9fb25696045a589a73
  manifest digest: sha256:d7181f8d5716a088597edaace6c7cfc9c42709c5a7aa780a3f4a3aab96b420b4
  ```

- The managed installer built that image from its two-file tar stream,
  validated its labels/platform/immutable ID, and reused it on the next call.
- A direct runtime check verified UID 1000, zero effective/bounding
  capabilities, `NoNewPrivs=1`, read-only root, 4 GiB memory, 512 PIDs,
  Pi 0.82.0, and public HTTPS.
- `TestLiveSSHStageAndContainerMaterialize` passed against that exact image:
  dirty tracked state crossed Lima SSH, entered a private volume, materialized
  inside the hardened container, and could be deleted there without altering
  the Mac checkout.
- That same live test generated fresh client and host keys, pinned the host
  key, connected with the generated ProxyCommand, and wrote a file through the
  Zed-compatible OpenSSH session. A Pi executable in the same container saw
  that exact file and workspace. The SSH session ran as UID 1000 with no
  forwarded agent or `/Users`, and `podman port` reported no published port.

Run that end-to-end test with:

```sh
PISAFE_LIVE_LIMA=1 \
PISAFE_LIVE_RUN_IMAGE=sha256:d0acf9f290d4bf49792a03fc2ed9080ccb18631ba2bfea9fb25696045a589a73 \
go test -v -run TestLiveSSHStageAndContainerMaterialize ./internal/lima
```

## Live VM state

A persistent Lima instance named `pisafe` was left running. It contains no
project runs or user data. It was freshly provisioned from the current
generated configuration and contains only cached base/test images plus the
current `localhost/pisafe-run:managed-c4c10720d220f2eb` image. Older managed
recipe images remain only as rebuildable cache entries.

Fresh provisioning verified all security-sensitive setup together:

- automatic subordinate UID/GID assignment and `podman system migrate`;
- the clock-step and firewall-status helpers work through their exact sudo
  rules;
- unrestricted `sudo -n true` is denied;
- the root-owned security-profile record is mode 0444 and currently contains
  `sha256:ebb0446c0c2b37610a54e5784c51e2a7866c7cf5172d4e88ce49942fc8a7cb8a`;
- the full container network suite passes; and
- the managed image install/reuse and end-to-end staging tests pass.

The final cleanup audit found no run-labelled containers, run volumes, or
VM-side staging directories.

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
9. Fedora Podman's image-inspection `Id` is a bare 64-character hex value,
   while run specs deliberately require `sha256:` form. The installer now
   validates and normalizes it before returning the immutable ID.
10. Fedora Podman rejects `uid=` and `gid=` in `--tmpfs` option strings. The
    bounded writable `/run` now uses Podman's supported
    `type=tmpfs,...,U=true` mount form.
11. A VM-loopback published SSH port could not cross the intended firewall,
    which denies loopback to the unprivileged Lima user. SSH now uses a
    portless `podman exec` stdio relay to container loopback instead of adding
    a mutable firewall exception.

When Codex runs inside a restricted filesystem sandbox, it may be unable to
connect to `~/.lima/pisafe/ha.sock`; `limactl list` then falsely labels the
instance `Broken`. Run the diagnostic with the needed host permission before
acting on that status. Do not delete or recreate the VM based only on the
sandboxed result.

## Known gaps

- No user-facing `run`, `connect`, `diff`, `apply`, `cp`, `discard`, or `gc`
  implementation exists. `list` is the only lifecycle command exposed.
- The Zed-compatible SSH transport is live-verified, but `pisafe zed` and
  automatic Zed launch are not wired. Internal creation writes an isolated
  per-run config and deliberately does not edit global SSH or Zed settings.
- Persistent workspace disk quota and wall-clock enforcement are missing.
  CPU, memory, PID, and tmpfs limits are implemented and live-verified.
- Selected untracked/ignored input archive handling is missing.
- Submodule staging and journaled multi-repository apply are missing.
- The broker port set and reverse SSH inference relay are not implemented.
- No inference broker or OAuth/Keychain integration exists.
- Firewall behavioral coverage still needs DNS-to-private answers, redirects,
  raw UDP, `host.containers.internal`, VM loopback attempts, and the exact
  broker exception.
- Security-profile drift is detected and fails closed, but automated
  replacement/reconciliation is intentionally absent because deleting a VM is
  destructive and must be an explicit lifecycle operation.
- Pi is installed, but inference intentionally remains unusable until the
  broker/relay exists. No raw provider credential may be added as a shortcut.
- Pi's top-level tarball is integrity-pinned, but a reproducible published
  image/digest workflow is still needed to freeze transitive npm resolution.

## Next implementation slice

Finish the first run lifecycle without weakening the boundary:

1. Wire `pisafe run` around host-network verification, managed-image
   installation, `gitstage.Prepare`, and `runctl.StartPrepared`; keep Pi
   inference unavailable rather than injecting a credential. Print the
   generated SSH command/config and add explicit Zed launch without silently
   editing global user settings.
2. Implement stop/resume and exact-confirmation discard, including reliable
   cleanup recovery and wall-clock enforcement. Choose and live-test a
   persistent workspace disk-quota mechanism before claiming the disk limit.
3. Implement the reverse inference relay and run-scoped capability.
4. Then add selected untracked inputs and submodule-aware journaled apply
   before exposing `pisafe apply`.

## Useful references

- Lima plain mode: <https://lima-vm.io/docs/config/plain/>
- Lima SSH: <https://lima-vm.io/docs/usage/ssh/>
- Lima Podman example: <https://lima-vm.io/docs/examples/containers/podman/>
- Podman rootless networking:
  <https://docs.podman.io/en/stable/markdown/podman-network.1.html>
