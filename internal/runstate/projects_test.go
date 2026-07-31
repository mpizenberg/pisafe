package runstate

import (
	"os"
	"path/filepath"
	"strings"
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

	records, err := store.ListProjects()
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
	records, err = store.ListProjects()
	if err != nil {
		t.Fatal(err)
	}
	if records[0].MissingSince == nil || !records[0].MissingSince.Equal(missing) {
		t.Fatalf("record = %#v", records[0])
	}
	if err := store.RegisterProject(project); err != nil {
		t.Fatal(err)
	}
	records, err = store.ListProjects()
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
	records, err = store.ListProjects()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("records = %#v", records)
	}
}

// TestAProjectRecordThatDoesNotFollowFromItsCheckoutIsRefused matters because
// the record is what a sweep deletes a whole filesystem on the strength of. A
// key that does not follow from the root it is filed with describes some other
// project, and acting on it would reclaim the wrong one.
func TestAProjectRecordThatDoesNotFollowFromItsCheckoutIsRefused(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	projects := filepath.Join(root, "projects")
	if err := os.MkdirAll(projects, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"widget-00000000": `{"version":1,"key":"widget-00000000","root":"/tmp/widget"}`,
		"widget-11111111": `{"version":1,"key":"other-11111111","root":"/tmp/widget"}`,
		"widget-22222222": `{"version":99,"key":"widget-22222222","root":"/tmp/widget"}`,
	} {
		path := filepath.Join(projects, name+".json")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := store.ListProjects(); err == nil {
			t.Errorf("%s was listed", name)
		} else if !strings.Contains(err.Error(), "widget-") {
			t.Errorf("%s: error does not say which record: %v", name, err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
}
