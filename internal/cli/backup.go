package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/mpizenberg/pisafe/internal/backup"
	"github.com/mpizenberg/pisafe/internal/lima"
	"github.com/mpizenberg/pisafe/internal/runctl"
	"github.com/mpizenberg/pisafe/internal/runid"
)

var (
	errBackupUsage  = errors.New("usage: pisafe backup DIRECTORY")
	errRestoreUsage = errors.New("usage: pisafe restore DIRECTORY")
)

func runBackup(ctx context.Context, args []string, out io.Writer) error {
	if len(args) != 1 {
		return errBackupUsage
	}
	// Backing up reads the VM and writes only here. A VM whose boundary records
	// no longer hold still holds transcripts nothing can refetch, and refusing
	// to copy them to the Mac protects nothing either record describes.
	vm := lima.New()
	if err := vm.StartUnverified(ctx); err != nil {
		return err
	}
	return writeBackup(ctx, vm, args[0], out)
}

func runRestore(ctx context.Context, args []string, out io.Writer) error {
	if len(args) != 1 {
		return errRestoreUsage
	}
	// The backup is read before the VM is touched, so a directory that is not
	// one says so instead of failing behind a boot.
	held, err := backup.Read(args[0])
	if err != nil {
		return err
	}
	controller, err := prepareLifecycle(ctx)
	if err != nil {
		return err
	}
	if err := restoreProjects(ctx, controller, held, args[0], out); err != nil {
		return err
	}
	return restoreProfile(ctx, lima.New(), held, out)
}

// writeBackup copies out what nothing can refetch. A project that has no
// transcript is left out of the manifest rather than recorded as empty: what a
// restore would put back for it is the record and the filesystem, both of which
// its next run creates anyway.
func writeBackup(
	ctx context.Context,
	vm lima.VM,
	directory string,
	out io.Writer,
) error {
	store, err := runStore()
	if err != nil {
		return err
	}
	recorded, err := store.ListProjects()
	if err != nil {
		return err
	}
	if err := vm.EnsureGlobalStorage(ctx); err != nil {
		return err
	}
	extensions, err := vm.ReadProfileRecord(ctx)
	if err != nil {
		return err
	}
	tools, err := vm.ReadProfileTools(ctx)
	if err != nil {
		return err
	}

	held := backup.Backup{Extensions: extensions, Tools: tools}
	for _, record := range recorded {
		project, err := runid.NewProject(record.Root)
		if err != nil {
			return err
		}
		// A project's filesystem is mounted per VM boot, and archiving one that
		// is not mounted finds no session directory and reports no transcripts.
		// Nothing else here would fail, so the backup would claim to hold a
		// store it had not read.
		if err := vm.EnsureProjectStorage(ctx, project.Key); err != nil {
			return err
		}
		refused, err := backupSessions(ctx, vm, directory, project.Key)
		if err != nil {
			return err
		}
		transcripts, err := backup.Sessions(directory, project.Key)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "%s: %d transcript(s)\n", record.Root, len(transcripts))
		if refused > 0 {
			fmt.Fprintf(
				out,
				"  %d file(s) in its session store are not named like transcripts "+
					"and were left in the VM\n",
				refused,
			)
		}
		if len(transcripts) > 0 {
			held.Projects = append(held.Projects, project)
		}
	}
	held.CreatedAt = time.Now().UTC()
	if err := backup.Write(directory, held); err != nil {
		return err
	}
	fmt.Fprintf(
		out,
		"Backed up %d project store(s), %d extension(s), and %d tool(s) to %s.\n"+
			"Dependency caches are not backed up: nothing needs one to be correct.\n"+
			"No provider credential was written; those stay in the macOS Keychain.\n",
		len(held.Projects),
		len(extensions.Extensions),
		len(tools.Tools),
		directory,
	)
	return nil
}

// backupSessions streams one project's session store into the backup. The
// archive is produced and consumed at once, so a store larger than the Mac's
// memory is never held in it.
func backupSessions(
	ctx context.Context,
	vm lima.VM,
	directory string,
	projectKey string,
) (int, error) {
	reader, writer := io.Pipe()
	streamed := make(chan error, 1)
	go func() {
		err := vm.ArchiveSessions(ctx, projectKey, writer)
		writer.CloseWithError(err)
		streamed <- err
	}()
	_, refused, addErr := backup.AddSessions(reader, directory, projectKey)
	// Closing the read half unblocks the VM when the Mac stops early, so a
	// refused backup never leaves a script writing into nothing.
	reader.CloseWithError(addErr)
	if streamErr := <-streamed; streamErr != nil {
		return 0, streamErr
	}
	return refused, addErr
}

// restoreProjects puts every project store the backup holds back, and stops at
// the first that cannot be. Unlike a package, a transcript has no second source
// to come from, so a failure here is the VM's storage rather than one project's
// bad luck, and the projects after it would fail the same way.
func restoreProjects(
	ctx context.Context,
	controller runctl.Controller,
	held backup.Backup,
	directory string,
	out io.Writer,
) error {
	for _, project := range held.Projects {
		transcripts, err := backup.Sessions(directory, project.Key)
		if err != nil {
			return err
		}
		reader, writer := io.Pipe()
		archived := make(chan error, 1)
		go func() {
			err := backup.ArchiveSessions(directory, project.Key, writer)
			writer.CloseWithError(err)
			archived <- err
		}()
		restoreErr := controller.RestoreProject(ctx, project, reader)
		reader.CloseWithError(restoreErr)
		if err := <-archived; err != nil {
			return err
		}
		if restoreErr != nil {
			return restoreErr
		}
		fmt.Fprintf(out, "%s: %d transcript(s)\n", project.Root, len(transcripts))
	}
	return nil
}

// restoreProfile reinstalls what the profile held, from the pins the backup
// recorded rather than from what npm resolves those names to now: the fetched
// tarball is checked against the hash the backup carries, so a release
// republished since is refused instead of installed. A package that will not
// install leaves the others alone — npm is a third party, and one unpublished
// package is not a reason to hand back none of a profile.
func restoreProfile(
	ctx context.Context,
	vm lima.VM,
	held backup.Backup,
	out io.Writer,
) error {
	if len(held.Extensions.Extensions) == 0 && len(held.Tools.Tools) == 0 {
		fmt.Fprintln(out, "The backup recorded no extension and no tool.")
		return nil
	}
	imageID, err := ensureRunImage(ctx, vm)
	if err != nil {
		return err
	}
	var failures []error
	if err := restoreExtensions(ctx, vm, imageID, held, out); err != nil {
		failures = append(failures, err)
	}
	if err := restoreTools(ctx, vm, imageID, held, out); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

// restoreExtensions installs what is missing and keeps what is there. A package
// already installed is left alone whatever it is pinned to: the profile it is
// being restored into may have moved on, and a restore puts back what was lost
// rather than making the profile identical to the backup.
func restoreExtensions(
	ctx context.Context,
	vm lima.VM,
	imageID string,
	held backup.Backup,
	out io.Writer,
) error {
	record, err := vm.ReadProfileRecord(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, pin := range held.Extensions.Extensions {
		if installed, found := record.Find(pin.Name); found {
			fmt.Fprintf(out, "%s is already installed at %s\n", pin.Name, installed.Version)
			continue
		}
		if err := vm.InstallExtension(ctx, imageID, pin); err != nil {
			failures = append(failures, err)
			continue
		}
		record = record.With(pin)
		// The record is written per package, so a restore that stops partway
		// leaves a profile describing exactly what is in it.
		if err := vm.WriteProfileRecord(ctx, record); err != nil {
			return errors.Join(append(failures, err)...)
		}
		fmt.Fprintf(out, "Installed %s@%s\n", pin.Name, pin.Version)
	}
	return errors.Join(failures...)
}

// restoreTools does the same for the commands, reading back what each installed
// tree claims rather than trusting the names the backup recorded: which
// commands a package provides is the package's own choice, and a name another
// tool has taken since is refused rather than shadowed.
func restoreTools(
	ctx context.Context,
	vm lima.VM,
	imageID string,
	held backup.Backup,
	out io.Writer,
) error {
	installed, err := vm.ReadProfileTools(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, backedUp := range held.Tools.Tools {
		if existing, found := installed.Find(backedUp.Name); found {
			fmt.Fprintf(out, "%s is already installed at %s\n", backedUp.Name, existing.Version)
			continue
		}
		tool, err := claimTool(ctx, vm, imageID, installed, backedUp.Pin)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		installed = installed.With(tool)
		if err := vm.LinkToolBinaries(ctx, installed); err != nil {
			return errors.Join(append(failures, err)...)
		}
		if err := vm.WriteProfileTools(ctx, installed); err != nil {
			return errors.Join(append(failures, err)...)
		}
		fmt.Fprintf(out, "Installed %s@%s\n", tool.Name, tool.Version)
	}
	return errors.Join(failures...)
}
