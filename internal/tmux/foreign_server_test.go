package tmux

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEmptySocketResolvesAwayFromDefault(t *testing.T) {
	for _, c := range []struct {
		name, tmuxEnv, tmpdir string
		want                  bool
	}{
		{"no tmux", "", "", false},
		{"inside default server", "/tmp/tmux-501/default,123,0", "", false},
		{"inside default server, darwin spelling", "/private/tmp/tmux-501/default,123,0", "", false},
		{"inside default server under TMUX_TMPDIR", "/x/y/tmux-501/default,1,0", "/x/y", false},
		{"inside a -L server (the rc incident)", "/private/tmp/tmux-501/uxaudit,5369,0", "", true},
		{"inside a -S server", "/tmp/ad-sock-1/s,77,0", "", true},
		{"default name under another tmpdir", "/other/tmux-501/default,1,0", "", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := map[string]string{"TMUX": c.tmuxEnv, "TMUX_TMPDIR": c.tmpdir}
			if got := emptySocketResolvesAwayFromDefault(func(k string) string { return env[k] }, 501); got != c.want {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}

// A session with its own socket is never answered by the default server.
func TestAbsenceIsForeignServer_ExplicitSocketIsNeverForeign(t *testing.T) {
	s := &Session{Name: "agentdeck_x", SocketName: "work"}
	if s.AbsenceIsForeignServer() {
		t.Fatal("a session with an explicit socket must keep its own verdict")
	}
}

// startServerOnSocket starts a private server at the absolute socket path with
// one idle session and kills it at cleanup.
func startServerOnSocket(t *testing.T, socket, session string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if out, err := tmuxExecContext(ctx, "", "-S", socket, "-f", "/dev/null",
		"new-session", "-d", "-s", session, "sleep 3600").CombinedOutput(); err != nil {
		t.Fatalf("start server on %s: %v: %s", socket, err, out)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tmuxExecContext(ctx, "", "-S", socket, "kill-server").Run()
	})
}

// The fork's socket inventories read a missing socket file as a server with no
// sessions (isEmptyTmuxServerResult), which holds where this process computes
// the server's own path. The guard cannot: it computes the DEFAULT socket from
// its own TMUX_TMPDIR while $TMUX names another server, so a missing default
// socket is no evidence that the default server is gone. It may be healthy at
// a path this process does not compute (a TMUX_TMPDIR mismatch) or behind an
// unlinked socket file. Reading that as "gone everywhere" published error for
// healthy sessions from a nested TUI. Only a default server that answers
// without the session permits a verdict.
func TestAbsenceIsForeignServer_UnlistableDefaultServerFormsNoVerdict(t *testing.T) {
	skipIfNoTmuxBinary(t)
	const name = "agentdeck_foreign_probe"
	for _, c := range []struct {
		name  string
		setup func(t *testing.T, defaultSocket string)
		want  bool
	}{
		{"default socket missing (TMUX_TMPDIR mismatch)", func(*testing.T, string) {}, true},
		{"default socket unlinked, server alive", func(t *testing.T, socket string) {
			startServerOnSocket(t, socket, name)
			if err := os.Rename(socket, socket+".moved"); err != nil {
				t.Fatalf("move socket: %v", err)
			}
			// Runs before the kill-server cleanup above (LIFO), so it can reach the server.
			t.Cleanup(func() { _ = os.Rename(socket+".moved", socket) })
		}, true},
		{"default server has the session", func(t *testing.T, socket string) {
			startServerOnSocket(t, socket, name)
		}, true},
		{"default server answers without the session", func(t *testing.T, socket string) {
			startServerOnSocket(t, socket, "agentdeck_other")
		}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			// Short /tmp root, not t.TempDir(): socket paths must fit sun_path.
			tmpdir, err := os.MkdirTemp("/tmp", "adfs-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(tmpdir) })
			socketDir := filepath.Join(tmpdir, fmt.Sprintf("tmux-%d", os.Getuid()))
			if err := os.MkdirAll(socketDir, 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("TMUX_TMPDIR", tmpdir)
			c.setup(t, filepath.Join(socketDir, defaultTmuxSocketName))
			// This process runs inside another server: $TMUX names a socket
			// other than the default one, so its socket-less probes miss the
			// session.
			t.Setenv("TMUX", filepath.Join(tmpdir, "foreign")+",1,0")
			ResetDefaultServerSessionsForTest(t)

			s := &Session{Name: name}
			if got := s.AbsenceIsForeignServer(); got != c.want {
				t.Fatalf("AbsenceIsForeignServer() = %v, want %v", got, c.want)
			}
		})
	}
}
