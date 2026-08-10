package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/mpizenberg/pisafe/internal/hostnet"
	limabackend "github.com/mpizenberg/pisafe/internal/lima"
	"github.com/mpizenberg/pisafe/internal/runimage"
)

type prerequisite struct {
	name     string
	command  string
	required bool
	hint     string
}

var prerequisites = []prerequisite{
	{name: "Git", command: "git", required: true, hint: "install Xcode command-line tools"},
	{name: "Lima", command: "limactl", required: true, hint: "install Lima"},
	{name: "OpenSSH", command: "ssh-keygen", required: true, hint: "install macOS OpenSSH tools"},
	{name: "Zed", command: "zed", required: false, hint: "install the Zed CLI to reopen saved run connections"},
}

func runDoctor(ctx context.Context, out io.Writer) error {
	fmt.Fprintf(out, "Host: %s/%s\n", runtime.GOOS, runtime.GOARCH)

	missingRequired := false
	for _, prerequisite := range prerequisites {
		path, err := exec.LookPath(prerequisite.command)
		if err != nil {
			label := "optional"
			if prerequisite.required {
				label = "required"
				missingRequired = true
			}
			fmt.Fprintf(out, "MISSING  %-7s (%s; %s)\n", prerequisite.name, label, prerequisite.hint)
			continue
		}

		fmt.Fprintf(out, "OK       %-7s %s\n", prerequisite.name, path)
	}

	if !supportedHost() {
		missingRequired = true
		fmt.Fprintln(out, "MISSING  platform (Phase 1 requires macOS on ARM64)")
	}
	if !missingRequired {
		guestPath, err := packagedGuestPath()
		if err != nil {
			missingRequired = true
			fmt.Fprintf(out, "MISSING  run image (%v)\n", err)
		} else if _, err := runimage.LoadPackagedArtifacts(guestPath); err != nil {
			missingRequired = true
			fmt.Fprintf(out, "MISSING  run image (%v)\n", err)
		} else {
			fmt.Fprintf(out, "OK       Run image %s\n", guestPath)
		}
	}
	if !missingRequired {
		if err := checkGeneratedLimaConfig(ctx, out); err != nil {
			missingRequired = true
			fmt.Fprintf(out, "MISSING  boundary (%v)\n", err)
		}
	}
	if missingRequired {
		return fmt.Errorf("required host prerequisites are missing")
	}
	return nil
}

func checkGeneratedLimaConfig(ctx context.Context, out io.Writer) error {
	prefixes, err := hostnet.OnLinkIPv4(ctx)
	if err != nil {
		return fmt.Errorf("host networks: %w", err)
	}
	config, err := limabackend.RenderConfig(prefixes)
	if err != nil {
		return fmt.Errorf("render Lima configuration: %w", err)
	}
	boundary, err := limabackend.CanonicalIPv4Prefixes(prefixes)
	if err != nil {
		return fmt.Errorf("host networks: %w", err)
	}

	tempDir, err := os.MkdirTemp("", "pisafe-doctor-*")
	if err != nil {
		return fmt.Errorf("create temporary directory: %w", err)
	}
	defer os.RemoveAll(tempDir)
	configPath := filepath.Join(tempDir, "pisafe.yaml")
	if err := os.WriteFile(configPath, config, 0o600); err != nil {
		return fmt.Errorf("write temporary Lima configuration: %w", err)
	}
	command := exec.CommandContext(ctx, "limactl", "template", "validate", configPath)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("validate Lima configuration: %s", output)
	}
	fmt.Fprintf(out, "OK       Boundary %d host IPv4 prefixes; Lima config valid\n", len(boundary))
	return nil
}
