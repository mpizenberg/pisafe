package runctl

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/mpizenberg/pisafe/internal/gitstage"
)

// Diff reports what a run changed since it began. It neither stops nor resumes
// the run: the report is produced by a throwaway container holding the run's
// workspace read-only, so an agent may be working in it meanwhile.
//
// The image is the controller's current run image for the same reason apply
// uses it: the helper that reads the run must match the controller that reads
// what it produced.
func (controller Controller) Diff(
	ctx context.Context,
	runID string,
	imageID string,
) (gitstage.RunDiff, error) {
	manifest, err := controller.workspaceRun(ctx, runID, "compare")
	if err != nil {
		return gitstage.RunDiff{}, err
	}
	spec := specForManifest(manifest, imageID)
	args, err := spec.DiffArgs(manifest.Project)
	if err != nil {
		return gitstage.RunDiff{}, err
	}
	requestJSON, err := runRequest(manifest)
	if err != nil {
		return gitstage.RunDiff{}, err
	}
	output, err := controller.podman(ctx, bytes.NewReader(requestJSON), args...)
	if err != nil {
		return gitstage.RunDiff{}, fmt.Errorf("compare run workspace: %w", err)
	}
	diff, err := decodeGuestResponse[gitstage.RunDiff](output, "run diff")
	if err != nil {
		return gitstage.RunDiff{}, err
	}
	if diff.RunID != manifest.RunID {
		return gitstage.RunDiff{}, errors.New("run diff does not match the run")
	}
	return diff, nil
}
