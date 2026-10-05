package session

import (
	"sync"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

func TestRuntimeLifecycle_StartRuntimeReturnsReconciledTupleBeforeLaterMutation(t *testing.T) {
	installRuntimeLifecycleTestSeams(t)
	db, inst := runtimeLifecycleTestDB(t, "pi", nil)
	want, found, err := db.ReadRuntimeState(inst.ID)
	if err != nil || !found {
		t.Fatalf("read runtime: found=%v err=%v", found, err)
	}
	runtimeCandidateInventoryFn = func(string, string) ([]tmux.RuntimeCandidate, error) {
		return []tmux.RuntimeCandidate{{
			SessionName: want.TmuxSession, SocketName: want.TmuxSocketName,
			InstanceID: want.InstanceID, Generation: want.Generation, GenerationKnown: true,
			StatusRevision: want.StatusRevision, Status: want.Status,
			LastStartedUnixNano: want.LastStartedAt.UnixNano(), StateKnown: true,
			BindingKnown: true, PanePID: 4242,
		}}, nil
	}

	later := want
	later.Generation++
	later.TmuxSession = "runtime-after-start"
	released := false
	instanceSpawnLockAcquireFn = func(string) (func(), error) {
		return func() {
			released = true
			inst.ApplyRuntimeState(later)
		}, nil
	}

	got, err := inst.StartRuntime()
	if err != nil {
		t.Fatal(err)
	}
	if !released {
		t.Fatal("test did not interleave the later mutation")
	}
	if got != want {
		t.Fatalf("StartRuntime returned %+v, want reconciled tuple %+v", got, want)
	}
	if current := inst.RuntimeState(); current != later {
		t.Fatalf("current runtime = %+v, want subsequent mutation %+v", current, later)
	}
}

func TestRuntimeLifecycle_RestartRuntimeReturnsLockWinnerBeforeLaterMutation(t *testing.T) {
	installRuntimeLifecycleTestSeams(t)
	db, inst := runtimeLifecycleTestDB(t, "pi", nil)
	initial, found, err := db.ReadRuntimeState(inst.ID)
	if err != nil || !found {
		t.Fatalf("read runtime: found=%v err=%v", found, err)
	}
	want := initial
	want.Generation++
	want.TmuxSession = "runtime-lock-winner"
	var publishOnce sync.Once
	var publishErr error
	runtimeTransitionObservedFn = func() {
		publishOnce.Do(func() {
			publishErr = db.CommitRuntimeTransition(initial.Generation, inst.PersistenceIncarnation(), want)
		})
	}

	later := want
	later.Generation++
	later.TmuxSession = "runtime-after-restart"
	released := false
	instanceSpawnLockAcquireFn = func(string) (func(), error) {
		return func() {
			released = true
			inst.ApplyRuntimeState(later)
		}, nil
	}

	got, err := inst.RestartRuntime()
	if publishErr != nil {
		t.Fatalf("publish lock winner: %v", publishErr)
	}
	if err != nil {
		t.Fatal(err)
	}
	if !released {
		t.Fatal("test did not interleave the later mutation")
	}
	if got != want {
		t.Fatalf("RestartRuntime returned %+v, want lock winner %+v", got, want)
	}
	if current := inst.RuntimeState(); current != later {
		t.Fatalf("current runtime = %+v, want subsequent mutation %+v", current, later)
	}
}
