package lima

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mpizenberg/pisafe/internal/runssh"
)

// ReverseForward is the ssh child process that publishes a Mac-loopback
// listener on the VM's dedicated broker address. The VM listener exists only
// while this process is alive, so the relay closes when the controller exits.
type ReverseForward struct {
	command *exec.Cmd
	stderr  *bytes.Buffer
	done    chan error
}

func reverseForwardArgs(gateway runssh.Gateway, localPort int) ([]string, error) {
	if !filepath.IsAbs(gateway.ConfigFile) ||
		strings.ContainsAny(gateway.ConfigFile, "\r\n\x00") {
		return nil, errors.New("invalid Lima SSH config path")
	}
	if gateway.Alias == "" || strings.ContainsAny(gateway.Alias, " \t\r\n\x00") {
		return nil, errors.New("invalid Lima SSH alias")
	}
	if localPort < 1 || localPort > 65535 {
		return nil, fmt.Errorf("invalid broker relay port %d", localPort)
	}
	return []string{
		"-F", gateway.ConfigFile,
		"-N", "-T",
		"-o", "BatchMode=yes",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "ControlMaster=no",
		"-o", "ControlPath=none",
		"-o", "ServerAliveInterval=30",
		"-o", "ServerAliveCountMax=3",
		"-R", BrokerAddress + ":" + strconv.Itoa(BrokerPort) +
			":127.0.0.1:" + strconv.Itoa(localPort),
		gateway.Alias,
	}, nil
}

// StartReverseForward connects the VM broker address to a local listener port.
// The child is bound to ctx and killed when it is cancelled.
func StartReverseForward(
	ctx context.Context,
	gateway runssh.Gateway,
	localPort int,
) (*ReverseForward, error) {
	args, err := reverseForwardArgs(gateway, localPort)
	if err != nil {
		return nil, err
	}
	command := exec.CommandContext(ctx, "ssh", args...)
	stderr := &bytes.Buffer{}
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start broker reverse relay: %w", err)
	}
	forward := &ReverseForward{
		command: command,
		stderr:  stderr,
		done:    make(chan error, 1),
	}
	go func() {
		forward.done <- forward.describeExit(command.Wait())
	}()
	return forward, nil
}

func (forward *ReverseForward) describeExit(waitErr error) error {
	detail := strings.TrimSpace(forward.stderr.String())
	if waitErr == nil {
		if detail != "" {
			return fmt.Errorf("broker reverse relay closed: %s", detail)
		}
		return errors.New("broker reverse relay closed")
	}
	if detail != "" {
		return fmt.Errorf("broker reverse relay failed: %s", detail)
	}
	return fmt.Errorf("broker reverse relay failed: %w", waitErr)
}

// Done reports the relay's exit. It always yields an error because the relay
// is expected to outlive the broker unless something failed or shut down.
func (forward *ReverseForward) Done() <-chan error {
	return forward.done
}

func (forward *ReverseForward) Close() error {
	if forward.command.Process != nil {
		_ = forward.command.Process.Kill()
	}
	select {
	case <-forward.done:
	case <-time.After(5 * time.Second):
		return errors.New("broker reverse relay did not exit")
	}
	return nil
}

// ProbeBrokerListener verifies from inside the VM that the dedicated broker
// address currently accepts connections.
func (transport Transport) ProbeBrokerListener(ctx context.Context) error {
	script := fmt.Sprintf("exec 3<>/dev/tcp/%s/%d", BrokerAddress, BrokerPort)
	if _, err := transport.Execute(ctx, nil, "bash", "-c", script); err != nil {
		return fmt.Errorf("broker relay is not reachable inside the VM: %w", err)
	}
	return nil
}
