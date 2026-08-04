package zedsettings

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const zedSettings = `// Zed settings
//
// See https://zed.dev/docs/configuring-zed
{
  "ssh_connections": [
    {
      "host": "office",
      "args": ["-F", "/home/user/.ssh/config"],
      "projects": [{"paths": ["/srv/app"]}]
    }
  ],
  // Whether to enable vim modes and key bindings.
  "vim_mode": true
}
`

func settingsFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	return path
}

func readSettings(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	return string(content)
}

func runConnection() Connection {
	return Connection{
		Host:       "pisafe-tessera-20260804-134311-bf9fdd2fcdb6",
		ConfigFile: "/Users/piz/Library/Application Support/pisafe/ssh/tessera/ssh.config",
	}
}

func TestEnsureAddsAConnectionWithoutDisturbingTheFileAroundIt(t *testing.T) {
	path := settingsFile(t, zedSettings)
	added, err := Ensure(path, runConnection())
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !added {
		t.Fatal("Ensure reported no change to a file without the connection")
	}
	want := `// Zed settings
//
// See https://zed.dev/docs/configuring-zed
{
  "ssh_connections": [
    {
      "host": "pisafe-tessera-20260804-134311-bf9fdd2fcdb6",
      "args": ["-F", "/Users/piz/Library/Application Support/pisafe/ssh/tessera/ssh.config"],
      "projects": []
    },
    {
      "host": "office",
      "args": ["-F", "/home/user/.ssh/config"],
      "projects": [{"paths": ["/srv/app"]}]
    }
  ],
  // Whether to enable vim modes and key bindings.
  "vim_mode": true
}
`
	if got := readSettings(t, path); got != want {
		t.Fatalf("settings after Ensure:\n%s\nwant:\n%s", got, want)
	}
}

func TestEnsureLeavesAConnectionThatIsAlreadySaved(t *testing.T) {
	connection := runConnection()
	path := settingsFile(t, zedSettings)
	if _, err := Ensure(path, connection); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	// Zed records what was opened over a connection in the entry itself, which
	// a second Ensure must not throw away.
	opened := strings.Replace(
		readSettings(t, path),
		`      "host": "`+connection.Host+`",
      "args": ["-F", "`+connection.ConfigFile+`"],
      "projects": []`,
		`      "host": "`+connection.Host+`",
      "args": ["-F", "`+connection.ConfigFile+`"],
      "projects": [{"paths": ["/work/tessera"]}]`,
		1,
	)
	if err := os.WriteFile(path, []byte(opened), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	added, err := Ensure(path, connection)
	if err != nil {
		t.Fatalf("Ensure again: %v", err)
	}
	if added {
		t.Fatal("Ensure wrote over a connection that was already saved")
	}
	if got := readSettings(t, path); got != opened {
		t.Fatalf("settings after a second Ensure:\n%s\nwant:\n%s", got, opened)
	}
}

func TestEnsureWritesTheKeyWhenTheSettingsHaveNone(t *testing.T) {
	path := settingsFile(t, "{\n  \"vim_mode\": true\n}\n")
	if _, err := Ensure(path, runConnection()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	want := `{
  "ssh_connections": [
    {
      "host": "pisafe-tessera-20260804-134311-bf9fdd2fcdb6",
      "args": ["-F", "/Users/piz/Library/Application Support/pisafe/ssh/tessera/ssh.config"],
      "projects": []
    }
  ],
  "vim_mode": true
}
`
	if got := readSettings(t, path); got != want {
		t.Fatalf("settings after Ensure:\n%s\nwant:\n%s", got, want)
	}
}

func TestEnsureWritesSettingsThatAreNotThereYet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zed", "settings.json")
	if _, err := Ensure(path, runConnection()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	want := `{
  "ssh_connections": [
    {
      "host": "pisafe-tessera-20260804-134311-bf9fdd2fcdb6",
      "args": ["-F", "/Users/piz/Library/Application Support/pisafe/ssh/tessera/ssh.config"],
      "projects": []
    }
  ]
}
`
	if got := readSettings(t, path); got != want {
		t.Fatalf("created settings:\n%s\nwant:\n%s", got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat settings: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("created settings mode %v, want 0600", info.Mode().Perm())
	}
}

func TestEnsureFillsAnEmptyConnectionList(t *testing.T) {
	path := settingsFile(t, "{\n  \"ssh_connections\": [],\n  \"vim_mode\": true\n}\n")
	if _, err := Ensure(path, runConnection()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	want := `{
  "ssh_connections": [
    {
      "host": "pisafe-tessera-20260804-134311-bf9fdd2fcdb6",
      "args": ["-F", "/Users/piz/Library/Application Support/pisafe/ssh/tessera/ssh.config"],
      "projects": []
    }
  ],
  "vim_mode": true
}
`
	if got := readSettings(t, path); got != want {
		t.Fatalf("settings after Ensure:\n%s\nwant:\n%s", got, want)
	}
}

func TestRemovePutsTheFileBackAsEnsureFoundIt(t *testing.T) {
	path := settingsFile(t, zedSettings)
	if _, err := Ensure(path, runConnection()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	removed, err := Remove(path, runConnection().Host)
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !removed {
		t.Fatal("Remove reported no change to a file holding the connection")
	}
	if got := readSettings(t, path); got != zedSettings {
		t.Fatalf("settings after Remove:\n%s\nwant:\n%s", got, zedSettings)
	}
}

func TestRemoveTakesTheLastConnectionWithoutLeavingASeparator(t *testing.T) {
	path := settingsFile(t, zedSettings)
	if _, err := Ensure(path, runConnection()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if _, err := Remove(path, "office"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	want := `// Zed settings
//
// See https://zed.dev/docs/configuring-zed
{
  "ssh_connections": [
    {
      "host": "pisafe-tessera-20260804-134311-bf9fdd2fcdb6",
      "args": ["-F", "/Users/piz/Library/Application Support/pisafe/ssh/tessera/ssh.config"],
      "projects": []
    }
  ],
  // Whether to enable vim modes and key bindings.
  "vim_mode": true
}
`
	if got := readSettings(t, path); got != want {
		t.Fatalf("settings after Remove:\n%s\nwant:\n%s", got, want)
	}
}

func TestRemoveEmptiesTheListWhenNothingElseIsSaved(t *testing.T) {
	path := settingsFile(t, zedSettings)
	if _, err := Remove(path, "office"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	want := `// Zed settings
//
// See https://zed.dev/docs/configuring-zed
{
  "ssh_connections": [],
  // Whether to enable vim modes and key bindings.
  "vim_mode": true
}
`
	if got := readSettings(t, path); got != want {
		t.Fatalf("settings after Remove:\n%s\nwant:\n%s", got, want)
	}
}

func TestRemoveChangesNothingItWasNotAskedFor(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "settings.json")
	removed, err := Remove(missing, "pisafe-anything")
	if err != nil {
		t.Fatalf("Remove from a file that is not there: %v", err)
	}
	if removed {
		t.Fatal("Remove reported writing a file that is not there")
	}
	path := settingsFile(t, zedSettings)
	if removed, err := Remove(path, "pisafe-a-run-never-opened"); err != nil || removed {
		t.Fatalf("Remove of an unsaved host: removed=%v err=%v", removed, err)
	}
	if got := readSettings(t, path); got != zedSettings {
		t.Fatalf("settings changed by a Remove that matched nothing:\n%s", got)
	}
}

// A run's config file lives under "Application Support" and its alias is a run
// ID, so both go through JSON quoting rather than into the file as they stand.
func TestConnectionsAreQuotedRatherThanPastedIn(t *testing.T) {
	path := settingsFile(t, zedSettings)
	if _, err := Ensure(path, Connection{
		Host:       `pisafe-"odd"`,
		ConfigFile: `/tmp/a"b\c/ssh.config`,
	}); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	content := readSettings(t, path)
	if !strings.Contains(content, `"host": "pisafe-\"odd\""`) ||
		!strings.Contains(content, `"args": ["-F", "/tmp/a\"b\\c/ssh.config"]`) {
		t.Fatalf("settings hold an unquoted connection:\n%s", content)
	}
	if removed, err := Remove(path, `pisafe-"odd"`); err != nil || !removed {
		t.Fatalf("Remove of a quoted host: removed=%v err=%v", removed, err)
	}
	if got := readSettings(t, path); got != zedSettings {
		t.Fatalf("settings after Remove:\n%s\nwant:\n%s", got, zedSettings)
	}
}

func TestSettingsThatCannotBeReadAreNotRewritten(t *testing.T) {
	for name, content := range map[string]string{
		"unterminated object": "{\n  \"ssh_connections\": [\n",
		"unterminated string": "{\n  \"ssh_connections: []\n}\n",
		"no member name":      "{\n  ssh_connections: []\n}\n",
	} {
		path := settingsFile(t, content)
		if _, err := Ensure(path, runConnection()); err == nil {
			t.Fatalf("%s: Ensure accepted settings it cannot read", name)
		}
		if got := readSettings(t, path); got != content {
			t.Fatalf("%s: settings were rewritten:\n%s", name, got)
		}
	}
}
