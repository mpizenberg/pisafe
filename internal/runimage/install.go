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
	"os"
	"path/filepath"
	"strings"
)

const (
	managedTagPrefix     = "localhost/pisafe-run:managed-"
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

type Result struct {
	ImageID string
	Tag     string
	Recipe  string
	Built   bool
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
) (Result, error) {
	if installer.backend == nil {
		return Result{}, errors.New("run-image backend is required")
	}
	if err := artifacts.Validate(); err != nil {
		return Result{}, err
	}
	recipe := artifacts.RecipeDigest()
	tag := managedTagPrefix + strings.TrimPrefix(recipe, "sha256:")[:16]

	if image, err := installer.inspect(ctx, tag); err == nil {
		if err := validateImage(image, recipe); err == nil {
			return Result{
				ImageID: image.ID,
				Tag:     tag,
				Recipe:  recipe,
			}, nil
		}
	}

	contextArchive, err := buildContext(artifacts)
	if err != nil {
		return Result{}, err
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
		return Result{}, fmt.Errorf("build managed run image: %w", err)
	}

	image, err := installer.inspect(ctx, tag)
	if err != nil {
		return Result{}, fmt.Errorf("inspect built run image: %w", err)
	}
	if err := validateImage(image, recipe); err != nil {
		return Result{}, fmt.Errorf("validate built run image: %w", err)
	}
	return Result{
		ImageID: image.ID,
		Tag:     tag,
		Recipe:  recipe,
		Built:   true,
	}, nil
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
		"io.pisafe.base.digest":   baseDigest,
		"io.pisafe.pi.version":    PiVersion,
		"io.pisafe.recipe.digest": recipe,
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

func readRegularFile(path string, limit int64) ([]byte, error) {
	path = filepath.Clean(path)
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !pathInfo.Mode().IsRegular() {
		return nil, fmt.Errorf("path is not a regular file")
	}
	if pathInfo.Size() <= 0 || pathInfo.Size() > limit {
		return nil, fmt.Errorf("file size is outside the allowed range")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	openInfo, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(pathInfo, openInfo) {
		return nil, fmt.Errorf("file changed while it was being opened")
	}
	content, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if len(content) == 0 || int64(len(content)) > limit {
		return nil, fmt.Errorf("file size is outside the allowed range")
	}
	return content, nil
}
