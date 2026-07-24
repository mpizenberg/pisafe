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
	"strconv"
	"strings"

	"github.com/mpizenberg/pisafe/internal/gitstage"
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
)

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

	artifacts := []struct {
		name string
		path string
		data []byte
	}{
		{name: "source.bundle", path: prepared.BundlePath},
		{name: "tracked.patch", path: prepared.PatchPath},
		{name: "snapshot.json", data: snapshotJSON},
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

// CreateStorage allocates one root-owned, fixed-capacity filesystem. The
// privileged helper accepts only validated run IDs and a fixed size policy.
func (transport Transport) CreateStorage(ctx context.Context, runID string) error {
	if err := runid.Validate(runID); err != nil {
		return err
	}
	if _, err := transport.Execute(
		ctx,
		nil,
		"sudo", "/usr/local/sbin/pisafe-run-storage", "create", runID,
	); err != nil {
		return fmt.Errorf("create quota-limited run storage: %w", err)
	}
	return nil
}

func (transport Transport) VerifyStorage(ctx context.Context, runID string) error {
	if err := runid.Validate(runID); err != nil {
		return err
	}
	if _, err := transport.Execute(
		ctx,
		nil,
		"sudo", "/usr/local/sbin/pisafe-run-storage", "verify", runID,
	); err != nil {
		return fmt.Errorf("verify quota-limited run storage: %w", err)
	}
	return nil
}

func (transport Transport) RemoveStorage(ctx context.Context, runID string) error {
	if err := runid.Validate(runID); err != nil {
		return err
	}
	if _, err := transport.Execute(
		ctx,
		nil,
		"sudo", "/usr/local/sbin/pisafe-run-storage", "remove", runID,
	); err != nil {
		return fmt.Errorf("remove quota-limited run storage: %w", err)
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

func (transport Transport) uploadArtifact(
	ctx context.Context,
	runID string,
	name string,
	path string,
	data []byte,
) error {
	if name != "source.bundle" && name != "tracked.patch" && name != "snapshot.json" {
		return fmt.Errorf("unsupported stage artifact %q", name)
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
