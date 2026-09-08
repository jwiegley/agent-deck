package session

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

func newRuntimeDeleteTestInstance(t *testing.T) (*Instance, *statedb.StateDB, statedb.RuntimeState) {
	t.Helper()
	db, err := statedb.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	parent := &statedb.InstanceRow{
		ID: "one", Title: "one", ProjectPath: "/tmp/one", GroupPath: "my-sessions",
		Tool: "pi", Status: "idle", TmuxSession: "runtime-g0", TmuxSocketName: "isolated",
		CreatedAt: time.Unix(1, 0), RuntimeBindings: map[string]statedb.RuntimeBinding{},
	}
	if err := db.SaveInstance(parent); err != nil {
		t.Fatal(err)
	}
	state := statedb.RuntimeState{
		InstanceID: "one", Generation: 1, StatusRevision: 0,
		TmuxSession: "runtime-g1", TmuxSocketName: "isolated", Status: "running",
		LastStartedAt: time.Unix(2, 0).UTC(),
	}
	if err := db.CommitRuntimeTransition(0, parent.Incarnation, state); err != nil {
		t.Fatal(err)
	}
	sess := tmux.ReconnectSessionLazy(state.TmuxSession, "one", "/tmp/one", "pi", state.Status)
	sess.SocketName = state.TmuxSocketName
	inst := &Instance{
		ID: state.InstanceID, Title: "one", ProjectPath: "/tmp/one", Tool: "pi",
		Status: Status(state.Status), RuntimeGeneration: state.Generation,
		StatusRevision: state.StatusRevision, TmuxSocketName: state.TmuxSocketName,
		tmuxSession: sess,
	}
	inst.adoptPersistenceIncarnation(parent.Incarnation)
	inst.setOwningDB(db)
	return inst, db, state
}

func runtimeDeletionCandidate(state statedb.RuntimeState) tmux.RuntimeCandidate {
	return tmux.RuntimeCandidate{
		SessionID: "$7", SessionName: state.TmuxSession, SocketName: state.TmuxSocketName, PaneID: "%9",
		InstanceID: state.InstanceID, Generation: state.Generation, GenerationKnown: true,
		StatusRevision: state.StatusRevision, Status: state.Status,
		LastStartedUnixNano: state.LastStartedAt.UnixNano(), StateKnown: true,
		BindingKnown: true, PanePID: 4242,
	}
}

func runtimeDeletionGenerationCandidate(state statedb.RuntimeState) tmux.RuntimeGenerationCandidate {
	return tmux.RuntimeGenerationCandidate{
		SessionName: state.TmuxSession, SessionID: "$7", SocketName: state.TmuxSocketName,
		PaneID: "%9", PanePID: 4242, InstanceID: state.InstanceID,
		Generation: state.Generation, GenerationKnown: true,
	}
}

func runtimeGenerationCandidateMatchesState(candidate tmux.RuntimeGenerationCandidate, state statedb.RuntimeState) bool {
	return candidate.InstanceID == state.InstanceID &&
		candidate.Generation == state.Generation && candidate.GenerationKnown &&
		candidate.SessionName == state.TmuxSession && candidate.SocketName == state.TmuxSocketName
}

func TestRuntimeLifecycle_DestructiveSelectionPreservesNativeDefaultSocket(t *testing.T) {
	oldDefault := tmux.DefaultSocketName()
	oldInventory := runtimeGenerationCandidateInventoryFn
	t.Cleanup(func() {
		tmux.SetDefaultSocketName(oldDefault)
		runtimeGenerationCandidateInventoryFn = oldInventory
	})
	tmux.SetDefaultSocketName("configured-socket")

	expected := statedb.RuntimeState{
		InstanceID: "native-instance", Generation: 2,
		TmuxSession: "agentdeck_native", TmuxSocketName: "",
	}
	want := runtimeDeletionGenerationCandidate(expected)
	runtimeGenerationCandidateInventoryFn = func(socketName, instanceID string) ([]tmux.RuntimeGenerationCandidate, error) {
		if socketName != "" || instanceID != expected.InstanceID {
			t.Fatalf("destructive inventory = socket %q instance %q, want native default and %q", socketName, instanceID, expected.InstanceID)
		}
		return []tmux.RuntimeGenerationCandidate{want}, nil
	}
	inst := &Instance{ID: expected.InstanceID}
	selected, err := inst.captureDestructiveRuntimeCandidateLocked(nil, expected, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if selected == nil || selected.SocketName != "" || *selected != want {
		t.Fatalf("destructive selection = %#v, want native-default %#v", selected, want)
	}
}

func installRuntimeDeletionCandidateSeams(t *testing.T, state statedb.RuntimeState) {
	t.Helper()
	oldInventory := runtimeCandidateInventoryFn
	oldGenerationInventory := runtimeGenerationCandidateInventoryFn
	oldRevalidate := runtimeCandidateRevalidateFn
	oldSweep := runtimeDuplicateSweepFn
	oldDiscover := discoverCapturedRuntimeChildrenFn
	runtimeCandidateInventoryFn = func(socketName, instanceID string) ([]tmux.RuntimeCandidate, error) {
		if socketName != state.TmuxSocketName || instanceID != state.InstanceID {
			return nil, nil
		}
		return []tmux.RuntimeCandidate{runtimeDeletionCandidate(state)}, nil
	}
	runtimeGenerationCandidateInventoryFn = func(socketName, instanceID string) ([]tmux.RuntimeGenerationCandidate, error) {
		if socketName != state.TmuxSocketName || instanceID != state.InstanceID {
			return nil, nil
		}
		return []tmux.RuntimeGenerationCandidate{runtimeDeletionGenerationCandidate(state)}, nil
	}
	runtimeCandidateRevalidateFn = func(candidate tmux.RuntimeCandidate) (tmux.RuntimeCandidate, error) {
		return candidate, nil
	}
	runtimeDuplicateSweepFn = func(*Instance, ...string) {}
	discoverCapturedRuntimeChildrenFn = func(*Instance, tmux.RuntimeGenerationCandidate) ([]tmux.ProcessIdentity, error) {
		return nil, nil
	}
	t.Cleanup(func() {
		runtimeCandidateInventoryFn = oldInventory
		runtimeGenerationCandidateInventoryFn = oldGenerationInventory
		runtimeCandidateRevalidateFn = oldRevalidate
		runtimeDuplicateSweepFn = oldSweep
		discoverCapturedRuntimeChildrenFn = oldDiscover
	})
}

func stubRuntimeDeletionPhysicalWork(t *testing.T, state statedb.RuntimeState, terminate func(tmux.RuntimeGenerationCandidate, bool) error) {
	t.Helper()
	installRuntimeDeletionCandidateSeams(t, state)
	oldLock := instanceSpawnLockAcquireFn
	oldTerminate := terminateCapturedRuntimeFn
	oldDiscover := discoverCapturedRuntimeChildrenFn
	oldReserved := runtimeDestructionReservedFn
	oldDelete := deleteInstanceIfRuntimeFn
	oldCompleteDelete := completeDeletedRuntimeDestructionFn
	oldCaptureService := captureParentDeleteServiceOwnershipFn
	oldRetireService := retireParentDeleteServiceUnitFn
	instanceSpawnLockAcquireFn = func(string) (func(), error) { return func() {}, nil }
	terminateCapturedRuntimeFn = terminate
	discoverCapturedRuntimeChildrenFn = func(*Instance, tmux.RuntimeGenerationCandidate) ([]tmux.ProcessIdentity, error) {
		return nil, nil
	}
	runtimeDestructionReservedFn = func(statedb.RuntimeState) {}
	captureParentDeleteServiceOwnershipFn = func(*Instance, RuntimeSelection) tmux.ServiceUnitOwnership { return tmux.ServiceUnitOwnership{} }
	retireParentDeleteServiceUnitFn = func(*Instance, tmux.ServiceUnitOwnership) {}
	t.Cleanup(func() {
		instanceSpawnLockAcquireFn = oldLock
		terminateCapturedRuntimeFn = oldTerminate
		discoverCapturedRuntimeChildrenFn = oldDiscover
		runtimeDestructionReservedFn = oldReserved
		deleteInstanceIfRuntimeFn = oldDelete
		completeDeletedRuntimeDestructionFn = oldCompleteDelete
		captureParentDeleteServiceOwnershipFn = oldCaptureService
		retireParentDeleteServiceUnitFn = oldRetireService
	})
}

func TestRuntimeLifecycle_CurrentExactSelectionKillsCapturedIdentity(t *testing.T) {
	inst, db, state := newRuntimeDeleteTestInstance(t)
	var terminated tmux.RuntimeGenerationCandidate
	var discovered tmux.RuntimeGenerationCandidate
	stubRuntimeDeletionPhysicalWork(t, state, func(got tmux.RuntimeGenerationCandidate, wait bool) error {
		terminated = got
		if wait {
			t.Fatal("KillCaptured unexpectedly requested synchronous termination")
		}
		return nil
	})
	discoverCapturedRuntimeChildrenFn = func(_ *Instance, got tmux.RuntimeGenerationCandidate) ([]tmux.ProcessIdentity, error) {
		discovered = got
		return nil, nil
	}

	selection := inst.CaptureRuntimeSelection()
	newer := tmux.ReconnectSessionLazy("mutable-pointer-must-not-win", "newer", "", "", "running")
	newer.SocketName = "other-socket"
	inst.tmuxSession = newer
	if err := inst.KillCaptured(selection); err != nil {
		t.Fatal(err)
	}
	if !runtimeGenerationCandidateMatchesState(terminated, state) {
		t.Fatalf("terminated %#v, want captured %#v", terminated, state)
	}
	if !runtimeGenerationCandidateMatchesState(discovered, state) {
		t.Fatalf("discovered %#v, want captured %#v", discovered, state)
	}
	got, found, err := db.ReadRuntimeState(state.InstanceID)
	if err != nil || !found || got.Status != string(StatusStopped) || got.StatusRevision != 2 {
		t.Fatalf("runtime after kill = %#v, found=%v err=%v", got, found, err)
	}
}

func TestRuntimeLifecycle_DestructiveOperationsRecoverReservationBeforeLateWriterFence(t *testing.T) {
	operations := []struct {
		name string
		run  func(*Instance, RuntimeSelection, func()) error
	}{
		{name: "kill", run: func(inst *Instance, selection RuntimeSelection, _ func()) error {
			return inst.KillCaptured(selection)
		}},
		{name: "delete", run: func(inst *Instance, selection RuntimeSelection, cleanup func()) error {
			return inst.DeleteCapturedWithCleanup(selection, cleanup)
		}},
		{name: "remove", run: func(inst *Instance, selection RuntimeSelection, _ func()) error {
			return inst.RemoveCaptured(selection)
		}},
	}
	for _, operation := range operations {
		for _, live := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/live=%t", operation.name, live), func(t *testing.T) {
				inst, db, state := newRuntimeDeleteTestInstance(t)
				terminated := 0
				stubRuntimeDeletionPhysicalWork(t, state, func(tmux.RuntimeGenerationCandidate, bool) error {
					terminated++
					return nil
				})
				retired := 0
				retireParentDeleteServiceUnitFn = func(*Instance, tmux.ServiceUnitOwnership) { retired++ }
				cleaned := 0
				selection := inst.CaptureRuntimeSelection()
				claimed, err := db.ReserveRuntimeDestruction(state, inst.PersistenceIncarnation())
				if err != nil {
					t.Fatal(err)
				}
				registerLateLegacyRuntimeWriter(t, db)
				if !live {
					runtimeCandidateInventoryFn = func(string, string) ([]tmux.RuntimeCandidate, error) {
						return nil, nil
					}
				}

				err = operation.run(inst, selection, func() { cleaned++ })
				if !errors.Is(err, statedb.ErrIncompatibleWriterSchema) {
					t.Fatalf("%s error=%v, want ErrIncompatibleWriterSchema", operation.name, err)
				}
				if terminated != 0 || cleaned != 0 || retired != 0 {
					t.Fatalf("late-writer recovery mutated auxiliaries: terminations=%d cleanup=%d retirements=%d",
						terminated, cleaned, retired)
				}
				if row, loadErr := db.LoadInstanceByID(inst.ID); loadErr != nil || row == nil {
					t.Fatalf("late-writer recovery removed parent: row=%#v err=%v", row, loadErr)
				}
				got, found, readErr := db.ReadRuntimeState(inst.ID)
				wantStatus := string(StatusStopped)
				if live {
					wantStatus = state.Status
				}
				if readErr != nil || !found || statedb.IsRuntimeDestructionReserved(got) ||
					got.Status != wantStatus || got.StatusRevision != claimed.StatusRevision+1 {
					t.Fatalf("post-fence runtime=%#v found=%v err=%v want status=%q revision=%d",
						got, found, readErr, wantStatus, claimed.StatusRevision+1)
				}
				var provenanceRows int
				if err := db.DB().QueryRow(`SELECT COUNT(*) FROM instance_runtime_destruction WHERE instance_id = ?`, inst.ID).
					Scan(&provenanceRows); err != nil || provenanceRows != 0 {
					t.Fatalf("%s stranded provenance: rows=%d err=%v", operation.name, provenanceRows, err)
				}
			})
		}
	}
}

func TestRuntimeLifecycle_DeletionPreservesCapturedMCPChildIdentity(t *testing.T) {
	inst, db, state := newRuntimeDeleteTestInstance(t)
	stubRuntimeDeletionPhysicalWork(t, state, func(tmux.RuntimeGenerationCandidate, bool) error { return nil })

	want := tmux.ProcessIdentity{PID: 999999, StartToken: "captured-before-tmux-kill"}
	discoverCapturedRuntimeChildrenFn = func(*Instance, tmux.RuntimeGenerationCandidate) ([]tmux.ProcessIdentity, error) {
		return []tmux.ProcessIdentity{want}, nil
	}

	oldCapture := mcpCaptureProcessIdentityFn
	oldReap := mcpReapProcessIdentitiesFn
	captureCalls := 0
	mcpCaptureProcessIdentityFn = func(pid int) (tmux.ProcessIdentity, error) {
		captureCalls++
		return tmux.ProcessIdentity{PID: pid, StartToken: "recaptured-after-kill"}, nil
	}
	var reaped []tmux.ProcessIdentity
	mcpReapProcessIdentitiesFn = func(identities []tmux.ProcessIdentity, _ tmux.ProcessReapTiming) {
		reaped = append([]tmux.ProcessIdentity(nil), identities...)
	}
	t.Cleanup(func() {
		mcpCaptureProcessIdentityFn = oldCapture
		mcpReapProcessIdentitiesFn = oldReap
	})

	if err := inst.DeleteCaptured(inst.CaptureRuntimeSelection()); err != nil {
		t.Fatal(err)
	}
	if captureCalls != 0 {
		t.Fatalf("delete path recaptured child identity %d times after discovery", captureCalls)
	}
	if len(reaped) != 1 || reaped[0] != want {
		t.Fatalf("reaped identities = %#v, want exact captured identity %#v", reaped, want)
	}
	if row, err := db.LoadInstanceByID(inst.ID); err != nil || row != nil {
		t.Fatalf("delete path parent row = %#v, err=%v; want deleted", row, err)
	}
}

func TestRuntimeLifecycle_ArchiveAdoptsPreCASCandidateAndKillsNothing(t *testing.T) {
	inst, db, state := newRuntimeDeleteTestInstance(t)
	terminated := 0
	stubRuntimeDeletionPhysicalWork(t, state, func(tmux.RuntimeGenerationCandidate, bool) error {
		terminated++
		return nil
	})

	next := state
	next.Generation++
	next.StatusRevision = 0
	next.TmuxSession = "runtime-g2"
	next.LastStartedAt = state.LastStartedAt.Add(time.Second)
	runtimeCandidateInventoryFn = func(socketName, instanceID string) ([]tmux.RuntimeCandidate, error) {
		if socketName != state.TmuxSocketName || instanceID != state.InstanceID {
			return nil, nil
		}
		return []tmux.RuntimeCandidate{
			runtimeDeletionCandidate(state),
			runtimeDeletionCandidate(next),
		}, nil
	}
	stableInventories := 0
	runtimeGenerationCandidateInventoryFn = func(string, string) ([]tmux.RuntimeGenerationCandidate, error) {
		stableInventories++
		return []tmux.RuntimeGenerationCandidate{runtimeDeletionGenerationCandidate(state)}, nil
	}

	selection := inst.CaptureRuntimeSelection()
	if _, err := inst.KillCapturedRuntime(selection); !errors.Is(err, statedb.ErrRuntimeGenerationConflict) {
		t.Fatalf("archive kill error = %v, want generation conflict", err)
	}
	if terminated != 0 || stableInventories != 0 {
		t.Fatalf("pre-CAS candidate path terminated=%d stable inventories=%d, want neither", terminated, stableInventories)
	}

	durable, found, err := db.ReadRuntimeState(state.InstanceID)
	if err != nil || !found || !sameDestructiveRuntime(durable, next) {
		t.Fatalf("adopted runtime = %#v, found=%v err=%v; want %#v", durable, found, err, next)
	}
	row, err := db.LoadInstanceByID(state.InstanceID)
	if err != nil || row == nil || !row.ArchivedAt.IsZero() {
		t.Fatalf("pre-CAS archive row=%#v err=%v", row, err)
	}
}

func TestRuntimeLifecycle_ArchivePreservesStableIdentityReplacement(t *testing.T) {
	inst, db, state := newRuntimeDeleteTestInstance(t)
	var terminated tmux.RuntimeGenerationCandidate
	stubRuntimeDeletionPhysicalWork(t, state, func(candidate tmux.RuntimeGenerationCandidate, wait bool) error {
		terminated = candidate
		if wait {
			t.Fatal("archive unexpectedly requested synchronous termination")
		}
		return tmux.ErrRuntimeGenerationCandidateChanged
	})
	discoverCapturedRuntimeChildrenFn = func(*Instance, tmux.RuntimeGenerationCandidate) ([]tmux.ProcessIdentity, error) {
		return []tmux.ProcessIdentity{{PID: 999999, StartToken: "captured-before-replacement"}}, nil
	}

	selection := inst.CaptureRuntimeSelection()
	killed, err := inst.KillCapturedRuntime(selection)
	if !errors.Is(err, tmux.ErrRuntimeGenerationCandidateChanged) {
		t.Fatalf("archive kill error = %v, want stable identity replacement", err)
	}
	if killed.InstanceID != "" {
		t.Fatalf("failed archive returned kill authority %#v", killed)
	}
	if !runtimeGenerationCandidateMatchesState(terminated, state) || terminated.SessionID != "$7" || terminated.PaneID != "%9" {
		t.Fatalf("atomic kill candidate = %#v", terminated)
	}
	if len(inst.TrackedMCPPIDs) != 0 {
		t.Fatalf("replacement inherited tracked cleanup PIDs: %v", inst.TrackedMCPPIDs)
	}

	durable, found, readErr := db.ReadRuntimeState(state.InstanceID)
	if readErr != nil || !found || durable.Generation != state.Generation ||
		durable.Status != state.Status || durable.StatusRevision != state.StatusRevision+2 {
		t.Fatalf("runtime after replacement = %#v, found=%v err=%v", durable, found, readErr)
	}
	if err := db.SetArchivedIfRuntime(selection.State, selection.Incarnation, time.Unix(4, 0).UTC()); !errors.Is(err, statedb.ErrRuntimeGenerationConflict) {
		t.Fatalf("stale archive CAS error = %v, want generation conflict", err)
	}
	row, rowErr := db.LoadInstanceByID(state.InstanceID)
	if rowErr != nil || row == nil || !row.ArchivedAt.IsZero() {
		t.Fatalf("replacement archive row=%#v err=%v", row, rowErr)
	}
}

func TestRuntimeLifecycle_PostKillArchiveRejectsReplacementRuntime(t *testing.T) {
	inst, db, state := newRuntimeDeleteTestInstance(t)
	stubRuntimeDeletionPhysicalWork(t, state, func(tmux.RuntimeGenerationCandidate, bool) error { return nil })

	killed, err := inst.KillCapturedRuntime(inst.CaptureRuntimeSelection())
	if err != nil {
		t.Fatal(err)
	}
	if killed.InstanceID != state.InstanceID || killed.Generation != state.Generation ||
		killed.Status != string(StatusStopped) || killed.StatusRevision != state.StatusRevision+2 {
		t.Fatalf("post-kill runtime = %#v", killed)
	}

	replacement := killed
	replacement.Generation++
	replacement.StatusRevision = 0
	replacement.TmuxSession = "runtime-g2"
	replacement.Status = string(StatusRunning)
	replacement.LastStartedAt = time.Unix(3, 0).UTC()
	if err := db.CommitRuntimeTransition(killed.Generation, inst.PersistenceIncarnation(), replacement); err != nil {
		t.Fatal(err)
	}
	if err := db.SetArchivedIfRuntime(killed, inst.PersistenceIncarnation(), time.Unix(4, 0).UTC()); !errors.Is(err, statedb.ErrRuntimeGenerationConflict) {
		t.Fatalf("stale post-kill archive error = %v, want generation conflict", err)
	}
	row, err := db.LoadInstanceByID(state.InstanceID)
	if err != nil || row == nil || !row.ArchivedAt.IsZero() {
		t.Fatalf("replacement archive row=%#v err=%v", row, err)
	}
}

func TestRuntimeLifecycle_StatusPublisherCannotPassReservedKillBarrier(t *testing.T) {
	inst, db, state := newRuntimeDeleteTestInstance(t)
	terminated := 0
	stubRuntimeDeletionPhysicalWork(t, state, func(tmux.RuntimeGenerationCandidate, bool) error {
		terminated++
		return nil
	})
	reserved := make(chan struct{})
	unblock := make(chan struct{})
	runtimeDestructionReservedFn = func(statedb.RuntimeState) {
		close(reserved)
		<-unblock
	}
	done := make(chan error, 1)
	selection := inst.CaptureRuntimeSelection()
	go func() { done <- inst.KillCaptured(selection) }()
	select {
	case <-reserved:
	case err := <-done:
		t.Fatalf("KillCaptured returned before runtime reservation: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for runtime reservation")
	}
	claimed, found, err := db.ReadRuntimeState(state.InstanceID)
	if err != nil || !found || claimed.StatusRevision != state.StatusRevision+1 {
		t.Fatalf("reserved runtime = %#v, found=%v err=%v", claimed, found, err)
	}
	if applied, err := db.WriteStatusIfVersion(claimed.InstanceID, selection.Incarnation, claimed.Generation, claimed.StatusRevision, "waiting"); err != nil || applied {
		t.Fatalf("status publisher crossed reservation: applied=%v err=%v", applied, err)
	}
	close(unblock)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for KillCaptured completion")
	}
	if terminated != 1 {
		t.Fatalf("terminator calls = %d, want 1", terminated)
	}
}

func TestRuntimeLifecycle_StatusRevisionConflictKillsNothing(t *testing.T) {
	inst, db, state := newRuntimeDeleteTestInstance(t)
	terminated := 0
	stubRuntimeDeletionPhysicalWork(t, state, func(tmux.RuntimeGenerationCandidate, bool) error {
		terminated++
		return nil
	})
	selection := inst.CaptureRuntimeSelection()
	if applied, err := db.WriteStatusIfVersion(state.InstanceID, selection.Incarnation, state.Generation, state.StatusRevision, "waiting"); err != nil || !applied {
		t.Fatalf("advance status revision: applied=%v err=%v", applied, err)
	}
	if err := inst.KillCaptured(selection); !errors.Is(err, statedb.ErrStatusRevisionConflict) {
		t.Fatalf("KillCaptured error = %v, want status revision conflict", err)
	}
	if terminated != 0 {
		t.Fatalf("stale status selection terminated %d runtimes", terminated)
	}
}

func TestRuntimeLifecycle_QueuedGenerationKillsNothing(t *testing.T) {
	inst, db, state := newRuntimeDeleteTestInstance(t)
	terminated := 0
	stubRuntimeDeletionPhysicalWork(t, state, func(tmux.RuntimeGenerationCandidate, bool) error {
		terminated++
		return nil
	})
	entered := make(chan struct{})
	unblock := make(chan struct{})
	instanceSpawnLockAcquireFn = func(string) (func(), error) {
		close(entered)
		<-unblock
		return func() {}, nil
	}

	done := make(chan error, 1)
	selection := inst.CaptureRuntimeSelection()
	go func() { done <- inst.KillCaptured(selection) }()
	<-entered
	next := state
	next.Generation++
	next.TmuxSession = "runtime-g2"
	if err := db.CommitRuntimeTransition(state.Generation, selection.Incarnation, next); err != nil {
		t.Fatal(err)
	}
	close(unblock)
	if err := <-done; !errors.Is(err, statedb.ErrRuntimeGenerationConflict) {
		t.Fatalf("queued KillCaptured error = %v, want generation conflict", err)
	}
	if terminated != 0 {
		t.Fatalf("queued stale selection terminated %d runtimes", terminated)
	}
}

func TestRuntimeLifecycle_StartWaiterCannotRecreateRuntimeAfterDelete(t *testing.T) {
	installRuntimeLifecycleTestSeams(t)
	db, inst := runtimeLifecycleTestDB(t, "pi", nil)
	state, found, err := db.ReadRuntimeState(inst.ID)
	if err != nil || !found {
		t.Fatalf("ReadRuntimeState = %#v, found=%v err=%v", state, found, err)
	}

	oldGenerationInventory := runtimeGenerationCandidateInventoryFn
	runtimeGenerationCandidateInventoryFn = func(string, string) ([]tmux.RuntimeGenerationCandidate, error) {
		return nil, nil
	}
	t.Cleanup(func() { runtimeGenerationCandidateInventoryFn = oldGenerationInventory })

	observed := make(chan struct{})
	resume := make(chan struct{})
	runtimeTransitionObservedFn = func() {
		close(observed)
		<-resume
	}
	type result struct {
		authority *runtimeTransitionAuthority
		winner    *statedb.RuntimeState
		err       error
	}
	done := make(chan result, 1)
	go func() {
		authority, winner, beginErr := inst.beginRuntimeTransition(false)
		done <- result{authority: authority, winner: winner, err: beginErr}
	}()
	<-observed

	if err := inst.DeleteCaptured(inst.CaptureRuntimeSelection()); err != nil {
		t.Fatalf("DeleteCaptured: %v", err)
	}
	close(resume)
	got := <-done
	if got.authority != nil {
		got.authority.close()
		t.Fatal("stale start waiter acquired spawn authority after deletion")
	}
	if got.winner != nil {
		t.Fatalf("stale start waiter adopted orphan winner %#v", *got.winner)
	}
	if !errors.Is(got.err, statedb.ErrInstanceParentConflict) {
		t.Fatalf("stale start waiter error = %v, want parent incarnation conflict", got.err)
	}
	if row, err := db.LoadInstanceByID(inst.ID); err != nil || row != nil {
		t.Fatalf("deleted logical row = %#v, err=%v", row, err)
	}
	if runtime, found, err := db.ReadRuntimeState(inst.ID); err != nil || found {
		t.Fatalf("orphan runtime = %#v, found=%v err=%v", runtime, found, err)
	}
}

func TestRuntimeLifecycle_RecreatedGenerationDeletePreservesNewRow(t *testing.T) {
	inst, db, state := newRuntimeDeleteTestInstance(t)
	terminated := 0
	stubRuntimeDeletionPhysicalWork(t, state, func(tmux.RuntimeGenerationCandidate, bool) error {
		terminated++
		return nil
	})
	selection := inst.CaptureRuntimeSelection()
	next := state
	next.Generation++
	next.TmuxSession = "runtime-g2"
	if err := db.CommitRuntimeTransition(state.Generation, selection.Incarnation, next); err != nil {
		t.Fatal(err)
	}
	if err := inst.DeleteAndWaitCaptured(selection); !errors.Is(err, statedb.ErrRuntimeGenerationConflict) {
		t.Fatalf("DeleteAndWaitCaptured error = %v, want generation conflict", err)
	}
	got, found, err := db.ReadRuntimeState(state.InstanceID)
	if err != nil || !found || got.Generation != next.Generation || got.TmuxSession != next.TmuxSession {
		t.Fatalf("recreated runtime = %#v, found=%v err=%v", got, found, err)
	}
	if terminated != 0 {
		t.Fatalf("recreated-generation delete terminated %d runtimes", terminated)
	}
}

func TestRuntimeLifecycle_DeleteCapturedRejectsByteIdenticalNewIncarnation(t *testing.T) {
	inst, db, state := newRuntimeDeleteTestInstance(t)
	selection := inst.CaptureRuntimeSelection()
	terminated := 0
	stubRuntimeDeletionPhysicalWork(t, state, func(tmux.RuntimeGenerationCandidate, bool) error {
		terminated++
		return nil
	})

	if err := db.DeleteInstance(state.InstanceID); err != nil {
		t.Fatal(err)
	}
	replacement := &statedb.InstanceRow{
		ID: state.InstanceID, Title: "replacement", ProjectPath: "/tmp/replacement",
		GroupPath: "my-sessions", Tool: "pi", Status: state.Status,
		TmuxSession: state.TmuxSession, TmuxSocketName: state.TmuxSocketName,
		RuntimeGeneration: state.Generation, StatusRevision: state.StatusRevision,
		LastStartedAt: state.LastStartedAt, CreatedAt: time.Unix(3, 0).UTC(),
	}
	if err := db.SaveInstance(replacement); err != nil {
		t.Fatal(err)
	}
	if replacement.Incarnation == selection.Incarnation {
		t.Fatal("same-ID replacement reused the deleted incarnation")
	}

	err := inst.DeleteCaptured(selection)
	if !errors.Is(err, statedb.ErrInstanceParentConflict) {
		t.Fatalf("DeleteCaptured error = %v, want parent incarnation conflict", err)
	}
	if terminated != 0 {
		t.Fatalf("stale selection terminated %d replacement runtimes", terminated)
	}
	row, loadErr := db.LoadInstanceByID(state.InstanceID)
	durable, found, readErr := db.ReadRuntimeState(state.InstanceID)
	if loadErr != nil || row == nil || row.Incarnation != replacement.Incarnation ||
		readErr != nil || !found || !sameDestructiveRuntime(durable, state) {
		t.Fatalf("replacement row=%#v loadErr=%v runtime=%#v found=%v readErr=%v", row, loadErr, durable, found, readErr)
	}
}

func TestRuntimeLifecycle_DeleteCleanupRunsBeforeParentRemoval(t *testing.T) {
	inst, db, state := newRuntimeDeleteTestInstance(t)
	selection := inst.CaptureRuntimeSelection()
	stubRuntimeDeletionPhysicalWork(t, state, func(tmux.RuntimeGenerationCandidate, bool) error { return nil })
	cleanupCalls := 0
	err := inst.DeleteCapturedWithCleanup(selection, func() {
		cleanupCalls++
		row, loadErr := db.LoadInstanceByID(state.InstanceID)
		if loadErr != nil || row == nil || row.Incarnation != selection.Incarnation {
			t.Fatalf("cleanup observed parent=%#v err=%v", row, loadErr)
		}
		candidate := &statedb.InstanceRow{
			ID: state.InstanceID, Incarnation: "cleanup-contender", Title: "too-early", ProjectPath: "/tmp/replacement",
			GroupPath: "my-sessions", Tool: "pi", Status: state.Status,
			TmuxSession: state.TmuxSession, TmuxSocketName: state.TmuxSocketName,
			RuntimeGeneration: state.Generation, StatusRevision: state.StatusRevision,
			LastStartedAt: state.LastStartedAt, CreatedAt: time.Unix(3, 0).UTC(),
		}
		if _, inserted, insertErr := db.InsertInstanceIfAbsent(candidate); insertErr != nil || inserted {
			t.Fatalf("same-ID insert during cleanup inserted=%v err=%v", inserted, insertErr)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if cleanupCalls != 1 {
		t.Fatalf("cleanup calls=%d, want 1", cleanupCalls)
	}
	if row, err := db.LoadInstanceByID(state.InstanceID); err != nil || row != nil {
		t.Fatalf("deleted parent remained after cleanup: row=%#v err=%v", row, err)
	}
}

func TestRuntimeLifecycle_ReplacementAfterReservationIsNotTerminated(t *testing.T) {
	inst, db, state := newRuntimeDeleteTestInstance(t)
	selection := inst.CaptureRuntimeSelection()
	terminated := 0
	stubRuntimeDeletionPhysicalWork(t, state, func(tmux.RuntimeGenerationCandidate, bool) error {
		terminated++
		return nil
	})
	oldReserved := runtimeDestructionReservedFn
	t.Cleanup(func() { runtimeDestructionReservedFn = oldReserved })
	var replacement *statedb.InstanceRow
	runtimeDestructionReservedFn = func(claimed statedb.RuntimeState) {
		if err := db.DeleteInstance(claimed.InstanceID); err != nil {
			t.Fatal(err)
		}
		replacement = &statedb.InstanceRow{
			ID: claimed.InstanceID, Title: "replacement", ProjectPath: "/tmp/replacement",
			GroupPath: "my-sessions", Tool: "pi", Status: string(StatusStopped),
			TmuxSession: claimed.TmuxSession, TmuxSocketName: claimed.TmuxSocketName,
			RuntimeGeneration: claimed.Generation, StatusRevision: claimed.StatusRevision,
			LastStartedAt: claimed.LastStartedAt, CreatedAt: time.Unix(3, 0).UTC(),
		}
		if err := db.SaveInstance(replacement); err != nil {
			t.Fatal(err)
		}
	}

	err := inst.DeleteCaptured(selection)
	if !errors.Is(err, statedb.ErrInstanceParentConflict) {
		t.Fatalf("DeleteCaptured error = %v, want parent incarnation conflict", err)
	}
	if terminated != 0 {
		t.Fatalf("post-reservation replacement received %d physical terminations", terminated)
	}
	row, loadErr := db.LoadInstanceByID(state.InstanceID)
	durable, found, readErr := db.ReadRuntimeState(state.InstanceID)
	if replacement == nil || loadErr != nil || row == nil || row.Incarnation != replacement.Incarnation ||
		readErr != nil || !found || durable.Status != string(StatusStopped) {
		t.Fatalf("replacement row=%#v loadErr=%v runtime=%#v found=%v readErr=%v", row, loadErr, durable, found, readErr)
	}
}

func TestRuntimeLifecycle_DeleteFailureReleasesReservationAndAdoptsStopped(t *testing.T) {
	inst, db, state := newRuntimeDeleteTestInstance(t)
	physicalKills := 0
	stubRuntimeDeletionPhysicalWork(t, state, func(tmux.RuntimeGenerationCandidate, bool) error {
		physicalKills++
		return nil
	})

	deletionFailure := errors.New("injected delete failure")
	deleteCalls, releaseCalls := 0, 0
	deleteInstanceIfRuntimeFn = func(*statedb.StateDB, statedb.RuntimeState, string) error {
		deleteCalls++
		return deletionFailure
	}
	completeDeletedRuntimeDestructionFn = func(gotDB *statedb.StateDB, claimed statedb.RuntimeState, incarnation string) (statedb.RuntimeState, error) {
		releaseCalls++
		return gotDB.CompleteRuntimeDestruction(claimed, incarnation, string(StatusStopped))
	}

	if err := inst.DeleteCaptured(inst.CaptureRuntimeSelection()); !errors.Is(err, deletionFailure) {
		t.Fatalf("delete error = %v, want injected failure", err)
	}
	if physicalKills != 1 || deleteCalls != 1 || releaseCalls != 1 {
		t.Fatalf("physical kills=%d delete calls=%d release calls=%d, want 1 each", physicalKills, deleteCalls, releaseCalls)
	}

	durable, found, err := db.ReadRuntimeState(state.InstanceID)
	if err != nil || !found || durable.Status != string(StatusStopped) || durable.StatusRevision != state.StatusRevision+2 {
		t.Fatalf("released runtime = %#v, found=%v err=%v", durable, found, err)
	}
	memory := inst.runtimeStateSnapshot()
	if !sameDestructiveRuntime(memory, durable) {
		t.Fatalf("memory runtime = %#v, want released %#v", memory, durable)
	}
	row, err := db.LoadInstanceByID(state.InstanceID)
	if err != nil || row == nil || row.Status != string(StatusStopped) {
		t.Fatalf("preserved row = %#v err=%v", row, err)
	}
}

func TestRuntimeLifecycle_DeleteFailureJoinsReservationReleaseFailure(t *testing.T) {
	inst, db, state := newRuntimeDeleteTestInstance(t)
	physicalKills := 0
	stubRuntimeDeletionPhysicalWork(t, state, func(tmux.RuntimeGenerationCandidate, bool) error {
		physicalKills++
		return nil
	})

	deletionFailure := errors.New("injected delete failure")
	releaseFailure := errors.New("injected release failure")
	deleteCalls, releaseCalls := 0, 0
	deleteInstanceIfRuntimeFn = func(*statedb.StateDB, statedb.RuntimeState, string) error {
		deleteCalls++
		return deletionFailure
	}
	completeDeletedRuntimeDestructionFn = func(*statedb.StateDB, statedb.RuntimeState, string) (statedb.RuntimeState, error) {
		releaseCalls++
		return statedb.RuntimeState{}, releaseFailure
	}

	err := inst.DeleteCaptured(inst.CaptureRuntimeSelection())
	if !errors.Is(err, deletionFailure) || !errors.Is(err, releaseFailure) {
		t.Fatalf("delete error = %v, want joined deletion and release failures", err)
	}
	if physicalKills != 1 || deleteCalls != 1 || releaseCalls != 1 {
		t.Fatalf("physical kills=%d delete calls=%d release calls=%d, want 1 each", physicalKills, deleteCalls, releaseCalls)
	}

	durable, found, readErr := db.ReadRuntimeState(state.InstanceID)
	if readErr != nil || !found || durable.Status != "__agentdeck_stopping" || durable.StatusRevision != state.StatusRevision+1 {
		t.Fatalf("unreleased runtime = %#v, found=%v err=%v", durable, found, readErr)
	}
}

func TestRuntimeLifecycle_RestartFallbackKillsCapturedPredecessor(t *testing.T) {
	inst, db, state := newRuntimeDeleteTestInstance(t)
	var terminated tmux.RuntimeGenerationCandidate
	stubRuntimeDeletionPhysicalWork(t, state, func(got tmux.RuntimeGenerationCandidate, wait bool) error {
		terminated = got
		if wait {
			t.Fatal("restart predecessor unexpectedly requested synchronous termination")
		}
		return nil
	})
	authority := &runtimeTransitionAuthority{
		expected: state, incarnation: inst.PersistenceIncarnation(), durable: true, db: db,
	}
	newer := tmux.ReconnectSessionLazy("mutable-pointer-must-not-win", "newer", "", "", "running")
	newer.SocketName = "other-socket"
	inst.tmuxSession = newer

	if err := inst.terminateTransitionPredecessor(authority); err != nil {
		t.Fatal(err)
	}
	if !runtimeGenerationCandidateMatchesState(terminated, state) {
		t.Fatalf("terminated %#v, want captured predecessor %#v", terminated, state)
	}
	got, found, err := db.ReadRuntimeState(state.InstanceID)
	if err != nil || !found || got.Status != string(StatusStopped) || got.StatusRevision != 2 {
		t.Fatalf("runtime after predecessor kill = %#v, found=%v err=%v", got, found, err)
	}
}

func TestRuntimeLifecycle_RestartFallbackCannotKillReplacement(t *testing.T) {
	inst, db, state := newRuntimeDeleteTestInstance(t)
	terminated := 0
	stubRuntimeDeletionPhysicalWork(t, state, func(tmux.RuntimeGenerationCandidate, bool) error {
		terminated++
		return nil
	})
	authority := &runtimeTransitionAuthority{
		expected: state, incarnation: inst.PersistenceIncarnation(), durable: true, db: db,
	}
	next := state
	next.Generation++
	next.TmuxSession = "runtime-g2"
	if err := db.CommitRuntimeTransition(state.Generation, authority.incarnation, next); err != nil {
		t.Fatal(err)
	}

	if err := inst.terminateTransitionPredecessor(authority); !errors.Is(err, statedb.ErrRuntimeGenerationConflict) {
		t.Fatalf("terminateTransitionPredecessor error = %v, want generation conflict", err)
	}
	if terminated != 0 {
		t.Fatalf("stale restart transition terminated %d runtimes", terminated)
	}
	got, found, err := db.ReadRuntimeState(state.InstanceID)
	if err != nil || !found || got.Generation != next.Generation || got.TmuxSession != next.TmuxSession {
		t.Fatalf("replacement runtime = %#v, found=%v err=%v", got, found, err)
	}
}

func TestRuntimeLifecycle_IdleTimeoutCannotKillReplacementGeneration(t *testing.T) {
	inst, db, state := newRuntimeDeleteTestInstance(t)
	inst.IdleTimeoutSecs = 1
	terminated := 0
	stubRuntimeDeletionPhysicalWork(t, state, func(tmux.RuntimeGenerationCandidate, bool) error {
		terminated++
		return nil
	})
	now := time.Unix(10, 0)
	stopCalls := 0
	logged := 0
	watcher := NewIdleTimeoutWatcher(IdleTimeoutWatcherConfig{
		Now:     func() time.Time { return now },
		Capture: func(*Instance) (string, error) { return "unchanged", nil },
		StopCaptured: func(got *Instance, selection RuntimeSelection) error {
			stopCalls++
			next := state
			next.Generation++
			next.TmuxSession = "runtime-g2"
			if err := db.CommitRuntimeTransition(state.Generation, selection.Incarnation, next); err != nil {
				return err
			}
			return got.KillCaptured(selection)
		},
		LogEvent: func(SessionLifecycleEvent) error {
			logged++
			return nil
		},
	})
	watcher.Tick([]*Instance{inst})
	now = now.Add(2 * time.Second)
	watcher.Tick([]*Instance{inst})

	if stopCalls != 1 {
		t.Fatalf("idle stop calls = %d, want 1", stopCalls)
	}
	if terminated != 0 {
		t.Fatalf("stale idle timeout terminated %d runtimes", terminated)
	}
	if logged != 0 {
		t.Fatalf("failed idle stop logged %d success events", logged)
	}
	got, found, err := db.ReadRuntimeState(state.InstanceID)
	if err != nil || !found || got.Generation != state.Generation+1 || got.TmuxSession != "runtime-g2" {
		t.Fatalf("replacement runtime = %#v, found=%v err=%v", got, found, err)
	}
}
