package lima

import (
	"strings"
	"testing"

	"github.com/mpizenberg/pisafe/internal/runssh"
)

func TestReverseForwardArgsBindOnlyTheBrokerAddress(t *testing.T) {
	gateway := runssh.Gateway{
		ConfigFile: "/Users/alice/.lima/pisafe/ssh.config",
		Alias:      "lima-pisafe",
	}
	args, err := reverseForwardArgs(gateway, 54321)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, expected := range []string{
		"-F /Users/alice/.lima/pisafe/ssh.config",
		"-N -T",
		"-o BatchMode=yes",
		"-o ExitOnForwardFailure=yes",
		"-o ControlMaster=no",
		"-R 192.0.2.1:18080:127.0.0.1:54321",
		"lima-pisafe",
	} {
		if !strings.Contains(joined, expected) {
			t.Errorf("args lack %q: %s", expected, joined)
		}
	}
	if strings.Contains(joined, "-L ") || strings.Contains(joined, "-D ") {
		t.Errorf("args contain a non-reverse forwarding: %s", joined)
	}
}

func TestReverseForwardArgsRejectUnsafeInputs(t *testing.T) {
	valid := runssh.Gateway{ConfigFile: "/tmp/ssh.config", Alias: "lima-pisafe"}
	for name, invalid := range map[string]struct {
		gateway runssh.Gateway
		port    int
	}{
		"relative config": {runssh.Gateway{ConfigFile: "ssh.config", Alias: "lima-pisafe"}, 1},
		"newline config":  {runssh.Gateway{ConfigFile: "/tmp/a\nb", Alias: "lima-pisafe"}, 1},
		"empty alias":     {runssh.Gateway{ConfigFile: "/tmp/ssh.config"}, 1},
		"space alias":     {runssh.Gateway{ConfigFile: "/tmp/ssh.config", Alias: "a b"}, 1},
		"zero port":       {valid, 0},
		"oversize port":   {valid, 70000},
	} {
		if _, err := reverseForwardArgs(invalid.gateway, invalid.port); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}
