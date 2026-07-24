// Package runssh owns the Mac-side SSH credential and configuration files for
// isolated runs.
package runssh

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/mpizenberg/pisafe/internal/runid"
)

const (
	publicKeyLimit = 4096
	sshUser        = "node"
)

type commandRunner interface {
	Run(context.Context, ...string) error
}

type Store struct {
	root   string
	runner commandRunner
}

type Prepared struct {
	RunID        string
	PublicKey    string
	IdentityFile string
}

type Gateway struct {
	ConfigFile string
	Alias      string
}

type Endpoint struct {
	Alias              string
	IdentityFile       string
	KnownHostsFile     string
	ConfigFile         string
	HostKeyFingerprint string
}

func NewStore(root string) Store {
	cleaned := filepath.Clean(root)
	if root != "" {
		absolute, err := filepath.Abs(cleaned)
		if err == nil {
			cleaned = absolute
		}
	} else {
		cleaned = ""
	}
	return Store{
		root:   cleaned,
		runner: execCommand{binary: "ssh-keygen"},
	}
}

func (store Store) Prepare(ctx context.Context, runID string) (_ Prepared, returnErr error) {
	if err := runid.Validate(runID); err != nil {
		return Prepared{}, err
	}
	if err := store.ensureRoot(); err != nil {
		return Prepared{}, err
	}
	runRoot := store.runRoot(runID)
	if err := os.Mkdir(runRoot, 0o700); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return Prepared{}, fmt.Errorf("SSH credentials for run %q already exist", runID)
		}
		return Prepared{}, fmt.Errorf("create run SSH directory: %w", err)
	}
	defer func() {
		if returnErr != nil {
			returnErr = errors.Join(returnErr, store.Remove(runID))
		}
	}()

	identity := filepath.Join(runRoot, "id_ed25519")
	if err := store.runner.Run(
		ctx,
		"-q",
		"-t", "ed25519",
		"-N", "",
		"-C", "pisafe:"+runID,
		"-f", identity,
	); err != nil {
		return Prepared{}, fmt.Errorf("generate run SSH client key: %w", err)
	}
	if err := restrictRegularFile(identity, 0o600); err != nil {
		return Prepared{}, fmt.Errorf("validate run SSH private key: %w", err)
	}
	publicPath := identity + ".pub"
	if err := restrictRegularFile(publicPath, 0o600); err != nil {
		return Prepared{}, fmt.Errorf("validate run SSH public key: %w", err)
	}
	content, err := readLimitedRegularFile(publicPath, publicKeyLimit)
	if err != nil {
		return Prepared{}, fmt.Errorf("read run SSH public key: %w", err)
	}
	publicKey, _, err := ParsePublicKey(string(content))
	if err != nil {
		return Prepared{}, fmt.Errorf("validate run SSH public key: %w", err)
	}
	return Prepared{
		RunID:        runID,
		PublicKey:    publicKey,
		IdentityFile: identity,
	}, nil
}

func (store Store) Finalize(
	prepared Prepared,
	hostPublicKey string,
	gateway Gateway,
	containerName string,
) (_ Endpoint, returnErr error) {
	if err := runid.Validate(prepared.RunID); err != nil {
		return Endpoint{}, err
	}
	if prepared.IdentityFile != filepath.Join(store.runRoot(prepared.RunID), "id_ed25519") {
		return Endpoint{}, errors.New("SSH identity path does not match run")
	}
	if !safeConfigPath(prepared.IdentityFile) {
		return Endpoint{}, errors.New("invalid SSH identity path")
	}
	if err := restrictRegularFile(prepared.IdentityFile, 0o600); err != nil {
		return Endpoint{}, fmt.Errorf("validate run SSH private key: %w", err)
	}
	if !safeAlias(gateway.Alias) {
		return Endpoint{}, errors.New("invalid Lima SSH alias")
	}
	if !safeConfigPath(gateway.ConfigFile) {
		return Endpoint{}, errors.New("invalid Lima SSH config path")
	}
	if _, err := readLimitedRegularFile(gateway.ConfigFile, 1<<20); err != nil {
		return Endpoint{}, fmt.Errorf("validate Lima SSH config: %w", err)
	}
	if containerName != "pisafe-run-"+prepared.RunID {
		return Endpoint{}, errors.New("SSH container does not match run")
	}
	hostKey, hostBlob, err := ParsePublicKey(hostPublicKey)
	if err != nil {
		return Endpoint{}, fmt.Errorf("validate run SSH host key: %w", err)
	}

	alias := "pisafe-" + prepared.RunID
	knownHosts := filepath.Join(store.runRoot(prepared.RunID), "known_hosts")
	configFile := filepath.Join(store.runRoot(prepared.RunID), "ssh.config")
	wroteKnownHosts := false
	defer func() {
		if returnErr != nil && wroteKnownHosts {
			os.Remove(knownHosts)
			os.Remove(configFile)
		}
	}()
	if err := writeExclusive(
		knownHosts,
		[]byte(alias+" "+hostKey+"\n"),
		0o600,
	); err != nil {
		return Endpoint{}, fmt.Errorf("write run known-hosts file: %w", err)
	}
	wroteKnownHosts = true
	config := renderConfig(
		alias,
		prepared.IdentityFile,
		knownHosts,
		gateway,
		containerName,
	)
	if err := writeExclusive(configFile, []byte(config), 0o600); err != nil {
		return Endpoint{}, fmt.Errorf("write run SSH config: %w", err)
	}

	digest := sha256.Sum256(hostBlob)
	return Endpoint{
		Alias:              alias,
		IdentityFile:       prepared.IdentityFile,
		KnownHostsFile:     knownHosts,
		ConfigFile:         configFile,
		HostKeyFingerprint: "SHA256:" + base64.RawStdEncoding.EncodeToString(digest[:]),
	}, nil
}

func (store Store) Remove(runID string) error {
	if err := runid.Validate(runID); err != nil {
		return err
	}
	if !safeConfigPath(store.root) {
		return errors.New("SSH state path must be absolute")
	}
	path := store.runRoot(runID)
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove run SSH credentials: %w", err)
	}
	return nil
}

func ParsePublicKey(input string) (string, []byte, error) {
	fields := strings.Fields(input)
	if len(fields) < 2 || fields[0] != "ssh-ed25519" {
		return "", nil, errors.New("expected an Ed25519 SSH public key")
	}
	if strings.ContainsAny(input, "\r\x00") || strings.Count(strings.TrimSpace(input), "\n") != 0 {
		return "", nil, errors.New("SSH public key must occupy one line")
	}
	blob, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil {
		return "", nil, errors.New("SSH public key has invalid base64")
	}
	algorithm, rest, ok := readSSHString(blob)
	if !ok || string(algorithm) != "ssh-ed25519" {
		return "", nil, errors.New("SSH public key algorithm does not match its prefix")
	}
	key, rest, ok := readSSHString(rest)
	if !ok || len(key) != 32 || len(rest) != 0 {
		return "", nil, errors.New("SSH public key has an invalid Ed25519 payload")
	}
	return "ssh-ed25519 " + fields[1], blob, nil
}

func readSSHString(value []byte) ([]byte, []byte, bool) {
	if len(value) < 4 {
		return nil, nil, false
	}
	length := binary.BigEndian.Uint32(value[:4])
	if uint64(length) > uint64(len(value)-4) {
		return nil, nil, false
	}
	end := 4 + int(length)
	return value[4:end], value[end:], true
}

func (store Store) ensureRoot() error {
	if !safeConfigPath(store.root) {
		return errors.New("SSH state path must be absolute")
	}
	if err := os.MkdirAll(store.root, 0o700); err != nil {
		return fmt.Errorf("create SSH state directory: %w", err)
	}
	info, err := os.Lstat(store.root)
	if err != nil {
		return fmt.Errorf("inspect SSH state directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("SSH state path is not a directory")
	}
	if info.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(store.root, 0o700); err != nil {
			return fmt.Errorf("restrict SSH state directory: %w", err)
		}
	}
	return nil
}

func (store Store) runRoot(runID string) string {
	return filepath.Join(store.root, runID)
}

func readLimitedRegularFile(path string, limit int64) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Size() > limit {
		return nil, errors.New("file is not a bounded regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit || !os.SameFile(before, info) {
		return nil, errors.New("file is not a bounded regular file")
	}
	content, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(content)) > limit {
		return nil, errors.New("file exceeds size limit")
	}
	return content, nil
}

func restrictRegularFile(path string, mode fs.FileMode) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("path is not a regular file")
	}
	if err := os.Chmod(path, mode); err != nil {
		return err
	}
	return nil
}

func writeExclusive(path string, content []byte, mode fs.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	complete := false
	defer func() {
		file.Close()
		if !complete {
			os.Remove(path)
		}
	}()
	if _, err := file.Write(content); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	complete = true
	return nil
}

func renderConfig(
	alias string,
	identity string,
	knownHosts string,
	gateway Gateway,
	containerName string,
) string {
	return "Host " + alias + "\n" +
		"  HostName 127.0.0.1\n" +
		"  User " + sshUser + "\n" +
		"  IdentityFile " + quoteConfig(identity) + "\n" +
		"  IdentitiesOnly yes\n" +
		"  BatchMode yes\n" +
		"  PasswordAuthentication no\n" +
		"  KbdInteractiveAuthentication no\n" +
		"  PubkeyAuthentication yes\n" +
		"  StrictHostKeyChecking yes\n" +
		"  UserKnownHostsFile " + quoteConfig(knownHosts) + "\n" +
		"  HostKeyAlias " + alias + "\n" +
		"  ForwardAgent no\n" +
		"  ForwardX11 no\n" +
		"  ClearAllForwardings yes\n" +
		"  ProxyCommand ssh -F " + quoteConfig(gateway.ConfigFile) +
		" -o ClearAllForwardings=yes -o BatchMode=yes " + gateway.Alias +
		" /usr/bin/podman exec --interactive " + containerName +
		" /usr/local/bin/pisafe-guest proxy-ssh\n"
}

func quoteConfig(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	return `"` + value + `"`
}

func safeConfigPath(path string) bool {
	return filepath.IsAbs(path) && !strings.ContainsAny(path, "\r\n\x00")
}

func safeAlias(alias string) bool {
	if alias == "" {
		return false
	}
	for _, character := range alias {
		if character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' ||
			character == '.' ||
			character == '_' ||
			character == '-' {
			continue
		}
		return false
	}
	return true
}

type execCommand struct {
	binary string
}

func (runner execCommand) Run(ctx context.Context, args ...string) error {
	command := exec.CommandContext(ctx, runner.binary, args...)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return errors.New(detail)
	}
	return nil
}
