package gitstage

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
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

// inputEntry is one selected path with the metadata the archive preserves.
// Modes are normalized: only the executable bit survives.
type inputEntry struct {
	path       string
	executable bool
	link       string
	size       int64
}

// SelectInputs resolves user-supplied paths against the repository, rejecting
// anything Git already tracks, anything that leaves the repository, special
// files, and credential-shaped names not explicitly marked unsafe.
func SelectInputs(
	ctx context.Context,
	sourcePath string,
	selection InputSelection,
) ([]string, error) {
	if selection.empty() {
		return nil, nil
	}
	root, err := RepositoryRoot(ctx, sourcePath)
	if err != nil {
		return nil, err
	}
	entries, err := selectInputEntries(ctx, root, selection)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		paths = append(paths, entry.path)
	}
	return paths, nil
}

func selectInputEntries(
	ctx context.Context,
	root string,
	selection InputSelection,
) ([]inputEntry, error) {
	if selection.empty() {
		return nil, nil
	}
	excluded, err := ListExcludedInputs(ctx, root)
	if err != nil {
		return nil, err
	}
	selectable := make(map[string]bool, len(excluded.Untracked)+len(excluded.Ignored))
	for _, name := range excluded.Untracked {
		selectable[name] = true
	}
	for _, name := range excluded.Ignored {
		selectable[name] = true
	}

	chosen := map[string]bool{}
	for _, request := range selection.Include {
		if err := chooseInput(root, request, selectable, chosen, false); err != nil {
			return nil, err
		}
	}
	for _, request := range selection.Unsafe {
		if err := chooseInput(root, request, selectable, chosen, true); err != nil {
			return nil, err
		}
	}

	names := make([]string, 0, len(chosen))
	for name := range chosen {
		names = append(names, name)
	}
	sort.Strings(names)
	return describeInputs(root, names)
}

// chooseInput expands one request into concrete selectable paths. A directory
// contributes every selectable path beneath it.
func chooseInput(
	root, request string,
	selectable, chosen map[string]bool,
	unsafe bool,
) error {
	name, err := repositoryRelative(root, request)
	if err != nil {
		return err
	}
	matches := []string{}
	if selectable[name] {
		matches = append(matches, name)
	} else {
		prefix := name + "/"
		for candidate := range selectable {
			if strings.HasPrefix(candidate, prefix) {
				matches = append(matches, candidate)
			}
		}
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

func describeInputs(root string, names []string) ([]inputEntry, error) {
	if len(names) > maxInputFiles {
		return nil, fmt.Errorf(
			"selected %d input files, more than the %d-file limit",
			len(names),
			maxInputFiles,
		)
	}
	entries := make([]inputEntry, 0, len(names))
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
			entries = append(entries, inputEntry{path: name, link: link})
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
			entries = append(entries, inputEntry{
				path:       name,
				executable: info.Mode().Perm()&0o100 != 0,
				size:       info.Size(),
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

func writeInputsArchive(root, archivePath string, entries []inputEntry) error {
	file, err := os.OpenFile(archivePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create input archive: %w", err)
	}
	defer file.Close()

	writer := tar.NewWriter(file)
	for _, entry := range entries {
		header := &tar.Header{
			Name:     entry.path,
			Typeflag: tar.TypeReg,
			Mode:     0o600,
			Size:     entry.size,
		}
		if entry.executable {
			header.Mode = 0o700
		}
		if entry.link != "" {
			header.Typeflag = tar.TypeSymlink
			header.Linkname = entry.link
			header.Mode = 0o777
			header.Size = 0
		}
		if err := writer.WriteHeader(header); err != nil {
			return fmt.Errorf("write input header %q: %w", entry.path, err)
		}
		if entry.link != "" {
			continue
		}
		if err := copyInputContent(
			writer,
			filepath.Join(root, filepath.FromSlash(entry.path)),
			entry.size,
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
		name, err := safeInputName(header.Name)
		if err != nil {
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

func safeInputName(name string) (string, error) {
	cleaned := path.Clean(name)
	if name == "" || cleaned != name ||
		path.IsAbs(name) || strings.HasPrefix(name, "../") || name == ".." ||
		filepath.IsAbs(filepath.FromSlash(name)) {
		return "", fmt.Errorf("input archive holds an unsafe path %q", name)
	}
	for _, segment := range strings.Split(name, "/") {
		if segment == ".git" {
			return "", fmt.Errorf("input archive tries to write into a repository directory")
		}
	}
	return name, nil
}

var (
	secretNames = map[string]bool{
		".htpasswd":        true,
		".netrc":           true,
		".npmrc":           true,
		".pgpass":          true,
		"credentials":      true,
		"credentials.json": true,
		"id_dsa":           true,
		"id_ecdsa":         true,
		"id_ed25519":       true,
		"id_rsa":           true,
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
