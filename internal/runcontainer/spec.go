// Package runcontainer defines the rootless Podman resources and hardened
// command line for one isolated run.
package runcontainer

import (
	"fmt"
	"regexp"
	"strconv"

	"github.com/mpizenberg/pisafe/internal/runid"
)

const (
	DefaultCPUs       = "2"
	DefaultMemory     = int64(4 * 1024 * 1024 * 1024)
	DefaultPIDs       = 512
	DefaultTemporary  = int64(1024 * 1024 * 1024)
	DefaultPersistent = int64(10 * 1024 * 1024 * 1024)
	DefaultWallClock  = int64(8 * 60 * 60)
	containerUser     = "1000:1000"
	containerWorkRoot = "/work"
	containerHome     = "/home/node"
	guestStorageRoot  = "/var/lib/pisafe/runs"
)

var imageIDPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

type Spec struct {
	RunID       string
	ImageID     string
	CPUs        string
	MemoryBytes int64
	PIDs        int
	TmpBytes    int64
	WallSeconds int64
}

func DefaultSpec(runID, imageID string) Spec {
	return Spec{
		RunID:       runID,
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

func (spec Spec) RunArgs() ([]string, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	memory := strconv.FormatInt(spec.MemoryBytes, 10)
	tmpSize := strconv.FormatInt(spec.TmpBytes, 10)
	return []string{
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
		"--workdir", containerWorkRoot,
		"--env", "HOME=" + containerHome,
		"--env", "GIT_TERMINAL_PROMPT=0",
		"--env", "PI_CODING_AGENT_DIR=" + containerHome + "/.pi/agent",
		"--env", "PI_SKIP_VERSION_CHECK=1",
		spec.ImageID,
		"pisafe-guest", "serve-ssh",
	}, nil
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
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	return []string{
		"exec",
		"--interactive",
		"--user", containerUser,
		spec.ContainerName(),
		"pisafe-guest", "configure-inference",
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
