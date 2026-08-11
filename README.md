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
explicitly. The run-image Containerfile is compiled into the controller. Both
commands are one build: `pisafe` refuses a helper that answers to a different
set of calls than it makes, before a run exists rather than inside a half-made
one, and names what to rebuild.

## Getting started

Check the host, then log in once and leave a broker running in its own
terminal — runs have no inference without it:

```sh
./pisafe doctor
pisafe login chatgpt
pisafe broker
```

From the repository you want worked on:

```sh
cd ~/code/my-project
pisafe run
```

The first run creates the VM and builds the run image, which takes a few
minutes; later ones start in seconds. `run` prints a run ID; `pisafe zed RUN`
then opens the staged repository in Zed, or reach the same container from your
terminal with `pisafe connect RUN`, which opens a shell in it — type `pi`
there to start the agent. It can build, fetch, and run tests without touching
your checkout.

When the run has something you want:

```sh
pisafe diff     # what changed, without stopping it
pisafe apply    # stops it, imports its history as branch pisafe/RUN
```

A command that takes a run finds it without being told, as long as the checkout
you are in has exactly one run left to import.

Review, merge, and push that branch from the Mac as usual — the run itself
never had GitHub access. `pisafe discard RUN --confirm RUN` throws away one you
do not want.

Runs of the same repository already share the transcripts of the runs before
them. They share a dependency cache only if the repository asks, by committing
`.config/pisafe.json`:

```json
{"caches": [
  {"name": "npm", "env": ["npm_config_cache"], "key": ["package-lock.json"]}
]}
```

Each entry names a cache, the environment variables that point a tool at it,
and the repository files whose contents decide which stored generation a run
starts from. Nothing else changes: the next `pisafe run` restores the snapshot
an earlier one published.

## Use

```sh
pisafe login chatgpt         # once: stores a ChatGPT Plus/Pro login in the Keychain
pisafe login anthropic       # or an API key, read from stdin and kept there too
pisafe login                 # what is logged in; runs are offered all of it
pisafe broker                # foreground; runs have no inference without it
```

A run opens Pi on GPT-5.6 Sol at high reasoning effort, from the first login
that offers that model, and on Pi's own choice when none does. Pick another with
`/model` inside the run and that run keeps it, across stop and resume.

```sh
pisafe run [--include PATH]... [--include-unsafe PATH]...
pisafe list
pisafe connect [RUN] [-- COMMAND...]
pisafe forward [RUN] [LOCAL:]PORT...
pisafe zed [RUN]
pisafe stop [RUN]
pisafe resume [RUN]
pisafe diff [RUN]
pisafe cp [RUN]:PATH [DEST] [--force]
pisafe cp PATH [RUN]: [--force] [--unsafe]
pisafe apply [RUN] [--keep-baseline|--drop-baseline] [--include-force]
pisafe discard RUN --confirm RUN
pisafe gc [--dry-run]
pisafe doctor
```

Every command that takes a `RUN` can be given none, and then means the one run
of the checkout you are standing in that has not been imported yet. Two live
runs of one repository is a question pisafe asks rather than answers, and
`discard` always names its run twice.

`run` stages the current repository's tracked state, including uncommitted
changes, as a baseline commit. Untracked and ignored files stay behind unless
`--include` names them; a credential-shaped path additionally needs
`--include-unsafe`, because everything in the run can read and exfiltrate it.
The command prints an `ssh -F` line that reaches the run from any SSH client.

An included path crosses as files rather than as history, so it arrives in the
run untracked just as it sits here, and `apply` copies the work left under it
back into your working tree. What `--include` records is the path you named, not
the files in it at the time: `--include plans/` on a directory that is empty, or
ignored, or both is how a run hands back work you never wanted committed. The
copy only ever adds and updates — a file the run deleted stays here — and a path
that changed both in the run and here holds the whole copy back until you
resolve it or pass `--include-force`.

`zed` opens a run's workspace in Zed. A run's alias is defined only in pisafe's
own per-run SSH config, and Zed hands `ssh` nothing but what a saved connection
carries, so `pisafe zed` saves one for the run and removes it again when the run
is discarded or collected. That list is the only file outside pisafe's own state
it writes, and it holds nothing but the run's alias and the config file to reach
it by; your SSH configuration is never touched.

`connect` attaches your terminal to a shell in the run's workspace, where `pi`
starts the agent. It needs no editor, and it reaches the same container, files,
and network policy the Zed terminal does. A command after `--` runs there
instead and exits with its status; its words are parsed by the run's own shell,
so redirects and pipes mean there what they mean here:

```sh
pisafe connect -- npm test
pisafe connect -- 'cat build.log' > build.log
```

`forward` is how you look at something a run is serving. `pisafe forward 5173
8080` starts a backend and frontend dev server's ports on this Mac's loopback,
so `http://127.0.0.1:5173` in your browser reaches the one inside the run; use
`3000:8080` when the local port is already taken. Nothing is published in the VM
or beyond this Mac, the run gets no way to reach anything here, and the forward
ends when you press Ctrl-C. It does put a page the run wrote in your browser, so
forward the ports you mean to look at rather than leaving one open.

`diff` reports a run's commits, changed paths with line counts, and untracked
leftovers, without stopping it and without printing file content. `cp` moves a
single file or directory across, in either direction: the colon marks the end
that is in the run, and naming the run is optional as everywhere else. A
destination that is already a directory takes the copy inside it, and any other
existing destination is replaced only with `--force`.

```sh
pisafe cp RUN:dist ./dist          # out of a run
pisafe cp cf-analytics.json RUN:   # into one, already under way
```

Copying in is how a run gets what it cannot fetch for itself — a query result,
a fixture, a log from somewhere it has no credential to reach. It is data, not
credentials: a credential-shaped name needs `--unsafe`, because everything in
the run can then read and exfiltrate it. `apply` stops the run
and imports its history as `pisafe/RUN`, in the superproject and in each changed
submodule, leaving your checkout, index, and current branch untouched.

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

These commands are the only thing that writes the profile every run mounts, and
each pins an exact version and refuses bytes that do not hash to the integrity
npm reported for it, so a release republished under the same version fails
rather than installs. Inside a run, `pi install` and `pi -e` still work — the
package lands in that run's own home, serves it, and dies with it, and stopping
the run tells you what it installed so keeping one is a command away. Nothing
updates itself: when a run stops, pisafe checks at most once a day what the
registry now resolves the installed names to and tells you, and a pin moves only
when you name the package.

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
