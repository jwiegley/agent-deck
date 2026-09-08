package ui

import (
	"errors"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

func undoSeedTestInstance(id, title string) *session.Instance {
	return &session.Instance{
		ID: id, Title: title, ProjectPath: "/tmp/" + id,
		GroupPath: session.DefaultGroupPath, Command: "bash", Tool: "shell",
		Status: session.StatusStopped, CreatedAt: time.Unix(1, 0).UTC(),
	}
}

func seedDeletedUndoTestInstance(t *testing.T, storage *session.Storage, id, title string) *session.Instance {
	t.Helper()
	inst := undoSeedTestInstance(id, title)
	if err := storage.InsertSessionAndVerify(inst, nil); err != nil {
		t.Fatalf("insert deleted session: %v", err)
	}
	if err := storage.DeleteInstance(inst.ID); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	return inst
}

func TestRuntimeLifecycle_TUIUndoRotatesDeletedIncarnationAndRejectsStaleWriter(t *testing.T) {
	h, storage := newHeadlessHomeForTest(t, "_test_tui_undo_incarnation_aba")
	deleted := undoSeedTestInstance("tui-undo-incarnation-aba", "deleted snapshot")
	if err := storage.InsertSessionAndVerify(deleted, nil); err != nil {
		t.Fatalf("insert deleted session: %v", err)
	}
	deletedIncarnation := deleted.PersistenceIncarnation()
	if err := storage.DeleteInstance(deleted.ID); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	h.undoStack = []deletedSessionEntry{{instance: deleted}}
	h.restartRuntimeFn = func(inst *session.Instance) (statedb.RuntimeState, error) {
		return inst.RuntimeState(), nil
	}

	_, cmd := h.handleMainKey(tea.KeyMsg{Type: tea.KeyCtrlZ})
	if cmd == nil {
		t.Fatal("Ctrl-Z returned no command")
	}
	msg, ok := cmd().(sessionRestoredMsg)
	if !ok {
		t.Fatal("Ctrl-Z command returned unexpected message")
	}
	if msg.err != nil {
		t.Fatalf("Ctrl-Z restore: %v", msg.err)
	}
	restoredIncarnation := deleted.PersistenceIncarnation()
	if restoredIncarnation == "" || restoredIncarnation == deletedIncarnation ||
		msg.seed.Incarnation != restoredIncarnation {
		t.Fatalf("restored incarnation=%q seed=%q deleted=%q, want a fresh shared token",
			restoredIncarnation, msg.seed.Incarnation, deletedIncarnation)
	}

	applied, err := storage.GetDB().WriteStatusIfVersion(
		deleted.ID, deletedIncarnation, msg.seed.Runtime.Generation,
		msg.seed.Runtime.StatusRevision, string(session.StatusRunning))
	if !errors.Is(err, statedb.ErrInstanceParentConflict) || applied {
		t.Fatalf("stale pre-delete status write applied=%v err=%v, want parent conflict", applied, err)
	}
	durable, found, err := storage.GetDB().ReadRuntimeState(deleted.ID)
	if err != nil || !found || durable != msg.seed.Runtime {
		t.Fatalf("runtime after stale write=%#v found=%v err=%v, want unchanged %#v",
			durable, found, err, msg.seed.Runtime)
	}
}

func TestRuntimeLifecycle_TUIUndoDoesNotOverwriteConcurrentSameIDWinner(t *testing.T) {
	h, storage := newHeadlessHomeForTest(t, "_test_tui_undo_same_id")
	deleted := seedDeletedUndoTestInstance(t, storage, "tui-undo-same-id", "deleted snapshot")
	h.undoStack = []deletedSessionEntry{{instance: deleted}}
	restartCalls := 0
	h.restartRuntimeFn = func(*session.Instance) (statedb.RuntimeState, error) {
		restartCalls++
		return statedb.RuntimeState{}, errors.New("stale undo must not restart")
	}

	_, cmd := h.handleMainKey(tea.KeyMsg{Type: tea.KeyCtrlZ})
	if cmd == nil {
		t.Fatal("Ctrl-Z returned no command")
	}
	winner := undoSeedTestInstance(deleted.ID, "concurrent winner")
	winner.ProjectPath = "/tmp/concurrent-winner"
	winner.Tool = "pi"
	if err := storage.InsertSessionAndVerify(winner, nil); err != nil {
		t.Fatalf("insert winner: %v", err)
	}
	winnerRuntime, found, err := storage.GetDB().ReadRuntimeState(winner.ID)
	if err != nil || !found {
		t.Fatalf("winner runtime=%#v found=%v err=%v", winnerRuntime, found, err)
	}

	msg, ok := cmd().(sessionRestoredMsg)
	if !ok {
		t.Fatalf("Ctrl-Z command returned unexpected message")
	}
	if !errors.Is(msg.err, session.ErrSessionAlreadyExists) {
		t.Fatalf("Ctrl-Z error=%v, want session already exists", msg.err)
	}
	if restartCalls != 0 {
		t.Fatalf("stale undo attempted %d restarts", restartCalls)
	}
	row, err := storage.GetDB().LoadInstanceByID(winner.ID)
	if err != nil || row == nil || row.Title != winner.Title || row.ProjectPath != winner.ProjectPath {
		t.Fatalf("same-ID winner was overwritten: row=%#v err=%v", row, err)
	}
	afterRuntime, found, err := storage.GetDB().ReadRuntimeState(winner.ID)
	if err != nil || !found || afterRuntime != winnerRuntime {
		t.Fatalf("winner runtime=%#v found=%v err=%v, want %#v", afterRuntime, found, err, winnerRuntime)
	}
}

func TestRuntimeLifecycle_TUIUndoRollbackPreservesConcurrentRuntimeWinner(t *testing.T) {
	h, storage := newHeadlessHomeForTest(t, "_test_tui_undo_rollback_winner")
	deleted := seedDeletedUndoTestInstance(t, storage, "tui-undo-runtime-winner", "restore seed")
	h.undoStack = []deletedSessionEntry{{instance: deleted}}
	h.restartRuntimeFn = func(inst *session.Instance) (statedb.RuntimeState, error) {
		initial, found, err := storage.GetDB().ReadRuntimeState(inst.ID)
		if err != nil || !found {
			t.Fatalf("seed runtime=%#v found=%v err=%v", initial, found, err)
		}
		winner := initial
		winner.Generation++
		winner.StatusRevision = 0
		winner.TmuxSession = "competing-runtime"
		winner.Status = string(session.StatusRunning)
		winner.LastStartedAt = time.Unix(9, 0).UTC()
		if err := storage.GetDB().CommitRuntimeTransition(
			initial.Generation, inst.PersistenceIncarnation(), winner); err != nil {
			t.Fatalf("commit competing runtime: %v", err)
		}
		return statedb.RuntimeState{}, errors.New("original restart failed")
	}

	_, cmd := h.handleMainKey(tea.KeyMsg{Type: tea.KeyCtrlZ})
	if cmd == nil {
		t.Fatal("Ctrl-Z returned no command")
	}
	msg, ok := cmd().(sessionRestoredMsg)
	if !ok {
		t.Fatal("Ctrl-Z command returned unexpected message")
	}
	if !errors.Is(msg.err, statedb.ErrRuntimeGenerationConflict) {
		t.Fatalf("Ctrl-Z rollback error=%v, want runtime generation conflict", msg.err)
	}
	if row, err := storage.GetDB().LoadInstanceByID(deleted.ID); err != nil || row == nil {
		t.Fatalf("competing winner parent=%#v err=%v", row, err)
	}
	runtime, found, err := storage.GetDB().ReadRuntimeState(deleted.ID)
	if err != nil || !found || runtime.Generation != 1 || runtime.TmuxSession != "competing-runtime" {
		t.Fatalf("competing winner runtime=%#v found=%v err=%v", runtime, found, err)
	}
}
