package runimage

import (
	"os"
	"strings"
	"testing"
)

func TestContainerfileUsesRecordedPins(t *testing.T) {
	content, err := os.ReadFile("../../Containerfile")
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	for _, pin := range []string{BaseImage, PiVersion, PiIntegrity} {
		if !strings.Contains(text, pin) {
			t.Errorf("Containerfile does not contain pin %q", pin)
		}
	}
	if strings.Contains(text, ":latest") {
		t.Error("Containerfile contains a floating latest tag")
	}
}
