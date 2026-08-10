package gitstage

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

const (
	inputsArchiveName = "inputs.tar"
	maxInputFiles     = 2048
	maxInputBytes     = 64 << 20
	maxInputFileBytes = 16 << 20
)

// InputSelection names the untracked or ignored paths the user chose to copy
// into the run. Unsafe entries skip the credential-shaped name check.
type InputSelection struct {
	Include []string
	Unsafe  []string
}

func (selection InputSelection) empty() bool {
	return len(selection.Include) == 0 && len(selection.Unsafe) == 0
}

// SelectedInput is one selected path with the metadata the archive preserves.
// Modes are normalized: only the executable bit survives.
type SelectedInput struct {
	Path       string
	Executable bool
	Link       string
	Size       int64
}

// Select resolves user-supplied paths against what the run would otherwise not
// receive, rejecting anything Git already tracks, anything that leaves the
// repository, special files, and credential-shaped names not explicitly marked
// unsafe. It also reports what stays behind once the selection is taken out, so
// the two lists a run prints are decided together and cannot disagree.
func (excluded ExcludedInputs) Select(
	selection InputSelection,
) ([]SelectedInput, ExcludedInputs, error) {
	if selection.empty() {
		return nil, excluded, nil
	}
	chosen, taken := map[string]bool{}, map[string]bool{}
	for _, request := range selection.Include {
		if err := excluded.choose(request, chosen, taken, false); err != nil {
			return nil, ExcludedInputs{}, err
		}
	}
	for _, request := range selection.Unsafe {
		if err := excluded.choose(request, chosen, taken, true); err != nil {
			return nil, ExcludedInputs{}, err
		}
	}

	names := make([]string, 0, len(chosen))
	for name := range chosen {
		names = append(names, name)
	}
	sort.Strings(names)
	inputs, err := describeInputs(excluded.Root, names)
	if err != nil {
		return nil, ExcludedInputs{}, err
	}
	return inputs, excluded.remaining(chosen, taken), nil
}

// choose expands one request into concrete files and records which excluded
// entries it emptied.
func (excluded ExcludedInputs) choose(
	request string,
	chosen, taken map[string]bool,
	unsafe bool,
) error {
	name, err := repositoryRelative(excluded.Root, request)
	if err != nil {
		return err
	}
	matches, err := excluded.expand(name, taken)
	if err != nil {
		return err
	}
	if len(matches) == 0 {
		return fmt.Errorf(
			"%q is not an untracked or ignored file in this repository",
			request,
		)
	}
	for _, match := range matches {
		if !unsafe && LooksLikeSecret(match) {
			return fmt.Errorf(
				"%q looks like a credential; including it lets everything in the run "+
					"read and exfiltrate it. Use --include-unsafe %s to override",
				match,
				match,
			)
		}
		chosen[match] = true
	}
	return nil
}

// expand names the files one request stands for. An entry Git collapsed into a
// directory is read from the filesystem instead of from the listing, so a
// request can be an entry, an ancestor of one, or a path inside one, and a
// directory always contributes its files one by one — which is what the
// credential check and the per-file limits need to see.
func (excluded ExcludedInputs) expand(name string, taken map[string]bool) ([]string, error) {
	matches := []string{}
	for _, entry := range slices.Concat(excluded.Untracked, excluded.Ignored) {
		directory := strings.TrimSuffix(entry, "/")
		if directory == entry {
			if entry == name || strings.HasPrefix(entry, name+"/") {
				matches = append(matches, entry)
			}
			continue
		}
		requested := directory
		if !strings.HasPrefix(directory, name+"/") && directory != name {
			if !strings.HasPrefix(name, directory+"/") {
				continue
			}
			// Only part of the directory was asked for, so it keeps the rest.
			requested = name
		} else {
			taken[entry] = true
		}
		files, err := walkInput(excluded.Root, requested)
		if err != nil {
			return nil, err
		}
		matches = append(matches, files...)
	}
	return matches, nil
}

// walkInput lists what one selectable path contributes: itself when it is not a
// directory, every file beneath it when it is. Nothing is followed through a
// symlink, and a path that is not there contributes nothing, which leaves a
// request naming it unselectable rather than failing over a missing file.
func walkInput(root, name string) ([]string, error) {
	base := filepath.Join(root, filepath.FromSlash(name))
	info, err := os.Lstat(base)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect input %q: %w", name, err)
	}
	if !info.IsDir() {
		return []string{name}, nil
	}
	files := []string{}
	walkErr := filepath.WalkDir(base, func(current string, entry fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("read input %q: %w", name, err)
		}
		relative, err := filepath.Rel(root, current)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if entry.Name() == ".git" {
			return fmt.Errorf(
				"input %q contains the Git repository %q; name paths inside it instead",
				name,
				path.Dir(relative),
			)
		}
		if entry.IsDir() {
			return nil
		}
		files = append(files, relative)
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	return files, nil
}

// remaining reports what stays behind once a selection is taken out. A
// collapsed directory survives unless the selection took the whole of it:
// naming one file inside it leaves the rest excluded.
func (excluded ExcludedInputs) remaining(chosen, taken map[string]bool) ExcludedInputs {
	keep := func(names []string) []string {
		kept := make([]string, 0, len(names))
		for _, name := range names {
			if chosen[name] || taken[name] {
				continue
			}
			kept = append(kept, name)
		}
		return kept
	}
	return ExcludedInputs{
		Root:      excluded.Root,
		Untracked: keep(excluded.Untracked),
		Ignored:   keep(excluded.Ignored),
	}
}

// repositoryRelative resolves a user-supplied path to a repository-relative
// name. The parent is resolved through symlinks so a symlinked working
// directory still lands inside the repository, while a symlinked leaf keeps
// its own name.
func repositoryRelative(root, request string) (string, error) {
	absolute := request
	if !filepath.IsAbs(absolute) {
		resolved, err := filepath.Abs(absolute)
		if err != nil {
			return "", fmt.Errorf("resolve %q: %w", request, err)
		}
		absolute = resolved
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", request, err)
	}
	relative, err := filepath.Rel(root, filepath.Join(parent, filepath.Base(absolute)))
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", request, err)
	}
	relative = filepath.ToSlash(relative)
	if relative == "." {
		return "", fmt.Errorf("%q selects the whole repository; name specific paths", request)
	}
	if relative == ".." || strings.HasPrefix(relative, "../") {
		return "", fmt.Errorf("%q is outside the repository", request)
	}
	return relative, nil
}

func describeInputs(root string, names []string) ([]SelectedInput, error) {
	if len(names) > maxInputFiles {
		return nil, fmt.Errorf(
			"selected %d input files, more than the %d-file limit",
			len(names),
			maxInputFiles,
		)
	}
	entries := make([]SelectedInput, 0, len(names))
	total := int64(0)
	for _, name := range names {
		absolute := filepath.Join(root, filepath.FromSlash(name))
		info, err := os.Lstat(absolute)
		if err != nil {
			return nil, fmt.Errorf("inspect input %q: %w", name, err)
		}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(absolute)
			if err != nil {
				return nil, fmt.Errorf("read input link %q: %w", name, err)
			}
			if err := checkLinkStaysInside(root, name, link); err != nil {
				return nil, err
			}
			entries = append(entries, SelectedInput{Path: name, Link: link})
		case info.Mode().IsRegular():
			if info.Size() > maxInputFileBytes {
				return nil, fmt.Errorf(
					"input %q is %d bytes, over the %d-byte per-file limit",
					name,
					info.Size(),
					maxInputFileBytes,
				)
			}
			total += info.Size()
			if total > maxInputBytes {
				return nil, fmt.Errorf(
					"selected inputs exceed the %d-byte total limit",
					maxInputBytes,
				)
			}
			entries = append(entries, SelectedInput{
				Path:       name,
				Executable: info.Mode().Perm()&0o100 != 0,
				Size:       info.Size(),
			})
		default:
			return nil, fmt.Errorf("input %q is not a regular file or symlink", name)
		}
	}
	return entries, nil
}

// checkLinkStaysInside rejects a symlink whose target leaves the repository,
// which would otherwise copy host content the run must never see.
func checkLinkStaysInside(root, name, link string) error {
	if filepath.IsAbs(link) {
		return fmt.Errorf("input link %q points outside the repository", name)
	}
	target := path.Join(path.Dir(name), path.Clean(link))
	if target == ".." || strings.HasPrefix(target, "../") {
		return fmt.Errorf("input link %q points outside the repository", name)
	}
	return nil
}

func writeInputsArchive(root, archivePath string, entries []SelectedInput) error {
	file, err := os.OpenFile(archivePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create input archive: %w", err)
	}
	defer file.Close()

	writer := tar.NewWriter(file)
	for _, entry := range entries {
		header := &tar.Header{
			Name:     entry.Path,
			Typeflag: tar.TypeReg,
			Mode:     0o600,
			Size:     entry.Size,
		}
		if entry.Executable {
			header.Mode = 0o700
		}
		if entry.Link != "" {
			header.Typeflag = tar.TypeSymlink
			header.Linkname = entry.Link
			header.Mode = 0o777
			header.Size = 0
		}
		if err := writer.WriteHeader(header); err != nil {
			return fmt.Errorf("write input header %q: %w", entry.Path, err)
		}
		if entry.Link != "" {
			continue
		}
		if err := copyInputContent(
			writer,
			filepath.Join(root, filepath.FromSlash(entry.Path)),
			entry.Size,
		); err != nil {
			return err
		}
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("finish input archive: %w", err)
	}
	return file.Close()
}

// copyInputContent writes exactly the size recorded in the header, so a file
// growing or shrinking during preparation fails instead of corrupting the
// archive.
func copyInputContent(writer io.Writer, sourcePath string, size int64) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("open input: %w", err)
	}
	defer source.Close()

	copied, err := io.Copy(writer, io.LimitReader(source, size))
	if err != nil {
		return fmt.Errorf("copy input %q: %w", sourcePath, err)
	}
	if copied != size {
		return fmt.Errorf("input %q changed size while staging", sourcePath)
	}
	return nil
}

// extractInputs unpacks the transferred archive into the staged workspace. It
// re-validates every entry: the archive crosses the boundary as data, and the
// workspace already contains a Git repository that inputs must never reach
// into.
func extractInputs(archivePath, workspace string) ([]string, error) {
	file, err := os.Open(archivePath)
	if err != nil {
		return nil, fmt.Errorf("open input archive: %w", err)
	}
	defer file.Close()

	reader := tar.NewReader(io.LimitReader(file, maxInputBytes+maxInputFiles*1024))
	extracted := []string{}
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read input archive: %w", err)
		}
		if len(extracted) == maxInputFiles {
			return nil, fmt.Errorf("input archive holds more than %d files", maxInputFiles)
		}
		name := header.Name
		if err := safePath("input", name); err != nil {
			return nil, err
		}
		target := filepath.Join(workspace, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return nil, fmt.Errorf("create input directory: %w", err)
		}
		switch header.Typeflag {
		case tar.TypeSymlink:
			if err := checkLinkStaysInside(workspace, name, header.Linkname); err != nil {
				return nil, err
			}
			if err := os.Symlink(header.Linkname, target); err != nil {
				return nil, fmt.Errorf("create input link %q: %w", name, err)
			}
		case tar.TypeReg:
			if header.Size > maxInputFileBytes {
				return nil, fmt.Errorf("input %q exceeds the per-file limit", name)
			}
			if err := writeExtractedInput(target, reader, header); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("input %q has an unsupported archive type", name)
		}
		extracted = append(extracted, name)
	}
	return extracted, nil
}

func writeExtractedInput(target string, reader io.Reader, header *tar.Header) error {
	mode := fs.FileMode(0o600)
	if header.Mode&0o100 != 0 {
		mode = 0o700
	}
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("create input %q: %w", header.Name, err)
	}
	defer file.Close()

	if _, err := io.Copy(file, io.LimitReader(reader, header.Size)); err != nil {
		return fmt.Errorf("write input %q: %w", header.Name, err)
	}
	return file.Close()
}

var (
	// secretNames are the names no word in them would catch.
	secretNames = map[string]bool{
		".htpasswd":  true,
		".netrc":     true,
		".npmrc":     true,
		".pgpass":    true,
		"id_dsa":     true,
		"id_ecdsa":   true,
		"id_ed25519": true,
		"id_rsa":     true,
	}
	secretExtensions = map[string]bool{
		".jks":      true,
		".key":      true,
		".keystore": true,
		".p12":      true,
		".pem":      true,
		".pfx":      true,
	}
	secretWords = map[string]bool{
		"apikey":      true,
		"apikeys":     true,
		"credential":  true,
		"credentials": true,
		"passwd":      true,
		"password":    true,
		"passwords":   true,
		"secret":      true,
		"secrets":     true,
		"token":       true,
		"tokens":      true,
	}
)

// LooksLikeSecret recognizes credential-shaped names. Whole words are matched
// so ordinary names such as tokenizer.json are not flagged.
func LooksLikeSecret(name string) bool {
	base := strings.ToLower(path.Base(name))
	if secretNames[base] || secretExtensions[path.Ext(base)] {
		return true
	}
	if base == ".env" || strings.HasPrefix(base, ".env.") {
		return true
	}
	for _, word := range strings.FieldsFunc(base, func(letter rune) bool {
		return !('a' <= letter && letter <= 'z') && !('0' <= letter && letter <= '9')
	}) {
		if secretWords[word] {
			return true
		}
	}
	return false
}
