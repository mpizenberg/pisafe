# pisafe

`pisafe` is an implementation in progress of the isolation model described in
[`pisafe-design.md`](pisafe-design.md).

The implementation now contains:

- a dependency-free Go controller;
- `pisafe run` for creating an isolated run from the current repository,
  `pisafe stop`/`resume` for preserving and reopening it, `pisafe diff` for
  reporting what it changed without stopping it, `pisafe cp` for taking a
  file or directory back out of it, `pisafe apply` for
  importing its commits, exact-confirmation `pisafe discard`, `pisafe gc` for
  the seven-day retention sweep, `pisafe list`
  for durable records, `pisafe zed` for reopening a connection explicitly
  saved in Zed, and `pisafe doctor` for prerequisites;
- a split Git staging core: the Mac produces a bundle and tracked-state patch
  for the superproject and for each initialized submodule, while
  materialization happens after transfer inside the isolated environment;
- the Git identity the user commits with in the source repository, resolved on
  the Mac and installed in the run so an agent's commits are attributed to
  them; a repository with none refuses to start a run;
- tracked dirty-state baseline capture, plus explicitly selected untracked or
  ignored inputs, which are validated, archived, and committed into that same
  baseline while credential-shaped names require an unsafe override;
- final tracked-state capture;
- split apply preparation/import, with SHA-256 verification and a journaled,
  idempotent compare-and-swap creation of a new `pisafe/<run>` branch in the
  superproject and in each changed submodule, the plan recorded in the run
  manifest before the first ref moves; and
- tests proving workspace deletion and apply do not modify the source checkout.

The Lima backend now also generates and manages a dedicated plain-mode Fedora
VM with a pinned image digest, no host mounts, no forwarded agent or Podman
socket, rootless Podman, disabled IPv6, and an nftables boundary covering both
VM and container egress. The controller discovers the Mac's active IPv4
on-link prefixes and verifies the VM's baked-in deny set at start/resume,
failing closed if the Mac has changed networks.

The next boundary slice is also implemented internally:

- size- and SHA-256-verified Git artifact streams through Lima control SSH;
- atomic Mac-side run manifests, cumulative active-time accounting, and
  strict lifecycle transitions;
- a pinned ARM64 run image containing Pi 0.82.0 and a Linux guest helper;
- rootless Podman launch arguments enforcing a non-root user, read-only root,
  dropped capabilities, `no-new-privileges`, public DNS, and CPU, memory, PID,
  and temporary-filesystem limits; and
- a controller transaction that imports the stage into private,
  quota-limited storage,
  materializes it inside the container, and rolls back partial creation;
- content-addressed run-image installation that sends only the embedded
  Containerfile and packaged static Linux helper and returns a validated
  immutable image ID; and
- a root-owned VM security-profile fingerprint checked on every start, so an
  instance created from an older security definition fails closed; and
- unique per-run Ed25519 client and host keys, a non-root loopback-only SSH
  daemon, strict host-key pinning, and a portless ProxyCommand through Lima's
  control SSH connection.

Fresh-VM provisioning, restricted sudo, clock synchronization,
security-profile drift detection, managed-image installation/reuse,
user-facing repository materialization, and the Zed-compatible OpenSSH path
into the exact Pi workspace are live-validated. `pisafe run` prints the exact
OpenSSH command to paste once into Zed's Remote Projects dialog; it does not
silently edit global SSH or Zed settings. Each run has a live-validated
10 GiB fixed-capacity persistent filesystem and eight cumulative active hours
enforced by Podman. Stop/resume and confirmed discard are live-validated.
`pisafe apply RUN` stops the run, captures it in a throwaway network-less
container, streams the verified bundles back, and creates `pisafe/RUN` in the
superproject and each changed submodule without touching the checkout. It is
live-validated against a run whose commits, submodule commit, uncommitted
changes, and untracked leftovers all landed exactly where the design says.
An imported run keeps its workspace until `pisafe discard` reclaims it.
`pisafe diff RUN` reports the run's commits, changed paths with line counts,
and untracked leftovers from a throwaway container holding the workspace
read-only, so it works on an active, stopped, or imported run without
disturbing it. It reports names and counts, never file content: everything it
names was written inside the run, so it is quoted rather than rendered.
`pisafe cp RUN:PATH [DEST]` takes one file or directory back out through the
same read-only container. Only regular files and directories are copied; a
symlink or special file stops the copy naming its path, the Mac re-validates
every archive entry and writes through a directory handle that no entry can
escape, and the copy lands beside the destination and is moved into place only
once it has all arrived. An existing destination is replaced only with
`--force`, and is removed rather than written through.
`pisafe gc [--dry-run]` reclaims what the seven-day retention window released:
an imported run's workspace, storage, and SSH key are removed and its record
becomes `expired` while keeping the branch and import timestamps, so a
`pisafe/RUN` branch stays attributable long after its workspace is gone. A
discarded record that names no branch is removed after the same week; one that
names a branch is kept indefinitely. A run whose work was never imported is
only reported, never reclaimed by age — `pisafe discard` remains the way to
release it. Superseded managed run images are pruned, keeping the current
recipe's image and any image a run can still start a container from.
`pisafe broker` relays inference from the Mac into runs over a reverse SSH
forward to `192.0.2.1:18080`, the firewall's single static exception; runs
hold only a revocable per-run capability, never a provider credential.

## Development

```sh
go test ./...
go build -trimpath -buildvcs=false -o pisafe ./cmd/pisafe
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
  go build -trimpath -buildvcs=false \
  -o pisafe-guest-linux-arm64 ./cmd/pisafe-guest
./pisafe doctor
./pisafe list
./pisafe run [--include PATH]... [--include-unsafe PATH]...
./pisafe stop RUN
./pisafe resume RUN
./pisafe diff RUN
./pisafe cp RUN:PATH [DEST] [--force]
./pisafe apply RUN
./pisafe discard RUN --confirm RUN
./pisafe gc [--dry-run]
./pisafe login chatgpt
./pisafe broker
```

The release layout places `pisafe-guest-linux-arm64` beside `pisafe`. During
development, `PISAFE_GUEST_HELPER=/absolute/path/to/helper` may select the
sidecar explicitly. The Containerfile is compiled into the controller.

Pi inference works while `pisafe broker` runs on the Mac. Run
`pisafe login chatgpt` once to store a ChatGPT Plus/Pro subscription login in
the macOS Keychain; the broker refreshes and attaches those tokens itself,
and no provider credential ever enters the VM or a run.

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
