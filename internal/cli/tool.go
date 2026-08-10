package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/mpizenberg/pisafe/internal/lima"
	"github.com/mpizenberg/pisafe/internal/profile"
)

var errToolUsage = errors.New(
	"usage: pisafe tool <install PACKAGE[@VERSION]|remove PACKAGE|list>",
)

func runTool(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errToolUsage
	}
	vm := lima.New()
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return errToolUsage
		}
		return listTools(ctx, vm, out)
	case "install":
		if len(args) != 2 {
			return errToolUsage
		}
		return installTool(ctx, vm, args[1], out)
	case "remove":
		if len(args) != 2 {
			return errToolUsage
		}
		return removeTool(ctx, vm, args[1], out)
	default:
		return errToolUsage
	}
}

func listTools(ctx context.Context, vm lima.VM, out io.Writer) error {
	if err := vm.EnsureGlobalStorage(ctx); err != nil {
		return err
	}
	tools, err := vm.ReadProfileTools(ctx)
	if err != nil {
		return err
	}
	if len(tools.Tools) == 0 {
		fmt.Fprintln(out, "No tools installed.")
		return nil
	}
	for _, tool := range tools.Tools {
		fmt.Fprintf(
			out,
			"%s@%s\n  %s\n  %s\n",
			tool.Name,
			tool.Version,
			strings.Join(tool.Binaries, " "),
			tool.Integrity,
		)
	}
	return nil
}

// installTool puts one command on every run's PATH. The package is pinned and
// fetched exactly as an extension is; what is different is that the names it
// answers to are the package's own choice, so they are read back from what was
// installed and refused when another tool already holds one.
func installTool(
	ctx context.Context,
	vm lima.VM,
	packageSpec string,
	out io.Writer,
) error {
	if err := validatePackageSpec(packageSpec); err != nil {
		return err
	}
	imageID, err := ensureRunImage(ctx, vm)
	if err != nil {
		return err
	}
	installed, err := vm.ReadProfileTools(ctx)
	if err != nil {
		return err
	}
	pin, err := vm.ResolvePackage(ctx, imageID, packageSpec)
	if err != nil {
		return err
	}
	tool, err := claimTool(ctx, vm, imageID, installed, pin)
	if err != nil {
		return err
	}
	updated := installed.With(tool)
	if err := vm.LinkToolBinaries(ctx, updated); err != nil {
		return err
	}
	if err := vm.WriteProfileTools(ctx, updated); err != nil {
		return err
	}
	fmt.Fprintf(
		out,
		"Installed %s@%s\n  %s\n  %s\n",
		tool.Name,
		tool.Version,
		strings.Join(tool.Binaries, " "),
		tool.Integrity,
	)
	fmt.Fprintln(
		out,
		"Runs started from now find it on their PATH. It stays at this version "+
			"until you install it again.",
	)
	return nil
}

// claimTool installs one pinned package and reports the commands it turns out
// to claim, leaving the profile as it was when it claims none or claims one
// another tool already holds. The tree is inert until a link points at it,
// which is what makes taking it back out enough.
func claimTool(
	ctx context.Context,
	vm lima.VM,
	imageID string,
	installed profile.Tools,
	pin profile.Pin,
) (profile.Tool, error) {
	if err := vm.InstallTool(ctx, imageID, pin); err != nil {
		return profile.Tool{}, err
	}
	tool, err := claimedTool(ctx, vm, installed, pin)
	if err != nil {
		if removeErr := vm.RemoveTool(ctx, pin); removeErr != nil {
			return profile.Tool{}, errors.Join(err, removeErr)
		}
		return profile.Tool{}, err
	}
	return tool, nil
}

// claimedTool reads back what the installed tree claims and refuses a package
// that would take a name from another tool. Which names a package installs is
// the package's own choice rather than the user's, so shadowing one silently
// would change what an unrelated command means.
func claimedTool(
	ctx context.Context,
	vm lima.VM,
	installed profile.Tools,
	pin profile.Pin,
) (profile.Tool, error) {
	binaries, err := vm.ToolBinaries(ctx, pin)
	if err != nil {
		return profile.Tool{}, err
	}
	tool := profile.Tool{Pin: pin, Binaries: binaries}
	if err := tool.Validate(); err != nil {
		return profile.Tool{}, err
	}
	conflicts := installed.Conflicts(tool)
	if len(conflicts) == 0 {
		return tool, nil
	}
	claims := make([]string, 0, len(conflicts))
	for _, conflict := range conflicts {
		claims = append(claims, fmt.Sprintf("%s (%s)", conflict.Binary, conflict.Holder))
	}
	return profile.Tool{}, fmt.Errorf(
		"%s provides a command another tool already provides: %s; "+
			"remove the other tool first",
		tool.Name,
		strings.Join(claims, ", "),
	)
}

// removeTool takes one command out of the profile. The links go first, so a run
// starting while the tree is being deleted was never going to reach it.
func removeTool(
	ctx context.Context,
	vm lima.VM,
	name string,
	out io.Writer,
) error {
	if err := profile.ValidatePackageName(name); err != nil {
		return err
	}
	if err := vm.EnsureGlobalStorage(ctx); err != nil {
		return err
	}
	installed, err := vm.ReadProfileTools(ctx)
	if err != nil {
		return err
	}
	remaining, removed, found := installed.Without(name)
	if !found {
		return fmt.Errorf("%s is not installed", name)
	}
	if err := vm.LinkToolBinaries(ctx, remaining); err != nil {
		return err
	}
	if err := vm.WriteProfileTools(ctx, remaining); err != nil {
		return err
	}
	if err := vm.RemoveTool(ctx, removed.Pin); err != nil {
		return err
	}
	fmt.Fprintf(out, "Removed %s@%s\n", removed.Name, removed.Version)
	return nil
}
