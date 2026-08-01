package runctl

import (
	"context"
	"io"

	"github.com/mpizenberg/pisafe/internal/runid"
)

// RestoreProject puts one project's store back and gives it the transcripts a
// backup holds. It is what a recreated VM needs: the filesystems are gone while
// the Mac's records of them may not be, and a run of the checkout would
// otherwise start against a store with no history in it.
//
// The record goes in before the filesystem for the reason it does everywhere: a
// project key is a one-way digest, so a store that exists before anything says
// where it came from could never afterwards be recognised as unused.
func (controller Controller) RestoreProject(
	ctx context.Context,
	project runid.Project,
	transcripts io.Reader,
) error {
	if err := controller.store.RegisterProject(project); err != nil {
		return err
	}
	if err := controller.backend.EnsureProjectStorage(ctx, project.Key); err != nil {
		return err
	}
	return controller.backend.RestoreSessions(ctx, project.Key, transcripts)
}
