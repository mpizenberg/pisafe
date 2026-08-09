package cli

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"runtime"
	"time"

	"github.com/mpizenberg/pisafe/internal/gitstage"
	"github.com/mpizenberg/pisafe/internal/hostnet"
	"github.com/mpizenberg/pisafe/internal/lima"
	"github.com/mpizenberg/pisafe/internal/profile"
	"github.com/mpizenberg/pisafe/internal/providers"
	"github.com/mpizenberg/pisafe/internal/runctl"
	"github.com/mpizenberg/pisafe/internal/runimage"
	"github.com/mpizenberg/pisafe/internal/runssh"
	"github.com/mpizenberg/pisafe/internal/runstate"
)

func runStop(ctx context.Context, runID string, out io.Writer) error {
	controller, err := prepareUnverified(ctx)
	if err != nil {
		return err
	}
	// Stopping keeps what the run cached, which for a large dependency tree is
	// a copy that takes a while and says nothing while it runs.
	fmt.Fprintf(out, "Stopping %s...\n", runID)
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
	// Stopping clears the record's error, so anything left on it happened while
	// publishing the run's caches, which is not a reason to fail a stop.
	if manifest.LastError != "" {
		fmt.Fprintf(out, "Warning: %s\n", manifest.LastError)
	}
	transport := lima.NewTransport()
	notifySelfInstalled(ctx, transport, runID, out)
	// The stopped run's own image is enough to ask npm a question, which keeps
	// the check off the path that installs and verifies the current one.
	notifyExtensionUpdates(ctx, transport, manifest.Image, out)
	return nil
}

// notifySelfInstalled reports what the run installed into its own package
// store, which nothing else will: a run's home is reclaimed with the run. It
// is an offer in the same sense an update is — pisafe names the command and
// applies nothing, because installing into the profile is a decision about
// every later run of every project.
func notifySelfInstalled(
	ctx context.Context,
	transport lima.Transport,
	runID string,
	out io.Writer,
) {
	printSelfInstalled(out, runID, transport.ReadSelfInstalled(ctx, runID))
}

// printSelfInstalled names what a run installed and what would keep it. Every
// source was chosen inside the run, so it is quoted rather than written to the
// terminal as it stands, and a source pisafe cannot pin is still reported: the
// run had it, and saying nothing would be the surprise.
func printSelfInstalled(out io.Writer, runID string, installed []profile.SelfInstalled) {
	if len(installed) == 0 {
		return
	}
	fmt.Fprintf(out, "%s installed %d package(s) for itself:\n", runID, len(installed))
	for _, entry := range installed {
		keep := "pisafe extension install " + entry.Name
		if entry.Name == "" {
			keep = "pisafe can only keep an npm package"
		}
		fmt.Fprintf(out, "  %q\n      %s\n", entry.Source, keep)
	}
	fmt.Fprintln(out, "They went with the run; nothing is kept unless you install it.")
}

func runResume(ctx context.Context, runID string, out io.Writer) error {
	controller, err := prepareLifecycle(ctx)
	if err != nil {
		return err
	}
	// Resume rebuilds the container over storage that survived, from the image
	// the manifest pinned. A VM recreated since that run started has the
	// storage but not the image.
	if _, err := ensureManagedRunImage(ctx); err != nil {
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

func runDiscard(ctx context.Context, runID string, out io.Writer) error {
	controller, err := prepareUnverified(ctx)
	if err != nil {
		return err
	}
	if err := controller.Discard(ctx, runID); err != nil {
		return err
	}
	forgetZedConnection(runID, out)
	fmt.Fprintf(
		out,
		"Discarded %s; its container, workspace, home, SSH key, and record were removed.\n",
		runID,
	)
	return nil
}

// prepareUnverified builds the controller for a command that starts no run:
// one that reads or writes a run's workspace from outside it, or one that only
// ends or removes what a run already holds. Such a command is not held to the
// VM's boundary records, for the reasons on lima.Manager.StartUnverified.
func prepareUnverified(ctx context.Context) (runctl.Controller, error) {
	controller, err := newController(ctx)
	if err != nil {
		return runctl.Controller{}, err
	}
	if err := lima.NewManager().StartUnverified(ctx); err != nil {
		return runctl.Controller{}, err
	}
	return controller, nil
}

// prepareInspection adds the current managed run image to that controller.
// Every command that reads a run's workspace from outside it needs that image,
// because the guest helper inside it must match this controller.
func prepareInspection(ctx context.Context) (runctl.Controller, string, error) {
	controller, err := prepareUnverified(ctx)
	if err != nil {
		return runctl.Controller{}, "", err
	}
	image, err := ensureManagedRunImage(ctx)
	if err != nil {
		return runctl.Controller{}, "", err
	}
	return controller, image, nil
}

// ensureManagedRunImage puts this controller's run image back in the VM. The
// image store is on the instance's disk while a run's storage is on the state
// disk, so a run outlives the image it was built from every time the VM is
// recreated, and nothing can be started over that storage until it is back.
func ensureManagedRunImage(ctx context.Context) (string, error) {
	artifacts, err := packagedRunArtifacts()
	if err != nil {
		return "", err
	}
	image, err := runimage.NewInstaller(lima.NewTransport()).Ensure(ctx, artifacts)
	if err != nil {
		return "", fmt.Errorf("install managed run image: %w", err)
	}
	return image.ImageID, nil
}

func prepareLifecycle(ctx context.Context) (runctl.Controller, error) {
	controller, err := newController(ctx)
	if err != nil {
		return runctl.Controller{}, err
	}
	if err := startBoundary(ctx); err != nil {
		return runctl.Controller{}, err
	}
	return controller, nil
}

// newController builds what every command drives the VM through. It reaches
// nothing itself, so the boundary a command is held to stays its caller's
// decision.
func newController(ctx context.Context) (runctl.Controller, error) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		return runctl.Controller{}, fmt.Errorf("pisafe lifecycle commands require macOS on ARM64")
	}
	catalog, err := providers.Load(ctx)
	if err != nil {
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
		inferenceConfig(catalog),
	), nil
}

// startBoundary brings the VM up for a command that may start a run, and holds
// it to the boundary that run would get. The networks the firewall is built
// around are discovered every time, because the Mac may have joined a different
// one since the VM was last started.
func startBoundary(ctx context.Context) error {
	prefixes, err := hostnet.OnLinkIPv4(ctx)
	if err != nil {
		return fmt.Errorf("discover host networks: %w", err)
	}
	prefixStrings := make([]string, 0, len(prefixes))
	for _, prefix := range prefixes {
		prefixStrings = append(prefixStrings, prefix.String())
	}
	return lima.NewManager().Start(ctx, prefixStrings)
}
