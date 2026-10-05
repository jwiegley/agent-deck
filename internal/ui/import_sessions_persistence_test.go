package ui

import (
	"errors"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

func TestRuntimeLifecycle_ImportSessionsPersistsExplicitCreationDuringReload(t *testing.T) {
	h, storage := newHeadlessHomeForTest(t, "_test_import_sessions_explicit_insert")
	imported := &session.Instance{
		ID: "imported-tmux-session", Title: "recovered", ProjectPath: t.TempDir(),
		GroupPath: "recovered", Command: "bash", Tool: "shell",
		Status: session.StatusIdle, CreatedAt: time.Unix(1, 0).UTC(),
	}
	h.discoverTmuxSessionsFn = func(existing []*session.Instance) ([]*session.Instance, error) {
		if len(existing) != 0 {
			t.Fatalf("discovery existing=%d, want 0", len(existing))
		}
		return []*session.Instance{imported}, nil
	}

	msg, ok := h.importSessions().(loadSessionsMsg)
	if !ok {
		t.Fatalf("importSessions returned %T", msg)
	}
	if msg.err != nil {
		t.Fatalf("importSessions: %v", msg.err)
	}
	if !h.isReloading {
		t.Fatal("import did not retain the active reload state; routine save would not be suppressed")
	}
	if row, err := storage.GetDB().LoadInstanceByID(imported.ID); err != nil || row == nil {
		t.Fatalf("imported parent=%#v err=%v", row, err)
	}
	if runtime, found, err := storage.GetDB().ReadRuntimeState(imported.ID); err != nil || !found {
		t.Fatalf("imported runtime=%#v found=%v err=%v", runtime, found, err)
	}
	groups, err := storage.GetDB().LoadGroups()
	if err != nil {
		t.Fatalf("LoadGroups: %v", err)
	}
	for _, group := range groups {
		if group.Path == "recovered" {
			return
		}
	}
	t.Fatalf("recovered group not persisted: %#v", groups)
}

func TestRuntimeLifecycle_ImportSessionsNeverPublishesNondurableSuffix(t *testing.T) {
	h, storage := newHeadlessHomeForTest(t, "_test_import_sessions_partial_insert")
	winner := &session.Instance{
		ID: "import-conflict", Title: "durable winner", ProjectPath: t.TempDir(),
		GroupPath: "winner", Command: "bash", Tool: "shell",
		Status: session.StatusIdle, CreatedAt: time.Unix(2, 0).UTC(),
	}
	if err := storage.InsertSessionAndVerify(winner, nil); err != nil {
		t.Fatalf("insert winner: %v", err)
	}
	first := &session.Instance{
		ID: "import-prefix", Title: "durable prefix", ProjectPath: t.TempDir(),
		GroupPath: "recovered", Command: "bash", Tool: "shell",
		Status: session.StatusIdle, CreatedAt: time.Unix(3, 0).UTC(),
	}
	conflict := &session.Instance{
		ID: winner.ID, Title: "nondurable suffix", ProjectPath: t.TempDir(),
		GroupPath: "recovered", Command: "bash", Tool: "shell",
		Status: session.StatusIdle, CreatedAt: time.Unix(4, 0).UTC(),
	}
	h.discoverTmuxSessionsFn = func([]*session.Instance) ([]*session.Instance, error) {
		return []*session.Instance{first, conflict}, nil
	}

	msg, ok := h.importSessions().(loadSessionsMsg)
	if !ok {
		t.Fatalf("importSessions returned %T", msg)
	}
	if !errors.Is(msg.err, session.ErrSessionAlreadyExists) {
		t.Fatalf("importSessions error = %v, want same-ID conflict", msg.err)
	}
	if len(h.instances) != 1 || h.instances[0] != first {
		t.Fatalf("published instances = %#v, want only committed prefix", h.instances)
	}
	if row, err := storage.GetDB().LoadInstanceByID(first.ID); err != nil || row == nil {
		t.Fatalf("committed prefix row = %#v, err=%v", row, err)
	}
	if row, err := storage.GetDB().LoadInstanceByID(winner.ID); err != nil || row == nil || row.Title != winner.Title {
		t.Fatalf("same-ID winner changed: row=%#v err=%v", row, err)
	}
}
