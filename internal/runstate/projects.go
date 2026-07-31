package runstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/mpizenberg/pisafe/internal/runid"
)

const projectRecordVersion = 1

// ProjectRecord attributes one project filesystem to the checkout that owns it.
// A project key is a one-way digest of the checkout path, so nothing stored on
// the VM can say which checkout a filesystem came from: without this record a
// project store could be created and never afterwards be recognised as unused.
//
// MissingSince is when a sweep first found the checkout gone. A checkout can
// come back — an unplugged disk, a mount that had not been made yet — and the
// store holds transcripts nothing can reproduce, so the stamp starts a window
// rather than authorising a removal.
type ProjectRecord struct {
	Version      int        `json:"version"`
	Key          string     `json:"key"`
	Root         string     `json:"root"`
	MissingSince *time.Time `json:"missing_since,omitempty"`
}

// describesItsCheckout is what stops a sweep acting on a record that has been
// corrupted or edited by hand. A key that does not follow from the root it is
// filed with names some other project, and reclaiming the wrong project's
// filesystem is the one mistake here that nothing can undo.
func (record ProjectRecord) describesItsCheckout() error {
	project, err := runid.NewProject(record.Root)
	if err != nil {
		return err
	}
	if project.Key != record.Key {
		return fmt.Errorf(
			"project record %q does not describe checkout %q", record.Key, record.Root,
		)
	}
	return nil
}

// RegisterProject records what a project filesystem belongs to. It is written
// every time a run reaches the project, which is what clears a stamp left by a
// checkout that has since come back.
func (store Store) RegisterProject(project runid.Project) error {
	if err := runid.Validate(project.Key); err != nil {
		return fmt.Errorf("invalid project key: %w", err)
	}
	if !filepath.IsAbs(project.Root) {
		return fmt.Errorf("project root %q is not absolute", project.Root)
	}
	if err := ensureDirectory(store.projectRoot()); err != nil {
		return err
	}
	path, err := store.projectPath(project.Key)
	if err != nil {
		return err
	}
	content, err := json.MarshalIndent(ProjectRecord{
		Version: projectRecordVersion,
		Key:     project.Key,
		Root:    project.Root,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode project record: %w", err)
	}
	return writeRecord(store.projectRoot(), path, append(content, '\n'), true)
}

func (store Store) ListProjects() ([]ProjectRecord, error) {
	entries, err := os.ReadDir(store.projectRoot())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list project records: %w", err)
	}
	records := make([]ProjectRecord, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		key := entry.Name()[:len(entry.Name())-len(".json")]
		record, err := store.getProject(key)
		if err != nil {
			// One unreadable record stops the listing, so it has to say which.
			return nil, fmt.Errorf("project %q: %w", key, err)
		}
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Key < records[j].Key })
	return records, nil
}

// HasProject reports whether a store is recorded under this key, without asking
// whether the record still describes something usable. A record corrupted by
// hand or by a rule that has since tightened must still be removable, or the
// only way out is deleting files by hand.
func (store Store) HasProject(key string) (bool, error) {
	path, err := store.projectPath(key)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read project record: %w", err)
	}
	return true, nil
}

// MarkProjectMissing starts one project's window. Recording the observation is
// what makes the window survive the process that made it, so a store is never
// removed on the strength of a single look at the filesystem.
func (store Store) MarkProjectMissing(key string, at time.Time) error {
	record, err := store.getProject(key)
	if err != nil {
		return err
	}
	at = at.UTC()
	record.MissingSince = &at
	path, err := store.projectPath(key)
	if err != nil {
		return err
	}
	content, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("encode project record: %w", err)
	}
	return writeRecord(store.projectRoot(), path, append(content, '\n'), true)
}

// ForgetProject drops the record of a project whose filesystem is gone. It is
// the last step of reclaiming one: while the record exists the store is still
// attributable, and a sweep that stops halfway simply finds it again.
func (store Store) ForgetProject(key string) error {
	path, err := store.projectPath(key)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove project record: %w", err)
	}
	return syncDirectory(store.projectRoot())
}

func (store Store) getProject(key string) (ProjectRecord, error) {
	path, err := store.projectPath(key)
	if err != nil {
		return ProjectRecord{}, err
	}
	content, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return ProjectRecord{}, fmt.Errorf("project %q is not recorded", key)
	}
	if err != nil {
		return ProjectRecord{}, fmt.Errorf("read project record: %w", err)
	}
	var record ProjectRecord
	if err := json.Unmarshal(content, &record); err != nil {
		return ProjectRecord{}, fmt.Errorf("decode project record: %w", err)
	}
	if record.Version != projectRecordVersion {
		return ProjectRecord{}, fmt.Errorf(
			"unsupported project record version %d", record.Version,
		)
	}
	if record.Key != key {
		return ProjectRecord{}, fmt.Errorf("project record identity mismatch")
	}
	if err := record.describesItsCheckout(); err != nil {
		return ProjectRecord{}, err
	}
	return record, nil
}

func (store Store) projectRoot() string {
	return filepath.Join(store.root, "projects")
}

func (store Store) projectPath(key string) (string, error) {
	if err := runid.Validate(key); err != nil {
		return "", fmt.Errorf("invalid project key: %w", err)
	}
	return filepath.Join(store.projectRoot(), key+".json"), nil
}
