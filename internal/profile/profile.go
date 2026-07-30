// Package profile describes the global Pi profile every run mounts read-only:
// what is installed in it, and what one run is told about it.
package profile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"

	"github.com/mpizenberg/pisafe/internal/runid"
)

// RecordVersion is the shape of the pin file. A record pisafe does not
// understand is refused rather than guessed at, because what it names is
// mounted into every run.
const RecordVersion = 1

// maxRecordBytes bounds the pin file. It holds a few hundred bytes per
// installed extension, so anything near this is corruption.
const maxRecordBytes = 1 << 20

var (
	// packageNamePattern is npm's own name grammar, narrowed to what a shell
	// argument and a directory name can carry without quoting.
	packageNamePattern = regexp.MustCompile(`^(@[a-z0-9][a-z0-9._-]*/)?[a-z0-9][a-z0-9._-]*$`)
	versionPattern     = regexp.MustCompile(`^[0-9][A-Za-z0-9.+-]*$`)
	integrityPattern   = regexp.MustCompile(`^sha512-[A-Za-z0-9+/]{86}==$`)
)

// Extension is one installed package, pinned to the exact release it was
// installed from. Directory is where it lives inside the profile: an npm name
// carries a scope and a version does not belong in a path, so neither names
// the directory on its own.
type Extension struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	Integrity string `json:"integrity"`
	Directory string `json:"directory"`
}

func (extension Extension) Validate() error {
	if !packageNamePattern.MatchString(extension.Name) {
		return fmt.Errorf("invalid package name %q", extension.Name)
	}
	if !versionPattern.MatchString(extension.Version) {
		return fmt.Errorf("package %q has an invalid version %q", extension.Name, extension.Version)
	}
	if !integrityPattern.MatchString(extension.Integrity) {
		return fmt.Errorf("package %q has an invalid integrity hash", extension.Name)
	}
	expected, err := runid.NewPackageDirectory(extension.Name)
	if err != nil {
		return err
	}
	if extension.Directory != expected {
		return fmt.Errorf("package %q does not belong in directory %q", extension.Name, extension.Directory)
	}
	return nil
}

// Record is everything installed in the profile. It is the authority on what a
// run loads: the packages are named from it, and a package the record does not
// mention is not mounted into any run's settings even if its directory is
// still there.
type Record struct {
	Version    int         `json:"version"`
	Extensions []Extension `json:"extensions"`
}

// ParseRecord reads the pin file. An absent profile is an empty record rather
// than an error, because a user who has installed nothing has a valid profile.
func ParseRecord(content []byte) (Record, error) {
	if len(bytes.TrimSpace(content)) == 0 {
		return Record{Version: RecordVersion}, nil
	}
	if len(content) > maxRecordBytes {
		return Record{}, errors.New("profile record exceeds size limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	var record Record
	if err := decoder.Decode(&record); err != nil {
		return Record{}, fmt.Errorf("decode profile record: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Record{}, errors.New("profile record contains trailing data")
	}
	if record.Version != RecordVersion {
		return Record{}, fmt.Errorf(
			"profile record version %d is not %d; reinstall the profile's extensions",
			record.Version,
			RecordVersion,
		)
	}
	seen := make(map[string]bool, len(record.Extensions))
	for _, extension := range record.Extensions {
		if err := extension.Validate(); err != nil {
			return Record{}, err
		}
		if seen[extension.Name] {
			return Record{}, fmt.Errorf("package %q is installed twice", extension.Name)
		}
		seen[extension.Name] = true
	}
	return record, nil
}

// Encode renders the record for storage, sorted by name so an install never
// reorders what it did not touch.
func (record Record) Encode() ([]byte, error) {
	record.Version = RecordVersion
	slices.SortFunc(record.Extensions, func(left, right Extension) int {
		return strings.Compare(left.Name, right.Name)
	})
	content, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode profile record: %w", err)
	}
	return append(content, '\n'), nil
}

// With adds or replaces one extension.
func (record Record) With(extension Extension) Record {
	updated := Record{Version: RecordVersion}
	for _, existing := range record.Extensions {
		if existing.Name != extension.Name {
			updated.Extensions = append(updated.Extensions, existing)
		}
	}
	return Record{
		Version:    RecordVersion,
		Extensions: append(updated.Extensions, extension),
	}
}

// Without removes one extension by name, reporting whether it was installed.
func (record Record) Without(name string) (Record, Extension, bool) {
	updated := Record{Version: RecordVersion}
	var removed Extension
	found := false
	for _, existing := range record.Extensions {
		if existing.Name == name {
			removed = existing
			found = true
			continue
		}
		updated.Extensions = append(updated.Extensions, existing)
	}
	return updated, removed, found
}

// Configuration is what one run is told: the packages it loads, and the one
// directory it may load project resources from. Both are paths inside the
// container, so neither discloses anything about the Mac.
type Configuration struct {
	Packages  []string `json:"packages"`
	Workspace string   `json:"workspace"`
}

// Configure renders the run-side configuration. The package root is where the
// profile is mounted, which is the run's business rather than the record's.
func (record Record) Configure(packageRoot, workspace string) Configuration {
	packages := make([]string, 0, len(record.Extensions))
	for _, extension := range record.Extensions {
		packages = append(
			packages,
			packageRoot+"/"+extension.Directory+"/node_modules/"+extension.Name,
		)
	}
	slices.Sort(packages)
	return Configuration{Packages: packages, Workspace: workspace}
}

// ValidatePackageName bounds what may be installed. Only a plain npm name is
// accepted: a git source cannot be pinned to an integrity hash, and a local
// path names something inside a container the user cannot see.
func ValidatePackageName(name string) error {
	if !packageNamePattern.MatchString(name) {
		return fmt.Errorf(
			"%q is not an npm package name; install one as name or @scope/name",
			name,
		)
	}
	return nil
}

// ValidateVersion bounds what may be pinned. An exact release only: a range
// would make two installs of one spec produce different profiles.
func ValidateVersion(version string) error {
	if !versionPattern.MatchString(version) {
		return fmt.Errorf("%q is not an exact package version", version)
	}
	return nil
}

// ValidateIntegrity bounds what may be recorded as a pin. npm reports the
// SHA-512 of the tarball it resolved, base64-encoded.
func ValidateIntegrity(integrity string) error {
	if !integrityPattern.MatchString(integrity) {
		return fmt.Errorf("%q is not a SHA-512 integrity hash", integrity)
	}
	return nil
}
