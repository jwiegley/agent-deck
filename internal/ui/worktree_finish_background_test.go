package ui

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"
)

// A confirmed worktree finish cannot be cancelled, and under runtime authority
// its command also prunes hook artifacts, so its progress dialog may wait on
// slow IO (TestHookCleanupDeletionKeepsUIResponsive covers that case). Esc
// dismisses the progress into the background. The single dialog still carries
// that in-flight finish: it must not be reopened over it, and the finish's
// result, a failure included, must report through the status line and release
// it for the next finish.
func TestWorktreeFinishDismissedIntoBackgroundHoldsTheDialog(t *testing.T) {
	h, running, next, pressFinish := newWorktreeFinishFixture(t)
	dismissFinishIntoBackground(t, h, running, pressFinish)

	// Reopening the dialog would reset the running finish's state, and its
	// result would then fail or hide the new dialog.
	require.Nil(t, pressFinish(next), "no second finish while one runs")
	require.False(t, h.worktreeFinishDialog.IsVisible())
	require.Equal(t, running.ID, h.worktreeFinishDialog.GetSessionID())
	require.EqualError(t, h.err, "still finishing worktree 'running'")

	_, cmd := h.Update(worktreeFinishResultMsg{
		sessionID: running.ID, sessionTitle: running.Title, err: errors.New("session changed before finish"),
	})
	require.Nil(t, cmd)
	require.EqualError(t, h.err, "session changed before finish", "a dismissed finish reports its failure in the status line")
	require.False(t, h.worktreeFinishDialog.IsVisible())
	require.False(t, h.worktreeFinishDialog.IsExecuting(), "the reported failure must release the dialog")
	require.Same(t, running, h.getInstanceByID(running.ID), "a failed finish keeps its session")

	require.NotNil(t, pressFinish(next), "the next finish opens once the previous one reported")
	require.True(t, h.worktreeFinishDialog.IsVisible())
	require.False(t, h.worktreeFinishDialog.IsExecuting())
	require.Equal(t, next.ID, h.worktreeFinishDialog.GetSessionID())
}

// Once Esc has dismissed a running finish, q, ctrl+c and double-Esc all reach
// tryQuit. Quitting ends the finish command partway: the merge may be done
// while the session is still listed, or the session deleted while its worktree
// and branch are left behind. So tryQuit must ask first. It must not refuse
// outright either, or a hung finish would make the TUI impossible to quit.
func TestQuitAsksBeforeAbandoningABackgroundWorktreeFinish(t *testing.T) {
	h, running, _, pressFinish := newWorktreeFinishFixture(t)
	dismissFinishIntoBackground(t, h, running, pressFinish)
	esc := tea.KeyMsg{Type: tea.KeyEsc}

	for _, tc := range []struct {
		name   string
		keys   []tea.KeyMsg
		cancel tea.KeyMsg
	}{
		{"q", []tea.KeyMsg{{Type: tea.KeyRunes, Runes: []rune{'q'}}}, esc},
		{"ctrl+c", []tea.KeyMsg{{Type: tea.KeyCtrlC}}, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}}},
		{"double Esc", []tea.KeyMsg{esc, esc}, tea.KeyMsg{Type: tea.KeyEnter}}, // Enter on the default Cancel
	} {
		var cmd tea.Cmd
		for _, key := range tc.keys {
			_, cmd = h.Update(key)
		}
		require.Nil(t, cmd, "%s must not quit while a finish runs", tc.name)
		require.False(t, h.isQuitting, "%s must not quit while a finish runs", tc.name)
		require.True(t, h.confirmDialog.IsVisible(), "%s must ask before abandoning the finish", tc.name)
		require.Equal(t, ConfirmQuitWithWorktreeFinish, h.confirmDialog.GetConfirmType())
		require.Contains(t, stripAnsi(h.View()), `Still finishing worktree:`)
		require.Contains(t, stripAnsi(h.View()), `"running"`)

		_, cmd = h.Update(tc.cancel)
		require.Nil(t, cmd, "cancelling the quit (%s)", tc.name)
		require.False(t, h.confirmDialog.IsVisible(), "cancelling the quit (%s) must close the confirmation", tc.name)
		require.False(t, h.isQuitting)
		require.True(t, h.worktreeFinishDialog.IsExecuting(), "cancelling the quit (%s) must leave the finish running", tc.name)
	}

	// Confirming, with y or with Enter on "Quit anyway", goes on to the
	// ordinary quit, including its MCP-pool prompt when a pool is running.
	for _, confirm := range []struct {
		name string
		keys []tea.KeyMsg
	}{
		{"y", []tea.KeyMsg{{Type: tea.KeyRunes, Runes: []rune{'y'}}}},
		{"Enter on Quit anyway", []tea.KeyMsg{{Type: tea.KeyLeft}, {Type: tea.KeyEnter}}},
	} {
		for _, poolRunning := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, MCP pool running %v", confirm.name, poolRunning), func(t *testing.T) {
				h, running, _, pressFinish := newWorktreeFinishFixture(t)
				if poolRunning {
					runMCPPool(t, 2)
				}
				dismissFinishIntoBackground(t, h, running, pressFinish)
				h.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
				require.Equal(t, ConfirmQuitWithWorktreeFinish, h.confirmDialog.GetConfirmType())
				var cmd tea.Cmd
				for _, key := range confirm.keys {
					_, cmd = h.Update(key)
				}
				want := quitMsg(true) // no pool to keep: the default clean exit
				if poolRunning {
					require.Nil(t, cmd, "the pool prompt comes before the quit")
					require.False(t, h.isQuitting, "the pool prompt comes before the quit")
					require.True(t, h.confirmDialog.IsVisible(), "confirming must go on to the MCP-pool prompt")
					require.Equal(t, ConfirmQuitWithPool, h.confirmDialog.GetConfirmType())
					require.Contains(t, stripAnsi(h.View()), "2 MCP servers are running in the pool.")
					_, cmd = h.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
					want = quitMsg(false) // keep the pool running
				}
				require.False(t, h.confirmDialog.IsVisible())
				require.True(t, h.isQuitting, "confirming must quit even though the finish still runs")
				require.True(t, h.worktreeFinishDialog.IsExecuting())
				require.NotNil(t, cmd)
				require.Equal(t, want, cmd(), "confirming must schedule the ordinary quit")
			})
		}
	}
}

// runMCPPool enables [mcp_pool] in the fixture's config and has the pool
// report running MCP servers, without starting any.
func runMCPPool(t *testing.T, running int) {
	t.Helper()
	session.ClearUserConfigCache()
	t.Cleanup(session.ClearUserConfigCache)
	require.NoError(t, session.SaveUserConfig(&session.UserConfig{MCPPool: session.MCPPoolSettings{Enabled: true}}))
	session.ClearUserConfigCache()
	prev := mcpPoolRunningCount
	mcpPoolRunningCount = func() int { return running }
	t.Cleanup(func() { mcpPoolRunningCount = prev })
}

// The finish can report while its quit confirmation is open. The confirmation
// must then stop claiming the finish is running, and it must stay open: the
// user asked to quit and has not answered yet. Closing it would drop that
// request, and quitting for them would drop a cancel already on its way and
// hide a failure. So the question stays with its buttons, keys and focus
// unchanged, and it now says how the finish ended.
func TestQuitConfirmationReportsTheFinishThatEndedUnderIt(t *testing.T) {
	left := tea.KeyMsg{Type: tea.KeyLeft}
	enter := tea.KeyMsg{Type: tea.KeyEnter}
	for _, tc := range []struct {
		name string
		// focus is pressed before the finish reports; Enter answers after.
		focus    []tea.KeyMsg
		err      error
		title    string
		body     []string
		wantQuit bool
	}{
		{
			name:  "completed, Enter keeps meaning Cancel",
			title: "Worktree Finish Done",
			body:  []string{"Finished worktree:", `"running"`, "The finish has completed"},
		},
		{
			name:     "failed, Enter keeps meaning Quit",
			focus:    []tea.KeyMsg{left},
			err:      errors.New("merge into main failed"),
			title:    "Worktree Finish Failed",
			body:     []string{"Could not finish worktree:", `"running"`, "merge into main failed", "The finish has stopped"},
			wantQuit: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, running, _, pressFinish := newWorktreeFinishFixture(t)
			dismissFinishIntoBackground(t, h, running, pressFinish)
			h.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
			for _, key := range tc.focus {
				h.Update(key)
			}
			focused := h.confirmDialog.GetFocusedButton()

			_, cmd := h.Update(worktreeFinishResultMsg{sessionID: running.ID, sessionTitle: running.Title, err: tc.err})
			require.Nil(t, cmd, "the finish's result must not answer the quit question")
			require.False(t, h.isQuitting, "the finish's result must not answer the quit question")
			require.False(t, h.worktreeFinishDialog.IsExecuting())
			require.True(t, h.confirmDialog.IsVisible(), "the user's quit request must stay open")
			require.Equal(t, ConfirmQuitWithWorktreeFinish, h.confirmDialog.GetConfirmType())
			require.Equal(t, focused, h.confirmDialog.GetFocusedButton(), "the focused button must keep its meaning")
			view := stripAnsi(h.View())
			require.NotContains(t, view, "Still finishing worktree", "the confirmation must not claim a finished finish still runs")
			require.NotContains(t, view, "Quit anyway")
			require.Contains(t, view, tc.title)
			for _, want := range tc.body {
				require.Contains(t, view, want)
			}

			_, cmd = h.Update(enter)
			require.False(t, h.confirmDialog.IsVisible())
			require.Equal(t, tc.wantQuit, h.isQuitting)
			if !tc.wantQuit {
				require.Nil(t, cmd)
				require.EqualError(t, h.err, "Finished worktree 'running'", "the status line keeps the finish's outcome")
				// Nothing runs any more, so q quits without asking.
				_, cmd = h.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
				require.False(t, h.confirmDialog.IsVisible())
				require.True(t, h.isQuitting)
			}
			require.NotNil(t, cmd)
			_, ok := cmd().(quitMsg)
			require.True(t, ok, "quitting must schedule the ordinary quit")
		})
	}
}

// A finish's result updates only the confirmation opened over that finish,
// and a later confirmation, over the next finish, starts out running again.
func TestQuitConfirmationTracksOnlyItsOwnFinish(t *testing.T) {
	c := NewConfirmDialog()
	c.ShowQuitWithWorktreeFinish("a", "a")
	c.NoteWorktreeFinishEnded("b", nil)
	require.Contains(t, stripAnsi(c.View()), "Still finishing worktree:", "another session's finish must not end this one")
	c.NoteWorktreeFinishEnded("a", errors.New("boom"))
	require.Contains(t, stripAnsi(c.View()), "Worktree Finish Failed")

	c.Hide()
	c.ShowQuitWithWorktreeFinish("b", "b")
	require.Contains(t, stripAnsi(c.View()), "Still finishing worktree:", "a new confirmation must not inherit the last finish's outcome")
	c.ShowQuitWithPool(2)
	c.NoteWorktreeFinishEnded("b", nil)
	require.Equal(t, ConfirmQuitWithPool, c.GetConfirmType())
	require.Contains(t, stripAnsi(c.View()), "MCP Pool Running", "any other dialog is left alone")
}

// The restart_deck key (ctrl+t) and auto_restart (on by default) end in the
// same quit as q, followed by a re-exec, so they would stop a background
// finish partway just the same. Neither can ask first, since auto_restart has
// no one to ask, so both wait the way restart already waits on a create,
// resume, fork or setup: ctrl+t is refused with the in-flight footer, and the
// auto path stays quiet until the finish reports.
func TestRestartWaitsForABackgroundWorktreeFinish(t *testing.T) {
	const reason = "a session action is still running, try again in a moment"
	for _, tc := range []struct {
		name    string
		restart func(h *Home) tea.Cmd
		// waited checks how the refused restart told the user it is waiting.
		waited func(t *testing.T, h *Home)
	}{
		{
			name: "ctrl+t",
			restart: func(h *Home) tea.Cmd {
				_, cmd := h.Update(tea.KeyMsg{Type: tea.KeyCtrlT})
				return cmd
			},
			waited: func(t *testing.T, h *Home) {
				require.ErrorIs(t, h.err, errRestartBlocked)
				require.Contains(t, h.err.Error(), reason)
			},
		},
		{
			name:    "auto_restart",
			restart: (*Home).maybeAutoRestart,
			waited: func(t *testing.T, h *Home) {
				require.Equal(t, reason, h.restartWaitReason)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, running, _, pressFinish := newWorktreeFinishFixture(t)
			armRestartIntoNewerBuild(t, h)
			dismissFinishIntoBackground(t, h, running, pressFinish)

			require.Nil(t, tc.restart(h), "%s must not start the quit sequence while a finish runs", tc.name)
			require.False(t, h.restartRequested, "%s must not arm a restart while a finish runs", tc.name)
			require.False(t, h.isQuitting, "%s must not quit while a finish runs", tc.name)
			_, armed := h.RestartTarget()
			require.False(t, armed, "main() must not re-exec over a running finish")
			tc.waited(t, h)
			require.True(t, h.worktreeFinishDialog.IsExecuting())

			// Once the finish reports, nothing else holds the restart back.
			h.Update(worktreeFinishResultMsg{sessionID: running.ID, sessionTitle: running.Title})
			require.False(t, h.worktreeFinishDialog.IsExecuting())
			require.NotNil(t, tc.restart(h), "%s proceeds once the finish has reported", tc.name)
			require.True(t, h.restartRequested)
			require.True(t, h.isQuitting)
		})
	}
}

// armRestartIntoNewerBuild puts a newer build "on disk" and stubs the restart
// target's pre-arm checks as newAutoRestartTestHome does, with auto_restart at
// its default (on), so only h's own state decides whether ctrl+t or
// auto_restart may re-exec.
func armRestartIntoNewerBuild(t *testing.T, h *Home) {
	t.Helper()
	stubUpdateSettings(t, session.UpdateSettings{})
	stubRestartTarget(t, nil, nil)
	stubStatBinary(t, fpAt(1, 1), nil)
	prevOrphan := orphanCheck
	orphanCheck = func(string) string { return "" }
	t.Cleanup(func() { orphanCheck = prevOrphan })
	h.binaryWatch = newBinaryWatch("/bin/agent-deck", "1.16.0", fpAt(1, 1))
	h.binaryWatch.observe(fpAt(2, 2))
	h.binaryWatch.recordProbe(fpAt(2, 2), "1.16.1", nil)
	require.Equal(t, "1.16.1", h.installedUpdateVersion())
	require.True(t, h.autoRestartEnabled())
}

// newWorktreeFinishFixture returns a loaded Home listing two stopped worktree
// sessions, "running" and "next", and a func that presses W on one of them.
func newWorktreeFinishFixture(t *testing.T) (h *Home, running, next *session.Instance, pressFinish func(*session.Instance) tea.Cmd) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	// W probes the repository's default branch; keep git from finding one
	// above the fixture's root.
	root := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(root))
	detachGlobalStateDB(t) // NewHome registers its own, soon-closed database.
	h = NewHome()
	require.NotNil(t, h.storage)
	t.Cleanup(func() { _ = h.storage.Close() })
	h.width, h.height = 100, 30
	h.initialLoading = false // Fixture represents an already loaded session list.
	worktree := func(id string) *session.Instance {
		return &session.Instance{
			ID: id, Title: id, Tool: "shell", Status: session.StatusStopped, CreatedAt: time.Now(),
			WorktreePath: filepath.Join(root, id), WorktreeRepoRoot: root, WorktreeBranch: id + "-branch",
		}
	}
	running, next = worktree("running"), worktree("next")
	h.instances = []*session.Instance{running, next}
	h.instanceByID = map[string]*session.Instance{running.ID: running, next.ID: next}
	h.groupTree = session.NewGroupTree(h.instances)
	h.rebuildFlatItems()
	pressFinish = func(inst *session.Instance) tea.Cmd {
		t.Helper()
		h.cursor = -1
		for i, item := range h.flatItems {
			if item.Session == inst {
				h.cursor = i
			}
		}
		require.GreaterOrEqual(t, h.cursor, 0, "%s is not listed", inst.ID)
		_, cmd := h.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'W'}})
		return cmd
	}
	return h, running, next, pressFinish
}

// dismissFinishIntoBackground confirms a finish of inst without merging, then
// dismisses its progress with Esc. The finish command is never run: callers
// deliver its result themselves.
func dismissFinishIntoBackground(t *testing.T, h *Home, inst *session.Instance, pressFinish func(*session.Instance) tea.Cmd) {
	t.Helper()
	require.NotNil(t, pressFinish(inst), "opening the finish must start its dirty check")
	require.Equal(t, inst.ID, h.worktreeFinishDialog.GetSessionID())
	h.Update(tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}})
	h.Update(tea.KeyMsg{Type: tea.KeyEnter})
	_, finish := h.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	require.NotNil(t, finish, "confirming must dispatch the finish")
	require.True(t, h.worktreeFinishDialog.IsExecuting())
	require.Contains(t, stripAnsi(h.View()), "Esc continue in background", "the progress must say how to dismiss it")
	h.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	require.True(t, h.worktreeFinishDialog.IsVisible(), "keys other than Esc stay blocked while finishing")

	_, cmd := h.Update(tea.KeyMsg{Type: tea.KeyEsc})
	require.Nil(t, cmd)
	require.False(t, h.worktreeFinishDialog.IsVisible(), "Esc must dismiss an executing finish")
	require.True(t, h.worktreeFinishDialog.IsExecuting(), "dismissing the finish must not forget it is running")
	require.EqualError(t, h.err, "finishing worktree '"+inst.Title+"' in the background")
}
