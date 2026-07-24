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
	"github.com/mpizenberg/pisafe/internal/chatgpt"
	"github.com/mpizenberg/pisafe/internal/gitstage"
	"github.com/mpizenberg/pisafe/internal/hostnet"
	"github.com/mpizenberg/pisafe/internal/lima"
	"github.com/mpizenberg/pisafe/internal/runctl"
	"github.com/mpizenberg/pisafe/internal/runimage"
	"github.com/mpizenberg/pisafe/internal/runssh"
	"github.com/mpizenberg/pisafe/internal/runstart"
	"github.com/mpizenberg/pisafe/internal/runstate"
)

const guestHelperEnvironment = "PISAFE_GUEST_HELPER"

func runCreate(ctx context.Context, out io.Writer) error {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		return fmt.Errorf("pisafe run requires macOS on ARM64")
	}
	guestPath, err := packagedGuestPath()
	if err != nil {
		return err
	}
	artifacts, err := runimage.LoadPackagedArtifacts(guestPath)
	if err != nil {
		return fmt.Errorf(
			"load packaged Linux helper (build pisafe-guest-linux-arm64 beside pisafe or set %s): %w",
			guestHelperEnvironment,
			err,
		)
	}
	provider, err := chatgpt.LoadProvider(ctx)
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
		inferenceConfig(provider),
	)
	service := runstart.New(
		lima.NewManager(),
		runimage.NewInstaller(transport),
		controller,
		artifacts,
	)

	fmt.Fprintln(out, "Preparing isolated run...")
	result, err := service.Start(ctx, ".", prefixes)
	if err != nil {
		return err
	}
	return printRunResult(out, result, provider != nil)
}

// inferenceConfig avoids handing runctl a non-nil interface wrapping a nil
// provider when no upstream is configured.
func inferenceConfig(provider *broker.Provider) runctl.InferenceConfig {
	if provider == nil {
		return nil
	}
	return provider
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
	return filepath.Join(filepath.Dir(executable), "pisafe-guest-linux-arm64"), nil
}

func printRunResult(out io.Writer, result runstart.Result, inferenceConfigured bool) error {
	manifest := result.Manifest
	if manifest.SSH == nil {
		return fmt.Errorf("active run has no SSH connection")
	}
	sshCommand := "ssh -F " + shellQuote(manifest.SSH.ConfigFile) +
		" " + manifest.SSH.Alias
	if _, err := fmt.Fprintf(
		out,
		"Run:       %s\nWorkspace: %s\nBranch:    %s\nSSH:       %s\n",
		manifest.RunID,
		manifest.Workspace,
		strings.TrimPrefix(manifest.Snapshot.WorkRef, "refs/heads/"),
		sshCommand,
	); err != nil {
		return err
	}
	if result.Manifest.Snapshot.BaselineCommit != "" {
		fmt.Fprintln(out, "Baseline:  tracked working-tree changes were flattened into one commit")
	}
	if len(result.Excluded.Untracked) != 0 || len(result.Excluded.Ignored) != 0 {
		fmt.Fprintf(
			out,
			"Excluded:  %d untracked, %d ignored file(s); none were copied\n",
			len(result.Excluded.Untracked),
			len(result.Excluded.Ignored),
		)
		printExcludedNames(out, result.Excluded)
	}
	fmt.Fprintln(out, "Zed:      Remote Projects > Connect New Server, then paste the SSH command")
	fmt.Fprintf(out, "           After that: pisafe zed %s\n", manifest.RunID)
	if inferenceConfigured {
		fmt.Fprintln(out, "Pi:        keep `pisafe broker` running on this Mac to serve inference")
	} else {
		fmt.Fprintln(out, "Pi:        inference unavailable; run pisafe login chatgpt, then pisafe broker")
	}
	return nil
}

func printExcludedNames(out io.Writer, excluded gitstage.ExcludedInputs) {
	const maximumNames = 12
	printed := 0
	for _, category := range []struct {
		label string
		names []string
	}{
		{label: "untracked", names: excluded.Untracked},
		{label: "ignored", names: excluded.Ignored},
	} {
		for _, name := range category.names {
			if printed == maximumNames {
				remaining := len(excluded.Untracked) + len(excluded.Ignored) - printed
				fmt.Fprintf(out, "           ... and %d more\n", remaining)
				return
			}
			fmt.Fprintf(out, "           %s %q\n", category.label, name)
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
