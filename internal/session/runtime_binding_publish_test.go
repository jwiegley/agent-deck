package session

import (
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

func runtimeBindingTestStorage(t *testing.T) *Storage {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := statedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	return &Storage{db: db, dbPath: path, profile: "binding-test"}
}

func runtimeBindingTestInstance(id string) *Instance {
	return &Instance{
		ID: id, Title: id, ProjectPath: "/tmp/" + id, GroupPath: "tests",
		Command: "shell", Tool: "shell", Status: StatusIdle,
		CreatedAt: time.Unix(100, 0).UTC(),
	}
}

func runtimeBindingValues(inst *Instance) map[string]string {
	return map[string]string{
		"claude": inst.ClaudeSessionID, "copilot": inst.CopilotSessionID,
		"codex": inst.CodexSessionID, "gemini": inst.GeminiSessionID,
		"opencode": inst.OpenCodeSessionID,
	}
}

func runtimeBindingDataValues(inst *InstanceData) map[string]string {
	return map[string]string{
		"claude": inst.ClaudeSessionID, "copilot": inst.CopilotSessionID,
		"codex": inst.CodexSessionID, "gemini": inst.GeminiSessionID,
		"opencode": inst.OpenCodeSessionID,
	}
}

func TestRuntimeLifecycle_BindingPublishAllKinds(t *testing.T) {
	cases := []struct {
		kind, first, second string
	}{
		{"claude", "claude-one", "claude-two"},
		{"copilot", "copilot-one", "copilot-two"},
		{"codex", "11111111-2222-3333-4444-555555555555", "22222222-3333-4444-5555-666666666666"},
		{"gemini", "gemini-one", "gemini-two"},
		{"opencode", "opencode-one", "opencode-two"},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			storage := runtimeBindingTestStorage(t)
			inst := runtimeBindingTestInstance("publish-" + tc.kind)
			if err := storage.InsertSessionAndVerify(inst, nil); err != nil {
				t.Fatal(err)
			}

			t1 := time.Unix(1001, 1).UTC()
			t2 := time.Unix(1002, 2).UTC()
			t3 := time.Unix(1003, 3).UTC()
			memoryPublishes := 0
			oldBeforeMemory := runtimeBindingBeforeMemoryPublishFn
			runtimeBindingBeforeMemoryPublishFn = func(got *Instance, binding statedb.RuntimeBinding) {
				memoryPublishes++
				durable, found, err := storage.db.ReadRuntimeBinding(inst.ID, tc.kind)
				if err != nil || !found || durable != binding {
					t.Fatalf("durable binding before memory publish = %+v found=%v err=%v, want %+v", durable, found, err, binding)
				}
				if value := runtimeBindingValues(got)[tc.kind]; value == binding.Value {
					t.Fatalf("in-memory %s binding became visible before durable publish seam", tc.kind)
				}
			}
			t.Cleanup(func() { runtimeBindingBeforeMemoryPublishFn = oldBeforeMemory })

			if err := inst.PublishRuntimeBindingObservation(
				inst.CaptureRuntimeBindingObservation(tc.kind), tc.first, t1); err != nil {
				t.Fatal(err)
			}
			first, found, err := storage.db.ReadRuntimeBinding(inst.ID, tc.kind)
			if err != nil || !found {
				t.Fatalf("ReadRuntimeBinding(%s) found=%v err=%v", tc.kind, found, err)
			}
			if first.Value != tc.first || first.Generation != 0 || first.Revision != 1 || !first.DetectedAt.Equal(t1) {
				t.Fatalf("first durable %s binding = %+v", tc.kind, first)
			}
			if got := runtimeBindingValues(inst)[tc.kind]; got != tc.first {
				t.Fatalf("in-memory %s binding = %q, want %q", tc.kind, got, tc.first)
			}

			if err := inst.PublishRuntimeBindingObservation(
				inst.CaptureRuntimeBindingObservation(tc.kind), tc.first, t2); err != nil {
				t.Fatal(err)
			}
			unchanged, found, err := storage.db.ReadRuntimeBinding(inst.ID, tc.kind)
			if err != nil || !found || unchanged != first || memoryPublishes != 1 {
				t.Fatalf("same-value publish changed state: binding=%+v first=%+v memory_publishes=%d found=%v err=%v",
					unchanged, first, memoryPublishes, found, err)
			}

			if err := inst.PublishRuntimeBindingObservation(
				inst.CaptureRuntimeBindingObservation(tc.kind), tc.second, t3); err != nil {
				t.Fatal(err)
			}
			second, found, err := storage.db.ReadRuntimeBinding(inst.ID, tc.kind)
			if err != nil || !found || second.Value != tc.second || second.Generation != 0 ||
				second.Revision != 2 || !second.DetectedAt.Equal(t3) || memoryPublishes != 2 {
				t.Fatalf("second durable %s binding = %+v memory_publishes=%d found=%v err=%v",
					tc.kind, second, memoryPublishes, found, err)
			}
		})
	}
}

func TestRuntimeLifecycle_BindingPublishRestampsCleanupAroundDurableCommit(t *testing.T) {
	db, inst, _ := prepareRuntimeBindingSweep(t)
	oldAcquire := instanceSpawnLockAcquireFn
	oldInvalidate := runtimeBindingCleanupInvalidateFn
	oldStamp := runtimeCleanupIdentityStampFn
	oldSetEnv := runtimeCandidateSetEnvFn
	t.Cleanup(func() {
		instanceSpawnLockAcquireFn = oldAcquire
		runtimeBindingCleanupInvalidateFn = oldInvalidate
		runtimeCleanupIdentityStampFn = oldStamp
		runtimeCandidateSetEnvFn = oldSetEnv
	})

	lockHeld := false
	instanceSpawnLockAcquireFn = func(instanceID string) (func(), error) {
		if instanceID != inst.ID || lockHeld {
			t.Fatalf("cleanup restamp lock acquisition instance=%q held=%v", instanceID, lockHeld)
		}
		lockHeld = true
		return func() { lockHeld = false }, nil
	}
	var order []string
	runtimeBindingCleanupInvalidateFn = func(session *tmux.Session) error {
		if !lockHeld || session != inst.tmuxSession {
			t.Fatalf("cleanup invalidation lock=%v session=%p want=%p", lockHeld, session, inst.tmuxSession)
		}
		binding, found, err := db.ReadRuntimeBinding(inst.ID, "claude")
		if err != nil || !found || binding.Value != "conversation" {
			t.Fatalf("binding at invalidation = %+v found=%v err=%v", binding, found, err)
		}
		order = append(order, "invalidate")
		return nil
	}
	runtimeCleanupIdentityStampFn = func(session *tmux.Session, instanceID string, generation uint64, key, value string) error {
		if !lockHeld || session != inst.tmuxSession || instanceID != inst.ID || generation != 1 ||
			key != "CLAUDE_SESSION_ID" || value != "rebound" {
			t.Fatalf("cleanup stamp lock=%v session=%p instance=%q generation=%d key=%q value=%q",
				lockHeld, session, instanceID, generation, key, value)
		}
		binding, found, err := db.ReadRuntimeBinding(inst.ID, "claude")
		if err != nil || !found || binding.Value != "rebound" || binding.Revision != 2 {
			t.Fatalf("binding at restamp = %+v found=%v err=%v", binding, found, err)
		}
		order = append(order, "stamp")
		return nil
	}
	runtimeCandidateSetEnvFn = func(session *tmux.Session, key, value string) error {
		if !lockHeld || session != inst.tmuxSession || key != "AGENTDECK_RUNTIME_GENERATION" || value != "1" {
			t.Fatalf("cleanup completeness lock=%v session=%p key=%q value=%q", lockHeld, session, key, value)
		}
		order = append(order, "complete")
		return nil
	}

	if err := inst.PublishRuntimeBindingObservation(
		inst.CaptureRuntimeBindingObservation("claude"), "rebound", time.Unix(1010, 0).UTC(),
	); err != nil {
		t.Fatal(err)
	}
	if lockHeld || strings.Join(order, ",") != "invalidate,stamp,complete" {
		t.Fatalf("cleanup publication lock=%v order=%q", lockHeld, order)
	}
	if got := inst.RuntimeBindings["claude"]; got.Value != "rebound" || got.Revision != 2 {
		t.Fatalf("published binding = %+v", got)
	}
}

func TestRuntimeLifecycle_BindingPublishRestampFailureRemainsFailClosed(t *testing.T) {
	db, inst, _ := prepareRuntimeBindingSweep(t)
	oldAcquire := instanceSpawnLockAcquireFn
	oldInvalidate := runtimeBindingCleanupInvalidateFn
	oldStamp := runtimeCleanupIdentityStampFn
	oldSetEnv := runtimeCandidateSetEnvFn
	t.Cleanup(func() {
		instanceSpawnLockAcquireFn = oldAcquire
		runtimeBindingCleanupInvalidateFn = oldInvalidate
		runtimeCleanupIdentityStampFn = oldStamp
		runtimeCandidateSetEnvFn = oldSetEnv
	})

	lockHeld := false
	instanceSpawnLockAcquireFn = func(string) (func(), error) {
		lockHeld = true
		return func() { lockHeld = false }, nil
	}
	invalidations, stamps, completions := 0, 0, 0
	runtimeBindingCleanupInvalidateFn = func(*tmux.Session) error {
		if !lockHeld {
			t.Fatal("cleanup invalidation ran outside the lifecycle lock")
		}
		invalidations++
		return nil
	}
	wantErr := errors.New("restamp failed")
	runtimeCleanupIdentityStampFn = func(*tmux.Session, string, uint64, string, string) error {
		if !lockHeld {
			t.Fatal("cleanup restamp ran outside the lifecycle lock")
		}
		stamps++
		return wantErr
	}
	runtimeCandidateSetEnvFn = func(*tmux.Session, string, string) error {
		completions++
		return nil
	}

	err := inst.PublishRuntimeBindingObservation(
		inst.CaptureRuntimeBindingObservation("claude"), "rebound", time.Unix(1020, 0).UTC(),
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("restamp failure = %v, want %v", err, wantErr)
	}
	if lockHeld || invalidations != 1 || stamps != 1 || completions != 0 {
		t.Fatalf("failed restamp lock=%v invalidations=%d stamps=%d completions=%d",
			lockHeld, invalidations, stamps, completions)
	}
	durable, found, readErr := db.ReadRuntimeBinding(inst.ID, "claude")
	if readErr != nil || !found || durable.Value != "rebound" || durable.Revision != 2 {
		t.Fatalf("durable binding after restamp failure = %+v found=%v err=%v", durable, found, readErr)
	}
	if got := inst.RuntimeBindings["claude"]; got != durable {
		t.Fatalf("memory binding after restamp failure = %+v, want durable %+v", got, durable)
	}
}

func TestRuntimeLifecycle_StaleBindingPublisherCannotInvalidateNewGeneration(t *testing.T) {
	db, stale, binding := prepareRuntimeBindingSweep(t)
	observation := stale.CaptureRuntimeBindingObservation("claude")
	next := stale.RuntimeState()
	next.Generation = 2
	next.StatusRevision++
	if err := db.CommitRuntimeTransitionWithBindingPlan(1, stale.PersistenceIncarnation(), next, []statedb.RuntimeBindingTransition{{
		Kind: "claude", ExpectedRevision: binding.Revision,
		NextValue: binding.Value, DetectedAt: binding.DetectedAt,
	}}); err != nil {
		t.Fatal(err)
	}

	oldInvalidate := runtimeBindingCleanupInvalidateFn
	oldStamp := runtimeCleanupIdentityStampFn
	oldSetEnv := runtimeCandidateSetEnvFn
	t.Cleanup(func() {
		runtimeBindingCleanupInvalidateFn = oldInvalidate
		runtimeCleanupIdentityStampFn = oldStamp
		runtimeCandidateSetEnvFn = oldSetEnv
	})
	invalidations, stamps, completions := 0, 0, 0
	runtimeBindingCleanupInvalidateFn = func(*tmux.Session) error { invalidations++; return nil }
	runtimeCleanupIdentityStampFn = func(*tmux.Session, string, uint64, string, string) error {
		stamps++
		return nil
	}
	runtimeCandidateSetEnvFn = func(*tmux.Session, string, string) error { completions++; return nil }

	err := stale.PublishRuntimeBindingObservation(observation, "late-generation-one", time.Unix(1030, 0).UTC())
	if !errors.Is(err, statedb.ErrRuntimeGenerationConflict) {
		t.Fatalf("stale publisher error = %v, want generation conflict", err)
	}
	if invalidations != 0 || stamps != 0 || completions != 0 {
		t.Fatalf("stale publisher touched cleanup authority: invalidations=%d stamps=%d completions=%d",
			invalidations, stamps, completions)
	}
	durable, found, readErr := db.ReadRuntimeBinding(stale.ID, "claude")
	if readErr != nil || !found || durable.Generation != 2 || durable.Value != binding.Value {
		t.Fatalf("generation-two binding = %+v found=%v err=%v", durable, found, readErr)
	}
}

func TestRuntimeLifecycle_BindingObservationRejectsByteIdenticalIncarnationABA(t *testing.T) {
	t.Run("write", func(t *testing.T) {
		storage := runtimeBindingTestStorage(t)
		stale := runtimeBindingTestInstance("binding-incarnation-write-aba")
		if err := storage.InsertSessionAndVerify(stale, nil); err != nil {
			t.Fatal(err)
		}
		observation := stale.CaptureRuntimeBindingObservation("copilot")
		incarnationA := stale.PersistenceIncarnation()
		if err := storage.db.DeleteInstance(stale.ID); err != nil {
			t.Fatal(err)
		}
		winner := runtimeBindingTestInstance(stale.ID)
		if err := storage.InsertSessionAndVerify(winner, nil); err != nil {
			t.Fatal(err)
		}
		if winner.PersistenceIncarnation() == incarnationA {
			t.Fatalf("byte-identical B reused A incarnation %q", incarnationA)
		}

		err := stale.PublishRuntimeBindingObservation(
			observation, "late-a-binding", time.Unix(1000, 0).UTC(),
		)
		if !errors.Is(err, statedb.ErrInstanceParentConflict) {
			t.Fatalf("stale A observation error = %v, want parent conflict", err)
		}
		if binding, found, err := storage.db.ReadRuntimeBinding(winner.ID, "copilot"); err != nil || found {
			t.Fatalf("stale A wrote B binding = %#v found=%v err=%v", binding, found, err)
		}
	})

	t.Run("same value and validated hook cache", func(t *testing.T) {
		storage := runtimeBindingTestStorage(t)
		stale := runtimeBindingTestInstance("binding-incarnation-cache-aba")
		if err := storage.InsertSessionAndVerify(stale, nil); err != nil {
			t.Fatal(err)
		}
		fingerprint := hookStatusSourceFingerprint([]byte("same cached hook"))
		if err := stale.publishHookRuntimeBindingObservation(
			stale.CaptureRuntimeBindingObservation("copilot"), "same-binding", fingerprint,
		); err != nil {
			t.Fatal(err)
		}
		observation := stale.CaptureRuntimeBindingObservation("copilot")
		bindingA := stale.RuntimeBindings["copilot"]
		incarnationA := stale.PersistenceIncarnation()
		if err := storage.db.DeleteInstance(stale.ID); err != nil {
			t.Fatal(err)
		}

		winner := runtimeBindingTestInstance(stale.ID)
		winner.RuntimeBindings = map[string]statedb.RuntimeBinding{"copilot": bindingA}
		winner.CopilotSessionID = bindingA.Value
		winner.CopilotDetectedAt = bindingA.DetectedAt
		if err := storage.InsertSessionAndVerify(winner, nil); err != nil {
			t.Fatal(err)
		}
		if winner.PersistenceIncarnation() == incarnationA {
			t.Fatalf("byte-identical B reused A incarnation %q", incarnationA)
		}
		bindingB, found, err := storage.db.ReadRuntimeBinding(winner.ID, "copilot")
		if err != nil || !found || bindingB != bindingA {
			t.Fatalf("B binding = %#v found=%v err=%v; want %#v", bindingB, found, err, bindingA)
		}

		if err := stale.PublishRuntimeBindingObservation(
			observation, bindingA.Value, bindingA.DetectedAt,
		); !errors.Is(err, statedb.ErrInstanceParentConflict) {
			t.Fatalf("stale A same-value error = %v, want parent conflict", err)
		}
		if err := stale.publishHookRuntimeBindingObservation(
			observation, bindingA.Value, fingerprint,
		); !errors.Is(err, statedb.ErrInstanceParentConflict) {
			t.Fatalf("stale A cache-hit error = %v, want parent conflict", err)
		}
		if got, found, err := storage.db.ReadRuntimeBinding(winner.ID, "copilot"); err != nil || !found || got != bindingB {
			t.Fatalf("stale A changed B binding = %#v found=%v err=%v; want %#v", got, found, err, bindingB)
		}
	})
}

func TestRuntimeLifecycle_UnchangedCachedHookPublishesOnceAllKinds(t *testing.T) {
	cases := []struct {
		kind, value string
	}{
		{"claude", "claude-hook"},
		{"copilot", "copilot-hook"},
		{"codex", "11111111-2222-3333-4444-555555555555"},
		{"gemini", "gemini-hook"},
		{"opencode", "opencode-hook"},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			storage := runtimeBindingTestStorage(t)
			inst := runtimeBindingTestInstance("cached-hook-" + tc.kind)
			if err := storage.InsertSessionAndVerify(inst, nil); err != nil {
				t.Fatal(err)
			}

			oldAcquire := instanceSpawnLockAcquireFn
			lockAcquisitions := 0
			instanceSpawnLockAcquireFn = func(string) (func(), error) {
				lockAcquisitions++
				return func() {}, nil
			}
			t.Cleanup(func() { instanceSpawnLockAcquireFn = oldAcquire })

			oldBeforeMemory := runtimeBindingBeforeMemoryPublishFn
			publications := 0
			runtimeBindingBeforeMemoryPublishFn = func(*Instance, statedb.RuntimeBinding) {
				publications++
			}
			t.Cleanup(func() { runtimeBindingBeforeMemoryPublishFn = oldBeforeMemory })

			fingerprint := hookStatusSourceFingerprint([]byte("cached hook " + tc.kind))
			if err := inst.publishHookRuntimeBindingObservation(
				inst.CaptureRuntimeBindingObservation(tc.kind), tc.value, fingerprint); err != nil {
				t.Fatal(err)
			}
			// The transition daemon reloads Instance values on every pass. Carry
			// its process-local state each time so this exercises the real cached
			// hook path. Cache hits still validate the durable incarnation, but do
			// not republish or advance the binding revision.
			for n := 1; n < 100; n++ {
				state := inst.pollingStateForIdentity(inst.pollingIdentity())
				binding := inst.RuntimeBindings[tc.kind]
				reloaded := runtimeBindingTestInstance(inst.ID)
				reloaded.adoptPersistenceIncarnation(inst.PersistenceIncarnation())
				reloaded.RuntimeGeneration = inst.RuntimeGeneration
				reloaded.owningDB = storage.db
				if err := reloaded.applyRuntimeBindingLocked(binding); err != nil {
					t.Fatal(err)
				}
				if !reloaded.restorePollingState(state) {
					t.Fatal("validated hook cache was not restored")
				}
				if err := reloaded.publishHookRuntimeBindingObservation(
					reloaded.CaptureRuntimeBindingObservation(tc.kind), tc.value, fingerprint); err != nil {
					t.Fatalf("repeat %d: %v", n, err)
				}
				inst = reloaded
			}

			binding := inst.RuntimeBindings[tc.kind]
			if lockAcquisitions != 1 || publications != 1 || binding.Revision != 1 || binding.Value != tc.value {
				t.Fatalf("locks=%d publications=%d binding=%+v", lockAcquisitions, publications, binding)
			}
		})
	}
}

func TestRuntimeLifecycle_DistinctSameSecondHookStillDetectsStaleBinding(t *testing.T) {
	for _, kind := range []string{"claude", "copilot", "codex", "gemini", "opencode"} {
		t.Run(kind, func(t *testing.T) {
			storage := runtimeBindingTestStorage(t)
			inst := runtimeBindingTestInstance("changed-cached-hook-" + kind)
			if err := storage.InsertSessionAndVerify(inst, nil); err != nil {
				t.Fatal(err)
			}

			firstFingerprint := hookStatusSourceFingerprint([]byte(`{"event":"first","ts":1200}`))
			secondFingerprint := hookStatusSourceFingerprint([]byte(`{"event":"second","ts":1200}`))
			if err := inst.publishHookRuntimeBindingObservation(
				inst.CaptureRuntimeBindingObservation(kind), "first", firstFingerprint); err != nil {
				t.Fatal(err)
			}
			stale := inst.CaptureRuntimeBindingObservation(kind)
			if _, err := storage.db.CommitRuntimeBinding(inst.ID, inst.PersistenceIncarnation(), 0, kind, stale.revision, "peer-winner"); err != nil {
				t.Fatal(err)
			}

			err := inst.publishHookRuntimeBindingObservation(stale, "first", secondFingerprint)
			if !errors.Is(err, statedb.ErrBindingRevisionConflict) {
				t.Fatalf("changed hook error = %v, want stale revision", err)
			}
			if got := runtimeBindingValues(inst)[kind]; got != "peer-winner" || inst.RuntimeBindings[kind].Revision != stale.revision+1 {
				t.Fatalf("durable winner was not adopted: value=%q binding=%+v", got, inst.RuntimeBindings[kind])
			}
		})
	}
}

func TestRuntimeLifecycle_LoadUsesAllAuthoritativeBindings(t *testing.T) {
	storage := runtimeBindingTestStorage(t)
	inst := runtimeBindingTestInstance("load-all")
	if err := storage.InsertSessionAndVerify(inst, nil); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"claude": "claude-current", "copilot": "copilot-current",
		"codex":  "22222222-3333-4444-5555-666666666666",
		"gemini": "gemini-current", "opencode": "opencode-current",
	}
	for kind, value := range want {
		if _, applied, err := storage.db.WriteRuntimeBindingIfVersion(
			inst.ID, inst.PersistenceIncarnation(), 0, kind, 0, value, time.Unix(500, 0).UTC()); err != nil || !applied {
			t.Fatalf("publish %s: applied=%v err=%v", kind, applied, err)
		}
	}

	// Deliberately corrupt every compatibility key. Load must ignore this JSON
	// and materialize the five fields from instance_runtime_binding.
	raw, err := sql.Open("sqlite", storage.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	stale := `{"claude_session_id":"claude-stale","copilot_session_id":"copilot-stale","codex_session_id":"33333333-4444-5555-6666-777777777777","gemini_session_id":"gemini-stale","opencode_session_id":"opencode-stale"}`
	if _, err := raw.Exec(`UPDATE instances SET tool_data = ? WHERE id = ?`, stale, inst.ID); err != nil {
		t.Fatal(err)
	}

	lite, _, err := storage.LoadLite()
	if err != nil || len(lite) != 1 {
		t.Fatalf("LoadLite len=%d err=%v", len(lite), err)
	}
	for kind, value := range want {
		if got := runtimeBindingDataValues(lite[0])[kind]; got != value {
			t.Errorf("LoadLite %s = %q, want %q", kind, got, value)
		}
	}
	loaded, _, err := storage.LoadWithGroups()
	if err != nil || len(loaded) != 1 {
		t.Fatalf("LoadWithGroups len=%d err=%v", len(loaded), err)
	}
	for kind, value := range want {
		if got := runtimeBindingValues(loaded[0])[kind]; got != value {
			t.Errorf("LoadWithGroups %s = %q, want %q", kind, got, value)
		}
	}
}

func TestRuntimeLifecycle_GenerationOneObservationCannotBindGenerationTwo(t *testing.T) {
	storage := runtimeBindingTestStorage(t)
	inst := runtimeBindingTestInstance("binding-transition-lock")
	if err := storage.InsertSessionAndVerify(inst, nil); err != nil {
		t.Fatal(err)
	}
	generationOne := statedb.RuntimeState{InstanceID: inst.ID, Generation: 1, Status: string(StatusIdle)}
	if err := storage.db.CommitRuntimeTransition(0, inst.PersistenceIncarnation(), generationOne); err != nil {
		t.Fatal(err)
	}
	inst.adoptRuntimeState(generationOne)
	prior, err := storage.db.CommitRuntimeBinding(inst.ID, inst.PersistenceIncarnation(), 1, "copilot", 0, "before")
	if err != nil {
		t.Fatal(err)
	}
	inst.adoptRuntimeBindings(map[string]statedb.RuntimeBinding{"copilot": prior})
	observation := inst.CaptureRuntimeBindingObservation("copilot")

	oldAcquire := instanceSpawnLockAcquireFn
	lock := make(chan struct{}, 1)
	lock <- struct{}{}
	entered := make(chan struct{}, 2)
	instanceSpawnLockAcquireFn = func(string) (func(), error) {
		entered <- struct{}{}
		<-lock
		return func() { lock <- struct{}{} }, nil
	}
	t.Cleanup(func() { instanceSpawnLockAcquireFn = oldAcquire })

	release, err := acquireInstanceSpawnLock(inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	published := make(chan error, 1)
	go func() {
		published <- inst.PublishRuntimeBindingObservation(observation, "stale-candidate", time.Unix(550, 0).UTC())
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		release()
		t.Fatal("binding publisher did not wait on the transition lock")
	}
	if current, _, err := storage.db.ReadRuntimeBinding(inst.ID, "copilot"); err != nil || current != prior {
		release()
		t.Fatalf("binding changed while transition held lock: current=%+v err=%v", current, err)
	}

	next := statedb.RuntimeState{InstanceID: inst.ID, Generation: 2, Status: string(StatusIdle)}
	if err := storage.db.CommitRuntimeTransitionWithBindingPlan(1, inst.PersistenceIncarnation(), next, []statedb.RuntimeBindingTransition{{
		Kind: "copilot", ExpectedRevision: prior.Revision,
		NextValue: prior.Value, DetectedAt: prior.DetectedAt,
	}}); err != nil {
		release()
		t.Fatal(err)
	}
	carried, found, err := storage.db.ReadRuntimeBinding(inst.ID, "copilot")
	if err != nil || !found {
		release()
		t.Fatalf("read carried binding found=%v err=%v", found, err)
	}
	inst.adoptRuntimeState(next)
	inst.adoptRuntimeBindings(map[string]statedb.RuntimeBinding{"copilot": carried})
	release()

	if err := <-published; err == nil || !errors.Is(err, ErrRuntimeBindingObservationStale) {
		t.Fatalf("publisher error = %v, want stale observation", err)
	}
	current, found, err := storage.db.ReadRuntimeBinding(inst.ID, "copilot")
	if err != nil || !found || current != carried || current.Generation != 2 || current.Value != "before" {
		t.Fatalf("post-transition binding = %+v found=%v err=%v", current, found, err)
	}
}

func TestRuntimeLifecycle_BindingRejectsStaleGeneration(t *testing.T) {
	storage := runtimeBindingTestStorage(t)
	inst := runtimeBindingTestInstance("stale-generation")
	if err := storage.InsertSessionAndVerify(inst, nil); err != nil {
		t.Fatal(err)
	}
	if err := storage.db.CommitRuntimeTransition(0, inst.PersistenceIncarnation(), statedb.RuntimeState{
		InstanceID: inst.ID, Generation: 1, Status: string(StatusIdle),
	}); err != nil {
		t.Fatal(err)
	}

	err := inst.publishRuntimeBinding("copilot", "stale-candidate", time.Unix(600, 0).UTC())
	var typed *RuntimeBindingPublishError
	if !errors.As(err, &typed) || !errors.Is(err, statedb.ErrRuntimeGenerationConflict) {
		t.Fatalf("error = %v, want typed generation conflict", err)
	}
	if inst.RuntimeGeneration != 1 || inst.CopilotSessionID == "stale-candidate" {
		t.Fatalf("stale observation leaked into memory: generation=%d copilot=%q", inst.RuntimeGeneration, inst.CopilotSessionID)
	}
}

func TestRuntimeLifecycle_BindingRejectsStaleRevision(t *testing.T) {
	storage := runtimeBindingTestStorage(t)
	inst := runtimeBindingTestInstance("stale-revision")
	if err := storage.InsertSessionAndVerify(inst, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.db.CommitRuntimeBinding(inst.ID, inst.PersistenceIncarnation(), 0, "copilot", 0, "winner"); err != nil {
		t.Fatal(err)
	}

	err := inst.publishRuntimeBinding("copilot", "stale-candidate", time.Unix(700, 0).UTC())
	var typed *RuntimeBindingPublishError
	if !errors.As(err, &typed) || !errors.Is(err, statedb.ErrBindingRevisionConflict) {
		t.Fatalf("error = %v, want typed revision conflict", err)
	}
	if inst.CopilotSessionID != "winner" || inst.RuntimeBindings["copilot"].Revision != 1 {
		t.Fatalf("durable winner was not adopted: id=%q binding=%+v", inst.CopilotSessionID, inst.RuntimeBindings["copilot"])
	}
}

func TestRuntimeLifecycle_ConcurrentBindingConflictAdoptsWinner(t *testing.T) {
	storage := runtimeBindingTestStorage(t)
	winner := runtimeBindingTestInstance("concurrent-binding")
	if err := storage.InsertSessionAndVerify(winner, nil); err != nil {
		t.Fatal(err)
	}
	seedAt := time.Unix(710, 0).UTC()
	if err := winner.PublishRuntimeBindingObservation(
		winner.CaptureRuntimeBindingObservation("copilot"), "before", seedAt); err != nil {
		t.Fatal(err)
	}

	loser := runtimeBindingTestInstance(winner.ID)
	loser.adoptPersistenceIncarnation(winner.PersistenceIncarnation())
	loser.owningDB = storage.db
	loser.RuntimeGeneration = winner.RuntimeGeneration
	loser.StatusRevision = winner.StatusRevision
	loser.RuntimeBindings = map[string]statedb.RuntimeBinding{
		"copilot": winner.RuntimeBindings["copilot"],
	}
	loser.CopilotSessionID = winner.CopilotSessionID
	loser.CopilotDetectedAt = winner.CopilotDetectedAt

	winnerObservation := winner.CaptureRuntimeBindingObservation("copilot")
	loserObservation := loser.CaptureRuntimeBindingObservation("copilot")
	winnerAt := time.Unix(720, 0).UTC()
	if err := winner.PublishRuntimeBindingObservation(winnerObservation, "winner", winnerAt); err != nil {
		t.Fatal(err)
	}
	loserAt := time.Unix(730, 0).UTC()
	err := loser.PublishRuntimeBindingObservation(loserObservation, "loser", loserAt)
	var typed *RuntimeBindingPublishError
	if !errors.As(err, &typed) || !errors.Is(err, statedb.ErrBindingRevisionConflict) {
		t.Fatalf("loser error = %v, want typed revision conflict", err)
	}
	durable, found, readErr := storage.db.ReadRuntimeBinding(winner.ID, "copilot")
	if readErr != nil || !found || durable.Value != "winner" || durable.Revision != 2 || !durable.DetectedAt.Equal(winnerAt) {
		t.Fatalf("durable winner = %+v found=%v err=%v", durable, found, readErr)
	}
	if loser.CopilotSessionID != durable.Value || loser.RuntimeBindings["copilot"] != durable {
		t.Fatalf("loser did not adopt durable winner: id=%q binding=%+v durable=%+v",
			loser.CopilotSessionID, loser.RuntimeBindings["copilot"], durable)
	}
}

func TestRuntimeLifecycle_BindingPublisherAcquiresTransitionLockWithoutInstanceMutex(t *testing.T) {
	storage := runtimeBindingTestStorage(t)
	inst := runtimeBindingTestInstance("binding-lock-order")
	if err := storage.InsertSessionAndVerify(inst, nil); err != nil {
		t.Fatal(err)
	}

	oldAcquire := instanceSpawnLockAcquireFn
	instanceSpawnLockAcquireFn = func(string) (func(), error) {
		if !inst.mu.TryLock() {
			t.Fatal("binding publisher acquired transition lock while holding instance mutex")
		}
		inst.mu.Unlock()
		return func() {}, nil
	}
	t.Cleanup(func() { instanceSpawnLockAcquireFn = oldAcquire })

	observation := inst.CaptureRuntimeBindingObservation("copilot")
	if err := inst.PublishRuntimeBindingObservation(observation, "normal", time.Unix(740, 0).UTC()); err != nil {
		t.Fatal(err)
	}

	stale := inst.CaptureRuntimeBindingObservation("copilot")
	if _, err := storage.db.CommitRuntimeBinding(inst.ID, inst.PersistenceIncarnation(), 0, "copilot", 1, "durable-winner"); err != nil {
		t.Fatal(err)
	}
	err := inst.PublishRuntimeBindingObservation(stale, "loser", time.Unix(750, 0).UTC())
	if !errors.Is(err, statedb.ErrBindingRevisionConflict) {
		t.Fatalf("conflict error = %v", err)
	}
	if inst.CopilotSessionID != "durable-winner" {
		t.Fatalf("conflict path did not adopt durable winner: %q", inst.CopilotSessionID)
	}
}

func TestRuntimeLifecycle_StatusRefreshPublishesBindingWithoutInstanceMutex(t *testing.T) {
	storage := runtimeBindingTestStorage(t)
	inst := runtimeBindingTestInstance("status-binding-lock-order")
	inst.Tool = "claude"
	inst.Status = StatusRunning
	inst.hookSessionID = "status-hook-session"
	if err := storage.InsertSessionAndVerify(inst, nil); err != nil {
		t.Fatal(err)
	}

	oldAcquire := instanceSpawnLockAcquireFn
	instanceSpawnLockAcquireFn = func(string) (func(), error) {
		if !inst.mu.TryLock() {
			t.Fatal("status refresh entered binding publisher while holding instance mutex")
		}
		inst.mu.Unlock()
		return func() {}, nil
	}
	t.Cleanup(func() { instanceSpawnLockAcquireFn = oldAcquire })

	observation := inst.captureActiveRuntimeBindingObservation()
	inst.refreshStatusMetadataIfCurrent(inst.runtimeStateSnapshot(), observation)
	if inst.ClaudeSessionID != "status-hook-session" {
		t.Fatalf("status hook binding = %q", inst.ClaudeSessionID)
	}
}

func TestRuntimeLifecycle_RestartBindingRecoveryDoesNotReenterTransitionLock(t *testing.T) {
	inst := runtimeBindingTestInstance("restart-binding-lock-order")
	inst.Tool = "gemini"
	inst.ProjectPath = t.TempDir()

	oldAcquire := instanceSpawnLockAcquireFn
	held, acquisitions := false, 0
	instanceSpawnLockAcquireFn = func(string) (func(), error) {
		acquisitions++
		if held {
			return nil, errors.New("recursive transition lock acquisition")
		}
		held = true
		return func() { held = false }, nil
	}
	t.Cleanup(func() { instanceSpawnLockAcquireFn = oldAcquire })

	release, err := acquireInstanceSpawnLock(inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	inst.updateGeminiSessionForTransition()
	release()
	if acquisitions != 1 {
		t.Fatalf("transition lock acquisitions = %d, want 1", acquisitions)
	}
}

func TestRuntimeLifecycle_BindingPublisherRejectsUnsupportedKindBeforeWrite(t *testing.T) {
	storage := runtimeBindingTestStorage(t)
	inst := runtimeBindingTestInstance("unsupported-binding")
	if err := storage.InsertSessionAndVerify(inst, nil); err != nil {
		t.Fatal(err)
	}

	err := inst.PublishRuntimeBindingObservation(
		inst.CaptureRuntimeBindingObservation("unsupported"), "must-not-commit", time.Unix(760, 0).UTC())
	if err == nil || !strings.Contains(err.Error(), "unsupported runtime binding kind") {
		t.Fatalf("unsupported kind error = %v", err)
	}
	if binding, found, readErr := storage.db.ReadRuntimeBinding(inst.ID, "unsupported"); readErr != nil || found {
		t.Fatalf("unsupported kind reached durable storage: binding=%+v found=%v err=%v", binding, found, readErr)
	}
}

func TestRuntimeLifecycle_BindingRejectsForeignOwner(t *testing.T) {
	storage := runtimeBindingTestStorage(t)
	owner := runtimeBindingTestInstance("binding-owner")
	contender := runtimeBindingTestInstance("binding-contender")
	for _, inst := range []*Instance{owner, contender} {
		if err := storage.InsertSessionAndVerify(inst, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := storage.db.CommitRuntimeBinding(owner.ID, owner.PersistenceIncarnation(), 0, "copilot", 0, "shared"); err != nil {
		t.Fatal(err)
	}
	prior, err := storage.db.CommitRuntimeBinding(contender.ID, contender.PersistenceIncarnation(), 0, "copilot", 0, "prior")
	if err != nil {
		t.Fatal(err)
	}
	contender.RuntimeBindings = map[string]statedb.RuntimeBinding{"copilot": prior}
	contender.CopilotSessionID = prior.Value
	contender.CopilotDetectedAt = prior.DetectedAt

	err = contender.publishRuntimeBinding("copilot", "shared", time.Unix(800, 0).UTC())
	var typed *RuntimeBindingPublishError
	if !errors.As(err, &typed) || !errors.Is(err, ErrRuntimeBindingOwnership) {
		t.Fatalf("error = %v, want typed ownership conflict", err)
	}
	if contender.CopilotSessionID != "prior" || contender.RuntimeBindings["copilot"] != prior {
		t.Fatalf("ownership failure mutated memory: id=%q binding=%+v", contender.CopilotSessionID, contender.RuntimeBindings["copilot"])
	}
}

func TestRuntimeLifecycle_CopilotBindingRoundTrip(t *testing.T) {
	storage := runtimeBindingTestStorage(t)
	detectedAt := time.Unix(900, 0).UTC()
	inst := runtimeBindingTestInstance("copilot-roundtrip")
	inst.CopilotSessionID = "copilot-roundtrip-value"
	inst.CopilotDetectedAt = detectedAt
	if err := storage.InsertSessionAndVerify(inst, nil); err != nil {
		t.Fatal(err)
	}

	lite, _, err := storage.LoadLite()
	if err != nil || len(lite) != 1 || lite[0].CopilotSessionID != inst.CopilotSessionID || !lite[0].CopilotDetectedAt.Equal(detectedAt) {
		t.Fatalf("LoadLite Copilot round trip = %+v err=%v", lite, err)
	}
	full, _, err := storage.LoadWithGroups()
	if err != nil || len(full) != 1 || full[0].CopilotSessionID != inst.CopilotSessionID || !full[0].CopilotDetectedAt.Equal(detectedAt) {
		t.Fatalf("LoadWithGroups Copilot round trip = %+v err=%v", full, err)
	}

	encoded, err := json.Marshal(&InstanceData{CopilotSessionID: inst.CopilotSessionID, CopilotDetectedAt: detectedAt})
	if err != nil {
		t.Fatal(err)
	}
	var decoded InstanceData
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.CopilotSessionID != inst.CopilotSessionID || !decoded.CopilotDetectedAt.Equal(detectedAt) {
		t.Fatalf("InstanceData Copilot JSON round trip = %+v", decoded)
	}
}
