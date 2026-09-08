package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

func runtimeLifecycleTestState(id string, generation uint64) statedb.RuntimeState {
	return statedb.RuntimeState{
		InstanceID: id, Generation: generation, StatusRevision: 2,
		TmuxSession: "agentdeck-test", TmuxSocketName: "cli-test",
		Status: string(session.StatusRunning), LastStartedAt: time.Unix(1700000000, 0).UTC(),
	}
}

func TestRuntimeLifecycle_CLIConsumesReturnedState(t *testing.T) {
	inst := &session.Instance{ID: "cli-success", Status: session.StatusIdle}
	want := runtimeLifecycleTestState(inst.ID, 5)

	failure, warning := consumeRuntimeResult(inst, want, nil)
	if failure != nil || warning != "" {
		t.Fatalf("failure = %v, warning = %q", failure, warning)
	}
	if got := inst.RuntimeState(); got != want {
		t.Fatalf("runtime = %+v, want %+v", got, want)
	}
}

func TestRuntimeLifecycle_CLILeavesPhysicalFailureInFailureChannel(t *testing.T) {
	inst := &session.Instance{ID: "cli-failure", Status: session.StatusIdle}
	spawnErr := errors.New("spawn failed")

	failure, warning := consumeRuntimeResult(inst, statedb.RuntimeState{}, spawnErr)
	if !errors.Is(failure, spawnErr) || warning != "" {
		t.Fatalf("failure = %v, warning = %q", failure, warning)
	}
	if got := inst.RuntimeState().Generation; got != 0 {
		t.Fatalf("generation = %d, physical failure must not apply a runtime", got)
	}
}

func TestRuntimeLifecycle_CLIPreservesPartialCandidateAsWarning(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	inst := &session.Instance{ID: "cli-partial", Status: session.StatusIdle}
	want := runtimeLifecycleTestState(inst.ID, 6)
	partial := &session.RestartPartialSuccessError{
		InstanceID: inst.ID, Runtime: want, NeedsReconciliation: true,
		Err: errors.New("database is locked"),
	}

	failure, warning := consumeRuntimeResult(inst, want, partial)
	if failure != nil {
		t.Fatalf("failure = %v, completed physical start must not enter retry/rollback", failure)
	}
	if !strings.Contains(warning, "durability reconciliation failed") {
		t.Fatalf("warning = %q, want incomplete reconciliation warning", warning)
	}
	if got := inst.RuntimeState(); got != want {
		t.Fatalf("runtime = %+v, want preserved candidate %+v", got, want)
	}
}

func TestRuntimeLifecycle_CLIKeepsEqualGenerationWinner(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	inst := &session.Instance{ID: "cli-winner", Status: session.StatusIdle}
	winner := runtimeLifecycleTestState(inst.ID, 8)
	winner.TmuxSession = "winner"
	winner.TmuxSocketName = "socket-winner"
	loser := winner
	loser.TmuxSession = "loser"
	loser.TmuxSocketName = "socket-loser"
	if !inst.ApplyRuntimeState(winner) {
		t.Fatal("winner apply was rejected")
	}
	partial := &session.RestartPartialSuccessError{
		InstanceID: inst.ID, Runtime: loser, NeedsReconciliation: true,
		Err: errors.New("lost runtime CAS"),
	}
	failure, warning := consumeRuntimeResult(inst, loser, partial)
	if failure != nil || warning == "" {
		t.Fatalf("failure=%v warning=%q", failure, warning)
	}
	if got := inst.RuntimeState(); got != winner {
		t.Fatalf("CLI restored loser: got %#v want %#v", got, winner)
	}
}
