// Package runcopy moves files across the boundary as an archive: out of an
// isolated run or a project store, and back in again.
//
// The far side produces the archive coming out, so nothing it says is trusted:
// the Mac re-validates every entry and writes only through a directory handle
// it opened itself, which is why a destination cannot be swapped for a symlink
// while the copy is in flight. An archive going the other way is held to the
// same limits, so what leaves the Mac is what the Mac would have accepted.
package runcopy

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const (
	// MaxFiles, MaxTotalBytes, and MaxFileBytes bound one copy. A run holds ten
	// gigabytes and can write whatever it likes there, so a copy that outgrows
	// these stops instead of filling the Mac's disk.
	MaxFiles      = 4096
	MaxTotalBytes = int64(1) << 30
	MaxFileBytes  = int64(256) << 20

	// headerAllowance leaves room for the archive's own metadata on top of the
	// content limit, so a legal copy is never cut short by its headers.
	headerAllowance = int64(MaxFiles) * 4096
)

// Entry is one path a copy delivered.
type Entry struct {
	Path      string
	Size      int64
	Directory bool
}

// SafePath resolves what the user asked to copy into a run-relative path. A
// path that is absolute, climbs out, or names the whole workspace is refused
// before it reaches the run.
func SafePath(request string) (string, error) {
	if request == "" {
		return "", errors.New("a path inside the run is required")
	}
	slashed := filepath.ToSlash(request)
	if path.IsAbs(slashed) || filepath.IsAbs(request) {
		return "", fmt.Errorf("%q is absolute; name a path inside the run's workspace", request)
	}
	cleaned := path.Clean(slashed)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("%q leaves the run's workspace", request)
	}
	if cleaned == "." {
		return "", fmt.Errorf("%q names the whole workspace; name what you want copied", request)
	}
	return cleaned, nil
}

// Archive writes one path of the run's workspace as a tar. Only regular files
// and directories cross: a symlink, device, socket, or named pipe stops the
// copy naming the path, because on the Mac they would resolve against a
// filesystem the run never saw.
func Archive(workspace, request string, out io.Writer) error {
	name, err := SafePath(request)
	if err != nil {
		return err
	}
	base := path.Base(name)
	source := filepath.Join(filepath.Clean(workspace), filepath.FromSlash(name))
	writer := tar.NewWriter(out)
	files := 0
	total := int64(0)
	walkErr := filepath.WalkDir(source, func(current string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		archiveName, err := archiveNameFor(source, current, base)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("inspect %q: %w", archiveName, err)
		}
		switch {
		case entry.IsDir():
			files++
			if files > MaxFiles {
				return fmt.Errorf("%q holds more than %d entries", request, MaxFiles)
			}
			return writer.WriteHeader(&tar.Header{
				Name:     archiveName + "/",
				Typeflag: tar.TypeDir,
				Mode:     0o700,
			})
		case info.Mode().IsRegular():
			files++
			if files > MaxFiles {
				return fmt.Errorf("%q holds more than %d entries", request, MaxFiles)
			}
			if info.Size() > MaxFileBytes {
				return fmt.Errorf(
					"%q is %d bytes, over the %d-byte per-file limit",
					archiveName,
					info.Size(),
					MaxFileBytes,
				)
			}
			total += info.Size()
			if total > MaxTotalBytes {
				return fmt.Errorf("%q exceeds the %d-byte copy limit", request, MaxTotalBytes)
			}
			mode := int64(0o600)
			if info.Mode().Perm()&0o100 != 0 {
				mode = 0o700
			}
			if err := writer.WriteHeader(&tar.Header{
				Name:     archiveName,
				Typeflag: tar.TypeReg,
				Mode:     mode,
				Size:     info.Size(),
			}); err != nil {
				return err
			}
			return copyExactly(writer, current, info.Size())
		default:
			return fmt.Errorf(
				"%q is not a regular file or directory, so it cannot be copied out",
				archiveName,
			)
		}
	})
	if walkErr != nil {
		return walkErr
	}
	return writer.Close()
}

func archiveNameFor(source, current, base string) (string, error) {
	relative, err := filepath.Rel(source, current)
	if err != nil {
		return "", err
	}
	if relative == "." {
		return base, nil
	}
	return base + "/" + filepath.ToSlash(relative), nil
}

// copyExactly writes the size the header promised, so a file the run rewrites
// mid-copy fails the copy instead of corrupting the archive.
func copyExactly(writer io.Writer, sourcePath string, size int64) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer source.Close()

	copied, err := io.Copy(writer, io.LimitReader(source, size))
	if err != nil {
		return err
	}
	if copied != size {
		return fmt.Errorf("%q changed size while being copied", sourcePath)
	}
	return nil
}

// CheckDestination reports whether a copy could land at destination. The
// controller calls it before starting the run, so a copy that cannot land
// costs nothing and says why instead of failing behind a broken stream.
func CheckDestination(destination string, replace bool) error {
	target := filepath.Clean(destination)
	name := filepath.Base(target)
	if name == "." || name == ".." || name == string(filepath.Separator) {
		return fmt.Errorf("%q is not a usable destination", destination)
	}
	switch _, err := os.Lstat(target); {
	case err == nil && !replace:
		return fmt.Errorf("%q already exists; pass --force to replace it", destination)
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("inspect destination: %w", err)
	}
	return nil
}

// CopyTo unpacks what the run sent to destination. Everything lands in a
// staging directory beside it first, so a copy that is refused part-way leaves
// the destination exactly as it was.
func CopyTo(reader io.Reader, base, destination string, replace bool) ([]Entry, error) {
	if err := CheckDestination(destination, replace); err != nil {
		return nil, err
	}
	target := filepath.Clean(destination)
	staging, err := os.MkdirTemp(filepath.Dir(target), ".pisafe-cp-*")
	if err != nil {
		return nil, fmt.Errorf("reserve copy staging directory: %w", err)
	}
	complete := false
	defer func() {
		if !complete {
			os.RemoveAll(staging)
		}
	}()

	root, err := os.OpenRoot(staging)
	if err != nil {
		return nil, fmt.Errorf("open copy staging directory: %w", err)
	}
	defer root.Close()
	entries, err := ExtractInto(reader, root, base)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, errors.New("the run sent nothing to copy")
	}

	// Replacing happens only once the whole copy has arrived and been checked.
	// An existing symlink is removed rather than written through.
	if replace {
		if err := os.RemoveAll(target); err != nil {
			return nil, fmt.Errorf("replace destination: %w", err)
		}
	}
	if err := os.Rename(filepath.Join(staging, base), target); err != nil {
		return nil, fmt.Errorf("move copy into place: %w", err)
	}
	if err := os.RemoveAll(staging); err != nil {
		return nil, fmt.Errorf("remove copy staging directory: %w", err)
	}
	complete = true
	return entries, nil
}

// ExtractInto unpacks an archive under an already-opened directory root. Every
// write goes through that root, so no entry can escape it however the archive
// is shaped, and nothing but regular files and directories is written at all.
// It consumes the whole archive: the sender is a process writing into a stream,
// and one left blocked on a stream nobody drains never finishes.
func ExtractInto(reader io.Reader, root *os.Root, base string) ([]Entry, error) {
	bounded := io.LimitReader(reader, MaxTotalBytes+headerAllowance)
	archive := tar.NewReader(bounded)
	entries := []Entry{}
	total := int64(0)
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			// A tar writer rounds its output up to its own block size, which the
			// archive reader stops short of at the end-of-archive marker.
			if _, err := io.Copy(io.Discard, bounded); err != nil {
				return nil, fmt.Errorf("read copied archive: %w", err)
			}
			return entries, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read copied archive: %w", err)
		}
		if len(entries) == MaxFiles {
			return nil, fmt.Errorf("the copy holds more than %d entries", MaxFiles)
		}
		name, err := safeArchiveName(header.Name, base)
		if err != nil {
			return nil, err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0o700); err != nil {
				return nil, fmt.Errorf("create copied directory %q: %w", name, err)
			}
			entries = append(entries, Entry{Path: name, Directory: true})
		case tar.TypeReg:
			if header.Size > MaxFileBytes {
				return nil, fmt.Errorf("copied file %q is over the per-file limit", name)
			}
			total += header.Size
			if total > MaxTotalBytes {
				return nil, fmt.Errorf("the copy exceeds the %d-byte limit", MaxTotalBytes)
			}
			if err := writeCopiedFile(root, name, archive, header); err != nil {
				return nil, err
			}
			entries = append(entries, Entry{Path: name, Size: header.Size})
		default:
			return nil, fmt.Errorf(
				"the copy holds %q, which is not a regular file or directory",
				name,
			)
		}
	}
}

func writeCopiedFile(root *os.Root, name string, archive io.Reader, header *tar.Header) error {
	if parent := path.Dir(name); parent != "." {
		if err := root.MkdirAll(parent, 0o700); err != nil {
			return fmt.Errorf("create copied directory %q: %w", parent, err)
		}
	}
	mode := fs.FileMode(0o600)
	if header.Mode&0o100 != 0 {
		mode = 0o700
	}
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("create copied file %q: %w", name, err)
	}
	defer file.Close()

	copied, err := io.Copy(file, io.LimitReader(archive, header.Size))
	if err != nil {
		return fmt.Errorf("write copied file %q: %w", name, err)
	}
	if copied != header.Size {
		return fmt.Errorf("copied file %q ended early", name)
	}
	return file.Close()
}

// safeArchiveName refuses anything the Mac did not ask for: an absolute or
// climbing path, and any entry outside the single directory the copy requested.
func safeArchiveName(name, base string) (string, error) {
	trimmed := strings.TrimSuffix(name, "/")
	if trimmed == "" || path.Clean(trimmed) != trimmed || path.IsAbs(trimmed) ||
		filepath.IsAbs(filepath.FromSlash(trimmed)) {
		return "", fmt.Errorf("the copy holds an unsafe path %q", name)
	}
	if trimmed != base && !strings.HasPrefix(trimmed, base+"/") {
		return "", fmt.Errorf("the copy holds %q, which is not part of what was requested", name)
	}
	return trimmed, nil
}
