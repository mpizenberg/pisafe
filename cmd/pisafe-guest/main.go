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
	"strings"
	"syscall"

	"github.com/mpizenberg/pisafe/internal/gitstage"
	"github.com/mpizenberg/pisafe/internal/profile"
	"github.com/mpizenberg/pisafe/internal/runcopy"
	"github.com/mpizenberg/pisafe/internal/runssh"
)

const (
	runHome = "/home/node"
	// profileRoot is where the read-only profile is mounted and workRoot where
	// the run's own workspace is. The controller composes what this helper is
	// told, and the helper still refuses a path outside either, so no
	// configuration can point Pi at something that is not the run's.
	profileRoot           = "/opt/pisafe/profile"
	workRoot              = "/work"
	sshPublicKeySize      = 4096
	modelsConfigSizeLimit = int64(1 << 20)
	documentSizeLimit     = int64(1 << 20)
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
	case "prepare-apply":
		if len(args) != 4 {
			return usageError()
		}
		return prepareApply(ctx, args[1], args[2], args[3], in, out)
	case "diff":
		if len(args) != 2 {
			return usageError()
		}
		return diffRun(ctx, args[1], in, out)
	case "export":
		if len(args) != 3 {
			return usageError()
		}
		return runcopy.Archive(args[1], args[2], out)
	case "import":
		if len(args) != 5 {
			return usageError()
		}
		return importCopy(args[1], args[2], args[3], args[4], in, out)
	case "configure-ssh":
		if len(args) != 1 {
			return usageError()
		}
		return configureSSH(ctx, runHome, in, out, generateHostKey)
	case "configure-inference":
		if len(args) != 1 {
			return usageError()
		}
		return configureInference(runHome, in)
	case "configure-identity":
		if len(args) != 1 {
			return usageError()
		}
		return configureIdentity(ctx, runHome, in)
	case "configure-profile":
		if len(args) != 1 {
			return usageError()
		}
		return configureProfile(runHome, in)
	case "serve-ssh":
		if len(args) != 1 {
			return usageError()
		}
		return serveSSH(runHome)
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
			"|prepare-apply <keep|drop> <workspace> <package-directory>" +
			"|diff <workspace>" +
			"|export <workspace> <path>" +
			"|import <workspace> <destination> <name> <replace|refuse>" +
			"|configure-ssh|configure-inference|configure-identity|configure-profile" +
			"|serve-ssh|proxy-ssh>",
	)
}

// importCopy unpacks an archive the Mac sent into the run's workspace. An empty
// destination is the workspace itself, and a destination that is already a
// directory takes the copy inside it under the name it arrived with, which is
// what a copy out of a run does with a destination on the Mac. The archive
// lands in a staging directory and is renamed into place, so a run watching the
// destination never sees a half-written copy.
func importCopy(workspace, destination, name, decision string, in io.Reader, out io.Writer) error {
	replace := decision == "replace"
	if !replace && decision != "refuse" {
		return usageError()
	}
	if name == "" || name != filepath.Base(name) || name == "." || name == ".." {
		return fmt.Errorf("%q is not a name a copy can arrive under", name)
	}
	target := filepath.Clean(workspace)
	if destination != "" {
		relative, err := runcopy.SafePath(destination)
		if err != nil {
			return err
		}
		target = filepath.Join(target, filepath.FromSlash(relative))
	}
	if info, err := os.Stat(target); err == nil && info.IsDir() {
		target = filepath.Join(target, name)
	}
	entries, err := runcopy.CopyTo(in, name, target, replace)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(entries)
}

// configureIdentity records the user's Git identity in the run's global Git
// configuration, so an agent commits as the person whose work it is instead of
// failing with an unknown author.
func configureIdentity(ctx context.Context, home string, in io.Reader) error {
	identity, err := decodeControllerDocument[gitstage.Identity](in, "Git identity")
	if err != nil {
		return err
	}
	return gitstage.InstallIdentity(
		ctx,
		filepath.Join(filepath.Clean(home), ".gitconfig"),
		identity,
	)
}

// configureInference replaces ~/.pi/agent/models.json with the content piped
// from the Mac controller. It replaces atomically because resume rotates the
// run capability while Pi may already be installed and configured. It also
// pins Pi's transport to SSE: the default auto transport first dials a
// WebSocket, which the broker's HTTP relay cannot speak.
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
	if err := writeAgentFile(agentDirectory, "models.json", content); err != nil {
		return err
	}
	settings, err := updateAgentSettings(
		filepath.Join(agentDirectory, "settings.json"),
		func(settings map[string]any) { settings["transport"] = "sse" },
	)
	if err != nil {
		return err
	}
	if settings == nil {
		return nil
	}
	return writeAgentFile(agentDirectory, "settings.json", settings)
}

// configureProfile tells Pi which packages the read-only profile offers and
// which directory it may load project resources from. Both files it writes are
// ones Pi writes too, so they are copied into the run rather than mounted, and
// whatever Pi then makes of them dies with the run.
func configureProfile(home string, in io.Reader) error {
	configuration, err := decodeControllerDocument[profile.Configuration](
		in,
		"profile configuration",
	)
	if err != nil {
		return err
	}
	packages := make([]any, 0, len(configuration.Packages))
	for _, path := range configuration.Packages {
		if !strings.HasPrefix(path, profileRoot+"/") || filepath.Clean(path) != path {
			return fmt.Errorf("profile package %q is not in the mounted profile", path)
		}
		packages = append(packages, path)
	}
	workspace := configuration.Workspace
	if !strings.HasPrefix(workspace, workRoot+"/") || filepath.Clean(workspace) != workspace {
		return fmt.Errorf("workspace %q is not in the run", workspace)
	}

	agentDirectory := filepath.Join(filepath.Clean(home), ".pi", "agent")
	if err := os.MkdirAll(agentDirectory, 0o700); err != nil {
		return fmt.Errorf("create Pi agent directory: %w", err)
	}
	settings, err := updateAgentSettings(
		filepath.Join(agentDirectory, "settings.json"),
		func(settings map[string]any) {
			settings["packages"] = append(packages, runInstalledPackages(settings)...)
		},
	)
	if err != nil {
		return err
	}
	if settings != nil {
		if err := writeAgentFile(agentDirectory, "settings.json", settings); err != nil {
			return err
		}
	}
	return trustWorkspace(agentDirectory, workspace)
}

// runInstalledPackages is everything in the run's settings that the profile did
// not put there — what pi install added inside the run. The profile is the
// authority on its own entries, so those are rebuilt from the record every
// start and a stale one disappears; a run's own belong to the run, and dropping
// them on resume would unload a package that is still installed.
func runInstalledPackages(settings map[string]any) []any {
	existing, _ := settings["packages"].([]any)
	kept := make([]any, 0, len(existing))
	for _, entry := range existing {
		if path, ok := entry.(string); ok && strings.HasPrefix(path, profileRoot+"/") {
			continue
		}
		kept = append(kept, entry)
	}
	return kept
}

// trustWorkspace records the run's own workspace as trusted, so Pi loads the
// repository's own settings, extensions, and skills without asking. Project
// trust guards a Pi that runs outside a sandbox against the repository it opens;
// here the container is that guard, and leaving the question unanswered would
// cost a prompt every run and silently drop the repository's own configuration
// whenever Pi runs non-interactively.
func trustWorkspace(agentDirectory, workspace string) error {
	path := filepath.Join(agentDirectory, "trust.json")
	decisions := readAgentDocument(path)
	if decisions[workspace] == true {
		return nil
	}
	decisions[workspace] = true
	content, err := json.MarshalIndent(decisions, "", "  ")
	if err != nil {
		return fmt.Errorf("encode project trust: %w", err)
	}
	return writeAgentFile(agentDirectory, "trust.json", append(content, '\n'))
}

// updateAgentSettings applies update to the run's Pi settings and returns the
// result, or nil when they already say what update wants. Settings Pi wrote
// during the run are preserved; an unreadable file is replaced rather than
// allowed to break the run.
func updateAgentSettings(path string, update func(map[string]any)) ([]byte, error) {
	settings := readAgentDocument(path)
	before, err := json.Marshal(settings)
	if err != nil {
		return nil, fmt.Errorf("encode agent settings: %w", err)
	}
	update(settings)
	after, err := json.Marshal(settings)
	if err != nil {
		return nil, fmt.Errorf("encode agent settings: %w", err)
	}
	if bytes.Equal(before, after) {
		return nil, nil
	}
	merged, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode agent settings: %w", err)
	}
	return append(merged, '\n'), nil
}

// readAgentDocument reads one JSON object Pi maintains. Anything unreadable
// reads as empty, because a run that cannot be configured is worse than one
// that loses a setting the agent corrupted.
func readAgentDocument(path string) map[string]any {
	existing, err := os.ReadFile(path)
	if err != nil || int64(len(existing)) > modelsConfigSizeLimit {
		return map[string]any{}
	}
	var parsed map[string]any
	if json.Unmarshal(existing, &parsed) != nil || parsed == nil {
		return map[string]any{}
	}
	return parsed
}

func writeAgentFile(agentDirectory, name string, content []byte) error {
	target := filepath.Join(agentDirectory, name)
	temporary, err := os.CreateTemp(agentDirectory, "."+name+"-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary %s: %w", name, err)
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
		return fmt.Errorf("restrict %s: %w", name, err)
	}
	if _, err := temporary.Write(content); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", name, err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close %s: %w", name, err)
	}
	if err := os.Rename(temporaryPath, target); err != nil {
		return fmt.Errorf("install %s: %w", name, err)
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
	snapshot, err := decodeSnapshot(snapshotFile)
	if err != nil {
		return err
	}

	submodules := make([]gitstage.PreparedSubmodule, 0, len(snapshot.Submodules))
	for index, submodule := range snapshot.Submodules {
		submodules = append(submodules, gitstage.PreparedSubmodule{
			Path:       submodule.Path,
			BundlePath: filepath.Join(stageDirectory, fmt.Sprintf("submodule-%d.bundle", index)),
			PatchPath:  filepath.Join(stageDirectory, fmt.Sprintf("submodule-%d.patch", index)),
		})
	}
	materialized, err := gitstage.Materialize(ctx, gitstage.PreparedStage{
		Snapshot:   snapshot,
		BundlePath: filepath.Join(stageDirectory, "source.bundle"),
		PatchPath:  filepath.Join(stageDirectory, "tracked.patch"),
		InputsPath: filepath.Join(stageDirectory, "inputs.tar"),
		Submodules: submodules,
	}, workspace)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(out)
	return encoder.Encode(materialized)
}

// prepareApply captures whatever the agent left in the run and writes the
// bundles the controller then fetches. A previous attempt's package is
// superseded, so a failed apply can simply be retried.
func prepareApply(
	ctx context.Context,
	baseline string,
	workspacePath string,
	packagePath string,
	in io.Reader,
	out io.Writer,
) error {
	choice, err := gitstage.ParseBaselineChoice(baseline)
	if err != nil {
		return err
	}
	snapshot, err := decodeSnapshot(in)
	if err != nil {
		return err
	}
	packageDirectory := filepath.Clean(packagePath)
	if err := os.RemoveAll(packageDirectory); err != nil {
		return fmt.Errorf("clear apply package: %w", err)
	}
	prepared, err := gitstage.PrepareApply(
		ctx,
		snapshot,
		filepath.Clean(workspacePath),
		packageDirectory,
		choice,
	)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(prepared)
}

// diffRun reports what the run changed. It only reads the workspace, so it is
// safe to run while an agent is working in it.
func diffRun(ctx context.Context, workspacePath string, in io.Reader, out io.Writer) error {
	snapshot, err := decodeSnapshot(in)
	if err != nil {
		return err
	}
	diff, err := gitstage.DiffRun(ctx, snapshot, filepath.Clean(workspacePath))
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(diff)
}

// decodeSnapshot reads the run description the controller sends across the
// boundary. A snapshot that names a Mac path is refused rather than used.
func decodeSnapshot(in io.Reader) (gitstage.Snapshot, error) {
	snapshot, err := decodeControllerDocument[gitstage.Snapshot](in, "stage snapshot")
	if err != nil {
		return gitstage.Snapshot{}, err
	}
	if snapshot.SourceRoot != "" {
		return gitstage.Snapshot{}, errors.New(
			"stage snapshot unexpectedly contains a host source path",
		)
	}
	return snapshot, nil
}

// decodeControllerDocument reads one bounded JSON document piped in from the
// Mac controller, refusing unknown fields and trailing data so the run acts
// only on what this helper's version understands.
func decodeControllerDocument[Document any](in io.Reader, subject string) (Document, error) {
	var document Document
	decoder := json.NewDecoder(io.LimitReader(in, documentSizeLimit))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return document, fmt.Errorf("decode %s: %w", subject, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return document, fmt.Errorf("%s contains trailing data", subject)
		}
		return document, fmt.Errorf("decode %s trailer: %w", subject, err)
	}
	return document, nil
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

	home = filepath.Clean(home)
	directory := filepath.Join(home, ".ssh")
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
	authorizedKey := "no-agent-forwarding,no-X11-forwarding,no-user-rc " +
		publicKey + "\n"
	if err := writeNewFile(authorized, []byte(authorizedKey), 0o600); err != nil {
		return fmt.Errorf("write SSH authorized key: %w", err)
	}
	config := sshdConfig(home)
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

// sshdConfig renders the run's daemon configuration. sshd builds each session's
// environment from scratch rather than from its own, so the variables the
// container declares are restated here; otherwise a terminal session would run
// under a different environment than the container contract states.
//
// Local forwarding is how the Mac reaches a server the run is hosting, and is
// the only direction allowed. Only the holder of the run's private key can ask
// for one, and bounding it to this container's loopback keeps the Mac from
// asking the run to reach anything else on its behalf. Remote forwarding stays
// off, so nothing can put a Mac-side service in front of the run either.
func sshdConfig(home string) string {
	directory := filepath.Join(home, ".ssh")
	return "Port 2222\n" +
		"ListenAddress 127.0.0.1\n" +
		"HostKey " + filepath.Join(directory, "ssh_host_ed25519_key") + "\n" +
		"AuthorizedKeysFile " + filepath.Join(directory, "authorized_keys") + "\n" +
		"PidFile /run/sshd.pid\n" +
		"SetEnv GIT_TERMINAL_PROMPT=0" +
		" PI_CODING_AGENT_DIR=" + filepath.Join(home, ".pi", "agent") +
		" PI_SKIP_VERSION_CHECK=1\n" +
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
		"AllowTcpForwarding local\n" +
		"PermitOpen 127.0.0.1:*\n" +
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
