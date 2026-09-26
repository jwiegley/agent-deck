package session

import (
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
	"github.com/stretchr/testify/require"
)

// A snapshot save drops the runtime-owned Status, so an operator verdict
// (queued, or error after a failed spawn) reaches the store only through the
// status CAS, and only for the runtime generation that earned it.
func TestPersistSelectedStatusUsesRuntimeCAS(t *testing.T) {
	storage, err := NewStorageWithProfile("_test_persist_selected_status")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, storage.Close()) })

	inst := NewInstanceWithTool("persist-selected-status", t.TempDir(), "customfail2099")
	require.NoError(t, storage.InsertSessionAndVerify(inst, nil))

	for _, status := range []Status{StatusQueued, StatusError} {
		before := inst.RuntimeState()
		require.NoError(t, PersistSelectedStatus(storage, inst, status))
		after := inst.RuntimeState()
		require.Equal(t, status, inst.GetStatusThreadSafe())
		require.Equal(t, before.Generation, after.Generation)
		require.Equal(t, before.StatusRevision+1, after.StatusRevision)
		durable, found, err := storage.GetDB().ReadRuntimeState(inst.ID)
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, after, durable)
	}

	// A replacement started after the verdict was decided must stay untouched.
	decided := inst.RuntimeState()
	replacement := decided
	replacement.Generation++
	replacement.StatusRevision = 0
	replacement.Status = string(StatusRunning)
	require.NoError(t, storage.GetDB().CommitRuntimeTransition(decided.Generation, inst.PersistenceIncarnation(), replacement))
	require.ErrorIs(t, PersistSelectedStatus(storage, inst, StatusError), statedb.ErrStatusRevisionConflict)
	require.Equal(t, decided, inst.RuntimeState(), "a refused verdict must not change the instance")
	durable, found, err := storage.GetDB().ReadRuntimeState(inst.ID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, replacement, durable)
}

func TestPersistSelectedStatusRequiresStorage(t *testing.T) {
	inst := NewInstance("persist-selected-status-nil", t.TempDir())
	before := inst.RuntimeState()
	require.Error(t, PersistSelectedStatus(nil, inst, StatusQueued))
	require.Error(t, PersistSpawnFailureStatus(nil, inst))
	require.Equal(t, before, inst.RuntimeState())
}

// persistSpawnFailureFixture stores one instance and has it adopt the
// never-committed successor a dead-before-commit start leaves behind.
func persistSpawnFailureFixture(t *testing.T, profile string) (*Storage, *Instance, statedb.RuntimeState) {
	t.Helper()
	installRuntimeLifecycleTestSeams(t)
	storage, err := NewStorageWithProfile(profile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, storage.Close()) })
	inst := NewInstanceWithTool("spawn-failure-verdict", t.TempDir(), "customfail2099")
	require.NoError(t, storage.InsertSessionAndVerify(inst, nil))
	durable := inst.RuntimeState()
	phantom := durable
	phantom.Generation++
	phantom.StatusRevision = 0
	phantom.Status = string(StatusStarting)
	require.True(t, inst.ApplyRuntimeState(phantom))
	return storage, inst, durable
}

// The spawn died before its generation was committed: the error verdict
// lands on the durable predecessor, not on the phantom the CAS would refuse.
func TestPersistSpawnFailureStatusErrorsDurablePredecessor(t *testing.T) {
	storage, inst, durable := persistSpawnFailureFixture(t, "_test_spawn_failure_predecessor")

	require.NoError(t, PersistSpawnFailureStatus(storage, inst))
	want := durable
	want.Status = string(StatusError)
	want.StatusRevision++
	got, found, err := storage.GetDB().ReadRuntimeState(inst.ID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, want, got)
	require.Equal(t, want, inst.RuntimeState())
}

// A runtime proven live for the durable generation is a replacement, not the
// failed spawn, and must not inherit the error.
func TestPersistSpawnFailureStatusSparesLiveRuntime(t *testing.T) {
	storage, inst, durable := persistSpawnFailureFixture(t, "_test_spawn_failure_live")
	runtimeCandidateInventoryFn = func(string, string) ([]tmux.RuntimeCandidate, error) {
		return []tmux.RuntimeCandidate{{
			SessionID: "$1", PaneID: "%1", SessionName: durable.TmuxSession, SocketName: durable.TmuxSocketName,
			InstanceID: durable.InstanceID, Generation: durable.Generation, GenerationKnown: true, PanePID: 4242,
			StatusRevision: durable.StatusRevision, Status: durable.Status,
			LastStartedUnixNano: durable.LastStartedAt.UnixNano(), StateKnown: true, BindingKnown: true,
		}}, nil
	}

	require.ErrorIs(t, PersistSpawnFailureStatus(storage, inst), statedb.ErrRuntimeGenerationConflict)
	got, found, err := storage.GetDB().ReadRuntimeState(inst.ID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, durable, got)
}
