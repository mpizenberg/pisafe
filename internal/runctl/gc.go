package runctl

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"time"

	"github.com/mpizenberg/pisafe/internal/runstate"
)

// Retention is how long something pisafe has finished with is kept. For a run
// it starts when the run was imported, not when it was created, so a run
// resumed for a month is never close to being reclaimed. For a project store it
// starts when a sweep first found the checkout gone.
const Retention = 7 * 24 * time.Hour

// GCPlan is what collection would do, and afterwards what it did.
type GCPlan struct {
	// Reclaimed are imported runs the retention window released, workspace
	// and record together. Their commits stay on the pisafe/<run> branch.
	Reclaimed []string
	// Kept are runs collection deliberately leaves alone, each with the
	// reason a user needs in order to act on it.
	Kept []KeptRun
	// KeepImages are the images runs can still start containers from. Every
	// other managed image is superseded.
	KeepImages []string
	// MissingProjects are project stores whose checkout is gone but whose
	// window has not run out. Collection stamps them and reports them; it is
	// the report that gives a user the chance to say the disappearance was not
	// what it looked like.
	MissingProjects []runstate.ProjectRecord
	// ReclaimedProjects are stores whose checkout stayed gone for a whole
	// window: cache, session store, and filesystem together.
	ReclaimedProjects []runstate.ProjectRecord
	// UnreadableProjects are project records this version cannot read.
	// Collection reports them and acts on none of them: whether the checkout is
	// gone is exactly what could not be read. They hold up nothing else, because
	// a project record's key is the name of its own file — so unlike a run
	// record that cannot be read, one of these leaves no other store in doubt.
	UnreadableProjects []runstate.UnreadableProject
}

func (plan GCPlan) Empty() bool {
	return len(plan.Reclaimed) == 0 && len(plan.ReclaimedProjects) == 0
}

type KeptRun struct {
	RunID  string
	Reason string
}

// Plan decides what collection would do from the durable records alone. It
// reads nothing from the VM, so it is safe to show before anything is removed.
func (controller Controller) Plan(now time.Time) (GCPlan, error) {
	manifests, unreadable, err := controller.store.List()
	if err != nil {
		return GCPlan{}, err
	}
	now = now.UTC()
	var plan GCPlan
	// A record this version cannot read is reported so it can be discarded, and
	// never acted on: its age, its state, and the project it belongs to are all
	// exactly what could not be read.
	for _, record := range unreadable {
		plan.Kept = append(plan.Kept, KeptRun{
			RunID:  record.RunID,
			Reason: "its record cannot be read by this version",
		})
	}
	// A project whose runs still have records is in use whatever its checkout
	// looks like, including the runs this same plan reclaims: a store is worth
	// removing only once nothing at all refers to it, and the next sweep sees it
	// as soon as that is true.
	inUse := make(map[string]struct{}, len(manifests))
	for _, manifest := range manifests {
		inUse[manifest.ProjectKey] = struct{}{}
		switch manifest.State {
		case runstate.StateImported:
			// An imported run cannot resume, so it pins no image; the commands
			// that still read its workspace run the current one instead.
			if manifest.ImportedAt != nil && now.Sub(*manifest.ImportedAt) >= Retention {
				plan.Reclaimed = append(plan.Reclaimed, manifest.RunID)
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
	projects, unreadableProjects, err := controller.store.ListProjects()
	if err != nil {
		return GCPlan{}, err
	}
	plan.UnreadableProjects = unreadableProjects
	for _, project := range projects {
		if _, busy := inUse[project.Key]; busy || !checkoutIsGone(project.Root) {
			continue
		}
		// An unreadable record names no project, so every store has to be
		// treated as the one it refers to until it is discarded.
		if len(unreadable) > 0 {
			plan.MissingProjects = append(plan.MissingProjects, project)
			continue
		}
		if project.MissingSince != nil && now.Sub(*project.MissingSince) >= Retention {
			plan.ReclaimedProjects = append(plan.ReclaimedProjects, project)
			continue
		}
		plan.MissingProjects = append(plan.MissingProjects, project)
	}
	return plan, nil
}

// checkoutIsGone reports whether the checkout a project is keyed by is
// definitely no longer there. Only the filesystem denying its existence counts:
// an unreadable parent or an answer that never came leaves the store alone,
// and whether the path is still a Git repository is deliberately not asked,
// because git failing to run cannot be told apart from git saying no and would
// orphan every project at once.
func checkoutIsGone(root string) bool {
	_, err := os.Stat(root)
	return errors.Is(err, fs.ErrNotExist)
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

// Collect carries out a plan. Runs and projects are independent, so one that
// cannot be reclaimed does not stop the rest; the returned plan holds what was
// done.
func (controller Controller) Collect(
	ctx context.Context,
	plan GCPlan,
	now time.Time,
) (GCPlan, error) {
	done := GCPlan{
		Kept:               plan.Kept,
		KeepImages:         plan.KeepImages,
		UnreadableProjects: plan.UnreadableProjects,
	}
	var failures []error
	for _, runID := range plan.Reclaimed {
		if err := controller.release(ctx, runID, "collect"); err != nil {
			failures = append(failures, err)
			continue
		}
		done.Reclaimed = append(done.Reclaimed, runID)
	}
	for _, project := range plan.ReclaimedProjects {
		if err := controller.releaseProject(ctx, project.Key); err != nil {
			failures = append(failures, err)
			continue
		}
		done.ReclaimedProjects = append(done.ReclaimedProjects, project)
	}
	for _, project := range plan.MissingProjects {
		if project.MissingSince == nil {
			if err := controller.store.MarkProjectMissing(project.Key, now); err != nil {
				failures = append(failures, err)
				continue
			}
			stamped := now.UTC()
			project.MissingSince = &stamped
		}
		done.MissingProjects = append(done.MissingProjects, project)
	}
	return done, errors.Join(failures...)
}
