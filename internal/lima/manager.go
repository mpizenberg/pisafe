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

// Start starts (or reuses) the VM and refreshes the host-network deny set
// before returning. Callers must not start run containers if this fails.
func (manager Manager) Start(ctx context.Context, hostPrefixes []string) error {
	if _, err := renderPrefixUpdate(hostPrefixes); err != nil {
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
	return manager.RefreshFirewall(ctx, hostPrefixes)
}

// RefreshFirewall atomically replaces the host on-link prefix set after each
// start or resume. It must complete before any run container is resumed.
func (manager Manager) RefreshFirewall(ctx context.Context, prefixes []string) error {
	batch, err := renderPrefixUpdate(prefixes)
	if err != nil {
		return err
	}
	if _, err := manager.runner.Run(
		ctx,
		strings.NewReader(batch),
		"shell",
		manager.instance,
		"sudo",
		"nft",
		"--file",
		"-",
	); err != nil {
		return fmt.Errorf("refresh VM firewall: %w", err)
	}
	return nil
}

func renderPrefixUpdate(prefixes []string) (string, error) {
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
	return fmt.Sprintf(
		"flush set inet pisafe host_onlink_v4\nadd element inet pisafe host_onlink_v4 { %s }\n",
		strings.Join(canonical, ", "),
	), nil
}

type execRunner struct {
	binary string
}

func (runner execRunner) Run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, runner.binary, args...)
	command.Stdin = stdin
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return nil, fmt.Errorf("%s %s: %s", runner.binary, strings.Join(args, " "), detail)
	}
	return stdout.Bytes(), nil
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
