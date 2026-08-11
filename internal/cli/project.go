package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/mpizenberg/pisafe/internal/gitstage"
	"github.com/mpizenberg/pisafe/internal/runid"
	"github.com/mpizenberg/pisafe/internal/runstate"
)

var errProjectUsage = errors.New(
	"usage: pisafe project <list|reset [PATH]|drop PATH --confirm PATH|rebind OLD-PATH>",
)

func runProject(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errProjectUsage
	}
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return errProjectUsage
		}
		return listProjects(out)
	case "reset":
		if len(args) > 2 {
			return errProjectUsage
		}
		if len(args) == 1 {
			return resetProject(ctx, ".", out)
		}
		return resetProject(ctx, args[1], out)
	case "drop":
		path, err := confirmedTarget(args, "project drop PATH --confirm PATH", "")
		if err != nil {
			return err
		}
		return dropProject(ctx, path, out)
	case "rebind":
		if len(args) != 2 {
			return errProjectUsage
		}
		return rebindProject(ctx, args[1], out)
	default:
		return errProjectUsage
	}
}

// listProjects shows the stores this Mac has recorded. It reads records only
// and starts nothing: a project key is a one-way digest, so the VM could not
// answer this question even if it were asked, and a user wanting to know what
// pisafe is holding should not have to boot a virtual machine to find out.
func listProjects(out io.Writer) error {
	store, err := runStore()
	if err != nil {
		return err
	}
	projects, unreadableProjects, err := store.ListProjects()
	if err != nil {
		return err
	}
	if len(projects) == 0 && len(unreadableProjects) == 0 {
		fmt.Fprintln(out, "No project stores.")
		return nil
	}
	runs, unreadable, err := store.List()
	if err != nil {
		return err
	}
	table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "CHECKOUT\tRUNS\tSTATUS")
	for _, project := range projects {
		held := 0
		for _, run := range runs {
			if run.ProjectKey == project.Key {
				held++
			}
		}
		fmt.Fprintf(
			table,
			"%s\t%d\t%s\n",
			project.Root,
			held,
			projectStatus(project, held, len(unreadable) > 0),
		)
	}
	// A store whose record cannot be read is listed under the key it is filed
	// by, because which checkout it came from is exactly what could not be read.
	for _, project := range unreadableProjects {
		fmt.Fprintf(table, "%s\t-\tunreadable\n", project.Key)
	}
	if err := table.Flush(); err != nil {
		return fmt.Errorf("write project list: %w", err)
	}
	for _, project := range unreadableProjects {
		fmt.Fprintf(
			out,
			"%s: %s.\nIts store is left whole and pisafe backup still copies its "+
				"transcripts out.\n",
			project.Key,
			project.Reason,
		)
	}
	if len(unreadable) > 0 {
		fmt.Fprintf(
			out,
			"%d run record(s) cannot be read by this version, and each could belong\n"+
				"to any of these projects; pisafe list names them.\n",
			len(unreadable),
		)
	}
	fmt.Fprintln(
		out,
		"A store holds this project's dependency caches and every transcript its\n"+
			"runs finished with. Only the transcripts are irreplaceable.",
	)
	return nil
}

// projectStatus says whether a store is being used. A run whose record cannot
// be read is a use that cannot be counted, so no store is called idle while
// one exists — calling it idle is what invites removing it.
func projectStatus(project runstate.ProjectRecord, runs int, uncounted bool) string {
	if project.MissingSince != nil {
		return "checkout " + missingSince(project)
	}
	if runs > 0 {
		return "in use"
	}
	if uncounted {
		return "unknown"
	}
	return "idle"
}

// missingSince says how long a project's checkout has been gone. A sweep dates
// a checkout the first time it finds it absent, so a record reported by the
// sweep that is about to date it has no date to show yet.
func missingSince(project runstate.ProjectRecord) string {
	if project.MissingSince == nil {
		return "first seen missing"
	}
	return "missing since " + project.MissingSince.Format(time.DateOnly)
}

func resetProject(ctx context.Context, path string, out io.Writer) error {
	project, err := namedProject(ctx, path)
	if err != nil {
		return err
	}
	controller, err := prepareLifecycle(ctx)
	if err != nil {
		return err
	}
	if err := controller.ResetProjectCache(ctx, project); err != nil {
		return err
	}
	fmt.Fprintf(
		out,
		"Emptied the shared cache of %s; its next run fetches from scratch.\n"+
			"Its session transcripts are untouched.\n",
		project.Root,
	)
	return nil
}

func dropProject(ctx context.Context, path string, out io.Writer) error {
	project, err := namedProject(ctx, path)
	if err != nil {
		return err
	}
	controller, err := prepareLifecycle(ctx)
	if err != nil {
		return err
	}
	if err := controller.DropProject(ctx, project); err != nil {
		return err
	}
	fmt.Fprintf(
		out,
		"Dropped the store of %s: its caches, its session transcripts, and the\n"+
			"filesystem holding them. Nothing reproduces the transcripts.\n",
		project.Root,
	)
	return nil
}

// rebindProject gives the checkout the user is standing in the history of the
// one they name. A project key is a digest of the checkout path, so moving or
// renaming a repository leaves its store behind under a key nothing reaches
// any more; this is what reaches it.
func rebindProject(ctx context.Context, oldPath string, out io.Writer) error {
	root, err := gitstage.RepositoryRoot(ctx, ".")
	if err != nil {
		return err
	}
	to, err := runid.NewProject(root)
	if err != nil {
		return err
	}
	from, err := namedProject(ctx, oldPath)
	if err != nil {
		return err
	}
	controller, err := prepareLifecycle(ctx)
	if err != nil {
		return err
	}
	if err := controller.RebindProject(ctx, from, to); err != nil {
		return err
	}
	fmt.Fprintf(
		out,
		"Moved the session history of %s to %s.\n"+
			"Its caches were left behind; the next run fetches from scratch.\n",
		from.Root,
		to.Root,
	)
	return nil
}

// namedProject identifies the store one path's runs share. A checkout is
// resolved the way a run resolves it, and a path that resolves to nothing is
// taken as it stands: the reason to name a project store is often that its
// checkout is gone, and the key is a digest of the path either way.
func namedProject(ctx context.Context, path string) (runid.Project, error) {
	root, err := gitstage.RepositoryRoot(ctx, path)
	if err != nil {
		root, err = filepath.Abs(path)
		if err != nil {
			return runid.Project{}, fmt.Errorf("resolve %q: %w", path, err)
		}
	}
	return runid.NewProject(root)
}
