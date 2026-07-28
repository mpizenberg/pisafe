package runcontainer

import (
	"strings"
	"testing"

	"github.com/mpizenberg/pisafe/internal/gitstage"
)

const testProjectKey = "project-3f9c2a1b"

const testImageID = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestRunArgsAreHardenedAndImmutable(t *testing.T) {
	spec := DefaultSpec("run-123", testProjectKey, testImageID)
	args, err := spec.RunArgs()
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, required := range []string{
		"--pull=never",
		"--user 1000:1000",
		"--read-only",
		"--cap-drop=all",
		"--security-opt=no-new-privileges",
		"--network=pasta",
		"--dns=1.1.1.1",
		"--dns=9.9.9.9",
		"--cpus 2",
		"--memory 4294967296",
		"--memory-swap 4294967296",
		"--pids-limit 512",
		"--timeout 28800",
		"type=tmpfs,dst=/run,tmpfs-size=16777216,tmpfs-mode=0755,U=true",
		"type=bind,src=/var/lib/pisafe/runs/run-123/workspace,dst=/work,nodev,nosuid",
		"type=bind,src=/var/lib/pisafe/runs/run-123/home,dst=/home/node,nodev,nosuid",
		testImageID,
		"pisafe-guest serve-ssh",
	} {
		if !strings.Contains(joined, required) {
			t.Errorf("run args lack %q:\n%s", required, joined)
		}
	}
	for _, forbidden := range []string{
		"--privileged",
		"/var/run/docker.sock",
		"podman.sock",
		"/Users",
		"SSH_AUTH_SOCK",
	} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("run args contain forbidden value %q", forbidden)
		}
	}
	if got := strings.Join(args[len(args)-2:], " "); got != "pisafe-guest serve-ssh" {
		t.Fatalf("container command = %q", got)
	}
}

func TestStorageAndMaterializeArgsAreRunScoped(t *testing.T) {
	spec := DefaultSpec("run-123", testProjectKey, testImageID)
	if spec.WorkspacePath() != "/var/lib/pisafe/runs/run-123/workspace" ||
		spec.HomePath() != "/var/lib/pisafe/runs/run-123/home" {
		t.Fatalf("storage paths = %q %q", spec.WorkspacePath(), spec.HomePath())
	}
	materialize, err := spec.MaterializeArgs("project")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(materialize, " "); !strings.Contains(
		got,
		"pisafe-guest materialize /work/stage /work/project",
	) {
		t.Fatalf("materialize args = %q", got)
	}
	configureSSH, err := spec.ConfigureSSHArgs()
	if err != nil {
		t.Fatal(err)
	}
	configureJoined := strings.Join(configureSSH, " ")
	for _, expected := range []string{
		"--rm",
		"--interactive",
		"--network=none",
		"--user 1000:1000",
		"--read-only",
		"--cap-drop=all",
		"--security-opt=no-new-privileges",
		"type=bind,src=/var/lib/pisafe/runs/run-123/home,dst=/home/node,nodev,nosuid",
		testImageID,
		"pisafe-guest configure-ssh",
	} {
		if !strings.Contains(configureJoined, expected) {
			t.Errorf("SSH init args lack %q:\n%s", expected, configureJoined)
		}
	}
}

func TestPrepareApplyArgsRunWithoutNetworkOrRunHome(t *testing.T) {
	spec := DefaultSpec("run-123", testProjectKey, testImageID)
	args, err := spec.PrepareApplyArgs("project", gitstage.KeepBaseline)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, expected := range []string{
		"--rm",
		"--interactive",
		"--network=none",
		"--user 1000:1000",
		"--read-only",
		"--cap-drop=all",
		"--security-opt=no-new-privileges",
		"type=bind,src=/var/lib/pisafe/runs/run-123/workspace,dst=/work,nodev,nosuid",
		testImageID,
		"pisafe-guest prepare-apply keep /work/project /work/apply",
	} {
		if !strings.Contains(joined, expected) {
			t.Errorf("apply args lack %q:\n%s", expected, joined)
		}
	}
	for _, forbidden := range []string{
		spec.HomePath(),
		spec.ContainerName(),
		"--timeout",
		"--detach",
	} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("apply args unexpectedly contain %q:\n%s", forbidden, joined)
		}
	}
	if _, err := spec.PrepareApplyArgs("../project", gitstage.KeepBaseline); err == nil {
		t.Fatal("unsafe project directory was accepted")
	}
	if _, err := spec.PrepareApplyArgs("project", "replay"); err == nil {
		t.Fatal("unknown baseline choice was accepted")
	}
}

// A report must not be able to alter what it reports, so diff gets the
// workspace read-only where apply, which commits, gets it writable.
func TestDiffArgsMountTheWorkspaceReadOnly(t *testing.T) {
	spec := DefaultSpec("run-123", testProjectKey, testImageID)
	args, err := spec.DiffArgs("project")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, expected := range []string{
		"--network=none",
		"type=bind,src=/var/lib/pisafe/runs/run-123/workspace,dst=/work,nodev,nosuid,ro",
		"pisafe-guest diff /work/project",
	} {
		if !strings.Contains(joined, expected) {
			t.Errorf("diff args lack %q:\n%s", expected, joined)
		}
	}
	if _, err := spec.DiffArgs("../project"); err == nil {
		t.Fatal("unsafe project directory was accepted")
	}
}

func TestExportArgsStreamOneRequestedPathReadOnly(t *testing.T) {
	spec := DefaultSpec("run-123", testProjectKey, testImageID)
	args, err := spec.ExportArgs("project", "./dist/../dist")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, expected := range []string{
		"--network=none",
		"type=bind,src=/var/lib/pisafe/runs/run-123/workspace,dst=/work,nodev,nosuid,ro",
		"pisafe-guest export /work/project dist",
	} {
		if !strings.Contains(joined, expected) {
			t.Errorf("export args lack %q:\n%s", expected, joined)
		}
	}
	for name, requested := range map[string]string{
		"absolute":        "/etc/passwd",
		"climbing":        "../../etc/passwd",
		"whole workspace": ".",
	} {
		if _, err := spec.ExportArgs("project", requested); err == nil {
			t.Errorf("%s path was accepted", name)
		}
	}
}

func TestSpecRejectsMutableImageAndUnsafeNames(t *testing.T) {
	for _, spec := range []Spec{
		DefaultSpec("../escape", testProjectKey, testImageID),
		DefaultSpec("safe", testProjectKey, "localhost/pisafe-run:latest"),
	} {
		if _, err := spec.RunArgs(); err == nil {
			t.Fatalf("RunArgs(%#v) unexpectedly succeeded", spec)
		}
	}
	spec := DefaultSpec("safe", testProjectKey, testImageID)
	if _, err := spec.MaterializeArgs("../project"); err == nil {
		t.Fatal("unsafe project directory was accepted")
	}
}

func TestSharedLayersKeepTheProjectSideReadOnlyToTheRun(t *testing.T) {
	spec := DefaultSpec("run-123", testProjectKey, testImageID)
	args, err := spec.RunArgs()
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	// The lower layer is the project's and the upper is the run's, which is
	// what stops one run's install or transcript from reaching another's.
	for _, expected := range []string{
		"--volume /var/lib/pisafe/projects/project-3f9c2a1b/cache:/cache:O," +
			"upperdir=/var/lib/pisafe/runs/run-123/overlay/cache/upper," +
			"workdir=/var/lib/pisafe/runs/run-123/overlay/cache/work",
		"--volume /var/lib/pisafe/projects/project-3f9c2a1b/sessions:/sessions:O," +
			"upperdir=/var/lib/pisafe/runs/run-123/overlay/sessions/upper," +
			"workdir=/var/lib/pisafe/runs/run-123/overlay/sessions/work",
		"PI_CODING_AGENT_SESSION_DIR=/sessions",
		"npm_config_cache=/cache/npm",
		"npm_config_logs_dir=/home/node/.npm/_logs",
		"npm_config_update_notifier=false",
	} {
		if !strings.Contains(joined, expected) {
			t.Errorf("run args lack %q:\n%s", expected, joined)
		}
	}
	// Inspecting a run reports on its workspace and must not reach, or hold
	// open, state shared with every other run of the project.
	for _, inspection := range []func() ([]string, error){
		func() ([]string, error) { return spec.DiffArgs("project") },
		func() ([]string, error) { return spec.ExportArgs("project", "file.txt") },
		func() ([]string, error) { return spec.PrepareApplyArgs("project", gitstage.KeepBaseline) },
		spec.ConfigureSSHArgs,
	} {
		args, err := inspection()
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(strings.Join(args, " "), "/var/lib/pisafe/projects") {
			t.Errorf("inspection container mounts project state:\n%s", strings.Join(args, " "))
		}
	}
}

func TestSpecRefusesAProjectKeyItCannotAddress(t *testing.T) {
	for _, key := range []string{"", "../escape", "key/sub"} {
		spec := DefaultSpec("run-123", key, testImageID)
		if _, err := spec.RunArgs(); err == nil {
			t.Errorf("project key %q was accepted", key)
		}
	}
}
