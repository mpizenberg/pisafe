package gitstage

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

// BaselineChoice decides what apply does with the commit that carried the
// user's uncommitted work into a run. A run is imported once, so the choice is
// made before anything is captured and never revisited.
type BaselineChoice string

const (
	KeepBaseline BaselineChoice = "keep"
	DropBaseline BaselineChoice = "drop"
)

func ParseBaselineChoice(value string) (BaselineChoice, error) {
	switch BaselineChoice(value) {
	case KeepBaseline:
		return KeepBaseline, nil
	case DropBaseline:
		return DropBaseline, nil
	default:
		return "", fmt.Errorf("baseline choice must be %q or %q", KeepBaseline, DropBaseline)
	}
}

// BaselineReplayConflict reports that the run's own commits change lines the
// carried-in work also changed, so the baseline cannot be left out without a
// decision only the user can make.
type BaselineReplayConflict struct {
	Paths []string
}

func (conflict *BaselineReplayConflict) Error() string {
	return fmt.Sprintf(
		"the run's commits do not replay onto the captured HEAD without the baseline commit: %s",
		strings.Join(conflict.Paths, ", "),
	)
}

// SubmoduleBaselines names the submodules whose history starts with a baseline
// commit of their own. Every commit the run made in the superproject records
// where its submodules stood, so dropping one of those baselines would leave
// the imported superproject pointing at commits no branch keeps.
func (snapshot Snapshot) SubmoduleBaselines() []string {
	paths := []string{}
	for _, submodule := range snapshot.Submodules {
		if submodule.BaselineCommit != "" {
			paths = append(paths, submodule.Path)
		}
	}
	return paths
}

// applyBundleRef is the ref the superproject's apply bundle carries. Both sides
// derive it from the run and the choice, so neither has to be told.
func applyBundleRef(snapshot Snapshot, choice BaselineChoice) string {
	if choice == DropBaseline {
		return "refs/pisafe/replay/" + snapshot.RunID
	}
	return snapshot.WorkRef
}

// replayWithoutBaseline rewrites the run's history to start at the commit the
// source repository was on, leaving out the baseline commit. It works in a
// throwaway worktree and publishes the result under its own ref, so the run's
// branch and working tree are the same afterwards whether it succeeds or not.
func replayWithoutBaseline(
	ctx context.Context,
	snapshot Snapshot,
	workspace string,
	packageDir string,
) (tip string, conflicts []string, err error) {
	if snapshot.BaselineCommit == "" {
		return "", nil, fmt.Errorf("run has no baseline commit to leave out")
	}
	if paths := snapshot.SubmoduleBaselines(); len(paths) != 0 {
		return "", nil, fmt.Errorf(
			"submodule %q also carries uncommitted work, which the run's commits depend on",
			paths[0],
		)
	}
	if err := gitRun(
		ctx,
		workspace,
		nil,
		nil,
		"merge-base", "--is-ancestor", snapshot.BaselineCommit, snapshot.WorkRef,
	); err != nil {
		return "", nil, fmt.Errorf("the run's history no longer contains the baseline commit")
	}

	worktree := filepath.Join(packageDir, "replay")
	if err := gitRun(ctx, workspace, nil, nil, "worktree", "prune"); err != nil {
		return "", nil, fmt.Errorf("prune replay worktrees: %w", err)
	}
	if err := gitRun(
		ctx,
		workspace,
		nil,
		nil,
		"worktree", "add", "--quiet", "--detach", worktree, snapshot.WorkRef,
	); err != nil {
		return "", nil, fmt.Errorf("create replay worktree: %w", err)
	}
	defer func() {
		_ = gitRun(ctx, workspace, nil, nil, "worktree", "remove", "--force", worktree)
	}()

	rebaseArgs := append(
		append([]string{}, pisafeCommitConfig...),
		"rebase", "--quiet", "--empty=keep",
		"--onto", snapshot.SourceHead, snapshot.BaselineCommit,
	)
	if rebaseErr := gitRun(ctx, worktree, nil, nil, rebaseArgs...); rebaseErr != nil {
		unmerged, err := unmergedPaths(ctx, worktree)
		_ = gitRun(ctx, worktree, nil, nil, "rebase", "--abort")
		if err != nil {
			return "", nil, err
		}
		if len(unmerged) == 0 {
			return "", nil, fmt.Errorf("replay the run's commits: %w", rebaseErr)
		}
		return "", unmerged, nil
	}

	tip, err = gitOutput(ctx, worktree, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return "", nil, fmt.Errorf("resolve replayed tip: %w", err)
	}
	if err := gitRun(
		ctx,
		workspace,
		nil,
		nil,
		"update-ref", applyBundleRef(snapshot, DropBaseline), tip,
	); err != nil {
		return "", nil, fmt.Errorf("record replayed history: %w", err)
	}
	return tip, nil, nil
}

func unmergedPaths(ctx context.Context, worktree string) ([]string, error) {
	output, err := gitOutputBytes(
		ctx,
		worktree,
		"diff", "-z", "--name-only", "--diff-filter=U",
	)
	if err != nil {
		return nil, fmt.Errorf("list conflicting paths: %w", err)
	}
	return splitNUL(output), nil
}

// requireBaselineDropped proves the run really left the baseline out. The
// commit only ever existed inside the run, so a source repository that knows it
// at all learned it from the bundle that just arrived.
func requireBaselineDropped(ctx context.Context, repository, ref, baseline string) error {
	err := gitRun(ctx, repository, nil, nil, "cat-file", "-e", baseline)
	switch {
	case isExitCode(err, 1):
		return nil
	case err != nil:
		return fmt.Errorf("inspect baseline commit: %w", err)
	}
	if gitRun(ctx, repository, nil, nil, "merge-base", "--is-ancestor", baseline, ref) == nil {
		return fmt.Errorf("the run returned a history that still contains the baseline commit")
	}
	return nil
}
