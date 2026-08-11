package gitstage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newIncludedRepository is a source whose ignored plans directory and untracked
// note are the two shapes an included root takes: one Git hides entirely, one
// it reports as untracked.
func newIncludedRepository(t *testing.T) string {
	t.Helper()
	source := newInputRepository(t)
	mustWrite(t, filepath.Join(source, ".gitignore"), "build/\n.env\nplans/\n")
	runGit(t, source, "commit", "-qm", "ignore plans", "--", ".gitignore")
	if err := os.Mkdir(filepath.Join(source, "plans"), 0o700); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(source, "plans", "carried.md"), "carried in\n")
	return source
}

func stageIncluded(t *testing.T, source, runID string) (Snapshot, string) {
	t.Helper()
	inputs, _, err := selectInputs(t, source, InputSelection{
		Include: []string{
			filepath.Join(source, "plans"),
			filepath.Join(source, "notes.txt"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(t.TempDir(), "workspace")
	snapshot, err := Stage(context.Background(), PrepareRequest{
		SourcePath: source,
		RunID:      runID,
		Inputs:     inputs,
	}, workspace)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot, workspace
}

func TestPrepareApplyCapturesWorkUnderIncludedRoots(t *testing.T) {
	ctx := context.Background()
	source := newIncludedRepository(t)
	snapshot, workspace := stageIncluded(t, source, "outputs-run")

	// The agent edits what it was given, creates more beside it, and commits
	// one plan deliberately.
	mustWrite(t, filepath.Join(workspace, "plans", "carried.md"), "edited in the run\n")
	mustWrite(t, filepath.Join(workspace, "plans", "created.md"), "created in the run\n")
	mustWrite(t, filepath.Join(workspace, "plans", "committed.md"), "kept as history\n")
	runGit(t, workspace, "add", "--force", "plans/committed.md")
	runGit(t, workspace, "commit", "-qm", "keep one plan as history")

	packageDir := filepath.Join(t.TempDir(), "package")
	prepared, err := PrepareApply(ctx, snapshot, workspace, packageDir, KeepBaseline)
	if err != nil {
		t.Fatal(err)
	}

	// A path the agent chose to track travels as history, so it is not also
	// handed back as a file.
	if inputPaths(prepared.Outputs) != "notes.txt,plans/carried.md,plans/created.md" {
		t.Fatalf("outputs = %#v", prepared.Outputs)
	}
	if prepared.OutputsSHA256 == "" {
		t.Fatal("outputs were not archived")
	}
	// What is coming back is not also reported as having stayed behind.
	if len(prepared.Untracked) != 0 {
		t.Fatalf("untracked = %#v", prepared.Untracked)
	}

	extracted, err := extractFileArchive(
		filepath.Join(packageDir, applyOutputsName),
		t.TempDir(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(extracted, ",") != "notes.txt,plans/carried.md,plans/created.md" {
		t.Fatalf("archive held %#v", extracted)
	}
}

// TestPrepareApplyReportsNoOutputsWithoutRoots keeps a run that was given no
// included paths from handing anything back: the copy-back channel exists only
// where the user opened it.
func TestPrepareApplyReportsNoOutputsWithoutRoots(t *testing.T) {
	ctx := context.Background()
	source := newInputRepository(t)
	workspace := filepath.Join(t.TempDir(), "workspace")
	snapshot, err := Stage(ctx, PrepareRequest{
		SourcePath: source,
		RunID:      "no-roots-run",
	}, workspace)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(workspace, "invented.md"), "run made this\n")

	prepared, err := PrepareApply(
		ctx,
		snapshot,
		workspace,
		filepath.Join(t.TempDir(), "package"),
		KeepBaseline,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared.Outputs) != 0 || prepared.OutputsSHA256 != "" {
		t.Fatalf("outputs = %#v", prepared.Outputs)
	}
	// It stays in the run, and is reported as such.
	if strings.Join(prepared.Untracked, ",") != "invented.md" {
		t.Fatalf("untracked = %#v", prepared.Untracked)
	}
}

// TestImportApplyRefusesOutputsOutsideIncludedRoots covers the boundary the
// outputs list crosses: it is written inside the run, so where each path
// belongs is proved on arrival rather than trusted.
func TestImportApplyRefusesOutputsOutsideIncludedRoots(t *testing.T) {
	ctx := context.Background()
	source := newIncludedRepository(t)
	snapshot, workspace := stageIncluded(t, source, "escape-run")
	packageDir := filepath.Join(t.TempDir(), "package")
	prepared, err := PrepareApply(ctx, snapshot, workspace, packageDir, KeepBaseline)
	if err != nil {
		t.Fatal(err)
	}

	for name, path := range map[string]string{
		"outside every root": "elsewhere.md",
		"climbing out":       "../escape.md",
		"into the Git dir":   ".git/hooks/post-checkout",
		"root as a prefix":   "plansible/other.md",
	} {
		tampered := prepared
		tampered.Outputs = append(
			append([]SelectedInput{}, prepared.Outputs...),
			SelectedInput{Path: path},
		)
		if _, err := ImportApply(ctx, snapshot, tampered, packageDir, KeepBaseline); err == nil {
			t.Errorf("%s (%q) was accepted", name, path)
		}
	}
}
