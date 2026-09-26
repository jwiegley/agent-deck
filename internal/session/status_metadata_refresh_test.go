package session

import (
	"os/exec"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// loadStatusMetadataFixture reloads one stored row the way a one-pass CLI
// process does (`session show`, `list --json`): from SQLite, never added or
// started in this process, and old enough to be past the tmux grace window.
func loadStatusMetadataFixture(t *testing.T, tool, tmuxName string, status Status) (*Storage, *Instance) {
	t.Helper()
	storage := newTestStorage(t)
	created := time.Now().Add(-time.Hour).UTC()
	row := &statedb.InstanceRow{
		ID: "status-metadata", Title: "status metadata", ProjectPath: t.TempDir(),
		GroupPath: "my-sessions", Tool: tool, Status: string(status),
		TmuxSession: tmuxName, CreatedAt: created, LastAccessed: created,
		ToolData: []byte(`{}`),
	}
	if err := storage.db.SaveInstances([]*statedb.InstanceRow{row}); err != nil {
		t.Fatal(err)
	}
	loaded, _, err := storage.LoadWithGroups()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 || loaded[0].GetTmuxSession() == nil {
		t.Fatalf("fixture did not reload one tmux-backed instance: %+v", loaded)
	}
	return storage, loaded[0]
}

// Tool identity is refreshed only after sampling a live pane. A stored claude
// row with no recorded command whose tmux session is gone must report the
// dead-pane status without the failed pane capture renaming it "shell"; that
// rename dropped the Claude-only fields from `session show`.
func TestStatusMetadataRefresh_AbsentPaneKeepsStoredTool(t *testing.T) {
	storage, inst := loadStatusMetadataFixture(t, "claude", "agentdeck-status-metadata-absent", StatusIdle)

	if err := inst.UpdateStatus(); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	if got := inst.GetStatusThreadSafe(); got != StatusError {
		t.Fatalf("status = %q, want %q for a reloaded row whose tmux session is gone", got, StatusError)
	}
	durable, found, err := storage.db.ReadRuntimeState(inst.ID)
	if err != nil || !found || durable.Status != string(StatusError) {
		t.Fatalf("dead-pane observation not committed: state=%+v found=%v err=%v", durable, found, err)
	}
	if got := inst.GetToolThreadSafe(); got != "claude" {
		t.Fatalf("tool = %q, want stored %q: absent pane was re-detected as a shell", got, "claude")
	}
}

// The gate must not suppress the refresh a live pane sample still owes: a
// shell row whose tmux command names claude is promoted to claude.
func TestStatusMetadataRefresh_LivePaneStillRefreshesTool(t *testing.T) {
	skipIfNoTmuxBinary(t)
	const name = "agentdeck-status-metadata-live"
	if out, err := exec.Command("tmux", "new-session", "-d", "-s", name, "sh", "-c", "sleep 60").CombinedOutput(); err != nil {
		t.Fatalf("tmux new-session: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", name).Run() })

	_, inst := loadStatusMetadataFixture(t, "shell", name, StatusIdle)
	inst.mu.Lock()
	inst.tmuxSession = tmux.ReconnectSessionLazy(name, inst.Title, inst.ProjectPath, "claude", string(StatusIdle))
	inst.mu.Unlock()
	tmux.RefreshExistingSessions()
	tmux.RefreshPaneInfoCache()

	if err := inst.UpdateStatus(); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	if got := inst.GetToolThreadSafe(); got != "claude" {
		t.Fatalf("tool = %q, want live pane detection to promote the shell row to %q", got, "claude")
	}
}
