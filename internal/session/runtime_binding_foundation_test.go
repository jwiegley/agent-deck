package session

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

func runtimeLifecycleBindings(generation, revision uint64, prefix string) map[string]statedb.RuntimeBinding {
	bindings := make(map[string]statedb.RuntimeBinding, len(runtimeBindingKinds))
	for _, kind := range runtimeBindingKinds {
		bindings[kind] = statedb.RuntimeBinding{
			InstanceID: "one", Kind: kind, Generation: generation,
			Revision: revision, Value: prefix + "-" + kind,
			DetectedAt: time.Unix(int64(revision), 0).UTC(),
		}
	}
	return bindings
}

func TestRuntimeLifecycle_ReloadMergesAllBindingKindsByRevision(t *testing.T) {
	for _, promoted := range runtimeBindingKinds {
		t.Run(promoted, func(t *testing.T) {
			canonical := &Instance{
				ID: "one", Tool: "pi", Status: StatusIdle,
				RuntimeGeneration: 2, StatusRevision: 1,
			}
			canonical.adoptRuntimeBindings(runtimeLifecycleBindings(2, 2, "current"))
			incomingBindings := runtimeLifecycleBindings(2, 1, "stale")
			newer := incomingBindings[promoted]
			newer.Revision = 3
			newer.Value = "new-" + promoted
			incomingBindings[promoted] = newer
			loaded := &Instance{
				ID: "one", Tool: "pi", Status: StatusIdle,
				RuntimeGeneration: 2,
				RuntimeBindings:   incomingBindings,
			}

			if !canonical.MergeReloaded(loaded) {
				t.Fatal("same-generation reload was rejected")
			}
			for _, kind := range runtimeBindingKinds {
				value, _ := canonical.currentRuntimeBinding(kind)
				want := "current-" + kind
				if kind == promoted {
					want = "new-" + kind
				}
				if value != want {
					t.Errorf("%s binding = %q, want %q", kind, value, want)
				}
			}

			higher := &Instance{
				ID: "one", Tool: "pi", Status: StatusIdle,
				RuntimeGeneration: 3,
				RuntimeBindings:   runtimeLifecycleBindings(2, 99, "old-generation"),
			}
			if !canonical.MergeReloaded(higher) {
				t.Fatal("higher-generation reload was rejected")
			}
			if len(canonical.RuntimeBindings) != 0 {
				t.Fatalf("higher generation retained old bindings: %#v", canonical.RuntimeBindings)
			}
			for _, kind := range runtimeBindingKinds {
				if value, _ := canonical.currentRuntimeBinding(kind); value != "" {
					t.Errorf("higher generation retained %s value %q", kind, value)
				}
			}
		})
	}
}

func TestRuntimeLifecycle_LowerGenerationReloadMergesMetadataOnly(t *testing.T) {
	canonical := &Instance{
		ID: "one", Title: "before", Notes: "before", Tool: "claude", Status: StatusRunning,
		RuntimeGeneration: 2, StatusRevision: 4, TmuxSocketName: "current-socket",
		tmuxSession: &tmux.Session{Name: "runtime-g2", SocketName: "current-socket"},
	}
	canonical.adoptRuntimeBindings(runtimeLifecycleBindings(2, 5, "current"))
	loaded := &Instance{
		ID: "one", Title: "edited", Notes: "safe metadata", Tool: "claude", Status: StatusError,
		RuntimeGeneration: 1, StatusRevision: 99, TmuxSocketName: "stale-socket",
		tmuxSession:     &tmux.Session{Name: "runtime-g1", SocketName: "stale-socket"},
		RuntimeBindings: runtimeLifecycleBindings(1, 99, "stale"),
		ClaudeSessionID: "stale-claude",
	}

	if !canonical.MergeReloaded(loaded) {
		t.Fatal("lower-generation metadata merge was rejected")
	}
	if canonical.Title != "edited" || canonical.Notes != "safe metadata" {
		t.Fatalf("safe metadata did not merge: title=%q notes=%q", canonical.Title, canonical.Notes)
	}
	state := canonical.RuntimeState()
	if state.Generation != 2 || state.StatusRevision != 4 || state.Status != string(StatusRunning) ||
		state.TmuxSession != "runtime-g2" || state.TmuxSocketName != "current-socket" {
		t.Fatalf("lower-generation reload changed canonical runtime: %#v", state)
	}
	for _, kind := range runtimeBindingKinds {
		binding := canonical.RuntimeBindings[kind]
		if binding.Generation != 2 || binding.Revision != 5 || binding.Value != "current-"+kind {
			t.Errorf("lower-generation reload changed %s binding: %#v", kind, binding)
		}
	}
}

func TestRuntimeLifecycle_ReloadMergeRejectsDifferentPersistenceIncarnation(t *testing.T) {
	canonical := &Instance{
		ID: "one", Title: "original", Tool: "pi", Status: StatusRunning,
		RuntimeGeneration: 2, StatusRevision: 3,
	}
	canonical.adoptPersistenceIncarnation("incarnation-a")
	loaded := &Instance{
		ID: "one", Title: "replacement", Tool: "pi", Status: StatusRunning,
		RuntimeGeneration: 2, StatusRevision: 3,
	}
	loaded.adoptPersistenceIncarnation("incarnation-b")

	if canonical.MergeReloaded(loaded) {
		t.Fatal("same-ID replacement was merged into the previous incarnation")
	}
	if canonical.Title != "original" || canonical.PersistenceIncarnation() != "incarnation-a" {
		t.Fatalf("rejected reload changed canonical instance: title=%q incarnation=%q",
			canonical.Title, canonical.PersistenceIncarnation())
	}
}

func TestRuntimeLifecycle_OutOfOrderRuntimeApplyCannotRegress(t *testing.T) {
	inst := &Instance{ID: "one", Status: StatusIdle}
	oldHook := runtimeApplyBeforePublishFn
	entered, release := make(chan struct{}), make(chan struct{})
	runtimeApplyBeforePublishFn = func(state statedb.RuntimeState) {
		if state.Generation == 2 {
			close(entered)
			<-release
		}
	}
	t.Cleanup(func() { runtimeApplyBeforePublishFn = oldHook })
	result := make(chan bool, 1)
	go func() {
		result <- inst.ApplyRuntimeState(statedb.RuntimeState{
			InstanceID: "one", Generation: 2, TmuxSession: "runtime-g2", Status: "running",
		})
	}()
	<-entered
	if !inst.ApplyRuntimeState(statedb.RuntimeState{
		InstanceID: "one", Generation: 3, TmuxSession: "runtime-g3", Status: "waiting",
	}) {
		t.Fatal("generation 3 apply was rejected")
	}
	close(release)
	if <-result {
		t.Fatal("delayed generation 2 apply was accepted after generation 3")
	}
	if state := inst.RuntimeState(); state.Generation != 3 || state.TmuxSession != "runtime-g3" {
		t.Fatalf("runtime regressed: %#v", state)
	}
}

func TestRuntimeLifecycle_EqualGenerationCannotReplacePhysicalRuntime(t *testing.T) {
	inst := &Instance{ID: "one", Status: StatusIdle}
	winner := statedb.RuntimeState{
		InstanceID: "one", Generation: 2, StatusRevision: 3,
		TmuxSession: "winner", TmuxSocketName: "socket-winner", Status: "running",
	}
	if !inst.ApplyRuntimeState(winner) {
		t.Fatal("winner apply was rejected")
	}
	for _, revision := range []uint64{3, 4} {
		loser := winner
		loser.StatusRevision = revision
		loser.TmuxSession = "loser"
		loser.TmuxSocketName = "socket-loser"
		if inst.ApplyRuntimeState(loser) {
			t.Fatalf("equal-generation loser at revision %d was accepted", revision)
		}
		if got := inst.RuntimeState(); got != winner {
			t.Fatalf("equal-generation loser changed winner: got %#v want %#v", got, winner)
		}
	}
	refresh := winner
	refresh.StatusRevision = 4
	refresh.Status = "waiting"
	if !inst.ApplyRuntimeState(refresh) {
		t.Fatal("same-identity status refresh was rejected")
	}
	if got := inst.RuntimeState(); got != refresh {
		t.Fatalf("same-identity refresh = %#v, want %#v", got, refresh)
	}
}

func TestRuntimeLifecycle_TransitionDuringReloadKeepsNewRuntimeAndBindings(t *testing.T) {
	canonical := &Instance{ID: "one", Title: "before", Tool: "claude", Status: StatusRunning}
	canonical.adoptRuntimeSnapshot(statedb.RuntimeState{
		InstanceID: "one", Generation: 1, TmuxSession: "runtime-g1", Status: "running",
	}, runtimeLifecycleBindings(1, 1, "generation1"))
	loaded := &Instance{
		ID: "one", Title: "metadata edit", Tool: "claude", Status: StatusError,
		RuntimeGeneration: 1, RuntimeBindings: runtimeLifecycleBindings(1, 2, "reloaded"),
		tmuxSession: &tmux.Session{Name: "runtime-g1"},
	}
	oldHook := runtimeReloadBeforePublishFn
	entered, release := make(chan struct{}), make(chan struct{})
	runtimeReloadBeforePublishFn = func() {
		close(entered)
		<-release
	}
	t.Cleanup(func() { runtimeReloadBeforePublishFn = oldHook })
	merged := make(chan bool, 1)
	go func() { merged <- canonical.MergeReloaded(loaded) }()
	<-entered
	canonical.adoptRuntimeSnapshot(statedb.RuntimeState{
		InstanceID: "one", Generation: 2, TmuxSession: "runtime-g2", Status: "waiting",
	}, runtimeLifecycleBindings(2, 1, "generation2"))
	close(release)
	if !<-merged {
		t.Fatal("reload metadata merge was rejected")
	}
	if state := canonical.RuntimeState(); state.Generation != 2 || state.TmuxSession != "runtime-g2" {
		t.Fatalf("reload regressed committed runtime: %#v", state)
	}
	if canonical.Title != "metadata edit" {
		t.Fatalf("safe metadata did not merge: %q", canonical.Title)
	}
	for _, kind := range runtimeBindingKinds {
		binding := canonical.RuntimeBindings[kind]
		if binding.Generation != 2 || binding.Value != "generation2-"+kind {
			t.Errorf("reload regressed %s binding: %#v", kind, binding)
		}
	}
}

func TestRuntimeLifecycle_MetadataOnlyReloadCannotRestorePredecessorPointer(t *testing.T) {
	canonical := &Instance{ID: "one", Title: "before", Tool: "codex", Status: StatusRunning}
	canonical.adoptRuntimeSnapshot(statedb.RuntimeState{
		InstanceID: "one", Generation: 1, TmuxSession: "predecessor",
		TmuxSocketName: "socket-old", Status: "running",
	}, runtimeLifecycleBindings(1, 1, "predecessor"))
	loaded := &Instance{
		ID: "one", Title: "metadata edit", Tool: "codex", Status: StatusRunning,
		RuntimeGeneration: 1, TmuxSocketName: "socket-old",
		tmuxSession:     &tmux.Session{Name: "predecessor", SocketName: "socket-old"},
		RuntimeBindings: runtimeLifecycleBindings(1, 2, "stale"),
		CodexSessionID:  "stale-codex",
	}

	oldHook := runtimeReloadBeforePublishFn
	entered, release := make(chan struct{}), make(chan struct{})
	runtimeReloadBeforePublishFn = func() {
		close(entered)
		<-release
	}
	t.Cleanup(func() { runtimeReloadBeforePublishFn = oldHook })
	merged := make(chan bool, 1)
	go func() { merged <- canonical.MergeDeferredReload(loaded) }()
	<-entered
	successor := statedb.RuntimeState{
		InstanceID: "one", Generation: 1, TmuxSession: "successor",
		TmuxSocketName: "socket-new", Status: "starting",
	}
	successorBindings := runtimeLifecycleBindings(1, 3, "successor")
	canonical.adoptRuntimeSnapshot(successor, successorBindings)
	close(release)
	if !<-merged {
		t.Fatal("metadata-only reload was rejected")
	}
	if canonical.Title != "metadata edit" {
		t.Fatalf("metadata was not merged: title=%q", canonical.Title)
	}
	if got := canonical.RuntimeState(); got != successor {
		t.Fatalf("metadata-only reload restored predecessor: got %#v want %#v", got, successor)
	}
	if canonical.CodexSessionID != "successor-codex" {
		t.Fatalf("metadata-only reload changed scalar binding to %q", canonical.CodexSessionID)
	}
	for kind, want := range successorBindings {
		if got := canonical.RuntimeBindings[kind]; got != want {
			t.Errorf("metadata-only reload changed %s binding: got %#v want %#v", kind, got, want)
		}
	}
}

func TestRuntimeLifecycle_BindingPlanDropsOldGenerationRows(t *testing.T) {
	inst := &Instance{
		ID: "one", Tool: "claude", RuntimeGeneration: 2,
		RuntimeBindings: runtimeLifecycleBindings(1, 9, "old"),
		ClaudeSessionID: "old-claude",
	}
	bindings, plan, err := inst.readRuntimeBindingPlan(nil, statedb.RuntimeState{
		InstanceID: "one", Generation: 2,
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 0 || len(plan) != 0 {
		t.Fatalf("old-generation bindings exposed: bindings=%#v plan=%#v", bindings, plan)
	}
}

func TestRuntimeLifecycle_PhysicalPlanReleasesEveryInactiveBinding(t *testing.T) {
	tools := map[string]string{
		"claude": "claude", "copilot": "copilot", "codex": "codex",
		"gemini": "gemini", "opencode": "opencode",
	}
	for activeKind, tool := range tools {
		t.Run(activeKind, func(t *testing.T) {
			inst := &Instance{ID: "one", Tool: tool}
			inst.adoptRuntimeBindings(runtimeLifecycleBindings(4, 7, "current"))
			inst.mu.Lock()
			switch activeKind {
			case "claude":
				inst.ClaudeSessionID = "selected"
			case "copilot":
				inst.CopilotSessionID = "selected"
			case "codex":
				inst.CodexSessionID = "selected"
			case "gemini":
				inst.GeminiSessionID = "selected"
			case "opencode":
				inst.OpenCodeSessionID = "selected"
			}
			inst.mu.Unlock()
			authority := &runtimeTransitionAuthority{plan: []statedb.RuntimeBindingTransition{}}
			for _, kind := range runtimeBindingKinds {
				authority.plan = append(authority.plan, statedb.RuntimeBindingTransition{
					Kind: kind, ExpectedRevision: 7, NextValue: "current-" + kind,
				})
			}

			plan := authority.bindingPlanForCommit(inst)
			if len(plan) != len(runtimeBindingKinds) {
				t.Fatalf("plan has %d decisions, want %d", len(plan), len(runtimeBindingKinds))
			}
			for _, item := range plan {
				want := ""
				if item.Kind == activeKind {
					want = "selected"
				}
				if item.NextValue != want || item.ExpectedRevision != 7 {
					t.Errorf("%s decision = %#v, want value %q revision 7", item.Kind, item, want)
				}
			}
		})
	}
}

func TestRuntimeLifecycle_DurableGenerationZeroNeverUsesLegacySweep(t *testing.T) {
	_, inst := runtimeLifecycleTestDB(t, "claude", json.RawMessage(`{"claude_session_id":"conversation"}`))
	inst.ClaudeSessionID = "conversation"
	legacyCalls, generationCalls := 0, 0
	oldLegacy, oldInventory := killDuplicateSessionsFn, runtimeCleanupCandidatesFn
	killDuplicateSessionsFn = func(string, string, string) { legacyCalls++ }
	runtimeCleanupCandidatesFn = func(string, string) ([]tmux.RuntimeBindingCandidate, error) {
		generationCalls++
		return nil, nil
	}
	t.Cleanup(func() {
		killDuplicateSessionsFn = oldLegacy
		runtimeCleanupCandidatesFn = oldInventory
	})

	inst.sweepDuplicateToolSessions()
	if legacyCalls != 0 || generationCalls != 0 {
		t.Fatalf("generation-zero durable sweep called legacy=%d generation-aware=%d", legacyCalls, generationCalls)
	}
}

func TestRuntimeLifecycle_SameInstanceSweepRejectsByteIdenticalIncarnationABA(t *testing.T) {
	db, inst := runtimeLifecycleTestDB(t, "pi", nil)
	incarnationA := inst.PersistenceIncarnation()
	state := statedb.RuntimeState{
		InstanceID: inst.ID, Generation: 1, StatusRevision: 2,
		TmuxSession: "runtime-g1", TmuxSocketName: "isolated",
		Status: "running", LastStartedAt: time.Unix(200, 123).UTC(),
	}
	if err := db.CommitRuntimeTransition(0, incarnationA, state); err != nil {
		t.Fatal(err)
	}
	inst.adoptRuntimeState(state)
	inst.tmuxSession = &tmux.Session{Name: state.TmuxSession, SocketName: state.TmuxSocketName, InstanceID: inst.ID}

	oldCandidates, oldKill, oldReport := runtimeCleanupCandidatesFn, killRuntimeGenerationCandidateFn, runtimeBindingSweepReportFn
	t.Cleanup(func() {
		runtimeCleanupCandidatesFn, killRuntimeGenerationCandidateFn, runtimeBindingSweepReportFn = oldCandidates, oldKill, oldReport
	})
	replaced := false
	runtimeCleanupCandidatesFn = func(socketName, _ string) ([]tmux.RuntimeBindingCandidate, error) {
		if !replaced {
			replaced = true
			if err := db.DeleteInstance(inst.ID); err != nil {
				t.Fatal(err)
			}
			replacement := &statedb.InstanceRow{
				ID: inst.ID, Incarnation: "sweep-incarnation-b", Title: "one",
				ProjectPath: "/tmp/one", GroupPath: "my-sessions", Tool: "pi",
				Status: state.Status, TmuxSession: state.TmuxSession,
				TmuxSocketName: state.TmuxSocketName, RuntimeGeneration: state.Generation,
				StatusRevision: state.StatusRevision, LastStartedAt: state.LastStartedAt,
				CreatedAt: time.Unix(1, 0).UTC(),
			}
			if _, inserted, err := db.InsertInstanceIfAbsent(replacement); err != nil || !inserted {
				t.Fatalf("insert byte-identical B: inserted=%v err=%v", inserted, err)
			}
		}
		if socketName != state.TmuxSocketName {
			return nil, nil
		}
		return []tmux.RuntimeBindingCandidate{{
			SessionName: "runtime-g0", SessionID: "$7", SocketName: socketName,
			PaneID: "%9", PanePID: 3131, InstanceID: inst.ID, InstanceKnown: true,
			Generation: 0, GenerationKnown: true,
		}}, nil
	}
	kills := 0
	killRuntimeGenerationCandidateFn = func(tmux.RuntimeGenerationCandidate, bool) error {
		kills++
		return nil
	}
	var reports []error
	runtimeBindingSweepReportFn = func(_, _, _ string, err error) { reports = append(reports, err) }

	inst.sweepDuplicateToolSessions()

	if kills != 0 || len(reports) != 1 || !errors.Is(reports[0], statedb.ErrInstanceParentConflict) {
		t.Fatalf("stale sweep kills=%d reports=%v, want zero kills and parent conflict", kills, reports)
	}
	parent, err := db.LoadInstanceByID(inst.ID)
	if err != nil || parent == nil || parent.Incarnation != "sweep-incarnation-b" || parent.Incarnation == incarnationA {
		t.Fatalf("replacement parent=%#v err=%v, old incarnation=%q", parent, err, incarnationA)
	}
	durable, found, err := db.ReadRuntimeState(inst.ID)
	if err != nil || !found || durable != state {
		t.Fatalf("replacement runtime=%#v found=%v err=%v, want byte-identical %#v", durable, found, err, state)
	}
}

func prepareRuntimeBindingSweep(t *testing.T) (*statedb.StateDB, *Instance, statedb.RuntimeBinding) {
	return prepareRuntimeBindingSweepOnSocket(t, "isolated")
}

func prepareRuntimeBindingSweepOnSocket(t *testing.T, socketName string) (*statedb.StateDB, *Instance, statedb.RuntimeBinding) {
	t.Helper()
	db, inst := runtimeLifecycleTestDB(t, "claude", json.RawMessage(`{"claude_session_id":"conversation"}`))
	state := statedb.RuntimeState{
		InstanceID: "one", Generation: 1, TmuxSession: "runtime-g1",
		TmuxSocketName: socketName, Status: "starting", LastStartedAt: time.Unix(200, 0).UTC(),
	}
	if err := db.CommitRuntimeTransitionWithBindingPlan(0, inst.PersistenceIncarnation(), state, []statedb.RuntimeBindingTransition{{
		Kind: "claude", ExpectedRevision: 0, NextValue: "conversation",
	}}); err != nil {
		t.Fatal(err)
	}
	binding, found, err := db.ReadRuntimeBinding("one", "claude")
	if err != nil || !found {
		t.Fatalf("ReadRuntimeBinding found=%v err=%v", found, err)
	}
	inst.adoptRuntimeState(state)
	inst.adoptRuntimeBindings(map[string]statedb.RuntimeBinding{"claude": binding})
	inst.tmuxSession = &tmux.Session{Name: state.TmuxSession, SocketName: state.TmuxSocketName, InstanceID: state.InstanceID}
	return db, inst, binding
}

func runtimeBindingSweepCandidate(name, sessionID, paneID, instanceID string, generation uint64) tmux.RuntimeBindingCandidate {
	return runtimeBindingSweepCandidateOnSocket("isolated", name, sessionID, paneID, instanceID, generation)
}

func runtimeBindingSweepCandidateOnSocket(socketName, name, sessionID, paneID, instanceID string, generation uint64) tmux.RuntimeBindingCandidate {
	return tmux.RuntimeBindingCandidate{
		SessionName: name, SessionID: sessionID, SocketName: socketName,
		PaneID: paneID, PanePID: 4000 + int(generation),
		InstanceID: instanceID, InstanceKnown: true,
		Generation: generation, GenerationKnown: true,
		BindingKey: "CLAUDE_SESSION_ID", BindingValue: "conversation",
	}
}

func runtimeBindingSweepInventory(candidates ...tmux.RuntimeBindingCandidate) func(string, string) ([]tmux.RuntimeBindingCandidate, error) {
	return func(socketName, _ string) ([]tmux.RuntimeBindingCandidate, error) {
		var found []tmux.RuntimeBindingCandidate
		for _, candidate := range candidates {
			if candidate.SocketName == socketName {
				found = append(found, candidate)
			}
		}
		return found, nil
	}
}

func installRuntimeBindingSweepSpies(t *testing.T) (*[]tmux.RuntimeBindingCandidate, *int, *bool, *bool) {
	t.Helper()
	oldLock, oldObserved, oldReport := runtimeBindingSweepLockFn, runtimeBindingSweepObservedFn, runtimeBindingSweepReportFn
	oldCandidates, oldTargetLock, oldKill := runtimeCleanupCandidatesFn, runtimeBindingTargetLockFn, killRuntimeBindingCandidateFn
	oldGenerationKill := killRuntimeGenerationCandidateFn
	killed := []tmux.RuntimeBindingCandidate{}
	reports := 0
	lockHeld, targetLockHeld := false, false
	runtimeCleanupCandidatesFn = func(socketName, envKey string) ([]tmux.RuntimeBindingCandidate, error) {
		if envKey != "CLAUDE_SESSION_ID" {
			t.Errorf("binding inventory = socket %q key %q", socketName, envKey)
		}
		if socketName != "isolated" {
			return nil, nil
		}
		return []tmux.RuntimeBindingCandidate{
			runtimeBindingSweepCandidate("agentdeck_other", "$2", "%2", "other", 1),
		}, nil
	}
	runtimeBindingTargetLockFn = func(string) (func(), bool, error) {
		targetLockHeld = true
		return func() { targetLockHeld = false }, true, nil
	}
	killRuntimeBindingCandidateFn = func(candidate tmux.RuntimeBindingCandidate) error {
		if !lockHeld {
			t.Error("tool binding cleanup ran without its binding lock")
		}
		if !targetLockHeld {
			t.Error("tool binding cleanup ran without its target instance lock")
		}
		killed = append(killed, candidate)
		return nil
	}
	killRuntimeGenerationCandidateFn = func(tmux.RuntimeGenerationCandidate, bool) error {
		if !lockHeld {
			t.Error("same-instance cleanup ran without its binding lock")
		}
		return nil
	}
	runtimeBindingSweepLockFn = func(string, string) (func(), error) {
		lockHeld = true
		return func() { lockHeld = false }, nil
	}
	runtimeBindingSweepObservedFn = func() {}
	runtimeBindingSweepReportFn = func(string, string, string, error) { reports++ }
	t.Cleanup(func() {
		runtimeBindingSweepLockFn, runtimeBindingSweepObservedFn, runtimeBindingSweepReportFn = oldLock, oldObserved, oldReport
		runtimeCleanupCandidatesFn, runtimeBindingTargetLockFn, killRuntimeBindingCandidateFn = oldCandidates, oldTargetLock, oldKill
		killRuntimeGenerationCandidateFn = oldGenerationKill
	})
	return &killed, &reports, &lockHeld, &targetLockHeld
}

func TestRuntimeLifecycle_CrossInstanceBindingSweepRequiresUniqueOwnerLease(t *testing.T) {
	_, inst, _ := prepareRuntimeBindingSweep(t)
	killed, reports, lockHeld, targetLockHeld := installRuntimeBindingSweepSpies(t)

	inst.sweepDuplicateToolSessions()

	if len(*killed) != 1 || (*killed)[0].InstanceID != "other" || *reports != 0 || *lockHeld || *targetLockHeld {
		t.Fatalf("sweep result killed=%#v reports=%d bindingLockHeld=%v targetLockHeld=%v", *killed, *reports, *lockHeld, *targetLockHeld)
	}
}

func TestRuntimeLifecycle_CrossInstanceBindingSweepPreservesCurrentReboundTarget(t *testing.T) {
	db, inst, _ := prepareRuntimeBindingSweep(t)
	targetState := statedb.RuntimeState{
		InstanceID: "other", Generation: 1, StatusRevision: 1,
		TmuxSession: "agentdeck_other", TmuxSocketName: "isolated",
		Status: "running", LastStartedAt: time.Unix(210, 0).UTC(),
	}
	targetBinding := statedb.RuntimeBinding{
		InstanceID: targetState.InstanceID, Kind: "claude", Generation: targetState.Generation,
		Revision: 2, Value: "rebound-conversation", DetectedAt: time.Unix(211, 0).UTC(),
	}
	_, inserted, err := db.InsertInstanceIfAbsent(&statedb.InstanceRow{
		ID: targetState.InstanceID, Incarnation: "other-incarnation", Title: "other",
		ProjectPath: "/tmp/other", GroupPath: "my-sessions", Tool: "claude",
		Status: targetState.Status, TmuxSession: targetState.TmuxSession,
		TmuxSocketName: targetState.TmuxSocketName, RuntimeGeneration: targetState.Generation,
		StatusRevision: targetState.StatusRevision, LastStartedAt: targetState.LastStartedAt,
		CreatedAt:       time.Unix(100, 0).UTC(),
		RuntimeBindings: map[string]statedb.RuntimeBinding{"claude": targetBinding},
	})
	if err != nil || !inserted {
		t.Fatalf("insert rebound target: inserted=%v err=%v", inserted, err)
	}

	// Inventory still advertises target T's old A stamp ("conversation"),
	// while T's exact current durable runtime now owns B. Source S owns the
	// released A. The final lease must preserve T before invoking the kill seam.
	killed, reports, lockHeld, targetLockHeld := installRuntimeBindingSweepSpies(t)
	inst.sweepDuplicateToolSessions()

	if len(*killed) != 0 || *reports != 1 || *lockHeld || *targetLockHeld {
		t.Fatalf("rebound target sweep killed=%#v reports=%d bindingLockHeld=%v targetLockHeld=%v",
			*killed, *reports, *lockHeld, *targetLockHeld)
	}
	durable, found, readErr := db.ReadRuntimeBinding(targetState.InstanceID, "claude")
	if readErr != nil || !found || durable != targetBinding {
		t.Fatalf("rebound target binding = %+v found=%v err=%v, want %+v", durable, found, readErr, targetBinding)
	}
}

func TestRuntimeLifecycle_BindingSweepReusesTwoSocketInventoryForLowerAndForeignCleanup(t *testing.T) {
	_, inst, _ := prepareRuntimeBindingSweep(t)
	killed, reports, lockHeld, _ := installRuntimeBindingSweepSpies(t)
	oldDefault := tmux.DefaultSocketName()
	t.Cleanup(func() { tmux.SetDefaultSocketName(oldDefault) })
	tmux.SetDefaultSocketName("configured-socket")

	lower := runtimeBindingSweepCandidateOnSocket(
		"old-socket", inst.tmuxSession.Name, "$3", "%3", inst.ID, 0)
	lower.BindingValue = ""
	foreign := runtimeBindingSweepCandidateOnSocket(
		"old-socket", "agentdeck_foreign", "$4", "%4", "other", 1)
	calls := make(map[string]int)
	runtimeCleanupCandidatesFn = func(socketName, envKey string) ([]tmux.RuntimeBindingCandidate, error) {
		calls[socketName]++
		if envKey != "CLAUDE_SESSION_ID" {
			t.Fatalf("inventory key = %q", envKey)
		}
		if socketName == "old-socket" {
			return []tmux.RuntimeBindingCandidate{lower, foreign}, nil
		}
		return nil, nil
	}
	var lowerKills []tmux.RuntimeGenerationCandidate
	killRuntimeGenerationCandidateFn = func(candidate tmux.RuntimeGenerationCandidate, wait bool) error {
		if !*lockHeld || wait {
			t.Fatalf("lower cleanup lock=%v wait=%v", *lockHeld, wait)
		}
		lowerKills = append(lowerKills, candidate)
		return nil
	}

	inst.sweepDuplicateToolSessions("old-socket")

	for _, socketName := range []string{"isolated", "configured-socket", "", "old-socket"} {
		if calls[socketName] != 1 {
			t.Errorf("inventory calls for socket %q = %d, want 1", socketName, calls[socketName])
		}
	}
	if len(lowerKills) != 1 || lowerKills[0].SocketName != "old-socket" ||
		lowerKills[0].SessionName != inst.tmuxSession.Name || lowerKills[0].Generation != 0 {
		t.Fatalf("lower-generation kills = %#v", lowerKills)
	}
	if len(*killed) != 1 || (*killed)[0] != foreign || *reports != 0 {
		t.Fatalf("foreign kills=%#v reports=%d", *killed, *reports)
	}
}

func TestRuntimeLifecycle_BindingSweepFailedSocketPreservesEarlierCandidates(t *testing.T) {
	_, inst, _ := prepareRuntimeBindingSweep(t)
	killed, reports, _, _ := installRuntimeBindingSweepSpies(t)
	wantErr := errors.New("second socket unavailable")
	foreign := runtimeBindingSweepCandidate("agentdeck_foreign", "$4", "%4", "other", 1)
	runtimeCleanupCandidatesFn = func(socketName, _ string) ([]tmux.RuntimeBindingCandidate, error) {
		switch socketName {
		case "isolated":
			return []tmux.RuntimeBindingCandidate{foreign}, nil
		case "failed-socket":
			return nil, wantErr
		default:
			return nil, nil
		}
	}

	inst.sweepDuplicateToolSessions("failed-socket")

	if len(*killed) != 0 || *reports != 1 {
		t.Fatalf("failed multi-socket inventory killed=%#v reports=%d", *killed, *reports)
	}
}

func TestRuntimeLifecycle_BindingSweepPreservesNativeDefaultSocketAfterConfigChange(t *testing.T) {
	_, inst, _ := prepareRuntimeBindingSweepOnSocket(t, "")
	killed, reports, _, _ := installRuntimeBindingSweepSpies(t)
	oldDefault := tmux.DefaultSocketName()
	t.Cleanup(func() { tmux.SetDefaultSocketName(oldDefault) })
	tmux.SetDefaultSocketName("configured-socket")

	foreign := runtimeBindingSweepCandidateOnSocket(
		"", "agentdeck_native_foreign", "$4", "%4", "other", 1)
	calls := make(map[string]int)
	runtimeCleanupCandidatesFn = func(socketName, envKey string) ([]tmux.RuntimeBindingCandidate, error) {
		calls[socketName]++
		if envKey != "CLAUDE_SESSION_ID" {
			t.Fatalf("inventory key = %q", envKey)
		}
		if socketName == "" {
			return []tmux.RuntimeBindingCandidate{foreign}, nil
		}
		return nil, nil
	}

	inst.sweepDuplicateToolSessions()

	if calls[""] != 1 || calls["configured-socket"] != 1 {
		t.Fatalf("inventory calls = %#v, want native and configured once", calls)
	}
	if len(*killed) != 1 || (*killed)[0].SocketName != "" || *reports != 0 {
		t.Fatalf("native binding sweep killed=%#v reports=%d", *killed, *reports)
	}
}

func TestRuntimeLifecycle_CrossInstanceBindingChangePreservesPeers(t *testing.T) {
	db, inst, binding := prepareRuntimeBindingSweep(t)
	killed, reports, _, _ := installRuntimeBindingSweepSpies(t)
	lowerGenerationKills := 0
	lower := runtimeBindingSweepCandidate("agentdeck_lower", "$1", "%1", "one", 0)
	lower.BindingValue = ""
	runtimeCleanupCandidatesFn = runtimeBindingSweepInventory(
		lower,
		runtimeBindingSweepCandidate("agentdeck_other", "$2", "%2", "other", 1),
	)
	killRuntimeGenerationCandidateFn = func(tmux.RuntimeGenerationCandidate, bool) error {
		lowerGenerationKills++
		return nil
	}
	runtimeBindingSweepObservedFn = func() {
		if _, err := db.CommitRuntimeBinding("one", inst.PersistenceIncarnation(), 1, "claude", binding.Revision, ""); err != nil {
			t.Fatal(err)
		}
	}

	inst.sweepDuplicateToolSessions()

	if len(*killed) != 0 || lowerGenerationKills != 1 || *reports != 1 {
		t.Fatalf("changed owner sweep result killed=%#v lowerKills=%d reports=%d", *killed, lowerGenerationKills, *reports)
	}
}

func TestRuntimeLifecycle_CrossInstanceBindingSweepExcludesSameInstance(t *testing.T) {
	_, inst, _ := prepareRuntimeBindingSweep(t)
	killed, reports, _, _ := installRuntimeBindingSweepSpies(t)
	runtimeCleanupCandidatesFn = runtimeBindingSweepInventory(
		runtimeBindingSweepCandidate("agentdeck_equal", "$2", "%2", "one", 1),
		runtimeBindingSweepCandidate("agentdeck_higher", "$3", "%3", "one", 2),
		runtimeBindingSweepCandidate("agentdeck_other", "$4", "%4", "other", 7),
	)

	inst.sweepDuplicateToolSessions()

	if len(*killed) != 1 || (*killed)[0].SessionName != "agentdeck_other" {
		t.Fatalf("unsafe binding cleanup targets: %#v", *killed)
	}
	if *reports != 0 {
		t.Fatalf("binding cleanup reports = %d, want 0", *reports)
	}
}

func TestRuntimeLifecycle_CrossInstanceBindingAmbiguityPreservesEveryCandidate(t *testing.T) {
	_, inst, _ := prepareRuntimeBindingSweep(t)
	killed, reports, _, _ := installRuntimeBindingSweepSpies(t)
	unknownGeneration := runtimeBindingSweepCandidate("agentdeck_unknown_generation", "$3", "%3", "one", 1)
	unknownGeneration.Generation, unknownGeneration.GenerationKnown = 0, false
	unknownIdentity := runtimeBindingSweepCandidate("agentdeck_unknown_identity", "$4", "%4", "unknown", 1)
	unknownIdentity.InstanceID, unknownIdentity.InstanceKnown = "", false
	runtimeCleanupCandidatesFn = runtimeBindingSweepInventory(
		runtimeBindingSweepCandidate("agentdeck_other", "$2", "%2", "other", 7),
		unknownGeneration,
		unknownIdentity,
	)

	inst.sweepDuplicateToolSessions()

	if len(*killed) != 0 {
		t.Fatalf("ambiguous inventory killed candidates: %#v", *killed)
	}
	if *reports != 1 {
		t.Fatalf("ambiguous inventory reports = %d, want 1", *reports)
	}
}

func TestRuntimeLifecycle_CrossInstanceBindingBusyTargetLockPreservesPeers(t *testing.T) {
	_, inst, _ := prepareRuntimeBindingSweep(t)
	killed, reports, _, _ := installRuntimeBindingSweepSpies(t)
	targetAttempts := 0
	runtimeBindingTargetLockFn = func(instanceID string) (func(), bool, error) {
		targetAttempts++
		if instanceID != "other" {
			t.Errorf("target lock instance = %q, want other", instanceID)
		}
		return nil, false, nil
	}

	inst.sweepDuplicateToolSessions()

	if targetAttempts != 1 || len(*killed) != 0 || *reports != 1 {
		t.Fatalf("busy target sweep attempts=%d killed=%#v reports=%d", targetAttempts, *killed, *reports)
	}
}

func TestRuntimeLifecycle_CrossInstanceBindingInventoryAndTargetTryLockDoNotWaitForWriter(t *testing.T) {
	db, inst, _ := prepareRuntimeBindingSweep(t)
	killed, reports, _, _ := installRuntimeBindingSweepSpies(t)
	tx, err := db.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO metadata (key, value) VALUES ('sweep-writer', 'held')`); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}

	targetReached := make(chan struct{})
	runtimeBindingTargetLockFn = func(string) (func(), bool, error) {
		close(targetReached)
		return nil, false, nil
	}
	done := make(chan struct{})
	go func() {
		inst.sweepDuplicateToolSessions()
		close(done)
	}()

	select {
	case <-targetReached:
	case <-time.After(2 * time.Second):
		_ = tx.Rollback()
		<-done
		t.Fatal("inventory and target try-lock were blocked behind the owner lease")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = tx.Rollback()
		t.Fatal("busy target lock did not abort while an unrelated writer held SQLite")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if len(*killed) != 0 || *reports != 1 {
		t.Fatalf("writer-held sweep killed=%#v reports=%d", *killed, *reports)
	}
}

func TestRuntimeLifecycle_CrossInstanceBindingReplacementAfterInventoryIsPreserved(t *testing.T) {
	_, inst, _ := prepareRuntimeBindingSweep(t)
	_, reports, lockHeld, targetLockHeld := installRuntimeBindingSweepSpies(t)
	original := runtimeBindingSweepCandidate("agentdeck_other", "$2", "%2", "other", 1)
	current := original
	runtimeCleanupCandidatesFn = runtimeBindingSweepInventory(original)
	runtimeBindingTargetLockFn = func(string) (func(), bool, error) {
		current.SessionID, current.PaneID, current.PanePID = "$9", "%9", 9009
		*targetLockHeld = true
		return func() { *targetLockHeld = false }, true, nil
	}
	kills := 0
	killRuntimeBindingCandidateFn = func(candidate tmux.RuntimeBindingCandidate) error {
		if !*lockHeld || !*targetLockHeld {
			t.Error("conditional cleanup ran without both sweep locks")
		}
		if candidate != current {
			return tmux.ErrRuntimeBindingCandidateChanged
		}
		kills++
		return nil
	}

	inst.sweepDuplicateToolSessions()

	if kills != 0 || *reports != 1 || *lockHeld || *targetLockHeld {
		t.Fatalf("replacement sweep kills=%d reports=%d bindingLockHeld=%v targetLockHeld=%v", kills, *reports, *lockHeld, *targetLockHeld)
	}
}

func TestRuntimeLifecycle_CrossInstanceBindingOwnerChangeBetweenKillsAborts(t *testing.T) {
	db, inst, binding := prepareRuntimeBindingSweep(t)
	killed, reports, _, targetLockHeld := installRuntimeBindingSweepSpies(t)
	first := runtimeBindingSweepCandidate("agentdeck_other_a", "$2", "%2", "other-a", 1)
	second := runtimeBindingSweepCandidate("agentdeck_other_b", "$3", "%3", "other-b", 1)
	runtimeCleanupCandidatesFn = runtimeBindingSweepInventory(first, second)
	releases := 0
	runtimeBindingTargetLockFn = func(string) (func(), bool, error) {
		*targetLockHeld = true
		return func() {
			*targetLockHeld = false
			releases++
			if releases == 1 {
				if _, err := db.CommitRuntimeBinding("one", inst.PersistenceIncarnation(), 1, "claude", binding.Revision, ""); err != nil {
					t.Fatal(err)
				}
			}
		}, true, nil
	}

	inst.sweepDuplicateToolSessions()

	if len(*killed) != 1 || (*killed)[0] != first || releases != 2 || *reports != 1 || *targetLockHeld {
		t.Fatalf("owner-change sweep killed=%#v releases=%d reports=%d targetLockHeld=%v", *killed, releases, *reports, *targetLockHeld)
	}
}

func TestRuntimeLifecycle_NoDatabaseSweepKillsNothing(t *testing.T) {
	inst := NewInstanceWithTool("one", "/tmp/one", "claude")
	inst.ClaudeSessionID = "conversation"
	inst.tmuxSession = &tmux.Session{Name: "runtime"}
	oldKill, oldLegacy := killDuplicateSessionsFn, legacyDuplicateSweepForTests
	kills := 0
	killDuplicateSessionsFn = func(string, string, string) { kills++ }
	legacyDuplicateSweepForTests = false
	t.Cleanup(func() {
		killDuplicateSessionsFn, legacyDuplicateSweepForTests = oldKill, oldLegacy
	})

	inst.sweepDuplicateToolSessions()

	if kills != 0 {
		t.Fatalf("unfenced no-database sweep killed %d candidates", kills)
	}
}
