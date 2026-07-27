# pisafe

`pisafe` runs a coding agent (Pi) against your repository without giving it
your repository, your Mac, or your credentials.

Each run gets a staged copy of the current checkout inside a dedicated,
mountless Lima VM, in a rootless Podman container reachable over Zed Remote
SSH. The original checkout is never mounted and never modified: work comes
back only when you ask for it, as a local `pisafe/RUN` branch. Internet egress
is open, but the Mac, the LAN, and link-local/metadata addresses are denied by
a static VM firewall, and no provider or GitHub credential ever enters a run —
inference is relayed from a Mac-side broker through a revocable per-run
capability.

The isolation model is specified in [`pisafe-design.md`](pisafe-design.md),
with the reasoning behind individual choices in
[`DECISIONS.md`](DECISIONS.md). Implementation status, verification, and known
gaps are in [`IMPLEMENTATION_PROGRESS.md`](IMPLEMENTATION_PROGRESS.md). Phase 1
is in progress: every command below exists and works.

## Requirements

macOS on Apple silicon, Lima 2.2.0 or newer, and Go 1.26 to build. Run
`./pisafe doctor` to check.

## Build

```sh
go build -trimpath -buildvcs=false -o pisafe ./cmd/pisafe
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
  go build -trimpath -buildvcs=false \
  -o pisafe-guest-linux-arm64 ./cmd/pisafe-guest
```

The release layout places `pisafe-guest-linux-arm64` beside `pisafe`; during
development, `PISAFE_GUEST_HELPER=/absolute/path/to/helper` selects it
explicitly. The run-image Containerfile is compiled into the controller.

## Use

```sh
pisafe login chatgpt         # once: stores a ChatGPT Plus/Pro login in the Keychain
pisafe broker                # foreground; runs have no inference without it
```

```sh
pisafe run [--include PATH]... [--include-unsafe PATH]...
pisafe list
pisafe zed RUN
pisafe stop RUN
pisafe resume RUN
pisafe diff RUN
pisafe cp RUN:PATH [DEST] [--force]
pisafe apply RUN
pisafe discard RUN --confirm RUN
pisafe gc [--dry-run]
pisafe doctor
```

`run` stages the current repository's tracked state, including uncommitted
changes, as a baseline commit. Untracked and ignored files stay behind unless
`--include` names them; a credential-shaped path additionally needs
`--include-unsafe`, because everything in the run can read and exfiltrate it.
The command prints a one-time `ssh -F` line to paste into Zed's Remote
Projects dialog — pisafe never edits your global SSH or Zed settings.

`diff` reports a run's commits, changed paths with line counts, and untracked
leftovers, without stopping it and without printing file content. `cp` takes a
single file or directory back out. `apply` stops the run and imports its
history as `pisafe/RUN`, in the superproject and in each changed submodule,
leaving your checkout, index, and current branch untouched.

`discard` reclaims a run at any time; `gc` reclaims imported runs seven days
after they were applied and prunes superseded run images. Both delete the run's
record along with what it owned — the `pisafe/RUN` branch is what keeps the
work. A run whose work was never imported is never reclaimed by age.

Runs have no GitHub access: push, publish, and every authenticated mutation
happen on the Mac after `apply`.

## Development

```sh
go test -race -cover ./...
go vet ./...
```

The live suite is gated because it creates or reuses the dedicated `pisafe` VM
and may download images:

```sh
PISAFE_LIVE_LIMA=1 go test -v ./internal/lima
PISAFE_LIVE_LIMA=1 go test -v ./internal/runimage
```

The end-to-end artifact/container test additionally needs the immutable ID of
a locally built run image:

```sh
PISAFE_LIVE_LIMA=1 \
PISAFE_LIVE_RUN_IMAGE=sha256:<image-id> \
go test -v -run TestLiveSSHStageAndContainerMaterialize ./internal/lima
```
