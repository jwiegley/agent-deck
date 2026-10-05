package session

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
	"github.com/stretchr/testify/require"
)

// spawnOnPrivateSocket stores one instance whose initial process is a long
// sleep, on a private tmux socket the test tears down whole: some cases leave
// behind a pane no inventory may claim.
func spawnOnPrivateSocket(t *testing.T, profile string) (*Storage, *Instance) {
	t.Helper()
	skipIfNoTmuxBinary(t)
	socketName := fmt.Sprintf("adtest-spawn-%d", time.Now().UnixNano())
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", socketName, "kill-server").Run() })
	storage, err := NewStorageWithProfile(profile)
	require.NoError(t, err)
	t.Cleanup(func() { _ = storage.Close() })
	inst := NewInstanceWithTool("spawn outcome", t.TempDir(), "customlive2099")
	inst.Command = "sleep 60"
	inst.tmuxSession.SocketName = socketName
	inst.TmuxSocketName = socketName
	t.Cleanup(func() { clearSpawnFailureRecord(inst.ID) })
	require.NoError(t, storage.InsertSessionAndVerify(inst, nil))
	return storage, inst
}

func durableRuntimeForTest(t *testing.T, storage *Storage, id string) statedb.RuntimeState {
	t.Helper()
	state, found, err := storage.GetDB().ReadRuntimeState(id)
	require.NoError(t, err)
	require.True(t, found)
	return state
}

// commitProbesSpawn replaces commitPhysicalRuntime's exact liveness probe of
// the pane tmux just accepted.
func commitProbesSpawn(t *testing.T, live bool, probeErr error) {
	t.Helper()
	old := runtimeCandidateExistsFn
	t.Cleanup(func() { runtimeCandidateExistsFn = old })
	runtimeCandidateExistsFn = func(*tmux.Session) (bool, error) { return live, probeErr }
}

// commitSeesSpawnGone makes commitPhysicalRuntime's live check prove the pane
// tmux just accepted already gone: the process died before its generation
// could be published.
func commitSeesSpawnGone(t *testing.T) {
	t.Helper()
	commitProbesSpawn(t, false, nil)
}

// Only proven absence makes a spawn gone. An indeterminate liveness probe (a
// permission error, a server that exited unexpectedly) proves nothing either
// way, so the spawn goes on to its stamp and CAS, which are the proof of life:
// a live pane is published as usual instead of being dropped as dead.
func TestSpawnDiedBeforeCommit_IndeterminateProbePublishesLiveSpawn(t *testing.T) {
	storage, inst := spawnOnPrivateSocket(t, "_test_spawn_probe_indeterminate")
	before := durableRuntimeForTest(t, storage, inst.ID)
	commitProbesSpawn(t, false, errors.New("error connecting to the server (Permission denied)"))

	runtime, err := inst.StartRuntime()
	require.NoError(t, err)
	require.Equal(t, before.Generation+1, runtime.Generation, "the live spawn is published")
	require.Equal(t, runtime, durableRuntimeForTest(t, storage, inst.ID))
	require.Equal(t, runtime, inst.RuntimeState())
}

// When the stamp fails as well, the result is the reconciling partial success
// of a spawn that could not be published, never the silent nil of a spawn
// proved gone.
func TestSpawnDiedBeforeCommit_IndeterminateProbeKeepsReconcilingPartialSuccess(t *testing.T) {
	storage, inst := spawnOnPrivateSocket(t, "_test_spawn_probe_partial")
	before := durableRuntimeForTest(t, storage, inst.ID)
	commitProbesSpawn(t, false, errors.New("server exited unexpectedly"))
	oldStamp := runtimeCandidateStampFn
	t.Cleanup(func() { runtimeCandidateStampFn = oldStamp })
	runtimeCandidateStampFn = func(*tmux.Session, statedb.RuntimeState, string, string) error {
		return errors.New("server exited unexpectedly")
	}

	_, err := inst.StartRuntime()
	var partial *RestartPartialSuccessError
	require.ErrorAs(t, err, &partial, "start = %v, want a partial success", err)
	require.True(t, partial.NeedsReconciliation)
	require.Equal(t, "start", partial.Operation)
	require.Equal(t, before, durableRuntimeForTest(t, storage, inst.ID), "nothing was published")
}

// A start whose pane died before publication published nothing and left
// nothing live. Like upstream, it returns nil once tmux accepted the spawn; it
// reports the canonical runtime its transition kept, never an uncommitted
// successor, and no durability warning. The death stays spawn verification's
// to report, and its error verdict lands on that canonical runtime.
func TestSpawnDiedBeforeCommit_StartPublishesNothingAndReturnsNil(t *testing.T) {
	storage, inst := spawnOnPrivateSocket(t, "_test_spawn_died_start")
	before := durableRuntimeForTest(t, storage, inst.ID)
	commitSeesSpawnGone(t)

	runtime, err := inst.StartRuntime()
	require.NoError(t, err, "tmux accepted the spawn")
	require.Equal(t, before, runtime, "start reports the canonical runtime it kept")
	require.Equal(t, before, durableRuntimeForTest(t, storage, inst.ID), "nothing was published")
	require.Equal(t, before, inst.RuntimeState(), "the instance holds the runtime start reports, not its starting status")
	canonical, failure, warning := ConsumePhysicalRuntimeResult(inst, runtime, err, nil)
	require.NoError(t, failure)
	require.Empty(t, warning, "there is no durability to reconcile")
	require.Equal(t, before.Generation, canonical.Generation, "no uncommitted successor was adopted")

	require.NoError(t, PersistSpawnFailureStatus(storage, inst))
	want := before
	want.Status = string(StatusError)
	want.StatusRevision++
	require.Equal(t, want, durableRuntimeForTest(t, storage, inst.ID))
}

// StartWithMessage has no live pane to deliver its message to: it must say so
// rather than report the message sent, and still publish nothing.
func TestSpawnDiedBeforeCommit_StartWithMessageReportsMessageUndelivered(t *testing.T) {
	storage, inst := spawnOnPrivateSocket(t, "_test_spawn_died_message")
	before := durableRuntimeForTest(t, storage, inst.ID)
	commitSeesSpawnGone(t)

	runtime, err := inst.StartWithMessageRuntime("hello")
	require.True(t, InitialMessageUndelivered(err), "start with message = %v, want the message reported undelivered", err)
	var partial *RestartPartialSuccessError
	require.ErrorAs(t, err, &partial)
	require.False(t, partial.NeedsReconciliation, "nothing was published to reconcile")
	require.Equal(t, before, partial.Runtime)
	require.Equal(t, before, runtime)
	require.Equal(t, before, durableRuntimeForTest(t, storage, inst.ID), "nothing was published")

	// The operator is told the session exited, not that it started, and no
	// reconciliation of a runtime that never lived is attempted or reported.
	require.True(t, inst.ReconcileRestartResult(err) == err, "a spawn proved gone has nothing to reconcile")
	canonical, failure, warning := ConsumePhysicalRuntimeResult(inst, runtime, err, nil)
	require.NoError(t, failure)
	require.Equal(t, "session "+inst.ID+" exited before its initial message could be delivered", warning)
	require.Equal(t, before, canonical)
	require.Equal(t, before, inst.RuntimeState())
}

// A restart whose replacement died before publication behaves the same way:
// nil, the canonical runtime the transition kept (the stopped predecessor),
// and no published successor. The fallback recreate gave the instance a new
// tmux name at the kept generation; the instance gives it back, so the result,
// the instance and the durable row agree, and the error verdict spawn
// verification then records lands on that same row.
func TestSpawnDiedBeforeCommit_RestartPublishesNothingAndReturnsNil(t *testing.T) {
	storage, inst := spawnOnPrivateSocket(t, "_test_spawn_died_restart")
	require.NoError(t, inst.Start())
	started := durableRuntimeForTest(t, storage, inst.ID)
	require.Equal(t, uint64(1), started.Generation)
	commitSeesSpawnGone(t)

	runtime, err := inst.RestartRuntime()
	require.NoError(t, err, "tmux accepted the replacement spawn")
	durable := durableRuntimeForTest(t, storage, inst.ID)
	require.Equal(t, started.Generation, durable.Generation, "no successor was published")
	require.Equal(t, started.TmuxSession, durable.TmuxSession)
	require.Equal(t, durable, runtime, "restart reports the canonical runtime it kept")
	require.Equal(t, durable, inst.RuntimeState(), "the instance gave back the replacement's uncommitted tmux name")
	canonical, failure, warning := ConsumePhysicalRuntimeResult(inst, runtime, err, nil)
	require.NoError(t, failure)
	require.Empty(t, warning)
	require.Equal(t, durable, canonical, "the consumed runtime is the durable row")
	require.Equal(t, durable, inst.RuntimeState())

	require.NoError(t, PersistSpawnFailureStatus(storage, inst))
	want := durable
	want.Status = string(StatusError)
	want.StatusRevision++
	require.Equal(t, want, durableRuntimeForTest(t, storage, inst.ID))
	require.Equal(t, want, inst.RuntimeState(), "the row and the instance agree")
}

// A fresh restart whose replacement died before publication also gets back
// the conversation binding prepareFresh cleared: nothing was published, so the
// session still resumes the conversation it had.
func TestSpawnDiedBeforeCommit_FreshRestartKeepsItsBinding(t *testing.T) {
	skipIfNoTmuxBinary(t)
	isolatedHomeDir(t)
	previousDB := statedb.GetGlobal()
	statedb.SetGlobal(nil)
	t.Cleanup(func() { statedb.SetGlobal(previousDB) })
	const conversation = "gemini-conversation-before-fresh-restart"
	stub := filepath.Join(t.TempDir(), "gemini")
	require.NoError(t, os.WriteFile(stub, []byte("#!/bin/sh\nsleep 30\n"), 0o755))
	withConfig(t, &UserConfig{Gemini: GeminiSettings{Command: stub}})

	socketName := fmt.Sprintf("adtest-fresh-gone-%d", time.Now().UnixNano())
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", socketName, "kill-server").Run() })
	inst := NewInstanceWithTool("fresh gone", t.TempDir(), "gemini")
	inst.Command = "gemini"
	inst.tmuxSession.SocketName = socketName
	inst.TmuxSocketName = socketName
	require.NoError(t, inst.Start())
	inst.GeminiSessionID = conversation
	inst.GeminiDetectedAt = time.Now()
	commitSeesSpawnGone(t)

	runtime, err := inst.RestartFreshRuntime()
	require.NoError(t, err, "tmux accepted the replacement spawn")
	require.Equal(t, conversation, inst.GeminiSessionID, "the cleared binding came back")
	require.Equal(t, runtime, inst.RuntimeState())
}

// A live spawn whose publication failed is a partial success, and its warning
// names the call that produced it: a start is not a restart.
func TestPartialSuccessNamesStartOrRestart(t *testing.T) {
	storage, inst := spawnOnPrivateSocket(t, "_test_partial_success_operation")
	oldStamp := runtimeCandidateStampFn
	t.Cleanup(func() { runtimeCandidateStampFn = oldStamp })
	failStamp := func() {
		runtimeCandidateStampFn = func(*tmux.Session, statedb.RuntimeState, string, string) error {
			return errors.New("injected stamp failure")
		}
	}

	failStamp()
	_, err := inst.StartRuntime()
	var partial *RestartPartialSuccessError
	require.ErrorAs(t, err, &partial)
	require.True(t, partial.NeedsReconciliation)
	require.Equal(t, "start", partial.Operation)
	require.True(t, strings.HasPrefix(err.Error(), "start completed for "+inst.ID+" but runtime generation persistence failed"), "start warning = %q", err)

	// Publish a real generation, then fail the restart's publication.
	_ = exec.Command("tmux", "-L", inst.TmuxSocketName, "kill-server").Run()
	runtimeCandidateStampFn = oldStamp
	fresh := durableRuntimeForTest(t, storage, inst.ID)
	inst.ApplyRuntimeState(fresh)
	require.NoError(t, inst.Start())
	failStamp()
	_, err = inst.RestartRuntime()
	require.ErrorAs(t, err, &partial)
	require.True(t, partial.NeedsReconciliation)
	require.Equal(t, "restart", partial.Operation)
	require.True(t, strings.HasPrefix(err.Error(), "restart completed for "+inst.ID+" but runtime generation persistence failed"), "restart warning = %q", err)
}
