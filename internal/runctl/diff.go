package runctl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/mpizenberg/pisafe/internal/gitstage"
	"github.com/mpizenberg/pisafe/internal/runstate"
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
	manifest, err := controller.store.Get(runID)
	if err != nil {
		return gitstage.RunDiff{}, err
	}
	switch manifest.State {
	case runstate.StateActive, runstate.StateStopped, runstate.StateImported:
	default:
		return gitstage.RunDiff{}, fmt.Errorf(
			"run %q is %s and has no workspace to compare",
			runID,
			manifest.State,
		)
	}
	// A run's storage is mounted per VM boot, not per run, so a VM that
	// restarted since the run started presents an empty run root.
	if err := controller.backend.VerifyRunStorage(ctx, manifest.RunID); err != nil {
		return gitstage.RunDiff{}, err
	}
	spec := specForManifest(manifest, imageID)
	args, err := spec.DiffArgs(manifest.Project)
	if err != nil {
		return gitstage.RunDiff{}, err
	}
	request := manifest.Snapshot
	request.SourceRoot = ""
	requestJSON, err := json.Marshal(request)
	if err != nil {
		return gitstage.RunDiff{}, fmt.Errorf("encode diff request: %w", err)
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
