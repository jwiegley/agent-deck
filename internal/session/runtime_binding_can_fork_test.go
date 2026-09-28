package session

import (
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

// claudeForkSpawn inserts a Claude session and commits its generation-one
// spawn the way commitPhysicalRuntime does for a freshly minted --session-id:
// the binding carries the ID but no detection time.
func claudeForkSpawn(t *testing.T, storage *Storage, id string, startedAt time.Time, status Status) *Instance {
	t.Helper()
	inst := runtimeBindingTestInstance(id)
	inst.Tool, inst.Command = "claude", "claude"
	if err := storage.InsertSessionAndVerify(inst, nil); err != nil {
		t.Fatal(err)
	}
	next := statedb.RuntimeState{InstanceID: inst.ID, Generation: 1, Status: string(status), LastStartedAt: startedAt}
	if err := storage.db.CommitRuntimeTransitionWithBindingPlan(0, inst.PersistenceIncarnation(), next,
		[]statedb.RuntimeBindingTransition{{Kind: "claude", NextValue: id + "-conversation"}}); err != nil {
		t.Fatal(err)
	}
	binding, found, err := storage.db.ReadRuntimeBinding(inst.ID, "claude")
	if err != nil || !found || binding.Generation != 1 || !binding.DetectedAt.IsZero() {
		t.Fatalf("spawn binding = %+v found=%v err=%v, want generation 1 with no detection time", binding, found, err)
	}
	inst.adoptRuntimeState(next)
	inst.adoptRuntimeBindings(map[string]statedb.RuntimeBinding{"claude": binding})
	return inst
}

// claudeForkStop records the durable stop of inst's current runtime.
func claudeForkStop(t *testing.T, storage *Storage, inst *Instance) {
	t.Helper()
	state := inst.runtimeStateSnapshot()
	applied, err := storage.db.WriteStatusIfVersion(inst.ID, inst.PersistenceIncarnation(),
		state.Generation, state.StatusRevision, string(StatusStopped))
	if err != nil || !applied {
		t.Fatalf("stop %s: applied=%v err=%v", inst.ID, applied, err)
	}
	stopped, _, err := storage.db.ReadRuntimeState(inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	inst.adoptRuntimeState(stopped)
}

func TestRuntimeLifecycle_ClaudeCanForkLiveSpawnBindingWithoutDetectionTime(t *testing.T) {
	for _, status := range []Status{StatusStarting, StatusWaiting, StatusRunning, StatusIdle} {
		t.Run(string(status), func(t *testing.T) {
			storage := runtimeBindingTestStorage(t)
			inst := claudeForkSpawn(t, storage, "fork-spawn-"+string(status), time.Now().UTC(), status)
			if !inst.CanFork() {
				t.Fatalf("CanFork() = false for a %s runtime whose spawn binding %q has no detection time",
					status, inst.ClaudeSessionID)
			}
		})
	}
}

func TestRuntimeLifecycle_ClaudeCanForkAfterRepeatedObservationPastWindow(t *testing.T) {
	storage := runtimeBindingTestStorage(t)
	longAgo := time.Now().Add(-10 * time.Minute).UTC()
	inst := claudeForkSpawn(t, storage, "fork-repeat", longAgo, StatusWaiting)
	if err := inst.PublishRuntimeBindingObservation(
		inst.CaptureRuntimeBindingObservation("claude"), "fork-repeat-after-clear", longAgo); err != nil {
		t.Fatal(err)
	}
	detected, _, err := storage.db.ReadRuntimeBinding(inst.ID, "claude")
	if err != nil {
		t.Fatal(err)
	}

	// The poller's validated repeat keeps the binding's revision and its
	// original detection time: no durable write refreshes the timestamp.
	if err := inst.PublishRuntimeBindingObservation(
		inst.CaptureRuntimeBindingObservation("claude"), "fork-repeat-after-clear", time.Now()); err != nil {
		t.Fatalf("validated repeated observation: %v", err)
	}
	repeated, _, err := storage.db.ReadRuntimeBinding(inst.ID, "claude")
	if err != nil || repeated != detected || !inst.ClaudeDetectedAt.Equal(longAgo) {
		t.Fatalf("repeated observation changed the binding: durable=%+v before=%+v in-memory detected_at=%v err=%v",
			repeated, detected, inst.ClaudeDetectedAt, err)
	}
	if !inst.CanFork() {
		t.Fatal("CanFork() = false for a live runtime's current binding detected more than 5 minutes ago")
	}

	// Once that runtime has stopped, the binding is as stale as upstream's
	// detection time: nothing confirmed it within the last five minutes.
	claudeForkStop(t, storage, inst)
	if inst.CanFork() {
		t.Fatal("CanFork() = true for a binding whose runtime stopped after its last confirmation aged out")
	}
}

func TestRuntimeLifecycle_ClaudeCanForkFreshLoadFromDB(t *testing.T) {
	storage := runtimeBindingTestStorage(t)
	longAgo := time.Now().Add(-10 * time.Minute).UTC()
	claudeForkSpawn(t, storage, "fork-load-live", longAgo, StatusWaiting)
	claudeForkStop(t, storage, claudeForkSpawn(t, storage, "fork-load-just-stopped", time.Now().Add(-time.Minute).UTC(), StatusWaiting))
	claudeForkStop(t, storage, claudeForkSpawn(t, storage, "fork-load-long-stopped", longAgo, StatusWaiting))

	loaded, _, err := storage.LoadWithGroups()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"fork-load-live":         true,  // live runtime, current binding
		"fork-load-just-stopped": true,  // upstream forks a session detected within 5 minutes
		"fork-load-long-stopped": false, // upstream refuses once detection is 5 minutes old
	}
	for _, inst := range loaded {
		wantFork, ok := want[inst.ID]
		if !ok {
			t.Fatalf("unexpected loaded instance %s", inst.ID)
		}
		delete(want, inst.ID)
		if inst.ClaudeSessionID != inst.ID+"-conversation" || !inst.ClaudeDetectedAt.IsZero() {
			t.Fatalf("%s loaded claude binding %q detected %v, want the zero-time spawn binding",
				inst.ID, inst.ClaudeSessionID, inst.ClaudeDetectedAt)
		}
		if got := inst.CanFork(); got != wantFork {
			t.Errorf("%s (status %s): CanFork() = %v, want %v", inst.ID, inst.Status, got, wantFork)
		}
	}
	if len(want) != 0 {
		t.Fatalf("instances missing from load: %v", want)
	}
}

func TestRuntimeLifecycle_ClaudeCanForkSupersededBindingKeepsDetectionWindow(t *testing.T) {
	storage := runtimeBindingTestStorage(t)
	inst := claudeForkSpawn(t, storage, "fork-superseded", time.Now().UTC(), StatusWaiting)

	// An ID the live runtime's binding does not name is only as fresh as its
	// own detection time, exactly as upstream.
	inst.ClaudeSessionID, inst.ClaudeDetectedAt = "in-memory-only", time.Now().Add(-10*time.Minute)
	if inst.CanFork() {
		t.Fatal("CanFork() = true for an ID that is not the runtime's current binding")
	}

	// A binding from an earlier generation does not speak for the live one.
	binding := inst.RuntimeBindings["claude"]
	inst.ClaudeSessionID = binding.Value
	inst.RuntimeGeneration = binding.Generation + 1
	if inst.CanFork() {
		t.Fatal("CanFork() = true for a binding superseded by a newer runtime generation")
	}
}
