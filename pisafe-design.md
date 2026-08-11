# `pisafe` Design Specification

Date: 2026-07-23
Status: open egress with structural credential isolation, after weighing
maintenance cost against the threat model

This document states what must hold. The reasoning behind individual choices is
in [`DECISIONS.md`](DECISIONS.md); what the code currently does, and what has
been verified, is in [`IMPLEMENTATION_PROGRESS.md`](IMPLEMENTATION_PROGRESS.md).
Where the design and the implementation disagree, this document is the authority
on what must be true.

## The guarantee

A run executes in a dedicated, mountless Linux VM, in one rootless container per
run, over a staged copy of the repository transferred as Git bundles. Internet
egress is open; the Mac, the LAN, link-local, and metadata addresses are not.

This deliberately trades exfiltration resistance for simplicity and zero network
prompts. Pi can edit files, browse, download, and install anything without
asking. The boundary instead guarantees two things:

1. A run cannot touch the Mac or the original checkout.
2. `pisafe` never hands a run any reusable user credential, so nothing it
   executes can act as the user.

The only credential `pisafe` creates is a revocable, run-scoped inference
capability. An earlier draft included a dynamic approval proxy and a GitHub
credential broker; they were removed as a maintenance and interruption cost
disproportionate to the mostly public, non-secret projects this tool targets.

## Requirements

- Protect the Mac and everything outside the selected project from project
  content, model output, dependencies, and unreviewed Pi extensions.
- Pi must modify the project and commit autonomously; the working copy may be
  staged as long as applying the result is easy, and the staged files must be
  editable over Zed Remote SSH.
- Nothing in a run may push, publish, deploy, or otherwise mutate as the user.
- Internet reads, package installs, and tool installs need no approval. The
  user's private repositories and registries stay unavailable; making them
  available reopens the credential-broker question and is out of scope.
- Untracked and ignored files are excluded unless explicitly selected, and
  secret-bearing files only as an unsafe override. There is no separate
  secret-injection mechanism. An explicitly selected path crosses as files in
  both directions and never as Git history; the copy back is additive and never
  leaves the paths the user named.
- Submodules must stage and apply. Git LFS is out of scope and must fail closed.
- Provider login persists on the Mac across runs; Pi extensions, settings,
  tools, and sessions persist inside the sandbox.
- Concurrent runs are independent, and completed runs may be reclaimed
  automatically after seven days.

## Trust model

Untrusted: model output and commands, project files and project-local Pi
resources, dependencies and lifecycle scripts, Pi catalog extensions, programs
downloaded during a run, and Zed language servers, tasks, and remote extensions.

The trusted computing base is deliberately small: the `pisafe` Mac controller,
the inference broker, the VM and container runtimes, a pinned base image, a
minimal provider integration, and the staging and Git import/export code.

Pi itself and its catalog extensions run inside the isolated environment. This
is why host Pi plus sandboxed shell tools is not sufficient for this threat
model: an extension runs in the Pi process and could otherwise access the host.

## Architecture

```text
macOS
├── original repository (never mounted into the VM)
├── pisafe controller
│   └── model credential/inference broker
├── macOS Keychain (provider refresh tokens)
└── dedicated ARM64 Linux VM (no host filesystem mounts)
    ├── static firewall: internet open; loopback/LAN/metadata denied
    ├── state disk, outliving the instance
    │   ├── shared read-only tools/extensions profile
    │   ├── per-project session and dependency-cache storage
    │   └── per-run quota-backed filesystems
    └── one rootless container per run
        ├── Pi and unreviewed extensions
        ├── staged Git repository
        ├── run-local writable home
        ├── Zed remote server, terminal, tasks, and language servers
        └── no reusable user credentials; only a run-scoped
            inference capability
```

The VM is a dedicated Lima ARM64 instance in `plain` mode — no filesystem
mounts, no SSH-agent forwarding, no dynamic port forwarding, no guest agent —
provisioned with rootless Podman. Never mount `/Users`, the repository, the
Docker socket, or the host SSH agent. A general-purpose Podman machine will not
do: it exposes `/Users`, `/private`, and `/var/folders` read-write, so a
container escape reaches Mac files without a hypervisor escape.

Everything the VM cannot rebuild sits on a second virtual disk owned by Lima
rather than by the instance: every run's filesystem, every project's transcripts
and caches, and the shared profile. The instance's own disk carries only the
distribution, Podman, and the run image, each of which provisioning reproduces.
Recreating the VM is therefore the cure for every drift the boundary checks
detect without also being what destroys the work they protect, and `pisafe vm
rebuild` is that cure as a command: it reports what the rebuild costs, stops
every active run so the stretch is charged from its container's own account,
replaces the instance, and verifies the boundary the new one was built to. The
disk is
identified by the filesystem label it is given the first time it is seen; a
device carrying neither a partition table nor a filesystem is the only thing
provisioning will ever format, and finding anything but exactly one of those
fails the boot.

Each run gets its own container, staged repository, quota-backed writable
storage, SSH endpoint, and broker capability, plus a read-only global profile
and per-project session and cache volumes. Containers run as a non-root user
with a read-only root, dropped capabilities, `no-new-privileges`, no container
socket, and bounded CPU, memory, processes, and temporary filesystems. Product
policy caps a run at 10 GiB of persistent storage and eight cumulative active
hours. Multiple runs for one project are independent snapshots: they do not see
one another's staged files or conversational context.

## Command-line experience

`pisafe run` validates the current repository and records its HEAD, reports
untracked and ignored files and excludes them unless explicitly selected,
creates the staged repository inside the VM, imports tracked working-tree
changes as a baseline commit when needed, starts the run container and Pi, and
prints the run ID, staged path, branch, and the SSH command for Zed. The run
survives terminal or editor disconnection. Output stays short:

```text
Run:       api-refactor-20260723-1432
Workspace: /work/api-refactor
Zed:       ssh://pisafe-api-refactor/work/api-refactor
Branch:    work/api-refactor-20260723-1432
```

The Zed terminal connects to the same run container as Pi, so it sees the same
files, tools, dependency cache, Git commits, and network policy.

```text
pisafe list
pisafe connect [run] [-- <command>...]
pisafe forward [run] [<local>:]<port>...
pisafe zed [run]
pisafe stop [run]
pisafe resume [run]
pisafe diff [run]
pisafe cp [<run>]:<path> [dest]
pisafe cp <path> [<run>]:[<path>]
pisafe apply [run]
pisafe discard <run> --confirm <run>
pisafe gc [--dry-run]
pisafe vm rebuild [--confirm] [--discard-state]
pisafe doctor

pisafe login [chatgpt|anthropic|openai|<name> --url --api --models]
pisafe logout <name>
pisafe broker

pisafe extension install <package>[@<version>]
pisafe extension update [package...]
pisafe extension remove <package>
pisafe extension list
pisafe tool install <package>[@<version>]
pisafe tool remove <package>
pisafe tool list

pisafe project list
pisafe project reset [path]
pisafe project drop <path> --confirm <path>
pisafe project rebind <old-path>
pisafe profile reset --confirm
pisafe backup <directory>
pisafe restore <directory>
```

`connect` opens a shell in the run's workspace, from which Pi is one word away;
a command after `--` runs there instead and the terminal's exit status is its
own. The shell is the default because it reaches every other state a run can be
worked in, while Pi replaces the session it was started in and so reaches none.
`zed` prints the path and opens Zed. `diff` reports what a run changed without
stopping it, and without printing file content, which would make the terminal
an injection surface. `cp` takes build artifacts, logs, or screenshots out of a
run, and puts into one what the run cannot fetch for itself; because the
outward direction writes to the Mac from untrusted content, it must treat every
archive entry as hostile — acceptance test 14 states the requirement. The
inward direction is how data reaches a run already under way, and it carries
data and never credentials: a credential-shaped name costs an explicit
override, for the reason `run` makes `--include-unsafe` explicit. `discard` is
destructive, so its confirmation argument must repeat the exact run ID before
anything is deleted.

A run may be named or left out. Left out, it is the one run of the checkout the
user is standing in that has not been imported yet; several are a question
pisafe asks rather than answers, and `discard` names its run twice however few
there are. Nothing about which run a command reaches may depend on anything
inside a run.

The second group manages what outlives a run. `extension` and `tool` are the
only way anything reaches the profile runs mount, `project` and `profile` name
a durable scope and empty or remove it, and `backup` and `restore` carry what
nothing else reproduces off the VM and back. The destructive ones confirm the
way `discard` does: `project drop` repeats the exact checkout path, and
`profile reset` is refused without `--confirm`.

## Staging and Git behavior

### Starting the run

Create the staged repository at the original HEAD; Pi may then commit
autonomously. If the checkout has tracked changes, capture the final tracked
working-tree state — staged and unstaged changes, deletions, executable bits,
symlinks — and commit it first as:

```text
pisafe: imported working-tree baseline
```

Flattening the working tree into one commit loses the staged/unstaged
distinction; `pisafe run` should say so when it happens.

Untracked and ignored inputs are never silently copied. Selection shows names,
types, and sizes, rejects special files and escaping symlinks, and warns about
likely secrets. A file that looks like a secret may still be included, but the
prompt must present it as an unsafe override: under open egress, everything in
the run can read and exfiltrate it, so including one voids the run's credential
isolation.

An included path crosses as files in both directions and never enters Git. It
arrives in the run untracked, exactly as it sits on the Mac, so the agent cannot
commit it by accident and the run's own history stays independent of it. What
`--include` records is the **path the user named**, not the files under it at
that moment: a directory that is empty at run start is still a root, and is how a
run collects work the user wants back without committing it. Such a root is
created in the run, because neither Git nor the archive carries an empty
directory and the agent has to have somewhere to write. Naming a path that no
listing covers and naming one that is simply absent are different refusals.

### Submodules

Git bundles do not carry submodule contents. Staging bundles the superproject
and each initialized submodule and reconstructs the same layout in the run;
uninitialized submodules stay uninitialized. `apply` imports the superproject
branch and the referenced submodule histories into the corresponding local
repositories, creating a `pisafe/<run>` ref in each changed submodule so its
commits stay reachable, and reports which submodule commits the imported branch
expects.

Git LFS is out of scope: a repository using it is detected and refused rather
than staged incompletely.

### `pisafe apply`

`apply` does not merge into the current branch or check out history. It imports
the completed history as `refs/heads/pisafe/<run>`, preserving the agent's
commits individually. Tracked changes the agent left uncommitted are captured as
a final clearly labelled commit. New files the run created are reported and stay
in the run, on the same terms that kept them out of it: what crosses either
boundary is what Git tracks, plus what the user named.

The one exception is what the user named. Work left under an included path is
copied into the working tree, because that is how it arrived and committing it is
exactly what the user chose not to do. This is the only part of an apply that
writes files, so it is bounded deliberately:

- Only paths under a recorded include root, proved on arrival rather than
  trusted: the list is written inside the run.
- Only what the run does not track. A path the agent committed travels as
  history instead, never twice.
- **Additive only.** A path the run deleted stays on the Mac and is reported as
  kept. A run cannot delete the user's files through this channel.
- A path that changed both in the run and on the Mac is a conflict, and one
  conflict holds back the whole copy rather than leaving the working tree in a
  state neither side described. A path only the Mac changed keeps the Mac's
  version. `--include-force` overrides a conflict by taking the run's version.

The refs and the files are separate steps, so a refused copy leaves the branch
imported and the work recoverable: the run's returned files are kept beside the
run's manifest, and `pisafe apply` on an already-imported run finishes the copy
and nothing else, with no VM involved.

All-or-nothing describes the conflict decision, which is made for every path
before any is written. A copy that fails partway — a full disk, a path whose
parent is not a directory — is recovered by retrying rather than by unwinding:
each file is replaced whole, and a path already holding what the run holds is
skipped on the next pass, so repeating the copy converges instead of redoing it.

The branch travels as an incremental bundle containing only commits new since
the captured HEAD, fetched into a temporary ref and moved into place only after
verification, so an interrupted transfer cannot leave a partial branch.

Because superproject and submodule refs live in separate repositories with no
cross-repository transaction, `apply` is journaled and idempotent: import and
verify every object set first, record in the run manifest which commit each
repository must end up at, then update refs one repository at a time. The
branch is `pisafe/<run>`, which follows from the run itself, so a stored
journal names no ref and cannot be tampered into moving one the run never
earned. Every forward and rollback update is compare-and-swap (`git update-ref
<ref> <new> <expected-old>`): a step whose ref already holds the recorded
commit is complete, a rollback deletes the ref only while it still holds that
commit — apply only ever creates the branch, so no step has a previous value to
restore — and a ref holding anything else stops recovery for manual
reconciliation rather than overwriting a change the user made meanwhile. The
run is marked imported only when every ref matches the manifest.

If a dirty baseline commit exists, prompt to either keep it with all following
commits, or replay only the later commits onto the original captured HEAD. The
replay happens in the isolated staged environment; if it cannot be done cleanly,
stop without changing the host repository and offer to keep the baseline,
resolve manually in the staged environment, or cancel.

If the original repository has advanced since the run began, import the branch
unchanged and report the divergence. Never silently rebase or merge; the user
can merge, rebase, or compare `pisafe/<run>` normally. Name collisions must fail
safely or select a new explicit branch name, never force-update an existing
branch.

## Persistent state without broad cross-project writes

Do not restore a single writable `/home/pi` volume. Split state by purpose:

| State | Scope | Writable during normal run |
|---|---|---:|
| Pi/core version | pinned image | no |
| Global settings | all runs | no |
| Installed extensions | all runs | no |
| Global tools | all runs | no |
| ChatGPT/provider credentials | Mac broker | not present |
| GitHub credentials | nowhere in pisafe | not present |
| Pi sessions | project store, run-local copy | yes |
| Dependency caches | project | yes |
| Staged repository | run | yes |
| Temporary downloads | run | yes |

Three invariants hold however this is built. Agent code cannot write global
settings, extensions, or tools: those change only through a management command
that pins an exact version and integrity hash, and the profile mounts
read-only. A pinned extension is still untrusted at runtime — pinning prevents
a surprise update, the boundary limits its reach, and updates are offered
rather than applied. And one project's runs cannot read another project's
sessions or caches, nor concurrent runs one another's live transcripts.

A fourth property is not an invariant but is what makes the rest tractable:
**shared state is disposable, and no run's correctness may depend on it.** A
cache that is missing, stale, or wrong must cost time and nothing else, which is
what licenses every approximation below and what obliges every shared scope to
have a reset. Session transcripts are the exception that proves it — nothing
reproduces one — so they are never evicted, never overwritten, and are the thing
a backup exists to carry.

What follows:

- **A run reads shared project state and writes only its own copy.** Each shared
  directory is mounted copy-on-write with the run's upper layer inside the run's
  own storage. No package manager's concurrency behaviour is load-bearing, and
  no run can corrupt what the project holds for the next one.
- **Caches are keyed immutable snapshots, not merged directories**, selected by
  exact key and otherwise by recency within the namespace, and published when a
  run stops. Directory merging is refused outright: the presence or absence of a
  file carries tool-specific meaning that no generic rule reconstructs.
- **Nothing is cached unless the repository asks for it**, in
  `.config/pisafe.json` at its root. That file arrives with the repository and
  is parsed on the Mac before any sandbox exists, so its schema is inert: it
  names caches and the environment variables that point at them, and nothing in
  it selects a path pisafe mounts, a command pisafe runs, or a variable pisafe
  sets itself. The worst a hostile declaration achieves is a useless cache key
  or a full project filesystem, and a reset fixes both.
- **A session store only ever gains entries.** A finished run's transcripts are
  added to it under names that carry a session UUID; nothing a run does modifies
  or removes what another run handed over, and a live run's transcripts reach no
  concurrent run.
- **The profile is written only from the Mac**, by a command the user runs
  deliberately, and every run mounts it read-only at a path of pisafe's
  choosing. Pi's own global package store is part of the run's writable home, so
  installing inside a run works and serves that run alone; nothing it writes has
  a path to the profile. A package reaches the profile only at an exact version
  whose fetched bytes match the recorded integrity hash. What a run installed
  for itself is reported when it stops, so keeping it is a command away and
  losing it is never a surprise.
- **An update is discovered without being applied.** What the registry now
  resolves an installed name to is checked at most once a day, never on the path
  that starts a run, and reported when a run stops; moving a pin happens only
  when the user names the package.

## Network policy

Internet egress is open. There is no proxy, no approval queue, and no
per-destination policy — only a static packet filter, written once as VM-root
nftables rules:

- IPv6 is disabled in the VM initially; it can return later with an
  equivalently tested ruleset.
- Deny IPv4 loopback, link-local (including the metadata address
  `169.254.169.254`), RFC1918, CGNAT (`100.64.0.0/10`), multicast, broadcast,
  and the VM's own gateway and host-side addresses.
- Additionally deny the Mac's directly connected on-link prefixes, gathered at
  VM start/resume, so a LAN using globally routed IPv4 space is still covered;
  fail closed if they cannot be determined.
- Allow one exact exception: the inference broker relay address and port. It is
  fixed at provisioning time. The VM user has no firewall-mutation privilege of
  any kind, so there is nothing to extend at runtime.
- Deny inbound connections. Per-run SSH enters through Lima's existing control
  connection and a container-local stdio relay, not a VM listener.
- Allow everything else, over any protocol.
- All chains accept established/related traffic, so policy gates connection
  initiation and conntrack owns replies.

Two details are load-bearing. Rules must filter all VM egress (output and
forward hooks), not only forwarded container packets: rootless Podman's `pasta`
networking emits container traffic from a userspace process in the VM, which a
forward-only ruleset never sees. And the VM's resolver must be a public DNS
server, since the default resolver is the private gateway address the deny set
blocks.

Filtering on resolved destination addresses means DNS rebinding cannot reach
denied ranges, and the rules apply uniformly to Pi, extensions, shell commands,
dependencies, and Zed remote tooling. Bypass tests must cover raw TCP and UDP,
numeric IPs, DNS answers pointing at private ranges, HTTP redirects, and
`host.containers.internal`.

The accepted consequence, stated plainly: any code in a run can send anything
the run can read to any internet destination, without record or interruption.
A run reads more than the working tree — full reachable history, submodule
histories, project-local Pi resources, prior sessions, the dependency cache,
and the global profile — so the non-confidentiality assumption covers the
repository *including its history* and persisted project state. Keeping secrets
out of all of that is the load-bearing control; a warning scan is a reminder,
not proof of absence.

If a genuinely secret project ever needs sandboxing, the answer is not to bolt
approvals back on; it is to run it with the VM's egress switched to a deny-all
or allowlist profile for the duration (`pisafe run --offline` is a plausible
future flag).

## Credentials and provider login

### Inference

Provider credentials live on the Mac, in the Keychain, and never enter the VM.
`pisafe login chatgpt` runs the ChatGPT Plus/Pro OAuth flow and persists the
refresh token there; `pisafe login <provider>` stores an API key for an upstream
reached with one. Pi normally stores OAuth tokens in `~/.pi/agent/auth.json`;
that file must not exist in the run container.

Instead the broker is declared in Pi's `models.json` as a local provider
endpoint speaking a supported standard API. It attaches credentials, refreshes
OAuth itself, calls the provider, and streams the response back unchanged; the
run receives only a revocable, run-scoped capability. A standard wire format
rather than a custom protocol keeps streaming and tool-call fidelity upstream's
problem, and `models.json` rather than an extension keeps pisafe off a pre-1.0
API.

Every configured upstream is declared, one `models.json` entry each, so a run
chooses between them in Pi's own model list rather than through a pisafe
command. One relay serves them all: the provider's name leads the path, and the
run capability authorizes the run rather than any one provider.

Which of those models a run opens on is pisafe's to say, because Pi's own
per-provider defaults are keyed by Pi's provider names and pisafe's are not
among them. A run is told to open on the model pisafe prefers, at the reasoning
effort it prefers, wherever a configured upstream offers it; where none does,
the choice goes back to Pi. What a run then chooses for itself is the run's own:
the settings a resume writes fill in only what the run has not answered.

The broker lives on the Mac, which the firewall denies, so its path into runs is
explicit and narrow: the controller opens one reverse SSH relay into the VM, the
VM exposes a single dedicated relay address and port to containers — the
firewall's one exception — and the relay speaks only the inference API. It
requires the run-scoped capability, cannot reach any other host address or port,
closes when the controller exits, rejects capabilities of stopped runs
immediately (resume issues a fresh one), and fails closed on unknown paths or
methods, oversized requests, and any attempt to use it as a general proxy.

Untrusted code can consume inference while its run is active, because Pi must be
able to, but it cannot extract the reusable OAuth token. A per-run concurrency
cap and the provider's own subscription limits bound the abuse. Other providers
use the same interface, added by an explicit `pisafe login` command.

### GitHub

There is no GitHub integration, by design. No token, `hosts.yml`, SSH key, or
agent socket ever enters the VM or container, and `pisafe` stores no GitHub
credentials anywhere. Public clones, fetches, and API reads work over the open
network; the user's private repositories are unavailable; pushing, publishing,
and every authenticated mutation happen on the Mac, typically after
`pisafe apply`.

This is what makes open egress acceptable, and it is structural rather than
enforced: a run given no user credentials cannot act as the user no matter what
it executes, so there is nothing to test, bypass, or maintain. Malicious code
can still write anonymously or use credentials it carries itself, which is part
of the accepted exfiltration surface. Copying user credentials into the sandbox
"just for convenience" would silently void the guarantee and must stay out of
scope.

## Zed Remote

Generate one SSH alias per run and connect Zed to the run container through the
VM, printing both the URI and the staged path:

```text
zed ssh://pisafe-<run>/work/<project>
```

No container port is published in the VM or on macOS: the alias reaches the
container through Lima's own control connection and a stdio relay. The client
private key stays on macOS, the container generates its own host key and runs
`sshd` as the non-root run user, and the Mac pins that host key before the first
connection. `pisafe` writes only its own per-run config fragment and never edits
the user's SSH configuration.

That fragment is reached with `ssh -F`, which Zed passes along only from a saved
connection, so `pisafe zed` adds one for the run it is opening and takes it back
out when the run is reclaimed. Zed's saved connections are the one file outside
`pisafe`'s own state it writes, and it writes nothing there but the host and the
config file to reach it by.

Zed runs source, language servers, tasks, and terminal commands on the remote
machine, which keeps executable project tooling in the sandbox. The local UI
still sees source text, parses it, and stores unsaved state; Zed is therefore
trusted as a local editor, but project build tools do not execute on macOS.

A run may also host a server the user needs to look at, and `pisafe forward`
carries one to a listener on the Mac's loopback. That is a channel on the run's
own SSH connection, not a published port: nothing binds in the VM, whose
firewall drops inbound traffic that is not SSH, and only the holder of the run's
key can open one. Forwarding is local-only and bounded to the container's
loopback, so a run gains no listener of the Mac's and the Mac cannot ask a run
to reach anything else on its behalf. It is per-invocation and dies with the
command, because it does hand run-authored content to a browser at a loopback
origin, which the user chooses one port at a time rather than pisafe granting
for the life of a run.

## Lifecycle, cleanup, and the run record

```text
creating → active → stopped → imported → reclaimed
```

- A run has a record for exactly as long as it owns something. Reclaiming it
  removes the record with the resources, so there is no terminal state.
- A stopped run resumes, and so does one whose container is gone. Only a run
  that is genuinely still running refuses, because resuming would restart an
  agent mid-work. Resuming issues a fresh short-lived broker capability rather
  than extending the old one.
- Stopped time does not consume the eight-hour active budget. A run is killed
  independently when its remaining budget expires; the next lifecycle command
  reconciles the durable record to stopped.
- A run is active only while its container runs, so rebooting or recreating the
  VM leaves records claiming containers that are gone. Settling one of those
  costs it nothing: the container carried the only account of how much of the
  stretch was spent and went with the VM, and charging the wall clock instead
  would spend a whole budget on an outage the run did not cause. Nothing inside
  a run can bring its own container down, so this is not a budget agent code
  can extend.
- Every route into a run asks the VM before handing over, and brings back one
  the VM stopped rather than reporting it gone. A run the user stopped stays
  stopped — resuming spends a budget and is theirs to ask for — but a run the VM
  stopped was nobody's decision, and the record pisafe printed as active is a
  claim to make true. `pisafe list` reads the same answer, so a record no longer
  asserts on its own that a run is up or that a deadline in the past was a
  budget spent rather than an outage.
- Successful `apply` marks a run imported but keeps it recoverable for seven
  days: the workspace still holds untracked leftovers the branch never took.
- `discard` reclaims at any point, after exact run confirmation.
- `pisafe gc` reclaims imported runs older than seven days, and reports or
  prunes long-unused per-project caches and session stores.
- Every shared scope is also nameable and disposable on demand: a project store
  can be emptied of its caches or dropped whole, and the profile emptied of
  everything installed in it. A project is keyed by its checkout path, so a
  moved repository claims its own history back rather than starting over.
- What no scope can refetch — every project's session transcripts, and the pins
  naming what the profile holds — exports to a directory on the Mac and restores
  into a VM whose storage starts empty: another Mac, or a state disk that was
  lost rather than an instance that was replaced. Caches are excluded because
  losing one costs time only; credentials are excluded because a key copied out
  of the Keychain into a directory is the boundary the broker exists to
  prevent. Both directions only ever add, so a backup taken twice loses nothing
  and a restore run twice changes nothing.
- Never reclaim a run with unimported commits merely because it is old. Warn and
  require explicit discard.
- A record pisafe cannot read must still be nameable and removable. Records are
  versioned rather than migrated, so a superseded one has to cost its own run
  and nothing else: it is listed with the reason it cannot be read, `discard`
  releases everything it holds, and no other record becomes unreachable because
  it sits beside one. Because such a record names no project and no cache
  generation, anything concluding that nothing refers to a shared thing treats
  it as a reference it cannot resolve and waits.
- A VM that fails its boundary checks still hands work back, still lets go of
  it, still exports what nothing can refetch, and still lets the profile be
  emptied. `diff`, `cp`, and `apply` reach a run's workspace through a container
  with no network, no home, and none of the shared profile; `stop` and `discard`
  only end what a run holds; `backup` reads the VM and writes to the Mac;
  `extension list`, `extension remove`, `tool list`, `tool remove`, and `profile
  reset` read the profile or take things out of it. Neither the host-network
  deny set nor the security profile bears on those, and neither is verified
  before them. Only a command that may start a run or fetch over the network is
  held to the records. `restore` stays verified because it installs over the
  network, and the VM it puts a backup into is the new one, never the VM that
  failed.
- A command that reaches the profile without fetching starts the VM it needs.
  None of them is a step in a longer session, so a stopped VM is a thing to
  start rather than a thing to report.
- `pisafe vm rebuild` is what a failed boundary check names, so the cure is a
  command rather than a sequence the user assembles. Nothing an unreachable VM
  does can refuse it: a run it cannot stop is settled by the next command that
  reaches for that run, charged nothing, and an instance too broken to shut down
  is killed rather than waited on — with the lock it leaves on the state disk
  released, or the replacement would be refused the work it exists to keep. Only
  a VM predating that disk holds the work on the disk being deleted; that one is
  refused outright until the loss is acknowledged, because the command a user
  reaches for after reading an error must not be the one that destroys the work.

While a run exists, its record holds the run ID, project identity, captured
source HEAD, timestamps, exact image and tool versions, baseline and final
commit IDs, the paths the user included together with the content each carried
in, and the imported branch name. It also holds a run's returned work until that
work reaches the working tree, so a refused copy survives. Nothing is retained
once the run is reclaimed: what it
produced is already in the user's repository, on a branch named `pisafe/<run>`
after the run itself, with the base commit as its parent and the author it
committed as — so a surviving record would only restate what git holds.

Do not log prompts, source contents, HTTP bodies, environment variables, OAuth
tokens, or package secrets by default.

## Implementation order

This was built in two phases, and the split still explains the shape of it.
Phase 1 — the mountless VM and its firewall, per-run containers and storage, Git
bundle staging and journaled apply, Zed Remote SSH, the brokered ChatGPT login,
and the run lifecycle commands — is a complete MVP whose configuration is
ephemeral per run. The broker belongs to it because Pi cannot function without
model access and raw credentials must never enter the container, even
temporarily. Phase 2 is managed persistence, quality of life rather than safety:
the read-only global profile, installer commands with version and integrity
pinning, update notifications, per-project sessions and caches, other providers,
and backup and recovery. None of it may weaken the boundary Phase 1 built.

The two components that carry security weight — the firewall rules and the
inference broker — deserve real tests: bypass attempts against the packet
filter, a broker contract test covering streaming, tool calls, reasoning events,
and errors, and fail-closed behavior when the broker or upstream is down.

## Acceptance tests

1. Deleting `/work/<project>` cannot alter the original checkout.
2. The run cannot read arbitrary `/Users/...` files.
3. Container escape into the VM user still finds no host filesystem mount.
4. No `pisafe`-supplied credential other than the run-scoped inference
   capability is readable in the VM or container: no Keychain or provider
   tokens, no `gh` token, no host-user SSH private key or forwarded agent, no
   cloud credentials. (Sandbox SSH host keys and authorized public keys are
   expected.)
5. The Mac, LAN, link-local, metadata, and VM- and Mac-loopback services are
   unreachable from a run — including via DNS names resolving to them, raw
   TCP/UDP, numeric IPs, and redirects — while container-local loopback and the
   broker relay work.
6. `npm install` and a public GitHub clone work with no prompt; a push to the
   user's repository fails because no host credential is ever offered.
7. Zed terminal and Pi see the same staged files and toolchain.
8. Dirty baseline keep/drop behavior preserves later commits or fails safely.
9. `pisafe apply` creates a new local branch and does not touch the current
   index or working tree.
10. Two simultaneous runs cannot see or overwrite one another.
11. `apply` interrupted at any point, including between per-repository ref
    updates, either completes or restores prior refs on retry.
12. Cleanup never deletes an unimported run without explicit confirmation.
13. A repository with submodules stages, runs, and applies with superproject and
    submodule commits preserved and reachable via per-submodule `pisafe/<run>`
    refs.
14. `pisafe cp` refuses traversal paths, escaping symlinks, and special files,
    enforces size and count limits, and never overwrites without confirmation.
    Copying into a run lands only inside its workspace, under the name the Mac
    chose, and reaches a run that was already under way.
15. Nothing in a run can write the global profile: installing a package inside
    a run succeeds, loads, and leaves the profile the next run mounts
    byte-for-byte what the user installed.
16. Two runs of one project each read what earlier runs finished and neither
    sees the other's live transcripts; a run of a different project reaches
    neither those transcripts nor that project's caches.
17. An extension or tool installs only at an exact version, refuses bytes that
    hash to anything but the recorded integrity, and a newer release reaches no
    run until the user names the package.
18. A backup writes no provider credential, and restoring it into a freshly
    recreated VM returns every transcript and reinstalls every pin; taking a
    backup twice or restoring twice loses and changes nothing.
19. Emptying a project's caches, or losing them with the VM, changes only how
    long the next run takes.
20. A forwarded port reaches a server inside the run and nothing else: a forward
    aimed anywhere but the container's loopback is refused, no listener binds in
    the VM or outside this Mac's loopback, and every forward ends with the
    command that asked for it.

## Residual risks

- Silent exfiltration of everything a run can read, as described under Network
  policy. Acceptable only while the projects involved are non-confidential.
- Open egress also permits outbound abuse — scanning, spam, cryptomining — that
  harms third parties rather than the user's data. Static bandwidth or
  connection caps, or restricting ordinary runs to DNS and TCP 80/443, would
  shrink this without reintroducing prompts.
- The selected model provider receives repository content sent as context.
- A malicious dependency can damage the staged repository or poison its
  per-project cache; a malicious extension can persist in the pinned global
  profile until removed, though it remains confined to future sandboxes.
- A VM/hypervisor or controller vulnerability remains possible.
- The inference capability can be abused during an active run even though its
  OAuth token is hidden; the practical bound is the subscription's own limits.
- Zed is a trusted local application and necessarily receives file contents.
- Driving the ChatGPT subscription OAuth flow from a non-official client may sit
  in a gray area of the provider's terms of use, and that backend is an
  unofficial surface that can change without notice; inference then fails closed
  and loudly until the pinned `pi-ai` dependency ships a fix.

## Primary references

- Pi security, containerization, and providers:
  <https://pi.dev/docs/latest/security>,
  <https://pi.dev/docs/latest/containerization>,
  <https://pi.dev/docs/latest/providers>
- Pi AI programmatic OAuth:
  <https://github.com/earendil-works/pi-mono/blob/main/packages/ai/README.md>
- Lima plain mode, mounts, and SSH: <https://lima-vm.io/docs/config/plain/>,
  <https://lima-vm.io/docs/config/mount/>, <https://lima-vm.io/docs/usage/ssh/>
- Podman rootless `pasta` networking:
  <https://docs.podman.io/en/stable/markdown/podman-network.1.html>
- Zed Remote Development: <https://zed.dev/docs/remote-development>
