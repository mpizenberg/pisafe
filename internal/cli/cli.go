package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/mpizenberg/pisafe/internal/runstate"
)

var errUsage = errors.New("usage: pisafe <doctor|list|help>")

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
	case "list":
		if len(args) != 1 {
			return errUsage
		}
		return runList(out)
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
  pisafe list      Show durable run records
  pisafe help      Show this help

Run creation is still under construction. The mountless VM transport and
hardened SSH container path are implemented internally but remain hidden until
user-facing orchestration and confirmed cleanup are complete.`)
}

func runList(out io.Writer) error {
	root, err := runstate.DefaultRoot()
	if err != nil {
		return err
	}
	runs, err := runstate.NewStore(root).List()
	if err != nil {
		return err
	}
	if len(runs) == 0 {
		fmt.Fprintln(out, "No runs.")
		return nil
	}
	table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "RUN\tSTATE\tPROJECT\tUPDATED")
	for _, run := range runs {
		state := string(run.State)
		if run.LastError != "" {
			state += " (error)"
		}
		fmt.Fprintf(
			table,
			"%s\t%s\t%s\t%s\n",
			run.RunID,
			state,
			run.Project,
			run.UpdatedAt.UTC().Format("2006-01-02 15:04Z"),
		)
	}
	if err := table.Flush(); err != nil {
		return fmt.Errorf("write run list: %w", err)
	}
	return nil
}
