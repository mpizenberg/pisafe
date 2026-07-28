// Package projectconfig reads what a repository declares about its own
// persistence, from .config/pisafe.json at the repository root.
//
// The file arrives with the repository and is parsed on the Mac before any
// sandbox exists, so the schema is inert: it names caches and the variables
// that point at them, and nothing in it selects a path pisafe mounts or a
// command pisafe runs. The worst a hostile declaration achieves is a useless
// cache key or a full project image.
package projectconfig

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"

	"github.com/mpizenberg/pisafe/internal/runcontainer"
	"github.com/mpizenberg/pisafe/internal/runid"
)

// RelativePath is where a repository declares its configuration. It sits under
// .config/ so adopting pisafe costs a repository no new root entry.
const RelativePath = ".config/pisafe.json"

const (
	maxConfigBytes  = 64 << 10
	maxCaches       = 16
	maxKeyPaths     = 32
	maxKeyFileBytes = 64 << 20
)

type Config struct {
	Caches []Cache `json:"caches"`
}

// Cache is one declared namespace: the variables that point a tool at it, and
// the repository files whose contents decide which generation is restored.
type Cache struct {
	Name string   `json:"name"`
	Env  []string `json:"env"`
	Key  []string `json:"key"`
}

// Load reads the declaration at the root of one repository. A repository
// without the file declares nothing, which is not an error.
func Load(repositoryRoot string) (Config, error) {
	root, err := os.OpenRoot(repositoryRoot)
	if err != nil {
		return Config{}, fmt.Errorf("open repository root: %w", err)
	}
	defer root.Close()
	file, err := root.Open(RelativePath)
	if errors.Is(err, fs.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("read %s: %w", RelativePath, err)
	}
	defer file.Close()

	content, err := io.ReadAll(io.LimitReader(file, maxConfigBytes+1))
	if err != nil {
		return Config{}, fmt.Errorf("read %s: %w", RelativePath, err)
	}
	if len(content) > maxConfigBytes {
		return Config{}, fmt.Errorf("%s exceeds %d bytes", RelativePath, maxConfigBytes)
	}
	var config Config
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", RelativePath, err)
	}
	if err := config.validate(); err != nil {
		return Config{}, fmt.Errorf("%s: %w", RelativePath, err)
	}
	return config, nil
}

func (config Config) validate() error {
	if len(config.Caches) > maxCaches {
		return fmt.Errorf("declares %d caches, at most %d are allowed", len(config.Caches), maxCaches)
	}
	seen := make(map[string]bool, len(config.Caches))
	for _, cache := range config.Caches {
		if err := runid.Validate(cache.Name); err != nil {
			return fmt.Errorf("invalid cache name %q", cache.Name)
		}
		if seen[cache.Name] {
			return fmt.Errorf("cache %q is declared twice", cache.Name)
		}
		seen[cache.Name] = true
		if len(cache.Env) == 0 {
			return fmt.Errorf("cache %q points no variable at itself", cache.Name)
		}
		if err := runcontainer.ValidateCacheEnvironment(cache.Env); err != nil {
			return fmt.Errorf("cache %q: %w", cache.Name, err)
		}
		if len(cache.Key) > maxKeyPaths {
			return fmt.Errorf(
				"cache %q lists %d key files, at most %d are allowed",
				cache.Name, len(cache.Key), maxKeyPaths,
			)
		}
		for _, relative := range cache.Key {
			if err := validateKeyPath(relative); err != nil {
				return fmt.Errorf("cache %q: %w", cache.Name, err)
			}
		}
	}
	return nil
}

// validateKeyPath rejects anything that is not a plain location inside the
// repository. os.Root already refuses an escape at open time; refusing it here
// keeps a hostile declaration from reaching that far and makes the failure
// name the declaration rather than the filesystem.
func validateKeyPath(relative string) error {
	if !filepath.IsLocal(relative) || filepath.Clean(relative) != relative {
		return fmt.Errorf("key file %q is not a plain path inside the repository", relative)
	}
	return nil
}

// Mounts resolves every declared cache into the mount one run needs, keyed by
// the contents of the declared files under the image that will run them. A
// cache produced by a different image is not restored into a run of this one,
// for the same reason CI keys start with the runner OS.
func (config Config) Mounts(repositoryRoot, imageID string) ([]runcontainer.CacheMount, error) {
	if len(config.Caches) == 0 {
		return nil, nil
	}
	root, err := os.OpenRoot(repositoryRoot)
	if err != nil {
		return nil, fmt.Errorf("open repository root: %w", err)
	}
	defer root.Close()
	mounts := make([]runcontainer.CacheMount, 0, len(config.Caches))
	for _, cache := range config.Caches {
		key, err := cache.key(root, imageID)
		if err != nil {
			return nil, fmt.Errorf("key cache %q: %w", cache.Name, err)
		}
		mounts = append(mounts, cache.mount(key))
	}
	return mounts, nil
}

func (cache Cache) mount(key string) runcontainer.CacheMount {
	return runcontainer.CacheMount{Name: cache.Name, Env: cache.Env, Key: key}
}

func (cache Cache) key(root *os.Root, imageID string) (string, error) {
	digest := sha256.New()
	// Every part is length-prefixed so no two declarations can hash the same
	// bytes by running their contents together.
	write := func(part string) {
		_, _ = digest.Write([]byte(strconv.Itoa(len(part))))
		_, _ = digest.Write([]byte{0})
		_, _ = digest.Write([]byte(part))
	}
	write("pisafe-cache-key-v1")
	write(imageID)
	for _, relative := range cache.Key {
		write(relative)
		content, err := fileDigest(root, relative)
		if err != nil {
			return "", err
		}
		write(content)
	}
	return hex.EncodeToString(digest.Sum(nil))[:16], nil
}

// fileDigest hashes one declared key file. A missing file is a state a
// repository is legitimately in — no lockfile yet means no dependencies yet —
// so it contributes a marker rather than an error.
func fileDigest(root *os.Root, relative string) (string, error) {
	file, err := root.Open(relative)
	if errors.Is(err, fs.ErrNotExist) {
		return "absent", nil
	}
	if err != nil {
		return "", err
	}
	defer file.Close()
	digest := sha256.New()
	copied, err := io.Copy(digest, io.LimitReader(file, maxKeyFileBytes+1))
	if err != nil {
		return "", err
	}
	if copied > maxKeyFileBytes {
		return "", fmt.Errorf("key file %q exceeds %d bytes", relative, maxKeyFileBytes)
	}
	return "sha256:" + hex.EncodeToString(digest.Sum(nil)), nil
}
