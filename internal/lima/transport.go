package lima

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/mpizenberg/pisafe/internal/gitstage"
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

	// prepareOverlaysScript builds the run-private half of every overlay. Both
	// the mode and the ownership are what the container needs to write through
	// its overlay, and are what the privileged helper would have applied had
	// the set of names been knowable when it allocated the filesystem.
	prepareOverlaysScript = `set -eu
exec podman unshare sh -ceu '
set -eu
overlay=/var/lib/pisafe/runs/$1/overlay
shift
test -d "$overlay"
create() {
	install -d -m 0700 -o 1000 -g 1000 "$@"
}
create "$overlay/sessions" "$overlay/sessions/upper" "$overlay/sessions/work"
while [ "$#" -gt 0 ]; do
	create "$overlay/cache" "$overlay/cache/$1"
	create "$overlay/cache/$1/upper" "$overlay/cache/$1/work" "$overlay/cache/$1/lower"
	shift
done
' pisafe-overlays "$@"
`
)

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

// PrepareRunOverlays creates the upper, work, and cold-start lower directories
// the run's overlays need. They sit inside directories the privileged helper
// already owns to the mapped UID, so the set of cache names — which comes from
// the repository — never reaches the helper.
func (transport Transport) PrepareRunOverlays(
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
	if _, err := transport.shellScript(ctx, nil, prepareOverlaysScript, arguments...); err != nil {
		return fmt.Errorf("prepare run overlay directories: %w", err)
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
