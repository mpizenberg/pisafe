package cli

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"time"

	"github.com/mpizenberg/pisafe/internal/runssh"
	"github.com/mpizenberg/pisafe/internal/runstate"
	"github.com/mpizenberg/pisafe/internal/zedsettings"
)

// zedSettleDelay is how long Zed is given to read a connection pisafe has just
// written. Zed reloads its settings from a file watcher and offers nothing to
// wait on, so a run handed over before that reload reaches ssh under a host
// name nothing can resolve. The reload lands inside a tenth of this; the rest is
// margin, and it is paid once per run.
const zedSettleDelay = 500 * time.Millisecond

func runZed(ctx context.Context, runID string, out io.Writer) error {
	manifest, err := activeRun(ctx, runID, out)
	if err != nil {
		return err
	}
	zed, err := exec.LookPath("zed")
	if err != nil {
		return fmt.Errorf("find Zed CLI: install it from Zed's command palette")
	}
	path, added, err := saveZedConnection(manifest)
	if err != nil {
		return fmt.Errorf(
			"save a Zed connection in %s: %w\n"+
				"Add it by hand instead with Remote Projects > Connect New Server:\n"+
				"  ssh -F %s %s",
			path,
			err,
			shellQuote(manifest.SSH.ConfigFile),
			manifest.SSH.Alias,
		)
	}
	if added {
		fmt.Fprintf(out, "Zed:       saved a connection for %s in %s\n", manifest.RunID, path)
		time.Sleep(zedSettleDelay)
	}
	command := exec.CommandContext(
		ctx,
		zed,
		"ssh://"+manifest.SSH.Alias+manifest.Workspace,
	)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("open run in Zed: %s", output)
	}
	return nil
}

// saveZedConnection tells Zed how to reach a run, reporting the settings file
// and whether it had to be written. Zed passes ssh nothing but what a saved
// connection carries, and a run's alias is defined only in pisafe's own per-run
// config, so without this the alias is a host name nothing on the Mac resolves.
func saveZedConnection(manifest runstate.Manifest) (string, bool, error) {
	path, err := zedsettings.Path()
	if err != nil {
		return "", false, err
	}
	added, err := zedsettings.Ensure(path, zedsettings.Connection{
		Host:       manifest.SSH.Alias,
		ConfigFile: manifest.SSH.ConfigFile,
	})
	return path, added, err
}

// forgetZedConnection takes a reclaimed run's saved connection back out, which
// is the other half of putting it there: the alias stops resolving the moment
// the run's own config is gone. Nothing is left to reclaim by the time this
// runs, so settings pisafe cannot edit are worth saying and no more.
func forgetZedConnection(runID string, out io.Writer) {
	path, err := zedsettings.Path()
	if err == nil {
		_, err = zedsettings.Remove(path, runssh.Alias(runID))
	}
	if err != nil {
		fmt.Fprintf(
			out,
			"Warning: left the saved Zed connection for %s in place: %v\n",
			runID,
			err,
		)
	}
}
