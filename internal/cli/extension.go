package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/mpizenberg/pisafe/internal/hostnet"
	"github.com/mpizenberg/pisafe/internal/lima"
	"github.com/mpizenberg/pisafe/internal/profile"
	"github.com/mpizenberg/pisafe/internal/runimage"
)

var errExtensionUsage = errors.New(
	"usage: pisafe extension <install PACKAGE[@VERSION]|remove PACKAGE|list>",
)

func runExtension(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errExtensionUsage
	}
	transport := lima.NewTransport()
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return errExtensionUsage
		}
		return listExtensions(ctx, transport, out)
	case "install":
		if len(args) != 2 {
			return errExtensionUsage
		}
		return installExtension(ctx, transport, args[1], out)
	case "remove", "uninstall":
		if len(args) != 2 {
			return errExtensionUsage
		}
		return removeExtension(ctx, transport, args[1], out)
	default:
		return errExtensionUsage
	}
}

func listExtensions(ctx context.Context, transport lima.Transport, out io.Writer) error {
	if err := transport.EnsureGlobalStorage(ctx); err != nil {
		return err
	}
	record, err := transport.ReadProfileRecord(ctx)
	if err != nil {
		return err
	}
	if len(record.Extensions) == 0 {
		fmt.Fprintln(out, "No extensions installed.")
		return nil
	}
	for _, extension := range record.Extensions {
		fmt.Fprintf(out, "%s@%s\n  %s\n", extension.Name, extension.Version, extension.Integrity)
	}
	return nil
}

// installExtension pins one package into the profile every run mounts. What is
// installed is resolved first and fetched second, so the version and the hash
// pisafe records are what the install is then checked against rather than a
// description of whatever arrived.
func installExtension(
	ctx context.Context,
	transport lima.Transport,
	packageSpec string,
	out io.Writer,
) error {
	if err := validateExtensionSpec(packageSpec); err != nil {
		return err
	}
	imageID, err := ensureRunImage(ctx, transport)
	if err != nil {
		return err
	}
	if err := transport.EnsureGlobalStorage(ctx); err != nil {
		return err
	}
	record, err := transport.ReadProfileRecord(ctx)
	if err != nil {
		return err
	}
	extension, err := transport.ResolveExtension(ctx, imageID, packageSpec)
	if err != nil {
		return err
	}
	if err := transport.InstallExtension(ctx, imageID, extension); err != nil {
		return err
	}
	if err := transport.WriteProfileRecord(ctx, record.With(extension)); err != nil {
		return err
	}
	fmt.Fprintf(out, "Installed %s@%s\n  %s\n", extension.Name, extension.Version, extension.Integrity)
	fmt.Fprintln(
		out,
		"Runs started from now load it. It is pinned: nothing updates it but this command.",
	)
	return nil
}

// removeExtension takes one package out of the profile. The record goes first,
// so a run starting while the tree is being deleted was never going to load it.
func removeExtension(
	ctx context.Context,
	transport lima.Transport,
	name string,
	out io.Writer,
) error {
	if err := profile.ValidatePackageName(name); err != nil {
		return err
	}
	if err := transport.EnsureGlobalStorage(ctx); err != nil {
		return err
	}
	record, err := transport.ReadProfileRecord(ctx)
	if err != nil {
		return err
	}
	remaining, removed, found := record.Without(name)
	if !found {
		return fmt.Errorf("%s is not installed", name)
	}
	if err := transport.WriteProfileRecord(ctx, remaining); err != nil {
		return err
	}
	if err := transport.RemoveExtension(ctx, removed); err != nil {
		return err
	}
	fmt.Fprintf(out, "Removed %s@%s\n", removed.Name, removed.Version)
	return nil
}

// validateExtensionSpec bounds what may be asked for before it becomes an
// argument inside a container: an npm name, and at most an exact version. A
// range is refused rather than resolved, because two installs of one spec would
// otherwise produce two different profiles.
func validateExtensionSpec(packageSpec string) error {
	name := packageSpec
	if index := strings.LastIndex(packageSpec, "@"); index > 0 {
		name = packageSpec[:index]
		if err := profile.ValidateVersion(packageSpec[index+1:]); err != nil {
			return err
		}
	}
	return profile.ValidatePackageName(name)
}

// ensureRunImage gives the extension commands the same image a run gets. The
// package is fetched and installed by a container of that image, so a profile
// built by one and loaded by another is never a question.
func ensureRunImage(ctx context.Context, transport lima.Transport) (string, error) {
	artifacts, err := packagedRunArtifacts()
	if err != nil {
		return "", err
	}
	prefixes, err := hostnet.OnLinkIPv4(ctx)
	if err != nil {
		return "", fmt.Errorf("discover host networks: %w", err)
	}
	if err := lima.NewManager().Ensure(ctx, prefixes); err != nil {
		return "", fmt.Errorf("prepare Lima boundary: %w", err)
	}
	image, err := runimage.NewInstaller(transport).Ensure(ctx, artifacts)
	if err != nil {
		return "", fmt.Errorf("install managed run image: %w", err)
	}
	return image.ImageID, nil
}
