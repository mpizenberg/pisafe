package gitstage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

type commandError struct {
	args   []string
	stderr string
	err    error
}

func (e *commandError) Error() string {
	detail := strings.TrimSpace(e.stderr)
	if detail == "" {
		return fmt.Sprintf("git %s: %v", strings.Join(e.args, " "), e.err)
	}
	return fmt.Sprintf("git %s: %s", strings.Join(e.args, " "), detail)
}

func (e *commandError) Unwrap() error {
	return e.err
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	var stdout bytes.Buffer
	if err := gitRun(ctx, dir, nil, &stdout, args...); err != nil {
		return "", err
	}
	return strings.TrimSpace(stdout.String()), nil
}

func gitRun(ctx context.Context, dir string, stdin io.Reader, stdout io.Writer, args ...string) error {
	commandArgs := make([]string, 0, len(args)+2)
	if dir != "" {
		commandArgs = append(commandArgs, "-C", dir)
	}
	commandArgs = append(commandArgs, args...)

	cmd := exec.CommandContext(ctx, "git", commandArgs...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return &commandError{args: commandArgs, stderr: stderr.String(), err: err}
	}
	return nil
}

func isExitCode(err error, code int) bool {
	var exitError *exec.ExitError
	return errors.As(err, &exitError) && exitError.ExitCode() == code
}

// isAncestor reports whether one commit is in another's history. Git says no by
// exiting 1, so any other failure is an unanswered question rather than a no.
func isAncestor(ctx context.Context, repository, ancestor, descendant string) (bool, error) {
	err := gitRun(
		ctx, repository, nil, nil,
		"merge-base", "--is-ancestor", ancestor, descendant,
	)
	switch {
	case err == nil:
		return true, nil
	case isExitCode(err, 1):
		return false, nil
	default:
		return false, fmt.Errorf("compare %s with %s: %w", ancestor, descendant, err)
	}
}

// requireAncestor refuses a history that is not built on the commit pisafe gave
// the run. Everything staging and apply conclude about what a run produced
// follows from that relationship, so refusal names what was expected of it.
func requireAncestor(ctx context.Context, repository, ancestor, descendant, refusal string) error {
	based, err := isAncestor(ctx, repository, ancestor, descendant)
	if err != nil {
		return err
	}
	if !based {
		return errors.New(refusal)
	}
	return nil
}
