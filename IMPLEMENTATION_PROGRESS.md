# Implementation progress

Last updated: 2026-08-01

The durable handoff for continuing `pisafe` from a fresh session: what is
implemented, what has been verified against a real VM, and what comes next.
[`pisafe-design.md`](pisafe-design.md) is the authority on what must hold, and
[`DECISIONS.md`](DECISIONS.md) records why the implementation looks as it does.

## Current milestone

Phase 1 and Phase 2 are both complete. Every command the design enumerates
exists.

Phase 1 — `run`, `list`, `connect`, `stop`, `resume`, `diff`, `cp`, `apply`,
`discard`, `gc`, `zed`, `login`, `broker`, `doctor` — was built in twelve
slices: the controller and Git isolation core; the Lima VM backend and firewall;
SSH transport, run records, run image and container contract; per-run SSH
credentials; user-facing run creation; stop/resume/discard and quota-backed
storage; the reverse inference relay; ChatGPT subscription login; getting work
back out (`diff`, `cp`, `gc`); terminal access without an editor (`connect`);
the keep-or-replay choice about a run's carried-in baseline commit; and freezing
the run image's transitive npm resolution. A thirteenth closed the last
verification debt, testing the packet filter against traffic shaped to look
permitted.

Phase 2 — managed persistence — added `project`, `profile`, `extension`,
`tool`, `backup`, and `restore`, and was built in sixteen slices: substrate
scouting against the live VM; per-project storage and the dependency cache;
sessions on the same mechanism; declared caches and snapshot restore;
publishing and eviction; session promotion; the global profile mount;
`extension install`; `extension update` and the offer made at a run's end; the
toolchain the run image carries; `tool install`; the `gc` sweep of project
stores; one relay serving several upstreams; `login` for API-key providers;
naming, emptying, and moving a durable scope; and backup and recovery.

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
  special files, nested repositories, and over-limit selections;
  credential-shaped names need `--include-unsafe`. They travel as a tar beside
  the bundle, are re-validated on extraction against the names in the staged
  snapshot, and join the baseline commit.
- What a run leaves behind is listed once per run, with a directory nobody
  tracks standing for what is under it, and a request naming such a directory is
  expanded from the filesystem so the credential check and the per-file limits
  still see one file at a time. Selection resolves against that listing and its
  result is what the stage archives, so the two lists a run prints are decided
  together. In a repository with 21 127 ignored files this replaced three walks
  of all of them with one listing of 28 names, 0.4 s of run start with 0.05 s.
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
  ever created — a journal derives it and its incoming ref from the run ID
  rather than storing either — with compare-and-swap; a contested ref stops with
  `ErrApplyNeedsReconciliation` rather than overwriting. Submodule refs are
  created first, so an interruption can leave commits reachable but never a
  branch whose gitlinks are not.
- A run commits as its user: `pisafe run` resolves the identity Git would use
  in the source repository and installs it in the run's own global config. A
  repository with no `user.name`/`user.email` refuses to start a run. Only
  pisafe's own baseline and final-capture commits keep the fixed `pisafe`
  author.

### Commands over the boundary

- Every command that takes a run accepts none, and then resolves to the one run
  of the current checkout that is not `imported`, matched on the project key the
  record already carries. No live run says so and points at `pisafe run`; more
  than one lists them with their states and stops. `discard` is deliberately
  outside this: its confirmation argument is the command. Nothing inside a run
  takes part in the choice.
- `pisafe apply [RUN] [--keep-baseline|--drop-baseline]` stops an active run,
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
- `pisafe diff [RUN]` reports commits, per-path line counts, and untracked
  leftovers for the superproject and each submodule, measured from the baseline
  commit so carried-in dirty state is not attributed to the agent. Every list is
  capped and paired with its exact total. It runs in a throwaway
  `--network=none` container mounting the workspace read-only with Git's
  optional index locks disabled, so it works on an active, stopped, or imported
  run without disturbing it, and it reports names and counts, never content —
  the controller quotes every run-authored name and subject.
- `pisafe cp [RUN:]PATH [DEST] [--force]` streams a tar from the same read-only
  container. The run refuses absolute, climbing, or whole-workspace requests and
  archives only regular files and directories, naming any symlink or special
  file that stops the copy. The Mac re-validates every entry, refuses anything
  outside the requested path, and writes through an `os.Root` handle, bounded at
  4096 entries, 1 GiB total, 256 MiB per file. Everything lands in a staging
  directory beside the destination and moves into place only once complete. A
  DEST that is already a directory receives the copy under the copied path's own
  name; any other existing destination is refused before the run is asked for
  anything, and `--force` removes it rather than writing through it.
- `pisafe cp PATH [RUN]: [--force] [--unsafe]` is the same command inward. The
  colon marks the end that is in the run, so which side carries it is the whole
  of what says which way a copy goes, and the name before it is optional exactly
  as it is everywhere else. The archive is produced on the Mac, so what arrives
  is held to the limits the Mac would itself have accepted, and the run is given
  no say in what it receives. It unpacks in a throwaway container like every
  other inspection, but with the workspace mounted read-write — the one that
  is — still with no network and nothing else the others are denied.
  `runcopy`'s staging-and-rename lands the copy, so a run watching it never
  sees a half-written one, and an occupied name is refused without `--force`.
  A credential-shaped name needs `--unsafe`, on the reasoning that made
  `--include-unsafe` explicit: this is the frictionless path that would
  otherwise void what a run guarantees. Because inspections use the current
  managed image rather than the one a run was created from, copying in reaches
  runs that predate the command — unlike `forward`, whose files are written
  into the run's home once, at creation.
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
  interface plus the default gateway, and reports them as observed. Discovery
  fails closed. `lima.CanonicalIPv4Prefixes` is the one place they are masked,
  deduplicated, ordered, and collapsed so nftables interval sets never overlap:
  the VM definition, its digest, and the check against a running VM all read the
  set from there, so an uncollapsed Mac and a collapsed one are never a drift.
- Dedicated instance `pisafe`, Lima ≥ 2.2.0, `plain: true` (no mounts, dynamic
  forwarding, containerd, guest agent, or agent forwarding), explicitly empty
  mounts, no forwarded X11/proxy/host-resolver/Podman socket, public DNS, IPv6
  disabled. Fixed resources: 4 CPUs, 8 GiB, 64 GiB sparse disk. How large the
  VM is bounds nothing a run may do, so it is outside the security profile and
  changing it never asks for a new instance.
- A second 64 GiB sparse Lima disk, `pisafe-state`, carries `/var/lib/pisafe`.
  It belongs to Lima rather than to the instance, so deleting the instance —
  what every failed boundary check asks for — leaves every run's filesystem,
  every project store, and the profile for the next instance to mount back.
  Provisioning finds it by filesystem label and formats it only when no device
  carries one, choosing among whole devices that hold neither a partition table
  nor any filesystem signature and refusing the boot unless there is exactly
  one. `pisafe-storage` and the Lima boundary probe both require
  `/var/lib/pisafe` to be a mountpoint, so a boot that failed to mount it never
  reaches the instance's own disk.
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
  definition fails closed. Both it and the firewall check name `pisafe vm
  rebuild`, which is the cure they prescribe.
- `pisafe vm rebuild` reports what a rebuild costs and changes nothing until
  `--confirm`. Confirmed, it stops every active run — charging each from its own
  container's account and publishing what it produced — then deletes the
  instance, recreates it from the current definition, verifies the boundary, and
  builds the run image. `Manager.Delete` shuts the VM down before removing it so
  the state disk's ext4 is flushed, kills one that will not shut down, and
  releases the lock Lima leaves on the disk when it does. Nothing here can
  refuse the rebuild: a run it could not stop is reported and settled later,
  charged nothing.
- An instance provisioned before the state disk existed keeps `/var/lib/pisafe`
  on the disk being deleted. `Manager.HasStateDisk` detects it, the plan says so,
  and the rebuild is refused until `--discard-state`, which also discards the
  run records that would otherwise name storage that is gone.
- Any state Lima calls neither running nor stopped is `StatusBroken`: the
  instance exists and can be replaced, but nothing may be concluded about what
  runs inside it. Starting one is refused, naming the rebuild, and `pisafe list`
  treats it as a VM it could not ask rather than as one holding no container.
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
  requires VM recreation. The record is parsed as IPv4 prefixes before it is
  compared, so a line the VM holds that is not one fails the check rather than
  being read past.
- **There is deliberately no runtime firewall-mutation privilege for the Lima
  user.** A merely syntax-valid refresh helper would still let an escaped
  process replace the real LAN set with an unrelated valid prefix.

### SSH transport and materialization

- `lima.VM` is the one handle on the instance: it creates, starts, verifies, and
  deletes it, and runs argv-style commands inside it over Lima's control SSH. It
  allocates a private VM-side run directory and uploads `source.bundle`,
  `tracked.patch`, and `snapshot.json` independently — each streamed as binary,
  checked for exact byte count and SHA-256 in the VM, then atomically renamed.
  The guest snapshot omits the Mac source path and is rejected if it names one.
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
  libraries. It also rejects a helper that does not carry
  `internal/guestcall.Contract`, the compile-time text naming every call and its
  arguments: the helper prints it as its usage error, so it is in the binary,
  and the controller looks for the exact bytes it was built with. The
  controller and the helper are separate binaries, so rebuilding one and not the
  other used to surface as a usage error from the guest halfway through creating
  a run; it now fails before the run exists, naming what to rebuild. The digest
  reaches its label through a build argument declared after the toolchain layers
  rather than before them: an argument in scope joins the cache key of every
  later instruction, so declaring it first made a one-byte helper change refetch
  Debian, npm, and Python. Measured on the run image, a helper-only rebuild went
  from 31.7 s to 0.5 s.
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
- A run is active only while its container runs. Rebooting or recreating the VM
  keeps every run's storage and none of its containers, so `stop` and `resume`
  settle a record that still claims one: `resume` adopts the run instead of
  refusing, and the stretch costs the run nothing, because the container carried
  the only account of how much of it was spent. A container that exited at its
  own deadline goes the same way but is charged what it recorded. A container
  still running keeps the refusal.

### Per-run SSH boundary

- `internal/runssh` creates a unique Ed25519 client key under mode-0700 run-local
  Mac state (key files 0600). Both client and host public keys are validated as
  exact 32-byte Ed25519 SSH wire values.
- The container generates its own Ed25519 host key; the Mac stores a strict
  per-run `known_hosts` and fingerprint before activation. `sshd` runs as UID
  1000 as the container's main process, listening only on container-local
  `127.0.0.1:2222`, public-key only, with password, keyboard-interactive, root,
  agent, X11, tunnel, user-RC, and remote forwarding disabled. Local forwarding
  is allowed and bounded by `PermitOpen 127.0.0.1:*`, which is what carries
  `pisafe forward`; the key's own options no longer refuse it. No port is
  published.
  `sshd` builds each session's environment from scratch, so its config restates
  what the container declares — `GIT_TERMINAL_PROMPT=0`, `PI_CODING_AGENT_DIR`,
  `PI_SKIP_VERSION_CHECK` — through `SetEnv`.
- The per-run OpenSSH config uses a `ProxyCommand` executing
  `podman exec --interactive <run-container> pisafe-guest proxy-ssh`, which
  relays binary stdio only to container loopback. The client private key never
  enters Lima or the run.
- `internal/zedsettings` splices Zed's saved connections in place, so the
  comments and layout of a settings file it edits survive: it walks JSON with
  comments to the byte range of the one value it changes and rewrites nothing
  else. `pisafe zed` adds the run's host and the `-F` path that reaches it;
  `discard` and `gc` take it back out under the same alias, derived from the run
  ID because a collected run has no record left to read it from. A host already
  saved is never rewritten, so whatever Zed keeps in that entry is left alone.

### Inference broker and provider logins

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
- Run creation and resume exec `pisafe-guest configure-models`, which atomically
  installs a validated mode-0600 `~/.pi/agent/models.json` whose `apiKey` is the
  current capability, and merges `transport: "sse"` into the run's Pi settings.
  It also names the model the run opens on — `gpt-5.6-sol` at `high` effort,
  from the first configured upstream offering it — as `defaultProvider`,
  `defaultModel`, and `defaultThinkingLevel`, filling in only what the run has
  not answered so a model chosen inside a run survives resume. Pi's own
  per-provider defaults are keyed by its provider names, which are not pisafe's,
  so without this a subscription run opened on whatever its catalog listed
  first. The document is `internal/piagent.Configuration`, shared by the
  controller and the guest; a run image predating it fails the unknown verb
  loudly rather than writing a `models.json` Pi cannot read. Base URLs were
  fixed against the pinned clients:
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
- Every configured upstream is served at once. `internal/providers` assembles
  one catalog from the ChatGPT subscription login and every API-key record, and
  the broker gives each its own route: the provider's name leads the API path
  the client would have sent, matched exactly. One run capability authorizes all
  of them, `models.json` carries one entry per provider, and a run chooses
  between them in Pi's own model list. A login the broker cannot use is named at
  startup and the others are still served; a request whose credential cannot be
  produced is refused per request, as before.
- `pisafe login anthropic|openai` reads the key from stdin, never argv, and
  stores it in the login keychain under service `pisafe` and the provider's own
  name. The record under `providers/<name>.json` holds no credential — it names
  what is stored, and the endpoint, wire format, and model list come from
  pisafe's own table, so a hand-edited record cannot redirect a stored key.
  `pisafe login NAME --url URL --api API --models FILE` adds any endpoint
  speaking `openai-completions`, `openai-responses`, or `anthropic-messages`,
  with its models declared as a JSON array of Pi model definitions whose
  per-model routing fields are stripped. Plain HTTP is refused except on
  loopback, and a URL already ending in the segment the relay appends is
  refused. `pisafe login` with no argument lists what is configured, and
  `pisafe logout NAME` removes one whether or not it still works. A run is never
  configured with a provider whose API has no canonical path, so an upstream
  pisafe cannot route reaches no run rather than being relayed by guess.
- A stored secret is read only when the broker relays a request, whichever kind
  of login it is. Starting a run renders `models.json`, which carries no
  upstream credential, and assembling the catalog asks the keychain only whether
  an item exists, so run creation touches no secret at all. A subscription
  credential that no longer parses is therefore reported by `pisafe broker`,
  which forces every login once at startup, rather than by refusing a run.

### Per-project storage: caches and sessions

- One root-owned fixed-capacity ext4 filesystem per project, allocated and
  mounted by the same narrow helper run storage uses, which became
  `pisafe-storage <action> <scope> <id>` with `run`, `project`, and `global`
  scopes. It holds two namespaces, `cache/` and `sessions/`. The project key is
  the checkout's directory slug plus eight hex characters of the SHA-256 of its
  Git root path; a mode-0600 Mac-side record under `projects/<key>.json` names
  the checkout and is written before the filesystem is allocated.
- Shared state reaches a run as an overlay — `-v <lower>:<dst>:O,upperdir=…,
  workdir=…` — with every upper in the run's own filesystem, under a third
  subdirectory that is not bind-mounted into the container. The helper allocates
  only namespace roots; pisafe builds each upper and work pair under
  `podman unshare` as the mapped UID, so no namespace of a run's own needs a
  privileged action.
- `.config/pisafe.json` at the repository root declares cache namespaces, each
  with the environment variables that should point at it and the files its key
  is computed from. Unknown fields are refused, a variable pisafe sets itself is
  refused, key files are opened through an `os.Root` after a lexical check, a
  missing key file hashes as absent, and the run image ID is mixed into every
  key. A repository declaring nothing gets no `/cache` mount and no redirection.
- A run mounts, per namespace, the snapshot whose key matches exactly, else the
  newest in the namespace. What it resolved to is recorded in its manifest
  (version 6) and remounted verbatim on resume, so an existing upper is never
  stacked on a lower its whiteouts were not recorded against.
- Publishing happens when a run stops, whatever its outcome: a throwaway
  `--network=none` container mounts the run's own overlay and streams the merged
  view out as a tar, which the VM-side script extracts under `podman unshare`
  into a dot-entry staging directory inside the namespace, stamps, and renames
  into place. A key that already has a generation, or an empty upper, publishes
  nothing. Eviction keeps the newest generation per namespace and spares any
  generation a recorded run may still mount.
- Sessions ride the same overlay mechanism at `/sessions`, named by
  `PI_CODING_AGENT_SESSION_DIR`, and are written back differently: promotion
  adds the run's finished transcripts to the project store by rename from a
  staging dot entry on the same filesystem, skipping any name the store already
  holds and any whiteout the run left. Nothing is ever evicted and
  `project reset` leaves the store alone.
- Publishing and promotion are joined rather than sequenced, and a failure in
  either is recorded on the run instead of failing a stop that worked:
  `pisafe stop` prints the warning and `pisafe list` marks the run.
- `pisafe gc` sweeps project stores as well as runs. A checkout that the
  filesystem denies starts the same seven-day window an imported run gets, with
  the stamp on the record; presence is rechecked every sweep, and a project any
  run record still names is skipped entirely.

### The global profile: extensions and tools

- The helper's `global` scope holds one profile, `default`, with three
  namespaces: `extensions/`, `tools/`, and `pins/`. Every run mounts the first
  read-only at `/opt/pisafe/profile` and the second at `/opt/pisafe/tools`,
  whose `bin/` directory of relative symlinks is the one `PATH` entry the
  profile adds — behind the image's own directories, ahead of the run's
  `~/.local/bin`. `pins/` is pisafe's record and is mounted nowhere. Both mounts
  are outside every path a run writes, and Pi's own package store,
  `~/.pi/agent/npm`, is an ordinary directory in the run's home: `pi install`
  succeeds there, serves that run, and dies with it. The directory is created
  before the container starts so Podman cannot leave it root-owned.
- Each run gets a `settings.json` and `trust.json` written into its home, not
  mounted: they list each installed package by absolute path — never as an
  `npm:` source, which would make Pi re-check versions against a read-only store
  — pin `transport: "sse"`, and trust the run's own workspace. Whatever Pi then
  writes dies with the run. Every start rebuilds the profile's own entries from
  the record, so a removed extension disappears, and keeps every entry that is
  not the profile's, so a package the run installed survives a resume.
- `pisafe extension install PACKAGE[@VERSION]` is two containers with pisafe
  holding the pin between them: the first reports what the spec resolves to
  through `npm pack --dry-run --json`, the second installs that exact version
  with `--ignore-scripts` into its own npm prefix root, refusing a tarball whose
  SHA-512 is not the resolved integrity. The tree is streamed into the profile
  and renamed into place before the old one goes, and the record is written
  after the tree and before any removal. `extension remove` and `extension list`
  complete the set.
- `pisafe extension update` never applies anything unasked. What npm resolves
  each installed name to now is checked at most once a day, bounded to 45
  seconds, when a run stops — never at run start, which reaches no registry at
  all — and kept beside the pins in an advisory `updates.json` that is discarded
  wholesale if it is absent, oversized, malformed, or shaped wrong. What is
  pending is derived by comparing that file with the record, so applying or
  removing a package silences an offer without clearing anything, and a stop
  prints only when the day's check moved what is pending. Naming a package runs
  the same resolve-and-verify install path a first install takes.
- Stopping a run also reports what the run installed into its own package
  store, read from the run's settings out of run storage after the container is
  gone, bounded at 1 MiB. Every entry the profile did not put there is the
  run's; an `npm:` source is named with the `pisafe extension install` line that
  would keep it, anything else is named as something pisafe cannot pin. Sources
  are quoted, because a run chose them. Nothing is applied and nothing fails: a
  run whose settings are missing, oversized, or malformed reports nothing.
- `pisafe tool install PACKAGE[@VERSION]` uses that install path and then reads
  back what the tree claims: npm's own `node_modules/.bin` links, filtered to
  those pointing into the named package. A name another tool already provides
  refuses the install, and the `bin` directory is rebuilt whole from the record
  rather than edited, so nothing a failed install left behind outlives the
  record naming it. `tool remove` and `tool list` complete the set; there is no
  `tool update`, because installing again is one.
- The run image carries the toolchain a run cannot install for itself: `curl`,
  `jq`, `ripgrep`, `fd`, and `unzip` from Debian, plus `pnpm` and `uv` pinned to
  a recorded digest, alongside the `node`, `npm`, `git`, `openssl`, and `ssh` it
  already had. `uv` then installs CPython 3.13.14 into the image and links it as
  `python`, `python3`, and `python3.13`, so both spellings are one interpreter.

### Naming, emptying, and exporting durable state

- `pisafe project list` reports every store pisafe holds by the checkout path
  its record names — resolved, so the printed form is the one that hashes to the
  key — with how many run records still belong to it and whether the checkout is
  still there. It reads records only and starts nothing.
- `pisafe project reset [PATH]` empties every cache namespace of one project and
  leaves the session store alone; `pisafe project drop PATH --confirm PATH`
  takes the whole store, transcripts included; `pisafe project rebind OLD-PATH`
  gives the current checkout the session history of the one it was moved from,
  by copying the transcripts into a fresh store — additively, skipping any name
  the destination holds — and leaving the caches behind. A rebind refuses a
  destination that already has a store, and both drop and rebind refuse while
  any run record names the project.
- `pisafe profile reset --confirm` empties the extension, tool, and pin
  directories rather than removing what the record names, so a tree left by an
  install that failed after fetching goes with everything else.
- `pisafe backup DIRECTORY` writes a manifest plus one directory per project of
  the transcripts that project's store holds, and the two profile records
  verbatim. No cache and no credential is written at all. A transcript name the
  Mac will not write is refused and counted rather than failing the backup, and
  a name the backup already holds is kept, so backing up again into the same
  directory adds and never removes.
- `pisafe restore DIRECTORY` reads and validates the backup before the VM is
  touched, then for each project registers the record, ensures the store, and
  puts back every transcript the store does not already hold, by rename and
  owned by the run user. Each extension and tool is reinstalled from the pin the
  backup recorded, so the fetched bytes are checked against the integrity that
  was installed rather than against what npm resolves the name to now; a package
  already installed is left alone. A transcript failure stops the restore;
  package failures are collected and reported at the end.

### Run records and controller

- `internal/runstate` writes version-6, mode-0600 JSON manifests through
  `internal/safefile` — which every store that keeps a small file uses, so
  "bounded on the way in, whole on the way out, never anything but a regular
  file" has one implementation and it is the strictest of the ones it replaced —
  under the user config directory (or `PISAFE_STATE_DIR`, resolved to an
  absolute path so every store filed under it is reached the same way), enforcing
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
- `pisafe connect [RUN] [-- COMMAND...]` attaches the terminal to a run: a login
  shell in the run's workspace, or the command after `--` run there instead,
  over the same per-run SSH config Zed uses. The shell is the default because
  `exec pi` ends the session Pi was started in, so a shell reaches Pi for free
  while Pi reaches a shell only by reconnecting. Command words are joined and
  parsed by the run's own shell, the way ssh passes a command along, so a
  redirect, a pipe, a command list, or a variable assignment means in the run
  what it means on the Mac. Only the shell replaces the shell that started it: a
  command is left in it, because an `exec` would run the first command of a list
  and drop the rest. A pty is asked
  for only when stdin and stdout are both terminals, which is what lets an
  interactive Pi and a redirected stream share one command. It replaces its own
  process with `ssh`, so signals, window resizes, and the exit status are the
  session's. Only an active run within its budget is reachable; a stopped one is
  refused naming `pisafe resume`. `zed` and `forward` share that check. A run
  the VM stopped is brought back instead of refused: the three ask Lima's status
  and one `podman ps` before handing over — about 150 ms, no VM started — and on
  a run with no container they resume it through the same verified boundary
  `pisafe resume` uses, then connect. The wall-clock reading comes after that
  question, because since an outage costs a run nothing, a deadline in the past
  no longer means the budget was spent.
- `pisafe list` renders each record against that same `podman ps`: a run
  recorded active with no container is labelled as one, `(limit reached)` is
  printed only for a container the VM still has, and a VM that could not be
  asked prints neither label and says so.
- `pisafe forward [RUN] [LOCAL:]PORT...` reaches a server a run is hosting. Each
  port becomes an `ssh -L` listener on the Mac's loopback carried to the same
  address inside the container, so nothing binds in the VM and nothing outside
  this Mac can connect. A port and a run name cannot be confused for each other,
  so both are positional in any order. The command replaces itself with `ssh`
  after printing what it forwarded: the listeners belong to that process and
  end with it. A run's own SSH config clears every forwarding, so the request is
  made where it is meant — `ClearAllForwardings=no` appears on this command line
  and nowhere else.
- Every path that touches a run's container proves its identity — name, label,
  and image against the manifest. What the container is mounted on is proved
  where pisafe has just started one and is about to hand it over, at run and at
  resume: a writable profile or an unexpected persistent mount there is agent
  code able to reach what a later run reads. Stopping proves identity only. It
  destroys the container and publishes from run storage rather than through it,
  and requiring the current layout would strand a run whose container was built
  by an earlier pisafe — which is not hypothetical, since moving the profile
  mount did exactly that.
- `pisafe stop` removes only the container and accounts elapsed active seconds
  conservatively; `pisafe resume` verifies VM boundary, storage identity, image,
  container identity, and mount sources before recreating with the remaining
  budget. `pisafe zed` saves the connection Zed needs before opening a run and
  takes it back out when the run is reclaimed; pisafe never edits SSH
  configuration.

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
pisafe         0.0%   projectconfig 90.7%
pisafe-guest  71.2%   providers      0.0%
apikey        82.2%   runcontainer  85.1%
backup        80.7%   runcopy       78.2%
broker        93.9%   runctl        75.8%
chatgpt       76.7%   runid         90.5%
cli           33.1%   runimage      77.7%
gitstage      80.0%   runssh        68.8%
hostnet       50.0%   runstart      74.4%
keychain      60.0%   runstate      74.4%
lima          61.3%   zedsettings   80.4%
profile       96.0%
```

`lima` and `cli` fell as Phase 2 added VM-side scripts and command surface that
only the gated live suite and the end-to-end exercises execute. `providers` has
no test file of its own; it is covered through the CLI and broker suites and by
the stub-upstream exercise below.

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
  snapshot/archive mismatch; and the whole-word secret matcher. A collapsed
  directory is expanded whole and subtracted from the report, one file inside it
  leaves the rest excluded, a credential inside it is still refused, and a
  repository inside it is refused while a file beside that repository is not.
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
- **connect**: the exact SSH argument vector for a shell and for a command, with
  a workspace path that needs quoting for the remote shell, a redirect that must
  survive as syntax rather than become a program name, and a command list whose
  every command has to reach the run; a pty asked for only
  when both ends are terminals; everything after `--` kept for the run including
  words pisafe would otherwise read as its own; and every inactive run refused
  before anything launches, whether asked through `connect` or `zed`, with a
  stopped one told to resume.
- **cp inward**: the colon read as the direction on either side and both ends in
  a run refused; a credential-shaped name refused without `--unsafe` going in
  and untouched coming out; the destination resolved against the workspace, an
  existing directory taking the copy inside it, an occupied name refused and
  then replaced under `--force`; escaping destinations and names refused before
  the run is reached at all; and an archive naming something other than what the
  Mac said is arriving refused by the guest whatever it holds.
- **Git identity**: repository config preferred over global, an unconfigured Mac
  refused, an installed identity proven by reading back a commit's author,
  empty/oversized/newline values refused, unknown guest fields refused, and run
  creation stopping before boundary and image work.
- **Run image pins**: the Containerfile is checked to contain every recorded
  pin, to carry no floating tag, to assert Pi still ships a shrinkwrap, and to
  repin each shrinkwrap gap at exactly `PiVersion` with a SHA-512 digest — so a
  Pi bump that forgets the three sibling packages fails before any image builds.
- **Declared caches**: a repository with no config declaring nothing; a hostile
  declaration refused field by field and an oversized one refused outright; a
  key that follows both the declared inputs and the run image; a key file that
  tries to leave the checkout refused lexically; and an absent key file hashing
  as a state rather than failing.
- **Publishing, promotion, and reset**: a stop publishing and trimming every
  declared namespace; a run declaring no cache publishing nothing; transcripts
  promoted by a run that shares no cache; a failed publish still promoting; and
  either failure recorded on the run instead of failing the stop. Reset empties
  a cache no run is holding and refuses while a run could still mount a
  generation.
- **Project stores**: a store whose checkout is gone still nameable; dropping
  refused while a run belongs to it and refused for a store nothing records;
  dropping taking the filesystem before the record; rebinding carrying the
  transcripts and leaving the caches, refusing a destination that already has a
  store and refusing while either end has a run; and both rebind and restore
  registering the checkout before allocating its store.
- **The profile**: an empty profile valid rather than missing; an unreadable
  record naming itself; a pin the record cannot vouch for refused; only an
  exactly pinned npm package installable; a reinstall replacing rather than
  accumulating; two tools refusing to answer to one name; the links a run
  searches derived from the record; installed extensions becoming the packages a
  run loads; a writable profile refused as a run's profile; a container built
  from an earlier mount layout still recognized as the run's and still
  stoppable, while refused as one to keep running; a container labelled for
  another run refused everywhere; the packages a run installed for itself read
  back without any of it being trusted; and an offer that survives storage,
  differs only from what is installed, degrades to silence when it is corrupt,
  and is made once per change.
- **Backup and restore**: a manifest recording what a restore needs and nothing
  more, checked field by field so a credential has nowhere to travel; a manifest
  a restore cannot trust refused; an unfinished backup not readable as one; only
  a transcript crossing into the backup; a name the backup already holds kept;
  and a restore sending back only what an export would have written.
- **Providers**: a built-in login recording nothing that could go stale; a
  custom endpoint carrying its own upstream and never redirecting a known one;
  declared models that cannot route around the broker; plaintext endpoints
  refused unless they stay on this Mac; an endpoint that would be requested
  twice over refused; a key read from stdin and never blank; a login of either
  kind removable without being read back; and a key read only when a request is
  relayed.
- **CLI**: every mistyped scope command refused before anything reaches the VM,
  an extension refusing what it cannot pin before reaching the VM, and a restore
  reading its backup before it starts anything.
- The generated Lima YAML is checked by the installed Lima validator in the
  normal suite.

### Live verification

```sh
PISAFE_LIVE_LIMA=1 go test -v ./internal/lima -run TestLiveCreateAndStart
PISAFE_LIVE_LIMA=1 go test -v ./internal/runimage
limactl shell pisafe -- podman images --no-trunc --format '{{.Id}} {{.Repository}}'
PISAFE_LIVE_LIMA=1 PISAFE_LIVE_RUN_IMAGE=sha256:<image-id> go test -v ./...
PISAFE_LIVE_STATE_DISK=1 go test -v ./internal/lima -run TestLiveStateDisk
```

The state-disk test is separate from `PISAFE_LIVE_LIMA` because it proves its
property by deleting the instance holding it. It drives a throwaway instance and
a renamed disk, and never touches the dedicated pair.

Everything that mounts a run needs the immutable ID of a locally built run
image, which is why the image list comes before the last command. Any change to
the VM definition moves the security profile digest, so the VM must be deleted
and recreated before these pass.

Verified against a real ARM64 VM:

- **Boundary**: Lima reaches READY, `/Users` is absent, no Podman socket is
  forwarded, the user namespace has the expected direct UID plus 65,536-ID
  subordinate mapping, the firewall service and table are active, IPv6 is
  disabled. Public HTTPS works from a rootless container; RFC1918 and metadata
  TCP destinations fail. Unrestricted `sudo -n true` is denied while the clock,
  firewall-status, and storage helpers work through their narrow rules; the
  root-owned security-profile record is mode 0444 and matches the generated
  definition.
- **State disk outliving the instance**: a first instance formats the attached
  blank device, mounts it at `/var/lib/pisafe`, and creates a run's storage on
  it. Deleting the instance removes its directory and leaves the disk. A second
  instance, which has never seen the disk, finds it by label, mounts it, and
  verifies that same run's storage — while the identical check against storage
  that was never created fails, so the first result is not the helper being
  lenient. Both instances report `/var/lib/pisafe` backed by the labelled disk.
- **Traffic shaped to look permitted**, from a rootless container: a public
  wildcard resolver answering `10.0.0.1` and `169.254.169.254` for names that
  encode them was refused at connect time, with the answer itself required so a
  resolver failure cannot pass as a denial; a client that followed a redirect
  into link-local failed on the second hop, naming it; a DNS query to
  `10.0.0.1` went unanswered, so the reject rules hold for a datagram expecting
  no handshake; and `host.containers.internal`, `host.docker.internal`, the
  container's own address, the default gateway, loopback, and the broker
  address were all unreachable on port 22, which the input chain accepts and
  sshd is listening on. From the VM's unprivileged user, sshd and the local
  resolver were both refused on loopback while both were listening, because the
  loopback exception is root's alone. Each denial was shown to be the ruleset
  rather than a dead path: every assertion was re-run against a permitted
  destination and reported the breach.
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
  `/Users`, and `podman port` reported no published port. Over the same
  endpoint, an HTTP server started on the container's loopback answered a
  request made on this Mac's loopback through an `ssh -L` channel, while the
  same client asking for one address over — `127.0.0.2` — was refused
  "administratively prohibited". Run against the previous image, whose runs were
  configured before local forwarding existed, that first assertion fails with
  the same refusal, so the test is measuring the policy rather than the wiring.
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
- **cp inward**, against a real run created before the command existed, which is
  the case it is for: a file landed in the workspace root and came back
  byte-for-byte; a directory arrived with its tree; a second copy of one name
  was refused naming `--force` and then replaced with it; a destination naming
  an existing directory landed inside it while one naming nothing was taken
  literally; and `../escape` was refused on the Mac before the run was reached.
  The refusal from inside the run reads through the transport's own framing —
  `copy into run: execute in Lima VM: limactl shell: pisafe-guest: ... pass
  --force` — which is how every guest-side error already reads and was left
  alone rather than special-cased here.
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
- **Shared project layers**, iterating every overlay the run spec declares
  (`TestLiveProjectLayersAreSharedToReadAndPrivateToWrite`): two concurrent runs
  both read the seeded project state and neither sees the other's writes.
- **Cache selection and publishing**
  (`TestLiveCacheSnapshotsAreSelectedByKeyThenRecency`,
  `TestLivePublishedGenerationsAreImmutableAndDisposable`): an empty namespace
  selects nothing, either seeded generation is found at its exact key, and an
  unknown key falls back to the newest. A run's fetches and its deletions are
  both in the generation it publishes, the generation it restored is
  byte-unchanged, the next run falls back to what was just published, eviction
  spares a generation a run may still mount, and reset empties the cache while
  leaving the session store alone.
- **Session promotion**
  (`TestLiveFinishedTranscriptsPromoteWhileLiveOnesStayPrivate`): a later run
  reads a finished run's transcript, a live run's transcript reaches no
  concurrent run, and neither a rewrite nor a deletion inside a run follows a
  transcript another run already promoted.
- **The profile mount** (`TestLiveTheProfileLoadsAndStaysReadOnlyToTheRun`): a
  package seeded into the profile registers a flag that `pi --help` then prints,
  so its code ran; the repository's own extension loads without a trust prompt;
  `pi -e` still works; the profile refuses every write; `pi install` succeeds,
  leaving the package in the run's own store and the profile exactly as it was;
  and what a stop would report is that package and not the profile's.
- **Pinned installs** (`TestLiveAnInstalledExtensionIsPinnedToWhatWasFetched`):
  the recorded pin is the registry's own answer, bytes that hash to anything
  else never reach the profile, the installed tree is that exact release,
  reinstalling replaces rather than accumulates, and the next run resolves the
  package.
- **Offers** (`TestLiveAnAvailableUpdateIsOfferedAndNeverApplied`): a check
  leaves the pin, the tree, and the mounted directory exactly as they were; what
  it found survives storage and is pending only while the record disagrees with
  it; a second check answering the same thing leaves nothing to say; and
  applying goes through the same fetch-and-verify path an install takes.
- **The toolchain** (`TestLiveTheToolchainIsReachableAndNeverShadowed`,
  `TestLiveAnInstalledToolIsOnEveryRunsPathAndNeverWritable`): every tool the
  image carries on purpose resolves inside a run, an executable the run drops in
  its own home is invocable by name, and one named `git` still loses to the
  image's. An installed command resolves and runs, its dependencies' commands do
  not reach the run's `PATH`, the run cannot add, replace, or delete a link, and
  removing one stops it resolving even in a run that is already live.
- **Project stores** (`TestLiveAReclaimedProjectStoreTakesEverythingWithIt`,
  `TestLiveARebindCarriesTheHistoryAndNotTheCaches`): a seeded store is mounted
  and holds a transcript and a cache generation, reclaiming it leaves neither
  the mount nor the directory, reclaiming again is not a failure, and the same
  key then allocates a filesystem with none of the old project in it. A rebind's
  transcripts arrive under the key another checkout path hashes to, a name the
  destination already holds is left as it is, the caches are not carried, and
  reading the source leaves it unchanged.
- **Backup and restore** (`TestLiveABackupCarriesTheTranscriptsOutAndBackIn`): a
  store's transcripts reach a directory on the Mac and go back into a different
  store, a name the destination already holds is left as it is, the restored
  transcripts belong to the run user, no cache travels, no staging survives, and
  reading a store leaves it unchanged.
- **Several upstreams at once**, verified end to end against a stub upstream on
  the Mac's own loopback rather than by a live test, since a real one would need
  real provider credentials: a real run's `models.json` carried both the
  subscription and a keyed provider on their own routes with one capability, and
  a request from inside the run reached the stub carrying the upstream key the
  run never held.
- **Backup end to end**, with the real binary in an isolated state directory: a
  run wrote a transcript, stopping promoted it, and `pisafe backup` carried it
  out while refusing and counting a planted name it would not write. Discarding
  the run, dropping the store, and resetting the profile left nothing;
  `pisafe restore` brought back the record, the store, the transcript — owned
  `1000:1000`, mode 0600, byte-identical — and both pinned packages; restoring
  again reported them already installed; and a new run of that checkout opened
  with the transcript in `/sessions`, ran the restored tool, and loaded the
  restored extension. A second backup into the same directory added the new
  transcript and kept its own copy of one the store had rewritten in place.
- **`pisafe profile reset` has no live test**, deliberately: there is one
  profile and nothing scopes a test to a profile of its own, so a live test
  would empty whatever the user has installed. It is covered by the transport's
  argument test and by a manual exercise against a seeded tree, in which an
  unrecorded tree and a stale link both went.

### Substrate for shared state

Established against the live VM and the pinned image before any of Phase 2 was
built, and worth re-checking after a Podman or Pi bump:

- **Podman mounts shared state rootlessly** as
  `-v <lower>:<dst>:O,upperdir=<upper>,workdir=<work>`, and is particular about
  the rest: `:O` is refused alongside any other mount option, so a shared mount
  cannot carry `nodev,nosuid`; `--mount type=overlay` does not exist; and both
  `upperdir` and `workdir` must already exist. The overlay may span two
  filesystems, with the lower on the project's image and the upper on the run's.
- **A mounted lower must not change underneath a live run.** overlayfs leaves
  behaviour undefined if the underlying filesystem is modified while part of a
  mounted overlay. This is a kernel constraint, not a merge-semantics one, and
  it is why shared state is written by creating new directories rather than by
  editing existing ones.
- **The lower's contents must be owned by the container's mapped UID**
  (`subuid_start + 999`) or the overlay is read-only in practice. The
  consequence worth remembering: everything the privileged helper creates is
  writable by the pisafe user under `podman unshare`, so pisafe can build
  structure *inside* a helper-created directory with no new privilege.
- **npm's cache is only half content-addressed.** `_cacache/content-v2` is keyed
  by the hash of the bytes, but `index-v5` is keyed by the hash of the *request
  URL*, so two runs fetching one package write one path with different contents.
  Do not build on directory merging. `npm_config_cache` moves the whole cache,
  and `npm_config_logs_dir` keeps per-run logs out of it.
- **Pi writes its transcript where the environment says.**
  `PI_CODING_AGENT_SESSION_DIR` is read by Pi — the literal never appears in
  `dist` because the name is assembled from `APP_NAME` at build time — and
  `--session-dir` overrides it. A relocated session directory is flat, with no
  per-cwd level, and listing one filters by each transcript's own recorded cwd,
  which is one value for every run of a project.
- **A transcript's name cannot collide, and Pi deliberately never locks one.**
  The file is `<ISO timestamp>_<randomUUID>.jsonl`, appended a line at a time.
  Pi does use `proper-lockfile` for `settings.json`, `trust.json`, and its auth
  store — the files it genuinely shares — and for no session file. The only case
  where it rewrites an existing transcript is migrating it to the current
  session version when it loads it.
- **Pi installs global packages under `$PI_CODING_AGENT_DIR/npm`**, and
  `PI_PACKAGE_DIR` does not relocate them: it locates Pi's own installation for
  Nix and Guix store paths. A package on a genuinely read-only mount loads and
  its code runs. A package named by absolute path is inert; one named `npm:` is
  not, because Pi compares the installed version against the configured one at
  every startup and installs when they disagree.
- **A bind mount is unreadable to a container without the container SELinux
  label**, and the failure is `EACCES` rather than a mount error. A mountpoint
  Podman creates inside the run home is root-owned, which would leave Pi unable
  to write its own settings. Podman also reports a read-only bind as
  `RW: false` rather than as an `ro` option, so a manifest check that only reads
  mount options cannot tell a read-only profile from a writable one.
- **`npm pack --dry-run --json` reports the version and integrity of what a spec
  resolves to**, scoped packages included, which is the pin without fetching
  anything into the profile.

```sh
# Overlay isolation: conflicting writes over one shared lower.
limactl shell pisafe podman run --rm --user 1000:1000 --network=none \
  -v <lower>:/cache:O,upperdir=<upper>,workdir=<work> \
  docker.io/library/alpine:3.22 sh -ec '...'

# npm cache shape, to confirm it is still not safe to merge.
ls ~/.npm/_cacache            # content-v2 (by content), index-v5 (by request URL)

# Where Pi installs a global package, and whether a package it loads by path
# stays inert. The profile mounts at the first, and the second is what keeps a
# read-only store from turning a version disagreement into a broken run start.
limactl shell pisafe -- podman run --rm --pull=never --network=none \
  --entrypoint sh <run-image> -c '
root=/usr/local/lib/node_modules/@earendil-works/pi-coding-agent
grep -n "getNpmInstallPath(source, scope)" -A 12 "$root/dist/core/package-manager.js"
grep -n "resolveLocalExtensionSource(parsed" -B 4 "$root/dist/core/package-manager.js"'

# Session layout, naming, and what Pi locks. A relocated session directory must
# still be flat, transcript names must still carry a UUID, and no session file
# may acquire a lock, or promotion needs a merge it cannot perform.
limactl shell pisafe -- podman run --rm --pull=never --network=none \
  --entrypoint sh <run-image> -c '
root=/usr/local/lib/node_modules/@earendil-works/pi-coding-agent
sed -n "1,12p" "$root/docs/session-format.md"
grep -n "sessionFile = join\|getDefaultSessionDirPath\|listSessionsFromDir" \
  "$root/dist/core/session-manager.js"
grep -rln "proper-lockfile" "$root/dist"'
```

Probe directories are owned by mapped UIDs, so clean them up with
`podman unshare rm -rf`.

## Live VM state

A persistent Lima instance named `pisafe` was left running with security profile
`sha256:906c5bd13b53594ed1513187e804705e301873b3f0969758eac460d8345fb20c`,
holding cached base/test images plus the current managed run image:

```text
recipe digest: sha256:4305e790f9baf24b9b44ee350006192fe166f364ba559b96b71e04b25d3db91d
image ID:      sha256:2e6d954bbbc57ad81193d03285bd60ca25eebb3a806a5a81803629b7f90925c5
```

The instance was recreated during Phase 2: the storage helper gained the project
and profile scopes and their namespaces, which are hashed into the security
profile. Every helper change costs one recreation and destroys every project
store with it, which is why they were batched and why `backup` exists.

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
16. Podman refusing `:O` alongside any other mount option, so shared mounts
    cannot carry `nodev,nosuid` and are the only mounts in a run without them.
17. An overlay whose lower is not owned by the container's mapped UID being
    read-only in practice, with nothing failing at mount time to say so.
18. A bind mount without the container SELinux label failing as `EACCES` inside
    the container rather than as a mount error, which is why the profile is
    allocated by the privileged helper rather than created by pisafe.
19. Podman creating the profile's mountpoint inside the run home as root,
    leaving Pi unable to write its own `settings.json`; pisafe now creates the
    path before the run starts.
20. `runcopy.ExtractInto` stopping at the tar end-of-archive marker while system
    `tar` pads its output to its blocking factor, so the VM-side sender stayed
    blocked writing into a pipe the Mac had already closed. Invisible for as
    long as every sender was Go's `archive/tar`, which writes exactly the two
    zero blocks the reader consumes; the extractor now drains what it is given.
21. `pisafe zed` failing on the first open of a run even with the connection
    saved: Zed rereads its settings from a file watcher, and a run handed over
    in the same breath as the write reaches `ssh` with no `-F` and an
    unresolvable host name. Zed's log names the exact `ssh` argument vector,
    which is what made this measurable rather than guessed at — probe
    connections written and opened at 0, 0.1, 0.25, 0.5, and 1.0 seconds failed
    to resolve only at none. `zed` now settles for half a second when, and only
    when, it wrote.

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
  equivalent in Pi and its dependencies without being byte-identical images. Now
  that a helper change reuses those layers, they are also refetched less often —
  what an image carries from Debian is as old as the last change to the
  Containerfile or the base image, both of which are pinned by digest anyway.
- The three sibling digests are pinned by hand and must be refreshed whenever
  `PiVersion` moves. A unit test refuses a mismatch, so the failure is loud, but
  regenerating them means packing each tarball and recording its SHA-512.
- A session store grows without bound. Nothing evicts a transcript, `project
  reset` leaves them, and the only thing that reclaims one is dropping the whole
  project store — so a long-lived project is bounded by its 10 GiB filesystem
  and nothing else warns before that.
- Declarable cache environment variables have no allowlist. A project may
  declare `CARGO_HOME`, which moves installed binaries and credentials as well
  as the registry cache. No boundary breaks, because it is shared across that
  repository's own runs only, but a compromised run would then read what an
  earlier one left, and nothing tells a repository which variables are a bad
  idea.
- Cache key inputs are a literal list of relative paths, so a monorepo cannot
  name `**/package-lock.json`. Globs were deferred rather than refused.
- A run that executed hostile code can publish a cache generation a later run
  restores. *Flagged as uncertain.* The containment is that the later run is
  itself sandboxed, that lockfile integrity checks reject a tampered tarball,
  and that the cache is disposable; publishing only on `apply` is the fallback.
- A package needing an install script cannot be installed into the profile,
  because both install containers run with `--ignore-scripts`.
- Tools have no update offer. Installing one again is the update, but nothing
  reports that a newer release exists, which extensions do get. pisafe also
  never claims one version is newer than another — it reports only that the
  registry's answer differs from the pin — and a declined offer is not raised
  again until the registry moves.
- Two installs running at once can lose an entry from the profile record. The
  loser's tree stays in the profile unrecorded and re-running the command fixes
  it; no lock was taken for a single-user command.
- A restore reinstalls each package from npm, so a backup does not carry the
  packages themselves and a release unpublished since the backup cannot be put
  back. What the backup does hold is enough to say exactly what was lost.
- There is no way to author a global `settings.json`. The mechanism exists —
  settings are written into the run rather than mounted — but nothing yet edits
  the file, so a run's settings are what pisafe renders.
- `pisafe profile reset` has no live test, because there is one profile and a
  live test would empty whatever the user has installed. A second profile name,
  which the layout already allows and nothing reads, is what would close this.
- `internal/providers` has no test file of its own. It is exercised through the
  CLI and broker suites and by the stub-upstream end-to-end run.

## What comes next

Nothing is planned. Every command the design enumerates is implemented and
live-verified, both components it singles out as carrying security weight — the
packet filter and the inference broker — have the bypass and contract tests it
asks for, and the three Phase 2 invariants each have a live test that fails if
they stop holding.

What is left is the Known gaps list above. Two of them are open questions
rather than missing work, and both wait for a real repository to answer them:
whether the environment variables a project may declare need an allowlist, and
whether tools should get the update offer extensions have. The rest are either
deliberate refusals with the reasoning recorded in
[`DECISIONS.md`](DECISIONS.md), or small things nobody has needed yet.

The one piece of upkeep that is not optional: the OAuth flow, the embedded
model catalog, and the three sibling digests all mirror the pinned Pi release,
and every one of them must be re-checked when `PiVersion` moves.

Do not weaken the boundary for any of it.

## Useful references

- Lima plain mode: <https://lima-vm.io/docs/config/plain/>
- Lima SSH: <https://lima-vm.io/docs/usage/ssh/>
- Lima Podman example: <https://lima-vm.io/docs/examples/containers/podman/>
- Podman rootless networking:
  <https://docs.podman.io/en/stable/markdown/podman-network.1.html>
