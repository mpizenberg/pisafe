# Phase 2 — managed persistence

Working document. Last updated: 2026-07-28.

This is the plan and progress log for Phase 2, and it is **temporary by
design**: when Phase 2 lands, its invariants and mechanisms fold back into
[`pisafe-design.md`](../pisafe-design.md) and
[`DECISIONS.md`](../DECISIONS.md), the delivered behaviour folds into
[`IMPLEMENTATION_PROGRESS.md`](../IMPLEMENTATION_PROGRESS.md), and this file is
deleted. Until then it is the authority on Phase 2 state, and those documents
carry only a pointer to it.

Phase 2 is quality of life rather than safety. That does not license weakening
the boundary for any of it.

## What must hold

The design's three invariants, however the work is built:

1. **Agent code cannot write global settings, extensions, or tools.** Those
   change only through a management command that pins an exact version and
   integrity hash, and the profile mounts read-only.
2. **A pinned extension is still untrusted at runtime.** Pinning prevents a
   surprise update, the boundary limits its reach, and updates are offered
   rather than applied.
3. **One project's runs cannot read another project's sessions or caches, nor
   concurrent runs one another's live transcripts.**

The design's state table is the specification for what is writable where; the
rows Phase 2 makes real are Pi sessions (project store, run-local), dependency
caches (project), global settings, installed extensions, and global tools.

A fourth property, not an invariant but the thing that makes the rest tractable:
**shared state is disposable, and no run's correctness may depend on it.** A
cache that is missing, stale, or wrong must cost time and nothing else. Every
mechanism below is allowed to be approximate because of this, and the reset
path is what makes the claim true rather than aspirational.

## Substrate

Verified against the pinned image and the live VM, not inferred from
documentation. Reproduce with the commands under [Verification](#verification).

- **Shared state is an overlay, not a shared mount.** Podman mounts it
  rootlessly: `-v <lower>:<dst>:O,upperdir=<upper>,workdir=<work>`. The lower
  is a project-owned directory, the upper lives in the run's own filesystem.
  Two containers on one lower were shown to hold conflicting states — one
  shadowed a shared file and added another, the second still read the original
  and saw neither, both uppers survived exit, the lower stayed byte-unchanged.
  No new VM privilege: the root helper gains per-project filesystem creation,
  not mounting.
- **The overlay spans two filesystems.** A lower on the project's ext4 image
  and an upper on the run's own was mounted and written. Podman is particular
  about the rest: `:O` is refused alongside any other mount option, so a shared
  mount cannot carry `nodev,nosuid`; `--mount type=overlay` does not exist, so
  `-v` is the only spelling; and both `upperdir` and `workdir` must already
  exist, so they are created before the container starts.
- **A mounted lower must not change underneath a live run.** overlayfs leaves
  behaviour undefined if the underlying filesystem is modified while part of a
  mounted overlay. This is a kernel constraint, not a merge-semantics one, and
  it is the reason shared state is written by creating new directories rather
  than by editing existing ones.
- **npm's cache is only half content-addressed.** `~/.npm/_cacache` holds
  `content-v2/` keyed by the hash of the bytes, but `index-v5/` is keyed by the
  hash of the *request URL*, so two runs fetching the same package write the
  same path with different contents. It happens to degrade into a re-fetch
  rather than corruption because each index line carries its own checksum — but
  that is npm being forgiving, and it generalizes to no other tool. Do not
  build on directory merging.
- **npm redirects wholesale.** `npm_config_cache` moves the whole cache, and
  `npm_config_logs_dir` keeps per-run logs out of it.
- **Pi writes its transcript where the environment says.** With
  `PI_CODING_AGENT_SESSION_DIR` set to the overlay, a real run's session file
  landed in the run's private upper and the project lower stayed empty. The
  file is written before any inference succeeds, so this is observable without
  a provider.
- **Ownership follows the existing rule.** The overlay is writable only when
  the lower's contents are owned by the container's mapped UID, the same
  `subuid_start + 999` the storage helper already derives. The consequence
  worth keeping in mind: everything the helper creates is writable by the
  pisafe user under `podman unshare`, so pisafe can build structure *inside* a
  helper-created directory without any new privilege or helper change.
- **The global profile is a read-only mount** named by absolute path in the
  `packages` array of `settings.json`. A package on a genuinely read-only mount
  was loaded and listed by the pinned Pi.
- **`PI_PACKAGE_DIR` does not relocate installed packages.** It locates Pi's
  own installation for Nix and Guix store paths; an install with it set still
  went to `$PI_CODING_AGENT_DIR/npm`. Do not build on it.
- **Sessions relocate.** `PI_CODING_AGENT_SESSION_DIR` is read by Pi — the
  literal never appears in `dist` because the name is assembled from `APP_NAME`
  at build time — and `--session-dir` overrides it. Both are ours to set.
- **A relocated session directory is flat.** Pi's default layout segregates
  transcripts per working directory under `sessions/--<cwd>--/`, but
  `SessionManager.list` and `listAll` use a directory handed to them verbatim,
  so the store pisafe points Pi at holds transcripts directly with no cwd level.
  Listing a relocated directory additionally filters by each transcript's own
  recorded cwd, which is one value for every run of a project.
- **A transcript's name cannot collide, and Pi deliberately never locks one.**
  The file is `<ISO timestamp>_<randomUUID>.jsonl`, appended a line at a time.
  Pi does use `proper-lockfile`, for `settings.json`, `trust.json` and its auth
  store — the files it genuinely shares — and for no session file: concurrent
  agents in one project share a directory and never a file. The only case where
  Pi rewrites an existing transcript is migrating it to the current session
  version when it loads it.
- **`settings.json` and `trust.json` must be copied in, not mounted
  read-only**, because Pi writes both. The copy dies with the run.
- **A package on a read-only mount loads, and its code runs.** A package seeded
  into the profile registered a CLI flag that `pi --help` then printed, offline,
  in a container with a read-only root and no capabilities.
- **A package named by absolute path is inert; one named `npm:` is not.** Pi
  stats a local path and reads its resources. For an `npm:` source it compares
  the installed version against the configured one at every startup and installs
  when they disagree, which on a read-only store turns a disagreement into a
  broken run start rather than a stale package.
- **Pi installs global packages under `$PI_CODING_AGENT_DIR/npm`.** With the
  profile mounted there, `pi install` fails on the read-only filesystem while
  `pi -e` still works, because that path installs to a temporary directory.
- **A bind mount is unreadable to a container unless it carries the container
  SELinux label.** The VM enforces, and an unlabelled mount fails as EACCES
  rather than as a mount error. Everything the storage helper allocates is
  already relabelled, which is why the profile is allocated by the helper too.
- **A mountpoint Podman creates inside the run home is root-owned.** Mounting
  the profile at `~/.pi/agent/npm` without preparing the path first leaves
  `.pi` and `.pi/agent` owned by the container's root, so Pi can no longer write
  its own settings. pisafe creates the mountpoint before the run starts.
- **`npm pack --dry-run --json` reports the version and integrity of what a
  spec resolves to**, scoped packages included, which is the pin without
  fetching anything into the profile.
- **Podman reports a read-only bind as `RW: false`, not as an `ro` option.** A
  manifest check that only reads mount options cannot tell a read-only profile
  from a writable one.

### How caches are shared

The model is the one CI systems converged on, because directory merging is not
a solvable problem in general — the presence or absence of a file carries
tool-specific meaning that no generic rule reconstructs.

A **cache namespace** is a name declared by the project. Under it the project
store holds **immutable snapshots**, each named by a key derived from the
project's own inputs:

```text
/var/lib/pisafe/projects/<project>/cache/<namespace>/<key>/
```

- **Restore.** A starting run looks for the snapshot whose key matches exactly.
  Failing that it takes the newest snapshot in the namespace — the directory
  itself is the restore prefix, so a changed lockfile restores the previous
  generation as a base and the tool fetches only the delta.
- **Publish.** When the run stops, if the run's key has no snapshot and the
  run's upper is not empty, the overlay's merged view is materialized into a
  new directory and renamed into place. overlayfs has already resolved
  deletions and whiteouts; pisafe copies what the container saw and never
  interprets an upper. The empty-upper condition is what keeps a run that
  fetched nothing from laying a blank generation over a namespace that has
  usable ones — a blank one would be the newest, and so the fallback.
- **Immutability is what makes this safe.** No snapshot is ever modified, so no
  mounted lower ever changes, so concurrent runs need no coordination and
  publishing needs no scheduling window. Two runs producing the same key race
  to rename; the loser discards its temporary directory.
- **Eviction** keeps the newest snapshot per namespace and drops the rest,
  because immutable generations are full copies on a fixed-capacity image. It
  also keeps any snapshot a recorded run may still mount, whatever its age.
- **Reset** empties every cache namespace of one project. It is refused while
  any run of the project could still mount a generation, for the same reason
  eviction protects one.

Sessions are not a cache and do not use this. They have no key and no
invalidation: filenames are session IDs, nothing is implied by a file's
presence, and folding a run's transcripts into the project store is pure
addition.

### How sessions are shared

The store is one flat directory, mounted as the sessions overlay's lower, and
promotion adds this run's transcripts to it when the run stops:

```text
/var/lib/pisafe/projects/<project>/sessions/<timestamp>_<uuid>.jsonl
```

- **Promotion only adds.** A name the store already holds is skipped, and a
  transcript the run deleted is a whiteout device rather than a file and is
  skipped too. So a run can neither rewrite nor delete what another run handed
  over — including when Pi rewrites a transcript in place to migrate it to the
  current session version on load, which promotion leaves in the run's own
  upper.
- **Nothing needs a merge**, because a transcript's name carries a UUID and no
  two runs ever choose the same one. This is Pi's own reason for having no lock
  on a session file while locking the settings, trust, and auth files it does
  share.
- **Each transcript arrives by rename** from a staging dot entry on the same
  filesystem, so a concurrent reader sees a whole transcript or none of it.
- **This is the one place a mounted lower gains entries.** overlayfs calls a
  lower changing underneath a mount undefined, and what is bounded here is the
  consequence: nothing existing is touched, so the only undefined part is
  whether a live run's listing shows the new names — which invariant 3 says it
  must not show anyway.
- **Nothing is ever evicted.** A transcript is not reproducible, so the store
  grows with the project's history and `reset` leaves it alone.

### How the profile is shared

The profile is one filesystem every run mounts read-only, at the path Pi
already uses for its global packages:

```text
/var/lib/pisafe/global/default/extensions/<directory>/node_modules/<name>
/var/lib/pisafe/global/default/pins/extensions.json
```

- **The mount is Pi's own global package store**, `$PI_CODING_AGENT_DIR/npm`.
  Nothing simulates the invariant: `pi install` fails because the store it
  writes to is read-only, and `pi -e` works because that path never touches it.
- **Each extension is its own npm prefix root**, so installing a second one
  never merges dependency trees with the first. Pi loads packages with separate
  module roots, so this is its model too.
- **A run reaches a package by absolute path**, listed in the `packages` array
  of the `settings.json` pisafe writes into the run home. This keeps loading
  inert and keeps profile packages out of `pi update`, which is what "offered,
  never applied" needs.
- **`settings.json` and `trust.json` are written into the run**, not mounted:
  Pi writes both, and a read-only copy would turn ordinary use into an I/O
  error. What pisafe puts in them is the package list, the transport pin, and a
  trust decision for the run's own workspace. Everything Pi then writes dies
  with the run.
- **Only pisafe writes the profile**, from the Mac, through a command the user
  runs deliberately. A run has no writable handle on it at all.

### Project configuration

Cache namespaces are declared by the project, in `.config/pisafe.json` at the
repository root, read from the checkout on the Mac at run start:

```json
{
  "caches": [
    { "name": "npm", "env": ["npm_config_cache"], "key": ["package-lock.json"] }
  ]
}
```

pisafe chooses the mount point (`/cache/<name>`), sets each listed variable to
it, and computes the key from the listed files plus the run image ID. The
schema is deliberately **inert**: no commands, no shell, no paths of pisafe's
own choosing. Unknown fields are refused rather than ignored, the declared key
files are opened through a root a symlink cannot escape, and no cache may bind
a variable pisafe sets itself. This file is the first repo-supplied input
pisafe parses on the host, and on an untrusted clone it is
attacker-controlled, so the worst a hostile config may achieve is a useless
cache key or a full project image — both contained, both fixed by a reset.

A repository that declares nothing gets no `/cache` mount at all, and no tool
is redirected anywhere: sharing is opt-in per project, per namespace.

### Layout

Project key: directory slug plus the first eight hex characters of the SHA-256
of the Mac-side Git root path, e.g. `api-3f9c2a1b`.

```text
/var/lib/pisafe/global/default/extensions/          read-only bind → ~/.pi/agent/npm
/var/lib/pisafe/global/default/pins/                pisafe's record, mounted nowhere
/var/lib/pisafe/projects/<key>/cache/<ns>/<snap>/   immutable snapshot → overlay lower
/var/lib/pisafe/projects/<key>/sessions/            overlay lower → session dir
/var/lib/pisafe/runs/<run>/workspace                bind mount → /work
/var/lib/pisafe/runs/<run>/home                     bind mount → /home/node
/var/lib/pisafe/runs/<run>/overlay/<ns>/            the run's private upper and work
```

The privileged helper allocates only the two project namespaces (`cache`,
`sessions`), the two profile namespaces (`extensions`, `pins`), and the run's
bare `overlay` directory. Everything below them — snapshot directories, one
upper/work pair per declared cache, one prefix root per installed extension —
is created by pisafe as the mapped UID, because none of it can be known when
the filesystem is allocated.

The profile, the project, and the run filesystem are all root-owned
fixed-capacity ext4 images allocated by that same helper, which is the only
thing that mounts anything.

## Slices

- [x] **0. Substrate scouting.** Establish the mechanisms above against the
      live VM. Commit `ce7cccb`.
- [x] **1. Per-project storage and the dependency cache.** Extend the
      root-owned helper from per-run to per-project filesystems; derive the
      project key; wire a cache overlay into `runcontainer.Spec`. Commit
      `e6e316f`.
- [x] **2. Sessions on the same mechanism.** `PI_CODING_AGENT_SESSION_DIR`
      points at a second per-project overlay. Live test:
      `TestLiveProjectLayersAreSharedToReadAndPrivateToWrite` — two concurrent
      runs read the seeded project state and neither sees the other's writes.
      Commit `1c5ec79`.
- [x] **3. Declared caches and snapshot restore.** Parse `.config/pisafe.json`,
      compute each namespace's key, select a snapshot by exact key then by
      newest-in-namespace, and mount one overlay per declared cache with uppers
      pisafe creates itself. Nothing publishes yet. Live test:
      `TestLiveCacheSnapshotsAreSelectedByKeyThenRecency` — an empty namespace
      selects nothing, either seeded generation is found at its exact key, and
      an unknown key falls back to the newest.
- [x] **4. Publishing and eviction.** Materialize the merged view into a new
      immutable snapshot at run end, keep the newest few per namespace, and add
      the reset that makes disposability real. Live test:
      `TestLivePublishedGenerationsAreImmutableAndDisposable` — a run's fetches
      and its deletions are both in the generation it publishes, the generation
      it restored is byte-unchanged, the next run falls back to what was just
      published, eviction spares a generation a run may still mount, and reset
      empties the cache while leaving the session store alone.
- [x] **5. Session promotion.** Fold a finished run's transcripts into the
      project session store — additive, no key, no invalidation. Live test:
      `TestLiveFinishedTranscriptsPromoteWhileLiveOnesStayPrivate` — a later run
      reads a finished run's transcript, a live run's transcript reaches no
      concurrent run, and neither a rewrite nor a deletion inside a run follows
      a transcript another run already promoted.
- [x] **6a. The global profile mount.** A fourth storage scope for the profile,
      mounted read-only at Pi's own global package store, with the `packages`
      list and the workspace trust decision written into each run. Live test:
      `TestLiveTheProfileLoadsAndStaysReadOnlyToTheRun` — a package seeded into
      the profile registers its flag in `pi --help`, so its code ran; the
      repository's own extension loads without a trust prompt; `pi -e` still
      works; and the run can neither write the store nor `pi install` into it.
- [x] **6b. `pisafe extension install`.** Resolve the pin from npm's own report,
      install that exact version into its own module root inside a throwaway
      container, and stream it into the profile by rename. Live test:
      `TestLiveAnInstalledExtensionIsPinnedToWhatWasFetched` — the recorded pin
      is the registry's own answer, bytes that hash to anything else never reach
      the profile, the installed tree is that exact release, reinstalling
      replaces rather than accumulates, and the next run resolves the package.
- [ ] **7. `pisafe extension update` and update notifications.** Offered, never
      applied. Depends on how pinning is recorded in slice 6.
- [ ] **8. Global tools.** See the open question below — the mechanism is not
      yet decided.
- [ ] **9. `gc` sweep of project stores.** Reclaim whole project filesystems
      whose repository is gone. Per-namespace eviction lands in slice 4; this
      is the outer sweep the design already specifies.
- [ ] **10. Other providers.** Additional `pisafe login <provider>` commands
      over the existing broker interface.
- [ ] **11. Backup, reset, recovery.** Cheapest useful piece is resetting or
      removing a scope, which is nearly free once scopes exist.

## Decisions

Appended as they occur, and revised in place when later work proves one wrong —
this document is the current truth, not an audit trail. These fold into
`DECISIONS.md` when Phase 2 lands.

- **Project identity is a path hash.** A project is keyed by its directory slug
  plus eight hex of the SHA-256 of the Mac-side Git root path. Keying on the
  repository's root-commit ID was not taken: it survives moves, but two clones
  of one upstream would then share a cache and session store, an empty
  repository has no key at all, and a multi-root history needs a tiebreak rule.
  The path hash is always available and always distinguishes two checkouts.
  Moving or renaming a repository therefore orphans its store rather than
  migrating it; a rebind command can adopt an orphan later without changing the
  key scheme.
- **Runs share project state through an overlay**, not a shared read-write
  mount. Sharing read-write would make every package manager's concurrency
  correctness a property of the boundary, unverifiable by us and fatal to a
  whole project's cache when one tool gets it wrong. A full copy per run was
  not taken either: run filesystems are ext4 with no reflink, so the copy would
  be real.
- **Caches are keyed immutable snapshots, not merged directories.** This
  supersedes an earlier plan to fold a run's upper into the project store
  additively. Merging was abandoned because the presence or absence of a file
  carries tool-specific meaning — index files, stamp files, manifests — so no
  generic rule reconstructs a state any tool would have produced. Every CI cache
  system converged on wholesale keyed snapshots for this reason, and adopting
  their model costs a full copy per generation and buys three things: whiteouts
  are resolved by the kernel instead of interpreted by us, no mounted lower ever
  changes so concurrent runs need no coordination, and eviction has an obvious
  unit.
- **The shared layer is disposable, not correct.** An earlier entry claimed
  every path under `/cache` was content-addressed and therefore safe to merge
  last-writer-wins. That was checked and is false: npm's `index-v5` is keyed by
  request URL, not by content, and nothing generalizes to other tools anyway.
  The property we actually hold, and can hold for any toolchain, is that a
  wrong or missing cache costs time and never correctness. This is what obliges
  slice 4 to ship a reset rather than defer it.
- **The namespace directory is the restore prefix.** All snapshots for a cache
  live under `cache/<namespace>/`, so falling back means taking the newest
  entry there. Also writing each snapshot under a short shared key, as some CI
  configurations do, was not taken: that overwrites an entry a concurrent run
  may have mounted as its lower, reintroducing the one overlayfs hazard
  immutability otherwise eliminates. Prefix search over immutable full keys
  gives the same partial-hit behaviour with no mutation.
- **Cache namespaces are declared by the project, in `.config/pisafe.json`.**
  Hardcoding a per-tool table in pisafe was not taken: the knowledge belongs to
  whoever knows the project, and a fixed table makes every new toolchain a
  pisafe release. `.config/` rather than the repository root keeps the root
  uncluttered.
- **The config is JSON, parsed with the standard library.** pisafe currently has
  zero dependencies. TOML would be the friendlier authoring format and the first
  third-party package in a tool whose entire argument is boundary control; that
  trade was declined. Reversibility: changing the format later means migrating
  every repository that adopted it, so this is the expensive decision on this
  page.
- **A declared cache may not bind a variable pisafe sets itself.** The
  rejected set is derived from the run's own environment rather than listed
  twice, so the two cannot drift. An allowlist of known-good cache variables
  was not taken, and neither was a denylist of process-semantics variables like
  `PATH` or `LD_PRELOAD`: what a cache carries into a later run of the *same*
  repository is that repository's business, and it is already true of any
  cache holding executable content. What is pisafe's business is that the
  session directory, the home, and npm's log path stay where pisafe put them,
  and that is exactly the set now refused.
- **The declared key files are read through `os.Root`.** The paths come from
  the repository and are opened on the Mac, so a symlink out of the checkout
  would let a declaration hash arbitrary host files. They are also rejected
  lexically first, so the failure names the declaration rather than the
  filesystem. A missing key file hashes as absent rather than failing: a
  repository with no lockfile yet has no dependencies yet, which is a state and
  not an error.
- **Snapshot selection is a project-store read, preparation is a run-store
  write, and they are separate calls.** Doing both in one call was the first
  shape and it forced the run filesystem to exist before the selection could be
  recorded in the manifest. Splitting them lets selection happen before the run
  is recorded at all, which is what makes the resolved snapshot part of the
  manifest rather than something reconstructed later.
- **The resolved snapshot is recorded in the manifest, and resume reuses it
  rather than selecting again.** A resumed run stacks its existing upper — with
  the whiteouts it recorded against one lower — so re-selecting could stack it
  on a newer generation that never contained the files those whiteouts delete.
  The manifest version moved to 6 and older records are rejected, not migrated,
  as before.
- **`EnsureProjectStorage` moved ahead of the manifest record.** It is the one
  allocation that is deliberately never rolled back, so it has nothing to gain
  from happening inside the rollback window, and selection needs it.
- **The config schema is inert.** Key inputs are literal relative paths whose
  contents pisafe hashes; there is no command execution, no shell, and no
  caller-chosen mount path. This matters because the file arrives from the
  repository and is parsed on the Mac before any sandbox exists. Globs were
  deferred rather than refused — a monorepo will want `**/package-lock.json`,
  but resolving one raises symlink-escape questions that a literal list does
  not, so it waits for the repository that needs it.
- **The key includes the run image ID.** A tool cache produced by a different
  image should not be restored into a run of this one, for the same reason CI
  keys start with the runner OS. The config does not control this part.
- **Publishing happens when a run stops, regardless of its outcome**, not only
  on `apply`. Under immutability a publish cannot damage anything that exists,
  so gating it behind a deliberate act buys less than it costs in cache misses.
  Stopping is also the one point every ending passes through — `apply` and
  `discard` both stop first — and the last point at which the overlay can still
  be mounted. Publishing at reclamation instead was not taken: an applied run
  is reclaimed a week later by `gc`, far too late to help anything.
  Two consequences. A stopped run that resumes and keeps working publishes
  nothing further under the same key, because that generation now exists; the
  work is not lost, it is simply not shared until the inputs move. And *flagged
  as uncertain*: a run that executed hostile code can leave a snapshot a later
  run restores. The containment is that the later run is itself sandboxed, that
  lockfile integrity checks reject a tampered tarball, and that the cache is
  disposable. If it proves wrong, publishing only on `apply` is a one-line
  change.
- **A publish that fails is recorded on the run, not returned as a stop
  failure.** The run did stop, its workspace is intact, and the only cost is
  that a later run fetches again — failing the command would misreport that.
  Joining it into the returned error was not taken: `apply` and `discard` stop
  a run on the way to something else, and neither may be aborted by a cache.
  `pisafe stop` prints the recorded warning and `pisafe list` marks the run.
- **The merged view is read by a throwaway container and written only by
  pisafe.** The container mounts the run's own overlay and streams a tar to
  standard output; the VM-side script extracts it under `podman unshare`. Bind
  mounting a staging directory into that container and copying inside it was
  not taken, though it is one command shorter: no container ever gets a
  writable handle on the project store, and that is worth keeping true.
- **A generation is stamped when it is published, not when the run last touched
  it.** tar restores the merged view's own timestamp onto the extracted
  directory, and recency is how a namespace is searched, so an unstamped
  generation could sort behind one it supersedes.
- **One generation per namespace, and reset covers a whole project's cache.**
  This settles the open question slice 4 inherited. Keeping three was the first
  choice, on the CI convention, and it was wrong for a fixed 10 GiB project
  image shared by every namespace and the session store: publishing writes a
  full copy before eviction runs, so three kept generations peak at four copies
  of one cache, and a 2.5 GiB npm tree fills the image on its own. One
  generation costs a peak of two and loses almost nothing, because the fallback
  only ever reads the newest: what three buys is an *exact* hit when a project
  alternates between a small set of input states, and the miss it turns that
  into still restores a warm base and fetches the delta. Reversibility: it is
  one constant, `DefaultCacheGenerations`. Reset takes the whole cache rather
  than one namespace or one generation, because the reason to reach for it is
  never "this one generation is wrong". It leaves the session store alone: a
  transcript is not reproducible and so is not disposable.
- **Eviction and reset both protect a generation a recorded run may still
  mount.** overlayfs leaves behaviour undefined when a mounted lower goes away,
  and the run manifests already record which generation each run resolved to,
  so the protected set is read from them rather than tracked separately. An
  active run has one mounted now and a stopped run remounts its own on resume;
  an imported run cannot resume, so it protects nothing.
- **A half-written generation stages as a dot entry inside its own namespace.**
  `ls` hides it, so neither selection nor eviction can see a generation that is
  still being written, and the rename into place is within one directory on one
  filesystem. A separate staging tree was not taken: it would need its own
  cleanup path, while a leaked staging directory here is swept by the same
  reset that empties the namespace.
- **pisafe creates the run-side uppers, not the privileged helper.** The set of
  namespaces comes from the project config and so cannot be known when the run
  filesystem is allocated. Everything the helper creates is owned by the mapped
  UID, so pisafe builds the per-namespace upper and work pair under
  `podman unshare` with no new privilege. This retires the helper's static layer
  list — a helper change, so it forces one VM recreate, worth batching with any
  other.
- **Sessions do not use the cache mechanism.** They keep the plain overlay,
  because isolation is what invariant 3 asks for, but their promotion is
  additive and unkeyed. Slice 2 recorded sessions as riding the cache's
  mechanism; that was right about the mount and wrong about the promotion, and
  this splits them.
- **A second run of a project can read an earlier run's finished transcripts.**
  Delivered by slice 5, which closes the gap slices 1 and 2 opened: they built
  the isolation half of sharing, whose value lives entirely in the publishing
  half, and until this landed the session lower stayed permanently empty and its
  overlay was behaviourally equivalent to a per-run directory.
- **Promotion writes into the mounted lower, rather than avoiding it.** This is
  the one place pisafe adds entries to a directory live runs have mounted, and
  overlayfs calls a lower changing underneath a mount undefined. Two shapes that
  never touch a mounted lower were weighed and not taken: immutable session
  generations reusing slice 4's publish path, which would copy the project's
  whole transcript history forward on every run and could drop a concurrent
  run's transcripts under keep-newest eviction; and dropping the overlay for a
  run-private directory seeded by copying the history in at start, which costs
  that copy on every run. What makes the chosen shape defensible is that the
  undefined part is bounded to one observable: nothing existing is ever written,
  so the only question is whether a live run's listing shows the new names, and
  invariant 3 requires that it not show them. Reversibility: the seeded-directory
  shape stays available and would replace the overlay rather than extend it, so
  changing course later is a rewrite of the sessions mount, not a migration of
  stored data.
- **A name the store already holds is skipped rather than replaced**, which is
  what keeps promotion additive in the one case where names do repeat: Pi
  rewrites a transcript in place to migrate it to the current session version
  when it loads one. Promoting that rewrite would modify a file concurrent runs
  have mounted, to no benefit — each run migrates its own copy on load anyway.
  The consequence worth stating: a migration never reaches the store, and a
  transcript deleted from Pi's own picker inside a run stays in the store.
- **Sessions are never evicted and `reset` leaves them alone.** A transcript is
  not reproducible, so the store grows with the project's history. Bounding it
  belongs with the slice 9 sweep that reclaims whole project filesystems, not
  with a per-namespace rule modelled on the cache.
- **Publishing caches and promoting sessions are joined, not sequenced.** Both
  run after the stop is recorded, both are recorded against the run rather than
  failing a stop that worked, and neither short-circuits the other — an
  unpublished cache costs a later run time, and an unpromoted transcript stays in
  the run's own storage until it is discarded, so silence about either would be
  worse than the failure.
- **The shared cache is a directory pisafe owns, and tools are redirected into
  it by cache-specific environment variables.** Overlaying each tool's own
  location instead — `~/.npm`, `~/.cargo/registry`, `~/go/pkg/mod` — was not
  taken: it needs one upper and work pair per path, and makes the run
  filesystem's shape depend on the list of toolchains. The rule this buys:
  nothing is shared unless a variable puts it there. npm's logs and
  update-notifier timestamp are pushed back out to the run home under the same
  rule — they are per-run, grow without bound, and are useful to nobody else.
- **One privileged helper serves both scopes.** `pisafe-run-storage` became
  `pisafe-storage <action> <scope> <id>` with a `run` and a `project` scope
  rather than gaining a near-identical sibling script; the two differ only in
  their root path, size policy, and subdirectory set. Renaming it changes the
  security profile digest, so this and any later helper change force a VM
  recreate, and such changes are worth batching.
- **The run's upper layers live in a third run-filesystem subdirectory**,
  `overlay/`, which is not bind-mounted into the container. Putting them under
  the existing home needs no helper change but exposes the raw upper, whiteouts
  and all, to the code whose writes it records.
- **A project filesystem is ensured, never created, and never rolled back.**
  Many runs of one project reach it and none may assume it is the first; a run
  that fails after ensuring it leaves it behind, because it is shared state
  that outlives every run. Reclaiming an unused one belongs to `gc`.
- **Shared mounts carry neither `nodev` nor `nosuid`**, because Podman refuses
  any other option alongside `:O`. They are the only mounts in a run without
  them, and it costs nothing: creating a device needs `CAP_MKNOD` and the run
  drops every capability, while `no-new-privileges` already neutralizes a setuid
  bit.
- **Run manifests recorded before per-project storage are rejected, not
  migrated.** A run without a project key cannot be resumed into a project
  cache, and the VM recreate that the profile change forces destroys its storage
  regardless, so the manifest version moved to 5 and old records fail loudly.
- **One live test covers every shared layer**, rather than a per-layer test per
  slice. The property under test is identical across layers, so the test
  iterates the spec's overlays and a layer added later is covered by
  construction.
- **`settings.json` and `trust.json` are copied into the run**, because Pi
  writes both — `/settings`, `pi install`, and `/trust` all do — and a
  read-only mount would turn ordinary Pi use into an I/O error. The copy dies
  with the run, so agent code still cannot change what any later run sees. A
  consequence worth keeping: with the package store read-only, `pi install`
  inside a run fails while `pi -e <package>` still works, because Pi installs
  those to a temporary directory for one run. Trying an extension stays
  ephemeral and keeping one goes through a pisafe command, without pisafe
  enforcing the split itself.

- **The profile mounts at Pi's own global package store**, `~/.pi/agent/npm`,
  rather than at a neutral path of pisafe's choosing. A neutral mount needs a
  second, purely negative one — an empty read-only filesystem over the package
  store — to keep `pi install` from silently succeeding into a run home and
  vanishing at stop. One mount states the property directly: the global package
  store is read-only, so installing globally fails and trying an extension for
  one run still works.
- **A run reaches a profile package by absolute path, not as an `npm:`
  source.** Both spellings load, but an `npm:` source makes Pi re-check the
  installed version at every startup and install when it disagrees, so a
  read-only store turns any disagreement into a broken run start. A local path
  is only stat'ed and read. It also keeps profile packages out of
  `pi update --extensions`, which is what invariant 2 asks for.
- **Each installed extension gets its own npm prefix root.** One shared
  `node_modules` would mean merging dependency trees across installs, which is
  the problem the cache design already refuses to solve, and Pi documents that
  packages load with separate module roots anyway. The cost is duplicated
  dependencies between extensions, which is disk on a bounded filesystem.
- **The profile is a fourth scope of the privileged helper**, not a directory
  pisafe creates. A bind mount is unreadable to a container without the
  container SELinux label, and the helper is what applies it; the helper also
  gives the profile the same root-owned fixed-capacity image and mapped-UID
  ownership every other shared filesystem has. This is a helper change, so it
  moves the security profile digest and forces one VM recreate.
- **There is one profile, named `default`, and no run selects another.** The
  path keeps the name level so a second profile can exist later, but nothing
  reads a profile name from a run: a per-run profile would have to be recorded
  in the manifest, and there is no use for one yet.
- **A run's own workspace is trusted without asking.** *Flagged as a judgement
  call.* Pi's project trust gates whether it loads `.pi/settings.json`,
  `.pi/extensions`, project skills, and system-prompt files from the working
  directory, and defaults to asking. Inside pisafe that guard protects nothing
  the container does not already contain — repository content is exactly what
  the sandbox exists to hold — while leaving it unanswered costs a prompt on
  every run and silently drops a team's project settings in non-interactive
  mode, which is the confusing failure. The consequence worth stating: a hostile
  repository's `.pi/settings.json` loads and can pull its own packages from npm
  inside the run. `pi --no-approve` overrides it for one run, and reverting the
  decision is deleting one line from the trust file pisafe writes.
- **A user-authored global `settings.json` is deferred.** The mechanism that
  makes one possible — settings written into the run rather than mounted — lands
  here, but nothing yet lets a user author the file: it lives in the VM, only
  pisafe writes it, and copying the Mac's own `~/.pi/agent/settings.json` in
  would carry host paths and host tool configuration across the boundary. Until
  a command exists to edit it, the run's settings are what pisafe renders.

- **An install is two containers, and pisafe holds the pin between them.** The
  first asks npm what a spec resolves to and reports the exact version and the
  integrity of that release; the second fetches that version and refuses to
  install bytes that hash to anything else. One container reporting both the
  pin and the tree was not taken: the pin would then be a claim by the same
  process that produced what it describes. This does not make the fetch
  trustworthy — it happens inside a container by necessity, and nothing pisafe
  can do changes that — but it makes the recorded pin something the install was
  checked against rather than a description of whatever arrived.
- **Only npm sources are installable, and only at an exact version.** A git
  source has no integrity hash to pin, and a local path names something inside a
  container the user cannot see. A version the user omits is resolved once and
  recorded, so a spec never means two different profiles.
- **Install scripts do not run.** `--ignore-scripts` matches how the run image
  installs Pi itself. The consequence: a package needing a build step is not
  installable this way. It is defence in depth rather than a boundary — the
  installing container is the same class of sandbox a run is.
- **The record is written after the tree and before a removal.** A record can
  name a package that is missing, which Pi skips silently, but never fail to
  name one that is there — the reverse would leave a run loading something the
  user removed. Two installs at once can lose an entry from the record; the
  loser's tree stays in the profile unrecorded, and re-running the command fixes
  it. A lock was not taken for a single-user command.
- **Replacing an extension swaps the new tree in before the old one goes**, so a
  run starting during an install finds one release or the other rather than a
  path that briefly does not exist.

## Open questions

- **Whether the declarable environment variables need an allowlist.** Slice 3
  refused only what pisafe sets itself, which leaves `CARGO_HOME` declarable:
  it moves the registry cache but also installed binaries and credentials. A
  project declaring it shares those across its own runs only, so no boundary
  breaks — but a compromised run would then read what an earlier one left. This
  stays open because the answer is a documentation question as much as a code
  one: nothing tells a repository which variables are a bad idea.
- **How global tools are installed.** The design lists `pisafe tool install`
  separately from `pisafe extension install`, and the state table has "Global
  tools" as its own row. Whether those are npm globals in the profile, a second
  read-only mount, or something built into the image is undecided. This is why
  slice 8 has no shape yet.
- **Whether a moved repository can adopt its orphaned store.** A rebind command
  is possible and does not change the key scheme, so this can stay deferred.

## Verification

Slice-by-slice live tests are listed under [Slices](#slices). They need a
provisioned VM and a built run image:

```sh
PISAFE_LIVE_LIMA=1 go test ./internal/lima/ -run TestLiveCreateAndStart
PISAFE_LIVE_LIMA=1 go test ./internal/runimage/
limactl shell pisafe -- podman images --no-trunc --format '{{.Id}} {{.Repository}}'
PISAFE_LIVE_LIMA=1 PISAFE_LIVE_RUN_IMAGE=sha256:<id> go test ./...
```

Any change to the VM definition moves the security profile digest, so the VM
must be deleted and recreated before those tests pass.

Substrate checks already run, for re-running after a Podman or Pi bump:

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

The lower's contents must be owned by the mapped UID (`podman unshare chown -R
1000:1000 …`) or the overlay is read-only in practice. Probe directories are
owned by mapped UIDs too, so clean them with `podman unshare rm -rf`.
