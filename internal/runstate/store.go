// Package runstate persists the Mac-side audit and lifecycle record for runs.
package runstate

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/mpizenberg/pisafe/internal/gitstage"
	"github.com/mpizenberg/pisafe/internal/runid"
)

const manifestVersion = 4

var (
	gitObjectPattern  = regexp.MustCompile(`^(?:[a-f0-9]{40}|[a-f0-9]{64})$`)
	capabilityPattern = regexp.MustCompile(`^pisafe-cap-[a-f0-9]{64}$`)
)

// NewInferenceCapability creates the revocable secret that lets one active
// run consume brokered inference. It is the only credential pisafe ever hands
// to sandboxed code.
func NewInferenceCapability() (string, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", fmt.Errorf("generate inference capability: %w", err)
	}
	return "pisafe-cap-" + hex.EncodeToString(secret), nil
}

func ValidInferenceCapability(capability string) bool {
	return capabilityPattern.MatchString(capability)
}

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
	Version              int               `json:"version"`
	RunID                string            `json:"run_id"`
	Project              string            `json:"project"`
	State                State             `json:"state"`
	Snapshot             gitstage.Snapshot `json:"snapshot"`
	Image                string            `json:"image,omitempty"`
	Container            string            `json:"container,omitempty"`
	Workspace            string            `json:"workspace,omitempty"`
	SSH                  *SSHConnection    `json:"ssh,omitempty"`
	InferenceCapability  string            `json:"inference_capability,omitempty"`
	ActiveLimitSeconds   int64             `json:"active_limit_seconds"`
	ActiveElapsedSeconds int64             `json:"active_elapsed_seconds"`
	ActiveStartedAt      *time.Time        `json:"active_started_at,omitempty"`
	ActiveDeadline       *time.Time        `json:"active_deadline,omitempty"`
	CreatedAt            time.Time         `json:"created_at"`
	UpdatedAt            time.Time         `json:"updated_at"`
	StoppedAt            *time.Time        `json:"stopped_at,omitempty"`
	ImportedAt           *time.Time        `json:"imported_at,omitempty"`
	DiscardedAt          *time.Time        `json:"discarded_at,omitempty"`
	ImportedBranch       string            `json:"imported_branch,omitempty"`
	LastError            string            `json:"last_error,omitempty"`
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

func (store Store) Stop(runID string, endedAt time.Time) (Manifest, error) {
	manifest, err := store.Get(runID)
	if err != nil {
		return Manifest{}, err
	}
	if manifest.State != StateActive {
		return Manifest{}, fmt.Errorf(
			"invalid run transition %q → %q",
			manifest.State,
			StateStopped,
		)
	}
	now := store.now().UTC()
	endedAt = endedAt.UTC()
	if endedAt.IsZero() {
		endedAt = now
	}
	if endedAt.After(now.Add(5 * time.Second)) {
		return Manifest{}, fmt.Errorf("container stop time is in the future")
	}
	if endedAt.After(now) {
		endedAt = now
	}
	if manifest.ActiveStartedAt == nil || endedAt.Before(*manifest.ActiveStartedAt) {
		return Manifest{}, fmt.Errorf("container stop time precedes activation")
	}
	elapsed := endedAt.Sub(*manifest.ActiveStartedAt)
	elapsedSeconds := int64((elapsed + time.Second - 1) / time.Second)
	remaining := manifest.ActiveLimitSeconds - manifest.ActiveElapsedSeconds
	if elapsedSeconds > remaining {
		elapsedSeconds = remaining
	}
	manifest.ActiveElapsedSeconds += elapsedSeconds
	manifest.ActiveStartedAt = nil
	manifest.ActiveDeadline = nil
	manifest.InferenceCapability = ""
	manifest.State = StateStopped
	manifest.LastError = ""
	manifest.UpdatedAt = now
	manifest.StoppedAt = &endedAt
	return store.replace(manifest)
}

func (store Store) Resume(runID string, capability string, startedAt time.Time) (Manifest, error) {
	manifest, err := store.Get(runID)
	if err != nil {
		return Manifest{}, err
	}
	if manifest.State != StateStopped {
		return Manifest{}, fmt.Errorf(
			"invalid run transition %q → %q",
			manifest.State,
			StateActive,
		)
	}
	remaining := manifest.ActiveLimitSeconds - manifest.ActiveElapsedSeconds
	if remaining <= 0 {
		return Manifest{}, fmt.Errorf("run %q exhausted its active wall-clock limit", runID)
	}
	if !ValidInferenceCapability(capability) {
		return Manifest{}, fmt.Errorf("resume requires a fresh inference capability")
	}
	now := store.now().UTC()
	startedAt = startedAt.UTC()
	if startedAt.IsZero() {
		startedAt = now
	}
	if startedAt.After(now.Add(5 * time.Second)) {
		return Manifest{}, fmt.Errorf("container start time is in the future")
	}
	if startedAt.After(now) {
		startedAt = now
	}
	deadline := startedAt.Add(time.Duration(remaining) * time.Second)
	manifest.State = StateActive
	manifest.ActiveStartedAt = &startedAt
	manifest.ActiveDeadline = &deadline
	manifest.InferenceCapability = capability
	manifest.LastError = ""
	manifest.UpdatedAt = now
	return store.replace(manifest)
}

func (store Store) Discard(runID string) (Manifest, error) {
	manifest, err := store.Get(runID)
	if err != nil {
		return Manifest{}, err
	}
	if manifest.State != StateCreating && manifest.State != StateStopped {
		return Manifest{}, fmt.Errorf(
			"invalid run transition %q → %q",
			manifest.State,
			StateDiscarded,
		)
	}
	now := store.now().UTC()
	manifest.State = StateDiscarded
	manifest.ActiveStartedAt = nil
	manifest.ActiveDeadline = nil
	manifest.InferenceCapability = ""
	manifest.LastError = ""
	manifest.UpdatedAt = now
	manifest.DiscardedAt = &now
	return store.replace(manifest)
}

func (store Store) Activate(
	runID string,
	connection SSHConnection,
	baselineCommit string,
	capability string,
	startedAt time.Time,
) (Manifest, error) {
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
	if baselineCommit != "" && !gitObjectPattern.MatchString(baselineCommit) {
		return Manifest{}, fmt.Errorf("invalid materialized baseline commit")
	}
	if !ValidInferenceCapability(capability) {
		return Manifest{}, fmt.Errorf("activation requires an inference capability")
	}
	now := store.now().UTC()
	if manifest.ActiveLimitSeconds <= 0 {
		return Manifest{}, fmt.Errorf("active wall-clock limit is required")
	}
	startedAt = startedAt.UTC()
	if startedAt.IsZero() {
		startedAt = now
	}
	if startedAt.After(now.Add(5 * time.Second)) {
		return Manifest{}, fmt.Errorf("container start time is in the future")
	}
	if startedAt.After(now) {
		startedAt = now
	}
	deadline := startedAt.Add(time.Duration(manifest.ActiveLimitSeconds) * time.Second)
	manifest.State = StateActive
	manifest.SSH = &connection
	manifest.Snapshot.BaselineCommit = baselineCommit
	manifest.ActiveStartedAt = &startedAt
	manifest.ActiveDeadline = &deadline
	manifest.InferenceCapability = capability
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

func (store Store) replace(manifest Manifest) (Manifest, error) {
	path, err := store.manifestPath(manifest.RunID)
	if err != nil {
		return Manifest{}, err
	}
	if err := store.writeAtomic(path, manifest, true); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
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
	if manifest.ActiveLimitSeconds <= 0 {
		return fmt.Errorf("active wall-clock limit is required")
	}
	if manifest.ActiveElapsedSeconds < 0 ||
		manifest.ActiveElapsedSeconds > manifest.ActiveLimitSeconds {
		return fmt.Errorf("invalid active wall-clock usage")
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
	if manifest.State != StateActive && manifest.InferenceCapability != "" {
		return fmt.Errorf("inactive run retains an inference capability")
	}
	switch manifest.State {
	case StateCreating:
		if manifest.SSH != nil ||
			manifest.ActiveStartedAt != nil ||
			manifest.ActiveDeadline != nil {
			return fmt.Errorf("creating run cannot have an SSH connection")
		}
	case StateActive:
		if manifest.SSH == nil {
			return fmt.Errorf("run state %q requires an SSH connection", manifest.State)
		}
		if !ValidInferenceCapability(manifest.InferenceCapability) {
			return fmt.Errorf("active run requires an inference capability")
		}
		if manifest.ActiveStartedAt == nil || manifest.ActiveDeadline == nil {
			return fmt.Errorf("active run requires wall-clock timestamps")
		}
		remaining := manifest.ActiveLimitSeconds - manifest.ActiveElapsedSeconds
		expected := manifest.ActiveStartedAt.Add(time.Duration(remaining) * time.Second)
		if !manifest.ActiveDeadline.Equal(expected) {
			return fmt.Errorf("active run has an inconsistent wall-clock deadline")
		}
	case StateStopped, StateImported, StateExpired:
		if manifest.SSH == nil {
			return fmt.Errorf("run state %q requires an SSH connection", manifest.State)
		}
		if manifest.ActiveStartedAt != nil || manifest.ActiveDeadline != nil {
			return fmt.Errorf("inactive run retains active wall-clock timestamps")
		}
	case StateDiscarded:
		if manifest.ActiveStartedAt != nil || manifest.ActiveDeadline != nil {
			return fmt.Errorf("discarded run retains active wall-clock timestamps")
		}
	default:
		return fmt.Errorf("invalid stored run state %q", manifest.State)
	}
	return nil
}

func RemainingSeconds(manifest Manifest, now time.Time) int64 {
	remaining := manifest.ActiveLimitSeconds - manifest.ActiveElapsedSeconds
	if remaining <= 0 {
		return 0
	}
	if manifest.State != StateActive || manifest.ActiveDeadline == nil {
		return remaining
	}
	duration := manifest.ActiveDeadline.Sub(now.UTC())
	if duration <= 0 {
		return 0
	}
	seconds := int64((duration + time.Second - 1) / time.Second)
	if seconds > remaining {
		return remaining
	}
	return seconds
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
