# pisafe

`pisafe` is an implementation in progress of the isolation model described in
[`pisafe-design.md`](pisafe-design.md).

The first implementation slice contains:

- a dependency-free Go controller;
- `pisafe doctor` for host prerequisite checks;
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
on-link prefixes and refreshes the corresponding nftables set at start/resume.

The run lifecycle is not exposed yet. It still needs the SSH artifact
transport, hardened run image/container, and submodule-aware journaled apply
protocol.

## Development

```sh
go test ./...
go build ./cmd/pisafe
./pisafe doctor
```

The gated live suite creates or reuses the dedicated `pisafe` VM and exercises
the mount, rootless-container, and network boundaries:

```sh
PISAFE_LIVE_LIMA=1 go test -v ./internal/lima
```
