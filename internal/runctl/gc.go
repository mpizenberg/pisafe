package runctl

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mpizenberg/pisafe/internal/runstate"
)

// Retention is how long a finished run is kept. It starts when the run
// finished, not when it was created, so a run resumed for a month is never
// close to expiry.
const Retention = 7 * 24 * time.Hour

// GCPlan is what collection would do, and afterwards what it did.
type GCPlan struct {
	// Expired are imported runs whose workspace the retention window
	// released. Their branch and import record survive.
	Expired []string
	// Forgotten are discarded runs whose record no longer attributes
	// anything and is removed outright.
	Forgotten []string
	// Kept are runs collection deliberately leaves alone, each with the
	// reason a user needs in order to act on it.
	Kept []KeptRun
	// KeepImages are the images runs can still start containers from. Every
	// other managed image is superseded.
	KeepImages []string
}

func (plan GCPlan) Empty() bool {
	return len(plan.Expired) == 0 && len(plan.Forgotten) == 0
}

type KeptRun struct {
	RunID  string
	Reason string
}

// Plan decides what collection would do from the durable records alone. It
// reads nothing from the VM, so it is safe to show before anything is removed.
func (controller Controller) Plan(now time.Time) (GCPlan, error) {
	manifests, err := controller.store.List()
	if err != nil {
		return GCPlan{}, err
	}
	now = now.UTC()
	var plan GCPlan
	for _, manifest := range manifests {
		switch manifest.State {
		case runstate.StateImported:
			if manifest.ImportedAt != nil && now.Sub(*manifest.ImportedAt) >= Retention {
				plan.Expired = append(plan.Expired, manifest.RunID)
			}
		case runstate.StateDiscarded:
			if manifest.ImportedBranch == "" &&
				manifest.DiscardedAt != nil &&
				now.Sub(*manifest.DiscardedAt) >= Retention {
				plan.Forgotten = append(plan.Forgotten, manifest.RunID)
			}
		case runstate.StateCreating, runstate.StateActive, runstate.StateStopped:
			// A run whose work was never imported is never removed by age.
			// It keeps its image, because it can still start a container.
			plan.KeepImages = append(plan.KeepImages, manifest.Image)
			if now.Sub(manifest.UpdatedAt) >= Retention {
				plan.Kept = append(plan.Kept, KeptRun{
					RunID:  manifest.RunID,
					Reason: keptReason(manifest.State),
				})
			}
		}
	}
	return plan, nil
}

func keptReason(state runstate.State) string {
	switch state {
	case runstate.StateCreating:
		return "creation never finished"
	case runstate.StateActive:
		return "still active"
	}
	return "stopped with work that was never imported"
}

// Collect carries out a plan. Runs are independent, so one that cannot be
// reclaimed does not stop the rest; the returned plan holds what was done.
func (controller Controller) Collect(ctx context.Context, plan GCPlan) (GCPlan, error) {
	done := GCPlan{Kept: plan.Kept, KeepImages: plan.KeepImages}
	var failures []error
	for _, runID := range plan.Expired {
		manifest, err := controller.store.Get(runID)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if err := controller.reclaim(ctx, manifest); err != nil {
			failures = append(failures, controller.recordLifecycleError(runID, "expire", err))
			continue
		}
		if _, err := controller.store.Expire(runID); err != nil {
			failures = append(
				failures,
				controller.recordLifecycleError(runID, "record expiry", err),
			)
			continue
		}
		done.Expired = append(done.Expired, runID)
	}
	for _, runID := range plan.Forgotten {
		if err := controller.store.Forget(runID); err != nil {
			failures = append(failures, fmt.Errorf("forget run %q: %w", runID, err))
			continue
		}
		done.Forgotten = append(done.Forgotten, runID)
	}
	return done, errors.Join(failures...)
}
