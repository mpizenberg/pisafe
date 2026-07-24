// Package runid validates identifiers used in Git refs, filesystem paths, and
// container names.
package runid

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
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

func New(project string, now time.Time) (string, error) {
	return newWithEntropy(project, now, rand.Reader)
}

func newWithEntropy(project string, now time.Time, entropy io.Reader) (string, error) {
	var suffix [6]byte
	if _, err := io.ReadFull(entropy, suffix[:]); err != nil {
		return "", fmt.Errorf("generate run ID entropy: %w", err)
	}
	id := ProjectSlug(project) + "-" +
		now.UTC().Format("20060102-150405") + "-" +
		hex.EncodeToString(suffix[:])
	if err := Validate(id); err != nil {
		return "", err
	}
	return id, nil
}

func ProjectSlug(project string) string {
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
