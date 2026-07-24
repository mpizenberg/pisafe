package runssh

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeCommand struct {
	err  error
	args []string
}

func (runner *fakeCommand) Run(_ context.Context, args ...string) error {
	runner.args = append([]string(nil), args...)
	if runner.err != nil {
		return runner.err
	}
	var identity string
	for index := range args {
		if args[index] == "-f" && index+1 < len(args) {
			identity = args[index+1]
		}
	}
	if identity == "" {
		return errors.New("missing identity path")
	}
	if err := os.WriteFile(identity, []byte("private"), 0o644); err != nil {
		return err
	}
	return os.WriteFile(identity+".pub", []byte(testPublicKey()+" comment\n"), 0o644)
}

func TestStorePreparesAndFinalizesRunConnection(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ssh")
	runner := &fakeCommand{}
	store := Store{root: root, runner: runner}
	prepared, err := store.Prepare(context.Background(), "run-123")
	if err != nil {
		t.Fatal(err)
	}
	if prepared.PublicKey != testPublicKey() {
		t.Fatalf("public key = %q", prepared.PublicKey)
	}
	if got := strings.Join(runner.args, " "); !strings.Contains(
		got,
		"-t ed25519 -N  -C pisafe:run-123",
	) {
		t.Fatalf("ssh-keygen args = %q", got)
	}
	info, err := os.Stat(prepared.IdentityFile)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("identity mode = %#o", info.Mode().Perm())
	}

	gatewayConfig := filepath.Join(t.TempDir(), "lima ssh.config")
	if err := os.WriteFile(gatewayConfig, []byte("Host lima-pisafe\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	endpoint, err := store.Finalize(
		prepared,
		testPublicKey()+" host\n",
		Gateway{ConfigFile: gatewayConfig, Alias: "lima-pisafe"},
		"pisafe-run-run-123",
	)
	if err != nil {
		t.Fatal(err)
	}
	if endpoint.Alias != "pisafe-run-123" ||
		!strings.HasPrefix(endpoint.HostKeyFingerprint, "SHA256:") {
		t.Fatalf("endpoint = %#v", endpoint)
	}
	config, err := os.ReadFile(endpoint.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"Host pisafe-run-123",
		"User node",
		"StrictHostKeyChecking yes",
		"ForwardAgent no",
		`ProxyCommand ssh -F "` + gatewayConfig + `"`,
		"lima-pisafe /usr/bin/podman exec --interactive pisafe-run-run-123 " +
			"/usr/local/bin/pisafe-guest proxy-ssh",
	} {
		if !strings.Contains(string(config), expected) {
			t.Errorf("SSH config lacks %q:\n%s", expected, config)
		}
	}
	knownHosts, err := os.ReadFile(endpoint.KnownHostsFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(knownHosts) != "pisafe-run-123 "+testPublicKey()+"\n" {
		t.Fatalf("known_hosts = %q", knownHosts)
	}
}

func TestStoreCleansPartialKeyGeneration(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ssh")
	store := Store{root: root, runner: &fakeCommand{err: errors.New("failed")}}
	if _, err := store.Prepare(context.Background(), "run-123"); err == nil {
		t.Fatal("Prepare unexpectedly succeeded")
	}
	if _, err := os.Stat(filepath.Join(root, "run-123")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial run directory remains: %v", err)
	}
}

func TestStoreRejectsUnsafeInputsAndDuplicateState(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ssh")
	store := Store{root: root, runner: &fakeCommand{}}
	if _, err := store.Prepare(context.Background(), "../escape"); err == nil {
		t.Fatal("unsafe run ID was accepted")
	}
	if _, err := NewStore("").Prepare(context.Background(), "run-empty"); err == nil {
		t.Fatal("empty SSH state root was accepted")
	}
	if _, err := store.Prepare(context.Background(), "run-123"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Prepare(context.Background(), "run-123"); err == nil {
		t.Fatal("duplicate SSH state was accepted")
	}
}

func TestParsePublicKeyRejectsMalformedKeys(t *testing.T) {
	for _, input := range []string{
		"",
		"ssh-rsa AAAA",
		"ssh-ed25519 !!!",
		"ssh-ed25519 " + base64.StdEncoding.EncodeToString(sshBlob("ssh-rsa", make([]byte, 32))),
		"ssh-ed25519 " + base64.StdEncoding.EncodeToString(sshBlob("ssh-ed25519", make([]byte, 31))),
		testPublicKey() + "\n" + testPublicKey(),
	} {
		if _, _, err := ParsePublicKey(input); err == nil {
			t.Errorf("ParsePublicKey(%q) unexpectedly succeeded", input)
		}
	}
}

func testPublicKey() string {
	return "ssh-ed25519 " + base64.StdEncoding.EncodeToString(
		sshBlob("ssh-ed25519", []byte("0123456789abcdef0123456789abcdef")),
	)
}

func sshBlob(algorithm string, key []byte) []byte {
	var blob []byte
	blob = appendSSHString(blob, []byte(algorithm))
	return appendSSHString(blob, key)
}

func appendSSHString(destination []byte, value []byte) []byte {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(value)))
	destination = append(destination, length[:]...)
	return append(destination, value...)
}
