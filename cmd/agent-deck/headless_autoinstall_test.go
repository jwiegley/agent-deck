package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/update"
)

// TestHeadlessAutoInstall_Gates pins how `web --no-tui` gets its own
// unattended installer: none when the process is test-, CI- or
// script-driven (issue #2251), none for a Homebrew-managed binary, and
// otherwise one that runs `<exe> update --unattended --trigger web` with
// the update settings as its on/off switch.
func TestHeadlessAutoInstall_Gates(t *testing.T) {
	// The package TestMain already isolates HOME; GetUpdateSettings sees
	// the defaults (auto_install on).
	stubHeadlessSuppression(t, "CI=true")
	if inst := newHeadlessAutoInstaller("/bin/agent-deck", func() bool { return false }); inst != nil {
		t.Fatal("a suppressed daemon must not build an installer")
	}

	stubHeadlessSuppression(t, "")
	if inst := newHeadlessAutoInstaller("/bin/agent-deck", func() bool { return true }); inst != nil {
		t.Fatal("a Homebrew-managed binary must not build an installer; brew owns it")
	}
	inst := newHeadlessAutoInstaller("/bin/agent-deck", func() bool { return false })
	if inst == nil {
		t.Fatal("an unsuppressed daemon must get an installer")
	}
	if inst.Exe != "/bin/agent-deck" || inst.Trigger != "web" || inst.RunningVersion != Version {
		t.Fatalf("installer = %+v, want exe, trigger web and the running version", inst)
	}
	if inst.Enabled == nil || !inst.Enabled() {
		t.Fatal("with default settings the installer must be enabled")
	}

	headlessAutoUpdateSuppressed = update.AutoUpdateSuppressed
	if inst := newHeadlessAutoInstaller("/bin/agent-deck", func() bool { return false }); inst != nil {
		t.Fatal("the real predicate must suppress under go test")
	}
}

// writeUpdatesConfig points HOME and XDG at a fresh directory whose
// config.toml holds body under [updates], and returns that home.
func writeUpdatesConfig(t *testing.T, body string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	dir := filepath.Join(home, ".config", "agent-deck")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("[updates]\n"+body), 0o600); err != nil {
		t.Fatal(err)
	}
	session.ClearUserConfigCache()
	t.Cleanup(session.ClearUserConfigCache)
	return home
}

// TestHeadlessAutoInstall_OffWithCheckDisabled pins [updates] check_enabled
// = false for the daemons (`web --no-tui`, notify-daemon): no installer is
// built, so the process never polls GitHub and never runs `update
// --unattended` on its own, even with auto_install at its default of on.
func TestHeadlessAutoInstall_OffWithCheckDisabled(t *testing.T) {
	writeUpdatesConfig(t, "check_enabled = false\n")
	stubHeadlessSuppression(t, "")
	if inst := newHeadlessAutoInstaller("/bin/agent-deck", func() bool { return false }); inst != nil {
		t.Fatal("check_enabled = false must not build an installer")
	}
}

// TestManualUpdate_RunsWithCheckDisabled guards the other side:
// check_enabled governs only the automatic paths, so `agent-deck update`
// still checks when asked. AGENTDECK_SKIP_UPDATE_CHECK keeps that check off
// the network.
func TestManualUpdate_RunsWithCheckDisabled(t *testing.T) {
	home := writeUpdatesConfig(t, "check_enabled = false\n")
	stdout, stderr, code := runAgentDeckEnv(t, home, "", []string{update.SkipUpdateCheckEnv + "=1"}, "update", "--check")
	if code != 0 || !strings.Contains(stdout, "Checking for updates...") || !strings.Contains(stdout, "running the latest version") {
		t.Fatalf("update --check: exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
}
