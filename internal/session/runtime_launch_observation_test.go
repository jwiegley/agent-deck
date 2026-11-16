package session

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

func TestLaunchRecord_RejectsSameNameReplacement(t *testing.T) {
	storage, inst := piStartFixture(t, "1.3.0", true)
	old := inst.RuntimeState()
	next := old
	next.Generation++
	next.LastStartedAt = time.Now().UTC()
	if err := storage.GetDB().CommitRuntimeTransition(old.Generation, inst.PersistenceIncarnation(), next); err != nil {
		t.Fatal(err)
	}
	loaded := mustLoad(t, storage)
	if len(loaded) != 1 || loaded[0].LaunchedTool != "" || loaded[0].PiSocketExported {
		t.Fatalf("the previous generation's launch decision was trusted: %+v", loaded)
	}
}

func TestLaunchRecord_CanonicalReloadFollowsRuntime(t *testing.T) {
	for _, tc := range []struct {
		from, to string
		exported bool
	}{
		{"pi", "claude", false},
		{"claude", "pi", true},
	} {
		t.Run(tc.from+"-to-"+tc.to, func(t *testing.T) {
			storage, err := NewStorageWithProfile("_test_launch_reload_" + randomString(8))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = storage.Close() })
			inst := NewInstanceWithTool("reload", t.TempDir(), tc.from)
			inst.GroupPath = DefaultGroupPath
			if err := storage.SaveWithGroups([]*Instance{inst}, nil); err != nil {
				t.Fatal(err)
			}
			state := inst.RuntimeState()
			if _, err := storage.GetDB().WriteLaunchDecision(state, inst.PersistenceIncarnation(), tc.from, !tc.exported); err != nil {
				t.Fatal(err)
			}
			canonical := mustLoad(t, storage)[0]
			stale := mustLoad(t, storage)[0]
			next := state
			next.Generation++
			next.LastStartedAt = time.Now().UTC()
			if err := storage.GetDB().CommitRuntimeTransition(state.Generation, inst.PersistenceIncarnation(), next); err != nil {
				t.Fatal(err)
			}
			if _, err := storage.GetDB().WriteLaunchDecision(next, inst.PersistenceIncarnation(), tc.to, tc.exported); err != nil {
				t.Fatal(err)
			}
			incoming := mustLoad(t, storage)[0]
			if !canonical.MergeReloaded(incoming) || canonical.LaunchedTool != tc.to || canonical.PiSocketExported != tc.exported {
				t.Fatalf("canonical launch = %q/%v, want %q/%v", canonical.LaunchedTool, canonical.PiSocketExported, tc.to, tc.exported)
			}
			canonical.MergeReloaded(stale)
			if canonical.LaunchedTool != tc.to || canonical.PiSocketExported != tc.exported || canonical.RuntimeState().Generation != next.Generation {
				t.Fatal("a lower-generation reload changed the accepted launch")
			}
			canonical.mu.Lock()
			canonical.StatusRevision++
			canonical.mu.Unlock()
			if _, err := storage.GetDB().WriteLaunchDecision(next, inst.PersistenceIncarnation(), tc.from, !tc.exported); err != nil {
				t.Fatal(err)
			}
			canonical.MergeReloaded(mustLoad(t, storage)[0])
			if canonical.LaunchedTool != tc.from || canonical.PiSocketExported == tc.exported {
				t.Fatal("a same-generation launch refresh was lost behind a newer status sample")
			}
		})
	}
}

func TestLaunchRecord_RejectsLegacyNameOnlyRecord(t *testing.T) {
	storage, inst := piStartFixture(t, "1.3.0", true)
	if _, err := storage.GetDB().DB().Exec(`UPDATE instances SET tool_data = ? WHERE id = ?`,
		`{"launched_tool":"pi","pi_socket_exported":true,"launch_tmux_session":"`+inst.RuntimeState().TmuxSession+`"}`, inst.ID); err != nil {
		t.Fatal(err)
	}
	loaded := mustLoad(t, storage)[0]
	if loaded.PiSocketExported || loaded.LaunchedTool != "" {
		t.Fatalf("legacy record accepted: %q/%v", loaded.LaunchedTool, loaded.PiSocketExported)
	}
}

func TestLaunchRecord_ReconcilesImmutableLaunchWithoutAnotherSpawn(t *testing.T) {
	for _, stage := range []RuntimeTransitionStage{RuntimeTransitionAfterStampBeforeCommit, RuntimeTransitionAfterCommitBeforeSweep} {
		t.Run(string(stage), func(t *testing.T) {
			installRuntimeLifecycleTestSeams(t)
			db, inst := runtimeLifecycleTestDB(t, "pi", nil)
			authority, winner, err := inst.beginRuntimeTransition(false)
			if err != nil || winner != nil {
				t.Fatalf("begin: winner=%v err=%v", winner, err)
			}
			setRuntimeTestCandidate(inst, "runtime-g1")
			inst.PiSocketExported = true
			stamps := 0
			runtimeCandidateStampFn = func(*tmux.Session, statedb.RuntimeState, string, string) error { stamps++; return nil }
			failure := errors.New("injected publication interruption")
			runtimeTransitionFaultFn = func(at RuntimeTransitionStage, _ statedb.RuntimeState) error {
				if at == stage {
					return failure
				}
				return nil
			}
			candidate, plan, committed, err := inst.commitPhysicalRuntime(authority)
			authority.close()
			if !errors.Is(err, failure) || committed != (stage == RuntimeTransitionAfterCommitBeforeSweep) {
				t.Fatalf("publication: committed=%v err=%v", committed, err)
			}
			partial := &RestartPartialSuccessError{
				InstanceID: inst.ID, Runtime: candidate, BindingPlan: plan,
				LaunchDecision: authority.launchDecision, NeedsReconciliation: !committed, Err: err,
			}
			runtimeCandidateInventoryFn = func(string, string) ([]tmux.RuntimeCandidate, error) {
				return []tmux.RuntimeCandidate{{
					SessionName: candidate.TmuxSession, SocketName: candidate.TmuxSocketName,
					InstanceID: candidate.InstanceID, Generation: candidate.Generation, GenerationKnown: true,
					StatusRevision: candidate.StatusRevision, Status: candidate.Status,
					LastStartedUnixNano: candidate.LastStartedAt.UnixNano(), StateKnown: true,
					BindingKnown: true, BindingKind: authority.launchDecision.BindingKind, PanePID: 4242,
				}}, nil
			}
			inst.Tool, inst.PiSocketExported = "claude", false
			if err := inst.ReconcileRestartResult(partial); err != nil {
				t.Fatal(err)
			}
			row, err := db.LoadInstanceByID(inst.ID)
			if err != nil {
				t.Fatal(err)
			}
			if exported, tool := rowLaunchDecision(row); !exported || tool != "pi" || inst.LaunchedTool != "pi" || !inst.PiSocketExported || stamps != 1 {
				t.Fatalf("reconciled launch: tool=%q exported=%v instance=%q/%v physical stamps=%d", tool, exported, inst.LaunchedTool, inst.PiSocketExported, stamps)
			}
		})
	}
}

func TestLaunchRecord_DelayedPartialCannotClaimItsSuccessor(t *testing.T) {
	installRuntimeLifecycleTestSeams(t)
	db, inst := runtimeLifecycleTestDB(t, "pi", nil)
	authority, winner, err := inst.beginRuntimeTransition(false)
	if err != nil || winner != nil {
		t.Fatalf("begin: winner=%v err=%v", winner, err)
	}
	setRuntimeTestCandidate(inst, "runtime-g1")
	inst.PiSocketExported = true
	candidate, plan, committed, err := inst.commitPhysicalRuntime(authority)
	authority.close()
	if err != nil || !committed {
		t.Fatalf("publish original: committed=%v err=%v", committed, err)
	}
	partial := &RestartPartialSuccessError{
		InstanceID: inst.ID, Runtime: candidate, BindingPlan: plan,
		LaunchDecision: authority.launchDecision, NeedsReconciliation: true,
		Err: errors.New("publication acknowledgement was lost"),
	}
	successor := candidate
	successor.Generation++
	successor.TmuxSession = "runtime-g2"
	successor.LastStartedAt = candidate.LastStartedAt.Add(time.Second)
	runtimeCandidateInventoryFn = func(string, string) ([]tmux.RuntimeCandidate, error) {
		return []tmux.RuntimeCandidate{{
			SessionName: successor.TmuxSession, SocketName: successor.TmuxSocketName,
			InstanceID: successor.InstanceID, Generation: successor.Generation, GenerationKnown: true,
			StatusRevision: successor.StatusRevision, Status: successor.Status,
			LastStartedUnixNano: successor.LastStartedAt.UnixNano(), StateKnown: true,
			BindingKnown: true, PanePID: 4242,
		}}, nil
	}
	if err := inst.ReconcileRestartResult(partial); !errors.Is(err, statedb.ErrRuntimeGenerationConflict) {
		t.Fatalf("delayed partial reconciliation = %v, want generation conflict", err)
	}
	row, err := db.LoadInstanceByID(inst.ID)
	if err != nil || row.RuntimeGeneration != successor.Generation {
		t.Fatalf("successor was not recovered: row=%+v err=%v", row, err)
	}
	if exported, tool := rowLaunchDecision(row); exported || tool != "" {
		t.Fatalf("old launch intent was rebound to successor: %q/%v", tool, exported)
	}
}

func TestLaunchRecord_ReservedNewerWinnerRetainsItsObservation(t *testing.T) {
	for _, lateWriter := range []bool{false, true} {
		t.Run(fmt.Sprintf("late_writer=%v", lateWriter), func(t *testing.T) {
			installRuntimeLifecycleTestSeams(t)
			db, inst := runtimeLifecycleTestDB(t, "pi", nil)
			authority, winner, err := inst.beginRuntimeTransition(false)
			if err != nil || winner != nil {
				t.Fatalf("begin: winner=%v err=%v", winner, err)
			}
			stale := &Instance{ID: inst.ID, Tool: "pi", owningDB: db, persistenceIncarnation: inst.PersistenceIncarnation()}
			stale.adoptRuntimeSnapshot(authority.expected, authority.bindings)
			setRuntimeTestCandidate(inst, "runtime-newer")
			inst.PiSocketExported = true
			candidate, _, committed, err := inst.commitPhysicalRuntime(authority)
			authority.close()
			if err != nil || !committed {
				t.Fatalf("publish: committed=%v err=%v", committed, err)
			}
			if _, err := db.ReserveRuntimeDestruction(candidate, inst.PersistenceIncarnation()); err != nil {
				t.Fatal(err)
			}
			if lateWriter {
				registerLateLegacyRuntimeWriter(t, db)
			}
			runtimeCandidateInventoryFn = func(socket, id string) ([]tmux.RuntimeCandidate, error) {
				if socket != candidate.TmuxSocketName || id != candidate.InstanceID {
					return nil, nil
				}
				return []tmux.RuntimeCandidate{{
					SessionName: candidate.TmuxSession, SocketName: candidate.TmuxSocketName,
					InstanceID: candidate.InstanceID, Generation: candidate.Generation, GenerationKnown: true,
					PanePID: 4242,
				}}, nil
			}
			result, err := stale.ReconcileRuntime()
			if err != nil || !result.Live || !samePhysicalRuntime(result.State, candidate) {
				t.Fatalf("reserved winner recovery: result=%+v err=%v", result, err)
			}
			if stale.LaunchedTool != "pi" || !stale.PiSocketExported {
				t.Fatalf("reserved winner lost launch observation: %q/%v", stale.LaunchedTool, stale.PiSocketExported)
			}
		})
	}
}
