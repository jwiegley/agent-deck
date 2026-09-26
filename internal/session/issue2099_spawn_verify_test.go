package session

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Issue #2099: `session start` printed "✓ Started" with exit 0 even when no
// tmux session was ever created. Start() can return nil while the pane is
// already gone (the initial process died before the first fast-death tick,
// or a sibling-spawn gate short-circuited it), and nothing read the result
// back. VerifySpawned is that read-back: it confirms the tmux session exists
// after Start()/Restart() and otherwise returns the recorded spawn-failure
// reason so the CLI can exit non-zero with it.

// TestIssue2099_VerifySpawnedReportsRecordedFailure: a spawn-failure record
// is present and the tmux session is gone → the error carries the record.
// No tmux needed: the isolated socket has no such session.
func TestIssue2099_VerifySpawnedReportsRecordedFailure(t *testing.T) {
	inst := NewInstance("test-2099-recorded", "/tmp")
	inst.Tool = "custom2099"
	t.Cleanup(func() { clearSpawnFailureRecord(inst.ID) })

	require.NoError(t, writeSpawnFailureRecord(SpawnFailureRecord{
		InstanceID:  inst.ID,
		Tool:        inst.Tool,
		Command:     "npx broken-tool",
		Reason:      "spawn_died_fast",
		DyingOutput: "command not found: broken-tool",
		ElapsedMs:   240,
	}))

	err := inst.VerifySpawned(300 * time.Millisecond)
	require.Error(t, err)

	var spawnErr *SpawnFailedError
	require.True(t, errors.As(err, &spawnErr), "error must be a *SpawnFailedError, got %T", err)
	require.NotNil(t, spawnErr.Record)
	assert.Equal(t, "spawn_died_fast", spawnErr.Record.Reason)
	assert.Contains(t, err.Error(), "spawn_died_fast")
	assert.Contains(t, err.Error(), "command not found: broken-tool")
}

// TestIssue2099_VerifySpawnedNoSessionNoRecord: nothing was recorded but the
// tmux session still does not exist → still an error (never a false
// "Started"), naming the missing tmux session.
func TestIssue2099_VerifySpawnedNoSessionNoRecord(t *testing.T) {
	inst := NewInstance("test-2099-missing", "/tmp")
	inst.Tool = "custom2099"
	t.Cleanup(func() { clearSpawnFailureRecord(inst.ID) })

	err := inst.VerifySpawned(300 * time.Millisecond)
	require.Error(t, err)

	var spawnErr *SpawnFailedError
	require.True(t, errors.As(err, &spawnErr), "error must be a *SpawnFailedError, got %T", err)
	assert.Nil(t, spawnErr.Record)
	assert.Contains(t, err.Error(), inst.tmuxSession.Name)
	assert.Contains(t, err.Error(), "does not exist")
}

// The initial process exits before the first watcher tick. Start returns nil
// once tmux accepted the spawn, even when the pane is gone before its
// generation could be published; the CLI consumes that result and still
// verifies the pane before reporting success.
func TestIssue2099_VerifySpawnedDiesBeforePane(t *testing.T) {
	skipIfNoTmuxBinary(t)

	inst := NewInstance("test-2099-diesfast", "/tmp")
	inst.Tool = "customfail2099"
	inst.Command = "sh -c 'exit 3'"
	t.Cleanup(func() {
		_ = inst.Kill()
		clearSpawnFailureRecord(inst.ID)
	})

	runtime, startErr := inst.StartRuntime()
	require.NoError(t, startErr, "tmux accepted the spawn; verification must report its death")
	_, failure, warning := ConsumePhysicalRuntimeResult(inst, runtime, startErr, nil)
	require.NoError(t, failure, "tmux accepted the spawn; verification must report its death")
	require.Empty(t, warning, "nothing was published, so there is no durability warning")

	err := inst.VerifySpawned(3 * time.Second)
	require.Error(t, err, "a session whose pane died must not verify as started")

	var spawnErr *SpawnFailedError
	require.True(t, errors.As(err, &spawnErr), "error must be a *SpawnFailedError, got %T", err)
	require.NotNil(t, spawnErr.Record, "the fast-death watcher's record must be read back")
	assert.Equal(t, "spawn_died_fast", spawnErr.Record.Reason)
}

// TestIssue2099_VerifySpawnedHealthy: a session whose pane stays alive
// verifies cleanly and quickly.
func TestIssue2099_VerifySpawnedHealthy(t *testing.T) {
	skipIfNoTmuxBinary(t)

	inst := NewInstance("test-2099-healthy", "/tmp")
	inst.Tool = "customlive2099"
	inst.Command = "sleep 60"
	t.Cleanup(func() {
		_ = inst.Kill()
		clearSpawnFailureRecord(inst.ID)
	})

	require.NoError(t, inst.Start())
	started := time.Now()
	require.NoError(t, inst.VerifySpawned(3*time.Second))
	assert.Less(t, time.Since(started), 2*time.Second, "a live session must verify without waiting out the window")
}
