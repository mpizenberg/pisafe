# pisafe

`pisafe` is an implementation in progress of the isolation model described in
[`pisafe-design.md`](pisafe-design.md).

The implementation now contains:

- a dependency-free Go controller;
- `pisafe doctor` for host prerequisite checks and `pisafe list` for durable
  run records;
- a split Git staging core: the Mac produces a bundle and tracked-state patch,
  while materialization happens after transfer inside the isolated
  environment;
- tracked dirty-state baseline capture;
- final tracked-state capture;
- split apply preparation/import, with SHA-256 verification and a
  compare-and-swap update of a new `pisafe/<run>` branch; and
- tests proving workspace deletion and apply do not modify the source checkout.

The Lima backend now also generates and manages a dedicated plain-mode Fedora
VM with a pinned image digest, no host mounts, no forwarded agent or Podman
socket, rootless Podman, disabled IPv6, and an nftables boundary covering both
VM and container egress. The controller discovers the Mac's active IPv4
on-link prefixes and verifies the VM's baked-in deny set at start/resume,
failing closed if the Mac has changed networks.

The next boundary slice is also implemented internally:

- size- and SHA-256-verified Git artifact streams through Lima control SSH;
- atomic Mac-side run manifests and strict lifecycle transitions;
- a pinned ARM64 run image containing Pi 0.82.0 and a Linux guest helper;
- rootless Podman launch arguments enforcing a non-root user, read-only root,
  dropped capabilities, `no-new-privileges`, public DNS, and CPU, memory, PID,
  and temporary-filesystem limits; and
- a controller transaction that imports the stage into private volumes,
  materializes it inside the container, and rolls back partial creation;
- content-addressed run-image installation that sends only the Containerfile
  and static Linux helper and returns a validated immutable image ID; and
- a root-owned VM security-profile fingerprint checked on every start, so an
  instance created from an older security definition fails closed.

User-facing run creation is still hidden. Fresh-VM provisioning, restricted
sudo, clock synchronization, security-profile drift detection, managed-image
installation/reuse, and end-to-end repository materialization are
live-validated. Persistent disk quota, wall-clock enforcement, per-run SSH/Zed
access, inference brokering, and confirmed discard must still be completed.

## Development

```sh
go test ./...
go build ./cmd/pisafe ./cmd/pisafe-guest
./pisafe doctor
./pisafe list
```

The gated live suite creates or reuses the dedicated `pisafe` VM and exercises
the mount, rootless-container, and network boundaries:

```sh
PISAFE_LIVE_LIMA=1 go test -v ./internal/lima
PISAFE_LIVE_LIMA=1 go test -v ./internal/runimage
```

The end-to-end artifact/container test additionally requires the immutable ID
of a locally built run image:

```sh
PISAFE_LIVE_LIMA=1 \
PISAFE_LIVE_RUN_IMAGE=sha256:<image-id> \
go test -v -run TestLiveSSHStageAndContainerMaterialize ./internal/lima
```
