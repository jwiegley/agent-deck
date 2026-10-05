package session

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

func runtimeLifecycleReviverFixture(t *testing.T, status Status, started time.Time) (*statedb.StateDB, *Instance) {
	t.Helper()
	db, err := statedb.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	row := &statedb.InstanceRow{
		ID: "reviver-runtime", Incarnation: "reviver-runtime-incarnation-a",
		Title: "reviver-runtime", ProjectPath: "/tmp",
		Tool: "claude", Status: string(status), CreatedAt: started.Add(-time.Minute),
		LastStartedAt: started,
	}
	if err := db.SaveInstance(row); err != nil {
		t.Fatal(err)
	}
	inst := &Instance{
		ID: "reviver-runtime", Title: "reviver-runtime", ProjectPath: "/tmp",
		Tool: "claude", Status: status, CreatedAt: row.CreatedAt,
		LastStartedAt: started, owningDB: db,
	}
	inst.adoptPersistenceIncarnation(row.Incarnation)
	return db, inst
}

func runtimeLifecycleTestReviver(now time.Time) *Reviver {
	r := NewReviver()
	r.Now = func() time.Time { return now }
	r.TmuxExists = func(string, string) bool { return true }
	r.PipeAlive = func(string) bool { return false }
	r.ConnectPipe = func(string, string) error { return nil }
	r.Acquire = func(string) (func(), error) { return func() {}, nil }
	r.Stagger = 0
	r.Breaker = nil
	r.AuthHeld = nil
	return r
}

func TestRuntimeLifecycle_ReviverGraceDefersFreshMissingPipe(t *testing.T) {
	started := time.Unix(1000, 0).UTC()
	grace := 3 * time.Second
	inst := &Instance{ID: "fresh", Title: "fresh", Status: StatusError, LastStartedAt: started}
	calls := 0
	r := &Reviver{
		Now:            func() time.Time { return started.Add(grace - time.Second) },
		ReadinessGrace: grace,
		TmuxExists:     func(string, string) bool { return true },
		PipeAlive:      func(string) bool { return false },
		ReviveAction:   func(*Instance) error { calls++; return nil },
	}

	out := r.ReviveOne(inst)
	if out.Class != ClassStarting || out.Revived || calls != 0 {
		t.Fatalf("fresh missing pipe = %+v, calls=%d; want starting without action", out, calls)
	}
}

func TestRuntimeLifecycle_ReviverHealsCurrentRuntimeAfterGrace(t *testing.T) {
	started := time.Unix(2000, 0).UTC()
	db, inst := runtimeLifecycleReviverFixture(t, StatusError, started)
	r := runtimeLifecycleTestReviver(started.Add(reviverControlPipeReadyGrace + time.Second))

	out := r.ReviveOne(inst)
	if out.Err != nil || !out.Revived {
		t.Fatalf("ReviveOne = %+v, want successful current-generation heal", out)
	}
	durable, found, err := db.ReadRuntimeState(inst.ID)
	if err != nil || !found {
		t.Fatalf("ReadRuntimeState = found %v, err %v", found, err)
	}
	if durable.Status != string(StatusRunning) || durable.StatusRevision != 1 {
		t.Fatalf("durable runtime = %+v, want running revision 1", durable)
	}
}

func TestRuntimeLifecycle_ReviverRejectsNewerGenerationAfterConnectBarrier(t *testing.T) {
	started := time.Unix(3000, 0).UTC()
	db, inst := runtimeLifecycleReviverFixture(t, StatusError, started)
	r := runtimeLifecycleTestReviver(started.Add(reviverControlPipeReadyGrace + time.Second))
	connectEntered := make(chan struct{})
	resumeConnect := make(chan struct{})
	r.ConnectPipe = func(string, string) error {
		close(connectEntered)
		<-resumeConnect
		return nil
	}
	outcome := make(chan ReviveOutcome, 1)
	go func() { outcome <- r.ReviveOne(inst) }()
	<-connectEntered
	if err := db.CommitRuntimeTransition(0, inst.PersistenceIncarnation(), statedb.RuntimeState{
		InstanceID: inst.ID, Generation: 1, Status: string(StatusStarting),
		LastStartedAt: started.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	close(resumeConnect)
	out := <-outcome
	if out.Revived || !out.Stale || !errors.Is(out.Err, statedb.ErrRuntimeGenerationConflict) {
		t.Fatalf("stale generation outcome = %+v", out)
	}
	durable, _, _ := db.ReadRuntimeState(inst.ID)
	if durable.Generation != 1 || durable.Status != string(StatusStarting) {
		t.Fatalf("new runtime was overwritten: %+v", durable)
	}
}

func TestRuntimeLifecycle_ReviverRejectsNewerGenerationAfterProbeBarrier(t *testing.T) {
	started := time.Unix(3500, 0).UTC()
	db, inst := runtimeLifecycleReviverFixture(t, StatusError, started)
	r := runtimeLifecycleTestReviver(started.Add(reviverControlPipeReadyGrace + time.Second))
	probeEntered := make(chan struct{})
	resumeProbe := make(chan struct{})
	connectCalls := 0
	r.TmuxExists = func(string, string) bool {
		close(probeEntered)
		<-resumeProbe
		return true
	}
	r.ConnectPipe = func(string, string) error {
		connectCalls++
		return nil
	}
	outcome := make(chan ReviveOutcome, 1)
	go func() { outcome <- r.ReviveOne(inst) }()
	<-probeEntered
	if err := db.CommitRuntimeTransition(0, inst.PersistenceIncarnation(), statedb.RuntimeState{
		InstanceID: inst.ID, Generation: 1, Status: string(StatusStarting),
		LastStartedAt: started.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	close(resumeProbe)
	out := <-outcome
	if out.Revived || !out.Stale || !errors.Is(out.Err, statedb.ErrRuntimeGenerationConflict) {
		t.Fatalf("stale probe outcome = %+v", out)
	}
	if connectCalls != 0 {
		t.Fatalf("stale probe performed %d reconnects", connectCalls)
	}
	durable, _, _ := db.ReadRuntimeState(inst.ID)
	if durable.Generation != 1 || durable.Status != string(StatusStarting) {
		t.Fatalf("new runtime was overwritten: %+v", durable)
	}
}

func TestRuntimeLifecycle_ReviverRejectsNewerStatusRevisionAfterConnectBarrier(t *testing.T) {
	started := time.Unix(4000, 0).UTC()
	db, inst := runtimeLifecycleReviverFixture(t, StatusError, started)
	r := runtimeLifecycleTestReviver(started.Add(reviverControlPipeReadyGrace + time.Second))
	connectEntered := make(chan struct{})
	resumeConnect := make(chan struct{})
	r.ConnectPipe = func(string, string) error {
		close(connectEntered)
		<-resumeConnect
		return nil
	}
	outcome := make(chan ReviveOutcome, 1)
	go func() { outcome <- r.ReviveOne(inst) }()
	<-connectEntered
	applied, err := db.WriteStatusIfVersion(inst.ID, inst.PersistenceIncarnation(), 0, 0, string(StatusWaiting))
	if err != nil || !applied {
		t.Fatalf("concurrent status CAS = applied %v, err %v", applied, err)
	}
	close(resumeConnect)
	out := <-outcome
	if out.Revived || !out.Stale || !errors.Is(out.Err, statedb.ErrStatusRevisionConflict) {
		t.Fatalf("stale revision outcome = %+v", out)
	}
	durable, _, _ := db.ReadRuntimeState(inst.ID)
	if durable.Status != string(StatusWaiting) || durable.StatusRevision != 1 {
		t.Fatalf("newer status was overwritten: %+v", durable)
	}
}

func TestRuntimeLifecycle_PersistRevivedConflictReloadsWinner(t *testing.T) {
	started := time.Unix(4500, 0).UTC()
	db, inst := runtimeLifecycleReviverFixture(t, StatusError, started)
	storage := &Storage{db: db}
	inst.Status = StatusRunning
	applied, err := db.WriteStatusIfVersion(inst.ID, inst.PersistenceIncarnation(), 0, 0, string(StatusWaiting))
	if err != nil || !applied {
		t.Fatalf("winner CAS = applied %v, err %v", applied, err)
	}

	err = storage.PersistRevivedInstances([]*Instance{inst})
	if !errors.Is(err, statedb.ErrStatusRevisionConflict) {
		t.Fatalf("PersistRevivedInstances error = %v, want status revision conflict", err)
	}
	current := inst.runtimeStateSnapshot()
	if current.Status != string(StatusWaiting) || current.StatusRevision != 1 {
		t.Fatalf("stale instance did not reload winner: %+v", current)
	}
	durable, _, _ := db.ReadRuntimeState(inst.ID)
	if durable.Status != string(StatusWaiting) || durable.StatusRevision != 1 {
		t.Fatalf("winner was overwritten: %+v", durable)
	}
}

func TestRuntimeLifecycle_PersistRevivedGenerationConflictReloadsWinner(t *testing.T) {
	started := time.Unix(4750, 0).UTC()
	db, inst := runtimeLifecycleReviverFixture(t, StatusError, started)
	storage := &Storage{db: db}
	inst.Status = StatusRunning
	if err := db.CommitRuntimeTransition(0, inst.PersistenceIncarnation(), statedb.RuntimeState{
		InstanceID: inst.ID, Generation: 1, TmuxSession: "replacement",
		TmuxSocketName: "replacement-socket", Status: string(StatusWaiting),
		LastStartedAt: started.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	err := storage.PersistRevivedInstances([]*Instance{inst})
	if !errors.Is(err, statedb.ErrRuntimeGenerationConflict) {
		t.Fatalf("PersistRevivedInstances error = %v, want runtime generation conflict", err)
	}
	current := inst.runtimeStateSnapshot()
	if current.Generation != 1 || current.TmuxSession != "replacement" || current.Status != string(StatusWaiting) {
		t.Fatalf("stale instance did not reload replacement: %+v", current)
	}
	durable, _, _ := db.ReadRuntimeState(inst.ID)
	if current != durable {
		t.Fatalf("reloaded runtime = %+v, durable winner = %+v", current, durable)
	}
}

func TestRuntimeLifecycle_ReviverBreakerResetsAcrossGeneration(t *testing.T) {
	b := NewReviveBreaker(nil)
	for range FutilityThreshold {
		if !b.OnClassifyVersion("same-id", "same", 7, ClassErrored) {
			t.Fatal("generation 7 probe unexpectedly blocked before threshold")
		}
		b.AfterReviveVersion("same-id", "same", 7, errors.New("failed"))
	}
	if b.openCircuits() != 1 {
		t.Fatalf("open circuits = %d, want 1", b.openCircuits())
	}
	if !b.OnClassifyVersion("same-id", "same", 8, ClassErrored) {
		t.Fatal("replacement generation inherited predecessor cooldown")
	}
	if b.openCircuits() != 0 {
		t.Fatalf("replacement generation retained %d open circuits", b.openCircuits())
	}
	b.AfterReviveVersion("same-id", "same", 7, errors.New("late old failure"))
	if !b.OnClassifyVersion("same-id", "same", 8, ClassErrored) || b.openCircuits() != 0 {
		t.Fatal("delayed predecessor work contaminated the replacement breaker")
	}
}

func TestRuntimeLifecycle_StatusCandidatePublishesOnlyAfterCAS(t *testing.T) {
	started := time.Unix(5000, 0).UTC()
	db, inst := runtimeLifecycleReviverFixture(t, StatusError, started)
	observed := inst.runtimeStateSnapshot()
	oldProbe, oldPublish := statusProbeCandidateOverride, statusBeforeMemoryPublishFn
	statusProbeCandidateOverride = func(context.Context, *Instance, statedb.RuntimeState) (Status, error) {
		return StatusRunning, nil
	}
	statusBeforeMemoryPublishFn = func(_ *Instance, next statedb.RuntimeState) {
		durable, found, err := db.ReadRuntimeState(inst.ID)
		if err != nil || !found || !sameStatusRuntime(next, durable) {
			t.Fatalf("memory publication preceded durable CAS: durable=%+v found=%v err=%v next=%+v", durable, found, err, next)
		}
		if inst.Status != StatusError {
			t.Fatalf("candidate was visible before durable CAS: %q", inst.Status)
		}
	}
	t.Cleanup(func() {
		statusProbeCandidateOverride = oldProbe
		statusBeforeMemoryPublishFn = oldPublish
	})
	if _, err := inst.UpdateStatusObserved(context.Background(), observed, inst.PersistenceIncarnation()); err != nil {
		t.Fatal(err)
	}
	if inst.Status != StatusRunning || inst.StatusRevision != 1 {
		t.Fatalf("published instance = status %q revision %d", inst.Status, inst.StatusRevision)
	}
}

func TestRuntimeLifecycle_StatusConflictReloadsWinner(t *testing.T) {
	started := time.Unix(6000, 0).UTC()
	db, inst := runtimeLifecycleReviverFixture(t, StatusError, started)
	observed := inst.runtimeStateSnapshot()
	oldProbe := statusProbeCandidateOverride
	statusProbeCandidateOverride = func(context.Context, *Instance, statedb.RuntimeState) (Status, error) {
		applied, err := db.WriteStatusIfVersion(inst.ID, inst.PersistenceIncarnation(), 0, 0, string(StatusWaiting))
		if err != nil || !applied {
			t.Fatalf("winner CAS = applied %v, err %v", applied, err)
		}
		return StatusRunning, nil
	}
	t.Cleanup(func() { statusProbeCandidateOverride = oldProbe })
	got, err := inst.UpdateStatusObserved(context.Background(), observed, inst.PersistenceIncarnation())
	if !errors.Is(err, statedb.ErrStatusRevisionConflict) {
		t.Fatalf("UpdateStatusObserved error = %v, want revision conflict", err)
	}
	if got.Status != string(StatusWaiting) || inst.Status != StatusWaiting || inst.StatusRevision != 1 {
		t.Fatalf("reloaded winner = %+v, instance status %q revision %d", got, inst.Status, inst.StatusRevision)
	}
}
