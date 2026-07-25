package gitstage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// addSubmodule attaches an existing repository to source at path. Local
// submodule URLs need the protocol override modern Git requires.
func addSubmodule(t *testing.T, source, submodule, path string) {
	t.Helper()
	runGit(
		t,
		source,
		"-c", "protocol.file.allow=always",
		"submodule", "add", "--quiet", submodule, path,
	)
	runGit(t, source, "commit", "-qm", "add submodule "+path)
}

func TestStageReconstructsInitializedSubmodules(t *testing.T) {
	ctx := context.Background()
	source := newRepository(t)
	dependency := newRepository(t)
	runGit(t, dependency, "commit", "-q", "--allow-empty", "-m", "second")
	addSubmodule(t, source, dependency, "dependency")

	// Uncommitted work in both repositories must survive staging.
	mustWrite(t, filepath.Join(source, "tracked.txt"), "superproject edit\n")
	mustWrite(t, filepath.Join(source, "dependency", "tracked.txt"), "submodule edit\n")
	submoduleHead := runGit(t, source, "-C", "dependency", "rev-parse", "HEAD")

	workspace := filepath.Join(t.TempDir(), "workspace")
	snapshot, err := Stage(
		ctx,
		PrepareRequest{SourcePath: source, RunID: "submodule-run"},
		workspace,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Submodules) != 1 {
		t.Fatalf("submodules = %#v", snapshot.Submodules)
	}
	staged := snapshot.Submodules[0]
	if staged.Path != "dependency" || staged.Head != submoduleHead {
		t.Fatalf("staged submodule = %#v", staged)
	}
	if staged.BaselineCommit == "" {
		t.Fatal("dirty submodule state was not committed")
	}

	target := filepath.Join(workspace, "dependency")
	assertFile(t, filepath.Join(target, "tracked.txt"), "submodule edit\n")
	assertFile(t, filepath.Join(workspace, "tracked.txt"), "superproject edit\n")
	if head := runGit(t, target, "rev-parse", "HEAD"); head != staged.BaselineCommit {
		t.Fatalf("submodule HEAD = %s, want the baseline commit", head)
	}
	// The superproject records where the submodule actually ended up, and both
	// repositories are clean so the agent starts from consistent state.
	gitlink := runGit(t, workspace, "rev-parse", "HEAD:dependency")
	if gitlink != staged.BaselineCommit {
		t.Fatalf("gitlink = %s, want %s", gitlink, staged.BaselineCommit)
	}
	if status := runGit(t, workspace, "status", "--short"); status != "" {
		t.Fatalf("workspace status = %q", status)
	}
	if url := runGit(t, workspace, "config", "submodule.dependency.url"); url == "" {
		t.Fatal("submodule was not registered in the staged repository")
	}

	// Deleting the run must not disturb the source or its submodule.
	if err := os.RemoveAll(workspace); err != nil {
		t.Fatal(err)
	}
	assertFile(t, filepath.Join(source, "dependency", "tracked.txt"), "submodule edit\n")
	if head := runGit(t, source, "-C", "dependency", "rev-parse", "HEAD"); head != submoduleHead {
		t.Fatalf("source submodule HEAD changed to %s", head)
	}
}

func TestStageLeavesUninitializedSubmodulesAlone(t *testing.T) {
	ctx := context.Background()
	source := newRepository(t)
	dependency := newRepository(t)
	addSubmodule(t, source, dependency, "dependency")
	runGit(t, source, "submodule", "deinit", "-f", "--", "dependency")

	workspace := filepath.Join(t.TempDir(), "workspace")
	snapshot, err := Stage(
		ctx,
		PrepareRequest{SourcePath: source, RunID: "deinit-run"},
		workspace,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Submodules) != 0 {
		t.Fatalf("submodules = %#v", snapshot.Submodules)
	}
	entries, err := os.ReadDir(filepath.Join(workspace, "dependency"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("uninitialized submodule was populated: %v", entries)
	}
	if status := runGit(t, workspace, "status", "--short"); status != "" {
		t.Fatalf("workspace status = %q", status)
	}
}

func TestStageRejectsNestedSubmodules(t *testing.T) {
	ctx := context.Background()
	source := newRepository(t)
	middle := newRepository(t)
	inner := newRepository(t)
	addSubmodule(t, middle, inner, "inner")
	addSubmodule(t, source, middle, "middle")

	_, err := Stage(
		ctx,
		PrepareRequest{SourcePath: source, RunID: "nested-run"},
		filepath.Join(t.TempDir(), "workspace"),
	)
	if !errors.Is(err, ErrNestedSubmodules) {
		t.Fatalf("err = %v, want ErrNestedSubmodules", err)
	}
}

func TestStageRejectsGitLFSInsideASubmodule(t *testing.T) {
	ctx := context.Background()
	source := newRepository(t)
	dependency := newRepository(t)
	mustWrite(t, filepath.Join(dependency, ".gitattributes"), "*.bin filter=lfs\n")
	mustWrite(t, filepath.Join(dependency, "asset.bin"), "content\n")
	runGit(t, dependency, "add", ".gitattributes", "asset.bin")
	runGit(t, dependency, "commit", "-qm", "add LFS path")
	addSubmodule(t, source, dependency, "dependency")

	_, err := Stage(
		ctx,
		PrepareRequest{SourcePath: source, RunID: "lfs-submodule-run"},
		filepath.Join(t.TempDir(), "workspace"),
	)
	if !errors.Is(err, ErrLFSNotSupported) {
		t.Fatalf("err = %v, want ErrLFSNotSupported", err)
	}
}

func TestMaterializeRejectsMismatchedSubmoduleArtifacts(t *testing.T) {
	ctx := context.Background()
	source := newRepository(t)
	dependency := newRepository(t)
	addSubmodule(t, source, dependency, "dependency")

	packageDir := filepath.Join(t.TempDir(), "package")
	prepared, err := Prepare(ctx, PrepareRequest{
		SourcePath: source,
		PackageDir: packageDir,
		RunID:      "mismatch-run",
	})
	if err != nil {
		t.Fatal(err)
	}
	missing := prepared
	missing.Submodules = nil
	if _, err := Materialize(ctx, missing, filepath.Join(t.TempDir(), "workspace")); err == nil ||
		!strings.Contains(err.Error(), "disagree") {
		t.Fatalf("missing artifacts err = %v", err)
	}

	renamed := prepared
	renamed.Submodules = []PreparedSubmodule{{
		Path:       "elsewhere",
		BundlePath: prepared.Submodules[0].BundlePath,
		PatchPath:  prepared.Submodules[0].PatchPath,
	}}
	if _, err := Materialize(ctx, renamed, filepath.Join(t.TempDir(), "workspace")); err == nil ||
		!strings.Contains(err.Error(), "disagree") {
		t.Fatalf("renamed artifacts err = %v", err)
	}
}

func TestSafeSubmodulePathRejectsEscapes(t *testing.T) {
	for _, path := range []string{
		"", "/absolute", "../outside", "..", "a/../b", "./a", "a/.git/hooks", ".git",
	} {
		if err := safeSubmodulePath(path); err == nil {
			t.Errorf("%q was accepted", path)
		}
	}
	for _, path := range []string{"dependency", "vendor/library"} {
		if err := safeSubmodulePath(path); err != nil {
			t.Errorf("%q was rejected: %v", path, err)
		}
	}
}
