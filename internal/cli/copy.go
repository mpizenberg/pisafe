package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/mpizenberg/pisafe/internal/runcopy"
	"github.com/mpizenberg/pisafe/internal/runctl"
)

// copyRequest is one parsed `pisafe cp RUN:PATH [DEST] [--force]`.
type copyRequest struct {
	runID       string
	path        string
	destination string
	force       bool
}

var errCopyUsage = fmt.Errorf("usage: pisafe cp [RUN:]PATH [DEST] [--force]")

// parseCopyRequest reads the copy arguments. The destination defaults to the
// copied path's own name in the current directory, and an existing directory
// means inside it, both of which is what cp does. A source naming no run comes
// from the checkout's own run; a path that carries a colon has to name one.
func parseCopyRequest(args []string) (copyRequest, error) {
	request := copyRequest{}
	positional := []string{}
	for _, argument := range args {
		if argument == "--force" {
			request.force = true
			continue
		}
		if strings.HasPrefix(argument, "-") {
			return copyRequest{}, fmt.Errorf("unknown cp option %q\n%w", argument, errCopyUsage)
		}
		positional = append(positional, argument)
	}
	if len(positional) == 0 || len(positional) > 2 {
		return copyRequest{}, errCopyUsage
	}
	sourcePath := positional[0]
	if runID, rest, found := strings.Cut(sourcePath, ":"); found {
		if runID == "" {
			return copyRequest{}, fmt.Errorf(
				"%q does not name a run and a path\n%w",
				positional[0],
				errCopyUsage,
			)
		}
		request.runID, sourcePath = runID, rest
	}
	cleaned, err := runcopy.SafePath(sourcePath)
	if err != nil {
		return copyRequest{}, err
	}
	request.path = cleaned
	request.destination = path.Base(cleaned)
	if len(positional) == 2 {
		request.destination = destinationFor(positional[1], cleaned)
	}
	return request, nil
}

// destinationFor resolves where a copy lands. A destination that is already a
// directory takes the copied path inside it under its own name, so replacing
// the directory is something only an explicit name asks for.
func destinationFor(destination, sourcePath string) string {
	if info, err := os.Stat(destination); err == nil && info.IsDir() {
		return filepath.Join(destination, path.Base(sourcePath))
	}
	return destination
}

func runCopy(ctx context.Context, args []string, out io.Writer) error {
	request, err := parseCopyRequest(args)
	if err != nil {
		return err
	}
	request.runID, err = resolveRunID(ctx, request.runID)
	if err != nil {
		return err
	}
	controller, imageID, err := prepareInspection(ctx)
	if err != nil {
		return err
	}
	entries, err := controller.CopyOut(ctx, runctl.CopyRequest{
		RunID:       request.runID,
		ImageID:     imageID,
		Path:        request.path,
		Destination: request.destination,
		Replace:     request.force,
	})
	if err != nil {
		return err
	}
	printCopyResult(out, request, entries)
	return nil
}

// printCopyResult lists what arrived. Every name was chosen inside the run, so
// it is quoted rather than written to the terminal as it stands.
func printCopyResult(out io.Writer, request copyRequest, entries []runcopy.Entry) {
	const maximumNames = 12
	files, total := 0, int64(0)
	for _, entry := range entries {
		if entry.Directory {
			continue
		}
		files++
		total += entry.Size
	}
	fmt.Fprintf(out, "Copied:    %s:%s\n", request.runID, request.path)
	printed := 0
	for _, entry := range entries {
		if entry.Directory {
			continue
		}
		if printed == maximumNames {
			fmt.Fprintf(out, "           ... and %d more\n", files-printed)
			break
		}
		fmt.Fprintf(out, "           %9s %q\n", humanBytes(entry.Size), entry.Path)
		printed++
	}
	fmt.Fprintf(
		out,
		"Wrote:     %d file(s), %s to %q\n",
		files,
		humanBytes(total),
		filepath.Clean(request.destination),
	)
}

func humanBytes(size int64) string {
	switch {
	case size >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(size)/(1<<30))
	case size >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(size)/(1<<20))
	case size >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(size)/(1<<10))
	default:
		return fmt.Sprintf("%d B", size)
	}
}
