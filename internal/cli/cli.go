package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
)

var errUsage = errors.New("usage: pisafe <doctor|help>")

func Run(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		printHelp(out)
		return nil
	}

	switch args[0] {
	case "doctor":
		if len(args) != 1 {
			return errUsage
		}
		return runDoctor(ctx, out)
	case "help", "-h", "--help":
		printHelp(out)
		return nil
	default:
		return fmt.Errorf("unknown command %q\n%w", args[0], errUsage)
	}
}

func printHelp(out io.Writer) {
	fmt.Fprintln(out, `pisafe isolates coding-agent runs from the original checkout.

Usage:
  pisafe doctor    Check Phase 1 host prerequisites
  pisafe help      Show this help

The run lifecycle is under construction. The Git staging and apply core is
implemented and tested, but is not exposed until the mountless VM transport is
available.`)
}
