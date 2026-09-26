package ui

import (
	"errors"
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
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	// W probes the repository's default branch; keep git from finding one
	// above the fixture's root.
	root := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(root))
	detachGlobalStateDB(t) // NewHome registers its own, soon-closed database.
	h := NewHome()
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
	running, next := worktree("running"), worktree("next")
	h.instances = []*session.Instance{running, next}
	h.instanceByID = map[string]*session.Instance{running.ID: running, next.ID: next}
	h.groupTree = session.NewGroupTree(h.instances)
	h.rebuildFlatItems()
	pressFinish := func(inst *session.Instance) tea.Cmd {
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

	// Confirm a finish without merging. The returned command is never run:
	// the fixture delivers its result below.
	require.NotNil(t, pressFinish(running), "opening the finish must start its dirty check")
	require.Equal(t, running.ID, h.worktreeFinishDialog.GetSessionID())
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
	require.EqualError(t, h.err, "finishing worktree 'running' in the background")

	// Reopening the dialog would reset the running finish's state, and its
	// result would then fail or hide the new dialog.
	require.Nil(t, pressFinish(next), "no second finish while one runs")
	require.False(t, h.worktreeFinishDialog.IsVisible())
	require.Equal(t, running.ID, h.worktreeFinishDialog.GetSessionID())
	require.EqualError(t, h.err, "still finishing worktree 'running'")

	_, cmd = h.Update(worktreeFinishResultMsg{
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
