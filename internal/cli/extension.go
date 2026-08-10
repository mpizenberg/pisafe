package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/mpizenberg/pisafe/internal/hostnet"
	"github.com/mpizenberg/pisafe/internal/lima"
	"github.com/mpizenberg/pisafe/internal/profile"
	"github.com/mpizenberg/pisafe/internal/runimage"
)

const (
	// updateCheckInterval is the cadence of the whole unsolicited path: a stop
	// reaches the network at most this often, and says something only when that
	// check moved what is on offer.
	updateCheckInterval = 24 * time.Hour

	// updateCheckTimeout bounds a check nobody asked for. An unreachable
	// registry must cost a stop seconds, not the ten minutes the installing
	// containers are allowed.
	updateCheckTimeout = 45 * time.Second
)

var errExtensionUsage = errors.New(
	"usage: pisafe extension <install PACKAGE[@VERSION]|update [PACKAGE...]|" +
		"remove PACKAGE|list>",
)

func runExtension(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errExtensionUsage
	}
	vm := lima.New()
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return errExtensionUsage
		}
		return listExtensions(ctx, vm, out)
	case "install":
		if len(args) != 2 {
			return errExtensionUsage
		}
		return installExtension(ctx, vm, args[1], out)
	case "update":
		return updateExtensions(ctx, vm, args[1:], out)
	case "remove", "uninstall":
		if len(args) != 2 {
			return errExtensionUsage
		}
		return removeExtension(ctx, vm, args[1], out)
	default:
		return errExtensionUsage
	}
}

func listExtensions(ctx context.Context, vm lima.VM, out io.Writer) error {
	if err := vm.EnsureGlobalStorage(ctx); err != nil {
		return err
	}
	record, err := vm.ReadProfileRecord(ctx)
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
	offers, err := vm.ReadProfileOffers(ctx)
	if err != nil {
		return err
	}
	printExtensionUpdates(out, record.Pending(offers))
	return nil
}

// installExtension pins one package into the profile every run mounts. What is
// installed is resolved first and fetched second, so the version and the hash
// pisafe records are what the install is then checked against rather than a
// description of whatever arrived.
func installExtension(
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
	record, err := vm.ReadProfileRecord(ctx)
	if err != nil {
		return err
	}
	extension, err := vm.ResolvePackage(ctx, imageID, packageSpec)
	if err != nil {
		return err
	}
	if _, err := pinExtension(ctx, vm, imageID, record, extension); err != nil {
		return err
	}
	fmt.Fprintf(out, "Installed %s@%s\n  %s\n", extension.Name, extension.Version, extension.Integrity)
	fmt.Fprintln(
		out,
		"Runs started from now load it. It stays at this version until you run "+
			"pisafe extension update.",
	)
	return nil
}

// updateExtensions offers without applying: named packages are updated because
// the user named them, and naming none reports what is available and changes
// nothing.
func updateExtensions(
	ctx context.Context,
	vm lima.VM,
	names []string,
	out io.Writer,
) error {
	for _, name := range names {
		if err := profile.ValidatePackageName(name); err != nil {
			return err
		}
	}
	imageID, err := ensureRunImage(ctx, vm)
	if err != nil {
		return err
	}
	record, err := vm.ReadProfileRecord(ctx)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return reportExtensionUpdates(ctx, vm, imageID, record, out)
	}
	for _, name := range names {
		record, err = applyExtensionUpdate(ctx, vm, imageID, record, name, out)
		if err != nil {
			return err
		}
	}
	return nil
}

// reportExtensionUpdates asks npm what the profile could hold and applies none
// of it. What it learns is kept so the offer can be repeated later without
// asking again.
func reportExtensionUpdates(
	ctx context.Context,
	vm lima.VM,
	imageID string,
	record profile.Record,
	out io.Writer,
) error {
	if len(record.Extensions) == 0 {
		fmt.Fprintln(out, "No extensions installed.")
		return nil
	}
	offers, checkErr := vm.ResolveExtensionUpdates(ctx, imageID, record, time.Now())
	if checkErr != nil {
		fmt.Fprintf(out, "Warning: %s\n", checkErr)
	}
	if len(offers.Latest) == 0 {
		return checkErr
	}
	if err := vm.WriteProfileOffers(ctx, offers); err != nil {
		return err
	}
	updates := record.Pending(offers)
	if len(updates) == 0 {
		fmt.Fprintln(out, "Every installed extension is what npm resolves its name to.")
		return nil
	}
	printExtensionUpdates(out, updates)
	return nil
}

// applyExtensionUpdate moves one pin to what npm resolves the name to now,
// through the same resolve-then-verify path an install takes: an offer is
// never what the fetched bytes are checked against.
func applyExtensionUpdate(
	ctx context.Context,
	vm lima.VM,
	imageID string,
	record profile.Record,
	name string,
	out io.Writer,
) (profile.Record, error) {
	installed, found := record.Find(name)
	if !found {
		return record, fmt.Errorf(
			"%s is not installed; add it with pisafe extension install %s",
			name,
			name,
		)
	}
	resolved, err := vm.ResolvePackage(ctx, imageID, name)
	if err != nil {
		return record, err
	}
	if resolved.Version == installed.Version {
		fmt.Fprintf(out, "%s is already at %s, which is what npm resolves it to.\n", name, installed.Version)
		return record, nil
	}
	updated, err := pinExtension(ctx, vm, imageID, record, resolved)
	if err != nil {
		return record, err
	}
	fmt.Fprintf(out, "Updated %s from %s to %s\n  %s\n", name, installed.Version, resolved.Version, resolved.Integrity)
	fmt.Fprintln(
		out,
		"Live runs mount the profile itself, so a Pi started in one from now loads the new release.",
	)
	return updated, nil
}

// pinExtension puts one package in the profile and records the pin, in that
// order: what the record names is always something already there, and what is
// there but unrecorded is loaded by no run. Every route into the profile ends
// here, so what is recorded is what the fetched bytes were checked against.
func pinExtension(
	ctx context.Context,
	vm lima.VM,
	imageID string,
	record profile.Record,
	pin profile.Pin,
) (profile.Record, error) {
	if err := vm.InstallExtension(ctx, imageID, pin); err != nil {
		return record, err
	}
	updated := record.With(pin)
	if err := vm.WriteProfileRecord(ctx, updated); err != nil {
		return record, err
	}
	return updated, nil
}

// notifyExtensionUpdates tells the user what the profile could hold, at the
// moment they have stopped working rather than while they wait to start, and
// only when a check moved the answer: a standing offer is repeated by pisafe
// extension list, on request. The check is bounded and best-effort throughout,
// so a stop that worked is never failed, delayed, or made to depend on npm
// being reachable.
func notifyExtensionUpdates(
	ctx context.Context,
	vm lima.VM,
	imageID string,
	out io.Writer,
) {
	record, err := vm.ReadProfileRecord(ctx)
	if err != nil || len(record.Extensions) == 0 {
		return
	}
	offers, err := vm.ReadProfileOffers(ctx)
	if err != nil || !offers.Stale(time.Now(), updateCheckInterval) {
		return
	}
	refreshed := refreshExtensionOffers(ctx, vm, imageID, record, offers)
	printExtensionUpdates(out, record.PendingChange(offers, refreshed))
}

// refreshExtensionOffers asks npm again, keeping what the last check found if
// this one learns nothing. A check that resolved no package at all is not a
// check, so it neither replaces a standing offer nor counts as having happened.
func refreshExtensionOffers(
	ctx context.Context,
	vm lima.VM,
	imageID string,
	record profile.Record,
	offers profile.Offers,
) profile.Offers {
	ctx, cancel := context.WithTimeout(ctx, updateCheckTimeout)
	defer cancel()
	// A package that would not resolve is already left out of the result, so
	// the error here says only that the offer may be short — which is not worth
	// a line on a terminal the user is reading for a stop.
	refreshed, _ := vm.ResolveExtensionUpdates(ctx, imageID, record, time.Now())
	if len(refreshed.Latest) == 0 {
		return offers
	}
	if err := vm.WriteProfileOffers(ctx, refreshed); err != nil {
		return offers
	}
	return refreshed
}

func printExtensionUpdates(out io.Writer, updates []profile.Update) {
	if len(updates) == 0 {
		return
	}
	fmt.Fprintf(out, "npm resolves %d installed extension(s) to another version:\n", len(updates))
	for _, update := range updates {
		fmt.Fprintf(out, "  %s %s -> %s\n", update.Name, update.Installed, update.Available)
	}
	fmt.Fprintln(out, "Nothing is applied until you run pisafe extension update PACKAGE.")
}

// removeExtension takes one package out of the profile. The record goes first,
// so a run starting while the tree is being deleted was never going to load it.
func removeExtension(
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
	record, err := vm.ReadProfileRecord(ctx)
	if err != nil {
		return err
	}
	remaining, removed, found := record.Without(name)
	if !found {
		return fmt.Errorf("%s is not installed", name)
	}
	if err := vm.WriteProfileRecord(ctx, remaining); err != nil {
		return err
	}
	if err := vm.RemoveExtension(ctx, removed); err != nil {
		return err
	}
	fmt.Fprintf(out, "Removed %s@%s\n", removed.Name, removed.Version)
	return nil
}

// validatePackageSpec bounds what may be asked for before it becomes an
// argument inside a container: an npm name, and at most an exact version. A
// range is refused rather than resolved, because two installs of one spec would
// otherwise produce two different profiles.
func validatePackageSpec(packageSpec string) error {
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
func ensureRunImage(ctx context.Context, vm lima.VM) (string, error) {
	artifacts, err := packagedRunArtifacts()
	if err != nil {
		return "", err
	}
	prefixes, err := hostnet.OnLinkIPv4(ctx)
	if err != nil {
		return "", fmt.Errorf("discover host networks: %w", err)
	}
	if err := vm.Ensure(ctx, prefixes); err != nil {
		return "", fmt.Errorf("prepare Lima boundary: %w", err)
	}
	image, err := runimage.NewInstaller(vm).Ensure(ctx, artifacts)
	if err != nil {
		return "", fmt.Errorf("install managed run image: %w", err)
	}
	// The profile filesystem is what every one of these commands then writes to,
	// and it can only be laid out once the VM this just brought up is there.
	if err := vm.EnsureGlobalStorage(ctx); err != nil {
		return "", err
	}
	return image, nil
}
