# `pisafe` decisions

Choices made while implementing [`pisafe-design.md`](pisafe-design.md) that the
design does not settle on its own. Each entry states what was decided, what was
not taken, and why; reversibility is noted only where undoing the choice would
be costly.

A few decisions constrain the design itself rather than its implementation —
the static broker port, the absence of any firewall-mutation privilege, the
lifetime of a run's record, no user credentials in the sandbox. Those are
stated as rules in the design document and repeated here with their reasoning.

This log was condensed once, when the documents were compressed: entries that
had become plain descriptions of shipped code, or that recorded a choice
already reversed by a later one, were dropped rather than kept as history. Git
history holds them. New entries are appended in full.

## VM boundary and SSH

- Per-run SSH uses a portless Lima-control-SSH `ProxyCommand` and `podman exec`
  stdio relay, not a VM-loopback published port: the static firewall correctly
  denies VM loopback to the unprivileged Lima user, and opening dynamic
  exceptions would add mutable privileged state. Reversing changes stored
  connection metadata and the firewall contract.
- A network-disabled one-shot container initializes the run home, then non-root
  `sshd` is the container's main process. A detached `sshd` started afterwards
  would disappear across stop/resume and need a second process-lifecycle
  mechanism.
- PiSafe writes one strict SSH config fragment per run and never edits the
  user's global SSH or Zed settings: `pisafe run` prints the exact command for
  Zed's one-time "Connect New Server" flow, which supports `-F`.
- The output and forward chains accept `ct state established,related` like the
  input chain always has. Live testing showed the broker handshake dying
  because sshd's SYN-ACK from `192.0.2.1:18080` carries the client's ephemeral
  port and was rejected by the TEST-NET deny. A narrow return rule matching
  only the broker address was not retained because per-flow exceptions recreate
  this bug for every future accepted flow; the stateful design gates connection
  initiation once and lets conntrack own replies. Deny-set changes now stop new
  connections rather than tearing down established ones, which is acceptable
  because start/resume already fail closed on network change.
- The run's `sshd_config` restates the container's declared environment through
  `SetEnv` rather than leaving sessions to inherit it. sshd builds each session
  environment from scratch, which was confirmed live: the container carries
  `NODE_VERSION=24.18.0` while an SSH session sees it unset. Without this, no
  terminal session — `connect` or Zed — ran under the environment the container
  contract states, so `GIT_TERMINAL_PROMPT=0` in particular was absent wherever
  a human could act on a prompt.
- `pisafe connect` replaces its own process with `ssh` instead of supervising it
  as a child. The terminal belongs to the run for the rest of the session, so a
  parent would only relay signals, window resizes, and the exit status. The cost
  is that `connect` can print nothing afterwards.
- `connect` refuses a stopped run and names `pisafe resume` instead of resuming
  it. Resuming spends the run's wall-clock budget, which stays an explicit act.
- Lima's default VZ user-mode network remains in the generated profile. Native
  `vzNAT` was tested but exhibited the same stopped-VM SSH recovery failure and
  made its Mac-side interface appear only after the immutable host-network
  profile was captured; QEMU would add a host dependency. Changing this
  requires VM recreation.
- The run-image Containerfile is embedded in the controller while the static
  Linux ARM64 guest helper is a sibling release artifact. Building the helper
  at runtime would require a Go toolchain in the installed product; checking a
  binary into Git would make history unreviewable. Changing the layout changes
  the managed recipe digest.
- Pi's transitive tree is frozen by the `npm-shrinkwrap.json` Pi publishes
  inside its own tarball, which the pinned top-level digest already covers, so
  the build asserts that shrinkwrap is still there rather than shipping a
  lockfile of pisafe's own. Three packages — `pi-agent-core`, `pi-ai`, `pi-tui`
  — appear in that shrinkwrap with a resolved URL but no integrity hash, which
  is not a pin: npm re-resolved their `^0.82.0` ranges and installed 0.82.1 into
  the real image while the shrinkwrap named 0.82.0. Each is therefore re-fetched
  by exact version, checked against a digest recorded beside `PiIntegrity`, and
  extracted over what npm installed. Shipping a `package-lock.json` and using
  `npm ci` was tested and rejected: when a dependency publishes a shrinkwrap npm
  reads that instead, so a corrupted *nested* integrity in our lockfile installs
  cleanly while only the *top-level* one raises `EINTEGRITY`. Every nested entry
  would have been decorative, and a reader would reasonably assume otherwise.
  npm `overrides` were rejected for the same reason — they do not penetrate a
  published shrinkwrap either. The cost is three digests that must move with
  `PiVersion`; a unit test fails the build until they do.

## Storage and lifecycle

- Persistent run data uses one fixed-size 10 GiB sparse ext4 filesystem holding
  the workspace and home, mounted and removed by a narrow fixed-policy helper.
  Unbounded rootless Podman volumes and Podman's XFS-only volume quota were not
  retained because the pinned Fedora image uses Btrfs and the quota options
  require root; a parent Btrfs qgroup was rejected because untrusted code could
  create uncharged nested subvolumes. Reversing requires a storage migration.
- Runs receive eight cumulative active hours. Podman's independent `--timeout`
  enforces each active interval, while stop removes the container and resume
  recreates it over the same storage with only the recorded remainder. A
  controller daemon or mutable VM-side timer would add a second trusted
  lifecycle service. Changing the default is cheap; changing accounting
  semantics requires a manifest migration.
- Destructive confirmation is the repeated non-interactive form
  `pisafe discard RUN --confirm RUN`, so it works identically in scripts and
  terminals. The same reasoning makes `cp --force` a flag rather than a prompt:
  the CLI has no stdin channel.
- Manifests are versioned rather than migrated. Version 2 made activation
  atomically require the SSH connection record, version 3 made active-budget
  accounting and deadlines durable, and version 4 bound the inference
  capability to the active state. Each time, no released records existed, and a
  compatibility path would have weakened the invariant being added or inferred
  untrustworthy history. Any future change after real users exist needs an
  explicit migration.
- A run's record lives exactly as long as the run owns something: `discard` and
  `gc` remove the record with the resources, and discard is reachable from
  every state that still owns them, including `imported`. An earlier increment
  kept an `expired` or `discarded` record forever to satisfy "keep
  branch/import metadata after workspace deletion", but that requirement is met
  by the branch's own name — `pisafe/<run>` contains the run ID — so the kept
  record restated the branch, base commit, author, and included files, all of
  which git holds. The one fact it added, when the import happened, is in the
  reflog for 90 days. Reversing means reintroducing a terminal state and
  re-deriving deleted records; the branches themselves are unaffected.
- Collection reclaims only imported runs. Reclaiming an old stopped run once a
  check proves it holds no unimported commits was not taken: `diff` can prove
  the repository is unchanged but sees nothing of the run's home directory, so
  "no commits" is not "nothing to lose", and deleting work the user never
  imported is the one mistake that cannot be undone. Adding the check later
  would gate a reclamation that already exists.
- Image pruning keeps the current recipe by reading the recipe label each image
  carries, rather than by resolving the recipe's image ID first: resolving
  cannot distinguish "no image for this recipe" from "the lookup failed", and
  the consequence of that confusion is deleting the image in use. Only images
  pinned by a run that can still start a container are retained; an imported
  run pins none, because every command that still reads its workspace runs the
  controller's current image.

## Staging and apply

- Selected untracked inputs are chosen with repeatable `--include PATH` and
  `--include-unsafe PATH` flags rather than an interactive picker, matching the
  non-interactive style of `discard --confirm` and keeping `pisafe run`
  scriptable. A credential-shaped name is refused by `--include` and needs the
  separate flag, so approving one can never be a slip of the finger. An
  interactive selector can be added later without changing the staging
  contract.
- Credential-shaped names are matched on whole words plus a fixed name and
  extension list, so `tokenizer.json` is not flagged while `api_token.json` is.
  Substring matching produced false positives that would have trained the user
  to reach for the unsafe flag by habit.
- Selected inputs cross the boundary as an uncompressed tar beside the bundle
  and patch, not as a second Git bundle or a commit synthesized on the Mac:
  these files are by definition outside Git, tar carries the executable bit and
  symlinks, and it reuses the existing size- and SHA-256-verified upload path.
  The staged snapshot, not the archive, decides which names are legitimate.
- A submodule is staged from its checked-out HEAD, not from the gitlink the
  superproject index records, and the superproject baseline then records where
  it actually ended up. Reconstructing the recorded gitlink would need a commit
  that may be unreachable from the submodule's refs and would silently discard a
  submodule the user had moved. Consequently the superproject patch is captured
  with `--ignore-submodules=all`, so gitlink changes travel exactly once.
- A dirty submodule working tree is captured and committed inside the
  submodule, symmetrically with the superproject. Refusing would be simpler but
  would strand uncommitted submodule work on the Mac.
- Nested submodules fail closed rather than being staged recursively: one level
  covers the repositories this is built for, and recursion multiplies the
  artifact, path-safety, and apply-journal surface. Lifting the limit is
  additive.
- The apply journal records only ref creations, because apply only ever creates
  `pisafe/<run>`. The compare-and-swap discipline and its recovery rules are
  implemented in full; the general old-value restore is not, because no code
  path produces a step with a previous value and an untested branch is worse
  than an absent one. Submodule refs are committed before the superproject ref,
  since the reverse order could leave a superproject branch whose gitlinks name
  commits no ref keeps reachable.
- Apply stops an active run before capturing it, refuses a second apply, and
  captures in a throwaway `--network=none` container over the run's workspace
  rather than exec-ing into the live run container. It therefore works whether
  or not the run is up, costs none of the eight-hour budget, and never captures
  a workspace the agent is still writing to.
- Apply uses the controller's current managed run image, not the image the
  manifest records: the guest helper that captures a run must match the
  controller that reads what it produced, and pinning each run to its original
  helper would strand runs created by an earlier pisafe.
- A prepared apply carries hashes and fixed artifact names, never filesystem
  paths. Reporting the paths the run wrote would let a compromised run name a
  file on the Mac; instead both sides derive the same names from the same
  helper, and the Mac reads only from the package directory it chose.
- A run commits as the identity Git would use in the source repository,
  resolved on the Mac and installed into the run's own global configuration. A
  neutral `pisafe` author would misattribute the user's work, and leaving the
  run unconfigured is what made every agent commit fail. This copies a name and
  address into the run, but every commit in the bundle already carries them. A
  repository with no configured identity refuses to start a run rather than
  falling back to a placeholder, which would be discovered only in the imported
  history when rewriting it is expensive.
- The keep-or-drop question about a run's baseline commit is asked
  interactively, unlike every other choice pisafe puts behind a flag. A run is
  imported once and cannot be applied again, so a default would settle the
  question for a user who never learned it was asked.
  `--keep-baseline`/`--drop-baseline` answer it in advance for scripts and for
  the second attempt after a conflict.
- The replay runs `git rebase --onto` in a throwaway worktree beside the run's
  package directory, publishes its result under `refs/pisafe/replay/<run>`, and
  deletes that ref once the bundle is written. Rebasing the run's own branch
  would have been simpler but destroys the alternative: an apply that then fails
  for any other reason would leave the user with no baseline left to keep. The
  cost is a second checkout inside run storage for the duration of the replay.
- A replay stopped by a conflict is reported as an answer, not a failed apply:
  the run keeps its state, no `last_error` is recorded, and the user is pointed
  at the three ways forward the design names — keep the baseline, resolve it in
  the run, or do nothing.
- The drop is refused outright when a submodule carried uncommitted work of its
  own, rather than dropping only the superproject's baseline or rewriting the
  run's commits to follow the submodule's new commit IDs. Every superproject
  commit records where its submodules stood, so the two histories cannot be
  separated without rewriting one of them; a partial drop would be a silent
  half-answer. Lifting this needs commit rewriting with a gitlink map, which is
  additive.
- The Mac verifies the drop instead of trusting the run's word for it: the
  baseline commit exists only inside the run, so a source repository that knows
  it after the fetch learned it from the bundle that just arrived, and apply
  stops.
- Activation records the baseline commit each submodule actually got, not just
  the superproject's. The materialized snapshot always carried them and the
  manifest always had the field; discarding them made `pisafe diff` report a
  user's carried-in submodule changes as the agent's work.

## Getting work out

- `pisafe diff` reports commit subjects, paths, and line counts rather than
  file content. Streaming the patch was rejected: everything in a run is
  untrusted, and writing it to the terminal is the injection surface pisafe
  exists to remove, while a sanitizer would be more code and still weaker than
  importing the run and using `git diff`. Content-level review stays behind
  `apply`; `cp` remains the way to take individual files out.
- Diff measures from the run's baseline commit, not the source HEAD, so dirty
  state the user carried in is not reported as the agent's work. The cost is
  that those carried-in changes never appear; they are already in the user's
  checkout.
- Diff and cp mount the run's workspace read-only in a throwaway container,
  with Git's optional index locks disabled, so they neither alter nor block a
  run an agent is still working in. `cp` streams the archive out of stdout
  instead of writing it into the run and fetching it as apply does, so nothing
  is written inside the run and no gigabyte-scale temporary file lands in run
  storage. The cost is that the transfer carries no separate hash: SSH protects
  it in flight, and the run is the authority on the content either way.
- `pisafe cp` copies only regular files and directories. A symlink stops the
  copy naming its path, rather than being recreated when it stays inside the
  copied tree: a link resolves against a filesystem the run never saw, and a
  copy out is a leaf operation with no later step that would catch a wrong
  target. This is stricter than it needs to be, so naming a narrower path is
  the way around it.

## Inference broker

- The broker relay port is the static firewall exception `192.0.2.1:18080`,
  baked into the nftables ruleset and an exact `PermitListen` at provisioning.
  A runtime-mutable broker port set, and any sudo helper to populate it, were
  not retained because the boundary deliberately grants the VM user no
  firewall-mutation privilege. Changing the port or address requires VM
  recreation.
- The reverse forward is a dedicated `ssh -N -R` child with multiplexing
  disabled and `ExitOnForwardFailure=yes`, and the run-side `models.json` is
  installed by `pisafe-guest configure-inference` through `podman exec` stdin at
  activation and resume. Reusing Lima's control master, or writing the
  configuration once at home initialization, were not retained because the
  forward must die exactly with the broker process and the capability rotates
  while the home directory persists.
- The ChatGPT OAuth flow is reimplemented in Go from the pinned Pi AI client's
  constants, not run through Node: the controller is dependency-free and the
  Mac has no pinned Node runtime. The trade is that upstream flow changes must
  be re-mirrored when the Pi pin moves. The browser flow is the only login
  method; the device-code variant is unnecessary on a Mac with a browser.
- Tokens persist in the login keychain through `/usr/bin/security`, written
  over its interactive stdin and base64-wrapped, with account `chatgpt` and
  service `pisafe`. Security.framework bindings (cgo) and a broker-only
  encrypted file were not retained: the CLI keeps the build dependency-free and
  the Keychain provides at-rest encryption plus user-visible audit. Passing the
  secret as a command argument was rejected because argv is visible to every
  local process. `pisafe login chatgpt` fully replaced the interim
  `PISAFE_INFERENCE_*` environment configuration, because two configuration
  surfaces for one upstream would have to be reconciled on every future
  provider change.
- Access tokens refresh proactively inside the broker within five minutes of
  expiry, serialized, and the rotated refresh token is persisted before use. A
  reactive refresh-on-401 path was not retained because the provider rotates
  refresh tokens and a retry layer would complicate the streaming relay for no
  additional safety.
- Runs speak Pi's `openai-codex-responses` API against the broker. The pinned
  client refuses an apiKey that does not parse as a JWT carrying a
  `chatgpt_account_id` claim, so the run capability is wrapped in an unsigned
  JWT whose payload holds only the placeholder account ID `pisafe` and whose
  signature segment is the capability; the broker strips the wrapper before
  constant-time matching and always sets the real Authorization and
  chatgpt-account-id headers itself. Translating between the standard Responses
  API and the Codex backend inside the broker was not retained because body
  rewriting would own streaming and tool-call fidelity, which the design
  leaves upstream.
- The run-side model list is a curated catalog embedded from the pinned Pi AI
  Codex data with per-model `api`/`provider`/`baseUrl`/`headers` stripped, so a
  models.json entry can never route a run around the broker. Live catalog
  refresh was not retained; the catalog moves with the Pi pin in the same
  commit.
- `pisafe-guest configure-inference` also pins `transport: "sse"` in the run's
  Pi settings, merging rather than replacing what Pi wrote itself: Pi's default
  auto transport dials a WebSocket first, which the HTTP relay cannot speak.

## Documentation

- The design document is a spec and this file is its decision log; what the
  code currently does lives in
  [`IMPLEMENTATION_PROGRESS.md`](IMPLEMENTATION_PROGRESS.md). Keeping all three
  in one document was not retained: the design had grown to 1000 lines, a third
  of it history, and the spec was unreadable in one sitting. The cost is that a
  reader must follow one link to find the reasoning behind a rule.
- The design states requirements and invariants, not mechanisms that the
  implementation already documents more precisely. Where the two disagree, the
  design is the authority on what must hold and the progress document is the
  authority on what currently happens.
- The design's Phase 2 sections were compressed to invariants, dropping the
  mechanism sketched for unbuilt features (cache overlays with merge-back under
  a lock, session-ID append semantics). That detail was written before any of
  Phase 1 existed and would most likely be re-decided when the work starts; the
  loss is that Phase 2 begins from constraints rather than from a plan.
