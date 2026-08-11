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
  user's SSH configuration. `pisafe zed` does write Zed's list of saved
  connections, because Zed passes `ssh` nothing but what such an entry carries
  and a run's alias resolves only through that fragment. The alternative, one
  `Include` line in `~/.ssh/config`, was rejected although it is a single
  idempotent edit that would serve every SSH client at once: it makes every run
  alias resolvable to everything on the Mac, and the file it edits is the one
  users guard most.
- Zed's settings are edited by splicing the one value that changes, never by
  re-encoding the file: they are JSON with comments, and a round-trip through an
  encoder would delete the user's. An entry whose host is already saved is left
  exactly as found, so what Zed records in it stays the user's.
- A run's saved connection is written when `pisafe zed` opens the run and
  removed when `discard` or `gc` reclaims it, giving it the lifetime of the SSH
  fragment it points at. Writing it at `pisafe run` instead would put an entry
  in Zed's settings for people who never open Zed.
- `pisafe zed` waits half a second after saving a connection before handing the
  run to Zed. Zed rereads its settings from a file watcher and offers nothing to
  synchronize against, and without the wait the first open of a run reaches
  `ssh` under an unresolvable host name — measured, not assumed: Zed applies a
  new connection's arguments at a 100ms delay and not at none. Removing the wait
  brings that first-open failure back.
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
- A command given to `connect` is left in the remote shell rather than `exec`ed
  by it. The `exec` was an optimization — one process fewer, signals reaching
  the command directly — but it silently truncated `pisafe connect -- 'a; b'`
  to its first command and refused a variable assignment outright, both of
  which the documented promise that the run's own shell parses the words says
  should work. An interactive shell is still `exec`ed, because it is one
  command and it is the whole session.
- `connect` opens a shell by default and takes an optional `-- COMMAND`, where
  it used to start Pi by default and open a shell under `--shell`. Keeping Pi
  as the default and adding a `--pi` flag was the alternative. The two are not
  symmetric: the remote command is the whole session, so quitting Pi ends the
  SSH session and reaching a shell costs a reconnection, while from a shell Pi
  is a child and quitting it returns to the run's prompt. The shell default
  reaches both states and the Pi default reaches one. It also matches what
  every other command treats a run as — a machine that is worked in, whose
  ports `forward` exposes and whose files `cp` moves — rather than a wrapper
  around one program. The cost is that a bare prompt does not announce the
  agent, paid with one printed line naming `pi`. Reversible: one function and
  its help text.
- A command after `--` is joined with spaces and parsed by the run's own shell,
  not quoted word by word. Quoting each word is safer against surprise splitting
  but would send the run a program named `cat > file`, and redirects and pipes
  are most of why the general form is worth having. This is what `ssh` itself
  does with a remote command, so the surprising behaviour is at least the
  familiar one. Only the workspace path is quoted, because pisafe supplies it:
  it is derived from the run's validated project name, never read back as a
  string the record could aim elsewhere.
- A pty is requested only when stdin and stdout are both terminals. Always
  asking corrupts a redirected copy through CRLF translation; never asking with
  a command, which is `ssh`'s own rule, breaks `-- pi`. Testing both ends serves
  both without a flag, and is the same test `login` already applies.
- Reaching a server a run hosts is an `ssh -L` channel on the run's own
  connection, which cost the run's `sshd` its blanket `AllowTcpForwarding no`
  and its key its `no-port-forwarding` option. Both are replaced by the exact
  grant: local forwarding only, bounded by `PermitOpen 127.0.0.1:*`. Relaying
  each connection over a fresh `podman exec`, as the SSH `ProxyCommand` does,
  was the alternative and would have kept those two settings absolute and worked
  on runs created before this — but it spawns an ssh client, a `podman exec`,
  and a helper per TCP connection, and a browser opens many, so a dev server
  would have paid a process and a container exec for every one. The capability
  is the same either way: whoever holds the run's key can already open arbitrary
  stdio into the container, and only this Mac holds it. The cost is that a run
  created before this change cannot forward, because its `sshd_config` and
  `authorized_keys` are written once at creation and survive stop and resume.
- The forward is per-invocation and ends with the command rather than being a
  property of a run. It is the one thing that puts run-authored content in the
  user's browser at a loopback origin, where it can reach other services on this
  Mac's loopback subject only to what the browser enforces. Making it a
  long-lived per-run setting was not taken: the exposure should last as long as
  the user is looking, and no longer.
- A run's SSH config keeps `ClearAllForwardings yes`, and `pisafe forward`
  overrides it on the command line. Removing it from the config would have been
  simpler but would let any forwarding request through `connect`, `zed`, or a
  hand-written `ssh` reach the run silently; leaving it means a forward exists
  only where one was asked for by name.
- Lima's default VZ user-mode network remains in the generated profile. Native
  `vzNAT` was tested but exhibited the same stopped-VM SSH recovery failure and
  made its Mac-side interface appear only after the immutable host-network
  profile was captured; QEMU would add a host dependency. Changing this
  requires VM recreation.
- How large the VM is — its CPUs, memory, and disk — is three constants, not a
  configurable option. They were carried as a `ConfigOptions` struct with one
  default constructor and no second caller, which cost a validator for values
  nothing could make invalid. Sizing stays deliberately outside the security
  profile digest: it bounds nothing a run may do, since a run is bounded by its
  container's limits and its own filesystem, so changing it must not demand the
  recreation that destroys every project store.
- The run-image Containerfile is embedded in the controller while the static
  Linux ARM64 guest helper is a sibling release artifact. Building the helper
  at runtime would require a Go toolchain in the installed product; checking a
  binary into Git would make history unreviewable. Changing the layout changes
  the managed recipe digest.
- Because the helper is a separate artifact, the controller refuses one that
  does not carry the exact call contract it was itself built with: a
  compile-time constant naming every call and its arguments, which the helper
  prints as its usage error and therefore carries in its bytes. A hand-bumped
  version constant was not taken, because forgetting to bump it is the same
  forgetting that leaves a helper unbuilt; a VCS or build-time stamp was not
  taken either, because two builds from one dirty tree carry the same commit,
  which is exactly the case that skews during development. Renaming a call, or
  changing what it takes, now costs a clear failure before a run exists rather
  than a usage error from the guest halfway through creating one. A call whose
  meaning changes while its name does not stays invisible to this check, so such
  a change renames the call.
- The recipe digest reaches its label through a build argument declared after
  the toolchain layers instead of at the top of the Containerfile. An argument
  in scope joins the cache key of every instruction after it, and this one
  changes whenever the helper does, so declaring it first cost a full Debian,
  npm, and Python refetch — 31.7 s measured, against 0.5 s once the layers are
  reused — for a change none of them depend on. Rebuilding the whole image on
  every recipe change was not kept: what the layers install is decided by the
  Containerfile text and the digest-pinned base image, and Podman already
  invalidates them when either moves. The cost is that floating Debian packages
  are refetched only when the Containerfile or the base image changes, rather
  than incidentally on every helper rebuild. Reversal is one line.
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
- The firewall's DNS test resolves names through a public wildcard resolver
  that encodes an address in the name, so the container acts on a real answer
  pointing into a denied range. Seeding `/etc/hosts` with `--add-host` was not
  taken: it is deterministic but never asks a resolver anything, which is the
  half of the path the gap was about. The test requires the answer before
  asserting the denial, so an unreachable resolver fails loudly instead of
  passing as a refusal — at the price of a live test that depends on a third
  party staying up.
- That the broker port is the exception rather than the broker address is
  established across two tests: the shaped-traffic test shows every other port
  on `192.0.2.1` refused, and the relay test shows `18080` served. Binding a
  stand-in listener inside the one test was not taken, because it would race a
  real broker for the port and fail for a reason unrelated to the boundary.
- The security-profile and firewall records are verified before a command that
  may start a run or fetch over the network, and not before one that only
  reaches what is already there. `diff`, `cp`, and `apply` read or write a run's
  workspace through a container with no network, no home, and none of the shared
  profile; `stop` and `discard` take a run's claim on the VM away; `extension
  list`, `extension remove`, `tool list`, `tool remove`, `profile reset`, and
  `backup` read the profile or empty it. Neither record bears on any of them,
  while a VM that fails either check has one cure — recreating it — that deletes
  every run's storage. Verifying everywhere was the safer-looking option and is
  the one rejected: it destroyed exactly the work it was asked to protect, since
  a stale VM could not hand a run's commits back and the instruction it printed
  was the one that would lose them, and it refused to stop a run left alive
  under the very boundary the failed check called untrustworthy.
- Writing to the profile does not by itself require a verified boundary; only
  fetching does. `extension remove`, `tool remove`, and `profile reset` rewrite
  what every later run mounts, exactly as `restore` does, and they are exempt
  where `restore` is not: what separates them is that restore is the one command
  that installs packages over the network, in a `--network=pasta` container the
  deny set governs. Holding removal to the records was rejected because it made
  a VM under suspicion the one VM whose extensions could not be taken back out.
- `backup` is exempt and `restore` is not, although they are one pair. Backup is
  the only half that ever runs against a stale VM: restore's target is the VM
  that has just replaced it, so the exemption would buy it nothing. Exempting
  neither was rejected because it left recreation, the only cure a stale VM has,
  with no way to save the transcripts nothing can refetch.
- Every command that reaches the profile without fetching starts the VM it needs
  rather than failing on a stopped one. Reporting "start the VM first" was
  rejected: each of these commands is a single VM contact whose whole purpose is
  the profile, so the instruction would only ever be the step the user then
  performs. The four that install go on reaching the VM through the image path,
  which has already brought it up verified.
- The run's sshd renders its `SetEnv` from the same list the container is given
  rather than from a second one kept by hand. The hand-kept copy carried three
  of the eight declared variables, and the one it dropped was
  `PI_CODING_AGENT_SESSION_DIR`: declared on a container whose main process is
  sshd, and absent from every session a user reaches, since sshd builds a
  session environment from scratch. Pi therefore never saw the relocation and
  fell back to its default layout in the run's own home — nested one directory
  per workspace, where promotion's flat glob would not have matched it either —
  so `/sessions` stayed empty, no project ever received a transcript, and the
  only visible symptom was a store that held nothing. A test holds the rendering
  to the whole declared list, and to values free of the whitespace one `SetEnv`
  directive cannot carry. `ContainerHome` is exported for the same reason the
  list is shared: the guest was keeping its own copy of that path too.
- The same declared list is also written into the run's home as `.bash_profile`,
  because sshd setting a variable does not settle what a shell has: Debian's
  `/etc/profile` assigns `PATH` outright for every login shell, and an SSH
  session with a terminal is one, so `pisafe tool install` put a command on a
  PATH that no prompt ever saw. A `/etc/profile.d` drop-in in the run image was
  the other candidate and was rejected: the value would have to be baked at
  build time, which is a second copy of the list again and a build argument that
  moves whenever the list does. The home is a mount over the image's own, so
  nothing is displaced, and the file sources `.bashrc` for the same reason
  Debian's own `.profile` does. It is in writable storage, where a run can edit
  it — which costs nothing, since a run already chooses its own `PATH`, and what
  keeps the tools read-only is the mount rather than the variable.
- Promotion keeps its flat `*.jsonl` glob. Pi nests transcripts one directory
  per working directory only in its default location; a relocated session
  directory is flat, which is what the store is, and reading one back filters by
  each transcript's own recorded cwd. Mirroring the nesting was written and
  rejected on the evidence: it matched what the broken runs had produced, not
  what a run with the variable set produces.
- Every container pisafe starts is rendered from one hardened base rather than
  from five hand-copied prologues: unprivileged user, read-only root, no
  capabilities, no new privileges, and bounded memory and PIDs. The network is
  opt-in, so a container that says nothing about it reaches nothing, and a
  container added later inherits all of it instead of restating it. Writing the
  prologue down once is what showed that the container generating a run's SSH
  host key had no memory or PID limit at all; it has the run's now. One
  difference survives and is not understood: the container that fetches and
  unpacks an npm tarball mounts its scratch `/tmp` without `noexec` where every
  other container has it, and whether npm needs that is unverified, so making
  the base uniformly strict is backlog work behind a live install rather than a
  silent change inside a deduplication.
- The Mac's on-link deny set is canonicalized in exactly one place. `hostnet`
  reports the prefixes it observed; `lima.CanonicalIPv4Prefixes` masks,
  deduplicates, orders, and drops any prefix another already covers, and the VM
  definition, its security-profile digest, and the check against a running VM
  all read the set from there. `hostnet` doing half of it as well was the
  previous arrangement: two functions that agreed, with nothing making them go
  on agreeing. The digest therefore depends on the canonical set alone, which
  was verified by diffing rendered configs and digests across the change,
  including the collapsed-versus-raw pair that could otherwise have made every
  running VM read as stale.
- `lima.VM` is the one handle on the instance. `Manager` and `Transport` were
  the same `{instance, runner}` value with different method sets, and four
  commands built both to talk to one VM; nothing recorded why they were split.
  Keeping either name was rejected — one would have named instance creation
  after a transport, the other is a nothing-word — at the cost of a large
  mechanical rename.
- `guestcall` owns what a document crossing the controller/helper boundary is,
  beside the names of the calls that carry them: bounded, decoded whole, unknown
  fields and trailing data refused. The two binaries had a copy each.
  `runctl.inspectContainer` is deliberately outside it: it reads Podman's own
  inspection output, which must accept unknown fields, and folding it in would
  have put a flag at every call site deciding which kind of document it is.
- Backing up mounts each project's filesystem before archiving it. Reusing
  `EnsureProjectStorage` can create a store for a record whose filesystem is
  gone, which a verify-only helper call would instead refuse; creating was
  taken because the empty store is what the project's next run makes anyway,
  while refusing would fail a whole backup over one project that has nothing
  to back up.

## Storage and lifecycle

- `/var/lib/pisafe` sits on a Lima disk of its own rather than on the instance's
  disk, so deleting the instance leaves every run's filesystem, every project's
  transcripts, and the profile in place for the next one to mount back. The
  alternative considered was reducing how often recreation is demanded, by
  taking the Mac's on-link prefixes out of the boundary records; it was rejected
  as addressing one trigger out of many, since the config template is hashed too
  and so every release that touches the VM definition demands the same
  recreation. Nothing about a run's storage changes: the same loop-mounted
  images, quotas, ownership, and `nodev,nosuid` mounts, on a different disk.
  Reversing this after runs exist means copying them back off the disk. It also
  retires the reason recorded above for exempting `backup` from the boundary
  checks: the exemption stands because a stale VM should still hand back what it
  holds, not because recreating it was the only way out. What backup and restore
  are for narrows to a second Mac, or a state disk lost rather than an instance
  replaced.
- A run whose container is gone is charged nothing for the stretch it was
  active. The alternatives were charging the wall clock, which is what the code
  did and which spends a whole eight-hour budget on an outage the run did not
  cause, and having the VM leave a heartbeat the record could bill against. The
  heartbeat was rejected as machinery bought for a limit that exists to stop a
  runaway agent rather than to bill anyone accurately, and the generosity is
  safe because nothing inside a run can bring its own container down. This is a
  distinct store transition, `Abandon`, rather than a `Stop` with a chosen end
  time: `Activate`, `Resume`, and `Stop` all read a zero timestamp as "now", and
  giving `Stop` alone a second reading of it would be one name for two ideas.
- `connect`, `zed`, and `forward` bring back a run the VM stopped rather than
  reporting it. Which side stopped it is the whole rule: a run the user stopped
  stays stopped, because resuming spends a budget and is theirs to ask for,
  while a run the VM stopped was nobody's decision and the record pisafe printed
  as active is a claim to make true. Reporting it and naming `pisafe resume` was
  rejected as leaving the user to carry out the fix pisafe had already
  diagnosed. The three ask the VM without starting it and resume through the
  same verified boundary `pisafe resume` uses, so a command that enters a
  container never boots a VM under a weaker profile: booting only happens on the
  path that is starting a run.
- The check is ordered so the common case pays for nothing else: a Lima status
  query and one `podman ps` settle it in about 150 ms measured, and the
  controller — with the Keychain read every other run-touching command already
  does — is built only on the path that resumes, where a VM is booting anyway.
- The wall-clock reading moved behind that check, and `pisafe list` gained the
  same ordering. A deadline in the past used to mean one thing; since an outage
  no longer charges a run, it can also mean the Mac was off, and neither the old
  refusal nor the old `(limit reached)` label could tell those apart. A VM that
  cannot be asked now prints neither reading rather than guessing.
- A run is active only while its container runs, so `resume` settles a record
  that claims one the VM no longer has instead of refusing. A container that
  vanished with a rebooted or recreated VM and one that exited at its own
  deadline are deliberately the same case, differing only in how much of the
  stretch was observed; a container that is still running keeps the refusal,
  because resuming would restart an agent mid-work.
- `pisafe vm rebuild` replaces the instance, and the boundary checks name it
  instead of telling the user to recreate the VM. Leaving it as prose was
  rejected because the sequence is not one to assemble under pressure: an active
  run has to be stopped before the container carrying its only account of the
  stretch goes, a broken instance has to be killed rather than waited on, and
  the lock that kill leaves on the state disk has to be released or the
  replacement is refused the work it exists to keep.
- Nothing an unreachable VM does can refuse the rebuild. Stopping active runs is
  best-effort and reports what it could not do, because a VM too broken to
  answer is exactly why a rebuild was asked for; a run left active is settled by
  the next command that reaches for it, charged nothing. Failing the rebuild on
  an unstoppable run was rejected as withholding the cure from the only VM that
  needs it.
- Named no flag, the command reports what the rebuild costs and changes nothing,
  following `extension update`. A VM that predates the state disk is refused
  outright until `--discard-state` acknowledges the loss, rather than being
  covered by `--confirm` alone: the command a user reaches for straight out of
  an error message must not be the one that destroys every run's files, every
  project's transcripts, and the profile.
- Migrating that VM's `/var/lib/pisafe` onto a new state disk was considered and
  not built. Lima cannot attach a disk to a running instance, so it would mean
  streaming the whole tree — including sparse multi-gigabyte run images — out to
  the Mac and back. `pisafe backup` and `restore` already carry what nothing can
  refetch, and this case ends the first time anyone rebuilds.
- `--discard-state` also discards the run records, through the same `Discard`
  every run takes. Leaving them was rejected as reintroducing exactly the
  dishonesty `pisafe list` was just fixed for: after that rebuild every record
  names a workspace, an SSH key, and a saved Zed connection that are gone.
  Imported runs keep their `pisafe/RUN` branches, which discard never touches.
- The rebuild builds the run image before returning. Leaving it to the next
  `pisafe run` was rejected because the image lives on the instance's disk and
  so goes with it every time: deferring it only moves several minutes into a
  command that did not ask for them.
- A state Lima will call neither running nor stopped is now `StatusBroken`
  rather than an error out of `Status`. As an error it refused the rebuild on
  the instance most likely to need one. Nothing gained a reason to start such an
  instance: `bringUp` still refuses it and now names the rebuild, and
  `runningRuns` treats it as unanswerable rather than as holding no container,
  so a stale `active` record is never settled against a VM that may still be
  running the container.
- The test double `errorRunner` was folded into `fakeRunner`, which gained the
  same positional `errors` slice. Two runners differing only in whether they
  record calls is one idea under two names, and the rebuild path needs both.
- The state disk is found by filesystem label, and formatted only when no device
  carries one. Identifying it by device path was rejected outright — the cidata
  ISO takes a virtio slot too, and boot decides the order. The candidate for a
  first-ever format is a whole device carrying neither a partition table nor any
  filesystem signature, and provisioning fails the boot unless there is exactly
  one: the cost of formatting the wrong device is the instance, so a VM this
  rule does not recognise must stop rather than guess.
- `resume` now installs the managed run image before rebuilding a container,
  which only `run` and the inspection commands did. The image store is on the
  instance's disk and a run's storage is not, so the two now have different
  lifetimes and a recreated VM held storage it could not start anything over. A
  run still resumes only onto the image its manifest pinned: an image that has
  changed since fails, which is the pinning working rather than a gap.
- `pisafe-storage` refuses to act unless `/var/lib/pisafe` is a mountpoint. Boot
  ordering alone would have been enough in practice, and was rejected as the
  only guard: a boot that silently failed to mount would otherwise fill the
  instance's disk with filesystems the next instance cannot inherit, and nothing
  would report anything missing until the instance was deleted.
- The digest now covers the broker address and port, which were substituted into
  the template but never hashed, contradicting the rule stated above the
  function. Left alone they would let a changed broker exception produce a
  profile matching an older one. Bundled here because the digest's input shape
  was already changing, which is what the version tag bump records.
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
  accounting and deadlines durable, version 4 bound the inference capability to
  the active state, version 5 required the project key a run's shared storage is
  reached through, version 6 recorded the cache generation the run resolved to,
  and version 7 recorded a size and digest per included file rather than a bare
  path. A compatibility path would each time have weakened the invariant being
  added or inferred untrustworthy history; version 7 could not have one at all,
  because the digests are taken from host files at staging time and a run
  already staged has none to compute them from.
- What a superseded record costs is bounded by staying nameable, not by a
  migration. A record this version cannot read is listed as unreadable with the
  reason, and `discard` releases everything it holds, because reclamation is
  keyed by the run ID the filename already carries. This is what makes
  versioning-without-migration honest now that released records exist: the
  version bump costs the run, never the storage behind it, and never the
  commands that would otherwise enumerate past it. Anything that concludes
  nothing refers to a thing — project reclamation, cache reset and eviction,
  project removal — treats such a record as a reference it cannot resolve and
  waits instead. Reversing this means restoring a state where one stale file
  disables `list`, `gc`, and every project command at once.
- A manifest records only what it cannot derive and something reads. The
  workspace path is `/work/<project>`, so it is computed from the project name —
  which is validated as an identifier, as every name the slug generator produces
  already was — rather than stored where a tampered record could aim it
  somewhere the derivation cannot reach. The container name was stored and read
  by nothing. Dropping both needed no manifest version: existing records hold
  exactly the value the derivation produces, and their now-unknown keys decode
  and are ignored.
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

- `internal/safefile` is the one implementation of "bounded on the way in, whole
  on the way out, never anything but a regular file". Five writers and two
  readers had said it slightly differently across `runstate`, `runssh`,
  `runimage`, `backup`, `lima`, and the guest. The survivor is the strictest of
  each variant, not the first one reached for: the reader rechecks the handle
  against the path it opened, and every writer goes through a temporary file in
  the same directory, fsyncs it, installs it by rename or by hard link, and
  fsyncs the directory — which three of the writers did not do.
- `runstart` stays a package of its own. Folding it into `runctl` would remove a
  package and a mirrored interface, but the split is what keeps host-Git staging
  and run-image sequencing out of the controller that drives containers, which
  is the boundary that package names.

## Shared project state

- A project is keyed by its directory slug plus eight hex characters of the
  SHA-256 of the Mac-side Git root path. Keying on the repository's root-commit
  ID was not taken: it survives moves, but two clones of one upstream would then
  share a cache and a session store, an empty repository has no key at all, and
  a multi-root history needs a tiebreak rule. The path hash is always available
  and always distinguishes two checkouts. Moving a repository therefore orphans
  its store rather than migrating it, which `pisafe project rebind` claims back.
- The key is one-way, so a filesystem cannot say which checkout it came from and
  no amount of looking at the VM would answer it. A Mac-side record per project
  is what attributes one, written before the filesystem is allocated: the only
  failure that ordering admits is a record whose store was never created, and
  removing one of those costs nothing. `runid.Project` carries the checkout it
  was made from so that registration sits beside allocation in the controller
  and no future caller can separate them. It also keeps attribution off the
  privileged helper — the storage roots are `711` and `700`, so enumerating the
  VM's stores would need a new privileged action and therefore a VM recreation.
- Runs share project state through an overlay, not a shared read-write mount.
  Sharing read-write would make every package manager's concurrency correctness
  a property of the boundary: unverifiable by us, and fatal to a whole project's
  cache when one tool gets it wrong. A full copy per run was not taken either,
  because run filesystems are ext4 with no reflink, so the copy would be real.
- Caches are keyed immutable snapshots rather than directories merged back into
  the project store, which is what an earlier plan called for. Merging was
  abandoned because the presence or absence of a file carries tool-specific
  meaning — index files, stamp files, manifests — that no generic rule
  reconstructs. Every CI cache system converged on wholesale keyed snapshots for
  this reason; adopting their model costs a full copy per generation and buys
  three things: whiteouts are resolved by the kernel instead of interpreted by
  us, no mounted lower ever changes so concurrent runs need no coordination, and
  eviction has an obvious unit.
- The shared layer is disposable, not correct. An earlier claim that everything
  under a cache was content-addressed and therefore safe to merge
  last-writer-wins was checked and is false: npm's `_cacache` holds `content-v2`
  keyed by content but `index-v5` keyed by the hash of the *request URL*, so two
  runs fetching one package write one path with different bytes. It degrades
  into a re-fetch rather than corruption only because each index line carries
  its own checksum, which is npm being forgiving and generalizes to no other
  tool. The property that does hold for any toolchain is that a wrong or missing
  cache costs time and never correctness — which is what obliges every shared
  scope to ship a reset rather than defer one.
- The namespace directory is the restore prefix: all of a cache's snapshots live
  under `cache/<namespace>/`, so falling back means taking the newest entry
  there. Also writing each snapshot under a short shared key, as some CI
  configurations do, was not taken — that overwrites an entry a concurrent run
  may have mounted as its lower, reintroducing the one overlayfs hazard
  immutability otherwise eliminates.
- Cache namespaces are declared by the project, in `.config/pisafe.json` at the
  repository root. Hardcoding a per-tool table in pisafe was not taken: the
  knowledge belongs to whoever knows the project, and a fixed table makes every
  new toolchain a pisafe release. The file is JSON parsed with the standard
  library; TOML would be the friendlier authoring format and the first
  third-party package in a tool whose entire argument is boundary control, and
  that trade was declined. Reversibility: changing the format later means
  migrating every repository that adopted it, so this is the expensive decision
  in this section.
- The config schema is inert, because the file arrives from the repository and
  is parsed on the Mac before any sandbox exists. Key inputs are literal
  relative paths whose contents pisafe hashes; there is no command execution, no
  shell, and no caller-chosen mount path. Globs were deferred rather than
  refused — a monorepo will want `**/package-lock.json`, but resolving one
  raises symlink-escape questions a literal list does not, so it waits for the
  repository that needs it.
- A declared cache may not bind a variable pisafe sets itself, and the refused
  set is derived from the run's own environment rather than listed twice so the
  two cannot drift. An allowlist of known-good cache variables was not taken,
  and neither was a denylist of process-semantics variables like `PATH`: what a
  cache carries into a later run of the *same* repository is that repository's
  business, and is already true of any cache holding executable content. What is
  pisafe's business is that the session directory, the home, and npm's log path
  stay where pisafe put them, which is exactly the set refused.
- The declared key files are opened through an `os.Root`, because the paths come
  from the repository and are read on the Mac, so a symlink out of the checkout
  would let a declaration hash arbitrary host files. They are rejected lexically
  first, so a failure names the declaration rather than the filesystem. A
  missing key file hashes as absent rather than failing: a repository with no
  lockfile yet has no dependencies yet, which is a state and not an error. The
  key also includes the run image ID, for the same reason CI keys start with the
  runner OS, and the config does not control that part.
- The snapshot each run resolved to is recorded in its manifest, and resume
  reuses it rather than selecting again. A resumed run stacks its existing
  upper, with the whiteouts it recorded against one lower, so re-selecting could
  stack it on a newer generation that never held the files those whiteouts
  delete. Selection is therefore a project-store read separate from the
  run-store write that prepares the mount, which is what lets it happen before
  the run is recorded at all.
- Publishing happens when a run stops, regardless of its outcome, not on
  `apply`. Under immutability a publish cannot damage anything that exists, so
  gating it behind a deliberate act buys less than it costs in cache misses;
  stopping is also the one point every ending passes through and the last point
  at which the overlay can still be mounted. Publishing at reclamation was not
  taken: an applied run is reclaimed a week later, far too late to help. Two
  consequences. A stopped run that resumes and keeps working publishes nothing
  further under the same key, so its work is not lost but is not shared until
  the inputs move. And *flagged as uncertain*: a run that executed hostile code
  can leave a snapshot a later run restores. The containment is that the later
  run is itself sandboxed, that lockfile integrity checks reject a tampered
  tarball, and that the cache is disposable; if that proves wrong, publishing
  only on `apply` is a one-line change.
- A publish or promotion that fails is recorded on the run rather than returned
  as a stop failure, and the two are joined rather than sequenced so neither
  short-circuits the other. The run did stop and its workspace is intact;
  failing the command would misreport that, and `apply` and `discard` both stop
  a run on the way to something else and may not be aborted by a cache. `pisafe
  stop` prints the recorded warning and `pisafe list` marks the run.
- The merged view is read by a throwaway container and written only by pisafe:
  the container mounts the run's own overlay and streams a tar to standard
  output, which the VM-side script extracts under `podman unshare`. Bind
  mounting a staging directory into that container and copying inside it is one
  command shorter and was not taken, because no container ever gets a writable
  handle on a project store.
- One generation is kept per namespace, and reset empties a whole project's
  cache. Keeping three was the first choice, on the CI convention, and it was
  wrong for a fixed 10 GiB project filesystem shared by every namespace and the
  session store: publishing writes a full copy before eviction runs, so three
  kept generations peak at four copies and a 2.5 GiB npm tree fills the image on
  its own. One costs a peak of two and loses little, because the fallback only
  ever reads the newest — what three buys is an *exact* hit when a project
  alternates between a few input states, and the miss it becomes still restores
  a warm base. Reversible: it is one constant. Reset takes the whole cache
  rather than one namespace or generation, because the reason to reach for it is
  never "this one generation is wrong".
- Eviction and reset both spare a generation a recorded run may still mount.
  overlayfs leaves behaviour undefined when a mounted lower goes away, and the
  run manifests already record which generation each run resolved to, so the
  protected set is read from them rather than tracked separately. An active run
  has one mounted now and a stopped run remounts its own on resume; an imported
  run cannot resume, so it protects nothing.
- A generation is stamped when it is published, not when the run last touched
  it, because tar restores the merged view's own timestamp and recency is how a
  namespace is searched. A half-written one stages as a dot entry inside its own
  namespace, so neither selection nor eviction can see it and the rename into
  place stays within one directory on one filesystem; a separate staging tree
  would need its own cleanup path, while a leaked dot entry is swept by the
  reset that empties the namespace.
- pisafe creates the run-side upper and work directories, not the privileged
  helper, because the set of namespaces comes from the project config and cannot
  be known when the run filesystem is allocated. Everything the helper creates
  is owned by the mapped UID, so pisafe builds them under `podman unshare` with
  no new privilege. This retired the helper's static layer list. They live in a
  third run-filesystem subdirectory that is not bind-mounted into the container:
  putting them under the run home needs no helper change but exposes the raw
  upper, whiteouts and all, to the code whose writes it records.
- One privileged helper serves every scope. `pisafe-run-storage` became
  `pisafe-storage <action> <scope> <id>` with `run`, `project`, and `global`
  scopes rather than near-identical siblings; they differ only in root path,
  size policy, and subdirectory set. The helper and its namespace list are
  hashed into the VM's security profile, and its `verify` asserts each namespace
  exists rather than creating one, so every helper change forces a VM
  recreation — which destroys every project store, and is why such changes are
  batched and why nothing in a later slice added a privileged action.
- A project filesystem is ensured, never created, and never rolled back. Many
  runs of one project reach it and none may assume it is the first; a run that
  fails after ensuring it leaves it behind, because it is shared state that
  outlives every run. It is also ensured ahead of the manifest record, since it
  is the one allocation deliberately never undone and selection needs it.
- Shared mounts carry neither `nodev` nor `nosuid`, because Podman refuses any
  other option alongside `:O`. They are the only mounts in a run without them
  and it costs nothing: creating a device needs `CAP_MKNOD` and the run drops
  every capability, while `no-new-privileges` already neutralizes a setuid bit.
- Sessions keep the plain overlay for isolation but do not use the cache
  mechanism, because promotion is additive and unkeyed: transcripts have no key
  and no invalidation, filenames are session IDs, and nothing is implied by a
  file's presence. An earlier increment recorded sessions as riding the cache's
  mechanism, which was right about the mount and wrong about the write-back.
- Promotion writes into the mounted lower rather than avoiding it. This is the
  one place pisafe adds entries to a directory live runs have mounted, and
  overlayfs calls a lower changing underneath a mount undefined. Two shapes that
  never touch a mounted lower were weighed and not taken: immutable session
  generations reusing the cache's publish path, which would copy a project's
  whole transcript history forward on every run and could drop a concurrent
  run's transcripts under keep-newest eviction; and a run-private directory
  seeded by copying the history in at start, which costs that copy every run.
  What makes the chosen shape defensible is that the undefined part is bounded
  to one observable — nothing existing is ever written, so the only question is
  whether a live run's listing shows the new names, and the isolation invariant
  requires that it not show them. Reversibility: the seeded-directory shape
  stays available and would replace the overlay rather than extend it, so
  changing course is a rewrite of the sessions mount, not a data migration.
- A name the session store already holds is skipped rather than replaced, which
  is what keeps promotion additive in the one case where names repeat: Pi
  rewrites a transcript in place to migrate it to the current session version
  when it loads one. Promoting that rewrite would modify a file concurrent runs
  have mounted, to no benefit, since each run migrates its own copy on load
  anyway. The consequence worth stating: a migration never reaches the store,
  and a transcript deleted from Pi's own picker inside a run stays in the store.
- Sessions are never evicted and `reset` leaves them alone: a transcript is not
  reproducible, so the store grows with the project's history. Bounding it
  belongs to the sweep that reclaims whole project filesystems, not to a
  per-namespace rule modelled on the cache.
- The shared cache is a directory pisafe owns, and tools are redirected into it
  by cache-specific environment variables. Overlaying each tool's own location
  instead — `~/.npm`, `~/.cargo/registry`, `~/go/pkg/mod` — was not taken: it
  needs one upper and work pair per path and makes the run filesystem's shape
  depend on the list of toolchains. The rule this buys is that nothing is shared
  unless a variable puts it there. npm's logs and update-notifier timestamp are
  pushed back out to the run home under the same rule, being per-run, unbounded,
  and useful to nobody else.
- A store whose checkout is missing starts a retention window rather than being
  released. Reclaiming on the first sweep that finds the path gone is what "the
  repository is gone" literally means, and was not taken because the evidence is
  indistinguishable from an unplugged disk or a network mount that had not come
  up, while a project store is the one thing pisafe keeps that it cannot
  reproduce. Presence is the path existing, not the path being a Git repository:
  `RepositoryRoot` cannot distinguish "not a repository" from "git could not
  run", so a broken git would report every project orphaned at once. And a
  project with any run record is skipped entirely, including runs the same sweep
  is reclaiming, which costs an orphan one extra week and buys a predicate with
  no ordering in it.
- One live test covers every shared layer, rather than one per layer per slice.
  The property under test is identical across layers, so the test iterates the
  run spec's overlays and a layer added later is covered by construction.

## The global profile

- `settings.json` and `trust.json` are copied into the run, not mounted, because
  Pi writes both — `/settings`, `pi install`, and `/trust` all do — and a
  read-only mount would turn ordinary Pi use into an I/O error. The copy dies
  with the run, so agent code still cannot change what any later run sees.
- The profile mounts at a neutral path, `/opt/pisafe/profile`, and Pi's own
  global package store stays an ordinary writable directory in the run's home.
  Mounting the profile *at* the package store was tried first, on the reasoning
  that one read-only mount states the property directly — installing globally
  inside a run fails, so nothing a run installs can outlive it. What that misses
  is where a blocked agent goes next: told `EROFS` by `pi install`, it reran the
  command as `pi install -l` and committed the package into the repository,
  which then arrived on the user's Mac through `apply`. Polluting the checkout
  is worse than an install that works for one run and dies with it, and the
  original objection to a neutral mount — that a run-local install vanishes at
  stop unremarked — is answered by remarking on it: stopping reports what the
  run installed and offers to keep it. *Reverses an earlier decision on
  evidence; the mount path is cheap to move back, but the invariant's wording
  moved with it.*
- A run reaches a profile package by absolute path, not as an `npm:` source.
  Both spellings load, but an `npm:` source makes Pi re-check the installed
  version at every startup and install when it disagrees, so a read-only store
  would turn any disagreement into a broken run start. A local path is only
  stat'ed and read. It also keeps profile packages out of `pi update
  --extensions`, which is what "offered, never applied" requires.
- Each installed extension or tool gets its own npm prefix root. One shared
  `node_modules` would mean merging dependency trees across installs, which is
  the problem the cache design already refuses to solve, and Pi documents that
  packages load with separate module roots anyway. The cost is duplicated
  dependencies, which is disk on a bounded filesystem.
- The profile is a scope of the privileged helper, not a directory pisafe
  creates: a bind mount is unreadable to a container without the container
  SELinux label, and the helper is what applies it, along with the root-owned
  fixed-capacity image and mapped-UID ownership every other shared filesystem
  has. There is one profile, named `default`; the path keeps the name level so a
  second can exist later, but nothing reads a profile name from a run, since a
  per-run profile would have to be recorded in the manifest and there is no use
  for one yet.
- A run's own workspace is trusted without asking. *Flagged as a judgement
  call.* Pi's project trust gates whether it loads `.pi/settings.json`,
  `.pi/extensions`, project skills, and system-prompt files from the working
  directory, and defaults to asking. Inside pisafe that guard protects nothing
  the container does not already contain — repository content is exactly what
  the sandbox exists to hold — while leaving it unanswered costs a prompt on
  every run and silently drops a team's project settings in non-interactive
  mode. The consequence worth stating: a hostile repository's
  `.pi/settings.json` loads and can pull its own packages from npm inside the
  run. `pi --no-approve` overrides it for one run, and reverting means deleting
  one line from the trust file pisafe writes.
- A user-authored global `settings.json` is deferred. The mechanism that makes
  one possible — settings written into the run rather than mounted — exists, but
  nothing yet lets a user author the file: it lives in the VM, only pisafe
  writes it, and copying the Mac's own would carry host paths and host tool
  configuration across the boundary.
- An install is two containers, and pisafe holds the pin between them. The first
  asks npm what a spec resolves to and reports the exact version and the
  integrity of that release; the second fetches that version and refuses to
  install bytes that hash to anything else. One container reporting both the pin
  and the tree was not taken: the pin would then be a claim by the same process
  that produced what it describes. This does not make the fetch trustworthy — it
  happens inside a container by necessity — but it makes the recorded pin
  something the install was checked against rather than a description of
  whatever arrived.
- Only npm sources are installable, and only at an exact version. A git source
  has no integrity hash to pin, and a local path names something inside a
  container the user cannot see. A version the user omits is resolved once and
  recorded, so a spec never means two different profiles. Install scripts do not
  run, matching how the run image installs Pi itself; the consequence is that a
  package needing a build step is not installable this way.
- The record is written after the tree and before a removal. A record can name a
  package that is missing, which Pi skips silently, but never fail to name one
  that is there — the reverse would leave a run loading something the user
  removed. Replacing a package swaps the new tree in before the old one goes, so
  a run starting mid-install finds one release or the other rather than a path
  that briefly does not exist. Two installs at once can lose an entry from the
  record; the loser's tree stays unrecorded and re-running the command fixes it.
  A lock was not taken for a single-user command.
- The container mount check guards starting a container, not every path that
  touches one; identity — name, label, image — is proved everywhere. Checking
  mounts before a teardown too was the first shape, and it stranded every run
  whose container predated the profile mount moving: `stop` refused the
  container, and so did `apply`, which stops first, leaving no way to reach the
  work except discarding it. The check protects what a running container can
  reach, and a stop neither reuses the container nor reads anything through it —
  what it publishes it reads from run storage. Special-casing the old
  destination was not taken: it would have left the same trap for the next
  layout change, and one is enough to learn from.
- What a run installed for itself is reported at stop, and reported rather than
  prompted for. An interactive yes/no was not taken: every other offer in pisafe
  prints a command and applies nothing, and stopping is also reached from apply,
  where a prompt would interrupt an import. The report is unconditional, unlike
  the update offer's once-per-change rule, because it is news about the run in
  front of the user rather than a standing fact they may already have declined.
  It is read from the run's own settings out of run storage after the container
  is gone, which is where a run's home lives until it is discarded — so nothing
  has to happen before teardown, and a run whose settings say nothing usable
  costs a failed read and no message.
- The update offer is made when a run stops, not when one starts. Starting a run
  is when the user is waiting and when nothing may depend on the registry being
  reachable; stopping one is when they have finished, when slow best-effort work
  already happens, and when they might act on it. So no run start reaches npm at
  all. Checking in the background at start was not taken: it buys a fresher
  answer at the cost of a network call on the path that matters most.
- An unsolicited offer is made once per change, not at every stop: a stop prints
  only when that day's check moved what is pending, while `extension list` and
  `extension update` repeat a standing offer on request. Printing at every stop
  was the first design and was dropped, because the end of a run is also where
  the run's own failure warning prints and a channel that repeats what the
  reader already declined is one they stop reading. The window is between checks
  rather than within a run, because knowing what changed *during* a run would
  need an answer from the registry at run start. The cost is that a declined
  offer is not raised again until the registry moves; cheaply reversible with an
  announced-at timestamp and a second interval.
- The check refreshes at most once a day, is bounded to 45 seconds against the
  ten minutes an install container is allowed, and one that resolved nothing did
  not happen — it neither replaces a standing offer nor counts, so an
  unreachable registry leaves what is known alone and says nothing. The offers
  file itself is advisory: absent, oversized, malformed, or unknown-shaped all
  mean the same thing as never having checked, and an entry that is not a name
  and an exact version is dropped rather than printed, because these strings
  reach a terminal.
- An offer carries no integrity hash, and applying one re-resolves through the
  same fetch-and-verify path a first install takes, so an offer can never be
  what fetched bytes are checked against and can go stale harmlessly. What is
  pending is derived rather than stored: an offer shows only while the record
  disagrees with it, so applying an update or removing a package silences it
  without anything having to clear the file. This is also why pisafe does not
  order versions — it reports that the registry's answer differs from the pin
  rather than claiming one is newer, which would mean implementing semver
  comparison to say something no decision depends on.
- An update applied while runs are live reaches them. *Verified rather than
  reasoned about*: the run mounts the extensions directory itself, so replacing
  a tree by rename inside it is visible immediately to a container already
  holding that mount. A Pi process keeps whatever it loaded at startup, but one
  started later in that same run gets the new release. Pinning still means no
  update happens without the user asking, which is what the invariant requires.
- A run's toolchain is the image's, because nothing else can supply it.
  *Verified rather than reasoned about*: a run holds `node`, `npm`, `git`,
  `openssl`, and `ssh` and cannot obtain another — the rootfs is read-only, the
  agent is unprivileged so no package manager will run, and `npm install
  --global` writes to `/usr/local`. So global tools are not a convenience layer
  over something that works. The image now carries a wider set at 132 MB;
  leaving it to `pisafe tool install` was not taken, because the useful binaries
  are not npm packages, so the command would never have reached them.
- pnpm and uv are pinned to a digest recorded in the recipe; Debian packages are
  not. A build whose pnpm tarball or uv release changed fails rather than
  installing something else, while the apt packages ride the suite as the build
  already does for `git` and `openssl` — pinning them would mean carrying a
  version set that Debian security updates then invalidate. uv also introduces
  `github.com` as a build-time origin beyond the npm registry and the Debian
  mirrors, because uv publishes no npm package.
- Python comes from `uv python install`, not from Debian, and is linked as
  `python` as well as `python3`. Debian ships no `python` name at all, which is
  the spelling anything written this decade reaches for first, and adding
  `python-is-python3` would have been a second package to carry a symlink. The
  uv route also pins the interpreter to an exact build — 3.13.14 — whose
  checksum the already-pinned uv release carries, so it is the one part of the
  image's Debian layer that stops floating. It costs 94 MB and depends on
  `--default`, which uv still marks experimental; the build names the flag
  explicitly and asserts both spellings report the pinned version, so a uv bump
  that changes the behaviour fails the build rather than shipping an image
  without a `python`.
- pisafe keeps npm as its own installer. pnpm was measured against it rather
  than adopted: `npm install` already writes a lockfile covering every
  transitive dependency with an integrity hash, and that lockfile is inside the
  tree pisafe streams into the profile, so npm and pnpm pin identically. pnpm's
  non-hoisted layout buys nothing here either, because each package already gets
  its own module root. Adopting it would have meant pinning pnpm into the image
  before any of it worked, then rewriting a verified install path for parity.
- `PATH` is a predictability default, not a boundary. Anything in a run can
  prepend to its own `PATH` or call a binary by absolute path, so the order
  pisafe sets controls nothing an attacker cares about; what holds is that the
  profile mounts read-only and its contents are pinned, at any position. The
  image comes first so an installed tool never decides what `git` or `node`
  means, and the run's own `~/.local/bin` comes last so `uv tool install` yields
  something invocable. Restating the image's search path copies something the
  base image owns, which is why a live test fails if a base bump moves it.
- A tool claims the names its own tree gives it, read back after the install.
  npm writes a link in `node_modules/.bin` for every package in the tree,
  dependencies included, so the alternative was to trust the registry's metadata
  before installing. The tree is the truth and the metadata is a copy of it, and
  filtering the links by whether they point into the named package needs no JSON
  parsing on the VM at all. The cost is ordering: the tree lands in the profile
  before pisafe knows what it claims, so a package that provides no command is
  installed and then removed — safe, because a tree nothing links to is inert.
  The directory of links is rebuilt whole from the record rather than edited, so
  nothing a failed install left behind outlives the record that named it.
- A command name another tool already provides refuses the install. Letting the
  newer win, and merely reporting the shadowing, were both defensible; refusing
  is the one that never silently changes what an existing command means, and is
  the easiest to relax later. Tools get no `update` command for the symmetric
  reason: installing one again resolves the name afresh and replaces it, which
  is the whole of what an update would do, so a second command would be a second
  name for one idea. What tools genuinely lack is the offer.

## Naming, emptying, and exporting state

- A project store is named by its checkout path, never by its key. The key is
  what the store is filed under and `project list` could print it, but it is a
  digest nobody recognises, while the path is the only handle a user has for a
  store whose directory is gone. A path that no longer resolves is taken as it
  stands rather than refused, which is what lets a store be dropped after its
  checkout has been deleted. The consequence: a path reached through a symlink
  that git would have resolved differently hashes to a different key, so
  `project list` prints the resolved form to copy from. It reads records only
  and starts nothing — reporting each store's size would help most in deciding
  what to drop, and was not taken because enumerating the storage roots needs a
  new privileged action and therefore a VM recreation.
- `pisafe cache reset` became `pisafe project reset [PATH]`. The old command
  could only mean the working directory's project, which is exactly what the
  stores that most need attention do not have. What it does is unchanged, and
  the session store is still left alone.
- A rebind copies the transcripts into a fresh store instead of renaming the
  filesystem. Renaming is what the operation actually is, and it would be a
  `rename` action on the privileged helper — not taken, because a new action
  forces a VM recreation, and recreating the VM destroys every project store,
  which is the loss rebind exists to prevent. Copying needs no new privilege at
  all. Reversible: if a helper change is being made for another reason, a rename
  would replace the copy without changing the command.
- A rebind carries the transcripts and leaves the caches. Carrying them too
  would be correct — a generation is still valid after a move — and was not
  taken because a generation is a full copy on a fixed-capacity image, so it
  could exhaust space partway through an operation whose whole purpose is losing
  nothing.
- A rebind refuses a destination that already has a store, and dropping or
  rebinding is refused while any run record names the project. An interrupted
  rebind and two genuine checkouts are indistinguishable from the records, and
  silently merging two projects' histories is the worse mistake; the way out is
  `project drop`, at which point the source store is still intact. The
  run-record predicate is the one eviction and cache reset already obey, for the
  reason overlayfs gives: a mounted lower may not go away, and a stopped run
  remounts its own on resume.
- `profile reset` empties the directories rather than removing what the record
  names. A tree left behind by an install that failed after fetching is named by
  nothing, so removing the record's entries would leave it there for ever.
- A backup is a directory, not an archive. One tar file was not taken: a
  directory lets a transcript be read out with the tools already on the Mac, and
  it is what makes repeating a backup into the same place additive rather than a
  rewrite. The cost is that a backup is not one thing to copy around.
- A backup only ever adds, in both directions. Mirroring the VM's current state
  — the rsync semantic — was not taken: it would mean that dropping a project
  and then backing up destroys the last copy of its transcripts, which is
  precisely the disaster a backup exists to prevent. So a name either side
  already holds is left as it is, which is the rule promotion already follows,
  and a second backup and a second restore are both harmless.
- A restore installs from the backed-up pin rather than resolving the name
  again. Resolving `name@version` and comparing the integrity to the backup's
  was the sketched design and was dropped as redundant: the install script
  already checks the fetched tarball against the integrity it is handed, so
  installing from the pin makes the *bytes* the thing verified rather than npm's
  metadata. A release republished under the same version fails the install. A
  package already installed is skipped rather than replaced, because the profile
  may have moved on and putting the backup's older pin over it would be a silent
  downgrade: a restore puts back what was lost, it does not make a profile
  identical to a backup.
- A backup records only projects that contributed a transcript. Recording every
  registered project was not taken: what a restore would put back for an empty
  store is the record and the filesystem, and a run of that checkout creates
  both anyway, so an empty store is genuinely nothing to restore.
- Transcripts fail a restore, packages do not. A transcript has no second source
  to come from, so a failure there is the VM's storage and the projects after it
  would fail the same way — it stops. A package failing is npm being a third
  party, so the rest are still installed and every failure is reported at the
  end.
- A transcript name a run chose is refused rather than fatal. A run can write
  any `*.jsonl` name into its own session directory and promotion carries it, so
  a name the Mac will not write costs that one file and is counted in the
  output, rather than letting one run block the backup of everything else.

## Staging and apply

- Selected untracked inputs are chosen with repeatable `--include PATH` and
  `--include-unsafe PATH` flags rather than an interactive picker, matching the
  non-interactive style of `discard --confirm` and keeping `pisafe run`
  scriptable. A credential-shaped name is refused by `--include` and needs the
  separate flag, so approving one can never be a slip of the finger. An
  interactive selector can be added later without changing the staging
  contract.
- What a run leaves behind is listed with `--directory`, so a directory nobody
  tracks arrives as one name instead of one name per file in it, and a request
  naming such a directory is expanded by walking the filesystem. Enumerating
  every file was not kept: a repository of 21 127 ignored files spent 0.4 s of
  each run start being walked three times, and the report it produced named
  twelve database shards and "21 115 more" instead of `node_modules/`. The
  expansion is what keeps the credential check and the per-file limits looking
  at one file at a time, and it refuses a nested repository rather than copying
  a checkout in as loose files. Two consequences worth knowing: naming one file
  inside a collapsed directory leaves the directory in the report, because the
  rest of it did stay behind; and a file inside a nested repository, which the
  old exact-match listing could not select, can now be included on its own.
- Selection resolves against that listing once, and what it resolves is what the
  stage archives, rather than each of the two being derived from its own call to
  Git. Re-deriving them cost a third walk and let the report and the archive
  describe different states of a repository that changed in between.
- Credential-shaped names are matched on whole words plus a fixed name and
  extension list, so `tokenizer.json` is not flagged while `api_token.json` is.
  Substring matching produced false positives that would have trained the user
  to reach for the unsafe flag by habit.
- Selected inputs cross the boundary as an uncompressed tar beside the bundle
  and patch, not as a second Git bundle or a commit synthesized on the Mac:
  these files are by definition outside Git, tar carries the executable bit and
  symlinks, and it reuses the existing size- and SHA-256-verified upload path.
  The staged snapshot, not the archive, decides which names are legitimate.
- An included path crosses as files in both directions and never enters Git.
  Committing it into the baseline was simpler on the way in and is what the code
  did, but it made the run's history depend on content the user had deliberately
  not committed: the agent could commit an ignored file back as history, and
  `--include` could not be combined with `--drop-baseline` at all, because
  replaying without the baseline conflicted on paths the run never touched as
  history. Carrying them as files removed the second problem outright rather
  than special-casing it.
- What `--include` records is the path the user named, not the file list it
  resolved to. Re-resolving from the original command-line arguments at apply
  time was rejected: they are host paths that may be gone or relative to another
  working directory, while the snapshot is the record both sides already trust.
  This is what makes `--include DIR` meaningful for a directory that is empty at
  run start — previously refused as "not an untracked or ignored file", which
  named the wrong cause — and it is what apply re-expands to find work the run
  created. The roots are a persisted snapshot field, so revisiting the shape
  costs a manifest version.
- Apply writes files into the working tree, but only under the paths the user
  included. This is a deliberate exception to apply otherwise never touching the
  working tree: the alternative is that work created under an included path is
  silently lost, which is what the old behaviour did. The channel is bounded by
  proving each returned path is safe and under a recorded root — the list is
  written inside the run — and by never deleting.
- Copy-back is additive and refuses as a whole. Mirroring deletions would give a
  sandboxed run the ability to delete the user's files, which is not a capability
  an isolation tool should hand out for a convenience feature. Skipping only the
  conflicting files was rejected because a partial copy leaves the working tree
  in a state neither side described; refusing everything matches how
  `ImportApply` already verifies each object set before anything user-visible
  changes. A conflict is decided against the SHA-256 recorded when the run was
  staged, which is why a carried-in file records one.
- The copy runs after the refs are committed, not as a precondition for them. As
  a precondition it would be atomic with the import, but a conflict over a
  scratch file would then block importing real code work. The returned archive is
  moved to the Mac before the run's package is deleted, so finishing a refused
  copy never needs the VM — at the cost of one file per pending run in the state
  directory.
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
- A run that attaches a submodule of its own has its whole apply refused, rather
  than landing the branch with a warning. The superproject branch still carries
  every file change, which is what made warning tempting, but the branch is
  unusable exactly where submodules matter and nothing on the Mac can be told
  where to fetch the missing repository from. Refusing costs nothing that is
  gone: it happens before any ref moves, and the run is still there to be
  connected to and corrected.
- Each half of an apply was verified against what the Mac staged and neither
  against the other, so nothing related the superproject's gitlinks to the
  submodules imported beside it. The pointers the run moved are now checked for
  presence in the repository they name — presence, not reachability from the
  imported tip, because moving a pin to any commit the submodule already has is
  an ordinary change and reachability would refuse a downgrade. Only pointers
  the run changed are examined; one it left alone names what the source commit
  already named.
- For a submodule that was staged, that check cannot currently fail: a run may
  not leave a submodule below its staged base, and the final commit records
  where each one actually ended, so the gitlink always equals the tip the bundle
  carries. It is kept anyway. The property belongs to the branch pisafe hands
  over, and having it hold by coincidence of two rules in another function is
  how it would later stop holding without anything saying so.
- The apply journal records only ref creations, because apply only ever creates
  `pisafe/<run>`. It records no ref names either: the branch and the incoming
  ref both follow from the journal's run ID, so a tampered manifest cannot name
  a ref the run never earned — an alternative to validating stored ref strings
  on the way in and out, which is what it replaced. The compare-and-swap
  discipline and its recovery rules are implemented in full; the general
  old-value restore is not, because no code path produces a step with a
  previous value and an untested branch is worse than an absent one. Submodule refs are committed before the superproject ref,
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
- A stage package's file names have one owner, mirroring what the apply
  direction already had: the side that writes the package and the side that
  reads it both derive every path from `StagePackage`, and the transport's
  allow-list asks the same package whether a name is one a stage can hold. Four
  copies of that layout — the Mac's, the upload list's, the allow-list's, and
  the guest's — were the previous arrangement. `InputsPath` now means the same
  thing on both sides: set exactly when the package carries an input archive.
- The Mac verifies the drop instead of trusting the run's word for it: the
  baseline commit exists only inside the run, so a source repository that knows
  it after the fetch learned it from the bundle that just arrived, and apply
  stops.
- A `merge-base --is-ancestor` that fails for any reason but its exit code 1 is
  an unanswered question, not a "no". Staging and apply each refuse when the
  answer is no, so conflating the two was harmless there; the drop verification
  asks the opposite question, where it meant a git failure read as "the baseline
  was dropped" and let the apply through. One helper now separates the exit code
  from the failure, and every caller propagates the failure.
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
- A `cp` destination that is already a directory takes the copy inside it,
  under the copied path's own name. Refusing it as an existing destination was
  what the code did, and it made the common case — naming a directory to copy
  into — look like a request to replace that directory, answerable only with
  `--force`, which would have deleted it. The rule now matches `cp`, and
  `--force` again means only what it says.
- A run's name may be left out, and is then the one run of the current checkout
  that has not been imported yet. Runs are matched by the project key their
  record already carries, so nothing new is stored and nothing inside a run
  influences which run a command reaches. Imported runs are excluded because
  they accumulate for a week under `gc`, and counting them would have made the
  shorthand stop working exactly for the people using it most. Two live runs
  are reported with their states rather than resolved by recency: recency would
  be right most of the time, which is the wrong property for `stop` and
  `apply`. `discard` keeps requiring the name twice — its confirmation is the
  point of the command.
- `pisafe cp` gained an inward direction rather than a second command. The
  colon already marked which end was in the run, so which side carries it can
  say which way the copy goes; a `pisafe push` or `put` would have been a
  second door onto one set of limits, one archive format, and one report.
  A single bare path still means copying out, because that is the only thing it
  could have meant before. An empty name before the colon now means the
  checkout's own run on either side, where it used to be an error — the
  shorthand every other command has, spelled the one way the direction marker
  allows. Reversible: it is one parser and its usage string.
- Copying in unpacks in a throwaway inspection container with the workspace
  mounted read-write, rather than `podman exec` into the live run container.
  It reaches stopped runs as well as active ones, keeps the network off, and
  reuses `runcopy.CopyTo` — staging directory, then rename — exactly as it
  stands. The archive is produced on the Mac so the run has no say in what
  arrives, which is the mirror of the outward direction's rule that the Mac
  re-validates everything the run sends.
- A credential-shaped name copied *in* needs `--unsafe`; the same name copied
  *out* needs nothing. Leaving the inward direction unguarded was considered on
  the grounds that the user names the path explicitly, unlike `run --include`
  where a glob can sweep one in. It was rejected because this is now the
  frictionless way to put a reusable credential inside a run, which is the one
  act that voids what a run guarantees; the guard is a reminder rather than
  proof, as the design says of every such scan. Coming out is the user reading
  their own run's file and needs no ceremony.
- Copying in works on runs created before the command existed, because
  inspections run the current managed image rather than the one a run was
  created from. This is the opposite of `pisafe forward`, whose sshd policy is
  written into the run's home once at creation. Worth knowing when judging
  whether a new run-side capability will reach existing runs: anything carried
  by an inspection container will, anything written into the run's home will
  not.

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
- A run sees every configured upstream, not one active one. Keeping the broker's
  single provider and letting the newest login win was a much smaller change:
  nothing about routing, models.json, or the relay would have moved. It was not
  taken because what a user wants from a second provider is to choose per task —
  a subscription for long sessions, a metered key for a one-off — and an
  active-provider switch makes that choice a command plus a run restart. Pi
  already namespaces models by provider, so several entries cost a run nothing.
- The provider's name leads its relay path: `/<name>` prefixes the API path the
  client would send on its own, matched exactly. Routing on the model ID in the
  request body was rejected outright, because it would make the relay parse what
  a run sends, which is the one thing it never does. The name is validated as a
  lowercase slug because it is simultaneously a URL path segment, a models.json
  key, and a Keychain account.
- A dead login no longer stops the broker from starting. With one provider,
  failing at startup was right: nothing worked anyway, so failing loudly beat
  failing per request. With several it inverts — one expired refresh token would
  withhold every other upstream — so the broker names the unusable logins and
  serves the rest. Nothing is weakened: the relay's per-request fail-closed path
  is what actually refuses a request whose credentials cannot be produced.
- A built-in provider's login records only its name. Writing the endpoint, wire
  format, and model list into the record at login time makes it self-describing
  and was not taken, because a release that adds a model or moves an endpoint
  would then never reach a key stored months earlier. The table wins over the
  record, so a record hand-edited to point a known name elsewhere is ignored
  rather than obeyed — which also means a stored key cannot be redirected to
  another host by editing a file.
- A custom endpoint declares its models; pisafe does not discover them. Asking
  the endpoint's own `/v1/models` returns identifiers and nothing else — no
  context window, no cost — and a wrong context window is a real failure rather
  than a cosmetic one. The declared list has its per-model routing fields
  stripped rather than refused, because Pi's own provider data is the obvious
  thing to copy an entry from and every entry there carries them; a model naming
  its own `baseUrl` would reach the provider without passing the relay.
- Plain HTTP is refused except on loopback. The key is a credential, and sending
  it in clear over a LAN is the exposure a broker holding it exists to prevent.
  Loopback is exempt because it never leaves the Mac, which is also what makes a
  local model server a first-class upstream. Reversible if it proves too strict.
- An endpoint may not end in the path segment pisafe appends. Every
  OpenAI-compatible service documents its base URL ending in `/v1`, and the
  relay appends the client's own API path, so pasting the documented URL
  produces `/v1/v1/responses` and nothing but the upstream's 404 would say so.
  Refusing, rather than silently rewriting, teaches the shape once.
- An API pisafe does not know has no canonical path, and a catalog holding one
  configures no run. The path used to fall through to `/v1/responses`, which
  made the known OpenAI-responses case indistinguishable from the unknown one
  and would have relayed an unrecognised wire format upstream under a shape it
  does not speak. Naming all four APIs and refusing the rest costs a run
  nothing — no login path can produce another — and turns a guess into a
  refusal at the one place that already refuses catalogs no run could use.
- Everything an API name decides lives in one table in `broker`, keyed by the
  name: the relayed path, the suffix that completes a run's base URL, whether
  the capability is JWT-wrapped, the header the Mac's key travels upstream in,
  and which error envelope a refusal is written in. Each was a switch in a
  different file, which is five chances to describe one API five ways, and the
  two furthest apart — the header a key is sent in and the envelope an error
  comes back in — were the ones most likely to drift. Adding an API is adding a
  row; the only thing outside the table is `apikey.validateAPI`, which answers a
  different question, whether a login the user writes by hand may declare it at
  all, and excludes the Codex flow that pisafe owns.
- Making an unroutable API unrepresentable rather than refused was considered
  and dropped as machinery with nothing left to catch. Every `Provider` is built
  from one of the four constants or from a record whose `Validate` has already
  bounded its API, so a validating type at the parse boundary would restate a
  check that already runs there.
- A login is removable whether or not it is still usable, whichever kind it is.
  `logout` used to confirm the login existed by loading it — the whole key
  catalog for a key, the parsed and validated credential for the subscription —
  which made anything the current rules no longer accept impossible to remove
  through the CLI. Removal now asks only whether something is stored under the
  name.
- A stored secret is read only when a request is relayed, whichever kind of
  login it is. Starting a run renders models.json, which contains no upstream
  credential, and assembling the catalog asks the keychain only whether an item
  exists — `security` hands a password over only when it is told to print one —
  so run creation touches no secret at all. Loading the subscription credential
  to prove the login was there was the alternative; it made every run creation,
  every resume, and every `pisafe login` listing read the OAuth token. The cost
  of dropping it is that a stored credential that no longer parses is now
  reported when the broker starts, which already forces every login once, rather
  than by refusing to start a run that would not have used it.
- The ChatGPT login stays a bare Keychain secret with no record file beside it,
  unlike an API-key login. Giving it one would make "a stored login is a record
  naming a Keychain secret" the single storage model and remove three
  `chatgpt.Name` branches, at the cost of every existing ChatGPT login having to
  re-authenticate once. That trade was worth considering while existence was
  proved by reading and parsing the credential; it is not now that existence is
  an attribute-only keychain lookup, and the OAuth flow would stay special-cased
  whatever the storage looks like.
- pisafe names the model a run opens on, rather than leaving it to Pi. Pi picks
  from a table keyed by its own provider names, which do not include pisafe's,
  so a subscription run opened on whatever the embedded catalog happened to list
  first. Reordering the catalog so the wanted model leads it was the cheaper
  alternative and was not taken: it is invisible, it says nothing about
  reasoning effort, and a catalog re-sync would undo it. Renaming the provider
  to `openai-codex` so Pi's table applies was refused outright — that name is
  also a relay path segment and a Keychain account, and it collides with a
  provider Pi already defines.
- The preference is one model id and one effort held in the broker and matched
  against each configured catalog, not a default recorded per provider. The
  first upstream offering the model decides, which makes "the default exists"
  intrinsic rather than an invariant to check, and leaves a Mac whose logins
  offer nothing preferred with Pi's own choice instead of a broken name.
- The run-side defaults are filled in only where the run has not answered. Pi
  writes the same keys when a model is chosen inside a run, and resume
  configures the same file again, so overwriting them would undo a choice the
  run made. The cost is that changing the preferred model does not reach a run
  that already recorded one.
- What the controller pipes into a run is a document holding both the models and
  the model to open on — `internal/piagent.Configuration` — rather than
  `models.json` alone with the default carried some other way. It is a leaf
  package because the guest binary must not link the Mac's VM manager, which
  the broker does.
- The guest verb was renamed from `configure-inference` to `configure-models`
  in the same change. A run keeps the image it was created with, so a run
  created before this change is resumed by the old helper, which would accept
  the new document as a JSON object and write it whole into `models.json` —
  leaving Pi with no providers and nothing to report. An unknown verb fails
  loudly instead. Reversibility note: runs created before this change cannot be
  resumed after it; they can still be diffed, applied, and discarded.

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
- The design's Phase 2 sections were compressed to invariants before the work
  started, dropping the mechanism sketched for unbuilt features: cache overlays
  with merge-back under a lock, and session-ID append semantics. Both were in
  fact re-decided once the substrate was measured — merge-back became keyed
  immutable snapshots, and the lock turned out to be unnecessary because Pi
  never locks a session file — so compressing to invariants cost nothing and
  would have cost a rewrite otherwise.
- Phase 2 was planned in its own document under `plans/`, temporary by design:
  it held the slice order, the substrate established against the live VM, and
  its decisions as they were made, and it folded into these three documents and
  was deleted when the last slice landed. A phase-shaped section inside this log
  was not taken, because a plan is a thing being revised in place while a
  decision log is append-mostly, and mixing them makes it unclear which entries
  are still proposals. The cost is that the fold-back is a real piece of work
  that has to happen while the reasoning is still fresh; the reward is that
  nothing here describes something unbuilt.
