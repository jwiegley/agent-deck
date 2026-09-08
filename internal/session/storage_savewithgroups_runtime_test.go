package session

import (
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

func TestRuntimeLifecycle_SaveWithGroupsStaleSnapshotCannotRevertRuntimeOrBinding(t *testing.T) {
	storage := newInsertIfAbsentStorage(t)
	inst := NewInstanceWithTool("before", "/tmp/save-with-groups", "claude")
	inst.ID = "save-with-groups-stale"
	inst.tmuxSession = &tmux.Session{Name: "tmux-g0", InstanceID: inst.ID}
	if err := storage.InsertSessionAndVerify(inst, nil); err != nil {
		t.Fatal(err)
	}

	loaded, _, err := storage.LoadWithGroups()
	if err != nil || len(loaded) != 1 {
		t.Fatalf("load stale snapshot: instances=%d err=%v", len(loaded), err)
	}
	stale := loaded[0]
	started := time.Unix(200, 123).UTC()
	next := statedb.RuntimeState{
		InstanceID: inst.ID, Generation: 1,
		TmuxSession: "tmux-g1", TmuxSocketName: "isolated",
		Status: string(StatusStarting), LastStartedAt: started,
	}
	if err := storage.db.CommitRuntimeTransitionWithBindingPlan(
		0, inst.PersistenceIncarnation(), next, []statedb.RuntimeBindingTransition{
			{Kind: "claude", ExpectedRevision: 0, NextValue: "conversation-g0"},
		}); err != nil {
		t.Fatal(err)
	}
	binding, err := storage.db.CommitRuntimeBinding(
		inst.ID, inst.PersistenceIncarnation(), 1, "claude", 1, "conversation-g1")
	if err != nil {
		t.Fatal(err)
	}

	// Save the complete stale G0 object through the public storage boundary.
	// Its metadata edit must persist, while every runtime-owned field remains G1.
	stale.Title = "after"
	stale.Status = StatusError
	stale.TmuxSocketName = ""
	stale.tmuxSession = &tmux.Session{Name: "tmux-g0", InstanceID: stale.ID}
	stale.ClaudeSessionID = "conversation-g0"
	stale.RuntimeBindings = map[string]statedb.RuntimeBinding{
		"claude": {
			InstanceID: stale.ID, Kind: "claude", Generation: 0,
			Revision: 0, Value: "conversation-g0",
		},
	}
	if err := storage.SaveWithGroups([]*Instance{stale}, nil); err != nil {
		t.Fatal(err)
	}

	gotInstances, _, err := storage.LoadWithGroups()
	if err != nil || len(gotInstances) != 1 {
		t.Fatalf("load saved snapshot: instances=%d err=%v", len(gotInstances), err)
	}
	got := gotInstances[0]
	state := got.RuntimeState()
	if got.Title != "after" || state != next {
		t.Fatalf("stale SaveWithGroups changed runtime or lost metadata: title=%q state=%#v want=%#v",
			got.Title, state, next)
	}
	if gotBinding := got.RuntimeBindings["claude"]; gotBinding != binding {
		t.Fatalf("stale SaveWithGroups changed binding: got=%#v want=%#v", gotBinding, binding)
	}
}
