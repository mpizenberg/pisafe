package cli

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"runtime"
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
	{name: "Podman", command: "podman", required: true, hint: "install Podman"},
	{name: "Zed", command: "zed", required: false, hint: "install the Zed CLI to open runs automatically"},
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

	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		missingRequired = true
		fmt.Fprintln(out, "MISSING  platform (Phase 1 requires macOS on ARM64)")
	}
	if missingRequired {
		return fmt.Errorf("required host prerequisites are missing")
	}
	return nil
}
