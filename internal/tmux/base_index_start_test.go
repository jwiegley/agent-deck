package tmux

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// baseIndexOneServer returns an isolated server whose new windows and panes
// are numbered from 1, as `set -g base-index 1` and `setw -g pane-base-index
// 1` in a user's ~/.tmux.conf leave every server agent-deck starts (the spawn
// argv never passes -f).
func baseIndexOneServer(t *testing.T) (socket string, ctl func(args ...string) string) {
	t.Helper()
	requireTmux(t)
	socket, _ = makeIsolatedServer(t)
	ctl = tmuxCtl(t, socket)
	ctl("set-option", "-g", "base-index", "1")
	ctl("set-option", "-g", "pane-base-index", "1")
	return socket, ctl
}

// TestStartRetainsRemainOnExitSessionUnderBaseIndexOne drives the full Start
// for a session carrying a remain-on-exit override (sandboxed sessions,
// one-shots, [tmux] options). The override rides in the new-session command
// queue; aimed at window 0 it made tmux reject the whole call on this server,
// so Start returned "failed to create tmux session" and the one-shot's output
// was torn down with its pane. The retained pane must then read back as dead
// with its exit status through the direct (uncached) probes.
func TestStartRetainsRemainOnExitSessionUnderBaseIndexOne(t *testing.T) {
	socket, ctl := baseIndexOneServer(t)

	s := NewSession("base-index-one-shot", t.TempDir())
	s.SocketName = socket
	s.RunCommandAsInitialProcess = true
	s.OptionOverrides = map[string]string{"remain-on-exit": "on"}
	require.NoError(t, s.Start("printf 'one-shot answer\\n'; exit 7"))
	t.Cleanup(func() { _ = s.Kill() })

	if index := ctl("display-message", "-p", "-t", "="+s.primaryWindowTarget(), "#{window_index}.#{pane_index}"); index != "1.1" {
		t.Fatalf("primary pane index = %q, want 1.1 (server did not apply base-index 1)", index)
	}
	if _, cached := GetCachedPaneInfo(s.Name); cached {
		t.Fatal("pane cache holds this session; the direct probes would not be exercised")
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		if code, dead := s.PaneDeadExitStatus(); dead {
			require.Equal(t, 7, code, "retained exit status")
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("one-shot pane was not retained with its exit status")
		}
		time.Sleep(10 * time.Millisecond)
	}
	require.True(t, s.IsPaneDead(), "retained dead pane must read back as dead")
	require.Contains(t, ctl("capture-pane", "-p", "-S", "-", "-t", "="+s.primaryWindowTarget()), "one-shot answer")
}
