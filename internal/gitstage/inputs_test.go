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

	workspace := filepath.Join(t.TempDir(), "workspace")
	snapshot, err := Stage(ctx, PrepareRequest{
		SourcePath: source,
		RunID:      "inputs-run",
		Inputs: InputSelection{
			Include: []string{
				filepath.Join(source, "notes.txt"),
				filepath.Join(source, "build"),
				filepath.Join(source, "link.txt"),
				filepath.Join(source, "tool.sh"),
			},
		},
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

func TestSelectInputsRequiresUnsafeFlagForCredentialNames(t *testing.T) {
	ctx := context.Background()
	source := newInputRepository(t)

	_, err := SelectInputs(ctx, source, InputSelection{
		Include: []string{filepath.Join(source, ".env")},
	})
	if err == nil || !strings.Contains(err.Error(), "--include-unsafe") {
		t.Fatalf("err = %v", err)
	}
	// A directory holding a credential-shaped file is refused as a whole, so a
	// broad selection cannot smuggle one in.
	mustWrite(t, filepath.Join(source, "build", "id_rsa"), "key\n")
	if _, err := SelectInputs(ctx, source, InputSelection{
		Include: []string{filepath.Join(source, "build")},
	}); err == nil {
		t.Fatal("directory containing a credential was accepted")
	}

	included, err := SelectInputs(ctx, source, InputSelection{
		Unsafe: []string{filepath.Join(source, ".env")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(included, ",") != ".env" {
		t.Fatalf("included = %#v", included)
	}
}

func TestSelectInputsRejectsUnusableSelections(t *testing.T) {
	ctx := context.Background()
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
		if _, err := SelectInputs(ctx, source, InputSelection{
			Include: []string{request},
		}); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestSelectInputsEnforcesSizeLimits(t *testing.T) {
	ctx := context.Background()
	source := newInputRepository(t)
	large := filepath.Join(source, "large.bin")
	if err := os.WriteFile(large, make([]byte, maxInputFileBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := SelectInputs(ctx, source, InputSelection{
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
	packageDir := filepath.Join(t.TempDir(), "package")
	prepared, err := Prepare(ctx, PrepareRequest{
		SourcePath: source,
		PackageDir: packageDir,
		RunID:      "mismatch-run",
		Inputs:     InputSelection{Include: []string{filepath.Join(source, "notes.txt")}},
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
