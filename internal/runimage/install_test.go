package runimage

import (
	"archive/tar"
	"bytes"
	"context"
	"debug/elf"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type imageCall struct {
	args  []string
	stdin []byte
}

type imageBackend struct {
	calls   []imageCall
	outputs [][]byte
	errors  []error
}

func (backend *imageBackend) Execute(
	_ context.Context,
	stdin io.Reader,
	args ...string,
) ([]byte, error) {
	call := imageCall{args: append([]string(nil), args...)}
	if stdin != nil {
		call.stdin, _ = io.ReadAll(stdin)
	}
	backend.calls = append(backend.calls, call)
	var output []byte
	if len(backend.outputs) > 0 {
		output = backend.outputs[0]
		backend.outputs = backend.outputs[1:]
	}
	var err error
	if len(backend.errors) > 0 {
		err = backend.errors[0]
		backend.errors = backend.errors[1:]
	}
	return output, err
}

func TestEnsureReusesValidatedImage(t *testing.T) {
	artifacts := testArtifacts(t)
	recipe := artifacts.RecipeDigest()
	backend := &imageBackend{outputs: [][]byte{inspectJSON(t, recipe)}}

	result, err := NewInstaller(backend).Ensure(context.Background(), artifacts)
	if err != nil {
		t.Fatal(err)
	}
	if result.Built {
		t.Fatal("validated image was unexpectedly rebuilt")
	}
	if result.ImageID != testImageID() || len(backend.calls) != 1 {
		t.Fatalf("result = %#v, calls = %#v", result, backend.calls)
	}
}

func TestEnsureNormalizesPodmanBareImageID(t *testing.T) {
	artifacts := testArtifacts(t)
	recipe := artifacts.RecipeDigest()
	inspection := inspectJSON(t, recipe)
	inspection = bytes.ReplaceAll(inspection, []byte("sha256:012345"), []byte("012345"))
	backend := &imageBackend{outputs: [][]byte{inspection}}

	result, err := NewInstaller(backend).Ensure(context.Background(), artifacts)
	if err != nil {
		t.Fatal(err)
	}
	if result.ImageID != testImageID() {
		t.Fatalf("image ID = %q", result.ImageID)
	}
}

func TestEnsureBuildsFromTwoFileContextAndValidatesResult(t *testing.T) {
	artifacts := testArtifacts(t)
	recipe := artifacts.RecipeDigest()
	backend := &imageBackend{
		outputs: [][]byte{nil, nil, inspectJSON(t, recipe)},
		errors:  []error{errors.New("image not found"), nil, nil},
	}

	result, err := NewInstaller(backend).Ensure(context.Background(), artifacts)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Built || result.ImageID != testImageID() {
		t.Fatalf("result = %#v", result)
	}
	if len(backend.calls) != 3 {
		t.Fatalf("calls = %#v", backend.calls)
	}
	build := backend.calls[1]
	if build.args[0] != "podman" || build.args[1] != "build" {
		t.Fatalf("build args = %#v", build.args)
	}
	if !containsPair(build.args, "--build-arg", "PISAFE_RECIPE_DIGEST="+recipe) {
		t.Fatalf("recipe build arg missing from %#v", build.args)
	}
	reader := tar.NewReader(bytes.NewReader(build.stdin))
	var names []string
	var contents [][]byte
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, header.Name)
		content, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		contents = append(contents, content)
	}
	if !reflect.DeepEqual(names, []string{"Containerfile", "pisafe-guest"}) {
		t.Fatalf("context files = %#v", names)
	}
	if !bytes.Equal(contents[0], artifacts.Containerfile) ||
		!bytes.Equal(contents[1], artifacts.Guest) {
		t.Fatal("build context contents changed")
	}
}

func TestEnsureRejectsMislabeledBuiltImage(t *testing.T) {
	artifacts := testArtifacts(t)
	backend := &imageBackend{
		outputs: [][]byte{nil, nil, inspectJSON(t, "sha256:"+string(make([]byte, 64)))},
		errors:  []error{errors.New("image not found"), nil, nil},
	}
	if _, err := NewInstaller(backend).Ensure(context.Background(), artifacts); err == nil {
		t.Fatal("Ensure accepted a mislabeled image")
	}
}

func TestRecipeDigestSeparatesArtifactBoundaries(t *testing.T) {
	left := Artifacts{Containerfile: []byte("ab"), Guest: []byte("c")}
	right := Artifacts{Containerfile: []byte("a"), Guest: []byte("bc")}
	if left.RecipeDigest() == right.RecipeDigest() {
		t.Fatal("recipe digest does not separate artifact boundaries")
	}
}

func TestLoadArtifactsAcceptsRegularARM64Helper(t *testing.T) {
	root := t.TempDir()
	containerfilePath := filepath.Join(root, "Containerfile")
	guestPath := filepath.Join(root, "pisafe-guest")
	if err := os.WriteFile(containerfilePath, []byte("FROM scratch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(guestPath, minimalARM64ELF(), 0o700); err != nil {
		t.Fatal(err)
	}
	artifacts, err := LoadArtifacts(containerfilePath, guestPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(artifacts.Containerfile) != "FROM scratch\n" {
		t.Fatalf("Containerfile = %q", artifacts.Containerfile)
	}
}

func TestLoadArtifactsRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	realPath := filepath.Join(root, "real-Containerfile")
	linkPath := filepath.Join(root, "Containerfile")
	guestPath := filepath.Join(root, "pisafe-guest")
	if err := os.WriteFile(realPath, []byte("FROM scratch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realPath, linkPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(guestPath, minimalARM64ELF(), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadArtifacts(linkPath, guestPath); err == nil {
		t.Fatal("LoadArtifacts accepted a symlink")
	}
}

func testArtifacts(t *testing.T) Artifacts {
	t.Helper()
	return Artifacts{
		Containerfile: []byte("FROM scratch\n"),
		Guest:         minimalARM64ELF(),
	}
}

func minimalARM64ELF() []byte {
	header := make([]byte, 64)
	copy(header[:4], []byte{0x7f, 'E', 'L', 'F'})
	header[4] = byte(elf.ELFCLASS64)
	header[5] = byte(elf.ELFDATA2LSB)
	header[6] = byte(elf.EV_CURRENT)
	binary.LittleEndian.PutUint16(header[16:18], uint16(elf.ET_EXEC))
	binary.LittleEndian.PutUint16(header[18:20], uint16(elf.EM_AARCH64))
	binary.LittleEndian.PutUint32(header[20:24], uint32(elf.EV_CURRENT))
	binary.LittleEndian.PutUint16(header[52:54], 64)
	return header
}

func inspectJSON(t *testing.T, recipe string) []byte {
	t.Helper()
	image := inspectedImage{
		ID:           testImageID(),
		Architecture: "arm64",
		OS:           "linux",
	}
	image.Config.Labels = map[string]string{
		"io.pisafe.base.digest":   stringsTrimBase(),
		"io.pisafe.pi.version":    PiVersion,
		"io.pisafe.recipe.digest": recipe,
	}
	content, err := json.Marshal([]inspectedImage{image})
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func stringsTrimBase() string {
	const prefix = "docker.io/library/node@"
	return BaseImage[len(prefix):]
}

func testImageID() string {
	return "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
}

func containsPair(args []string, first string, second string) bool {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == first && args[index+1] == second {
			return true
		}
	}
	return false
}
