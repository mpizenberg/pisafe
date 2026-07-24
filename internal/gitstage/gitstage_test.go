package gitstage

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestStageCleanRepositoryIsIndependent(t *testing.T) {
	ctx := context.Background()
	source := newRepository(t)
	workspace := filepath.Join(t.TempDir(), "workspace")

	snapshot, err := Stage(ctx, source, workspace, "clean-run")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.BaselineCommit != "" {
		t.Fatalf("clean repository created baseline %s", snapshot.BaselineCommit)
	}

	if err := os.RemoveAll(workspace); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(source, "tracked.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "initial\n" {
		t.Fatalf("source changed after workspace deletion: %q", content)
	}
}

func TestStageCapturesFinalTrackedBaseline(t *testing.T) {
	ctx := context.Background()
	source := newRepository(t)
	mustWrite(t, filepath.Join(source, "tracked.txt"), "working tree\n")
	mustWrite(t, filepath.Join(source, "staged.txt"), "staged\n")
	runGit(t, source, "add", "staged.txt")
	mustWrite(t, filepath.Join(source, "untracked.txt"), "excluded\n")

	workspace := filepath.Join(t.TempDir(), "workspace")
	snapshot, err := Stage(ctx, source, workspace, "dirty-run")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.BaselineCommit == "" {
		t.Fatal("dirty tracked repository did not create a baseline")
	}

	assertFile(t, filepath.Join(workspace, "tracked.txt"), "working tree\n")
	assertFile(t, filepath.Join(workspace, "staged.txt"), "staged\n")
	if _, err := os.Stat(filepath.Join(workspace, "untracked.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("untracked file was copied: %v", err)
	}
	message := runGit(t, workspace, "log", "-1", "--format=%s")
	if message != baselineMessage {
		t.Fatalf("baseline message = %q", message)
	}
}

func TestMaterializeDoesNotAccessSourceCheckout(t *testing.T) {
	ctx := context.Background()
	source := newRepository(t)
	mustWrite(t, filepath.Join(source, "tracked.txt"), "prepared state\n")

	packageDir := filepath.Join(t.TempDir(), "package")
	prepared, err := Prepare(ctx, source, packageDir, "boundary-run")
	if err != nil {
		t.Fatal(err)
	}
	originalRoot := prepared.Snapshot.SourceRoot
	prepared.Snapshot.SourceRoot = filepath.Join(t.TempDir(), "unreachable-source")

	workspace := filepath.Join(t.TempDir(), "workspace")
	snapshot, err := Materialize(ctx, prepared, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.SourceRoot == originalRoot {
		t.Fatal("test did not replace source root")
	}
	assertFile(t, filepath.Join(workspace, "tracked.txt"), "prepared state\n")
}

func TestApplyCreatesBranchWithoutChangingCheckout(t *testing.T) {
	ctx := context.Background()
	source := newRepository(t)
	mustWrite(t, filepath.Join(source, "local-uncommitted.txt"), "stay local\n")
	indexBefore := runGit(t, source, "write-tree")
	statusBefore := runGit(t, source, "status", "--porcelain=v1", "--untracked-files=all")

	workspace := filepath.Join(t.TempDir(), "workspace")
	snapshot, err := Stage(ctx, source, workspace, "apply-run")
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(workspace, "tracked.txt"), "agent result\n")
	mustWrite(t, filepath.Join(workspace, "not-imported.txt"), "needs confirmation\n")

	result, err := Apply(ctx, snapshot, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if result.Branch != "pisafe/apply-run" {
		t.Fatalf("branch = %q", result.Branch)
	}
	if result.FinalCommit == "" {
		t.Fatal("tracked workspace changes were not finalized")
	}
	if len(result.Untracked) != 1 || result.Untracked[0] != "not-imported.txt" {
		t.Fatalf("untracked = %#v", result.Untracked)
	}
	if result.BundleSHA256 == "" {
		t.Fatal("apply bundle hash is empty")
	}

	branchContent := runGit(t, source, "show", "pisafe/apply-run:tracked.txt")
	if branchContent != "agent result" {
		t.Fatalf("branch content = %q", branchContent)
	}
	assertFile(t, filepath.Join(source, "tracked.txt"), "initial\n")
	indexAfter := runGit(t, source, "write-tree")
	statusAfter := runGit(t, source, "status", "--porcelain=v1", "--untracked-files=all")
	if indexAfter != indexBefore {
		t.Fatalf("index changed: %s -> %s", indexBefore, indexAfter)
	}
	if statusAfter != statusBefore {
		t.Fatalf("working status changed:\nbefore %q\nafter  %q", statusBefore, statusAfter)
	}

	_, err = Apply(ctx, snapshot, workspace)
	if !errors.Is(err, ErrBranchExists) {
		t.Fatalf("second apply error = %v, want ErrBranchExists", err)
	}
}

func TestImportRejectsTamperedApplyBundle(t *testing.T) {
	ctx := context.Background()
	source := newRepository(t)
	workspace := filepath.Join(t.TempDir(), "workspace")
	snapshot, err := Stage(ctx, source, workspace, "tampered-run")
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(workspace, "tracked.txt"), "agent result\n")

	bundlePath := filepath.Join(t.TempDir(), "apply.bundle")
	prepared, err := PrepareApply(ctx, snapshot, workspace, bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(bundlePath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("tampered"); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = ImportApply(ctx, snapshot, prepared)
	if err == nil || !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("ImportApply error = %v, want hash mismatch", err)
	}
	if output, branchErr := gitOutput(ctx, source, "rev-parse", "--verify", "refs/heads/pisafe/tampered-run"); branchErr == nil {
		t.Fatalf("branch unexpectedly exists at %s", output)
	}
}

func TestStageRejectsSubmodulesUntilJournaledApplyExists(t *testing.T) {
	ctx := context.Background()
	source := newRepository(t)
	submodule := newRepository(t)
	runGit(
		t,
		source,
		"-c", "protocol.file.allow=always",
		"submodule", "add", "--quiet", submodule, "dependency",
	)
	runGit(t, source, "commit", "-qm", "add submodule")

	_, err := Stage(ctx, source, filepath.Join(t.TempDir(), "workspace"), "submodule-run")
	if !errors.Is(err, ErrSubmodulesNotReady) {
		t.Fatalf("Stage error = %v, want ErrSubmodulesNotReady", err)
	}
}

func TestStageRejectsGitLFS(t *testing.T) {
	ctx := context.Background()
	source := newRepository(t)
	mustWrite(t, filepath.Join(source, ".gitattributes"), "*.bin filter=lfs\n")
	mustWrite(t, filepath.Join(source, "asset.bin"), "lfs-like content\n")
	runGit(t, source, "add", ".gitattributes", "asset.bin")
	runGit(t, source, "commit", "-qm", "add LFS path")

	_, err := Stage(ctx, source, filepath.Join(t.TempDir(), "workspace"), "lfs-run")
	if !errors.Is(err, ErrLFSNotSupported) {
		t.Fatalf("Stage error = %v, want ErrLFSNotSupported", err)
	}
}

func TestFinalizeReportsUnusualUntrackedNames(t *testing.T) {
	ctx := context.Background()
	source := newRepository(t)
	workspace := filepath.Join(t.TempDir(), "workspace")
	if _, err := Stage(ctx, source, workspace, "names-run"); err != nil {
		t.Fatal(err)
	}
	name := "line\nbreak.txt"
	mustWrite(t, filepath.Join(workspace, name), "untracked\n")

	_, untracked, err := FinalizeTracked(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(untracked) != 1 || untracked[0] != name {
		t.Fatalf("untracked = %#v", untracked)
	}
}

func TestListExcludedInputsSeparatesUntrackedAndIgnored(t *testing.T) {
	ctx := context.Background()
	source := newRepository(t)
	mustWrite(t, filepath.Join(source, ".gitignore"), "ignored/\n")
	runGit(t, source, "add", ".gitignore")
	runGit(t, source, "commit", "-qm", "ignore generated files")
	mustWrite(t, filepath.Join(source, "untracked.txt"), "excluded\n")
	if err := os.Mkdir(filepath.Join(source, "ignored"), 0o700); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(source, "ignored", "output.txt"), "ignored\n")

	excluded, err := ListExcludedInputs(ctx, filepath.Join(source, "ignored"))
	if err != nil {
		t.Fatal(err)
	}
	if len(excluded.Untracked) != 1 || excluded.Untracked[0] != "untracked.txt" {
		t.Fatalf("untracked = %#v", excluded.Untracked)
	}
	if len(excluded.Ignored) != 1 || excluded.Ignored[0] != "ignored/output.txt" {
		t.Fatalf("ignored = %#v", excluded.Ignored)
	}
}

func TestStageRejectsUnsafeRunID(t *testing.T) {
	_, err := Stage(context.Background(), t.TempDir(), filepath.Join(t.TempDir(), "workspace"), "../escape")
	if err == nil || !strings.Contains(err.Error(), "invalid run ID") {
		t.Fatalf("Stage error = %v", err)
	}
}

func newRepository(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-q", "--initial-branch=main")
	runGit(t, dir, "config", "user.name", "Test User")
	runGit(t, dir, "config", "user.email", "test@example.invalid")
	runGit(t, dir, "config", "commit.gpgsign", "false")
	mustWrite(t, filepath.Join(dir, "tracked.txt"), "initial\n")
	runGit(t, dir, "add", "tracked.txt")
	runGit(t, dir, "commit", "-qm", "initial")
	return dir
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	commandArgs := append([]string{"-C", dir}, args...)
	cmd := exec.Command("git", commandArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(commandArgs, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertFile(t *testing.T, path, want string) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != want {
		t.Fatalf("%s = %q, want %q", path, content, want)
	}
}
