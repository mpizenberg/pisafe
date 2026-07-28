package projectconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testImageID = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// writeRepository builds a checkout with the given files, using the same
// relative paths a declaration would name.
func writeRepository(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for relative, content := range files {
		path := filepath.Join(root, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestLoadDeclaredCaches(t *testing.T) {
	root := writeRepository(t, map[string]string{
		RelativePath: `{"caches":[
			{"name":"npm","env":["npm_config_cache"],"key":["package-lock.json"]}
		]}`,
	})
	config, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(config.Caches) != 1 {
		t.Fatalf("caches = %#v", config.Caches)
	}
	cache := config.Caches[0]
	if cache.Name != "npm" ||
		strings.Join(cache.Env, ",") != "npm_config_cache" ||
		strings.Join(cache.Key, ",") != "package-lock.json" {
		t.Fatalf("cache = %#v", cache)
	}
}

// TestRepositoryWithoutAConfigDeclaresNothing keeps adoption opt-in: a
// repository that has never heard of pisafe shares no state between runs.
func TestRepositoryWithoutAConfigDeclaresNothing(t *testing.T) {
	config, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(config.Caches) != 0 {
		t.Fatalf("caches = %#v", config.Caches)
	}
	mounts, err := config.Mounts(t.TempDir(), testImageID)
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 0 {
		t.Fatalf("mounts = %#v", mounts)
	}
}

// TestLoadRefusesAHostileDeclaration covers the file as what it is: input from
// the repository, parsed on the Mac before any sandbox exists.
func TestLoadRefusesAHostileDeclaration(t *testing.T) {
	for name, document := range map[string]string{
		"not JSON":      `caches = []`,
		"unknown field": `{"caches":[],"exec":"curl evil.example"}`,
		"unknown cache field": `{"caches":[{"name":"npm","env":["npm_config_cache"],
			"key":[],"command":"rm -rf /"}]}`,
		"climbing name":     `{"caches":[{"name":"../../etc","env":["npm_config_cache"],"key":[]}]}`,
		"nested name":       `{"caches":[{"name":"a/b","env":["npm_config_cache"],"key":[]}]}`,
		"duplicate name":    `{"caches":[{"name":"a","env":["X"],"key":[]},{"name":"a","env":["Y"],"key":[]}]}`,
		"no variable":       `{"caches":[{"name":"npm","env":[],"key":[]}]}`,
		"reserved variable": `{"caches":[{"name":"npm","env":["PI_CODING_AGENT_SESSION_DIR"],"key":[]}]}`,
		"shell variable":    `{"caches":[{"name":"npm","env":["X=1; rm -rf /"],"key":[]}]}`,
		"absolute key":      `{"caches":[{"name":"npm","env":["X"],"key":["/etc/passwd"]}]}`,
		"climbing key":      `{"caches":[{"name":"npm","env":["X"],"key":["../../etc/passwd"]}]}`,
		"uncleaned key":     `{"caches":[{"name":"npm","env":["X"],"key":["a/./b"]}]}`,
	} {
		root := writeRepository(t, map[string]string{RelativePath: document})
		if _, err := Load(root); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// TestLoadRefusesAnOversizedDeclaration stops a repository from making pisafe
// allocate before it has parsed anything.
func TestLoadRefusesAnOversizedDeclaration(t *testing.T) {
	root := writeRepository(t, map[string]string{
		RelativePath: `{"caches":[]}` + strings.Repeat(" ", maxConfigBytes),
	})
	if _, err := Load(root); err == nil {
		t.Fatal("an oversized declaration was accepted")
	}
}

// TestKeyFollowsInputsAndImage is what makes restoring correct: a key that
// moved means the declared inputs or the image moved, and a key that did not
// means neither did.
func TestKeyFollowsInputsAndImage(t *testing.T) {
	declaration := `{"caches":[{"name":"npm","env":["npm_config_cache"],
		"key":["package-lock.json","packages/api/package-lock.json"]}]}`
	files := map[string]string{
		RelativePath:                     declaration,
		"package-lock.json":              "one",
		"packages/api/package-lock.json": "two",
	}
	root := writeRepository(t, files)
	config, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	original, err := config.Mounts(root, testImageID)
	if err != nil {
		t.Fatal(err)
	}
	if len(original) != 1 {
		t.Fatalf("mounts = %#v", original)
	}

	unchanged, err := config.Mounts(writeRepository(t, files), testImageID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged[0].Key != original[0].Key {
		t.Errorf("key moved without an input moving: %q then %q", original[0].Key, unchanged[0].Key)
	}

	otherImage, err := config.Mounts(root, strings.ReplaceAll(testImageID, "a", "b"))
	if err != nil {
		t.Fatal(err)
	}
	if otherImage[0].Key == original[0].Key {
		t.Error("a different run image restored the same generation")
	}

	edited := map[string]string{}
	for relative, content := range files {
		edited[relative] = content
	}
	edited["packages/api/package-lock.json"] = "three"
	changed, err := config.Mounts(writeRepository(t, edited), testImageID)
	if err != nil {
		t.Fatal(err)
	}
	if changed[0].Key == original[0].Key {
		t.Error("an edited lockfile kept its key")
	}
}

// TestAbsentKeyFileIsAState covers a repository with no lockfile yet: it has
// no dependencies yet either, which is a key rather than a failure.
func TestAbsentKeyFileIsAState(t *testing.T) {
	declaration := `{"caches":[{"name":"npm","env":["npm_config_cache"],"key":["package-lock.json"]}]}`
	empty := writeRepository(t, map[string]string{RelativePath: declaration})
	config, err := Load(empty)
	if err != nil {
		t.Fatal(err)
	}
	absent, err := config.Mounts(empty, testImageID)
	if err != nil {
		t.Fatal(err)
	}
	if err := absent[0].Validate(); err != nil {
		t.Fatal(err)
	}
	present, err := config.Mounts(writeRepository(t, map[string]string{
		RelativePath:        declaration,
		"package-lock.json": "",
	}), testImageID)
	if err != nil {
		t.Fatal(err)
	}
	// An empty file and a missing file are different states of the repository,
	// so they must not collide.
	if present[0].Key == absent[0].Key {
		t.Error("a missing lockfile keyed the same as an empty one")
	}
}

// TestKeyFileCannotEscapeTheCheckout is the reason the declared paths are
// opened through a root: the file is attacker-controlled on an untrusted
// clone, and hashing arbitrary Mac files is not something it may ask for.
func TestKeyFileCannotEscapeTheCheckout(t *testing.T) {
	root := writeRepository(t, map[string]string{
		RelativePath: `{"caches":[{"name":"npm","env":["X"],"key":["escape"]}]}`,
	})
	if err := os.Symlink("/etc/passwd", filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	config, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := config.Mounts(root, testImageID); err == nil {
		t.Fatal("a key file outside the checkout was read")
	}
}
