package gitstage

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveIdentityPrefersTheRepositoryOverTheGlobalConfiguration(t *testing.T) {
	global := isolatedGitConfiguration(t)
	mustWrite(t, global, "[user]\n\tname = Global Name\n\temail = global@example.invalid\n")
	source := newRepository(t)

	identity, err := ResolveIdentity(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	if identity != (Identity{Name: "Test User", Email: "test@example.invalid"}) {
		t.Fatalf("identity = %#v", identity)
	}
}

func TestResolveIdentityRefusesAnUnconfiguredMac(t *testing.T) {
	isolatedGitConfiguration(t)
	source := t.TempDir()
	runGit(t, source, "init", "-q", "--initial-branch=main")

	if _, err := ResolveIdentity(context.Background(), source); !errors.Is(err, ErrNoIdentity) {
		t.Fatalf("err = %v", err)
	}
}

// A run's whole point is that the agent can commit in it, so the installed
// identity is checked by making a commit exactly as the run would.
func TestInstalledIdentityAuthorsCommitsInsideARun(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, ".gitconfig")
	identity := Identity{Name: "Ada Lovelace", Email: "ada@example.invalid"}
	if err := InstallIdentity(context.Background(), configPath, identity); err != nil {
		t.Fatal(err)
	}

	workspace := t.TempDir()
	runGit(t, workspace, "init", "-q", "--initial-branch=main")
	mustWrite(t, filepath.Join(workspace, "agent.txt"), "agent work\n")
	runGit(t, workspace, "add", "agent.txt")
	commit := exec.Command("git", "-C", workspace, "commit", "-qm", "agent commit")
	commit.Env = append(
		os.Environ(),
		"GIT_CONFIG_GLOBAL="+configPath,
		"GIT_CONFIG_SYSTEM="+os.DevNull,
	)
	if output, err := commit.CombinedOutput(); err != nil {
		t.Fatalf("commit: %v\n%s", err, output)
	}
	if author := runGit(t, workspace, "log", "-1", "--format=%an <%ae>"); author !=
		"Ada Lovelace <ada@example.invalid>" {
		t.Fatalf("author = %q", author)
	}
}

func TestInstallIdentityRefusesUnusableValues(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), ".gitconfig")
	for name, identity := range map[string]Identity{
		"empty name":  {Email: "someone@example.invalid"},
		"empty email": {Name: "Someone"},
		"newline":     {Name: "Someone\n[core]\n\tpager = sh", Email: "someone@example.invalid"},
		"oversize":    {Name: strings.Repeat("n", identityFieldLimit+1), Email: "someone@example.invalid"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := InstallIdentity(context.Background(), configPath, identity); err == nil {
				t.Fatal("unusable identity was installed")
			}
			if _, err := os.Stat(configPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("configuration was written: %v", err)
			}
		})
	}
}

// isolatedGitConfiguration keeps these tests independent of whoever runs them,
// and returns the global configuration path they control.
func isolatedGitConfiguration(t *testing.T) string {
	t.Helper()
	global := filepath.Join(t.TempDir(), "gitconfig")
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	return global
}
