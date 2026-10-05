package main

import (
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/stretchr/testify/require"
)

// Issue #2209: launch JSON must report the durable runtime, not the stale
// in-memory metadata snapshot that launched it.
func TestAddLaunchStateJSONReportsMergedState(t *testing.T) {
	storage, err := session.NewStorageWithProfile("_test_launch_state_json")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, storage.Close()) })
	inst := &session.Instance{ID: "sess", Title: "worker", Tool: "claude", Status: session.StatusStarting}
	require.NoError(t, storage.InsertSessionAndVerify(inst, nil))
	db := storage.GetDB()
	state := statedb.RuntimeState{
		InstanceID: inst.ID, Generation: 1, Status: "running",
		TmuxSession: "agentdeck_worker_c5322ee1", LastStartedAt: time.Unix(1999, 0),
	}
	require.NoError(t, db.CommitRuntimeTransition(0, inst.PersistenceIncarnation(), state))
	_, applied, err := db.WriteRuntimeBindingIfVersion(
		inst.ID, inst.PersistenceIncarnation(), 1, "claude", 0, "conv-1", time.Unix(2000, 0),
	)
	require.NoError(t, err)
	require.True(t, applied)
	applied, err = db.WriteStatusIfVersion(inst.ID, inst.PersistenceIncarnation(), 1, 0, "waiting")
	require.NoError(t, err)
	require.True(t, applied)

	jsonData := map[string]interface{}{}
	require.NoError(t, addLaunchStateJSON(jsonData, storage, inst))
	require.Equal(t, "waiting", jsonData["status"])
	require.Equal(t, "agentdeck_worker_c5322ee1", jsonData["tmux_session"])
	require.Equal(t, "conv-1", jsonData["claude_session_id"])

	bareInst := &session.Instance{ID: "bare", Title: "bare", Status: session.StatusRunning}
	require.NoError(t, storage.InsertSessionAndVerify(bareInst, nil))
	bare := map[string]interface{}{}
	require.NoError(t, addLaunchStateJSON(bare, storage, bareInst))
	require.Equal(t, "running", bare["status"])
	require.NotContains(t, bare, "tmux_session")
	require.NotContains(t, bare, "claude_session_id")

	require.NoError(t, storage.DeleteInstance(inst.ID))
	require.ErrorContains(t, addLaunchStateJSON(map[string]interface{}{}, storage, inst), "deleted or replaced")
}
