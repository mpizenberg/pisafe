# `pisafe` Design Specification

Date: 2026-07-23  
Status: proposed design; simplified to open egress with structural credential
isolation after weighing maintenance cost against the threat model

## Executive decision

Keep the `pisafe` name and the useful parts of the current container image, but
replace the current “mount this checkout into Podman” execution model.

The recommended design is:

1. A dedicated, mountless Linux VM on the Mac.
2. One isolated container and staged Git repository per run.
3. Zed Remote SSH into that same run container.
4. Open internet egress, with loopback, LAN, link-local, and metadata
   destinations denied by static VM firewall rules.
5. A Mac-side inference broker so provider credentials never enter the run.
6. No GitHub credentials anywhere in the sandbox; the user pushes from the Mac
   after `apply`.
7. Git bundle transfer over SSH, with no macOS directory mounted into the VM.
8. `pisafe apply` importing a local `pisafe/<run>` branch without touching the
   current checkout.

This deliberately trades exfiltration resistance for simplicity and zero
network prompts. Pi can edit files, browse, download, and install anything
without asking. The security boundary instead guarantees two things: a run
cannot touch the Mac or the original checkout, and `pisafe` never hands a run
any reusable user credential, so nothing it executes can act as the user. The
only credential `pisafe` creates is a revocable, run-scoped inference
capability. An earlier draft of this design
included a dynamic approval proxy and a GitHub credential broker; they were
removed as a maintenance and interruption cost disproportionate to the mostly
public, non-secret projects this tool targets.

## Requirements captured

- Protect the Mac and files outside the selected projects from mistakes,
  malicious project content, model output, dependencies, and unreviewed Pi
  extensions.
- Pi must be able to modify the project and commit autonomously when asked.
- The working copy may be staged as long as applying the result is easy.
- The staged files must be viewable and editable in Zed.
- Zed Remote SSH is acceptable.
- `git push`, publishing, deployment, and cloud CLI mutations must not happen
  as the user autonomously. Satisfied structurally: `pisafe` supplies no user
  credentials to the sandbox, so acting on the user's accounts is only
  possible from the Mac. Code may still write anonymously or with credentials
  it carries itself; that is part of the accepted exfiltration surface.
- Public GitHub and general internet reads are automatic (open egress).
- The user's private GitHub repositories and registries are unavailable
  through `pisafe`. If this becomes a real need, it reopens the
  credential-broker question and is out of scope for now.
- Package and tool installation needs no approval; it downloads over the open
  network into the sandbox.
- Global tools may persist inside the isolated environment; installation and
  update happen through explicit `pisafe` management commands.
- Dependency caches are per project.
- Pi extensions, settings, tools, and sessions persist.
- Extension/provider installation is an explicit `pisafe` command. Versions
  are pinned and updates are offered to the user rather than applied
  automatically.
- Ignored and untracked source files are excluded initially unless explicitly
  selected.
- Secret-bearing files such as `.env` are included only through that same
  explicit selection flow, after a strong warning. There is no separate secret
  injection mechanism.
- Repositories may contain Git submodules; staging must support them. Git LFS
  is out of scope initially.
- ChatGPT subscription login persists.
- Other providers such as Kimi or DeepSeek may be added later.
- No access to host-local services is currently needed.
- Completed runs may be automatically removed after seven days.
- Concurrent side-quest runs must be independent.

## Trust model

Treat these as untrusted:

- Model output and commands.
- Project files and project-local Pi resources.
- Dependencies and lifecycle scripts.
- Pi catalog extensions, even popular ones.
- Programs downloaded during a run.
- Zed language servers, tasks, and remote extensions.

The trusted computing base is deliberately small:

- The `pisafe` Mac controller.
- The inference credential broker.
- The VM and container runtimes.
- A pinned base image.
- A minimal provider integration: a `models.json` entry plus, if needed, a
  thin pinned extension.
- The staging and Git import/export code.

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
    ├── shared read-only tools/extensions profile
    ├── per-project session and dependency-cache storage
    └── one rootless container per run
        ├── Pi and unreviewed extensions
        ├── staged Git repository
        ├── run-local writable home
        ├── Zed remote server, terminal, tasks, and language servers
        └── no reusable user credentials; only a run-scoped
            inference capability
```

### VM backend

Use a dedicated Lima ARM64 VM in `plain` mode, provisioned with rootless Podman.
Lima plain mode explicitly disables filesystem mounts, SSH-agent forwarding,
dynamic port forwarding, and its guest agent. Plain SSH and static forwarding
remain available.

Transfer repositories through SSH streams using Git bundles and a validated
archive for selected untracked files. Never mount `/Users`, the repository, the
Docker socket, or the host SSH agent.

This is preferable to the existing general-purpose Podman machine, which
currently exposes `/Users`, `/private`, and `/var/folders` read-write inside the
VM. A container escape there could reach Mac files without a hypervisor escape.

The controller embeds its managed run-image Containerfile and packages the
static Linux ARM64 guest helper as a sidecar. Podman builds that exact context
inside the dedicated mountless VM.

### Per-run container

Each run receives:

- A unique run ID and container.
- Its own staged Git repository and quota-backed writable run storage.
- A read-only mounted global profile.
- A per-project session volume.
- A per-project dependency-cache volume.
- A unique SSH endpoint and short-lived broker capability.
- CPU, memory, process-count, temporary-filesystem, 10 GiB persistent-storage,
  and eight-hour cumulative active wall-clock limits.

Use a non-root user, a read-only container root, dropped capabilities,
`no-new-privileges`, and no container socket. The container has open internet
egress through the VM's static firewall, which denies host, LAN, link-local,
metadata, and loopback destinations.

Multiple runs for one project are independent snapshots. They do not see one
another's staged files or conversational context.

## Command-line experience

### `pisafe run`

`pisafe run`:

1. Validates the current Git repository and records its HEAD.
2. Shows untracked and ignored files; excludes them unless explicitly approved.
3. Creates the staged repository inside the VM.
4. Imports tracked working-tree changes as a baseline commit when needed.
5. Starts the run container and Pi.
6. Prints the run ID, staged path, and SSH URI.
7. Opens the staged project automatically in Zed Remote SSH.
8. Leaves the run resumable if the terminal or Zed disconnects.

Example output should remain short:

```text
Run:       api-refactor-20260723-1432
Workspace: /work/api-refactor
Zed:       ssh://pisafe-api-refactor/work/api-refactor
Branch:    work/api-refactor-20260723-1432
```

The Zed terminal connects to the same run container as Pi. It therefore sees
the same files, tools, dependency cache, Git commits, and network policy.

### Supporting commands

```text
pisafe list
pisafe connect <run>
pisafe zed <run>
pisafe diff <run>
pisafe log <run>
pisafe cp <run>:<path> [dest]
pisafe apply <run>
pisafe discard <run> --confirm <run>
pisafe gc
pisafe doctor

pisafe login chatgpt

# Phase 2 (persistent profile management):
pisafe extension install <package>
pisafe extension update [package]
pisafe tool install <package>
```

`connect` resumes Pi or opens a shell. `zed` prints the path and opens Zed.
`cp` copies a file or directory out of a run — build artifacts, logs,
screenshots that do not belong in Git history — after showing exactly what
will be copied. Because `cp` writes to the Mac from untrusted content, it must
reject absolute and `..` paths, escaping symlinks and hard links, and special
files, must not write through existing destination symlinks, and must confirm
before overwriting. It also enforces limits on total expanded bytes, file
count, and individual file size, and extracts with
directory-descriptor-relative operations so the destination cannot be swapped
for a symlink mid-copy. `discard` is explicit and destructive, so its
confirmation argument must repeat the exact run ID before anything is deleted.

## Staging and Git behavior

### Starting from a clean checkout

Create the staged repository at the original HEAD. Pi may create ordinary
commits autonomously. The original repository remains unchanged.

### Starting with tracked changes

Capture the final tracked working-tree state, including staged and unstaged
changes, deletions, executable bits, and symlinks. Commit it first as:

```text
pisafe: imported working-tree baseline
```

This commit is followed by the agent's own commits. Flattening the working
tree into one commit loses the staged/unstaged distinction; `pisafe run`
should say so when it happens.

Untracked and ignored inputs are not silently copied. Selection should show
file names, types, and sizes, reject special files and escaping symlinks, and
warn about likely secrets. A file that looks like a secret, such as `.env`,
may still be included when the user insists, but the prompt must present it
as an unsafe override: under open egress, everything in the run —
dependencies, extensions, and downloaded code — can read, use, and exfiltrate
those credentials, so including one voids the run's credential isolation.
Explicitly included input files become part of the baseline commit.

### Submodules

Git bundles do not carry submodule contents. Staging must therefore bundle
the superproject and each initialized submodule and reconstruct the same
layout in the staged repository; uninitialized submodules stay uninitialized.
`pisafe apply` imports the superproject branch and the referenced submodule
histories into the corresponding local repositories, creating a
`pisafe/<run>` ref in each changed submodule so its commits stay reachable,
and reports which submodule commits the imported branch expects. Ref updates
across the superproject and submodules follow the journaled `apply` protocol
below.

Git LFS is explicitly out of scope for now; a repository using it should be
detected and refused with a clear message rather than staged incompletely.

### Uncommitted changes at the end

Before import, show:

- Uncommitted tracked changes.
- New non-ignored files.
- Ignored outputs separately.

Tracked changes must be captured to keep the staged state consistent. Create a
final clearly labelled commit if the agent left them uncommitted. Include new
files after confirmation; do not import ignored build outputs by default.

### `pisafe apply`

Despite its convenient name, `apply` does not check out files or merge into the
current branch. It imports the completed history as:

```text
refs/heads/pisafe/<run>
```

The branch is transferred as an incremental Git bundle containing only
commits new since the captured HEAD, fetched into a temporary ref and moved
into place only after verification, so an interrupted transfer cannot leave a
partial branch. Preserve the agent's commits individually.

Because superproject and submodule refs live in separate repositories with no
cross-repository transaction, `apply` is journaled and idempotent: import and
verify every object set first, record the intended old/new refs in the run
manifest, then update refs one repository at a time. Every forward and
rollback update is compare-and-swap (`git update-ref <ref> <new>
<expected-old>`): a step whose ref already holds the new value is complete, a
rollback restores the old value only while the ref still holds the recorded
new value, and a ref matching neither stops recovery for manual
reconciliation rather than overwriting a change the user made meanwhile. The
run is marked imported only when every ref matches the manifest.

If a dirty baseline commit exists, prompt:

1. Keep the baseline commit and all following commits.
2. Try replaying only the later commits onto the original captured HEAD.

The second operation occurs in the isolated staged environment. If later
commits depend on the baseline and cannot be replayed cleanly, stop without
changing the host repository and offer:

- Keep the baseline.
- Resolve manually in the staged environment.
- Cancel.

If the original repository has advanced since the run began, import the branch
unchanged and report the divergence. Never silently rebase or merge. The user
can merge, rebase, or compare the local `pisafe/<run>` branch normally.

Name collisions must fail safely or select a new explicit branch name; never
force-update an existing branch.

## Persistent state without broad cross-project writes

Do not restore the current single writable `/home/pi` volume. Split state by
purpose:

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

Global settings are changed through a management command or a dedicated
configuration session, not by ordinary agent code.

Extension and global-tool installation runs in a separate installer context,
triggered by an explicit `pisafe` command. Resolve and pin an exact version
and integrity hash. Mount the resulting profile read-only into agent runs.

An installed extension is still untrusted at runtime; pinning prevents a
surprise update, while the VM/container boundary limits its reach. Offer
available updates rather than applying them automatically.

Per-project sessions meet the persistence requirement without allowing one
project to read another project's transcripts by default. Each run starts
from an immutable snapshot of the project's session store and writes new
run-specific session IDs to a run-local directory; on stop, new IDs are
appended to the project store and an existing ID is never overwritten.
Concurrent runs never read one another's live transcripts.

Per-project dependency caches are mounted as run-local writable overlays;
content-addressed entries may be merged back under a lock, while mutable or
conflicting entries stay run-local. Until this exists (Phase 2), runs get
private caches rather than a shared writable volume.

## Network policy

Internet egress is open. There is no proxy, no approval queue, and no
per-destination policy. The policy is a static packet filter, written once as
VM-root nftables rules:

- IPv6 is disabled in the VM initially; it can return later with an
  equivalently tested ruleset.
- Deny IPv4 loopback, link-local (including the metadata address
  `169.254.169.254`), RFC1918, CGNAT (`100.64.0.0/10`), multicast, broadcast,
  and the VM's own gateway and host-side addresses.
- Additionally deny the Mac's directly connected on-link prefixes, gathered
  at VM start/resume, so a LAN using globally routed IPv4 space is still
  covered; fail closed if they cannot be determined.
- Allow one exact exception: the inference broker relay address and port.
- Deny inbound connections. Per-run SSH enters through Lima's existing
  control connection and a container-local stdio relay, not a VM listener.
- Allow everything else, over any protocol.

Two implementation details are load-bearing:

- Rules must filter all VM egress (output and forward hooks), not only
  forwarded container packets: rootless Podman's default `pasta` networking
  emits container traffic from a userspace process in the VM, which a
  forward-only ruleset never sees.
- The VM's resolver must be a public DNS server, since the usual default
  resolver is the private gateway address the deny set blocks. A container
  may use its own loopback freely; VM and Mac loopback services stay
  unreachable.

Because filtering happens at the packet level on resolved destination
addresses, DNS tricks such as rebinding cannot reach denied ranges; a name
that resolves to a LAN address is simply unreachable. The rules apply
uniformly to Pi, extensions, shell commands, dependencies, Zed remote
tooling, and language servers, and there is nothing dynamic to maintain.
Bypass tests should cover raw TCP and UDP, numeric IPs, DNS answers pointing
at private ranges, HTTP redirects, and `host.containers.internal`.

The accepted consequence, stated plainly: any code in a run — including a
malicious dependency — can send anything the run can read to any internet
destination, without record or interruption. A run can read more than the
working tree: the staged repository's full reachable history, initialized
submodule histories, project-local Pi resources, its snapshot of prior
project sessions, the per-project dependency cache, and the read-only global
profile. The non-confidentiality assumption therefore covers the repository
*including its history* and persisted project state, not just today's files.
Keeping secrets out of all of that is the load-bearing control. A best-effort
warning scan of the tree and selected inputs is useful, but it is a reminder,
not proof of absence.

If a genuinely secret project ever needs sandboxing, the answer is not to
bolt approvals back on; it is to run that project with the VM's egress
switched to a deny-all or allowlist profile for the duration
(`pisafe run --offline` is a plausible future flag).

## Credentials and provider login

### ChatGPT subscription

`pisafe login chatgpt` runs the ChatGPT Plus/Pro OAuth flow through trusted
broker code based on `@earendil-works/pi-ai`. Persist the refresh token in the
macOS Keychain or a broker-only encrypted store.

Pi normally stores OAuth tokens in `~/.pi/agent/auth.json`. Do not put that file
in the run container. Instead:

1. The broker is declared in Pi's `models.json` as a local provider endpoint
   speaking a supported standard API (OpenAI Responses/Completions or
   Anthropic Messages).
2. The broker attaches credentials, refreshes OAuth, and calls the provider.
3. It streams the model response back unchanged.
4. The run receives only a revocable, run-scoped broker capability.

Speaking a standard wire format instead of a custom pisafe protocol keeps
streaming and tool-call fidelity upstream's problem, not ours. Pi's extension
API is pre-1.0 and changes often, so any extension code (for example, for the
capability handshake) stays thin; the `models.json` route is the primary
integration.

The broker lives on the Mac, which the firewall denies, so its path into runs
is explicit: the controller opens one reverse SSH relay into the VM (plain
Lima SSH supports static forwarding), the VM exposes a single dedicated relay
address and port to containers — the firewall's one exception — and the relay
speaks only the standard inference API, requires the run-scoped capability,
and cannot reach any other host address or port.

Relay implementation notes: the reverse forward binds only the dedicated
container-reachable VM address, with `sshd` configured to permit exactly that
binding, and the firewall must admit it in every hook the rootless `pasta`
connection traverses. The relay closes when the controller exits, rejects
capabilities of stopped runs immediately (resume issues a fresh one), and
fails closed on unknown paths or methods, oversized requests, and any attempt
to use it as a general proxy.

Untrusted code can consume inference while its run is active, because Pi must
be able to do so, but it cannot extract the reusable OAuth token. A simple
per-run concurrency cap and the provider's own subscription limits bound the
abuse; elaborate metering is not worth its maintenance.

Other providers, including Kimi or DeepSeek, use the same broker interface.
Adding a provider or credential is an explicit `pisafe login` command.

### GitHub

There is no GitHub integration, by design. No token, `hosts.yml`, SSH key, or
agent socket ever enters the VM or container, and `pisafe` stores no GitHub
credentials anywhere.

- Public clones, fetches, and API reads work directly over the open network.
- The user's private repositories are unavailable through `pisafe`.
- Pushing, publishing, and every authenticated mutation as the user happen on
  the Mac, typically after `pisafe apply` — exactly as for any local branch.

This one decision is what makes open egress acceptable: a run that receives
no user credentials cannot push, publish, or deploy *as the user* no matter
what code it executes. Malicious code can still write anonymously or use
credentials it carries itself, reaching whatever those credentials authorize
— that is part of the accepted exfiltration surface, not a breach of this
guarantee. The guarantee is structural rather than enforced, so there is
nothing to test, bypass, or maintain. Copying user credentials into the
sandbox "just for convenience" would silently void it and must remain out of
scope.

## Zed Remote

Generate one SSH alias per run and connect Zed to the run container through the
VM. `pisafe run` invokes the equivalent of:

```text
zed ssh://pisafe-<run>/work/<project>
```

Also print this URI and the staged path.

Each alias uses a strict per-run OpenSSH config. Its `ProxyCommand` opens
Lima's generated control-SSH connection and runs an interactive, non-TTY
`podman exec` whose only job is to relay bytes to `sshd` on the container's
loopback. No container port is published in the VM or on macOS. The client
private key remains on macOS; the container receives only its public key,
generates its own host key, and runs `sshd` as the non-root run user with
password, agent, X11, and TCP forwarding disabled. The Mac pins that host key
before the first connection.

Zed documents that source, language servers, tasks, and terminal commands run
on the remote machine. This keeps executable project tooling in the sandbox.
The local UI still sees source text, parses it, and stores unsaved state; Zed is
therefore trusted as a local editor, but project build tools do not execute on
macOS.

Zed's remote server, extensions, and language-server downloads fetch directly
over the open network; no special handling is needed.

## Lifecycle and cleanup

Run states:

```text
creating → active → stopped → imported | discarded | expired
```

- Active/stopped runs are resumable. Resuming issues a fresh short-lived
  broker capability rather than extending the old one.
- Stopped time does not consume the eight-hour active budget. Podman kills a
  run independently when its current remaining budget expires; the next
  lifecycle command reconciles the durable record to stopped.
- Successful `apply` marks a run imported but keeps it recoverable for seven
  days.
- `discard` deletes only after exact run confirmation.
- `pisafe gc` removes imported/discarded runs older than seven days, and
  reports or prunes long-unused per-project caches and session stores, which
  otherwise grow without bound.
- Never expire a run with unimported commits merely because it is old. Warn and
  require explicit discard.
- Keep branch/import metadata after workspace deletion so an imported branch
  remains attributable to its source run.

## Audit record

For each run, retain:

- Run ID, project identity, captured source HEAD, and timestamps.
- Exact image, Pi, extension, and tool versions.
- Baseline and final commit IDs.
- Files explicitly included beyond the tracked tree, by name.
- Git bundle hash and imported branch name.
- Cleanup/discard event.

Do not log prompts, source contents, HTTP bodies, environment variables, OAuth
tokens, or package secrets by default.

## How this compares with the current `pisafe`

Keep:

- Whole Pi process inside the sandbox.
- Non-root execution.
- Dropped capabilities and `no-new-privileges`.
- Read-only container root.
- Resource limits.
- Persistent state as a product feature.
- Podman/OCI image workflow.

Replace:

- Real checkout read-write bind mount → staged Git repository inside a mountless
  VM.
- General-purpose Podman machine → dedicated Lima plain VM.
- Shared writable home → split read-only global and per-project writable state.
- Unrestricted `pasta` network → open egress behind a static VM firewall that
  denies host, LAN, and metadata destinations.
- Raw API-key environment forwarding → credential/inference broker.
- Old package scope → pinned `@earendil-works/pi-coding-agent`.
- Deprecated config paths → current `~/.pi/agent/` layout.
- One-shot container lifecycle → named resumable runs plus `apply`.

The result preserves the convenience features that were previously labelled
high risk, but changes *where* those capabilities terminate. Pi can write
freely, just not into the original checkout. It can use the internet freely,
just not reach the Mac or the LAN. Inference authentication persists, but no
reusable secret is readable by anything in the sandbox.

## Implementation order

### Phase 1: safe workspace, editor, and brokered inference

- Dedicated mountless Lima VM with the static firewall rules.
- Pinned ARM64 run image and current Pi package.
- Per-run containers and quota-backed storage.
- Git bundle staging/import, including submodules.
- Dirty baseline choices.
- Zed Remote SSH.
- Minimal ChatGPT OAuth inference broker with macOS Keychain persistence,
  registered as a standard-API `models.json` provider.
- Run listing, resume, diff, apply, cp, discard, and seven-day GC.

The inference broker is in Phase 1 because Pi cannot function without model
access, and raw credentials must never enter the container, even temporarily.
Phase 1 is a complete MVP whose configuration is ephemeral per run:
persistent extensions, tools, sessions, and caches — and their management
commands — arrive only in Phase 2. Everything after Phase 1 is quality of
life, not safety.

### Phase 2: managed persistence

- Read-only global settings/extensions/tools profile.
- Installer commands with exact version/integrity pinning.
- Update notifications.
- Per-project sessions and dependency caches.
- Other provider onboarding.
- Backup/reset/recovery commands.

The two components that carry security weight — the firewall rules and the
inference broker — deserve real tests: bypass attempts against the packet
filter, a broker contract test covering streaming, tool calls, reasoning
events, and errors, and fail-closed behavior when the broker or upstream is
down.

## Acceptance tests

The first usable release should prove:

1. Deleting `/work/<project>` cannot alter the original checkout.
2. The run cannot read arbitrary `/Users/...` files.
3. Container escape into the VM user still finds no host filesystem mount.
4. No `pisafe`-supplied credential other than the run-scoped inference
   capability is readable in the VM or container: no Keychain or provider
   refresh/access tokens, no `gh` token, no host-user SSH private key or
   forwarded agent, no cloud credentials. (Sandbox SSH host keys and
   authorized public keys are expected.)
5. The Mac, LAN, link-local, metadata, and VM- and Mac-loopback services are
   unreachable from a run — including via DNS names resolving to them, raw
   TCP/UDP, numeric IPs, and redirects — while container-local loopback and
   the broker relay work.
6. `npm install` and a public GitHub clone work with no prompt; a push to the
   user's repository fails because no host credential is ever offered.
7. Zed terminal and Pi see the same staged files and toolchain.
8. Dirty baseline keep/drop behavior preserves later commits or fails safely.
9. `pisafe apply` creates a new local branch and does not touch the current
   index or working tree.
10. Two simultaneous runs cannot see or overwrite one another.
11. `apply` interrupted at any point, including between per-repository ref
    updates, either completes or restores prior refs on retry; no silently
    partial branch or half-updated repository set is possible.
12. Cleanup never deletes an unimported run without explicit confirmation.
13. A repository with submodules stages, runs, and applies with superproject
    and submodule commits preserved and reachable via per-submodule
    `pisafe/<run>` refs.
14. `pisafe cp` refuses traversal paths, escaping symlinks, and special
    files, enforces size and count limits, and never overwrites without
    confirmation.

## Residual risks

- Anything a run can read — the staged project and its full Git history,
  submodules, sessions, caches, explicitly included files, and run outputs —
  can be exfiltrated to any internet destination, silently. This is the
  deliberate trade of open egress and is acceptable only while the projects
  involved are non-confidential and no reusable user credential is supplied
  to the run, except through the explicitly unsafe override.
- Open egress also permits outbound abuse — scanning, spam, cryptomining,
  bandwidth waste — that harms third parties or the connection's reputation
  rather than the user's data. Static bandwidth/connection caps, or
  restricting ordinary runs to DNS and TCP 80/443, would shrink this without
  reintroducing prompts; both are optional tightenings, not Phase 1
  requirements.
- The selected model provider receives repository content sent as context.
- A malicious dependency can damage the staged repository or poison its
  per-project cache.
- A malicious extension can persist in the pinned global extension profile
  until removed, though it remains confined to future sandboxes.
- A VM/hypervisor or controller vulnerability remains possible.
- The inference capability can be abused during an active run even though its
  underlying OAuth token is hidden; the practical bound is the subscription's
  own limits.
- Zed is a trusted local application and necessarily receives file contents.
- Driving the ChatGPT subscription OAuth flow from a non-official client may
  sit in a gray area of the provider's terms of use.
- The subscription backend is an unofficial surface that can change without
  notice; inference then fails closed and loudly until the pinned `pi-ai`
  dependency ships a fix and is updated.

## Decisions

- Per-run SSH uses a portless Lima-control-SSH `ProxyCommand` and
  `podman exec` stdio relay. A VM-loopback published port was not retained
  because the static firewall correctly denies VM loopback to the unprivileged
  Lima user; opening dynamic exceptions would add mutable privileged state.
  Reversing this later would change stored connection metadata and the
  firewall contract.
- A network-disabled one-shot container initializes the run home directory, then
  non-root `sshd` is the run container's main process. A detached `sshd`
  started after the container was not retained because it would disappear
  across stop/resume and require a second process-lifecycle mechanism. This is
  inexpensive to reverse before lifecycle commands are exposed.
- PiSafe writes one strict SSH config fragment per run and does not edit the
  user's global SSH or Zed settings. `pisafe run` prints the exact command for
  Zed's explicit "Connect New Server" flow, and `pisafe zed` opens a connection
  only after Zed has saved it. Editing global SSH config automatically was not
  retained because Zed supports `-F` in that one-time flow and the design
  should not own unrelated user configuration. Reversing this choice later is
  cheap.
- Run manifests moved to version 2 and activation now atomically requires the
  SSH connection record. Accepting version-1 active records with no connection
  data was not retained because no user-facing runs existed when the format
  changed and a compatibility path would weaken the lifecycle invariant.
  Reversing this after release would require an explicit manifest migration.
- The run-image Containerfile is embedded in the controller while the static
  Linux ARM64 guest helper is a sibling release artifact. Building the helper
  at runtime or checking a generated binary into Git were not retained because
  the installed product should not require a Go toolchain and source history
  should remain reviewable. Reversing the release layout later is inexpensive,
  but would change the managed recipe digest.
- `pisafe run` is exposed before inference brokering and reports Pi as
  unavailable. Waiting to expose the isolated editor workspace or injecting a
  raw provider credential were not retained because the former would delay
  lifecycle validation and the latter would violate the core boundary. This is
  cheap to reverse when the broker lands.
- Persistent run data uses one fixed-size 10 GiB sparse ext4 filesystem
  containing the workspace and home, mounted and removed by a narrow
  fixed-policy helper. Unbounded rootless Podman volumes and Podman's XFS-only
  volume quota were not retained because the pinned Fedora image uses Btrfs
  and Podman's quota options require root. A parent Btrfs qgroup was also
  rejected because untrusted code could create uncharged nested subvolumes.
  Reversing this requires a storage-layout migration.
- Runs receive eight cumulative active hours. Podman's independent
  `--timeout` enforces each active interval, while stop removes the container
  and resume recreates it over the same storage with only the recorded
  remainder. A controller daemon or mutable VM-side timer was not retained
  because either adds a second trusted lifecycle service. Changing the default
  is cheap, but changing accounting semantics requires a manifest migration.
- Destructive confirmation is the repeated non-interactive form
  `pisafe discard RUN --confirm RUN`. A terminal-only prompt was not retained
  because exact confirmation should work identically in scripts and terminals.
  Reversing this is cheap.
- Run manifests moved to version 3 to make active-budget accounting and
  deadlines durable. Automatic version-2 migration was not retained because
  the pre-change audit found no default-state runs to preserve and inferred
  wall-clock history would be untrustworthy. Adding an explicit migration
  later remains possible, but released version-2 records would need a policy
  for their unknown elapsed time.
- The broker relay port is the static firewall exception `192.0.2.1:18080`,
  baked into the nftables ruleset and an exact `PermitListen` at provisioning.
  A runtime-mutable broker port set (and any sudo helper to populate it) was
  not retained because the boundary deliberately grants the VM user no
  firewall-mutation privilege, and one fixed port is all the design needs.
  Changing the port or address requires VM recreation.
- The relay initially shipped with an interim
  `PISAFE_INFERENCE_UPSTREAM/API/KEY/MODELS` environment configuration
  (waiting for the OAuth broker, or injecting a raw credential into runs,
  were not retained: the environment key stayed on the Mac exactly where the
  Keychain-backed credential now lives). `pisafe login chatgpt` replaced it
  entirely; keeping the environment path alongside the Keychain credential
  was not retained because two configuration surfaces for one upstream would
  have to be reconciled on every future provider change.
- The ChatGPT OAuth flow is reimplemented in Go from the pinned Pi AI
  client's constants (endpoints, client ID, PKCE parameters, refresh
  request), not run through Node. Shelling out to the Pi package on the Mac
  was not retained because the controller is dependency-free and the Mac has
  no pinned Node runtime; the trade is that upstream flow changes must be
  re-mirrored when the Pi pin moves.
- Tokens persist in the login keychain through `/usr/bin/security`, written
  over its interactive stdin and base64-wrapped, with account `chatgpt` and
  service `pisafe`. Security.framework bindings (cgo) and a broker-only
  encrypted file were not retained: the CLI keeps the build dependency-free
  and the Keychain provides at-rest encryption plus user-visible audit.
  Passing the secret as a command argument was rejected because argv is
  visible to every local process.
- Runs speak Pi's `openai-codex-responses` API against the broker. The
  pinned client refuses an apiKey that does not parse as a JWT carrying a
  `chatgpt_account_id` claim, so the run capability is wrapped in an
  unsigned JWT shape whose payload holds only the placeholder account ID
  `pisafe` and whose signature segment is the capability; the broker strips
  the wrapper before constant-time matching and always sets the real
  Authorization and chatgpt-account-id headers itself. Translating between
  the standard Responses API and the Codex backend inside the broker was not
  retained because body rewriting would own streaming and tool-call
  fidelity, which the design explicitly leaves upstream.
- `pisafe-guest configure-inference` also pins `transport: "sse"` in the
  run's Pi settings (merging, not replacing, settings Pi wrote itself):
  Pi's default auto transport dials a WebSocket first, which the HTTP relay
  cannot speak, and would otherwise pay a failed dial plus diagnostic per
  session before falling back to SSE.
- Access tokens refresh proactively inside the broker when within five
  minutes of expiry, serialized, and the rotated refresh token is persisted
  before use; a reactive refresh-on-401 path was not retained because the
  provider rotates refresh tokens and a retry layer would complicate the
  streaming relay for no additional safety. The browser flow is the only
  login method; the device-code variant was not retained because the
  controller always runs on a Mac with a browser.
- The run-side model list is a curated catalog embedded from the pinned Pi
  AI Codex data (context windows, cost rates, thinking-level maps) with
  per-model `api`/`provider`/`baseUrl`/`headers` stripped so a models.json
  entry can never route a run around the broker. Live catalog refresh from
  the package was not retained; the catalog moves with the Pi pin in the
  same commit.
- Run manifests moved to version 4: the inference capability is a manifest
  field bound to the active state, issued at activation, rotated on resume,
  and cleared on stop/discard. The broker validates every request against the
  durable records rather than in-memory sessions, so revocation needs no
  broker restart. No version-3 migration was retained because no released
  records existed; reversing after release would need an explicit migration.
- The reverse forward is a dedicated `ssh -N -R` child with multiplexing
  disabled and `ExitOnForwardFailure=yes`; the run-side `models.json` is
  installed by `pisafe-guest configure-inference` through `podman exec` stdin
  at activation and resume. Reusing Lima's control master, or writing the
  configuration only at image/home initialization, were not retained because
  the forward must die exactly with the broker process and the capability
  rotates while the home directory persists. Both choices are cheap to
  reverse.
- The output and forward chains accept `ct state established,related` like the
  input chain always has, after live testing showed the broker handshake dying:
  reply packets of an accepted connection (sshd's SYN-ACK from
  `192.0.2.1:18080`) carry the client's ephemeral port and were rejected by the
  TEST-NET deny. A narrow return rule matching only the broker source address
  and port was not retained because per-flow exceptions recreate this bug for
  every future accepted flow; the stateful design gates connection initiation
  once and lets conntrack own replies. Deny-set changes now stop new
  connections rather than tearing down established ones, which is acceptable
  because start/resume already fail closed on network change.
- Lima's default VZ user-mode network remains in the generated profile.
  Native `vzNAT` was tested but not retained because it exhibited the same
  stopped-VM SSH recovery failure and made its Mac-side interface appear only
  after the immutable host-network profile was captured. QEMU was not selected
  because it is not installed and would add a host dependency. This can be
  revisited when upstream VZ restart behavior or the supported host toolchain
  changes; changing it requires VM recreation.

- Selected untracked inputs are chosen with repeatable `--include PATH` and
  `--include-unsafe PATH` flags rather than an interactive picker. This
  matches the existing non-interactive confirmation style of
  `discard --confirm` and keeps `pisafe run` scriptable; a credential-shaped
  name is refused by `--include` and needs the separate unsafe flag, so
  approving one can never be a slip of the finger. An interactive selector can
  be added later without changing the staging contract.
- Selected inputs cross the boundary as an uncompressed tar beside the bundle
  and patch, not as a second Git bundle or a synthesized commit on the Mac.
  Reason: these files are by definition outside Git, tar carries the
  executable bit and symlinks the run needs, and it reuses the existing
  size- and SHA-256-verified upload path. The staged snapshot, not the
  archive, decides which names are legitimate; a mismatch fails staging.
- Credential-shaped names are matched on whole words (plus a fixed name and
  extension list), so `tokenizer.json` is not flagged while `api_token.json`
  is. The alternative, substring matching, produced false positives that would
  have trained the user to reach for the unsafe flag by habit.
- The selection is validated before the boundary and image work rather than
  inside `Prepare`, so a typo or a refused credential fails in seconds instead
  of after an image build. `Prepare` re-resolves the selection and remains the
  authority.

- A submodule is staged from its checked-out HEAD, not from the gitlink the
  superproject index records, and the superproject baseline then records where
  the submodule actually ended up. The alternative, reconstructing the
  recorded gitlink with `git submodule update`, would need the bundle to carry
  a commit that may be unreachable from the submodule's refs and would
  silently discard a submodule the user had moved. As a consequence the
  superproject patch is captured with `--ignore-submodules=all`, so gitlink
  changes travel exactly once.
- Nested submodules fail closed rather than being staged recursively. One
  level covers the repositories this is built for, and recursion multiplies
  the artifact, path-safety, and apply-journal surface. Lifting the limit is
  additive and does not change the stage format.
- A dirty submodule working tree is captured and committed inside the
  submodule, symmetrically with the superproject, rather than refused.
  Refusing would be simpler but would strand uncommitted submodule work on the
  Mac with no way to carry it into the run.

- The apply journal records only ref creations, because apply only ever
  creates `pisafe/<run>`. The documented compare-and-swap discipline and its
  recovery rules are implemented in full; the general old-value restore is
  not, because no code path produces a step with a previous value and an
  untested branch is worse than an absent one. Adding update steps later is
  additive.
- Submodule refs are committed before the superproject ref. The reverse order
  would let an interruption leave a superproject branch whose gitlinks name
  commits that no ref keeps reachable.
- Apply captures a run in a throwaway `--network=none` container over the
  run's workspace, rather than exec-ing into the live run container. It then
  works whether or not the run is up, costs none of the eight-hour budget,
  and needs no network or home mount.
- A prepared apply carries hashes and fixed artifact names, never filesystem
  paths. The alternative, reporting the paths the run wrote, would let a
  compromised run name a file on the Mac; instead both sides derive the same
  names from the same helper, and the Mac reads only from the package
  directory it chose.

## Primary references

- Pi security: <https://pi.dev/docs/latest/security>
- Pi containerization: <https://pi.dev/docs/latest/containerization>
- Pi providers and OAuth storage: <https://pi.dev/docs/latest/providers>
- Pi documentation/install scope: <https://pi.dev/docs/latest>
- Pi AI programmatic OAuth:  
  <https://github.com/earendil-works/pi-mono/blob/main/packages/ai/README.md>
- Lima plain mode: <https://lima-vm.io/docs/config/plain/>
- Lima filesystem mounts: <https://lima-vm.io/docs/config/mount/>
- Lima SSH: <https://lima-vm.io/docs/usage/ssh/>
- Podman machine volume behavior:  
  <https://docs.podman.io/en/latest/markdown/podman-machine-init.1.html>
- Podman rootless `pasta` networking: <https://docs.podman.io/en/stable/markdown/podman-network.1.html>
- Zed Remote Development: <https://zed.dev/docs/remote-development>
