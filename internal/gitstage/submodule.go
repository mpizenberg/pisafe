package gitstage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// SubmoduleStage records one initialized submodule staged alongside the
// superproject. Head is the commit the source working tree actually had, which
// is not necessarily the commit the superproject index recorded.
type SubmoduleStage struct {
	Path           string `json:"path"`
	Head           string `json:"head"`
	BaselineCommit string `json:"baseline_commit,omitempty"`
}

// Base is the commit the run's work in this submodule starts from, on the same
// terms as the superproject's.
func (submodule SubmoduleStage) Base() string {
	if submodule.BaselineCommit != "" {
		return submodule.BaselineCommit
	}
	return submodule.Head
}

// PreparedSubmodule locates the artifacts of one staged submodule. Paths are
// meaningful only on the side that holds the stage package.
type PreparedSubmodule struct {
	Path       string
	BundlePath string
	PatchPath  string
}

var ErrNestedSubmodules = errors.New("submodules containing submodules are not supported")

func submoduleBundleName(index int) string {
	return "submodule-" + strconv.Itoa(index) + ".bundle"
}

func submodulePatchName(index int) string {
	return "submodule-" + strconv.Itoa(index) + ".patch"
}

// gitlinks reports the submodule paths one index records, which is what a
// submodule is before anything asks whether it has been initialized.
func gitlinks(ctx context.Context, root string) ([]string, error) {
	output, err := gitOutputBytes(ctx, root, "ls-files", "-z", "--stage")
	if err != nil {
		return nil, fmt.Errorf("inspect index: %w", err)
	}
	paths := []string{}
	for _, entry := range splitNULBytes(output) {
		if !bytes.HasPrefix(entry, []byte("160000 ")) {
			continue
		}
		tab := bytes.IndexByte(entry, '\t')
		if tab < 0 {
			return nil, errors.New("unreadable index entry for a submodule")
		}
		paths = append(paths, string(entry[tab+1:]))
	}
	return paths, nil
}

// listSubmodules reports the initialized submodules of one repository.
// Uninitialized submodules are left alone: they stay uninitialized in the run,
// exactly as they are on the Mac.
func listSubmodules(ctx context.Context, root string) ([]SubmoduleStage, error) {
	paths, err := gitlinks(ctx, root)
	if err != nil {
		return nil, err
	}
	staged := []SubmoduleStage{}
	for _, path := range paths {
		working := filepath.Join(root, filepath.FromSlash(path))
		if _, err := os.Stat(filepath.Join(working, ".git")); err != nil {
			continue
		}
		head, err := gitOutput(ctx, working, "rev-parse", "--verify", "HEAD")
		if err != nil {
			return nil, fmt.Errorf("resolve submodule %q HEAD: %w", path, err)
		}
		if err := rejectNestedSubmodules(ctx, working); err != nil {
			return nil, fmt.Errorf("submodule %q: %w", path, err)
		}
		if err := rejectLFS(ctx, working); err != nil {
			return nil, fmt.Errorf("submodule %q: %w", path, err)
		}
		staged = append(staged, SubmoduleStage{Path: path, Head: head})
	}
	return staged, nil
}

func rejectNestedSubmodules(ctx context.Context, root string) error {
	nested, err := gitlinks(ctx, root)
	if err != nil {
		return err
	}
	if len(nested) != 0 {
		return ErrNestedSubmodules
	}
	return nil
}

// captureRepository writes the bundle and tracked-state patch for one
// repository. Submodule changes are excluded from the patch because each
// submodule carries its own artifacts and its gitlink is recorded from the
// materialized result.
func captureRepository(ctx context.Context, root, bundlePath, patchPath string) error {
	if err := gitRun(ctx, root, nil, nil, "bundle", "create", bundlePath, "HEAD"); err != nil {
		return fmt.Errorf("create bundle: %w", err)
	}
	patch, err := gitOutputBytes(
		ctx,
		root,
		"diff", "--binary", "--full-index", "--no-ext-diff", "--no-textconv",
		"--ignore-submodules=all", "HEAD", "--",
	)
	if err != nil {
		return fmt.Errorf("capture tracked working tree: %w", err)
	}
	if err := os.WriteFile(patchPath, patch, 0o600); err != nil {
		return fmt.Errorf("write tracked patch: %w", err)
	}
	return nil
}

// restoreSubmodules reconstructs each staged submodule inside the materialized
// workspace and stages the gitlink each one ended up at.
func restoreSubmodules(
	ctx context.Context,
	prepared PreparedStage,
	workspace string,
) ([]SubmoduleStage, error) {
	staged := prepared.Snapshot.Submodules
	if len(staged) == 0 {
		return nil, nil
	}
	if len(prepared.Submodules) != len(staged) {
		return nil, errors.New("staged snapshot and submodule artifacts disagree")
	}
	named, err := namedSubmodules(ctx, workspace)
	if err != nil {
		return nil, err
	}

	restored := make([]SubmoduleStage, 0, len(staged))
	for index, submodule := range staged {
		artifacts := prepared.Submodules[index]
		if artifacts.Path != submodule.Path {
			return nil, errors.New("staged snapshot and submodule artifacts disagree")
		}
		if err := safePath("submodule", submodule.Path); err != nil {
			return nil, err
		}
		target := filepath.Join(workspace, filepath.FromSlash(submodule.Path))
		if err := cloneStaged(ctx, artifacts.BundlePath, target, submodule.Head); err != nil {
			return nil, fmt.Errorf("submodule %q: %w", submodule.Path, err)
		}
		// A submodule Git knows by name becomes a normal one: its git
		// directory moves under the superproject and its URL is registered.
		if named[submodule.Path] {
			for _, args := range [][]string{
				{"submodule", "absorbgitdirs", "--", submodule.Path},
				{"submodule", "init", "--", submodule.Path},
			} {
				if err := gitRun(ctx, workspace, nil, nil, args...); err != nil {
					return nil, fmt.Errorf("register submodule %q: %w", submodule.Path, err)
				}
			}
		}
		baseline, err := applySubmoduleBaseline(ctx, target, artifacts.PatchPath)
		if err != nil {
			return nil, fmt.Errorf("submodule %q: %w", submodule.Path, err)
		}
		if err := gitRun(
			ctx,
			workspace,
			nil,
			nil,
			"add", "--", submodule.Path,
		); err != nil {
			return nil, fmt.Errorf("stage submodule %q: %w", submodule.Path, err)
		}
		restored = append(restored, SubmoduleStage{
			Path:           submodule.Path,
			Head:           submodule.Head,
			BaselineCommit: baseline,
		})
	}
	return restored, nil
}

func applySubmoduleBaseline(ctx context.Context, target, patchPath string) (string, error) {
	patch, err := os.ReadFile(patchPath)
	if err != nil {
		return "", fmt.Errorf("read tracked patch: %w", err)
	}
	if len(patch) == 0 {
		return "", nil
	}
	if err := gitRun(
		ctx,
		target,
		bytes.NewReader(patch),
		nil,
		"apply", "--index", "--binary", "-",
	); err != nil {
		return "", fmt.Errorf("apply tracked baseline: %w", err)
	}
	if err := commit(ctx, target, baselineMessage); err != nil {
		return "", fmt.Errorf("commit tracked baseline: %w", err)
	}
	return gitOutput(ctx, target, "rev-parse", "HEAD")
}

// namedSubmodules reports which paths .gitmodules gives a name to. A gitlink
// without an entry has no name and stays a plain checkout.
func namedSubmodules(ctx context.Context, workspace string) (map[string]bool, error) {
	if _, err := os.Stat(filepath.Join(workspace, ".gitmodules")); err != nil {
		return map[string]bool{}, nil
	}
	output, err := gitOutputBytes(
		ctx,
		workspace,
		"config", "-f", ".gitmodules", "-z", "--get-regexp", `^submodule\..*\.path$`,
	)
	if err != nil {
		if isExitCode(err, 1) {
			return map[string]bool{}, nil
		}
		return nil, fmt.Errorf("read submodule configuration: %w", err)
	}
	named := map[string]bool{}
	for _, entry := range splitNUL(output) {
		if _, path, found := strings.Cut(entry, "\n"); found {
			named[path] = true
		}
	}
	return named, nil
}
