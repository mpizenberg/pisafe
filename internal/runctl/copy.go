package runctl

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"

	"github.com/mpizenberg/pisafe/internal/runcopy"
	"github.com/mpizenberg/pisafe/internal/runstate"
)

// copyReportLimit bounds the entry list a run sends back describing what a copy
// wrote. It is generous for MaxFiles entries and far short of anything that
// could exhaust this process.
const copyReportLimit = int64(8) << 20

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
	manifest, err := controller.workspaceRun(ctx, request.RunID, "copy through")
	if err != nil {
		return nil, err
	}
	spec := specForManifest(manifest, request.ImageID)
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

// CopyIntoRequest names one path on the Mac and where it lands in a run. An
// empty Destination is the workspace itself, and the copy keeps the name it had
// here. Replace allows an existing destination to be replaced, which is
// otherwise refused.
type CopyIntoRequest struct {
	RunID       string
	ImageID     string
	Source      string
	Destination string
	Replace     bool
}

// CopyIn puts one file or directory from the Mac into a run's workspace. The
// archive is produced here rather than in the run, so what arrives is bounded
// by the same limits as anything the Mac would have accepted coming the other
// way, and the run is given no say in what it receives.
func (controller Controller) CopyIn(
	ctx context.Context,
	request CopyIntoRequest,
) ([]runcopy.Entry, error) {
	source := filepath.Clean(request.Source)
	name := filepath.Base(source)
	if name == "." || name == ".." || name == string(filepath.Separator) {
		return nil, fmt.Errorf("%q does not name something to copy", request.Source)
	}
	if _, err := os.Stat(source); err != nil {
		return nil, fmt.Errorf("copy into run: %w", err)
	}
	// A copy that cannot land is refused before the run is asked to receive
	// anything, so its reason is the one the user sees.
	if request.Destination != "" {
		if _, err := runcopy.SafePath(request.Destination); err != nil {
			return nil, err
		}
	}
	manifest, err := controller.workspaceRun(ctx, request.RunID, "copy through")
	if err != nil {
		return nil, err
	}
	spec := specForManifest(manifest, request.ImageID)
	args, err := spec.ImportArgs(manifest.Project, request.Destination, name, request.Replace)
	if err != nil {
		return nil, err
	}

	reader, writer := io.Pipe()
	archived := make(chan error, 1)
	go func() {
		err := runcopy.Archive(filepath.Dir(source), name, writer)
		writer.CloseWithError(err)
		archived <- err
	}()
	output, execErr := controller.backend.Execute(
		ctx,
		reader,
		append([]string{"podman"}, args...)...,
	)
	// Closing the read half unblocks the archive when the run stops reading, so
	// a refused copy never leaves this process writing into nothing.
	reader.CloseWithError(execErr)
	// An archive this Mac could not produce explains itself; whatever the run
	// then made of the truncated stream is only the consequence.
	if archiveErr := <-archived; archiveErr != nil {
		return nil, archiveErr
	}
	if execErr != nil {
		return nil, fmt.Errorf("copy into run: %w", execErr)
	}
	var entries []runcopy.Entry
	decoder := json.NewDecoder(io.LimitReader(bytes.NewReader(output), copyReportLimit))
	if err := decoder.Decode(&entries); err != nil {
		return nil, fmt.Errorf("read what the copy wrote: %w", err)
	}
	return entries, nil
}

// workspaceRun resolves a run whose workspace still exists to reach through.
// purpose is what the caller would do with it, so a run in the wrong state is
// refused in the terms the user asked in.
func (controller Controller) workspaceRun(
	ctx context.Context,
	runID string,
	purpose string,
) (runstate.Manifest, error) {
	manifest, err := controller.store.Get(runID)
	if err != nil {
		return runstate.Manifest{}, err
	}
	switch manifest.State {
	case runstate.StateActive, runstate.StateStopped, runstate.StateImported:
	default:
		return runstate.Manifest{}, fmt.Errorf(
			"run %q is %s and has no workspace to %s",
			runID,
			manifest.State,
			purpose,
		)
	}
	// A run's storage is mounted per VM boot, not per run, so a VM that
	// restarted since the run started presents an empty run root.
	if err := controller.backend.VerifyRunStorage(ctx, manifest.RunID); err != nil {
		return runstate.Manifest{}, err
	}
	return manifest, nil
}
