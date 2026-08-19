# Implementation progress

Last updated: 2026-08-11

What `pisafe` does now, what has been verified against a real VM, and what is
missing. [`pisafe-design.md`](pisafe-design.md) is the authority on what must
hold and [`DECISIONS.md`](DECISIONS.md) records why the implementation looks as
it does. This document describes the present; git history holds the rest.

Every package under `internal/` carries a doc comment saying what it owns, so
`go doc ./internal/...` is the map. What follows is what neither those comments
nor the design document already say.

## Current state

Phase 1 (isolated runs) and Phase 2 (managed persistence) are both complete.
Every command the design enumerates exists, has unit coverage, and has been
exercised against the live VM. Nothing further is planned: what is left is the
Known gaps list below.

Dependency-free Go module. `cmd/pisafe` runs on the Mac and `cmd/pisafe-guest`
is a static Linux ARM64 binary shipped into the run image; the two are separate
artifacts that must be rebuilt together, which the controller enforces by
refusing a helper not carrying `guestcall.Contract`.

`pisafe run` materializes inside private quota-backed VM storage over the tested
mountless path. **Do not add a local-workspace fallback.**

## Pins and versions

```text
Fedora 44 ARM64 cloud image
  sha256:55c60a3b80d3616a08705afd0459e75fe9f03c54aba7a46e4002a41a72fa0d5b
Node root image (ARM64 manifest)
  sha256:af01d58b748ec92b1d6e8e11429aad424fd1e68c848185399dca0596a1ab8f5c
@earendil-works/pi-coding-agent@0.82.0, tarball checked against registry SHA-512
CPython 3.13.14, installed by digest-pinned uv; pnpm digest-pinned
```

Run manifest version **7**; project record version **1**, never moved. Lima
≥ 2.2.0, instance `pisafe`, `plain: true`, 4 CPUs, 8 GiB, 64 GiB sparse disk
plus a second 64 GiB `pisafe-state` disk carrying `/var/lib/pisafe`. Each run
gets one sparse 10 GiB ext4 filesystem and 24 hours from creation. `cp`
is bounded at 4096 entries, 1 GiB total, 256 MiB per file; a run's apply
response at 1 MiB.

Pi's published `npm-shrinkwrap.json` freezes its tree, and the three packages it
names without an integrity hash — `pi-agent-core`, `pi-ai`, `pi-tui` — are
re-fetched by exact version against digests recorded beside `PiIntegrity`. A
unit test fails the build if those digests do not move with `PiVersion`.

## Current behavior

Beyond what the design requires:

- Every VM records a root-owned SHA-256 of its complete generated definition and
  the canonical host-network set, checked before clock or firewall verification.
  Both that check and the firewall check name `pisafe vm rebuild`.
  `VM.StartUnverified` serves the commands deliberately exempt from it, which
  start a stopped VM rather than reporting one.
- Any state Lima calls neither running nor stopped is `StatusBroken`: the
  instance can be replaced, but nothing may be concluded about what runs in it.
- `runimage.Installer` derives a recipe digest from the exact Containerfile and
  guest-helper bytes and labels the image with it; reuse requires matching
  recipe/base/Pi labels, Linux/ARM64, and a valid immutable ID. Artifact loading
  rejects symlinks, file swaps, oversize inputs, non-ELF or non-ARM64 helpers,
  dynamic interpreters, imported shared libraries, and a helper without the
  contract.
- Every container is rendered from one hardened base: immutable `sha256:` ID,
  UID/GID 1000, read-only root, all capabilities dropped, `no-new-privileges`,
  bounded memory, swap, and PIDs, and opt-in networking. Exactly one may execute
  out of its scratch `/tmp` — the half of `packageArgs` that installs.
- A run is active only while its container runs. Rebooting or recreating the VM
  keeps storage and no containers, so `resume` adopts such a run; a container
  still running keeps the refusal.
- A run expires 24 hours after `created_at`, derived rather than stored, and no
  record holds elapsed time. Stopping and resuming do not move it, `list` names
  an expired run expired without asking the VM, and an expired run still stops,
  diffs, copies, and applies. Each container start passes what is left to
  Podman's `--timeout`.
- What `--include` records is the path named plus the files under it, each with a
  SHA-256 — the copy back compares against those. A root that carried no files is
  created as a directory in the run. Included paths never enter the run's Git
  history, so `--include` composes with `--drop-baseline`.
- Untracked leftovers are listed once per run with an untracked directory
  standing for its contents; a request naming one is expanded from the filesystem
  so per-file checks still see one file at a time.
- Submodules go one level deep, each with its own bundle, patch, and baseline
  commit. Nested submodules and Git LFS fail closed; external diff/textconv
  drivers are disabled during capture; parsing is NUL-delimited.
- Apply is journaled — `PrepareApply` → `ImportApply` → `CommitApply` /
  `RollbackApply` — and a recorded plan is always finished, never redone. A
  failed import rolls back its own part-built journal, and bundles fetch into
  `refs/pisafe/incoming/<run>` with a forced refspec, so a ref left by a killed
  import is overwritten rather than refusing the next apply. A ref holding a
  commit other than the one imported there is never deleted.
- `ImportApply` checks that every submodule pointer the run moved names a commit
  the repository at that path holds. Apply's copy into the working tree only adds
  and updates, runs after the refs move, and is finishable with the VM gone.
- Every command that takes a run accepts none and resolves to the one unimported
  run of the current checkout; `discard` is deliberately outside this, and
  nothing inside a run takes part in the choice.
- `Store.List` and `Store.ListProjects` both separate records this version can
  read from ones it cannot. An unreadable run record is listed with its reason
  and the discard that releases its workspace. An unreadable project record is
  reported by its key and never acted on: `gc` neither stamps nor reclaims it,
  and `backup` copies its transcripts out under that key while leaving it out of
  the manifest. A `.json` file not named like a key is not a record at all.
- While any run record cannot be read, nothing is evicted from a cache and no
  project store is reported idle, because what is held is unknowable.
- `pisafe list` renders each record against one `podman ps`; `connect`, `zed`,
  and `forward` resume a run the VM stopped before handing over, in about 150 ms
  when nothing is wrong.
- Every configured upstream is served at once, each on a route led by the
  provider's own name, authorized by one run capability. A provider record holds
  no credential: endpoint, wire format, and model list come from pisafe's table.

## Platform constraints

Established against the live VM and the pinned image; worth re-checking after a
Podman, Lima, or Pi bump.

- Podman refuses `:O` alongside any other mount option, so shared mounts cannot
  carry `nodev,nosuid`; `--mount type=overlay` does not exist, and both
  `upperdir` and `workdir` must already exist. The overlay may span filesystems.
- overlayfs leaves behaviour undefined if a mounted lower is modified — a kernel
  constraint, which is why shared state is written by creating new directories.
- The lower must be owned by the container's mapped UID (`subuid_start + 999`) or
  the overlay is read-only in practice, with nothing failing at mount time to say
  so. Everything the privileged helper creates is writable by the pisafe user
  under `podman unshare`, so pisafe can build structure inside it with no new
  privilege.
- A bind mount is unreadable without the container SELinux label, failing as
  `EACCES` rather than a mount error. A mountpoint Podman creates inside the run
  home is root-owned. Podman reports a read-only bind as `RW: false` rather than
  an `ro` option, so a check reading only mount options cannot tell a read-only
  profile from a writable one.
- Fedora Podman reports image `Id` as bare 64-char hex where run specs need
  `sha256:` form, has no `podman cp --chown`, and rejects `uid=`/`gid=` in
  `--tmpfs` (so `/run` uses `type=tmpfs,...,U=true`). Its local-volume `size`
  needs root and XFS while the pinned VM uses Btrfs, whose qgroups are bypassable
  with nested subvolumes.
- A VM-loopback published port cannot cross the firewall, which denies loopback
  to the unprivileged Lima user.
- Pi writes its transcript where `PI_CODING_AGENT_SESSION_DIR` says; a relocated
  session directory is flat, and listing one filters by each transcript's own
  recorded cwd. Names are `<ISO timestamp>_<randomUUID>.jsonl`, appended a line
  at a time, and Pi never locks a session file — it locks `settings.json`,
  `trust.json`, and its auth store, and rewrites a transcript only to migrate it.
- Pi installs global packages under `$PI_CODING_AGENT_DIR/npm`, which
  `PI_PACKAGE_DIR` does not relocate. A package on a read-only mount loads and
  runs. One named by absolute path is inert; one named `npm:` is not, because Pi
  compares versions at every startup.
- `npm pack --dry-run --json` reports the version and integrity of what a spec
  resolves to, scoped packages included — the pin without fetching.
- System `tar` pads output past the end-of-archive marker to its blocking factor,
  so an extractor stopping at the marker leaves the sender blocked on a closed
  pipe.
- Zed applies a new saved connection's arguments at a 100 ms file-watcher delay
  and not at none.
- Lima 2.2.0 repeatedly booted a stopped VZ VM without restoring SSH, `vzNAT`
  failing identically. Fresh creation is reliable; stopped-VM restart remains an
  upstream gap.

## Tests and verification

```sh
go test -race -cover ./...
go vet ./...
go build -trimpath -buildvcs=false ./cmd/pisafe
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
  go build -trimpath -buildvcs=false ./cmd/pisafe-guest
git diff --check
```

The suite is green, including a run whose expiry is pinned across a stop and a
resume, an expired run refusing to resume, and `list` naming an expired run
expired with the VM unasked. Coverage runs from 96% (`profile`) down to 33%
(`cli`) and
31% (`hostnet`, whose remaining functions read the Mac's real interfaces and
routing table); `guestcall`, `piagent`, and `providers` have no test file of
their own and are covered through the CLI, broker, guest, and controller suites.
The generated Lima YAML is checked by the installed Lima validator.

Unit and integration coverage runs mostly against real repositories with a fake
VM boundary, and covers broker capability auth and fail-closed paths, SSE
fidelity and the Codex JWT wrapper; input selection and archive path safety;
submodule reconstruction and the journaled apply including rollback and
contested refs; diff and cp in both directions; collection, discard, and image
pruning, including records no version can read; the baseline keep/drop choice on
both sides of the boundary; git identity; run-image pin assertions; declared
caches; publishing, promotion, and reset; project drop and rebind; the profile,
its pins, and its offers; backup and restore; and provider records that cannot
route around the broker.

Gated live suites:

```sh
PISAFE_LIVE_LIMA=1 go test -v ./internal/lima -run TestLiveCreateAndStart
PISAFE_LIVE_LIMA=1 go test -v ./internal/runimage
limactl shell pisafe -- podman images --no-trunc --format '{{.Id}} {{.Repository}}'
PISAFE_LIVE_LIMA=1 PISAFE_LIVE_RUN_IMAGE=sha256:<image-id> go test -v ./...
PISAFE_LIVE_STATE_DISK=1 go test -v ./internal/lima -run TestLiveStateDisk
```

Anything that mounts a run needs the immutable ID of a locally built run image,
which is why the image list comes before the last command. Any change to the VM
definition moves the security profile digest, so the VM must be deleted and
recreated before these pass. The state-disk test is separate because it proves
its property by deleting the instance holding it, on a throwaway instance.

Verified against a real ARM64 VM: the boundary (no `/Users`, no Podman socket,
expected subordinate ID mapping, firewall active, IPv6 off, public HTTPS from a
rootless container while RFC1918 and metadata fail, narrow sudo only); traffic
shaped to look permitted — wildcard-DNS answers into denied ranges, redirects
into link-local, unanswered UDP, and every host-reachable address on port 22 all
refused, each re-run against a permitted destination to prove the ruleset rather
than a dead path; the state disk outliving its instance; frozen npm resolution in
the built image; container hardening at runtime; staging, SSH, and `ssh -L`
forwarding; the lifecycle `run → stop → resume → connect → apply → discard`;
`diff` and `cp` against active, stopped, and imported runs; the baseline choice
on three scratch repositories; `gc` by aging real timestamps; the broker relay
and a real ChatGPT subscription driving Pi with no provider credential anywhere
in the run; shared project layers, cache selection and publishing, and session
promotion; the profile staying read-only while `pi install` succeeds in a run;
pinned installs, offers, and the toolchain; project reclamation and rebind; and
backup and restore end to end, including a second backup into the same directory
adding without removing.

`pisafe profile reset` has no live test, deliberately: there is one profile and
nothing scopes a test to a profile of its own, so a live test would empty
whatever the user has installed.

## Live VM state

A persistent Lima instance named `pisafe` is left running with security profile
`sha256:906c5bd13b53594ed1513187e804705e301873b3f0969758eac460d8345fb20c`,
holding the current managed run image:

```text
recipe digest: sha256:4305e790f9baf24b9b44ee350006192fe166f364ba559b96b71e04b25d3db91d
image ID:      sha256:2e6d954bbbc57ad81193d03285bd60ca25eebb3a806a5a81803629b7f90925c5
```

When a recorded digest and a rebuild disagree, the rebuild is right: the recipe
derives from the exact Containerfile and guest-helper bytes.

The VM once entered `VirtualMachineStateError` unattended overnight, almost
certainly on host sleep, while `limactl list` still reported `Running` because
the host agent survived. `limactl stop --force` then `limactl start` recovered
it with the disk and both runs intact.

**An agent inside a restricted filesystem sandbox may be unable to connect to
`~/.lima/pisafe/ha.sock`, and `limactl list` then falsely labels the instance
`Broken`. Run the diagnostic with the needed host permission before acting on
that status; never delete or recreate the VM based on the sandboxed result.**

## Known gaps

Getting work out:

- `pisafe diff` lists untracked paths with `--exclude-standard`, so work under an
  included root the repository ignores is invisible while the run is going. It
  arrives at apply, and the roots are in the snapshot, so `diff` has what it
  needs to report them separately.
- `pisafe diff` reports paths and line counts, never content, so reviewing what a
  run wrote means importing it or taking single files out with `cp`, which
  refuses any symlink rather than recreating one that stays inside the tree.
- A run leaving roughly twenty thousand untracked files exceeds the 1 MiB apply
  response and fails instead of reporting them; returned included work shares
  that budget.
- A run whose submodule carried uncommitted work can only import its baseline,
  never replay without it: separating the histories means rewriting the run's
  commits to follow the submodule's new commit IDs.
- The replay checks the run's tree out a second time inside run storage, so a
  repository large enough to fill the 10 GiB filesystem fails it — safely, but
  the message is a disk error rather than an explanation.
- Apply uses the controller's current run image, which must still exist in the
  VM; a pruned image fails apply as it fails resume.
- Runs created before activation recorded submodule baselines have none, so
  `diff` measures their submodules from the staged head. No such run exists here.

Lifecycle and records:

- A project record this version cannot read is reported and its transcripts still
  export, but removing it means naming the checkout it belonged to — exactly what
  could not be read, since `project drop` takes a path and derives the key. The
  key is shown and opens with the checkout's directory name, so a user who
  recognises the project can still drop it. Nothing takes a key directly.
- Collection never reclaims an unimported run, even one a check could prove holds
  no commits, because `diff` sees the repository but not the run's home. Their
  10 GiB filesystems are reclaimed by explicit discard.
- A reclaimed run leaves no record, so `pisafe list` shows only runs that still
  own something; which runs existed is answerable from the `pisafe/<run>`
  branches and the reflog.
- Retention is measured against the Mac's clock with no grace for a manifest
  whose timestamps are in the future; such a record is never collected.
- `pisafe connect` hands over the terminal and reports nothing afterwards, so a
  session that ended at the wall-clock limit looks like one the user quit.
- `pisafe zed` launches a connection saved once in Zed. Fully automatic first
  launch is intentionally absent because Zed's CLI URL cannot carry `-F`.

Shared state and profile:

- A session store grows without bound: nothing evicts a transcript, `project
  reset` leaves them, and only dropping the whole store reclaims one — so a
  long-lived project is bounded by its 10 GiB filesystem and nothing warns first.
- Declarable cache environment variables have no allowlist. A project may declare
  `CARGO_HOME`, which moves installed binaries and credentials as well as the
  registry cache. No boundary breaks, since it is shared across that repository's
  own runs only, but nothing tells a repository which variables are a bad idea.
  *Open question, waiting on a real repository.*
- Cache key inputs are a literal list of relative paths, so a monorepo cannot
  name `**/package-lock.json`.
- A run that executed hostile code can publish a cache generation a later run
  restores. *Flagged as uncertain*; containment and fallback are in
  [`DECISIONS.md`](DECISIONS.md).
- A package needing an install script cannot be installed, because both install
  containers run with `--ignore-scripts`.
- Tools have no update offer, which extensions get. *Open question, waiting on a
  real need.*
- Two installs at once can lose an entry from the profile record; the loser's
  tree stays unrecorded and re-running the command fixes it.
- A restore reinstalls from npm, so a release unpublished since the backup cannot
  be put back. What the backup holds is enough to say what was lost.
- There is no way to author a global `settings.json`. The mechanism exists —
  settings are written into the run rather than mounted — but nothing edits it.
- `pisafe profile reset` has no live test. A second profile name, which the
  layout allows and nothing reads, is what would close this.

VM and inference:

- A crashed VM leaves `limactl list` reporting `Running` while the VZ machine is
  in `error`, visible only in `ha.stderr.log`. Nothing detects this, so the first
  command to touch the VM fails with an opaque SSH reset.
- Stopped-VM recovery previously failed here over both default user-mode
  networking and `vzNAT`. One force-stop and start regained SSH with runs intact;
  one success is not a reliable recovery path.
- Security-profile drift fails closed and `pisafe vm rebuild` is the cure, but
  nothing replaces a VM automatically: deleting one is destructive and stays an
  explicit act.
- While no broker is connected, a process escaped to the unprivileged VM user
  could bind `192.0.2.1:18080` itself. It gains nothing beyond what that user
  already has, and a real broker then fails loudly at bind time.
- Broker-side token refresh has only ever run against a stub: the live session
  never crossed an access-token expiry.
- npm resolution inside the run image is frozen, but the `apt-get` layer floats,
  so two builds of one recipe digest are equivalent in Pi and its dependencies
  without being byte-identical.

## What comes next

Nothing is planned. What is left is the Known gaps list above: two open
questions waiting on a real repository, and otherwise deliberate refusals whose
reasoning is in [`DECISIONS.md`](DECISIONS.md) or small things nobody has needed.

The one piece of upkeep that is not optional: the ChatGPT OAuth flow, the
embedded model catalog, and the three sibling digests all mirror the pinned Pi
release, and every one must be re-checked when `PiVersion` moves. That backend
is an unofficial surface that can change without notice; inference then fails
closed and loudly.

Do not weaken the boundary for any of it.
