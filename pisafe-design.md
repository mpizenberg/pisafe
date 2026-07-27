# `pisafe` Design Specification

Date: 2026-07-23
Status: proposed design; simplified to open egress with structural credential
isolation after weighing maintenance cost against the threat model

## Executive decision

Replace the "mount this checkout into Podman" execution model with:

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
capability. An earlier draft included a dynamic approval proxy and a GitHub
credential broker; they were removed as a maintenance and interruption cost
disproportionate to the mostly public, non-secret projects this tool targets.

## Requirements captured

- Protect the Mac and files outside the selected project from mistakes,
  malicious project content, model output, dependencies, and unreviewed Pi
  extensions.
- Pi must be able to modify the project and commit autonomously; the working
  copy may be staged as long as applying the result is easy, and the staged
  files must be viewable and editable over Zed Remote SSH.
- `git push`, publishing, deployment, and cloud CLI mutations must not happen
  as the user autonomously.
- Public GitHub and general internet reads are automatic; package and tool
  installation needs no approval. The user's private repositories and
  registries are unavailable through `pisafe`, and making them available
  reopens the credential-broker question.
- Ignored and untracked source files are excluded unless explicitly selected.
  Secret-bearing files such as `.env` are included only through that same
  selection flow, after a strong warning; there is no separate secret
  injection mechanism.
- Repositories may contain Git submodules; staging must support them. Git LFS
  is out of scope initially.
- ChatGPT subscription login persists; other providers such as Kimi or
  DeepSeek may be added later through the same broker.
- Pi extensions, settings, tools, and sessions persist. Global tools live
  inside the isolated environment, installed and updated through explicit
  `pisafe` commands with pinned versions; dependency caches are per project.
- No access to host-local services is currently needed.
- Completed runs may be automatically removed after seven days, and concurrent
  side-quest runs must be independent.

## Trust model

Untrusted: model output and commands, project files and project-local Pi
resources, dependencies and lifecycle scripts, Pi catalog extensions,
programs downloaded during a run, and Zed language servers, tasks, and remote
extensions.

The trusted computing base is deliberately small: the `pisafe` Mac controller,
the inference broker, the VM and container runtimes, a pinned base image, a
minimal provider integration (a `models.json` entry plus, if needed, a thin
pinned extension), and the staging and Git import/export code.

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

A dedicated Lima ARM64 VM in `plain` mode, provisioned with rootless Podman.
Plain mode explicitly disables filesystem mounts, SSH-agent forwarding,
dynamic port forwarding, and the guest agent; plain SSH and static forwarding
remain.

Repositories cross the boundary as SSH streams carrying Git bundles and a
validated archive for selected untracked files. Never mount `/Users`, the
repository, the Docker socket, or the host SSH agent. This is the point of
departure from a general-purpose Podman machine, which exposes `/Users`,
`/private`, and `/var/folders` read-write inside the VM: a container escape
there reaches Mac files without a hypervisor escape.

The controller embeds its managed run-image Containerfile and packages the
static Linux ARM64 guest helper as a sidecar. Podman builds that exact context
inside the dedicated mountless VM.

### Per-run container

Each run receives a unique run ID and container, its own staged Git repository
and quota-backed writable storage, a read-only global profile, per-project
session and dependency-cache volumes, a unique SSH endpoint, and a short-lived
broker capability. Limits cover CPU, memory, process count, temporary
filesystems, 10 GiB persistent storage, and eight cumulative active hours.

Use a non-root user, a read-only container root, dropped capabilities,
`no-new-privileges`, and no container socket. Egress is open through the VM's
static firewall, which denies host, LAN, link-local, metadata, and loopback
destinations.

Multiple runs for one project are independent snapshots: they do not see one
another's staged files or conversational context.

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
pisafe connect <run>
pisafe zed <run>
pisafe diff <run>
pisafe cp <run>:<path> [dest]
pisafe apply <run>
pisafe discard <run> --confirm <run>
pisafe gc [--dry-run]
pisafe doctor

pisafe login chatgpt

# Phase 2 (persistent profile management):
pisafe extension install <package>
pisafe extension update [package]
pisafe tool install <package>
```

`connect` resumes Pi or opens a shell. `zed` prints the path and opens Zed.
`diff` reports what a run changed without stopping it. `cp` copies a file or
directory out of a run — build artifacts, logs, screenshots that do not belong
in Git history. Because `cp` writes to the Mac from untrusted content, it must
reject absolute and `..` paths, escaping symlinks and hard links, and special
files, must not write through existing destination symlinks, must confirm
before overwriting, must enforce limits on total bytes, file count, and
individual file size, and must extract with directory-descriptor-relative
operations so the destination cannot be swapped for a symlink mid-copy.
`discard` is explicit and destructive, so its confirmation argument must
repeat the exact run ID before anything is deleted.

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

Untracked and ignored inputs are not silently copied. Selection shows file
names, types, and sizes, rejects special files and escaping symlinks, and
warns about likely secrets. A file that looks like a secret may still be
included when the user insists, but the prompt must present it as an unsafe
override: under open egress, everything in the run can read and exfiltrate it,
so including one voids the run's credential isolation. Explicitly included
inputs become part of the baseline commit.

### Submodules

Git bundles do not carry submodule contents. Staging bundles the superproject
and each initialized submodule and reconstructs the same layout in the run;
uninitialized submodules stay uninitialized. `apply` imports the superproject
branch and the referenced submodule histories into the corresponding local
repositories, creating a `pisafe/<run>` ref in each changed submodule so its
commits stay reachable, and reports which submodule commits the imported
branch expects.

Git LFS is out of scope for now: a repository using it is detected and refused
rather than staged incompletely.

### `pisafe apply`

Despite its name, `apply` does not check out files or merge into the current
branch. It imports the completed history as `refs/heads/pisafe/<run>`,
preserving the agent's commits individually. Before import it shows
uncommitted tracked changes, new non-ignored files, and ignored outputs
separately; tracked changes are captured as a final clearly labelled commit,
new files are included after confirmation, and ignored build outputs are not
imported by default.

The branch travels as an incremental bundle containing only commits new since
the captured HEAD, fetched into a temporary ref and moved into place only
after verification, so an interrupted transfer cannot leave a partial branch.

Because superproject and submodule refs live in separate repositories with no
cross-repository transaction, `apply` is journaled and idempotent: import and
verify every object set first, record the intended old/new refs in the run
manifest, then update refs one repository at a time. Every forward and
rollback update is compare-and-swap (`git update-ref <ref> <new>
<expected-old>`): a step whose ref already holds the new value is complete, a
rollback restores the old value only while the ref still holds the recorded
new value, and a ref matching neither stops recovery for manual reconciliation
rather than overwriting a change the user made meanwhile. The run is marked
imported only when every ref matches the manifest.

If a dirty baseline commit exists, prompt to either keep it with all following
commits, or replay only the later commits onto the original captured HEAD. The
replay happens in the isolated staged environment; if it cannot be done
cleanly, stop without changing the host repository and offer to keep the
baseline, resolve manually in the staged environment, or cancel.

If the original repository has advanced since the run began, import the branch
unchanged and report the divergence. Never silently rebase or merge; the user
can merge, rebase, or compare `pisafe/<run>` normally. Name collisions must
fail safely or select a new explicit branch name, never force-update an
existing branch.

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

Global settings change through a management command or a dedicated
configuration session, not by ordinary agent code. Extension and global-tool
installation runs in a separate installer context triggered by an explicit
`pisafe` command, resolving and pinning an exact version and integrity hash;
the resulting profile is mounted read-only into runs. An installed extension
is still untrusted at runtime — pinning prevents a surprise update, while the
VM/container boundary limits its reach. Offer updates rather than applying
them.

Each run starts from an immutable snapshot of the project's session store and
writes new run-specific session IDs to a run-local directory; on stop, new IDs
are appended to the project store and an existing ID is never overwritten, so
concurrent runs never read one another's live transcripts. Per-project
dependency caches are mounted as run-local writable overlays; content-addressed
entries may be merged back under a lock, while mutable or conflicting entries
stay run-local. Until this exists (Phase 2), runs get private caches.

## Network policy

Internet egress is open. There is no proxy, no approval queue, and no
per-destination policy — only a static packet filter, written once as VM-root
nftables rules:

- IPv6 is disabled in the VM initially; it can return later with an
  equivalently tested ruleset.
- Deny IPv4 loopback, link-local (including the metadata address
  `169.254.169.254`), RFC1918, CGNAT (`100.64.0.0/10`), multicast, broadcast,
  and the VM's own gateway and host-side addresses.
- Additionally deny the Mac's directly connected on-link prefixes, gathered
  at VM start/resume, so a LAN using globally routed IPv4 space is still
  covered; fail closed if they cannot be determined.
- Allow one exact exception: the inference broker relay address and port.
- Deny inbound connections. Per-run SSH enters through Lima's existing control
  connection and a container-local stdio relay, not a VM listener.
- Allow everything else, over any protocol.

Two implementation details are load-bearing. Rules must filter all VM egress
(output and forward hooks), not only forwarded container packets: rootless
Podman's `pasta` networking emits container traffic from a userspace process in
the VM, which a forward-only ruleset never sees. And the VM's resolver must be
a public DNS server, since the usual default resolver is the private gateway
address the deny set blocks. A container may use its own loopback freely; VM
and Mac loopback services stay unreachable.

Because filtering happens at the packet level on resolved destination
addresses, DNS tricks such as rebinding cannot reach denied ranges. The rules
apply uniformly to Pi, extensions, shell commands, dependencies, Zed remote
tooling, and language servers, and there is nothing dynamic to maintain.
Bypass tests should cover raw TCP and UDP, numeric IPs, DNS answers pointing
at private ranges, HTTP redirects, and `host.containers.internal`.

The accepted consequence, stated plainly: any code in a run can send anything
the run can read to any internet destination, without record or interruption.
A run can read more than the working tree — the staged repository's full
reachable history, initialized submodule histories, project-local Pi
resources, its snapshot of prior project sessions, the dependency cache, and
the read-only global profile. The non-confidentiality assumption therefore
covers the repository *including its history* and persisted project state.
Keeping secrets out of all of that is the load-bearing control; a warning scan
is a reminder, not proof of absence.

If a genuinely secret project ever needs sandboxing, the answer is not to bolt
approvals back on; it is to run it with the VM's egress switched to a deny-all
or allowlist profile for the duration (`pisafe run --offline` is a plausible
future flag).

## Credentials and provider login

### ChatGPT subscription

`pisafe login chatgpt` runs the ChatGPT Plus/Pro OAuth flow through trusted
broker code based on `@earendil-works/pi-ai`, persisting the refresh token in
the macOS Keychain. Pi normally stores OAuth tokens in `~/.pi/agent/auth.json`;
that file must not exist in the run container. Instead the broker is declared
in Pi's `models.json` as a local provider endpoint speaking a supported
standard API, attaches credentials and refreshes OAuth itself, calls the
provider, and streams the response back unchanged. The run receives only a
revocable, run-scoped capability.

Speaking a standard wire format instead of a custom pisafe protocol keeps
streaming and tool-call fidelity upstream's problem. Pi's extension API is
pre-1.0 and changes often, so any extension code stays thin; the `models.json`
route is the primary integration.

The broker lives on the Mac, which the firewall denies, so its path into runs
is explicit: the controller opens one reverse SSH relay into the VM, the VM
exposes a single dedicated relay address and port to containers — the
firewall's one exception — and the relay speaks only the standard inference
API, requires the run-scoped capability, and cannot reach any other host
address or port. The reverse forward binds only that dedicated address, with
`sshd` configured to permit exactly that binding, and the firewall must admit
it in every hook the rootless `pasta` connection traverses. The relay closes
when the controller exits, rejects capabilities of stopped runs immediately
(resume issues a fresh one), and fails closed on unknown paths or methods,
oversized requests, and any attempt to use it as a general proxy.

Untrusted code can consume inference while its run is active, because Pi must
be able to, but it cannot extract the reusable OAuth token. A per-run
concurrency cap and the provider's own subscription limits bound the abuse;
elaborate metering is not worth its maintenance. Other providers use the same
broker interface, added by an explicit `pisafe login` command.

### GitHub

There is no GitHub integration, by design. No token, `hosts.yml`, SSH key, or
agent socket ever enters the VM or container, and `pisafe` stores no GitHub
credentials anywhere. Public clones, fetches, and API reads work directly over
the open network; the user's private repositories are unavailable through
`pisafe`; pushing, publishing, and every authenticated mutation as the user
happen on the Mac, typically after `pisafe apply`.

This one decision is what makes open egress acceptable: a run that receives no
user credentials cannot push, publish, or deploy *as the user* no matter what
code it executes. Malicious code can still write anonymously or use
credentials it carries itself — that is part of the accepted exfiltration
surface, not a breach of this guarantee. The guarantee is structural rather
than enforced, so there is nothing to test, bypass, or maintain. Copying user
credentials into the sandbox "just for convenience" would silently void it and
must remain out of scope.

## Zed Remote

Generate one SSH alias per run and connect Zed to the run container through the
VM, printing both the URI and the staged path:

```text
zed ssh://pisafe-<run>/work/<project>
```

Each alias uses a strict per-run OpenSSH config whose `ProxyCommand` opens
Lima's generated control-SSH connection and runs an interactive, non-TTY
`podman exec` that relays bytes to `sshd` on the container's loopback. No
container port is published in the VM or on macOS. The client private key
remains on macOS; the container receives only its public key, generates its own
host key, and runs `sshd` as the non-root run user with password, agent, X11,
and TCP forwarding disabled. The Mac pins that host key before first connection.

Zed runs source, language servers, tasks, and terminal commands on the remote
machine, which keeps executable project tooling in the sandbox. The local UI
still sees source text, parses it, and stores unsaved state; Zed is therefore
trusted as a local editor, but project build tools do not execute on macOS.
Zed's remote server, extensions, and language-server downloads fetch directly
over the open network.

## Lifecycle, cleanup, and the run record

```text
creating → active → stopped → imported → reclaimed
```

- A run has a record for exactly as long as it owns something. Reclaiming it
  removes the record with the resources, so there is no terminal state.
- Active/stopped runs are resumable. Resuming issues a fresh short-lived broker
  capability rather than extending the old one.
- Stopped time does not consume the eight-hour active budget. Podman kills a
  run independently when its current remaining budget expires; the next
  lifecycle command reconciles the durable record to stopped.
- Successful `apply` marks a run imported but keeps it recoverable for seven
  days: the workspace still holds untracked leftovers the branch never took.
- `discard` reclaims at any point, after exact run confirmation.
- `pisafe gc` reclaims imported runs older than seven days, and reports or
  prunes long-unused per-project caches and session stores.
- Never reclaim a run with unimported commits merely because it is old. Warn
  and require explicit discard.

While a run exists, its record holds the run ID, project identity, captured
source HEAD, timestamps, exact image and tool versions, baseline and final
commit IDs, explicitly included input files by name, bundle hashes, and the
imported branch name. Nothing is retained once the run is reclaimed: what it
produced is already in the user's repository, on a branch named `pisafe/<run>`
after the run itself, with the base commit as its parent and the author it
committed as — so a surviving record would only restate what git holds.

Do not log prompts, source contents, HTTP bodies, environment variables, OAuth
tokens, or package secrets by default.

## Implementation order

### Phase 1: safe workspace, editor, and brokered inference

Dedicated mountless Lima VM with the static firewall rules; pinned ARM64 run
image and current Pi package; per-run containers and quota-backed storage; Git
bundle staging/import including submodules; dirty baseline choices; Zed Remote
SSH; a minimal ChatGPT OAuth inference broker with Keychain persistence,
registered as a standard-API `models.json` provider; and run listing, resume,
diff, apply, cp, discard, and seven-day GC.

The inference broker is in Phase 1 because Pi cannot function without model
access, and raw credentials must never enter the container, even temporarily.
Phase 1 is a complete MVP whose configuration is ephemeral per run; everything
after it is quality of life, not safety.

### Phase 2: managed persistence

Read-only global settings/extensions/tools profile; installer commands with
exact version/integrity pinning; update notifications; per-project sessions and
dependency caches; other provider onboarding; backup/reset/recovery commands.

The two components that carry security weight — the firewall rules and the
inference broker — deserve real tests: bypass attempts against the packet
filter, a broker contract test covering streaming, tool calls, reasoning
events, and errors, and fail-closed behavior when the broker or upstream is
down.

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
    updates, either completes or restores prior refs on retry.
12. Cleanup never deletes an unimported run without explicit confirmation.
13. A repository with submodules stages, runs, and applies with superproject
    and submodule commits preserved and reachable via per-submodule
    `pisafe/<run>` refs.
14. `pisafe cp` refuses traversal paths, escaping symlinks, and special files,
    enforces size and count limits, and never overwrites without confirmation.

## Residual risks

- Silent exfiltration of everything a run can read, as described under Network
  policy. Acceptable only while the projects involved are non-confidential.
- Open egress also permits outbound abuse — scanning, spam, cryptomining —
  that harms third parties rather than the user's data. Static
  bandwidth/connection caps, or restricting ordinary runs to DNS and TCP
  80/443, would shrink this without reintroducing prompts.
- The selected model provider receives repository content sent as context.
- A malicious dependency can damage the staged repository or poison its
  per-project cache; a malicious extension can persist in the pinned global
  profile until removed, though it remains confined to future sandboxes.
- A VM/hypervisor or controller vulnerability remains possible.
- The inference capability can be abused during an active run even though its
  OAuth token is hidden; the practical bound is the subscription's own limits.
- Zed is a trusted local application and necessarily receives file contents.
- Driving the ChatGPT subscription OAuth flow from a non-official client may
  sit in a gray area of the provider's terms of use, and that backend is an
  unofficial surface that can change without notice; inference then fails
  closed and loudly until the pinned `pi-ai` dependency ships a fix.

## Decisions

Each entry states what was decided, what was not taken, and why. Reversibility
is noted only where undoing the choice would be costly.

This log was condensed once, when the documents were compressed: entries that
had become plain descriptions of shipped code, or that recorded a choice
already reversed by a later one, were dropped rather than kept as history. Git
history holds them. New entries are appended in full.

### VM boundary and SSH

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

### Storage and lifecycle

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

### Staging and apply

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

### Getting work out

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

### Inference broker

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
  rewriting would own streaming and tool-call fidelity, which this design
  leaves upstream.
- The run-side model list is a curated catalog embedded from the pinned Pi AI
  Codex data with per-model `api`/`provider`/`baseUrl`/`headers` stripped, so a
  models.json entry can never route a run around the broker. Live catalog
  refresh was not retained; the catalog moves with the Pi pin in the same
  commit.
- `pisafe-guest configure-inference` also pins `transport: "sse"` in the run's
  Pi settings, merging rather than replacing what Pi wrote itself: Pi's default
  auto transport dials a WebSocket first, which the HTTP relay cannot speak.

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
