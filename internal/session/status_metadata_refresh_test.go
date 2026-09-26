package session

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
// shell row whose tmux command names claude is promoted to claude. As
// upstream, that holds for a read-only listing's status-only pass too; only
// native session-ID discovery is left to the poller.
func TestStatusMetadataRefresh_LivePaneStillRefreshesTool(t *testing.T) {
	skipIfNoTmuxBinary(t)
	for _, c := range []struct {
		name   string
		update func(*Instance) error
	}{
		{"poller", (*Instance).UpdateStatus},
		{"status-only listing", func(inst *Instance) error {
			var pass StatusUpdatePass
			return pass.UpdateStatusOnly(inst)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
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

			if err := c.update(inst); err != nil {
				t.Fatalf("status pass: %v", err)
			}
			if got := inst.GetToolThreadSafe(); got != "claude" {
				t.Fatalf("tool = %q, want live pane detection to promote the shell row to %q", got, "claude")
			}
		})
	}
}

// startMetadataPane starts a live Claude pane showing a busy spinner and
// reloads it as a fresh process past the pane's startup window, with the
// capture-resume pattern's CLAUDE_SESSION_ID in its tmux environment.
func startMetadataPane(t *testing.T, name, sessionID string) *Instance {
	t.Helper()
	inst, cleanup := startPaneInstance(t, "claude", name, auditConductorBusyPane)
	t.Cleanup(cleanup)
	storage, err := NewStorageWithProfile("_test-" + name)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	fresh := persistAndReload(t, storage, inst, StatusRunning)
	if err := fresh.tmuxSession.SetEnvironment("CLAUDE_SESSION_ID", sessionID); err != nil {
		t.Fatalf("set tmux env: %v", err)
	}
	return fresh
}

// The metadata refresh keeps its place at the end of the tmux path. The hook
// fast path skips it (TestAudit_B_RunningFastPathMakesNoTmuxCalls), but a
// verdict the pane itself decided still syncs, in the same pass, the session
// ID the capture-resume pattern left in the tmux environment.
func TestStatusMetadataRefresh_PaneVerdictSyncsSessionIDInSamePass(t *testing.T) {
	const sessionID = "cccccccc-cccc-cccc-cccc-cccccccccccc"
	fresh := startMetadataPane(t, "pane-verdict-metadata", sessionID)

	evidence, err := fresh.updateStatusWithEvidence(nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := fresh.GetStatusThreadSafe(); got != StatusRunning || !evidence.paneSampled.Load() {
		t.Fatalf("status = %q (pane sampled %v), want running decided by the live spinner", got, evidence.paneSampled.Load())
	}
	if got := fresh.ClaudeSessionID; got != sessionID {
		t.Fatalf("session ID after a pane verdict = %q, want %q from the tmux environment", got, sessionID)
	}
}

// Native session-ID discovery is the poller's job alone: a read-only
// listing's status-only pass samples the same pane, but leaves the tmux
// environment unread and the discovery throttle untouched, so the next poll
// still binds the session ID.
func TestStatusMetadataRefresh_StatusOnlyPassLeavesDiscoveryToPoller(t *testing.T) {
	const sessionID = "dddddddd-dddd-dddd-dddd-dddddddddddd"
	fresh := startMetadataPane(t, "status-only-discovery", sessionID)

	var pass StatusUpdatePass
	if err := pass.UpdateStatusOnly(fresh); err != nil {
		t.Fatal(err)
	}
	if got := fresh.GetStatusThreadSafe(); got != StatusRunning {
		t.Fatalf("status = %q, want running from the live spinner", got)
	}
	if got := fresh.ClaudeSessionID; got != "" {
		t.Fatalf("status-only pass discovered session ID %q; discovery belongs to the poller", got)
	}
	if err := fresh.UpdateStatus(); err != nil {
		t.Fatal(err)
	}
	if got := fresh.ClaudeSessionID; got != sessionID {
		t.Fatalf("poller session ID = %q, want %q from the tmux environment", got, sessionID)
	}
}

// A read-only listing publishes a cached hook's binding without waiting for
// the instance spawn lock: a start or restart can hold it for the lock's whole
// 30-second budget, and a listing that waited would stall that long for every
// such session. A contended listing still commits its verdict and skips the
// binding, which the next uncontended pass publishes.
func TestStatusMetadataRefresh_StatusOnlyPassSkipsHeldSpawnLock(t *testing.T) {
	const hookSessionID = "ffffffff-ffff-ffff-ffff-ffffffffffff"
	t.Setenv("HOME", t.TempDir())
	storage, inst := loadStatusMetadataFixture(t, "claude", "agentdeck-status-spawn-lock", StatusRunning)
	stageForeignServerGuard(t, func(*tmux.Session) bool { return false })
	inst.mu.Lock()
	inst.hookSessionID = hookSessionID
	inst.mu.Unlock()
	listing := func() error {
		var pass StatusUpdatePass
		return pass.UpdateStatusOnly(inst)
	}

	release, err := acquireInstanceSpawnLock(inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- listing() }()
	select {
	case err := <-done:
		release()
		if err != nil {
			t.Fatalf("status-only pass: %v", err)
		}
	case <-time.After(5 * time.Second):
		release()
		<-done
		t.Fatal("a status-only pass waited for the held instance spawn lock")
	}
	if got := inst.GetStatusThreadSafe(); got != StatusError {
		t.Fatalf("status = %q, want the contended pass's verdict %q committed", got, StatusError)
	}
	if binding, bound, err := storage.db.ReadRuntimeBinding(inst.ID, "claude"); err != nil || bound && binding.Value == hookSessionID {
		t.Fatalf("contended pass published the binding: %+v bound=%v err=%v", binding, bound, err)
	}

	// Once the error recheck is due again, an uncontended listing publishes it.
	inst.mu.Lock()
	inst.lastErrorCheck = time.Now().Add(-errorRecheckInterval)
	inst.mu.Unlock()
	if err := listing(); err != nil {
		t.Fatalf("uncontended status-only pass: %v", err)
	}
	if binding, bound, err := storage.db.ReadRuntimeBinding(inst.ID, "claude"); err != nil || !bound || binding.Value != hookSessionID {
		t.Fatalf("claude binding = %+v bound=%v err=%v, want the hook's %q published", binding, bound, err, hookSessionID)
	}
}

// startEarlyExitPane starts a live tmux pane that prints frame and stays up,
// and waits until the frame is on screen.
func startEarlyExitPane(t *testing.T, name, frame string) {
	t.Helper()
	panePath := filepath.Join(t.TempDir(), "pane.txt")
	if err := os.WriteFile(panePath, []byte(frame), 0o644); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf("cat %q; exec sleep 3600", panePath)
	if out, err := exec.Command("tmux", "new-session", "-d", "-s", name, "sh", "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("tmux new-session: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", name).Run() })
	want := strings.TrimSpace(strings.SplitN(frame, "\n", 2)[0])
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		out, _ := exec.Command("tmux", "capture-pane", "-p", "-t", name).Output()
		if strings.Contains(string(out), want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pane %s never showed %q; screen:\n%s", name, want, out)
		}
	}
}

// Every early exit of the status probe settles its verdict without sampling
// the pane, so none may refresh tool identity, as none reaches upstream's tool
// re-detection at the end of the tmux path. Each row is stored under a tool
// its wrapper's recorded command contradicts (codex), so a refresh that ran
// would rename it; the control row shows that a live pane sample does. The
// exits that take no observation at all also form no verdict.
func TestStatusProbe_EarlyExitsTakeNoPaneSample(t *testing.T) {
	skipIfNoTmuxBinary(t)
	for n, c := range []struct {
		name    string
		tool    string
		status  Status
		frame   string // "" leaves the tmux session absent
		arrange func(t *testing.T, inst *Instance)
		want    Status
		// noVerdict marks an exit that observes nothing; sampled marks the
		// control row, whose verdict came from the pane.
		noVerdict, sampled bool
	}{
		{name: "hook fast path", tool: "claude", status: StatusIdle, frame: "hook fast path\n",
			arrange: func(t *testing.T, inst *Instance) { writeHookFile(t, inst.ID, "running", 1) },
			want:    StatusRunning},
		{name: "SSE fast path", tool: "opencode", status: StatusIdle, frame: "sse fast path\n",
			arrange: func(_ *testing.T, inst *Instance) { inst.UpdateOpenCodeSSEStatus("running", time.Now()) },
			want:    StatusRunning},
		{name: "idle tier skip", tool: "claude", status: StatusIdle, frame: "idle tier\n",
			arrange: func(_ *testing.T, inst *Instance) {
				inst.mu.Lock()
				inst.lastIdleCheck = time.Now()
				inst.lastKnownActivity = inst.tmuxSession.GetCachedWindowActivity()
				inst.mu.Unlock()
			},
			want: StatusIdle, noVerdict: true},
		{name: "error recheck skip", tool: "claude", status: StatusError,
			arrange: func(_ *testing.T, inst *Instance) {
				inst.mu.Lock()
				inst.lastErrorCheck = time.Now()
				inst.lastErrorCheckGeneration = inst.RuntimeGeneration
				inst.mu.Unlock()
			},
			want: StatusError, noVerdict: true},
		{name: "tmux grace window", tool: "claude", status: StatusStarting,
			arrange: func(_ *testing.T, inst *Instance) {
				inst.mu.Lock()
				inst.CreatedAt = time.Now()
				inst.mu.Unlock()
			},
			want: StatusStarting},
		{name: "debounce hold", tool: "claude", status: StatusRunning, frame: codexIdleFrame,
			// This process settled running itself, so one idle frame is held.
			arrange: func(_ *testing.T, inst *Instance) { inst.SeedLiveStatusPrior(StatusRunning, false) },
			want:    StatusRunning},
		{name: "foreign server absence", tool: "claude", status: StatusRunning,
			arrange: func(t *testing.T, _ *Instance) {
				old := statusAbsenceIsForeignServerFn
				statusAbsenceIsForeignServerFn = func(*tmux.Session) bool { return true }
				t.Cleanup(func() { statusAbsenceIsForeignServerFn = old })
			},
			want: StatusRunning, noVerdict: true},
		{name: "live pane sample (control)", tool: "claude", status: StatusIdle, frame: "control\n",
			want: StatusIdle, sampled: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			name := fmt.Sprintf("agentdeck-early-exit-%d", n)
			if c.frame != "" {
				startEarlyExitPane(t, name, c.frame)
			}
			_, inst := loadStatusMetadataFixture(t, c.tool, name, c.status)
			inst.mu.Lock()
			inst.tmuxSession = tmux.ReconnectSessionLazy(name, inst.Title, inst.ProjectPath, "codex", string(c.status))
			inst.mu.Unlock()
			tmux.RefreshExistingSessions()
			tmux.RefreshPaneInfoCache()
			if c.arrange != nil {
				c.arrange(t, inst)
			}

			evidence, err := inst.updateStatusWithEvidence(nil, true)
			if err != nil {
				t.Fatalf("status pass: %v", err)
			}
			if got := inst.GetStatusThreadSafe(); got != c.want {
				t.Fatalf("status = %q, want %q", got, c.want)
			}
			if got := evidence.paneSampled.Load(); got != c.sampled {
				t.Fatalf("pane sampled = %v, want %v", got, c.sampled)
			}
			if got := evidence.noVerdict.Load(); got != c.noVerdict {
				t.Fatalf("no verdict = %v, want %v", got, c.noVerdict)
			}
			wantTool := c.tool
			if c.sampled {
				wantTool = "codex"
			}
			if got := inst.GetToolThreadSafe(); got != wantTool {
				t.Fatalf("tool = %q, want %q", got, wantTool)
			}
		})
	}
}
