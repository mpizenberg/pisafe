// Package safefile reads and writes the small files pisafe keeps: run records,
// project records, SSH configuration, backup manifests, the VM definition, and
// what a run's agent is configured with. Each of them has to be bounded on the
// way in, whole on the way out, and never something other than a regular file,
// so each of those is answered once here rather than at every store that keeps
// one.
package safefile

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// Read returns the content of a bounded regular file. The path is inspected
// before it is opened and the handle after, and the two must be the same file,
// so a path that becomes something else between those moments is refused rather
// than read.
func Read(path string, limit int64) ([]byte, error) {
	path = filepath.Clean(path)
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Size() > limit {
		return nil, errors.New("file is not a bounded regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() || opened.Size() > limit || !os.SameFile(before, opened) {
		return nil, errors.New("file is not a bounded regular file")
	}
	content, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(content)) > limit {
		return nil, errors.New("file exceeds its size limit")
	}
	return content, nil
}

// Create installs content at path and fails if anything is already there.
func Create(path string, content []byte, mode fs.FileMode) error {
	return install(path, content, mode, false)
}

// Replace installs content at path over whatever it finds.
func Replace(path string, content []byte, mode fs.FileMode) error {
	return install(path, content, mode, true)
}

// install writes through a temporary file in the same directory, so nothing
// partial is ever visible under the file's own name and the content is on disk
// once this returns.
func install(path string, content []byte, mode fs.FileMode, replace bool) error {
	name := filepath.Base(path)
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".pisafe-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary file for %s: %w", name, err)
	}
	temporaryPath := temporary.Name()
	complete := false
	defer func() {
		temporary.Close()
		if !complete {
			os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(mode); err != nil {
		return fmt.Errorf("restrict %s: %w", name, err)
	}
	if _, err := temporary.Write(content); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", name, err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close %s: %w", name, err)
	}
	if replace {
		if err := os.Rename(temporaryPath, path); err != nil {
			return fmt.Errorf("replace %s: %w", name, err)
		}
	} else {
		// A hard link provides portable no-replace semantics; unlike a
		// preflight Lstat followed by Rename, concurrent creators cannot
		// overwrite one another.
		if err := os.Link(temporaryPath, path); err != nil {
			if errors.Is(err, fs.ErrExist) {
				return fmt.Errorf("%s already exists", name)
			}
			return fmt.Errorf("install %s: %w", name, err)
		}
		if err := os.Remove(temporaryPath); err != nil {
			return fmt.Errorf("remove temporary link for %s: %w", name, err)
		}
	}
	complete = true
	return SyncDirectory(directory)
}

// SyncDirectory makes a name as durable as the content it points at, which a
// removal needs as much as an arrival does.
func SyncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open directory to sync: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync directory: %w", err)
	}
	return nil
}
