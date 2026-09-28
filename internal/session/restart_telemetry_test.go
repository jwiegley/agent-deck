package session

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/telemetry"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
	"github.com/stretchr/testify/require"
)

// grantTelemetryForTest lifts every telemetry hard-off for the test, grants
// consent in the package's isolated HOME, and returns a counter of the
// session.end events of one kind spooled since. Cleanup removes the state and
// spool, so no later test inherits the consent. It keeps the package HOME
// because a restart leaves ownership attribution writing under the data dir,
// which a t.TempDir HOME would race on removal.
func grantTelemetryForTest(t *testing.T) func(telemetry.EndKind) int {
	t.Helper()
	telemetry.EnableForTest(t)
	telemetry.SetTerminalForTest(t, true)
	telemetry.ClearCIForTest(t)
	for _, key := range []string{telemetry.EnvTelemetry, telemetry.EnvDoNotTrack} {
		t.Setenv(key, "")
		os.Unsetenv(key) // set-but-empty is a hard off
	}
	telemetry.SetConfigDisabled(false)
	statePath, err := telemetry.StatePath()
	require.NoError(t, err)
	spoolPath := filepath.Join(filepath.Dir(statePath), telemetry.SpoolFileName)
	removeTelemetry := func() {
		for _, path := range []string{statePath, spoolPath} {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Errorf("remove %s: %v", path, err)
			}
		}
	}
	removeTelemetry()
	t.Cleanup(removeTelemetry)
	state := telemetry.LoadState()
	require.NoError(t, telemetry.Grant(state, "9.9.9", time.Now()))
	require.NoError(t, telemetry.SaveState(state))

	return func(kind telemetry.EndKind) int {
		t.Helper()
		data, err := os.ReadFile(spoolPath)
		if errors.Is(err, os.ErrNotExist) {
			return 0
		}
		require.NoError(t, err)
		ends := 0
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if line == "" {
				continue
			}
			var event struct {
				Name  string         `json:"e"`
				Props map[string]any `json:"p"`
			}
			require.NoError(t, json.Unmarshal([]byte(line), &event), "spool line %q", line)
			if event.Name == "session.end" && event.Props["end_kind"] == string(kind) {
				ends++
			}
		}
		return ends
	}
}

// A lock loser that adopts a concurrent durable winner stops and spawns
// nothing. The winner's own restart records the replacement, so the loser
// records no session.end(restart) of its own.
func TestRestartTelemetry_AdoptedWinnerRecordsNothing(t *testing.T) {
	for _, entrypoint := range []struct {
		name    string
		restart func(*Instance) error
	}{
		{"RestartRuntime", func(inst *Instance) error { _, err := inst.RestartRuntime(); return err }},
		{"RestartWithEnvRuntime", func(inst *Instance) error { _, err := inst.RestartWithEnvRuntime(nil); return err }},
		{"Restart", (*Instance).Restart},
	} {
		t.Run(entrypoint.name, func(t *testing.T) {
			ends := grantTelemetryForTest(t)
			installRuntimeLifecycleTestSeams(t)
			db, inst := runtimeLifecycleTestDB(t, "pi", nil)
			initial, found, err := db.ReadRuntimeState(inst.ID)
			require.NoError(t, err)
			require.True(t, found)
			winner := initial
			winner.Generation++
			winner.TmuxSession = "runtime-concurrent-winner"
			published := false
			runtimeTransitionObservedFn = func() {
				if !published {
					published = true
					require.NoError(t, db.CommitRuntimeTransition(initial.Generation, inst.PersistenceIncarnation(), winner))
				}
			}
			respawns := 0
			runtimeCandidateRespawnFn = func(*tmux.Session, tmux.RuntimeGenerationCandidate, string) error {
				respawns++
				return nil
			}

			require.NoError(t, entrypoint.restart(inst))
			require.Equal(t, winner.Generation, inst.RuntimeState().Generation, "the loser adopted the winner")
			require.Zero(t, respawns)
			require.Zero(t, ends(telemetry.EndRestart))
		})
	}
}
