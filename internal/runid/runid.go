// Package runid validates identifiers used in Git refs, filesystem paths, and
// container names.
package runid

import (
	"fmt"
	"regexp"
)

var pattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func Validate(id string) error {
	if len(id) > 64 || !pattern.MatchString(id) {
		return fmt.Errorf("invalid run ID %q", id)
	}
	return nil
}
