package runstate

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mpizenberg/pisafe/internal/runid"
)

// TestAProjectRecordSaysWhereAStoreCameFromUntilItIsReclaimed is what makes a
// project filesystem collectable at all: its key is a one-way digest, so the
// checkout it belongs to has to be written down while the checkout is still
// there to be seen.
func TestAProjectRecordSaysWhereAStoreCameFromUntilItIsReclaimed(t *testing.T) {
	store := NewStore(t.TempDir())
	checkout := filepath.Join(t.TempDir(), "widget")
	project, err := runid.NewProject(checkout)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RegisterProject(project); err != nil {
		t.Fatal(err)
	}

	records, _, err := store.ListProjects()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %#v", records)
	}
	if records[0].Key != project.Key || records[0].Root != checkout {
		t.Fatalf("record = %#v", records[0])
	}
	if records[0].MissingSince != nil {
		t.Fatalf("a registered project was already missing: %#v", records[0])
	}

	// A checkout that is not there starts a window rather than authorising a
	// removal, and one that comes back closes it: registering is what a run of
	// the project does, and a run proves the checkout is there.
	missing := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	if err := store.MarkProjectMissing(project.Key, missing); err != nil {
		t.Fatal(err)
	}
	records, _, err = store.ListProjects()
	if err != nil {
		t.Fatal(err)
	}
	if records[0].MissingSince == nil || !records[0].MissingSince.Equal(missing) {
		t.Fatalf("record = %#v", records[0])
	}
	if err := store.RegisterProject(project); err != nil {
		t.Fatal(err)
	}
	records, _, err = store.ListProjects()
	if err != nil {
		t.Fatal(err)
	}
	if records[0].MissingSince != nil {
		t.Fatalf("a checkout that came back stayed missing: %#v", records[0])
	}

	if err := store.ForgetProject(project.Key); err != nil {
		t.Fatal(err)
	}
	// Forgetting is the last step of reclaiming a store, and a sweep that fails
	// partway repeats it, so it may not object to having nothing left to do.
	if err := store.ForgetProject(project.Key); err != nil {
		t.Fatal(err)
	}
	records, _, err = store.ListProjects()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("records = %#v", records)
	}
}

// TestAProjectRecordThatCannotBeTrustedIsReportedNotReturned matters because a
// record is what a sweep deletes a whole filesystem on the strength of. A key
// that does not follow from the root it is filed with describes some other
// project, and acting on it would reclaim the wrong one — but the store behind
// it holds transcripts nothing reproduces, so it is handed to the caller as
// something to report rather than something to act on.
func TestAProjectRecordThatCannotBeTrustedIsReportedNotReturned(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	projects := filepath.Join(root, "projects")
	if err := os.MkdirAll(projects, 0o700); err != nil {
		t.Fatal(err)
	}
	// A sound record sits beside each bad one throughout: one file nothing can
	// read may not cost the listing every other store depends on.
	sound, err := runid.NewProject(filepath.Join(t.TempDir(), "sound"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RegisterProject(sound); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"widget-00000000": `{"version":1,"key":"widget-00000000","root":"/tmp/widget"}`,
		"widget-11111111": `{"version":1,"key":"other-11111111","root":"/tmp/widget"}`,
		"widget-22222222": `{"version":99,"key":"widget-22222222","root":"/tmp/widget"}`,
		"widget-33333333": `{`,
	} {
		path := filepath.Join(projects, name+".json")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		records, unreadable, err := store.ListProjects()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(records) != 1 || records[0].Key != sound.Key {
			t.Errorf("%s: records = %#v", name, records)
		}
		if len(unreadable) != 1 || unreadable[0].Key != name {
			t.Fatalf("%s: unreadable = %#v", name, unreadable)
		}
		if unreadable[0].Reason == "" {
			t.Errorf("%s: unreadable record gives no reason", name)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
}

// TestAFileNotNamedLikeAKeyIsNotAProjectRecord matters because everything a
// caller may do with an unreadable record — report it, reach its filesystem,
// remove it — goes through the key. A name no store could ever have been
// created under promises a store that is not behind it.
func TestAFileNotNamedLikeAKeyIsNotAProjectRecord(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	projects := filepath.Join(root, "projects")
	if err := os.MkdirAll(projects, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(projects, "not a key.json")
	if err := os.WriteFile(path, []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	records, unreadable, err := store.ListProjects()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 || len(unreadable) != 0 {
		t.Fatalf("records = %#v, unreadable = %#v", records, unreadable)
	}
}
