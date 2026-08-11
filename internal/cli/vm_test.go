package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/mpizenberg/pisafe/internal/lima"
	"github.com/mpizenberg/pisafe/internal/runstate"
)

// What a rebuild costs is invisible from the outside: the same command either
// keeps every run's work or destroys it, decided by a disk the user never chose.
func TestRebuildPlanSaysWhetherTheWorkSurvives(t *testing.T) {
	runs := []runstate.Manifest{
		{RunID: "run-up", State: runstate.StateActive},
		{RunID: "run-done", State: runstate.StateImported},
	}

	var keeps bytes.Buffer
	printRebuildPlan(&keeps, lima.StatusRunning, false, runs, len(runs))
	for _, expected := range []string{"Keeps:", "pisafe-state", "1 active run(s)"} {
		if !strings.Contains(keeps.String(), expected) {
			t.Fatalf("output lacks %q:\n%s", expected, keeps.String())
		}
	}
	if strings.Contains(keeps.String(), "Loses:") {
		t.Fatalf("a VM with the state disk was reported as losing its work:\n%s", keeps.String())
	}

	// Without the disk the rebuild takes every run's files, so it may not offer
	// to stop runs for a stretch that is about to be deleted either.
	var loses bytes.Buffer
	printRebuildPlan(&loses, lima.StatusRunning, true, runs, len(runs))
	for _, expected := range []string{"Loses:", "pisafe backup", "Forgets: 2 run record(s)"} {
		if !strings.Contains(loses.String(), expected) {
			t.Fatalf("output lacks %q:\n%s", expected, loses.String())
		}
	}
	if strings.Contains(loses.String(), "Keeps:") || strings.Contains(loses.String(), "Stops:") {
		t.Fatalf("a VM without the state disk was reported as keeping its work:\n%s", loses.String())
	}
}

// An absent instance is the one case with nothing to replace, and reporting
// what a rebuild "keeps" there would describe a VM that does not exist.
func TestRebuildPlanReportsAnAbsentInstanceAsACreation(t *testing.T) {
	var output bytes.Buffer
	printRebuildPlan(&output, lima.StatusAbsent, false, nil, 0)
	if !strings.Contains(output.String(), "absent") {
		t.Fatalf("output = %q", output.String())
	}
	if strings.Contains(output.String(), "Keeps:") {
		t.Fatalf("an absent instance was reported as keeping anything:\n%s", output.String())
	}
}

func TestVMRejectsAnythingButRebuildAndItsFlags(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"status"},
		{"rebuild", "--force"},
		{"rebuild", "pisafe"},
	} {
		if err := runVM(t.Context(), args, &bytes.Buffer{}); err != errVMUsage {
			t.Fatalf("runVM(%q) = %v, want a usage error", args, err)
		}
	}
}
