package main

import (
	"errors"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/fleet"
	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/stretchr/testify/require"
)

// newFleetCLIStorage opens a real Storage handle on the isolated test profile.
// Each call returns a fresh handle on the SAME on-disk state.db so we can
// simulate two concurrent processes the way production does (a recovery sweep
// running for minutes while the operator adds a session in the TUI).
func newFleetCLIStorage(t *testing.T) *session.Storage {
	t.Helper()
	s, err := session.NewStorageWithProfile("")
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func fleetPersistInstance(id, title string, st session.Status) *session.Instance {
	return &session.Instance{
		ID:          id,
		Title:       title,
		ProjectPath: "/tmp/" + title,
		GroupPath:   "test",
		Command:     "claude",
		Tool:        "claude",
		Status:      st,
		CreatedAt:   time.Now().Add(-2 * time.Minute),
	}
}

// DATA-SAFETY REGRESSION GUARD for the whole recovery command.
//
// A 65-session sweep runs for minutes, so the snapshot it loaded is stale by the
// time it writes. The 2026-06-04 incident was exactly this shape: a stale
// snapshot written back through a sweeping save deleted rows another process had
// added. This test drives the EXACT seam the CLI wires (a fleet.Recoverer whose
// Persist is storage.PersistRecoveredInstances) and asserts a session added
// mid-sweep survives.
//
// A regression that swaps the persist back to the sweeping SaveInstances path
// is caught here, not only in a storage unit test.
func TestRuntimeLifecycle_FleetRecoverCLIPersistDoesNotClobberConcurrentAdd(t *testing.T) {
	// Own profile: routine saves are update-only, so rows written by other tests
	// in the shared _test profile would otherwise leak into this snapshot.
	t.Setenv("AGENTDECK_PROFILE", "_test_fleet_recover_persist")
	sweepStorage := newFleetCLIStorage(t)

	down := fleetPersistInstance("fleet-down", "down", session.StatusError)
	require.NoError(t, sweepStorage.InsertSessionAndVerify(
		down, session.NewGroupTree([]*session.Instance{down})))

	// Step 1: the sweep loads its snapshot (one down session).
	snapshot, _, err := sweepStorage.LoadWithGroups()
	require.NoError(t, err)
	require.Len(t, snapshot, 1)

	// Step 2: a concurrent process inserts a brand-new session.
	addStorage := newFleetCLIStorage(t)
	added := fleetPersistInstance("fleet-added-concurrently", "added", session.StatusRunning)
	require.NoError(t, addStorage.InsertSessionAndVerify(
		added,
		session.NewGroupTree([]*session.Instance{down, added}),
	))

	// Step 3: the sweep restarts + persists through the production seam.
	as := fleet.Assessment{Total: 1, Down: 1, Candidates: []fleet.Candidate{{
		Instance: snapshot[0],
		Health:   fleet.HealthDown,
		Status:   string(session.StatusError),
	}}}
	rec := &fleet.Recoverer{
		RestartRuntime: func(inst *session.Instance) (statedb.RuntimeState, error) {
			// Mirror the result-aware production restart seam: publish and return
			// one exact next-generation runtime tuple.
			current, found, readErr := sweepStorage.GetDB().ReadRuntimeState(inst.ID)
			if readErr != nil {
				return statedb.RuntimeState{}, readErr
			}
			if !found {
				return statedb.RuntimeState{}, errors.New("runtime state not found")
			}
			next := current
			next.Generation++
			next.StatusRevision = 0
			next.Status = string(session.StatusRunning)
			next.LastStartedAt = time.Now().UTC()
			if commitErr := sweepStorage.GetDB().CommitRuntimeTransition(
				current.Generation, inst.PersistenceIncarnation(), next); commitErr != nil {
				return statedb.RuntimeState{}, commitErr
			}
			return next, nil
		},
		Verify: func(*session.Instance) fleet.VerifyReport {
			return fleet.VerifyReport{PaneAlive: true, ToolStarted: true, Status: string(session.StatusRunning)}
		},
		Persist:   sweepStorage.PersistRecoveredInstances,
		Sleep:     func(time.Duration) {},
		NoSpacing: true,
	}

	sum := rec.Recover(as)
	require.Equal(t, 1, sum.Recovered, "sweep summary: %+v", sum)

	// Invariant 1: the concurrently-added session still exists.
	verifyStorage := newFleetCLIStorage(t)
	after, _, err := verifyStorage.LoadWithGroups()
	require.NoError(t, err)

	byID := map[string]*session.Instance{}
	for _, inst := range after {
		byID[inst.ID] = inst
	}
	require.Contains(t, byID, "fleet-added-concurrently",
		"recovery clobbered a session added during the sweep (the 2026-06-04 data-loss class)")

	// Invariant 2: the restarted session's new status was persisted.
	recovered := byID["fleet-down"]
	require.NotNil(t, recovered)
	require.Equal(t, session.StatusRunning, recovered.Status)

	// Invariant 3: the untouched row was not rewritten from the sweep's stale
	// snapshot — its own status survives.
	require.Equal(t, session.StatusRunning, byID["fleet-added-concurrently"].Status)
}

// PersistRecoveredInstances must ignore nils and validate the non-nil rows
// without replaying their stale metadata.
func TestPersistRecoveredInstances_ToleratesNilEntries(t *testing.T) {
	t.Setenv("AGENTDECK_PROFILE", "_test_fleet_persist_nil")
	storage := newFleetCLIStorage(t)

	inst := fleetPersistInstance("fleet-nil-tolerant", "nil-tolerant", session.StatusRunning)
	require.NoError(t, storage.InsertSessionAndVerify(inst, nil))
	inst.Title = "stale-recovery-title"
	require.NoError(t, storage.PersistRecoveredInstances([]*session.Instance{nil, inst, nil}))

	after, _, err := newFleetCLIStorage(t).LoadWithGroups()
	require.NoError(t, err)
	var found bool
	for _, got := range after {
		if got.ID == inst.ID {
			found = true
			require.Equal(t, "nil-tolerant", got.Title,
				"runtime validation must not persist stale recovery metadata")
		}
	}
	require.True(t, found, "the existing non-nil instance was not updated")
}

func TestRuntimeLifecycle_PersistRecoveredInstancesDoesNotResurrectConcurrentDelete(t *testing.T) {
	t.Setenv("AGENTDECK_PROFILE", "_test_fleet_persist_delete")
	recoveryStorage := newFleetCLIStorage(t)
	deleteStorage := newFleetCLIStorage(t)

	inst := fleetPersistInstance("fleet-deleted-during-recovery", "before", session.StatusError)
	require.NoError(t, recoveryStorage.InsertSessionAndVerify(inst, nil))
	snapshot, _, err := recoveryStorage.LoadWithGroups()
	require.NoError(t, err)
	require.Len(t, snapshot, 1)

	require.NoError(t, deleteStorage.DeleteInstance(inst.ID))
	snapshot[0].Title = "stale recovery snapshot"
	err = recoveryStorage.PersistRecoveredInstances(snapshot)
	require.ErrorIs(t, err, statedb.ErrInstanceParentConflict)

	row, err := recoveryStorage.GetDB().LoadInstanceByID(inst.ID)
	require.NoError(t, err)
	require.Nil(t, row, "fleet recovery resurrected a concurrently deleted parent")
	_, found, err := recoveryStorage.GetDB().ReadRuntimeState(inst.ID)
	require.NoError(t, err)
	require.False(t, found, "fleet recovery recreated deleted runtime state")
}

func TestRuntimeLifecycle_PersistRecoveredInstancesPreservesConcurrentMetadataEdits(t *testing.T) {
	t.Setenv("AGENTDECK_PROFILE", "_test_fleet_persist_same_row_metadata")
	recoveryStorage := newFleetCLIStorage(t)
	editorStorage := newFleetCLIStorage(t)

	inst := fleetPersistInstance("fleet-edited-during-recovery", "before", session.StatusError)
	inst.Account = "old-account"
	require.NoError(t, recoveryStorage.InsertSessionAndVerify(inst, nil))
	snapshot, _, err := recoveryStorage.LoadWithGroups()
	require.NoError(t, err)
	require.Len(t, snapshot, 1)

	edited, groups, err := editorStorage.LoadWithGroups()
	require.NoError(t, err)
	require.Len(t, edited, 1)
	archivedAt := time.Unix(1_900_000_000, 0).UTC()
	edited[0].Title = "renamed concurrently"
	edited[0].GroupPath = "moved/concurrently"
	edited[0].Account = "new-account"
	edited[0].ArchivedAt = archivedAt
	require.NoError(t, editorStorage.SaveWithGroups(edited, session.NewGroupTreeWithGroups(edited, groups)))

	current, found, err := recoveryStorage.GetDB().ReadRuntimeState(inst.ID)
	require.NoError(t, err)
	require.True(t, found)
	next := current
	next.Generation++
	next.StatusRevision = 0
	next.Status = string(session.StatusRunning)
	next.TmuxSession = "fleet-recovered-g1"
	next.LastStartedAt = time.Unix(1_900_000_001, 0).UTC()
	require.NoError(t, recoveryStorage.GetDB().CommitRuntimeTransition(
		current.Generation, snapshot[0].PersistenceIncarnation(), next,
	))
	snapshot[0].ApplyRuntimeState(next)
	require.NoError(t, recoveryStorage.PersistRecoveredInstances(snapshot))

	row, err := recoveryStorage.GetDB().LoadInstanceByID(inst.ID)
	require.NoError(t, err)
	require.NotNil(t, row)
	require.Equal(t, "renamed concurrently", row.Title)
	require.Equal(t, "moved/concurrently", row.GroupPath)
	require.Equal(t, "new-account", row.Account)
	require.True(t, row.ArchivedAt.Equal(archivedAt), "archived_at = %v", row.ArchivedAt)
	durable, found, err := recoveryStorage.GetDB().ReadRuntimeState(inst.ID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, next, durable)
}
