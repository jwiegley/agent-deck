package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// Defect 317: `session stop` kills the pane and then saves stopped. Another
// process (the notify daemon, a TUI) that samples the pane in between sees it
// gone and commits error. That refused the stop's save (stale concurrent
// Status conflict), the stop exited 1, the row stayed error, and the
// idle-death watch (#2806) told the parent its child had died.
//
// This fork's stop does not have that window: KillCaptured reserves the
// runtime under a destruction sentinel that status writers cannot overwrite,
// holds the spawn lock across the kill, and completes as stopped
// (TestRuntimeLifecycle_StatusPublisherCannotPassReservedKillBarrier,
// TestRuntimeLifecycle_ReplacementAfterReservationIsNotTerminated). Upstream's
// race cases inject a writer through session.StopAfterKillHook, which no stop
// path of the fork calls, so only the plain stop is kept here.

func defect317StartedSession(t *testing.T, title string) (home, id string) {
	t.Helper()
	if testing.Short() {
		t.Skip("subprocess CLI test skipped in short mode")
	}
	home = t.TempDir()
	id = addExitStatusSession(t, home, title, "#!/bin/sh\nsleep 600\n")
	stdout, stderr, code := runAgentDeck(t, home, "-p", "_test", "session", "start", id)
	require.Zero(t, code, "%s %s", stdout, stderr)
	t.Setenv("HOME", home)
	t.Setenv("AGENTDECK_PROFILE", "_test")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	return home, id
}

// defect317Stop runs the legacy stop path (AGENTDECK_CORE_REGISTRY=0, and
// `session command /exit`) in process. The default core path is covered in
// internal/core.
func defect317Stop(t *testing.T, id string) error {
	t.Helper()
	storage, instances, groups, err := loadSessionData("_test")
	require.NoError(t, err)
	defer storage.Close()
	inst, msg, _ := ResolveSession(id, instances)
	require.NotNil(t, inst, msg)
	require.True(t, inst.Exists(), "the session must be running before the stop")
	_, _, err = stopInstanceAndSave(storage, inst, inst.CaptureRuntimeSelection(), instances, groups)
	return err
}

func TestDefect317_StopWithoutARaceSavesStopped(t *testing.T) {
	_, id := defect317StartedSession(t, "stop317-plain")
	require.NoError(t, defect317Stop(t, id))
	require.Equal(t, string(session.StatusStopped), issue2806RowStatus(t, id))
}
