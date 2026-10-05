package statedb

import (
	"errors"
	"testing"
	"time"
)

func TestRuntimeLifecycle_BindingCleanupLeasePreservesCurrentReboundTarget(t *testing.T) {
	db := newRuntimeTestDB(t)
	insert := func(state RuntimeState, incarnation, bindingValue string) RuntimeBinding {
		t.Helper()
		binding := RuntimeBinding{
			InstanceID: state.InstanceID, Kind: "claude", Generation: state.Generation,
			Revision: 1, Value: bindingValue, DetectedAt: time.Unix(190, 0).UTC(),
		}
		_, inserted, err := db.InsertInstanceIfAbsent(&InstanceRow{
			ID: state.InstanceID, Incarnation: incarnation, Title: state.InstanceID,
			ProjectPath: "/tmp/" + state.InstanceID, GroupPath: "my-sessions",
			Tool: "claude", Status: state.Status, TmuxSession: state.TmuxSession,
			TmuxSocketName: state.TmuxSocketName, RuntimeGeneration: state.Generation,
			StatusRevision: state.StatusRevision, LastStartedAt: state.LastStartedAt,
			CreatedAt:       time.Unix(100, 0).UTC(),
			RuntimeBindings: map[string]RuntimeBinding{"claude": binding},
		})
		if err != nil || !inserted {
			t.Fatalf("insert %s: inserted=%v err=%v", state.InstanceID, inserted, err)
		}
		got, found, err := db.ReadRuntimeBinding(state.InstanceID, "claude")
		if err != nil || !found {
			t.Fatalf("read %s binding: found=%v err=%v", state.InstanceID, found, err)
		}
		return got
	}

	source := RuntimeState{
		InstanceID: "source", Generation: 1, StatusRevision: 1,
		TmuxSession: "agentdeck_source", TmuxSocketName: "isolated",
		Status: "running", LastStartedAt: time.Unix(200, 0).UTC(),
	}
	target := RuntimeState{
		InstanceID: "target", Generation: 7, StatusRevision: 3,
		TmuxSession: "agentdeck_target", TmuxSocketName: "isolated",
		Status: "running", LastStartedAt: time.Unix(210, 0).UTC(),
	}
	sourceBinding := insert(source, "source-incarnation", "conversation-a")
	insert(target, "target-incarnation", "conversation-b")

	ran := false
	leased, err := db.WithRuntimeBindingCleanupLease(
		source, "source-incarnation", sourceBinding, target, func() { ran = true },
	)
	if err != nil || leased || ran {
		t.Fatalf("current rebound target lease leased=%v ran=%v err=%v", leased, ran, err)
	}

	// A candidate tuple that is no longer the target's durable physical runtime
	// remains eligible for stale/orphan cleanup; the target binding fence does
	// not disable cross-instance cleanup wholesale.
	staleTarget := target
	staleTarget.TmuxSession = "agentdeck_target_old"
	leased, err = db.WithRuntimeBindingCleanupLease(
		source, "source-incarnation", sourceBinding, staleTarget, func() { ran = true },
	)
	if err != nil || !leased || !ran {
		t.Fatalf("stale target lease leased=%v ran=%v err=%v", leased, ran, err)
	}
}

func TestRuntimeLifecycle_BindingCommitFenceRunsOnlyForCurrentVersion(t *testing.T) {
	db := newRuntimeTestDB(t)
	state := RuntimeState{
		InstanceID: "fenced", Generation: 1, StatusRevision: 1,
		TmuxSession: "agentdeck_fenced", TmuxSocketName: "isolated",
		Status: "running", LastStartedAt: time.Unix(200, 0).UTC(),
	}
	seed := RuntimeBinding{
		InstanceID: state.InstanceID, Kind: "claude", Generation: state.Generation,
		Revision: 1, Value: "before", DetectedAt: time.Unix(190, 0).UTC(),
	}
	_, inserted, err := db.InsertInstanceIfAbsent(&InstanceRow{
		ID: state.InstanceID, Incarnation: "fenced-incarnation", Title: state.InstanceID,
		ProjectPath: "/tmp/fenced", GroupPath: "my-sessions", Tool: "claude",
		Status: state.Status, TmuxSession: state.TmuxSession, TmuxSocketName: state.TmuxSocketName,
		RuntimeGeneration: state.Generation, StatusRevision: state.StatusRevision,
		LastStartedAt: state.LastStartedAt, CreatedAt: time.Unix(100, 0).UTC(),
		RuntimeBindings: map[string]RuntimeBinding{"claude": seed},
	})
	if err != nil || !inserted {
		t.Fatalf("insert fenced binding: inserted=%v err=%v", inserted, err)
	}
	current, found, err := db.ReadRuntimeBinding(state.InstanceID, "claude")
	if err != nil || !found {
		t.Fatalf("read fenced binding: found=%v err=%v", found, err)
	}

	calls := 0
	if _, applied, err := db.WriteRuntimeBindingIfVersionWithCommitFence(
		state.InstanceID, "fenced-incarnation", 2, "claude", current.Revision, "stale", time.Time{},
		func() error { calls++; return nil },
	); err != nil || applied || calls != 0 {
		t.Fatalf("stale fenced write applied=%v calls=%d err=%v", applied, calls, err)
	}

	wantErr := errors.New("invalidate failed")
	if _, applied, err := db.WriteRuntimeBindingIfVersionWithCommitFence(
		state.InstanceID, "fenced-incarnation", state.Generation, "claude", current.Revision, "after", time.Time{},
		func() error { calls++; return wantErr },
	); applied || !errors.Is(err, wantErr) || calls != 1 {
		t.Fatalf("failed fenced write applied=%v calls=%d err=%v", applied, calls, err)
	}
	after, found, err := db.ReadRuntimeBinding(state.InstanceID, "claude")
	if err != nil || !found || after != current {
		t.Fatalf("rolled-back fenced binding = %+v found=%v err=%v, want %+v", after, found, err, current)
	}
}
