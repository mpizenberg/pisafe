package gitstage

import (
	"archive/tar"
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// selectInputs is the two steps a run start performs: list what the run would
// not receive, then resolve the paths the user named against that listing.
func selectInputs(
	t *testing.T,
	source string,
	selection InputSelection,
) (InputPlan, ExcludedInputs, error) {
	t.Helper()
	excluded, err := ListExcludedInputs(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	return excluded.Select(selection)
}

func inputPaths(inputs []SelectedInput) string {
	paths := make([]string, 0, len(inputs))
	for _, input := range inputs {
		paths = append(paths, input.Path)
	}
	return strings.Join(paths, ",")
}

// newInputRepository adds the untracked and ignored files the selection tests
// choose between.
func newInputRepository(t *testing.T) string {
	t.Helper()
	source := newRepository(t)
	mustWrite(t, filepath.Join(source, ".gitignore"), "build/\n.env\n")
	runGit(t, source, "add", ".gitignore")
	runGit(t, source, "commit", "-qm", "ignore rules")
	mustWrite(t, filepath.Join(source, "notes.txt"), "untracked note\n")
	mustWrite(t, filepath.Join(source, ".env"), "TOKEN=secret\n")
	if err := os.Mkdir(filepath.Join(source, "build"), 0o700); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(source, "build", "artifact.bin"), "output\n")
	mustWrite(t, filepath.Join(source, "build", "nested.log"), "log\n")
	return source
}

func TestStageCopiesInputsBesideHistoryRatherThanIntoIt(t *testing.T) {
	ctx := context.Background()
	source := newInputRepository(t)
	if err := os.Symlink("notes.txt", filepath.Join(source, "link.txt")); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(source, "tool.sh"), "#!/bin/sh\n")
	if err := os.Chmod(filepath.Join(source, "tool.sh"), 0o700); err != nil {
		t.Fatal(err)
	}

	inputs, _, err := selectInputs(t, source, InputSelection{
		Include: []string{
			filepath.Join(source, "notes.txt"),
			filepath.Join(source, "build"),
			filepath.Join(source, "link.txt"),
			filepath.Join(source, "tool.sh"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(t.TempDir(), "workspace")
	snapshot, err := Stage(ctx, PrepareRequest{
		SourcePath: source,
		RunID:      "inputs-run",
		Inputs:     inputs,
	}, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if inputPaths(snapshot.Inputs) !=
		"build/artifact.bin,build/nested.log,link.txt,notes.txt,tool.sh" {
		t.Fatalf("inputs = %#v", snapshot.Inputs)
	}
	// Nothing an input carries reaches the index, so a repository whose tracked
	// state is clean needs no baseline commit at all.
	if snapshot.BaselineCommit != "" {
		t.Fatalf("inputs produced a baseline commit %q", snapshot.BaselineCommit)
	}
	assertFile(t, filepath.Join(workspace, "notes.txt"), "untracked note\n")
	assertFile(t, filepath.Join(workspace, "build", "artifact.bin"), "output\n")

	if tracked := runGit(t, workspace, "ls-files", "build/", "notes.txt", "tool.sh"); tracked != "" {
		t.Fatalf("inputs entered the run's history: %q", tracked)
	}
	// The run sees them as the host does: ignored where the staged .gitignore
	// ignores them, untracked otherwise.
	if status := runGit(t, workspace, "status", "--short"); status !=
		"?? link.txt\n?? notes.txt\n?? tool.sh" {
		t.Fatalf("workspace status = %q", status)
	}

	// Mode and link target survive on disk, where they now live.
	info, err := os.Lstat(filepath.Join(workspace, "tool.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("executable input lost its mode: %v", info.Mode())
	}
	if link, err := os.Readlink(filepath.Join(workspace, "link.txt")); err != nil ||
		link != "notes.txt" {
		t.Fatalf("symlink input = %q (%v)", link, err)
	}

	// The source keeps its own untracked state; staging copies, never moves.
	assertFile(t, filepath.Join(source, "notes.txt"), "untracked note\n")
}

// TestApplyDropsBaselineWithSelectedInputs covers what kept --include and
// --baseline drop apart while inputs were committed: the baseline held the
// included files, so replaying without it conflicted on paths the run never
// touched as history. Carrying them as files removes the conflict entirely.
func TestApplyDropsBaselineWithSelectedInputs(t *testing.T) {
	ctx := context.Background()
	source := newInputRepository(t)
	mustWrite(t, filepath.Join(source, "tracked.txt"), "uncommitted host work\n")

	inputs, _, err := selectInputs(t, source, InputSelection{
		Include: []string{filepath.Join(source, "build")},
	})
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(t.TempDir(), "workspace")
	snapshot, err := Stage(ctx, PrepareRequest{
		SourcePath: source,
		RunID:      "drop-inputs-run",
		Inputs:     inputs,
	}, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.BaselineCommit == "" {
		t.Fatal("dirty tracked file did not produce a baseline commit")
	}

	mustWrite(t, filepath.Join(workspace, "build", "artifact.bin"), "rebuilt in the run\n")
	mustWrite(t, filepath.Join(workspace, "feature.txt"), "agent work\n")
	runGit(t, workspace, "add", "feature.txt")
	runGit(t, workspace, "commit", "-qm", "agent commit")

	result, err := Apply(ctx, snapshot, workspace, DropBaseline)
	if err != nil {
		t.Fatalf("apply with dropped baseline: %v", err)
	}
	branch := "refs/heads/" + result.Branch
	if listed := runGit(t, source, "ls-tree", "-r", "--name-only", branch); listed !=
		".gitignore\nfeature.txt\ntracked.txt" {
		t.Fatalf("imported tree = %q", listed)
	}
	// The baseline is gone, so the host's uncommitted work is not in the branch.
	if content := runGit(t, source, "show", branch+":tracked.txt"); content != "initial" {
		t.Fatalf("tracked.txt on branch = %q", content)
	}
}

func TestSelectRequiresUnsafeFlagForCredentialNames(t *testing.T) {
	source := newInputRepository(t)

	_, _, err := selectInputs(t, source, InputSelection{
		Include: []string{filepath.Join(source, ".env")},
	})
	if err == nil || !strings.Contains(err.Error(), "--include-unsafe") {
		t.Fatalf("err = %v", err)
	}
	// A directory holding a credential-shaped file is refused as a whole, so a
	// broad selection cannot smuggle one in — including when Git reported that
	// directory as one name rather than as the files under it.
	mustWrite(t, filepath.Join(source, "build", "id_rsa"), "key\n")
	if _, _, err := selectInputs(t, source, InputSelection{
		Include: []string{filepath.Join(source, "build")},
	}); err == nil {
		t.Fatal("directory containing a credential was accepted")
	}

	included, _, err := selectInputs(t, source, InputSelection{
		Unsafe: []string{filepath.Join(source, ".env")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if inputPaths(included.Files) != ".env" {
		t.Fatalf("included = %#v", included)
	}
}

// TestSelectExpandsAndSubtractsCollapsedDirectories covers what Git's own
// listing leaves implicit: a directory nobody tracks arrives as a single name,
// so selecting it must reach the files under it, and the report must lose the
// directory only when the whole of it was taken.
func TestSelectExpandsAndSubtractsCollapsedDirectories(t *testing.T) {
	source := newInputRepository(t)

	excluded, err := ListExcludedInputs(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(excluded.Ignored, ",") != ".env,build/" {
		t.Fatalf("ignored = %#v", excluded.Ignored)
	}

	whole, remaining, err := excluded.Select(InputSelection{
		Include: []string{filepath.Join(source, "build")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if inputPaths(whole.Files) != "build/artifact.bin,build/nested.log" {
		t.Fatalf("whole = %#v", whole)
	}
	if strings.Join(remaining.Ignored, ",") != ".env" {
		t.Fatalf("remaining = %#v", remaining.Ignored)
	}

	part, remaining, err := excluded.Select(InputSelection{
		Include: []string{filepath.Join(source, "build", "nested.log")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if inputPaths(part.Files) != "build/nested.log" {
		t.Fatalf("part = %#v", part)
	}
	if strings.Join(remaining.Ignored, ",") != ".env,build/" {
		t.Fatalf("remaining = %#v", remaining.Ignored)
	}
}

// TestSelectRefusesARepositoryInsideASelectedDirectory keeps a nested checkout
// from being copied in as loose files: a repository crosses into a run as
// history or not at all.
func TestSelectRefusesARepositoryInsideASelectedDirectory(t *testing.T) {
	source := newInputRepository(t)
	nested := filepath.Join(source, "build", "vendor")
	runGit(t, source, "init", "-q", filepath.Join("build", "vendor"))
	mustWrite(t, filepath.Join(nested, "vendored.txt"), "content\n")

	_, _, err := selectInputs(t, source, InputSelection{
		Include: []string{filepath.Join(source, "build")},
	})
	if err == nil || !strings.Contains(err.Error(), "build/vendor") {
		t.Fatalf("err = %v", err)
	}
	inputs, _, err := selectInputs(t, source, InputSelection{
		Include: []string{filepath.Join(nested, "vendored.txt")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if inputPaths(inputs.Files) != "build/vendor/vendored.txt" {
		t.Fatalf("inputs = %#v", inputs)
	}
}

// TestSelectRecordsRootsForEmptyDirectories covers the difference between a
// path Git has never heard of and one it reports as excluded that holds nothing
// yet. The second is what a run collects work under, so it is a root even
// though it carries no files in.
func TestSelectRecordsRootsForEmptyDirectories(t *testing.T) {
	source := newInputRepository(t)
	mustWrite(t, filepath.Join(source, ".gitignore"), "build/\n.env\nplans/\n")
	runGit(t, source, "commit", "-qm", "ignore plans", "--", ".gitignore")
	if err := os.Mkdir(filepath.Join(source, "plans"), 0o700); err != nil {
		t.Fatal(err)
	}

	plan, remaining, err := selectInputs(t, source, InputSelection{
		Include: []string{
			filepath.Join(source, "plans"),
			filepath.Join(source, "notes.txt"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if inputPaths(plan.Files) != "notes.txt" {
		t.Fatalf("files = %#v", plan.Files)
	}
	if strings.Join(plan.Roots, ",") != "notes.txt,plans" {
		t.Fatalf("roots = %#v", plan.Roots)
	}
	// An empty root leaves nothing behind, so it drops out of the excluded
	// report exactly as a directory whose files were all taken does.
	if strings.Join(remaining.Ignored, ",") != ".env,build/" {
		t.Fatalf("remaining = %#v", remaining.Ignored)
	}
}

// TestSelectDistinguishesUnknownFromAbsent keeps the two refusals apart: a path
// no listing covers is not selectable at all, while a path an excluded
// directory covers but that is not there is simply missing.
func TestSelectDistinguishesUnknownFromAbsent(t *testing.T) {
	source := newInputRepository(t)

	_, _, err := selectInputs(t, source, InputSelection{
		Include: []string{filepath.Join(source, "tracked.txt")},
	})
	if err == nil || !strings.Contains(err.Error(), "not an untracked or ignored path") {
		t.Fatalf("err = %v", err)
	}
	_, _, err = selectInputs(t, source, InputSelection{
		Include: []string{filepath.Join(source, "build", "absent.log")},
	})
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("err = %v", err)
	}
}

func TestSelectRejectsUnusableSelections(t *testing.T) {
	source := newInputRepository(t)
	outside := filepath.Join(t.TempDir(), "outside.txt")
	mustWrite(t, outside, "host content\n")
	if err := os.Symlink(outside, filepath.Join(source, "escape.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../etc", filepath.Join(source, "climb.txt")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(source, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}

	for name, request := range map[string]string{
		"tracked file":      filepath.Join(source, "tracked.txt"),
		"missing file":      filepath.Join(source, "absent.txt"),
		"outside repo":      outside,
		"absolute link":     filepath.Join(source, "escape.txt"),
		"climbing link":     filepath.Join(source, "climb.txt"),
		"special file":      filepath.Join(source, "pipe"),
		"repository itself": source,
	} {
		if _, _, err := selectInputs(t, source, InputSelection{
			Include: []string{request},
		}); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestSelectEnforcesSizeLimits(t *testing.T) {
	source := newInputRepository(t)
	large := filepath.Join(source, "large.bin")
	if err := os.WriteFile(large, make([]byte, maxArchiveFileBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := selectInputs(t, source, InputSelection{
		Include: []string{large},
	}); err == nil || !strings.Contains(err.Error(), "per-file limit") {
		t.Fatalf("err = %v", err)
	}
}

func TestExtractInputsRejectsUnsafeArchiveEntries(t *testing.T) {
	for name, header := range map[string]*tar.Header{
		"parent escape": {Name: "../escape.txt", Typeflag: tar.TypeReg, Mode: 0o600},
		"absolute path": {Name: "/etc/passwd", Typeflag: tar.TypeReg, Mode: 0o600},
		"unclean path":  {Name: "./notes.txt", Typeflag: tar.TypeReg, Mode: 0o600},
		"git directory": {Name: ".git/config", Typeflag: tar.TypeReg, Mode: 0o600},
		"escaping link": {
			Name:     "link.txt",
			Typeflag: tar.TypeSymlink,
			Linkname: "../../secret",
			Mode:     0o777,
		},
		"absolute link": {
			Name:     "link.txt",
			Typeflag: tar.TypeSymlink,
			Linkname: "/etc/passwd",
			Mode:     0o777,
		},
		"device entry": {Name: "device", Typeflag: tar.TypeChar, Mode: 0o600},
	} {
		archive := filepath.Join(t.TempDir(), "inputs.tar")
		file, err := os.Create(archive)
		if err != nil {
			t.Fatal(err)
		}
		writer := tar.NewWriter(file)
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		workspace := t.TempDir()
		if _, err := extractFileArchive(archive, workspace); err == nil {
			t.Errorf("%s was extracted", name)
		}
	}
}

func TestMaterializeRejectsInputArchiveDisagreeingWithSnapshot(t *testing.T) {
	ctx := context.Background()
	source := newInputRepository(t)
	inputs, _, err := selectInputs(t, source, InputSelection{
		Include: []string{filepath.Join(source, "notes.txt")},
	})
	if err != nil {
		t.Fatal(err)
	}
	packageDir := filepath.Join(t.TempDir(), "package")
	prepared, err := Prepare(ctx, PrepareRequest{
		SourcePath: source,
		PackageDir: packageDir,
		RunID:      "mismatch-run",
		Inputs:     inputs,
	})
	if err != nil {
		t.Fatal(err)
	}
	prepared.Snapshot.Inputs = append(
		prepared.Snapshot.Inputs,
		SelectedInput{Path: "never-archived.txt"},
	)

	_, err = Materialize(ctx, prepared, filepath.Join(t.TempDir(), "workspace"))
	if err == nil || !strings.Contains(err.Error(), "does not match the staged snapshot") {
		t.Fatalf("err = %v", err)
	}
}

func TestLooksLikeSecretMatchesWholeWordsOnly(t *testing.T) {
	for _, name := range []string{
		".env", ".env.local", "config/.npmrc", "deploy/id_rsa", "certs/server.pem",
		"my-secret.txt", "api_token.json", "PASSWORD.txt", "passwords.csv",
		"credentials", "credentials.json",
	} {
		if !LooksLikeSecret(name) {
			t.Errorf("%q was not flagged", name)
		}
	}
	for _, name := range []string{
		"tokenizer.json", "environment.md", "notes.txt",
		"build/artifact.bin", "secretary.md", "keyboard.ts",
	} {
		if LooksLikeSecret(name) {
			t.Errorf("%q was flagged", name)
		}
	}
}
