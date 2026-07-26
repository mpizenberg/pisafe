package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"runtime"
	"time"

	"github.com/mpizenberg/pisafe/internal/chatgpt"
	"github.com/mpizenberg/pisafe/internal/gitstage"
	"github.com/mpizenberg/pisafe/internal/hostnet"
	"github.com/mpizenberg/pisafe/internal/lima"
	"github.com/mpizenberg/pisafe/internal/runctl"
	"github.com/mpizenberg/pisafe/internal/runimage"
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

func runApply(ctx context.Context, runID string, out io.Writer) error {
	controller, err := prepareLifecycle(ctx)
	if err != nil {
		return err
	}
	artifacts, err := packagedRunArtifacts()
	if err != nil {
		return err
	}
	image, err := runimage.NewInstaller(lima.NewTransport()).Ensure(ctx, artifacts)
	if err != nil {
		return fmt.Errorf("install managed run image: %w", err)
	}
	manifest, result, err := controller.Apply(ctx, runID, image.ImageID)
	if err != nil {
		if errors.Is(err, gitstage.ErrApplyNeedsReconciliation) {
			return fmt.Errorf(
				"%w\nThe recorded plan is kept: rerun pisafe apply %s once the ref is resolved",
				err,
				runID,
			)
		}
		return err
	}
	printApplyResult(out, manifest, result)
	return nil
}

func printApplyResult(out io.Writer, manifest runstate.Manifest, result gitstage.ApplyResult) {
	fmt.Fprintf(out, "Imported:  %s\nTip:       %s\n", result.Branch, result.Tip)
	for _, submodule := range result.Submodules {
		if submodule.Branch == "" {
			fmt.Fprintf(out, "Submodule: %s unchanged\n", submodule.Path)
			continue
		}
		fmt.Fprintf(
			out,
			"Submodule: %s imported as %s (%s)\n",
			submodule.Path,
			submodule.Branch,
			submodule.Tip,
		)
	}
	if result.FinalCommit != "" {
		fmt.Fprintln(out, "Final:     uncommitted tracked changes became one labelled commit")
	}
	if len(result.Untracked) != 0 {
		fmt.Fprintf(
			out,
			"Left:      %d untracked file(s) stayed in the run\n",
			len(result.Untracked),
		)
		printNames(out, namedList{names: result.Untracked})
	}
	fmt.Fprintf(
		out,
		"Next:      git log %s\n           %s keeps its workspace until pisafe discard %s\n",
		result.Branch,
		manifest.RunID,
		manifest.RunID,
	)
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
	provider, err := chatgpt.LoadProvider(ctx)
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
