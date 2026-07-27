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

func runDiff(ctx context.Context, runID string, out io.Writer) error {
	controller, imageID, err := prepareInspection(ctx)
	if err != nil {
		return err
	}
	diff, err := controller.Diff(ctx, runID, imageID)
	if err != nil {
		return err
	}
	printRunDiff(out, diff)
	return nil
}

// printRunDiff renders a report a run produced, so every name and subject it
// contains is quoted rather than written to the terminal as it stands.
func printRunDiff(out io.Writer, diff gitstage.RunDiff) {
	for _, repository := range diff.Repositories {
		scope := "Repository"
		if repository.Path != "" {
			scope = "Submodule"
		}
		fmt.Fprintf(out, "%s: %s\n", scope, repositoryLabel(repository.Path))
		fmt.Fprintf(
			out,
			"  Commits: %d since %s\n",
			repository.CommitTotal,
			shortCommit(repository.Base),
		)
		for _, commit := range repository.Commits {
			fmt.Fprintf(out, "    %s %q\n", shortCommit(commit.Commit), commit.Subject)
		}
		printMore(out, len(repository.Commits), repository.CommitTotal)
		fmt.Fprintf(out, "  Changed: %d file(s) against the run's starting state\n", repository.FileTotal)
		for _, file := range repository.Files {
			fmt.Fprintf(out, "    %s %q\n", changeCounts(file), file.Path)
		}
		printMore(out, len(repository.Files), repository.FileTotal)
		if repository.UntrackedTotal == 0 {
			continue
		}
		fmt.Fprintf(out, "  Untracked: %d file(s), which apply leaves behind\n", repository.UntrackedTotal)
		for _, name := range repository.Untracked {
			fmt.Fprintf(out, "    %q\n", name)
		}
		printMore(out, len(repository.Untracked), repository.UntrackedTotal)
	}
}

func repositoryLabel(path string) string {
	if path == "" {
		return "superproject"
	}
	return fmt.Sprintf("%q", path)
}

func shortCommit(commit string) string {
	if len(commit) <= 12 {
		return commit
	}
	return commit[:12]
}

// changeCounts renders one path's line counts. Git reports no line count at all
// for a binary file, which DiffFile carries as -1.
func changeCounts(file gitstage.DiffFile) string {
	if file.Insertions < 0 || file.Deletions < 0 {
		return "binary"
	}
	return fmt.Sprintf("+%d/-%d", file.Insertions, file.Deletions)
}

func printMore(out io.Writer, shown, total int) {
	if total > shown {
		fmt.Fprintf(out, "    ... and %d more\n", total-shown)
	}
}

func runApply(ctx context.Context, runID string, out io.Writer) error {
	controller, imageID, err := prepareInspection(ctx)
	if err != nil {
		return err
	}
	manifest, result, err := controller.Apply(ctx, runID, imageID)
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
	if err := controller.Discard(ctx, runID); err != nil {
		return err
	}
	fmt.Fprintf(
		out,
		"Discarded %s; its container, workspace, home, SSH key, and record were removed.\n",
		runID,
	)
	return nil
}

// prepareInspection adds the current managed run image to the lifecycle
// controller. Every command that reads a run's workspace from outside it needs
// that image, because the guest helper inside it must match this controller.
func prepareInspection(ctx context.Context) (runctl.Controller, string, error) {
	controller, err := prepareLifecycle(ctx)
	if err != nil {
		return runctl.Controller{}, "", err
	}
	artifacts, err := packagedRunArtifacts()
	if err != nil {
		return runctl.Controller{}, "", err
	}
	image, err := runimage.NewInstaller(lima.NewTransport()).Ensure(ctx, artifacts)
	if err != nil {
		return runctl.Controller{}, "", fmt.Errorf("install managed run image: %w", err)
	}
	return controller, image.ImageID, nil
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
