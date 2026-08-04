package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mpizenberg/pisafe/internal/gitstage"
	"github.com/mpizenberg/pisafe/internal/piagent"
	"github.com/mpizenberg/pisafe/internal/profile"
)

func TestMaterializeCommand(t *testing.T) {
	source := initGuestTestRepository(t)
	packageDirectory := filepath.Join(t.TempDir(), "stage")
	prepared, err := gitstage.Prepare(context.Background(), gitstage.PrepareRequest{
		SourcePath: source,
		PackageDir: packageDirectory,
		RunID:      "guest-test",
	})
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
		strings.NewReader(""),
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
		strings.NewReader(""),
		&bytes.Buffer{},
	)
	if err == nil || !strings.Contains(err.Error(), "host source path") {
		t.Fatalf("error = %v", err)
	}
}

func TestDiffCommandReportsChangesAndLeavesTheWorkspaceAlone(t *testing.T) {
	source := initGuestTestRepository(t)
	workspace := filepath.Join(t.TempDir(), "workspace")
	snapshot, err := gitstage.Stage(
		context.Background(),
		gitstage.PrepareRequest{SourcePath: source, RunID: "diff-guest"},
		workspace,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(workspace, "tracked.txt"), []byte("agent work\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	snapshot.SourceRoot = ""
	snapshotJSON, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := run(
		context.Background(),
		[]string{"diff", workspace},
		bytes.NewReader(snapshotJSON),
		&output,
	); err != nil {
		t.Fatal(err)
	}
	var diff gitstage.RunDiff
	decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&diff); err != nil {
		t.Fatal(err)
	}
	if diff.RunID != "diff-guest" || len(diff.Repositories) != 1 {
		t.Fatalf("diff = %#v", diff)
	}
	if files := diff.Repositories[0].Files; len(files) != 1 || files[0].Path != "tracked.txt" {
		t.Fatalf("files = %#v", files)
	}
	// Reporting a run must not commit, stage, or otherwise disturb it.
	if status := guestGit(t, workspace, "status", "--porcelain=v1"); status != "M tracked.txt" {
		t.Fatalf("diff altered the workspace: %q", status)
	}
}

func TestExportCommandArchivesOnlyTheRequestedPath(t *testing.T) {
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "dist"), 0o700); err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{
		"dist/index.html": "<html>\n",
		"secret.txt":      "not requested\n",
	} {
		if err := os.WriteFile(
			filepath.Join(workspace, filepath.FromSlash(path)), []byte(content), 0o600,
		); err != nil {
			t.Fatal(err)
		}
	}

	var output bytes.Buffer
	if err := run(
		context.Background(),
		[]string{"export", workspace, "dist"},
		nil,
		&output,
	); err != nil {
		t.Fatal(err)
	}
	names := []string{}
	archive := tar.NewReader(bytes.NewReader(output.Bytes()))
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, header.Name)
	}
	if strings.Join(names, ",") != "dist/,dist/index.html" {
		t.Fatalf("archived %v", names)
	}

	if err := run(
		context.Background(),
		[]string{"export", workspace, "../escape"},
		nil,
		&bytes.Buffer{},
	); err == nil {
		t.Fatal("an escaping path was archived")
	}
}

// The archive comes from the Mac, so what this checks is where it is allowed to
// land: inside the workspace, under a name the Mac chose, and never over
// something already there unless the Mac said so.
func TestImportCommandUnpacksOnlyInsideTheWorkspace(t *testing.T) {
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	archived := func(name, content string) io.Reader {
		var buffer bytes.Buffer
		writer := tar.NewWriter(&buffer)
		if err := writer.WriteHeader(&tar.Header{
			Name:     name,
			Typeflag: tar.TypeReg,
			Mode:     0o600,
			Size:     int64(len(content)),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		return &buffer
	}

	// An empty destination is the workspace, and the copy keeps its name.
	if err := run(
		context.Background(),
		[]string{"import", workspace, "", "cf.json", "refuse"},
		archived("cf.json", "{\"requests\":12}\n"),
		&bytes.Buffer{},
	); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(workspace, "cf.json"))
	if err != nil || string(content) != "{\"requests\":12}\n" {
		t.Fatalf("content = %q, err = %v", content, err)
	}

	// A destination that already exists as a directory takes the copy inside
	// it, which is what the copy in the other direction does on the Mac.
	if err := run(
		context.Background(),
		[]string{"import", workspace, "data", "cf.json", "refuse"},
		archived("cf.json", "nested\n"),
		&bytes.Buffer{},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "data", "cf.json")); err != nil {
		t.Fatal(err)
	}

	// An occupied name is refused, and the file that is there is untouched.
	if err := run(
		context.Background(),
		[]string{"import", workspace, "", "cf.json", "refuse"},
		archived("cf.json", "overwritten\n"),
		&bytes.Buffer{},
	); err == nil {
		t.Fatal("an occupied destination was overwritten")
	}
	content, err = os.ReadFile(filepath.Join(workspace, "cf.json"))
	if err != nil || string(content) != "{\"requests\":12}\n" {
		t.Fatalf("a refused copy changed the file: %q, err = %v", content, err)
	}

	for name, args := range map[string][]string{
		"absolute destination": {"import", workspace, "/etc/cron.d/x", "cf.json", "refuse"},
		"climbing destination": {"import", workspace, "../../elsewhere", "cf.json", "refuse"},
		"name with a path":     {"import", workspace, "", "../cf.json", "refuse"},
		"unknown decision":     {"import", workspace, "", "cf.json", "maybe"},
	} {
		if err := run(
			context.Background(),
			args,
			archived("cf.json", "escaped\n"),
			&bytes.Buffer{},
		); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}

	// An archive naming something other than what the Mac said is arriving is
	// refused whatever it holds, so one copy can only ever write one path.
	if err := run(
		context.Background(),
		[]string{"import", workspace, "", "cf.json", "replace"},
		archived("../../etc/passwd", "escaped\n"),
		&bytes.Buffer{},
	); err == nil {
		t.Fatal("an archive escaping its own name was unpacked")
	}
}

func TestDiffCommandRejectsHostPathInSnapshot(t *testing.T) {
	snapshot, err := json.Marshal(gitstage.Snapshot{
		RunID:      "unsafe",
		SourceRoot: "/Users/alice/project",
		SourceHead: strings.Repeat("a", 40),
		WorkRef:    "refs/heads/work/unsafe",
	})
	if err != nil {
		t.Fatal(err)
	}
	err = run(
		context.Background(),
		[]string{"diff", t.TempDir()},
		bytes.NewReader(snapshot),
		&bytes.Buffer{},
	)
	if err == nil || !strings.Contains(err.Error(), "host source path") {
		t.Fatalf("error = %v", err)
	}
}

func TestPrepareApplyCommandReportsBundlesWithoutPaths(t *testing.T) {
	source := initGuestTestRepository(t)
	workspace := filepath.Join(t.TempDir(), "workspace")
	snapshot, err := gitstage.Stage(
		context.Background(),
		gitstage.PrepareRequest{SourcePath: source, RunID: "apply-guest"},
		workspace,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(workspace, "tracked.txt"), []byte("agent result\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	snapshot.SourceRoot = ""
	snapshotJSON, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}

	// A leftover package from an interrupted attempt must not block a retry.
	packageDirectory := filepath.Join(workspace, "apply")
	if err := os.Mkdir(packageDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(packageDirectory, "apply.bundle"), []byte("stale"), 0o600,
	); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := run(
		context.Background(),
		[]string{"prepare-apply", "keep", workspace, packageDirectory},
		bytes.NewReader(snapshotJSON),
		&output,
	); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), packageDirectory) {
		t.Fatalf("prepared apply disclosed a path: %s", output.String())
	}
	var prepared gitstage.PreparedApply
	decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&prepared); err != nil {
		t.Fatal(err)
	}
	if prepared.RunID != "apply-guest" || prepared.FinalCommit == "" {
		t.Fatalf("prepared = %#v", prepared)
	}
	artifacts := prepared.Artifacts()
	if len(artifacts) != 1 || artifacts[0].Name != "apply.bundle" {
		t.Fatalf("artifacts = %#v", artifacts)
	}
	content, err := os.ReadFile(filepath.Join(packageDirectory, artifacts[0].Name))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(content, []byte("stale")) {
		t.Fatal("stale bundle survived")
	}
}

func TestPrepareApplyRejectsHostPathInSnapshot(t *testing.T) {
	snapshot, err := json.Marshal(gitstage.Snapshot{
		RunID:      "unsafe",
		SourceRoot: "/Users/alice/project",
		SourceHead: strings.Repeat("a", 40),
		WorkRef:    "refs/heads/work/unsafe",
	})
	if err != nil {
		t.Fatal(err)
	}
	err = run(
		context.Background(),
		[]string{"prepare-apply", "keep", t.TempDir(), filepath.Join(t.TempDir(), "apply")},
		bytes.NewReader(snapshot),
		&bytes.Buffer{},
	)
	if err == nil || !strings.Contains(err.Error(), "host source path") {
		t.Fatalf("error = %v", err)
	}

	// The baseline choice is settled before the run is even read.
	err = run(
		context.Background(),
		[]string{"prepare-apply", "replay", t.TempDir(), filepath.Join(t.TempDir(), "apply")},
		bytes.NewReader(snapshot),
		&bytes.Buffer{},
	)
	if err == nil || !strings.Contains(err.Error(), "baseline choice") {
		t.Fatalf("error = %v", err)
	}
}

func TestConfigureSSHCreatesRestrictedRunFiles(t *testing.T) {
	home := t.TempDir()
	publicKey := guestPublicKey(t)
	var output bytes.Buffer
	err := configureSSH(
		context.Background(),
		home,
		strings.NewReader(publicKey+" client-comment\n"),
		&output,
		func(_ context.Context, path string) error {
			if err := os.WriteFile(path, []byte("private"), 0o600); err != nil {
				return err
			}
			return os.WriteFile(path+".pub", []byte(publicKey+" host-comment\n"), 0o644)
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if output.String() != publicKey+"\n" {
		t.Fatalf("host key output = %q", output.String())
	}
	directory := filepath.Join(home, ".ssh")
	authorized, err := os.ReadFile(filepath.Join(directory, "authorized_keys"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(authorized); got !=
		"no-agent-forwarding,no-X11-forwarding,no-user-rc "+
			publicKey+"\n" {
		t.Fatalf("authorized_keys = %q", got)
	}
	config, err := os.ReadFile(filepath.Join(directory, "sshd_config"))
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"Port 2222",
		"PasswordAuthentication no",
		"AuthenticationMethods publickey",
		"PermitRootLogin no",
		"AllowUsers node",
		"AllowAgentForwarding no",
		// The Mac may reach a server the run is hosting, and nothing else: a
		// forward aimed anywhere but this container's loopback is refused, and
		// the run gets no listener of the Mac's in return.
		"AllowTcpForwarding local",
		"PermitOpen 127.0.0.1:*",
		// sshd builds a session environment from scratch, so the container's
		// own environment reaches a terminal only if the daemon restates it.
		"SetEnv GIT_TERMINAL_PROMPT=0 PI_CODING_AGENT_DIR=" +
			filepath.Join(home, ".pi", "agent") + " PI_SKIP_VERSION_CHECK=1",
	} {
		if !strings.Contains(string(config), expected) {
			t.Errorf("sshd config lacks %q:\n%s", expected, config)
		}
	}
	for _, name := range []string{"authorized_keys", "sshd_config", "ssh_host_ed25519_key"} {
		info, err := os.Stat(filepath.Join(directory, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %#o", name, info.Mode().Perm())
		}
	}
}

func TestConfigureSSHFailsClosed(t *testing.T) {
	for _, input := range []string{
		"not-a-key",
		strings.Repeat("x", sshPublicKeySize+1),
	} {
		err := configureSSH(
			context.Background(),
			t.TempDir(),
			strings.NewReader(input),
			&bytes.Buffer{},
			func(context.Context, string) error {
				t.Fatal("keygen called for invalid input")
				return nil
			},
		)
		if err == nil {
			t.Fatalf("configureSSH(%q) unexpectedly succeeded", input[:min(len(input), 32)])
		}
	}
}

func TestConfigureModelsInstallsAndReplacesModelsConfig(t *testing.T) {
	home := t.TempDir()
	target := filepath.Join(home, ".pi", "agent", "models.json")
	// A run's own copy is indented whatever the wire carried, because the run is
	// where somebody reads it to find out what their agent was offered.
	installed := func(capability string) string {
		return `{
  "providers": {
    "pisafe": {
      "apiKey": "` + capability + `"
    }
  }
}
`
	}

	if err := configureModels(home, inferenceDocument(t, capabilityModels("first"))); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != installed("first") {
		t.Fatalf("models.json = %q", content)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("models.json mode = %#o", info.Mode().Perm())
	}

	if err := configureModels(home, inferenceDocument(t, capabilityModels("second"))); err != nil {
		t.Fatal(err)
	}
	content, err = os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != installed("second") {
		t.Fatalf("replaced models.json = %q", content)
	}
}

func capabilityModels(capability string) json.RawMessage {
	return json.RawMessage(`{"providers":{"pisafe":{"apiKey":"` + capability + `"}}}`)
}

func inferenceDocument(
	t *testing.T,
	models json.RawMessage,
	selection ...piagent.Selection,
) io.Reader {
	t.Helper()
	configuration := piagent.Configuration{Models: models}
	if len(selection) == 1 {
		configuration.Default = selection[0]
	}
	document, err := json.Marshal(configuration)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(document)
}

func TestConfigureIdentityGivesTheRunAnAuthor(t *testing.T) {
	home := t.TempDir()
	identity := `{"name":"Ada Lovelace","email":"ada@example.invalid"}`
	if err := configureIdentity(
		context.Background(),
		home,
		strings.NewReader(identity),
	); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(home, ".gitconfig")
	if got := guestGit(
		t,
		home,
		"config", "--file", configPath, "--get", "user.email",
	); got != "ada@example.invalid" {
		t.Fatalf("user.email = %q", got)
	}

	for name, document := range map[string]string{
		"unknown field": `{"name":"Ada","email":"ada@example.invalid","home":"/etc"}`,
		"trailing data": `{"name":"Ada","email":"ada@example.invalid"}{}`,
		"empty email":   `{"name":"Ada","email":""}`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := configureIdentity(
				context.Background(),
				t.TempDir(),
				strings.NewReader(document),
			); err == nil {
				t.Fatal("run accepted an unusable identity")
			}
		})
	}
}

func TestConfigureModelsPinsSSETransportPreservingSettings(t *testing.T) {
	home := t.TempDir()
	models := capabilityModels("run-capability")
	settingsPath := filepath.Join(home, ".pi", "agent", "settings.json")

	if err := configureModels(home, inferenceDocument(t, models)); err != nil {
		t.Fatal(err)
	}
	assertSettings := func(expected map[string]any) {
		t.Helper()
		content, err := os.ReadFile(settingsPath)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(settingsPath)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("settings.json mode = %#o", info.Mode().Perm())
		}
		var parsed map[string]any
		if err := json.Unmarshal(content, &parsed); err != nil {
			t.Fatal(err)
		}
		if len(parsed) != len(expected) {
			t.Fatalf("settings = %v, want %v", parsed, expected)
		}
		for key, value := range expected {
			if parsed[key] != value {
				t.Fatalf("settings[%q] = %v, want %v", key, parsed[key], value)
			}
		}
	}
	assertSettings(map[string]any{"transport": "sse"})

	// Settings Pi wrote during the run survive the resume-time rewrite.
	if err := os.WriteFile(
		settingsPath,
		[]byte(`{"theme":"dark","transport":"auto"}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := configureModels(home, inferenceDocument(t, models)); err != nil {
		t.Fatal(err)
	}
	assertSettings(map[string]any{"theme": "dark", "transport": "sse"})

	// A corrupt settings file is replaced instead of breaking resume.
	if err := os.WriteFile(settingsPath, []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := configureModels(home, inferenceDocument(t, models)); err != nil {
		t.Fatal(err)
	}
	assertSettings(map[string]any{"transport": "sse"})
}

// A run opens on the model pisafe named, so that what it starts on does not
// depend on a table Pi keys by its own provider names. What the run then
// chooses for itself is the run's, and resume configures the same file again.
func TestConfigureModelsNamesTheModelARunOpensOnWithoutOverridingIt(t *testing.T) {
	home := t.TempDir()
	models := capabilityModels("run-capability")
	selection := piagent.Selection{
		Provider: "pisafe",
		Model:    "gpt-5.6-sol",
		Thinking: "high",
	}
	settingsPath := filepath.Join(home, ".pi", "agent", "settings.json")
	read := func() map[string]any {
		t.Helper()
		content, err := os.ReadFile(settingsPath)
		if err != nil {
			t.Fatal(err)
		}
		var parsed map[string]any
		if err := json.Unmarshal(content, &parsed); err != nil {
			t.Fatal(err)
		}
		return parsed
	}

	if err := configureModels(home, inferenceDocument(t, models, selection)); err != nil {
		t.Fatal(err)
	}
	settings := read()
	if settings["defaultProvider"] != "pisafe" ||
		settings["defaultModel"] != "gpt-5.6-sol" ||
		settings["defaultThinkingLevel"] != "high" {
		t.Fatalf("settings = %v", settings)
	}

	if err := os.WriteFile(
		settingsPath,
		[]byte(`{"defaultProvider":"pisafe","defaultModel":"gpt-5.4","defaultThinkingLevel":"low"}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := configureModels(home, inferenceDocument(t, models, selection)); err != nil {
		t.Fatal(err)
	}
	settings = read()
	if settings["defaultModel"] != "gpt-5.4" || settings["defaultThinkingLevel"] != "low" {
		t.Fatalf("resume overrode what the run chose: %v", settings)
	}

	// A Mac with no preference leaves the choice to Pi rather than writing one.
	bare := t.TempDir()
	if err := configureModels(bare, inferenceDocument(t, models)); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(bare, ".pi", "agent", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), "defaultModel") {
		t.Fatalf("settings = %s", content)
	}
}

func TestConfigureProfileNamesThePackagesAndTrustsTheWorkspace(t *testing.T) {
	home := t.TempDir()
	agent := filepath.Join(home, ".pi", "agent")
	installed := profileRoot + "/earendil-works-plan-mode-bf0f2759/node_modules/@earendil-works/plan-mode"
	configure := func(packages ...string) error {
		document, err := json.Marshal(profile.Configuration{
			Packages:  packages,
			Workspace: "/work/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		return configureProfile(home, bytes.NewReader(document))
	}
	read := func(name string) map[string]any {
		t.Helper()
		content, err := os.ReadFile(filepath.Join(agent, name))
		if err != nil {
			t.Fatal(err)
		}
		var parsed map[string]any
		if err := json.Unmarshal(content, &parsed); err != nil {
			t.Fatal(err)
		}
		return parsed
	}

	if err := configure(installed); err != nil {
		t.Fatal(err)
	}
	settings := read("settings.json")
	packages, _ := settings["packages"].([]any)
	if len(packages) != 1 || packages[0] != installed {
		t.Fatalf("settings packages = %v", settings["packages"])
	}
	// A repository's own extensions and settings load without a prompt,
	// because inside a run the container is what project trust would guard.
	if trust := read("trust.json"); trust["/work/project"] != true {
		t.Fatalf("trust = %v", trust)
	}

	// The profile changes between runs, and what a run was told about the
	// previous one must not survive as a package it can no longer load.
	if err := os.WriteFile(
		filepath.Join(agent, "settings.json"),
		[]byte(`{"theme":"dark","packages":["`+installed+`"]}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := configure(); err != nil {
		t.Fatal(err)
	}
	settings = read("settings.json")
	if packages, _ := settings["packages"].([]any); len(packages) != 0 {
		t.Fatalf("settings packages = %v", settings["packages"])
	}
	if settings["theme"] != "dark" {
		t.Fatalf("settings dropped what Pi wrote: %v", settings)
	}

	// What the run installed for itself is the run's, and a resume that
	// dropped it would unload a package still sitting in the run's home.
	if err := os.WriteFile(
		filepath.Join(agent, "settings.json"),
		[]byte(`{"packages":["`+installed+`","npm:pi-web-access"]}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := configure(installed); err != nil {
		t.Fatal(err)
	}
	settings = read("settings.json")
	packages, _ = settings["packages"].([]any)
	if len(packages) != 2 || packages[0] != installed || packages[1] != "npm:pi-web-access" {
		t.Fatalf("settings packages = %v", settings["packages"])
	}
}

func TestConfigureProfileRefusesWhatIsNotTheRuns(t *testing.T) {
	for name, configuration := range map[string]profile.Configuration{
		"package outside the profile": {
			Packages:  []string{"/home/node/.ssh"},
			Workspace: "/work/project",
		},
		"climbing package": {
			Packages:  []string{profileRoot + "/../../.ssh"},
			Workspace: "/work/project",
		},
		"workspace outside the run": {Workspace: "/etc"},
		"climbing workspace":        {Workspace: "/work/../etc"},
		"no workspace":              {},
	} {
		home := t.TempDir()
		document, err := json.Marshal(configuration)
		if err != nil {
			t.Fatal(err)
		}
		if err := configureProfile(home, bytes.NewReader(document)); err == nil {
			t.Errorf("%s was accepted", name)
		}
		if _, err := os.Stat(filepath.Join(home, ".pi", "agent", "trust.json")); err == nil {
			t.Errorf("%s left a trust decision behind", name)
		}
	}
	home := t.TempDir()
	if err := configureProfile(home, strings.NewReader(`{"unknown":1}`)); err == nil {
		t.Error("a configuration with an unknown field was accepted")
	}
}

func TestConfigureModelsFailsClosed(t *testing.T) {
	for name, input := range map[string]string{
		"not json":      "providers",
		"trailing":      `{"models":{"providers":{"pisafe":{}}}} extra`,
		"oversize":      `{"models":{"providers":{"pisafe":{"apiKey":"` + strings.Repeat("x", int(documentSizeLimit)) + `"}}}}`,
		"unknown field": `{"models":{"providers":{"pisafe":{}}},"transport":"auto"}`,
		"non-object":    `{"models":["providers"]}`,
		"no provider":   `{"models":{"providers":{}}}`,
		"default elsewhere": `{"models":{"providers":{"pisafe":{}}},` +
			`"default":{"provider":"other","model":"gpt-5.6-sol","thinking":"high"}}`,
		"default without a model": `{"models":{"providers":{"pisafe":{}}},` +
			`"default":{"provider":"pisafe","thinking":"high"}}`,
		"unknown effort": `{"models":{"providers":{"pisafe":{}}},` +
			`"default":{"provider":"pisafe","model":"gpt-5.6-sol","thinking":"deep"}}`,
	} {
		home := t.TempDir()
		if err := configureModels(home, strings.NewReader(input)); err == nil {
			t.Errorf("%s was accepted", name)
		}
		if _, err := os.Stat(filepath.Join(home, ".pi", "agent", "models.json")); err == nil {
			t.Errorf("%s left a models.json behind", name)
		}
	}
}

func TestProxySSHRelaysBinaryStdio(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	input := []byte("request\x00payload")
	serverDone := make(chan error, 1)
	go func() {
		defer server.Close()
		content := make([]byte, len(input))
		_, readErr := io.ReadFull(server, content)
		if readErr != nil {
			serverDone <- readErr
			return
		}
		_, writeErr := server.Write(append([]byte("reply:"), content...))
		serverDone <- writeErr
	}()

	var output bytes.Buffer
	if err := relaySSH(
		client,
		bytes.NewReader(input),
		&output,
	); err != nil {
		t.Fatal(err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
	if got := output.String(); got != "reply:request\x00payload" {
		t.Fatalf("relayed output = %q", got)
	}
}

func guestPublicKey(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	path := filepath.Join(directory, "id_ed25519")
	command := exec.Command(
		"ssh-keygen",
		"-q",
		"-t", "ed25519",
		"-N", "",
		"-C", "guest-test",
		"-f", path,
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, output)
	}
	content, err := os.ReadFile(path + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(content))
	return strings.Join(fields[:2], " ")
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
