package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/mpizenberg/pisafe/internal/runstate"
)

// connectRequest is one parsed `pisafe connect [RUN] [-- COMMAND...]`. An empty
// command is an interactive shell, which is what reaches every other state a
// run can be worked in, including the agent.
type connectRequest struct {
	runID   string
	command []string
}

var errConnectUsage = fmt.Errorf("usage: pisafe connect [RUN] [-- COMMAND...]")

func parseConnectRequest(args []string) (connectRequest, error) {
	request := connectRequest{}
	for index, argument := range args {
		if argument == "--" {
			request.command = args[index+1:]
			if len(request.command) == 0 {
				return connectRequest{}, errConnectUsage
			}
			return request, nil
		}
		if strings.HasPrefix(argument, "-") {
			return connectRequest{}, fmt.Errorf(
				"unknown connect option %q\n%w",
				argument,
				errConnectUsage,
			)
		}
		if request.runID != "" {
			return connectRequest{}, errConnectUsage
		}
		request.runID = argument
	}
	return request, nil
}

func runConnect(ctx context.Context, args []string, out io.Writer) error {
	request, err := parseConnectRequest(args)
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
	interactive := isTerminal(os.Stdin) && isTerminal(os.Stdout)
	argv := connectArgv(manifest, request.command, interactive)
	binary, err := exec.LookPath(argv[0])
	if err != nil {
		return fmt.Errorf("find ssh: %w", err)
	}
	argv[0] = binary
	if len(request.command) == 0 && interactive {
		// A shell says where it is but not what is installed in it, and the
		// agent is the reason the run exists.
		fmt.Fprintf(out, "%s: shell in %s. Run `pi` to start the agent.\n", runID, manifest.Workspace)
	}
	// The terminal now belongs to the run, and pisafe has nothing left to do
	// with it: replacing this process hands over signals, window resizes, and
	// the exit status without relaying any of them.
	return syscall.Exec(binary, argv, os.Environ())
}

// connectArgv renders the SSH invocation that runs one command in a run's
// workspace, or an interactive shell there when no command was given. Command
// words are joined and parsed by the run's own shell, the way ssh itself passes
// a command along, so a redirect or a pipe written on the pisafe command line
// means in the run what it would mean here. Only the workspace path is quoted,
// because it is the one word pisafe supplies. A shell replaces the shell that
// started it; a command does not, because exec would run the first command of
// a list and silently drop the rest.
func connectArgv(manifest runstate.Manifest, command []string, interactive bool) []string {
	remote := "cd " + shellQuote(manifest.Workspace) + " && "
	if len(command) == 0 {
		remote += `exec "$SHELL" -l`
	} else {
		remote += strings.Join(command, " ")
	}
	// A pty is what an editor or a shell needs and what a redirected stream
	// cannot survive, so it is asked for exactly when both ends are a terminal.
	terminal := "-T"
	if interactive {
		terminal = "-t"
	}
	return []string{
		"ssh",
		"-F", manifest.SSH.ConfigFile,
		terminal,
		manifest.SSH.Alias,
		remote,
	}
}
