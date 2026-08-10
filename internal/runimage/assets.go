package runimage

import (
	_ "embed"
	"fmt"

	"github.com/mpizenberg/pisafe/internal/safefile"
)

//go:embed Containerfile
var packagedContainerfile []byte

// LoadPackagedArtifacts combines the Containerfile embedded in the controller
// with the separately packaged static Linux ARM64 guest helper.
func LoadPackagedArtifacts(guestPath string) (Artifacts, error) {
	guest, err := safefile.Read(guestPath, maxGuestSize)
	if err != nil {
		return Artifacts{}, fmt.Errorf("load guest helper: %w", err)
	}
	artifacts := Artifacts{
		Containerfile: append([]byte(nil), packagedContainerfile...),
		Guest:         guest,
	}
	if err := artifacts.Validate(); err != nil {
		return Artifacts{}, err
	}
	return artifacts, nil
}
