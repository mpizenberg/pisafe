// Package backup is the shape an exported pisafe state takes on the Mac: the
// session transcripts no run reproduces, and the pins naming what the profile
// holds. A cache is left out because it is refetchable, and no provider
// credential is written at all — a key copied out of the Keychain into a
// directory would be the boundary the broker exists to prevent.
//
// A backup is a directory rather than one archive, so a transcript can be read
// out of it with the tools already on the Mac. Nothing here ever removes:
// backing up again into the same place adds what is new and keeps what is
// there, so a project store dropped since the last backup does not take the
// only remaining copy of its transcripts with it.
package backup

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"time"

	"github.com/mpizenberg/pisafe/internal/profile"
	"github.com/mpizenberg/pisafe/internal/runcopy"
	"github.com/mpizenberg/pisafe/internal/runid"
)

// Version is the shape of the manifest. A backup this pisafe does not
// understand is refused rather than guessed at, because what a restore does
// with one is install packages and write into project stores.
const Version = 1

const (
	ManifestFile      = "backup.json"
	ProjectsDirectory = "projects"

	// SessionsDirectory is where one project's transcripts sit in the backup,
	// and is the single name every archive is rooted at in either direction, so
	// nothing that crosses is ever addressed by a name the archive chose.
	SessionsDirectory = "sessions"
)

// maxManifestBytes bounds the manifest. It holds a few hundred bytes per
// installed package and per project, so anything near this is corruption.
const maxManifestBytes = 1 << 22

// transcriptPattern bounds what may cross as a transcript. The name is chosen
// inside a run and becomes a file name on the Mac, so it is held to what a
// transcript looks like rather than to what a filesystem would accept.
var transcriptPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}\.jsonl$`)

// Backup is everything an export records apart from the transcripts, which are
// files beside the manifest.
type Backup struct {
	CreatedAt  time.Time
	Extensions profile.Record
	Tools      profile.Tools
	Projects   []runid.Project
}

// manifest is the encoded form. The two profile records travel as they were
// written, so a restore reads them back through the parser an installed profile
// is read through rather than through a second one that could disagree with it.
type manifest struct {
	Version    int             `json:"version"`
	CreatedAt  time.Time       `json:"createdAt"`
	Extensions json.RawMessage `json:"extensions"`
	Tools      json.RawMessage `json:"tools"`
	Projects   []storedProject `json:"projects"`
}

// storedProject identifies one project store. A key is a one-way digest of the
// root, so recording both is what lets a restore check that a manifest
// describes the checkouts it claims to.
type storedProject struct {
	Key  string `json:"key"`
	Root string `json:"root"`
}

func (held Backup) Encode() ([]byte, error) {
	extensions, err := held.Extensions.Encode()
	if err != nil {
		return nil, err
	}
	tools, err := held.Tools.Encode()
	if err != nil {
		return nil, err
	}
	projects := make([]storedProject, 0, len(held.Projects))
	for _, project := range held.Projects {
		projects = append(projects, storedProject{Key: project.Key, Root: project.Root})
	}
	content, err := json.MarshalIndent(manifest{
		Version:    Version,
		CreatedAt:  held.CreatedAt.UTC(),
		Extensions: extensions,
		Tools:      tools,
		Projects:   projects,
	}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode backup manifest: %w", err)
	}
	return append(content, '\n'), nil
}

func Parse(content []byte) (Backup, error) {
	if len(content) > maxManifestBytes {
		return Backup{}, errors.New("backup manifest exceeds size limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	var stored manifest
	if err := decoder.Decode(&stored); err != nil {
		return Backup{}, fmt.Errorf("decode backup manifest: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Backup{}, errors.New("backup manifest contains trailing data")
	}
	if stored.Version != Version {
		return Backup{}, fmt.Errorf(
			"backup version %d is not %d; restore it with the pisafe that wrote it",
			stored.Version,
			Version,
		)
	}
	extensions, err := profile.ParseRecord(stored.Extensions)
	if err != nil {
		return Backup{}, err
	}
	tools, err := profile.ParseTools(stored.Tools)
	if err != nil {
		return Backup{}, err
	}
	projects := make([]runid.Project, 0, len(stored.Projects))
	recorded := make(map[string]bool, len(stored.Projects))
	for _, entry := range stored.Projects {
		project, err := runid.NewProject(entry.Root)
		if err != nil {
			return Backup{}, err
		}
		// A restore writes transcripts into whichever store the key resolves to,
		// so a manifest that disagrees with itself would file one checkout's
		// history under another's name.
		if project.Key != entry.Key {
			return Backup{}, fmt.Errorf(
				"backup entry %q does not describe checkout %q",
				entry.Key,
				entry.Root,
			)
		}
		if recorded[project.Key] {
			return Backup{}, fmt.Errorf("checkout %q is recorded twice", entry.Root)
		}
		recorded[project.Key] = true
		projects = append(projects, project)
	}
	return Backup{
		CreatedAt:  stored.CreatedAt,
		Extensions: extensions,
		Tools:      tools,
		Projects:   projects,
	}, nil
}

// Read loads what a backup directory records.
func Read(directory string) (Backup, error) {
	content, err := os.ReadFile(filepath.Join(directory, ManifestFile))
	if errors.Is(err, fs.ErrNotExist) {
		return Backup{}, fmt.Errorf("%q holds no pisafe backup", directory)
	}
	if err != nil {
		return Backup{}, fmt.Errorf("read backup manifest: %w", err)
	}
	return Parse(content)
}

// Write finishes a backup. The manifest is written after the transcripts it
// accounts for, so a directory an export did not finish filling holds no
// manifest at all and is refused rather than restored in part.
func Write(directory string, held Backup) error {
	content, err := held.Encode()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create backup directory: %w", err)
	}
	staging, err := os.CreateTemp(directory, "."+ManifestFile+".*")
	if err != nil {
		return fmt.Errorf("reserve backup manifest: %w", err)
	}
	defer os.Remove(staging.Name())
	if _, err := staging.Write(content); err != nil {
		staging.Close()
		return fmt.Errorf("write backup manifest: %w", err)
	}
	if err := staging.Chmod(0o600); err != nil {
		staging.Close()
		return fmt.Errorf("write backup manifest: %w", err)
	}
	if err := staging.Close(); err != nil {
		return fmt.Errorf("write backup manifest: %w", err)
	}
	if err := os.Rename(staging.Name(), filepath.Join(directory, ManifestFile)); err != nil {
		return fmt.Errorf("write backup manifest: %w", err)
	}
	return nil
}

// ProjectDirectory is where one project's part of the backup lives.
func ProjectDirectory(directory, key string) string {
	return filepath.Join(directory, ProjectsDirectory, key)
}

// IsTranscript reports whether a name may cross as a session transcript.
func IsTranscript(name string) bool {
	return transcriptPattern.MatchString(name)
}

// AddSessions puts the transcripts an archive carries into one project's place
// in the backup, keeping every transcript already there. It reports how many
// arrived and how many were refused: a transcript's name is chosen inside a
// run, so one the Mac will not write must not stop the rest being backed up.
func AddSessions(archive io.Reader, directory, key string) (int, int, error) {
	if err := runid.Validate(key); err != nil {
		return 0, 0, fmt.Errorf("invalid project key: %w", err)
	}
	project := ProjectDirectory(directory, key)
	sessions := filepath.Join(project, SessionsDirectory)
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		return 0, 0, fmt.Errorf("create backup directory: %w", err)
	}
	staging, err := os.MkdirTemp(project, ".pisafe-backup-*")
	if err != nil {
		return 0, 0, fmt.Errorf("reserve backup staging directory: %w", err)
	}
	defer os.RemoveAll(staging)

	root, err := os.OpenRoot(staging)
	if err != nil {
		return 0, 0, fmt.Errorf("open backup staging directory: %w", err)
	}
	defer root.Close()
	entries, err := runcopy.ExtractInto(archive, root, SessionsDirectory)
	if err != nil {
		return 0, 0, err
	}

	added, refused := 0, 0
	for _, entry := range entries {
		if entry.Directory {
			continue
		}
		name := path.Base(entry.Path)
		if path.Dir(entry.Path) != SessionsDirectory || !IsTranscript(name) {
			refused++
			continue
		}
		target := filepath.Join(sessions, name)
		// A name the backup already holds is a transcript Pi rewrote in place
		// when it migrated the session on load, and the copy taken first is the
		// one kept, exactly as the session store keeps its own.
		if _, err := os.Lstat(target); err == nil {
			continue
		}
		if err := os.Rename(filepath.Join(staging, SessionsDirectory, name), target); err != nil {
			return 0, 0, fmt.Errorf("add transcript to backup: %w", err)
		}
		added++
	}
	return added, refused, nil
}

// Sessions reports the transcripts the backup holds for one project. It is
// deliberately strict about what it finds: a backup is a directory on the Mac,
// and what a restore sends into a project store must be what an export wrote.
func Sessions(directory, key string) ([]string, error) {
	sessions := filepath.Join(ProjectDirectory(directory, key), SessionsDirectory)
	entries, err := os.ReadDir(sessions)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read backup transcripts: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !IsTranscript(entry.Name()) {
			return nil, fmt.Errorf(
				"the backup of %q holds %q, which is not a session transcript",
				key,
				entry.Name(),
			)
		}
		names = append(names, entry.Name())
	}
	return names, nil
}

// ArchiveSessions writes what the backup holds for one project as a tar for the
// VM to add to that project's session store.
func ArchiveSessions(directory, key string, out io.Writer) error {
	if _, err := Sessions(directory, key); err != nil {
		return err
	}
	return runcopy.Archive(ProjectDirectory(directory, key), SessionsDirectory, out)
}
