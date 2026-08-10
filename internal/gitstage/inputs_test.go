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
) ([]SelectedInput, ExcludedInputs, error) {
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

func TestStageIncludesSelectedInputsInBaselineCommit(t *testing.T) {
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
	if strings.Join(snapshot.Inputs, ",") !=
		"build/artifact.bin,build/nested.log,link.txt,notes.txt,tool.sh" {
		t.Fatalf("inputs = %#v", snapshot.Inputs)
	}
	if snapshot.BaselineCommit == "" {
		t.Fatal("selected inputs did not produce a baseline commit")
	}
	assertFile(t, filepath.Join(workspace, "notes.txt"), "untracked note\n")
	assertFile(t, filepath.Join(workspace, "build", "artifact.bin"), "output\n")

	// Ignored inputs must be committed despite the staged .gitignore, and the
	// workspace must be clean so the agent starts from a consistent state.
	if tracked := runGit(t, workspace, "ls-files", "build/artifact.bin"); tracked == "" {
		t.Fatal("ignored input was not committed")
	}
	if status := runGit(t, workspace, "status", "--short"); status != "" {
		t.Fatalf("workspace status = %q", status)
	}
	if mode := runGit(t, workspace, "ls-files", "-s", "tool.sh"); !strings.HasPrefix(mode, "100755") {
		t.Fatalf("executable input mode = %q", mode)
	}
	if mode := runGit(t, workspace, "ls-files", "-s", "link.txt"); !strings.HasPrefix(mode, "120000") {
		t.Fatalf("symlink input mode = %q", mode)
	}

	// The source keeps its own untracked state; staging copies, never moves.
	assertFile(t, filepath.Join(source, "notes.txt"), "untracked note\n")
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
	if inputPaths(included) != ".env" {
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
	if inputPaths(whole) != "build/artifact.bin,build/nested.log" {
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
	if inputPaths(part) != "build/nested.log" {
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
	if inputPaths(inputs) != "build/vendor/vendored.txt" {
		t.Fatalf("inputs = %#v", inputs)
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
	if err := os.WriteFile(large, make([]byte, maxInputFileBytes+1), 0o600); err != nil {
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
		if _, err := extractInputs(archive, workspace); err == nil {
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
	prepared.Snapshot.Inputs = append(prepared.Snapshot.Inputs, "never-archived.txt")

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
