package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/mpizenberg/pisafe/internal/lima"
	"github.com/mpizenberg/pisafe/internal/profile"
)

var errProfileUsage = errors.New("usage: pisafe profile reset --confirm")

func runProfile(ctx context.Context, args []string, out io.Writer) error {
	if len(args) != 2 || args[0] != "reset" || args[1] != "--confirm" {
		return errProfileUsage
	}
	return resetProfile(ctx, lima.NewTransport(), out)
}

// resetProfile takes every extension and every tool back out at once. The
// records are emptied before the trees they name, so a run starting partway
// through loads nothing rather than half a profile, and the directory of links
// is rebuilt last so a run's PATH ends up pointing at an empty directory rather
// than a missing one.
func resetProfile(ctx context.Context, transport lima.Transport, out io.Writer) error {
	if err := transport.EnsureGlobalStorage(ctx); err != nil {
		return err
	}
	if err := transport.WriteProfileRecord(ctx, profile.Record{}); err != nil {
		return err
	}
	if err := transport.WriteProfileTools(ctx, profile.Tools{}); err != nil {
		return err
	}
	// What npm last resolved describes packages that are gone, and a check that
	// never happened is what makes the next one happen.
	if err := transport.WriteProfileOffers(ctx, profile.Offers{}); err != nil {
		return err
	}
	if err := transport.ResetProfile(ctx); err != nil {
		return err
	}
	if err := transport.LinkToolBinaries(ctx, profile.Tools{}); err != nil {
		return err
	}
	fmt.Fprintln(
		out,
		"Emptied the profile: runs started from now load no extension and find no\n"+
			"installed command on their PATH. Each is refetchable from npm, but pisafe\n"+
			"no longer knows what was there.",
	)
	return nil
}
