// Package runcontainer defines the rootless Podman resources and hardened
// command line for one isolated run.
package runcontainer

import (
	"fmt"
	"regexp"
	"strconv"

	"github.com/mpizenberg/pisafe/internal/gitstage"
	"github.com/mpizenberg/pisafe/internal/runcopy"
	"github.com/mpizenberg/pisafe/internal/runid"
)

const (
	DefaultCPUs       = "2"
	DefaultMemory     = int64(4 * 1024 * 1024 * 1024)
	DefaultPIDs       = 512
	DefaultTemporary  = int64(1024 * 1024 * 1024)
	DefaultPersistent = int64(10 * 1024 * 1024 * 1024)
	DefaultProject    = int64(10 * 1024 * 1024 * 1024)
	DefaultWallClock  = int64(8 * 60 * 60)
	// applyPackage is where a run leaves the bundles apply fetches. It sits in
	// the run's own workspace, the only writable place both the run and the
	// VM-side transport can reach.
	applyPackage      = "apply"
	containerUser     = "1000:1000"
	containerWorkRoot = "/work"
	containerHome     = "/home/node"
	// containerCacheRoot is a layout pisafe owns rather than a tool's own
	// default location. Only what a cache-specific environment variable points
	// here is shared, so the project layer can never accumulate arbitrary state.
	containerCacheRoot   = "/cache"
	containerSessionRoot = "/sessions"
	guestStorageRoot     = "/var/lib/pisafe/runs"
	guestProjectRoot     = "/var/lib/pisafe/projects"
)

// projectLayers is what runs of one project share. A layer is one directory in
// the project filesystem, one upper and work pair in the run filesystem, and
// one path in the container, all filed under the same name.
var projectLayers = []struct{ name, destination string }{
	{name: "cache", destination: containerCacheRoot},
	{name: "sessions", destination: containerSessionRoot},
}

// ProjectLayerNames is for the privileged helper, which allocates a directory
// per layer in both filesystems and knows nothing else about them.
func ProjectLayerNames() []string {
	names := make([]string, 0, len(projectLayers))
	for _, layer := range projectLayers {
		names = append(names, layer.name)
	}
	return names
}

var imageIDPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

type Spec struct {
	RunID       string
	ProjectKey  string
	ImageID     string
	CPUs        string
	MemoryBytes int64
	PIDs        int
	TmpBytes    int64
	WallSeconds int64
}

func DefaultSpec(runID, projectKey, imageID string) Spec {
	return Spec{
		RunID:       runID,
		ProjectKey:  projectKey,
		ImageID:     imageID,
		CPUs:        DefaultCPUs,
		MemoryBytes: DefaultMemory,
		PIDs:        DefaultPIDs,
		TmpBytes:    DefaultTemporary,
		WallSeconds: DefaultWallClock,
	}
}

func (spec Spec) Validate() error {
	if err := runid.Validate(spec.RunID); err != nil {
		return err
	}
	if err := runid.Validate(spec.ProjectKey); err != nil {
		return fmt.Errorf("invalid project key: %w", err)
	}
	if !imageIDPattern.MatchString(spec.ImageID) {
		return fmt.Errorf("image must be an immutable sha256 ID")
	}
	if spec.CPUs == "" {
		return fmt.Errorf("CPU limit is required")
	}
	if spec.MemoryBytes <= 0 {
		return fmt.Errorf("memory limit is required")
	}
	if spec.PIDs <= 0 {
		return fmt.Errorf("process limit is required")
	}
	if spec.TmpBytes <= 0 {
		return fmt.Errorf("temporary-filesystem limit is required")
	}
	if spec.WallSeconds <= 0 {
		return fmt.Errorf("wall-clock limit is required")
	}
	return nil
}

func (spec Spec) ContainerName() string {
	return "pisafe-run-" + spec.RunID
}

func (spec Spec) StoragePath() string {
	return guestStorageRoot + "/" + spec.RunID
}

func (spec Spec) WorkspacePath() string {
	return spec.StoragePath() + "/workspace"
}

func (spec Spec) HomePath() string {
	return spec.StoragePath() + "/home"
}

// ProjectOverlay is one shared layer as the run sees it. Lower belongs to the
// project and is never written; Upper and Work belong to the run alone, which
// is what keeps one run's writes out of every other run of the project.
type ProjectOverlay struct {
	Destination string
	Lower       string
	Upper       string
	Work        string
}

func (spec Spec) ProjectOverlays() []ProjectOverlay {
	overlays := make([]ProjectOverlay, 0, len(projectLayers))
	for _, layer := range projectLayers {
		overlays = append(overlays, ProjectOverlay{
			Destination: layer.destination,
			Lower:       guestProjectRoot + "/" + spec.ProjectKey + "/" + layer.name,
			Upper:       spec.StoragePath() + "/overlay/" + layer.name + "/upper",
			Work:        spec.StoragePath() + "/overlay/" + layer.name + "/work",
		})
	}
	return overlays
}

// volume renders the only spelling Podman accepts for a rootless overlay.
// It refuses nodev and nosuid alongside one, which costs nothing here:
// creating a device needs CAP_MKNOD and the run drops every capability, and
// no-new-privileges already neutralizes a setuid bit.
func (overlay ProjectOverlay) volume() string {
	return overlay.Lower + ":" + overlay.Destination +
		":O,upperdir=" + overlay.Upper + ",workdir=" + overlay.Work
}

func (spec Spec) RunArgs() ([]string, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	memory := strconv.FormatInt(spec.MemoryBytes, 10)
	tmpSize := strconv.FormatInt(spec.TmpBytes, 10)
	args := []string{
		"run",
		"--detach",
		"--pull=never",
		"--name", spec.ContainerName(),
		"--hostname", spec.ContainerName(),
		"--label", "io.pisafe.run=" + spec.RunID,
		"--user", containerUser,
		"--read-only",
		"--cap-drop=all",
		"--security-opt=no-new-privileges",
		"--network=pasta",
		"--dns=1.1.1.1",
		"--dns=9.9.9.9",
		"--cpus", spec.CPUs,
		"--memory", memory,
		"--memory-swap", memory,
		"--pids-limit", strconv.Itoa(spec.PIDs),
		"--timeout", strconv.FormatInt(spec.WallSeconds, 10),
		"--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=" + tmpSize,
		"--mount", "type=tmpfs,dst=/run,tmpfs-size=16777216,tmpfs-mode=0755,U=true",
		"--mount", "type=bind,src=" + spec.WorkspacePath() + ",dst=" + containerWorkRoot + ",nodev,nosuid",
		"--mount", "type=bind,src=" + spec.HomePath() + ",dst=" + containerHome + ",nodev,nosuid",
	}
	for _, overlay := range spec.ProjectOverlays() {
		args = append(args, "--volume", overlay.volume())
	}
	return append(args,
		"--workdir", containerWorkRoot,
		"--env", "HOME="+containerHome,
		"--env", "GIT_TERMINAL_PROMPT=0",
		"--env", "PI_CODING_AGENT_DIR="+containerHome+"/.pi/agent",
		"--env", "PI_CODING_AGENT_SESSION_DIR="+containerSessionRoot,
		"--env", "PI_SKIP_VERSION_CHECK=1",
		"--env", "npm_config_cache="+containerCacheRoot+"/npm",
		// Logs and the update check write uncacheable per-run state. Keeping
		// them out of the shared layer is what leaves every path in it
		// content-addressed, and so safe to merge last-writer-wins.
		"--env", "npm_config_logs_dir="+containerHome+"/.npm/_logs",
		"--env", "npm_config_update_notifier=false",
		spec.ImageID,
		"pisafe-guest", "serve-ssh",
	), nil
}

func (spec Spec) ConfigureSSHArgs() ([]string, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	return []string{
		"run",
		"--rm",
		"--interactive",
		"--pull=never",
		"--label", "io.pisafe.run=" + spec.RunID,
		"--label", "io.pisafe.kind=ssh-init",
		"--user", containerUser,
		"--read-only",
		"--cap-drop=all",
		"--security-opt=no-new-privileges",
		"--network=none",
		"--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=16777216",
		"--mount", "type=bind,src=" + spec.HomePath() + ",dst=" + containerHome + ",nodev,nosuid",
		"--env", "HOME=" + containerHome,
		spec.ImageID,
		"pisafe-guest", "configure-ssh",
	}, nil
}

func (spec Spec) MaterializeArgs(projectDirectory string) ([]string, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if err := runid.Validate(projectDirectory); err != nil {
		return nil, fmt.Errorf("invalid project directory: %w", err)
	}
	return []string{
		"exec",
		"--user", containerUser,
		"--workdir", containerWorkRoot,
		spec.ContainerName(),
		"pisafe-guest", "materialize",
		containerWorkRoot + "/stage",
		containerWorkRoot + "/" + projectDirectory,
	}, nil
}

// ConfigureInferenceArgs installs the Pi provider configuration piped through
// stdin into the run home. It runs after activation and resume so a fresh
// capability always replaces the previous one.
func (spec Spec) ConfigureInferenceArgs() ([]string, error) {
	return spec.configureArgs("configure-inference")
}

// ConfigureIdentityArgs installs the Git identity piped through stdin into the
// run home. It runs once at creation, because the run home keeps it.
func (spec Spec) ConfigureIdentityArgs() ([]string, error) {
	return spec.configureArgs("configure-identity")
}

func (spec Spec) configureArgs(command string) ([]string, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	return []string{
		"exec",
		"--interactive",
		"--user", containerUser,
		spec.ContainerName(),
		"pisafe-guest", command,
	}, nil
}

// PrepareApplyArgs captures a run's result in a throwaway container over the
// run's persistent workspace.
func (spec Spec) PrepareApplyArgs(
	projectDirectory string,
	baseline gitstage.BaselineChoice,
) ([]string, error) {
	if _, err := gitstage.ParseBaselineChoice(string(baseline)); err != nil {
		return nil, err
	}
	args, err := spec.inspectionArgs("apply", projectDirectory, "")
	if err != nil {
		return nil, err
	}
	return append(
		args,
		"pisafe-guest", "prepare-apply",
		string(baseline),
		containerWorkRoot+"/"+projectDirectory,
		containerWorkRoot+"/"+applyPackage,
	), nil
}

// DiffArgs reports what a run changed. The workspace is mounted read-only
// because a report must not be able to alter what it reports, which also lets
// it run against a container that is still working.
func (spec Spec) DiffArgs(projectDirectory string) ([]string, error) {
	args, err := spec.inspectionArgs("diff", projectDirectory, ",ro")
	if err != nil {
		return nil, err
	}
	return append(args, "pisafe-guest", "diff", containerWorkRoot+"/"+projectDirectory), nil
}

// ExportArgs streams one path of the run's workspace out as a tar on standard
// output. The workspace is read-only for the same reason diff's is: taking a
// copy must not change what was copied.
func (spec Spec) ExportArgs(projectDirectory, requestPath string) ([]string, error) {
	requested, err := runcopy.SafePath(requestPath)
	if err != nil {
		return nil, err
	}
	args, err := spec.inspectionArgs("copy", projectDirectory, ",ro")
	if err != nil {
		return nil, err
	}
	return append(
		args,
		"pisafe-guest", "export",
		containerWorkRoot+"/"+projectDirectory,
		requested,
	), nil
}

// inspectionArgs builds a throwaway container over the run's persistent
// workspace, up to and including the image. Inspecting a run needs no network,
// no home, and no part of its wall-clock budget, so it gets none of them, and
// it works whether or not the run container exists.
func (spec Spec) inspectionArgs(
	kind string,
	projectDirectory string,
	mountOptions string,
) ([]string, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if err := runid.Validate(projectDirectory); err != nil {
		return nil, fmt.Errorf("invalid project directory: %w", err)
	}
	memory := strconv.FormatInt(spec.MemoryBytes, 10)
	return []string{
		"run",
		"--rm",
		"--interactive",
		"--pull=never",
		"--label", "io.pisafe.run=" + spec.RunID,
		"--label", "io.pisafe.kind=" + kind,
		"--user", containerUser,
		"--read-only",
		"--cap-drop=all",
		"--security-opt=no-new-privileges",
		"--network=none",
		"--memory", memory,
		"--memory-swap", memory,
		"--pids-limit", strconv.Itoa(spec.PIDs),
		"--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=" + strconv.FormatInt(spec.TmpBytes, 10),
		"--mount", "type=bind,src=" + spec.WorkspacePath() +
			",dst=" + containerWorkRoot + ",nodev,nosuid" + mountOptions,
		"--workdir", containerWorkRoot,
		"--env", "HOME=/tmp",
		"--env", "GIT_TERMINAL_PROMPT=0",
		spec.ImageID,
	}, nil
}

func (spec Spec) CleanupStageArgs() ([]string, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	return []string{
		"exec",
		"--user", containerUser,
		spec.ContainerName(),
		"rm", "-rf", containerWorkRoot + "/stage",
	}, nil
}
