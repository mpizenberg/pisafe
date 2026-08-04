package cli

import (
	"strings"
	"testing"

	"github.com/mpizenberg/pisafe/internal/runstate"
)

// TestParseForwardRequestTellsPortsFromRunNames is why the command needs no
// flag to separate them: a port cannot be mistaken for a run name, so ports may
// be listed in any order and the run named or left out.
func TestParseForwardRequestTellsPortsFromRunNames(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		args  []string
		runID string
		ports []forwardPort
	}{
		{
			name:  "one port and no run",
			args:  []string{"5173"},
			ports: []forwardPort{{local: 5173, remote: 5173}},
		},
		{
			name:  "a named run and two ports",
			args:  []string{"demo-20260804-101010-abcdef012345", "5173", "8080"},
			runID: "demo-20260804-101010-abcdef012345",
			ports: []forwardPort{{local: 5173, remote: 5173}, {local: 8080, remote: 8080}},
		},
		{
			name:  "a port moved to a free local one",
			args:  []string{"3000:8080"},
			ports: []forwardPort{{local: 3000, remote: 8080}},
		},
		{
			name:  "a run named after its ports",
			args:  []string{"5173", "demo-20260804-101010-abcdef012345"},
			runID: "demo-20260804-101010-abcdef012345",
			ports: []forwardPort{{local: 5173, remote: 5173}},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			request, err := parseForwardRequest(testCase.args)
			if err != nil {
				t.Fatal(err)
			}
			if request.runID != testCase.runID {
				t.Errorf("run = %q, want %q", request.runID, testCase.runID)
			}
			if len(request.ports) != len(testCase.ports) {
				t.Fatalf("ports = %v, want %v", request.ports, testCase.ports)
			}
			for index, port := range request.ports {
				if port != testCase.ports[index] {
					t.Errorf("port %d = %v, want %v", index, port, testCase.ports[index])
				}
			}
		})
	}
}

func TestParseForwardRequestRefusesWhatItCannotCarry(t *testing.T) {
	for _, testCase := range []struct {
		name string
		args []string
	}{
		{name: "no port at all", args: []string{}},
		{name: "a run and no port", args: []string{"demo-20260804-101010-abcdef012345"}},
		{name: "two run names", args: []string{"first-run", "second-run", "5173"}},
		{name: "a port out of range", args: []string{"70000"}},
		{name: "port zero", args: []string{"0"}},
		{name: "an option", args: []string{"--all", "5173"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := parseForwardRequest(testCase.args); err == nil {
				t.Fatalf("%v was accepted", testCase.args)
			}
		})
	}
}

// TestForwardArgvBindsEveryPortToThisMacsLoopback pins what the command asks
// ssh for: a listener no other machine can reach, carried to the run's own
// loopback, over a connection whose config otherwise clears every forwarding.
func TestForwardArgvBindsEveryPortToThisMacsLoopback(t *testing.T) {
	manifest := runstate.Manifest{SSH: &runstate.SSHConnection{
		Alias:      "pisafe-demo",
		ConfigFile: "/tmp/pisafe/demo/ssh.config",
	}}
	argv := forwardArgv(manifest, []forwardPort{
		{local: 5173, remote: 5173},
		{local: 3000, remote: 8080},
	})

	joined := strings.Join(argv, " ")
	for _, expected := range []string{
		"-F /tmp/pisafe/demo/ssh.config",
		"-N -T",
		"-o ClearAllForwardings=no",
		"-o ExitOnForwardFailure=yes",
		"-L 127.0.0.1:5173:127.0.0.1:5173",
		"-L 127.0.0.1:3000:127.0.0.1:8080",
	} {
		if !strings.Contains(joined, expected) {
			t.Errorf("forward argv lacks %q: %s", expected, joined)
		}
	}
	if argv[len(argv)-1] != "pisafe-demo" {
		t.Errorf("forward argv does not end at the run: %s", joined)
	}
	// A remote forward would put something of this Mac's in front of the run.
	if strings.Contains(joined, "-R") || strings.Contains(joined, "-D") {
		t.Errorf("forward argv asks for more than a local forward: %s", joined)
	}
}
