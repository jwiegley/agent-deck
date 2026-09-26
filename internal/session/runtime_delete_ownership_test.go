package session

import (
	"errors"
	"fmt"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// A destruction whose inventories hold no provable candidate may complete as
// stopped only once the selected tmux session stops answering, from a process
// that can see its server. Every destructive entry point shares that capture,
// so each one refuses a live session the inventories cannot prove and leaves
// the runtime, the row and the process exactly as they were.
func TestRuntimeLifecycle_DestructionRefusesLiveSessionWithoutOwnershipProof(t *testing.T) {
	refusals := []struct {
		name    string
		exists  func() (bool, error)
		foreign bool
	}{
		{name: "live without stamp", exists: func() (bool, error) { return true, nil }},
		{name: "indeterminate probe", exists: func() (bool, error) { return false, errors.New("tmux: probe timed out") }},
		{name: "invisible default server", exists: func() (bool, error) { return false, nil }, foreign: true},
	}
	operations := []struct {
		name string
		run  func(*Instance, *statedb.StateDB, statedb.RuntimeState) error
	}{
		{name: "kill", run: func(inst *Instance, _ *statedb.StateDB, _ statedb.RuntimeState) error {
			return inst.KillCaptured(inst.CaptureRuntimeSelection())
		}},
		{name: "kill and wait", run: func(inst *Instance, _ *statedb.StateDB, _ statedb.RuntimeState) error {
			return inst.KillAndWaitCaptured(inst.CaptureRuntimeSelection())
		}},
		{name: "delete", run: func(inst *Instance, _ *statedb.StateDB, _ statedb.RuntimeState) error {
			return inst.DeleteCaptured(inst.CaptureRuntimeSelection())
		}},
		{name: "restart predecessor", run: func(inst *Instance, db *statedb.StateDB, state statedb.RuntimeState) error {
			return inst.terminateTransitionPredecessor(&runtimeTransitionAuthority{
				expected: state, incarnation: inst.PersistenceIncarnation(), durable: true, db: db,
			})
		}},
		{name: "non-durable restart predecessor", run: func(inst *Instance, _ *statedb.StateDB, state statedb.RuntimeState) error {
			return inst.terminateTransitionPredecessor(&runtimeTransitionAuthority{
				expected: state, incarnation: inst.PersistenceIncarnation(),
			})
		}},
	}
	for _, refusal := range refusals {
		for _, operation := range operations {
			t.Run(refusal.name+"/"+operation.name, func(t *testing.T) {
				inst, db, state := newRuntimeDeleteTestInstance(t)
				terminated := 0
				stubRuntimeDeletionPhysicalWork(t, state, func(tmux.RuntimeGenerationCandidate, bool) error {
					terminated++
					return nil
				})
				retired := 0
				retireParentDeleteServiceUnitFn = func(*Instance, tmux.ServiceUnitOwnership) { retired++ }
				runtimeCandidateInventoryFn = func(string, string) ([]tmux.RuntimeCandidate, error) { return nil, nil }
				runtimeGenerationCandidateInventoryFn = func(string, string) ([]tmux.RuntimeGenerationCandidate, error) {
					return nil, nil
				}
				var probed []string
				selectedRuntimeSessionExistsFn = func(socketName, sessionName string) (bool, error) {
					probed = append(probed, socketName+"/"+sessionName)
					return refusal.exists()
				}
				destructionAbsenceIsForeignServerFn = func(got statedb.RuntimeState) bool {
					if got.TmuxSession != state.TmuxSession || got.TmuxSocketName != state.TmuxSocketName {
						t.Errorf("foreign-server check for %q/%q, want the selected identity", got.TmuxSocketName, got.TmuxSession)
					}
					return refusal.foreign
				}
				before := inst.RuntimeState()

				err := operation.run(inst, db, state)
				if !errors.Is(err, ErrRuntimeOwnershipUnproven) || !strings.Contains(err.Error(), state.TmuxSession) {
					t.Fatalf("%s error = %v, want ErrRuntimeOwnershipUnproven naming %q", operation.name, err, state.TmuxSession)
				}
				if want := []string{state.TmuxSocketName + "/" + state.TmuxSession}; !reflect.DeepEqual(probed, want) {
					t.Fatalf("selected-session probes = %q, want %q", probed, want)
				}
				if terminated != 0 || retired != 0 {
					t.Fatalf("refused destruction mutated auxiliaries: terminations=%d retirements=%d", terminated, retired)
				}
				got, found, readErr := db.ReadRuntimeState(state.InstanceID)
				if readErr != nil || !found || !sameDestructiveRuntime(got, state) {
					t.Fatalf("durable runtime after refusal = %#v found=%v err=%v, want untouched %#v", got, found, readErr, state)
				}
				if row, loadErr := db.LoadInstanceByID(state.InstanceID); loadErr != nil || row == nil {
					t.Fatalf("refused destruction removed the row: row=%#v err=%v", row, loadErr)
				}
				if current := inst.RuntimeState(); !sameDestructiveRuntime(current, before) {
					t.Fatalf("in-memory runtime after refusal = %#v, want %#v", current, before)
				}
			})
		}
	}
}

// Once the selected session no longer answers, a destruction with no
// candidate is an already-stopped runtime and completes. A destruction that
// captured its candidate never pays for the extra probe.
func TestRuntimeLifecycle_DestructionOfVanishedSessionCompletesStopped(t *testing.T) {
	for _, vanished := range []bool{true, false} {
		t.Run(fmt.Sprintf("vanished=%t", vanished), func(t *testing.T) {
			inst, db, state := newRuntimeDeleteTestInstance(t)
			terminated := 0
			stubRuntimeDeletionPhysicalWork(t, state, func(tmux.RuntimeGenerationCandidate, bool) error {
				terminated++
				return nil
			})
			if vanished {
				runtimeCandidateInventoryFn = func(string, string) ([]tmux.RuntimeCandidate, error) { return nil, nil }
				runtimeGenerationCandidateInventoryFn = func(string, string) ([]tmux.RuntimeGenerationCandidate, error) {
					return nil, nil
				}
			}
			var probed []string
			selectedRuntimeSessionExistsFn = func(socketName, sessionName string) (bool, error) {
				probed = append(probed, socketName+"/"+sessionName)
				return false, nil
			}

			if err := inst.KillCaptured(inst.CaptureRuntimeSelection()); err != nil {
				t.Fatal(err)
			}
			wantProbes, wantTerminated := []string(nil), 1
			if vanished {
				wantProbes, wantTerminated = []string{state.TmuxSocketName + "/" + state.TmuxSession}, 0
			}
			if !reflect.DeepEqual(probed, wantProbes) || terminated != wantTerminated {
				t.Fatalf("probes=%q terminations=%d, want %q and %d", probed, terminated, wantProbes, wantTerminated)
			}
			got, found, err := db.ReadRuntimeState(state.InstanceID)
			if err != nil || !found || got.Status != string(StatusStopped) || got.StatusRevision != state.StatusRevision+2 {
				t.Fatalf("runtime after kill = %#v found=%v err=%v", got, found, err)
			}
		})
	}
}

// startUnownedTmuxSession starts a plain tmux session on a private socket, as
// a user (or `agent-deck` import) would find it: no Agent Deck stamp at all.
func startUnownedTmuxSession(t *testing.T, socketName, sessionName string) {
	t.Helper()
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", socketName, "kill-server").Run() })
	if out, err := exec.Command("tmux", "-L", socketName, "new-session", "-d", "-s", sessionName, "sleep 300").CombinedOutput(); err != nil {
		t.Fatalf("start unowned tmux session: %v: %s", err, out)
	}
}

func tmuxSessionAnswers(socketName, sessionName string) bool {
	return exec.Command("tmux", "-L", socketName, "has-session", "-t", "="+sessionName).Run() == nil
}

// startStampedUnprefixedRuntime has Agent Deck start (and so stamp) a shell
// runtime under a tmux name without SessionPrefix, as it does for an imported
// session or the storage goldens' "ad-golden-sess-shell" fixture.
func startStampedUnprefixedRuntime(t *testing.T, profile string) (*Storage, *Instance, string, string) {
	t.Helper()
	skipIfNoTmuxBinary(t)
	socketName := fmt.Sprintf("adtest-unprefixed-%d", time.Now().UnixNano())
	sessionName := "ad-unprefixed-shell"
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", socketName, "kill-server").Run() })

	storage, err := NewStorageWithProfile(profile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	inst := NewInstanceWithTool("stamped unprefixed", t.TempDir(), "shell")
	sess := tmux.ReconnectSessionLazy(sessionName, inst.Title, inst.ProjectPath, "", string(StatusIdle))
	sess.SocketName = socketName
	sess.InstanceID = inst.ID
	applyTmuxSessionSettings(sess)
	inst.tmuxSession = sess
	inst.TmuxSocketName = socketName
	if err := storage.InsertSessionAndVerify(inst, nil); err != nil {
		t.Fatal(err)
	}
	if err := inst.Start(); err != nil {
		t.Fatal(err)
	}
	if !tmuxSessionAnswers(socketName, sessionName) {
		t.Fatalf("started runtime %q is not live", sessionName)
	}
	return storage, inst, socketName, sessionName
}

// Reconciliation sees the runtime Agent Deck stamped whatever its tmux name, so
// a second start adopts it instead of spawning a same-name duplicate.
func TestUnprefixedRuntime_StampedRuntimeIsLiveToReconciliation(t *testing.T) {
	_, inst, _, sessionName := startStampedUnprefixedRuntime(t, "_test_unprefixed_reconcile")

	reconciled, err := inst.ReconcileRuntime()
	if err != nil {
		t.Fatal(err)
	}
	if !reconciled.Live || reconciled.State.TmuxSession != sessionName {
		t.Fatalf("reconciliation = %+v, want the live stamped runtime %q", reconciled, sessionName)
	}
	runtime, err := inst.StartRuntime()
	if err != nil {
		t.Fatalf("second start: %v", err)
	}
	if runtime != reconciled.State {
		t.Fatalf("second start returned %+v, want the adopted live runtime %+v", runtime, reconciled.State)
	}
}

// An instance whose durable runtime names a live tmux session Agent Deck never
// stamped (an imported session, or one from before the stamp) must not be
// reported stopped while that process keeps running: stop refuses, and the
// session and the stored runtime both survive untouched.
func TestUnprefixedRuntime_UnownedLiveSessionRefusesStop(t *testing.T) {
	skipIfNoTmuxBinary(t)
	socketName := fmt.Sprintf("adtest-unowned-%d", time.Now().UnixNano())
	sessionName := "user-work"
	startUnownedTmuxSession(t, socketName, sessionName)

	storage, err := NewStorageWithProfile("_test_unowned_live_session")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	inst := NewInstanceWithTool("imported work", t.TempDir(), "shell")
	imported := tmux.ReconnectSessionLazy(sessionName, inst.Title, inst.ProjectPath, "", string(StatusIdle))
	imported.SocketName = socketName
	imported.InstanceID = inst.ID
	inst.tmuxSession = imported
	inst.TmuxSocketName = socketName
	if err := storage.InsertSessionAndVerify(inst, nil); err != nil {
		t.Fatal(err)
	}
	before, found, err := storage.GetDB().ReadRuntimeState(inst.ID)
	if err != nil || !found || before.TmuxSession != sessionName {
		t.Fatalf("stored runtime = %#v found=%v err=%v", before, found, err)
	}

	err = inst.KillCaptured(inst.CaptureRuntimeSelection())
	if !errors.Is(err, ErrRuntimeOwnershipUnproven) {
		t.Fatalf("stop of an unowned live session = %v, want ErrRuntimeOwnershipUnproven", err)
	}
	if !tmuxSessionAnswers(socketName, sessionName) {
		t.Fatal("refused stop killed the unowned session")
	}
	after, found, err := storage.GetDB().ReadRuntimeState(inst.ID)
	if err != nil || !found || !sameDestructiveRuntime(after, before) {
		t.Fatalf("stored runtime after refused stop = %#v found=%v err=%v, want %#v", after, found, err, before)
	}
}
