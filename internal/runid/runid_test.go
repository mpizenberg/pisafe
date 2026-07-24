package runid

import (
	"strings"
	"testing"
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
