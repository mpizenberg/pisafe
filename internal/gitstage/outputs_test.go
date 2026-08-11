package gitstage

import (
	"context"
	"errors"
	"io/fs"
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

// captureRun is what a run hands back: the declared outputs and the archive
// holding them, ready for a copy into the source working tree.
func captureRun(t *testing.T, snapshot Snapshot, workspace string) ([]SelectedInput, string) {
	t.Helper()
	packageDir := filepath.Join(t.TempDir(), "package")
	prepared, err := PrepareApply(
		context.Background(),
		snapshot,
		workspace,
		packageDir,
		KeepBaseline,
	)
	if err != nil {
		t.Fatal(err)
	}
	return prepared.Outputs, filepath.Join(packageDir, OutputsArtifactName)
}

func TestCopyBackWritesRunWorkIntoTheWorkingTree(t *testing.T) {
	source := newIncludedRepository(t)
	snapshot, workspace := stageIncluded(t, source, "copyback-run")

	mustWrite(t, filepath.Join(workspace, "plans", "carried.md"), "edited in the run\n")
	mustWrite(t, filepath.Join(workspace, "plans", "created.md"), "created in the run\n")

	outputs, archive := captureRun(t, snapshot, workspace)
	result, err := CopyBack(snapshot, outputs, archive, false)
	if err != nil {
		t.Fatal(err)
	}
	// notes.txt came back unchanged on both sides, so it is not rewritten.
	if strings.Join(result.Written, ",") != "plans/carried.md,plans/created.md" {
		t.Fatalf("written = %#v", result.Written)
	}
	if len(result.Conflicts) != 0 || len(result.Kept) != 0 {
		t.Fatalf("result = %#v", result)
	}
	assertFile(t, filepath.Join(source, "plans", "carried.md"), "edited in the run\n")
	assertFile(t, filepath.Join(source, "plans", "created.md"), "created in the run\n")
}

// TestCopyBackFillsAnEmptyIncludedDirectory is the case that started this: an
// ignored directory with nothing in it yet, which the run fills.
func TestCopyBackFillsAnEmptyIncludedDirectory(t *testing.T) {
	source := newRepository(t)
	mustWrite(t, filepath.Join(source, ".gitignore"), "plans/\n")
	runGit(t, source, "add", ".gitignore")
	runGit(t, source, "commit", "-qm", "ignore plans")
	if err := os.Mkdir(filepath.Join(source, "plans"), 0o700); err != nil {
		t.Fatal(err)
	}

	inputs, _, err := selectInputs(t, source, InputSelection{
		Include: []string{filepath.Join(source, "plans")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs.Files) != 0 || strings.Join(inputs.Roots, ",") != "plans" {
		t.Fatalf("plan = %#v", inputs)
	}
	workspace := filepath.Join(t.TempDir(), "workspace")
	snapshot, err := Stage(context.Background(), PrepareRequest{
		SourcePath: source,
		RunID:      "empty-root-run",
		Inputs:     inputs,
	}, workspace)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(workspace, "plans"), 0o700); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(workspace, "plans", "2026-08-11-task.md"), "planned in the run\n")

	outputs, archive := captureRun(t, snapshot, workspace)
	result, err := CopyBack(snapshot, outputs, archive, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(result.Written, ",") != "plans/2026-08-11-task.md" {
		t.Fatalf("written = %#v", result.Written)
	}
	assertFile(t, filepath.Join(source, "plans", "2026-08-11-task.md"), "planned in the run\n")
}

// TestCopyBackRefusesWhenBothSidesChanged pins the all-or-nothing rule: one
// path that moved on both sides holds back every file, so the working tree is
// never left in a state neither side described.
func TestCopyBackRefusesWhenBothSidesChanged(t *testing.T) {
	source := newIncludedRepository(t)
	snapshot, workspace := stageIncluded(t, source, "conflict-run")

	mustWrite(t, filepath.Join(workspace, "plans", "carried.md"), "edited in the run\n")
	mustWrite(t, filepath.Join(workspace, "plans", "created.md"), "created in the run\n")
	outputs, archive := captureRun(t, snapshot, workspace)
	// The Mac moves the same path while the run holds its own copy.
	mustWrite(t, filepath.Join(source, "plans", "carried.md"), "edited on the Mac\n")

	result, err := CopyBack(snapshot, outputs, archive, false)
	conflict := &IncludedConflict{}
	if !errors.As(err, &conflict) {
		t.Fatalf("err = %v", err)
	}
	if strings.Join(result.Conflicts, ",") != "plans/carried.md" {
		t.Fatalf("conflicts = %#v", result.Conflicts)
	}
	assertFile(t, filepath.Join(source, "plans", "carried.md"), "edited on the Mac\n")
	if _, err := os.Lstat(filepath.Join(source, "plans", "created.md")); !errors.Is(
		err,
		fs.ErrNotExist,
	) {
		t.Fatal("a conflicting path did not hold back the rest of the copy")
	}

	// Forcing writes exactly what the refusal held back, conflicts included.
	forced, err := CopyBack(snapshot, outputs, archive, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(forced.Written, ",") != "plans/carried.md,plans/created.md" {
		t.Fatalf("forced = %#v", forced.Written)
	}
	assertFile(t, filepath.Join(source, "plans", "carried.md"), "edited in the run\n")
	assertFile(t, filepath.Join(source, "plans", "created.md"), "created in the run\n")
}

// TestCopyBackNeverRemovesOrClobbersHostWork covers the two ways the host wins:
// a path the run deleted stays, and a path only the host changed keeps the
// host's version.
func TestCopyBackNeverRemovesOrClobbersHostWork(t *testing.T) {
	source := newIncludedRepository(t)
	mustWrite(t, filepath.Join(source, "plans", "kept.md"), "carried in\n")
	snapshot, workspace := stageIncluded(t, source, "additive-run")

	// The run removes one included file and leaves the other alone.
	if err := os.Remove(filepath.Join(workspace, "plans", "kept.md")); err != nil {
		t.Fatal(err)
	}
	outputs, archive := captureRun(t, snapshot, workspace)
	mustWrite(t, filepath.Join(source, "plans", "carried.md"), "newer on the Mac\n")

	result, err := CopyBack(snapshot, outputs, archive, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(result.Kept, ",") != "plans/kept.md" {
		t.Fatalf("kept = %#v", result.Kept)
	}
	if len(result.Conflicts) != 0 || len(result.Written) != 0 {
		t.Fatalf("result = %#v", result)
	}
	// Deleted in the run, still here.
	assertFile(t, filepath.Join(source, "plans", "kept.md"), "carried in\n")
	// Changed only here, so the run's untouched copy does not overwrite it.
	assertFile(t, filepath.Join(source, "plans", "carried.md"), "newer on the Mac\n")
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
		filepath.Join(packageDir, OutputsArtifactName),
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
