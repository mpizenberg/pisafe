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
	"syscall"
	"time"

	"github.com/mpizenberg/pisafe/internal/runssh"
)

// brokerProbeReadTimeout bounds how long one probe waits inside the VM. It is
// short because a working relay answers immediately, and it must stay under
// whatever deadline a caller puts on the probe so the VM side is always the
// one that gives up.
const brokerProbeReadTimeout = 3 * time.Second

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
	// A cancelled context must withdraw the forward rather than kill the client
	// outright, for the same reason Close asks first: the broker exits this way
	// on Ctrl-C, and a killed client leaves the VM-side listener bound.
	command.Cancel = func() error {
		return command.Process.Signal(syscall.SIGTERM)
	}
	command.WaitDelay = relayShutdownGrace
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

// relayShutdownGrace bounds each half of the relay's shutdown: how long the
// client is given to withdraw its forward, and then how long a killed client
// is given to die.
const relayShutdownGrace = 5 * time.Second

// Close ends the relay. The client is asked to exit before it is killed,
// because ssh withdraws the forward on its way out and sshd keeps the VM-side
// listener bound until something withdraws it. A client that is only killed
// leaves the broker address taken for whatever tries to bind it next.
func (forward *ReverseForward) Close() error {
	if forward.command.Process == nil {
		return nil
	}
	_ = forward.command.Process.Signal(syscall.SIGTERM)
	select {
	case <-forward.done:
		return nil
	case <-time.After(relayShutdownGrace):
	}
	_ = forward.command.Process.Kill()
	select {
	case <-forward.done:
	case <-time.After(relayShutdownGrace):
		return errors.New("broker reverse relay did not exit")
	}
	return nil
}

// brokerProbeScript asks the relayed address for a response and requires a
// status line back. Any status counts, including the 401 an unauthorized
// request earns: the question is whether bytes cross the relay, not what the
// broker makes of them.
//
// The read carries its own deadline because cancelling from the Mac does not
// reach it. Killing the local client leaves a blocked remote shell holding the
// session open, so a probe that waits indefinitely in the VM is a probe that
// cannot be given up on.
var brokerProbeScript = fmt.Sprintf(`exec 3<>/dev/tcp/%s/%d
printf 'GET / HTTP/1.0\r\nHost: %s\r\nUser-Agent: pisafe-relay-probe\r\nConnection: close\r\n\r\n' >&3
IFS= read -t %d -r status <&3
case "$status" in
HTTP/1.*) ;;
*) exit 1 ;;
esac
`, BrokerAddress, BrokerPort, BrokerAddress, int(brokerProbeReadTimeout.Seconds()))

// ProbeBrokerListener verifies from inside the VM that the dedicated broker
// address relays a request end to end. Connecting is not enough on its own:
// sshd accepts on the forwarded address as soon as it binds, and goes on
// accepting while the client that owns the forward is exiting, so a bare
// connect can succeed against a relay that is already gone.
func (vm VM) ProbeBrokerListener(ctx context.Context) error {
	if _, err := vm.Execute(
		ctx,
		nil,
		"bash", "-ceu", brokerProbeScript, "pisafe-relay-probe",
	); err != nil {
		return fmt.Errorf("broker relay is not reachable inside the VM: %w", err)
	}
	return nil
}
