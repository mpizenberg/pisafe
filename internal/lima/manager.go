package lima

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

type Status string

const (
	StatusAbsent  Status = "absent"
	StatusRunning Status = "running"
	StatusStopped Status = "stopped"
	// StatusBroken is any state Lima will call neither running nor stopped. The
	// instance is there, so a rebuild has something to replace, but nothing may
	// be concluded about what is inside it.
	StatusBroken Status = "broken"
)

type Runner interface {
	Run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error)
	// Stream writes a command's standard output to the caller as it arrives,
	// so an artifact leaving the VM is never held in memory as a whole.
	Stream(ctx context.Context, stdout io.Writer, args ...string) error
}

// VM is the handle on the one Lima instance pisafe owns. It creates and
// destroys the instance, holds a running one to the boundary it was built with,
// and runs commands and streams artifacts inside it through Lima's control SSH
// connection — no host mount, no guest agent, and no Podman socket in any of it.
type VM struct {
	instance string
	runner   Runner
}

func New() VM {
	return VM{
		instance: InstanceName,
		runner:   execRunner{binary: "limactl"},
	}
}

func (vm VM) Status(ctx context.Context) (Status, error) {
	output, err := vm.runner.Run(ctx, nil, "list", "--format", "{{.Name}}\t{{.Status}}")
	if err != nil {
		return "", fmt.Errorf("list Lima instances: %w", err)
	}
	for _, line := range strings.Split(string(output), "\n") {
		name, status, found := strings.Cut(line, "\t")
		if !found || name != vm.instance {
			continue
		}
		switch strings.ToLower(status) {
		case "running":
			return StatusRunning, nil
		case "stopped":
			return StatusStopped, nil
		default:
			return StatusBroken, nil
		}
	}
	return StatusAbsent, nil
}

// create builds the instance from a configuration Lima has agreed to first, so
// a definition Lima rejects never becomes an instance a rebuild has to clear.
func (vm VM) create(ctx context.Context, configPath string) error {
	if _, err := vm.runner.Run(ctx, nil, "template", "validate", configPath); err != nil {
		return fmt.Errorf("validate Lima configuration: %w", err)
	}
	if _, err := vm.runner.Run(
		ctx,
		nil,
		"--tty=false",
		"create",
		"--name="+vm.instance,
		configPath,
	); err != nil {
		return fmt.Errorf("create Lima instance: %w", err)
	}
	return nil
}

// Ensure creates the dedicated VM when absent, then starts and verifies it
// against the current host-network boundary.
func (vm VM) Ensure(ctx context.Context, prefixes []netip.Prefix) error {
	status, err := vm.Status(ctx)
	if err != nil {
		return err
	}
	if status == StatusAbsent {
		if err := vm.provision(ctx, prefixes); err != nil {
			return err
		}
	}
	return vm.Start(ctx, prefixes)
}

// provision renders the VM definition and builds an instance from it. The
// configuration lives no longer than the call: what a running instance was
// built from is Lima's copy, and rendering it again is how a stale one is
// detected rather than quietly reconciled.
func (vm VM) provision(ctx context.Context, prefixes []netip.Prefix) error {
	config, err := RenderConfig(prefixes)
	if err != nil {
		return err
	}
	if err := vm.ensureStateDisk(ctx); err != nil {
		return err
	}
	temporary, err := os.MkdirTemp("", "pisafe-lima-config-*")
	if err != nil {
		return fmt.Errorf("create temporary Lima config directory: %w", err)
	}
	defer os.RemoveAll(temporary)
	configPath := filepath.Join(temporary, "pisafe.yaml")
	if err := WriteConfig(configPath, config); err != nil {
		return err
	}
	return vm.create(ctx, configPath)
}

// ensureStateDisk creates the disk that carries /var/lib/pisafe, unless Lima
// already holds one. The disk belongs to Lima rather than to the instance, so
// an instance that has to be recreated leaves every run, every project store,
// and the profile where they are, and the new instance mounts them back.
func (vm VM) ensureStateDisk(ctx context.Context) error {
	disk, err := vm.stateDisk(ctx)
	if err != nil {
		return err
	}
	if disk != nil {
		return nil
	}
	if _, err := vm.runner.Run(
		ctx,
		nil,
		"disk",
		"create",
		StateDiskName,
		"--size",
		StateDiskSize,
	); err != nil {
		return fmt.Errorf("create Lima state disk: %w", err)
	}
	return nil
}

// HasStateDisk reports whether Lima holds the disk that carries every run's
// filesystem, every project's transcripts, and the profile. An instance
// provisioned before the disk existed keeps all of it on its own disk instead,
// where deleting the instance takes it too.
func (vm VM) HasStateDisk(ctx context.Context) (bool, error) {
	disk, err := vm.stateDisk(ctx)
	if err != nil {
		return false, err
	}
	return disk != nil, nil
}

type limaDisk struct {
	Name string `json:"name"`
	// Instance names whichever instance holds the disk's lock. One that was
	// killed rather than shut down leaves its name here and the disk refused to
	// its replacement.
	Instance string `json:"instance"`
}

// stateDisk finds the disk carrying /var/lib/pisafe, or reports that Lima has
// none.
func (vm VM) stateDisk(ctx context.Context) (*limaDisk, error) {
	output, err := vm.runner.Run(ctx, nil, "disk", "list", "--json")
	if err != nil {
		return nil, fmt.Errorf("list Lima disks: %w", err)
	}
	for _, line := range strings.Split(string(output), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var disk limaDisk
		if err := json.Unmarshal([]byte(line), &disk); err != nil {
			return nil, fmt.Errorf("read Lima disk list: %w", err)
		}
		if disk.Name == StateDiskName {
			return &disk, nil
		}
	}
	return nil, nil
}

// Delete takes the instance away and leaves the state disk for the next one to
// mount. The VM is asked to shut down first because that disk carries every
// run's filesystem and an instance killed outright never flushes its writes to
// it. One too broken to shut down never flushes them either, so that one is
// killed rather than waited on — refusing to delete it would withhold the
// rebuild from the VM that most needs one — and the lock it leaves on the disk
// is released here.
func (vm VM) Delete(ctx context.Context) error {
	status, err := vm.Status(ctx)
	if err != nil {
		return err
	}
	if status == StatusAbsent {
		return nil
	}
	// A stopped instance has nothing to shut down. Anything else may, including
	// one Lima cannot classify, which is the case where only the kill gets
	// through.
	if status != StatusStopped {
		if _, err := vm.runner.Run(
			ctx,
			nil,
			"--tty=false",
			"stop",
			vm.instance,
		); err != nil {
			if _, err := vm.runner.Run(
				ctx,
				nil,
				"--tty=false",
				"stop",
				"--force",
				vm.instance,
			); err != nil {
				return fmt.Errorf("stop Lima instance: %w", err)
			}
		}
	}
	if _, err := vm.runner.Run(
		ctx,
		nil,
		"--tty=false",
		"delete",
		"--force",
		vm.instance,
	); err != nil {
		return fmt.Errorf("delete Lima instance: %w", err)
	}
	return vm.unlockStateDisk(ctx)
}

func (vm VM) unlockStateDisk(ctx context.Context) error {
	disk, err := vm.stateDisk(ctx)
	if err != nil {
		return err
	}
	if disk == nil || disk.Instance == "" {
		return nil
	}
	if _, err := vm.runner.Run(ctx, nil, "disk", "unlock", StateDiskName); err != nil {
		return fmt.Errorf("unlock Lima state disk: %w", err)
	}
	return nil
}

// Start starts (or reuses) the VM and verifies that its immutable host-network
// deny set and generated security profile still match the controller. Callers
// must not start run containers if this fails.
func (vm VM) Start(ctx context.Context, hostPrefixes []netip.Prefix) error {
	prefixes, err := CanonicalIPv4Prefixes(hostPrefixes)
	if err != nil {
		return err
	}
	if err := vm.bringUp(ctx); err != nil {
		return err
	}
	if err := vm.verifySecurityProfile(ctx, prefixes); err != nil {
		return err
	}
	if err := vm.SyncClock(ctx); err != nil {
		return err
	}
	return vm.verifyFirewall(ctx, prefixes)
}

// StartUnverified starts (or reuses) the VM without holding it to the boundary
// records, for a command that starts no run: one that reads or writes a run's
// own workspace through a container with no network, no home, and none of the
// shared profile, or one that only ends or removes what a run already holds.
// Neither record bears on what such a command reaches, and the cure for a VM
// that fails either is a rebuild, which ends every run that is working. Holding
// these commands to the records would make handing back a finished run's diff
// cost every other run's session.
func (vm VM) StartUnverified(ctx context.Context) error {
	if err := vm.bringUp(ctx); err != nil {
		return err
	}
	return vm.SyncClock(ctx)
}

// bringUp starts the VM unless it is already running. It proves nothing about
// the boundary: what a command must have proved before it reaches a run is its
// caller's to decide.
func (vm VM) bringUp(ctx context.Context) error {
	status, err := vm.Status(ctx)
	if err != nil {
		return err
	}
	switch status {
	case StatusAbsent:
		return fmt.Errorf("Lima instance %q has not been created", vm.instance)
	case StatusRunning:
		return nil
	case StatusStopped:
		if _, err := vm.runner.Run(
			ctx,
			nil,
			"--tty=false",
			"start",
			vm.instance,
		); err != nil {
			return fmt.Errorf("start Lima instance: %w", err)
		}
		return nil
	default:
		return fmt.Errorf(
			"Lima instance %q is in no state it can be started from; "+
				"replace it with pisafe vm rebuild",
			vm.instance,
		)
	}
}

// verifySecurityProfile detects an instance provisioned by an older or locally
// modified VM definition. The record is root-owned and immutable to the
// unprivileged Lima user after provisioning. The prefixes are already canonical:
// what the digest is taken over is decided once, by Start.
func (vm VM) verifySecurityProfile(ctx context.Context, prefixes []string) error {
	expected := securityProfileDigest(prefixes)
	output, err := vm.runner.Run(ctx, nil, vm.inVM([]string{
		"cat", "/etc/pisafe/security-profile",
	})...)
	if err != nil {
		return fmt.Errorf(
			"read VM security profile: %w; rebuild the VM with pisafe vm rebuild",
			err,
		)
	}
	if strings.TrimSpace(string(output)) != expected {
		return fmt.Errorf(
			"VM security profile is stale; rebuild the VM with pisafe vm rebuild",
		)
	}
	return nil
}

// SyncClock steps a clock that drifted while the plain-mode VM was suspended.
// Plain mode has no Lima guest agent to perform host/guest time correction.
func (vm VM) SyncClock(ctx context.Context) error {
	if _, err := vm.runner.Run(ctx, nil, vm.inVM([]string{
		"sudo", "/usr/local/sbin/pisafe-clock-step",
	})...); err != nil {
		return fmt.Errorf("synchronize VM clock: %w", err)
	}
	return nil
}

// verifyFirewall refuses to reuse a VM after the Mac's on-link networks change.
// The prefix set is immutable at runtime so a process that escapes to the Lima
// user cannot use a privileged refresh operation to weaken it. The VM's copy is
// the one deny set pisafe did not compose, so it is parsed rather than trusted:
// a line that is not an IPv4 prefix fails the check instead of being skipped.
func (vm VM) verifyFirewall(ctx context.Context, prefixes []string) error {
	output, err := vm.runner.Run(ctx, nil, vm.inVM([]string{
		"cat", "/etc/pisafe/host-prefixes",
	})...)
	if err != nil {
		return fmt.Errorf("read VM firewall networks: %w", err)
	}
	installed := make([]netip.Prefix, 0, len(prefixes))
	for _, field := range strings.Fields(string(output)) {
		prefix, err := netip.ParsePrefix(field)
		if err != nil || !prefix.Addr().Is4() {
			return fmt.Errorf("validate VM firewall networks: invalid IPv4 prefix %q", field)
		}
		installed = append(installed, prefix)
	}
	actual, err := CanonicalIPv4Prefixes(installed)
	if err != nil {
		return fmt.Errorf("validate VM firewall networks: %w", err)
	}
	if !slices.Equal(actual, prefixes) {
		return fmt.Errorf(
			"VM firewall networks are stale; rebuild the VM with pisafe vm rebuild",
		)
	}
	return nil
}

type execRunner struct {
	binary string
}

func (runner execRunner) Run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	var stdout bytes.Buffer
	command := exec.CommandContext(ctx, runner.binary, args...)
	command.Stdin = stdin
	command.Stdout = &stdout
	if err := runner.wait(command, args); err != nil {
		return nil, err
	}
	return stdout.Bytes(), nil
}

func (runner execRunner) Stream(ctx context.Context, stdout io.Writer, args ...string) error {
	command := exec.CommandContext(ctx, runner.binary, args...)
	command.Stdout = stdout
	return runner.wait(command, args)
}

// wait reports what the command said went wrong. The full argument list is
// included only when the command said nothing at all, because a container
// command is dozens of arguments long and would bury its own error message.
func (runner execRunner) wait(command *exec.Cmd, args []string) error {
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		if detail := strings.TrimSpace(stderr.String()); detail != "" {
			return fmt.Errorf("%s %s: %s", runner.binary, args[0], detail)
		}
		return fmt.Errorf("%s %s: %v", runner.binary, strings.Join(args, " "), err)
	}
	return nil
}

func WriteConfig(path string, config []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create Lima configuration: %w", err)
	}
	complete := false
	defer func() {
		file.Close()
		if !complete {
			os.Remove(path)
		}
	}()
	if _, err := file.Write(config); err != nil {
		return fmt.Errorf("write Lima configuration: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync Lima configuration: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close Lima configuration: %w", err)
	}
	complete = true
	return nil
}
