//go:build !windows

package tmux

import (
	"context"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func newUnresponsiveTmuxSession(t *testing.T, tag string) (*Session, *os.Process) {
	t.Helper()
	s := newSharedViewSession(t, tag)
	out, err := s.runBoundedOutput("display-message", "-p", "#{pid}")
	require.NoError(t, err)
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	require.NoError(t, err)
	require.Positive(t, pid)
	server, err := os.FindProcess(pid)
	require.NoError(t, err)
	// Resume our private server before the session helper's cleanup runs.
	t.Cleanup(func() {
		require.NoError(t, server.Signal(syscall.SIGCONT))
		_ = server.Release()
	})
	require.NoError(t, server.Signal(syscall.SIGSTOP))
	return s, server
}

func TestListViewers_UnresponsiveServer(t *testing.T) {
	s, _ := newUnresponsiveTmuxSession(t, "unresponsive-viewers")

	for _, query := range []struct {
		name string
		run  func(context.Context) error
	}{
		{"session", func(ctx context.Context) error {
			_, err := ListViewers(ctx, s.SocketName, s.Name)
			return err
		}},
		{"all", func(ctx context.Context) error {
			_, err := ListAllViewers(ctx, s.SocketName)
			return err
		}},
	} {
		t.Run(query.name, func(t *testing.T) {
			t.Parallel()
			for _, caller := range []string{"no deadline", "earlier deadline", "canceled"} {
				t.Run(caller, func(t *testing.T) {
					ctx, cancel := context.WithCancel(context.Background())
					if caller == "earlier deadline" {
						cancel()
						ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
					} else if caller == "canceled" {
						cancel()
					}
					defer cancel()
					// Let a regression fail rather than leave a blocked tmux client behind.
					guard := time.AfterFunc(2*tmuxPollTimeout, cancel)
					defer guard.Stop()

					start := time.Now()
					err := query.run(ctx)
					require.Error(t, err, "an unresponsive server is unknown, not nobody")
					require.False(t, viewersListingIsEmpty(err))
					if caller == "no deadline" {
						require.NoError(t, ctx.Err(), "query must time out before the caller's safety guard")
					} else {
						require.Error(t, ctx.Err())
						require.Less(t, time.Since(start), tmuxPollTimeout, "respect caller cancellation")
					}
				})
			}
		})
	}
}

func TestStatusMouseBindings_UnresponsiveServer(t *testing.T) {
	s, server := newUnresponsiveTmuxSession(t, "unresponsive-mouse")
	original := DefaultSocketName()
	SetDefaultSocketName(s.SocketName)
	t.Cleanup(func() { SetDefaultSocketName(original) })

	for _, binding := range []struct {
		name string
		run  func() error
	}{
		{"bind", BindMouseStatusRightDetach},
		{"unbind", UnbindMouseStatusClicks},
	} {
		t.Run(binding.name, func(t *testing.T) {
			// Release the server if an unbounded command regresses, so the test can fail.
			guard := time.AfterFunc(2*tmuxMutationTimeout, func() { _ = server.Signal(syscall.SIGCONT) })
			defer guard.Stop()
			err := binding.run()
			require.True(t, guard.Stop(), "binding must finish without the server's cooperation")
			// Both report the failure, so the TUI can log it (agent-deck-j70).
			require.ErrorIs(t, err, errTmuxTimeout)
		})
	}
}
