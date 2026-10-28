package tmux

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLOC4_RespawnPaneLogRedactsEnvValues pins LOC-4 for the tmux layer: the
// respawn_pane_executing debug line records the full respawn argv, which
// carries the session's env prelude. It must keep the variable name and hide
// the value.
func TestLOC4_RespawnPaneLogRedactsEnvValues(t *testing.T) {
	binDir := t.TempDir()
	fakeTmux := filepath.Join(binDir, "tmux")
	// Every tmux call (the conditional respawn, the new tree's list-panes, …)
	// succeeds instantly, so the test needs no tmux server.
	if err := os.WriteFile(fakeTmux, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// The fork's respawn first captures the pane's stable identity and its
	// processes, which a tmux that answers every call with nothing cannot
	// supply. The captured target stands in for it; no old process means no
	// escalation runs after the respawn.
	oldCapture := captureStableSessionProcessTreeFn
	captureStableSessionProcessTreeFn = func(s *Session) (stableSessionTarget, []ProcessIdentity, error) {
		return stableSessionTargetForTest(s.Name, s.SocketName), nil, nil
	}
	t.Cleanup(func() { captureStableSessionProcessTreeFn = oldCapture })

	buf := &bytes.Buffer{}
	orig := mcpLog
	mcpLog = slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Cleanup(func() { mcpLog = orig })

	const secret = "loc4-respawn-SECRET-value"
	s := NewSession("loc4-respawn-redact", t.TempDir())
	if err := s.RespawnPane("export LOC4_API_KEY='" + secret + "' && exec sleep 300"); err != nil {
		t.Fatalf("RespawnPane: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "respawn_pane_executing") {
		t.Fatalf("respawn_pane_executing line not logged:\n%s", out)
	}
	if strings.Contains(out, secret) {
		t.Errorf("respawn log leaked the env value:\n%s", out)
	}
	if !strings.Contains(out, "LOC4_API_KEY=***") {
		t.Errorf("respawn log should keep the env name as LOC4_API_KEY=***:\n%s", out)
	}
}
