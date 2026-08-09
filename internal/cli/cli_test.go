package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mpizenberg/pisafe/internal/gitstage"
	"github.com/mpizenberg/pisafe/internal/profile"
	"github.com/mpizenberg/pisafe/internal/runcopy"
	"github.com/mpizenberg/pisafe/internal/runctl"
	"github.com/mpizenberg/pisafe/internal/runid"
	"github.com/mpizenberg/pisafe/internal/runssh"
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

// What a record says and what the VM shows are two different things, and only
// their combination is worth printing: a run recorded active that the VM has no
// container for is neither running nor out of time.
func TestListShowsDurableStateAgainstWhatTheVMHas(t *testing.T) {
	started := time.Now().UTC().Add(-9 * time.Hour)
	deadline := started.Add(8 * time.Hour)
	runs := []runstate.Manifest{
		{RunID: "run-123", Project: "project", State: runstate.StateCreating},
		{
			RunID: "run-up", Project: "project", State: runstate.StateActive,
			ActiveLimitSeconds: 8 * 60 * 60,
			ActiveStartedAt:    &started,
			ActiveDeadline:     &deadline,
		},
		{
			RunID: "run-gone", Project: "project", State: runstate.StateActive,
			ActiveLimitSeconds: 8 * 60 * 60,
			ActiveStartedAt:    &started,
			ActiveDeadline:     &deadline,
		},
	}
	// Both active runs are past the deadline they were given. The one the VM
	// still has spent that time; the one it does not have was not running for it.
	up := map[string]bool{"run-up": true}

	var asked bytes.Buffer
	if err := printRuns(&asked, runs, up, true); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"RUN", "run-123", "creating", "project",
		"active (limit reached)",
		"active (no container)",
	} {
		if !strings.Contains(asked.String(), expected) {
			t.Fatalf("output lacks %q:\n%s", expected, asked.String())
		}
	}

	// A VM that could not be asked leaves both readings open, so neither is
	// printed as though it had been settled.
	var unasked bytes.Buffer
	if err := printRuns(&unasked, runs, nil, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(unasked.String(), "no container") ||
		strings.Contains(unasked.String(), "limit reached") {
		t.Fatalf("unasked VM was reported as answered:\n%s", unasked.String())
	}
	if !strings.Contains(unasked.String(), "could not be asked") {
		t.Fatalf("output does not say the VM went unasked:\n%s", unasked.String())
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

func TestApplyRefusesTwoRuns(t *testing.T) {
	var output bytes.Buffer
	args := []string{"apply", "run-123", "run-124"}
	if err := Run(context.Background(), args, nil, &output); err == nil ||
		!strings.Contains(err.Error(), "usage") {
		t.Fatalf("Run(%v) error = %v", args, err)
	}
}

// A command that takes a run is usually about the checkout the user is standing
// in, and that checkout usually has one run. Guessing is only ever safe while it
// stays unambiguous: an imported run is finished with, so it is not a candidate,
// and two live ones are a question rather than a default.
func TestChooseProjectRunPicksTheCheckoutsOnlyLiveRun(t *testing.T) {
	project := runid.Project{Directory: "tessera", Key: "tessera-aabbccdd"}
	run := func(runID, key string, state runstate.State) runstate.Manifest {
		return runstate.Manifest{RunID: runID, ProjectKey: key, State: state}
	}
	elsewhere := run("elsewhere-run", "elsewhere-11223344", runstate.StateActive)
	imported := run("imported-run", project.Key, runstate.StateImported)
	stopped := run("stopped-run", project.Key, runstate.StateStopped)
	active := run("active-run", project.Key, runstate.StateActive)

	for name, runs := range map[string][]runstate.Manifest{
		"nothing at all":        {},
		"another project's run": {elsewhere},
		"only an imported run":  {elsewhere, imported},
	} {
		if _, err := chooseProjectRun(runs, project); err == nil ||
			!strings.Contains(err.Error(), "tessera has no live run") {
			t.Errorf("%s error = %v", name, err)
		}
	}

	runID, err := chooseProjectRun([]runstate.Manifest{elsewhere, imported, stopped}, project)
	if err != nil {
		t.Fatal(err)
	}
	if runID != "stopped-run" {
		t.Fatalf("runID = %q", runID)
	}

	_, err = chooseProjectRun([]runstate.Manifest{imported, stopped, active}, project)
	if err == nil || !strings.Contains(err.Error(), "tessera has 2 live runs") ||
		!strings.Contains(err.Error(), "active-run") ||
		!strings.Contains(err.Error(), "stopped-run") {
		t.Fatalf("two live runs error = %v", err)
	}
}

// A named run is never second-guessed: whatever the checkout holds, and whether
// or not there is one.
func TestResolveRunIDLeavesANamedRunAlone(t *testing.T) {
	runID, err := resolveRunID(context.Background(), "named-run")
	if err != nil || runID != "named-run" {
		t.Fatalf("resolveRunID(named-run) = %q, %v", runID, err)
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
		{"connect", "inactive-run", "--", "pi"},
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

func TestParseConnectRequestSeparatesTheRunFromTheCommand(t *testing.T) {
	request, err := parseConnectRequest([]string{"run-123"})
	if err != nil {
		t.Fatal(err)
	}
	if request.runID != "run-123" || len(request.command) != 0 {
		t.Fatalf("request = %#v", request)
	}

	// Everything after -- belongs to the run, including words pisafe would
	// otherwise have read as its own options.
	command, err := parseConnectRequest([]string{"run-123", "--", "pi", "--force"})
	if err != nil {
		t.Fatal(err)
	}
	if command.runID != "run-123" || !slices.Equal(command.command, []string{"pi", "--force"}) {
		t.Fatalf("request = %#v", command)
	}

	// A request naming no run is one the checkout has to answer, so parsing
	// leaves the name empty rather than refusing it.
	inferred, err := parseConnectRequest([]string{"--", "npm", "test"})
	if err != nil {
		t.Fatal(err)
	}
	if inferred.runID != "" || !slices.Equal(inferred.command, []string{"npm", "test"}) {
		t.Fatalf("request = %#v", inferred)
	}

	for name, args := range map[string][]string{
		"two runs":         {"run-123", "run-124"},
		"unknown option":   {"run-123", "--root"},
		"separator alone":  {"run-123", "--"},
		"run after option": {"--shell", "run-123"},
	} {
		if _, err := parseConnectRequest(args); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// The remote command is executed by a shell inside the run, so the workspace
// path must reach it as one word whatever the project is called, and the
// command's own words must reach it as the shell syntax they were written as.
func TestConnectArgvRunsInTheWorkspaceWithATerminalOnlyWhenInteractive(t *testing.T) {
	manifest := runstate.Manifest{
		Workspace: "/work/my project",
		SSH: &runstate.SSHConnection{
			Alias:      "pisafe-run-123",
			ConfigFile: "/Users/alice/Library/Application Support/pisafe/ssh.config",
		},
	}
	shell := connectArgv(manifest, nil, true)
	expected := []string{
		"ssh",
		"-F", "/Users/alice/Library/Application Support/pisafe/ssh.config",
		"-t",
		"pisafe-run-123",
		`cd '/work/my project' && exec "$SHELL" -l`,
	}
	if !slices.Equal(shell, expected) {
		t.Fatalf("argv = %#v, want %#v", shell, expected)
	}

	// A redirect written on the pisafe command line has to survive as syntax:
	// quoting it per word would send the run a program with that name.
	piped := connectArgv(manifest, []string{"cat > cf-analytics.json"}, false)
	if piped[len(piped)-1] != `cd '/work/my project' && cat > cf-analytics.json` {
		t.Fatalf("remote command = %q", piped[len(piped)-1])
	}
	if !slices.Contains(piped, "-T") || slices.Contains(piped, "-t") {
		t.Fatalf("a redirected copy asked for a terminal: %#v", piped)
	}

	// Every command of a list has to reach the run: an exec would run the first
	// and drop the rest, and would refuse a variable assignment outright.
	list := connectArgv(manifest, []string{"npm", "test;", "CI=1", "npm", "run", "build"}, false)
	if list[len(list)-1] != `cd '/work/my project' && npm test; CI=1 npm run build` {
		t.Fatalf("remote command = %q", list[len(list)-1])
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

// TestAMistypedScopeCommandNeverReachesTheVM covers every verb that removes
// something a scope holds. One of them removes session transcripts, which
// nothing reproduces, so none of them may be reached by a typo.
func TestAMistypedScopeCommandNeverReachesTheVM(t *testing.T) {
	var output bytes.Buffer
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"project"}, "usage: pisafe project"},
		{[]string{"project", "clear"}, "usage: pisafe project"},
		{[]string{"project", "reset", "one", "two"}, "usage: pisafe project"},
		{[]string{"project", "rebind"}, "usage: pisafe project"},
		{[]string{"project", "drop", "/tmp/gone"}, "exact confirmation"},
		{[]string{"project", "drop", "/tmp/gone", "--confirm", "/tmp/other"}, "does not exactly match"},
		{[]string{"profile"}, "usage: pisafe profile reset"},
		{[]string{"profile", "reset"}, "usage: pisafe profile reset"},
		{[]string{"profile", "clear", "--confirm"}, "usage: pisafe profile reset"},
		{[]string{"backup"}, "usage: pisafe backup"},
		{[]string{"backup", "one", "two"}, "usage: pisafe backup"},
		{[]string{"restore"}, "usage: pisafe restore"},
		{[]string{"restore", "one", "two"}, "usage: pisafe restore"},
	} {
		err := Run(context.Background(), test.args, nil, &output)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("Run(%v) error = %v", test.args, err)
		}
	}
}

// TestARestoreReadsTheBackupBeforeItStartsAnything keeps a mistyped path from
// costing a VM boot and a run image: what a restore is pointed at is a
// directory on the Mac, and a directory that is not a backup says so.
func TestARestoreReadsTheBackupBeforeItStartsAnything(t *testing.T) {
	var output bytes.Buffer
	err := Run(context.Background(), []string{"restore", t.TempDir()}, nil, &output)
	if err == nil || !strings.Contains(err.Error(), "holds no pisafe backup") {
		t.Fatalf("error = %v", err)
	}
}

// TestAStoreWhoseCheckoutIsGoneCanStillBeNamed is what makes a report of one
// worth printing: the key is a digest, the directory it was made from is gone,
// and the recorded checkout path is the only handle left to drop it by.
func TestAStoreWhoseCheckoutIsGoneCanStillBeNamed(t *testing.T) {
	t.Setenv("PISAFE_STATE_DIR", t.TempDir())
	root, err := runstate.DefaultRoot()
	if err != nil {
		t.Fatal(err)
	}
	store := runstate.NewStore(root)
	project, err := runid.NewProject("/tmp/pisafe-test/departed")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RegisterProject(project); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := Run(context.Background(), []string{"project", "list"}, nil, &output); err != nil {
		t.Fatal(err)
	}
	listing := output.String()
	if !strings.Contains(listing, project.Root) {
		t.Fatalf("listing does not name the checkout:\n%s", listing)
	}
	// A store no run refers to is what drop exists for, and saying so is the
	// difference between a report and a report with something to do about it.
	if !strings.Contains(listing, "idle") {
		t.Fatalf("listing does not say the store is unused:\n%s", listing)
	}
	if strings.Contains(listing, project.Key) {
		t.Fatalf("listing offers a digest as a handle:\n%s", listing)
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
		if err := validatePackageSpec(spec); err == nil {
			t.Errorf("%s %q was accepted", name, spec)
		}
	}
	for _, spec := range []string{
		"is-number",
		"is-number@7.0.0",
		"@earendil-works/plan-mode",
		"@earendil-works/plan-mode@1.2.3",
	} {
		if err := validatePackageSpec(spec); err != nil {
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
	if request.runID != "run-123" || request.inbound || request.runPath != "dist/index.html" ||
		request.localPath != "index.html" || request.force {
		t.Fatalf("request = %#v", request)
	}

	forced, err := parseCopyRequest([]string{"run-123:dist", "--force", "out"})
	if err != nil {
		t.Fatal(err)
	}
	if forced.localPath != "out" || !forced.force {
		t.Fatalf("request = %#v", forced)
	}

	// A source carrying no run leaves the name for the checkout to answer,
	// whether it is a bare path or an empty name before the colon.
	for _, args := range [][]string{{"dist/index.html"}, {":dist/index.html"}} {
		inferred, err := parseCopyRequest(args)
		if err != nil {
			t.Fatal(err)
		}
		if inferred.runID != "" || inferred.inbound ||
			inferred.runPath != "dist/index.html" || inferred.localPath != "index.html" {
			t.Fatalf("parseCopyRequest(%v) = %#v", args, inferred)
		}
	}

	for name, args := range map[string][]string{
		"absolute path":  {"run-123:/etc/passwd"},
		"climbing path":  {"run-123:../../secrets"},
		"whole run":      {"run-123:."},
		"unknown option": {"run-123:dist", "--all"},
		"too many":       {"run-123:dist", "out", "extra"},
		"nothing":        {},
		"neither end":    {"here.json", "there.json"},
		"both ends":      {"run-123:dist", "run-124:dist"},
	} {
		if _, err := parseCopyRequest(args); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// The colon marks the end that is in the run, so which side carries it is the
// whole of what says which way a copy goes.
func TestParseCopyRequestReadsTheColonAsTheDirection(t *testing.T) {
	request, err := parseCopyRequest([]string{"cf-analytics.json", "run-123:"})
	if err != nil {
		t.Fatal(err)
	}
	if !request.inbound || request.runID != "run-123" ||
		request.localPath != "cf-analytics.json" || request.runPath != "" {
		t.Fatalf("request = %#v", request)
	}

	named, err := parseCopyRequest([]string{"./out/dist", ":public/dist", "--force"})
	if err != nil {
		t.Fatal(err)
	}
	if !named.inbound || named.runID != "" || named.localPath != "./out/dist" ||
		named.runPath != "public/dist" || !named.force {
		t.Fatalf("request = %#v", named)
	}

	for name, args := range map[string][]string{
		"absolute destination": {"cf.json", "run-123:/etc/cron.d/x"},
		"climbing destination": {"cf.json", "run-123:../../elsewhere"},
	} {
		if _, err := parseCopyRequest(args); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// A credential going into a run is the one thing that voids what the run
// guarantees, so it costs a word rather than nothing.
func TestRunCopyRefusesACredentialShapedNameWithoutUnsafe(t *testing.T) {
	err := runCopy(context.Background(), []string{".env", "run-123:"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "--unsafe") {
		t.Fatalf("error = %v", err)
	}
	// The same name coming out of a run is the user reading their own run's
	// file, which needs no override.
	request, err := parseCopyRequest([]string{"run-123:.env"})
	if err != nil || request.inbound {
		t.Fatalf("request = %#v, err = %v", request, err)
	}
}

// A destination that is already a directory means inside it, which is what cp
// does and what a --force prompt should never be the answer to.
func TestParseCopyRequestPutsACopyInsideAnExistingDirectory(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "plans")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	request, err := parseCopyRequest([]string{"run-123:plans/note.md", directory})
	if err != nil {
		t.Fatal(err)
	}
	if request.localPath != filepath.Join(directory, "note.md") {
		t.Fatalf("destination = %q", request.localPath)
	}

	// A name that is not a directory is still taken literally, whether or not
	// something is already there.
	file := filepath.Join(root, "note.md")
	if err := os.WriteFile(file, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, destination := range []string{file, filepath.Join(root, "absent.md")} {
		request, err := parseCopyRequest([]string{"run-123:plans/note.md", destination})
		if err != nil {
			t.Fatal(err)
		}
		if request.localPath != destination {
			t.Fatalf("destination = %q, want %q", request.localPath, destination)
		}
	}
}

func TestPrintCopyResultQuotesNamesChosenInsideTheRun(t *testing.T) {
	var output bytes.Buffer
	printCopyResult(
		&output,
		copyRequest{runID: "run-123", runPath: "dist", localPath: "out"},
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
	missing := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	plan := runctl.GCPlan{
		Reclaimed: []string{"run-imported"},
		Kept: []runctl.KeptRun{{
			RunID:  "run-stopped",
			Reason: "stopped with work that was never imported",
		}},
		MissingProjects: []runstate.ProjectRecord{{
			Key:          "widget-00000000",
			Root:         "/Users/dev/widget",
			MissingSince: &missing,
		}},
		ReclaimedProjects: []runstate.ProjectRecord{{
			Key:          "gadget-11111111",
			Root:         "/Users/dev/gadget",
			MissingSince: &missing,
		}},
	}
	var done bytes.Buffer
	printCollection(&done, plan, []string{"sha256:abc"}, false)
	for _, expected := range []string{
		"Reclaimed:",
		"run-imported",
		"pisafe/RUN branches keep the work",
		`"/Users/dev/gadget" (missing since 2026-07-24)`,
		"Pruned:",
		"sha256:abc",
		"Missing:",
		`"/Users/dev/widget" (missing since 2026-07-24)`,
		"move one back to keep its transcripts",
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
	inferred, err := parseApplyRequest([]string{"--drop-baseline"})
	if err != nil {
		t.Fatal(err)
	}
	if inferred.runID != "" || inferred.baseline != gitstage.DropBaseline {
		t.Fatalf("parseApplyRequest([--drop-baseline]) = %#v", inferred)
	}
	for _, args := range [][]string{{"a", "b"}, {"run-123", "--replay"}} {
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

// A login is refused before it reaches the Keychain when what it says about
// the upstream cannot be true, so a stored key always names something the
// broker can actually route to.
func TestALoginDescribesItsUpstreamOrIsRefusedEarly(t *testing.T) {
	models := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(models, []byte(`[{"id":"local-a"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string][]string{
		"known provider given an endpoint": {"anthropic", "--url", "https://x.example"},
		"custom provider given nothing":    {"local"},
		"custom provider without models":   {"local", "--url", "https://x.example", "--api", "openai-responses"},
		"custom provider over plain http": {
			"local", "--url", "http://x.example", "--api", "openai-responses",
			"--models", models,
		},
		"custom provider with an unknown wire format": {
			"local", "--url", "https://x.example", "--api", "grpc", "--models", models,
		},
		"custom provider with no such model list": {
			"local", "--url", "https://x.example", "--api", "openai-responses",
			"--models", filepath.Join(t.TempDir(), "absent.json"),
		},
		"unknown option": {"local", "--endpoint", "https://x.example"},
		"option without a value": {
			"local", "--url", "https://x.example", "--api", "openai-responses", "--models",
		},
	} {
		if _, err := parseKeyedLogin(args[0], args[1:], io.Discard); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}

	known, err := parseKeyedLogin("openai", nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if known.Custom() || known.URL != "" || len(known.Models) != 0 {
		t.Fatalf("record = %#v", known)
	}
	custom, err := parseKeyedLogin(
		"local",
		[]string{"--url", "http://127.0.0.1:1234", "--api", "openai-completions", "--models", models},
		io.Discard,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !custom.Custom() || custom.Describe() != "http://127.0.0.1:1234" ||
		len(custom.Models) != 1 {
		t.Fatalf("record = %#v", custom)
	}
}

// The key is read from stdin because an argument is visible to every process
// on the Mac and stays in the shell's history.
func TestAKeyIsReadFromStdinAndMustNotBeBlank(t *testing.T) {
	for name, input := range map[string]string{
		"blank":          "   \n",
		"nothing at all": "",
		"only a newline": "\n",
	} {
		if _, err := readKey("openai", strings.NewReader(input), io.Discard); err == nil {
			t.Errorf("%s was accepted as a key", name)
		}
	}
	for name, input := range map[string]string{
		"with a newline":     " sk-test-key \n",
		"without a newline":  "sk-test-key",
		"with more after it": "sk-test-key\nnot the key\n",
	} {
		key, err := readKey("openai", strings.NewReader(input), io.Discard)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if key != "sk-test-key" {
			t.Errorf("%s: key = %q", name, key)
		}
	}
}

// TestPrintSelfInstalledQuotesWhatTheRunChose covers the report a stop makes
// about the run's own packages. Every source came from inside the run, so none
// of it reaches the terminal unquoted, and a source pisafe cannot pin is named
// rather than dropped.
func TestPrintSelfInstalledQuotesWhatTheRunChose(t *testing.T) {
	var output bytes.Buffer
	printSelfInstalled(&output, "project-run", []profile.SelfInstalled{
		{Source: "npm:pi-web-access", Name: "pi-web-access"},
		{Source: "git:github.com/user/repo\n\033[2Kfake line"},
	})
	for _, expected := range []string{
		"project-run installed 2 package(s) for itself",
		`"npm:pi-web-access"`,
		"pisafe extension install pi-web-access",
		`"git:github.com/user/repo\n\x1b[2Kfake line"`,
		"pisafe can only keep an npm package",
		"nothing is kept unless you install it",
	} {
		if !strings.Contains(output.String(), expected) {
			t.Errorf("output lacks %q:\n%s", expected, output.String())
		}
	}

	output.Reset()
	printSelfInstalled(&output, "project-run", nil)
	if output.Len() != 0 {
		t.Errorf("a run that installed nothing said %q", output.String())
	}
}

func TestZedConnectionsAreSavedAndReclaimedUnderTheSameAlias(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	settings := filepath.Join(home, ".config", "zed", "settings.json")
	runID := "tessera-20260804-134311-bf9fdd2fcdb6"
	manifest := runstate.Manifest{
		RunID:     runID,
		Workspace: "/work/tessera",
		SSH: &runstate.SSHConnection{
			Alias:      runssh.Alias(runID),
			ConfigFile: filepath.Join(home, "ssh", runID, "ssh.config"),
		},
	}

	path, added, err := saveZedConnection(manifest)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if path != settings || !added {
		t.Fatalf("saved to %q (added=%v), want %q", path, added, settings)
	}
	saved, err := os.ReadFile(settings)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	for _, expected := range []string{manifest.SSH.Alias, manifest.SSH.ConfigFile} {
		if !strings.Contains(string(saved), expected) {
			t.Fatalf("saved settings lack %q:\n%s", expected, saved)
		}
	}

	// Nothing is written twice, so opening a run again never waits for Zed to
	// reread settings it already has.
	if _, added, err := saveZedConnection(manifest); err != nil || added {
		t.Fatalf("saving again: added=%v err=%v", added, err)
	}

	var output bytes.Buffer

	// Removal derives the alias from the run ID alone, because a collected run
	// has no record left to read it from.
	forgetZedConnection(runID, &output)
	if output.Len() != 0 {
		t.Fatalf("reclaiming a saved connection warned: %s", output.String())
	}
	reclaimed, err := os.ReadFile(settings)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if strings.Contains(string(reclaimed), runID) {
		t.Fatalf("settings still hold the reclaimed run:\n%s", reclaimed)
	}
}
