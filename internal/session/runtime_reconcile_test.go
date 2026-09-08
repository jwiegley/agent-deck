package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

func runtimeLifecycleTestDB(t *testing.T, tool string, toolData json.RawMessage) (*statedb.StateDB, *Instance) {
	t.Helper()
	db, err := statedb.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	if toolData == nil {
		toolData = json.RawMessage(`{}`)
	}
	parent := &statedb.InstanceRow{
		ID: "one", Title: "one", ProjectPath: "/tmp/one", GroupPath: "my-sessions",
		Tool: tool, Status: "idle", TmuxSession: "runtime-g0", TmuxSocketName: "isolated",
		CreatedAt: time.Unix(1, 0).UTC(), ToolData: toolData,
	}
	if err := db.SaveInstance(parent); err != nil {
		t.Fatal(err)
	}
	inst := &Instance{
		ID: "one", Title: "one", ProjectPath: "/tmp/one", GroupPath: "my-sessions",
		Tool: tool, Status: StatusIdle, TmuxSocketName: "isolated", owningDB: db,
		tmuxSession: &tmux.Session{Name: "runtime-g0", SocketName: "isolated", InstanceID: "one"},
	}
	inst.adoptPersistenceIncarnation(parent.Incarnation)
	return db, inst
}

func installRuntimeLifecycleTestSeams(t *testing.T) {
	t.Helper()
	oldAcquire := instanceSpawnLockAcquireFn
	oldObserved := runtimeTransitionObservedFn
	oldFault := runtimeTransitionFaultFn
	oldInventory := runtimeCandidateInventoryFn
	oldRevalidate := runtimeCandidateRevalidateFn
	oldExists := runtimeCandidateExistsFn
	oldStamp := runtimeCandidateStampFn
	oldSetEnv := runtimeCandidateSetEnvFn
	oldCleanupStamp := runtimeCleanupIdentityStampFn
	oldRespawn := runtimeCandidateRespawnFn
	oldCommit := runtimeTransitionCommitFn
	oldSweep := runtimeDuplicateSweepFn
	oldGenerationKill := killRuntimeGenerationCandidateFn
	oldNow := nowFn

	lock := make(chan struct{}, 1)
	lock <- struct{}{}
	instanceSpawnLockAcquireFn = func(string) (func(), error) {
		<-lock
		return func() { lock <- struct{}{} }, nil
	}
	runtimeTransitionObservedFn = func() {}
	runtimeTransitionFaultFn = func(RuntimeTransitionStage, statedb.RuntimeState) error { return nil }
	runtimeCandidateInventoryFn = func(string, string) ([]tmux.RuntimeCandidate, error) { return nil, nil }
	runtimeCandidateRevalidateFn = func(candidate tmux.RuntimeCandidate) (tmux.RuntimeCandidate, error) { return candidate, nil }
	runtimeCandidateExistsFn = func(*tmux.Session) bool { return true }
	runtimeCandidateStampFn = func(*tmux.Session, statedb.RuntimeState, string, string) error { return nil }
	runtimeCandidateSetEnvFn = func(*tmux.Session, string, string) error { return nil }
	runtimeCleanupIdentityStampFn = func(*tmux.Session, string, uint64, string, string) error { return nil }
	runtimeCandidateRespawnFn = func(*tmux.Session, tmux.RuntimeGenerationCandidate, string) error { return nil }
	runtimeTransitionCommitFn = func(db *statedb.StateDB, expected uint64, incarnation string, next statedb.RuntimeState, plan []statedb.RuntimeBindingTransition) error {
		return db.CommitRuntimeTransitionWithBindingPlan(expected, incarnation, next, plan)
	}
	runtimeDuplicateSweepFn = func(*Instance, ...string) {}
	killRuntimeGenerationCandidateFn = func(tmux.RuntimeGenerationCandidate, bool) error { return nil }
	nowFn = func() time.Time { return time.Unix(100, 123).UTC() }

	t.Cleanup(func() {
		instanceSpawnLockAcquireFn = oldAcquire
		runtimeTransitionObservedFn = oldObserved
		runtimeTransitionFaultFn = oldFault
		runtimeCandidateInventoryFn = oldInventory
		runtimeCandidateRevalidateFn = oldRevalidate
		runtimeCandidateExistsFn = oldExists
		runtimeCandidateStampFn = oldStamp
		runtimeCandidateSetEnvFn = oldSetEnv
		runtimeCleanupIdentityStampFn = oldCleanupStamp
		runtimeCandidateRespawnFn = oldRespawn
		runtimeTransitionCommitFn = oldCommit
		runtimeDuplicateSweepFn = oldSweep
		killRuntimeGenerationCandidateFn = oldGenerationKill
		nowFn = oldNow
	})
}

func setRuntimeTestCandidate(i *Instance, name string) {
	i.mu.Lock()
	i.tmuxSession = &tmux.Session{Name: name, SocketName: "isolated", InstanceID: i.ID}
	i.TmuxSocketName = "isolated"
	i.Status = StatusStarting
	i.mu.Unlock()
}

func TestRuntimeLifecycle_MissingRuntimeBypassesParentGuardOnlyForNewUnsavedInstance(t *testing.T) {
	db, err := statedb.Open(filepath.Join(t.TempDir(), "new-runtime.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	previousGlobal := statedb.GetGlobal()
	statedb.SetGlobal(db)
	t.Cleanup(func() { statedb.SetGlobal(previousGlobal) })

	newUnsaved := NewInstanceWithTool("new-unsaved", "/tmp/new-unsaved", "pi")
	if newUnsaved.owningDB != nil || !newUnsaved.addedThisProcess {
		t.Fatalf("new-unsaved precondition owningDB=%p added=%v", newUnsaved.owningDB, newUnsaved.addedThisProcess)
	}
	state, found, err := newUnsaved.durableRuntimeState()
	if err != nil || !found || state.InstanceID != newUnsaved.ID {
		t.Fatalf("new-unsaved runtime state=%#v found=%v err=%v", state, found, err)
	}

	persisted := NewInstanceWithTool("persisted-deleted", "/tmp/persisted", "pi")
	storage := &Storage{db: db}
	if err := storage.InsertSessionAndVerify(persisted, nil); err != nil {
		t.Fatal(err)
	}
	persistedID := persisted.ID
	persisted.mu.RLock()
	owner, added := persisted.owningDB, persisted.addedThisProcess
	persisted.mu.RUnlock()
	if owner != db || !added {
		t.Fatalf("saved constructor instance precondition owningDB=%p added=%v", owner, added)
	}
	if err := db.DeleteInstance(persistedID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := persisted.durableRuntimeState(); !errors.Is(err, statedb.ErrInstanceParentConflict) {
		t.Fatalf("persisted-deleted runtime error = %v, want parent incarnation conflict", err)
	}
	if _, found, err := db.ReadRuntimeState(persistedID); err != nil || found {
		t.Fatalf("persisted-deleted runtime was recreated: found=%v err=%v", found, err)
	}
}

func TestRuntimeLifecycle_ConcurrentTransitionLoserAdoptsWinner(t *testing.T) {
	installRuntimeLifecycleTestSeams(t)
	db, first := runtimeLifecycleTestDB(t, "pi", nil)
	second := &Instance{
		ID: first.ID, Title: first.Title, ProjectPath: first.ProjectPath, GroupPath: first.GroupPath,
		Tool: first.Tool, Status: first.Status, TmuxSocketName: first.TmuxSocketName, owningDB: db,
		tmuxSession: &tmux.Session{Name: "runtime-g0", SocketName: "isolated", InstanceID: first.ID},
	}
	second.adoptPersistenceIncarnation(first.PersistenceIncarnation())

	observed := make(chan struct{}, 2)
	continueAfterObservation := make(chan struct{})
	runtimeTransitionObservedFn = func() {
		observed <- struct{}{}
		<-continueAfterObservation
	}
	spawnCount, stampCount, sweepCount := 0, 0, 0
	runtimeCandidateStampFn = func(*tmux.Session, statedb.RuntimeState, string, string) error {
		stampCount++
		return nil
	}
	runtimeDuplicateSweepFn = func(*Instance, ...string) { sweepCount++ }

	type transitionResult struct {
		winner bool
		err    error
	}
	results := make(chan transitionResult, 2)
	run := func(i *Instance, name string) {
		authority, winner, err := i.beginRuntimeTransition(false)
		if err != nil || winner != nil {
			results <- transitionResult{winner: winner != nil, err: err}
			return
		}
		defer authority.close()
		spawnCount++
		setRuntimeTestCandidate(i, name)
		_, _, _, err = i.commitPhysicalRuntime(authority)
		if err == nil {
			runtimeDuplicateSweepFn(i)
		}
		results <- transitionResult{err: err}
	}
	go run(first, "runtime-first")
	go run(second, "runtime-second")
	<-observed
	<-observed
	close(continueAfterObservation)

	one, two := <-results, <-results
	if one.err != nil || two.err != nil {
		t.Fatalf("transition errors: %v, %v", one.err, two.err)
	}
	if one.winner == two.winner {
		t.Fatalf("winner adoption flags = %v, %v; want exactly one loser", one.winner, two.winner)
	}
	if spawnCount != 1 || stampCount != 1 || sweepCount != 1 {
		t.Fatalf("spawn=%d stamp=%d sweep=%d; lock loser performed work", spawnCount, stampCount, sweepCount)
	}
	state, found, err := db.ReadRuntimeState("one")
	if err != nil || !found || state.Generation != 1 {
		t.Fatalf("durable state=%#v found=%v err=%v", state, found, err)
	}
}

func TestRuntimeLifecycle_PersistenceFailureRetriesDurabilityOnly(t *testing.T) {
	installRuntimeLifecycleTestSeams(t)
	db, inst := runtimeLifecycleTestDB(t, "pi", nil)
	spawnCount, sweepCount := 0, 0
	runtimeDuplicateSweepFn = func(*Instance, ...string) { sweepCount++ }
	authority, winner, err := inst.beginRuntimeTransition(false)
	if err != nil || winner != nil {
		t.Fatalf("begin winner=%v err=%v", winner, err)
	}
	spawnCount++
	setRuntimeTestCandidate(inst, "runtime-g1")
	persistFailure := errors.New("injected transaction failure")
	runtimeTransitionCommitFn = func(*statedb.StateDB, uint64, string, statedb.RuntimeState, []statedb.RuntimeBindingTransition) error {
		return persistFailure
	}
	candidate, plan, committed, err := inst.commitPhysicalRuntime(authority)
	authority.close()
	if !errors.Is(err, persistFailure) || committed {
		t.Fatalf("candidate=%#v committed=%v err=%v", candidate, committed, err)
	}
	state, _, _ := db.ReadRuntimeState("one")
	if state.Generation != 0 || sweepCount != 0 {
		t.Fatalf("failed transaction left generation=%d sweep=%d", state.Generation, sweepCount)
	}

	runtimeTransitionCommitFn = func(db *statedb.StateDB, expected uint64, incarnation string, next statedb.RuntimeState, plan []statedb.RuntimeBindingTransition) error {
		return db.CommitRuntimeTransitionWithBindingPlan(expected, incarnation, next, plan)
	}
	runtimeCandidateInventoryFn = func(string, string) ([]tmux.RuntimeCandidate, error) {
		return []tmux.RuntimeCandidate{{
			SessionName: candidate.TmuxSession, SocketName: candidate.TmuxSocketName,
			InstanceID: candidate.InstanceID, Generation: candidate.Generation,
			GenerationKnown: true, PanePID: 4242,
			StatusRevision: candidate.StatusRevision, Status: candidate.Status,
			LastStartedUnixNano: candidate.LastStartedAt.UnixNano(), StateKnown: true,
			BindingKnown: true,
		}}, nil
	}
	partial := &RestartPartialSuccessError{
		InstanceID: inst.ID, Runtime: candidate, BindingPlan: plan,
		NeedsReconciliation: true, Err: persistFailure,
	}
	if err := inst.ReconcileRestartResult(partial); err != nil {
		t.Fatal(err)
	}
	state, _, _ = db.ReadRuntimeState("one")
	if state.Generation != 1 || spawnCount != 1 || sweepCount != 1 {
		t.Fatalf("generation=%d spawn=%d sweep=%d", state.Generation, spawnCount, sweepCount)
	}
}

func TestRuntimeLifecycle_ReconcileRecoversReservedLiveRuntime(t *testing.T) {
	for _, priorStatus := range []Status{StatusIdle, StatusWaiting, StatusRunning} {
		t.Run(string(priorStatus), func(t *testing.T) {
			installRuntimeLifecycleTestSeams(t)
			db, inst := runtimeLifecycleTestDB(t, "pi", nil)
			durable, found, err := db.ReadRuntimeState(inst.ID)
			if err != nil || !found {
				t.Fatalf("read durable: state=%#v found=%v err=%v", durable, found, err)
			}
			if string(priorStatus) != durable.Status {
				applied, err := db.WriteStatusIfVersion(inst.ID, inst.PersistenceIncarnation(),
					durable.Generation, durable.StatusRevision, string(priorStatus))
				if err != nil || !applied {
					t.Fatalf("set prior status %q: applied=%v err=%v", priorStatus, applied, err)
				}
				durable, found, err = db.ReadRuntimeState(inst.ID)
				if err != nil || !found {
					t.Fatalf("read updated durable: state=%#v found=%v err=%v", durable, found, err)
				}
				inst.ApplyRuntimeState(durable)
			}
			claimed, err := db.ReserveRuntimeDestruction(durable, inst.PersistenceIncarnation())
			if err != nil {
				t.Fatal(err)
			}
			candidate := tmux.RuntimeCandidate{
				SessionID: "$1", SessionName: claimed.TmuxSession, SocketName: claimed.TmuxSocketName,
				PaneID: "%1", PanePID: 4242, InstanceID: claimed.InstanceID,
				Generation: claimed.Generation, GenerationKnown: true,
			}
			runtimeCandidateInventoryFn = func(socketName, instanceID string) ([]tmux.RuntimeCandidate, error) {
				if socketName == candidate.SocketName && instanceID == candidate.InstanceID {
					return []tmux.RuntimeCandidate{candidate}, nil
				}
				return nil, nil
			}
			result, err := inst.ReconcileRuntime()
			if err != nil {
				t.Fatal(err)
			}
			want := claimed
			want.Status = string(priorStatus)
			want.StatusRevision++
			if result.State != want || !result.Live || result.Adopted {
				t.Fatalf("reconciliation result=%#v, want live recovered state %#v", result, want)
			}
			if got := inst.RuntimeState(); got != want {
				t.Fatalf("canonical state=%#v, want %#v", got, want)
			}
			got, found, err := db.ReadRuntimeState(inst.ID)
			if err != nil || !found || got != want {
				t.Fatalf("durable state=%#v found=%v err=%v, want %#v", got, found, err, want)
			}
			if row, err := db.LoadInstanceByID(inst.ID); err != nil || row == nil || row.Status != string(priorStatus) {
				t.Fatalf("reserved recovery parent=%#v err=%v, want status %q", row, err, priorStatus)
			}
		})
	}
}

func registerLateLegacyRuntimeWriter(t *testing.T, db *statedb.StateDB) {
	t.Helper()
	legacyPID := os.Getppid()
	now := time.Now().Unix()
	if _, err := db.DB().Exec(`INSERT OR REPLACE INTO instance_heartbeats
		(pid, started, heartbeat, is_primary) VALUES (?, ?, ?, 0)`, legacyPID, now, now); err != nil {
		t.Fatalf("register late legacy writer: %v", err)
	}
	if err := db.RequireRuntimeWriterCompatibility(); !errors.Is(err, statedb.ErrIncompatibleWriterSchema) {
		t.Fatalf("writer compatibility error=%v, want ErrIncompatibleWriterSchema", err)
	}
}

func TestRuntimeLifecycle_ReservedRecoveryBypassesLateLegacyWriter(t *testing.T) {
	for _, entrypoint := range []string{"direct", "snapshot"} {
		for _, live := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/live=%t", entrypoint, live), func(t *testing.T) {
				installRuntimeLifecycleTestSeams(t)
				db, inst := runtimeLifecycleTestDB(t, "pi", nil)
				durable, found, err := db.ReadRuntimeState(inst.ID)
				if err != nil || !found {
					t.Fatalf("read durable: state=%#v found=%v err=%v", durable, found, err)
				}
				claimed, err := db.ReserveRuntimeDestruction(durable, inst.PersistenceIncarnation())
				if err != nil {
					t.Fatal(err)
				}
				registerLateLegacyRuntimeWriter(t, db)

				candidate := tmux.RuntimeCandidate{
					SessionID: "$1", SessionName: claimed.TmuxSession, SocketName: claimed.TmuxSocketName,
					PaneID: "%1", PanePID: 4242, InstanceID: claimed.InstanceID,
					Generation: claimed.Generation, GenerationKnown: true,
				}
				runtimeCandidateInventoryFn = func(socketName, instanceID string) ([]tmux.RuntimeCandidate, error) {
					if live && socketName == candidate.SocketName && instanceID == candidate.InstanceID {
						return []tmux.RuntimeCandidate{candidate}, nil
					}
					return nil, nil
				}

				var result RuntimeReconciliationResult
				if entrypoint == "direct" {
					result, err = inst.ReconcileRuntime()
				} else {
					snapshot := make(tmux.RuntimeCandidateSnapshot)
					for _, socketName := range []string{
						claimed.TmuxSocketName, inst.TmuxSocketName, tmux.DefaultSocketName(), "",
					} {
						snapshot[socketName] = tmux.RuntimeCandidateSocketSnapshot{
							CandidatesByInstance: map[string][]tmux.RuntimeCandidate{},
						}
					}
					if live {
						socketSnapshot := snapshot[claimed.TmuxSocketName]
						socketSnapshot.CandidatesByInstance[claimed.InstanceID] = []tmux.RuntimeCandidate{candidate}
						snapshot[claimed.TmuxSocketName] = socketSnapshot
					}
					result, err = inst.ReconcileRuntimeFromSnapshot(snapshot)
				}
				if err != nil {
					t.Fatal(err)
				}
				wantStatus := string(StatusStopped)
				if live {
					wantStatus = durable.Status
				}
				if result.State.Status != wantStatus || result.State.StatusRevision != claimed.StatusRevision+1 || result.Live != live {
					t.Fatalf("result=%#v, want status=%q revision=%d live=%v",
						result, wantStatus, claimed.StatusRevision+1, live)
				}
			})
		}
	}
}

func TestRuntimeLifecycle_TransitionRecoversReservationBeforeLateWriterFence(t *testing.T) {
	for _, live := range []bool{false, true} {
		t.Run(fmt.Sprintf("live=%t", live), func(t *testing.T) {
			installRuntimeLifecycleTestSeams(t)
			db, inst := runtimeLifecycleTestDB(t, "pi", nil)
			durable, found, err := db.ReadRuntimeState(inst.ID)
			if err != nil || !found {
				t.Fatalf("read durable: state=%#v found=%v err=%v", durable, found, err)
			}
			claimed, err := db.ReserveRuntimeDestruction(durable, inst.PersistenceIncarnation())
			if err != nil {
				t.Fatal(err)
			}
			registerLateLegacyRuntimeWriter(t, db)

			candidate := tmux.RuntimeCandidate{
				SessionID: "$1", SessionName: claimed.TmuxSession, SocketName: claimed.TmuxSocketName,
				PaneID: "%1", PanePID: 4242, InstanceID: claimed.InstanceID,
				Generation: claimed.Generation, GenerationKnown: true,
			}
			runtimeCandidateInventoryFn = func(socketName, instanceID string) ([]tmux.RuntimeCandidate, error) {
				if live && socketName == candidate.SocketName && instanceID == candidate.InstanceID {
					return []tmux.RuntimeCandidate{candidate}, nil
				}
				return nil, nil
			}

			authority, winner, err := inst.beginRuntimeTransition(false)
			if authority != nil {
				authority.close()
			}
			if !errors.Is(err, statedb.ErrIncompatibleWriterSchema) || winner != nil {
				t.Fatalf("beginRuntimeTransition authority=%#v winner=%#v err=%v", authority, winner, err)
			}
			got, found, readErr := db.ReadRuntimeState(inst.ID)
			wantStatus := string(StatusStopped)
			if live {
				wantStatus = durable.Status
			}
			if readErr != nil || !found || statedb.IsRuntimeDestructionReserved(got) ||
				got.Status != wantStatus || got.StatusRevision != claimed.StatusRevision+1 {
				t.Fatalf("post-fence runtime=%#v found=%v err=%v want status=%q revision=%d",
					got, found, readErr, wantStatus, claimed.StatusRevision+1)
			}
			var provenanceRows int
			if err := db.DB().QueryRow(`SELECT COUNT(*) FROM instance_runtime_destruction WHERE instance_id = ?`, inst.ID).
				Scan(&provenanceRows); err != nil || provenanceRows != 0 {
				t.Fatalf("transition stranded provenance: rows=%d err=%v", provenanceRows, err)
			}
		})
	}
}

func TestRuntimeLifecycle_TransitionRecoversReservationPublishedAfterObservation(t *testing.T) {
	for _, live := range []bool{false, true} {
		t.Run(fmt.Sprintf("live=%t", live), func(t *testing.T) {
			installRuntimeLifecycleTestSeams(t)
			db, inst := runtimeLifecycleTestDB(t, "pi", nil)
			durable, found, err := db.ReadRuntimeState(inst.ID)
			if err != nil || !found {
				t.Fatalf("read durable: state=%#v found=%v err=%v", durable, found, err)
			}

			var claimed statedb.RuntimeState
			oldObserved := runtimeTransitionObservedFn
			runtimeTransitionObservedFn = func() {
				var reserveErr error
				claimed, reserveErr = db.ReserveRuntimeDestruction(durable, inst.PersistenceIncarnation())
				if reserveErr != nil {
					t.Fatal(reserveErr)
				}
				registerLateLegacyRuntimeWriter(t, db)
			}
			t.Cleanup(func() { runtimeTransitionObservedFn = oldObserved })

			runtimeCandidateInventoryFn = func(socketName, instanceID string) ([]tmux.RuntimeCandidate, error) {
				if !live || claimed.InstanceID == "" || socketName != claimed.TmuxSocketName || instanceID != claimed.InstanceID {
					return nil, nil
				}
				return []tmux.RuntimeCandidate{{
					SessionID: "$1", SessionName: claimed.TmuxSession, SocketName: claimed.TmuxSocketName,
					PaneID: "%1", PanePID: 4242, InstanceID: claimed.InstanceID,
					Generation: claimed.Generation, GenerationKnown: true,
				}}, nil
			}

			authority, winner, err := inst.beginRuntimeTransition(false)
			if authority != nil {
				authority.close()
			}
			if !errors.Is(err, statedb.ErrIncompatibleWriterSchema) || winner != nil {
				t.Fatalf("beginRuntimeTransition authority=%#v winner=%#v err=%v", authority, winner, err)
			}
			got, found, readErr := db.ReadRuntimeState(inst.ID)
			wantStatus := string(StatusStopped)
			if live {
				wantStatus = durable.Status
			}
			if readErr != nil || !found || statedb.IsRuntimeDestructionReserved(got) ||
				got.Status != wantStatus || got.StatusRevision != claimed.StatusRevision+1 {
				t.Fatalf("post-fence runtime=%#v found=%v err=%v want status=%q revision=%d",
					got, found, readErr, wantStatus, claimed.StatusRevision+1)
			}
			var provenanceRows int
			if err := db.DB().QueryRow(`SELECT COUNT(*) FROM instance_runtime_destruction WHERE instance_id = ?`, inst.ID).
				Scan(&provenanceRows); err != nil || provenanceRows != 0 {
				t.Fatalf("transition stranded provenance: rows=%d err=%v", provenanceRows, err)
			}
		})
	}
}

func TestRuntimeLifecycle_ReservedRecoveryHelperFencesOnlyAfterRecovery(t *testing.T) {
	for _, live := range []bool{false, true} {
		t.Run(fmt.Sprintf("live=%t", live), func(t *testing.T) {
			installRuntimeLifecycleTestSeams(t)
			db, inst := runtimeLifecycleTestDB(t, "pi", nil)
			durable, found, err := db.ReadRuntimeState(inst.ID)
			if err != nil || !found {
				t.Fatalf("read durable: state=%#v found=%v err=%v", durable, found, err)
			}
			claimed, err := db.ReserveRuntimeDestruction(durable, inst.PersistenceIncarnation())
			if err != nil {
				t.Fatal(err)
			}
			registerLateLegacyRuntimeWriter(t, db)
			runtimeCandidateInventoryFn = func(socketName, instanceID string) ([]tmux.RuntimeCandidate, error) {
				if !live || socketName != claimed.TmuxSocketName || instanceID != claimed.InstanceID {
					return nil, nil
				}
				return []tmux.RuntimeCandidate{{
					SessionID: "$1", SessionName: claimed.TmuxSession, SocketName: claimed.TmuxSocketName,
					PaneID: "%1", PanePID: 4242, InstanceID: claimed.InstanceID,
					Generation: claimed.Generation, GenerationKnown: true,
				}}, nil
			}

			_, _, err = inst.reconcileReservedRuntimeBeforeCompatibilityLocked(
				db, claimed, true, inst.PersistenceIncarnation())
			if !errors.Is(err, statedb.ErrIncompatibleWriterSchema) {
				t.Fatalf("recovery helper error=%v, want ErrIncompatibleWriterSchema", err)
			}
			got, found, readErr := db.ReadRuntimeState(inst.ID)
			wantStatus := string(StatusStopped)
			if live {
				wantStatus = durable.Status
			}
			if readErr != nil || !found || statedb.IsRuntimeDestructionReserved(got) ||
				got.Status != wantStatus || got.StatusRevision != claimed.StatusRevision+1 {
				t.Fatalf("post-fence runtime=%#v found=%v err=%v want status=%q revision=%d",
					got, found, readErr, wantStatus, claimed.StatusRevision+1)
			}
		})
	}
}

func TestRuntimeLifecycle_ReconcileRecoversReservedMissingRuntimeAsStopped(t *testing.T) {
	installRuntimeLifecycleTestSeams(t)
	db, inst := runtimeLifecycleTestDB(t, "pi", nil)
	durable, found, err := db.ReadRuntimeState(inst.ID)
	if err != nil || !found {
		t.Fatalf("read durable: state=%#v found=%v err=%v", durable, found, err)
	}
	claimed, err := db.ReserveRuntimeDestruction(durable, inst.PersistenceIncarnation())
	if err != nil {
		t.Fatal(err)
	}
	result, err := inst.ReconcileRuntime()
	if err != nil {
		t.Fatal(err)
	}
	want := claimed
	want.Status = string(StatusStopped)
	want.StatusRevision++
	if result.State != want || result.Live || result.Adopted {
		t.Fatalf("reconciliation result=%#v, want stopped recovered state %#v", result, want)
	}
	if got := inst.RuntimeState(); got != want {
		t.Fatalf("canonical state=%#v, want %#v", got, want)
	}
	if row, err := db.LoadInstanceByID(inst.ID); err != nil || row == nil {
		t.Fatalf("reserved recovery removed logical row: row=%#v err=%v", row, err)
	}
}

func TestRuntimeLifecycle_ReconcilePreservesReservedAmbiguity(t *testing.T) {
	installRuntimeLifecycleTestSeams(t)
	db, inst := runtimeLifecycleTestDB(t, "pi", nil)
	durable, found, err := db.ReadRuntimeState(inst.ID)
	if err != nil || !found {
		t.Fatalf("read durable: state=%#v found=%v err=%v", durable, found, err)
	}
	claimed, err := db.ReserveRuntimeDestruction(durable, inst.PersistenceIncarnation())
	if err != nil {
		t.Fatal(err)
	}
	next := tmux.RuntimeCandidate{
		SessionID: "$2", SessionName: "runtime-g1", SocketName: claimed.TmuxSocketName,
		PaneID: "%2", PanePID: 5252, InstanceID: claimed.InstanceID,
		Generation: claimed.Generation + 1, GenerationKnown: true,
		Status: string(StatusStarting), StateKnown: true, BindingKnown: true,
	}
	runtimeCandidateInventoryFn = func(socketName, instanceID string) ([]tmux.RuntimeCandidate, error) {
		if socketName == next.SocketName && instanceID == next.InstanceID {
			return []tmux.RuntimeCandidate{next}, nil
		}
		return nil, nil
	}
	_, err = inst.ReconcileRuntime()
	var ambiguity *RuntimeReconciliationAmbiguityError
	if !errors.As(err, &ambiguity) {
		t.Fatalf("reconciliation error=%v, want ambiguity", err)
	}
	got, found, readErr := db.ReadRuntimeState(inst.ID)
	if readErr != nil || !found || got != claimed {
		t.Fatalf("ambiguous recovery changed sentinel: state=%#v found=%v err=%v want=%#v",
			got, found, readErr, claimed)
	}
}

func TestRuntimeLifecycle_BeginTransitionUsesRecoveredReservationRevision(t *testing.T) {
	installRuntimeLifecycleTestSeams(t)
	db, inst := runtimeLifecycleTestDB(t, "pi", nil)
	durable, found, err := db.ReadRuntimeState(inst.ID)
	if err != nil || !found {
		t.Fatalf("read durable: state=%#v found=%v err=%v", durable, found, err)
	}
	claimed, err := db.ReserveRuntimeDestruction(durable, inst.PersistenceIncarnation())
	if err != nil {
		t.Fatal(err)
	}
	authority, winner, err := inst.beginRuntimeTransition(false)
	if err != nil || winner != nil || authority == nil {
		t.Fatalf("begin authority=%#v winner=%#v err=%v", authority, winner, err)
	}
	defer authority.close()
	if authority.expected.Status != string(StatusStopped) ||
		authority.expected.StatusRevision != claimed.StatusRevision+1 {
		t.Fatalf("authority retained reserved revision: %#v", authority.expected)
	}
}

func TestRuntimeLifecycle_PartialSuccessRejectsAnyAmbiguousInventory(t *testing.T) {
	installRuntimeLifecycleTestSeams(t)
	inst := &Instance{ID: "one", Tool: "pi", Status: StatusStarting}
	durable := statedb.RuntimeState{
		InstanceID: "one", Generation: 0, TmuxSession: "runtime-g0",
		TmuxSocketName: "isolated", Status: "idle",
	}
	expected := statedb.RuntimeState{
		InstanceID: "one", Generation: 1, StatusRevision: 0,
		TmuxSession: "runtime-g1", TmuxSocketName: "isolated", Status: "starting",
		LastStartedAt: time.Unix(100, 123).UTC(),
	}
	target := tmux.RuntimeCandidate{
		SessionID: "$2", SessionName: expected.TmuxSession, SocketName: expected.TmuxSocketName,
		PaneID: "%2", PanePID: 4242, InstanceID: expected.InstanceID,
		Generation: expected.Generation, GenerationKnown: true,
		StatusRevision: expected.StatusRevision, Status: expected.Status,
		LastStartedUnixNano: expected.LastStartedAt.UnixNano(), StateKnown: true,
		BindingKnown: true,
	}

	tests := []struct {
		name       string
		candidates []tmux.RuntimeCandidate
	}{
		{
			name: "incomplete lower generation",
			candidates: []tmux.RuntimeCandidate{
				target,
				{SessionID: "$1", SessionName: "runtime-old", SocketName: "isolated", PaneID: "%1", PanePID: 3131, InstanceID: "one"},
			},
		},
		{
			name: "duplicate next generation",
			candidates: []tmux.RuntimeCandidate{
				target,
				{SessionID: "$3", SessionName: "runtime-other", SocketName: "isolated", PaneID: "%3", PanePID: 4343, InstanceID: "one", Generation: 1, GenerationKnown: true},
			},
		},
		{
			name: "newer inadmissible generation",
			candidates: []tmux.RuntimeCandidate{
				target,
				{SessionID: "$4", SessionName: "runtime-future", SocketName: "isolated", PaneID: "%4", PanePID: 4444, InstanceID: "one", Generation: 2, GenerationKnown: true},
			},
		},
		{
			name: "same generation identity conflict",
			candidates: []tmux.RuntimeCandidate{
				target,
				{SessionID: "$5", SessionName: "runtime-wrong-g0", SocketName: "isolated", PaneID: "%5", PanePID: 4545, InstanceID: "one", GenerationKnown: true},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := inst.validatePartialRuntimeCandidate(durable, expected, nil, test.candidates)
			var ambiguity *RuntimeReconciliationAmbiguityError
			if !errors.As(err, &ambiguity) || len(ambiguity.Candidates) != len(test.candidates) {
				t.Fatalf("ambiguous inventory error = %v", err)
			}
		})
	}
}

func TestRuntimeLifecycle_PostCommitReconcileRequiresLiveWinner(t *testing.T) {
	inst := &Instance{ID: "one", Tool: "pi", Status: StatusStarting}
	interrupted := errors.New("cleanup interrupted")
	partial := &RestartPartialSuccessError{
		InstanceID:          "one",
		Runtime:             statedb.RuntimeState{InstanceID: "one", Generation: 1, TmuxSession: "runtime-g1"},
		NeedsReconciliation: false,
		Err:                 interrupted,
	}
	err := inst.ReconcileRestartResult(partial)
	var unresolved *RestartPartialSuccessError
	if !errors.As(err, &unresolved) || unresolved.NeedsReconciliation || !errors.Is(err, interrupted) {
		t.Fatalf("post-commit no-winner error = %#v", err)
	}
	if !strings.Contains(err.Error(), "no live durable runtime winner") {
		t.Fatalf("post-commit no-winner error lacks evidence: %v", err)
	}
}

func TestRuntimeLifecycle_PreCommitCrashAdoptsUniqueCandidate(t *testing.T) {
	installRuntimeLifecycleTestSeams(t)
	db, inst := runtimeLifecycleTestDB(t, "pi", nil)
	authority, winner, err := inst.beginRuntimeTransition(false)
	if err != nil || winner != nil {
		t.Fatalf("begin winner=%v err=%v", winner, err)
	}
	setRuntimeTestCandidate(inst, "runtime-g1")
	crash := errors.New("crash after stamp")
	runtimeTransitionFaultFn = func(stage RuntimeTransitionStage, _ statedb.RuntimeState) error {
		if stage == RuntimeTransitionAfterStampBeforeCommit {
			return crash
		}
		return nil
	}
	candidate, _, committed, err := inst.commitPhysicalRuntime(authority)
	authority.close()
	if !errors.Is(err, crash) || committed {
		t.Fatalf("committed=%v err=%v", committed, err)
	}
	runtimeTransitionFaultFn = func(RuntimeTransitionStage, statedb.RuntimeState) error { return nil }
	runtimeCandidateInventoryFn = func(string, string) ([]tmux.RuntimeCandidate, error) {
		return []tmux.RuntimeCandidate{{
			SessionName: candidate.TmuxSession, SocketName: candidate.TmuxSocketName,
			InstanceID: candidate.InstanceID, Generation: candidate.Generation, GenerationKnown: true,
			StatusRevision: candidate.StatusRevision, Status: candidate.Status,
			LastStartedUnixNano: candidate.LastStartedAt.UnixNano(), StateKnown: true,
			BindingKnown: true, PanePID: 4242,
		}}, nil
	}
	sweepCount := 0
	runtimeDuplicateSweepFn = func(*Instance, ...string) { sweepCount++ }
	restarted := &Instance{ID: "one", Title: "one", ProjectPath: "/tmp/one", Tool: "pi", owningDB: db}
	restarted.adoptPersistenceIncarnation(inst.PersistenceIncarnation())
	result, err := restarted.ReconcileRuntime()
	if err != nil {
		t.Fatal(err)
	}
	if !result.Live || !result.Adopted || result.State.Generation != 1 || sweepCount != 1 {
		t.Fatalf("result=%#v sweep=%d", result, sweepCount)
	}
}

func TestRuntimeLifecycle_PostCommitCrashRetainsWinnerAndSweepsLower(t *testing.T) {
	installRuntimeLifecycleTestSeams(t)
	db, inst := runtimeLifecycleTestDB(t, "pi", nil)
	authority, winner, err := inst.beginRuntimeTransition(false)
	if err != nil || winner != nil {
		t.Fatalf("begin winner=%v err=%v", winner, err)
	}
	setRuntimeTestCandidate(inst, "runtime-g1")
	crash := errors.New("crash after commit")
	runtimeTransitionFaultFn = func(stage RuntimeTransitionStage, _ statedb.RuntimeState) error {
		if stage == RuntimeTransitionAfterCommitBeforeSweep {
			return crash
		}
		return nil
	}
	candidate, _, committed, err := inst.commitPhysicalRuntime(authority)
	authority.close()
	if !errors.Is(err, crash) || !committed {
		t.Fatalf("committed=%v err=%v", committed, err)
	}
	runtimeTransitionFaultFn = func(RuntimeTransitionStage, statedb.RuntimeState) error { return nil }
	lowerSocket := "old-socket"
	runtimeCandidateInventoryFn = func(socketName, _ string) ([]tmux.RuntimeCandidate, error) {
		switch socketName {
		case candidate.TmuxSocketName:
			return []tmux.RuntimeCandidate{{
				SessionName: candidate.TmuxSession, SocketName: candidate.TmuxSocketName,
				InstanceID: "one", Generation: 1, GenerationKnown: true, PanePID: 4242,
			}}, nil
		case lowerSocket:
			return []tmux.RuntimeCandidate{{
				SessionID: "$1", SessionName: candidate.TmuxSession, SocketName: lowerSocket, PaneID: "%1",
				InstanceID: "one", Generation: 0, GenerationKnown: true, PanePID: 3131,
			}}, nil
		default:
			return nil, nil
		}
	}
	sweepCount := 0
	var sweepSockets []string
	runtimeDuplicateSweepFn = func(_ *Instance, observedSockets ...string) {
		sweepCount++
		sweepSockets = append([]string(nil), observedSockets...)
	}
	restarted := &Instance{
		ID: "one", Title: "one", ProjectPath: "/tmp/one", Tool: "pi",
		TmuxSocketName: lowerSocket, owningDB: db,
	}
	restarted.adoptPersistenceIncarnation(inst.PersistenceIncarnation())
	result, err := restarted.ReconcileRuntime()
	if err != nil {
		t.Fatal(err)
	}
	if !result.Live || result.Adopted || result.State.Generation != 1 || sweepCount != 1 {
		t.Fatalf("result=%#v sweep=%d", result, sweepCount)
	}
	if !slices.Contains(sweepSockets, candidate.TmuxSocketName) || !slices.Contains(sweepSockets, lowerSocket) {
		t.Fatalf("sweep sockets = %q, want winner %q and observed lower %q", sweepSockets, candidate.TmuxSocketName, lowerSocket)
	}
}

func TestRuntimeLifecycle_LowerCleanupFailureBlocksReconcileAndDelete(t *testing.T) {
	installRuntimeLifecycleTestSeams(t)
	inst, db, state := newRuntimeDeleteTestInstance(t)
	current := runtimeDeletionCandidate(state)
	current.SessionID, current.PaneID = "$2", "%2"
	lower := tmux.RuntimeCandidate{
		SessionID: "$1", SessionName: "runtime-g0", SocketName: state.TmuxSocketName,
		PaneID: "%1", PanePID: 3131, InstanceID: state.InstanceID,
		Generation: state.Generation - 1, GenerationKnown: true,
	}
	live := map[string]bool{current.SessionID: true, lower.SessionID: true}
	runtimeCandidateInventoryFn = func(socketName, instanceID string) ([]tmux.RuntimeCandidate, error) {
		if socketName != state.TmuxSocketName || instanceID != state.InstanceID {
			return nil, nil
		}
		var candidates []tmux.RuntimeCandidate
		if live[current.SessionID] {
			candidates = append(candidates, current)
		}
		if live[lower.SessionID] {
			candidates = append(candidates, lower)
		}
		return candidates, nil
	}

	cleanupFailure := errors.New("injected lower cleanup failure")
	lowerAttempts := 0
	killRuntimeGenerationCandidateFn = func(candidate tmux.RuntimeGenerationCandidate, wait bool) error {
		lowerAttempts++
		if wait || candidate.SessionID != lower.SessionID || candidate.SessionName != lower.SessionName ||
			candidate.SocketName != lower.SocketName || candidate.PaneID != lower.PaneID ||
			candidate.PanePID != lower.PanePID || candidate.InstanceID != lower.InstanceID ||
			candidate.Generation != lower.Generation || !candidate.GenerationKnown {
			t.Fatalf("lower cleanup candidate = %#v wait=%v, want exact %#v", candidate, wait, lower)
		}
		return cleanupFailure
	}

	sweepCalls := 0
	runtimeDuplicateSweepFn = func(*Instance, ...string) { sweepCalls++ }
	oldGenerationInventory := runtimeGenerationCandidateInventoryFn
	oldTerminate := terminateCapturedRuntimeFn
	t.Cleanup(func() {
		runtimeGenerationCandidateInventoryFn = oldGenerationInventory
		terminateCapturedRuntimeFn = oldTerminate
	})
	destructiveInventories, winnerKills := 0, 0
	runtimeGenerationCandidateInventoryFn = func(string, string) ([]tmux.RuntimeGenerationCandidate, error) {
		destructiveInventories++
		return nil, nil
	}
	terminateCapturedRuntimeFn = func(tmux.RuntimeGenerationCandidate, bool) error {
		winnerKills++
		return nil
	}

	result, err := inst.ReconcileRuntime()
	if !errors.Is(err, cleanupFailure) || result.Live || result.State != state {
		t.Fatalf("reconcile result=%#v err=%v, want current non-live result and cleanup failure", result, err)
	}
	if err := inst.DeleteCaptured(inst.CaptureRuntimeSelection()); !errors.Is(err, cleanupFailure) {
		t.Fatalf("DeleteCaptured error = %v, want cleanup failure", err)
	}
	if lowerAttempts != 2 || sweepCalls != 0 || destructiveInventories != 0 || winnerKills != 0 {
		t.Fatalf("lower attempts=%d sweeps=%d destructive inventories=%d winner kills=%d",
			lowerAttempts, sweepCalls, destructiveInventories, winnerKills)
	}
	if !live[current.SessionID] || !live[lower.SessionID] {
		t.Fatalf("cleanup failure orphaned physical runtimes: %#v", live)
	}
	durable, found, readErr := db.ReadRuntimeState(state.InstanceID)
	if readErr != nil || !found || durable != state {
		t.Fatalf("durable runtime=%#v found=%v err=%v, want %#v", durable, found, readErr, state)
	}
	row, loadErr := db.LoadInstanceByID(state.InstanceID)
	if loadErr != nil || row == nil {
		t.Fatalf("logical instance=%#v err=%v, want preserved", row, loadErr)
	}
}

func TestRuntimeLifecycle_AmbiguousCrashCandidatesArePreserved(t *testing.T) {
	installRuntimeLifecycleTestSeams(t)
	db, inst := runtimeLifecycleTestDB(t, "pi", nil)
	runtimeCandidateInventoryFn = func(string, string) ([]tmux.RuntimeCandidate, error) {
		return []tmux.RuntimeCandidate{
			{SessionName: "runtime-a", SocketName: "isolated", InstanceID: "one", Generation: 1, GenerationKnown: true, PanePID: 1001},
			{SessionName: "runtime-b", SocketName: "isolated", InstanceID: "one", Generation: 1, GenerationKnown: true, PanePID: 1002},
		}, nil
	}
	sweepCount := 0
	runtimeDuplicateSweepFn = func(*Instance, ...string) { sweepCount++ }
	_, err := inst.ReconcileRuntime()
	var ambiguity *RuntimeReconciliationAmbiguityError
	if !errors.As(err, &ambiguity) || len(ambiguity.Candidates) != 2 {
		t.Fatalf("err=%v", err)
	}
	message := err.Error()
	for _, want := range []string{
		`socket="isolated" session="runtime-a" generation=1 pid=1001 proof="none"`,
		`socket="isolated" session="runtime-b" generation=1 pid=1002 proof="none"`,
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("ambiguity error %q does not include %q", message, want)
		}
	}
	state, _, _ := db.ReadRuntimeState("one")
	if state.Generation != 0 || sweepCount != 0 {
		t.Fatalf("generation=%d sweep=%d", state.Generation, sweepCount)
	}
}

func TestRuntimeLifecycle_FreshBindingChangesOnlyAfterAuthorityAndCommitsAtomically(t *testing.T) {
	installRuntimeLifecycleTestSeams(t)
	db, inst := runtimeLifecycleTestDB(t, "claude", json.RawMessage(`{"claude_session_id":"old"}`))
	authority, winner, err := inst.beginRuntimeTransition(true)
	if err != nil || winner != nil {
		t.Fatalf("begin winner=%v err=%v", winner, err)
	}
	if inst.ClaudeSessionID != "old" {
		t.Fatalf("binding cleared before transition authority: %q", inst.ClaudeSessionID)
	}
	before, found, err := db.ReadRuntimeBinding("one", "claude")
	if err != nil || !found || before.Value != "old" {
		t.Fatalf("binding before fresh=%#v found=%v err=%v", before, found, err)
	}
	authority.prepareFresh(inst)
	if inst.ClaudeSessionID != "" {
		t.Fatalf("fresh preparation retained old binding %q", inst.ClaudeSessionID)
	}
	inst.ClaudeSessionID = "new"
	inst.ClaudeDetectedAt = time.Unix(90, 0).UTC()
	setRuntimeTestCandidate(inst, "runtime-g1")
	_, _, committed, err := inst.commitPhysicalRuntime(authority)
	authority.close()
	if err != nil || !committed {
		t.Fatalf("committed=%v err=%v", committed, err)
	}
	after, found, err := db.ReadRuntimeBinding("one", "claude")
	state, stateFound, stateErr := db.ReadRuntimeState("one")
	if err != nil || !found || stateErr != nil || !stateFound ||
		after.Value != "new" || after.Generation != 1 || state.Generation != 1 {
		t.Fatalf("binding=%#v found=%v err=%v state=%#v stateFound=%v stateErr=%v", after, found, err, state, stateFound, stateErr)
	}
}

func TestRuntimeLifecycle_StartupSnapshotScalesWithDistinctSockets(t *testing.T) {
	installRuntimeLifecycleTestSeams(t)
	oldSnapshot := runtimeCandidateSnapshotFn
	oldDefaultSocket := tmux.DefaultSocketName()
	t.Cleanup(func() {
		runtimeCandidateSnapshotFn = oldSnapshot
		tmux.SetDefaultSocketName(oldDefaultSocket)
	})
	tmux.SetDefaultSocketName("socket-default")

	db, err := statedb.Open(filepath.Join(t.TempDir(), "startup-scale.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}

	const instanceCount = 400
	socketNames := []string{"socket-a", "socket-b", "socket-c", "socket-d"}
	instances := make([]*Instance, 0, instanceCount)
	for index := 0; index < instanceCount; index++ {
		id := fmt.Sprintf("instance-%03d", index)
		socketName := socketNames[index%len(socketNames)]
		sessionName := fmt.Sprintf("agentdeck_runtime-%03d", index)
		row := &statedb.InstanceRow{
			ID: id, Title: id, ProjectPath: "/tmp/" + id, GroupPath: "my-sessions",
			Tool: "pi", Status: "idle", TmuxSession: sessionName, TmuxSocketName: socketName,
			CreatedAt: time.Unix(1, 0).UTC(), ToolData: json.RawMessage(`{}`),
		}
		if err := db.SaveInstance(row); err != nil {
			t.Fatal(err)
		}
		inst := &Instance{
			ID: id, Title: id, ProjectPath: "/tmp/" + id, Tool: "pi", Status: StatusIdle,
			TmuxSocketName: socketName, owningDB: db,
			tmuxSession: &tmux.Session{Name: sessionName, SocketName: socketName, InstanceID: id},
		}
		inst.adoptPersistenceIncarnation(row.Incarnation)
		instances = append(instances, inst)
	}

	snapshotCalls := 0
	var inventoried []string
	runtimeCandidateSnapshotFn = func(sockets []string) tmux.RuntimeCandidateSnapshot {
		snapshotCalls++
		inventoried = append([]string(nil), sockets...)
		snapshot := make(tmux.RuntimeCandidateSnapshot, len(sockets))
		for _, socketName := range sockets {
			snapshot[socketName] = tmux.RuntimeCandidateSocketSnapshot{
				CandidatesByInstance: map[string][]tmux.RuntimeCandidate{},
			}
		}
		return snapshot
	}

	results := ReconcileStartupRuntimes(instances)
	if len(results) != instanceCount {
		t.Fatalf("startup results = %d, want %d", len(results), instanceCount)
	}
	for _, result := range results {
		if result.Err != nil {
			t.Fatalf("startup reconcile %s: %v", result.InstanceID, result.Err)
		}
	}
	if snapshotCalls != 1 {
		t.Fatalf("socket-complete snapshot calls = %d, want 1", snapshotCalls)
	}
	wantSockets := map[string]bool{
		"socket-a": true, "socket-b": true, "socket-c": true,
		"socket-d": true, "socket-default": true, "": true,
	}
	if len(inventoried) != len(wantSockets) {
		t.Fatalf("inventoried sockets = %q, want %v", inventoried, wantSockets)
	}
	for _, socketName := range inventoried {
		if !wantSockets[socketName] {
			t.Fatalf("unexpected inventoried socket %q in %q", socketName, inventoried)
		}
	}
}

func TestRuntimeLifecycle_StartupSnapshotRevalidatesSelectedLiveTarget(t *testing.T) {
	for _, test := range []struct {
		name      string
		current   bool
		changePID bool
		wantAdopt bool
		wantLive  bool
		wantErr   bool
	}{
		{name: "stable adoption target", wantAdopt: true, wantLive: true},
		{name: "changed adoption target is ambiguous", changePID: true, wantErr: true},
		{name: "stable durable-current target", current: true, wantLive: true},
		{name: "stale durable-current target is ambiguous", current: true, changePID: true, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			installRuntimeLifecycleTestSeams(t)
			oldRevalidate := runtimeCandidateRevalidateFn
			oldDefaultSocket := tmux.DefaultSocketName()
			t.Cleanup(func() {
				runtimeCandidateRevalidateFn = oldRevalidate
				tmux.SetDefaultSocketName(oldDefaultSocket)
			})
			tmux.SetDefaultSocketName("")

			db, inst := runtimeLifecycleTestDB(t, "pi", nil)
			started := time.Unix(100, 123).UTC()
			candidate := tmux.RuntimeCandidate{
				SessionName: "agentdeck_runtime-g1", SocketName: "isolated", InstanceID: inst.ID,
				Generation: 1, GenerationKnown: true, Status: "running",
				LastStartedUnixNano: started.UnixNano(), StateKnown: true,
				BindingKnown: true, PanePID: 4242,
			}
			if test.current {
				candidate.SessionName = "runtime-g0"
				candidate.Generation = 0
				candidate.Status = "idle"
				candidate.LastStartedUnixNano = 0
				candidate.StateKnown = false
				candidate.BindingKnown = false
			}
			runtimeCandidateInventoryFn = func(socketName, instanceID string) ([]tmux.RuntimeCandidate, error) {
				if socketName == candidate.SocketName && instanceID == candidate.InstanceID {
					return []tmux.RuntimeCandidate{candidate}, nil
				}
				return nil, nil
			}
			snapshot := tmux.RuntimeCandidateSnapshot{
				"isolated": {
					CandidatesByInstance: map[string][]tmux.RuntimeCandidate{inst.ID: {candidate}},
				},
				"": {CandidatesByInstance: map[string][]tmux.RuntimeCandidate{}},
			}
			revalidateCalls := 0
			runtimeCandidateRevalidateFn = func(got tmux.RuntimeCandidate) (tmux.RuntimeCandidate, error) {
				revalidateCalls++
				if got != candidate {
					t.Fatalf("revalidation target = %#v, want %#v", got, candidate)
				}
				if test.changePID {
					got.PanePID++
				}
				return got, nil
			}

			result, err := inst.ReconcileRuntimeFromSnapshot(snapshot)
			if revalidateCalls != 1 {
				t.Fatalf("target revalidation calls = %d, want 1", revalidateCalls)
			}
			if test.wantErr {
				var ambiguity *RuntimeReconciliationAmbiguityError
				if !errors.As(err, &ambiguity) || len(ambiguity.Candidates) != 2 {
					t.Fatalf("changed target error = %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if result.Adopted != test.wantAdopt {
				t.Fatalf("adopted = %v, want %v", result.Adopted, test.wantAdopt)
			}
			if result.Live != test.wantLive {
				t.Fatalf("live = %v, want %v", result.Live, test.wantLive)
			}
			durable, found, readErr := db.ReadRuntimeState(inst.ID)
			if readErr != nil || !found {
				t.Fatalf("read runtime: found=%v err=%v", found, readErr)
			}
			wantGeneration := uint64(0)
			if test.wantAdopt {
				wantGeneration = 1
			}
			if durable.Generation != wantGeneration {
				t.Fatalf("durable generation = %d, want %d", durable.Generation, wantGeneration)
			}
		})
	}
}

func TestRuntimeLifecycle_StartupSnapshotRefreshesCompleteInventoryUnderLock(t *testing.T) {
	installRuntimeLifecycleTestSeams(t)
	db, inst := runtimeLifecycleTestDB(t, "pi", nil)
	_ = db
	selected := tmux.RuntimeCandidate{
		SessionID: "$1", SessionName: "runtime-g1", SocketName: "isolated",
		PaneID: "%1", PanePID: 4242, InstanceID: inst.ID,
		Generation: 1, GenerationKnown: true,
		Status: "starting", LastStartedUnixNano: time.Unix(100, 123).UnixNano(), StateKnown: true,
		BindingKnown: true,
	}
	conflict := selected
	conflict.SessionID, conflict.SessionName, conflict.PaneID, conflict.PanePID = "$2", "runtime-conflict", "%2", 4343
	runtimeCandidateInventoryFn = func(socketName, instanceID string) ([]tmux.RuntimeCandidate, error) {
		if socketName == "isolated" && instanceID == inst.ID {
			return []tmux.RuntimeCandidate{selected, conflict}, nil
		}
		return nil, nil
	}
	snapshot := tmux.RuntimeCandidateSnapshot{
		"isolated": {CandidatesByInstance: map[string][]tmux.RuntimeCandidate{inst.ID: {selected}}},
		"":         {CandidatesByInstance: map[string][]tmux.RuntimeCandidate{}},
	}
	result, err := inst.ReconcileRuntimeFromSnapshot(snapshot)
	var ambiguity *RuntimeReconciliationAmbiguityError
	if !errors.As(err, &ambiguity) || len(ambiguity.Candidates) != 2 || result.Adopted || result.Live {
		t.Fatalf("fresh complete inventory result=%#v err=%v", result, err)
	}
	state, found, readErr := db.ReadRuntimeState(inst.ID)
	if readErr != nil || !found || state.Generation != 0 {
		t.Fatalf("ambiguous startup snapshot changed durable state=%#v found=%v err=%v", state, found, readErr)
	}
}
