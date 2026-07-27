package cli

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/mpizenberg/pisafe/internal/runstate"
)

// connectRequest is one parsed `pisafe connect RUN [--shell]`.
type connectRequest struct {
	runID string
	shell bool
}

var errConnectUsage = fmt.Errorf("usage: pisafe connect RUN [--shell]")

func parseConnectRequest(args []string) (connectRequest, error) {
	request := connectRequest{}
	positional := []string{}
	for _, argument := range args {
		if argument == "--shell" {
			request.shell = true
			continue
		}
		if strings.HasPrefix(argument, "-") {
			return connectRequest{}, fmt.Errorf(
				"unknown connect option %q\n%w",
				argument,
				errConnectUsage,
			)
		}
		positional = append(positional, argument)
	}
	if len(positional) != 1 {
		return connectRequest{}, errConnectUsage
	}
	request.runID = positional[0]
	return request, nil
}

func runConnect(args []string) error {
	request, err := parseConnectRequest(args)
	if err != nil {
		return err
	}
	manifest, err := activeRun(request.runID)
	if err != nil {
		return err
	}
	argv := connectArgv(manifest, request.shell)
	binary, err := exec.LookPath(argv[0])
	if err != nil {
		return fmt.Errorf("find ssh: %w", err)
	}
	argv[0] = binary
	// The terminal now belongs to the run, and pisafe has nothing left to do
	// with it: replacing this process hands over signals, window resizes, and
	// the exit status without relaying any of them.
	return syscall.Exec(binary, argv, os.Environ())
}

// connectArgv renders the SSH invocation that attaches this terminal to a run.
// The remote side runs it through a shell, so the workspace path is quoted for
// one; nothing else in the command comes from outside pisafe.
func connectArgv(manifest runstate.Manifest, shell bool) []string {
	remote := "cd " + shellQuote(manifest.Workspace) + " && exec "
	if shell {
		remote += `"$SHELL" -l`
	} else {
		remote += "pi"
	}
	return []string{
		"ssh",
		"-F", manifest.SSH.ConfigFile,
		"-t",
		manifest.SSH.Alias,
		remote,
	}
}
