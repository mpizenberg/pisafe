package runid

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestValidate(t *testing.T) {
	for _, id := range []string{"run", "run-123", "a.b_c"} {
		if err := Validate(id); err != nil {
			t.Fatalf("Validate(%q): %v", id, err)
		}
	}
	for _, id := range []string{
		"", "../run", "-run", "run/name", "run name", strings.Repeat("a", 65),
	} {
		if err := Validate(id); err == nil {
			t.Fatalf("Validate(%q) unexpectedly succeeded", id)
		}
	}
}

func TestNewUsesProjectTimeAndEntropy(t *testing.T) {
	id, err := newWithEntropy(
		"My Project",
		time.Date(2026, 7, 24, 14, 32, 5, 0, time.FixedZone("CEST", 2*60*60)),
		bytes.NewReader([]byte{0xde, 0xad, 0xbe, 0xef, 0xca, 0xfe}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if id != "my-project-20260724-123205-deadbeefcafe" {
		t.Fatalf("ID = %q", id)
	}
}

func TestNewFailsWhenEntropyFails(t *testing.T) {
	if _, err := newWithEntropy("project", time.Time{}, errorReader{}); err == nil {
		t.Fatal("New unexpectedly accepted failed entropy")
	}
}

func TestProjectSlug(t *testing.T) {
	tests := map[string]string{
		"My Project.v2":                   "my-project-v2",
		"éø":                              "project",
		"---hello___world---":             "hello-world",
		strings.Repeat("a", 64) + "-tail": strings.Repeat("a", 32),
	}
	for input, expected := range tests {
		if actual := projectSlug(input); actual != expected {
			t.Errorf("projectSlug(%q) = %q, want %q", input, actual, expected)
		}
	}
}

// TestNewPackageDirectorySeparatesNamesThatReduceAlike matters because two
// directories in one profile are two packages a run loads: a collision would
// serve one package's code under another's name.
func TestNewPackageDirectorySeparatesNamesThatReduceAlike(t *testing.T) {
	scoped, err := NewPackageDirectory("@earendil-works/plan-mode")
	if err != nil {
		t.Fatal(err)
	}
	if scoped != "earendil-works-plan-mode-bf0f2759" {
		t.Fatalf("directory = %q", scoped)
	}
	flat, err := NewPackageDirectory("earendil-works-plan-mode")
	if err != nil {
		t.Fatal(err)
	}
	if flat == scoped {
		t.Fatalf("two package names share directory %q", flat)
	}
	if _, err := NewPackageDirectory(""); err == nil {
		t.Fatal("an empty package name was accepted")
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) {
	return 0, errors.New("failed")
}

func TestNewProjectSeparatesCheckoutsSharingADirectoryName(t *testing.T) {
	first, err := NewProject("/Users/alice/work/api")
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewProject("/Users/alice/fork/api")
	if err != nil {
		t.Fatal(err)
	}
	if first.Directory != "api" || second.Directory != "api" {
		t.Fatalf("directories = %q, %q", first.Directory, second.Directory)
	}
	if first.Key == second.Key {
		t.Fatalf("two checkouts share the key %q", first.Key)
	}
	// The key is what a filesystem is named after, so it has to survive the
	// same validation a run ID does.
	for _, project := range []Project{first, second} {
		if err := Validate(project.Key); err != nil {
			t.Fatalf("Validate(%q): %v", project.Key, err)
		}
		if !strings.HasPrefix(project.Key, "api-") || len(project.Key) != len("api-")+8 {
			t.Fatalf("key = %q", project.Key)
		}
	}
	repeated, err := NewProject("/Users/alice/work/api/")
	if err != nil {
		t.Fatal(err)
	}
	if repeated.Key != first.Key {
		t.Fatalf("key = %q, want the stable %q", repeated.Key, first.Key)
	}
}

func TestNewProjectRefusesARootItCannotAddress(t *testing.T) {
	if _, err := NewProject("work/api"); err == nil {
		t.Fatal("a relative repository root was accepted")
	}
}
