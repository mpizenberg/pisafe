package backup

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mpizenberg/pisafe/internal/profile"
	"github.com/mpizenberg/pisafe/internal/runid"
)

const testIntegrity = "sha512-" + "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=="

func testPin(t *testing.T, name, version string) profile.Pin {
	t.Helper()
	directory, err := runid.NewPackageDirectory(name)
	if err != nil {
		t.Fatal(err)
	}
	return profile.Pin{
		Name:      name,
		Version:   version,
		Integrity: testIntegrity,
		Directory: directory,
	}
}

func testProject(t *testing.T, root string) runid.Project {
	t.Helper()
	project, err := runid.NewProject(root)
	if err != nil {
		t.Fatal(err)
	}
	return project
}

// sessionArchive builds what the VM sends, rooted at the one name every archive
// crossing the boundary uses.
func sessionArchive(t *testing.T, transcripts map[string]string) io.Reader {
	t.Helper()
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	if err := writer.WriteHeader(&tar.Header{
		Name:     SessionsDirectory + "/",
		Typeflag: tar.TypeDir,
		Mode:     0o700,
	}); err != nil {
		t.Fatal(err)
	}
	for name, content := range transcripts {
		if err := writer.WriteHeader(&tar.Header{
			Name:     SessionsDirectory + "/" + name,
			Typeflag: tar.TypeReg,
			Mode:     0o600,
			Size:     int64(len(content)),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(writer, content); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return &archive
}

// TestABackupRecordsWhatARestoreNeedsAndNothingMore is the whole of what an
// export writes down. The pins travel as the profile wrote them, so a restore
// reads them back through the parser an installed profile is read through.
func TestABackupRecordsWhatARestoreNeedsAndNothingMore(t *testing.T) {
	extension := testPin(t, "@pisafe/example", "1.2.3")
	tool := profile.Tool{Pin: testPin(t, "cowsay", "1.6.0"), Binaries: []string{"cowsay"}}
	written := Backup{
		CreatedAt:  time.Date(2026, 7, 28, 9, 0, 0, 0, time.UTC),
		Extensions: profile.Record{Version: profile.RecordVersion, Extensions: []profile.Pin{extension}},
		Tools:      profile.Tools{Version: profile.ToolsVersion, Tools: []profile.Tool{tool}},
		Projects:   []runid.Project{testProject(t, "/tmp/pisafe-test/alpha")},
	}
	content, err := written.Encode()
	if err != nil {
		t.Fatal(err)
	}
	read, err := Parse(content)
	if err != nil {
		t.Fatal(err)
	}
	if !read.CreatedAt.Equal(written.CreatedAt) {
		t.Errorf("createdAt = %v", read.CreatedAt)
	}
	if len(read.Extensions.Extensions) != 1 || read.Extensions.Extensions[0] != extension {
		t.Errorf("extensions = %#v", read.Extensions.Extensions)
	}
	if len(read.Tools.Tools) != 1 || read.Tools.Tools[0].Name != tool.Name {
		t.Errorf("tools = %#v", read.Tools.Tools)
	}
	if len(read.Projects) != 1 || read.Projects[0].Root != "/tmp/pisafe-test/alpha" {
		t.Errorf("projects = %#v", read.Projects)
	}
	// A credential has no field to travel in, which is what keeps the boundary
	// the broker exists to hold from being crossed by a backup.
	var raw map[string]any
	if err := json.Unmarshal(content, &raw); err != nil {
		t.Fatal(err)
	}
	for field := range raw {
		if !slices.Contains([]string{"version", "createdAt", "extensions", "tools", "projects"}, field) {
			t.Errorf("the manifest carries an unexpected field %q", field)
		}
	}
}

// TestARestoreRefusesAManifestItCannotTrust is what stands between a directory
// on the Mac and a command that installs packages and writes into project
// stores. A key is a digest of the root, so a manifest disagreeing with itself
// would file one checkout's history under another's name.
func TestARestoreRefusesAManifestItCannotTrust(t *testing.T) {
	valid := testProject(t, "/tmp/pisafe-test/alpha")
	other := testProject(t, "/tmp/pisafe-test/beta")
	for name, content := range map[string]string{
		"no version":    `{"createdAt":"2026-07-28T09:00:00Z","extensions":null,"tools":null,"projects":[]}`,
		"later version": `{"version":2,"extensions":null,"tools":null,"projects":[]}`,
		"unknown field": `{"version":1,"extensions":null,"tools":null,"projects":[],"credentials":"x"}`,
		"trailing data": `{"version":1,"extensions":{"version":1},"tools":{"version":1},"projects":[]}{}`,
		"key names another checkout": `{"version":1,"extensions":{"version":1},"tools":{"version":1},` +
			`"projects":[{"key":"` + other.Key + `","root":"` + valid.Root + `"}]}`,
		"relative checkout": `{"version":1,"extensions":{"version":1},"tools":{"version":1},` +
			`"projects":[{"key":"` + valid.Key + `","root":"pisafe-test/alpha"}]}`,
		"checkout twice": `{"version":1,"extensions":{"version":1},"tools":{"version":1},` +
			`"projects":[{"key":"` + valid.Key + `","root":"` + valid.Root + `"},` +
			`{"key":"` + valid.Key + `","root":"` + valid.Root + `"}]}`,
		"unpinned extension": `{"version":1,"tools":{"version":1},"projects":[],"extensions":` +
			`{"version":1,"extensions":[{"name":"a","version":"^1.0.0","integrity":"` +
			testIntegrity + `","directory":"x"}]}}`,
	} {
		if _, err := Parse([]byte(content)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// TestABackupKeepsTheTranscriptItAlreadyHeld is why a backup can be repeated
// into the same directory. A transcript Pi rewrote in place when it migrated
// the session on load arrives under a name the backup already has, and the copy
// taken first is the one that survives — the same rule promotion follows.
func TestABackupKeepsTheTranscriptItAlreadyHeld(t *testing.T) {
	directory := t.TempDir()
	key := testProject(t, "/tmp/pisafe-test/alpha").Key

	added, refused, err := AddSessions(
		sessionArchive(t, map[string]string{"1_aaaa.jsonl": "first"}), directory, key,
	)
	if err != nil || added != 1 || refused != 0 {
		t.Fatalf("added = %d, refused = %d, err = %v", added, refused, err)
	}
	added, refused, err = AddSessions(
		sessionArchive(t, map[string]string{
			"1_aaaa.jsonl": "rewritten",
			"2_bbbb.jsonl": "new",
		}),
		directory,
		key,
	)
	if err != nil || added != 1 || refused != 0 {
		t.Fatalf("added = %d, refused = %d, err = %v", added, refused, err)
	}

	held, err := Sessions(directory, key)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(held, []string{"1_aaaa.jsonl", "2_bbbb.jsonl"}) {
		t.Fatalf("the backup holds %v", held)
	}
	content, err := os.ReadFile(filepath.Join(ProjectDirectory(directory, key), SessionsDirectory, "1_aaaa.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "first" {
		t.Errorf("1_aaaa.jsonl = %q, want %q", content, "first")
	}
	// Nothing is left beside the transcripts: the staging the archive landed in
	// would otherwise be read back as part of the backup.
	entries, err := os.ReadDir(ProjectDirectory(directory, key))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != SessionsDirectory {
		t.Errorf("the project directory holds %v", entries)
	}
}

// TestOnlyATranscriptCrossesIntoTheBackup covers a name a run chose. A run can
// write whatever it likes into its own session directory and promotion carries
// it, so one file the Mac will not write must cost that file and not the backup
// of everything else.
func TestOnlyATranscriptCrossesIntoTheBackup(t *testing.T) {
	directory := t.TempDir()
	key := testProject(t, "/tmp/pisafe-test/alpha").Key

	added, refused, err := AddSessions(
		sessionArchive(t, map[string]string{
			"1_aaaa.jsonl":                      "kept",
			"notes.txt":                         "not a transcript",
			".hidden.jsonl":                     "would hide in a listing",
			"nested/inner.jsonl":                "would flatten onto another name",
			strings.Repeat("x", 200) + ".jsonl": "unbounded",
		}),
		directory,
		key,
	)
	if err != nil {
		t.Fatal(err)
	}
	if added != 1 || refused != 4 {
		t.Fatalf("added = %d, refused = %d", added, refused)
	}
	held, err := Sessions(directory, key)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(held, []string{"1_aaaa.jsonl"}) {
		t.Fatalf("the backup holds %v", held)
	}
}

// TestARestoreSendsBackOnlyWhatAnExportWouldHaveWritten checks the other
// direction at the same place. A backup is a directory on the Mac and anything
// could have been dropped into it since, so what goes into a project store is
// held to what came out of one.
func TestARestoreSendsBackOnlyWhatAnExportWouldHaveWritten(t *testing.T) {
	directory := t.TempDir()
	key := testProject(t, "/tmp/pisafe-test/alpha").Key
	if _, _, err := AddSessions(
		sessionArchive(t, map[string]string{"1_aaaa.jsonl": "kept"}), directory, key,
	); err != nil {
		t.Fatal(err)
	}
	planted := filepath.Join(ProjectDirectory(directory, key), SessionsDirectory, "planted.sh")
	if err := os.WriteFile(planted, []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var archive bytes.Buffer
	if err := ArchiveSessions(directory, key, &archive); err == nil {
		t.Fatal("a planted file was sent into a project store")
	}
	if archive.Len() != 0 {
		t.Errorf("%d bytes were sent before the refusal", archive.Len())
	}

	if err := os.Remove(planted); err != nil {
		t.Fatal(err)
	}
	if err := ArchiveSessions(directory, key, &archive); err != nil {
		t.Fatal(err)
	}
	names := []string{}
	reader := tar.NewReader(&archive)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, header.Name)
	}
	if !slices.Equal(names, []string{SessionsDirectory + "/", SessionsDirectory + "/1_aaaa.jsonl"}) {
		t.Fatalf("the archive holds %v", names)
	}
}

// TestAnUnfinishedBackupIsNotOneAtAll is why the manifest is written last: a
// directory an export filled halfway holds transcripts and no manifest, and a
// restore refuses it rather than putting a fraction of a profile back.
func TestAnUnfinishedBackupIsNotOneAtAll(t *testing.T) {
	directory := t.TempDir()
	key := testProject(t, "/tmp/pisafe-test/alpha").Key
	if _, _, err := AddSessions(
		sessionArchive(t, map[string]string{"1_aaaa.jsonl": "kept"}), directory, key,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(directory); err == nil {
		t.Fatal("a directory with no manifest was read as a backup")
	}
	if err := Write(directory, Backup{CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(directory); err != nil {
		t.Fatal(err)
	}
}
