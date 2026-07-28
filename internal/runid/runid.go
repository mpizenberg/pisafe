// Package runid validates identifiers used in Git refs, filesystem paths, and
// container names.
package runid

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
)

var pattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func Validate(id string) error {
	if len(id) > 64 || !pattern.MatchString(id) {
		return fmt.Errorf("invalid run ID %q", id)
	}
	return nil
}

// Project names one checkout twice: Directory is what a run calls it, and Key
// is what its persistent state is filed under. Two checkouts of one repository
// share a Directory and must never share a Key, so the key carries a digest of
// the checkout path.
type Project struct {
	Directory string
	Key       string
}

func NewProject(repositoryRoot string) (Project, error) {
	if !filepath.IsAbs(repositoryRoot) {
		return Project{}, fmt.Errorf("repository root %q is not absolute", repositoryRoot)
	}
	directory := projectSlug(filepath.Base(repositoryRoot))
	digest := sha256.Sum256([]byte(filepath.Clean(repositoryRoot)))
	project := Project{
		Directory: directory,
		Key:       directory + "-" + hex.EncodeToString(digest[:4]),
	}
	if err := Validate(project.Key); err != nil {
		return Project{}, err
	}
	return project, nil
}

func New(project string, now time.Time) (string, error) {
	return newWithEntropy(project, now, rand.Reader)
}

func newWithEntropy(project string, now time.Time, entropy io.Reader) (string, error) {
	var suffix [6]byte
	if _, err := io.ReadFull(entropy, suffix[:]); err != nil {
		return "", fmt.Errorf("generate run ID entropy: %w", err)
	}
	id := projectSlug(project) + "-" +
		now.UTC().Format("20060102-150405") + "-" +
		hex.EncodeToString(suffix[:])
	if err := Validate(id); err != nil {
		return "", err
	}
	return id, nil
}

// projectSlug reduces a directory name to something a Git ref, a filesystem
// path, and a container name all accept.
func projectSlug(project string) string {
	var slug strings.Builder
	lastSeparator := false
	for _, character := range strings.ToLower(project) {
		switch {
		case character >= 'a' && character <= 'z',
			character >= '0' && character <= '9':
			slug.WriteRune(character)
			lastSeparator = false
		case unicode.IsSpace(character) || character == '-' ||
			character == '_' || character == '.':
			if slug.Len() > 0 && !lastSeparator {
				slug.WriteByte('-')
				lastSeparator = true
			}
		}
		if slug.Len() >= 32 {
			break
		}
	}
	result := strings.Trim(slug.String(), "-")
	if result == "" {
		return "project"
	}
	return result
}
