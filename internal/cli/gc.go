package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/mpizenberg/pisafe/internal/lima"
	"github.com/mpizenberg/pisafe/internal/runctl"
	"github.com/mpizenberg/pisafe/internal/runimage"
	"github.com/mpizenberg/pisafe/internal/runstate"
)

var errGCUsage = errors.New("usage: pisafe gc [--dry-run]")

func runGC(ctx context.Context, args []string, out io.Writer) error {
	dryRun := false
	switch {
	case len(args) == 0:
	case len(args) == 1 && args[0] == "--dry-run":
		dryRun = true
	default:
		return errGCUsage
	}
	controller, err := prepareLifecycle(ctx)
	if err != nil {
		return err
	}
	artifacts, err := packagedRunArtifacts()
	if err != nil {
		return err
	}
	installer := runimage.NewInstaller(lima.NewTransport())
	recipe := artifacts.RecipeDigest()

	// Nothing is pruned without a complete set of images runs still need, so
	// a plan that could not be built stops collection entirely.
	now := time.Now()
	plan, err := controller.Plan(now)
	if err != nil {
		return err
	}
	if dryRun {
		images, err := installer.Superseded(ctx, recipe, plan.KeepImages)
		if err != nil {
			return err
		}
		printCollection(out, plan, images, true)
		return nil
	}
	done, collectErr := controller.Collect(ctx, plan, now)
	images, pruneErr := installer.Prune(ctx, recipe, plan.KeepImages)
	printCollection(out, done, images, false)
	return errors.Join(collectErr, pruneErr)
}

func printCollection(out io.Writer, plan runctl.GCPlan, images []string, dryRun bool) {
	if plan.Empty() && len(images) == 0 &&
		len(plan.Kept) == 0 && len(plan.MissingProjects) == 0 {
		fmt.Fprintln(out, "Nothing to collect.")
		return
	}
	reclaimed, pruned := "Reclaimed:", "Pruned:"
	if dryRun {
		reclaimed, pruned = "Would reclaim:", "Would prune:"
	}
	printCollected(
		out,
		reclaimed,
		fmt.Sprintf(
			"%d imported run(s) done over seven days ago; their pisafe/RUN branches keep the work",
			len(plan.Reclaimed),
		),
		plan.Reclaimed,
	)
	printCollected(
		out,
		reclaimed,
		fmt.Sprintf(
			"%d project store(s) whose checkout has been gone for over seven days",
			len(plan.ReclaimedProjects),
		),
		projectLabels(plan.ReclaimedProjects),
	)
	printCollected(
		out,
		pruned,
		fmt.Sprintf("%d superseded run image(s)", len(images)),
		images,
	)
	printCollected(
		out,
		"Missing:",
		fmt.Sprintf(
			"%d project store(s) whose checkout is gone; move one back to keep its transcripts",
			len(plan.MissingProjects),
		),
		projectLabels(plan.MissingProjects),
	)
	if len(plan.Kept) == 0 {
		return
	}
	fmt.Fprintf(
		out,
		"%-15s%d run(s) left alone; age alone never removes unimported work\n",
		"Kept:",
		len(plan.Kept),
	)
	for _, kept := range plan.Kept {
		fmt.Fprintf(out, "               %s (%s)\n", kept.RunID, kept.Reason)
	}
}

// projectLabels names each store by the checkout it is keyed by, which is the
// only part of it a user recognises: the key itself is a digest, and the
// directory it was made from is gone by the time the store is reported.
func projectLabels(projects []runstate.ProjectRecord) []string {
	labels := make([]string, 0, len(projects))
	for _, project := range projects {
		since := "first seen missing"
		if project.MissingSince != nil {
			since = "missing since " + project.MissingSince.Format(time.DateOnly)
		}
		labels = append(labels, fmt.Sprintf("%q (%s)", project.Root, since))
	}
	return labels
}

func printCollected(out io.Writer, label, summary string, names []string) {
	if len(names) == 0 {
		return
	}
	fmt.Fprintf(out, "%-15s%s\n", label, summary)
	for _, name := range names {
		fmt.Fprintf(out, "               %s\n", name)
	}
}
