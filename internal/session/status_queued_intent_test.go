package session

import (
	"fmt"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

// Queued is operator intent (v1.9.1 group concurrency): the session waits for
// group capacity and has never been started, so the status probe must keep it
// queued, as it keeps stopped. Under runtime authority every status poll
// commits what it observes, so a probe that reads the absent tmux session as a
// death (error), or as a spawn in progress (starting), durably removes the
// session from FindNextQueued's drain and exposes it to `fleet recover`. One
// `list --json` or TUI tick was enough.

// requireQueuedRuntime asserts inst and its durable row still hold exactly the
// queued runtime tuple want: same status, same generation, same revision.
func requireQueuedRuntime(t *testing.T, storage *Storage, inst *Instance, want statedb.RuntimeState, pass string) {
	t.Helper()
	if got := inst.GetStatusThreadSafe(); got != StatusQueued {
		t.Fatalf("%s: status = %q, want the queued operator intent kept", pass, got)
	}
	if got := inst.RuntimeState(); got != want {
		t.Fatalf("%s: runtime = %+v, want the queued tuple %+v untouched", pass, got, want)
	}
	durable, found, err := storage.GetDB().ReadRuntimeState(inst.ID)
	if err != nil || !found || durable != want {
		t.Fatalf("%s: durable runtime = %+v found=%v err=%v, want %+v", pass, durable, found, err, want)
	}
}

// A one-pass CLI process (`list --json`, `session show`) loads the queued row
// from SQLite and refreshes it before printing; the row must come out queued
// at the same revision whether its tmux session is absent, was never named,
// or the load falls inside the tmux grace window.
func TestStatusQueued_CLIStatusRefreshKeepsQueuedRow(t *testing.T) {
	for n, c := range []struct {
		name     string
		tmuxName string
		inGrace  bool
	}{
		{"absent tmux session", "agentdeck-status-queued-absent", false},
		{"no tmux session", "", false},
		{"inside the tmux grace window", "agentdeck-status-queued-grace", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			storage := newTestStorage(t)
			created := time.Now().Add(-time.Hour).UTC()
			row := &statedb.InstanceRow{
				ID: fmt.Sprintf("status-queued-cli-%d", n), Title: "status queued", ProjectPath: t.TempDir(),
				GroupPath: "backend/api", Tool: "codex", Status: string(StatusQueued),
				TmuxSession: c.tmuxName, CreatedAt: created, LastAccessed: created,
				ToolData: []byte(`{}`),
			}
			if err := storage.db.SaveInstances([]*statedb.InstanceRow{row}); err != nil {
				t.Fatal(err)
			}
			loaded, _, err := storage.LoadWithGroups()
			if err != nil || len(loaded) != 1 {
				t.Fatalf("load: %v (%d instances)", err, len(loaded))
			}
			inst := loaded[0]
			if (inst.GetTmuxSession() != nil) != (c.tmuxName != "") {
				t.Fatalf("fixture tmux wrapper = %v, want one only for a named tmux session", inst.GetTmuxSession())
			}
			if c.inGrace {
				inst.mu.Lock()
				inst.CreatedAt = time.Now()
				inst.mu.Unlock()
			}
			queued := inst.RuntimeState()

			RefreshInstancesForCLIStatus([]*Instance{inst})
			var pass StatusUpdatePass
			for i := 1; i <= 2; i++ {
				if err := pass.UpdateStatusOnly(inst); err != nil {
					t.Fatalf("pass %d: %v", i, err)
				}
				requireQueuedRuntime(t, storage, inst, queued, fmt.Sprintf("CLI pass %d", i))
			}
		})
	}
}

// The TUI's background worker polls every session with UpdateStatus, starting
// right after `session start` queued it (inside the tmux grace window) and on
// every tick after. The queued session built in this process keeps its status
// through both.
func TestStatusQueued_TUIPollKeepsQueuedSession(t *testing.T) {
	storage, err := NewStorageWithProfile("_test_status_queued_tui")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	inst := NewInstanceWithTool("status-queued-tui", t.TempDir(), "codex")
	inst.GroupPath = "backend/api"
	if err := storage.InsertSessionAndVerify(inst, nil); err != nil {
		t.Fatal(err)
	}
	if err := PersistSelectedStatus(storage, inst, StatusQueued); err != nil {
		t.Fatal(err)
	}
	queued := inst.RuntimeState()

	for _, phase := range []struct {
		name    string
		created time.Time
	}{
		{"inside the tmux grace window", time.Now()},
		{"after the tmux grace window", time.Now().Add(-time.Hour)},
	} {
		inst.mu.Lock()
		inst.CreatedAt = phase.created
		inst.mu.Unlock()
		for i := 1; i <= 2; i++ {
			if err := inst.UpdateStatus(); err != nil {
				t.Fatalf("%s, poll %d: %v", phase.name, i, err)
			}
			requireQueuedRuntime(t, storage, inst, queued, fmt.Sprintf("%s, poll %d", phase.name, i))
		}
	}
	if next := FindNextQueued([]*Instance{inst}, inst.GroupPath); next != inst {
		t.Fatalf("FindNextQueued = %v, want the polled session still waiting for capacity", next)
	}
}
