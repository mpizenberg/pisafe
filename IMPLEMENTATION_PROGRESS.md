# Implementation progress

Last updated: 2026-07-27

The durable handoff for continuing `pisafe` from a fresh session: what is
implemented, what has been verified against a real VM, and what comes next.
[`pisafe-design.md`](pisafe-design.md) is the authority on what must hold, and
[`DECISIONS.md`](DECISIONS.md) records why the implementation looks as it does.

## Current milestone

Phase 1 is in progress. Every command the design enumerates for it exists:
`run`, `list`, `connect`, `stop`, `resume`, `diff`, `cp`, `apply`, `discard`,
`gc`, `zed`, `login`, `broker`, `doctor`. Built in twelve slices: the controller
and Git isolation core; the Lima VM backend and firewall; SSH transport, run
records, run image and container contract; per-run SSH credentials; user-facing
run creation; stop/resume/discard and quota-backed storage; the reverse
inference relay; ChatGPT subscription login; getting work back out (`diff`,
`cp`, `gc`); terminal access without an editor (`connect`); the
keep-or-replay choice about a run's carried-in baseline commit; and freezing the
run image's transitive npm resolution.

`pisafe run` uses the tested mountless path and materializes inside private
quota-backed VM storage. **Do not add a local-workspace fallback.**

## Implemented

### Git boundary

- Dependency-free Go module, `cmd/pisafe` and `cmd/pisafe-guest`.
- Staging is split at the boundary: `gitstage.Prepare` runs on the Mac and
  produces a Git bundle plus a binary patch of the final tracked state;
  `gitstage.Materialize` consumes them inside the run and never sees the Mac
  source path. Dirty tracked state becomes a labelled baseline commit.
- Untracked and ignored inputs enter only through `--include PATH`. Selection
  refuses tracked files, paths outside the repository, escaping symlinks,
  special files, and over-limit selections; credential-shaped names need
  `--include-unsafe`. They travel as a tar beside the bundle, are re-validated
  on extraction against the names in the staged snapshot, and join the baseline
  commit.
- Initialized submodules each contribute their own bundle and patch, are
  reconstructed and registered from `.gitmodules`, and get their own baseline
  commit; the superproject baseline records the gitlink they actually ended up
  at, and its patch ignores submodules so gitlink changes travel once.
  Uninitialized submodules stay uninitialized. Nested submodules and Git LFS
  fail closed. External diff/textconv drivers are disabled during capture, and
  parsing is NUL-delimited so unusual filenames survive.
- Apply is split in the opposite direction and journaled: `PrepareApply`
  commits what the agent left uncommitted (submodules first), `ImportApply`
  verifies and fetches every object set into temporary refs before anything
  user-visible changes, `CommitApply` executes the journal one repository at a
  time, `RollbackApply` undoes a partial one. Only `refs/heads/pisafe/<run>` is
  ever created, with compare-and-swap; a contested ref stops with
  `ErrApplyNeedsReconciliation` rather than overwriting. Submodule refs are
  created first, so an interruption can leave commits reachable but never a
  branch whose gitlinks are not.
- A run commits as its user: `pisafe run` resolves the identity Git would use
  in the source repository and installs it in the run's own global config. A
  repository with no `user.name`/`user.email` refuses to start a run. Only
  pisafe's own baseline and final-capture commits keep the fixed `pisafe`
  author.

### Commands over the boundary

- `pisafe apply RUN [--keep-baseline|--drop-baseline]` stops an active run,
  captures it with `pisafe-guest prepare-apply` in a throwaway `--network=none`
  container mounted only on the workspace, streams each bundle back bounded and
  SHA-256 verified,
  imports into temporary refs, records the plan in the manifest, and only then
  moves refs. A recorded plan is replayed rather than redone, so apply on a run
  that already has one never touches the run. The run becomes `imported` only
  once every ref holds its recorded commit, and cannot then be resumed or
  applied again. Apply runs the controller's *current* image, and verifies the
  run's storage first (a fixed-capacity filesystem is mounted per VM boot, not
  per run).
- A run whose history starts with a baseline commit is asked about once, before
  anything is captured: import it with everything after it, or replay only the
  run's own commits onto the captured HEAD. The replay is a `git rebase --onto`
  in a throwaway worktree inside the run, published under
  `refs/pisafe/replay/<run>` and torn down with the bundle, so the run's branch,
  working tree, and baseline are the same afterwards either way. A conflict
  reports the paths, imports nothing, records no failure, and names the three
  ways forward. The Mac then proves the baseline really is absent from what
  arrived rather than trusting the run. The drop is refused when a submodule
  carried uncommitted work of its own, because every superproject commit records
  where its submodules stood.
- `pisafe diff RUN` reports commits, per-path line counts, and untracked
  leftovers for the superproject and each submodule, measured from the baseline
  commit so carried-in dirty state is not attributed to the agent. Every list is
  capped and paired with its exact total. It runs in a throwaway
  `--network=none` container mounting the workspace read-only with Git's
  optional index locks disabled, so it works on an active, stopped, or imported
  run without disturbing it, and it reports names and counts, never content —
  the controller quotes every run-authored name and subject.
- `pisafe cp RUN:PATH [DEST] [--force]` streams a tar from the same read-only
  container. The run refuses absolute, climbing, or whole-workspace requests and
  archives only regular files and directories, naming any symlink or special
  file that stops the copy. The Mac re-validates every entry, refuses anything
  outside the requested path, and writes through an `os.Root` handle, bounded at
  4096 entries, 1 GiB total, 256 MiB per file. Everything lands in a staging
  directory beside the destination and moves into place only once complete; an
  existing destination is refused before the run is asked for anything, and
  `--force` removes it rather than writing through it.
- `pisafe gc [--dry-run]` applies the seven-day window, measured from
  `imported_at`. An imported run past it is reclaimed — container, storage, VM
  stage, Mac-side SSH key, and record — through the same idempotent path discard
  uses; the `pisafe/<run>` branch is what keeps the work. Age alone never
  removes an unimported run: a `creating`, `active`, or `stopped` run past the
  window is reported with its reason and keeps the image it can still start
  from. Superseded managed images are pruned, recognizing the current recipe by
  the label each image carries rather than by resolving its ID; an unlabelled
  image is never a candidate, and an image still in use is reported rather than
  forced away. A plan that cannot be built stops collection entirely.
- `pisafe discard RUN --confirm RUN` reclaims from any state, removing the
  record with the resources. Failed `creating` records use the same path.

### Host network and Lima backend

- `internal/hostnet` gathers IPv4 prefixes from every active non-loopback Mac
  interface plus the default gateway, canonicalized, deduplicated, and collapsed
  so nftables interval sets never overlap. Discovery fails closed.
- Dedicated instance `pisafe`, Lima ≥ 2.2.0, `plain: true` (no mounts, dynamic
  forwarding, containerd, guest agent, or agent forwarding), explicitly empty
  mounts, no forwarded X11/proxy/host-resolver/Podman socket, public DNS, IPv6
  disabled. Default resources: 4 CPUs, 8 GiB, 64 GiB sparse disk.
- Pinned Fedora 44 ARM64 cloud image:

  ```text
  sha256:55c60a3b80d3616a08705afd0459e75fe9f03c54aba7a46e4002a41a72fa0d5b
  ```

- Provisioning installs rootless Podman, Git, nftables, and OpenSSH, assigns a
  65,536-ID subordinate UID/GID range, runs `podman system migrate`, and enables
  chrony; start/resume calls a narrowly privileged clock step because plain mode
  has no guest-agent time correction.
- The generated config removes the cloud image's unrestricted passwordless sudo
  and the Lima user from `wheel`, granting only exact no-argument helpers: clock
  step, firewall status, and the fixed-policy run-storage helper.
- Every VM records a root-owned SHA-256 fingerprint of the complete generated
  definition and immutable host-network set. `Manager.Start` checks it before
  clock or firewall verification, so an older or locally modified security
  definition fails closed.
- A dedicated `192.0.2.1/32` dummy address carries the broker relay; SSH remote
  forwarding is restricted to exactly `192.0.2.1:18080` via `PermitListen`.

### Firewall

- Boot-persistent `pisafe-firewall.service` owns the ruleset; `firewalld` is
  disabled. All three chains accept established/related conntrack traffic, so
  policy gates connection initiation and conntrack owns replies.
- Input defaults to drop, admitting DHCP replies, control SSH, and exactly
  `192.0.2.1:18080`. Output and forward deny new connections toward IPv4
  loopback, RFC1918, CGNAT, link-local and metadata, TEST-NET except the static
  broker exception, multicast and reserved/broadcast, and the Mac's current
  on-link networks. Root DHCP and root loopback exceptions keep VM
  infrastructure working.
- `Start` requires a non-empty host-prefix set and compares it against the
  root-owned record baked into the firewall; a network change fails closed and
  requires VM recreation.
- **There is deliberately no runtime firewall-mutation privilege for the Lima
  user.** A merely syntax-valid refresh helper would still let an escaped
  process replace the real LAN set with an unrelated valid prefix.

### SSH transport and materialization

- `lima.Transport` runs argv-style commands over Lima's control SSH, allocates a
  private VM-side run directory, and uploads `source.bundle`, `tracked.patch`,
  and `snapshot.json` independently — each streamed as binary, checked for exact
  byte count and SHA-256 in the VM, then atomically renamed. The guest snapshot
  omits the Mac source path and is rejected if it names one.
- Stage files are imported with `podman unshare` into a private fixed-capacity
  filesystem: no Mac mount, no Podman socket, no `podman cp --chown`.

### Run image and container contract

- Root image pinned to the ARM64 Node manifest:

  ```text
  docker.io/library/node@sha256:af01d58b748ec92b1d6e8e11429aad424fd1e68c848185399dca0596a1ab8f5c
  ```

- Pi pinned to `@earendil-works/pi-coding-agent@0.82.0`, with the downloaded
  tarball checked against its registry SHA-512 before installation. Pi's own
  published `npm-shrinkwrap.json` freezes the rest of the tree and travels
  inside that verified tarball, so the build refuses a Pi release that no longer
  ships one. The three packages that shrinkwrap names without an integrity hash
  — `pi-agent-core`, `pi-ai`, `pi-tui` — are re-fetched by exact version,
  checked against digests recorded beside `PiIntegrity`, and extracted over what
  npm installed, because without a hash npm resolves their `^` ranges freely.
- `runimage.Installer` derives a recipe digest from the exact embedded
  Containerfile and static guest-helper bytes, streams a two-file tar context,
  and labels the image with that digest. A mutable recipe-derived tag is only a
  cache key: reuse requires matching recipe/base/Pi labels, Linux/ARM64
  platform, and a valid immutable SHA-256 ID, which is what run containers
  receive. Artifact loading rejects symlinks, file swaps, oversize inputs,
  non-ELF or non-ARM64 helpers, dynamic interpreters, and imported shared
  libraries.
- Build-time SSH host keys are removed; a network-disabled one-shot container
  generates each run's host key and installs only its client public key.
- Run commands require an immutable `sha256:` ID and use UID/GID 1000, read-only
  root, all capabilities dropped, `no-new-privileges`, rootless pasta networking
  with explicit public DNS, 2 CPUs, 4 GiB memory with no extra swap, 512 PIDs,
  bounded `/tmp` and `/run` tmpfs, and unique workspace/home directories inside a
  run-scoped fixed-capacity filesystem owned by the mapped non-root user. No
  Podman/Docker socket, forwarded agent, or credential environment is added.
- Each run gets one sparse 10 GiB ext4 filesystem for workspace and home.
  Root-owned image storage and the fixed-policy helper prevent the rootless VM
  user from resizing or remounting it. Podman's independent `--timeout` enforces
  the remaining active budget even after the controller exits.

### Per-run SSH boundary

- `internal/runssh` creates a unique Ed25519 client key under mode-0700 run-local
  Mac state (key files 0600). Both client and host public keys are validated as
  exact 32-byte Ed25519 SSH wire values.
- The container generates its own Ed25519 host key; the Mac stores a strict
  per-run `known_hosts` and fingerprint before activation. `sshd` runs as UID
  1000 as the container's main process, listening only on container-local
  `127.0.0.1:2222`, public-key only, with password, keyboard-interactive, root,
  agent, X11, tunnel, user-RC, and TCP forwarding disabled. No port is published.
  `sshd` builds each session's environment from scratch, so its config restates
  what the container declares — `GIT_TERMINAL_PROMPT=0`, `PI_CODING_AGENT_DIR`,
  `PI_SKIP_VERSION_CHECK` — through `SetEnv`.
- The per-run OpenSSH config uses a `ProxyCommand` executing
  `podman exec --interactive <run-container> pisafe-guest proxy-ssh`, which
  relays binary stdio only to container loopback. The client private key never
  enters Lima or the run.

### Inference broker and ChatGPT login

- `internal/broker` is the Mac-side relay. Runs authenticate with a
  `pisafe-cap-<64 hex>` capability from `crypto/rand`, stored only in the
  version-4 manifest: activation and resume require a fresh one, stop clears it,
  and a manifest is invalid if an inactive state retains one. The broker
  re-reads durable records per request, so a run that stopped, was reclaimed, or
  exhausted its wall clock is rejected immediately with a uniform 401. Matching
  is constant-time over SHA-256 digests.
- The relay accepts exactly one method and path derived from the configured API
  (`POST /v1/messages`, `/v1/chat/completions`, `/v1/responses`, or
  `/codex/responses`), caps bodies at 64 MiB and each run at 4 concurrent
  upstream requests, refuses upstream redirects, and streams responses
  (SSE-safe flushing) unrewritten. Client content/beta/version/originator/
  session/user-agent headers are forwarded; credentials never are, because the
  broker sets its own.
- `pisafe broker` runs in the foreground: verifies the VM boundary, serves on an
  ephemeral `127.0.0.1` port, and publishes it at `192.0.2.1:18080` through a
  dedicated `ssh -N -R` child with `ExitOnForwardFailure` and multiplexing
  disabled, confirming reachability from inside the VM. The listener dies with
  the process and a second broker fails loudly.
- Run creation and resume exec `pisafe-guest configure-inference`, which
  atomically installs a validated mode-0600 `~/.pi/agent/models.json` whose
  `apiKey` is the current capability, and merges `transport: "sse"` into the
  run's Pi settings. Base URLs were fixed against the pinned clients:
  `http://192.0.2.1:18080` for anthropic-messages and Codex,
  `http://192.0.2.1:18080/v1` for the standard OpenAI APIs. The capability is
  accepted as `x-api-key`, a Bearer token, or the JWT-wrapped Bearer token the
  Codex client requires.
- `pisafe login chatgpt` runs the browser OAuth flow — PKCE S256 against
  `auth.openai.com` with the pinned client's exact client ID, scope, redirect
  (`localhost:1455`), and parameters — serving the callback locally with escaped
  output and a strict state check, and extracting the account ID from the
  access-token JWT. The credential persists in the login keychain (service
  `pisafe`, account `chatgpt`) written over `security`'s stdin base64-wrapped, so
  tokens never appear in argv; a missing item becomes a not-logged-in error the
  CLI turns into a login prompt.
- The broker's credential source is serialized: access tokens refresh
  proactively within five minutes of expiry and the rotated refresh token is
  persisted before use. `pisafe broker` forces one credential check at startup.
  Upstream requests to `https://chatgpt.com/backend-api/codex/responses` carry
  the real Bearer token and `chatgpt-account-id`; the run only ever sees the
  placeholder account ID `pisafe`. Its model catalog is embedded from the pinned
  Pi AI Codex data with per-model routing fields stripped.

### Run records and controller

- `internal/runstate` writes version-4, mode-0600 JSON manifests atomically
  under the user config directory (or `PISAFE_STATE_DIR`), enforcing
  `creating → active → stopped → active|imported` and binding one capability to
  exactly the active state. Activation records the baseline commit each
  repository actually got, superproject and submodules alike, and refuses a
  materialized snapshot that describes different submodules. There is no terminal state: reclaiming a run removes
  its record, and only an active run's record is refused, because it is the one
  route back to a running container. Failed creation stays visibly `creating`
  with `last_error`.
- `runctl.StartPrepared` composes stage upload, private-storage import, per-run
  key generation, network-disabled SSH init, hardened container start, host-key
  pinning, in-container materialization, transfer cleanup, activation, and
  bounded rollback. Rollback removes partial SSH state with the container,
  filesystem, and VM stage; allocation commands are treated as possibly
  successful even when their transport response fails. Activation records the
  baseline commit returned by actual materialization, not the host placeholder.
- `pisafe run` resolves the Git root, mints a project-derived run ID with a UTC
  timestamp plus 48 bits of entropy, discovers Mac networks, creates or reuses
  and verifies the VM, installs or reuses the image, starts the run, and prints
  the run, workspace, branch, exact `ssh -F` command, excluded inputs, and
  whether the broker will serve inference.
- `pisafe connect RUN [--shell]` attaches the terminal to a run: it starts Pi in
  the run's workspace, or a login shell with `--shell`, over the same per-run
  SSH config Zed uses. It replaces its own process with `ssh`, so signals,
  window resizes, and the exit status are the session's. Only an active run
  within its budget is reachable; a stopped one is refused naming
  `pisafe resume`. `zed` shares that check.
- `pisafe stop` removes only the container and accounts elapsed active seconds
  conservatively; `pisafe resume` verifies VM boundary, storage identity, image,
  container identity, and mount sources before recreating with the remaining
  budget. `pisafe zed` opens a connection the user saved once through Zed's
  "Connect New Server" flow; pisafe never edits global SSH or Zed settings.

## Tests and verification

```sh
go test -race -cover ./...
go vet ./...
go build -trimpath -buildvcs=false ./cmd/pisafe
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
  go build -trimpath -buildvcs=false ./cmd/pisafe-guest
git diff --check
```

Package coverage at this milestone:

```text
pisafe        0.0%    runcontainer  73.4%
pisafe-guest  64.8%   runcopy       80.3%
broker        96.2%   runctl        70.3%
chatgpt       70.1%   runid         92.3%
cli           43.3%   runimage      76.8%
gitstage      79.1%   runssh        68.0%
hostnet       50.0%   runstart      80.9%
lima          67.0%   runstate      75.5%
```

What the unit and integration suites cover, mostly against real repositories
with a fake VM boundary:

- **Broker**: capability auth against durable records (unknown, malformed,
  stopped, and deadline-exhausted all return the same 401), missing-provider
  503, method/path/oversize fail-closed, upstream credential injection without
  leaking the client header, SSE fidelity, redirect refusal, concurrency cap,
  and the Codex JWT wrapper and account-id replacement. **chatgpt**: the login
  exchange and state rejection against a stub endpoint, proactive refresh with
  rotated-token persistence, Keychain round-trips through a fake `security`
  runner, and the catalog's no-routing-override invariant.
- **Input selection**: a baseline commit carrying a selected untracked file, an
  ignored artifact, an executable, and a symlink, leaving the workspace clean
  and the source untouched; refusal of credential-shaped, tracked, missing,
  outside, escaping, special, and whole-repository selections; size limits;
  archive entries that climb, are absolute, name `.git`, or are devices; a
  snapshot/archive mismatch; and the whole-word secret matcher.
- **Submodules and journaled apply**: reconstruction with dirty state in both
  repositories, an uninitialized submodule left alone, refusal of nested
  submodules and LFS, mismatched artifacts, path-escape refusal, gitlink
  resolving to exactly the imported commit, an unchanged submodule getting no
  branch, journal replay as a no-op, a contested ref stopping for
  reconciliation, and rollback removing only what apply created. End to end: a
  stopped run captured, imported, and marked imported while the checkout stays
  untouched; a second apply refused; a recorded plan finished without touching
  the run; a contested ref leaving the run stopped with its plan intact.
- **Diff**: commits, an uncommitted edit, an untracked file, a deletion, and a
  binary path each reported correctly; carried-in baseline work excluded; a
  submodule reporting its own work without the superproject repeating it; caps
  with exact totals; escaping submodule path refused; the guest command refusing
  a Mac-path snapshot and leaving the workspace byte-identical; the controller
  refusing a run with no workspace, verifying storage first, and leaving an
  active run active; read-only mount arguments; and output quoting every
  run-authored name.
- **cp**: a directory round-tripping with its executable bit and nothing from
  outside the request; a single file to a named destination; a symlink stopping
  the copy run-side; Mac-side refusal of climbing, absolute, outside-request,
  symlink, hard-link, device, named-pipe entries and lookalike prefixes; an
  occupied destination refused before the run starts and left intact; `--force`
  replacing it; a destination symlink replaced rather than followed; an
  oversized file and a failed run each leaving nothing behind.
- **Collection**: an imported run inside the window untouched; the same run past
  it reclaimed with container, storage, stage, SSH key, and record all gone,
  after which every command refuses it as unknown; an unimported run only
  reported whatever its age, keeping its image and seeing no backend call; a
  discarded run leaving a later sweep nothing. The store's own test covers
  removal directly: a reclaimed record is deleted, disappears from listings,
  cannot be deleted twice, and an active run's record is refused. Image pruning
  covers the label filter, Podman's bare hex ID, an unlabelled image surviving,
  the current recipe recognized without a lookup, a missing recipe digest
  refused, and an in-use image reported without stopping the sweep.
- **Baseline choice**: a clean drop importing only the run's own commits with
  the carried-in edit absent; a conflict naming the path, leaving the run's
  branch, HEAD, and working tree untouched, after which keeping the baseline
  succeeds; refusal of a run with no baseline and of one whose submodule carries
  its own; an import that asked for the drop refused because the history still
  contained the baseline; choice parsing on both sides of the boundary; and,
  through the controller, the choice reaching the run's argv and a conflict
  leaving the record stopped, unmarked, and still applicable. The CLI's own
  tests cover the question being asked exactly where an answer still changes
  the outcome: a repeated prompt after an unusable answer, an EOF naming the
  flags, no question at all when there is nothing to decide, and each refusal.
  The store's test covers activation filling submodule baselines in and
  refusing a snapshot that renames or moves one.
- **connect**: the exact SSH argument vector for Pi and for a shell, with a
  workspace path that needs quoting for the remote shell; option parsing;
  and every inactive run refused before anything launches, whether asked
  through `connect` or `zed`, with a stopped one told to resume.
- **Git identity**: repository config preferred over global, an unconfigured Mac
  refused, an installed identity proven by reading back a commit's author,
  empty/oversized/newline values refused, unknown guest fields refused, and run
  creation stopping before boundary and image work.
- **Run image pins**: the Containerfile is checked to contain every recorded
  pin, to carry no floating tag, to assert Pi still ships a shrinkwrap, and to
  repin each shrinkwrap gap at exactly `PiVersion` with a SHA-512 digest — so a
  Pi bump that forgets the three sibling packages fails before any image builds.
- The generated Lima YAML is checked by the installed Lima validator in the
  normal suite.

### Live verification

```sh
PISAFE_LIVE_LIMA=1 go test -v ./internal/lima
PISAFE_LIVE_LIMA=1 go test -v ./internal/runimage
PISAFE_LIVE_LIMA=1 PISAFE_LIVE_RUN_IMAGE=sha256:ae321357e51ae824bd020941565fb28ffca4bf9856ae3eb450b023a162b55970 \
  go test -v -run TestLiveSSHStageAndContainerMaterialize ./internal/lima
```

Verified against a real ARM64 VM:

- **Boundary**: Lima reaches READY, `/Users` is absent, no Podman socket is
  forwarded, the user namespace has the expected direct UID plus 65,536-ID
  subordinate mapping, the firewall service and table are active, IPv6 is
  disabled. Public HTTPS works from a rootless container; RFC1918 and metadata
  TCP destinations fail. Unrestricted `sudo -n true` is denied while the clock,
  firewall-status, and storage helpers work through their narrow rules; the
  root-owned security-profile record is mode 0444 and matches the generated
  definition.
- **Frozen npm resolution**: the drift was reproduced in the real ARM64 image
  before it was fixed — `pi-coding-agent` 0.82.0 installed `pi-agent-core`,
  `pi-ai`, and `pi-tui` at 0.82.1, the versions its own shrinkwrap pinned to
  0.82.0. After the repinning step the built image reports 0.82.0 for all four
  and `pi --version` agrees. Both new guards were shown to fail closed in the
  built image: a wrong sibling digest and a package carrying no shrinkwrap each
  exit non-zero, while the recorded digest matches. Tampering established why a
  lockfile was not used: a corrupted nested `integrity` in a pisafe-authored
  `package-lock.json` installs cleanly under `npm ci` and an `overrides` entry
  is ignored, while a corrupted top-level integrity raises `EINTEGRITY`. A real
  run started from that image — not an inspection container — reports 0.82.0 for
  `pi --version` and for all three siblings.
- **Image and container**: the tracked Containerfile builds on ARM64; the
  installer built it from its two-file tar stream, validated labels, platform,
  and immutable ID, and reused it on the next call. A runtime check confirmed
  UID 1000, zero effective/bounding capabilities, `NoNewPrivs=1`, read-only
  root, 4 GiB memory, 512 PIDs, Pi 0.82.0, and public HTTPS. The storage helper
  created an exact mapped-UID 10 GiB ext4 filesystem, accepted an in-limit and
  rejected an over-limit allocation, and removed both mount and backing image. A
  container with a two-second `--timeout` was killed independently and left
  exited for reconciliation.
- **Staging and SSH**: dirty tracked state crossed Lima SSH into private
  quota-backed storage, materialized inside the hardened container, and was
  deleted there without altering the Mac checkout. Fresh client and host keys
  were generated and pinned, the generated ProxyCommand connected, and a file
  written over the Zed-compatible session was seen by a Pi executable in the
  same container. The session ran as UID 1000 with no forwarded agent or
  `/Users`, and `podman port` reported no published port.
- **Lifecycle**: the real CLI completed `run → stop → resume → SSH → discard`
  with isolated Mac state — the manifest carried the real dirty baseline commit,
  untracked files were reported as excluded, stop preserved the workspace,
  resume reused the pinned host identity with the reduced budget, and discard
  removed container, filesystem, remote stage, and client key.
- **apply**: a scratch repository with an initialized submodule, one dirty
  tracked file, and one untracked file became a run in which commits were made
  in both repositories, one tracked change was left uncommitted, and one
  untracked file was left behind. Applying it produced a superproject history
  with baseline, run commit, and final capture; a gitlink pointing at exactly
  the submodule commit made in the run, held by the submodule's own
  `pisafe/<run>`; the untracked file reported and absent; an unchanged source
  checkout (same HEAD, branch, dirty file, submodule); a second apply refused;
  and discard reclaiming everything. Two runs predating `prepare-apply` also
  applied cleanly with the current image. The repinned image drove its own pass:
  a run carrying a dirty tracked file and an untracked leftover made one commit
  and left one tracked change uncommitted, `diff` reported the agent's line
  without the one carried in, and `--keep-baseline` imported baseline, agent
  commit, and final capture — the agent's commit authored by the repository
  identity and pisafe's own by `pisafe` — leaving the source checkout on `main`
  still dirty and refusing a second apply.
- **diff**: on a still-active run, one commit since baseline, `+2/-0` for an
  edited file, `binary` for a binary one, and the untracked leftover, without
  reporting the dirty line the user carried in. The run's `git status` and HEAD
  were identical afterwards, and the same run reported identically while stopped
  and while imported.
- **cp**: a `dist` directory with a nested executable and a 2 MiB binary arrived
  intact with nothing from outside the request; a log file copied to a named
  destination; an occupied destination was refused naming `--force` before the
  run was asked for anything, then replaced with it; absolute, climbing, and
  whole-workspace requests were refused Mac-side; a directory holding a symlink
  to `/etc/passwd` was refused naming `"linked/escape"`. The run stayed active
  and unchanged.
- **Baseline choice**, on three scratch repositories with isolated Mac state:
  a run carrying an uncommitted edit answered `drop` at the prompt (after one
  unusable answer) imported `initial → agent commit → final capture` with the
  carried-in edit absent from the branch and the agent's uncommitted work
  present, while the run's own history kept its baseline and no replay ref
  survived, and the source checkout stayed on `main` still dirty. A second run
  whose commit built on the carried-in line was refused by name — `"shared.txt"`
  — with no import branch created, the record left `stopped` and unmarked, and
  `--keep-baseline` then importing the run's history unchanged at exactly the
  tip the run already had. A third run with an initialized submodule dirty in
  both repositories recorded both baselines in its manifest, reported no agent
  work in either repository, refused `--drop-baseline` before touching the VM,
  and printed the note instead of a prompt on the plain apply.
- **connect**: `pisafe connect --shell` landed in `/work/connect-demo` as UID
  1000, on the run's own branch, with `pi` resolving to `/usr/local/bin/pi` and
  `pi --version` reporting 0.82.0; `pisafe connect` with no flag ran `pi`
  itself. The session carried `GIT_TERMINAL_PROMPT=0`,
  `PI_CODING_AGENT_DIR=/home/node/.pi/agent`, `PI_SKIP_VERSION_CHECK=1`,
  `HOME=/home/node`, and `SHELL=/bin/bash`. That `SetEnv` is what supplies them
  was shown directly: the same container reports `NODE_VERSION=24.18.0` while
  the SSH session sees it unset. Stopping the run made `connect` refuse it
  naming `pisafe resume`, and after resume the same session worked in the
  recreated container.
- **Git identity**: a repository whose local identity differed from the global
  one produced a run whose `~/.gitconfig` held the local one; an agent commit
  over SSH with no hand configuration was authored by it while pisafe's own
  commits stayed authored by `pisafe`, and the imported branch carried that
  split. A repository with no identity refused before any boundary work.
- **gc**, by aging the exact timestamps the policy reads in an isolated state
  directory: a freshly imported run was left alone while superseded images were
  still proposed; with `imported_at` moved back eight days, `--dry-run` named the
  run and changed nothing, and the sweep then reclaimed it. The manifest file
  itself was gone and `pisafe list` reported no runs, the Mac-side key directory
  was empty, no mount or loop device survived, and the root-owned storage
  helper's `verify` exited non-zero for that run while `remove` stayed
  idempotent. The `pisafe/<run>` branch still held every commit, and `diff`,
  `cp`, `apply`, and `resume` each refused the run as nonexistent before touching
  the VM. The image sweep pruned exactly the superseded managed images and kept
  the current one, recognized by its label. An earlier sweep reported a stopped
  run aged thirty days without touching it, and resumed it cleanly.
- **Broker relay**: the full gated suites pass including
  `TestLiveBrokerReverseRelay` and `TestLiveSecondRelayFailsClosed`. With a
  loopback stub upstream, requests from inside a run traversed pasta, the
  firewall, the reverse forward, and capability auth — plain JSON and streamed
  SSE both intact. A wrong capability got 401, a non-canonical path 404, a
  capability replayed after `pisafe stop` 401, and `pisafe resume` rotated it so
  the new one worked in the recreated container.
- **ChatGPT subscription**, against the real provider: login stored the
  credential in the Keychain, `pisafe broker` passed its startup check and
  reported the Codex upstream, and a run drove real Pi conversations through the
  relay. Inspection confirmed `settings.json` carrying `"transport": "sse"`
  merged with Pi's own session state; `models.json` exposing only the curated
  catalog under provider `pisafe` with no per-model routing override; the
  unsigned-JWT `apiKey` accepted by Pi's Codex client; four assistant turns
  served by provider `pisafe` across two catalog models; and **no provider
  credential in the run** — `auth.json` is `{}`, no OpenAI/ChatGPT environment
  variable exists, and the real token and account ID appear only on the broker's
  upstream request.

## Live VM state

A persistent Lima instance named `pisafe` was left running with security profile
`sha256:35c2cd370359201ce6861c91bc7fb25d8ada1497cf1db2d29c0017eea7e1f459`,
holding cached base/test images plus the current managed run image:

```text
recipe digest: sha256:26643d0cbfdca35379ef38e7e05f8381a26812ab946f113f8525c3c328a6e7af
image ID:      sha256:ae321357e51ae824bd020941565fb28ffca4bf9856ae3eb450b023a162b55970
```

Each time the recipe moves, the next run rebuilds and every later run reuses it,
which live-checks content-addressed reuse. When a recorded digest and a rebuild
disagree, the rebuild is right: the recipe derives from the exact Containerfile
and guest-helper bytes.

The VM once entered `VirtualMachineStateError` unattended overnight, almost
certainly on host sleep, while `limactl list` still reported `Running` because
the host agent survived. `limactl stop --force` then `limactl start` recovered
it with the disk and both runs intact.

**When Codex runs inside a restricted filesystem sandbox it may be unable to
connect to `~/.lima/pisafe/ha.sock`, and `limactl list` then falsely labels the
instance `Broken`. Run the diagnostic with the needed host permission before
acting on that status; never delete or recreate the VM based on the sandboxed
result.**

### Live-only issues found and fixed

These explain why parts of the code look the way they do:

1. Overlapping `/24` and gateway `/32` nftables interval elements.
2. A readiness probe that enumerated nftables without `sudo`.
3. Missing rootless Podman subordinate IDs, and Podman's existing pause
   namespace needing `podman system migrate` afterwards.
4. Pasta's default link-local DNS forwarder being denied by the firewall; run
   and build commands now specify public DNS explicitly.
5. Plain-mode VM time drifting ~6.5 hours after host sleep; start/resume now
   invokes the restricted chrony step helper.
6. Fedora cloud-init granting the Lima user unrestricted passwordless sudo,
   which would let a VM-user escape remove the firewall.
7. Fedora Podman not supporting `podman cp --chown`; stage transfer uses
   `podman unshare` extraction into quota-backed storage.
8. Fedora Podman reporting image `Id` as bare 64-char hex while run specs
   require `sha256:` form; the installer normalizes it.
9. Fedora Podman rejecting `uid=`/`gid=` in `--tmpfs`; `/run` uses
   `type=tmpfs,...,U=true`.
10. A VM-loopback published SSH port could not cross the firewall, which denies
    loopback to the unprivileged Lima user; SSH uses the portless `podman exec`
    stdio relay instead of a mutable exception.
11. Podman's local-volume `size` requiring root and XFS while the pinned Fedora
    VM uses Btrfs, and Btrfs qgroups being bypassable with nested subvolumes;
    hence fixed-size ext4 images through the fixed-policy helper.
12. Container UID 1000 mapping to host 100999; the storage helper derives IDs
    from `/etc/subuid` and `/etc/subgid`.
13. Lima 2.2.0 repeatedly booting a stopped VZ VM without restoring SSH, with
    `vzNAT` failing identically. Fresh creation is reliable; stopped-VM restart
    remains an upstream/platform gap.
14. The `gc` dry run proposing to prune the current image, because it was found
    by resolving the recipe's tag and that lookup returned "nothing installed"
    for every kind of failure. Pruning now reads the label each image carries;
    the tag-resolving helper was deleted rather than repaired.
15. Broker connections timing out because sshd's SYN-ACK replies carry the
    client's ephemeral port and matched no accept rule in the then-stateless
    output chain; all chains now accept established/related.

## Known gaps

- `pisafe connect` hands over the terminal and reports nothing afterwards, so a
  session that ends because the run hit its wall-clock limit looks the same as
  one the user quit.
- A run whose submodule carried uncommitted work of its own can only import its
  baseline, never replay without it: separating the two histories would mean
  rewriting the run's commits to follow the submodule's new commit IDs.
- The replay checks out the run's tree a second time inside run storage for the
  duration of the rebase, so a repository large enough to fill the run's 10 GiB
  filesystem fails the replay. It fails safely — nothing is imported and the run
  is untouched — but the message is a disk error rather than an explanation.
- Runs created before activation recorded submodule baselines have none in their
  manifest, so `diff` measures their submodules from the staged head and the
  drop would be offered where it should not be. No such run exists on this host.
- `gc` reclaims runs and images, but the per-project caches and session stores
  the design also asks it to sweep do not exist until Phase 2.
- Collection never reclaims an unimported run, even one a check could prove
  holds no commits, because `diff` sees the repository but not the run's home
  directory. Their 10 GiB filesystems are reclaimed on the user's schedule, by
  explicit discard.
- A run leaves no record once reclaimed, so `pisafe list` shows only runs that
  still own something. Which runs existed and when they were imported is
  answerable from the `pisafe/<run>` branches and the reflog, not from pisafe.
- Retention is measured against the Mac's clock with no grace for a manifest
  whose timestamps are in the future; such a record is never collected.
- `pisafe cp` refuses any symlink rather than recreating one that stays inside
  the copied tree, so a directory holding a single link cannot be copied whole.
- `pisafe diff` reports paths and line counts, never content, so reviewing what
  a run wrote means importing it or taking single files out with `cp`.
- `pisafe zed` launches a connection saved once in Zed. Fully automatic first
  launch is intentionally absent because Zed's CLI URL cannot carry `-F`.
- Apply uses the controller's current run image, which must still exist in the
  VM; a pruned image fails apply as it fails resume.
- A run's apply response is bounded at 1 MiB, so a run leaving roughly twenty
  thousand untracked files fails apply instead of reporting them.
- Broker-side token refresh has only ever run against a stub: the live session
  never crossed an access-token expiry. The first long-lived broker exercises it.
- The OAuth flow and embedded catalog mirror the pinned Pi AI 0.82.0 client;
  both must be re-checked whenever the pin moves, and the subscription backend
  can change underneath them (inference then fails closed and loudly).
- While no broker is connected, a process escaped to the unprivileged VM user
  could bind `192.0.2.1:18080` itself. It gains nothing beyond what that user
  already has, and a real broker then fails loudly at bind time.
- Firewall behavioral coverage still needs DNS-to-private answers, redirects,
  raw UDP, `host.containers.internal`, and VM loopback attempts.
- Security-profile drift fails closed, but automated replacement is
  intentionally absent because deleting a VM is destructive and must be an
  explicit lifecycle operation.
- Stopped-VM recovery previously failed on this host over both default user-mode
  networking and `vzNAT`. A force-stop and start after the crash above did regain
  SSH over a vsock forwarder with runs intact; one success is not yet a reliable
  recovery path.
- A crashed VM leaves `limactl list` reporting `Running` while the VZ machine is
  in `error`, visible only in `ha.stderr.log`. Nothing in pisafe detects this, so
  the first command to touch the VM fails with an opaque SSH reset.
- npm resolution inside the run image is frozen, but the `apt-get` layer is not:
  the Debian packages it installs float, so two builds of one recipe digest are
  equivalent in Pi and its dependencies without being byte-identical images.
- The three sibling digests are pinned by hand and must be refreshed whenever
  `PiVersion` moves. A unit test refuses a mismatch, so the failure is loud, but
  regenerating them means packing each tarball and recording its SHA-512.

## Next implementation slice

Every Phase 1 command is implemented and live-verified, and nothing the design
names is now unbuilt.

One verification debt remains: the firewall behaviours listed under Known gaps —
DNS answers pointing into private ranges, redirects, raw UDP,
`host.containers.internal`, and VM loopback attempts. Unlike the debts already
paid, these need test code rather than a re-run, because no live test exercises
what the ruleset does with traffic that is shaped to look permitted.

Do not weaken the boundary for any of it.

## Useful references

- Lima plain mode: <https://lima-vm.io/docs/config/plain/>
- Lima SSH: <https://lima-vm.io/docs/usage/ssh/>
- Lima Podman example: <https://lima-vm.io/docs/examples/containers/podman/>
- Podman rootless networking:
  <https://docs.podman.io/en/stable/markdown/podman-network.1.html>
