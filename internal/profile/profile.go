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

// ToolsVersion is the shape of the file recording the installed commands.
const ToolsVersion = 1

// The three files pisafe keeps beside the profile. None is mounted into a run.
const (
	RecordFile = "extensions.json"
	OffersFile = "updates.json"
	ToolsFile  = "tools.json"
)

// maxRecordBytes bounds the pin file. It holds a few hundred bytes per
// installed package, so anything near this is corruption.
const maxRecordBytes = 1 << 20

var (
	// packageNamePattern is npm's own name grammar, narrowed to what a shell
	// argument and a directory name can carry without quoting.
	packageNamePattern = regexp.MustCompile(`^(@[a-z0-9][a-z0-9._-]*/)?[a-z0-9][a-z0-9._-]*$`)
	versionPattern     = regexp.MustCompile(`^[0-9][A-Za-z0-9.+-]*$`)
	integrityPattern   = regexp.MustCompile(`^sha512-[A-Za-z0-9+/]{86}==$`)
	// binaryNamePattern is narrower than the filesystem allows on purpose: a
	// leading dot would hide a link, and a name that needs quoting is not a
	// command anyone would type.
	binaryNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

// Pin is one installed package, fixed to the exact release it was installed
// from. Directory is where it lives inside the profile: an npm name carries a
// scope and a version does not belong in a path, so neither names the
// directory on its own.
type Pin struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	Integrity string `json:"integrity"`
	Directory string `json:"directory"`
}

func (pin Pin) Validate() error {
	if !packageNamePattern.MatchString(pin.Name) {
		return fmt.Errorf("invalid package name %q", pin.Name)
	}
	if !versionPattern.MatchString(pin.Version) {
		return fmt.Errorf("package %q has an invalid version %q", pin.Name, pin.Version)
	}
	if !integrityPattern.MatchString(pin.Integrity) {
		return fmt.Errorf("package %q has an invalid integrity hash", pin.Name)
	}
	expected, err := runid.NewPackageDirectory(pin.Name)
	if err != nil {
		return err
	}
	if pin.Directory != expected {
		return fmt.Errorf("package %q does not belong in directory %q", pin.Name, pin.Directory)
	}
	return nil
}

// Record is everything installed in the profile. It is the authority on what a
// run loads: the packages are named from it, and a package the record does not
// mention is not mounted into any run's settings even if its directory is
// still there.
type Record struct {
	Version    int   `json:"version"`
	Extensions []Pin `json:"extensions"`
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
	for _, pin := range record.Extensions {
		if err := pin.Validate(); err != nil {
			return Record{}, err
		}
		if seen[pin.Name] {
			return Record{}, fmt.Errorf("package %q is installed twice", pin.Name)
		}
		seen[pin.Name] = true
	}
	return record, nil
}

// Encode renders the record for storage, sorted by name so an install never
// reorders what it did not touch.
func (record Record) Encode() ([]byte, error) {
	record.Version = RecordVersion
	slices.SortFunc(record.Extensions, func(left, right Pin) int {
		return strings.Compare(left.Name, right.Name)
	})
	content, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode profile record: %w", err)
	}
	return append(content, '\n'), nil
}

// With adds or replaces one package.
func (record Record) With(pin Pin) Record {
	return Record{
		Version:    RecordVersion,
		Extensions: with(record.Extensions, pin, pinName),
	}
}

// Without removes one package by name, reporting whether it was installed.
func (record Record) Without(name string) (Record, Pin, bool) {
	remaining, removed, found := without(record.Extensions, name, pinName)
	return Record{Version: RecordVersion, Extensions: remaining}, removed, found
}

// Find reports what one package is pinned to, and whether it is installed at
// all.
func (record Record) Find(name string) (Pin, bool) {
	return find(record.Extensions, name, pinName)
}

func pinName(pin Pin) string { return pin.Name }

// A package name is what identifies an entry in either record, so adding,
// removing, and looking one up are the same three operations over both.

func with[Entry any](entries []Entry, entry Entry, name func(Entry) string) []Entry {
	kept := make([]Entry, 0, len(entries)+1)
	for _, existing := range entries {
		if name(existing) != name(entry) {
			kept = append(kept, existing)
		}
	}
	return append(kept, entry)
}

func without[Entry any](
	entries []Entry,
	target string,
	name func(Entry) string,
) ([]Entry, Entry, bool) {
	var kept []Entry
	var removed Entry
	found := false
	for _, existing := range entries {
		if name(existing) == target {
			removed = existing
			found = true
			continue
		}
		kept = append(kept, existing)
	}
	return kept, removed, found
}

func find[Entry any](entries []Entry, target string, name func(Entry) string) (Entry, bool) {
	for _, existing := range entries {
		if name(existing) == target {
			return existing, true
		}
	}
	var missing Entry
	return missing, false
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
	for _, pin := range record.Extensions {
		version, offered := available[pin.Name]
		if !offered || version == pin.Version {
			continue
		}
		updates = append(updates, Update{
			Name:      pin.Name,
			Installed: pin.Version,
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
	for _, pin := range record.Extensions {
		packages = append(
			packages,
			packageRoot+"/"+pin.Directory+"/node_modules/"+pin.Name,
		)
	}
	slices.Sort(packages)
	return Configuration{Packages: packages, Workspace: workspace}
}

// Tool is one installed command. It is pinned exactly as an extension is; what
// differs is the effect, which is the binary names it claims in the one
// directory a run has on its PATH. A package that claims none is not a tool,
// so Binaries is never empty.
type Tool struct {
	Pin
	Binaries []string `json:"binaries"`
}

func (tool Tool) Validate() error {
	if err := tool.Pin.Validate(); err != nil {
		return err
	}
	if len(tool.Binaries) == 0 {
		return fmt.Errorf("package %q provides no command", tool.Name)
	}
	seen := make(map[string]bool, len(tool.Binaries))
	for _, binary := range tool.Binaries {
		if err := ValidateBinaryName(binary); err != nil {
			return fmt.Errorf("package %q: %w", tool.Name, err)
		}
		if seen[binary] {
			return fmt.Errorf("package %q claims %q twice", tool.Name, binary)
		}
		seen[binary] = true
	}
	return nil
}

// Tools is every command installed in the profile. Unlike the extension
// record, no run reads it: what a run sees is the directory of links this
// record is the recipe for.
type Tools struct {
	Version int    `json:"version"`
	Tools   []Tool `json:"tools"`
}

// ParseTools reads the tool record. An absent profile is an empty one, for the
// same reason an absent extension record is.
func ParseTools(content []byte) (Tools, error) {
	if len(bytes.TrimSpace(content)) == 0 {
		return Tools{Version: ToolsVersion}, nil
	}
	if len(content) > maxRecordBytes {
		return Tools{}, errors.New("profile tool record exceeds size limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	var record Tools
	if err := decoder.Decode(&record); err != nil {
		return Tools{}, fmt.Errorf("decode profile tool record: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Tools{}, errors.New("profile tool record contains trailing data")
	}
	if record.Version != ToolsVersion {
		return Tools{}, fmt.Errorf(
			"profile tool record version %d is not %d; reinstall the profile's tools",
			record.Version,
			ToolsVersion,
		)
	}
	claimed := make(map[string]string, len(record.Tools))
	for _, tool := range record.Tools {
		if err := tool.Validate(); err != nil {
			return Tools{}, err
		}
		if _, installed := claimed[tool.Name]; installed {
			return Tools{}, fmt.Errorf("package %q is installed twice", tool.Name)
		}
		claimed[tool.Name] = tool.Name
		for _, binary := range tool.Binaries {
			if holder, taken := claimed[binary]; taken && holder != tool.Name {
				return Tools{}, fmt.Errorf(
					"%q is claimed by both %s and %s",
					binary,
					holder,
					tool.Name,
				)
			}
			claimed[binary] = tool.Name
		}
	}
	return record, nil
}

// Encode renders the tool record for storage.
func (record Tools) Encode() ([]byte, error) {
	record.Version = ToolsVersion
	slices.SortFunc(record.Tools, func(left, right Tool) int {
		return strings.Compare(left.Name, right.Name)
	})
	content, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode profile tool record: %w", err)
	}
	return append(content, '\n'), nil
}

// With adds or replaces one tool.
func (record Tools) With(tool Tool) Tools {
	return Tools{Version: ToolsVersion, Tools: with(record.Tools, tool, toolName)}
}

// Without removes one tool by name, reporting whether it was installed.
func (record Tools) Without(name string) (Tools, Tool, bool) {
	remaining, removed, found := without(record.Tools, name, toolName)
	return Tools{Version: ToolsVersion, Tools: remaining}, removed, found
}

// Find reports what one tool is pinned to, and whether it is installed at all.
func (record Tools) Find(name string) (Tool, bool) {
	return find(record.Tools, name, toolName)
}

func toolName(tool Tool) string { return tool.Name }

// Conflict is one binary name two packages both claim. Which name a package
// installs is the package's own choice rather than the user's, so a collision
// is something pisafe can only report.
type Conflict struct {
	Binary string
	Holder string
}

// Conflicts reports what stands in the way of installing one tool. A tool never
// conflicts with the release of itself it replaces.
func (record Tools) Conflicts(candidate Tool) []Conflict {
	var conflicts []Conflict
	for _, tool := range record.Tools {
		if tool.Name == candidate.Name {
			continue
		}
		for _, binary := range candidate.Binaries {
			if slices.Contains(tool.Binaries, binary) {
				conflicts = append(conflicts, Conflict{Binary: binary, Holder: tool.Name})
			}
		}
	}
	slices.SortFunc(conflicts, func(left, right Conflict) int {
		return strings.Compare(left.Binary, right.Binary)
	})
	return conflicts
}

// Link is one entry of the directory a run has on its PATH: a name, and the
// module root that answers it.
type Link struct {
	Binary    string
	Directory string
}

// Links is the whole of what that directory holds. It is derived from the
// record every time rather than edited, so nothing a failed install left behind
// can outlive the record that named it.
func (record Tools) Links() []Link {
	links := make([]Link, 0, len(record.Tools))
	for _, tool := range record.Tools {
		for _, binary := range tool.Binaries {
			links = append(links, Link{Binary: binary, Directory: tool.Directory})
		}
	}
	slices.SortFunc(links, func(left, right Link) int {
		return strings.Compare(left.Binary, right.Binary)
	})
	return links
}

// ValidateBinaryName bounds what a package may claim. The name comes from the
// package rather than from the user, and it becomes both a path component in
// the profile and a word on a terminal, so it is held to what a command may
// look like rather than to what a filesystem would accept.
func ValidateBinaryName(name string) error {
	if !binaryNamePattern.MatchString(name) {
		return fmt.Errorf("%q is not a usable command name", name)
	}
	return nil
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
