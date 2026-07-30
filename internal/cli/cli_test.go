package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mpizenberg/pisafe/internal/gitstage"
	"github.com/mpizenberg/pisafe/internal/runcopy"
	"github.com/mpizenberg/pisafe/internal/runctl"
	"github.com/mpizenberg/pisafe/internal/runstart"
	"github.com/mpizenberg/pisafe/internal/runstate"
)

func TestListWithNoRuns(t *testing.T) {
	t.Setenv("PISAFE_STATE_DIR", t.TempDir())
	var output bytes.Buffer
	if err := Run(context.Background(), []string{"list"}, nil, &output); err != nil {
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
		ProjectKey:         "project-3f9c2a1b",
		ActiveLimitSeconds: 8 * 60 * 60,
		Snapshot: gitstage.Snapshot{
			RunID:   "run-123",
			WorkRef: "refs/heads/work/run-123",
		},
	}); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := Run(context.Background(), []string{"list"}, nil, &output); err != nil {
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
		if err := Run(context.Background(), args, nil, &output); err == nil ||
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

// Nothing may reach a run that is not currently running, whichever route asks.
func TestInactiveRunIsRefusedBeforeAnythingLaunches(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PISAFE_STATE_DIR", root)
	store := runstate.NewStore(root)
	if _, err := store.Create(runstate.Manifest{
		RunID:              "inactive-run",
		Project:            "project",
		ProjectKey:         "project-3f9c2a1b",
		ActiveLimitSeconds: 8 * 60 * 60,
		Snapshot: gitstage.Snapshot{
			RunID:   "inactive-run",
			WorkRef: "refs/heads/work/inactive-run",
		},
	}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"zed", "inactive-run"},
		{"connect", "inactive-run"},
		{"connect", "inactive-run", "--shell"},
	} {
		err := Run(context.Background(), args, nil, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "not active") {
			t.Errorf("Run(%v) error = %v", args, err)
		}
	}
	if err := Run(context.Background(), []string{"connect", "missing-run"}, nil, io.Discard); err == nil {
		t.Error("connect to an unknown run succeeded")
	}
}

// A stopped run is the one inactive state the user can act on, so its refusal
// names the command that fixes it.
func TestConnectPointsAStoppedRunAtResume(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PISAFE_STATE_DIR", root)
	store := runstate.NewStore(root)
	if _, err := store.Create(runstate.Manifest{
		RunID:              "stopped-run",
		Project:            "project",
		ProjectKey:         "project-3f9c2a1b",
		ActiveLimitSeconds: 8 * 60 * 60,
		Snapshot:           gitstage.Snapshot{RunID: "stopped-run", WorkRef: "refs/heads/work/stopped-run"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Activate(
		"stopped-run",
		runstate.SSHConnection{
			Alias:          "pisafe-stopped-run",
			IdentityFile:   "/state/id_ed25519",
			KnownHostsFile: "/state/known_hosts",
			ConfigFile:     "/state/ssh.config",
			HostKeyFingerprint: "SHA256:" + base64.RawStdEncoding.EncodeToString(
				bytes.Repeat([]byte{7}, 32),
			),
		},
		gitstage.Snapshot{BaselineCommit: strings.Repeat("a", 40)},
		"pisafe-cap-"+strings.Repeat("b", 64),
		time.Now(),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Stop("stopped-run", time.Now()); err != nil {
		t.Fatal(err)
	}
	err := Run(context.Background(), []string{"connect", "stopped-run"}, nil, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "pisafe resume stopped-run") {
		t.Fatalf("error = %v", err)
	}
}

func TestParseConnectRequest(t *testing.T) {
	request, err := parseConnectRequest([]string{"run-123"})
	if err != nil {
		t.Fatal(err)
	}
	if request.runID != "run-123" || request.shell {
		t.Fatalf("request = %#v", request)
	}

	shell, err := parseConnectRequest([]string{"--shell", "run-123"})
	if err != nil {
		t.Fatal(err)
	}
	if shell.runID != "run-123" || !shell.shell {
		t.Fatalf("request = %#v", shell)
	}

	for name, args := range map[string][]string{
		"no run":         {},
		"only an option": {"--shell"},
		"two runs":       {"run-123", "run-124"},
		"unknown option": {"run-123", "--root"},
	} {
		if _, err := parseConnectRequest(args); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// The remote command is executed by a shell inside the run, so the workspace
// path must reach it as one word whatever the project is called.
func TestConnectArgvStartsPiOrAShellInTheWorkspace(t *testing.T) {
	manifest := runstate.Manifest{
		Workspace: "/work/my project",
		SSH: &runstate.SSHConnection{
			Alias:      "pisafe-run-123",
			ConfigFile: "/Users/alice/Library/Application Support/pisafe/ssh.config",
		},
	}
	agent := connectArgv(manifest, false)
	expected := []string{
		"ssh",
		"-F", "/Users/alice/Library/Application Support/pisafe/ssh.config",
		"-t",
		"pisafe-run-123",
		`cd '/work/my project' && exec pi`,
	}
	if !slices.Equal(agent, expected) {
		t.Fatalf("argv = %#v, want %#v", agent, expected)
	}
	shell := connectArgv(manifest, true)
	if shell[len(shell)-1] != `cd '/work/my project' && exec "$SHELL" -l` {
		t.Fatalf("shell command = %q", shell[len(shell)-1])
	}
}

func TestDiscardRequiresExactRepeatedRunID(t *testing.T) {
	var output bytes.Buffer
	for _, args := range [][]string{
		{"discard", "run-123"},
		{"discard", "run-123", "--confirm", "run-124"},
	} {
		err := Run(context.Background(), args, nil, &output)
		if err == nil || !strings.Contains(err.Error(), "confirm") {
			t.Fatalf("Run(%v) error = %v", args, err)
		}
	}
}

// TestCacheTakesOnlyReset keeps a mistyped subcommand from reaching the VM,
// where the only cache operation is destructive.
func TestCacheTakesOnlyReset(t *testing.T) {
	var output bytes.Buffer
	for _, args := range [][]string{{"cache"}, {"cache", "clear"}, {"cache", "reset", "npm"}} {
		err := Run(context.Background(), args, nil, &output)
		if err == nil || !strings.Contains(err.Error(), "usage: pisafe cache reset") {
			t.Fatalf("Run(%v) error = %v", args, err)
		}
	}
}

// TestExtensionRefusesWhatItCannotPinBeforeReachingTheVM keeps a spec that
// cannot be pinned from becoming a container argument, and keeps a source
// pisafe cannot pin at all — a git repository, a path — from looking supported.
func TestExtensionRefusesWhatItCannotPinBeforeReachingTheVM(t *testing.T) {
	var output bytes.Buffer
	for _, args := range [][]string{
		{"extension"},
		{"extension", "install"},
		{"extension", "install", "a", "b"},
		{"extension", "list", "extra"},
	} {
		err := Run(context.Background(), args, nil, &output)
		if err == nil || !strings.Contains(err.Error(), "usage: pisafe extension") {
			t.Fatalf("Run(%v) error = %v", args, err)
		}
	}
	// An update names packages rather than a spec, and every name is bounded
	// before the command reaches the VM at all.
	err := Run(context.Background(), []string{"extension", "update", "is-number;id"}, nil, &output)
	if err == nil || !strings.Contains(err.Error(), "is not an npm package name") {
		t.Fatalf("update with an unpinnable name: error = %v", err)
	}
	for name, spec := range map[string]string{
		"range":       "is-number@^7.0.0",
		"tag":         "is-number@latest",
		"git source":  "git:github.com/user/repo",
		"local path":  "/absolute/path",
		"npm prefix":  "npm:is-number@7.0.0",
		"empty":       "",
		"shell metac": "is-number;id",
	} {
		if err := validateExtensionSpec(spec); err == nil {
			t.Errorf("%s %q was accepted", name, spec)
		}
	}
	for _, spec := range []string{
		"is-number",
		"is-number@7.0.0",
		"@earendil-works/plan-mode",
		"@earendil-works/plan-mode@1.2.3",
	} {
		if err := validateExtensionSpec(spec); err != nil {
			t.Errorf("%q: %v", spec, err)
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

func TestPrintCollectionSeparatesWhatWasDoneFromWhatWasKept(t *testing.T) {
	plan := runctl.GCPlan{
		Reclaimed: []string{"run-imported"},
		Kept: []runctl.KeptRun{{
			RunID:  "run-stopped",
			Reason: "stopped with work that was never imported",
		}},
	}
	var done bytes.Buffer
	printCollection(&done, plan, []string{"sha256:abc"}, false)
	for _, expected := range []string{
		"Reclaimed:",
		"run-imported",
		"pisafe/RUN branches keep the work",
		"Pruned:",
		"sha256:abc",
		"Kept:",
		"run-stopped (stopped with work that was never imported)",
	} {
		if !strings.Contains(done.String(), expected) {
			t.Errorf("collection output lacks %q:\n%s", expected, done.String())
		}
	}

	// A preview must never read as though anything was removed.
	var preview bytes.Buffer
	printCollection(&preview, plan, []string{"sha256:abc"}, true)
	for _, expected := range []string{"Would reclaim:", "Would prune:"} {
		if !strings.Contains(preview.String(), expected) {
			t.Errorf("dry-run output lacks %q:\n%s", expected, preview.String())
		}
	}
	for _, unexpected := range []string{"Reclaimed:", "Pruned:"} {
		if strings.Contains(preview.String(), unexpected) {
			t.Errorf("dry-run output claims %q:\n%s", unexpected, preview.String())
		}
	}

	var empty bytes.Buffer
	printCollection(&empty, runctl.GCPlan{}, nil, false)
	if empty.String() != "Nothing to collect.\n" {
		t.Fatalf("empty collection = %q", empty.String())
	}
}

func TestGCRejectsUnknownArguments(t *testing.T) {
	for _, args := range [][]string{{"--force"}, {"--dry-run", "extra"}, {"run-123"}} {
		if err := Run(context.Background(), append([]string{"gc"}, args...), nil, io.Discard); err == nil ||
			!strings.Contains(err.Error(), "usage: pisafe gc") {
			t.Errorf("gc %v error = %v", args, err)
		}
	}
}

func TestParseApplyRequest(t *testing.T) {
	for _, testCase := range []struct {
		args     []string
		baseline gitstage.BaselineChoice
	}{
		{args: []string{"run-123"}},
		{args: []string{"run-123", "--keep-baseline"}, baseline: gitstage.KeepBaseline},
		{args: []string{"--drop-baseline", "run-123"}, baseline: gitstage.DropBaseline},
	} {
		request, err := parseApplyRequest(testCase.args)
		if err != nil {
			t.Fatalf("parseApplyRequest(%v) error = %v", testCase.args, err)
		}
		if request.runID != "run-123" || request.baseline != testCase.baseline {
			t.Fatalf("parseApplyRequest(%v) = %#v", testCase.args, request)
		}
	}
	for _, args := range [][]string{{}, {"a", "b"}, {"run-123", "--replay"}} {
		if _, err := parseApplyRequest(args); err == nil {
			t.Fatalf("parseApplyRequest(%v) was accepted", args)
		}
	}
}

// A run is imported once, so the question is asked exactly where an answer can
// still change the outcome.
func TestBaselineIsDecidedOnceBeforeAnythingIsCaptured(t *testing.T) {
	dirty := runstate.Manifest{
		RunID: "dirty-run",
		Snapshot: gitstage.Snapshot{
			BaselineCommit: strings.Repeat("a", 40),
			SourceHead:     strings.Repeat("b", 40),
		},
	}
	for _, testCase := range []struct {
		answer   string
		expected gitstage.BaselineChoice
	}{
		{answer: "keep\n", expected: gitstage.KeepBaseline},
		{answer: "drop\n", expected: gitstage.DropBaseline},
		{answer: "yes\n drop \n", expected: gitstage.DropBaseline},
	} {
		var output bytes.Buffer
		choice, err := decideBaseline(dirty, "", strings.NewReader(testCase.answer), &output)
		if err != nil || choice != testCase.expected {
			t.Fatalf("answer %q gave %q, %v", testCase.answer, choice, err)
		}
		if !strings.Contains(output.String(), "[keep/drop]") {
			t.Fatalf("output = %q", output.String())
		}
	}
	if _, err := decideBaseline(dirty, "", strings.NewReader(""), io.Discard); err == nil ||
		!strings.Contains(err.Error(), "--drop-baseline") {
		t.Fatalf("error = %v", err)
	}
	// An answer given on the command line replaces the question, not the check.
	choice, err := decideBaseline(dirty, gitstage.DropBaseline, nil, io.Discard)
	if err != nil || choice != gitstage.DropBaseline {
		t.Fatalf("choice = %q, %v", choice, err)
	}
}

func TestBaselineIsNotOfferedWhenItCannotBeLeftOut(t *testing.T) {
	clean := runstate.Manifest{RunID: "clean-run"}
	var output bytes.Buffer
	choice, err := decideBaseline(clean, "", nil, &output)
	if err != nil || choice != gitstage.KeepBaseline || output.Len() != 0 {
		t.Fatalf("choice = %q, %v, output = %q", choice, err, output.String())
	}
	if _, err := decideBaseline(clean, gitstage.DropBaseline, nil, io.Discard); err == nil ||
		!strings.Contains(err.Error(), "no baseline commit") {
		t.Fatalf("error = %v", err)
	}

	entangled := runstate.Manifest{
		RunID: "submodule-run",
		Snapshot: gitstage.Snapshot{
			BaselineCommit: strings.Repeat("a", 40),
			Submodules: []gitstage.SubmoduleStage{
				{Path: "dependency", BaselineCommit: strings.Repeat("c", 40)},
			},
		},
	}
	output.Reset()
	choice, err = decideBaseline(entangled, "", nil, &output)
	if err != nil || choice != gitstage.KeepBaseline {
		t.Fatalf("choice = %q, %v", choice, err)
	}
	if !strings.Contains(output.String(), "1 submodule(s)") {
		t.Fatalf("output = %q", output.String())
	}
	if _, err := decideBaseline(entangled, gitstage.DropBaseline, nil, io.Discard); err == nil ||
		!strings.Contains(err.Error(), "cannot be left out") {
		t.Fatalf("error = %v", err)
	}

	// A verified plan already decided this, and replaying it is the only thing
	// left to do.
	planned := runstate.Manifest{
		RunID:    "planned-run",
		Snapshot: gitstage.Snapshot{BaselineCommit: strings.Repeat("a", 40)},
		Apply:    &gitstage.PlannedApply{},
	}
	if choice, err := decideBaseline(planned, "", nil, io.Discard); err != nil ||
		choice != gitstage.KeepBaseline {
		t.Fatalf("choice = %q, %v", choice, err)
	}
	if _, err := decideBaseline(planned, gitstage.DropBaseline, nil, io.Discard); err == nil ||
		!strings.Contains(err.Error(), "import plan") {
		t.Fatalf("error = %v", err)
	}
}

func TestPrintReplayConflictQuotesPathsAndNamesEveryWayForward(t *testing.T) {
	var output bytes.Buffer
	printReplayConflict(&output, "run-123", &gitstage.BaselineReplayConflict{
		Paths: []string{"src/a b.go", "README.md"},
	})
	for _, expected := range []string{
		`"src/a b.go"`,
		`"README.md"`,
		"pisafe apply run-123 --keep-baseline",
		"pisafe resume run-123",
	} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("output = %q, missing %s", output.String(), expected)
		}
	}
}
