package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
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
		[]string{"prepare-apply", workspace, packageDirectory},
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
		[]string{"prepare-apply", t.TempDir(), filepath.Join(t.TempDir(), "apply")},
		bytes.NewReader(snapshot),
		&bytes.Buffer{},
	)
	if err == nil || !strings.Contains(err.Error(), "host source path") {
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
		"no-agent-forwarding,no-port-forwarding,no-X11-forwarding,no-user-rc "+
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
		"AllowTcpForwarding no",
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

func TestConfigureInferenceInstallsAndReplacesModelsConfig(t *testing.T) {
	home := t.TempDir()
	first := `{"providers":{"pisafe":{"apiKey":"first"}}}`
	if err := configureInference(home, strings.NewReader(first)); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(home, ".pi", "agent", "models.json")
	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != first {
		t.Fatalf("models.json = %q", content)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("models.json mode = %#o", info.Mode().Perm())
	}

	second := `{"providers":{"pisafe":{"apiKey":"second"}}}`
	if err := configureInference(home, strings.NewReader(second)); err != nil {
		t.Fatal(err)
	}
	content, err = os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != second {
		t.Fatalf("replaced models.json = %q", content)
	}
}

func TestConfigureInferencePinsSSETransportPreservingSettings(t *testing.T) {
	home := t.TempDir()
	models := `{"providers":{}}`
	settingsPath := filepath.Join(home, ".pi", "agent", "settings.json")

	if err := configureInference(home, strings.NewReader(models)); err != nil {
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
	if err := configureInference(home, strings.NewReader(models)); err != nil {
		t.Fatal(err)
	}
	assertSettings(map[string]any{"theme": "dark", "transport": "sse"})

	// A corrupt settings file is replaced instead of breaking resume.
	if err := os.WriteFile(settingsPath, []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := configureInference(home, strings.NewReader(models)); err != nil {
		t.Fatal(err)
	}
	assertSettings(map[string]any{"transport": "sse"})
}

func TestConfigureInferenceFailsClosed(t *testing.T) {
	for name, input := range map[string]string{
		"not json":   "providers",
		"trailing":   `{"providers":{}} extra`,
		"oversize":   `{"pad":"` + strings.Repeat("x", int(modelsConfigSizeLimit)) + `"}`,
		"non-object": `["providers"]`,
	} {
		home := t.TempDir()
		if err := configureInference(home, strings.NewReader(input)); err == nil {
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
