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
	"github.com/mpizenberg/pisafe/internal/lima"
	"github.com/mpizenberg/pisafe/internal/runid"
	"github.com/mpizenberg/pisafe/internal/runstate"
)

var errUsage = errors.New(
	"usage: pisafe <run|connect|forward|stop|resume|diff|cp|apply|discard|project|profile" +
		"|extension|tool|backup|restore|gc|list|zed|login|logout|broker|vm|doctor|help>",
)

func Run(ctx context.Context, args []string, in io.Reader, out io.Writer) error {
	if len(args) == 0 {
		printHelp(out)
		return nil
	}

	// Commands taking an optional run name and nothing else, which is the whole
	// of what runIDArgument reads.
	if command, named := map[string]func(context.Context, string, io.Writer) error{
		"zed":    runZed,
		"stop":   runStop,
		"resume": runResume,
		"diff":   runDiff,
	}[args[0]]; named {
		runID, err := runIDArgument(ctx, args[1:])
		if err != nil {
			return err
		}
		return command(ctx, runID, out)
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
		return runList(ctx, out)
	case "cp":
		return runCopy(ctx, args[1:], out)
	case "apply":
		return runApply(ctx, args[1:], in, out)
	case "discard":
		runID, err := confirmedTarget(args, "discard RUN --confirm RUN", "run ")
		if err != nil {
			return err
		}
		return runDiscard(ctx, runID, out)
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
	case "vm":
		return runVM(ctx, args[1:], out)
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
                   which voids the run's credential isolation. An included path
                   travels as files, never as commits, and apply copies the work
                   left under it back here. Naming a directory that is empty for
                   now is how a run hands back work you do not want committed.
  pisafe connect [RUN] [-- COMMAND...]
                   Open a shell in a run's workspace, where pi starts the
                   agent. With -- COMMAND, run that instead and exit with
                   its status: the words are parsed by the run's own shell, so
                   a redirect or a pipe means there what it means here. A run
                   the VM stopped, by a reboot or a rebuild, is brought back
                   first; one you stopped waits for pisafe resume.
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
  pisafe apply [RUN] [--keep-baseline|--drop-baseline] [--include-force]
                   Import a run's commits as the local branch pisafe/RUN.
                   The run is stopped first and cannot be resumed afterwards;
                   your index and current branch are not touched, and neither
                   is your checkout except under the paths you included, where
                   the run's work is copied back. That copy only adds and
                   updates, and stops if a path changed both in the run and
                   here; --include-force then takes the run's version.
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
  pisafe vm rebuild [--confirm] [--discard-state]
                   Replace the VM with one built from the current definition.
                   This is the cure for every drift the boundary checks report,
                   and it takes no work with it: every run's files, every
                   project's transcripts, and the profile sit on a disk owned by
                   Lima rather than by the instance, which the new VM mounts
                   back. Active runs are stopped first, so each is charged only
                   for the time its container recorded, and resume as they were.
                   Named no flag, it reports what the rebuild would cost and
                   changes nothing. A VM predating that disk holds all of it on
                   the disk being deleted, and only then does --discard-state
                   apply: back up first, because nothing else saves it.
  pisafe doctor    Check Phase 1 host prerequisites
  pisafe list      Show every run's record against what the VM has. A run
                   recorded active with no container is named as one, because
                   the record alone cannot tell a spent budget from a VM that
                   went down.
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

func runList(ctx context.Context, out io.Writer) error {
	runs, unreadable, err := recordedRuns()
	if err != nil {
		return err
	}
	if len(runs) == 0 && len(unreadable) == 0 {
		fmt.Fprintln(out, "No runs.")
		return nil
	}
	running, asked := runningRuns(ctx)
	return printRuns(out, runs, unreadable, running, asked)
}

// printRuns renders the durable records against what the VM shows. A record
// this version cannot read is listed too: it still holds a workspace, and
// naming it is what lets it be discarded.
func printRuns(
	out io.Writer,
	runs []runstate.Manifest,
	unreadable []runstate.UnreadableRun,
	running map[string]bool,
	asked bool,
) error {
	table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "RUN\tSTATE\tPROJECT\tUPDATED")
	for _, run := range runs {
		fmt.Fprintf(
			table,
			"%s\t%s\t%s\t%s\n",
			run.RunID,
			listedState(run, running, asked),
			run.Project,
			run.UpdatedAt.UTC().Format("2006-01-02 15:04Z"),
		)
	}
	for _, record := range unreadable {
		fmt.Fprintf(table, "%s\tunreadable\t-\t-\n", record.RunID)
	}
	if err := table.Flush(); err != nil {
		return fmt.Errorf("write run list: %w", err)
	}
	for _, record := range unreadable {
		fmt.Fprintf(
			out,
			"%s: %s.\nIt still holds a workspace; pisafe discard %s releases it.\n",
			record.RunID,
			record.Reason,
			record.RunID,
		)
	}
	if !asked {
		fmt.Fprintln(
			out,
			"The VM could not be asked which runs still have a container, "+
				"so an active one may no longer be running.",
		)
	}
	return nil
}

// listedState renders a run's record against what the VM shows. Only the VM
// settles what a record calling a run active means: a container gone with a
// rebooted VM leaves the claim standing, and a deadline that passed then
// measures an outage rather than a budget spent.
func listedState(run runstate.Manifest, running map[string]bool, asked bool) string {
	state := string(run.State)
	if run.State == runstate.StateActive && asked {
		switch {
		case !running[run.RunID]:
			state += " (no container)"
		case runstate.RemainingSeconds(run, time.Now()) == 0:
			state += " (limit reached)"
		}
	}
	if run.LastError != "" {
		state += " (error)"
	}
	return state
}

// runningRuns names every run the VM still has a container for, and reports
// whether it could be asked at all. A VM that is stopped or absent holds no
// container, so that is an answer rather than a failure; a VM that could not be
// reached, or one in a state Lima cannot classify, may hold any of them, and
// nothing acts on a guess about that.
func runningRuns(ctx context.Context) (map[string]bool, bool) {
	status, err := lima.New().Status(ctx)
	if err != nil {
		return nil, false
	}
	if status == lima.StatusStopped || status == lima.StatusAbsent {
		return map[string]bool{}, true
	}
	if status != lima.StatusRunning {
		return nil, false
	}
	running, err := lima.New().RunningRuns(ctx)
	if err != nil {
		return nil, false
	}
	return running, true
}

func recordedRuns() ([]runstate.Manifest, []runstate.UnreadableRun, error) {
	store, err := runStore()
	if err != nil {
		return nil, nil, err
	}
	return store.List()
}

func runRecord(runID string) (runstate.Manifest, error) {
	store, err := runStore()
	if err != nil {
		return runstate.Manifest{}, err
	}
	return store.Get(runID)
}

func runStore() (runstate.Store, error) {
	root, err := runstate.DefaultRoot()
	if err != nil {
		return runstate.Store{}, err
	}
	return runstate.NewStore(root), nil
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
	// A record this version cannot read is not a candidate: every command that
	// resolves a run then reads it, so naming one could only fail later.
	// Discarding is the exception and always names its run outright.
	runs, _, err := recordedRuns()
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

// confirmedTarget reads a command that has to name what it destroys twice. The
// second naming is typed rather than defaulted, because nothing these take away
// comes back and a command line one word short of what was meant must do
// nothing at all. usage is how the command is spelled in full, and noun what it
// calls the thing it takes.
func confirmedTarget(args []string, usage, noun string) (string, error) {
	if len(args) != 4 || args[2] != "--confirm" {
		return "", fmt.Errorf("%s requires exact confirmation: pisafe %s", args[0], usage)
	}
	if args[3] != args[1] {
		return "", fmt.Errorf(
			"%s confirmation does not exactly match %s%q",
			args[0],
			noun,
			args[1],
		)
	}
	return args[1], nil
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

// activeRun returns a run an editor or terminal can reach right now, bringing
// back one the VM stopped rather than reporting that it is gone. Which side
// stopped it decides that: a run the user stopped stays stopped, because
// resuming spends a budget and is theirs to ask for, while a run the VM stopped
// was never anyone's decision — pisafe called it active, and the record is a
// claim to make true rather than to explain away.
func activeRun(ctx context.Context, runID string, out io.Writer) (runstate.Manifest, error) {
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
	if running, asked := runningRuns(ctx); asked && !running[runID] {
		return restoreRun(ctx, runID, out)
	}
	// A container the VM still has past its deadline is a run that spent its
	// budget, which is the only reading of a passed deadline the VM confirms.
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

// restoreRun rebuilds a run the VM stopped and hands it back. It says so rather
// than passing it off as the session that was left: only the run's storage
// survived a VM going down, so the agent that was working in it is not there,
// and the wall clock starts again from here.
func restoreRun(
	ctx context.Context,
	runID string,
	out io.Writer,
) (runstate.Manifest, error) {
	fmt.Fprintf(out, "%s lost its container when the VM went down. Bringing it back...\n", runID)
	manifest, err := resumeRun(ctx, runID)
	if err != nil {
		return runstate.Manifest{}, err
	}
	fmt.Fprintf(
		out,
		"Resumed %s with its workspace as it was and %s of active time.\n",
		runID,
		remainingTime(manifest),
	)
	return manifest, nil
}
