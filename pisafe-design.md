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
cannot touch the Mac or the original checkout, and a run holds no credentials,
so it cannot act as the user anywhere. An earlier draft of this design
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
  autonomously. Satisfied structurally: no credentials exist in the sandbox,
  so these operations are only possible from the Mac, by the user.
- Public GitHub and general internet reads are automatic (open egress).
- Private GitHub repositories and registries are not reachable from inside a
  run. If this becomes a real need, it reopens the credential-broker question
  and is out of scope for now.
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
        └── no credentials of any kind
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

The current `pisafe/Containerfile` remains useful as a starting point for the
run image, but Podman should run inside this dedicated mountless VM.

### Per-run container

Each run receives:

- A unique run ID and container.
- Its own staged Git repository and writable run volume.
- A read-only mounted global profile.
- A per-project session volume.
- A per-project dependency-cache volume.
- A unique SSH endpoint and short-lived broker capability.
- CPU, memory, process-count, disk, and wall-clock limits.

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
pisafe discard <run>
pisafe gc
pisafe doctor

pisafe login chatgpt
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
before overwriting. `discard` is explicit and destructive, so it identifies
the exact run before deleting it.

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
may still be included when the user insists; the warning must state that
everything in the run — dependencies, extensions, and downloaded code — can
read it. Explicitly included input files become part of the baseline commit.

### Submodules

Git bundles do not carry submodule contents. Staging must therefore bundle
the superproject and each initialized submodule and reconstruct the same
layout in the staged repository; uninitialized submodules stay uninitialized.
`pisafe apply` imports the superproject branch and the referenced submodule
histories into the corresponding local repositories, creating a
`pisafe/<run>` ref in each changed submodule so its commits stay reachable,
and reports which submodule commits the imported branch expects. Verify all
transfers before updating any repository's refs.

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
from a snapshot of the project's session store and writes to a run-local
session directory that is merged back when the run stops, so concurrent runs
never read one another's live transcripts.

Per-project dependency caches need locking or a run-local overlay so that two
concurrent installs cannot corrupt them.

## Network policy

Internet egress is open. There is no proxy, no approval queue, and no
per-destination policy. The policy is a static packet filter, written once as
VM-root nftables rules and applied to all container traffic:

- Deny loopback, link-local, cloud-metadata, and RFC1918/LAN destination
  addresses, including the Mac and anything on the home network.
- Deny inbound connections except the per-run SSH endpoint.
- Allow everything else, over any protocol.

Because filtering happens at the packet level on resolved destination
addresses, DNS tricks such as rebinding cannot reach denied ranges; a name
that resolves to a LAN address is simply unreachable. The rules apply
uniformly to Pi, extensions, shell commands, dependencies, Zed remote
tooling, and language servers, and there is nothing dynamic to maintain.

The accepted consequence, stated plainly: any code in a run — including a
malicious dependency — can send anything the run can read to any internet
destination, without record or interruption. What a run can read is the
staged project, explicitly included files, and its own outputs. Keeping
secrets out of runs is therefore the load-bearing control, which is why
`.env`-style inclusion carries a strong warning and no credentials of any
kind exist inside the sandbox.

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
- Private repositories are unreachable from inside a run.
- Pushing, publishing, and every authenticated mutation happen on the Mac,
  by the user, typically after `pisafe apply` — exactly as for any local
  branch.

This one decision is what makes open egress acceptable: a run that holds no
credentials cannot push, publish, or deploy as the user no matter what code
it executes. The guarantee is structural rather than enforced, so there is
nothing to test, bypass, or maintain. Copying credentials into the sandbox
"just for convenience" would silently void it and must remain out of scope.

## Zed Remote

Generate one SSH alias per run and connect Zed to the run container through the
VM. `pisafe run` invokes the equivalent of:

```text
zed ssh://pisafe-<run>/work/<project>
```

Also print this URI and the staged path.

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
- Per-run containers and volumes.
- Git bundle staging/import, including submodules.
- Dirty baseline choices.
- Zed Remote SSH.
- Minimal ChatGPT OAuth inference broker with macOS Keychain persistence,
  registered as a standard-API `models.json` provider.
- Run listing, resume, diff, apply, cp, discard, and seven-day GC.

The inference broker is in Phase 1 because Pi cannot function without model
access, and raw credentials must never enter the container, even temporarily.
Phase 1 is a complete, usable product: everything after it is quality of
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
4. No credential — ChatGPT token or otherwise — is readable anywhere in the
   VM or container.
5. The Mac, LAN, link-local, metadata, and loopback destinations are
   unreachable from a run, including via DNS names resolving to them.
6. `npm install` and a public GitHub clone work with no prompt; a push
   attempt fails for lack of credentials.
7. Zed terminal and Pi see the same staged files and toolchain.
8. Dirty baseline keep/drop behavior preserves later commits or fails safely.
9. `pisafe apply` creates a new local branch and does not touch the current
   index or working tree.
10. Two simultaneous runs cannot see or overwrite one another.
11. Interrupted transfer/import cannot create a silently partial branch.
12. Cleanup never deletes an unimported run without explicit confirmation.
13. A repository with submodules stages, runs, and applies with superproject
    and submodule commits preserved and reachable via per-submodule
    `pisafe/<run>` refs.
14. `pisafe cp` refuses traversal paths, escaping symlinks, and special
    files, and never overwrites without confirmation.

## Residual risks

- Anything a run can read — the staged project, explicitly included files,
  and run outputs — can be exfiltrated to any internet destination, silently.
  This is the deliberate trade of open egress and is acceptable only while
  the projects involved are non-secret and no credentials enter runs.
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
- Zed Remote Development: <https://zed.dev/docs/remote-development>

