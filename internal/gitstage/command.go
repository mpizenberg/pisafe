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
