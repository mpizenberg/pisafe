// Package runstate persists the Mac-side audit and lifecycle record for runs.
package runstate

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mpizenberg/pisafe/internal/gitstage"
	"github.com/mpizenberg/pisafe/internal/runid"
)

const manifestVersion = 2

type State string

const (
	StateCreating  State = "creating"
	StateActive    State = "active"
	StateStopped   State = "stopped"
	StateImported  State = "imported"
	StateDiscarded State = "discarded"
	StateExpired   State = "expired"
)

type Manifest struct {
	Version        int               `json:"version"`
	RunID          string            `json:"run_id"`
	Project        string            `json:"project"`
	State          State             `json:"state"`
	Snapshot       gitstage.Snapshot `json:"snapshot"`
	Image          string            `json:"image,omitempty"`
	Container      string            `json:"container,omitempty"`
	Workspace      string            `json:"workspace,omitempty"`
	SSH            *SSHConnection    `json:"ssh,omitempty"`
	CreatedAt      time.Time         `json:"created_at"`
	UpdatedAt      time.Time         `json:"updated_at"`
	StoppedAt      *time.Time        `json:"stopped_at,omitempty"`
	ImportedAt     *time.Time        `json:"imported_at,omitempty"`
	DiscardedAt    *time.Time        `json:"discarded_at,omitempty"`
	ImportedBranch string            `json:"imported_branch,omitempty"`
	LastError      string            `json:"last_error,omitempty"`
}

type SSHConnection struct {
	Alias              string `json:"alias"`
	IdentityFile       string `json:"identity_file"`
	KnownHostsFile     string `json:"known_hosts_file"`
	ConfigFile         string `json:"config_file"`
	HostKeyFingerprint string `json:"host_key_fingerprint"`
}

type Store struct {
	root string
	now  func() time.Time
}

func NewStore(root string) Store {
	return Store{root: root, now: time.Now}
}

func DefaultRoot() (string, error) {
	if override := os.Getenv("PISAFE_STATE_DIR"); override != "" {
		return filepath.Clean(override), nil
	}
	config, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("find user state directory: %w", err)
	}
	return filepath.Join(config, "pisafe"), nil
}

func (store Store) Create(manifest Manifest) (Manifest, error) {
	if err := validateManifestIdentity(manifest); err != nil {
		return Manifest{}, err
	}
	if manifest.State != "" && manifest.State != StateCreating {
		return Manifest{}, fmt.Errorf("new run must be in %q state", StateCreating)
	}
	now := store.now().UTC()
	manifest.Version = manifestVersion
	manifest.State = StateCreating
	manifest.CreatedAt = now
	manifest.UpdatedAt = now
	if err := store.ensureRoot(); err != nil {
		return Manifest{}, err
	}
	path, err := store.manifestPath(manifest.RunID)
	if err != nil {
		return Manifest{}, err
	}
	if _, err := os.Lstat(path); err == nil {
		return Manifest{}, fmt.Errorf("run %q already exists", manifest.RunID)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return Manifest{}, fmt.Errorf("inspect run manifest: %w", err)
	}
	if err := store.writeAtomic(path, manifest, false); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func (store Store) Get(runID string) (Manifest, error) {
	path, err := store.manifestPath(runID)
	if err != nil {
		return Manifest{}, err
	}
	content, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Manifest{}, fmt.Errorf("run %q does not exist", runID)
	}
	if err != nil {
		return Manifest{}, fmt.Errorf("read run manifest: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(content, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode run manifest %q: %w", runID, err)
	}
	if err := validateStoredManifest(manifest, runID); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func (store Store) List() ([]Manifest, error) {
	entries, err := os.ReadDir(store.root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list run manifests: %w", err)
	}
	manifests := make([]Manifest, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		runID := entry.Name()[:len(entry.Name())-len(".json")]
		manifest, err := store.Get(runID)
		if err != nil {
			return nil, err
		}
		manifests = append(manifests, manifest)
	}
	sort.Slice(manifests, func(i, j int) bool {
		return manifests[i].CreatedAt.After(manifests[j].CreatedAt)
	})
	return manifests, nil
}

func (store Store) Transition(runID string, next State) (Manifest, error) {
	manifest, err := store.Get(runID)
	if err != nil {
		return Manifest{}, err
	}
	if !allowedTransition(manifest.State, next) {
		return Manifest{}, fmt.Errorf(
			"invalid run transition %q → %q",
			manifest.State,
			next,
		)
	}
	now := store.now().UTC()
	manifest.State = next
	manifest.LastError = ""
	manifest.UpdatedAt = now
	switch next {
	case StateStopped:
		manifest.StoppedAt = &now
	case StateImported:
		manifest.ImportedAt = &now
	case StateDiscarded:
		manifest.DiscardedAt = &now
	}
	path, err := store.manifestPath(runID)
	if err != nil {
		return Manifest{}, err
	}
	if err := store.writeAtomic(path, manifest, true); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func (store Store) Activate(runID string, connection SSHConnection) (Manifest, error) {
	manifest, err := store.Get(runID)
	if err != nil {
		return Manifest{}, err
	}
	if manifest.State != StateCreating {
		return Manifest{}, fmt.Errorf(
			"invalid run transition %q → %q",
			manifest.State,
			StateActive,
		)
	}
	if err := validateSSHConnection(manifest.RunID, connection); err != nil {
		return Manifest{}, err
	}
	now := store.now().UTC()
	manifest.State = StateActive
	manifest.SSH = &connection
	manifest.LastError = ""
	manifest.UpdatedAt = now
	path, err := store.manifestPath(runID)
	if err != nil {
		return Manifest{}, err
	}
	if err := store.writeAtomic(path, manifest, true); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

// RecordError preserves a failed operation without inventing a lifecycle
// state outside the design. A failed creation remains visibly "creating".
func (store Store) RecordError(runID string, operationErr error) (Manifest, error) {
	if operationErr == nil {
		return Manifest{}, fmt.Errorf("operation error is required")
	}
	manifest, err := store.Get(runID)
	if err != nil {
		return Manifest{}, err
	}
	manifest.LastError = operationErr.Error()
	manifest.UpdatedAt = store.now().UTC()
	path, err := store.manifestPath(runID)
	if err != nil {
		return Manifest{}, err
	}
	if err := store.writeAtomic(path, manifest, true); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func allowedTransition(current, next State) bool {
	switch current {
	case StateActive:
		return next == StateStopped
	case StateStopped:
		return next == StateActive ||
			next == StateImported ||
			next == StateDiscarded ||
			next == StateExpired
	default:
		return false
	}
}

func (store Store) ensureRoot() error {
	if err := os.MkdirAll(store.root, 0o700); err != nil {
		return fmt.Errorf("create run-state directory: %w", err)
	}
	info, err := os.Lstat(store.root)
	if err != nil {
		return fmt.Errorf("inspect run-state directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("run-state path is not a directory")
	}
	if info.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(store.root, 0o700); err != nil {
			return fmt.Errorf("restrict run-state directory: %w", err)
		}
	}
	return nil
}

func (store Store) manifestPath(runID string) (string, error) {
	if err := runid.Validate(runID); err != nil {
		return "", err
	}
	return filepath.Join(store.root, runID+".json"), nil
}

func (store Store) writeAtomic(path string, manifest Manifest, replace bool) error {
	content, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode run manifest: %w", err)
	}
	content = append(content, '\n')
	temporary, err := os.CreateTemp(store.root, ".manifest-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary run manifest: %w", err)
	}
	temporaryPath := temporary.Name()
	complete := false
	defer func() {
		temporary.Close()
		if !complete {
			os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("restrict temporary run manifest: %w", err)
	}
	if _, err := temporary.Write(content); err != nil {
		return fmt.Errorf("write run manifest: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync run manifest: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close run manifest: %w", err)
	}
	if replace {
		if err := os.Rename(temporaryPath, path); err != nil {
			return fmt.Errorf("replace run manifest: %w", err)
		}
	} else {
		// A hard link provides portable no-replace semantics; unlike a
		// preflight Lstat followed by Rename, concurrent creators cannot
		// overwrite one another.
		if err := os.Link(temporaryPath, path); err != nil {
			if errors.Is(err, fs.ErrExist) {
				return fmt.Errorf("run manifest already exists")
			}
			return fmt.Errorf("install run manifest: %w", err)
		}
		if err := os.Remove(temporaryPath); err != nil {
			return fmt.Errorf("remove temporary run manifest link: %w", err)
		}
	}
	complete = true
	directory, err := os.Open(store.root)
	if err != nil {
		return fmt.Errorf("open run-state directory: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync run-state directory: %w", err)
	}
	return nil
}

func validateManifestIdentity(manifest Manifest) error {
	if err := runid.Validate(manifest.RunID); err != nil {
		return err
	}
	if manifest.Snapshot.RunID != manifest.RunID {
		return fmt.Errorf("snapshot does not match run ID")
	}
	if manifest.Project == "" {
		return fmt.Errorf("project name is required")
	}
	return nil
}

func validateStoredManifest(manifest Manifest, expectedRunID string) error {
	if manifest.Version != manifestVersion {
		return fmt.Errorf("unsupported run manifest version %d", manifest.Version)
	}
	if manifest.RunID != expectedRunID {
		return fmt.Errorf("run manifest identity mismatch")
	}
	if err := validateManifestIdentity(manifest); err != nil {
		return err
	}
	if manifest.SSH != nil {
		if err := validateSSHConnection(manifest.RunID, *manifest.SSH); err != nil {
			return fmt.Errorf("invalid stored SSH connection: %w", err)
		}
	}
	switch manifest.State {
	case StateCreating:
		if manifest.SSH != nil {
			return fmt.Errorf("creating run cannot have an SSH connection")
		}
	case StateActive, StateStopped, StateImported, StateDiscarded, StateExpired:
		if manifest.SSH == nil {
			return fmt.Errorf("run state %q requires an SSH connection", manifest.State)
		}
	default:
		return fmt.Errorf("invalid stored run state %q", manifest.State)
	}
	return nil
}

func validateSSHConnection(runID string, connection SSHConnection) error {
	if connection.Alias != "pisafe-"+runID {
		return fmt.Errorf("SSH alias does not match run ID")
	}
	for name, path := range map[string]string{
		"identity":    connection.IdentityFile,
		"known-hosts": connection.KnownHostsFile,
		"config":      connection.ConfigFile,
	} {
		if !filepath.IsAbs(path) {
			return fmt.Errorf("%s path must be absolute", name)
		}
	}
	fingerprint := strings.TrimPrefix(connection.HostKeyFingerprint, "SHA256:")
	decoded, err := base64.RawStdEncoding.DecodeString(fingerprint)
	if err != nil || len(decoded) != 32 {
		return fmt.Errorf("invalid SSH host-key fingerprint")
	}
	return nil
}
