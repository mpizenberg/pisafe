package runcontainer

import (
	"slices"
	"strings"
	"testing"
)

const testImageID = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestRunArgsAreHardenedAndImmutable(t *testing.T) {
	spec := DefaultSpec("run-123", testImageID)
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
		"type=tmpfs,dst=/run,tmpfs-size=16777216,tmpfs-mode=0755,U=true",
		"type=volume,src=pisafe-work-run-123,dst=/work,nodev,nosuid,U=true",
		"type=volume,src=pisafe-home-run-123,dst=/home/node,nodev,nosuid,U=true",
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

func TestVolumeAndMaterializeArgsAreRunScoped(t *testing.T) {
	spec := DefaultSpec("run-123", testImageID)
	volumes, err := spec.CreateVolumeArgs()
	if err != nil {
		t.Fatal(err)
	}
	if len(volumes) != 2 {
		t.Fatalf("volumes = %#v", volumes)
	}
	if !slices.Contains(volumes[0], "pisafe-work-run-123") ||
		!slices.Contains(volumes[1], "pisafe-home-run-123") {
		t.Fatalf("volumes = %#v", volumes)
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
		"type=volume,src=pisafe-home-run-123,dst=/home/node,nodev,nosuid,U=true",
		testImageID,
		"pisafe-guest configure-ssh",
	} {
		if !strings.Contains(configureJoined, expected) {
			t.Errorf("SSH init args lack %q:\n%s", expected, configureJoined)
		}
	}
}

func TestSpecRejectsMutableImageAndUnsafeNames(t *testing.T) {
	for _, spec := range []Spec{
		DefaultSpec("../escape", testImageID),
		DefaultSpec("safe", "localhost/pisafe-run:latest"),
	} {
		if _, err := spec.RunArgs(); err == nil {
			t.Fatalf("RunArgs(%#v) unexpectedly succeeded", spec)
		}
	}
	spec := DefaultSpec("safe", testImageID)
	if _, err := spec.MaterializeArgs("../project"); err == nil {
		t.Fatal("unsafe project directory was accepted")
	}
}
