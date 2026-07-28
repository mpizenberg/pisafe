package lima

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mpizenberg/pisafe/internal/gitstage"
	"github.com/mpizenberg/pisafe/internal/runcontainer"
)

const testRunImage = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestTransportCreateStageStreamsVerifiedArtifacts(t *testing.T) {
	root := t.TempDir()
	bundle := filepath.Join(root, "source.bundle")
	patch := filepath.Join(root, "tracked.patch")
	if err := os.WriteFile(bundle, []byte("bundle\x00content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(patch, []byte("patch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	transport := Transport{instance: InstanceName, runner: runner}
	prepared := gitstage.PreparedStage{
		Snapshot: gitstage.Snapshot{
			RunID:      "safe-run",
			SourceRoot: "/Users/alice/secret-path/project",
			SourceHead: strings.Repeat("a", 40),
			WorkRef:    "refs/heads/work/safe-run",
			CreatedAt:  time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC),
		},
		BundlePath: bundle,
		PatchPath:  patch,
	}

	runner.outputs = [][]byte{[]byte("/home/piz/.local/share/pisafe/runs/safe-run/stage\n")}
	stagePath, err := transport.CreateStage(context.Background(), prepared)
	if err != nil {
		t.Fatal(err)
	}
	if stagePath != "/home/piz/.local/share/pisafe/runs/safe-run/stage" {
		t.Fatalf("stage path = %q", stagePath)
	}
	if len(runner.calls) != 4 {
		t.Fatalf("calls = %d, want 4", len(runner.calls))
	}
	assertArgsPrefix(t, runner.calls[0], "shell", "pisafe", "sh", "-ceu")
	if got := runner.calls[1].stdin; got != "bundle\x00content" {
		t.Fatalf("bundle stdin = %q", got)
	}
	if got := runner.calls[2].stdin; got != "patch\n" {
		t.Fatalf("patch stdin = %q", got)
	}
	if strings.Contains(runner.calls[3].stdin, "/Users/") {
		t.Fatalf("snapshot disclosed host path: %s", runner.calls[3].stdin)
	}
	if !strings.Contains(runner.calls[3].stdin, `"run_id":"safe-run"`) {
		t.Fatalf("snapshot stdin = %q", runner.calls[3].stdin)
	}

	bundleDigest := sha256.Sum256([]byte("bundle\x00content"))
	uploadArgs := runner.calls[1].args
	if got, want := uploadArgs[len(uploadArgs)-4:], []string{
		"safe-run", "source.bundle", "14", hex.EncodeToString(bundleDigest[:]),
	}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("upload args = %#v, want %#v", got, want)
	}
}

func TestTransportRejectsUnsafeRunBeforeCallingLima(t *testing.T) {
	runner := &fakeRunner{}
	transport := Transport{instance: InstanceName, runner: runner}
	_, err := transport.CreateStage(context.Background(), gitstage.PreparedStage{
		Snapshot: gitstage.Snapshot{RunID: "../escape"},
	})
	if err == nil || !strings.Contains(err.Error(), "invalid run ID") {
		t.Fatalf("error = %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("calls = %#v", runner.calls)
	}
}

func TestTransportRemoveRunUsesPositionalArgument(t *testing.T) {
	runner := &fakeRunner{}
	transport := Transport{instance: InstanceName, runner: runner}
	if err := transport.RemoveRun(context.Background(), "run-123"); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("calls = %#v", runner.calls)
	}
	call := runner.calls[0]
	assertArgsPrefix(t, call, "shell", "pisafe", "sh", "-ceu")
	if got := call.args[len(call.args)-1]; got != "run-123" {
		t.Fatalf("run argument = %q", got)
	}
	if strings.Contains(call.args[4], "run-123") {
		t.Fatal("run ID was interpolated into the remote script")
	}
}

func TestTransportImportStageIsRunScoped(t *testing.T) {
	runner := &fakeRunner{}
	transport := Transport{instance: InstanceName, runner: runner}
	if err := transport.ImportStage(
		context.Background(),
		"run-123",
	); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("calls = %#v", runner.calls)
	}
	call := runner.calls[0]
	assertArgsPrefix(t, call, "shell", "pisafe", "bash", "-ceu")
	if got := call.args[len(call.args)-1:]; strings.Join(got, "|") !=
		"run-123" {
		t.Fatalf("positional arguments = %#v", got)
	}
	if err := transport.ImportStage(
		context.Background(),
		"../escape",
	); err == nil {
		t.Fatal("unsafe run was accepted")
	}
}

func TestTransportStorageUsesFixedPrivilegedHelper(t *testing.T) {
	runner := &fakeRunner{}
	transport := Transport{instance: InstanceName, runner: runner}
	for _, operation := range []struct {
		action string
		scope  string
		id     string
		call   func() error
	}{
		{action: "create", scope: "run", id: "run-123", call: func() error {
			return transport.CreateRunStorage(context.Background(), "run-123")
		}},
		{action: "verify", scope: "run", id: "run-123", call: func() error {
			return transport.VerifyRunStorage(context.Background(), "run-123")
		}},
		{action: "remove", scope: "run", id: "run-123", call: func() error {
			return transport.RemoveRunStorage(context.Background(), "run-123")
		}},
		{action: "ensure", scope: "project", id: "api-3f9c2a1b", call: func() error {
			return transport.EnsureProjectStorage(context.Background(), "api-3f9c2a1b")
		}},
	} {
		if err := operation.call(); err != nil {
			t.Fatal(err)
		}
		call := runner.calls[len(runner.calls)-1]
		assertArgsPrefix(
			t,
			call,
			"shell", "pisafe", "sudo",
			"/usr/local/sbin/pisafe-storage", operation.action, operation.scope, operation.id,
		)
	}
}

func TestTransportReturnsLimaSSHGateway(t *testing.T) {
	config := filepath.Join(t.TempDir(), "ssh.config")
	if err := os.WriteFile(config, []byte("Host lima-pisafe\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{outputs: [][]byte{[]byte(config + "\n")}}
	transport := Transport{instance: InstanceName, runner: runner}
	gateway, err := transport.SSHGateway(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gateway.ConfigFile != config || gateway.Alias != "lima-pisafe" {
		t.Fatalf("gateway = %#v", gateway)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("calls = %#v", runner.calls)
	}
	assertArgsPrefix(
		t,
		runner.calls[0],
		"list",
		"--format", "{{.SSHConfigFile}}",
		InstanceName,
	)
}

func assertArgsPrefix(t *testing.T, call recordedCall, want ...string) {
	t.Helper()
	if len(call.args) < len(want) {
		t.Fatalf("args = %#v", call.args)
	}
	for index := range want {
		if call.args[index] != want[index] {
			t.Fatalf("args = %#v, want prefix %#v", call.args, want)
		}
	}
}

func TestTransportStreamsSubmoduleAndInputArtifacts(t *testing.T) {
	root := t.TempDir()
	write := func(name, content string) string {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	runner := &fakeRunner{
		outputs: [][]byte{[]byte("/home/piz/.local/share/pisafe/runs/safe-run/stage\n")},
	}
	transport := Transport{instance: InstanceName, runner: runner}
	prepared := gitstage.PreparedStage{
		Snapshot: gitstage.Snapshot{
			RunID:      "safe-run",
			SourceHead: strings.Repeat("a", 40),
			WorkRef:    "refs/heads/work/safe-run",
			Submodules: []gitstage.SubmoduleStage{
				{Path: "dependency", Head: strings.Repeat("b", 40)},
			},
		},
		BundlePath: write("source.bundle", "bundle"),
		PatchPath:  write("tracked.patch", "patch"),
		InputsPath: write("inputs.tar", "inputs"),
		Submodules: []gitstage.PreparedSubmodule{{
			Path:       "dependency",
			BundlePath: write("sub.bundle", "submodule bundle"),
			PatchPath:  write("sub.patch", "submodule patch"),
		}},
	}

	if _, err := transport.CreateStage(context.Background(), prepared); err != nil {
		t.Fatal(err)
	}
	uploaded := map[string]string{}
	for _, call := range runner.calls[1:] {
		uploaded[call.args[len(call.args)-3]] = call.stdin
	}
	for name, content := range map[string]string{
		"source.bundle":      "bundle",
		"tracked.patch":      "patch",
		"inputs.tar":         "inputs",
		"submodule-0.bundle": "submodule bundle",
		"submodule-0.patch":  "submodule patch",
	} {
		if uploaded[name] != content {
			t.Errorf("%s uploaded %q, want %q", name, uploaded[name], content)
		}
	}
	if !strings.Contains(uploaded["snapshot.json"], `"path":"dependency"`) {
		t.Errorf("snapshot lacks the submodule: %s", uploaded["snapshot.json"])
	}
}

func TestFetchApplyArtifactKeepsOnlyAVerifiedTransfer(t *testing.T) {
	content := []byte("apply bundle\x00content")
	digest := sha256.Sum256(content)
	artifact := gitstage.ApplyArtifact{
		Name:   "apply-submodule-0.bundle",
		SHA256: hex.EncodeToString(digest[:]),
	}
	runner := &fakeRunner{outputs: [][]byte{content}}
	transport := Transport{instance: InstanceName, runner: runner}
	destination := filepath.Join(t.TempDir(), artifact.Name)

	if err := transport.FetchApplyArtifact(
		context.Background(),
		"safe-run",
		artifact,
		destination,
	); err != nil {
		t.Fatal(err)
	}
	received, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(received, content) {
		t.Fatalf("fetched %q", received)
	}
	assertArgsPrefix(t, runner.calls[0], "shell", "pisafe", "sh", "-ceu")
	fetchArgs := runner.calls[0].args
	if got, want := fetchArgs[len(fetchArgs)-2:], []string{
		"safe-run", artifact.Name,
	}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("fetch args = %#v, want %#v", got, want)
	}
	if strings.Contains(strings.Join(fetchArgs, " "), destination) {
		t.Fatalf("VM was told the Mac path: %v", fetchArgs)
	}

	// A transfer that does not hash to what the run declared leaves nothing
	// behind for the import to read.
	corrupted := filepath.Join(t.TempDir(), artifact.Name)
	transport.runner = &fakeRunner{outputs: [][]byte{[]byte("tampered")}}
	err = transport.FetchApplyArtifact(context.Background(), "safe-run", artifact, corrupted)
	if err == nil || !strings.Contains(err.Error(), "changed in transfer") {
		t.Fatalf("error = %v", err)
	}
	if _, err := os.Stat(corrupted); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("corrupted artifact was kept: %v", err)
	}
}

func TestFetchApplyArtifactRejectsNamesOutsideTheApplyContract(t *testing.T) {
	runner := &fakeRunner{}
	transport := Transport{instance: InstanceName, runner: runner}
	hash := strings.Repeat("a", 64)
	for _, artifact := range []gitstage.ApplyArtifact{
		{Name: "../escape", SHA256: hash},
		{Name: "apply.bundle.bak", SHA256: hash},
		{Name: "apply-submodule-.bundle", SHA256: hash},
		{Name: "source.bundle", SHA256: hash},
		{Name: "apply.bundle", SHA256: "not-a-hash"},
		{Name: "apply.bundle"},
	} {
		err := transport.FetchApplyArtifact(
			context.Background(),
			"safe-run",
			artifact,
			filepath.Join(t.TempDir(), "out"),
		)
		if err == nil {
			t.Errorf("FetchApplyArtifact(%#v) unexpectedly succeeded", artifact)
		}
	}
	if len(runner.calls) != 0 {
		t.Fatalf("rejected artifacts still reached the VM: %#v", runner.calls)
	}
}

func TestUploadArtifactRejectsNamesOutsideTheStageContract(t *testing.T) {
	transport := Transport{instance: InstanceName, runner: &fakeRunner{}}
	for _, name := range []string{
		"../escape", "submodule-.bundle", "submodule-0.tar", "submodule-99999.patch",
		"submodule-0.bundle.extra", "source.bundle.bak",
	} {
		err := transport.uploadArtifact(context.Background(), "safe-run", name, "", []byte("x"))
		if err == nil || !strings.Contains(err.Error(), "unsupported stage artifact") {
			t.Errorf("uploadArtifact(%q) error = %v", name, err)
		}
	}
}

func TestSelectCacheSnapshotsPairsEveryCacheWithItsGeneration(t *testing.T) {
	runner := &fakeRunner{outputs: [][]byte{
		[]byte("0123456789abcdef\n\nfedcba9876543210\n"),
	}}
	transport := Transport{instance: InstanceName, runner: runner}
	requested := []runcontainer.CacheMount{
		{Name: "npm", Env: []string{"npm_config_cache"}, Key: "0123456789abcdef"},
		{Name: "cargo", Env: []string{"CARGO_HOME"}, Key: "1111111111111111"},
		{Name: "go", Env: []string{"GOMODCACHE"}, Key: "2222222222222222"},
	}

	selected, err := transport.SelectCacheSnapshots(context.Background(), "project-3f9c2a1b", requested)
	if err != nil {
		t.Fatal(err)
	}
	// An exact hit, an empty namespace, and a fallback to an older generation
	// are the three outcomes, and each must land on its own cache.
	for index, want := range []string{"0123456789abcdef", "", "fedcba9876543210"} {
		if selected[index].Snapshot != want {
			t.Errorf("%s = %q, want %q", selected[index].Name, selected[index].Snapshot, want)
		}
	}
	arguments := runner.calls[0].args
	if !slices.Contains(arguments, "project-3f9c2a1b") || !slices.Contains(arguments, "cargo") {
		t.Fatalf("selection command = %v", arguments)
	}
}

// TestPublishCacheSnapshotNamesTheRunItReads pins what the publishing command
// is told: which namespace the generation lands in, which key names it, and
// which run's overlay is read for it. Getting any of them from a different run
// would publish one run's work into another's history.
func TestPublishCacheSnapshotNamesTheRunItReads(t *testing.T) {
	runner := &fakeRunner{}
	transport := Transport{instance: InstanceName, runner: runner}
	spec := runcontainer.DefaultSpec("run-1", "project-3f9c2a1b", testRunImage)
	cache := runcontainer.CacheMount{
		Name:     "npm",
		Env:      []string{"npm_config_cache"},
		Key:      "0123456789abcdef",
		Snapshot: "fedcba9876543210",
	}
	spec.Caches = []runcontainer.CacheMount{cache}

	if err := transport.PublishCacheSnapshot(context.Background(), spec, cache); err != nil {
		t.Fatal(err)
	}
	arguments := runner.calls[0].args
	for _, want := range []string{"project-3f9c2a1b", "npm", "0123456789abcdef", "run-1"} {
		if !slices.Contains(arguments, want) {
			t.Fatalf("publish command omits %q: %v", want, arguments)
		}
	}
	// The generation is read out of a container that mounts the run's overlay,
	// so the run container's own lower and upper have to be in the command.
	joined := strings.Join(arguments, " ")
	if !strings.Contains(joined, "/var/lib/pisafe/projects/project-3f9c2a1b/cache/npm/fedcba9876543210:") ||
		!strings.Contains(joined, "upperdir=/var/lib/pisafe/runs/run-1/overlay/cache/npm/upper") {
		t.Fatalf("publish command does not mount the run's overlay: %v", arguments)
	}
}

func TestPublishAndEvictionRefuseWhatTheyCannotAddress(t *testing.T) {
	spec := runcontainer.DefaultSpec("run-1", "project-3f9c2a1b", testRunImage)
	transport := Transport{instance: InstanceName, runner: &fakeRunner{}}
	if err := transport.PublishCacheSnapshot(
		context.Background(),
		spec,
		runcontainer.CacheMount{Name: "npm", Env: []string{"npm_config_cache"}, Key: "not-a-key"},
	); err == nil {
		t.Error("an unkeyed cache was published")
	}
	for name, evict := range map[string]func() error{
		// Keeping nothing would evict the generation the namespace was just
		// published to, which is the one every later run starts from.
		"keeping nothing": func() error {
			return transport.EvictCacheSnapshots(context.Background(), "project-3f9c2a1b", "npm", 0, nil)
		},
		"climbing held generation": func() error {
			return transport.EvictCacheSnapshots(
				context.Background(), "project-3f9c2a1b", "npm", 3, []string{"../../sessions"},
			)
		},
		"nested namespace": func() error {
			return transport.EvictCacheSnapshots(context.Background(), "project-3f9c2a1b", "a/b", 3, nil)
		},
	} {
		if err := evict(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// TestSelectCacheSnapshotsRefusesAnUnexpectedListing keeps a directory name
// the VM produced from becoming half of a mount argument unchecked.
func TestSelectCacheSnapshotsRefusesAnUnexpectedListing(t *testing.T) {
	requested := []runcontainer.CacheMount{
		{Name: "npm", Env: []string{"npm_config_cache"}, Key: "0123456789abcdef"},
	}
	for name, output := range map[string]string{
		"climbing directory": "../../../etc\n",
		"unkeyed directory":  "not-a-key\n",
		"extra lines":        "0123456789abcdef\nfedcba9876543210\n",
	} {
		transport := Transport{
			instance: InstanceName,
			runner:   &fakeRunner{outputs: [][]byte{[]byte(output)}},
		}
		if _, err := transport.SelectCacheSnapshots(
			context.Background(),
			"project-3f9c2a1b",
			requested,
		); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}
