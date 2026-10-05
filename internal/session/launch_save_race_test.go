package session

import (
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/stretchr/testify/require"
)

// Issue #2209: a detector may advance the runtime status and binding after a
// launch publishes its physical runtime but before its post-start metadata save.
// The metadata save must preserve that newer authoritative runtime tuple.
func TestPostStartMetadataSavePreservesConcurrentDetection(t *testing.T) {
	s := newTestStorage(t)
	inst := &Instance{
		ID: "sess", Title: "worker", ProjectPath: t.TempDir(), GroupPath: "fleet",
		Tool: "claude", Command: "claude", Status: StatusStarting,
		ParentSessionID: "parent", LoadedMCPNames: []string{"neo4j"},
		WorktreeBranch: "sess", CreatedAt: time.Unix(1000, 0), LastAccessedAt: time.Unix(1000, 0),
	}
	require.NoError(t, s.InsertSessionAndVerify(inst, nil))

	initial := inst.RuntimeState()
	launched := initial
	launched.Generation++
	launched.TmuxSession = "agentdeck_worker_c5322ee1"
	launched.Status = string(StatusRunning)
	launched.LastStartedAt = time.Unix(1999, 0).UTC()
	require.NoError(t, s.db.CommitRuntimeTransitionWithBindingPlan(
		initial.Generation, inst.PersistenceIncarnation(), launched,
		[]statedb.RuntimeBindingTransition{{
			Kind: "claude", ExpectedRevision: 0, NextValue: "conv-1", DetectedAt: time.Unix(1999, 0),
		}},
	))
	require.True(t, inst.ApplyRuntimeState(launched))
	binding, found, err := s.db.ReadRuntimeBinding(inst.ID, "claude")
	require.NoError(t, err)
	require.True(t, found)
	inst.adoptRuntimeBindings(map[string]statedb.RuntimeBinding{"claude": binding})

	// A concurrent detector observes the same conversation and a newer status.
	_, applied, err := s.db.WriteRuntimeBindingIfVersion(
		inst.ID, inst.PersistenceIncarnation(), launched.Generation, "claude", binding.Revision,
		"conv-1", time.Unix(2000, 0),
	)
	require.NoError(t, err)
	require.True(t, applied)
	applied, err = s.db.WriteStatusIfVersion(
		inst.ID, inst.PersistenceIncarnation(), launched.Generation, launched.StatusRevision, string(StatusWaiting),
	)
	require.NoError(t, err)
	require.True(t, applied)

	inst.LastAccessedAt = time.Unix(1999, 0)
	require.NoError(t, s.SaveWithGroups([]*Instance{inst}, nil), "post-start metadata save must not clobber runtime")

	got, err := s.Load()
	require.NoError(t, err)
	require.Len(t, got, 1, "no duplicate rows")
	row := got[0]
	require.Equal(t, "agentdeck_worker_c5322ee1", row.GetTmuxSession().Name)
	require.Equal(t, StatusWaiting, row.Status)
	require.Equal(t, "conv-1", row.ClaudeSessionID)
	require.Equal(t, int64(2000), row.ClaudeDetectedAt.Unix())
	require.Equal(t, "parent", row.ParentSessionID)
	require.Equal(t, []string{"neo4j"}, row.LoadedMCPNames)
	require.Equal(t, "sess", row.WorktreeBranch)
	require.Equal(t, "fleet", row.GroupPath)

	inst.Title = "worker-renamed"
	require.NoError(t, s.Save([]*Instance{inst}))
}
