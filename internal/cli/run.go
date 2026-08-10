package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/mpizenberg/pisafe/internal/broker"
	"github.com/mpizenberg/pisafe/internal/gitstage"
	"github.com/mpizenberg/pisafe/internal/hostnet"
	"github.com/mpizenberg/pisafe/internal/lima"
	"github.com/mpizenberg/pisafe/internal/providers"
	"github.com/mpizenberg/pisafe/internal/runctl"
	"github.com/mpizenberg/pisafe/internal/runimage"
	"github.com/mpizenberg/pisafe/internal/runssh"
	"github.com/mpizenberg/pisafe/internal/runstart"
	"github.com/mpizenberg/pisafe/internal/runstate"
)

const guestHelperEnvironment = "PISAFE_GUEST_HELPER"

// parseInputSelection reads the repeatable input flags. Credential-shaped
// paths need the separate unsafe flag, so approving one can never be a typo.
func parseInputSelection(args []string) (gitstage.InputSelection, error) {
	selection := gitstage.InputSelection{}
	for index := 0; index < len(args); index++ {
		var target *[]string
		switch args[index] {
		case "--include":
			target = &selection.Include
		case "--include-unsafe":
			target = &selection.Unsafe
		default:
			return gitstage.InputSelection{}, fmt.Errorf(
				"unknown run option %q; expected --include PATH or --include-unsafe PATH",
				args[index],
			)
		}
		index++
		if index == len(args) {
			return gitstage.InputSelection{}, fmt.Errorf("%s requires a path", args[index-1])
		}
		*target = append(*target, args[index])
	}
	return selection, nil
}

func runCreate(ctx context.Context, args []string, out io.Writer) error {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		return fmt.Errorf("pisafe run requires macOS on ARM64")
	}
	inputs, err := parseInputSelection(args)
	if err != nil {
		return err
	}
	artifacts, err := packagedRunArtifacts()
	if err != nil {
		return err
	}
	catalog, err := providers.Load(ctx)
	if err != nil {
		return err
	}
	prefixes, err := hostnet.OnLinkIPv4(ctx)
	if err != nil {
		return fmt.Errorf("discover host networks: %w", err)
	}
	stateRoot, err := runstate.DefaultRoot()
	if err != nil {
		return err
	}
	transport := lima.NewTransport()
	controller := runctl.New(
		transport,
		runstate.NewStore(stateRoot),
		runssh.NewStore(filepath.Join(stateRoot, "ssh")),
		inferenceConfig(catalog),
	)
	service := runstart.New(
		lima.NewManager(),
		runimage.NewInstaller(transport),
		controller,
		artifacts,
	)

	fmt.Fprintln(out, "Preparing isolated run...")
	result, err := service.Start(ctx, ".", prefixes, inputs)
	if err != nil {
		return err
	}
	return printRunResult(out, result, len(catalog) > 0)
}

// inferenceConfig keeps "no upstream is configured" a nil the controller can
// test, rather than an interface holding an empty catalog.
func inferenceConfig(catalog broker.Catalog) runctl.InferenceConfig {
	if len(catalog) == 0 {
		return nil
	}
	return catalog
}

// packagedRunArtifacts loads what the managed run image is built from. Both
// creating a run and applying one need it, because the guest helper inside the
// image must match the controller that drives it.
func packagedRunArtifacts() (runimage.Artifacts, error) {
	guestPath, err := packagedGuestPath()
	if err != nil {
		return runimage.Artifacts{}, err
	}
	artifacts, err := runimage.LoadPackagedArtifacts(guestPath)
	if err != nil {
		return runimage.Artifacts{}, fmt.Errorf(
			"load packaged Linux helper (build %s beside pisafe or set %s): %w",
			runimage.PackagedGuestName,
			guestHelperEnvironment,
			err,
		)
	}
	return artifacts, nil
}

func packagedGuestPath() (string, error) {
	if override := os.Getenv(guestHelperEnvironment); override != "" {
		return filepath.Clean(override), nil
	}
	executable, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate pisafe executable: %w", err)
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return "", fmt.Errorf("resolve pisafe executable: %w", err)
	}
	return filepath.Join(filepath.Dir(executable), runimage.PackagedGuestName), nil
}

func printRunResult(out io.Writer, result runstart.Result, inferenceConfigured bool) error {
	manifest := result.Manifest
	if manifest.SSH == nil {
		return fmt.Errorf("active run has no SSH connection")
	}
	sshCommand := "ssh -F " + shellQuote(manifest.SSH.ConfigFile) +
		" " + manifest.SSH.Alias
	fmt.Fprintf(
		out,
		"Run:       %s\nWorkspace: %s\nBranch:    %s\nSSH:       %s\n",
		manifest.RunID,
		manifest.Workspace(),
		strings.TrimPrefix(manifest.Snapshot.WorkRef, "refs/heads/"),
		sshCommand,
	)
	if result.Manifest.Snapshot.BaselineCommit != "" {
		fmt.Fprintln(out, "Baseline:  tracked working-tree changes were flattened into one commit")
	}
	if len(result.Included) != 0 {
		fmt.Fprintf(out, "Included:  %d selected input file(s)\n", len(result.Included))
		printNames(out, namedList{names: result.Included})
	}
	if len(result.Excluded.Untracked) != 0 || len(result.Excluded.Ignored) != 0 {
		fmt.Fprintf(
			out,
			"Excluded:  %d untracked, %d ignored path(s) stayed behind\n",
			len(result.Excluded.Untracked),
			len(result.Excluded.Ignored),
		)
		printNames(
			out,
			namedList{label: "untracked", names: result.Excluded.Untracked},
			namedList{label: "ignored", names: result.Excluded.Ignored},
		)
	}
	fmt.Fprintf(out, "Zed:       pisafe zed %s\n", manifest.RunID)
	if inferenceConfigured {
		fmt.Fprintln(out, "Pi:        keep `pisafe broker` running on this Mac to serve inference")
	} else {
		fmt.Fprintln(out, "Pi:        inference unavailable; run pisafe login, then pisafe broker")
	}
	return nil
}

type namedList struct {
	label string
	names []string
}

// printNames lists file names under a run summary line, keeping the output
// short enough to read whatever the repository holds.
func printNames(out io.Writer, lists ...namedList) {
	const maximumNames = 12
	total := 0
	for _, list := range lists {
		total += len(list.names)
	}
	printed := 0
	for _, list := range lists {
		prefix := "           "
		if list.label != "" {
			prefix += list.label + " "
		}
		for _, name := range list.names {
			if printed == maximumNames {
				fmt.Fprintf(out, "           ... and %d more\n", total-printed)
				return
			}
			fmt.Fprintf(out, "%s%q\n", prefix, name)
			printed++
		}
	}
}

func shellQuote(value string) string {
	if value != "" && !strings.ContainsAny(value, " \t\r\n'\"\\$`!") {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
