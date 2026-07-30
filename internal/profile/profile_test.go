package profile

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func planMode() Extension {
	return Extension{
		Name:      "@earendil-works/plan-mode",
		Version:   "1.2.3",
		Integrity: "sha512-" + strings.Repeat("A", 86) + "==",
		Directory: "earendil-works-plan-mode-bf0f2759",
	}
}

func TestAnEmptyProfileIsValidRatherThanMissing(t *testing.T) {
	for name, content := range map[string][]byte{
		"absent": nil,
		"blank":  []byte("\n"),
	} {
		record, err := ParseRecord(content)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(record.Extensions) != 0 || record.Version != RecordVersion {
			t.Errorf("%s: record = %+v", name, record)
		}
		if configured := record.Configure("/home/node/.pi/agent/npm", "/work/project"); len(configured.Packages) != 0 {
			t.Errorf("%s: packages = %v", name, configured.Packages)
		}
	}
}

// TestAPinTheRecordCannotVouchForIsRefused covers the whole record path: what
// it names is mounted into every run, so anything pisafe cannot address, an
// unknown shape, or a package in the wrong directory fails loudly instead of
// being skipped.
func TestAPinTheRecordCannotVouchForIsRefused(t *testing.T) {
	misplaced := planMode()
	misplaced.Directory = "somewhere-else-71e17b6c"
	unpinned := planMode()
	unpinned.Version = "^1.2.3"
	unhashed := planMode()
	unhashed.Integrity = "sha1-short"
	climbing := planMode()
	climbing.Name = "../../etc"

	for name, record := range map[string]Record{
		"misplaced": {Version: RecordVersion, Extensions: []Extension{misplaced}},
		"unpinned":  {Version: RecordVersion, Extensions: []Extension{unpinned}},
		"unhashed":  {Version: RecordVersion, Extensions: []Extension{unhashed}},
		"climbing":  {Version: RecordVersion, Extensions: []Extension{climbing}},
		"twice": {
			Version:    RecordVersion,
			Extensions: []Extension{planMode(), planMode()},
		},
		"future": {Version: RecordVersion + 1},
	} {
		content, err := record.Encode()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		// Encode always stamps the current version, so the record a future
		// pisafe would have written is spelled out rather than round-tripped.
		if name == "future" {
			content = []byte(`{"version": 2, "extensions": []}`)
		}
		if _, err := ParseRecord(content); err == nil {
			t.Errorf("%s: record was accepted", name)
		}
	}
	if _, err := ParseRecord([]byte(`{"version":1,"extra":true}`)); err == nil {
		t.Error("a record with an unknown field was accepted")
	}
}

func TestInstalledExtensionsBecomeThePackagesARunLoads(t *testing.T) {
	second := Extension{
		Name:      "aardvark",
		Version:   "0.1.0",
		Integrity: "sha512-" + strings.Repeat("B", 86) + "==",
		Directory: "aardvark-cf9c1cb8",
	}
	record := Record{Version: RecordVersion}.With(planMode()).With(second)
	content, err := record.Encode()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseRecord(content)
	if err != nil {
		t.Fatal(err)
	}
	configured := parsed.Configure("/home/node/.pi/agent/npm", "/work/project")
	if configured.Workspace != "/work/project" {
		t.Errorf("workspace = %q", configured.Workspace)
	}
	want := []string{
		"/home/node/.pi/agent/npm/aardvark-cf9c1cb8/node_modules/aardvark",
		"/home/node/.pi/agent/npm/earendil-works-plan-mode-bf0f2759/node_modules/@earendil-works/plan-mode",
	}
	if !slices.Equal(configured.Packages, want) {
		t.Errorf("packages = %v, want %v", configured.Packages, want)
	}
}

// TestReinstallingReplacesRatherThanAccumulates is what keeps one package from
// appearing at two versions, which would leave the profile holding a package
// no run loads.
func TestReinstallingReplacesRatherThanAccumulates(t *testing.T) {
	upgraded := planMode()
	upgraded.Version = "2.0.0"
	record := Record{Version: RecordVersion}.With(planMode()).With(upgraded)
	if len(record.Extensions) != 1 || record.Extensions[0].Version != "2.0.0" {
		t.Fatalf("record = %+v", record.Extensions)
	}
	emptied, removed, found := record.Without(planMode().Name)
	if !found || removed.Version != "2.0.0" {
		t.Fatalf("removed = %+v, found = %v", removed, found)
	}
	if len(emptied.Extensions) != 0 {
		t.Errorf("record still holds %+v", emptied.Extensions)
	}
	if _, _, found := emptied.Without(planMode().Name); found {
		t.Error("removing what is not installed reported a removal")
	}
}

func TestOnlyAnExactlyPinnedNpmPackageMayBeInstalled(t *testing.T) {
	for _, name := range []string{
		"@earendil-works/plan-mode",
		"is-number",
	} {
		if err := ValidatePackageName(name); err != nil {
			t.Errorf("%q: %v", name, err)
		}
	}
	for _, name := range []string{
		"git:github.com/user/repo",
		"/absolute/path",
		"./relative",
		"name@1.0.0",
		"Name",
		"",
		"../escape",
	} {
		if err := ValidatePackageName(name); err == nil {
			t.Errorf("%q was accepted as a package name", name)
		}
	}
	if err := ValidateVersion("1.2.3-rc.1+build"); err != nil {
		t.Error(err)
	}
	for _, version := range []string{"^1.2.3", "latest", "", "~1.0"} {
		if err := ValidateVersion(version); err == nil {
			t.Errorf("%q was accepted as an exact version", version)
		}
	}
	if err := ValidateIntegrity("sha512-" + strings.Repeat("A", 86) + "=="); err != nil {
		t.Error(err)
	}
	for _, integrity := range []string{
		"sha1-" + strings.Repeat("A", 86) + "==",
		"sha512-short",
		"",
	} {
		if err := ValidateIntegrity(integrity); err == nil {
			t.Errorf("%q was accepted as an integrity hash", integrity)
		}
	}
}

// TestAnOfferIsAdvisoryAndNeverBreaksWhatReadsIt covers the whole offers path.
// The file is what a terminal is told rather than what a run loads, so nothing
// in it may fail the command that read it.
func TestAnOfferIsAdvisoryAndNeverBreaksWhatReadsIt(t *testing.T) {
	for name, content := range map[string][]byte{
		"absent":        nil,
		"blank":         []byte(""),
		"not json":      []byte("{{{"),
		"unknown shape": []byte(`{"version":99,"latest":[{"name":"is-number","version":"7.0.0"}]}`),
		"oversized":     []byte(strings.Repeat("x", maxRecordBytes+1)),
		"wrong type":    []byte(`{"version":1,"latest":"everything"}`),
	} {
		offers := ParseOffers(content)
		if offers.Version != OffersVersion || len(offers.Latest) != 0 {
			t.Errorf("%s: offers = %+v", name, offers)
		}
		if !offers.Stale(time.Now(), time.Hour) {
			t.Errorf("%s: an unreadable offer was treated as a check that happened", name)
		}
	}
	// An offer reaches a terminal, so an entry pisafe could not have written is
	// dropped rather than printed.
	offers := ParseOffers([]byte(`{"version":1,"latest":[
		{"name":"is-number","version":"7.0.0"},
		{"name":"is-number\u001b[2J","version":"7.0.0"},
		{"name":"cowsay","version":"latest"}
	]}`))
	if len(offers.Latest) != 1 || offers.Latest[0].Name != "is-number" {
		t.Fatalf("offers = %+v", offers.Latest)
	}
}

// TestAnOfferSurvivesStorageAndOnlyDiffersFromWhatIsInstalled covers what the
// user is told: an offer is pending only while the record disagrees with it,
// so applying an update or removing a package silences it without anything
// having to clear the file.
func TestAnOfferSurvivesStorageAndOnlyDiffersFromWhatIsInstalled(t *testing.T) {
	checkedAt := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	stored, err := NewOffers(checkedAt, []Offer{
		{Name: "is-number", Version: "7.0.0"},
		{Name: planMode().Name, Version: "1.2.3"},
	}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	offers := ParseOffers(stored)
	if !offers.CheckedAt.Equal(checkedAt) {
		t.Fatalf("checkedAt = %v", offers.CheckedAt)
	}
	if offers.Stale(checkedAt.Add(23*time.Hour), 24*time.Hour) {
		t.Error("a check made 23 hours ago was due again after 24")
	}
	if !offers.Stale(checkedAt.Add(25*time.Hour), 24*time.Hour) {
		t.Error("a check made 25 hours ago was not due again after 24")
	}

	installed := Extension{
		Name:      "is-number",
		Version:   "6.0.0",
		Integrity: "sha512-" + strings.Repeat("B", 86) + "==",
		Directory: "is-number-5e0e83b1",
	}
	record := Record{Version: RecordVersion}.With(installed).With(planMode())
	updates := record.Pending(offers)
	if len(updates) != 1 {
		t.Fatalf("pending = %+v", updates)
	}
	if updates[0] != (Update{Name: "is-number", Installed: "6.0.0", Available: "7.0.0"}) {
		t.Errorf("update = %+v", updates[0])
	}
	applied := installed
	applied.Version = "7.0.0"
	if pending := record.With(applied).Pending(offers); len(pending) != 0 {
		t.Errorf("an applied update is still offered: %+v", pending)
	}
	removed, _, _ := record.Without("is-number")
	if pending := removed.Pending(offers); len(pending) != 0 {
		t.Errorf("a removed extension is still offered: %+v", pending)
	}
	if pending := record.Pending(Offers{Version: OffersVersion}); len(pending) != 0 {
		t.Errorf("an unchecked profile offered %+v", pending)
	}
}

// TestAnOfferNobodyAskedForIsMadeOncePerChange is what keeps the end of a run
// worth reading: an unsolicited offer appears when a check moved the answer,
// and says nothing when it would repeat one the user has already declined.
func TestAnOfferNobodyAskedForIsMadeOncePerChange(t *testing.T) {
	installed := Extension{
		Name:      "is-number",
		Version:   "6.0.0",
		Integrity: "sha512-" + strings.Repeat("B", 86) + "==",
		Directory: "is-number-5e0e83b1",
	}
	record := Record{Version: RecordVersion}.With(installed)
	never := Offers{Version: OffersVersion}
	offered := NewOffers(time.Now(), []Offer{{Name: "is-number", Version: "7.0.0"}})

	changed := record.PendingChange(never, offered)
	if len(changed) != 1 || changed[0].Available != "7.0.0" {
		t.Fatalf("the first check to find an update reported %+v", changed)
	}
	if repeated := record.PendingChange(offered, offered); repeated != nil {
		t.Errorf("a check that moved nothing spoke again: %+v", repeated)
	}

	// Applied before the check, and withdrawn by the registry, both mean there
	// is nothing to say whatever the check before them had found.
	applied := installed
	applied.Version = "7.0.0"
	if spoke := record.With(applied).PendingChange(never, offered); spoke != nil {
		t.Errorf("an already applied update was offered: %+v", spoke)
	}
	withdrawn := NewOffers(time.Now(), []Offer{{Name: "is-number", Version: "6.0.0"}})
	if spoke := record.PendingChange(offered, withdrawn); spoke != nil {
		t.Errorf("a registry that came back to the pin was reported: %+v", spoke)
	}

	// A second package picking up an offer is a change, and the standing one
	// rides along rather than being dropped for having been seen.
	both := NewOffers(time.Now(), []Offer{
		{Name: "is-number", Version: "7.0.0"},
		{Name: planMode().Name, Version: "2.0.0"},
	})
	if rode := record.With(planMode()).PendingChange(offered, both); len(rode) != 2 {
		t.Errorf("a new offer left the standing one out: %+v", rode)
	}
}
