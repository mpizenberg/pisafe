package safefile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadAcceptsOnlyABoundedRegularFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "record")
	if err := os.WriteFile(path, []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	content, err := Read(path, 64)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "content" {
		t.Fatalf("content = %q", content)
	}

	if _, err := Read(path, 3); err == nil {
		t.Error("a file past the limit was read")
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(link, 64); err == nil {
		t.Error("a symlink was read")
	}
	if _, err := Read(root, 64); err == nil {
		t.Error("a directory was read")
	}
	if _, err := Read(filepath.Join(root, "absent"), 64); err == nil {
		t.Error("a missing file was read")
	}
}

// What a store keeps must never be half of itself under its own name, and a
// creation must lose to whoever got there first rather than overwrite them.
func TestCreateRefusesToReplaceAndReplaceDoesNot(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "record")
	if err := Create(path, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := Create(path, []byte("second"), 0o600)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("error = %v", err)
	}
	content, err := Read(path, 64)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "first" {
		t.Fatalf("refused creation changed the file to %q", content)
	}

	if err := Replace(path, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	if content, err := Read(path, 64); err != nil || string(content) != "second" {
		t.Fatalf("content = %q, err = %v", content, err)
	}

	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", info.Mode().Perm())
	}
	// Neither path may leave its temporary file behind: the directory holding a
	// store's records is listed to find them.
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "record" {
		t.Fatalf("directory holds %#v", entries)
	}
}

func TestWritingRefusesADirectoryThatIsNotThere(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "absent", "record")
	if err := Create(absent, []byte("content"), 0o600); err == nil {
		t.Error("a file was created outside any directory")
	}
	if err := Replace(absent, []byte("content"), 0o600); err == nil {
		t.Error("a file was replaced outside any directory")
	}
}
