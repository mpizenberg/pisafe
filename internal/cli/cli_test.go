package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mpizenberg/pisafe/internal/gitstage"
	"github.com/mpizenberg/pisafe/internal/runstart"
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
		RunID:              "run-123",
		Project:            "project",
		ActiveLimitSeconds: 8 * 60 * 60,
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

func TestPrintRunResultShowsExactConnectionAndExclusions(t *testing.T) {
	var output bytes.Buffer
	result := runstart.Result{
		Manifest: runstate.Manifest{
			RunID:     "project-run",
			Workspace: "/work/project",
			Snapshot: gitstage.Snapshot{
				BaselineCommit: strings.Repeat("a", 40),
				WorkRef:        "refs/heads/work/project-run",
			},
			SSH: &runstate.SSHConnection{
				Alias:      "pisafe-project-run",
				ConfigFile: "/Users/alice/Library/Application Support/pisafe/ssh.config",
			},
		},
		Excluded: gitstage.ExcludedInputs{
			Untracked: []string{"local.txt"},
			Ignored:   []string{"build/one", "build/two"},
		},
	}
	if err := printRunResult(&output, result); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"Run:       project-run",
		"Workspace: /work/project",
		"Branch:    work/project-run",
		"ssh -F '/Users/alice/Library/Application Support/pisafe/ssh.config' pisafe-project-run",
		"tracked working-tree changes were flattened",
		"1 untracked, 2 ignored",
		"pisafe zed project-run",
		"inference unavailable",
	} {
		if !strings.Contains(output.String(), expected) {
			t.Errorf("output lacks %q:\n%s", expected, output.String())
		}
	}
}

func TestPackagedGuestPathUsesExplicitOverride(t *testing.T) {
	expected := filepath.Join(t.TempDir(), "guest")
	t.Setenv(guestHelperEnvironment, expected)
	actual, err := packagedGuestPath()
	if err != nil {
		t.Fatal(err)
	}
	if actual != expected {
		t.Fatalf("path = %q, want %q", actual, expected)
	}
}

func TestShellQuote(t *testing.T) {
	if actual := shellQuote("simple-path"); actual != "simple-path" {
		t.Fatalf("simple quote = %q", actual)
	}
	if actual := shellQuote("it's here"); actual != `'it'\''s here'` {
		t.Fatalf("complex quote = %q", actual)
	}
}

func TestZedRejectsInactiveRunBeforeLaunching(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PISAFE_STATE_DIR", root)
	store := runstate.NewStore(root)
	if _, err := store.Create(runstate.Manifest{
		RunID:              "inactive-run",
		Project:            "project",
		ActiveLimitSeconds: 8 * 60 * 60,
		Snapshot: gitstage.Snapshot{
			RunID:   "inactive-run",
			WorkRef: "refs/heads/work/inactive-run",
		},
	}); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err := Run(context.Background(), []string{"zed", "inactive-run"}, &output)
	if err == nil || !strings.Contains(err.Error(), "not active") {
		t.Fatalf("error = %v", err)
	}
}

func TestDiscardRequiresExactRepeatedRunID(t *testing.T) {
	var output bytes.Buffer
	for _, args := range [][]string{
		{"discard", "run-123"},
		{"discard", "run-123", "--confirm", "run-124"},
	} {
		err := Run(context.Background(), args, &output)
		if err == nil || !strings.Contains(err.Error(), "confirm") {
			t.Fatalf("Run(%v) error = %v", args, err)
		}
	}
}
