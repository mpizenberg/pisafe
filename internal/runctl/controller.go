// Package runctl composes host staging, SSH transport, rootless Podman, and
// the durable Mac-side lifecycle record.
package runctl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/mpizenberg/pisafe/internal/gitstage"
	"github.com/mpizenberg/pisafe/internal/runcontainer"
	"github.com/mpizenberg/pisafe/internal/runstate"
)

type Backend interface {
	CreateStage(context.Context, gitstage.PreparedStage) (string, error)
	ImportStage(context.Context, string, string) error
	Execute(context.Context, io.Reader, ...string) ([]byte, error)
	RemoveRun(context.Context, string) error
}

type StateStore interface {
	Create(runstate.Manifest) (runstate.Manifest, error)
	Transition(string, runstate.State) (runstate.Manifest, error)
	RecordError(string, error) (runstate.Manifest, error)
}

type Controller struct {
	backend Backend
	store   StateStore
}

func New(backend Backend, store StateStore) Controller {
	return Controller{backend: backend, store: store}
}

// StartPrepared creates a run from host-prepared artifacts. The image must
// already exist in rootless Podman and is addressed only by immutable ID.
func (controller Controller) StartPrepared(
	ctx context.Context,
	prepared gitstage.PreparedStage,
	projectDirectory string,
	imageID string,
) (_ runstate.Manifest, returnErr error) {
	spec := runcontainer.DefaultSpec(prepared.Snapshot.RunID, imageID)
	if err := spec.Validate(); err != nil {
		return runstate.Manifest{}, err
	}
	if _, err := spec.MaterializeArgs(projectDirectory); err != nil {
		return runstate.Manifest{}, err
	}

	manifest, err := controller.store.Create(runstate.Manifest{
		RunID:     prepared.Snapshot.RunID,
		Project:   projectDirectory,
		Snapshot:  prepared.Snapshot,
		Image:     imageID,
		Container: spec.ContainerName(),
		Workspace: "/work/" + projectDirectory,
	})
	if err != nil {
		return runstate.Manifest{}, err
	}

	remoteAllocated := false
	var createdVolumes []string
	containerAllocated := false
	defer func() {
		if returnErr == nil {
			return
		}
		cleanupContext, cancelCleanup := context.WithTimeout(
			context.WithoutCancel(ctx),
			30*time.Second,
		)
		defer cancelCleanup()
		cleanupErr := controller.rollback(
			cleanupContext,
			spec,
			remoteAllocated,
			createdVolumes,
			containerAllocated,
		)
		recordErr := returnErr
		if cleanupErr != nil {
			recordErr = errors.Join(returnErr, cleanupErr)
		}
		if _, err := controller.store.RecordError(manifest.RunID, recordErr); err != nil {
			returnErr = errors.Join(recordErr, fmt.Errorf("record failed run: %w", err))
			return
		}
		returnErr = recordErr
	}()

	if _, err := controller.backend.CreateStage(ctx, prepared); err != nil {
		return runstate.Manifest{}, fmt.Errorf("stream stage into VM: %w", err)
	}
	remoteAllocated = true

	volumeCommands, err := spec.CreateVolumeArgs()
	if err != nil {
		return runstate.Manifest{}, err
	}
	for _, args := range volumeCommands {
		if _, err := controller.podman(ctx, nil, args...); err != nil {
			return runstate.Manifest{}, fmt.Errorf("create run volume: %w", err)
		}
		createdVolumes = append(createdVolumes, args[len(args)-1])
	}
	if err := controller.backend.ImportStage(
		ctx,
		spec.RunID,
		spec.WorkspaceVolume(),
	); err != nil {
		return runstate.Manifest{}, err
	}

	runArgs, err := spec.RunArgs()
	if err != nil {
		return runstate.Manifest{}, err
	}
	if _, err := controller.podman(ctx, nil, runArgs...); err != nil {
		return runstate.Manifest{}, fmt.Errorf("start run container: %w", err)
	}
	containerAllocated = true

	materializeArgs, err := spec.MaterializeArgs(projectDirectory)
	if err != nil {
		return runstate.Manifest{}, err
	}
	if _, err := controller.podman(ctx, nil, materializeArgs...); err != nil {
		return runstate.Manifest{}, fmt.Errorf("materialize staged repository: %w", err)
	}
	cleanupStageArgs, err := spec.CleanupStageArgs()
	if err != nil {
		return runstate.Manifest{}, err
	}
	if _, err := controller.podman(ctx, nil, cleanupStageArgs...); err != nil {
		return runstate.Manifest{}, fmt.Errorf("remove transferred stage: %w", err)
	}
	if err := controller.backend.RemoveRun(ctx, spec.RunID); err != nil {
		return runstate.Manifest{}, fmt.Errorf("remove VM staging directory: %w", err)
	}
	remoteAllocated = false

	manifest, err = controller.store.Transition(spec.RunID, runstate.StateActive)
	if err != nil {
		return runstate.Manifest{}, fmt.Errorf("activate run manifest: %w", err)
	}
	return manifest, nil
}

func (controller Controller) podman(
	ctx context.Context,
	stdin io.Reader,
	args ...string,
) ([]byte, error) {
	return controller.backend.Execute(
		ctx,
		stdin,
		append([]string{"podman"}, args...)...,
	)
}

func (controller Controller) rollback(
	ctx context.Context,
	spec runcontainer.Spec,
	remoteAllocated bool,
	createdVolumes []string,
	containerAllocated bool,
) error {
	var failures []error
	if containerAllocated {
		if _, err := controller.podman(
			ctx,
			nil,
			"rm", "--force", spec.ContainerName(),
		); err != nil {
			failures = append(failures, fmt.Errorf("remove failed container: %w", err))
		}
	}
	for _, volume := range createdVolumes {
		if _, err := controller.podman(
			ctx,
			nil,
			"volume", "rm", "--force", volume,
		); err != nil && !strings.Contains(err.Error(), "no such volume") {
			failures = append(failures, fmt.Errorf("remove failed volume %s: %w", volume, err))
		}
	}
	if remoteAllocated {
		if err := controller.backend.RemoveRun(ctx, spec.RunID); err != nil {
			failures = append(failures, fmt.Errorf("remove failed remote stage: %w", err))
		}
	}
	return errors.Join(failures...)
}
