package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/mpizenberg/pisafe/internal/hostnet"
	"github.com/mpizenberg/pisafe/internal/lima"
	"github.com/mpizenberg/pisafe/internal/runstate"
)

var errVMUsage = errors.New("usage: pisafe vm rebuild [--confirm] [--discard-state]")

// vmRebuildRequest is one parsed `pisafe vm rebuild`. Neither flag is a
// preference: a rebuild ends every run that is working, and on a VM holding its
// own state it also destroys what those runs produced.
type vmRebuildRequest struct {
	confirmed    bool
	discardState bool
}

func runVM(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 || args[0] != "rebuild" {
		return errVMUsage
	}
	request := vmRebuildRequest{}
	for _, argument := range args[1:] {
		switch argument {
		case "--confirm":
			request.confirmed = true
		case "--discard-state":
			request.discardState = true
		default:
			return errVMUsage
		}
	}
	return rebuildVM(ctx, request, out)
}

// rebuildVM replaces the instance with one built from the current definition.
// It is the cure every boundary check prescribes: a stale security profile or a
// firewall built around networks this Mac has left is drift in the instance,
// and nothing short of a new one settles it.
func rebuildVM(ctx context.Context, request vmRebuildRequest, out io.Writer) error {
	vm := lima.New()
	status, err := vm.Status(ctx)
	if err != nil {
		return err
	}
	stateDisk, err := vm.HasStateDisk(ctx)
	if err != nil {
		return err
	}
	runs, err := recordedRuns()
	if err != nil {
		return err
	}
	// An instance provisioned before the state disk existed keeps every run's
	// files on its own disk, so deleting it is what destroys them. An absent
	// instance holds nothing either way.
	loses := status != lima.StatusAbsent && !stateDisk

	printRebuildPlan(out, status, loses, runs)
	if !request.confirmed {
		fmt.Fprintf(out, "\nNothing was changed. Rebuild with %s\n", rebuildCommand(loses))
		return nil
	}
	if loses && !request.discardState {
		return fmt.Errorf(
			"this VM holds every run's files on its own disk, so rebuilding it destroys them; "+
				"save what can be saved with pisafe backup DIRECTORY, then run %s",
			rebuildCommand(loses),
		)
	}

	// The networks the new VM's firewall is built around are discovered before
	// anything is deleted, so a Mac that cannot answer costs nothing rather than
	// leaving no VM at all.
	prefixes, err := hostnet.OnLinkIPv4(ctx)
	if err != nil {
		return fmt.Errorf("discover host networks: %w", err)
	}

	if loses {
		// Stopping a run publishes its caches and transcripts into the project
		// store, which is on the disk about to be deleted. There is nothing to
		// publish them to.
		fmt.Fprintln(out, "Deleting the instance and everything it holds...")
	} else {
		stopRunsBeforeRebuild(ctx, runs, out)
		fmt.Fprintln(out, "Deleting the instance...")
	}
	if err := vm.Delete(ctx); err != nil {
		return err
	}

	fmt.Fprintln(out, "Creating the VM and verifying its boundary...")
	if err := vm.Ensure(ctx, prefixes); err != nil {
		return err
	}
	// A run image is built inside the instance, so it goes with one. Building it
	// here rather than leaving it to the next run keeps the whole cost of a
	// rebuild in the command that asked for it.
	fmt.Fprintln(out, "Building the run image...")
	if _, err := ensureManagedRunImage(ctx); err != nil {
		return err
	}

	fmt.Fprintln(out, "Rebuilt the VM against the current definition; its boundary verified.")
	if loses {
		return forgetLostRuns(ctx, runs, out)
	}
	if len(runs) != 0 {
		fmt.Fprintln(out, "Every run kept its workspace; resume one with pisafe resume RUN.")
	}
	return nil
}

func rebuildCommand(loses bool) string {
	if loses {
		return "pisafe vm rebuild --confirm --discard-state"
	}
	return "pisafe vm rebuild --confirm"
}

// printRebuildPlan says what the rebuild costs before it is asked for. What a
// VM keeps is the whole difference between a routine cure and losing every
// run's work, and it is not something the user can see from the outside.
func printRebuildPlan(out io.Writer, status lima.Status, loses bool, runs []runstate.Manifest) {
	if status == lima.StatusAbsent {
		fmt.Fprintln(out, "VM:      absent; this creates one rather than replacing anything")
		return
	}
	fmt.Fprintf(out, "VM:      %s\n", status)
	if active := activeRunIDs(runs); len(active) != 0 && !loses {
		fmt.Fprintf(
			out,
			"Stops:   %d active run(s), charged for the time their containers record\n",
			len(active),
		)
	}
	if !loses {
		fmt.Fprintf(
			out,
			"Keeps:   every run's files, every project's transcripts, and the profile,\n"+
				"         on the %s disk the new VM mounts back\n",
			lima.StateDiskName,
		)
		return
	}
	fmt.Fprintf(
		out,
		"Loses:   every run's files, every project's transcripts, and the profile.\n"+
			"         This VM predates the %s disk and holds all of it on the\n"+
			"         disk the rebuild deletes. pisafe backup DIRECTORY saves the\n"+
			"         transcripts and the profile's pins; nothing saves a workspace.\n",
		lima.StateDiskName,
	)
	if len(runs) != 0 {
		fmt.Fprintf(
			out,
			"Forgets: %d run record(s), which would otherwise name storage that is gone\n",
			len(runs),
		)
	}
}

// stopRunsBeforeRebuild ends every run still holding a container, so the
// stretch it spent is charged from the container's own account and what it
// produced reaches the project store before the instance goes. Nothing here can
// refuse the rebuild: a VM too broken to answer is the reason one was asked
// for, and a run left active is settled by the next command that reaches for
// it, charged nothing for the outage.
func stopRunsBeforeRebuild(ctx context.Context, runs []runstate.Manifest, out io.Writer) {
	active := activeRunIDs(runs)
	if len(active) == 0 {
		return
	}
	controller, err := prepareUnverified(ctx)
	if err != nil {
		fmt.Fprintf(out, "Warning: could not reach the VM to stop %d run(s): %v\n", len(active), err)
		return
	}
	for _, runID := range active {
		fmt.Fprintf(out, "Stopping %s...\n", runID)
		if _, err := controller.Stop(ctx, runID); err != nil {
			fmt.Fprintf(out, "Warning: could not stop %s: %v\n", runID, err)
		}
	}
}

// forgetLostRuns clears the records of runs whose files went with the instance.
// Every one of them describes a workspace that no longer exists, and so does
// the SSH key it names and the connection Zed saved for it.
func forgetLostRuns(ctx context.Context, runs []runstate.Manifest, out io.Writer) error {
	if len(runs) == 0 {
		return nil
	}
	controller, err := prepareUnverified(ctx)
	if err != nil {
		return err
	}
	failures := []error{}
	for _, run := range runs {
		if err := controller.Discard(ctx, run.RunID); err != nil {
			failures = append(failures, err)
			continue
		}
		forgetZedConnection(run.RunID, out)
	}
	fmt.Fprintf(
		out,
		"Forgot %d of %d run record(s), whose storage went with the instance.\n",
		len(runs)-len(failures),
		len(runs),
	)
	return errors.Join(failures...)
}

func activeRunIDs(runs []runstate.Manifest) []string {
	active := []string{}
	for _, run := range runs {
		if run.State == runstate.StateActive {
			active = append(active, run.RunID)
		}
	}
	return active
}
