package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/mpizenberg/pisafe/internal/gitstage"
	"github.com/mpizenberg/pisafe/internal/runcopy"
	"github.com/mpizenberg/pisafe/internal/runctl"
)

// copyRequest is one parsed `pisafe cp`. Exactly one side of a copy carries a
// colon, and which side it is decides the direction: runPath is what is copied
// when the run is the source and where the copy lands when it is the
// destination, and localPath is the other end.
type copyRequest struct {
	runID     string
	inbound   bool
	runPath   string
	localPath string
	force     bool
	unsafe    bool
}

var errCopyUsage = fmt.Errorf(
	"usage: pisafe cp [RUN]:PATH [DEST] [--force]        take a copy out of a run\n" +
		"       pisafe cp PATH [RUN]: [--force] [--unsafe]  put one into a run",
)

// parseCopyRequest reads the copy arguments. A colon marks the side that is in
// a run, and the run's name before it is optional exactly as it is everywhere
// else. Taking a copy out is what a single path means, because that is the
// direction a path alone can only have meant.
func parseCopyRequest(args []string) (copyRequest, error) {
	request := copyRequest{}
	positional := []string{}
	for _, argument := range args {
		switch {
		case argument == "--force":
			request.force = true
		case argument == "--unsafe":
			request.unsafe = true
		case strings.HasPrefix(argument, "-"):
			return copyRequest{}, fmt.Errorf("unknown cp option %q\n%w", argument, errCopyUsage)
		default:
			positional = append(positional, argument)
		}
	}
	if len(positional) == 0 || len(positional) > 2 {
		return copyRequest{}, errCopyUsage
	}
	sourceRun, sourcePath, sourceInRun := strings.Cut(positional[0], ":")
	if len(positional) == 1 {
		if !sourceInRun {
			sourceRun, sourcePath = "", positional[0]
		}
		return request.takingOut(sourceRun, sourcePath, "")
	}
	destinationRun, destinationPath, destinationInRun := strings.Cut(positional[1], ":")
	switch {
	case sourceInRun && destinationInRun:
		return copyRequest{}, fmt.Errorf(
			"both %q and %q are in a run; a copy has one end on this Mac\n%w",
			positional[0],
			positional[1],
			errCopyUsage,
		)
	case sourceInRun:
		return request.takingOut(sourceRun, sourcePath, positional[1])
	case destinationInRun:
		request.runID = destinationRun
		request.inbound = true
		request.localPath = positional[0]
		if destinationPath != "" {
			cleaned, err := runcopy.SafePath(destinationPath)
			if err != nil {
				return copyRequest{}, err
			}
			request.runPath = cleaned
		}
		return request, nil
	}
	return copyRequest{}, fmt.Errorf(
		"neither %q nor %q is in a run; mark the one that is with a colon\n%w",
		positional[0],
		positional[1],
		errCopyUsage,
	)
}

// takingOut completes a request that reads out of a run. The destination
// defaults to the copied path's own name in the current directory, and an
// existing directory means inside it, both of which is what cp does.
func (request copyRequest) takingOut(runID, sourcePath, destination string) (copyRequest, error) {
	cleaned, err := runcopy.SafePath(sourcePath)
	if err != nil {
		return copyRequest{}, err
	}
	request.runID = runID
	request.runPath = cleaned
	request.localPath = path.Base(cleaned)
	if destination != "" {
		request.localPath = destinationFor(destination, cleaned)
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
	if request.inbound && !request.unsafe && gitstage.LooksLikeSecret(request.localPath) {
		return fmt.Errorf(
			"%q looks like a credential; everything in the run could read and "+
				"exfiltrate it. Pass --unsafe to copy it in anyway",
			request.localPath,
		)
	}
	request.runID, err = resolveRunID(ctx, request.runID)
	if err != nil {
		return err
	}
	controller, imageID, err := prepareInspection(ctx)
	if err != nil {
		return err
	}
	if request.inbound {
		entries, err := controller.CopyIn(ctx, runctl.CopyIntoRequest{
			RunID:       request.runID,
			ImageID:     imageID,
			Source:      request.localPath,
			Destination: request.runPath,
			Replace:     request.force,
		})
		if err != nil {
			return err
		}
		printCopyResult(out, request, entries)
		return nil
	}
	entries, err := controller.CopyOut(ctx, runctl.CopyRequest{
		RunID:       request.runID,
		ImageID:     imageID,
		Path:        request.runPath,
		Destination: request.localPath,
		Replace:     request.force,
	})
	if err != nil {
		return err
	}
	printCopyResult(out, request, entries)
	return nil
}

// printCopyResult lists what arrived. Coming out of a run every name was chosen
// inside it, so they are quoted rather than written to the terminal as they
// stand, and going in they are quoted for the symmetry of reading one report.
func printCopyResult(out io.Writer, request copyRequest, entries []runcopy.Entry) {
	files, total := 0, int64(0)
	for _, entry := range entries {
		if entry.Directory {
			continue
		}
		files++
		total += entry.Size
	}
	source, destination := request.runID+":"+request.runPath, filepath.Clean(request.localPath)
	if request.inbound {
		source, destination = request.localPath, request.runID+":"+request.runPath
	}
	fmt.Fprintf(out, "Copied:    %s\n", source)
	printed := 0
	for _, entry := range entries {
		if entry.Directory {
			continue
		}
		if printed == maximumNames {
			printMoreNames(out, files-printed)
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
		destination,
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
