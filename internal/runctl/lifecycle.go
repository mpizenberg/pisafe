package runctl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/mpizenberg/pisafe/internal/runcontainer"
	"github.com/mpizenberg/pisafe/internal/runstate"
)

const inspectContainerScript = `set -eu
if podman container exists "$1"; then
	exec podman container inspect "$1"
fi
printf 'null\n'
`

type containerInspection struct {
	ID     string `json:"Id"`
	Name   string `json:"Name"`
	Image  string `json:"Image"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	State struct {
		Status     string    `json:"Status"`
		StartedAt  time.Time `json:"StartedAt"`
		FinishedAt time.Time `json:"FinishedAt"`
	} `json:"State"`
	Mounts []containerMount `json:"Mounts"`
}

type containerMount struct {
	Type        string   `json:"Type"`
	Source      string   `json:"Source"`
	Destination string   `json:"Destination"`
	Options     []string `json:"Options"`
}

func (controller Controller) Stop(
	ctx context.Context,
	runID string,
) (runstate.Manifest, error) {
	manifest, err := controller.store.Get(runID)
	if err != nil {
		return runstate.Manifest{}, err
	}
	if manifest.State != runstate.StateActive {
		return runstate.Manifest{}, fmt.Errorf("run %q is %s, not active", runID, manifest.State)
	}
	spec := specForManifest(manifest, manifest.Image)
	endedAt, err := controller.stopAndRemoveContainer(ctx, spec)
	if err == nil {
		err = controller.backend.VerifyRunStorage(ctx, runID)
	}
	if err != nil {
		return runstate.Manifest{}, controller.recordLifecycleError(runID, "stop", err)
	}
	stopped, err := controller.store.Stop(runID, endedAt)
	if err != nil {
		return runstate.Manifest{}, controller.recordLifecycleError(runID, "record stop", err)
	}
	return stopped, nil
}

func (controller Controller) Resume(
	ctx context.Context,
	runID string,
) (runstate.Manifest, error) {
	manifest, err := controller.store.Get(runID)
	if err != nil {
		return runstate.Manifest{}, err
	}
	if manifest.State != runstate.StateStopped {
		return runstate.Manifest{}, fmt.Errorf("run %q is %s, not stopped", runID, manifest.State)
	}
	remaining := runstate.RemainingSeconds(manifest, time.Now())
	if remaining == 0 {
		return runstate.Manifest{}, fmt.Errorf(
			"run %q exhausted its %s active wall-clock limit",
			runID,
			time.Duration(manifest.ActiveLimitSeconds)*time.Second,
		)
	}
	spec := specForManifest(manifest, manifest.Image)
	spec.WallSeconds = remaining
	if err := spec.Validate(); err != nil {
		return runstate.Manifest{}, err
	}
	if err := controller.backend.VerifyRunStorage(ctx, runID); err != nil {
		return runstate.Manifest{}, controller.recordLifecycleError(runID, "resume", err)
	}

	existing, err := controller.inspectContainer(ctx, spec)
	if err != nil {
		return runstate.Manifest{}, controller.recordLifecycleError(runID, "resume", err)
	}
	if existing != nil {
		if _, err := controller.stopAndRemoveContainer(ctx, spec); err != nil {
			return runstate.Manifest{}, controller.recordLifecycleError(runID, "recover resume", err)
		}
	}

	runArgs, err := spec.RunArgs()
	if err != nil {
		return runstate.Manifest{}, err
	}
	if _, err := controller.podman(ctx, nil, runArgs...); err != nil {
		cleanupContext, cancelCleanup := lifecycleCleanupContext(ctx)
		defer cancelCleanup()
		_, cleanupErr := controller.stopAndRemoveContainer(cleanupContext, spec)
		return runstate.Manifest{}, controller.recordLifecycleError(
			runID,
			"resume",
			errors.Join(fmt.Errorf("start run container: %w", err), cleanupErr),
		)
	}
	inspection, err := controller.inspectContainer(ctx, spec)
	if err != nil || inspection == nil || inspection.State.Status != "running" {
		if err == nil {
			err = errors.New("resumed container is not running")
		}
		cleanupContext, cancelCleanup := lifecycleCleanupContext(ctx)
		defer cancelCleanup()
		_, cleanupErr := controller.stopAndRemoveContainer(
			cleanupContext,
			spec,
		)
		return runstate.Manifest{}, controller.recordLifecycleError(
			runID,
			"resume",
			errors.Join(err, cleanupErr),
		)
	}
	capability, err := runstate.NewInferenceCapability()
	if err == nil {
		err = controller.configureInference(ctx, spec, capability)
	}
	if err != nil {
		cleanupContext, cancelCleanup := lifecycleCleanupContext(ctx)
		defer cancelCleanup()
		_, cleanupErr := controller.stopAndRemoveContainer(cleanupContext, spec)
		return runstate.Manifest{}, controller.recordLifecycleError(
			runID,
			"resume",
			errors.Join(err, cleanupErr),
		)
	}
	resumed, err := controller.store.Resume(runID, capability, inspection.State.StartedAt)
	if err != nil {
		cleanupContext, cancelCleanup := lifecycleCleanupContext(ctx)
		defer cancelCleanup()
		_, cleanupErr := controller.stopAndRemoveContainer(
			cleanupContext,
			spec,
		)
		return runstate.Manifest{}, controller.recordLifecycleError(
			runID,
			"record resume",
			errors.Join(err, cleanupErr),
		)
	}
	return resumed, nil
}

// Discard reclaims a run at the user's request. An active run is stopped first
// so its elapsed time is accounted before the container goes.
func (controller Controller) Discard(ctx context.Context, runID string) error {
	manifest, err := controller.store.Get(runID)
	if err != nil {
		return err
	}
	if manifest.State == runstate.StateActive {
		manifest, err = controller.Stop(ctx, runID)
		if err != nil {
			return err
		}
	}
	return controller.release(ctx, manifest, "discard")
}

// release reclaims what a run owns and then removes its record. The record
// outlives the resources only while reclamation is incomplete, so a failure
// always leaves something to retry against.
func (controller Controller) release(
	ctx context.Context,
	manifest runstate.Manifest,
	operation string,
) error {
	if err := controller.reclaim(ctx, manifest); err != nil {
		return controller.recordLifecycleError(manifest.RunID, operation, err)
	}
	if err := controller.store.Forget(manifest.RunID); err != nil {
		return controller.recordLifecycleError(manifest.RunID, "record "+operation, err)
	}
	return nil
}

// reclaim removes everything a run still owns, in the VM and on the Mac. Every
// step is idempotent, so a partially reclaimed run can always be finished.
func (controller Controller) reclaim(
	ctx context.Context,
	manifest runstate.Manifest,
) error {
	if _, err := controller.stopAndRemoveContainer(ctx, specForManifest(manifest, manifest.Image)); err != nil {
		return err
	}
	var failures []error
	if err := controller.backend.RemoveRunStorage(ctx, manifest.RunID); err != nil {
		failures = append(failures, err)
	}
	if err := controller.backend.RemoveRun(ctx, manifest.RunID); err != nil {
		failures = append(failures, err)
	}
	if err := controller.ssh.Remove(manifest.RunID); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

// specForManifest rebuilds the container definition of a recorded run. The
// image is a separate argument because inspecting a run uses the current
// managed image, not the one the run was created from.
func specForManifest(manifest runstate.Manifest, imageID string) runcontainer.Spec {
	spec := runcontainer.DefaultSpec(manifest.RunID, manifest.ProjectKey, imageID)
	spec.WallSeconds = manifest.ActiveLimitSeconds
	spec.Caches = manifest.Caches
	return spec
}

func (controller Controller) stopAndRemoveContainer(
	ctx context.Context,
	spec runcontainer.Spec,
) (time.Time, error) {
	inspection, err := controller.inspectContainer(ctx, spec)
	if err != nil {
		return time.Time{}, err
	}
	if inspection == nil {
		return time.Now().UTC(), nil
	}
	if inspection.State.Status == "running" {
		if _, err := controller.podman(
			ctx,
			nil,
			"stop", "--time", "10", spec.ContainerName(),
		); err != nil {
			return time.Time{}, fmt.Errorf("stop run container: %w", err)
		}
		inspection, err = controller.inspectContainer(ctx, spec)
		if err != nil {
			return time.Time{}, err
		}
		if inspection == nil {
			return time.Now().UTC(), nil
		}
	}
	if inspection.State.Status == "running" {
		return time.Time{}, errors.New("run container remained active after stop")
	}
	endedAt := inspection.State.FinishedAt
	if endedAt.IsZero() {
		endedAt = time.Now().UTC()
	}
	if _, err := controller.podman(
		ctx,
		nil,
		"rm", "--force", spec.ContainerName(),
	); err != nil {
		return time.Time{}, fmt.Errorf("remove stopped run container: %w", err)
	}
	return endedAt, nil
}

func (controller Controller) inspectContainer(
	ctx context.Context,
	spec runcontainer.Spec,
) (*containerInspection, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	output, err := controller.backend.Execute(
		ctx,
		nil,
		"sh", "-ceu", inspectContainerScript,
		"pisafe-inspect-container", spec.ContainerName(),
	)
	if err != nil {
		return nil, fmt.Errorf("inspect run container: %w", err)
	}
	if len(output) > 2<<20 {
		return nil, errors.New("run container inspection exceeds size limit")
	}
	decoder := json.NewDecoder(io.LimitReader(bytes.NewReader(output), 2<<20))
	var inspections []containerInspection
	if bytes.Equal(bytes.TrimSpace(output), []byte("null")) {
		return nil, nil
	}
	if err := decoder.Decode(&inspections); err != nil {
		return nil, fmt.Errorf("decode run container inspection: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("run container inspection contains trailing data")
		}
		return nil, fmt.Errorf("decode run container inspection trailer: %w", err)
	}
	if len(inspections) != 1 {
		return nil, fmt.Errorf("expected one exact run container, got %d", len(inspections))
	}
	inspection := &inspections[0]
	if err := validateContainerInspection(spec, *inspection); err != nil {
		return nil, err
	}
	return inspection, nil
}

// mountRequirement pins one mount a run must have. A bind is named by its
// source; an overlay by the layers it was stacked from, because Podman
// substitutes its own merged directory for the source.
type mountRequirement struct {
	source  string
	options []string
}

func validateContainerInspection(
	spec runcontainer.Spec,
	inspection containerInspection,
) error {
	if inspection.ID == "" || strings.TrimPrefix(inspection.Name, "/") != spec.ContainerName() {
		return errors.New("run container identity does not match manifest")
	}
	image := inspection.Image
	if len(image) == 64 {
		image = "sha256:" + image
	}
	if image != spec.ImageID {
		return errors.New("run container image does not match manifest")
	}
	if inspection.Config.Labels["io.pisafe.run"] != spec.RunID {
		return errors.New("run container label does not match manifest")
	}
	expected := map[string]mountRequirement{
		"/work":      {source: spec.WorkspacePath()},
		"/home/node": {source: spec.HomePath()},
	}
	// Podman reports an overlay as a bind onto its own merged directory, whose
	// path it chooses, so a shared layer is pinned by what it was stacked from
	// instead: the lower must be this project's and nothing else's, and the
	// upper must be inside this run.
	for _, overlay := range spec.ProjectOverlays() {
		expected[overlay.Destination] = mountRequirement{options: []string{
			"lowerdir=" + overlay.Lower,
			"upperdir=" + overlay.Upper,
			"workdir=" + overlay.Work,
		}}
	}
	for _, mount := range inspection.Mounts {
		required, isExpected := expected[mount.Destination]
		if !isExpected {
			if mount.Type == "bind" || mount.Type == "volume" {
				return fmt.Errorf("run container has unexpected persistent mount %s", mount.Destination)
			}
			continue
		}
		matched := mount.Type == "bind" &&
			(required.source == "" || mount.Source == required.source)
		for _, option := range required.options {
			matched = matched && slices.Contains(mount.Options, option)
		}
		if !matched {
			return fmt.Errorf("run container mount %s does not match manifest", mount.Destination)
		}
		delete(expected, mount.Destination)
	}
	if len(expected) != 0 {
		return errors.New("run container is missing persistent storage mounts")
	}
	return nil
}

func lifecycleCleanupContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), 30*time.Second)
}

func (controller Controller) recordLifecycleError(
	runID string,
	operation string,
	operationErr error,
) error {
	wrapped := fmt.Errorf("%s run: %w", operation, operationErr)
	if _, err := controller.store.RecordError(runID, wrapped); err != nil {
		return errors.Join(wrapped, fmt.Errorf("record lifecycle failure: %w", err))
	}
	return wrapped
}
