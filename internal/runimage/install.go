package runimage

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/mpizenberg/pisafe/internal/guestcall"
)

const (
	// PackagedGuestName is the file the controller is shipped beside, holding
	// the helper that goes into the run image.
	PackagedGuestName = "pisafe-guest-linux-arm64"

	managedTagPrefix     = "localhost/pisafe-run:managed-"
	recipeLabel          = "io.pisafe.recipe.digest"
	maxContainerfileSize = 1 << 20
	maxGuestSize         = 64 << 20
)

type Backend interface {
	Execute(context.Context, io.Reader, ...string) ([]byte, error)
}

// Artifacts are the complete build context allowed to cross into the VM.
type Artifacts struct {
	Containerfile []byte
	Guest         []byte
}

type Installer struct {
	backend Backend
}

func NewInstaller(backend Backend) Installer {
	return Installer{backend: backend}
}

func (artifacts Artifacts) Validate() error {
	if len(artifacts.Containerfile) == 0 || len(artifacts.Containerfile) > maxContainerfileSize {
		return fmt.Errorf("Containerfile size is outside the allowed range")
	}
	if len(artifacts.Guest) == 0 || len(artifacts.Guest) > maxGuestSize {
		return fmt.Errorf("guest helper size is outside the allowed range")
	}
	file, err := elf.NewFile(bytes.NewReader(artifacts.Guest))
	if err != nil {
		return fmt.Errorf("guest helper is not an ELF executable: %w", err)
	}
	defer file.Close()
	if file.Class != elf.ELFCLASS64 ||
		file.Data != elf.ELFDATA2LSB ||
		file.Machine != elf.EM_AARCH64 ||
		(file.Type != elf.ET_EXEC && file.Type != elf.ET_DYN) {
		return fmt.Errorf("guest helper must be a Linux ARM64 executable")
	}
	for _, program := range file.Progs {
		if program.Type == elf.PT_INTERP {
			return fmt.Errorf("guest helper must not require a dynamic interpreter")
		}
	}
	libraries, err := file.ImportedLibraries()
	if err != nil {
		return fmt.Errorf("inspect guest helper linkage: %w", err)
	}
	if len(libraries) != 0 {
		return fmt.Errorf("guest helper must be statically linked")
	}
	if !bytes.Contains(artifacts.Guest, []byte(guestcall.Contract)) {
		return fmt.Errorf(
			"guest helper answers to a different set of calls than this pisafe "+
				"makes; rebuild %s from this source tree",
			PackagedGuestName,
		)
	}
	return nil
}

func (artifacts Artifacts) RecipeDigest() string {
	digest := sha256.New()
	for _, part := range [][]byte{
		[]byte("pisafe-run-image-v1"),
		artifacts.Containerfile,
		artifacts.Guest,
	} {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(part)))
		_, _ = digest.Write(length[:])
		_, _ = digest.Write(part)
	}
	return "sha256:" + hex.EncodeToString(digest.Sum(nil))
}

// Ensure returns an immutable image ID. A mutable local tag is only used as a
// cache lookup key and is never returned as the run-container identity.
func (installer Installer) Ensure(
	ctx context.Context,
	artifacts Artifacts,
) (string, error) {
	if installer.backend == nil {
		return "", errors.New("run-image backend is required")
	}
	if err := artifacts.Validate(); err != nil {
		return "", err
	}
	recipe := artifacts.RecipeDigest()
	tag := managedTag(recipe)

	if image, err := installer.inspect(ctx, tag); err == nil {
		if err := validateImage(image, recipe); err == nil {
			return image.ID, nil
		}
	}

	contextArchive, err := buildContext(artifacts)
	if err != nil {
		return "", err
	}
	if _, err := installer.backend.Execute(
		ctx,
		bytes.NewReader(contextArchive),
		"podman", "build",
		"--file", "Containerfile",
		"--pull=missing",
		"--dns=1.1.1.1",
		"--dns=9.9.9.9",
		"--build-arg", "PISAFE_RECIPE_DIGEST="+recipe,
		"--tag", tag,
		"-",
	); err != nil {
		return "", fmt.Errorf("build managed run image: %w", err)
	}

	image, err := installer.inspect(ctx, tag)
	if err != nil {
		return "", fmt.Errorf("inspect built run image: %w", err)
	}
	if err := validateImage(image, recipe); err != nil {
		return "", fmt.Errorf("validate built run image: %w", err)
	}
	return image.ID, nil
}

// Superseded lists managed run images that nothing can still start a container
// from: neither the recipe this controller builds, nor an image a run pins.
// Only images carrying pisafe's own recipe label are ever named, so nothing
// else in the VM's image store can be reached through here.
//
// The current recipe is recognized by the label an image carries rather than
// by looking its ID up first, so a lookup that fails can never be mistaken for
// a recipe that has no image.
func (installer Installer) Superseded(
	ctx context.Context,
	recipe string,
	keep []string,
) ([]string, error) {
	if installer.backend == nil {
		return nil, errors.New("run-image backend is required")
	}
	if recipe == "" {
		return nil, errors.New("current recipe digest is required")
	}
	output, err := installer.backend.Execute(
		ctx,
		nil,
		"podman", "image", "list",
		"--filter", "label="+recipeLabel,
		"--format", "json",
	)
	if err != nil {
		return nil, fmt.Errorf("list managed run images: %w", err)
	}
	var images []listedImage
	if err := json.Unmarshal(output, &images); err != nil {
		return nil, fmt.Errorf("decode managed run image list: %w", err)
	}
	retained := make(map[string]struct{}, len(keep))
	for _, id := range keep {
		if normalized, ok := normalizeImageID(id); ok {
			retained[normalized] = struct{}{}
		}
	}
	superseded := make([]string, 0, len(images))
	for _, image := range images {
		// The filter is Podman's; this check is ours, so the promise that an
		// unlabelled image is never removed does not depend on it.
		if image.Labels[recipeLabel] == "" || image.Labels[recipeLabel] == recipe {
			continue
		}
		normalized, ok := normalizeImageID(image.ID)
		if !ok {
			return nil, errors.New("Podman listed an invalid image ID")
		}
		if _, keeping := retained[normalized]; keeping {
			continue
		}
		superseded = append(superseded, normalized)
	}
	return superseded, nil
}

// Prune removes the superseded managed images. An image still in use fails to
// remove and is reported rather than forced away, because a live container is
// exactly what must not be broken.
func (installer Installer) Prune(
	ctx context.Context,
	recipe string,
	keep []string,
) ([]string, error) {
	superseded, err := installer.Superseded(ctx, recipe, keep)
	if err != nil {
		return nil, err
	}
	removed := make([]string, 0, len(superseded))
	var failures []error
	for _, id := range superseded {
		if _, err := installer.backend.Execute(ctx, nil, "podman", "rmi", id); err != nil {
			failures = append(failures, fmt.Errorf("remove run image %s: %w", id, err))
			continue
		}
		removed = append(removed, id)
	}
	return removed, errors.Join(failures...)
}

func managedTag(recipe string) string {
	return managedTagPrefix + strings.TrimPrefix(recipe, "sha256:")[:16]
}

type listedImage struct {
	ID     string            `json:"Id"`
	Labels map[string]string `json:"Labels"`
}

type inspectedImage struct {
	ID           string `json:"Id"`
	Architecture string `json:"Architecture"`
	OS           string `json:"Os"`
	Config       struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
}

func (installer Installer) inspect(ctx context.Context, tag string) (inspectedImage, error) {
	output, err := installer.backend.Execute(
		ctx,
		nil,
		"podman", "image", "inspect", tag,
	)
	if err != nil {
		return inspectedImage{}, err
	}
	var images []inspectedImage
	if err := json.Unmarshal(output, &images); err != nil {
		return inspectedImage{}, fmt.Errorf("decode Podman image inspection: %w", err)
	}
	if len(images) != 1 {
		return inspectedImage{}, fmt.Errorf("expected one inspected image, got %d", len(images))
	}
	if normalized, ok := normalizeImageID(images[0].ID); ok {
		images[0].ID = normalized
	}
	return images[0], nil
}

func validateImage(image inspectedImage, recipe string) error {
	if !validImageID(image.ID) {
		return fmt.Errorf("Podman returned an invalid immutable image ID")
	}
	if image.OS != "linux" || image.Architecture != "arm64" {
		return fmt.Errorf("run image platform is %s/%s, expected linux/arm64", image.OS, image.Architecture)
	}
	baseDigest := strings.TrimPrefix(BaseImage, "docker.io/library/node@")
	expectedLabels := map[string]string{
		"io.pisafe.base.digest": baseDigest,
		"io.pisafe.pi.version":  PiVersion,
		recipeLabel:             recipe,
	}
	for key, expected := range expectedLabels {
		if image.Config.Labels[key] != expected {
			return fmt.Errorf("run image label %s does not match the managed recipe", key)
		}
	}
	return nil
}

func validImageID(id string) bool {
	normalized, ok := normalizeImageID(id)
	return ok && normalized == id
}

func normalizeImageID(id string) (string, bool) {
	raw := strings.TrimPrefix(id, "sha256:")
	if len(raw) != sha256.Size*2 {
		return "", false
	}
	if _, err := hex.DecodeString(raw); err != nil {
		return "", false
	}
	return "sha256:" + raw, true
}

func buildContext(artifacts Artifacts) ([]byte, error) {
	var buffer bytes.Buffer
	archive := tar.NewWriter(&buffer)
	files := []struct {
		name string
		mode int64
		data []byte
	}{
		{name: "Containerfile", mode: 0o600, data: artifacts.Containerfile},
		{name: "pisafe-guest", mode: 0o700, data: artifacts.Guest},
	}
	for _, file := range files {
		header := &tar.Header{
			Name:     file.name,
			Mode:     file.mode,
			Size:     int64(len(file.data)),
			Typeflag: tar.TypeReg,
			Format:   tar.FormatPAX,
		}
		if err := archive.WriteHeader(header); err != nil {
			return nil, fmt.Errorf("write image context header: %w", err)
		}
		if _, err := archive.Write(file.data); err != nil {
			return nil, fmt.Errorf("write image context file: %w", err)
		}
	}
	if err := archive.Close(); err != nil {
		return nil, fmt.Errorf("close image context: %w", err)
	}
	return buffer.Bytes(), nil
}
