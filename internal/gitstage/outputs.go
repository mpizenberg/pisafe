package gitstage

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mpizenberg/pisafe/internal/safefile"
)

// OutputsArtifactName is the archive a run hands back, holding the work it left
// under the paths the user included. The controller moves it out of the
// transfer directory by this name, so it is named once, here.
const OutputsArtifactName = "outputs.tar"

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
	archivePath := filepath.Join(packageDir, OutputsArtifactName)
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

// IncludedResult reports what a run's included work did to the host. Conflicts
// is the whole answer when it is set: nothing was written.
type IncludedResult struct {
	Written   []string `json:"written,omitempty"`
	Kept      []string `json:"kept,omitempty"`
	Conflicts []string `json:"conflicts,omitempty"`
}

// IncludedConflict reports that the host's copy of an included path moved while
// the run held its own. Copy-back is all or nothing, so one conflict holds every
// file back rather than leaving the host in a state neither side described.
type IncludedConflict struct {
	Paths []string
}

func (conflict *IncludedConflict) Error() string {
	return fmt.Sprintf(
		"%d included path(s) changed both in the run and on this Mac: %s",
		len(conflict.Paths),
		strings.Join(conflict.Paths, ", "),
	)
}

// CopyBack writes the work a run left under the included paths into the source
// working tree. It only ever creates and updates: a path the run removed stays
// on the host, so a run can never delete host data through this channel.
//
// Force overwrites conflicting paths instead of refusing them, which is what
// the user asks for after resolving one by hand.
func CopyBack(
	snapshot Snapshot,
	outputs []SelectedInput,
	archivePath string,
	force bool,
) (IncludedResult, error) {
	if err := requireOutputsUnderRoots(snapshot.IncludeRoots, outputs); err != nil {
		return IncludedResult{}, err
	}
	sourceRoot, err := filepath.EvalSymlinks(snapshot.SourceRoot)
	if err != nil {
		return IncludedResult{}, fmt.Errorf("resolve source repository: %w", err)
	}
	result, write, err := planCopyBack(sourceRoot, snapshot, outputs)
	if err != nil {
		return IncludedResult{}, err
	}
	if len(result.Conflicts) != 0 {
		if !force {
			return result, &IncludedConflict{Paths: result.Conflicts}
		}
		write = append(write, result.forced(outputs)...)
	}
	if len(write) == 0 {
		return result, nil
	}

	staging, err := os.MkdirTemp("", "pisafe-included-*")
	if err != nil {
		return IncludedResult{}, fmt.Errorf("reserve included work path: %w", err)
	}
	defer os.RemoveAll(staging)

	extracted, err := extractFileArchive(archivePath, staging)
	if err != nil {
		return IncludedResult{}, err
	}
	if !sameNames(outputs, extracted) {
		return IncludedResult{}, errors.New("outputs archive does not match what the run declared")
	}
	for _, output := range write {
		if err := placeIncluded(staging, sourceRoot, output); err != nil {
			return IncludedResult{}, err
		}
		result.Written = append(result.Written, output.Path)
	}
	sort.Strings(result.Written)
	return result, nil
}

// forced names the outputs a conflicted path stands for, so an overriding pass
// writes exactly what the refusing pass held back.
func (result IncludedResult) forced(outputs []SelectedInput) []SelectedInput {
	conflicted := make(map[string]bool, len(result.Conflicts))
	for _, path := range result.Conflicts {
		conflicted[path] = true
	}
	forced := make([]SelectedInput, 0, len(result.Conflicts))
	for _, output := range outputs {
		if conflicted[output.Path] {
			forced = append(forced, output)
		}
	}
	return forced
}

// planCopyBack decides each returned path against what the host holds now and
// what it held when the run was staged. A path is in conflict only when it
// changed on both sides: the host moving alone leaves the host's version
// standing, and the run moving alone is what copy-back is for.
func planCopyBack(
	sourceRoot string,
	snapshot Snapshot,
	outputs []SelectedInput,
) (IncludedResult, []SelectedInput, error) {
	carried := make(map[string]SelectedInput, len(snapshot.Inputs))
	for _, input := range snapshot.Inputs {
		carried[input.Path] = input
	}
	result := IncludedResult{}
	write := []SelectedInput{}
	returned := make(map[string]bool, len(outputs))
	for _, output := range outputs {
		returned[output.Path] = true
		host, err := hostState(sourceRoot, output.Path)
		if err != nil {
			return IncludedResult{}, nil, err
		}
		staged, carriedIn := carried[output.Path]
		switch {
		case !host.present:
			write = append(write, output)
		case host.usable && sameContent(host.content, output):
			// The host already holds what the run holds.
		case host.usable && carriedIn && sameContent(host.content, staged):
			// Untouched here since staging, so the run's version wins.
			write = append(write, output)
		case host.usable && carriedIn && sameContent(output, staged):
			// The run did not change it; whatever replaced it here stands.
		default:
			result.Conflicts = append(result.Conflicts, output.Path)
		}
	}
	for _, input := range snapshot.Inputs {
		if !returned[input.Path] {
			result.Kept = append(result.Kept, input.Path)
		}
	}
	sort.Strings(result.Conflicts)
	sort.Strings(result.Kept)
	return result, write, nil
}

// hostPath is what the source working tree holds at one path right now.
// Anything that is neither a regular file nor a symlink is present but not
// usable, which copy-back treats as a conflict rather than overwriting.
type hostPath struct {
	present bool
	usable  bool
	content SelectedInput
}

func hostState(sourceRoot, name string) (hostPath, error) {
	absolute := filepath.Join(sourceRoot, filepath.FromSlash(name))
	info, err := os.Lstat(absolute)
	if errors.Is(err, fs.ErrNotExist) {
		return hostPath{}, nil
	}
	if err != nil {
		return hostPath{}, fmt.Errorf("inspect %q: %w", name, err)
	}
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		link, err := os.Readlink(absolute)
		if err != nil {
			return hostPath{}, fmt.Errorf("read link %q: %w", name, err)
		}
		return hostPath{present: true, usable: true, content: SelectedInput{Link: link}}, nil
	case info.Mode().IsRegular():
		hash, err := fileSHA256(absolute)
		if err != nil {
			return hostPath{}, fmt.Errorf("hash %q: %w", name, err)
		}
		return hostPath{present: true, usable: true, content: SelectedInput{SHA256: hash}}, nil
	default:
		return hostPath{present: true}, nil
	}
}

// sameContent compares what two records hold rather than how they were
// recorded: a regular file always has a hash and no link target, a symlink
// always the reverse.
func sameContent(first, second SelectedInput) bool {
	return first.Link == second.Link && first.SHA256 == second.SHA256
}

// placeIncluded installs one returned path in the source working tree. Regular
// files go through a temporary file in their own directory, so a path is never
// half of itself under its own name.
func placeIncluded(staging, sourceRoot string, output SelectedInput) error {
	target := filepath.Join(sourceRoot, filepath.FromSlash(output.Path))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("create directory for %q: %w", output.Path, err)
	}
	source := filepath.Join(staging, filepath.FromSlash(output.Path))
	if output.Link != "" {
		link, err := os.Readlink(source)
		if err != nil {
			return fmt.Errorf("read returned link %q: %w", output.Path, err)
		}
		if err := os.Remove(target); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("replace %q: %w", output.Path, err)
		}
		if err := os.Symlink(link, target); err != nil {
			return fmt.Errorf("write returned link %q: %w", output.Path, err)
		}
		return nil
	}
	content, err := os.ReadFile(source)
	if err != nil {
		return fmt.Errorf("read returned file %q: %w", output.Path, err)
	}
	mode := fs.FileMode(0o644)
	if output.Executable {
		mode = 0o755
	}
	if err := safefile.Replace(target, content, mode); err != nil {
		return fmt.Errorf("write returned file %q: %w", output.Path, err)
	}
	return nil
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
