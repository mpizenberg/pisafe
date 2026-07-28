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

## Substrate

Verified against the pinned image and the live VM, not inferred from
documentation. Reproduce with the commands under [Verification](#verification).

- **Shared state is an overlay, not a shared mount.** Podman mounts it
  rootlessly: `-v <lower>:<dst>:O,upperdir=<upper>,workdir=<work>`. The lower
  is the project store, the upper lives in the run's own filesystem. Two
  containers on one lower were shown to hold conflicting states — one shadowed
  a shared file and added another, the second still read the original and saw
  neither, both uppers survived exit, the lower stayed byte-unchanged. No new
  VM privilege: the root helper gains per-project filesystem creation, not
  mounting.
- **The overlay spans two filesystems.** A lower on the project's ext4 image
  and an upper on the run's own was mounted and written. Podman is particular
  about the rest: `:O` is refused alongside any other mount option, so the
  cache mount cannot carry `nodev,nosuid`; `--mount type=overlay` does not
  exist, so `-v` is the only spelling; and both `upperdir` and `workdir` must
  already exist, so the run filesystem is created with them.
- **npm redirects into that overlay.** `npm_config_cache` moves the whole
  cache, and `npm_config_logs_dir` keeps logs out of it.
- **Pi writes its transcript where the environment says.** With
  `PI_CODING_AGENT_SESSION_DIR` set to the overlay, a real run's session file
  landed in the run's private upper and the project lower stayed empty. The
  file is written before any inference succeeds, so this is observable without
  a provider.
- **Ownership follows the existing rule.** The overlay is writable only when
  the lower's contents are owned by the container's mapped UID, the same
  `subuid_start + 999` the run-storage helper already derives.
- **The global profile is a read-only mount** named by absolute path in the
  `packages` array of `settings.json`. A package on a genuinely read-only mount
  was loaded and listed by the pinned Pi.
- **`PI_PACKAGE_DIR` does not relocate installed packages.** It locates Pi's
  own installation for Nix and Guix store paths; an install with it set still
  went to `$PI_CODING_AGENT_DIR/npm`. Do not build on it.
- **Sessions relocate.** `PI_CODING_AGENT_SESSION_DIR` is read by Pi — the
  literal never appears in `dist` because the name is assembled from `APP_NAME`
  at build time — and `--session-dir` overrides it. Both are ours to set.
- **`settings.json` and `trust.json` must be copied in, not mounted
  read-only**, because Pi writes both. The copy dies with the run.

### Layout

Project key: directory slug plus the first eight hex characters of the SHA-256
of the Mac-side Git root path, e.g. `api-3f9c2a1b`.

```text
/var/lib/pisafe/global/<profile>            read-only mount, listed in packages
/var/lib/pisafe/projects/<key>/cache/       overlay lower → /cache
/var/lib/pisafe/projects/<key>/sessions/    overlay lower → session dir
/var/lib/pisafe/runs/<run>/workspace        bind mount → /work
/var/lib/pisafe/runs/<run>/home             bind mount → /home/node
/var/lib/pisafe/runs/<run>/overlay/<layer>/ the run's private upper and work
```

`cache` and `sessions` are the shared layers. One name files a layer in three
places — the project lower, the run's upper and work pair, and the container
path — so adding one is a single entry in `runcontainer.ProjectLayerNames`,
which the privileged helper is templated from and the security profile digest
covers.

Both the project and the run filesystem are root-owned fixed-capacity ext4
images allocated by the same privileged helper, which is the only thing that
mounts anything.

## Slices

- [x] **0. Substrate scouting.** Establish the mechanisms above against the
      live VM. Commit `ce7cccb`.
- [x] **1. Per-project storage and the dependency cache.** Extend the
      root-owned helper from per-run to per-project filesystems; derive the
      project key; wire the cache overlay into `runcontainer.Spec`. Live test:
      two concurrent runs of one project, both reading the seeded cache,
      neither seeing the other's writes, the lower unchanged.
- [x] **2. Sessions on the same mechanism.** `PI_CODING_AGENT_SESSION_DIR`
      points at a second per-project overlay, and the layer list is one
      declaration the helper and the mount check are both derived from. Live
      test: `TestLiveProjectLayersAreSharedToReadAndPrivateToWrite` now covers
      every shared layer — two concurrent runs read the seeded project state
      and neither sees the other's writes.
- [ ] **3. Promotion.** Fold a run's upper into the project store —
      deliberately, and only while no run holds that lower. Decide the trigger
      and the surface. Live test: a promoted cache entry is present for the
      next run; a refused promotion leaves the store untouched.
- [ ] **4. Global profile and `pisafe extension install`.** Read-only profile
      mount, copied-in `settings.json`/`trust.json`, installer pinning exact
      version and integrity hash. Live test: a pinned extension loads in a run;
      `pi install` inside a run fails; `pi -e` still works.
- [ ] **5. `pisafe extension update` and update notifications.** Offered, never
      applied. Depends on how pinning is recorded in slice 4.
- [ ] **6. Global tools.** See the open question below — the mechanism is not
      yet decided.
- [ ] **7. `gc` sweep of caches and session stores.** The design already
      specifies this; it has been a Known gap only because the targets did not
      exist.
- [ ] **8. Other providers.** Additional `pisafe login <provider>` commands
      over the existing broker interface.
- [ ] **9. Backup, reset, recovery.** Cheapest useful piece is resetting or
      removing a scope, which is nearly free once scopes exist.

## Decisions

Appended as they occur. These fold into `DECISIONS.md` when Phase 2 lands.

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
- **Promotion is deliberate, not automatic on run exit**, and may happen only
  while no run holds that lower layer, because overlayfs leaves behaviour
  undefined when a mounted lower changes underneath it. A run that executed
  hostile code should also not fatten the shared layer merely by exiting. If
  the no-live-run condition proves too restrictive, the escape hatch is
  versioned lower generations, where a promotion writes a new generation and
  live runs keep the one they mounted; that would also give `gc` another
  natural sweep target.
- **Sessions use the same overlay as caches**, rather than the run-local copy
  the design's state table names. The semantics are what invariant 3 asks for
  without paying for a copy, and session files are uniquely named per session
  ID, so promotion is pure addition with no conflicting paths. This re-derives
  a mechanism an earlier draft of the design sketched and dropped.
- **The global profile is a read-only mount named by absolute path** in the
  `packages` array. Relocating Pi's package store with `PI_PACKAGE_DIR` was
  investigated and does not work, as recorded above.
- **The shared cache is a directory pisafe owns, and tools are redirected into
  it by cache-specific environment variables.** One overlay is mounted at
  `/cache` and `npm_config_cache` points npm there. Overlaying each tool's own
  location instead — `~/.npm`, `~/.cargo/registry`, `~/go/pkg/mod` — was not
  taken: it needs one upper and work directory per path, which would make the
  privileged helper's idea of a run filesystem depend on the list of
  toolchains. This replaces the question of which paths are shared with a
  rule: nothing is in the shared layer unless a variable puts it there, so the
  layer can never accumulate arbitrary state. npm's logs and update-notifier
  timestamp are pushed back out to the run home for the same reason — every
  remaining path under `/cache` is content-addressed, which is the whole basis
  for merging it last-writer-wins.
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
- **The cache overlay carries neither `nodev` nor `nosuid`**, because Podman
  refuses any other option alongside `:O`. It is the only mount in a run
  without them, and it costs nothing: creating a device needs `CAP_MKNOD` and
  the run drops every capability, while `no-new-privileges` already neutralizes
  a setuid bit.
- **Run manifests recorded before this change are rejected, not migrated.**
  A run without a project key cannot be resumed into a project cache, and the
  VM recreate that the profile change forces destroys its storage regardless,
  so the manifest version moves to 5 and old records fail loudly.
- **The set of shared layers is one declaration.** `runcontainer` names them,
  the privileged helper is templated from that list, and the container mount
  check derives its expectations from the same place. Spelling the directory
  set out separately in the provisioning script was not taken: adding a layer
  would then silently produce a run filesystem with no upper for it, and Podman
  would fail at container start rather than at the boundary check. The security
  profile digest now covers the list, so a VM provisioned with an older set is
  rejected instead of half-working.
- **One live test covers every shared layer**, rather than the per-layer test
  this plan named for each slice. The property under test is identical for
  cache and sessions, so the test iterates `ProjectOverlays()` and a layer
  added later is covered by construction.
- **A second run of a project still cannot read an earlier run's finished
  transcripts.** That half of this slice's stated test needs promotion, which
  is slice 3; the live test seeds the project lower to stand in for it, exactly
  as the cache test does. What slice 2 delivers is the other half — concurrent
  runs cannot see one another's live transcripts — plus the shared layer that
  promotion will write into.
- **`settings.json` and `trust.json` are copied into the run**, because Pi
  writes both — `/settings`, `pi install`, and `/trust` all do — and a
  read-only mount would turn ordinary Pi use into an I/O error. The copy dies
  with the run, so agent code still cannot change what any later run sees. A
  consequence worth keeping: with the package store read-only, `pi install`
  inside a run fails while `pi -e <package>` still works, because Pi installs
  those to a temporary directory for one run. Trying an extension stays
  ephemeral and keeping one goes through a pisafe command, without pisafe
  enforcing the split itself.

## Open questions

- **How a toolchain without a cache-only variable is shared.** Cargo is the
  known case: `CARGO_HOME` moves the registry cache but also installed
  binaries and credentials, which the shared layer must not hold. Nothing in
  the run image needs it yet, so the answer can wait for the toolchain that
  forces it.
- **What triggers promotion, and what the user sees.** Deliberate is decided;
  whether that means a flag on `apply`, a separate command, or a prompt is not.
- **How global tools are installed.** The design lists `pisafe tool install`
  separately from `pisafe extension install`, and the state table has "Global
  tools" as its own row. Whether those are npm globals in the profile, a second
  read-only mount, or something built into the image is undecided. This is why
  slice 6 has no shape yet.
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

# Profile loading from a genuinely read-only mount.
limactl shell pisafe podman run --rm --user 1000:1000 --network=none \
  --mount type=bind,src=<profile>,dst=/opt/pisafe/profile,ro \
  <run-image> sh -ec 'pi list'
```

The lower's contents must be owned by the mapped UID (`podman unshare chown -R
1000:1000 …`) or the overlay is read-only in practice. Probe directories are
owned by mapped UIDs too, so clean them with `podman unshare rm -rf`.
