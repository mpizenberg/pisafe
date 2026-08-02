package profile

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func planMode() Pin {
	return Pin{
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
		"misplaced": {Version: RecordVersion, Extensions: []Pin{misplaced}},
		"unpinned":  {Version: RecordVersion, Extensions: []Pin{unpinned}},
		"unhashed":  {Version: RecordVersion, Extensions: []Pin{unhashed}},
		"climbing":  {Version: RecordVersion, Extensions: []Pin{climbing}},
		"twice": {
			Version:    RecordVersion,
			Extensions: []Pin{planMode(), planMode()},
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
	second := Pin{
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

	installed := Pin{
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
	installed := Pin{
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

func ripgrep() Tool {
	return Tool{
		Pin: Pin{
			Name:      "ripgrep",
			Version:   "14.1.1",
			Integrity: "sha512-" + strings.Repeat("A", 86) + "==",
			Directory: "ripgrep-33165223",
		},
		Binaries: []string{"rg"},
	}
}

// TestAToolIsAPinAndTheNamesItClaims is what separates a tool from an
// extension. The pin is held to the same rules; what is new is that a package
// with no command is not a tool at all, and that a name it claims has to be
// something pisafe can put in a path and print.
func TestAToolIsAPinAndTheNamesItClaims(t *testing.T) {
	silent := ripgrep()
	silent.Binaries = nil
	hidden := ripgrep()
	hidden.Binaries = []string{".hidden"}
	climbing := ripgrep()
	climbing.Binaries = []string{"../../bin/sh"}
	repeated := ripgrep()
	repeated.Binaries = []string{"rg", "rg"}
	misplaced := ripgrep()
	misplaced.Directory = "somewhere-else-71e17b6c"

	for name, tool := range map[string]Tool{
		"no command":       silent,
		"hidden command":   hidden,
		"climbing command": climbing,
		"repeated command": repeated,
		"misplaced":        misplaced,
	} {
		if err := tool.Validate(); err == nil {
			t.Errorf("%s was accepted as a tool", name)
		}
		encoded, err := Tools{Version: ToolsVersion, Tools: []Tool{tool}}.Encode()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ParseTools(encoded); err == nil {
			t.Errorf("%s survived the record", name)
		}
	}
	if err := ripgrep().Validate(); err != nil {
		t.Errorf("a pinned tool with one command was refused: %v", err)
	}
}

// TestTwoToolsMayNotAnswerToOneName is what stops installing a second tool from
// quietly changing what a command already on every run's PATH means. Which
// names a package claims is the package's own choice, so the collision is
// reported rather than resolved.
func TestTwoToolsMayNotAnswerToOneName(t *testing.T) {
	installed := Tools{Version: ToolsVersion}.With(ripgrep())
	rival := Tool{
		Pin: Pin{
			Name:      "rg-lookalike",
			Version:   "1.0.0",
			Integrity: "sha512-" + strings.Repeat("B", 86) + "==",
			Directory: "rg-lookalike-f9d011a6",
		},
		Binaries: []string{"rg", "rgl"},
	}
	conflicts := installed.Conflicts(rival)
	if len(conflicts) != 1 || conflicts[0] != (Conflict{Binary: "rg", Holder: "ripgrep"}) {
		t.Fatalf("conflicts = %+v", conflicts)
	}
	// Replacing a tool with another release of itself is not a collision, or no
	// tool could ever be reinstalled.
	newer := ripgrep()
	newer.Version = "14.1.2"
	if conflicts := installed.Conflicts(newer); len(conflicts) != 0 {
		t.Errorf("a tool collided with itself: %+v", conflicts)
	}
	// Two tools claiming one name cannot arrive through the record either, which
	// is what a record hand-edited between installs would do.
	encoded, err := Tools{Version: ToolsVersion, Tools: []Tool{ripgrep(), rival}}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseTools(encoded); err == nil {
		t.Error("a record claiming one name twice was accepted")
	}
}

// TestTheLinksARunSearchesAreDerivedFromTheRecord is why nothing edits that
// directory. It is rebuilt whole every time, so a name that is no longer
// claimed cannot survive the tool that claimed it.
func TestTheLinksARunSearchesAreDerivedFromTheRecord(t *testing.T) {
	fd := Tool{
		Pin: Pin{
			Name:      "fd-find",
			Version:   "10.2.0",
			Integrity: "sha512-" + strings.Repeat("C", 86) + "==",
			Directory: "fd-find-35860d41",
		},
		Binaries: []string{"fd", "fdfind"},
	}
	installed := Tools{Version: ToolsVersion}.With(ripgrep()).With(fd)
	links := installed.Links()
	expected := []Link{
		{Binary: "fd", Directory: fd.Directory},
		{Binary: "fdfind", Directory: fd.Directory},
		{Binary: "rg", Directory: ripgrep().Directory},
	}
	if !slices.Equal(links, expected) {
		t.Fatalf("links = %+v", links)
	}
	remaining, removed, found := installed.Without("fd-find")
	if !found || removed.Name != "fd-find" {
		t.Fatalf("removed %+v, found %v", removed, found)
	}
	if links := remaining.Links(); !slices.Equal(links, expected[2:]) {
		t.Errorf("removing a tool left %+v", links)
	}
}

// TestAToolRecordSurvivesStorage keeps what a tool claims addressable after a
// round trip, because the directory a run searches is rebuilt from it long
// after the install that wrote it.
func TestAToolRecordSurvivesStorage(t *testing.T) {
	for name, content := range map[string][]byte{
		"absent": nil,
		"blank":  []byte("\n"),
	} {
		record, err := ParseTools(content)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(record.Tools) != 0 || record.Version != ToolsVersion {
			t.Errorf("%s: record = %+v", name, record)
		}
	}
	for name, content := range map[string][]byte{
		"future shape": []byte(`{"version":` + strings.Repeat("9", 3) + `,"tools":[]}`),
		"unknown field": []byte(
			`{"version":1,"tools":[],"binaries":["rg"]}`),
		"trailing data": []byte(`{"version":1,"tools":[]}{"version":1}`),
	} {
		if _, err := ParseTools(content); err == nil {
			t.Errorf("%s was accepted as a tool record", name)
		}
	}
	encoded, err := Tools{Version: ToolsVersion}.With(ripgrep()).Encode()
	if err != nil {
		t.Fatal(err)
	}
	stored, err := ParseTools(encoded)
	if err != nil {
		t.Fatal(err)
	}
	tool, found := stored.Find("ripgrep")
	if !found || !slices.Equal(tool.Binaries, []string{"rg"}) || tool.Version != "14.1.1" {
		t.Fatalf("stored %+v, found %v", tool, found)
	}
}

// TestARunsOwnPackagesAreReadBackWithoutBeingTrusted covers what stopping a run
// reports. The settings file is written inside the run, so the point is as much
// what is ignored as what is found: only the profile's own entries are dropped
// as pisafe's, and only an npm source becomes something pisafe offers to keep.
func TestARunsOwnPackagesAreReadBackWithoutBeingTrusted(t *testing.T) {
	const profileRoot = "/opt/pisafe/profile"
	settings := []byte(`{
		"theme": "dark",
		"packages": [
			"` + profileRoot + `/aardvark-cf9c1cb8/node_modules/aardvark",
			"npm:pi-web-access",
			"npm:@earendil-works/plan-mode@0.4.1",
			"npm:pi-web-access",
			"npm:Not A Name",
			"git:github.com/user/repo",
			{"source": "npm:not-a-string"},
			""
		]
	}`)
	installed := ReadSelfInstalled(settings, profileRoot)
	want := []SelfInstalled{
		{Source: "npm:pi-web-access", Name: "pi-web-access"},
		{Source: "npm:@earendil-works/plan-mode@0.4.1", Name: "@earendil-works/plan-mode"},
		{Source: "npm:Not A Name"},
		{Source: "git:github.com/user/repo"},
	}
	if !slices.Equal(installed, want) {
		t.Errorf("installed = %#v, want %#v", installed, want)
	}

	// Nothing here may fail: a run that wrote rubbish still has to stop.
	for name, content := range map[string][]byte{
		"empty":           nil,
		"not JSON":        []byte("{"),
		"packages absent": []byte(`{"theme":"dark"}`),
		"packages wrong":  []byte(`{"packages":"all of them"}`),
		"only profile's":  []byte(`{"packages":["` + profileRoot + `/a/node_modules/a"]}`),
	} {
		if found := ReadSelfInstalled(content, profileRoot); len(found) != 0 {
			t.Errorf("%s reported %#v", name, found)
		}
	}
}
