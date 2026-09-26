package session

import (
	"errors"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// stageForeignServerGuard replaces the probe's foreign-server guard for one
// test: fn stands in for a process inside another tmux server.
func stageForeignServerGuard(t *testing.T, fn func(*tmux.Session) bool) {
	t.Helper()
	old := statusAbsenceIsForeignServerFn
	statusAbsenceIsForeignServerFn = fn
	t.Cleanup(func() { statusAbsenceIsForeignServerFn = old })
}

// The guard can list the default server from inside another tmux server, so,
// like probeTmuxExists, it runs without the instance lock: status readers do
// not wait behind it. A status committed meanwhile makes its answer stale, and
// the pass must then discard it before classifying a death on top.
func TestStatusProbe_ForeignServerGuardRunsUnlockedAndRevalidates(t *testing.T) {
	for _, c := range []struct {
		name     string
		interim  bool // another writer commits while the guard runs
		foreign  bool
		want     Status
		conflict bool
	}{
		{"guard runs unlocked", false, true, StatusRunning, false},
		{"interim commit discards the guard's answer", true, false, StatusWaiting, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			storage, inst := loadStatusMetadataFixture(t, "claude", "agentdeck-status-guard-lock", StatusRunning)
			exitProbed := false
			inst.paneDeadExitStatusForTest = func() (int, bool) {
				exitProbed = true
				return 1, true
			}
			guardCalls, lockHeld := 0, false
			stageForeignServerGuard(t, func(*tmux.Session) bool {
				guardCalls++
				if !inst.mu.TryLock() {
					lockHeld = true
					return c.foreign
				}
				inst.mu.Unlock()
				if c.interim {
					if err := PersistSelectedStatus(storage, inst, StatusWaiting); err != nil {
						t.Errorf("interim commit: %v", err)
					}
				}
				return c.foreign
			})

			err := inst.UpdateStatus()
			if guardCalls != 1 || lockHeld {
				t.Fatalf("guard calls = %d, instance lock held during the guard = %v; want one unlocked call", guardCalls, lockHeld)
			}
			if gotConflict := errors.Is(err, statedb.ErrStatusRevisionConflict); gotConflict != c.conflict {
				t.Fatalf("status pass error = %v, want revision conflict %v", err, c.conflict)
			}
			if exitProbed {
				t.Fatal("the pass classified a death after the guard: a stale or foreign absence reached the exit-status probe")
			}
			if got := inst.GetStatusThreadSafe(); got != c.want {
				t.Fatalf("status = %q, want %q", got, c.want)
			}
			durable, found, err := storage.db.ReadRuntimeState(inst.ID)
			if err != nil || !found || durable.Status != string(c.want) {
				t.Fatalf("durable status = %+v found=%v err=%v, want %q", durable, found, err, c.want)
			}
		})
	}
}
