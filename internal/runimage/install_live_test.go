package runimage_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/mpizenberg/pisafe/internal/hostnet"
	"github.com/mpizenberg/pisafe/internal/lima"
	"github.com/mpizenberg/pisafe/internal/runimage"
)

func TestLiveInstallAndReuseManagedImage(t *testing.T) {
	if os.Getenv("PISAFE_LIVE_LIMA") != "1" {
		t.Skip("set PISAFE_LIVE_LIMA=1 to install the managed run image")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	prefixes, err := hostnet.OnLinkIPv4(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := lima.NewManager().Start(ctx, prefixes); err != nil {
		t.Fatal(err)
	}

	guestPath := filepath.Join(t.TempDir(), "pisafe-guest")
	command := exec.CommandContext(
		ctx,
		"go", "build", "-trimpath", "-buildvcs=false",
		"-o", guestPath, "../../cmd/pisafe-guest",
	)
	command.Env = append(
		os.Environ(),
		"CGO_ENABLED=0",
		"GOOS=linux",
		"GOARCH=arm64",
		"GOCACHE="+filepath.Join(t.TempDir(), "go-cache"),
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build Linux guest helper: %v\n%s", err, output)
	}
	artifacts, err := runimage.LoadPackagedArtifacts(guestPath)
	if err != nil {
		t.Fatal(err)
	}

	installer := runimage.NewInstaller(lima.NewTransport())
	first, err := installer.Ensure(ctx, artifacts)
	if err != nil {
		t.Fatal(err)
	}
	if first == "" {
		t.Fatal("installer returned no immutable image ID")
	}
	second, err := installer.Ensure(ctx, artifacts)
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("managed image was not reused: first=%q second=%q", first, second)
	}
}
