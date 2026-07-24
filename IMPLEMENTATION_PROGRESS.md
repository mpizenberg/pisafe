# Implementation progress

Last updated: 2026-07-24

This file is the durable handoff for continuing `pisafe` from a fresh session.
The design authority remains [`pisafe-design.md`](pisafe-design.md); this file
records what is implemented, what has been verified, and what should happen
next.

## Current milestone

Phase 1 is in progress. Two implementation slices now exist:

1. The Go controller skeleton and single-repository Git isolation core.
2. The dedicated Lima VM backend, host-network discovery, and initial static
   firewall boundary.

There is not yet a user-facing `pisafe run`. Do not add a local-workspace
fallback: materialization belongs inside the mountless VM, after an SSH
transfer.

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
- IPv6 is disabled.
- A dedicated `192.0.2.1/32` dummy address is reserved for the future
  inference-broker reverse relay.
- SSH remote forwarding is restricted to that address; the dynamic
  `broker_ports` nftables set is empty by default.

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
- `lima.Manager.Start` requires a non-empty host-prefix set and atomically
  refreshes it even when the VM is already running. Callers therefore cannot
  resume through this API without refreshing the LAN boundary.

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
gitstage  68.7%
hostnet   50.0%
lima      62.9%
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

## Live VM state

A persistent Lima instance named `pisafe` was created during development and
was left running at this milestone. It contains no project runs or user data,
only the provisioned VM and a cached Alpine test image.

The VM was recreated while fixing live-only provisioning issues. Those issues
were:

1. Overlapping `/24` and gateway `/32` nftables interval elements.
2. A readiness probe that enumerated nftables without `sudo`.
3. Missing rootless Podman subordinate IDs.
4. Podman's existing pause namespace needing migration after the ID assignment.

The current running VM received `podman system migrate` manually, and the exact
command now generated by the config was verified successfully. The updated
config containing that command has not yet been exercised during a completely
fresh VM creation. Re-run the full gated live test from an absent VM before
calling clean provisioning fully covered.

When Codex runs inside a restricted filesystem sandbox, it may be unable to
connect to `~/.lima/pisafe/ha.sock`; `limactl list` then falsely labels the
instance `Broken`. Run the diagnostic with the needed host permission before
acting on that status. Do not delete or recreate the VM based only on the
sandboxed result.

## Known gaps

- No SSH artifact transport is wired to `gitstage.Prepare` or
  `gitstage.Materialize`.
- No pinned Pi run image or hardened per-run container exists.
- No run manifests or controller state directory exist.
- No user-facing `run`, `list`, `connect`, `diff`, `apply`, `cp`, `discard`, or
  `gc` implementation exists.
- Selected untracked/ignored input archive handling is missing.
- Submodule staging and journaled multi-repository apply are missing.
- The broker port set and reverse SSH inference relay are not implemented.
- No inference broker or OAuth/Keychain integration exists.
- Firewall behavioral coverage still needs DNS-to-private answers, redirects,
  raw UDP, `host.containers.internal`, VM loopback attempts, and the exact
  broker exception.
- Existing Lima configuration drift is not detected or reconciled. The
  manager reuses an existing instance based on name/status.
- The run container resource limits and filesystem hardening remain to be
  implemented.

## Next implementation slice

Build the SSH-stream transport and first hardened run container:

1. Add a transport abstraction whose real implementation executes through the
   dedicated Lima control SSH connection.
2. Stream the stage bundle and tracked patch into a newly allocated run
   directory; do not use a host mount or Podman socket.
3. Add a pinned ARM64 `Containerfile` with Pi, Git, an SSH server, and only the
   tools needed for the first end-to-end test.
4. Start one rootless Podman container with:
   - read-only root;
   - non-root user;
   - dropped capabilities;
   - `no-new-privileges`;
   - no container socket;
   - CPU, memory, PID, disk, and wall-clock limits; and
   - a unique workspace volume and SSH endpoint.
5. Invoke `gitstage.Materialize` inside that boundary.
6. Add a minimal run manifest and state transitions for `creating → active →
   stopped`.
7. Wire only enough `pisafe run`, `list`, and `discard` behavior to prove
   acceptance tests 1, 2, 3, 7, 10, and 12.
8. Keep inference unavailable until the reverse relay and run-scoped
   capability are implemented; never place a raw provider credential in the
   VM as a temporary shortcut.

After that, implement selected untracked inputs and submodule-aware,
journaled apply before exposing `pisafe apply`.

## Useful references

- Lima plain mode: <https://lima-vm.io/docs/config/plain/>
- Lima SSH: <https://lima-vm.io/docs/usage/ssh/>
- Lima Podman example: <https://lima-vm.io/docs/examples/containers/podman/>
- Podman rootless networking:
  <https://docs.podman.io/en/stable/markdown/podman-network.1.html>
