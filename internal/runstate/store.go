// Package runstate persists the Mac-side lifecycle record for runs.
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
	"github.com/mpizenberg/pisafe/internal/runcontainer"
	"github.com/mpizenberg/pisafe/internal/runid"
)

const manifestVersion = 6

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

// A run has a record for exactly as long as it owns something. Reclaiming it
// removes the record with the resources, so there is no terminal state: what an
// imported run produced survives as the pisafe/<run> branch in the user's own
// repository, which needs no record to stay attributable.
const (
	StateCreating State = "creating"
	StateActive   State = "active"
	StateStopped  State = "stopped"
	StateImported State = "imported"
)

type Manifest struct {
	Version             int               `json:"version"`
	RunID               string            `json:"run_id"`
	Project             string            `json:"project"`
	ProjectKey          string            `json:"project_key"`
	State               State             `json:"state"`
	Snapshot            gitstage.Snapshot `json:"snapshot"`
	Image               string            `json:"image,omitempty"`
	SSH                 *SSHConnection    `json:"ssh,omitempty"`
	InferenceCapability string            `json:"inference_capability,omitempty"`
	// Caches records which snapshot each declared cache was resolved to. A
	// resume must stack the run's existing upper back onto the same lower it
	// recorded its whiteouts against, so the selection is made once and kept
	// rather than made again from a project store that has moved on.
	Caches []runcontainer.CacheMount `json:"caches,omitempty"`
	// Apply is the plan of an import that has been verified but whose refs
	// may not all have moved yet. It exists only between BeginApply and
	// CompleteApply, and is what makes an interrupted apply replayable.
	Apply                *gitstage.PlannedApply `json:"apply,omitempty"`
	ActiveLimitSeconds   int64                  `json:"active_limit_seconds"`
	ActiveElapsedSeconds int64                  `json:"active_elapsed_seconds"`
	ActiveStartedAt      *time.Time             `json:"active_started_at,omitempty"`
	ActiveDeadline       *time.Time             `json:"active_deadline,omitempty"`
	CreatedAt            time.Time              `json:"created_at"`
	UpdatedAt            time.Time              `json:"updated_at"`
	StoppedAt            *time.Time             `json:"stopped_at,omitempty"`
	ImportedAt           *time.Time             `json:"imported_at,omitempty"`
	ImportedBranch       string                 `json:"imported_branch,omitempty"`
	LastError            string                 `json:"last_error,omitempty"`
}

// Workspace is where the run's checkout is inside its container. It follows
// from the project name, which is validated, so nothing a stored record says
// can send a shell somewhere else.
func (manifest Manifest) Workspace() string {
	return runcontainer.ContainerWorkRoot + "/" + manifest.Project
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
		// Everything filed under the state root is reached by absolute path, so
		// the root is made one here rather than by each store that takes it.
		absolute, err := filepath.Abs(override)
		if err != nil {
			return "", fmt.Errorf("resolve PISAFE_STATE_DIR: %w", err)
		}
		return absolute, nil
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
			// One unreadable record stops the listing, so it has to say which.
			return nil, fmt.Errorf("run %q: %w", runID, err)
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
	if err := requireActive(manifest); err != nil {
		return Manifest{}, err
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
	return store.replace(endActiveStretch(manifest, now, endedAt, elapsedSeconds))
}

// Abandon records a run whose container went out from under it. Rebooting or
// recreating the VM keeps every run's storage and leaves none of its
// containers, so the record saying active is the stale half of the
// disagreement. The stretch costs the run nothing: the container carried the
// only account of how much of it was spent and went with the VM, and charging
// the wall clock instead would spend a whole budget on an outage the run did
// not cause. Nothing inside a run can bring its own container down, so this is
// not a budget agent code can extend.
func (store Store) Abandon(runID string) (Manifest, error) {
	manifest, err := store.Get(runID)
	if err != nil {
		return Manifest{}, err
	}
	if err := requireActive(manifest); err != nil {
		return Manifest{}, err
	}
	now := store.now().UTC()
	return store.replace(endActiveStretch(manifest, now, now, 0))
}

func requireActive(manifest Manifest) error {
	if manifest.State != StateActive {
		return fmt.Errorf(
			"invalid run transition %q → %q",
			manifest.State,
			StateStopped,
		)
	}
	return nil
}

// endActiveStretch closes the stretch a run spent active and charges
// elapsedSeconds of its budget for it. Every route out of StateActive comes
// through here, and they differ only in what the stretch cost.
func endActiveStretch(
	manifest Manifest,
	now time.Time,
	endedAt time.Time,
	elapsedSeconds int64,
) Manifest {
	manifest.ActiveElapsedSeconds += elapsedSeconds
	manifest.ActiveStartedAt = nil
	manifest.ActiveDeadline = nil
	manifest.InferenceCapability = ""
	manifest.State = StateStopped
	manifest.LastError = ""
	manifest.UpdatedAt = now
	manifest.StoppedAt = &endedAt
	return manifest
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

// Forget removes a run's record once everything it owned has been reclaimed.
// An active run is refused: its record is the only route back to a container
// that is still running.
func (store Store) Forget(runID string) error {
	manifest, err := store.Get(runID)
	if err != nil {
		return err
	}
	if manifest.State == StateActive {
		return fmt.Errorf("run %q is active and must be stopped first", runID)
	}
	path, err := store.manifestPath(runID)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove run manifest: %w", err)
	}
	return syncDirectory(store.root)
}

// BeginApply records a verified import plan before any user-visible ref moves.
// Every object the plan names is already in the local repositories, so the
// recorded journal is enough to finish or inspect an interrupted apply.
func (store Store) BeginApply(runID string, planned gitstage.PlannedApply) (Manifest, error) {
	manifest, err := store.Get(runID)
	if err != nil {
		return Manifest{}, err
	}
	if manifest.State != StateStopped {
		return Manifest{}, fmt.Errorf("run %q is %s, not stopped", runID, manifest.State)
	}
	if manifest.Apply != nil {
		return Manifest{}, fmt.Errorf("run %q already has an apply in progress", runID)
	}
	if err := validateApplyPlan(runID, planned); err != nil {
		return Manifest{}, err
	}
	manifest.Apply = &planned
	manifest.LastError = ""
	manifest.UpdatedAt = store.now().UTC()
	return store.replace(manifest)
}

// CompleteApply marks a run imported. Callers reach it only once every ref in
// the journal holds the commit the journal recorded.
func (store Store) CompleteApply(runID string) (Manifest, error) {
	manifest, err := store.Get(runID)
	if err != nil {
		return Manifest{}, err
	}
	if manifest.State != StateStopped {
		return Manifest{}, fmt.Errorf(
			"invalid run transition %q → %q",
			manifest.State,
			StateImported,
		)
	}
	if manifest.Apply == nil {
		return Manifest{}, fmt.Errorf("run %q has no apply in progress", runID)
	}
	now := store.now().UTC()
	manifest.State = StateImported
	manifest.ImportedBranch = manifest.Apply.Result.Branch
	manifest.ImportedAt = &now
	manifest.Apply = nil
	manifest.LastError = ""
	manifest.UpdatedAt = now
	return store.replace(manifest)
}

// Activate records the run as materialization actually produced it. Only the
// baselines are taken from the materialized snapshot: every other field was
// settled on the Mac before the run existed, and the run does not get to
// restate it.
func (store Store) Activate(
	runID string,
	connection SSHConnection,
	materialized gitstage.Snapshot,
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
	baselines, err := materializedBaselines(manifest.Snapshot, materialized)
	if err != nil {
		return Manifest{}, err
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
	manifest.Snapshot.BaselineCommit = materialized.BaselineCommit
	manifest.Snapshot.Submodules = baselines
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

// materializedBaselines returns the staged submodules carrying the baseline
// commit each one actually got. Materialization may only fill those in: a
// snapshot that renames a submodule or moves its head describes a different run.
func materializedBaselines(
	staged, materialized gitstage.Snapshot,
) ([]gitstage.SubmoduleStage, error) {
	if err := validateBaseline(materialized.BaselineCommit); err != nil {
		return nil, err
	}
	if len(materialized.Submodules) != len(staged.Submodules) {
		return nil, fmt.Errorf("materialized snapshot does not match the staged submodules")
	}
	recorded := append([]gitstage.SubmoduleStage(nil), staged.Submodules...)
	for index := range recorded {
		if materialized.Submodules[index].Path != recorded[index].Path ||
			materialized.Submodules[index].Head != recorded[index].Head {
			return nil, fmt.Errorf("materialized snapshot does not match the staged submodules")
		}
		if err := validateBaseline(materialized.Submodules[index].BaselineCommit); err != nil {
			return nil, err
		}
		recorded[index].BaselineCommit = materialized.Submodules[index].BaselineCommit
	}
	return recorded, nil
}

func validateBaseline(commit string) error {
	if commit != "" && !gitObjectPattern.MatchString(commit) {
		return fmt.Errorf("invalid materialized baseline commit")
	}
	return nil
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
	return ensureDirectory(store.root)
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
	return writeRecord(store.root, path, append(content, '\n'), replace)
}

func ensureDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect state directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("state path %q is not a directory", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(path, 0o700); err != nil {
			return fmt.Errorf("restrict state directory: %w", err)
		}
	}
	return nil
}

// writeRecord installs one durable record in directory, replacing what is there
// only when asked to. Nothing partial is ever visible under the record's own
// name, and the write is durable once it returns.
func writeRecord(directory, path string, content []byte, replace bool) error {
	temporary, err := os.CreateTemp(directory, ".record-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary record: %w", err)
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
		return fmt.Errorf("restrict temporary record: %w", err)
	}
	if _, err := temporary.Write(content); err != nil {
		return fmt.Errorf("write record: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync record: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close record: %w", err)
	}
	if replace {
		if err := os.Rename(temporaryPath, path); err != nil {
			return fmt.Errorf("replace record: %w", err)
		}
	} else {
		// A hard link provides portable no-replace semantics; unlike a
		// preflight Lstat followed by Rename, concurrent creators cannot
		// overwrite one another.
		if err := os.Link(temporaryPath, path); err != nil {
			if errors.Is(err, fs.ErrExist) {
				return fmt.Errorf("record %q already exists", filepath.Base(path))
			}
			return fmt.Errorf("install record: %w", err)
		}
		if err := os.Remove(temporaryPath); err != nil {
			return fmt.Errorf("remove temporary record link: %w", err)
		}
	}
	complete = true
	return syncDirectory(directory)
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open state directory: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync state directory: %w", err)
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
	if err := runid.Validate(manifest.Project); err != nil {
		return fmt.Errorf("invalid project name: %w", err)
	}
	if err := runid.Validate(manifest.ProjectKey); err != nil {
		return fmt.Errorf("invalid project key: %w", err)
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
	if manifest.Apply != nil {
		if manifest.State != StateStopped {
			return fmt.Errorf("run state %q cannot hold an apply in progress", manifest.State)
		}
		if err := validateApplyPlan(manifest.RunID, *manifest.Apply); err != nil {
			return fmt.Errorf("invalid stored apply plan: %w", err)
		}
	}
	if manifest.State == StateImported &&
		(manifest.ImportedBranch == "" || manifest.ImportedAt == nil) {
		return fmt.Errorf("imported run does not record its branch")
	}
	switch manifest.State {
	case StateCreating:
		if manifest.SSH != nil {
			return fmt.Errorf("creating run cannot have an SSH connection")
		}
		if manifest.ActiveStartedAt != nil || manifest.ActiveDeadline != nil {
			return fmt.Errorf("inactive run retains active wall-clock timestamps")
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
	case StateStopped, StateImported:
		if manifest.SSH == nil {
			return fmt.Errorf("run state %q requires an SSH connection", manifest.State)
		}
		if manifest.ActiveStartedAt != nil || manifest.ActiveDeadline != nil {
			return fmt.Errorf("inactive run retains active wall-clock timestamps")
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

// validateApplyPlan bounds what a stored journal can later ask Git to do. It
// runs on the way in and on the way out, so a tampered manifest cannot move a
// ref the run never earned.
func validateApplyPlan(runID string, planned gitstage.PlannedApply) error {
	branch := "pisafe/" + runID
	if planned.Journal.RunID != runID || planned.Result.Branch != branch {
		return fmt.Errorf("apply plan does not match run %q", runID)
	}
	if len(planned.Journal.Steps) == 0 {
		return fmt.Errorf("apply plan has no steps")
	}
	for _, step := range planned.Journal.Steps {
		if !filepath.IsAbs(step.Repository) {
			return fmt.Errorf("apply step repository must be absolute")
		}
		if !gitObjectPattern.MatchString(step.Commit) {
			return fmt.Errorf("apply step names an invalid commit")
		}
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
