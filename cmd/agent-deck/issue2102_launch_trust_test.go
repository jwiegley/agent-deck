package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// Issue #2102: `agent-deck launch` into a directory Claude Code has never
// opened interactively reports success, then fails silently — the pane
// dies on the "do you trust the files in this folder?" prompt, no tmux
// session is created, and no log is written. PreAcceptClaudeTrust already
// solves this for conductor dirs (#1359) and worktree parents (#1149), but
// nothing on the ordinary `launch` path called it. preAcceptLaunchTrust is
// the launch-path caller that closes that gap.
func TestIssue2102_PreAcceptLaunchTrustSeedsUnopenedDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	projectDir := t.TempDir()
	inst := &session.Instance{Tool: "claude", ProjectPath: projectDir}

	preAcceptLaunchTrust(inst)

	claudeJSONPath := filepath.Join(home, ".claude.json")
	data, err := os.ReadFile(claudeJSONPath)
	if err != nil {
		t.Fatalf("expected %s to be written: %v", claudeJSONPath, err)
	}

	var cfg struct {
		Projects map[string]struct {
			HasTrustDialogAccepted bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse %s: %v", claudeJSONPath, err)
	}

	realDir, err := filepath.EvalSymlinks(projectDir)
	if err != nil {
		t.Fatalf("resolve %s: %v", projectDir, err)
	}
	entry, ok := cfg.Projects[realDir]
	if !ok || !entry.HasTrustDialogAccepted {
		t.Fatalf("expected projects[%q].hasTrustDialogAccepted = true, got %+v", realDir, cfg.Projects)
	}
}

// A non-claude tool (shell, codex, ...) has no trust dialog to pre-seed;
// calling this for every tool would just write a useless ~/.claude.json.
func TestIssue2102_PreAcceptLaunchTrustSkipsNonClaudeTool(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	inst := &session.Instance{Tool: "shell", ProjectPath: t.TempDir()}
	preAcceptLaunchTrust(inst)

	if _, err := os.Stat(filepath.Join(home, ".claude.json")); !os.IsNotExist(err) {
		t.Fatalf("expected no ~/.claude.json to be written for a non-claude tool, stat err = %v", err)
	}
}

// A session whose group sets [groups."<g>".claude].config_dir runs Claude
// with CLAUDE_CONFIG_DIR exported, and Claude then keys folder trust in
// <config_dir>/.claude.json; it never consults the root ~/.claude.json.
// Seeding the root file left a never-opened directory untrusted, so such a
// launch still died on the trust dialog exactly as in #2102.
func TestPreAcceptLaunchTrustSeedsResolvedClaudeConfigDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("AGENTDECK_PROFILE", "")
	session.ClearUserConfigCache()
	t.Cleanup(session.ClearUserConfigCache)

	configDir := filepath.Join(home, ".config", "claude", "team")
	agentDeckConfigDir := filepath.Join(home, ".config", "agent-deck")
	if err := os.MkdirAll(agentDeckConfigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf("[groups.\"team\".claude]\nconfig_dir = %q\n", configDir)
	if err := os.WriteFile(filepath.Join(agentDeckConfigDir, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	projectDir := t.TempDir()
	inst := &session.Instance{Tool: "claude", ProjectPath: projectDir, GroupPath: "team"}
	preAcceptLaunchTrust(inst)

	claudeJSONPath := filepath.Join(configDir, ".claude.json")
	data, err := os.ReadFile(claudeJSONPath)
	if err != nil {
		t.Fatalf("expected %s to be written: %v", claudeJSONPath, err)
	}
	var seeded struct {
		Projects map[string]struct {
			HasTrustDialogAccepted bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(data, &seeded); err != nil {
		t.Fatalf("parse %s: %v", claudeJSONPath, err)
	}
	realDir, err := filepath.EvalSymlinks(projectDir)
	if err != nil {
		t.Fatalf("resolve %s: %v", projectDir, err)
	}
	if entry, ok := seeded.Projects[realDir]; !ok || !entry.HasTrustDialogAccepted {
		t.Fatalf("expected projects[%q].hasTrustDialogAccepted = true in %s, got %+v", realDir, claudeJSONPath, seeded.Projects)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude.json")); !os.IsNotExist(err) {
		t.Fatalf("expected no root ~/.claude.json when the session exports CLAUDE_CONFIG_DIR, stat err = %v", err)
	}
}
