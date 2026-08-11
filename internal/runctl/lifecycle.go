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
	"github.com/mpizenberg/pisafe/internal/runid"
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
	// Podman reports a read-only bind here and not among the options, so this
	// is the only field that distinguishes a read-only profile from a writable
	// one.
	Writable bool `json:"RW"`
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
	// Nothing to stop means the container went with a rebooted or recreated VM,
	// taking the only account of how much of the run's budget the stretch spent.
	var stopped runstate.Manifest
	if endedAt.IsZero() {
		stopped, err = controller.store.Abandon(runID)
	} else {
		stopped, err = controller.store.Stop(runID, endedAt)
	}
	if err != nil {
		return runstate.Manifest{}, controller.recordLifecycleError(runID, "record stop", err)
	}
	// What the run produced is handed to the project whatever the run's outcome
	// was: a cache a failed run warmed still restores, and a failed run's
	// transcript is the one most worth reading. Neither half is worth failing a
	// stop that worked — an unpublished cache costs a later run time, and an
	// unpromoted transcript stays in the run's own storage until it is discarded
	// — so both are recorded against the run instead.
	persistErr := errors.Join(
		controller.publishCaches(ctx, stopped),
		controller.backend.PromoteSessions(ctx, stopped.ProjectKey, runID),
	)
	if persistErr == nil {
		return stopped, nil
	}
	recorded, err := controller.store.RecordError(runID, persistErr)
	if err != nil {
		return stopped, errors.Join(persistErr, err)
	}
	return recorded, nil
}

func (controller Controller) Resume(
	ctx context.Context,
	runID string,
) (_ runstate.Manifest, returnErr error) {
	manifest, err := controller.store.Get(runID)
	if err != nil {
		return runstate.Manifest{}, err
	}
	manifest, err = controller.endWhatIsNoLongerRunning(ctx, manifest)
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
	if err := controller.backend.EnsureGlobalStorage(ctx); err != nil {
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

	// From here a failure may have left a container behind, and a resume that
	// did not finish must leave the run stopped as it was found.
	operation := "resume"
	defer func() {
		if returnErr == nil {
			return
		}
		cleanupContext, cancelCleanup := lifecycleCleanupContext(ctx)
		defer cancelCleanup()
		_, cleanupErr := controller.stopAndRemoveContainer(cleanupContext, spec)
		returnErr = controller.recordLifecycleError(
			runID,
			operation,
			errors.Join(returnErr, cleanupErr),
		)
	}()

	if _, err := controller.podman(ctx, nil, runArgs...); err != nil {
		return runstate.Manifest{}, fmt.Errorf("start run container: %w", err)
	}
	inspection, err := controller.inspectStartedContainer(ctx, spec)
	if err != nil {
		return runstate.Manifest{}, err
	}
	if inspection == nil || inspection.State.Status != "running" {
		return runstate.Manifest{}, errors.New("resumed container is not running")
	}
	capability, err := runstate.NewInferenceCapability()
	if err != nil {
		return runstate.Manifest{}, err
	}
	if err := controller.configureProfile(ctx, spec, manifest.Workspace()); err != nil {
		return runstate.Manifest{}, err
	}
	if err := controller.configureModels(ctx, spec, capability); err != nil {
		return runstate.Manifest{}, err
	}
	operation = "record resume"
	return controller.store.Resume(runID, capability, inspection.State.StartedAt)
}

// endWhatIsNoLongerRunning settles a record that still claims a container the
// VM no longer runs, so a resume is refused only by a run that is genuinely
// working. A container that vanished with a rebooted or recreated VM and one
// that exited at its own deadline are the same condition here: the run is over
// and its storage is not, and stopping it accounts for as much of the stretch
// as was observed before publishing what the run produced.
func (controller Controller) endWhatIsNoLongerRunning(
	ctx context.Context,
	manifest runstate.Manifest,
) (runstate.Manifest, error) {
	if manifest.State != runstate.StateActive {
		return manifest, nil
	}
	existing, err := controller.inspectContainer(
		ctx,
		specForManifest(manifest, manifest.Image),
	)
	if err != nil {
		return runstate.Manifest{}, controller.recordLifecycleError(
			manifest.RunID,
			"resume",
			err,
		)
	}
	if existing != nil && existing.State.Status == "running" {
		return manifest, nil
	}
	return controller.Stop(ctx, manifest.RunID)
}

// Discard reclaims a run at the user's request. An active run is stopped first
// so its elapsed time is accounted before the container goes. The record is
// read only to ask that one question: everything a run owns is keyed by its ID,
// so a record this version cannot decode is still discarded rather than
// stranding what it holds.
func (controller Controller) Discard(ctx context.Context, runID string) error {
	manifest, err := controller.store.Get(runID)
	if err == nil && manifest.State == runstate.StateActive {
		if _, err := controller.Stop(ctx, runID); err != nil {
			return err
		}
	}
	return controller.release(ctx, runID, "discard")
}

// release reclaims what a run owns and then removes its record. The record
// outlives the resources only while reclamation is incomplete, so a failure
// always leaves something to retry against.
func (controller Controller) release(
	ctx context.Context,
	runID string,
	operation string,
) error {
	if err := controller.reclaim(ctx, runID); err != nil {
		return controller.recordLifecycleError(runID, operation, err)
	}
	if err := controller.store.Forget(runID); err != nil {
		return controller.recordLifecycleError(runID, "record "+operation, err)
	}
	return nil
}

// reclaim removes everything a run still owns, in the VM and on the Mac. Every
// step is idempotent, so a partially reclaimed run can always be finished, and
// every one is named by the run ID alone. The container is taken by force
// because nothing here waits on it: a run still active was stopped gracefully
// before this ran, so what is left is a leftover to sweep.
func (controller Controller) reclaim(ctx context.Context, runID string) error {
	if err := runid.Validate(runID); err != nil {
		return err
	}
	if _, err := controller.podman(
		ctx,
		nil,
		"rm", "--force", "--ignore", runcontainer.ContainerName(runID),
	); err != nil {
		return fmt.Errorf("remove run container: %w", err)
	}
	var failures []error
	if err := controller.backend.RemoveRunStorage(ctx, runID); err != nil {
		failures = append(failures, err)
	}
	if err := controller.backend.RemoveRun(ctx, runID); err != nil {
		failures = append(failures, err)
	}
	if err := controller.ssh.Remove(runID); err != nil {
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

// stopAndRemoveContainer takes a run's container down and reports when it
// ended. A zero time means there was nothing to take down: the container went
// with a VM that was rebooted or recreated, and the only account of when it
// ended went with it.
func (controller Controller) stopAndRemoveContainer(
	ctx context.Context,
	spec runcontainer.Spec,
) (time.Time, error) {
	inspection, err := controller.inspectContainer(ctx, spec)
	if err != nil {
		return time.Time{}, err
	}
	if inspection == nil {
		return time.Time{}, nil
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
	decoder := json.NewDecoder(bytes.NewReader(output))
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
	if err := validateContainerIdentity(spec, *inspection); err != nil {
		return nil, err
	}
	return inspection, nil
}

// inspectStartedContainer is inspectContainer for a container pisafe has just
// started, where the mounts it came up with are the thing being proved.
func (controller Controller) inspectStartedContainer(
	ctx context.Context,
	spec runcontainer.Spec,
) (*containerInspection, error) {
	inspection, err := controller.inspectContainer(ctx, spec)
	if err != nil || inspection == nil {
		return inspection, err
	}
	if err := validateRunMounts(spec, *inspection); err != nil {
		return nil, err
	}
	return inspection, nil
}

// mountRequirement pins one mount a run must have. A bind is named by its
// source; an overlay by the layers it was stacked from, because Podman
// substitutes its own merged directory for the source.
type mountRequirement struct {
	source   string
	options  []string
	readOnly bool
}

// validateContainerIdentity proves the container is the run's own before
// anything acts on it. Every path needs this much, including the ones whose
// next move is to destroy it.
func validateContainerIdentity(
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
	return nil
}

// validateRunMounts proves a container reaches only what its run may, and
// nothing writable that a later run reads. It guards what pisafe has just
// started and is about to hand over, which is where the property has to hold.
// Tearing a container down does not need it: what a stop publishes it reads
// from run storage rather than through the container, and requiring the check
// there would strand a run whose container predates a change to the layout.
func validateRunMounts(
	spec runcontainer.Spec,
	inspection containerInspection,
) error {
	profileMount := runcontainer.ProfileMount()
	toolsMount := runcontainer.ToolsMount()
	expected := map[string]mountRequirement{
		"/work":      {source: spec.WorkspacePath()},
		"/home/node": {source: spec.HomePath()},
		// A writable profile would be agent code able to change what every
		// later run of every project loads, so it is checked and not assumed.
		profileMount.Destination: {source: profileMount.Source, readOnly: true},
		// The installed commands are on every run's PATH, so a writable one
		// would be agent code able to change what a later run executes.
		toolsMount.Destination: {source: toolsMount.Source, readOnly: true},
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
			(required.source == "" || mount.Source == required.source) &&
			mount.Writable != required.readOnly
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
