package profile

import (
	"slices"
	"strings"
	"testing"
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
		if configured := record.Configure("/work/project"); len(configured.Packages) != 0 {
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
	configured := parsed.Configure("/work/project")
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
