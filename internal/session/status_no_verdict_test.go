package session

import (
	"errors"
	"testing"
	"time"

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

// From inside another tmux server a running session reads as absent, and
// upstream's guard forms no verdict for it: its early return leaves every
// piece of state alone. Under the status authority that pass must be as much
// a no-op: no status commit, no "sampled live" mark, no release of the
// cross-process auth-hold sidecar, and no metadata refresh publishing the
// cached hook's session ID. The control row, whose absence is real, shows the
// same fixture does commit, and does publish the binding, once a verdict forms.
func TestStatusNoVerdict_ForeignAbsenceCommitsAndRefreshesNothing(t *testing.T) {
	const hookSessionID = "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"
	for _, c := range []struct {
		name    string
		foreign bool
	}{
		{"foreign server absence", true},
		{"real absence (control)", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			storage, inst := loadStatusMetadataFixture(t, "claude", "agentdeck-status-no-verdict", StatusRunning)
			t.Cleanup(func() { clearAuthHoldRecord(inst.ID) })
			if err := writeAuthHoldRecord(AuthHoldRecord{InstanceID: inst.ID, Reason: AuthHoldReasonDeath}); err != nil {
				t.Fatal(err)
			}
			inst.mu.Lock()
			inst.hookSessionID = hookSessionID
			inst.mu.Unlock()
			before := inst.RuntimeState()
			stageForeignServerGuard(t, func(*tmux.Session) bool { return c.foreign })

			evidence, err := inst.updateStatusWithEvidence(nil, true)
			if err != nil {
				t.Fatalf("status pass: %v", err)
			}
			binding, bound, err := storage.db.ReadRuntimeBinding(inst.ID, "claude")
			if err != nil {
				t.Fatal(err)
			}
			inst.mu.RLock()
			sampledLive := inst.statusSampledLive
			inst.mu.RUnlock()

			if !c.foreign {
				if got := inst.GetStatusThreadSafe(); got != StatusError || evidence.noVerdict.Load() {
					t.Fatalf("control: status = %q (no verdict %v), want a committed error", got, evidence.noVerdict.Load())
				}
				if !bound || binding.Value != hookSessionID {
					t.Fatalf("control: claude binding = %+v bound=%v, want the hook's %q published", binding, bound, hookSessionID)
				}
				return
			}
			if !evidence.noVerdict.Load() {
				t.Fatal("foreign-server absence formed a verdict")
			}
			durable, found, err := storage.db.ReadRuntimeState(inst.ID)
			if err != nil || !found || durable != before || inst.RuntimeState() != before {
				t.Fatalf("runtime changed: memory=%+v durable=%+v found=%v err=%v, want %+v",
					inst.RuntimeState(), durable, found, err, before)
			}
			if bound && binding.Value == hookSessionID || inst.ClaudeSessionID == hookSessionID {
				t.Fatalf("a pass without a verdict published the hook binding: durable=%+v memory=%q", binding, inst.ClaudeSessionID)
			}
			if sampledLive {
				t.Fatal("a pass without a verdict marked the status as sampled live")
			}
			if inst.AuthHold() == nil {
				t.Fatal("a pass without a verdict released the auth-hold sidecar")
			}
		})
	}
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

// The notify daemon reloads every instance on each pass and carries the error
// recheck throttle across. The throttle trusts only the generation it confirmed
// absent, so the daemon carries that generation too: otherwise the throttle
// lapses on every reload, and each ghost session past generation 0 is probed
// on every pass.
func TestStatusProbe_DaemonReloadCarriesRecheckThrottle(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	storage, seeded := loadStatusMetadataFixture(t, "claude", "agentdeck-status-throttle-carry", StatusError)
	// Any session that was ever started is past generation 0.
	observed := seeded.RuntimeState()
	next := observed
	next.Generation++
	next.StatusRevision = 0
	next.LastStartedAt = time.Now().UTC()
	if err := storage.db.CommitRuntimeTransition(observed.Generation, seeded.PersistenceIncarnation(), next); err != nil {
		t.Fatal(err)
	}
	reload := func() *Instance {
		t.Helper()
		loaded, _, err := storage.LoadWithGroups()
		if err != nil || len(loaded) != 1 {
			t.Fatalf("reload: %v (%d instances)", err, len(loaded))
		}
		return loaded[0]
	}
	// The guard runs only after a probe found the tmux session absent.
	probes := 0
	stageForeignServerGuard(t, func(*tmux.Session) bool {
		probes++
		return false
	})

	polled := reload()
	if evidence, err := polled.updateStatusWithEvidence(nil, true); err != nil || evidence.noVerdict.Load() || probes != 1 {
		t.Fatalf("first pass: err=%v no verdict=%v probes=%d; want the absence confirmed", err, evidence.noVerdict.Load(), probes)
	}
	carried := reload()
	if !carried.restorePollingState(polled.pollingStateForIdentity(polled.pollingIdentity())) {
		t.Fatal("the daemon's polling state was not restored onto the reloaded instance")
	}
	evidence, err := carried.updateStatusWithEvidence(nil, true)
	if err != nil || !evidence.noVerdict.Load() || probes != 1 {
		t.Fatalf("pass after reload: err=%v no verdict=%v probes=%d; want the carried throttle to skip the probe",
			err, evidence.noVerdict.Load(), probes)
	}
}
