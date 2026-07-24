// Package gitstage implements the host-side Git boundary for a pisafe run.
//
// It deliberately knows nothing about Lima, SSH, or containers. A caller can
// stream the bundle into a mountless VM and invoke the same staging operations
// there without granting the VM access to the source checkout.
package gitstage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/mpizenberg/pisafe/internal/runid"
)

const (
	baselineMessage = "pisafe: imported working-tree baseline"
	finalMessage    = "pisafe: captured final working-tree state"
)

var (
	ErrBranchExists       = errors.New("pisafe import branch already exists")
	ErrSubmodulesNotReady = errors.New("submodule staging is not implemented yet")
	ErrLFSNotSupported    = errors.New("Git LFS repositories are not supported yet")
)

type Snapshot struct {
	RunID          string    `json:"run_id"`
	SourceRoot     string    `json:"source_root"`
	SourceHead     string    `json:"source_head"`
	WorkRef        string    `json:"work_ref"`
	BaselineCommit string    `json:"baseline_commit,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

type ApplyResult struct {
	Branch       string
	Tip          string
	FinalCommit  string
	Untracked    []string
	BundleSHA256 string
}

type PreparedStage struct {
	Snapshot   Snapshot
	BundlePath string
	PatchPath  string
}

type ExcludedInputs struct {
	Untracked []string
	Ignored   []string
}

func RepositoryRoot(ctx context.Context, sourcePath string) (string, error) {
	root, err := gitOutput(ctx, sourcePath, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("find repository: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve repository path: %w", err)
	}
	return root, nil
}

func ListExcludedInputs(ctx context.Context, sourcePath string) (ExcludedInputs, error) {
	root, err := RepositoryRoot(ctx, sourcePath)
	if err != nil {
		return ExcludedInputs{}, err
	}
	untracked, err := gitOutputBytes(
		ctx,
		root,
		"ls-files", "-z", "--others", "--exclude-standard",
	)
	if err != nil {
		return ExcludedInputs{}, fmt.Errorf("list untracked inputs: %w", err)
	}
	ignored, err := gitOutputBytes(
		ctx,
		root,
		"ls-files", "-z", "--others", "--ignored", "--exclude-standard",
	)
	if err != nil {
		return ExcludedInputs{}, fmt.Errorf("list ignored inputs: %w", err)
	}
	return ExcludedInputs{
		Untracked: splitNUL(untracked),
		Ignored:   splitNUL(ignored),
	}, nil
}

// Prepare creates the only two source artifacts that cross the VM boundary:
// a Git bundle rooted at HEAD and a binary patch of the final tracked state.
// packageDir must not already exist.
func Prepare(ctx context.Context, sourcePath, packageDir, runID string) (prepared PreparedStage, err error) {
	if err := runid.Validate(runID); err != nil {
		return PreparedStage{}, err
	}
	if _, err := os.Stat(packageDir); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return PreparedStage{}, fmt.Errorf("stage package already exists: %s", packageDir)
		}
		return PreparedStage{}, fmt.Errorf("inspect stage package: %w", err)
	}

	root, err := RepositoryRoot(ctx, sourcePath)
	if err != nil {
		return PreparedStage{}, err
	}
	head, err := gitOutput(ctx, root, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return PreparedStage{}, fmt.Errorf("resolve HEAD: %w", err)
	}
	if err := rejectGitlinks(ctx, root); err != nil {
		return PreparedStage{}, err
	}
	if err := rejectLFS(ctx, root); err != nil {
		return PreparedStage{}, err
	}

	if err := os.Mkdir(packageDir, 0o700); err != nil {
		return PreparedStage{}, fmt.Errorf("create stage package: %w", err)
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(packageDir)
		}
	}()

	bundlePath := filepath.Join(packageDir, "source.bundle")
	if err := gitRun(ctx, root, nil, nil, "bundle", "create", bundlePath, "HEAD"); err != nil {
		return PreparedStage{}, fmt.Errorf("create staging bundle: %w", err)
	}
	patch, err := gitOutputBytes(
		ctx,
		root,
		"diff", "--binary", "--full-index", "--no-ext-diff", "--no-textconv", "HEAD", "--",
	)
	if err != nil {
		return PreparedStage{}, fmt.Errorf("capture tracked working tree: %w", err)
	}
	headAfter, err := gitOutput(ctx, root, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return PreparedStage{}, fmt.Errorf("recheck HEAD: %w", err)
	}
	if headAfter != head {
		return PreparedStage{}, fmt.Errorf("repository HEAD changed while preparing the run")
	}
	patchPath := filepath.Join(packageDir, "tracked.patch")
	if err := os.WriteFile(patchPath, patch, 0o600); err != nil {
		return PreparedStage{}, fmt.Errorf("write tracked patch: %w", err)
	}

	snapshot := Snapshot{
		RunID:      runID,
		SourceRoot: root,
		SourceHead: head,
		WorkRef:    "refs/heads/work/" + runID,
		CreatedAt:  time.Now().UTC(),
	}
	complete = true
	return PreparedStage{
		Snapshot:   snapshot,
		BundlePath: bundlePath,
		PatchPath:  patchPath,
	}, nil
}

// Materialize consumes a transferred stage package inside the isolated
// environment. It does not access Snapshot.SourceRoot.
func Materialize(ctx context.Context, prepared PreparedStage, workspace string) (snapshot Snapshot, err error) {
	if err := runid.Validate(prepared.Snapshot.RunID); err != nil {
		return Snapshot{}, err
	}
	if prepared.Snapshot.WorkRef != "refs/heads/work/"+prepared.Snapshot.RunID {
		return Snapshot{}, fmt.Errorf("work ref does not match run ID")
	}
	if _, err := os.Stat(workspace); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return Snapshot{}, fmt.Errorf("workspace already exists: %s", workspace)
		}
		return Snapshot{}, fmt.Errorf("inspect workspace: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(workspace), 0o700); err != nil {
		return Snapshot{}, fmt.Errorf("create workspace parent: %w", err)
	}
	if err := gitRun(ctx, "", nil, nil, "clone", "--quiet", prepared.BundlePath, workspace); err != nil {
		return Snapshot{}, fmt.Errorf("clone staged repository: %w", err)
	}

	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(workspace)
		}
	}()

	clonedHead, err := gitOutput(ctx, workspace, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return Snapshot{}, fmt.Errorf("resolve staged HEAD: %w", err)
	}
	if clonedHead != prepared.Snapshot.SourceHead {
		return Snapshot{}, fmt.Errorf(
			"staged HEAD mismatch: wanted %s, got %s",
			prepared.Snapshot.SourceHead,
			clonedHead,
		)
	}
	if err := gitRun(
		ctx,
		workspace,
		nil,
		nil,
		"switch", "--quiet", "-c", "work/"+prepared.Snapshot.RunID,
	); err != nil {
		return Snapshot{}, fmt.Errorf("create work branch: %w", err)
	}

	patch, err := os.ReadFile(prepared.PatchPath)
	if err != nil {
		return Snapshot{}, fmt.Errorf("read tracked patch: %w", err)
	}

	baseline := ""
	if len(patch) != 0 {
		if err := gitRun(ctx, workspace, bytes.NewReader(patch), nil, "apply", "--index", "--binary", "-"); err != nil {
			return Snapshot{}, fmt.Errorf("apply tracked baseline: %w", err)
		}
		if err := commit(ctx, workspace, baselineMessage); err != nil {
			return Snapshot{}, fmt.Errorf("commit tracked baseline: %w", err)
		}
		baseline, err = gitOutput(ctx, workspace, "rev-parse", "HEAD")
		if err != nil {
			return Snapshot{}, fmt.Errorf("resolve baseline commit: %w", err)
		}
	}

	cleanup = false
	snapshot = prepared.Snapshot
	snapshot.BaselineCommit = baseline
	return snapshot, nil
}

// Stage is a local composition of Prepare and Materialize, primarily useful
// in tests. The controller uses the two operations separately with an SSH
// transfer between them.
func Stage(ctx context.Context, sourcePath, workspace, runID string) (Snapshot, error) {
	packageDir, err := os.MkdirTemp(filepath.Dir(workspace), ".pisafe-stage-package-*")
	if err != nil {
		return Snapshot{}, fmt.Errorf("reserve stage package path: %w", err)
	}
	if err := os.Remove(packageDir); err != nil {
		return Snapshot{}, fmt.Errorf("prepare stage package path: %w", err)
	}
	defer os.RemoveAll(packageDir)

	prepared, err := Prepare(ctx, sourcePath, packageDir, runID)
	if err != nil {
		return Snapshot{}, err
	}
	return Materialize(ctx, prepared, workspace)
}

func FinalizeTracked(ctx context.Context, workspace string) (commitID string, untracked []string, err error) {
	if err := gitRun(ctx, workspace, nil, nil, "add", "-u", "--"); err != nil {
		return "", nil, fmt.Errorf("stage final tracked changes: %w", err)
	}

	untrackedOutput, err := gitOutputBytes(
		ctx,
		workspace,
		"ls-files", "-z", "--others", "--exclude-standard",
	)
	if err != nil {
		return "", nil, fmt.Errorf("list final untracked files: %w", err)
	}
	untracked = splitNUL(untrackedOutput)

	quietErr := gitRun(ctx, workspace, nil, nil, "diff", "--cached", "--quiet", "--")
	switch {
	case quietErr == nil:
		return "", untracked, nil
	case !isExitCode(quietErr, 1):
		return "", nil, fmt.Errorf("inspect final tracked changes: %w", quietErr)
	}

	if err := commit(ctx, workspace, finalMessage); err != nil {
		return "", nil, fmt.Errorf("commit final tracked state: %w", err)
	}
	commitID, err = gitOutput(ctx, workspace, "rev-parse", "HEAD")
	if err != nil {
		return "", nil, fmt.Errorf("resolve final commit: %w", err)
	}
	return commitID, untracked, nil
}

func rejectGitlinks(ctx context.Context, root string) error {
	output, err := gitOutputBytes(ctx, root, "ls-files", "-z", "--stage")
	if err != nil {
		return fmt.Errorf("inspect index: %w", err)
	}
	for _, entry := range splitNULBytes(output) {
		if bytes.HasPrefix(entry, []byte("160000 ")) {
			return ErrSubmodulesNotReady
		}
	}
	return nil
}

func rejectLFS(ctx context.Context, root string) error {
	files, err := gitOutputBytes(ctx, root, "ls-files", "-z")
	if err != nil {
		return fmt.Errorf("list files for LFS check: %w", err)
	}
	var attributes bytes.Buffer
	if err := gitRun(
		ctx,
		root,
		bytes.NewReader(files),
		&attributes,
		"check-attr", "--cached", "-z", "--stdin", "filter",
	); err != nil {
		return fmt.Errorf("inspect LFS attributes: %w", err)
	}

	fields := splitNUL(attributes.Bytes())
	for index := 2; index < len(fields); index += 3 {
		if fields[index] == "lfs" {
			return ErrLFSNotSupported
		}
	}
	return nil
}

func splitNUL(data []byte) []string {
	parts := splitNULBytes(data)
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		result = append(result, string(part))
	}
	return result
}

func splitNULBytes(data []byte) [][]byte {
	parts := bytes.Split(data, []byte{0})
	if len(parts) > 0 && len(parts[len(parts)-1]) == 0 {
		parts = parts[:len(parts)-1]
	}
	return parts
}

func commit(ctx context.Context, dir, message string) error {
	return gitRun(
		ctx,
		dir,
		nil,
		nil,
		"-c", "user.name=pisafe",
		"-c", "user.email=pisafe@localhost.invalid",
		"-c", "commit.gpgsign=false",
		"commit", "--quiet", "--no-verify", "-m", message,
	)
}

func gitOutputBytes(ctx context.Context, dir string, args ...string) ([]byte, error) {
	var stdout bytes.Buffer
	if err := gitRun(ctx, dir, nil, &stdout, args...); err != nil {
		return nil, err
	}
	return stdout.Bytes(), nil
}
