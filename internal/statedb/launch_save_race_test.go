package statedb

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// launchRaceFixture inserts the parent/runtime seed written before a launch
// spawns anything and returns the caller's metadata baseline.
func launchRaceFixture(t *testing.T) (*StateDB, *InstanceRow) {
	t.Helper()
	db := newTestDB(t)
	require.NoError(t, db.SaveInstance(&InstanceRow{
		ID: "sess", Title: "worker", ProjectPath: "/repo/wt/sess", GroupPath: "fleet",
		Command: "claude", Tool: "claude", Status: "starting", ParentSessionID: "parent",
		WorktreePath: "/repo/wt/sess", WorktreeRepo: "/repo", WorktreeBranch: "sess",
		CreatedAt: time.Unix(1000, 0), LastAccessed: time.Unix(1000, 0),
		ToolData: json.RawMessage(`{"loaded_mcp_names":["neo4j"]}`),
	}))
	rows, err := db.LoadInstances()
	require.NoError(t, err)
	require.Len(t, rows, 1)
	return db, rows[0]
}

func publishLaunchRuntime(t *testing.T, db *StateDB, base *InstanceRow) RuntimeState {
	t.Helper()
	initial, found, err := db.ReadRuntimeState(base.ID)
	require.NoError(t, err)
	require.True(t, found)
	next := initial
	next.Generation++
	next.TmuxSession = "agentdeck_worker_c5322ee1"
	next.Status = "running"
	next.LastStartedAt = time.Unix(1999, 0).UTC()
	require.NoError(t, db.CommitRuntimeTransitionWithBindingPlan(
		initial.Generation, base.Incarnation, next, []RuntimeBindingTransition{{
			Kind: "claude", ExpectedRevision: 0, NextValue: "conv-1", DetectedAt: time.Unix(1999, 0),
		}},
	))
	return next
}

func advanceLaunchStatus(t *testing.T, db *StateDB, base *InstanceRow, status string) {
	t.Helper()
	current, found, err := db.ReadRuntimeState(base.ID)
	require.NoError(t, err)
	require.True(t, found)
	applied, err := db.WriteStatusIfVersion(
		base.ID, base.Incarnation, current.Generation, current.StatusRevision, status)
	require.NoError(t, err)
	require.True(t, applied)
}

func advanceLaunchBinding(t *testing.T, db *StateDB, base *InstanceRow, value string, detectedAt time.Time) {
	t.Helper()
	state, found, err := db.ReadRuntimeState(base.ID)
	require.NoError(t, err)
	require.True(t, found)
	current, found, err := db.ReadRuntimeBinding(base.ID, "claude")
	require.NoError(t, err)
	require.True(t, found)
	_, applied, err := db.WriteRuntimeBindingIfVersion(
		base.ID, base.Incarnation, state.Generation, "claude", current.Revision, value, detectedAt)
	require.NoError(t, err)
	require.True(t, applied)
}

// postStartSave is the metadata snapshot a launch submits after publishing its
// runtime. Runtime fields are deliberately stale input to the metadata merger.
func postStartSave(base *InstanceRow) *InstanceRow {
	desired := CloneInstanceRow(base)
	desired.TmuxSession = "agentdeck_worker_c5322ee1"
	desired.Status = "running"
	desired.LastAccessed = time.Unix(1999, 0)
	desired.ToolData = json.RawMessage(`{"loaded_mcp_names":["neo4j"],"claude_session_id":"conv-1","claude_detected_at":1999}`)
	return desired
}

func TestLaunchPostStartSaveMergesConcurrentDetection(t *testing.T) {
	db, base := launchRaceFixture(t)
	publishLaunchRuntime(t, db, base)

	// A concurrent detector advances the same binding and samples the pane.
	advanceLaunchBinding(t, db, base, "conv-1", time.Unix(2000, 0))
	advanceLaunchStatus(t, db, base, "waiting")

	committed, err := db.MergeInstanceSnapshots(
		[]InstanceSnapshot{{Original: base, Stored: base, Desired: postStartSave(base)}}, nil)
	require.NoError(t, err, "post-start metadata save must not clobber runtime")
	require.Len(t, committed, 1)
	got := committed[0]
	require.Equal(t, "agentdeck_worker_c5322ee1", got.TmuxSession)
	require.Equal(t, "waiting", got.Status)
	require.Equal(t, int64(1999), got.LastAccessed.Unix())
	require.JSONEq(t,
		`{"loaded_mcp_names":["neo4j"],"claude_session_id":"conv-1","claude_detected_at":2000,"last_started_at":1999}`,
		string(got.ToolData))

	rows, err := db.LoadInstances()
	require.NoError(t, err)
	require.Len(t, rows, 1)
	persisted := rows[0]
	require.Equal(t, "parent", persisted.ParentSessionID)
	require.Equal(t, "/repo/wt/sess", persisted.WorktreePath)
	require.Equal(t, "sess", persisted.WorktreeBranch)
	require.Equal(t, "fleet", persisted.GroupPath)
	require.Equal(t, got.TmuxSession, persisted.TmuxSession)
	require.Equal(t, got.Status, persisted.Status)
	require.JSONEq(t, string(got.ToolData), string(persisted.ToolData))
}

func TestLaunchPostStartSaveMergesStatusVariant(t *testing.T) {
	db, base := launchRaceFixture(t)
	publishLaunchRuntime(t, db, base)
	advanceLaunchStatus(t, db, base, "idle")
	committed, err := db.MergeInstanceSnapshots(
		[]InstanceSnapshot{{Original: base, Stored: base, Desired: postStartSave(base)}}, nil)
	require.NoError(t, err)
	require.Equal(t, "idle", committed[0].Status)
	require.Equal(t, "agentdeck_worker_c5322ee1", committed[0].TmuxSession)
}

func TestLivenessMergeLastAccessedKeepsLater(t *testing.T) {
	for _, tc := range []struct {
		name            string
		committed, mine int64
		want            int64
	}{
		{"committed is later", 3000, 1999, 3000},
		{"mine is later", 1500, 1999, 1999},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, base := launchRaceFixture(t)
			_, err := db.DB().Exec("UPDATE instances SET last_accessed = ? WHERE id = 'sess'", tc.committed)
			require.NoError(t, err)
			desired := CloneInstanceRow(base)
			desired.LastAccessed = time.Unix(tc.mine, 0)
			committed, err := db.MergeInstanceSnapshots(
				[]InstanceSnapshot{{Original: base, Stored: base, Desired: desired}}, nil)
			require.NoError(t, err)
			require.Equal(t, tc.want, committed[0].LastAccessed.Unix())
		})
	}
}

func TestMetadataMergeStillConflictsOnMetadataIntent(t *testing.T) {
	for _, tc := range []struct {
		name      string
		committed func(t *testing.T, db *StateDB)
		edit      func(desired *InstanceRow)
		wantErr   string
	}{
		{
			"configuration keys keep the hard conflict",
			func(t *testing.T, db *StateDB) {
				_, err := db.DB().Exec(`UPDATE instances SET tool_data = json_set(tool_data, '$.notes', 'theirs') WHERE id = 'sess'`)
				require.NoError(t, err)
			},
			func(d *InstanceRow) {
				d.ToolData = json.RawMessage(`{"loaded_mcp_names":["neo4j"],"notes":"mine"}`)
			},
			"stale concurrent tool_data.notes conflict",
		},
		{
			"scalar configuration keeps the hard conflict",
			func(t *testing.T, db *StateDB) {
				_, err := db.DB().Exec(`UPDATE instances SET group_path = 'theirs' WHERE id = 'sess'`)
				require.NoError(t, err)
			},
			func(d *InstanceRow) { d.GroupPath = "mine" },
			"stale concurrent GroupPath conflict",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, base := launchRaceFixture(t)
			expected := publishLaunchRuntime(t, db, base)
			tc.committed(t, db)
			desired := postStartSave(base)
			tc.edit(desired)
			_, err := db.MergeInstanceSnapshots(
				[]InstanceSnapshot{{Original: base, Stored: base, Desired: desired}}, nil)
			require.ErrorContains(t, err, tc.wantErr)
			state, found, readErr := db.ReadRuntimeState(base.ID)
			require.NoError(t, readErr)
			require.True(t, found)
			require.Equal(t, expected, state, "a refused metadata save must not alter runtime")
		})
	}
}

func TestMetadataMergeCannotClobberRuntimeAuthority(t *testing.T) {
	for _, tc := range []struct {
		name    string
		advance func(t *testing.T, db *StateDB, base *InstanceRow)
		edit    func(desired *InstanceRow)
	}{
		{
			"stopped status",
			func(t *testing.T, db *StateDB, base *InstanceRow) { advanceLaunchStatus(t, db, base, "stopped") },
			func(d *InstanceRow) { d.Status = "running" },
		},
		{
			"queued status",
			func(t *testing.T, db *StateDB, base *InstanceRow) { advanceLaunchStatus(t, db, base, "queued") },
			func(d *InstanceRow) { d.Status = "running" },
		},
		{
			"diverging session binding",
			func(t *testing.T, db *StateDB, base *InstanceRow) {
				advanceLaunchBinding(t, db, base, "conv-other", time.Unix(2000, 0))
			},
			func(d *InstanceRow) {
				d.ToolData = json.RawMessage(`{"loaded_mcp_names":["neo4j"],"claude_session_id":"conv-1","claude_detected_at":1999}`)
			},
		},
		{
			"explicit stale binding clear",
			func(t *testing.T, db *StateDB, base *InstanceRow) {
				advanceLaunchBinding(t, db, base, "conv-1", time.Unix(2000, 0))
			},
			func(d *InstanceRow) {
				d.ToolData = json.RawMessage(`{"loaded_mcp_names":["neo4j"],"claude_session_id":"conv-1","claude_detected_at":0}`)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, base := launchRaceFixture(t)
			publishLaunchRuntime(t, db, base)
			tc.advance(t, db, base)
			expectedState, found, err := db.ReadRuntimeState(base.ID)
			require.NoError(t, err)
			require.True(t, found)
			expectedBinding, bindingFound, err := db.ReadRuntimeBinding(base.ID, "claude")
			require.NoError(t, err)

			desired := postStartSave(base)
			desired.Title = "updated"
			tc.edit(desired)
			_, err = db.MergeInstanceSnapshots(
				[]InstanceSnapshot{{Original: base, Stored: base, Desired: desired}}, nil)
			require.NoError(t, err)

			state, found, err := db.ReadRuntimeState(base.ID)
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, expectedState, state)
			binding, found, err := db.ReadRuntimeBinding(base.ID, "claude")
			require.NoError(t, err)
			require.Equal(t, bindingFound, found)
			require.Equal(t, expectedBinding, binding)
			rows, err := db.LoadInstances()
			require.NoError(t, err)
			require.Equal(t, "updated", rows[0].Title)
		})
	}
}
