package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

func TestRuntimeLifecycle_ObservedFullInterleaving(t *testing.T) {
	installRuntimeLifecycleTestSeams(t)
	db, canonical := runtimeLifecycleTestDB(t, "pi", nil)
	storage := &Storage{db: db}

	g0, found, err := db.ReadRuntimeState(canonical.ID)
	if err != nil || !found {
		t.Fatalf("read generation zero: found=%v err=%v", found, err)
	}
	g1 := g0
	g1.Generation = 1
	g1.TmuxSession = "runtime-g1"
	g1.Status = string(StatusError)
	g1.LastStartedAt = time.Unix(200, 123).UTC()
	if err := db.CommitRuntimeTransition(g0.Generation, canonical.PersistenceIncarnation(), g1); err != nil {
		t.Fatal(err)
	}
	canonical.ApplyRuntimeState(g1)

	loadOne := func() *Instance {
		t.Helper()
		instances, _, err := storage.LoadWithGroups()
		if err != nil || len(instances) != 1 {
			t.Fatalf("load: count=%d err=%v", len(instances), err)
		}
		return instances[0]
	}
	staleReload := loadOne()
	staleReload.Title = "metadata-from-stale-reload"
	completionObserver := loadOne()
	reviverObserver := loadOne()

	completionEntered := make(chan struct{})
	releaseCompletion := make(chan struct{})
	oldStatusProbe := statusProbeCandidateOverride
	statusProbeCandidateOverride = func(context.Context, *Instance, statedb.RuntimeState) (Status, error) {
		close(completionEntered)
		<-releaseCompletion
		return StatusStopped, nil
	}
	t.Cleanup(func() { statusProbeCandidateOverride = oldStatusProbe })
	type statusResult struct {
		state statedb.RuntimeState
		err   error
	}
	completionDone := make(chan statusResult, 1)
	go func() {
		state, err := completionObserver.UpdateStatusObserved(
			context.Background(), g1, completionObserver.PersistenceIncarnation(),
		)
		completionDone <- statusResult{state: state, err: err}
	}()
	<-completionEntered

	reviverEntered := make(chan struct{})
	releaseReviver := make(chan struct{})
	reviverActions := 0
	reviver := &Reviver{
		TmuxExists: func(string, string) bool {
			close(reviverEntered)
			<-releaseReviver
			return true
		},
		PipeAlive: func(string) bool { return false },
		ReviveActionIfCurrent: func(*Instance, statedb.RuntimeState, string) error {
			reviverActions++
			return nil
		},
		Acquire: func(string) (func(), error) { return func() {}, nil },
		Now:     func() time.Time { return g1.LastStartedAt.Add(time.Minute) },
	}
	reviverDone := make(chan ReviveOutcome, 1)
	go func() { reviverDone <- reviver.ReviveOne(reviverObserver) }()
	<-reviverEntered

	candidate := func(state statedb.RuntimeState, pid int) tmux.RuntimeCandidate {
		return tmux.RuntimeCandidate{
			SessionName: state.TmuxSession, SocketName: state.TmuxSocketName,
			InstanceID: state.InstanceID, Generation: state.Generation, GenerationKnown: true,
			StatusRevision: state.StatusRevision, Status: state.Status,
			LastStartedUnixNano: state.LastStartedAt.UnixNano(), StateKnown: true,
			BindingKnown: true, PanePID: pid,
		}
	}
	inventory := []tmux.RuntimeCandidate{candidate(g1, 1001)}
	runtimeCandidateInventoryFn = func(socketName, instanceID string) ([]tmux.RuntimeCandidate, error) {
		var matching []tmux.RuntimeCandidate
		for _, item := range inventory {
			if item.SocketName == socketName && item.InstanceID == instanceID {
				matching = append(matching, item)
			}
		}
		return matching, nil
	}

	phase := 0
	advance := func(want int, name string) {
		t.Helper()
		if phase != want {
			t.Fatalf("%s ran at phase %d, want %d", name, phase, want)
		}
		phase++
	}
	g2 := g1
	g2.Generation = 2
	g2.StatusRevision = 0
	g2.TmuxSession = "runtime-g2"
	g2.Status = string(StatusRunning)
	g2.LastStartedAt = time.Unix(300, 456).UTC()
	fakeRestarts := 0
	runtimeTransitionObservedFn = func() {
		advance(0, "restart")
		fakeRestarts++
		if err := db.CommitRuntimeTransition(g1.Generation, canonical.PersistenceIncarnation(), g2); err != nil {
			t.Fatalf("fake restart: %v", err)
		}
		inventory = append(inventory, candidate(g2, 2002))
	}
	got, err := canonical.RestartRuntime()
	if err != nil || got != g2 || fakeRestarts != 1 {
		t.Fatalf("restart: state=%+v count=%d err=%v; want %+v once", got, fakeRestarts, err, g2)
	}

	oldReload := runtimeReloadBeforePublishFn
	runtimeReloadBeforePublishFn = func() { advance(1, "reload") }
	t.Cleanup(func() { runtimeReloadBeforePublishFn = oldReload })
	if !canonical.MergeReloaded(staleReload) {
		t.Fatal("stale reload was not merged")
	}
	if got := canonical.RuntimeState(); got != g2 {
		t.Fatalf("stale reload regressed canonical runtime: %+v", got)
	}

	advance(2, "completion")
	close(releaseCompletion)
	completed := <-completionDone
	if !errors.Is(completed.err, statedb.ErrRuntimeGenerationConflict) || completed.state != g2 {
		t.Fatalf("stale completion: state=%+v err=%v", completed.state, completed.err)
	}

	advance(3, "save")
	if err := storage.SaveWithGroups([]*Instance{canonical}, nil); err != nil {
		t.Fatal(err)
	}

	advance(4, "reviver")
	close(releaseReviver)
	revived := <-reviverDone
	if !revived.Stale || revived.Revived || !errors.Is(revived.Err, statedb.ErrRuntimeGenerationConflict) || reviverActions != 0 {
		t.Fatalf("stale reviver: outcome=%+v actions=%d", revived, reviverActions)
	}

	oldInventory, oldGenerationKill, oldLegacy := runtimeCleanupCandidatesFn, killRuntimeGenerationCandidateFn, killDuplicateSessionsFn
	t.Cleanup(func() {
		runtimeCleanupCandidatesFn = oldInventory
		killRuntimeGenerationCandidateFn = oldGenerationKill
		killDuplicateSessionsFn = oldLegacy
	})
	var killed []string
	legacyKills := 0
	killDuplicateSessionsFn = func(string, string, string) { legacyKills++ }
	runtimeCleanupCandidatesFn = func(socketName, envKey string) ([]tmux.RuntimeBindingCandidate, error) {
		if envKey != "" {
			t.Fatalf("shell cleanup queried binding key %q", envKey)
		}
		var candidates []tmux.RuntimeBindingCandidate
		for _, item := range inventory {
			if item.SocketName != socketName {
				continue
			}
			candidates = append(candidates, tmux.RuntimeBindingCandidate{
				SessionName: item.SessionName, SessionID: "$7", SocketName: item.SocketName,
				PaneID: "%9", PanePID: item.PanePID, InstanceID: item.InstanceID, InstanceKnown: true,
				Generation: item.Generation, GenerationKnown: item.GenerationKnown,
			})
		}
		return candidates, nil
	}
	killRuntimeGenerationCandidateFn = func(candidate tmux.RuntimeGenerationCandidate, wait bool) error {
		advance(5, "sweeper")
		if wait || candidate.SocketName != g1.TmuxSocketName || candidate.InstanceID != g1.InstanceID || candidate.Generation != g1.Generation {
			t.Fatalf("sweep candidate = %#v wait=%v, want G1", candidate, wait)
		}
		remaining := inventory[:0]
		for _, item := range inventory {
			if item.GenerationKnown && item.Generation == candidate.Generation && item.SessionName == candidate.SessionName {
				killed = append(killed, item.SessionName)
				continue
			}
			remaining = append(remaining, item)
		}
		inventory = remaining
		return nil
	}
	canonical.sweepDuplicateToolSessions()
	if legacyKills != 0 || len(killed) != 1 || killed[0] != g1.TmuxSession {
		t.Fatalf("sweep killed=%v legacy=%d; want only stale G1", killed, legacyKills)
	}

	durable, found, err := db.ReadRuntimeState(canonical.ID)
	if err != nil || !found || durable != g2 || canonical.RuntimeState() != g2 {
		t.Fatalf("final DB=%+v found=%v err=%v canonical=%+v; want %+v", durable, found, err, canonical.RuntimeState(), g2)
	}
	if len(inventory) != 1 || inventory[0].Generation != g2.Generation || inventory[0].SessionName != g2.TmuxSession {
		t.Fatalf("final candidate inventory = %+v, want one G2", inventory)
	}
	result, err := canonical.ReconcileRuntime()
	if err != nil || !result.Live || result.Adopted || result.State != g2 || phase != 6 {
		t.Fatalf("final reconciliation=%+v phase=%d err=%v", result, phase, err)
	}
}
