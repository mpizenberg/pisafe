package cli

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"runtime"
	"time"

	"github.com/mpizenberg/pisafe/internal/broker"
	"github.com/mpizenberg/pisafe/internal/hostnet"
	"github.com/mpizenberg/pisafe/internal/lima"
	"github.com/mpizenberg/pisafe/internal/runctl"
	"github.com/mpizenberg/pisafe/internal/runssh"
	"github.com/mpizenberg/pisafe/internal/runstate"
)

func runStop(ctx context.Context, runID string, out io.Writer) error {
	controller, err := prepareLifecycle(ctx)
	if err != nil {
		return err
	}
	manifest, err := controller.Stop(ctx, runID)
	if err != nil {
		return err
	}
	fmt.Fprintf(
		out,
		"Stopped %s; %s of active time remains.\n",
		runID,
		time.Duration(runstate.RemainingSeconds(manifest, time.Now()))*time.Second,
	)
	return nil
}

func runResume(ctx context.Context, runID string, out io.Writer) error {
	controller, err := prepareLifecycle(ctx)
	if err != nil {
		return err
	}
	manifest, err := controller.Resume(ctx, runID)
	if err != nil {
		return err
	}
	if manifest.SSH == nil {
		return fmt.Errorf("resumed run has no SSH connection")
	}
	fmt.Fprintf(
		out,
		"Resumed %s for up to %s.\nSSH: ssh -F %s %s\n",
		runID,
		time.Duration(runstate.RemainingSeconds(manifest, time.Now()))*time.Second,
		shellQuote(manifest.SSH.ConfigFile),
		manifest.SSH.Alias,
	)
	return nil
}

func runDiscard(ctx context.Context, runID string, out io.Writer) error {
	controller, err := prepareLifecycle(ctx)
	if err != nil {
		return err
	}
	if _, err := controller.Discard(ctx, runID); err != nil {
		return err
	}
	fmt.Fprintf(out, "Discarded %s; its container, workspace, home, and SSH key were removed.\n", runID)
	return nil
}

func prepareLifecycle(ctx context.Context) (runctl.Controller, error) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		return runctl.Controller{}, fmt.Errorf("pisafe lifecycle commands require macOS on ARM64")
	}
	provider, err := broker.FromEnvironment()
	if err != nil {
		return runctl.Controller{}, err
	}
	prefixes, err := hostnet.OnLinkIPv4(ctx)
	if err != nil {
		return runctl.Controller{}, fmt.Errorf("discover host networks: %w", err)
	}
	prefixStrings := make([]string, 0, len(prefixes))
	for _, prefix := range prefixes {
		prefixStrings = append(prefixStrings, prefix.String())
	}
	if err := lima.NewManager().Start(ctx, prefixStrings); err != nil {
		return runctl.Controller{}, err
	}
	root, err := runstate.DefaultRoot()
	if err != nil {
		return runctl.Controller{}, err
	}
	return runctl.New(
		lima.NewTransport(),
		runstate.NewStore(root),
		runssh.NewStore(filepath.Join(root, "ssh")),
		inferenceConfig(provider),
	), nil
}
