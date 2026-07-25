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

// listSubmodules reports the initialized submodules of one repository.
// Uninitialized submodules are left alone: they stay uninitialized in the run,
// exactly as they are on the Mac.
func listSubmodules(ctx context.Context, root string) ([]SubmoduleStage, error) {
	output, err := gitOutputBytes(ctx, root, "ls-files", "-z", "--stage")
	if err != nil {
		return nil, fmt.Errorf("inspect index: %w", err)
	}
	staged := []SubmoduleStage{}
	for _, entry := range splitNULBytes(output) {
		if !bytes.HasPrefix(entry, []byte("160000 ")) {
			continue
		}
		tab := bytes.IndexByte(entry, '\t')
		if tab < 0 {
			return nil, fmt.Errorf("unreadable index entry for a submodule")
		}
		path := string(entry[tab+1:])
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
	output, err := gitOutputBytes(ctx, root, "ls-files", "-z", "--stage")
	if err != nil {
		return fmt.Errorf("inspect index: %w", err)
	}
	for _, entry := range splitNULBytes(output) {
		if bytes.HasPrefix(entry, []byte("160000 ")) {
			return ErrNestedSubmodules
		}
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
	names, err := submoduleNames(ctx, workspace)
	if err != nil {
		return nil, err
	}

	restored := make([]SubmoduleStage, 0, len(staged))
	for index, submodule := range staged {
		artifacts := prepared.Submodules[index]
		if artifacts.Path != submodule.Path {
			return nil, errors.New("staged snapshot and submodule artifacts disagree")
		}
		if err := safeSubmodulePath(submodule.Path); err != nil {
			return nil, err
		}
		target := filepath.Join(workspace, filepath.FromSlash(submodule.Path))
		if err := gitRun(
			ctx,
			"",
			nil,
			nil,
			"clone", "--quiet", artifacts.BundlePath, target,
		); err != nil {
			return nil, fmt.Errorf("clone submodule %q: %w", submodule.Path, err)
		}
		head, err := gitOutput(ctx, target, "rev-parse", "--verify", "HEAD")
		if err != nil {
			return nil, fmt.Errorf("resolve submodule %q HEAD: %w", submodule.Path, err)
		}
		if head != submodule.Head {
			return nil, fmt.Errorf(
				"submodule %q HEAD mismatch: wanted %s, got %s",
				submodule.Path,
				submodule.Head,
				head,
			)
		}
		// A submodule Git knows by name becomes a normal one: its git
		// directory moves under the superproject and its URL is registered.
		if _, named := names[submodule.Path]; named {
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

// submoduleNames maps submodule paths to the names .gitmodules gives them.
// A gitlink without a .gitmodules entry has no name and stays a plain
// checkout.
func submoduleNames(ctx context.Context, workspace string) (map[string]string, error) {
	if _, err := os.Stat(filepath.Join(workspace, ".gitmodules")); err != nil {
		return map[string]string{}, nil
	}
	output, err := gitOutputBytes(
		ctx,
		workspace,
		"config", "-f", ".gitmodules", "-z", "--get-regexp", `^submodule\..*\.path$`,
	)
	if err != nil {
		if isExitCode(err, 1) {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("read submodule configuration: %w", err)
	}
	names := map[string]string{}
	for _, entry := range splitNUL(output) {
		key, path, found := strings.Cut(entry, "\n")
		if !found {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(key, "submodule."), ".path")
		names[path] = name
	}
	return names, nil
}

// safeSubmodulePath refuses a path that would place a submodule outside the
// workspace or inside its Git directory.
func safeSubmodulePath(path string) error {
	cleaned := filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
	if path == "" || cleaned != path || filepath.IsAbs(filepath.FromSlash(path)) ||
		path == ".." || strings.HasPrefix(path, "../") {
		return fmt.Errorf("unsafe submodule path %q", path)
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == ".git" {
			return fmt.Errorf("unsafe submodule path %q", path)
		}
	}
	return nil
}
