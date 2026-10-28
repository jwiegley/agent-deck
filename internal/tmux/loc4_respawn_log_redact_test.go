package tmux

import (
	"bytes"
	"log/slog"
	"os/exec"
	"strings"
	"testing"
)

// TestLOC4_RespawnPaneLogRedactsEnvValues pins LOC-4 for the tmux layer: the
// respawn_pane_executing debug line records the full respawn argv, which
// carries the session's env prelude. It must keep the variable name and hide
// the value.
func TestLOC4_RespawnPaneLogRedactsEnvValues(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}
	buf := &bytes.Buffer{}
	orig := mcpLog
	mcpLog = slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Cleanup(func() { mcpLog = orig })

	// The fork's respawn first captures the pane's stable identity and its
	// processes, which a fake tmux that answers every call with nothing
	// cannot supply, so it runs against a session on TestMain's isolated
	// tmux server.
	const secret = "loc4-respawn-SECRET-value"
	s := NewSession("loc4-respawn-redact", t.TempDir())
	if err := s.Start("sleep 300"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Kill() })
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
