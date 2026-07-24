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
		if actual := ProjectSlug(input); actual != expected {
			t.Errorf("ProjectSlug(%q) = %q, want %q", input, actual, expected)
		}
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) {
	return 0, errors.New("failed")
}
