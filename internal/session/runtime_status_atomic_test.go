package session

import (
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

func TestRuntimeLifecycle_ApplyStatusIfRuntimeVersionIsAtomicWithRuntimeTransition(t *testing.T) {
	inst := &Instance{
		ID:                "one",
		RuntimeGeneration: 1,
		StatusRevision:    4,
		Status:            StatusRunning,
	}

	statusChecked := make(chan struct{})
	transitionAttempted := make(chan struct{})
	oldStatusHook := runtimeStatusAfterVersionFn
	oldApplyHook := runtimeApplyBeforePublishFn
	runtimeStatusAfterVersionFn = func() {
		close(statusChecked)
		<-transitionAttempted
	}
	runtimeApplyBeforePublishFn = func(state statedb.RuntimeState) {
		if state.Generation == 2 {
			close(transitionAttempted)
		}
	}
	t.Cleanup(func() {
		runtimeStatusAfterVersionFn = oldStatusHook
		runtimeApplyBeforePublishFn = oldApplyHook
	})

	type statusResult struct {
		applied bool
		changed bool
	}
	statusDone := make(chan statusResult, 1)
	go func() {
		applied, changed := inst.ApplyStatusIfRuntimeVersion(1, 4, StatusError)
		statusDone <- statusResult{applied: applied, changed: changed}
	}()
	<-statusChecked

	transitionDone := make(chan bool, 1)
	go func() {
		transitionDone <- inst.ApplyRuntimeState(statedb.RuntimeState{
			InstanceID:     "one",
			Generation:     2,
			StatusRevision: 0,
			Status:         string(StatusWaiting),
		})
	}()

	status := <-statusDone
	if !status.applied || !status.changed {
		t.Fatalf("status result = %#v, want applied change", status)
	}
	if !<-transitionDone {
		t.Fatal("generation-2 transition was not applied")
	}
	if got := inst.RuntimeState(); got.Generation != 2 || got.StatusRevision != 0 || got.Status != string(StatusWaiting) {
		t.Fatalf("runtime state = %#v, want generation-2 winner", got)
	}
}
