package gitstage

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// applyOutputsName is the archive a run hands back, holding the work it left
// under the paths the user included.
const applyOutputsName = "outputs.tar"

// captureIncludedOutputs archives what a run leaves under the paths the user
// included: everything there the run does not track. What the agent chose to
// commit travels as history instead, so no path crosses twice.
func captureIncludedOutputs(
	ctx context.Context,
	snapshot Snapshot,
	workspace string,
	packageDir string,
) ([]SelectedInput, string, error) {
	names, err := untrackedUnderRoots(ctx, workspace, snapshot.IncludeRoots)
	if err != nil || len(names) == 0 {
		return nil, "", err
	}
	files, err := describeFiles(workspace, names)
	if err != nil {
		return nil, "", err
	}
	archivePath := filepath.Join(packageDir, applyOutputsName)
	if err := writeFileArchive(workspace, archivePath, files); err != nil {
		return nil, "", err
	}
	hash, err := fileSHA256(archivePath)
	if err != nil {
		return nil, "", fmt.Errorf("hash outputs archive: %w", err)
	}
	return files, hash, nil
}

// untrackedUnderRoots names every file under the included roots that the run
// does not track. Overlapping roots name the same file once.
func untrackedUnderRoots(
	ctx context.Context,
	workspace string,
	roots []string,
) ([]string, error) {
	if len(roots) == 0 {
		return nil, nil
	}
	found := []string{}
	pathspecs := make([]string, 0, len(roots))
	for _, root := range roots {
		if err := safePath("include root", root); err != nil {
			return nil, err
		}
		files, err := walkInput(workspace, root)
		if err != nil {
			return nil, err
		}
		found = append(found, files...)
		// The literal prefix keeps a path that happens to look like pathspec
		// magic from widening what Git reports as tracked.
		pathspecs = append(pathspecs, ":(literal)"+root)
	}
	if len(found) == 0 {
		return nil, nil
	}
	listed, err := gitOutputBytes(ctx, workspace, append(
		[]string{"--no-optional-locks", "ls-files", "-z", "--"},
		pathspecs...,
	)...)
	if err != nil {
		return nil, fmt.Errorf("list tracked paths under included roots: %w", err)
	}
	tracked := map[string]bool{}
	for _, name := range splitNUL(listed) {
		tracked[name] = true
	}
	names, seen := make([]string, 0, len(found)), map[string]bool{}
	for _, name := range found {
		if tracked[name] || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// requireOutputsUnderRoots proves every path a run hands back is one the user
// asked for. The list crosses the boundary out of the run, so where each path
// belongs is checked here rather than trusted.
func requireOutputsUnderRoots(roots []string, outputs []SelectedInput) error {
	for _, output := range outputs {
		if err := safePath("output", output.Path); err != nil {
			return err
		}
		if !underAnyRoot(output.Path, roots) {
			return fmt.Errorf(
				"run returned %q, which is not under an included path",
				output.Path,
			)
		}
	}
	return nil
}

func underAnyRoot(name string, roots []string) bool {
	for _, root := range roots {
		if name == root || strings.HasPrefix(name, root+"/") {
			return true
		}
	}
	return false
}

// withoutReturned drops the paths that are coming back from the report of what
// stayed in the run, so no file is described as both.
func withoutReturned(untracked []string, outputs []SelectedInput) []string {
	if len(outputs) == 0 {
		return untracked
	}
	returned := make(map[string]bool, len(outputs))
	for _, output := range outputs {
		returned[output.Path] = true
	}
	kept := make([]string, 0, len(untracked))
	for _, name := range untracked {
		if returned[name] {
			continue
		}
		kept = append(kept, name)
	}
	return kept
}
