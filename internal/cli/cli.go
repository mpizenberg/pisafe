package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"text/tabwriter"
	"time"

	"github.com/mpizenberg/pisafe/internal/runstate"
)

var errUsage = errors.New(
	"usage: pisafe <run|stop|resume|apply|discard|list|zed|login|broker|doctor|help>",
)

func Run(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		printHelp(out)
		return nil
	}

	switch args[0] {
	case "run":
		return runCreate(ctx, args[1:], out)
	case "login":
		if len(args) != 2 {
			return fmt.Errorf("login requires a provider: pisafe login chatgpt")
		}
		return runLogin(ctx, args[1], out)
	case "broker":
		if len(args) != 1 {
			return errUsage
		}
		return runBroker(ctx, out)
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
	case "stop":
		if len(args) != 2 {
			return errUsage
		}
		return runStop(ctx, args[1], out)
	case "resume":
		if len(args) != 2 {
			return errUsage
		}
		return runResume(ctx, args[1], out)
	case "apply":
		if len(args) != 2 {
			return errUsage
		}
		return runApply(ctx, args[1], out)
	case "discard":
		if len(args) != 4 || args[2] != "--confirm" {
			return fmt.Errorf(
				"discard requires exact confirmation: pisafe discard RUN --confirm RUN",
			)
		}
		if args[3] != args[1] {
			return fmt.Errorf("discard confirmation does not exactly match run %q", args[1])
		}
		return runDiscard(ctx, args[1], out)
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
  pisafe run [--include PATH]... [--include-unsafe PATH]...
                   Create an isolated run from the current Git repository.
                   Untracked and ignored files stay out unless --include names
                   them; a credential-shaped path needs --include-unsafe,
                   which voids the run's credential isolation.
  pisafe stop RUN  Stop a run while preserving its workspace
  pisafe resume RUN
                   Resume a stopped run
  pisafe apply RUN
                   Import a run's commits as the local branch pisafe/RUN.
                   The run is stopped first and cannot be resumed afterwards;
                   your checkout, index, and current branch are not touched.
  pisafe discard RUN --confirm RUN
                   Permanently delete one exact run workspace
  pisafe zed RUN   Open a configured run in Zed
  pisafe login chatgpt
                   Store a ChatGPT subscription login in the macOS Keychain
  pisafe broker    Relay brokered inference to active runs (foreground)
  pisafe doctor    Check Phase 1 host prerequisites
  pisafe list      Show durable run records
  pisafe help      Show this help

Runs never receive provider credentials; pisafe login keeps them in the
macOS Keychain and pisafe broker relays inference to a revocable per-run
capability. After creating a run, use the printed one-time Zed Remote
Projects command; pisafe never edits global SSH or Zed settings.`)
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
		if run.State == runstate.StateActive &&
			runstate.RemainingSeconds(run, time.Now()) == 0 {
			state += " (limit reached)"
		}
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
	if runstate.RemainingSeconds(manifest, time.Now()) == 0 {
		return fmt.Errorf(
			"run %q reached its wall-clock limit; use pisafe stop %s to reconcile it",
			runID,
			runID,
		)
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
