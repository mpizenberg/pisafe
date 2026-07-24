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
	containerUser     = "1000:1000"
	containerWorkRoot = "/work"
	containerHome     = "/home/node"
)

var imageIDPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

type Spec struct {
	RunID       string
	ImageID     string
	CPUs        string
	MemoryBytes int64
	PIDs        int
	TmpBytes    int64
}

func DefaultSpec(runID, imageID string) Spec {
	return Spec{
		RunID:       runID,
		ImageID:     imageID,
		CPUs:        DefaultCPUs,
		MemoryBytes: DefaultMemory,
		PIDs:        DefaultPIDs,
		TmpBytes:    DefaultTemporary,
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
	return nil
}

func (spec Spec) ContainerName() string {
	return "pisafe-run-" + spec.RunID
}

func (spec Spec) WorkspaceVolume() string {
	return "pisafe-work-" + spec.RunID
}

func (spec Spec) HomeVolume() string {
	return "pisafe-home-" + spec.RunID
}

func (spec Spec) CreateVolumeArgs() ([][]string, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	return [][]string{
		{
			"volume", "create",
			"--label", "io.pisafe.run=" + spec.RunID,
			"--label", "io.pisafe.kind=workspace",
			spec.WorkspaceVolume(),
		},
		{
			"volume", "create",
			"--label", "io.pisafe.run=" + spec.RunID,
			"--label", "io.pisafe.kind=home",
			spec.HomeVolume(),
		},
	}, nil
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
		"--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=" + tmpSize,
		"--mount", "type=tmpfs,dst=/run,tmpfs-size=16777216,tmpfs-mode=0755,U=true",
		"--mount", "type=volume,src=" + spec.WorkspaceVolume() + ",dst=" + containerWorkRoot + ",nodev,nosuid,U=true",
		"--mount", "type=volume,src=" + spec.HomeVolume() + ",dst=" + containerHome + ",nodev,nosuid,U=true",
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
		"--mount", "type=volume,src=" + spec.HomeVolume() + ",dst=" + containerHome + ",nodev,nosuid,U=true",
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
