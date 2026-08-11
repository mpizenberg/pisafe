package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/mpizenberg/pisafe/internal/gitstage"
	"github.com/mpizenberg/pisafe/internal/runstate"
)

// applyRequest is one parsed `pisafe apply RUN [--keep-baseline|--drop-baseline]
// [--include-force]`.
type applyRequest struct {
	runID    string
	baseline gitstage.BaselineChoice
	force    bool
}

var errApplyUsage = fmt.Errorf(
	"usage: pisafe apply [RUN] [--keep-baseline|--drop-baseline] [--include-force]",
)

func parseApplyRequest(args []string) (applyRequest, error) {
	request := applyRequest{}
	positional := []string{}
	for _, argument := range args {
		switch argument {
		case "--keep-baseline":
			request.baseline = gitstage.KeepBaseline
		case "--drop-baseline":
			request.baseline = gitstage.DropBaseline
		case "--include-force":
			request.force = true
		default:
			if strings.HasPrefix(argument, "-") {
				return applyRequest{}, fmt.Errorf(
					"unknown apply option %q\n%w",
					argument,
					errApplyUsage,
				)
			}
			positional = append(positional, argument)
		}
	}
	if len(positional) > 1 {
		return applyRequest{}, errApplyUsage
	}
	if len(positional) == 1 {
		request.runID = positional[0]
	}
	return request, nil
}

// decideBaseline settles what apply does with the commit that carried the
// user's uncommitted work into the run. A run is imported once, so an
// unanswered question here is one nobody gets to revisit.
func decideBaseline(
	manifest runstate.Manifest,
	requested gitstage.BaselineChoice,
	in io.Reader,
	out io.Writer,
) (gitstage.BaselineChoice, error) {
	snapshot := manifest.Snapshot
	if manifest.Apply != nil {
		if requested != "" {
			return "", fmt.Errorf(
				"run %q already has a verified import plan; rerun pisafe apply %s "+
					"with no baseline option to finish it",
				manifest.RunID,
				manifest.RunID,
			)
		}
		return gitstage.KeepBaseline, nil
	}
	if snapshot.BaselineCommit == "" {
		if requested == gitstage.DropBaseline {
			return "", fmt.Errorf(
				"run %q started from a clean checkout and has no baseline commit",
				manifest.RunID,
			)
		}
		return gitstage.KeepBaseline, nil
	}
	if blocked := snapshot.SubmoduleBaselines(); len(blocked) != 0 {
		if requested == gitstage.DropBaseline {
			return "", fmt.Errorf(
				"the baseline cannot be left out: %d submodule(s) carried uncommitted work too, "+
					"and the run's commits record where those submodules stood",
				len(blocked),
			)
		}
		fmt.Fprintf(
			out,
			"Baseline:  kept; %d submodule(s) carried uncommitted work too, so the run's\n"+
				"           commits depend on it\n",
			len(blocked),
		)
		return gitstage.KeepBaseline, nil
	}
	if requested != "" {
		return requested, nil
	}
	return promptBaseline(manifest, in, out)
}

func promptBaseline(
	manifest runstate.Manifest,
	in io.Reader,
	out io.Writer,
) (gitstage.BaselineChoice, error) {
	fmt.Fprintf(
		out,
		"Run %s began with work you had not committed, which pisafe committed for it as\n"+
			"  %s pisafe: imported working-tree baseline\n"+
			"and the run's own commits sit on top of it.\n\n"+
			"  keep  import the whole history, that commit first\n"+
			"  drop  replay only the run's own commits onto %s\n\n",
		manifest.RunID,
		shortCommit(manifest.Snapshot.BaselineCommit),
		shortCommit(manifest.Snapshot.SourceHead),
	)
	reader := bufio.NewReader(in)
	for {
		fmt.Fprint(out, "Baseline commit [keep/drop]: ")
		line, readErr := reader.ReadString('\n')
		if answer, err := gitstage.ParseBaselineChoice(strings.TrimSpace(line)); err == nil {
			return answer, nil
		}
		if readErr != nil {
			return "", errors.New(
				"apply needs a decision about the baseline commit; answer keep or drop, " +
					"or rerun with --keep-baseline or --drop-baseline",
			)
		}
		fmt.Fprintln(out, `Answer "keep" or "drop".`)
	}
}

func runApply(ctx context.Context, args []string, in io.Reader, out io.Writer) error {
	request, err := parseApplyRequest(args)
	if err != nil {
		return err
	}
	runID, err := resolveRunID(ctx, request.runID)
	if err != nil {
		return err
	}
	record, err := runRecord(runID)
	if err != nil {
		return err
	}
	baseline, err := decideBaseline(record, request.baseline, in, out)
	if err != nil {
		return err
	}
	controller, imageID, err := prepareInspection(ctx)
	if err != nil {
		return err
	}
	manifest, result, err := controller.Apply(ctx, runID, imageID, baseline, request.force)
	if err != nil {
		conflict := &gitstage.BaselineReplayConflict{}
		if errors.As(err, &conflict) {
			printReplayConflict(out, runID, conflict)
			return errors.New("the baseline commit cannot be left out of this run's history")
		}
		included := &gitstage.IncludedConflict{}
		if errors.As(err, &included) {
			printIncludedConflict(out, runID, included)
			return errors.New("the included work was not copied back")
		}
		if errors.Is(err, gitstage.ErrApplyNeedsReconciliation) {
			return fmt.Errorf(
				"%w\nThe recorded plan is kept: rerun pisafe apply %s once the ref is resolved",
				err,
				runID,
			)
		}
		return err
	}
	printApplyResult(out, manifest, result)
	return nil
}

// printReplayConflict reports a replay the run could not finish. The paths come
// from the run, so they are quoted, and the run is untouched: every way forward
// is still open.
func printReplayConflict(out io.Writer, runID string, conflict *gitstage.BaselineReplayConflict) {
	fmt.Fprintf(
		out,
		"The run's own commits change lines the carried-in work also changed, "+
			"in %d file(s):\n",
		len(conflict.Paths),
	)
	printNames(out, namedList{names: conflict.Paths})
	fmt.Fprintf(
		out,
		"Nothing was imported and %s still holds everything it did. You can:\n"+
			"  pisafe apply %s --keep-baseline   import the whole history instead\n"+
			"  pisafe resume %s                  then resolve it in the run and apply again\n",
		runID,
		runID,
		runID,
	)
}

// printIncludedConflict reports a copy-back that was refused. The branch is
// imported by now, so what is left is only the files, and the run still holds
// its own copy of every one of them.
func printIncludedConflict(out io.Writer, runID string, conflict *gitstage.IncludedConflict) {
	fmt.Fprintf(
		out,
		"The branch was imported, but %d included path(s) changed both in the run\n"+
			"and here while it ran:\n",
		len(conflict.Paths),
	)
	printNames(out, namedList{names: conflict.Paths})
	fmt.Fprintf(
		out,
		"Nothing was copied back, and %s still holds its own copy. You can:\n"+
			"  pisafe apply %s --include-force   overwrite this Mac's copies\n"+
			"  resolve them here first, then rerun the same command\n",
		runID,
		runID,
	)
}

func printApplyResult(out io.Writer, manifest runstate.Manifest, result gitstage.ApplyResult) {
	fmt.Fprintf(out, "Imported:  %s\nTip:       %s\n", result.Branch, result.Tip)
	for _, submodule := range result.Submodules {
		if submodule.Branch == "" {
			fmt.Fprintf(out, "Submodule: %s unchanged\n", submodule.Path)
			continue
		}
		fmt.Fprintf(
			out,
			"Submodule: %s imported as %s (%s)\n",
			submodule.Path,
			submodule.Branch,
			submodule.Tip,
		)
	}
	if result.FinalCommit != "" {
		fmt.Fprintln(out, "Final:     uncommitted tracked changes became one labelled commit")
	}
	if len(result.Untracked) != 0 {
		fmt.Fprintf(
			out,
			"Left:      %d untracked file(s) stayed in the run\n",
			len(result.Untracked),
		)
		printNames(out, namedList{names: result.Untracked})
	}
	if len(result.Included.Written) != 0 {
		fmt.Fprintf(
			out,
			"Included:  %d file(s) copied back under the paths you selected\n",
			len(result.Included.Written),
		)
		printNames(out, namedList{names: result.Included.Written})
	}
	if len(result.Included.Kept) != 0 {
		fmt.Fprintf(
			out,
			"Kept:      %d included file(s) the run removed were left here\n",
			len(result.Included.Kept),
		)
		printNames(out, namedList{names: result.Included.Kept})
	}
	fmt.Fprintf(
		out,
		"Next:      git log %s\n           %s keeps its workspace until pisafe discard %s\n",
		result.Branch,
		manifest.RunID,
		manifest.RunID,
	)
}
