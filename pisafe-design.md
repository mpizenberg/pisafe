# `pisafe` Design Specification

Date: 2026-07-23  
Status: proposed design after requirements interview

## Executive decision

Keep the `pisafe` name and the useful parts of the current container image, but
replace the current “mount this checkout into Podman” execution model.

The recommended design is:

1. A dedicated, mountless Linux VM on the Mac.
2. One isolated container and staged Git repository per run.
3. Zed Remote SSH into that same run container.
4. A Mac-side controller for approvals, network mediation, and credentials.
5. Git bundle transfer over SSH, with no macOS directory mounted into the VM.
6. `pisafe apply` importing a local `pisafe/<run>` branch without touching the
   current checkout.

This is a middle ground rather than a locked-down offline sandbox. Pi can edit
files, browse, download, install dependencies after approval, use persistent
tools and extensions, and commit autonomously. The security boundary prevents
those capabilities from becoming general access to the Mac.

## Requirements captured

- Protect the Mac and files outside the selected projects from mistakes,
  malicious project content, model output, dependencies, and unreviewed Pi
  extensions.
- Pi must be able to modify the project and commit autonomously when asked.
- The working copy may be staged as long as applying the result is easy.
- The staged files must be viewable and editable in Zed.
- Zed Remote SSH is acceptable.
- `git push`, publishing, deployment, and cloud CLI mutations require approval.
- Public GitHub reads are automatic.
- Private GitHub access is available only after persistent `gh` authentication.
- Unknown network destinations support: once, this session, this project, and
  always.
- Network requests wait indefinitely for approval if the user is away.
- Package installation requires approval. One approval covers the command and
  the registry/CDN destinations it needs for that command's duration.
- Global tools may persist inside the isolated environment, but installation or
  update requires approval.
- Dependency caches are per project.
- Pi extensions, settings, tools, and sessions persist.
- Extension/provider installation requires approval. Versions are pinned and
  updates are offered to the user rather than applied automatically.
- Ignored and untracked source files are excluded initially unless explicitly
  selected.
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
- The approval and credential broker.
- The VM and container runtimes.
- A pinned base image.
- A small, pinned Pi provider/mediation extension.
- The staging and Git import/export code.

Pi itself and its catalog extensions run inside the isolated environment. This
is why host Pi plus sandboxed shell tools is not sufficient for this threat
model: an extension runs in the Pi process and could otherwise access the host.

## Architecture

```text
macOS
├── original repository (never mounted into the VM)
├── pisafe controller
│   ├── approval UI and policy database
│   ├── network proxy
│   ├── model credential/inference broker
│   └── GitHub credential broker
├── macOS Keychain / host gh credential store
└── dedicated ARM64 Linux VM (no host filesystem mounts)
    ├── root-enforced network policy
    ├── shared read-only tools/extensions profile
    ├── per-project session and dependency-cache storage
    └── one rootless container per run
        ├── Pi and unreviewed extensions
        ├── staged Git repository
        ├── run-local writable home
        ├── Zed remote server, terminal, tasks, and language servers
        └── no raw model or GitHub credentials
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
`no-new-privileges`, and no container socket. The container network can reach
only the mediation services. It has no general default route.

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
pisafe approvals
pisafe apply <run>
pisafe discard <run>
pisafe gc
pisafe doctor

pisafe login chatgpt
pisafe login github
pisafe extension install <package>
pisafe extension update [package]
pisafe tool install <package>
```

`connect` resumes Pi or opens a shell. `zed` prints the path and opens Zed.
`discard` is explicit and destructive, so it identifies the exact run before
deleting it.

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

This commit is followed by the agent's own commits.

Untracked and ignored inputs are not silently copied. Selection should show
file names, types, and sizes, reject special files and escaping symlinks, and
warn about likely secrets. Explicitly included input files become part of the
baseline commit.

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

The branch is transferred as a Git bundle. Preserve the agent's commits
individually.

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
| GitHub credentials | host `gh` store/broker | not present |
| Pi sessions | project | yes |
| Dependency caches | project | yes |
| Staged repository | run | yes |
| Temporary downloads | run | yes |

Global settings are changed through a management command or a dedicated
configuration session, not by ordinary agent code.

Extension and global-tool installation runs in a separate installer context
after approval. Resolve and pin an exact version and integrity hash. Use
`--ignore-scripts` where compatible. Mount the resulting profile read-only into
agent runs.

An installed extension is still untrusted at runtime; pinning prevents a
surprise update, while the VM/container boundary limits its reach. Offer
available updates, release notes, source, and version diff, then wait for
approval.

Per-project sessions meet the persistence requirement without allowing one
project to read another project's transcripts by default.

## Network policy

### Enforcement

Use a controller-owned forward proxy plus container/VM firewalling:

- Direct container egress is denied.
- DNS is mediated and checked again after resolution.
- Loopback, link-local, metadata, private LAN, and host-service destinations are
  denied by default.
- The only internal destinations are the narrowly scoped proxy and broker.
- HTTPS CONNECT requests can be held while awaiting approval.
- Policy applies to Pi, extensions, shell commands, dependencies, Zed remote
  tooling, and language servers.

Do not rely only on Pi tool hooks or shell wrappers; malicious code can bypass
in-process checks. Hooks are useful for explaining a request and attaching a
command-duration capability, while the external proxy is the enforcement
point.

### Approval scopes

Store an approval as exact scheme/host/port plus its scope:

- **Once**: one blocked network intent.
- **Session**: until that run stops.
- **Project**: future runs of that repository.
- **Always**: all projects.

Avoid wildcard subdomains by default. “Always” should visibly warn that any
future project-readable data could be sent to that destination.

When no user is present, keep the originating operation paused. Show pending
requests in the run terminal, through `pisafe approvals`, and preferably through
a macOS notification. Approval from any controller terminal resumes it.

### Default rules

Automatic:

- Model traffic to the internal inference broker.
- Unauthenticated public GitHub reads.
- Public Git clone/fetch over HTTPS.
- Required signed/pinned `pisafe` infrastructure downloads.

Approval required:

- A new Internet destination.
- `npm install`, `pip install`, and equivalent package-manager operations.
- Extension, provider, or global-tool installation/update.
- Any authenticated GitHub mutation.
- Git push.
- Package publish.
- Deployments and cloud CLI mutations.

Package approval grants a short-lived capability to that process tree and the
required registry/CDN hosts. It expires when the command ends. Package
lifecycle scripts execute inside the run container but remain capable of
reading that project's staged files, so the prompt should say when scripts will
run.

An approved general-purpose website remains an exfiltration destination. The
approval system reduces accidental reach; it cannot make an approved host safe.

## Credentials and provider login

### ChatGPT subscription

`pisafe login chatgpt` runs the ChatGPT Plus/Pro OAuth flow through trusted
broker code based on `@earendil-works/pi-ai`. Persist the refresh token in the
macOS Keychain or a broker-only encrypted store.

Pi normally stores OAuth tokens in `~/.pi/agent/auth.json`. Do not put that file
in the run container. Instead:

1. A pinned provider extension sends structured model requests to the broker.
2. The broker refreshes OAuth and calls the provider.
3. It streams the model response back.
4. The run receives only a revocable, run-scoped broker capability.

Untrusted code can consume inference while its run is active, because Pi must
be able to do so, but it cannot extract the reusable OAuth token. Apply rate,
concurrency, and spend/usage limits and keep an audit log.

Other providers, including Kimi or DeepSeek, use the same broker interface.
Adding a provider or credential is an explicit approval.

### GitHub

Use the host `gh` credential store because persistent authentication was
requested and it is the simplest native storage path. The run never receives
the token, host SSH agent, or `gh auth token` output.

The broker provides:

- Unauthenticated public GET/read operations automatically.
- Authenticated private reads only after host `gh` login.
- A Git HTTPS read proxy for clone/fetch (`git-upload-pack`).
- Explicit approval for writes (`git-receive-pack`, REST/GraphQL mutations, and
  mutating `gh` commands).

`gh api` GET requests may be read operations; POST, PUT, PATCH, DELETE, GraphQL
mutations, and ambiguous commands require approval. Git-over-SSH is disabled by
default because no SSH key or agent is shared.

This is more work than copying `hosts.yml` into the container, but it is
necessary to satisfy both “persistent GitHub login” and “no push without
confirmation.” Raw credentials plus unrestricted GitHub access cannot enforce
that promise against malicious code.

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

Set Zed's `upload_binary_over_ssh` option where practical so the matching remote
server can be downloaded on the Mac and uploaded rather than requiring a new
guest network exception. Zed extensions and language-server downloads that run
remotely go through the same approval proxy.

## Lifecycle and cleanup

Run states:

```text
creating → active → stopped → imported | discarded | expired
```

- Active/stopped runs are resumable.
- Successful `apply` marks a run imported but keeps it recoverable for seven
  days.
- `discard` deletes only after exact run confirmation.
- `pisafe gc` removes imported/discarded runs older than seven days.
- Never expire a run with unimported commits merely because it is old. Warn and
  require explicit discard.
- Keep branch/import metadata after workspace deletion so an imported branch
  remains attributable to its source run.

## Audit record

For each run, retain:

- Run ID, project identity, captured source HEAD, and timestamps.
- Exact image, Pi, extension, and tool versions.
- Baseline and final commit IDs.
- Network approvals and their scope.
- Package/global installation approvals.
- Credential use events without secret values.
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
- Unrestricted `pasta` network → externally enforced dynamic approval proxy.
- Raw API-key environment forwarding → credential/inference broker.
- Old package scope → pinned `@earendil-works/pi-coding-agent`.
- Deprecated config paths → current `~/.pi/agent/` layout.
- One-shot container lifecycle → named resumable runs plus `apply`.

The result preserves the convenience features that were previously labelled
high risk, but changes *where* those capabilities terminate. Pi can write
freely, just not into the original checkout. It can use the network, just not
silently reach arbitrary destinations. Authentication persists, but reusable
secrets are not readable by extensions.

## Implementation order

### Phase 1: safe workspace and editor

- Dedicated mountless Lima VM.
- Pinned ARM64 run image and current Pi package.
- Per-run containers and volumes.
- Git bundle staging/import.
- Dirty baseline choices.
- Zed Remote SSH.
- Run listing, resume, diff, apply, discard, and seven-day GC.

This phase already provides the largest host-filesystem safety improvement.
Keep network off except for explicitly temporary manual access until Phase 2.

### Phase 2: mediated useful networking

- Controller proxy and approval queue.
- Once/session/project/always policy database.
- Private/local-address denial.
- Public GitHub reads.
- Command-duration package installation approvals.
- Notifications and `pisafe approvals`.

### Phase 3: persistent safe credentials

- ChatGPT OAuth inference broker.
- macOS Keychain persistence.
- Host `gh` integration and read/write mediation.
- Other provider onboarding.
- Usage limits and credential audit events.

### Phase 4: managed persistence

- Read-only global settings/extensions/tools profile.
- Approved installers, exact version/integrity pinning.
- Update notifications.
- Per-project sessions and dependency caches.
- Backup/reset/recovery commands.

The broker and dynamic network policy are real security components, not a few
shell wrappers. They need protocol tests, adversarial bypass tests, and
fail-closed behavior before `pisafe` claims to enforce credential or write
controls.

## Acceptance tests

The first usable release should prove:

1. Deleting `/work/<project>` cannot alter the original checkout.
2. The run cannot read arbitrary `/Users/...` files.
3. Container escape into the VM user still finds no host filesystem mount.
4. Pi/extensions cannot read ChatGPT or GitHub tokens.
5. Direct sockets cannot bypass the proxy.
6. DNS rebinding cannot reach the Mac, LAN, metadata, or loopback services.
7. An unknown domain pauses and can be approved with all four scopes.
8. An approved `npm install` loses registry access when the command exits.
9. Public clone/fetch works; push pauses for approval.
10. Zed terminal and Pi see the same staged files and toolchain.
11. Dirty baseline keep/drop behavior preserves later commits or fails safely.
12. `pisafe apply` creates a new local branch and does not touch the current
    index or working tree.
13. Two simultaneous runs cannot see or overwrite one another.
14. Interrupted transfer/import cannot create a silently partial branch.
15. Cleanup never deletes an unimported run without explicit confirmation.

## Residual risks

- The selected model provider receives repository content sent as context.
- Any approved network destination can receive readable project data.
- A malicious dependency can damage the staged repository or poison its
  per-project cache.
- A malicious extension can persist in the approved global extension profile
  until removed, though it remains confined to future sandboxes.
- A VM/hypervisor or controller vulnerability remains possible.
- The inference capability can be abused during an active run even though its
  underlying OAuth token is hidden.
- Zed is a trusted local application and necessarily receives file contents.
- User-approved GitHub or cloud mutations can still be harmful.

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

