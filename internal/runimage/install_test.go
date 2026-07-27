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
	"strings"
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

func TestLoadPackagedArtifactsAcceptsRegularARM64Helper(t *testing.T) {
	root := t.TempDir()
	guestPath := filepath.Join(root, "pisafe-guest")
	if err := os.WriteFile(guestPath, minimalARM64ELF(), 0o700); err != nil {
		t.Fatal(err)
	}
	artifacts, err := LoadPackagedArtifacts(guestPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(artifacts.Containerfile, packagedContainerfile) {
		t.Fatal("packaged Containerfile was not loaded")
	}
}

func TestLoadPackagedArtifactsRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	guestPath := filepath.Join(root, "real-pisafe-guest")
	linkPath := filepath.Join(root, "pisafe-guest")
	if err := os.WriteFile(guestPath, minimalARM64ELF(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(guestPath, linkPath); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPackagedArtifacts(linkPath); err == nil {
		t.Fatal("LoadPackagedArtifacts accepted a symlink")
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

func TestPruneRemovesOnlySupersededManagedImages(t *testing.T) {
	current := testImageID()
	superseded := "sha256:" + strings.Repeat("a", 64)
	// Podman reports a bare hex ID here, and an image that is not pisafe's
	// must survive even if it reaches the list.
	foreign := strings.Repeat("f", 64)
	listed, err := json.Marshal([]listedImage{
		{ID: current, Labels: map[string]string{recipeLabel: "sha256:current"}},
		{ID: strings.TrimPrefix(superseded, "sha256:"), Labels: map[string]string{
			recipeLabel: "sha256:older",
		}},
		{ID: foreign, Labels: map[string]string{"org.opencontainers.image.title": "node"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	backend := &imageBackend{outputs: [][]byte{listed}}

	removed, err := NewInstaller(backend).Prune(
		context.Background(),
		"sha256:current",
		[]string{current},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(removed, []string{superseded}) {
		t.Fatalf("removed = %#v", removed)
	}
	if len(backend.calls) != 2 {
		t.Fatalf("calls = %#v", backend.calls)
	}
	if !containsPair(backend.calls[0].args, "--filter", "label="+recipeLabel) {
		t.Fatalf("list args = %#v", backend.calls[0].args)
	}
	remove := backend.calls[1].args
	if !reflect.DeepEqual(remove, []string{"podman", "rmi", superseded}) {
		t.Fatalf("remove args = %#v", remove)
	}
}

// An image a container still uses must fail to remove rather than be forced
// away, and must not stop the rest of the sweep.
func TestPruneReportsAnImageStillInUseAndContinues(t *testing.T) {
	first := "sha256:" + strings.Repeat("a", 64)
	second := "sha256:" + strings.Repeat("b", 64)
	listed, err := json.Marshal([]listedImage{
		{ID: first, Labels: map[string]string{recipeLabel: "sha256:older"}},
		{ID: second, Labels: map[string]string{recipeLabel: "sha256:oldest"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	backend := &imageBackend{
		outputs: [][]byte{listed, nil, nil},
		errors:  []error{nil, errors.New("image is in use by a container"), nil},
	}

	removed, pruneErr := NewInstaller(backend).Prune(context.Background(), "sha256:newest", nil)
	if pruneErr == nil || !strings.Contains(pruneErr.Error(), first) {
		t.Fatalf("prune error = %v", pruneErr)
	}
	if !reflect.DeepEqual(removed, []string{second}) {
		t.Fatalf("removed = %#v", removed)
	}
}

// A lookup that fails must never be mistaken for a recipe that has no image,
// so the current recipe is recognized by the label its image carries.
func TestSupersededKeepsTheCurrentRecipeWithoutLookingItUp(t *testing.T) {
	current := "sha256:" + strings.Repeat("a", 64)
	older := "sha256:" + strings.Repeat("b", 64)
	listed, err := json.Marshal([]listedImage{
		{ID: current, Labels: map[string]string{recipeLabel: "sha256:current"}},
		{ID: older, Labels: map[string]string{recipeLabel: "sha256:older"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	backend := &imageBackend{outputs: [][]byte{listed}}

	superseded, err := NewInstaller(backend).Superseded(
		context.Background(),
		"sha256:current",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(superseded, []string{older}) {
		t.Fatalf("superseded = %#v", superseded)
	}
	// Exactly one call: the image list. Nothing was resolved by tag.
	if len(backend.calls) != 1 {
		t.Fatalf("calls = %#v", backend.calls)
	}
	if _, err := NewInstaller(backend).Superseded(context.Background(), "", nil); err == nil {
		t.Fatal("collection proceeded without knowing the current recipe")
	}
}
