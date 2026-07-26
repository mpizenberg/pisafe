package runctl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/mpizenberg/pisafe/internal/gitstage"
	"github.com/mpizenberg/pisafe/internal/runcontainer"
	"github.com/mpizenberg/pisafe/internal/runstate"
)

// Apply imports one run's result into the source repository. The run is
// stopped first: an imported run is inactive, and a workspace nothing is
// writing to is the only one that can be captured consistently.
//
// The image is the controller's current run image rather than the one the
// manifest records, because the helper that captures the run must match the
// controller that reads what it produced.
func (controller Controller) Apply(
	ctx context.Context,
	runID string,
	imageID string,
) (runstate.Manifest, gitstage.ApplyResult, error) {
	manifest, err := controller.store.Get(runID)
	if err != nil {
		return runstate.Manifest{}, gitstage.ApplyResult{}, err
	}
	switch manifest.State {
	case runstate.StateActive:
		manifest, err = controller.Stop(ctx, runID)
		if err != nil {
			return runstate.Manifest{}, gitstage.ApplyResult{}, err
		}
	case runstate.StateStopped:
	case runstate.StateImported:
		return runstate.Manifest{}, gitstage.ApplyResult{}, fmt.Errorf(
			"run %q is already imported as %s",
			runID,
			manifest.ImportedBranch,
		)
	default:
		return runstate.Manifest{}, gitstage.ApplyResult{}, fmt.Errorf(
			"run %q is %s and cannot be applied",
			runID,
			manifest.State,
		)
	}

	// A recorded plan is always finished rather than redone: its objects are
	// already imported and verified, and every step is compare-and-swap.
	if manifest.Apply != nil {
		return controller.finishApply(ctx, *manifest.Apply)
	}

	planned, err := controller.importRun(ctx, manifest, imageID)
	if err != nil {
		return runstate.Manifest{}, gitstage.ApplyResult{},
			controller.recordLifecycleError(runID, "apply", err)
	}
	if _, err := controller.store.BeginApply(runID, planned); err != nil {
		cleanupContext, cancelCleanup := lifecycleCleanupContext(ctx)
		defer cancelCleanup()
		return runstate.Manifest{}, gitstage.ApplyResult{},
			controller.recordLifecycleError(runID, "record apply", errors.Join(
				err,
				gitstage.RollbackApply(cleanupContext, planned.Journal),
			))
	}
	return controller.finishApply(ctx, planned)
}

// finishApply moves the refs the plan names and marks the run imported. An
// apply that stops here keeps its recorded plan, so running apply again
// resumes exactly where it left off.
func (controller Controller) finishApply(
	ctx context.Context,
	planned gitstage.PlannedApply,
) (runstate.Manifest, gitstage.ApplyResult, error) {
	runID := planned.Journal.RunID
	if err := gitstage.CommitApply(ctx, planned.Journal); err != nil {
		return runstate.Manifest{}, gitstage.ApplyResult{},
			controller.recordLifecycleError(runID, "apply", err)
	}
	manifest, err := controller.store.CompleteApply(runID)
	if err != nil {
		return runstate.Manifest{}, gitstage.ApplyResult{},
			controller.recordLifecycleError(runID, "record apply", err)
	}
	return manifest, planned.Result, nil
}

// importRun captures the run's result, brings its bundles to the Mac, and
// imports every object set into temporary refs. Nothing user-visible changes
// until the returned plan is committed.
func (controller Controller) importRun(
	ctx context.Context,
	manifest runstate.Manifest,
	imageID string,
) (gitstage.PlannedApply, error) {
	spec := runcontainer.DefaultSpec(manifest.RunID, imageID)
	spec.WallSeconds = manifest.ActiveLimitSeconds
	args, err := spec.PrepareApplyArgs(manifest.Project)
	if err != nil {
		return gitstage.PlannedApply{}, err
	}
	request := manifest.Snapshot
	request.SourceRoot = ""
	requestJSON, err := json.Marshal(request)
	if err != nil {
		return gitstage.PlannedApply{}, fmt.Errorf("encode apply request: %w", err)
	}
	output, err := controller.podman(ctx, bytes.NewReader(requestJSON), args...)
	if err != nil {
		return gitstage.PlannedApply{}, fmt.Errorf("capture run result: %w", err)
	}
	prepared, err := decodeGuestResponse[gitstage.PreparedApply](output, "prepared apply")
	if err != nil {
		return gitstage.PlannedApply{}, err
	}
	if prepared.RunID != manifest.RunID {
		return gitstage.PlannedApply{}, errors.New("prepared apply does not match the run")
	}

	packageDir, err := os.MkdirTemp("", "pisafe-apply-*")
	if err != nil {
		return gitstage.PlannedApply{}, fmt.Errorf("reserve apply package path: %w", err)
	}
	defer os.RemoveAll(packageDir)
	for _, artifact := range prepared.Artifacts() {
		if err := controller.backend.FetchApplyArtifact(
			ctx,
			manifest.RunID,
			artifact,
			filepath.Join(packageDir, artifact.Name),
		); err != nil {
			return gitstage.PlannedApply{}, err
		}
	}

	planned, err := gitstage.ImportApply(ctx, manifest.Snapshot, prepared, packageDir)
	if err != nil {
		return gitstage.PlannedApply{}, err
	}
	// The bundles have served their purpose; the run rebuilds them from its
	// workspace if apply ever runs again. Failing here undoes the import, so
	// importRun either returns a plan or leaves nothing behind.
	if err := controller.backend.RemoveApplyPackage(ctx, manifest.RunID); err != nil {
		cleanupContext, cancelCleanup := lifecycleCleanupContext(ctx)
		defer cancelCleanup()
		return gitstage.PlannedApply{},
			errors.Join(err, gitstage.RollbackApply(cleanupContext, planned.Journal))
	}
	return planned, nil
}
