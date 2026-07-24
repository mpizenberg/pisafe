package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"text/tabwriter"

	"github.com/mpizenberg/pisafe/internal/runstate"
)

var errUsage = errors.New("usage: pisafe <run|list|zed|doctor|help>")

func Run(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		printHelp(out)
		return nil
	}

	switch args[0] {
	case "run":
		if len(args) != 1 {
			return errUsage
		}
		return runCreate(ctx, out)
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
	case "zed":
		if len(args) != 2 {
			return errUsage
		}
		return runZed(ctx, args[1])
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
  pisafe run       Create an isolated run from the current Git repository
  pisafe zed RUN   Open a configured run in Zed
  pisafe doctor    Check Phase 1 host prerequisites
  pisafe list      Show durable run records
  pisafe help      Show this help

Pi inference remains unavailable until the credential broker is implemented.
After creating a run, use the printed one-time Zed Remote Projects command;
pisafe never edits global SSH or Zed settings.`)
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

func runZed(ctx context.Context, runID string) error {
	root, err := runstate.DefaultRoot()
	if err != nil {
		return err
	}
	manifest, err := runstate.NewStore(root).Get(runID)
	if err != nil {
		return err
	}
	if manifest.State != runstate.StateActive {
		return fmt.Errorf("run %q is %s, not active", runID, manifest.State)
	}
	if manifest.SSH == nil {
		return fmt.Errorf("run %q has no SSH connection", runID)
	}
	zed, err := exec.LookPath("zed")
	if err != nil {
		return fmt.Errorf("find Zed CLI: install it from Zed's command palette")
	}
	command := exec.CommandContext(
		ctx,
		zed,
		"ssh://"+manifest.SSH.Alias+manifest.Workspace,
	)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("open run in Zed: %s", output)
	}
	return nil
}
