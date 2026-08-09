package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/mpizenberg/pisafe/internal/runstate"
)

// forwardRequest is one parsed `pisafe forward [RUN] [LOCAL:]PORT...`.
type forwardRequest struct {
	runID string
	ports []forwardPort
}

// forwardPort carries one port between this Mac and a run. The two numbers
// differ only when the local one is already taken by something else.
type forwardPort struct {
	local  int
	remote int
}

var errForwardUsage = fmt.Errorf("usage: pisafe forward [RUN] [LOCAL:]PORT...")

// portArgument matches what can only have been meant as a port. Run names
// always carry a letter, so nothing shaped like this is a name pisafe should
// go looking for.
var portArgument = regexp.MustCompile(`^[0-9]+(:[0-9]+)?$`)

func parseForwardRequest(args []string) (forwardRequest, error) {
	request := forwardRequest{}
	for _, argument := range args {
		switch {
		case strings.HasPrefix(argument, "-"):
			return forwardRequest{}, fmt.Errorf(
				"unknown forward option %q\n%w",
				argument,
				errForwardUsage,
			)
		case portArgument.MatchString(argument):
			port, err := parseForwardPort(argument)
			if err != nil {
				return forwardRequest{}, err
			}
			request.ports = append(request.ports, port)
		case request.runID != "":
			return forwardRequest{}, errForwardUsage
		default:
			request.runID = argument
		}
	}
	if len(request.ports) == 0 {
		return forwardRequest{}, errForwardUsage
	}
	return request, nil
}

func parseForwardPort(argument string) (forwardPort, error) {
	localText, remoteText, paired := strings.Cut(argument, ":")
	if !paired {
		remoteText = localText
	}
	local, err := portNumber(localText)
	if err != nil {
		return forwardPort{}, err
	}
	remote, err := portNumber(remoteText)
	if err != nil {
		return forwardPort{}, err
	}
	return forwardPort{local: local, remote: remote}, nil
}

func portNumber(text string) (int, error) {
	number, err := strconv.Atoi(text)
	if err != nil || number < 1 || number > 65535 {
		return 0, fmt.Errorf("%q is not a port between 1 and 65535", text)
	}
	return number, nil
}

func runForward(ctx context.Context, args []string, out io.Writer) error {
	request, err := parseForwardRequest(args)
	if err != nil {
		return err
	}
	runID, err := resolveRunID(ctx, request.runID)
	if err != nil {
		return err
	}
	manifest, err := activeRun(ctx, runID, out)
	if err != nil {
		return err
	}
	argv := forwardArgv(manifest, request.ports)
	binary, err := exec.LookPath(argv[0])
	if err != nil {
		return fmt.Errorf("find ssh: %w", err)
	}
	argv[0] = binary
	printForward(out, runID, request.ports)
	// The forward lasts exactly as long as this process, so ssh takes it over
	// rather than being supervised: Ctrl-C reaches the client that holds the
	// listeners, and its exit status is the one the user sees.
	return syscall.Exec(binary, argv, os.Environ())
}

// forwardArgv renders the SSH invocation that carries each port. Nothing is
// published anywhere: every listener is a channel on the run's own SSH
// connection, opened by the one key that can authenticate to it.
func forwardArgv(manifest runstate.Manifest, ports []forwardPort) []string {
	argv := []string{
		"ssh",
		"-F", manifest.SSH.ConfigFile,
		"-N", "-T",
		// A run's SSH config clears every forwarding, so an editor or terminal
		// session carries none. This is the command that asks for them, and it
		// says so where the request is made.
		"-o", "ClearAllForwardings=no",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "ServerAliveInterval=30",
		"-o", "ServerAliveCountMax=3",
	}
	for _, port := range ports {
		argv = append(argv, "-L", fmt.Sprintf(
			"127.0.0.1:%d:127.0.0.1:%d",
			port.local,
			port.remote,
		))
	}
	return append(argv, manifest.SSH.Alias)
}

func printForward(out io.Writer, runID string, ports []forwardPort) {
	fmt.Fprintf(out, "Forwarding into %s:\n", runID)
	for _, port := range ports {
		fmt.Fprintf(out, "  127.0.0.1:%d -> port %d in the run\n", port.local, port.remote)
	}
	fmt.Fprintln(out, "Reachable from this Mac only. Ctrl-C ends the forward.")
}
