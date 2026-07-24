package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mpizenberg/pisafe/internal/gitstage"
)

func TestMaterializeCommand(t *testing.T) {
	source := initGuestTestRepository(t)
	packageDirectory := filepath.Join(t.TempDir(), "stage")
	prepared, err := gitstage.Prepare(context.Background(), source, packageDirectory, "guest-test")
	if err != nil {
		t.Fatal(err)
	}
	prepared.Snapshot.SourceRoot = ""
	snapshotJSON, err := json.Marshal(prepared.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(packageDirectory, "snapshot.json"),
		snapshotJSON,
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(t.TempDir(), "workspace")
	var output bytes.Buffer
	if err := run(
		context.Background(),
		[]string{"materialize", packageDirectory, workspace},
		&output,
	); err != nil {
		t.Fatal(err)
	}
	if got := guestGit(t, workspace, "status", "--short"); got != "" {
		t.Fatalf("status = %q", got)
	}
	content, err := os.ReadFile(filepath.Join(workspace, "tracked.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(content); got != "changed\n" {
		t.Fatalf("tracked content = %q", got)
	}
	var snapshot gitstage.Snapshot
	if err := json.Unmarshal(output.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.BaselineCommit == "" {
		t.Fatal("dirty tracked baseline was not committed")
	}
}

func TestMaterializeRejectsHostPathInSnapshot(t *testing.T) {
	stage := t.TempDir()
	snapshot := gitstage.Snapshot{
		RunID:      "unsafe",
		SourceRoot: "/Users/alice/project",
		SourceHead: strings.Repeat("a", 40),
		WorkRef:    "refs/heads/work/unsafe",
	}
	content, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "snapshot.json"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	err = run(
		context.Background(),
		[]string{"materialize", stage, filepath.Join(t.TempDir(), "workspace")},
		&bytes.Buffer{},
	)
	if err == nil || !strings.Contains(err.Error(), "host source path") {
		t.Fatalf("error = %v", err)
	}
}

func initGuestTestRepository(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	guestGit(t, root, "init", "--quiet")
	guestGit(t, root, "config", "user.name", "Pi Safe Test")
	guestGit(t, root, "config", "user.email", "test@example.invalid")
	guestGit(t, root, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("original\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	guestGit(t, root, "add", "tracked.txt")
	guestGit(t, root, "commit", "--quiet", "-m", "initial")
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func guestGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	commandArgs := append([]string{"-C", directory}, args...)
	command := exec.Command("git", commandArgs...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}
