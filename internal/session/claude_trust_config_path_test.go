package session

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// isolateClaudeConfigEnv points every config lookup at a fresh home so the
// resolver sees only what the test writes.
func isolateClaudeConfigEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("AGENTDECK_PROFILE", "")
	ClearUserConfigCache()
	t.Cleanup(ClearUserConfigCache)
	return home
}

func writeAgentDeckConfig(t *testing.T, home, contents string) {
	t.Helper()
	dir := filepath.Join(home, ".config", "agent-deck")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	ClearUserConfigCache()
}

func TestClaudeTrustConfigPathForInstance_DefaultUsesRootClaudeJSON(t *testing.T) {
	home := isolateClaudeConfigEnv(t)

	got := ClaudeTrustConfigPathForInstance(&Instance{Tool: "claude", GroupPath: "team"})
	if want := filepath.Join(home, ".claude.json"); got != want {
		t.Fatalf("ClaudeTrustConfigPathForInstance() = %q, want %q", got, want)
	}
	if got := ClaudeTrustConfigPathForInstance(nil); got != filepath.Join(home, ".claude.json") {
		t.Fatalf("ClaudeTrustConfigPathForInstance(nil) = %q, want the root ~/.claude.json", got)
	}
}

func TestClaudeTrustConfigPathForInstance_GroupConfigDir(t *testing.T) {
	home := isolateClaudeConfigEnv(t)
	configDir := filepath.Join(home, ".config", "claude", "team")
	writeAgentDeckConfig(t, home, fmt.Sprintf("[groups.\"team\".claude]\nconfig_dir = %q\n", configDir))

	got := ClaudeTrustConfigPathForInstance(&Instance{Tool: "claude", GroupPath: "team"})
	if want := filepath.Join(configDir, ".claude.json"); got != want {
		t.Fatalf("ClaudeTrustConfigPathForInstance() = %q, want %q", got, want)
	}
	other := ClaudeTrustConfigPathForInstance(&Instance{Tool: "claude", GroupPath: "elsewhere"})
	if want := filepath.Join(home, ".claude.json"); other != want {
		t.Fatalf("instance outside the configured group = %q, want %q", other, want)
	}
}

func TestClaudeTrustConfigPathForInstance_EnvConfigDir(t *testing.T) {
	home := isolateClaudeConfigEnv(t)
	envDir := filepath.Join(home, "claude-env")
	t.Setenv("CLAUDE_CONFIG_DIR", envDir)
	ClearUserConfigCache()

	got := ClaudeTrustConfigPathForInstance(&Instance{Tool: "claude"})
	if want := filepath.Join(envDir, ".claude.json"); got != want {
		t.Fatalf("ClaudeTrustConfigPathForInstance() = %q, want %q", got, want)
	}
}
