package runcopy

import (
	"archive/tar"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestArchiveAndCopyRoundTripADirectory(t *testing.T) {
	workspace := t.TempDir()
	mustMkdir(t, filepath.Join(workspace, "dist", "assets"))
	mustWrite(t, filepath.Join(workspace, "dist", "index.html"), "<html>\n", 0o600)
	mustWrite(t, filepath.Join(workspace, "dist", "assets", "run.sh"), "#!/bin/sh\n", 0o700)
	mustWrite(t, filepath.Join(workspace, "untouched.txt"), "stays\n", 0o600)

	var archive bytes.Buffer
	if err := Archive(workspace, "dist", &archive); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "out")
	entries, err := CopyTo(&archive, "dist", destination, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 {
		t.Fatalf("entries = %#v", entries)
	}
	assertContent(t, filepath.Join(destination, "index.html"), "<html>\n")
	assertContent(t, filepath.Join(destination, "assets", "run.sh"), "#!/bin/sh\n")
	info, err := os.Stat(filepath.Join(destination, "assets", "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("executable bit was lost: %v", info.Mode())
	}
	if _, err := os.Lstat(filepath.Join(destination, "untouched.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the copy reached outside the requested path")
	}
}

func TestArchiveCopiesASingleFile(t *testing.T) {
	workspace := t.TempDir()
	mustMkdir(t, filepath.Join(workspace, "logs"))
	mustWrite(t, filepath.Join(workspace, "logs", "app.log"), "line\n", 0o600)

	var archive bytes.Buffer
	if err := Archive(workspace, "logs/app.log", &archive); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "app.log")
	entries, err := CopyTo(&archive, "app.log", destination, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Path != "app.log" || entries[0].Size != 5 {
		t.Fatalf("entries = %#v", entries)
	}
	assertContent(t, destination, "line\n")
}

// Anything that is not a regular file or directory would resolve on the Mac
// against a filesystem the run never saw.
func TestArchiveRefusesSymlinksAndSpecialFiles(t *testing.T) {
	workspace := t.TempDir()
	mustMkdir(t, filepath.Join(workspace, "dist"))
	mustWrite(t, filepath.Join(workspace, "dist", "real.txt"), "content\n", 0o600)
	if err := os.Symlink("/etc/passwd", filepath.Join(workspace, "dist", "escape")); err != nil {
		t.Fatal(err)
	}

	err := Archive(workspace, "dist", &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "not a regular file or directory") {
		t.Fatalf("err = %v", err)
	}

	// A symlink named directly is refused for the same reason.
	if err := Archive(workspace, "dist/escape", &bytes.Buffer{}); err == nil {
		t.Fatal("a symlink was copied out")
	}
}

func TestSafePathRefusesEscapingRequests(t *testing.T) {
	for name, request := range map[string]string{
		"absolute":        "/etc/passwd",
		"climbing":        "../../secrets",
		"whole workspace": ".",
		"empty":           "",
	} {
		if _, err := SafePath(request); err == nil {
			t.Errorf("%s request %q was accepted", name, request)
		}
	}
	cleaned, err := SafePath("./dist/../dist/index.html")
	if err != nil || cleaned != "dist/index.html" {
		t.Fatalf("cleaned = %q, err = %v", cleaned, err)
	}
}

// The archive comes from the run, so entries it was never asked for and paths
// that climb out must be refused on arrival.
func TestExtractRefusesEntriesTheMacDidNotRequest(t *testing.T) {
	for name, entry := range map[string]tar.Header{
		"climbing":    {Name: "../escape.txt", Typeflag: tar.TypeReg},
		"absolute":    {Name: "/etc/passwd", Typeflag: tar.TypeReg},
		"outside":     {Name: "other/file.txt", Typeflag: tar.TypeReg},
		"symlink":     {Name: "dist/link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
		"hard link":   {Name: "dist/hard", Typeflag: tar.TypeLink, Linkname: "/etc/passwd"},
		"device":      {Name: "dist/null", Typeflag: tar.TypeChar},
		"named pipe":  {Name: "dist/pipe", Typeflag: tar.TypeFifo},
		"sneaky base": {Name: "distraction/file.txt", Typeflag: tar.TypeReg},
	} {
		t.Run(name, func(t *testing.T) {
			var archive bytes.Buffer
			writer := tar.NewWriter(&archive)
			header := entry
			if err := writer.WriteHeader(&header); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			staging := t.TempDir()
			root, err := os.OpenRoot(staging)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			if _, err := ExtractInto(&archive, root, "dist"); err == nil {
				t.Fatalf("%s entry was extracted", name)
			}
		})
	}
}

// A destination that already exists is never replaced by accident, and a
// refused copy leaves it byte-for-byte as it was.
func TestCopyToProtectsAnExistingDestination(t *testing.T) {
	workspace := t.TempDir()
	mustMkdir(t, filepath.Join(workspace, "dist"))
	mustWrite(t, filepath.Join(workspace, "dist", "index.html"), "new\n", 0o600)
	var archive bytes.Buffer
	if err := Archive(workspace, "dist", &archive); err != nil {
		t.Fatal(err)
	}
	original := archive.Bytes()

	destination := filepath.Join(t.TempDir(), "out")
	mustMkdir(t, destination)
	mustWrite(t, filepath.Join(destination, "index.html"), "old\n", 0o600)

	_, err := CopyTo(bytes.NewReader(original), "dist", destination, false)
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("err = %v", err)
	}
	assertContent(t, filepath.Join(destination, "index.html"), "old\n")

	if _, err := CopyTo(bytes.NewReader(original), "dist", destination, true); err != nil {
		t.Fatal(err)
	}
	assertContent(t, filepath.Join(destination, "index.html"), "new\n")
}

// A destination symlink is replaced, never written through: the file it points
// at is not the file the user named.
func TestCopyToReplacesADestinationSymlinkWithoutFollowingIt(t *testing.T) {
	workspace := t.TempDir()
	mustMkdir(t, filepath.Join(workspace, "dist"))
	mustWrite(t, filepath.Join(workspace, "dist", "index.html"), "new\n", 0o600)
	var archive bytes.Buffer
	if err := Archive(workspace, "dist", &archive); err != nil {
		t.Fatal(err)
	}

	elsewhere := t.TempDir()
	victim := filepath.Join(elsewhere, "victim")
	mustMkdir(t, victim)
	mustWrite(t, filepath.Join(victim, "keep.txt"), "untouched\n", 0o600)
	destination := filepath.Join(t.TempDir(), "out")
	if err := os.Symlink(victim, destination); err != nil {
		t.Fatal(err)
	}

	if _, err := CopyTo(&archive, "dist", destination, true); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("the copy was written through a symlink")
	}
	assertContent(t, filepath.Join(victim, "keep.txt"), "untouched\n")
	if _, err := os.Lstat(filepath.Join(victim, "index.html")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the copy landed in the symlink's target")
	}
}

func TestCopyToRefusesAnOversizedFile(t *testing.T) {
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	if err := writer.WriteHeader(&tar.Header{
		Name:     "dist",
		Typeflag: tar.TypeReg,
		Size:     MaxFileBytes + 1,
	}); err != nil {
		t.Fatal(err)
	}

	destination := filepath.Join(t.TempDir(), "out")
	_, err := CopyTo(bytes.NewReader(archive.Bytes()), "dist", destination, false)
	if err == nil || !strings.Contains(err.Error(), "per-file limit") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a refused copy left a destination behind")
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func assertContent(t *testing.T, path, want string) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != want {
		t.Fatalf("%s = %q, want %q", path, content, want)
	}
}
