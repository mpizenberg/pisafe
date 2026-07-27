package runctl

import (
	"context"
	"fmt"
	"io"
	"path"

	"github.com/mpizenberg/pisafe/internal/runcontainer"
	"github.com/mpizenberg/pisafe/internal/runcopy"
	"github.com/mpizenberg/pisafe/internal/runstate"
)

// CopyRequest names one path to take out of a run and where it should land on
// the Mac. Replace allows an existing destination to be replaced, which is
// otherwise refused.
type CopyRequest struct {
	RunID       string
	ImageID     string
	Path        string
	Destination string
	Replace     bool
}

// CopyOut brings one file or directory out of a run. The archive streams
// straight from a throwaway container holding the workspace read-only, so
// nothing is written inside the run and an active run is not disturbed.
func (controller Controller) CopyOut(
	ctx context.Context,
	request CopyRequest,
) ([]runcopy.Entry, error) {
	requested, err := runcopy.SafePath(request.Path)
	if err != nil {
		return nil, err
	}
	// A copy that cannot land is refused before the run is asked to produce
	// anything, so its reason is the one the user sees.
	if err := runcopy.CheckDestination(request.Destination, request.Replace); err != nil {
		return nil, err
	}
	manifest, err := controller.store.Get(request.RunID)
	if err != nil {
		return nil, err
	}
	switch manifest.State {
	case runstate.StateActive, runstate.StateStopped, runstate.StateImported:
	default:
		return nil, fmt.Errorf(
			"run %q is %s and has no workspace to copy from",
			request.RunID,
			manifest.State,
		)
	}
	// A run's storage is mounted per VM boot, not per run, so a VM that
	// restarted since the run started presents an empty run root.
	if err := controller.backend.VerifyStorage(ctx, manifest.RunID); err != nil {
		return nil, err
	}
	spec := runcontainer.DefaultSpec(manifest.RunID, request.ImageID)
	spec.WallSeconds = manifest.ActiveLimitSeconds
	args, err := spec.ExportArgs(manifest.Project, requested)
	if err != nil {
		return nil, err
	}

	reader, writer := io.Pipe()
	streamed := make(chan error, 1)
	go func() {
		err := controller.backend.StreamExecute(
			ctx,
			writer,
			append([]string{"podman"}, args...)...,
		)
		writer.CloseWithError(err)
		streamed <- err
	}()
	entries, extractErr := runcopy.CopyTo(
		reader,
		path.Base(requested),
		request.Destination,
		request.Replace,
	)
	// Closing the read half unblocks the run when extraction stops early, so
	// the copy never leaves a container writing into nothing.
	reader.CloseWithError(extractErr)
	// A run that could not produce the copy explains why; whatever extraction
	// then made of the truncated stream is only the consequence.
	if streamErr := <-streamed; streamErr != nil {
		return nil, fmt.Errorf("copy out of run: %w", streamErr)
	}
	if extractErr != nil {
		return nil, extractErr
	}
	return entries, nil
}
