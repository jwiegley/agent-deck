package main

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

func runtimeLifecycleTestState(id string, generation uint64) statedb.RuntimeState {
	return statedb.RuntimeState{
		InstanceID: id, Generation: generation, StatusRevision: 2,
		TmuxSession: "agentdeck-test", TmuxSocketName: "cli-test",
		Status: string(session.StatusRunning), LastStartedAt: time.Unix(1700000000, 0).UTC(),
	}
}

func TestRuntimeLifecycle_CLIConsumesReturnedState(t *testing.T) {
	inst := &session.Instance{ID: "cli-success", Status: session.StatusIdle}
	want := runtimeLifecycleTestState(inst.ID, 5)

	failure, warning := consumeRuntimeResult(inst, want, nil)
	if failure != nil || warning != "" {
		t.Fatalf("failure = %v, warning = %q", failure, warning)
	}
	if got := inst.RuntimeState(); got != want {
		t.Fatalf("runtime = %+v, want %+v", got, want)
	}
}

func TestRuntimeLifecycle_CLILeavesPhysicalFailureInFailureChannel(t *testing.T) {
	inst := &session.Instance{ID: "cli-failure", Status: session.StatusIdle}
	spawnErr := errors.New("spawn failed")

	failure, warning := consumeRuntimeResult(inst, statedb.RuntimeState{}, spawnErr)
	if !errors.Is(failure, spawnErr) || warning != "" {
		t.Fatalf("failure = %v, warning = %q", failure, warning)
	}
	if got := inst.RuntimeState().Generation; got != 0 {
		t.Fatalf("generation = %d, physical failure must not apply a runtime", got)
	}
}

func TestRuntimeLifecycle_CLIPreservesPartialCandidateAsWarning(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	inst := &session.Instance{ID: "cli-partial", Status: session.StatusIdle}
	want := runtimeLifecycleTestState(inst.ID, 6)
	partial := &session.RestartPartialSuccessError{
		InstanceID: inst.ID, Runtime: want, NeedsReconciliation: true,
		Err: errors.New("database is locked"),
	}

	failure, warning := consumeRuntimeResult(inst, want, partial)
	if failure != nil {
		t.Fatalf("failure = %v, completed physical start must not enter retry/rollback", failure)
	}
	if !strings.Contains(warning, "durability reconciliation failed") {
		t.Fatalf("warning = %q, want incomplete reconciliation warning", warning)
	}
	if got := inst.RuntimeState(); got != want {
		t.Fatalf("runtime = %+v, want preserved candidate %+v", got, want)
	}
}

func TestRuntimeLifecycle_CLIKeepsEqualGenerationWinner(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	inst := &session.Instance{ID: "cli-winner", Status: session.StatusIdle}
	winner := runtimeLifecycleTestState(inst.ID, 8)
	winner.TmuxSession = "winner"
	winner.TmuxSocketName = "socket-winner"
	loser := winner
	loser.TmuxSession = "loser"
	loser.TmuxSocketName = "socket-loser"
	if !inst.ApplyRuntimeState(winner) {
		t.Fatal("winner apply was rejected")
	}
	partial := &session.RestartPartialSuccessError{
		InstanceID: inst.ID, Runtime: loser, NeedsReconciliation: true,
		Err: errors.New("lost runtime CAS"),
	}
	failure, warning := consumeRuntimeResult(inst, loser, partial)
	if failure != nil || warning == "" {
		t.Fatalf("failure=%v warning=%q", failure, warning)
	}
	if got := inst.RuntimeState(); got != winner {
		t.Fatalf("CLI restored loser: got %#v want %#v", got, winner)
	}
}

// F6: a start whose pane is live but whose initial message never reached it
// keeps exit 0 but must not claim the message was sent. `session start` (both
// paths) and `launch` render their verdict through renderStartSuccess, which
// the routing matrix requires each of them to call.
func TestRenderStartSuccess(t *testing.T) {
	type fields = map[string]interface{}
	// `session start` echoes its spawn receipt; launch reports the committed
	// row's tmux name and Claude id itself (addLaunchStateJSON).
	start := startSuccess{verb: "Started", id: "id-1", title: "alpha", tmux: "agentdeck_alpha", claudeSessionID: "claude-1"}
	launch := startSuccess{verb: "Launched", id: "id-1", title: "alpha"}
	with := func(base startSuccess, edit func(*startSuccess)) startSuccess {
		edit(&base)
		return base
	}
	merged := func(base fields, extra fields) fields {
		out := fields{}
		for k, v := range base {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	startJSON := fields{"success": true, "id": "id-1", "title": "alpha", "tmux": "agentdeck_alpha", "claude_session_id": "claude-1"}
	launchJSON := fields{"success": true, "id": "id-1", "title": "alpha"}
	sent := fields{"message": "hi", "message_pending": false}
	pending := fields{"message": "hi", "message_pending": true}

	tests := []struct {
		name     string
		in       startSuccess
		wantLine string
		wantJSON fields
	}{
		{"start", start, "Started session: alpha", startJSON},
		{"start with warning", with(start, func(s *startSuccess) { s.warning = "reconciliation failed" }),
			"Started session: alpha", merged(startJSON, fields{"warning": "reconciliation failed"})},
		{"start delivered", with(start, func(s *startSuccess) { s.message = "hi" }),
			"Started session: alpha (message sent)", merged(startJSON, sent)},
		{"start undelivered", with(start, func(s *startSuccess) { s.message, s.messageUndelivered = "hi", true }),
			"Started session: alpha (message not delivered)", merged(startJSON, pending)},
		{"launch delivered", with(launch, func(s *startSuccess) { s.message = "hi" }),
			"Launched session: alpha (message sent)", merged(launchJSON, sent)},
		{"launch undelivered", with(launch, func(s *startSuccess) { s.message, s.messageUndelivered = "hi", true }),
			"Launched session: alpha (message not delivered)", merged(launchJSON, pending)},
		{"launch --no-wait", with(launch, func(s *startSuccess) { s.message, s.messageDeferred = "hi", true }),
			"Launched session: alpha (message sent with --no-wait)", merged(launchJSON, pending)},
		{"launch --no-wait undelivered", with(launch, func(s *startSuccess) {
			s.message, s.messageDeferred, s.messageUndelivered = "hi", true, true
		}), "Launched session: alpha (message not delivered)", merged(launchJSON, pending)},
		{"launch --no-wait without message", with(launch, func(s *startSuccess) { s.messageDeferred = true }),
			"Launched session: alpha", launchJSON},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := fields{}
			if line := renderStartSuccess(tt.in, got); line != tt.wantLine {
				t.Errorf("line = %q, want %q", line, tt.wantLine)
			}
			if !reflect.DeepEqual(got, tt.wantJSON) {
				t.Errorf("json = %#v, want %#v", got, tt.wantJSON)
			}
		})
	}
}
