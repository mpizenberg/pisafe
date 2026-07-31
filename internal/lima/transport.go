package lima

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mpizenberg/pisafe/internal/gitstage"
	"github.com/mpizenberg/pisafe/internal/profile"
	"github.com/mpizenberg/pisafe/internal/runcontainer"
	"github.com/mpizenberg/pisafe/internal/runid"
	"github.com/mpizenberg/pisafe/internal/runssh"
)

const (
	remoteRunRoot = ".local/share/pisafe/runs"

	createStageScript = `set -eu
umask 077
base="$HOME/.local/share/pisafe/runs"
mkdir -p "$base"
chmod 0700 "$HOME/.local/share/pisafe" "$base"
run_root="$base/$1"
if ! mkdir "$run_root"; then
	echo "run directory already exists: $1" >&2
	exit 17
fi
mkdir "$run_root/stage"
printf '%s\n' "$run_root/stage"
`

	uploadStageScript = `set -eu
umask 077
stage="$HOME/.local/share/pisafe/runs/$1/stage"
test -d "$stage"
target="$stage/$2"
test ! -e "$target"
temporary="$stage/.$2.tmp.$$"
trap 'rm -f "$temporary"' EXIT HUP INT TERM
cat >"$temporary"
actual_size=$(wc -c <"$temporary")
test "$actual_size" = "$3"
actual_hash=$(sha256sum "$temporary")
actual_hash=${actual_hash%% *}
test "$actual_hash" = "$4"
chmod 0600 "$temporary"
mv "$temporary" "$target"
trap - EXIT HUP INT TERM
`

	removeRunScript = `set -eu
run_root="$HOME/.local/share/pisafe/runs/$1"
case "$run_root" in
	"$HOME/.local/share/pisafe/runs/"*) ;;
	*) exit 64 ;;
esac
rm -rf -- "$run_root"
`

	importStageScript = `set -euo pipefail
	run_root="$HOME/.local/share/pisafe/runs/$1"
	stage="$run_root/stage"
	test -d "$stage"
	workspace="/var/lib/pisafe/runs/$1/workspace"
	test -d "$workspace"
	tar --format=posix --numeric-owner --owner=1000 --group=1000 \
	-C "$run_root" -cf - stage |
	podman unshare tar --numeric-owner -C "$workspace" -xf -
`

	fetchApplyScript = `set -eu
workspace="/var/lib/pisafe/runs/$1/workspace"
test -d "$workspace"
exec podman unshare cat "$workspace/apply/$2"
`

	removeApplyScript = `set -eu
workspace="/var/lib/pisafe/runs/$1/workspace"
test -d "$workspace"
podman unshare rm -rf -- "$workspace/apply"
`

	// selectSnapshotsScript reads one line per requested cache: the exact key
	// when that generation exists, the newest generation when it does not, and
	// nothing when the namespace has never been published to. It only reads,
	// so it can run while other runs of the project are live.
	selectSnapshotsScript = `set -eu
exec podman unshare sh -ceu '
set -eu
namespace=/var/lib/pisafe/projects/$1/cache
shift
while [ "$#" -gt 0 ]; do
	directory=$namespace/$1
	key=$2
	shift 2
	if [ -d "$directory/$key" ]; then
		printf %s\\n "$key"
	else
		newest=$(ls -1t "$directory" 2>/dev/null | head -n 1 || true)
		printf %s\\n "$newest"
	fi
done
' pisafe-select "$@"
`

	// prepareRunLayoutScript builds what the run's mounts need inside the
	// filesystems the privileged helper allocated: the run-private half of
	// every overlay, and the mountpoint for the read-only profile. Both the
	// mode and the ownership are what the container needs, and are what the
	// helper would have applied had any of these names been knowable when it
	// allocated the filesystem. The mountpoint matters even though Podman would
	// create one: a mountpoint Podman creates is owned by the container's root,
	// which would leave Pi unable to write its own settings beside it.
	prepareRunLayoutScript = `set -eu
exec podman unshare sh -ceu '
set -eu
root=/var/lib/pisafe/runs/$1
overlay=$root/overlay
shift
test -d "$overlay"
create() {
	install -d -m 0700 -o 1000 -g 1000 "$@"
}
create "$root/home/.pi" "$root/home/.pi/agent" "$root/home/.pi/agent/npm"
create "$overlay/sessions" "$overlay/sessions/upper" "$overlay/sessions/work"
while [ "$#" -gt 0 ]; do
	create "$overlay/cache" "$overlay/cache/$1"
	create "$overlay/cache/$1/upper" "$overlay/cache/$1/work" "$overlay/cache/$1/lower"
	shift
done
' pisafe-layout "$@"
`

	// writeProfileFileScript replaces one of the files pisafe keeps beside the
	// profile. It arrives by rename, so a reader either sees the whole of it or
	// the whole of what it replaced, and a run starting mid-install is never
	// configured from half a record.
	writeProfileFileScript = `set -eu
exec podman unshare sh -ceu '
set -eu
record=$1/$2
staging=$1/.$2
cat >"$staging"
chown 1000:1000 "$staging"
chmod 0600 "$staging"
mv -T "$staging" "$record"
' pisafe-profile "$@"
`

	// installPackageScript takes the tree one throwaway container built and
	// makes it the profile's. Only this script writes the profile, the same way
	// only pisafe writes a project's cache: the container that fetched the
	// package never had a handle on anything but its own memory.
	//
	// Replacing an installed extension swaps the new tree in before the old one
	// goes, so a run that starts during an install finds one or the other rather
	// than nothing.
	installPackageScript = `set -euo pipefail
target="$1"
staging="$(dirname "$1")/.install-$(basename "$1")"
shift
podman unshare rm -rf -- "$staging" "$staging.replaced"
podman unshare install -d -m 0700 -o 1000 -g 1000 "$staging"
trap 'podman unshare rm -rf -- "$staging" "$staging.replaced" || true' EXIT
podman "$@" | podman unshare tar --numeric-owner -C "$staging" -xf -
if podman unshare test -d "$target"; then
	podman unshare mv -T "$target" "$staging.replaced"
fi
podman unshare mv -T "$staging" "$target"
`

	// removePackageScript takes one extension's tree out of the profile. The
	// record is written first, so what this removes is already something no run
	// starting now would load.
	removePackageScript = `set -eu
exec podman unshare rm -rf -- "$1"
`

	// toolBinariesScript reports the commands one installed tree claims. npm
	// writes a link per binary of every package in the tree, dependencies
	// included, and only the ones pointing into the package the user asked for
	// are the tool's to claim; the rest belong to packages that were pulled in
	// and were never named. Reading the tree rather than the registry's metadata
	// means what is reported is what was actually installed.
	toolBinariesScript = `set -eu
exec podman unshare sh -ceu '
set -eu
links=$1/node_modules/.bin
test -d "$links" || exit 0
for path in "$links"/*; do
	test -h "$path" || continue
	case "$(readlink "$path")" in
	"../$2/"*) printf %s\\n "${path##*/}" ;;
	esac
done
' pisafe-tool-binaries "$@"
`

	// linkToolBinariesScript rebuilds the one directory a run searches for
	// installed commands. It is built whole from what the record names and then
	// swapped in, so a name no tool claims any more cannot survive, and a run
	// that starts mid-install finds the directory as it was or as it will be.
	// Every link is relative and is checked against the tree it names, so a
	// record that has drifted from the profile fails here rather than leaving a
	// run with a command that resolves to nothing.
	linkToolBinariesScript = `set -eu
exec podman unshare sh -ceu '
set -eu
root=$1
shift
staging=$root/.bin
replaced=$root/.bin-replaced
rm -rf -- "$staging" "$replaced"
install -d -m 0700 -o 1000 -g 1000 "$staging"
trap "rm -rf -- \"$staging\" \"$replaced\"" EXIT
while [ "$#" -gt 0 ]; do
	test -x "$root/${2#../}"
	ln -s "$2" "$staging/$1"
	chown -h 1000:1000 "$staging/$1"
	shift 2
done
if [ -d "$root/bin" ]; then
	mv -T "$root/bin" "$replaced"
fi
mv -T "$staging" "$root/bin"
' pisafe-tool-links "$@"
`

	// readProfileFileScript prints one of the files pisafe keeps beside the
	// profile. An absent file is an empty one: a user who has installed nothing
	// and never checked for an update still has a valid profile, and every run
	// mounts it.
	readProfileFileScript = `set -eu
exec podman unshare sh -ceu '
set -eu
record=$1/$2
test -f "$record" || exit 0
exec cat "$record"
' pisafe-profile "$@"
`

	// publishSnapshotScript materializes one cache's merged view into a new
	// generation. The container reads; only this script writes, so nothing
	// inside a run ever holds the project store open for writing. The staging
	// directory is a dot entry, which is what keeps a half-written generation
	// out of every listing that selects or evicts one.
	publishSnapshotScript = `set -euo pipefail
namespace="/var/lib/pisafe/projects/$1/cache/$2"
key="$3"
upper="/var/lib/pisafe/runs/$4/overlay/cache/$2/upper"
staging="$namespace/.publish-$4"
shift 4

# A generation is never rewritten, and a run that added nothing must not lay an
# empty generation over a namespace that has usable ones in it.
if podman unshare test -d "$namespace/$key"; then
	exit 0
fi
if [ -z "$(podman unshare find "$upper" -mindepth 1 -maxdepth 1 -print -quit)" ]; then
	exit 0
fi

podman unshare rm -rf -- "$staging"
podman unshare install -d -m 0700 -o 1000 -g 1000 "$namespace" "$staging"
trap 'podman unshare rm -rf -- "$staging" || true' EXIT
podman "$@" | podman unshare tar --numeric-owner -C "$staging" -xf -
# tar restores the merged view's own timestamp onto the staging directory, and
# recency is how a namespace is searched, so the generation is stamped with
# when it was published rather than when the run last touched the directory.
podman unshare touch "$staging"
podman unshare mv -T "$staging" "$namespace/$key" ||
	podman unshare test -d "$namespace/$key"
`

	// evictSnapshotsScript drops the generations past the newest few. A
	// generation a recorded run may still stack an overlay on is kept whatever
	// its age: overlayfs leaves behaviour undefined when a mounted lower goes
	// away, and a stopped run remounts its own lower when it resumes.
	evictSnapshotsScript = `set -eu
exec podman unshare sh -ceu '
set -eu
namespace=/var/lib/pisafe/projects/$1/cache/$2
keep=$3
shift 3
test -d "$namespace" || exit 0
index=0
for entry in $(ls -1t "$namespace"); do
	index=$((index + 1))
	if [ "$index" -le "$keep" ]; then
		continue
	fi
	for held in "$@"; do
		if [ "$entry" = "$held" ]; then
			continue 2
		fi
	done
	rm -rf -- "$namespace/$entry"
done
' pisafe-evict "$@"
`

	// resetProjectCacheScript empties every cache namespace of one project
	// without touching the namespace roots the privileged helper owns, and
	// without touching the session store, which is not disposable.
	resetProjectCacheScript = `set -eu
exec podman unshare sh -ceu '
set -eu
cache=/var/lib/pisafe/projects/$1/cache
test -d "$cache" || exit 0
find "$cache" -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +
' pisafe-reset "$@"
`

	// promoteSessionsScript adds the transcripts one run wrote to the project
	// session store. A transcript's name carries a UUID, so two runs never choose
	// the same one and promotion needs no merge or key. Promotion only ever adds:
	// the store is a lower that other runs have mounted, and a name it already
	// holds is a transcript an earlier run promoted and this one rewrote when it
	// was migrated on load. Each file arrives by rename from the same
	// filesystem, so no reader ever sees a partial transcript.
	promoteSessionsScript = `set -eu
exec podman unshare sh -ceu '
set -eu
store=/var/lib/pisafe/projects/$1/sessions
upper=/var/lib/pisafe/runs/$2/overlay/sessions/upper
staging=$store/.promote-$2
test -d "$store"
test -d "$upper" || exit 0
trap "rm -f -- \"$staging\"" EXIT
for path in "$upper"/*.jsonl; do
	# Only a regular file is a transcript. A transcript the run deleted is a
	# whiteout device under the same name, and a deletion is not something an
	# additive promotion carries; anything else so named is a run trying to make
	# promotion fail on it.
	test -f "$path" || continue
	name=${path##*/}
	if [ -e "$store/$name" ]; then
		continue
	fi
	install -m 0600 -o 1000 -g 1000 "$path" "$staging"
	mv -T "$staging" "$store/$name"
done
' pisafe-promote "$@"
`
)

// maxResolveBytes bounds what npm may report about one package. Its report
// lists the tarball's files, so this is far above what any package produces
// and far below what would make the controller allocate without bound.
const maxResolveBytes = 1 << 22

// maxApplyArtifactBytes stops a run from filling the Mac's disk through the
// apply path. Run storage is smaller than this, so no bundle a run can
// legitimately produce reaches it.
const maxApplyArtifactBytes = int64(8 << 30)

// submoduleArtifactPattern bounds the stage file names a submodule may
// contribute, so an artifact name can never become a path.
var submoduleArtifactPattern = regexp.MustCompile(`^submodule-[0-9]{1,4}\.(bundle|patch)$`)

// applyArtifactPattern bounds the file names an apply package may hand back,
// so the run cannot steer the fetch outside its own package directory.
var applyArtifactPattern = regexp.MustCompile(`^apply(-submodule-[0-9]{1,4})?\.bundle$`)

var sha256Pattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// Transport executes commands and streams artifacts through Lima's control
// SSH connection. It does not use a host mount, guest agent, or Podman socket.
type Transport struct {
	instance string
	runner   Runner
}

func NewTransport() Transport {
	return Transport{
		instance: InstanceName,
		runner:   execRunner{binary: "limactl"},
	}
}

func (transport Transport) SSHGateway(ctx context.Context) (runssh.Gateway, error) {
	output, err := transport.runner.Run(
		ctx,
		nil,
		"list",
		"--format", "{{.SSHConfigFile}}",
		transport.instance,
	)
	if err != nil {
		return runssh.Gateway{}, fmt.Errorf("locate Lima SSH config: %w", err)
	}
	configFile := strings.TrimSpace(string(output))
	if !filepath.IsAbs(configFile) || strings.Contains(configFile, "\n") {
		return runssh.Gateway{}, fmt.Errorf("Lima returned an invalid SSH config path")
	}
	info, err := os.Lstat(configFile)
	if err != nil {
		return runssh.Gateway{}, fmt.Errorf("inspect Lima SSH config: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return runssh.Gateway{}, fmt.Errorf("Lima SSH config is not a regular file")
	}
	return runssh.Gateway{
		ConfigFile: configFile,
		Alias:      "lima-" + transport.instance,
	}, nil
}

// Execute runs one argv-style command in the dedicated VM.
func (transport Transport) Execute(
	ctx context.Context,
	stdin io.Reader,
	args ...string,
) ([]byte, error) {
	command := make([]string, 0, len(args)+2)
	command = append(command, "shell", transport.instance)
	command = append(command, args...)
	output, err := transport.runner.Run(ctx, stdin, command...)
	if err != nil {
		return nil, fmt.Errorf("execute in Lima VM: %w", err)
	}
	return output, nil
}

// StreamExecute runs one argv-style command in the dedicated VM, writing its
// standard output as it arrives so an artifact leaving a run is never held in
// memory as a whole.
func (transport Transport) StreamExecute(
	ctx context.Context,
	stdout io.Writer,
	args ...string,
) error {
	command := make([]string, 0, len(args)+2)
	command = append(command, "shell", transport.instance)
	command = append(command, args...)
	if err := transport.runner.Stream(ctx, stdout, command...); err != nil {
		return fmt.Errorf("stream from Lima VM: %w", err)
	}
	return nil
}

// CreateStage allocates a new, private VM-side run directory and streams the
// Git bundle, tracked patch, and a sanitized snapshot into it. Each upload is
// size- and SHA-256-verified before an atomic rename.
func (transport Transport) CreateStage(
	ctx context.Context,
	prepared gitstage.PreparedStage,
) (string, error) {
	runID := prepared.Snapshot.RunID
	if err := runid.Validate(runID); err != nil {
		return "", err
	}
	output, err := transport.shellScript(ctx, nil, createStageScript, runID)
	if err != nil {
		return "", fmt.Errorf("create remote stage: %w", err)
	}
	stagePath := strings.TrimSpace(string(output))
	if !strings.HasPrefix(stagePath, "/") ||
		!strings.HasSuffix(stagePath, "/"+remoteRunRoot+"/"+runID+"/stage") {
		return "", fmt.Errorf("remote returned an invalid stage path")
	}

	snapshot := prepared.Snapshot
	// The guest never needs the Mac path. Avoid disclosing it to code that can
	// read the staged package.
	snapshot.SourceRoot = ""
	snapshotJSON, err := json.Marshal(snapshot)
	if err != nil {
		return "", fmt.Errorf("encode stage snapshot: %w", err)
	}
	snapshotJSON = append(snapshotJSON, '\n')

	type stageArtifact struct {
		name string
		path string
		data []byte
	}
	artifacts := []stageArtifact{
		{name: "source.bundle", path: prepared.BundlePath},
		{name: "tracked.patch", path: prepared.PatchPath},
		{name: "snapshot.json", data: snapshotJSON},
	}
	if prepared.InputsPath != "" {
		artifacts = append(artifacts, stageArtifact{
			name: "inputs.tar",
			path: prepared.InputsPath,
		})
	}
	for index, submodule := range prepared.Submodules {
		artifacts = append(
			artifacts,
			stageArtifact{
				name: fmt.Sprintf("submodule-%d.bundle", index),
				path: submodule.BundlePath,
			},
			stageArtifact{
				name: fmt.Sprintf("submodule-%d.patch", index),
				path: submodule.PatchPath,
			},
		)
	}
	for _, artifact := range artifacts {
		if err := transport.uploadArtifact(ctx, runID, artifact.name, artifact.path, artifact.data); err != nil {
			return "", fmt.Errorf("upload %s: %w", artifact.name, err)
		}
	}
	return stagePath, nil
}

// RemoveRun removes one exact VM-side run directory. Callers are responsible
// for enforcing lifecycle confirmation before using it for user-facing
// cleanup.
func (transport Transport) RemoveRun(ctx context.Context, runID string) error {
	if err := runid.Validate(runID); err != nil {
		return err
	}
	if _, err := transport.shellScript(ctx, nil, removeRunScript, runID); err != nil {
		return fmt.Errorf("remove remote run: %w", err)
	}
	return nil
}

// CreateRunStorage allocates one root-owned, fixed-capacity filesystem. The
// privileged helper accepts only validated identifiers, two fixed scopes, and
// a fixed size policy per scope.
func (transport Transport) CreateRunStorage(ctx context.Context, runID string) error {
	return transport.storage(ctx, "create", "run", runID)
}

func (transport Transport) VerifyRunStorage(ctx context.Context, runID string) error {
	return transport.storage(ctx, "verify", "run", runID)
}

func (transport Transport) RemoveRunStorage(ctx context.Context, runID string) error {
	return transport.storage(ctx, "remove", "run", runID)
}

// EnsureProjectStorage allocates the filesystem holding one project's shared
// layers, or verifies the one already there. A project outlives every run of
// it, so no run may assume it is creating it.
func (transport Transport) EnsureProjectStorage(ctx context.Context, projectKey string) error {
	return transport.storage(ctx, "ensure", "project", projectKey)
}

// RemoveProjectStorage releases a project's whole filesystem: its shared cache,
// its session store, and the image behind them. Nothing here decides that a
// project is finished with; a store is unmounted and dropped only once the
// checkout it is keyed by has been gone for a whole retention window.
func (transport Transport) RemoveProjectStorage(ctx context.Context, projectKey string) error {
	return transport.storage(ctx, "remove", "project", projectKey)
}

// SelectCacheSnapshots names the generation each declared cache starts from.
// Falling back to the newest generation in a namespace is what keeps a changed
// lockfile from costing a cold fetch: the tool restores the previous
// generation and fetches only the delta.
func (transport Transport) SelectCacheSnapshots(
	ctx context.Context,
	projectKey string,
	caches []runcontainer.CacheMount,
) ([]runcontainer.CacheMount, error) {
	if len(caches) == 0 {
		return nil, nil
	}
	if err := runid.Validate(projectKey); err != nil {
		return nil, err
	}
	arguments := []string{projectKey}
	for _, cache := range caches {
		if err := cache.Validate(); err != nil {
			return nil, err
		}
		arguments = append(arguments, cache.Name, cache.Key)
	}
	output, err := transport.shellScript(ctx, nil, selectSnapshotsScript, arguments...)
	if err != nil {
		return nil, fmt.Errorf("select project cache snapshots: %w", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(output), "\n"), "\n")
	if len(lines) != len(caches) {
		return nil, fmt.Errorf(
			"project cache selection returned %d snapshots for %d caches",
			len(lines),
			len(caches),
		)
	}
	selected := make([]runcontainer.CacheMount, 0, len(caches))
	for index, cache := range caches {
		cache.Snapshot = strings.TrimSpace(lines[index])
		// The name came from a directory listing rather than from pisafe, so it
		// is checked before it becomes half of a mount argument.
		if err := cache.Validate(); err != nil {
			return nil, err
		}
		selected = append(selected, cache)
	}
	return selected, nil
}

// PrepareRunLayout creates the directories the run's mounts need: the upper,
// work, and cold-start lower of every overlay, and the mountpoint the profile
// binds onto. They sit inside directories the privileged helper already owns to
// the mapped UID, so the set of cache names — which comes from the repository —
// never reaches the helper.
func (transport Transport) PrepareRunLayout(
	ctx context.Context,
	runID string,
	caches []runcontainer.CacheMount,
) error {
	if err := runid.Validate(runID); err != nil {
		return err
	}
	arguments := []string{runID}
	for _, cache := range caches {
		if err := cache.Validate(); err != nil {
			return err
		}
		arguments = append(arguments, cache.Name)
	}
	if _, err := transport.shellScript(ctx, nil, prepareRunLayoutScript, arguments...); err != nil {
		return fmt.Errorf("prepare run directories: %w", err)
	}
	return nil
}

// EnsureGlobalStorage allocates the filesystem holding the profile every run
// mounts, or verifies the one already there. Like a project, it outlives every
// run, so no run may assume it is creating it.
func (transport Transport) EnsureGlobalStorage(ctx context.Context) error {
	return transport.storage(ctx, "ensure", "global", runcontainer.ProfileName)
}

// ReadProfileRecord reports what the profile has installed. It is read at every
// run start, because a run's copy of what the profile says does not survive the
// run and the profile may have changed since the last one.
func (transport Transport) ReadProfileRecord(ctx context.Context) (profile.Record, error) {
	output, err := transport.readProfileFile(ctx, profile.RecordFile)
	if err != nil {
		return profile.Record{}, fmt.Errorf("read profile record: %w", err)
	}
	return profile.ParseRecord(output)
}

// ReadProfileOffers reports what npm last said the installed extensions
// resolve to.
func (transport Transport) ReadProfileOffers(ctx context.Context) (profile.Offers, error) {
	output, err := transport.readProfileFile(ctx, profile.OffersFile)
	if err != nil {
		return profile.Offers{}, fmt.Errorf("read profile offers: %w", err)
	}
	return profile.ParseOffers(output), nil
}

// ReadProfileTools reports what commands the profile has installed. It is what
// the directory a run searches is rebuilt from, so it is the authority on that
// directory's contents rather than a description of them.
func (transport Transport) ReadProfileTools(ctx context.Context) (profile.Tools, error) {
	output, err := transport.readProfileFile(ctx, profile.ToolsFile)
	if err != nil {
		return profile.Tools{}, fmt.Errorf("read profile tool record: %w", err)
	}
	return profile.ParseTools(output)
}

func (transport Transport) readProfileFile(ctx context.Context, name string) ([]byte, error) {
	return transport.shellScript(
		ctx, nil, readProfileFileScript, runcontainer.ProfilePinsPath(), name,
	)
}

// PublishCacheSnapshot keeps what one run added to a declared cache as a new
// immutable generation. It does nothing when the run's key already names a
// generation or when the run added nothing, so a namespace never gains a
// duplicate or an empty entry that a later run would restore in preference to
// a usable one.
func (transport Transport) PublishCacheSnapshot(
	ctx context.Context,
	spec runcontainer.Spec,
	cache runcontainer.CacheMount,
) error {
	publishArgs, err := spec.PublishArgs(cache)
	if err != nil {
		return err
	}
	arguments := append(
		[]string{"pisafe-publish", spec.ProjectKey, cache.Name, cache.Key, spec.RunID},
		publishArgs...,
	)
	if _, err := transport.Execute(
		ctx,
		nil,
		append([]string{"bash", "-ceu", publishSnapshotScript}, arguments...)...,
	); err != nil {
		return fmt.Errorf("publish cache %q: %w", cache.Name, err)
	}
	return nil
}

// EvictCacheSnapshots trims one namespace to the newest keep generations, plus
// every generation held names, which are the ones a recorded run may still
// mount.
func (transport Transport) EvictCacheSnapshots(
	ctx context.Context,
	projectKey string,
	name string,
	keep int,
	held []string,
) error {
	if err := runid.Validate(projectKey); err != nil {
		return err
	}
	if err := runid.Validate(name); err != nil {
		return fmt.Errorf("invalid cache name %q", name)
	}
	if keep < 1 {
		return fmt.Errorf("a cache namespace must keep at least one generation")
	}
	arguments := []string{projectKey, name, strconv.Itoa(keep)}
	for _, snapshot := range held {
		if err := runcontainer.ValidateCacheGeneration(snapshot); err != nil {
			return err
		}
		arguments = append(arguments, snapshot)
	}
	if _, err := transport.shellScript(ctx, nil, evictSnapshotsScript, arguments...); err != nil {
		return fmt.Errorf("evict generations of cache %q: %w", name, err)
	}
	return nil
}

// ResetProjectCache empties one project's shared cache. Nothing a run needs
// for correctness lives there, so throwing all of it away is always allowed.
func (transport Transport) ResetProjectCache(ctx context.Context, projectKey string) error {
	if err := runid.Validate(projectKey); err != nil {
		return err
	}
	if _, err := transport.shellScript(ctx, nil, resetProjectCacheScript, projectKey); err != nil {
		return fmt.Errorf("reset project cache: %w", err)
	}
	return nil
}

// PromoteSessions folds the transcripts one run wrote into the project session
// store, where the project's later runs can read them. Sessions are not a
// cache: they have no key, nothing is implied by a transcript's absence, and
// nothing already promoted is ever replaced or evicted.
func (transport Transport) PromoteSessions(
	ctx context.Context,
	projectKey string,
	runID string,
) error {
	if err := runid.Validate(projectKey); err != nil {
		return err
	}
	if err := runid.Validate(runID); err != nil {
		return err
	}
	if _, err := transport.shellScript(
		ctx, nil, promoteSessionsScript, projectKey, runID,
	); err != nil {
		return fmt.Errorf("promote run sessions: %w", err)
	}
	return nil
}

// WriteProfileRecord replaces pisafe's record of what the profile holds. It is
// what a run is configured from, so it is written after the tree it names and
// before the tree it stops naming: a record can be ahead of nothing.
func (transport Transport) WriteProfileRecord(
	ctx context.Context,
	record profile.Record,
) error {
	content, err := record.Encode()
	if err != nil {
		return err
	}
	if err := transport.writeProfileFile(ctx, profile.RecordFile, content); err != nil {
		return fmt.Errorf("write profile record: %w", err)
	}
	return nil
}

// WriteProfileOffers keeps what a check learned, so the offer can be repeated
// to the user without asking npm again.
func (transport Transport) WriteProfileOffers(
	ctx context.Context,
	offers profile.Offers,
) error {
	content, err := offers.Encode()
	if err != nil {
		return err
	}
	if err := transport.writeProfileFile(ctx, profile.OffersFile, content); err != nil {
		return fmt.Errorf("write profile offers: %w", err)
	}
	return nil
}

// WriteProfileTools replaces pisafe's record of the installed commands. The
// directory a run searches is rebuilt from it first, so the record is never
// ahead of what a run would find.
func (transport Transport) WriteProfileTools(
	ctx context.Context,
	tools profile.Tools,
) error {
	content, err := tools.Encode()
	if err != nil {
		return err
	}
	if err := transport.writeProfileFile(ctx, profile.ToolsFile, content); err != nil {
		return fmt.Errorf("write profile tool record: %w", err)
	}
	return nil
}

func (transport Transport) writeProfileFile(
	ctx context.Context,
	name string,
	content []byte,
) error {
	_, err := transport.shellScript(
		ctx,
		bytes.NewReader(content),
		writeProfileFileScript,
		runcontainer.ProfilePinsPath(),
		name,
	)
	return err
}

// ResolvePackage asks npm what a package spec resolves to right now. The
// exact version and the integrity of that release are the pin, and they come
// from the registry's own answer rather than from anything pisafe composes.
func (transport Transport) ResolvePackage(
	ctx context.Context,
	imageID string,
	packageSpec string,
) (profile.Pin, error) {
	args, err := runcontainer.PackageResolveArgs(imageID, packageSpec)
	if err != nil {
		return profile.Pin{}, err
	}
	output, err := transport.Execute(ctx, nil, append([]string{"podman"}, args...)...)
	if err != nil {
		return profile.Pin{}, fmt.Errorf("resolve %s: %w", packageSpec, err)
	}
	return parseResolvedExtension(output)
}

// ResolveExtensionUpdates asks npm what each installed extension's name
// resolves to now. It installs nothing and touches the profile not at all: the
// answer is only what an offer is made from, and applying one resolves again.
// A package that cannot be resolved is left out rather than failing the check,
// because one unpublished or renamed package should not hide the others.
func (transport Transport) ResolveExtensionUpdates(
	ctx context.Context,
	imageID string,
	record profile.Record,
	checkedAt time.Time,
) (profile.Offers, error) {
	latest := make([]profile.Offer, 0, len(record.Extensions))
	var failures []error
	for _, extension := range record.Extensions {
		resolved, err := transport.ResolvePackage(ctx, imageID, extension.Name)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		latest = append(latest, profile.Offer{
			Name:    resolved.Name,
			Version: resolved.Version,
		})
	}
	return profile.NewOffers(checkedAt, latest), errors.Join(failures...)
}

// parseResolvedExtension reads npm's report. It arrives from a container with
// network access, so nothing in it is taken on trust: the pin is what pisafe
// can address, name, and put in a path, or the install does not happen.
func parseResolvedExtension(output []byte) (profile.Pin, error) {
	if len(output) > maxResolveBytes {
		return profile.Pin{}, errors.New("package resolution exceeds size limit")
	}
	var reported []struct {
		Name      string `json:"name"`
		Version   string `json:"version"`
		Integrity string `json:"integrity"`
	}
	if err := json.Unmarshal(output, &reported); err != nil {
		return profile.Pin{}, fmt.Errorf("decode package resolution: %w", err)
	}
	if len(reported) != 1 {
		return profile.Pin{}, fmt.Errorf(
			"expected one resolved package, got %d",
			len(reported),
		)
	}
	extension := profile.Pin{
		Name:      reported[0].Name,
		Version:   reported[0].Version,
		Integrity: reported[0].Integrity,
	}
	directory, err := runid.NewPackageDirectory(extension.Name)
	if err != nil {
		return profile.Pin{}, err
	}
	extension.Directory = directory
	if err := extension.Validate(); err != nil {
		return profile.Pin{}, err
	}
	return extension, nil
}

// InstallPackage puts one resolved package in the profile, under the module
// root the caller names. The container fetches and builds the tree; this script
// is what makes it the profile's, so nothing that ran with the network open
// ever held the profile open for writing.
func (transport Transport) InstallPackage(
	ctx context.Context,
	imageID string,
	root string,
	pin profile.Pin,
) error {
	if err := pin.Validate(); err != nil {
		return err
	}
	installArgs, err := runcontainer.PackageInstallArgs(
		imageID,
		pin.Name,
		pin.Version,
		pin.Integrity,
	)
	if err != nil {
		return err
	}
	arguments := append([]string{"pisafe-package", root}, installArgs...)
	if _, err := transport.Execute(
		ctx,
		nil,
		append([]string{"bash", "-ceu", installPackageScript}, arguments...)...,
	); err != nil {
		return fmt.Errorf("install %s@%s: %w", pin.Name, pin.Version, err)
	}
	return nil
}

// RemovePackage takes one module root out of the profile.
func (transport Transport) RemovePackage(ctx context.Context, root string) error {
	if _, err := transport.shellScript(ctx, nil, removePackageScript, root); err != nil {
		return fmt.Errorf("remove %s: %w", root, err)
	}
	return nil
}

// An extension and a tool are installed the same way and differ only in where
// they land and what a run then does with them, so which namespace a package
// belongs in is decided here rather than by whoever asked for it. A pin that
// does not describe itself consistently never becomes half of a path, whichever
// namespace it was headed for.

func (transport Transport) InstallExtension(
	ctx context.Context,
	imageID string,
	extension profile.Pin,
) error {
	if err := extension.Validate(); err != nil {
		return err
	}
	return transport.InstallPackage(
		ctx, imageID, runcontainer.ExtensionInstallRoot(extension.Directory), extension,
	)
}

func (transport Transport) RemoveExtension(
	ctx context.Context,
	extension profile.Pin,
) error {
	if err := extension.Validate(); err != nil {
		return err
	}
	return transport.RemovePackage(
		ctx, runcontainer.ExtensionInstallRoot(extension.Directory),
	)
}

func (transport Transport) InstallTool(
	ctx context.Context,
	imageID string,
	tool profile.Pin,
) error {
	if err := tool.Validate(); err != nil {
		return err
	}
	return transport.InstallPackage(
		ctx, imageID, runcontainer.ToolInstallRoot(tool.Directory), tool,
	)
}

func (transport Transport) RemoveTool(ctx context.Context, tool profile.Pin) error {
	if err := tool.Validate(); err != nil {
		return err
	}
	return transport.RemovePackage(ctx, runcontainer.ToolInstallRoot(tool.Directory))
}

// ToolBinaries reports the commands one installed tool claims. Which names a
// package installs is the package's own choice, so they are read back from the
// tree rather than asked for, and every one of them is held to what pisafe can
// put in a path and print.
func (transport Transport) ToolBinaries(
	ctx context.Context,
	tool profile.Pin,
) ([]string, error) {
	if err := tool.Validate(); err != nil {
		return nil, err
	}
	output, err := transport.shellScript(
		ctx,
		nil,
		toolBinariesScript,
		runcontainer.ToolInstallRoot(tool.Directory),
		tool.Name,
	)
	if err != nil {
		return nil, fmt.Errorf("read the commands %s provides: %w", tool.Name, err)
	}
	var binaries []string
	for _, binary := range strings.Fields(string(output)) {
		if err := profile.ValidateBinaryName(binary); err != nil {
			return nil, fmt.Errorf("%s provides %w", tool.Name, err)
		}
		binaries = append(binaries, binary)
	}
	slices.Sort(binaries)
	return binaries, nil
}

// LinkToolBinaries rebuilds the directory a run searches so that it holds
// exactly what the record names.
func (transport Transport) LinkToolBinaries(
	ctx context.Context,
	tools profile.Tools,
) error {
	arguments := []string{runcontainer.ToolsMount().Source}
	for _, link := range tools.Links() {
		if err := profile.ValidateBinaryName(link.Binary); err != nil {
			return err
		}
		arguments = append(
			arguments,
			link.Binary,
			runcontainer.ToolBinaryTarget(link.Directory, link.Binary),
		)
	}
	if _, err := transport.shellScript(ctx, nil, linkToolBinariesScript, arguments...); err != nil {
		return fmt.Errorf("link the profile's commands: %w", err)
	}
	return nil
}

func (transport Transport) storage(ctx context.Context, action, scope, id string) error {
	if err := runid.Validate(id); err != nil {
		return err
	}
	if _, err := transport.Execute(
		ctx,
		nil,
		"sudo", "/usr/local/sbin/pisafe-storage", action, scope, id,
	); err != nil {
		return fmt.Errorf("%s quota-limited %s storage: %w", action, scope, err)
	}
	return nil
}

// ImportStage copies the VM-side stage into the run's quota-limited workspace.
// The source directory is never mounted into the container or onto the Mac.
func (transport Transport) ImportStage(
	ctx context.Context,
	runID string,
) error {
	if err := runid.Validate(runID); err != nil {
		return err
	}
	if _, err := transport.Execute(
		ctx,
		nil,
		"bash", "-ceu", importStageScript,
		"pisafe-import", runID,
	); err != nil {
		return fmt.Errorf("import stage into run workspace: %w", err)
	}
	return nil
}

// FetchApplyArtifact streams one bundle out of the run's private storage into
// destination, which must not already exist. The transfer is kept only if the
// received bytes hash to what the run declared.
func (transport Transport) FetchApplyArtifact(
	ctx context.Context,
	runID string,
	artifact gitstage.ApplyArtifact,
	destination string,
) error {
	if err := runid.Validate(runID); err != nil {
		return err
	}
	if !applyArtifactPattern.MatchString(artifact.Name) {
		return fmt.Errorf("unsupported apply artifact %q", artifact.Name)
	}
	if !sha256Pattern.MatchString(artifact.SHA256) {
		return fmt.Errorf("apply artifact %q was announced without a hash", artifact.Name)
	}
	file, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create apply artifact: %w", err)
	}
	complete := false
	defer func() {
		file.Close()
		if !complete {
			os.Remove(destination)
		}
	}()

	digest := sha256.New()
	bounded := &boundedWriter{
		writer:    io.MultiWriter(file, digest),
		remaining: maxApplyArtifactBytes,
	}
	if err := transport.streamScript(
		ctx,
		bounded,
		fetchApplyScript,
		runID,
		artifact.Name,
	); err != nil {
		return fmt.Errorf("fetch %s: %w", artifact.Name, err)
	}
	if hex.EncodeToString(digest.Sum(nil)) != artifact.SHA256 {
		return fmt.Errorf("apply artifact %q changed in transfer", artifact.Name)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync apply artifact: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close apply artifact: %w", err)
	}
	complete = true
	return nil
}

// RemoveApplyPackage deletes the bundles a run produced. The run rebuilds them
// from its workspace whenever apply runs again.
func (transport Transport) RemoveApplyPackage(ctx context.Context, runID string) error {
	if err := runid.Validate(runID); err != nil {
		return err
	}
	if _, err := transport.shellScript(ctx, nil, removeApplyScript, runID); err != nil {
		return fmt.Errorf("remove apply package: %w", err)
	}
	return nil
}

// boundedWriter fails a transfer that outgrows its limit instead of letting a
// run fill the Mac's disk.
type boundedWriter struct {
	writer    io.Writer
	remaining int64
}

func (bounded *boundedWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > bounded.remaining {
		return 0, fmt.Errorf("artifact exceeds %d bytes", maxApplyArtifactBytes)
	}
	bounded.remaining -= int64(len(data))
	return bounded.writer.Write(data)
}

func (transport Transport) uploadArtifact(
	ctx context.Context,
	runID string,
	name string,
	path string,
	data []byte,
) error {
	switch name {
	case "source.bundle", "tracked.patch", "snapshot.json", "inputs.tar":
	default:
		if !submoduleArtifactPattern.MatchString(name) {
			return fmt.Errorf("unsupported stage artifact %q", name)
		}
	}

	var (
		reader io.Reader
		size   int64
		hash   string
		file   *os.File
	)
	if path != "" {
		var err error
		file, err = os.Open(filepath.Clean(path))
		if err != nil {
			return fmt.Errorf("open artifact: %w", err)
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			return fmt.Errorf("inspect artifact: %w", err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("artifact is not a regular file")
		}
		digest := sha256.New()
		if _, err := io.Copy(digest, file); err != nil {
			return fmt.Errorf("hash artifact: %w", err)
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("rewind artifact: %w", err)
		}
		reader = file
		size = info.Size()
		hash = hex.EncodeToString(digest.Sum(nil))
	} else {
		digest := sha256.Sum256(data)
		reader = bytes.NewReader(data)
		size = int64(len(data))
		hash = hex.EncodeToString(digest[:])
	}

	if _, err := transport.shellScript(
		ctx,
		reader,
		uploadStageScript,
		runID,
		name,
		strconv.FormatInt(size, 10),
		hash,
	); err != nil {
		return err
	}
	return nil
}

func (transport Transport) shellScript(
	ctx context.Context,
	stdin io.Reader,
	script string,
	args ...string,
) ([]byte, error) {
	command := []string{"sh", "-ceu", script, "pisafe-remote"}
	command = append(command, args...)
	return transport.Execute(ctx, stdin, command...)
}

func (transport Transport) streamScript(
	ctx context.Context,
	stdout io.Writer,
	script string,
	args ...string,
) error {
	command := []string{
		"shell", transport.instance,
		"sh", "-ceu", script, "pisafe-remote",
	}
	command = append(command, args...)
	return transport.runner.Stream(ctx, stdout, command...)
}
