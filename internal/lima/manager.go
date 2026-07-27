package lima

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type Status string

const (
	StatusAbsent  Status = "absent"
	StatusRunning Status = "running"
	StatusStopped Status = "stopped"
)

type Runner interface {
	Run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error)
	// Stream writes a command's standard output to the caller as it arrives,
	// so an artifact leaving the VM is never held in memory as a whole.
	Stream(ctx context.Context, stdout io.Writer, args ...string) error
}

type Manager struct {
	instance string
	runner   Runner
}

func NewManager() Manager {
	return Manager{
		instance: InstanceName,
		runner:   execRunner{binary: "limactl"},
	}
}

func (manager Manager) Status(ctx context.Context) (Status, error) {
	output, err := manager.runner.Run(ctx, nil, "list", "--format", "{{.Name}}\t{{.Status}}")
	if err != nil {
		return "", fmt.Errorf("list Lima instances: %w", err)
	}
	for _, line := range strings.Split(string(output), "\n") {
		name, status, found := strings.Cut(line, "\t")
		if !found || name != manager.instance {
			continue
		}
		switch strings.ToLower(status) {
		case "running":
			return StatusRunning, nil
		case "stopped":
			return StatusStopped, nil
		default:
			return "", fmt.Errorf("Lima instance %q is %s", manager.instance, status)
		}
	}
	return StatusAbsent, nil
}

func (manager Manager) Create(ctx context.Context, configPath string) error {
	status, err := manager.Status(ctx)
	if err != nil {
		return err
	}
	if status != StatusAbsent {
		return fmt.Errorf("Lima instance %q already exists (%s)", manager.instance, status)
	}
	if _, err := manager.runner.Run(ctx, nil, "template", "validate", configPath); err != nil {
		return fmt.Errorf("validate Lima configuration: %w", err)
	}
	if _, err := manager.runner.Run(
		ctx,
		nil,
		"--tty=false",
		"create",
		"--name="+manager.instance,
		configPath,
	); err != nil {
		return fmt.Errorf("create Lima instance: %w", err)
	}
	return nil
}

// Ensure creates the dedicated VM when absent, then starts and verifies it
// against the current host-network boundary.
func (manager Manager) Ensure(ctx context.Context, prefixes []netip.Prefix) error {
	config, err := RenderConfig(DefaultConfigOptions(prefixes))
	if err != nil {
		return err
	}
	prefixStrings := make([]string, 0, len(prefixes))
	for _, prefix := range prefixes {
		prefixStrings = append(prefixStrings, prefix.String())
	}
	status, err := manager.Status(ctx)
	if err != nil {
		return err
	}
	if status == StatusAbsent {
		temporary, err := os.MkdirTemp("", "pisafe-lima-config-*")
		if err != nil {
			return fmt.Errorf("create temporary Lima config directory: %w", err)
		}
		defer os.RemoveAll(temporary)
		configPath := filepath.Join(temporary, "pisafe.yaml")
		if err := WriteConfig(configPath, config); err != nil {
			return err
		}
		if err := manager.Create(ctx, configPath); err != nil {
			return err
		}
	}
	return manager.Start(ctx, prefixStrings)
}

// Start starts (or reuses) the VM and verifies that its immutable host-network
// deny set and generated security profile still match the controller. Callers
// must not start run containers if this fails.
func (manager Manager) Start(ctx context.Context, hostPrefixes []string) error {
	if _, err := renderPrefixInput(hostPrefixes); err != nil {
		return err
	}
	status, err := manager.Status(ctx)
	if err != nil {
		return err
	}
	switch status {
	case StatusAbsent:
		return fmt.Errorf("Lima instance %q has not been created", manager.instance)
	case StatusRunning:
		// Refresh below: a running VM may have crossed a host network change.
	case StatusStopped:
		if _, err := manager.runner.Run(
			ctx,
			nil,
			"--tty=false",
			"start",
			manager.instance,
		); err != nil {
			return fmt.Errorf("start Lima instance: %w", err)
		}
	default:
		return fmt.Errorf("unsupported Lima status %q", status)
	}
	if err := manager.VerifySecurityProfile(ctx, hostPrefixes); err != nil {
		return err
	}
	if err := manager.SyncClock(ctx); err != nil {
		return err
	}
	return manager.VerifyFirewall(ctx, hostPrefixes)
}

// VerifySecurityProfile detects an instance provisioned by an older or locally
// modified VM definition. The record is root-owned and immutable to the
// unprivileged Lima user after provisioning.
func (manager Manager) VerifySecurityProfile(ctx context.Context, prefixes []string) error {
	rendered, err := renderPrefixInput(prefixes)
	if err != nil {
		return err
	}
	expected := securityProfileDigest(strings.Fields(rendered))
	output, err := manager.runner.Run(
		ctx,
		nil,
		"shell",
		manager.instance,
		"cat",
		"/etc/pisafe/security-profile",
	)
	if err != nil {
		return fmt.Errorf(
			"read VM security profile: %w; recreate the pisafe VM before starting a run",
			err,
		)
	}
	if strings.TrimSpace(string(output)) != expected {
		return fmt.Errorf(
			"VM security profile is stale; recreate the pisafe VM before starting a run",
		)
	}
	return nil
}

// SyncClock steps a clock that drifted while the plain-mode VM was suspended.
// Plain mode has no Lima guest agent to perform host/guest time correction.
func (manager Manager) SyncClock(ctx context.Context) error {
	if _, err := manager.runner.Run(
		ctx,
		nil,
		"shell",
		manager.instance,
		"sudo",
		"/usr/local/sbin/pisafe-clock-step",
	); err != nil {
		return fmt.Errorf("synchronize VM clock: %w", err)
	}
	return nil
}

// VerifyFirewall refuses to reuse a VM after the Mac's on-link networks
// change. The prefix set is immutable at runtime so a process that escapes to
// the Lima user cannot use a privileged refresh operation to weaken it.
func (manager Manager) VerifyFirewall(ctx context.Context, prefixes []string) error {
	expected, err := renderPrefixInput(prefixes)
	if err != nil {
		return err
	}
	output, err := manager.runner.Run(
		ctx,
		nil,
		"shell",
		manager.instance,
		"cat",
		"/etc/pisafe/host-prefixes",
	)
	if err != nil {
		return fmt.Errorf("read VM firewall networks: %w", err)
	}
	actualLines := strings.Fields(string(output))
	actual, err := renderPrefixInput(actualLines)
	if err != nil {
		return fmt.Errorf("validate VM firewall networks: %w", err)
	}
	if actual != expected {
		return fmt.Errorf(
			"VM firewall networks are stale; recreate the pisafe VM before starting a run",
		)
	}
	return nil
}

func renderPrefixInput(prefixes []string) (string, error) {
	if len(prefixes) == 0 {
		return "", errors.New("host IPv4 prefixes are required; refusing to empty the firewall set")
	}
	parsed := make([]netip.Prefix, 0, len(prefixes))
	for _, rawPrefix := range prefixes {
		prefix, err := netip.ParsePrefix(rawPrefix)
		if err != nil || !prefix.Addr().Is4() {
			return "", fmt.Errorf("invalid IPv4 prefix %q", rawPrefix)
		}
		parsed = append(parsed, prefix)
	}
	canonical, err := canonicalIPv4Prefixes(parsed)
	if err != nil {
		return "", err
	}
	return strings.Join(canonical, "\n") + "\n", nil
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
