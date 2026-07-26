package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRefreshCodexOwnershipSnapshot_FiltersGlobalFallbackAndKeepsSessionClaims(t *testing.T) {
	const (
		globalID = "11111111-1111-4111-8111-111111111111"
		sharedID = "22222222-2222-4222-8222-222222222222"
	)

	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "tmux.log")
	fakeTmux := filepath.Join(binDir, "tmux")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$TMUX_TEST_LOG"
case " $* " in
  *" list-sessions "*)
    printf '%s\t%s\n' \
      'agentdeck_global_fallback' "$GLOBAL_CODEX_ID" \
      'agentdeck_claim_a' "$SHARED_CODEX_ID" \
      'agentdeck_claim_b' "$SHARED_CODEX_ID"
    ;;
  *" show-environment -g CODEX_SESSION_ID "*)
    printf 'CODEX_SESSION_ID=%s\n' "$GLOBAL_CODEX_ID"
    ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(fakeTmux, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("TMUX_TEST_LOG", logPath)
	t.Setenv("GLOBAL_CODEX_ID", globalID)
	t.Setenv("SHARED_CODEX_ID", sharedID)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	snapshot := refreshCodexOwnershipSnapshot("global-fallback-test", nil)
	if !snapshot.complete {
		t.Fatal("ownership snapshot unexpectedly incomplete")
	}
	if got := snapshot.idsByTmux["agentdeck_global_fallback"]; got != "" {
		t.Fatalf("global CODEX_SESSION_ID fallback entered ownership snapshot: %q", got)
	}
	for _, name := range []string{"agentdeck_claim_a", "agentdeck_claim_b"} {
		if got := snapshot.idsByTmux[name]; got != sharedID {
			t.Fatalf("genuine session-scoped claim %s = %q, want %q", name, got, sharedID)
		}
	}
	if got := snapshot.ownedIDs[globalID]; got != 0 {
		t.Fatalf("global fallback owner count = %d, want 0", got)
	}
	if got := snapshot.ownedIDs[sharedID]; got != 2 {
		t.Fatalf("duplicate session-scoped owner count = %d, want 2", got)
	}
	if exclusions := codexOwnershipExclusions(snapshot, "agentdeck_claim_a"); !exclusions.contains(sharedID) {
		t.Fatal("duplicate claim became available to one of its existing owners")
	}

	// A fresh session must still be able to bind a rollout whose ID happens to
	// equal the tmux server's inherited global fallback.
	codexHome := t.TempDir()
	projectPath := filepath.Join(codexHome, "project")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	t.Setenv("CODEX_HOME", codexHome)
	writeCodexSessionFile(t, codexHome, globalID, projectPath)
	fresh := &Instance{ProjectPath: projectPath, Command: "codex", Tool: "codex"}
	if got := fresh.queryCodexSession(codexOwnershipExclusions(snapshot, "agentdeck_fresh"), false); got != globalID {
		t.Fatalf("fresh session bound rollout %q, want globally leaked ID %q", got, globalID)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read fake tmux log: %v", err)
	}
	var listCalls, globalReads, sessionReads int
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.Contains(line, "list-sessions") {
			listCalls++
		}
		if strings.Contains(line, "show-environment -g CODEX_SESSION_ID") {
			globalReads++
		}
		if strings.Contains(line, "show-environment -t") {
			sessionReads++
		}
	}
	if listCalls != 1 || globalReads != 1 || sessionReads != 0 {
		t.Fatalf("ownership refresh calls: list=%d global=%d per-session=%d; want 1, 1, 0; log=%q",
			listCalls, globalReads, sessionReads, strings.TrimSpace(string(data)))
	}
}

func TestRefreshCodexOwnershipSnapshot_KeepsClaimsWhenGlobalIDIsUnset(t *testing.T) {
	const sessionID = "55555555-5555-4555-8555-555555555555"

	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "tmux.log")
	fakeTmux := filepath.Join(binDir, "tmux")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$TMUX_TEST_LOG"
case " $* " in
  *" list-sessions "*) printf '%s\t%s\n' 'agentdeck_claim' "$SESSION_CODEX_ID" ;;
  *" show-environment -g CODEX_SESSION_ID "*)
    printf '%s\n' 'unknown variable: CODEX_SESSION_ID' >&2
    exit 1
    ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(fakeTmux, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("TMUX_TEST_LOG", logPath)
	t.Setenv("SESSION_CODEX_ID", sessionID)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	snapshot := refreshCodexOwnershipSnapshot("global-unset-test", nil)
	if !snapshot.complete {
		t.Fatal("unset global CODEX_SESSION_ID made ownership snapshot incomplete")
	}
	if got := snapshot.idsByTmux["agentdeck_claim"]; got != sessionID {
		t.Fatalf("session-scoped claim = %q, want %q", got, sessionID)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read fake tmux log: %v", err)
	}
	if got := strings.Count(string(data), "show-environment -g CODEX_SESSION_ID"); got != 1 {
		t.Fatalf("global environment reads = %d, want 1; log=%q", got, strings.TrimSpace(string(data)))
	}
}

func TestRefreshCodexOwnershipSnapshot_GlobalProbeSharesRefreshDeadline(t *testing.T) {
	binDir := t.TempDir()
	fakeTmux := filepath.Join(binDir, "tmux")
	script := `#!/bin/sh
case " $* " in
  *" list-sessions "*) printf '%s\t%s\n' 'agentdeck_claim' '33333333-3333-4333-8333-333333333333' ;;
  *" show-environment -g CODEX_SESSION_ID "*) sleep 2 ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(fakeTmux, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	stale := &codexOwnershipSnapshotData{
		idsByTmux: map[string]string{"agentdeck_stale": "44444444-4444-4444-8444-444444444444"},
		ownedIDs:  map[string]int{"44444444-4444-4444-8444-444444444444": 1},
		complete:  true,
	}
	started := time.Now()
	snapshot := refreshCodexOwnershipSnapshot("global-timeout-test", stale)
	elapsed := time.Since(started)

	if snapshot.complete {
		t.Fatal("timed-out global environment probe published a complete snapshot")
	}
	if got := snapshot.idsByTmux["agentdeck_stale"]; got == "" {
		t.Fatal("timed-out global environment probe discarded the prior snapshot")
	}
	if elapsed >= 1500*time.Millisecond {
		t.Fatalf("global environment probe exceeded bounded refresh deadline: %s", elapsed)
	}
}
