package runctl

import (
	"context"
	"strings"
	"testing"

	"github.com/mpizenberg/pisafe/internal/runstate"
)

// TestRestoringRecordsTheCheckoutBeforeAllocatingItsStore is the invariant a
// one-way project key depends on, at the one command whose whole job is putting
// stores back: a filesystem that exists before anything says where it came from
// could never afterwards be recognised as unused.
func TestRestoringRecordsTheCheckoutBeforeAllocatingItsStore(t *testing.T) {
	store := runstate.NewStore(t.TempDir())
	backend := &fakeBackend{}

	if err := New(backend, store, &fakeSSHStore{}, testInference{}).
		RestoreProject(context.Background(), testProject, strings.NewReader("archive")); err != nil {
		t.Fatal(err)
	}
	calls := callsString(backend.calls)
	want := "ensure-project-storage\nrestore-sessions " + testProject.Key + " archive"
	if calls != want {
		t.Fatalf("calls =\n%s\nwant:\n%s", calls, want)
	}
	recorded, err := store.HasProject(testProject.Key)
	if err != nil {
		t.Fatal(err)
	}
	if !recorded {
		t.Fatal("a restored store was left unattributable")
	}

	interrupted := runstate.NewStore(t.TempDir())
	if err := New(
		&fakeBackend{failAt: "ensure-project-storage"}, interrupted, &fakeSSHStore{}, testInference{},
	).RestoreProject(
		context.Background(), testProject, strings.NewReader("archive"),
	); err == nil {
		t.Fatal("a failed allocation was reported as a restore")
	}
	recorded, err = interrupted.HasProject(testProject.Key)
	if err != nil {
		t.Fatal(err)
	}
	if !recorded {
		t.Fatal("the record went in after the filesystem, leaving one nothing can name")
	}
}
