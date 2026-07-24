package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/mpizenberg/pisafe/internal/gitstage"
	"github.com/mpizenberg/pisafe/internal/runstate"
)

func TestListWithNoRuns(t *testing.T) {
	t.Setenv("PISAFE_STATE_DIR", t.TempDir())
	var output bytes.Buffer
	if err := Run(context.Background(), []string{"list"}, &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != "No runs.\n" {
		t.Fatalf("output = %q", output.String())
	}
}

func TestListShowsDurableState(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PISAFE_STATE_DIR", root)
	store := runstate.NewStore(root)
	if _, err := store.Create(runstate.Manifest{
		RunID:   "run-123",
		Project: "project",
		Snapshot: gitstage.Snapshot{
			RunID:   "run-123",
			WorkRef: "refs/heads/work/run-123",
		},
	}); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := Run(context.Background(), []string{"list"}, &output); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"RUN", "run-123", "creating", "project"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("output lacks %q:\n%s", expected, output.String())
		}
	}
}
