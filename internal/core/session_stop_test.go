package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/telemetry"
)

// grantTelemetryForTest lifts every telemetry hard-off for the test, grants
// consent in the package's isolated HOME, and returns a counter of the
// session.end events of one kind spooled since. The state and spool are
// removed at cleanup, so no later test inherits the consent.
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
	if err != nil {
		t.Fatal(err)
	}
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
	if err := telemetry.Grant(state, "9.9.9", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := telemetry.SaveState(state); err != nil {
		t.Fatal(err)
	}

	return func(kind telemetry.EndKind) int {
		t.Helper()
		data, err := os.ReadFile(spoolPath)
		if errors.Is(err, os.ErrNotExist) {
			return 0
		}
		if err != nil {
			t.Fatal(err)
		}
		ends := 0
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if line == "" {
				continue
			}
			var event struct {
				Name  string         `json:"e"`
				Props map[string]any `json:"p"`
			}
			if err := json.Unmarshal([]byte(line), &event); err != nil {
				t.Fatalf("spool line %q: %v", line, err)
			}
			if event.Name == "session.end" && event.Props["end_kind"] == string(kind) {
				ends++
			}
		}
		return ends
	}
}

// The registry serves the default `session stop`, so it records
// session.end(stop) as the legacy handler does: once, after the kill
// succeeds. A stop that kills nothing records nothing.
func TestSessionStopRecordsOneSessionEnd(t *testing.T) {
	requireTmux(t)
	ends := grantTelemetryForTest(t)
	profile := fmt.Sprintf("_core_stop_telemetry_%d", time.Now().UnixNano())
	seedStore(t, profile, nil, session.NewInstance("alpha", t.TempDir()))
	stopLiveSessionsAtCleanup(t, profile)
	r := testRegistry(t, Deps{})
	if out, res := Invoke[SessionStartOut](context.Background(), r, IDSessionStart, SessionStartIn{Profile: profile, Session: "alpha", NoWait: true}); res.Err != nil || out.Status != StartStatusStarted {
		t.Fatalf("start alpha: %+v %v", out, res.Err)
	}

	_, res := Invoke[SessionStopOut](context.Background(), r, IDSessionStop, SessionStopIn{Profile: profile, Session: "alpha"})
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	res.Finish()
	if got := ends(telemetry.EndStop); got != 1 {
		t.Fatalf("session.end(stop) events after stop = %d, want 1", got)
	}

	res = r.Run(context.Background(), IDSessionStop, SessionStopIn{Profile: profile, Session: "alpha"})
	wantCode(t, res.Err, CodeNotRunning, "session 'alpha' is not running")
	if got := ends(telemetry.EndStop); got != 1 {
		t.Fatalf("session.end(stop) events after a stop of a stopped session = %d, want 1", got)
	}
}
