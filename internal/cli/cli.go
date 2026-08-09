package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/mpizenberg/pisafe/internal/gitstage"
	"github.com/mpizenberg/pisafe/internal/runid"
	"github.com/mpizenberg/pisafe/internal/runstate"
)

var errUsage = errors.New(
	"usage: pisafe <run|connect|forward|stop|resume|diff|cp|apply|discard|project|profile" +
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
		return runConnect(ctx, args[1:], out)
	case "forward":
		return runForward(ctx, args[1:], out)
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
		runID, err := runIDArgument(ctx, args[1:])
		if err != nil {
			return err
		}
		return runZed(ctx, runID, out)
	case "stop":
		runID, err := runIDArgument(ctx, args[1:])
		if err != nil {
			return err
		}
		return runStop(ctx, runID, out)
	case "resume":
		runID, err := runIDArgument(ctx, args[1:])
		if err != nil {
			return err
		}
		return runResume(ctx, runID, out)
	case "diff":
		runID, err := runIDArgument(ctx, args[1:])
		if err != nil {
			return err
		}
		return runDiff(ctx, runID, out)
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
  pisafe connect [RUN] [-- COMMAND...]
                   Open a shell in a run's workspace, where pi starts the
                   agent. With -- COMMAND, run that instead and exit with
                   its status: the words are parsed by the run's own shell, so
                   a redirect or a pipe means there what it means here.
  pisafe forward [RUN] [LOCAL:]PORT...
                   Reach a server a run is hosting, so a web app developed
                   inside one can be opened in your browser. Each port becomes
                   a listener on this Mac's loopback that carries TCP to the
                   same port inside the run; LOCAL:PORT moves it to another
                   local port when that one is taken. Nothing is published in
                   the VM or on this Mac, the run gains no way to reach
                   anything here, and the forward ends with Ctrl-C.
  pisafe stop [RUN]
                   Stop a run while preserving its workspace
  pisafe resume [RUN]
                   Resume a stopped run
  pisafe diff [RUN]
                   Report what a run changed since it started, without
                   stopping it. Commit subjects and file names come from the
                   run, so they are shown quoted, never as file content.
  pisafe cp [RUN]:PATH [DEST] [--force]
  pisafe cp PATH [RUN]: [--force] [--unsafe]
                   Copy one file or directory out of a run, or into one. The
                   colon marks the end that is in the run, and naming the run
                   is optional as everywhere else. Only regular files and
                   directories are copied. A destination that is already a
                   directory takes the copy inside it; any other existing
                   destination is replaced only with --force. Copying a
                   credential-shaped name in needs --unsafe, because
                   everything in the run can then read and exfiltrate it.
  pisafe apply [RUN] [--keep-baseline|--drop-baseline]
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
                   Put a backup back into a VM whose storage starts empty:
                   another Mac, or a state disk that was lost. Recreating the
                   VM alone does not need it, because its state disk outlives
                   it. Every extension and tool is reinstalled from the pin the
                   backup recorded rather than from what npm resolves the name
                   to now. Nothing already installed is replaced and no
                   transcript is overwritten, so restoring twice is harmless.
  pisafe extension install PACKAGE[@VERSION]
                   Install a Pi extension into the profile every run mounts
                   read-only, pinned to an exact version and integrity hash.
                   A run installing one for itself reaches its own home and
                   never the profile; stopping reports what it installed.
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
  pisafe zed [RUN] Open a run in Zed, saving the connection it needs first.
                   A run's alias resolves only through pisafe's own per-run SSH
                   config, so one entry per run goes into Zed's saved
                   connections and comes back out when the run is discarded or
                   collected. Nothing else outside pisafe's own state is
                   written.
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

A command that takes RUN finds it without being told when the checkout you are
standing in has exactly one run left to import. Discarding always names its run
twice, whatever the checkout holds.

Runs never receive provider credentials; pisafe login keeps them in the
macOS Keychain and pisafe broker relays inference to a revocable per-run
capability. Zed's saved connections are the only file outside pisafe's own
state it writes, one entry per run and only when you run pisafe zed; your SSH
configuration is never touched.`)
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

// resolveRunID names the run a command was given, or the one live run of the
// checkout the user is standing in when it was given none. A run whose work
// has already been imported is not a candidate, so runs waiting to be collected
// never make the shorthand ambiguous, and a checkout with several live runs is
// told to choose rather than guessed at.
func resolveRunID(ctx context.Context, runID string) (string, error) {
	if runID != "" {
		return runID, nil
	}
	root, err := gitstage.RepositoryRoot(ctx, ".")
	if err != nil {
		return "", fmt.Errorf("%w; name the run instead", err)
	}
	project, err := runid.NewProject(root)
	if err != nil {
		return "", err
	}
	stateRoot, err := runstate.DefaultRoot()
	if err != nil {
		return "", err
	}
	runs, err := runstate.NewStore(stateRoot).List()
	if err != nil {
		return "", err
	}
	return chooseProjectRun(runs, project)
}

// chooseProjectRun picks the one run a checkout still has to import. A run
// whose work is already on a branch is finished with and never a candidate, so
// runs waiting to be collected cannot make the choice ambiguous; two that are
// not is a question rather than something to guess at.
func chooseProjectRun(runs []runstate.Manifest, project runid.Project) (string, error) {
	live := []runstate.Manifest{}
	for _, run := range runs {
		if run.ProjectKey == project.Key && run.State != runstate.StateImported {
			live = append(live, run)
		}
	}
	switch len(live) {
	case 1:
		return live[0].RunID, nil
	case 0:
		return "", fmt.Errorf(
			"%s has no live run; start one with pisafe run",
			project.Directory,
		)
	default:
		names := make([]string, 0, len(live))
		for _, run := range live {
			names = append(names, "  "+run.RunID+"  "+string(run.State))
		}
		return "", fmt.Errorf(
			"%s has %d live runs; name the one you mean:\n%s",
			project.Directory,
			len(live),
			strings.Join(names, "\n"),
		)
	}
}

// runIDArgument reads the optional run name of a command that takes nothing
// else.
func runIDArgument(ctx context.Context, args []string) (string, error) {
	switch len(args) {
	case 0:
		return resolveRunID(ctx, "")
	case 1:
		return args[0], nil
	default:
		return "", errUsage
	}
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
