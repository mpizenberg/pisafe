package runimage

import (
	"fmt"
	"strings"
	"testing"
)

func TestContainerfileUsesRecordedPins(t *testing.T) {
	text := string(packagedContainerfile)
	for _, pin := range []string{BaseImage, PiVersion, PiIntegrity} {
		if !strings.Contains(text, pin) {
			t.Errorf("Containerfile does not contain pin %q", pin)
		}
	}
	if strings.Contains(text, ":latest") {
		t.Error("Containerfile contains a floating latest tag")
	}
}

// A shrinkwrap entry carrying no integrity hash lets npm install a later
// release than the one it names, so the build must repin each of these by exact
// version and digest. Recording them here is what makes a Pi version bump that
// forgets them fail before any image is built.
func TestContainerfileRepinsShrinkwrapGaps(t *testing.T) {
	text := string(packagedContainerfile)
	if !strings.Contains(text, "grep -qx 'package/npm-shrinkwrap.json'") {
		t.Error("Containerfile does not assert that Pi still ships a shrinkwrap")
	}
	for _, dependency := range PinnedDependencies {
		if dependency.Version != PiVersion {
			t.Errorf(
				"%s is pinned to %q while Pi is pinned to %q",
				dependency.Name, dependency.Version, PiVersion,
			)
		}
		if !strings.HasPrefix(dependency.Integrity, "sha512-") {
			t.Errorf("%s integrity %q is not a sha512 hash", dependency.Name, dependency.Integrity)
		}
		expected := fmt.Sprintf(
			"pin %s %s '%s'",
			dependency.Name, dependency.Version, dependency.Integrity,
		)
		if !strings.Contains(text, expected) {
			t.Errorf("Containerfile does not repin %s: expected %q", dependency.Name, expected)
		}
	}
}
