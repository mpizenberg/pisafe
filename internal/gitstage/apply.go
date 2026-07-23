package gitstage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type PreparedApply struct {
	RunID        string
	Tip          string
	FinalCommit  string
	Untracked    []string
	BundlePath   string
	BundleSHA256 string
}

// PrepareApply runs inside the isolated environment and creates the
// incremental bundle that will be streamed back to the controller.
func PrepareApply(
	ctx context.Context,
	snapshot Snapshot,
	workspace string,
	bundlePath string,
) (PreparedApply, error) {
	if !runIDPattern.MatchString(snapshot.RunID) {
		return PreparedApply{}, fmt.Errorf("invalid run ID %q", snapshot.RunID)
	}
	if snapshot.WorkRef != "refs/heads/work/"+snapshot.RunID {
		return PreparedApply{}, fmt.Errorf("work ref does not match run ID")
	}

	finalCommit, untracked, err := FinalizeTracked(ctx, workspace)
	if err != nil {
		return PreparedApply{}, err
	}
	tip, err := gitOutput(ctx, workspace, "rev-parse", "--verify", snapshot.WorkRef+"^{commit}")
	if err != nil {
		return PreparedApply{}, fmt.Errorf("resolve run tip: %w", err)
	}
	ancestorErr := gitRun(ctx, workspace, nil, nil, "merge-base", "--is-ancestor", snapshot.SourceHead, tip)
	if ancestorErr != nil {
		return PreparedApply{}, fmt.Errorf("run history is not based on captured source HEAD")
	}

	if tip == snapshot.SourceHead {
		return PreparedApply{
			RunID:       snapshot.RunID,
			Tip:         tip,
			FinalCommit: finalCommit,
			Untracked:   untracked,
		}, nil
	}

	if bundlePath == "" {
		return PreparedApply{}, fmt.Errorf("apply bundle path is required for changed history")
	}
	if _, err := os.Stat(bundlePath); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return PreparedApply{}, fmt.Errorf("apply bundle already exists: %s", bundlePath)
		}
		return PreparedApply{}, fmt.Errorf("inspect apply bundle path: %w", err)
	}
	if err := gitRun(
		ctx,
		workspace,
		nil,
		nil,
		"bundle", "create", bundlePath, snapshot.WorkRef, "^"+snapshot.SourceHead,
	); err != nil {
		return PreparedApply{}, fmt.Errorf("create incremental apply bundle: %w", err)
	}
	hash, err := fileSHA256(bundlePath)
	if err != nil {
		return PreparedApply{}, fmt.Errorf("hash apply bundle: %w", err)
	}

	return PreparedApply{
		RunID:        snapshot.RunID,
		Tip:          tip,
		FinalCommit:  finalCommit,
		Untracked:    untracked,
		BundlePath:   bundlePath,
		BundleSHA256: hash,
	}, nil
}

// ImportApply runs on the Mac. It verifies the transferred bundle before
// creating a new branch with a compare-and-swap ref update.
func ImportApply(ctx context.Context, snapshot Snapshot, prepared PreparedApply) (ApplyResult, error) {
	if !runIDPattern.MatchString(snapshot.RunID) || prepared.RunID != snapshot.RunID {
		return ApplyResult{}, fmt.Errorf("apply package does not match run")
	}
	sourceRoot, err := filepath.EvalSymlinks(snapshot.SourceRoot)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("resolve source repository: %w", err)
	}
	currentHead, err := gitOutput(ctx, sourceRoot, "rev-parse", "--verify", snapshot.SourceHead+"^{commit}")
	if err != nil || currentHead != snapshot.SourceHead {
		return ApplyResult{}, fmt.Errorf("captured source commit is unavailable")
	}

	targetRef := "refs/heads/pisafe/" + snapshot.RunID
	existing, err := gitOutput(ctx, sourceRoot, "rev-parse", "--verify", "--quiet", targetRef)
	if err == nil || existing != "" {
		return ApplyResult{}, ErrBranchExists
	}
	if !isExitCode(err, 1) {
		return ApplyResult{}, fmt.Errorf("inspect target branch: %w", err)
	}

	if prepared.Tip == snapshot.SourceHead {
		if prepared.BundlePath != "" {
			return ApplyResult{}, fmt.Errorf("unchanged apply unexpectedly contains a bundle")
		}
		if err := createRef(ctx, sourceRoot, targetRef, prepared.Tip, snapshot.SourceHead); err != nil {
			return ApplyResult{}, err
		}
		return resultFromPrepared(targetRef, prepared), nil
	}
	if prepared.BundlePath == "" {
		return ApplyResult{}, fmt.Errorf("changed apply has no bundle")
	}
	hash, err := fileSHA256(prepared.BundlePath)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("hash transferred apply bundle: %w", err)
	}
	if hash != prepared.BundleSHA256 {
		return ApplyResult{}, fmt.Errorf("apply bundle hash mismatch")
	}
	if err := gitRun(ctx, sourceRoot, nil, nil, "bundle", "verify", prepared.BundlePath); err != nil {
		return ApplyResult{}, fmt.Errorf("verify apply bundle: %w", err)
	}

	incomingRef := "refs/pisafe/incoming/" + snapshot.RunID
	if err := gitRun(
		ctx,
		sourceRoot,
		nil,
		nil,
		"fetch", "--quiet", "--no-write-fetch-head", prepared.BundlePath, snapshot.WorkRef+":"+incomingRef,
	); err != nil {
		return ApplyResult{}, fmt.Errorf("import apply bundle: %w", err)
	}
	defer func() {
		_ = gitRun(ctx, sourceRoot, nil, nil, "update-ref", "-d", incomingRef)
	}()

	importedTip, err := gitOutput(ctx, sourceRoot, "rev-parse", "--verify", incomingRef+"^{commit}")
	if err != nil {
		return ApplyResult{}, fmt.Errorf("resolve imported tip: %w", err)
	}
	if importedTip != prepared.Tip {
		return ApplyResult{}, fmt.Errorf(
			"imported tip mismatch: wanted %s, got %s",
			prepared.Tip,
			importedTip,
		)
	}
	if err := createRef(ctx, sourceRoot, targetRef, prepared.Tip, snapshot.SourceHead); err != nil {
		return ApplyResult{}, err
	}
	if err := gitRun(ctx, sourceRoot, nil, nil, "update-ref", "-d", incomingRef, prepared.Tip); err != nil {
		return ApplyResult{}, fmt.Errorf("remove temporary import ref: %w", err)
	}

	return resultFromPrepared(targetRef, prepared), nil
}

// Apply composes the isolated and host halves locally for tests. The
// controller uses PrepareApply and ImportApply separately over SSH.
func Apply(ctx context.Context, snapshot Snapshot, workspace string) (ApplyResult, error) {
	bundle, err := os.CreateTemp("", "pisafe-apply-*.bundle")
	if err != nil {
		return ApplyResult{}, fmt.Errorf("reserve apply bundle path: %w", err)
	}
	bundlePath := bundle.Name()
	if err := bundle.Close(); err != nil {
		return ApplyResult{}, fmt.Errorf("close apply bundle placeholder: %w", err)
	}
	if err := os.Remove(bundlePath); err != nil {
		return ApplyResult{}, fmt.Errorf("prepare apply bundle path: %w", err)
	}
	defer os.Remove(bundlePath)

	prepared, err := PrepareApply(ctx, snapshot, workspace, bundlePath)
	if err != nil {
		return ApplyResult{}, err
	}
	return ImportApply(ctx, snapshot, prepared)
}

func createRef(ctx context.Context, root, targetRef, tip, sourceHead string) error {
	zeroOID := strings.Repeat("0", len(sourceHead))
	if err := gitRun(ctx, root, nil, nil, "update-ref", targetRef, tip, zeroOID); err != nil {
		if _, checkErr := gitOutput(ctx, root, "rev-parse", "--verify", targetRef); checkErr == nil {
			return ErrBranchExists
		}
		return fmt.Errorf("create import branch: %w", err)
	}
	return nil
}

func resultFromPrepared(targetRef string, prepared PreparedApply) ApplyResult {
	return ApplyResult{
		Branch:       strings.TrimPrefix(targetRef, "refs/heads/"),
		Tip:          prepared.Tip,
		FinalCommit:  prepared.FinalCommit,
		Untracked:    prepared.Untracked,
		BundleSHA256: prepared.BundleSHA256,
	}
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func IsBranchExists(err error) bool {
	return errors.Is(err, ErrBranchExists)
}
