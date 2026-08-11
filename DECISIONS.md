# `pisafe` decisions

Choices that still constrain the project: what was decided, what was not taken,
why, and — where undoing it would be costly — what reversal would take. Entries
that became plain descriptions of shipped code, or that a later decision
reversed, have been dropped; git history holds them.

A few constrain the design rather than the implementation — the static broker
port, no firewall-mutation privilege, the lifetime of a run's record, no user
credentials in the sandbox — and are stated as rules in
[`pisafe-design.md`](pisafe-design.md).

## VM boundary and firewall

- `/var/lib/pisafe` sits on a Lima disk of its own, so deleting the instance
  leaves every run filesystem, every project's transcripts, and the profile
  behind. Reversing after runs exist means copying them off the disk.
- That disk is found by filesystem label, never by device path — the cidata ISO
  takes a virtio slot too — and formatted only when exactly one whole device
  carries no partition table and no filesystem signature, because formatting the
  wrong one costs the instance. `pisafe-storage` also refuses to act unless
  `/var/lib/pisafe` is a mountpoint, so a boot that failed to mount cannot fill
  the instance's disk with filesystems the next instance will not inherit.
- All three firewall chains accept `ct state established,related`, because
  sshd's SYN-ACK from the broker address carries the client's ephemeral port and
  was rejected by the TEST-NET deny. Per-flow return rules were rejected: they
  recreate this bug for every future accepted flow.
- The VM user gets no firewall-mutation privilege, and the relay port is the
  static exception `192.0.2.1:18080`; a syntax-valid refresh helper would still
  let an escaped process swap the LAN set for an unrelated valid prefix. Changing
  the port or address requires VM recreation.
- The boundary records are verified before a command that may start a run or
  fetch over the network, not before one that only reaches what is there.
  Verifying everywhere looked safer and is the one rejected: the only cure for a
  failed check deletes every run's storage, so the check destroyed exactly the
  work it was asked to protect. `restore` is not exempt because it installs over
  the network; `backup` is, being the half that runs against a stale VM.
- VM size is three constants outside the security-profile digest: it bounds
  nothing a run may do, so changing it must not demand the recreation that
  destroys every project store.
- `pisafe vm rebuild` exists and the boundary checks name it, because the
  sequence is not one to assemble under pressure and nothing an unreachable VM
  does may refuse it. A VM predating the state disk is refused until
  `--discard-state` rather than covered by `--confirm`: the command a user
  reaches for out of an error message must not destroy every run's files.

## SSH, terminals, and Zed

- Per-run SSH is a portless `ProxyCommand` over a `podman exec` stdio relay, not
  a published VM-loopback port, which the firewall correctly denies to the Lima
  user; dynamic exceptions would add mutable privileged state. `sshd` is the
  container's main process, a detached one needing a second process-lifecycle
  mechanism across stop and resume.
- pisafe never edits the user's SSH configuration. An `Include` line in
  `~/.ssh/config` would be one idempotent edit serving every client, and was
  rejected: it makes every run alias resolvable to everything on the Mac, in the
  file users guard most.
- Zed's saved connections are spliced, never re-encoded — they are JSON with
  comments and a round-trip would delete the user's — and `pisafe zed` waits half
  a second after writing, because Zed applies a new connection's arguments at a
  measured 100 ms file-watcher delay and not at none.
- `sshd_config` renders `SetEnv` from the same declared list the container is
  given, also written as `.bash_profile` because Debian's `/etc/profile` assigns
  `PATH` outright. The hand-kept copy had dropped the session directory, so Pi
  wrote to its own nested default and no project ever got a transcript.
- Forwarding is `ssh -L` bounded by `PermitOpen 127.0.0.1:*`. Relaying each
  connection over a fresh `podman exec` would keep sshd's blanket refusal and
  reach older runs, but costs a process per TCP connection and a browser opens
  many; the capability is identical either way. Runs created before this cannot
  forward, sshd policy being written into the run's home at creation.

## Containers, image, and the guest helper

- Every container is rendered from one hardened base, with the network opt-in so
  a container that says nothing about it reaches nothing. Writing the prologue
  down once is what showed the SSH-host-key container had no memory or PID limit.
- Exactly one container may execute out of its scratch `/tmp`: the half of
  `packageArgs` that installs, because npm runs an unpacked tarball's own files.
  Resolve shared the base and carried the exemption for no other reason. Whether
  install still needs it is unrecorded; only a live install can say.
- Run data is one fixed-size 10 GiB sparse ext4 filesystem behind a narrow
  helper. Rootless Podman volumes and its XFS-only quota need root while the
  pinned image uses Btrfs, whose qgroups untrusted code can bypass with nested
  subvolumes. Reversing requires a storage migration.
- The guest helper is a sibling release artifact — a Go toolchain in the
  installed product or a committed binary were the alternatives — so the
  controller refuses one not carrying the exact call contract it was built with,
  which the helper prints as its usage error and so carries in its bytes. A call
  whose meaning changes while its name does not stays invisible to this, so such
  a change renames the call.
- The recipe digest reaches its label through a build argument declared after the
  toolchain layers: an argument in scope joins every later cache key, so
  declaring it first cost a full Debian, npm, and Python refetch on every helper
  change — 31.7 s measured against 0.5 s. Reversal is one line.
- Pi's tree is frozen by the shrinkwrap Pi publishes inside its own tarball,
  which the pinned digest covers; the three packages it names without an
  integrity hash are re-fetched by exact version against recorded digests,
  because npm re-resolved their ranges and installed 0.82.1. `npm ci` with our
  own lockfile was tested and rejected: npm reads a dependency's published
  shrinkwrap instead, so a corrupted *nested* integrity installs cleanly. The
  cost is three digests that must move with `PiVersion`.

## Records and lifecycle

- Manifests are versioned rather than migrated. Each bump added an invariant a
  compatibility path would have weakened; version 7 could not have one at all,
  recording digests taken from host files a staged run cannot recompute.
- What a superseded record costs is bounded by staying nameable: a run record
  this version cannot read is listed as `unreadable`, and `discard` releases
  everything it holds, because reclamation is keyed by the run ID in the
  filename. Anything concluding that nothing refers to a thing treats it as an
  unresolvable reference and waits. Reversing restores a state where one stale
  file disables `list`, `gc`, and every project command at once.
- A project record this version cannot read is reported and never acted on,
  unlike a run's: discarding a run record costs that run, while a project store
  holds transcripts nothing reproduces. `backup` copies them out under the key
  its file is named by and leaves the store out of the manifest, because a
  restore needs the checkout it came from — exactly what could not be read.
  Refusing the whole backup is the failure mode this rule exists to prevent;
  skipping silently loses the only copy.
- A `.json` file in the projects directory not named like a key is not a record
  and is skipped, since everything a caller may do with an unreadable record goes
  through its key. Run records differ deliberately: a run ID is what a user types.
- A run's record lives exactly as long as the run owns something. A terminal
  record kept forever restated the branch, base commit, author, and included
  files, all of which git holds; the one fact it added is in the reflog.
- Collection reclaims only imported runs. `diff` proves the repository is
  unchanged but sees nothing of the run's home, so "no commits" is not "nothing
  to lose", and deleting work the user never imported cannot be undone.
- Image pruning reads the label each image carries rather than resolving the
  recipe's ID: resolving cannot distinguish "no image for this recipe" from "the
  lookup failed", and that confusion deletes the image in use.
- A run whose container is gone is charged nothing, through a distinct `Abandon`
  transition. Charging the wall clock spends a whole budget on an outage the run
  did not cause; a VM-side heartbeat is machinery for a limit that exists to stop
  a runaway agent rather than to bill accurately.
- Runs receive eight cumulative active hours, enforced per interval by Podman's
  own `--timeout` so it outlives the controller; a daemon or VM-side timer would
  add a second trusted lifecycle service. Changing accounting semantics needs a
  manifest migration.
- Destructive confirmation is the repeated non-interactive form
  (`--confirm RUN`), identical in scripts and terminals; the same reasoning makes
  `cp --force` a flag rather than a prompt.

## Shared project state

- A project is keyed by its directory slug plus eight hex characters of the
  SHA-256 of the Git root path. The root-commit ID survives moves but makes two
  clones of one upstream share a store and gives an empty repository no key.
  Moving a repository orphans its store, which `project rebind` claims back.
- The key is one-way, so a Mac-side record written before the filesystem is
  allocated is what attributes a store; that ordering admits only a record whose
  store was never created. It also keeps attribution off the privileged helper,
  since enumerating the VM's stores needs a new privileged action.
- One privileged helper serves every scope, and it and its namespace list are
  hashed into the security profile — so **every helper change forces a VM
  recreation, which destroys every project store**. That is why such changes are
  batched, why `backup` exists, and why no later slice added a privileged action.
- Runs share project state through an overlay, not a shared read-write mount,
  which would make every package manager's concurrency correctness a property of
  the boundary. A full copy per run was rejected: run filesystems are ext4 with
  no reflink.
- Caches are keyed immutable snapshots, not directories merged back: presence or
  absence of a file carries tool-specific meaning no generic rule reconstructs.
  Snapshots buy whiteouts resolved by the kernel, no mounted lower ever changing,
  and an obvious unit of eviction, at a full copy per generation. One generation
  is kept per namespace — three, the CI convention, peaks at four copies, and a
  2.5 GiB npm tree fills a 10 GiB image alone — and both eviction and reset spare
  a generation a recorded run may still mount.
- The shared layer is disposable, not correct. npm's `_cacache` keys `index-v5`
  by the hash of the *request URL*, so two runs fetching one package write one
  path with different bytes, degrading into a re-fetch only because each index
  line carries its own checksum — which generalizes to no other tool. What holds
  for any toolchain is that a wrong cache costs time and never correctness, which
  is what obliges every shared scope to ship a reset.
- Cache namespaces are declared by the project in `.config/pisafe.json`: a
  per-tool table inside pisafe makes every new toolchain a pisafe release. JSON
  over TOML, which would be the first third-party package in a tool whose whole
  argument is boundary control. **The expensive decision here** — changing the
  format means migrating every repository that adopted it.
- The config schema is inert: literal relative paths, no execution, no
  caller-chosen mount path. Key files are opened through an `os.Root`, since a
  symlink out of the checkout would hash arbitrary host files; a missing one
  hashes as absent. The run image ID is mixed into every key.
- A declared cache may not bind a variable pisafe sets itself. An allowlist of
  good cache variables and a denylist of `PATH`-like ones were both rejected:
  what a cache carries into a later run of the *same* repository is that
  repository's business; what is pisafe's is that the session directory, the
  home, and npm's log path stay put.
- The snapshot a run resolved to is recorded and reused on resume, because a
  resumed run stacks an upper whose whiteouts were recorded against one lower.
- Publishing happens when a run stops, whatever the outcome, not on `apply`:
  under immutability a publish cannot damage what exists, stopping is the one
  point every ending passes through, and reclamation is a week too late.
  *Flagged as uncertain*: a run that executed hostile code can leave a snapshot a
  later run restores. The containment is that the later run is itself sandboxed,
  that lockfile integrity rejects a tampered tarball, and that the cache is
  disposable; publishing only on `apply` is a one-line fallback.
- Sessions use the overlay but not the cache mechanism, promotion being additive
  and unkeyed. It writes into the mounted lower, which overlayfs calls undefined:
  immutable generations would copy a project's whole history forward every run,
  and a seeded run-private directory costs that copy every run. Nothing existing
  is ever written, so the undefined part is bounded to whether a live run's
  listing shows the new names — which isolation requires it not to.
- A store whose checkout is missing starts a retention window rather than being
  released: the evidence is indistinguishable from an unplugged disk, while a
  project store is the one thing pisafe cannot reproduce. Presence is the path
  existing, not being a Git repository, since a broken git would otherwise report
  every project orphaned at once.

## The global profile

- `settings.json` and `trust.json` are copied into the run, not mounted, because
  Pi writes both and a read-only mount would turn ordinary use into an I/O error.
  The copy dies with the run.
- The profile mounts at a neutral path and Pi's package store stays writable in
  the run's home. Mounting the profile *at* the package store was tried first, on
  the reasoning that one read-only mount states the property directly; what that
  missed is where a blocked agent goes next — told `EROFS`, it reran
  `pi install -l` and committed the package into the repository, which reached
  the Mac through `apply`. *Reverses an earlier decision on evidence.*
- A run reaches a profile package by absolute path, not as an `npm:` source: both
  load, but `npm:` makes Pi re-check the version at every startup and install
  when it disagrees, so a read-only store turns a disagreement into a broken run
  start. It also keeps profile packages out of `pi update --extensions`.
- A run's own workspace is trusted without asking. *Flagged as a judgement call.*
  The guard protects nothing the container does not already contain, while
  leaving it unanswered costs a prompt every run and silently drops project
  settings non-interactively. Consequence: a hostile repository's
  `.pi/settings.json` loads and can pull its own packages inside the run.
- An install is two containers with pisafe holding the pin between them. One
  container reporting both was rejected: the pin would be a claim by the same
  process that produced what it describes. Only npm sources are installable, at
  an exact version resolved once and recorded — a git source has no integrity
  hash, a local path names something inside a container the user cannot see — and
  install scripts do not run, so a package needing a build step is not
  installable.
- The container mount check guards starting a container, not teardown; identity
  is proved everywhere. Checking mounts before teardown stranded every run whose
  container predated the profile mount moving, since `apply` stops first;
  special-casing the old destination would leave the same trap next time.
- The update offer is made when a run stops, so no run start reaches npm at all,
  and once per change rather than at every stop — a channel that repeats what the
  reader declined is one they stop reading. Applying re-resolves through the same
  fetch-and-verify path, so an offer can go stale harmlessly. pisafe never orders
  versions; it reports that the registry differs from the pin.
- `PATH` is a predictability default, not a boundary — anything in a run can
  prepend to its own. What holds is that the profile mounts read-only and its
  contents are pinned, at any position. The image comes first so an installed
  tool never decides what `git` means.

## Staging and apply

- Untracked inputs are chosen with repeatable `--include`/`--include-unsafe`
  flags rather than a picker, keeping `pisafe run` scriptable and making approval
  of a credential-shaped name deliberate. Those names match on whole words plus a
  fixed list, since substring matching produced false positives that would train
  the user to reach for the unsafe flag by habit.
- What a run leaves behind is listed with `--directory`, and a request naming an
  untracked directory is expanded by walking it. Enumerating every file cost
  0.4 s of each run start across three walks of 21 127 ignored files and reported
  "21 115 more" instead of `node_modules/`; the expansion keeps the credential
  check and per-file limits looking at one file at a time. Selection resolves
  against that one listing and what it resolves is what the stage archives, so
  the report and the archive cannot describe different states.
- An included path crosses as files in both directions and never enters Git.
  Committing it into the baseline was simpler and is what the code did, but made
  the run's history depend on content the user deliberately had not committed,
  and could not combine with `--drop-baseline` at all.
- What `--include` records is the path named, not the file list it resolved to:
  re-resolving from the command line at apply time uses host paths that may be
  gone. This is what makes `--include DIR` meaningful for an empty directory, and
  what apply re-expands. The roots are persisted, so changing the shape costs a
  manifest version.
- Apply writes files into the working tree, but only under included paths — a
  deliberate exception, since the alternative silently loses work created there.
  Copy-back is additive and refuses as a whole: mirroring deletions would let a
  sandboxed run delete the user's files, and a partial copy leaves the tree in a
  state neither side described. It runs after the refs are committed, because as
  a precondition a conflict over a scratch file would block importing real work.
- A submodule is staged from its checked-out HEAD, not the recorded gitlink,
  which may be unreachable and would silently discard a submodule the user moved;
  the superproject patch therefore ignores submodules so gitlink changes travel
  once. Nested submodules fail closed: recursion multiplies the artifact,
  path-safety, and journal surface.
- A run that attaches a submodule of its own has its whole apply refused rather
  than landing the branch with a warning: the branch is unusable exactly where
  submodules matter, and nothing on the Mac can be told where to fetch the
  missing repository from. Refusing happens before any ref moves.
- Every submodule pointer the run moved must name a commit the repository at that
  path holds — presence, not reachability, since moving a pin to a commit the
  submodule already has is ordinary and reachability would refuse a downgrade.
  For a staged submodule it cannot currently fail and is kept anyway: a property
  holding by coincidence of two rules elsewhere is how it stops holding silently.
- The apply journal records only ref creations and no ref names — both refs
  follow from the run ID, so a tampered manifest cannot name a ref the run never
  earned. Submodule refs commit first, since the reverse could leave a branch
  whose gitlinks nothing keeps reachable.
- The incoming ref is scratch, not a lock, so bundles fetch into it with a forced
  refspec. Left alone, a leftover from a killed import becomes a precondition and
  refuses the next apply for a history that was amended rather than for anything
  wrong; rolling back a part-built journal cannot establish this alone, because
  no cleanup runs after a crash. Nothing is given up: a recorded journal is
  always finished rather than redone, so no forced fetch lands on a pending apply.
- A prepared apply carries hashes and fixed artifact names, never filesystem
  paths, so a compromised run cannot name a file on the Mac. Apply captures in a
  throwaway container over the run's workspace rather than exec-ing into the live
  one, and uses the controller's current image, because the helper that captures
  must match the controller that reads.
- A run commits as the identity Git would use in the source repository. A neutral
  `pisafe` author misattributes the user's work, and an unconfigured run made
  every agent commit fail; a repository with no identity refuses to start rather
  than falling back to a placeholder found when rewriting is expensive.
- The keep-or-drop baseline question is asked interactively, unlike every other
  choice: a run is imported once, so a default would settle it for a user who
  never learned it was asked. The replay rebases in a throwaway worktree rather
  than the run's own branch, which would destroy the alternative if the apply
  then failed for another reason; a conflict is an answer, not a failure.
- The Mac verifies the drop rather than trusting the run, the baseline existing
  only inside it. A `merge-base --is-ancestor` failing for any reason but exit
  code 1 is an unanswered question, not a "no" — conflating them let a git
  failure read as "the baseline was dropped".

## Getting work in and out

- `pisafe diff` reports subjects, paths, and line counts, never content:
  everything in a run is untrusted, and writing it to the terminal is the
  injection surface pisafe exists to remove. It measures from the baseline, so
  carried-in dirty state is not attributed to the agent and never appears.
- `pisafe cp` copies only regular files and directories. A symlink stops the copy
  rather than being recreated: it resolves against a filesystem the run never
  saw, and a copy out is a leaf operation with nothing later to catch a wrong
  target. Naming a narrower path is the way around it.
- A run's name may be omitted, resolving to the one unimported run of the current
  checkout by the project key the record carries. Imported runs are excluded
  because they accumulate for a week; two live runs are reported rather than
  resolved by recency, which would be right most of the time — the wrong property
  for `stop` and `apply`.
- `cp` gained an inward direction rather than a second command, the colon already
  marking which end was in the run, and the archive is produced on the Mac so the
  run has no say in what arrives. A credential-shaped name needs `--unsafe` going
  in and nothing coming out: this is the frictionless way to put a reusable
  credential inside a run, the one act that voids what a run guarantees.
- Copying in reaches runs created before the command existed, because inspections
  run the current image. The general rule: anything carried by an inspection
  container reaches existing runs, anything written into the run's home does not.

## Inference broker and provider logins

- The ChatGPT OAuth flow is reimplemented in Go from the pinned client's
  constants, not run through Node: the controller is dependency-free and the Mac
  has no pinned Node runtime. Upstream flow changes must be re-mirrored when the
  Pi pin moves.
- Tokens persist in the login keychain through `security` over stdin,
  base64-wrapped. cgo bindings and a broker-only encrypted file were rejected for
  a dependency-free build with at-rest encryption and user-visible audit; argv
  was rejected because it is visible to every local process.
- The run capability is wrapped in an unsigned JWT holding a placeholder account
  ID, because the pinned client refuses an apiKey that does not parse as one.
  Translating between the Responses API and the Codex backend was rejected: body
  rewriting would own streaming and tool-call fidelity.
- The run-side catalog is embedded from the pinned Pi data with per-model routing
  fields stripped, so no entry can route a run around the broker. Live refresh
  was rejected; the catalog moves with the Pi pin in the same commit.
- A run sees every configured upstream, not one active one. Letting the newest
  login win was much smaller and was rejected: what a user wants from a second
  provider is to choose per task, which an active-provider switch makes a command
  plus a run restart.
- The provider's name leads its relay path, matched exactly. Routing on the model
  ID in the body was rejected outright: it would make the relay parse what a run
  sends, which is the one thing it never does.
- A built-in provider's login records only its name; the table wins over the
  record. Self-describing records were rejected because a release that adds a
  model would never reach a key stored months earlier — and a record hand-edited
  to point a known name elsewhere is ignored rather than obeyed.
- A custom endpoint declares its models rather than being discovered, since
  `/v1/models` returns identifiers and nothing else and a wrong context window is
  a real failure. Plain HTTP is refused except on loopback, which is what makes a
  local model server first-class. An API pisafe does not know has no canonical
  path and configures no run, rather than falling through to `/v1/responses` and
  relaying an unrecognised wire format under a shape it does not speak.
- A stored secret is read only when a request is relayed; the catalog asks the
  keychain only whether an item exists, so run creation touches no secret at all.
  The cost is that a credential that no longer parses is reported when the broker
  starts rather than by refusing a run that would not have used it.
- The guest verb was renamed when the configuration document changed shape, so an
  older helper fails the unknown verb loudly instead of writing the new document
  whole into `models.json` and leaving Pi with no providers. *Runs created before
  that change cannot be resumed; they can still be diffed, applied, discarded.*

## Naming, emptying, and exporting state

- A project store is named by its checkout path, never by its key: the key is a
  digest nobody recognises, while the path is the only handle a user has for a
  store whose directory is gone. A path that no longer resolves is taken as it
  stands, which is what lets such a store be dropped. `project list` reports no
  sizes — enumerating the storage roots needs a new privileged action.
- A rebind copies the transcripts into a fresh store instead of renaming the
  filesystem. Renaming is what the operation is, and would be a new helper
  action — forcing the VM recreation that destroys every project store, the loss
  rebind exists to prevent. It leaves the caches, a full copy risking exhausting
  space partway through an operation whose purpose is losing nothing.
- A backup only ever adds, in both directions. The rsync semantic would mean that
  dropping a project and then backing up destroys the last copy of its
  transcripts, precisely the disaster a backup exists to prevent. It is a
  directory rather than an archive, which is what makes a repeat additive.
- A restore installs from the backed-up pin rather than resolving the name again,
  making the *bytes* the thing verified; a release republished under the same
  version fails. A package already installed is skipped, the backup's older pin
  being a silent downgrade — a restore puts back what was lost, it does not make
  a profile identical to a backup.
- Transcripts fail a restore, packages do not. A transcript has no second source,
  so a failure there is the VM's storage and every project after it would fail
  the same way; npm is a third party, so those failures are reported at the end.
  Outward, a transcript name the Mac will not write is refused and counted, since
  one run must not block the backup of everything else.

## Documentation

- Three documents with distinct jobs: [`pisafe-design.md`](pisafe-design.md) is
  the authority on what must hold,
  [`IMPLEMENTATION_PROGRESS.md`](IMPLEMENTATION_PROGRESS.md) on what currently
  happens and has been verified, and this file on why. One combined document was
  not retained: the design had grown to 1000 lines, a third of it history, and
  was unreadable in one sitting. The design states requirements, not mechanisms
  the implementation documents more precisely.
