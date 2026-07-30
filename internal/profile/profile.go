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
	"time"

	"github.com/mpizenberg/pisafe/internal/runid"
)

// RecordVersion is the shape of the pin file. A record pisafe does not
// understand is refused rather than guessed at, because what it names is
// mounted into every run.
const RecordVersion = 1

// OffersVersion is the shape of the file recording what npm last said. It is
// separate from RecordVersion because the two files change for different
// reasons: one is what runs load, the other is what a terminal is told.
const OffersVersion = 1

// The two files pisafe keeps beside the profile. Neither is mounted into a run.
const (
	RecordFile = "extensions.json"
	OffersFile = "updates.json"
)

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

// Find reports what one extension is pinned to, and whether it is installed at
// all.
func (record Record) Find(name string) (Extension, bool) {
	for _, existing := range record.Extensions {
		if existing.Name == name {
			return existing, true
		}
	}
	return Extension{}, false
}

// Offer is what npm resolved one installed extension's name to when it was
// last asked. It carries no integrity hash: applying an offer re-resolves and
// verifies what it fetches, so an offer is never something an install is
// checked against.
type Offer struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Offers is the whole of what the last check learned. It is advisory — nothing
// installs from it, no run reads it, and it exists so a user is told what is
// available without pisafe reaching the network while they wait.
type Offers struct {
	Version   int       `json:"version"`
	CheckedAt time.Time `json:"checkedAt"`
	Latest    []Offer   `json:"latest"`
}

// NewOffers records what a check found.
func NewOffers(checkedAt time.Time, latest []Offer) Offers {
	return Offers{Version: OffersVersion, CheckedAt: checkedAt, Latest: latest}
}

// ParseOffers reads what the last check found. It cannot fail: anything
// missing, oversized, malformed, or of a shape this pisafe does not know means
// the same thing as never having checked, and the next check replaces it. An
// entry that is not a name and an exact version is dropped rather than
// carried, because these strings reach a terminal.
func ParseOffers(content []byte) Offers {
	empty := Offers{Version: OffersVersion}
	if len(content) > maxRecordBytes {
		return empty
	}
	var offers Offers
	if err := json.Unmarshal(content, &offers); err != nil || offers.Version != OffersVersion {
		return empty
	}
	kept := make([]Offer, 0, len(offers.Latest))
	for _, offer := range offers.Latest {
		if packageNamePattern.MatchString(offer.Name) && versionPattern.MatchString(offer.Version) {
			kept = append(kept, offer)
		}
	}
	offers.Latest = kept
	return offers
}

// Encode renders the offers for storage.
func (offers Offers) Encode() ([]byte, error) {
	offers.Version = OffersVersion
	slices.SortFunc(offers.Latest, func(left, right Offer) int {
		return strings.Compare(left.Name, right.Name)
	})
	content, err := json.MarshalIndent(offers, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode profile offers: %w", err)
	}
	return append(content, '\n'), nil
}

// Stale reports whether a check is due. A check that never happened is stale,
// which is what makes the first one happen.
func (offers Offers) Stale(now time.Time, interval time.Duration) bool {
	return now.Sub(offers.CheckedAt) >= interval
}

// Update is one installed extension whose pin is not what npm last resolved
// its name to. It is stated as a difference rather than an upgrade: pisafe
// does not order versions, it reports that the registry's answer changed.
type Update struct {
	Name      string
	Installed string
	Available string
}

// Pending reports what is on offer. The record is the authority, so an offer
// for something no longer installed is invisible, and applying one stops it
// being offered without anything having to clear the file.
func (record Record) Pending(offers Offers) []Update {
	available := make(map[string]string, len(offers.Latest))
	for _, offer := range offers.Latest {
		available[offer.Name] = offer.Version
	}
	var updates []Update
	for _, extension := range record.Extensions {
		version, offered := available[extension.Name]
		if !offered || version == extension.Version {
			continue
		}
		updates = append(updates, Update{
			Name:      extension.Name,
			Installed: extension.Version,
			Available: version,
		})
	}
	slices.SortFunc(updates, func(left, right Update) int {
		return strings.Compare(left.Name, right.Name)
	})
	return updates
}

// PendingChange reports what is on offer when the latest check moved it, and
// nothing when it did not. An offer nobody asked for is made once per change
// rather than repeated, because a line that says what the last one said trains
// a reader to skip the place a run's own errors are printed too.
func (record Record) PendingChange(before, after Offers) []Update {
	pending := record.Pending(after)
	if slices.Equal(pending, record.Pending(before)) {
		return nil
	}
	return pending
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
