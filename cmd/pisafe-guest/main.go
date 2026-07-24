// pisafe-guest is the Linux-side helper embedded in the run image. It keeps
// Git materialization identical to the tested host implementation without
// exposing any Mac filesystem path to the VM or container.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/mpizenberg/pisafe/internal/gitstage"
	"github.com/mpizenberg/pisafe/internal/runssh"
)

const (
	sshHome               = "/home/node"
	sshPublicKeySize      = 4096
	modelsConfigSizeLimit = int64(1 << 20)
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "pisafe-guest: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, in io.Reader, out io.Writer) error {
	if len(args) == 0 {
		return usageError()
	}
	switch args[0] {
	case "materialize":
		if len(args) != 3 {
			return usageError()
		}
		return materialize(ctx, args[1], args[2], out)
	case "configure-ssh":
		if len(args) != 1 {
			return usageError()
		}
		return configureSSH(ctx, sshHome, in, out, generateHostKey)
	case "configure-inference":
		if len(args) != 1 {
			return usageError()
		}
		return configureInference(sshHome, in)
	case "serve-ssh":
		if len(args) != 1 {
			return usageError()
		}
		return serveSSH(sshHome)
	case "proxy-ssh":
		if len(args) != 1 {
			return usageError()
		}
		return proxySSH(ctx, in, out)
	default:
		return usageError()
	}
}

func usageError() error {
	return errors.New(
		"usage: pisafe-guest <materialize <stage-directory> <workspace>" +
			"|configure-ssh|configure-inference|serve-ssh|proxy-ssh>",
	)
}

// configureInference replaces ~/.pi/agent/models.json with the content piped
// from the Mac controller. It replaces atomically because resume rotates the
// run capability while Pi may already be installed and configured.
func configureInference(home string, in io.Reader) error {
	content, err := io.ReadAll(io.LimitReader(in, modelsConfigSizeLimit+1))
	if err != nil {
		return fmt.Errorf("read models configuration: %w", err)
	}
	if int64(len(content)) > modelsConfigSizeLimit {
		return errors.New("models configuration exceeds size limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	var parsed map[string]any
	if err := decoder.Decode(&parsed); err != nil {
		return fmt.Errorf("models configuration is not a JSON object: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("models configuration contains trailing data")
	}

	agentDirectory := filepath.Join(filepath.Clean(home), ".pi", "agent")
	if err := os.MkdirAll(agentDirectory, 0o700); err != nil {
		return fmt.Errorf("create Pi agent directory: %w", err)
	}
	target := filepath.Join(agentDirectory, "models.json")
	temporary, err := os.CreateTemp(agentDirectory, ".models-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary models configuration: %w", err)
	}
	temporaryPath := temporary.Name()
	complete := false
	defer func() {
		temporary.Close()
		if !complete {
			os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("restrict models configuration: %w", err)
	}
	if _, err := temporary.Write(content); err != nil {
		return fmt.Errorf("write models configuration: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync models configuration: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close models configuration: %w", err)
	}
	if err := os.Rename(temporaryPath, target); err != nil {
		return fmt.Errorf("install models configuration: %w", err)
	}
	complete = true
	return nil
}

func materialize(
	ctx context.Context,
	stagePath string,
	workspacePath string,
	out io.Writer,
) error {
	stageDirectory := filepath.Clean(stagePath)
	workspace := filepath.Clean(workspacePath)
	snapshotFile, err := os.Open(filepath.Join(stageDirectory, "snapshot.json"))
	if err != nil {
		return fmt.Errorf("open stage snapshot: %w", err)
	}
	defer snapshotFile.Close()
	decoder := json.NewDecoder(io.LimitReader(snapshotFile, 1<<20))
	decoder.DisallowUnknownFields()
	var snapshot gitstage.Snapshot
	if err := decoder.Decode(&snapshot); err != nil {
		return fmt.Errorf("decode stage snapshot: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("stage snapshot contains trailing data")
		}
		return fmt.Errorf("decode stage snapshot trailer: %w", err)
	}
	if snapshot.SourceRoot != "" {
		return errors.New("stage snapshot unexpectedly contains a host source path")
	}

	materialized, err := gitstage.Materialize(ctx, gitstage.PreparedStage{
		Snapshot:   snapshot,
		BundlePath: filepath.Join(stageDirectory, "source.bundle"),
		PatchPath:  filepath.Join(stageDirectory, "tracked.patch"),
	}, workspace)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(out)
	return encoder.Encode(materialized)
}

func configureSSH(
	ctx context.Context,
	home string,
	in io.Reader,
	out io.Writer,
	keygen func(context.Context, string) error,
) error {
	publicKeyContent, err := io.ReadAll(io.LimitReader(in, sshPublicKeySize+1))
	if err != nil {
		return fmt.Errorf("read SSH public key: %w", err)
	}
	if len(publicKeyContent) > sshPublicKeySize {
		return errors.New("SSH public key exceeds size limit")
	}
	publicKey, _, err := runssh.ParsePublicKey(string(publicKeyContent))
	if err != nil {
		return fmt.Errorf("validate SSH public key: %w", err)
	}

	directory := filepath.Join(filepath.Clean(home), ".ssh")
	if err := os.Mkdir(directory, 0o700); err != nil {
		return fmt.Errorf("create SSH directory: %w", err)
	}
	hostKey := filepath.Join(directory, "ssh_host_ed25519_key")
	if err := keygen(ctx, hostKey); err != nil {
		return fmt.Errorf("generate SSH host key: %w", err)
	}
	if err := os.Chmod(hostKey, 0o600); err != nil {
		return fmt.Errorf("restrict SSH host key: %w", err)
	}
	hostPublicKey, err := os.ReadFile(hostKey + ".pub")
	if err != nil {
		return fmt.Errorf("read SSH host public key: %w", err)
	}
	canonicalHostKey, _, err := runssh.ParsePublicKey(string(hostPublicKey))
	if err != nil {
		return fmt.Errorf("validate SSH host public key: %w", err)
	}

	authorized := filepath.Join(directory, "authorized_keys")
	authorizedKey := "no-agent-forwarding,no-port-forwarding,no-X11-forwarding,no-user-rc " +
		publicKey + "\n"
	if err := writeNewFile(authorized, []byte(authorizedKey), 0o600); err != nil {
		return fmt.Errorf("write SSH authorized key: %w", err)
	}
	config := sshdConfig(directory)
	if err := writeNewFile(
		filepath.Join(directory, "sshd_config"),
		[]byte(config),
		0o600,
	); err != nil {
		return fmt.Errorf("write SSH daemon config: %w", err)
	}
	if _, err := fmt.Fprintln(out, canonicalHostKey); err != nil {
		return fmt.Errorf("write SSH host public key: %w", err)
	}
	return nil
}

func generateHostKey(ctx context.Context, path string) error {
	command := exec.CommandContext(
		ctx,
		"/usr/bin/ssh-keygen",
		"-q",
		"-t", "ed25519",
		"-N", "",
		"-C", "pisafe-run-host",
		"-f", path,
	)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, output)
	}
	return nil
}

func serveSSH(home string) error {
	directory := filepath.Join(filepath.Clean(home), ".ssh")
	config := filepath.Join(directory, "sshd_config")
	for _, required := range []string{
		config,
		filepath.Join(directory, "authorized_keys"),
		filepath.Join(directory, "ssh_host_ed25519_key"),
	} {
		info, err := os.Lstat(required)
		if err != nil {
			return fmt.Errorf("inspect SSH runtime file: %w", err)
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("SSH runtime path is not a regular file: %s", required)
		}
	}
	if err := os.MkdirAll("/run/sshd", 0o700); err != nil {
		return fmt.Errorf("create SSH runtime directory: %w", err)
	}
	return syscall.Exec(
		"/usr/sbin/sshd",
		[]string{"/usr/sbin/sshd", "-D", "-e", "-f", config},
		os.Environ(),
	)
}

func proxySSH(ctx context.Context, in io.Reader, out io.Writer) error {
	return proxySSHTo(ctx, "127.0.0.1:2222", in, out)
}

func proxySSHTo(ctx context.Context, address string, in io.Reader, out io.Writer) error {
	connection, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err != nil {
		return fmt.Errorf("connect to container SSH daemon: %w", err)
	}
	return relaySSH(connection, in, out)
}

func relaySSH(connection net.Conn, in io.Reader, out io.Writer) error {
	defer connection.Close()

	inputDone := make(chan error, 1)
	go func() {
		_, copyErr := io.Copy(connection, in)
		if tcp, ok := connection.(*net.TCPConn); ok {
			if copyErr == nil {
				copyErr = tcp.CloseWrite()
			} else {
				_ = tcp.Close()
			}
		}
		inputDone <- copyErr
	}()
	_, outputErr := io.Copy(out, connection)
	if closeErr := connection.Close(); outputErr == nil {
		outputErr = closeErr
	}
	select {
	case inputErr := <-inputDone:
		return errors.Join(inputErr, outputErr)
	default:
		return outputErr
	}
}

func sshdConfig(directory string) string {
	return "Port 2222\n" +
		"ListenAddress 127.0.0.1\n" +
		"HostKey " + filepath.Join(directory, "ssh_host_ed25519_key") + "\n" +
		"AuthorizedKeysFile " + filepath.Join(directory, "authorized_keys") + "\n" +
		"PidFile /run/sshd.pid\n" +
		"PasswordAuthentication no\n" +
		"KbdInteractiveAuthentication no\n" +
		"PermitEmptyPasswords no\n" +
		"PubkeyAuthentication yes\n" +
		"AuthenticationMethods publickey\n" +
		"PermitRootLogin no\n" +
		"AllowUsers node\n" +
		"UsePAM no\n" +
		"StrictModes yes\n" +
		"X11Forwarding no\n" +
		"AllowAgentForwarding no\n" +
		"AllowTcpForwarding no\n" +
		"PermitTunnel no\n" +
		"GatewayPorts no\n" +
		"PermitUserEnvironment no\n" +
		"PermitUserRC no\n" +
		"PrintMotd no\n" +
		"UseDNS no\n" +
		"MaxAuthTries 2\n" +
		"MaxSessions 32\n" +
		"Subsystem sftp internal-sftp\n"
}

func writeNewFile(path string, content []byte, mode os.FileMode) error {
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
