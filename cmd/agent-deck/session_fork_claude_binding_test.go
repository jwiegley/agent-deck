package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/asheshgoplani/agent-deck/internal/testutil"
)

// TestSessionFork_ClaudeRuntimeBindingForksAcrossProcesses mirrors the
// readiness e2e repro with a fake claude: a started Claude session must report
// can_fork and fork from a fresh CLI process. Its spawn binding is stored with
// binding_detected_at = 0 (the shape existing rows already have), and the
// runtime is aged past CanFork's five-minute window, so only the live runtime's
// authoritative binding can make it forkable.
func TestSessionFork_ClaudeRuntimeBindingForksAcrossProcesses(t *testing.T) {
	home := t.TempDir()
	project := filepath.Join(home, "project")
	fakeBin := filepath.Join(home, "bin")
	for _, dir := range []string{project, fakeBin} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(fakeBin, "claude"), []byte("#!/bin/sh\nexec sleep 600\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	tmuxDir := shortTempDir(t, "adcf")
	env := []string{
		"TMUX_TMPDIR=" + tmuxDir,
		"PATH=" + fakeBin + string(os.PathListSeparator) + os.Getenv("PATH"),
	}
	run := func(args ...string) string {
		t.Helper()
		stdout, stderr, code := runAgentDeckEnv(t, home, "", env, args...)
		if code != 0 {
			t.Fatalf("%v: exit %d\nstdout: %s\nstderr: %s", args, code, stdout, stderr)
		}
		return stdout
	}
	t.Cleanup(func() {
		for _, title := range []string{"cf-parent", "cf-child"} {
			runAgentDeckEnv(t, home, "", env, "session", "stop", title)
		}
		testutil.KillTmuxServersUnder(tmuxDir)
	})

	var added struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(run("add", "-t", "cf-parent", "-c", "claude", "--no-parent", "--json", project)), &added); err != nil {
		t.Fatal(err)
	}
	run("session", "start", "cf-parent")

	db, err := statedb.Open(stateDBPath(t, home))
	if err != nil {
		t.Fatal(err)
	}
	binding, found, err := db.ReadRuntimeBinding(added.ID, "claude")
	if err != nil || !found || binding.Value == "" {
		_ = db.Close()
		t.Fatalf("claude binding after start: %+v found=%v err=%v", binding, found, err)
	}
	aged := time.Now().Add(-10 * time.Minute).UTC().UnixNano()
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE instance_runtime_state SET last_started_at = ? WHERE instance_id = ?`, []any{aged, added.ID}},
		{`UPDATE instance_runtime_binding SET binding_detected_at = 0 WHERE instance_id = ? AND binding_kind = 'claude'`, []any{added.ID}},
	} {
		if _, err := db.DB().Exec(stmt.sql, stmt.args...); err != nil {
			_ = db.Close()
			t.Fatalf("%s: %v", stmt.sql, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	var shown struct {
		Status          string `json:"status"`
		CanFork         *bool  `json:"can_fork"`
		ClaudeSessionID string `json:"claude_session_id"`
	}
	if err := json.Unmarshal([]byte(run("session", "show", "cf-parent", "--json")), &shown); err != nil {
		t.Fatal(err)
	}
	if shown.ClaudeSessionID != binding.Value {
		t.Fatalf("session show claude_session_id = %q, want the runtime binding %q", shown.ClaudeSessionID, binding.Value)
	}
	if shown.CanFork == nil || !*shown.CanFork {
		t.Fatalf("session show reports can_fork=%v (present=%v) for a live Claude session (status %q) with a current runtime binding",
			shown.CanFork != nil && *shown.CanFork, shown.CanFork != nil, shown.Status)
	}

	run("session", "fork", "cf-parent", "-t", "cf-child")
}
