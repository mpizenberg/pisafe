// Package runctl composes host staging, SSH transport, rootless Podman, and
// the durable Mac-side lifecycle record.
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

	"github.com/mpizenberg/pisafe/internal/gitstage"
	"github.com/mpizenberg/pisafe/internal/runcontainer"
	"github.com/mpizenberg/pisafe/internal/runid"
	"github.com/mpizenberg/pisafe/internal/runssh"
	"github.com/mpizenberg/pisafe/internal/runstate"
)

type Backend interface {
	CreateStage(context.Context, gitstage.PreparedStage) (string, error)
	CreateRunStorage(context.Context, string) error
	VerifyRunStorage(context.Context, string) error
	EnsureProjectStorage(context.Context, string) error
	SelectCacheSnapshots(
		context.Context,
		string,
		[]runcontainer.CacheMount,
	) ([]runcontainer.CacheMount, error)
	PrepareRunOverlays(context.Context, string, []runcontainer.CacheMount) error
	PublishCacheSnapshot(context.Context, runcontainer.Spec, runcontainer.CacheMount) error
	EvictCacheSnapshots(context.Context, string, string, int, []string) error
	ResetProjectCache(context.Context, string) error
	ImportStage(context.Context, string) error
	Execute(context.Context, io.Reader, ...string) ([]byte, error)
	StreamExecute(context.Context, io.Writer, ...string) error
	FetchApplyArtifact(context.Context, string, gitstage.ApplyArtifact, string) error
	RemoveApplyPackage(context.Context, string) error
	RemoveRun(context.Context, string) error
	RemoveRunStorage(context.Context, string) error
	SSHGateway(context.Context) (runssh.Gateway, error)
}

type StateStore interface {
	Create(runstate.Manifest) (runstate.Manifest, error)
	Activate(string, runstate.SSHConnection, gitstage.Snapshot, string, time.Time) (runstate.Manifest, error)
	Get(string) (runstate.Manifest, error)
	List() ([]runstate.Manifest, error)
	Stop(string, time.Time) (runstate.Manifest, error)
	Resume(string, string, time.Time) (runstate.Manifest, error)
	Forget(string) error
	BeginApply(string, gitstage.PlannedApply) (runstate.Manifest, error)
	CompleteApply(string) (runstate.Manifest, error)
	RecordError(string, error) (runstate.Manifest, error)
}

type SSHStore interface {
	Prepare(context.Context, string) (runssh.Prepared, error)
	Finalize(runssh.Prepared, string, runssh.Gateway, string) (runssh.Endpoint, error)
	Remove(string) error
}

// InferenceConfig renders the run-side Pi provider configuration around one
// run-scoped capability. A nil InferenceConfig means no provider is
// configured yet; the capability is still issued and rotated.
type InferenceConfig interface {
	ModelsJSON(capability string) ([]byte, error)
}

type Controller struct {
	backend   Backend
	store     StateStore
	ssh       SSHStore
	inference InferenceConfig
}

func New(
	backend Backend,
	store StateStore,
	ssh SSHStore,
	inference InferenceConfig,
) Controller {
	return Controller{backend: backend, store: store, ssh: ssh, inference: inference}
}

// StartPrepared creates a run from host-prepared artifacts. The image must
// already exist in rootless Podman and is addressed only by immutable ID.
func (controller Controller) StartPrepared(
	ctx context.Context,
	prepared gitstage.PreparedStage,
	project runid.Project,
	imageID string,
	identity gitstage.Identity,
	caches []runcontainer.CacheMount,
) (_ runstate.Manifest, returnErr error) {
	spec := runcontainer.DefaultSpec(prepared.Snapshot.RunID, project.Key, imageID)
	spec.Caches = caches
	if err := spec.Validate(); err != nil {
		return runstate.Manifest{}, err
	}
	if err := identity.Validate(); err != nil {
		return runstate.Manifest{}, err
	}
	if _, err := spec.MaterializeArgs(project.Directory); err != nil {
		return runstate.Manifest{}, err
	}

	// The project filesystem is not rolled back with a failed run. It is shared
	// with every other run of the project and outlives all of them, so it is
	// ensured before the run has anything to roll back.
	if err := controller.backend.EnsureProjectStorage(ctx, spec.ProjectKey); err != nil {
		return runstate.Manifest{}, err
	}
	selected, err := controller.backend.SelectCacheSnapshots(ctx, spec.ProjectKey, spec.Caches)
	if err != nil {
		return runstate.Manifest{}, err
	}
	spec.Caches = selected

	manifest, err := controller.store.Create(runstate.Manifest{
		RunID:              prepared.Snapshot.RunID,
		Project:            project.Directory,
		ProjectKey:         project.Key,
		Snapshot:           prepared.Snapshot,
		Image:              imageID,
		Container:          spec.ContainerName(),
		Workspace:          "/work/" + project.Directory,
		Caches:             spec.Caches,
		ActiveLimitSeconds: spec.WallSeconds,
	})
	if err != nil {
		return runstate.Manifest{}, err
	}

	remoteAllocated := false
	storageAllocated := false
	containerAllocated := false
	sshAllocated := false
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
			storageAllocated,
			containerAllocated,
			sshAllocated,
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

	preparedSSH, err := controller.ssh.Prepare(ctx, spec.RunID)
	if err != nil {
		return runstate.Manifest{}, fmt.Errorf("prepare run SSH credentials: %w", err)
	}
	sshAllocated = true

	remoteAllocated = true
	if _, err := controller.backend.CreateStage(ctx, prepared); err != nil {
		return runstate.Manifest{}, fmt.Errorf("stream stage into VM: %w", err)
	}

	storageAllocated = true
	if err := controller.backend.CreateRunStorage(ctx, spec.RunID); err != nil {
		return runstate.Manifest{}, err
	}
	if err := controller.backend.PrepareRunOverlays(ctx, spec.RunID, spec.Caches); err != nil {
		return runstate.Manifest{}, err
	}
	if err := controller.backend.ImportStage(ctx, spec.RunID); err != nil {
		return runstate.Manifest{}, err
	}

	configureSSHArgs, err := spec.ConfigureSSHArgs()
	if err != nil {
		return runstate.Manifest{}, err
	}
	hostPublicKey, err := controller.podman(
		ctx,
		strings.NewReader(preparedSSH.PublicKey+"\n"),
		configureSSHArgs...,
	)
	if err != nil {
		return runstate.Manifest{}, fmt.Errorf("configure run SSH server: %w", err)
	}

	runArgs, err := spec.RunArgs()
	if err != nil {
		return runstate.Manifest{}, err
	}
	containerAllocated = true
	if _, err := controller.podman(ctx, nil, runArgs...); err != nil {
		return runstate.Manifest{}, fmt.Errorf("start run container: %w", err)
	}
	inspection, err := controller.inspectContainer(ctx, spec)
	if err != nil {
		return runstate.Manifest{}, err
	}
	if inspection == nil || inspection.State.Status != "running" {
		return runstate.Manifest{}, fmt.Errorf("new run container is not running")
	}

	gateway, err := controller.backend.SSHGateway(ctx)
	if err != nil {
		return runstate.Manifest{}, err
	}
	endpoint, err := controller.ssh.Finalize(
		preparedSSH,
		string(hostPublicKey),
		gateway,
		spec.ContainerName(),
	)
	if err != nil {
		return runstate.Manifest{}, fmt.Errorf("finalize run SSH connection: %w", err)
	}

	materializeArgs, err := spec.MaterializeArgs(project.Directory)
	if err != nil {
		return runstate.Manifest{}, err
	}
	materializedOutput, err := controller.podman(ctx, nil, materializeArgs...)
	if err != nil {
		return runstate.Manifest{}, fmt.Errorf("materialize staged repository: %w", err)
	}
	materialized, err := decodeMaterializedSnapshot(materializedOutput, prepared.Snapshot)
	if err != nil {
		return runstate.Manifest{}, err
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

	if err := controller.configureIdentity(ctx, spec, identity); err != nil {
		return runstate.Manifest{}, err
	}
	capability, err := runstate.NewInferenceCapability()
	if err != nil {
		return runstate.Manifest{}, err
	}
	if err := controller.configureInference(ctx, spec, capability); err != nil {
		return runstate.Manifest{}, err
	}

	manifest, err = controller.store.Activate(spec.RunID, runstate.SSHConnection{
		Alias:              endpoint.Alias,
		IdentityFile:       endpoint.IdentityFile,
		KnownHostsFile:     endpoint.KnownHostsFile,
		ConfigFile:         endpoint.ConfigFile,
		HostKeyFingerprint: endpoint.HostKeyFingerprint,
	}, materialized, capability, inspection.State.StartedAt)
	if err != nil {
		return runstate.Manifest{}, fmt.Errorf("activate run manifest: %w", err)
	}
	return manifest, nil
}

// configureIdentity gives the run the author its commits are attributed to.
// It runs only at creation, because the identity lives in the run's persistent
// home and an agent may deliberately change it there.
func (controller Controller) configureIdentity(
	ctx context.Context,
	spec runcontainer.Spec,
	identity gitstage.Identity,
) error {
	content, err := json.Marshal(identity)
	if err != nil {
		return fmt.Errorf("encode run Git identity: %w", err)
	}
	args, err := spec.ConfigureIdentityArgs()
	if err != nil {
		return err
	}
	if _, err := controller.podman(ctx, bytes.NewReader(content), args...); err != nil {
		return fmt.Errorf("install run Git identity: %w", err)
	}
	return nil
}

// configureInference writes the run's models.json when a provider is
// configured. Without one, the run simply has no Pi provider entry until the
// next resume after configuration.
func (controller Controller) configureInference(
	ctx context.Context,
	spec runcontainer.Spec,
	capability string,
) error {
	if controller.inference == nil {
		return nil
	}
	content, err := controller.inference.ModelsJSON(capability)
	if err != nil {
		return fmt.Errorf("render run inference configuration: %w", err)
	}
	args, err := spec.ConfigureInferenceArgs()
	if err != nil {
		return err
	}
	if _, err := controller.podman(ctx, bytes.NewReader(content), args...); err != nil {
		return fmt.Errorf("install run inference configuration: %w", err)
	}
	return nil
}

// guestResponseLimit bounds every JSON document a run hands back. It is far
// above what any of them legitimately needs, and keeps a run from making the
// controller allocate or the manifest grow without bound.
const guestResponseLimit = 1 << 20

// decodeGuestResponse reads one document produced inside a run. Unknown fields
// and trailing data are refused, so a run cannot smuggle anything past what
// the controller expects to receive.
func decodeGuestResponse[Document any](output []byte, subject string) (Document, error) {
	var document Document
	if len(output) > guestResponseLimit {
		return document, fmt.Errorf("%s exceeds size limit", subject)
	}
	decoder := json.NewDecoder(bytes.NewReader(output))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return document, fmt.Errorf("decode %s: %w", subject, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return document, fmt.Errorf("%s contains trailing data", subject)
		}
		return document, fmt.Errorf("decode %s trailer: %w", subject, err)
	}
	return document, nil
}

func decodeMaterializedSnapshot(
	output []byte,
	hostSnapshot gitstage.Snapshot,
) (gitstage.Snapshot, error) {
	materialized, err := decodeGuestResponse[gitstage.Snapshot](output, "materialized snapshot")
	if err != nil {
		return gitstage.Snapshot{}, err
	}
	if materialized.SourceRoot != "" ||
		materialized.RunID != hostSnapshot.RunID ||
		materialized.SourceHead != hostSnapshot.SourceHead ||
		materialized.WorkRef != hostSnapshot.WorkRef ||
		!slices.Equal(materialized.Inputs, hostSnapshot.Inputs) ||
		!sameSubmodules(materialized.Submodules, hostSnapshot.Submodules) ||
		!materialized.CreatedAt.Equal(hostSnapshot.CreatedAt) {
		return gitstage.Snapshot{}, errors.New("materialized snapshot does not match prepared run")
	}
	return materialized, nil
}

// sameSubmodules compares what the Mac staged with what the run reported.
// Baseline commits are created inside the run, so only identity and the
// captured head must match.
func sameSubmodules(materialized, staged []gitstage.SubmoduleStage) bool {
	if len(materialized) != len(staged) {
		return false
	}
	for index := range staged {
		if materialized[index].Path != staged[index].Path ||
			materialized[index].Head != staged[index].Head {
			return false
		}
	}
	return true
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
	storageAllocated bool,
	containerAllocated bool,
	sshAllocated bool,
) error {
	var failures []error
	if containerAllocated {
		if _, err := controller.podman(
			ctx,
			nil,
			"rm", "--force", "--ignore", spec.ContainerName(),
		); err != nil {
			failures = append(failures, fmt.Errorf("remove failed container: %w", err))
		}
	}
	if storageAllocated {
		if err := controller.backend.RemoveRunStorage(ctx, spec.RunID); err != nil {
			failures = append(failures, fmt.Errorf("remove failed storage: %w", err))
		}
	}
	if remoteAllocated {
		if err := controller.backend.RemoveRun(ctx, spec.RunID); err != nil {
			failures = append(failures, fmt.Errorf("remove failed remote stage: %w", err))
		}
	}
	if sshAllocated {
		if err := controller.ssh.Remove(spec.RunID); err != nil {
			failures = append(failures, fmt.Errorf("remove failed SSH credentials: %w", err))
		}
	}
	return errors.Join(failures...)
}
