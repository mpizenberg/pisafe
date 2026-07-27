package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mpizenberg/pisafe/internal/gitstage"
	"github.com/mpizenberg/pisafe/internal/runcopy"
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
		Included: []string{"fixtures/sample.json"},
	}
	if err := printRunResult(&output, result, false); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"Run:       project-run",
		"Workspace: /work/project",
		"Branch:    work/project-run",
		"ssh -F '/Users/alice/Library/Application Support/pisafe/ssh.config' pisafe-project-run",
		"tracked working-tree changes were flattened",
		"1 untracked, 2 ignored",
		"Included:  1 selected input file(s)",
		`"fixtures/sample.json"`,
		"pisafe zed project-run",
		"inference unavailable",
	} {
		if !strings.Contains(output.String(), expected) {
			t.Errorf("output lacks %q:\n%s", expected, output.String())
		}
	}

	output.Reset()
	if err := printRunResult(&output, result, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "pisafe broker") {
		t.Errorf("configured output lacks broker guidance:\n%s", output.String())
	}
}

func TestPrintApplyResultShowsTheImportedBranchAndWhatStayedBehind(t *testing.T) {
	var output bytes.Buffer
	printApplyResult(
		&output,
		runstate.Manifest{RunID: "project-run"},
		gitstage.ApplyResult{
			Branch:      "pisafe/project-run",
			Tip:         strings.Repeat("a", 40),
			FinalCommit: strings.Repeat("b", 40),
			Untracked:   []string{"scratch.md"},
			Submodules: []gitstage.AppliedSubmodule{
				{Path: "dependency", Branch: "pisafe/project-run", Tip: strings.Repeat("c", 40)},
				{Path: "vendor/quiet"},
			},
		},
	)
	for _, expected := range []string{
		"Imported:  pisafe/project-run",
		"Tip:       " + strings.Repeat("a", 40),
		"Submodule: dependency imported as pisafe/project-run",
		"Submodule: vendor/quiet unchanged",
		"uncommitted tracked changes became one labelled commit",
		"Left:      1 untracked file(s) stayed in the run",
		`"scratch.md"`,
		"git log pisafe/project-run",
		"pisafe discard project-run",
	} {
		if !strings.Contains(output.String(), expected) {
			t.Errorf("output lacks %q:\n%s", expected, output.String())
		}
	}
}

func TestApplyRequiresExactlyOneRun(t *testing.T) {
	var output bytes.Buffer
	for _, args := range [][]string{{"apply"}, {"apply", "run-123", "run-124"}} {
		if err := Run(context.Background(), args, &output); err == nil ||
			!strings.Contains(err.Error(), "usage") {
			t.Fatalf("Run(%v) error = %v", args, err)
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

// Everything in a diff was written inside the run, so nothing may reach the
// terminal unquoted, and a truncated list must say so.
func TestPrintRunDiffQuotesRunContentAndReportsTruncation(t *testing.T) {
	var output bytes.Buffer
	printRunDiff(&output, gitstage.RunDiff{
		RunID: "diff-run",
		Repositories: []gitstage.RepositoryDiff{{
			Base:        strings.Repeat("a", 40),
			Head:        strings.Repeat("b", 40),
			Commits:     []gitstage.DiffCommit{{Commit: strings.Repeat("c", 40), Subject: "fix\x1b[31m"}},
			CommitTotal: 3,
			Files: []gitstage.DiffFile{
				{Path: "src/main.go", Insertions: 12, Deletions: 4},
				{Path: "logo.png", Insertions: -1, Deletions: -1},
			},
			FileTotal:      2,
			Untracked:      []string{"scratch\nlog"},
			UntrackedTotal: 1,
		}, {
			Path: "dependency",
			Base: strings.Repeat("d", 40),
			Head: strings.Repeat("d", 40),
		}},
	})
	rendered := output.String()
	for _, expected := range []string{
		`"fix\x1b[31m"`,
		"... and 2 more",
		"+12/-4 \"src/main.go\"",
		"binary \"logo.png\"",
		`"scratch\nlog"`,
		`Submodule: "dependency"`,
	} {
		if !strings.Contains(rendered, expected) {
			t.Errorf("diff output lacks %q:\n%s", expected, rendered)
		}
	}
	if strings.Contains(rendered, "\x1b") {
		t.Fatalf("diff output carried an escape sequence:\n%q", rendered)
	}
}

func TestParseCopyRequestSeparatesRunPathAndDestination(t *testing.T) {
	request, err := parseCopyRequest([]string{"run-123:dist/index.html"})
	if err != nil {
		t.Fatal(err)
	}
	if request.runID != "run-123" || request.path != "dist/index.html" ||
		request.destination != "index.html" || request.force {
		t.Fatalf("request = %#v", request)
	}

	forced, err := parseCopyRequest([]string{"run-123:dist", "--force", "out"})
	if err != nil {
		t.Fatal(err)
	}
	if forced.destination != "out" || !forced.force {
		t.Fatalf("request = %#v", forced)
	}

	for name, args := range map[string][]string{
		"no path":        {"run-123"},
		"absolute path":  {"run-123:/etc/passwd"},
		"climbing path":  {"run-123:../../secrets"},
		"whole run":      {"run-123:."},
		"no run":         {":dist"},
		"unknown option": {"run-123:dist", "--all"},
		"too many":       {"run-123:dist", "out", "extra"},
		"nothing":        {},
	} {
		if _, err := parseCopyRequest(args); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestPrintCopyResultQuotesNamesChosenInsideTheRun(t *testing.T) {
	var output bytes.Buffer
	printCopyResult(
		&output,
		copyRequest{runID: "run-123", path: "dist", destination: "out"},
		[]runcopy.Entry{
			{Path: "dist", Directory: true},
			{Path: "dist/index.html", Size: 2048},
			{Path: "dist/weird\nname", Size: 1},
		},
	)
	rendered := output.String()
	for _, expected := range []string{
		`2.0 KiB "dist/index.html"`,
		`"dist/weird\nname"`,
		`2 file(s), 2.0 KiB to "out"`,
	} {
		if !strings.Contains(rendered, expected) {
			t.Errorf("copy output lacks %q:\n%s", expected, rendered)
		}
	}
}

func TestParseInputSelectionSeparatesUnsafeApproval(t *testing.T) {
	selection, err := parseInputSelection([]string{
		"--include", "notes.txt",
		"--include-unsafe", ".env",
		"--include", "fixtures",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(selection.Include, ",") != "notes.txt,fixtures" ||
		strings.Join(selection.Unsafe, ",") != ".env" {
		t.Fatalf("selection = %#v", selection)
	}

	for name, args := range map[string][]string{
		"unknown option": {"--all"},
		"missing path":   {"--include"},
		"bare path":      {"notes.txt"},
	} {
		if _, err := parseInputSelection(args); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}
