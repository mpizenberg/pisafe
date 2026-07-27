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
	plan, err := controller.Plan(time.Now())
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
	done, collectErr := controller.Collect(ctx, plan)
	images, pruneErr := installer.Prune(ctx, recipe, plan.KeepImages)
	printCollection(out, done, images, false)
	return errors.Join(collectErr, pruneErr)
}

func printCollection(out io.Writer, plan runctl.GCPlan, images []string, dryRun bool) {
	if plan.Empty() && len(images) == 0 && len(plan.Kept) == 0 {
		fmt.Fprintln(out, "Nothing to collect.")
		return
	}
	expired, forgot, pruned := "Expired:", "Forgot:", "Pruned:"
	if dryRun {
		expired, forgot, pruned = "Would expire:", "Would forget:", "Would prune:"
	}
	printCollected(
		out,
		expired,
		fmt.Sprintf(
			"%d imported run(s) done over seven days ago; branch and import record kept",
			len(plan.Expired),
		),
		plan.Expired,
	)
	printCollected(
		out,
		forgot,
		fmt.Sprintf("%d discarded record(s) that attribute no branch", len(plan.Forgotten)),
		plan.Forgotten,
	)
	printCollected(
		out,
		pruned,
		fmt.Sprintf("%d superseded run image(s)", len(images)),
		images,
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

func printCollected(out io.Writer, label, summary string, names []string) {
	if len(names) == 0 {
		return
	}
	fmt.Fprintf(out, "%-15s%s\n", label, summary)
	for _, name := range names {
		fmt.Fprintf(out, "               %s\n", name)
	}
}
