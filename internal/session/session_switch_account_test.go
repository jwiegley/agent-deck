package session

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

func TestRuntimeLifecycle_SwitchAccountRuntime_BlocksRestartBetweenKillAndNewAccountSpawn(t *testing.T) {
	installRuntimeLifecycleTestSeams(t)
	db, inst := runtimeLifecycleTestDB(t, "claude", nil)
	inst.Account = "old"
	selection := inst.CaptureRuntimeSelection()
	installRuntimeDeletionCandidateSeams(t, selection.State)

	oldTerminate := terminateCapturedRuntimeFn
	oldSwitchRestart := switchAccountRestartWithTransitionFn
	t.Cleanup(func() {
		terminateCapturedRuntimeFn = oldTerminate
		switchAccountRestartWithTransitionFn = oldSwitchRestart
	})

	killed := make(chan struct{})
	var killedOnce sync.Once
	oldRuntimeAlive := true
	var terminated []tmux.RuntimeGenerationCandidate
	terminateCapturedRuntimeFn = func(candidate tmux.RuntimeGenerationCandidate, wait bool) error {
		if wait {
			return fmt.Errorf("switch-account unexpectedly requested synchronous termination")
		}
		terminated = append(terminated, candidate)
		oldRuntimeAlive = false
		killedOnce.Do(func() { close(killed) })
		return nil
	}

	externalObserved := make(chan struct{})
	var observations atomic.Int32
	runtimeTransitionObservedFn = func() {
		if observations.Add(1) == 2 {
			close(externalObserved)
		}
	}

	var spawnAccounts []string
	switchAccountRestartWithTransitionFn = func(i *Instance, authority *runtimeTransitionAuthority, result *statedb.RuntimeState) error {
		if oldRuntimeAlive {
			return fmt.Errorf("old-account runtime survived into replacement spawn")
		}
		spawnAccounts = append(spawnAccounts, i.Account)
		setRuntimeTestCandidate(i, "runtime-g1")
		candidate, _, committed, err := i.commitPhysicalRuntime(authority)
		captureRuntimeResult(result, candidate)
		if err != nil {
			return err
		}
		if !committed {
			return fmt.Errorf("replacement runtime was not committed")
		}
		return nil
	}

	type restartResult struct {
		runtime statedb.RuntimeState
		err     error
	}
	externalDone := make(chan restartResult, 1)
	go func() {
		<-killed
		runtime, err := inst.RestartRuntime()
		externalDone <- restartResult{runtime: runtime, err: err}
	}()

	runtime, err := inst.SwitchAccountRuntime(selection, true, func() error {
		<-externalObserved
		select {
		case early := <-externalDone:
			return fmt.Errorf("external restart interleaved after kill: runtime=%+v err=%v", early.runtime, early.err)
		default:
		}
		inst.Account = "new"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	var external restartResult
	select {
	case external = <-externalDone:
	case <-time.After(5 * time.Second):
		t.Fatal("external restart did not resume after switch-account released authority")
	}
	if external.err != nil {
		t.Fatalf("external restart: %v", external.err)
	}
	if external.runtime != runtime {
		t.Fatalf("external restart adopted %+v, want switch winner %+v", external.runtime, runtime)
	}
	if len(terminated) != 1 || !runtimeGenerationCandidateMatchesState(terminated[0], selection.State) {
		t.Fatalf("terminated runtimes = %+v, want only captured predecessor %+v", terminated, selection.State)
	}
	if oldRuntimeAlive {
		t.Fatal("old-account runtime remained alive")
	}
	if len(spawnAccounts) != 1 || spawnAccounts[0] != "new" {
		t.Fatalf("replacement spawn accounts = %v, want only new", spawnAccounts)
	}
	if runtime.Generation != selection.State.Generation+1 || runtime.TmuxSession != "runtime-g1" {
		t.Fatalf("switch runtime = %+v, want generation %d runtime-g1", runtime, selection.State.Generation+1)
	}
	durable, found, readErr := db.ReadRuntimeState(inst.ID)
	if readErr != nil || !found || durable != runtime {
		t.Fatalf("durable runtime = %+v found=%v err=%v, want %+v", durable, found, readErr, runtime)
	}
}
