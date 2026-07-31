package runctl

import (
	"context"
	"fmt"

	"github.com/mpizenberg/pisafe/internal/runid"
)

// DropProject throws away one project's whole store: its caches, its
// transcripts, the filesystem holding them, and the record naming the checkout.
// The transcripts are the part nothing can reproduce, which is why this is only
// ever asked for and never inferred — the sweep that removes an abandoned store
// waits a week and this does not wait at all.
func (controller Controller) DropProject(ctx context.Context, project runid.Project) error {
	if err := controller.refuseIfInUse(project.Key); err != nil {
		return err
	}
	// The record is what makes the filesystem attributable, so it is asked
	// rather than the VM: a store nothing names could not be recognised as this
	// project's in the first place.
	stored, err := controller.store.HasProject(project.Key)
	if err != nil {
		return err
	}
	if !stored {
		return fmt.Errorf("no project store is recorded for %q", project.Root)
	}
	if err := controller.releaseProject(ctx, project.Key); err != nil {
		return err
	}
	return nil
}

// RebindProject moves one project's history to the key a different checkout
// path hashes to, which is what lets a repository that was renamed or moved
// keep its transcripts. The caches are deliberately left behind: they are a
// full copy of whatever the project last fetched, and the one thing every
// design decision about them assumes is that losing them costs time only.
func (controller Controller) RebindProject(ctx context.Context, from, to runid.Project) error {
	if from.Key == to.Key {
		return fmt.Errorf("%q and %q are the same checkout", from.Root, to.Root)
	}
	for _, project := range []runid.Project{from, to} {
		if err := controller.refuseIfInUse(project.Key); err != nil {
			return err
		}
	}
	stored, err := controller.store.HasProject(from.Key)
	if err != nil {
		return err
	}
	if !stored {
		return fmt.Errorf("no project store is recorded for %q", from.Root)
	}
	// An interrupted rebind and two real projects look identical from here, and
	// merging two histories silently is the worse of the two mistakes, so the
	// destination having a store of its own always refuses.
	taken, err := controller.store.HasProject(to.Key)
	if err != nil {
		return err
	}
	if taken {
		return fmt.Errorf(
			"%q already has a project store; drop it first if a rebind left it behind",
			to.Root,
		)
	}

	// The record goes in before the filesystem, as it does for a run: a project
	// key is a one-way digest, so a store that exists before anything says where
	// it came from could never afterwards be recognised as unused.
	if err := controller.store.RegisterProject(to); err != nil {
		return err
	}
	if err := controller.backend.EnsureProjectStorage(ctx, to.Key); err != nil {
		return err
	}
	if err := controller.backend.AdoptSessions(ctx, to.Key, from.Key); err != nil {
		return err
	}
	return controller.releaseProject(ctx, from.Key)
}

// releaseProject takes one store away, filesystem before record. A release
// interrupted between the two finds the project again on the next attempt and
// removes a filesystem that is already gone, which costs nothing; the reverse
// order would leave a filesystem nothing could name.
func (controller Controller) releaseProject(ctx context.Context, projectKey string) error {
	if err := controller.backend.RemoveProjectStorage(ctx, projectKey); err != nil {
		return err
	}
	return controller.store.ForgetProject(projectKey)
}

// refuseIfInUse stops a store being disturbed while any recorded run refers to
// it. An active run has its overlays mounted on it now and a stopped one
// remounts them when it resumes, and overlayfs leaves behaviour undefined when
// a mounted lower goes away — so this is the same question eviction and cache
// reset ask, widened from one generation to the whole store.
func (controller Controller) refuseIfInUse(projectKey string) error {
	records, err := controller.store.List()
	if err != nil {
		return err
	}
	for _, record := range records {
		if record.ProjectKey == projectKey {
			return fmt.Errorf(
				"run %q still belongs to this project; discard it first",
				record.RunID,
			)
		}
	}
	return nil
}
