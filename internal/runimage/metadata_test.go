package runimage

import (
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
