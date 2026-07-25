# Implementation progress

Last updated: 2026-07-25

This file is the durable handoff for continuing `pisafe` from a fresh session.
The design authority remains [`pisafe-design.md`](pisafe-design.md); this file
records what is implemented, what has been verified, and what should happen
next.

## Current milestone

Phase 1 is in progress. Eight implementation slices now exist:

1. The Go controller skeleton and single-repository Git isolation core.
2. The dedicated Lima VM backend, host-network discovery, and initial static
   firewall boundary.
3. The SSH artifact transport, durable run records, pinned run image, hardened
   Podman launch contract, Linux materialization helper, and internal run
   creation transaction.
4. Per-run SSH credentials, non-root container `sshd`, strict host-key
   pinning, and a portless Lima control-SSH ProxyCommand.
5. User-facing run creation, fresh/reused VM orchestration, packaged image
   artifacts, excluded-input reporting, and explicit Zed connection launch.
6. Stop/resume, exact-confirmation discard and failed-creation cleanup
   recovery, cumulative wall-clock enforcement, and quota-backed persistent
   run storage.
7. The reverse inference relay: a static broker firewall exception, run-scoped
   revocable capabilities in the manifest lifecycle, the Mac-side `pisafe
   broker` relay with fail-closed request contract, and run-side Pi provider
   configuration.
8. `pisafe login chatgpt`: the ChatGPT subscription OAuth flow with macOS
   Keychain persistence, the Codex upstream with broker-side token refresh,
   and the curated run-side model catalog. This replaced the interim
   `PISAFE_INFERENCE_*` environment configuration.

`pisafe run` now uses the tested mountless path and materializes inside private
quota-backed VM storage. Do not add a local-workspace fallback.

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
- Untracked or ignored inputs enter a run only when `pisafe run --include PATH`
  names them. Selection resolves paths against the repository, expands
  directories, and refuses anything Git tracks, anything outside the
  repository, symlinks pointing out of it, special files, and selections over
  the file-count or byte limits. Credential-shaped names need
  `--include-unsafe PATH`; the message states that including one lets
  everything in the run read and exfiltrate it.
- Selected inputs travel as a tar beside the bundle and patch, are re-validated
  on extraction (no absolute, climbing, or `.git` paths, no non-regular
  entries), must match the names recorded in the staged snapshot, and are
  staged with literal pathspecs so they join the baseline commit.
- Apply is split in the opposite direction:
  - `gitstage.PrepareApply` creates an incremental bundle in the isolated
    environment.
  - `gitstage.ImportApply` verifies and imports it on the Mac.
- Apply creates only `refs/heads/pisafe/<run>` with a compare-and-swap
  `git update-ref`; it does not change the current branch, index, or working
  tree.
- Initialized submodules are staged with the superproject: each one
  contributes its own bundle and tracked-state patch, is reconstructed in the
  run, absorbs its git directory, and is registered from `.gitmodules`. Its
  dirty tracked state becomes a baseline commit inside the submodule, and the
  superproject baseline records the gitlink the submodule actually ended up
  at. Uninitialized submodules stay uninitialized.
- The superproject patch ignores submodule changes, so submodule state travels
  only through the submodule's own artifacts.
- Nested submodules and Git LFS fail closed rather than staging incomplete
  repositories.
- External diff and text-conversion drivers are disabled during host capture.
- NUL-delimited parsing preserves unusual Git filenames.
- User-facing staging reports untracked and ignored inputs separately,
  excludes everything not explicitly selected, and lists what was included.
  Output is safely quoted and capped to remain readable.

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
- A dedicated `192.0.2.1/32` dummy address carries the inference-broker
  reverse relay.
- SSH remote forwarding is restricted to exactly `192.0.2.1:18080` through
  `PermitListen`; there is no dynamic broker port set.
- The generated configuration removes the cloud image's unrestricted
  passwordless sudo and removes the Lima user from `wheel`. It grants only
  two exact no-argument helpers: clock synchronization and firewall status.

### Firewall

- A boot-persistent `pisafe-firewall.service` owns the nftables ruleset.
- `firewalld` is disabled in the dedicated VM.
- All three chains accept established/related conntrack traffic, so policy
  gates connection initiation and conntrack owns replies.
- Input otherwise defaults to drop, with DHCP replies, control SSH, and
  exactly `192.0.2.1:18080` (the broker relay) admitted.
- Both output and forward hooks deny new connections toward:
  - IPv4 loopback;
  - RFC1918;
  - CGNAT;
  - link-local and metadata;
  - TEST-NET broker space except the static `192.0.2.1:18080` exception;
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
- Stage files are imported with `podman unshare` into a private fixed-capacity
  filesystem; no Mac mount, Podman socket, or unsupported
  `podman cp --chown` path is used.
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
- `runimage.Installer` derives a recipe digest from the exact embedded
  Containerfile and static guest-helper bytes, streams a tar context containing
  only those two files, and passes that digest into an image label.
- The macOS controller embeds the Containerfile. Release/development layouts
  provide a sibling `pisafe-guest-linux-arm64` sidecar; an explicit
  `PISAFE_GUEST_HELPER` path is available for development.
- A mutable, recipe-derived local tag is used only as a cache key. Reuse
  requires matching recipe, base, and Pi labels, Linux/ARM64 platform, and a
  valid immutable SHA-256 image ID. Run containers continue to receive only
  that immutable ID.
- Artifact loading rejects symlinks, file swaps, oversize inputs, non-ELF or
  non-ARM64 helpers, dynamic interpreters, and imported shared libraries.
- The image contains Git, OpenSSH, Tini, Pi, and the static ARM64 guest helper.
- Build-time SSH host keys are removed. A network-disabled one-shot container
  generates each run's host key and installs only its client public key into
  private quota-backed home storage.
- Run commands require an immutable `sha256:` image ID and use:
  - UID/GID 1000;
  - read-only root;
  - all capabilities dropped;
  - `no-new-privileges`;
  - rootless pasta networking with explicit public DNS;
  - 2 CPUs, 4 GiB memory with no extra swap, and 512 PIDs;
  - bounded `/tmp` and `/run` tmpfs mounts; and
  - unique workspace and home directories inside a run-scoped,
    fixed-capacity filesystem owned by the mapped non-root user.
- No Podman/Docker socket, forwarded SSH agent, or credential environment is
  added.
- Each run receives one sparse 10 GiB ext4 filesystem shared by its persistent
  workspace and home. Root-owned image storage and a narrow fixed-policy helper
  prevent the rootless VM user from resizing or remounting it.
- Podman's independent `--timeout` enforces the run's remaining active budget
  even after the Mac controller exits.

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

### Inference broker relay and run-scoped capability

- `internal/broker` is the Mac-side relay. Provider credentials never leave
  the Mac; runs authenticate with a `pisafe-cap-<64 hex>` capability minted
  from 32 bytes of `crypto/rand`.
- Capabilities live only in the version-4 manifest: activation and resume
  require a fresh one, stop and discard clear it, and a stored manifest is
  invalid if an inactive state retains one. The broker re-reads the durable
  records on every request, so a stopped, discarded, or wall-clock-exhausted
  run is rejected immediately with the same uniform 401. Matching is
  constant-time over SHA-256 digests.
- The relay accepts exactly one method and path derived from the configured
  API (`POST /v1/messages`, `/v1/chat/completions`, `/v1/responses`, or
  `/codex/responses`), rejects unknown paths/methods, caps request bodies at
  64 MiB, caps each run at 4 concurrent upstream requests, refuses upstream
  redirects, and streams responses (SSE-safe flushing) without rewriting
  them. Client Content-Type/Accept/Content-Encoding, the Anthropic and
  OpenAI beta/version headers, originator, session-id, x-client-request-id,
  and User-Agent are forwarded; credentials never are, because the broker
  sets its own from a per-request credential source.
- `pisafe broker` runs in the foreground: it verifies the VM boundary, serves
  the relay on an ephemeral `127.0.0.1` port, and publishes it at
  `192.0.2.1:18080` inside the VM through a dedicated
  `ssh -N -R` child over Lima's generated SSH config with
  `ExitOnForwardFailure` and multiplexing disabled. The VM listener dies with
  the process, a second broker fails loudly instead of stealing the binding,
  and startup confirms reachability by probing `/dev/tcp/192.0.2.1/18080`
  from inside the VM.
- Run creation and resume exec `pisafe-guest configure-inference`, which
  atomically installs a validated `~/.pi/agent/models.json` (mode 0600) whose
  `apiKey` is the run's current capability, and pins `transport: "sse"` in
  the run's Pi settings (merging, because Pi writes that file too) since
  Pi's default auto transport dials a WebSocket the HTTP relay cannot speak.
  The pinned Pi clients were inspected to fix the base URLs:
  `http://192.0.2.1:18080` for anthropic-messages and the Codex API,
  `http://192.0.2.1:18080/v1` for the standard OpenAI APIs.
- The relay accepts the capability as `x-api-key`, a Bearer token, or the
  JWT-wrapped Bearer token the Codex client requires (the capability rides
  as the signature segment and is stripped before constant-time matching).

### ChatGPT subscription login

- `pisafe login chatgpt` runs the browser OAuth flow: PKCE S256 against
  `auth.openai.com` with the exact client ID, scope, redirect URI
  (`localhost:1455`), and authorize parameters of the pinned Pi AI client;
  the callback page is served locally with escaped output and a strict state
  check, and the code exchange yields access/refresh tokens plus the ChatGPT
  account ID extracted from the access-token JWT claim.
- The credential persists in the login keychain (`security` service
  `pisafe`, account `chatgpt`), written over `/usr/bin/security`'s
  interactive stdin base64-wrapped so tokens never appear in argv. A missing
  item maps to a distinct not-logged-in error that the CLI turns into a
  `pisafe login chatgpt` prompt.
- The broker holds a serialized credential source: access tokens refresh
  proactively within five minutes of expiry and the rotated refresh token is
  persisted before use. `pisafe broker` forces one credential check at
  startup so a dead login fails there, loudly, not inside runs. Upstream
  requests to `https://chatgpt.com/backend-api/codex/responses` carry
  `Authorization: Bearer` plus the real `chatgpt-account-id`; the run only
  ever sees the placeholder account ID `pisafe` in its wrapped capability.
- Runs receive a curated model catalog embedded from the pinned Pi AI Codex
  data (context windows, cost rates, thinking-level maps) with per-model
  `api`/`provider`/`baseUrl`/`headers` stripped so models.json can never
  route a run around the broker.

### Run records and internal controller

- `internal/runstate` writes version-4, mode-0600 JSON manifests atomically
  under the user config directory (or `PISAFE_STATE_DIR`).
- It enforces `creating → active → stopped → active|imported|discarded|expired`,
  and binds one inference capability to exactly the active state.
- Failed creation remains visibly `creating` with `last_error`; it is not
  silently deleted or mislabeled active.
- `internal/runctl.StartPrepared` composes stage upload, private-storage import,
  per-run key generation, network-disabled SSH initialization, hardened
  container start, host-key pinning, in-container materialization, transfer
  cleanup, manifest activation, and bounded rollback.
- Creation rollback removes partial SSH client state along with the container,
  persistent filesystem, and VM-side transfer directory. Allocation commands
  are treated as potentially successful even when their transport response
  fails, and the fixed storage helper independently removes exact partial
  image or mount-directory artifacts.
- Activation records the baseline commit returned by actual in-container
  materialization rather than retaining the host's pre-materialization
  placeholder.
- `pisafe run` now:
  - resolves the current Git root and creates a project-derived run ID with a
    UTC timestamp plus 48 bits of cryptographic entropy;
  - discovers the current Mac networks;
  - creates or reuses and verifies the dedicated VM;
  - installs/reuses the managed image;
  - prepares and starts the run through `runctl`; and
  - prints the run, workspace, branch, exact `ssh -F` command, excluded input
    summary, and whether `pisafe broker` will serve inference.
- `pisafe zed <run>` opens an active connection after the user has explicitly
  saved the printed `ssh -F` command through Zed's "Connect New Server" flow.
  PiSafe never edits global SSH or Zed settings.
- `pisafe list` displays the durable records.
- `pisafe stop <run>` stops and removes only the container, accounts elapsed
  active seconds conservatively, and preserves quota-backed storage and SSH
  identity.
- `pisafe resume <run>` verifies the current VM boundary, storage identity,
  image, container identity, and mount sources before recreating the container
  with only its remaining eight-hour cumulative active budget.
- `pisafe discard <run> --confirm <run>` requires an exact repeated ID, stops
  active work, and idempotently removes the exact container, storage
  filesystem image, transfer stage, and Mac-side SSH secret before recording the
  discard event. Failed `creating` records use the same recovery path.

## Tests and verification

Normal checks:

```sh
go test -race -cover ./...
go vet ./...
go build -trimpath -buildvcs=false ./cmd/pisafe
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
  go build -trimpath -buildvcs=false ./cmd/pisafe-guest
git diff --check
```

Current package coverage at this milestone:

```text
pisafe        0.0%
pisafe-guest  60.0%
broker        96.2%
chatgpt       70.1%
cli           24.2%
gitstage      76.5%
hostnet       50.0%
lima          68.9%
runcontainer  66.7%
runctl        67.2%
runid         92.3%
runimage      74.6%
runssh        68.0%
runstart      79.5%
runstate      71.8%
```

The broker contract is covered directly: capability auth against durable
records (unknown, malformed, stopped, and deadline-exhausted capabilities all
return the same 401), missing-provider 503, method/path/oversize fail-closed
responses, upstream credential injection without leaking the client header,
SSE streaming fidelity, redirect refusal, and the per-run concurrency cap.
The Codex path additionally covers the JWT-wrapped capability, the
account-id header replacement, forwarded client headers, and the 503 (with
no detail leak) when credentials cannot be refreshed. The chatgpt package
covers the full login exchange and state rejection against a stub token
endpoint, proactive refresh with persistence of the rotated refresh token,
Keychain round-trips through a fake `security` runner, and the embedded
catalog's no-routing-override invariant.

Input selection is covered end to end against real repositories: a run whose
baseline commit carries a selected untracked file, an ignored build artifact,
an executable, and a symlink, leaving the staged workspace clean and the
source untouched; refusal of credential-shaped names with and without the
unsafe flag, including a directory that contains one; refusal of tracked,
missing, outside, escaping-symlink, special, and whole-repository selections;
the per-file size limit; archive entries that climb, are absolute, name
`.git`, or are devices; a snapshot/archive name mismatch; and the whole-word
secret matcher.

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
- That session's image was:

  ```text
  recipe digest:   sha256:2ffa36731cbcc11510a87a1f2dbe205d788407cb3e8ba60dc74387f5471ad052
  image ID:        sha256:741fdd45acd031b010bd694b2a3bddeeaed1c57be531d98fc6ef4f95c7fa49b5
  manifest digest: sha256:adf1001f96e66a172cddf8d3f6964b943c0605ef9843f58f4fd863948153c6c0
  ```

- The managed installer built that image from its two-file tar stream,
  validated its labels/platform/immutable ID, and reused it on the next call.
- That verification session's VM used security profile
  `sha256:6a9c31943325290e23272c7c95af4ae80df8c94718c3ed8ec5e1824efbfcc927`.
- The storage helper created an exact mapped-UID 10 GiB ext4 filesystem,
  accepted an allocation within the limit, rejected an over-limit allocation,
  and removed both its mount and sparse backing image.
- A disposable Podman container with a two-second `--timeout` was killed
  independently and left exited for lifecycle reconciliation.
- A direct runtime check verified UID 1000, zero effective/bounding
  capabilities, `NoNewPrivs=1`, read-only root, 4 GiB memory, 512 PIDs,
  Pi 0.82.0, and public HTTPS.
- `TestLiveSSHStageAndContainerMaterialize` passed against that exact image:
  dirty tracked state crossed Lima SSH, entered private quota-backed storage,
  materialized inside the hardened container, and could be deleted there
  without altering the Mac checkout.
- That same live test generated fresh client and host keys, pinned the host
  key, connected with the generated ProxyCommand, and wrote a file through the
  Zed-compatible OpenSSH session. A Pi executable in the same container saw
  that exact file and workspace. The SSH session ran as UID 1000 with no
  forwarded agent or `/Users`, and `podman port` reported no published port.
- The actual `pisafe run` CLI was exercised from this working tree with
  isolated temporary Mac state. It reused the verified VM, installed the
  current recipe, created an active manifest containing the real dirty
  baseline commit, reported all five then-untracked implementation files as
  excluded, and connected through its printed SSH command as UID 1000. The
  imported baseline was clean and the original checkout remained unchanged.
- The actual CLI then completed
  `run → stop → resume → SSH → discard --confirm`: stop preserved the
  workspace, resume reused the pinned SSH host identity and connected as UID
  1000 with the reduced budget, and discard left a version-3 audit manifest
  while removing the container, persistent filesystem, remote stage, and
  client key.

Run that end-to-end test with:

```sh
PISAFE_LIVE_LIMA=1 \
PISAFE_LIVE_RUN_IMAGE=sha256:cfd452f96a7948fbf964d6870dad57dc81da3b82393d731bf701736c0092243a \
go test -v -run TestLiveSSHStageAndContainerMaterialize ./internal/lima
```

## Live VM state

A persistent Lima instance named `pisafe` was left running with security
profile
`sha256:35c2cd370359201ce6861c91bc7fb25d8ada1497cf1db2d29c0017eea7e1f459`.
It contains cached base/test images plus the current managed run image
`sha256:cfd452f96a7948fbf964d6870dad57dc81da3b82393d731bf701736c0092243a`.
The recipe digest last moved when the guest helper began pinning Pi's
transport; two consecutive runs built it once and reused it, which live-checks
content-addressed reuse.

The broker-relay slice is fully live-verified against this VM. The first
live run of the relay tests failed: sshd bound `192.0.2.1:18080` correctly,
but its SYN-ACK replies carry the client's ephemeral port, matched no accept
rule in the then-stateless output chain, and were rejected by the TEST-NET
deny — every broker connection timed out. The template now accepts
established/related conntrack traffic in the output and forward chains (the
input chain always did); after recreation the full gated lima and runimage
suites passed, including `TestLiveBrokerReverseRelay`,
`TestLiveSecondRelayFailsClosed`, and the end-to-end staging test above.

The brokered inference chain was then verified with the real CLI against a
loopback stub upstream, configured through the interim environment provider
that the ChatGPT login has since replaced: `pisafe run` installed a
correct `models.json`, and from inside the run container, requests through
`http://192.0.2.1:18080` traversed pasta, the firewall, the reverse forward,
and broker capability auth to reach the stub carrying the upstream key —
plain JSON and streamed SSE both intact. A wrong capability got 401, a
non-canonical path 404, the capability replayed after `pisafe stop` got 401,
and `pisafe resume` rotated the capability, after which the new one worked
inside the recreated container. The scratch run was discarded; no containers
remain.

The ChatGPT subscription slice was then verified against the real provider on
the same VM. `pisafe login chatgpt` completed the browser OAuth flow and stored
the credential in the Keychain, `pisafe broker` passed its startup credential
check and reported the Codex upstream, and a run created afterwards drove real
Pi conversations through the relay. Inspection of that run confirmed the
intended contract end to end:

- `settings.json` carries `"transport": "sse"` merged with the settings Pi
  itself wrote during the session (theme, default provider/model, thinking
  level), so the pin neither blocks nor discards Pi's own state;
- `models.json` exposes only the curated catalog under provider `pisafe` with
  `baseUrl` `http://192.0.2.1:18080` and `api` `openai-codex-responses`, and no
  per-model routing override;
- its `apiKey` is the unsigned-JWT wrapper whose payload carries the
  placeholder account id `pisafe` and whose signature segment is the run-scoped
  capability, and Pi's Codex client accepted it;
- the session record shows four assistant turns served entirely by provider
  `pisafe`, across two catalog models (`gpt-5.6-terra` and `gpt-5.6-luna`), so
  model selection works through the relay; and
- the run holds no provider credential: Pi's `auth.json` is `{}`, no
  OpenAI/ChatGPT environment variable exists in the container, and the real
  access token and account id appear only on the broker's upstream request.

Everything below was first verified against profile
`sha256:6a9c31943325290e23272c7c95af4ae80df8c94718c3ed8ec5e1824efbfcc927`
and re-verified by the gated suite during the two recreations above.

Fresh provisioning verified all security-sensitive setup together:

- automatic subordinate UID/GID assignment and `podman system migrate`;
- the clock-step, firewall-status, and fixed-policy run-storage helpers work
  through their narrow sudo rules;
- unrestricted `sudo -n true` is denied;
- the root-owned security-profile record is mode 0444 and matches the
  generated definition;
- the full container network suite passes; and
- the managed image install/reuse and end-to-end staging tests pass.

The final cleanup audit found no run-labelled containers, run filesystems, or
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
8. Fedora Podman not supporting `podman cp --chown`; stage transfer first used
   local-volume import and now uses `podman unshare` extraction into
   quota-backed storage.
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
12. Podman's persistent local-volume `size` option requires root and supports
    quota only on XFS, while the pinned Fedora VM stores its writable disk on
    Btrfs. A Btrfs parent qgroup can be bypassed with uncharged nested
    subvolumes, so runs use fixed-size ext4 filesystem images through a
    fixed-policy helper instead of either mechanism.
13. Container UID 1000 maps to subordinate host UID/GID 100999 in rootless
    Podman. The storage helper derives those IDs from `/etc/subuid` and
    `/etc/subgid`, and stage import runs inside `podman unshare`.
14. On this host, Lima 2.2.0 repeatedly booted a stopped VZ VM without
    restoring its SSH path, including with no run storage present. Native
    `vzNAT` was tested and had the same failure, so it was not retained.
    Fresh VM creation is reliable; stopped-VM restart remains an explicit
    upstream/platform gap.

When Codex runs inside a restricted filesystem sandbox, it may be unable to
connect to `~/.lima/pisafe/ha.sock`; `limactl list` then falsely labels the
instance `Broken`. Run the diagnostic with the needed host permission before
acting on that status. Do not delete or recreate the VM based only on the
sandboxed result.

## Known gaps

- No user-facing `connect`, `diff`, `apply`, `cp`, or `gc` implementation
  exists.
- `pisafe zed` launches a connection that was explicitly saved once in Zed.
  Fully automatic first-launch remains intentionally absent because Zed's CLI
  URL cannot carry `-F` and PiSafe does not silently edit global SSH or Zed
  settings.
- Journaled multi-repository apply is missing: a run of a repository with
  submodules stages and materializes correctly, but `ImportApply` still
  updates only the superproject ref, so applying such a run would create a
  branch whose gitlinks name commits the Mac does not have. `apply` is not
  exposed as a command yet, so this is not reachable from the CLI.
- Login, the Keychain round trip, and the real Codex handshake are
  live-verified, but broker-side token refresh has only ever run against a
  stub: the live session never crossed an access-token expiry. The first
  long-lived broker will exercise it.
- The OAuth flow and the embedded model catalog mirror the pinned Pi AI
  0.82.0 client; both must be re-checked whenever the Pi pin moves, and the
  subscription backend can change underneath them (inference then fails
  closed and loudly).
- While no broker is connected, a process escaped to the unprivileged VM user
  could bind `192.0.2.1:18080` itself. It gains nothing beyond what that user
  already has (it relays pasta traffic and can read run storage), and a real
  broker then fails loudly at bind time instead of silently coexisting.
- Firewall behavioral coverage still needs DNS-to-private answers, redirects,
  raw UDP, `host.containers.internal`, and VM loopback attempts; the exact
  broker exception is covered by passing gated live tests.
- Security-profile drift is detected and fails closed, but automated
  replacement/reconciliation is intentionally absent because deleting a VM is
  destructive and must be an explicit lifecycle operation.
- Run stop/resume is live-verified while the dedicated VM remains running.
  On this host, a cleanly stopped Lima 2.2.0 VZ VM boots but does not regain
  SSH over either default user-mode networking or `vzNAT`; automatic
  stopped-VM recovery is therefore not yet reliable.
- Pi's top-level tarball is integrity-pinned, but a reproducible published
  image/digest workflow is still needed to freeze transitive npm resolution.

## Next implementation slice

Continue Phase 1 without weakening the boundary:

1. Add journaled multi-repository apply before exposing `pisafe apply`:
   import the submodule histories, record the intended old/new refs in the run
   manifest, and update them one repository at a time with compare-and-swap
   recovery.
2. Add `diff`, hardened `cp`, and seven-day GC after the apply transaction is
   durable.

## Useful references

- Lima plain mode: <https://lima-vm.io/docs/config/plain/>
- Lima SSH: <https://lima-vm.io/docs/usage/ssh/>
- Lima Podman example: <https://lima-vm.io/docs/examples/containers/podman/>
- Podman rootless networking:
  <https://docs.podman.io/en/stable/markdown/podman-network.1.html>
