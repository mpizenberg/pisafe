package runctl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/mpizenberg/pisafe/internal/gitstage"
	"github.com/mpizenberg/pisafe/internal/guestcall"
	"github.com/mpizenberg/pisafe/internal/runstate"
	"github.com/mpizenberg/pisafe/internal/safefile"
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
	baseline gitstage.BaselineChoice,
	force bool,
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
		// The history is already in. A run whose included work was held back
		// still has something left to do, and it is all that is left.
		if manifest.Included == nil {
			return runstate.Manifest{}, gitstage.ApplyResult{}, fmt.Errorf(
				"run %q is already imported as %s",
				runID,
				manifest.ImportedBranch,
			)
		}
		result := gitstage.ApplyResult{Branch: manifest.ImportedBranch}
		manifest, result.Included, err = controller.writeIncluded(manifest, force)
		return manifest, result, err
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
		return controller.finishApply(ctx, *manifest.Apply, force)
	}

	planned, err := controller.importRun(ctx, manifest, imageID, baseline)
	if err != nil {
		// A replay the run could not complete left it exactly as it was, so it
		// is an answer to the question apply asked, not a failed apply.
		conflict := &gitstage.BaselineReplayConflict{}
		if errors.As(err, &conflict) {
			return runstate.Manifest{}, gitstage.ApplyResult{}, err
		}
		return runstate.Manifest{}, gitstage.ApplyResult{},
			controller.recordLifecycleError(runID, "apply", err)
	}
	if _, err := controller.store.BeginApply(runID, planned, planned.Outputs); err != nil {
		cleanupContext, cancelCleanup := lifecycleCleanupContext(ctx)
		defer cancelCleanup()
		return runstate.Manifest{}, gitstage.ApplyResult{},
			controller.recordLifecycleError(runID, "record apply", errors.Join(
				err,
				gitstage.RollbackApply(cleanupContext, planned.Journal),
			))
	}
	return controller.finishApply(ctx, planned, force)
}

// finishApply moves the refs the plan names, marks the run imported, and only
// then copies the run's included work into the working tree. An apply that
// stops before the refs move keeps its recorded plan; one that stops on a
// copy-back keeps the work itself, so either way running apply again resumes
// exactly where it left off.
func (controller Controller) finishApply(
	ctx context.Context,
	planned gitstage.PlannedApply,
	force bool,
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
	result := planned.Result
	manifest, result.Included, err = controller.writeIncluded(manifest, force)
	return manifest, result, err
}

// keepIncludedArchive moves the fetched outputs archive out of the transfer
// directory and into the run's own state, where it waits for the copy.
func (controller Controller) keepIncludedArchive(runID, packageDir string) error {
	target, err := controller.store.IncludedArchivePath(runID)
	if err != nil {
		return err
	}
	content, err := os.ReadFile(filepath.Join(packageDir, gitstage.OutputsArtifactName))
	if err != nil {
		return fmt.Errorf("read returned work: %w", err)
	}
	if err := safefile.Replace(target, content, 0o600); err != nil {
		return fmt.Errorf("keep returned work: %w", err)
	}
	return nil
}

// writeIncluded copies a run's returned work into the source working tree and
// forgets it once it is there. A refusal leaves both the archive and the record
// in place: the branch is imported either way, and the work is not lost.
func (controller Controller) writeIncluded(
	manifest runstate.Manifest,
	force bool,
) (runstate.Manifest, gitstage.IncludedResult, error) {
	if manifest.Included == nil {
		return manifest, gitstage.IncludedResult{}, nil
	}
	archive, err := controller.store.IncludedArchivePath(manifest.RunID)
	if err != nil {
		return manifest, gitstage.IncludedResult{}, err
	}
	result, err := gitstage.CopyBack(manifest.Snapshot, manifest.Included, archive, force)
	if err != nil {
		return manifest, result, err
	}
	manifest, err = controller.store.ClearIncluded(manifest.RunID)
	if err != nil {
		return manifest, result, controller.recordLifecycleError(
			manifest.RunID,
			"record included work",
			err,
		)
	}
	return manifest, result, nil
}

// importRun captures the run's result, brings its bundles to the Mac, and
// imports every object set into temporary refs. Nothing user-visible changes
// until the returned plan is committed.
func (controller Controller) importRun(
	ctx context.Context,
	manifest runstate.Manifest,
	imageID string,
	baseline gitstage.BaselineChoice,
) (gitstage.PlannedApply, error) {
	// A run's storage is mounted per VM boot, not per run, so a VM that
	// restarted between the run and its apply presents an empty run root.
	if err := controller.backend.VerifyRunStorage(ctx, manifest.RunID); err != nil {
		return gitstage.PlannedApply{}, err
	}
	spec := specForManifest(manifest, imageID)
	args, err := spec.PrepareApplyArgs(manifest.Project, baseline)
	if err != nil {
		return gitstage.PlannedApply{}, err
	}
	requestJSON, err := runRequest(manifest)
	if err != nil {
		return gitstage.PlannedApply{}, err
	}
	output, err := controller.podman(ctx, bytes.NewReader(requestJSON), args...)
	if err != nil {
		return gitstage.PlannedApply{}, fmt.Errorf("capture run result: %w", err)
	}
	prepared, err := guestcall.Decode[gitstage.PreparedApply](bytes.NewReader(output), "prepared apply")
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

	planned, err := gitstage.ImportApply(ctx, manifest.Snapshot, prepared, packageDir, baseline)
	if err != nil {
		return gitstage.PlannedApply{}, err
	}
	// The returned work outlives this package: it is written after the refs
	// move, and a refusal must be completable when the VM is long gone.
	if len(planned.Outputs) != 0 {
		if err := controller.keepIncludedArchive(manifest.RunID, packageDir); err != nil {
			cleanupContext, cancelCleanup := lifecycleCleanupContext(ctx)
			defer cancelCleanup()
			return gitstage.PlannedApply{},
				errors.Join(err, gitstage.RollbackApply(cleanupContext, planned.Journal))
		}
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
