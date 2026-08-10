package gitstage

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mpizenberg/pisafe/internal/runid"
)

// RunDiff is what a run changed since it began. It is computed inside the
// isolated environment and crosses the boundary as one bounded reply, so every
// list is capped and paired with the total it was cut from.
type RunDiff struct {
	RunID        string           `json:"run_id"`
	Repositories []RepositoryDiff `json:"repositories"`
}

// RepositoryDiff covers the superproject, whose Path is empty, or one
// submodule. Base is the commit the run started from rather than the source
// HEAD, so dirty state the user carried in is not reported as the run's work.
type RepositoryDiff struct {
	Path           string       `json:"path,omitempty"`
	Base           string       `json:"base"`
	Head           string       `json:"head"`
	Commits        []DiffCommit `json:"commits,omitempty"`
	CommitTotal    int          `json:"commit_total"`
	Files          []DiffFile   `json:"files,omitempty"`
	FileTotal      int          `json:"file_total"`
	Untracked      []string     `json:"untracked,omitempty"`
	UntrackedTotal int          `json:"untracked_total"`
}

type DiffCommit struct {
	Commit  string `json:"commit"`
	Subject string `json:"subject"`
}

// DiffFile counts the lines a path gained and lost between the base commit and
// the current working tree. Both are -1 for a binary file, which Git reports
// instead of a line count.
type DiffFile struct {
	Path       string `json:"path"`
	Insertions int    `json:"insertions"`
	Deletions  int    `json:"deletions"`
}

const (
	// diffListLimit caps each list a run hands back. A run that rewrites ten
	// thousand files still has to fit in one bounded reply, and the totals stay
	// exact so nothing is silently dropped.
	diffListLimit = 200
	// diffSubjectLimit bounds one untrusted commit subject.
	diffSubjectLimit = 120
)

// DiffRun reports what a run changed, reading its workspace without writing to
// it: optional index locks are disabled so this can run while an agent works.
func DiffRun(ctx context.Context, snapshot Snapshot, workspace string) (RunDiff, error) {
	if err := runid.Validate(snapshot.RunID); err != nil {
		return RunDiff{}, err
	}
	superproject, err := diffRepository(ctx, filepath.Clean(workspace), snapshot.Base(), true)
	if err != nil {
		return RunDiff{}, err
	}
	diff := RunDiff{RunID: snapshot.RunID, Repositories: []RepositoryDiff{superproject}}
	for _, submodule := range snapshot.Submodules {
		if err := safePath("submodule", submodule.Path); err != nil {
			return RunDiff{}, err
		}
		repository, err := diffRepository(
			ctx,
			filepath.Join(filepath.Clean(workspace), filepath.FromSlash(submodule.Path)),
			submodule.Base(),
			false,
		)
		if err != nil {
			return RunDiff{}, fmt.Errorf("submodule %q: %w", submodule.Path, err)
		}
		repository.Path = submodule.Path
		diff.Repositories = append(diff.Repositories, repository)
	}
	return diff, nil
}

func diffRepository(
	ctx context.Context,
	repository string,
	base string,
	ignoreSubmodules bool,
) (RepositoryDiff, error) {
	head, err := gitOutput(ctx, repository, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return RepositoryDiff{}, fmt.Errorf("resolve run head: %w", err)
	}
	if err := requireAncestor(
		ctx,
		repository,
		base,
		head,
		"run history is not based on the staged commit",
	); err != nil {
		return RepositoryDiff{}, err
	}
	commits, commitTotal, err := diffCommits(ctx, repository, base)
	if err != nil {
		return RepositoryDiff{}, err
	}
	files, fileTotal, err := diffFiles(ctx, repository, base, ignoreSubmodules)
	if err != nil {
		return RepositoryDiff{}, err
	}
	untracked, err := gitOutputBytes(
		ctx,
		repository,
		"--no-optional-locks", "ls-files", "-z", "--others", "--exclude-standard",
	)
	if err != nil {
		return RepositoryDiff{}, fmt.Errorf("list untracked files: %w", err)
	}
	names := splitNUL(untracked)
	return RepositoryDiff{
		Base:           base,
		Head:           head,
		Commits:        commits,
		CommitTotal:    commitTotal,
		Files:          files,
		FileTotal:      fileTotal,
		Untracked:      names[:min(len(names), diffListLimit)],
		UntrackedTotal: len(names),
	}, nil
}

func diffCommits(ctx context.Context, repository, base string) ([]DiffCommit, int, error) {
	counted, err := gitOutput(ctx, repository, "rev-list", "--count", base+"..HEAD")
	if err != nil {
		return nil, 0, fmt.Errorf("count run commits: %w", err)
	}
	total, err := strconv.Atoi(counted)
	if err != nil {
		return nil, 0, fmt.Errorf("parse run commit count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}
	listed, err := gitOutputBytes(
		ctx,
		repository,
		"log", "-z",
		"--max-count="+strconv.Itoa(diffListLimit),
		"--format=%H %s",
		base+"..HEAD",
	)
	if err != nil {
		return nil, 0, fmt.Errorf("list run commits: %w", err)
	}
	commits := []DiffCommit{}
	for _, entry := range splitNUL(listed) {
		hash, subject, _ := strings.Cut(entry, " ")
		if len(subject) > diffSubjectLimit {
			subject = subject[:diffSubjectLimit]
		}
		commits = append(commits, DiffCommit{Commit: hash, Subject: subject})
	}
	return commits, total, nil
}

// diffFiles compares the base commit with the current working tree. Renames are
// reported as a deletion and an addition so every record has exactly one path,
// and submodule changes are left to each submodule's own report.
func diffFiles(
	ctx context.Context,
	repository string,
	base string,
	ignoreSubmodules bool,
) ([]DiffFile, int, error) {
	args := []string{"--no-optional-locks", "diff", "--numstat", "-z", "--no-renames"}
	if ignoreSubmodules {
		args = append(args, "--ignore-submodules=all")
	}
	output, err := gitOutputBytes(ctx, repository, append(args, base, "--")...)
	if err != nil {
		return nil, 0, fmt.Errorf("compare run working tree: %w", err)
	}
	entries := splitNUL(output)
	files := []DiffFile{}
	for _, entry := range entries {
		if len(files) == diffListLimit {
			break
		}
		file, err := parseNumstat(entry)
		if err != nil {
			return nil, 0, err
		}
		files = append(files, file)
	}
	return files, len(entries), nil
}

func parseNumstat(entry string) (DiffFile, error) {
	fields := strings.SplitN(entry, "\t", 3)
	if len(fields) != 3 {
		return DiffFile{}, fmt.Errorf("unexpected diff record %q", entry)
	}
	insertions, err := numstatCount(fields[0])
	if err != nil {
		return DiffFile{}, err
	}
	deletions, err := numstatCount(fields[1])
	if err != nil {
		return DiffFile{}, err
	}
	return DiffFile{Path: fields[2], Insertions: insertions, Deletions: deletions}, nil
}

// numstatCount reads one side of a numstat record. Git writes "-" for a binary
// file, which has no line count at all.
func numstatCount(field string) (int, error) {
	if field == "-" {
		return -1, nil
	}
	count, err := strconv.Atoi(field)
	if err != nil {
		return 0, fmt.Errorf("parse diff line count %q: %w", field, err)
	}
	return count, nil
}
