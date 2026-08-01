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
gaps are in [`IMPLEMENTATION_PROGRESS.md`](IMPLEMENTATION_PROGRESS.md). Every
command below exists and works.

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
pisafe login anthropic       # or an API key, read from stdin and kept there too
pisafe login                 # what is logged in; runs are offered all of it
pisafe broker                # foreground; runs have no inference without it
```

```sh
pisafe run [--include PATH]... [--include-unsafe PATH]...
pisafe list
pisafe connect RUN [--shell]
pisafe zed RUN
pisafe stop RUN
pisafe resume RUN
pisafe diff RUN
pisafe cp RUN:PATH [DEST] [--force]
pisafe apply RUN [--keep-baseline|--drop-baseline]
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

`connect` attaches your terminal to a run and starts Pi in its workspace, or
opens a shell there with `--shell`. It needs no editor, and it reaches the same
container, files, and network policy the Zed terminal does.

`diff` reports a run's commits, changed paths with line counts, and untracked
leftovers, without stopping it and without printing file content. `cp` takes a
single file or directory back out. `apply` stops the run and imports its
history as `pisafe/RUN`, in the superproject and in each changed submodule,
leaving your checkout, index, and current branch untouched.

If the run started from an uncommitted working tree, pisafe committed that state
for it, and `apply` asks once whether to import that commit too or to replay
only the run's own commits onto the commit you were on. The replay happens
inside the run; if the run's commits change lines the carried-in work changed,
nothing is imported, the run is left exactly as it was, and you can keep the
baseline instead or resolve it in the run and apply again. `--keep-baseline` and
`--drop-baseline` answer in advance.

```sh
pisafe extension install PACKAGE[@VERSION]   # into the profile every run mounts
pisafe extension update [PACKAGE...]         # offered, applied only when named
pisafe extension remove PACKAGE
pisafe extension list
pisafe tool install PACKAGE[@VERSION]        # a command on every run's PATH
pisafe tool remove PACKAGE
pisafe tool list
```

Runs cannot install anything globally: the profile they mount is read-only, and
only these commands write it. Each pins an exact version and refuses bytes that
do not hash to the integrity npm reported for it, so a release republished under
the same version fails rather than installs. Inside a run, `pi -e PACKAGE` still
tries an extension for that run alone. Nothing updates itself: when a run stops,
pisafe checks at most once a day what the registry now resolves the installed
names to and tells you, and a pin moves only when you name the package.

```sh
pisafe project list                       # what pisafe holds, per checkout
pisafe project reset [PATH]               # throw away a project's caches
pisafe project drop PATH --confirm PATH   # and its session transcripts with them
pisafe project rebind OLD-PATH            # a moved checkout keeps its history
pisafe profile reset --confirm            # every extension and tool back out
```

Runs of one repository share a dependency cache and the transcripts of the runs
before them. `project reset` throws the caches away, which costs the next run a
fetch and nothing else; `project drop` takes the transcripts too, which nothing
reproduces. A project is keyed by the path of its checkout, so moving or
renaming a repository leaves its history behind — `project rebind` run from the
new location claims it, naming the old path.

```sh
pisafe backup DIRECTORY    # copy out what nothing can refetch
pisafe restore DIRECTORY   # put it back into a VM
```

A backup holds every project's session transcripts and the pins naming what the
profile has installed. Dependency caches are left out because nothing needs one
to be correct, and no provider credential is written at all — those stay in the
macOS Keychain, which is the boundary the broker exists to hold. A restore puts
the stores back and reinstalls each package from the pin the backup recorded, so
what arrives is checked against the hash that was installed rather than against
whatever npm resolves the name to now. Neither direction ever overwrites:
backing up again into the same directory adds to it, restoring twice is
harmless, and a package already installed is left at whatever it is pinned to.

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
