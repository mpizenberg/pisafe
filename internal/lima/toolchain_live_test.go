package lima_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mpizenberg/pisafe/internal/lima"
)

// imageToolchain is every binary pisafe puts in the run image on purpose. A run
// is read-only, unprivileged, and mounts the profile read-only, so a tool that
// is not here cannot be obtained from inside one at all.
var imageToolchain = []string{
	"node", "npm", "pnpm", "corepack", "git",
	"curl", "jq", "unzip", "rg", "fd", "python", "python3", "uv", "uvx",
}

// TestLiveTheToolchainIsReachableAndNeverShadowed covers what naming PATH costs
// and buys. Restating the image's own search path is a copy of something the
// base image owns, so this fails if a base bump moves any of it; and what a run
// writes into its home directory is reachable without being able to decide what
// git means for the rest of the run.
func TestLiveTheToolchainIsReachableAndNeverShadowed(t *testing.T) {
	if os.Getenv("PISAFE_LIVE_LIMA") != "1" {
		t.Skip("set PISAFE_LIVE_LIMA=1 to test the dedicated VM")
	}
	imageID := liveImageID(t)
	ensureLiveVM(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	vm := lima.New()

	stamp := time.Now().UTC().Format("20060102150405")
	projectKey := liveProject(t, vm, "livetools")
	spec := liveRun(t, ctx, vm, projectKey, imageID, "livetools-"+stamp)

	for _, tool := range imageToolchain {
		found := inContainer(t, ctx, vm, spec,
			"command -v "+tool+" || echo MISSING")
		if strings.Contains(found, "MISSING") {
			t.Errorf("%s is not on a run's PATH", tool)
		}
	}

	// uv installs an executable here, so a run that cannot reach it gets a tool
	// it can install and cannot invoke.
	invoked := inContainer(t, ctx, vm, spec, `
mkdir -p "$HOME/.local/bin"
printf '#!/bin/sh\necho from-home\n' >"$HOME/.local/bin/probe-tool"
chmod +x "$HOME/.local/bin/probe-tool"
probe-tool
`)
	if invoked != "from-home" {
		t.Errorf("an executable in the run's home was not reachable: %q", invoked)
	}

	// The same directory is writable by everything in the run, so it decides
	// nothing that the image already answers.
	shadowed := inContainer(t, ctx, vm, spec, `
printf '#!/bin/sh\necho shadowed\n' >"$HOME/.local/bin/git"
chmod +x "$HOME/.local/bin/git"
git --version
`)
	if !strings.HasPrefix(shadowed, "git version") {
		t.Errorf("a run's own home shadowed the image's git: %q", shadowed)
	}
}
