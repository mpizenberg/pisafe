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
	"usage: pisafe <run|connect|stop|resume|diff|cp|apply|discard|project|profile" +
		"|extension|tool|backup|restore|gc|list|zed|login|logout|broker|doctor|help>",
)

func Run(ctx context.Context, args []string, in io.Reader, out io.Writer) error {
	if len(args) == 0 {
		printHelp(out)
		return nil
	}

	switch args[0] {
	case "run":
		return runCreate(ctx, args[1:], out)
	case "connect":
		return runConnect(args[1:])
	case "login":
		return runLogin(ctx, args[1:], in, out)
	case "logout":
		if len(args) != 2 {
			return errUsage
		}
		return runLogout(ctx, args[1], out)
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
	case "diff":
		if len(args) != 2 {
			return errUsage
		}
		return runDiff(ctx, args[1], out)
	case "cp":
		return runCopy(ctx, args[1:], out)
	case "apply":
		return runApply(ctx, args[1:], in, out)
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
	case "project":
		return runProject(ctx, args[1:], out)
	case "profile":
		return runProfile(ctx, args[1:], out)
	case "backup":
		return runBackup(ctx, args[1:], out)
	case "restore":
		return runRestore(ctx, args[1:], out)
	case "extension":
		return runExtension(ctx, args[1:], out)
	case "tool":
		return runTool(ctx, args[1:], out)
	case "gc":
		return runGC(ctx, args[1:], out)
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
  pisafe connect RUN [--shell]
                   Attach this terminal to a run and start Pi, or with
                   --shell open a shell in the same container.
  pisafe stop RUN  Stop a run while preserving its workspace
  pisafe resume RUN
                   Resume a stopped run
  pisafe diff RUN  Report what a run changed since it started, without
                   stopping it. Commit subjects and file names come from the
                   run, so they are shown quoted, never as file content.
  pisafe cp RUN:PATH [DEST] [--force]
                   Copy one file or directory out of a run. Only regular
                   files and directories are copied; an existing DEST is
                   replaced only with --force.
  pisafe apply RUN [--keep-baseline|--drop-baseline]
                   Import a run's commits as the local branch pisafe/RUN.
                   The run is stopped first and cannot be resumed afterwards;
                   your checkout, index, and current branch are not touched.
                   A run that started from an uncommitted working tree asks
                   whether to import that commit too or replay only the run's
                   own commits without it.
  pisafe discard RUN --confirm RUN
                   Permanently delete one exact run workspace
  pisafe project list
                   Show every project store pisafe holds: where its checkout
                   is, how many runs still belong to it, and whether the
                   checkout is still there.
  pisafe project reset [PATH]
                   Throw away every dependency cache a project's runs share,
                   naming it by its checkout or defaulting to this one.
                   Nothing needs a cache to be correct, so the only cost is
                   that the next run fetches from scratch. Session transcripts
                   are not touched.
  pisafe project drop PATH --confirm PATH
                   Take a whole project store away now, caches and session
                   transcripts together, rather than waiting for gc to find
                   the checkout gone. Nothing reproduces a transcript.
  pisafe project rebind OLD-PATH
                   Give this checkout the session history of the one it was
                   moved or renamed from. Caches are left behind.
  pisafe profile reset --confirm
                   Take every extension and tool back out of the profile.
                   Each is refetchable from npm, but the record of what was
                   installed is not.
  pisafe backup DIRECTORY
                   Copy out what nothing can refetch: every project's session
                   transcripts, and the pins naming what the profile holds.
                   Dependency caches are left out because nothing needs one to
                   be correct, and no provider credential is written at all —
                   those stay in the macOS Keychain. Backing up again into the
                   same directory adds to it and removes nothing.
  pisafe restore DIRECTORY
                   Put a backup back into a VM, which is what a recreated one
                   needs. Every extension and tool is reinstalled from the pin
                   the backup recorded rather than from what npm resolves the
                   name to now. Nothing already installed is replaced and no
                   transcript is overwritten, so restoring twice is harmless.
  pisafe extension install PACKAGE[@VERSION]
                   Install a Pi extension into the profile every run mounts
                   read-only, pinned to an exact version and integrity hash.
                   Runs cannot install one themselves; inside a run, pi -e
                   still tries a package for that run alone.
  pisafe extension update [PACKAGE...]
                   Named, move those pins to what npm resolves them to now.
                   Named none, report what is available and change nothing:
                   an update is offered, never applied on pisafe's initiative.
  pisafe extension remove PACKAGE
                   Take an extension out of the profile
  pisafe extension list
                   Show what the profile has installed, what it is pinned to,
                   and any update still on offer
  pisafe tool install PACKAGE[@VERSION]
                   Install a command-line package into the profile every run
                   mounts read-only, pinned the same way an extension is. The
                   commands it provides are on every run's PATH, behind the
                   run image's own, and a name another tool already provides
                   is refused rather than shadowed. Runs cannot install one
                   themselves.
  pisafe tool remove PACKAGE
                   Take a tool out of the profile
  pisafe tool list Show what commands the profile provides and what each is
                   pinned to
  pisafe gc [--dry-run]
                   Reclaim imported runs older than seven days and prune
                   superseded run images. Their pisafe/RUN branches keep the
                   work. A run whose work was never imported is only reported;
                   discard it explicitly.
  pisafe zed RUN   Open a configured run in Zed
  pisafe login     Show which providers are logged in. Runs are offered all of
                   them at once and pick between them in Pi's model list.
  pisafe login chatgpt
                   Store a ChatGPT subscription login in the macOS Keychain
  pisafe login anthropic|openai
                   Store an API key for that provider. The key is read from
                   stdin, never from the command line.
  pisafe login NAME --url URL --api API --models FILE
                   Store a key for any other endpoint speaking a supported
                   wire format: openai-completions, openai-responses, or
                   anthropic-messages. FILE is a JSON array of Pi model
                   definitions, since pisafe cannot know what an endpoint it
                   has never heard of serves. Plain HTTP is refused except on
                   localhost, where the key stays on this Mac.
  pisafe logout NAME
                   Take one login away
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

func runRecord(runID string) (runstate.Manifest, error) {
	root, err := runstate.DefaultRoot()
	if err != nil {
		return runstate.Manifest{}, err
	}
	return runstate.NewStore(root).Get(runID)
}

// activeRun returns a run an editor or terminal can reach right now. Every
// route into a running container needs the same three facts: the run is
// active, its wall-clock budget is not spent, and it has an SSH endpoint.
func activeRun(runID string) (runstate.Manifest, error) {
	manifest, err := runRecord(runID)
	if err != nil {
		return runstate.Manifest{}, err
	}
	if manifest.State != runstate.StateActive {
		hint := ""
		if manifest.State == runstate.StateStopped {
			hint = "; resume it with pisafe resume " + runID
		}
		return runstate.Manifest{}, fmt.Errorf(
			"run %q is %s, not active%s",
			runID,
			manifest.State,
			hint,
		)
	}
	if runstate.RemainingSeconds(manifest, time.Now()) == 0 {
		return runstate.Manifest{}, fmt.Errorf(
			"run %q reached its wall-clock limit; use pisafe stop %s to reconcile it",
			runID,
			runID,
		)
	}
	if manifest.SSH == nil {
		return runstate.Manifest{}, fmt.Errorf("run %q has no SSH connection", runID)
	}
	return manifest, nil
}

func runZed(ctx context.Context, runID string) error {
	manifest, err := activeRun(runID)
	if err != nil {
		return err
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
