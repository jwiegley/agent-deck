package core

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

// The registry is the default path for session start/stop/restart. These
// tests hold its command bodies to the fork's runtime authority: partial
// successes are warnings, and operator verdicts (queued, error) are durable.

func requireTmux(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}
}

func setStartRuntimeHook(t *testing.T, fn func(*session.Instance, string) (statedb.RuntimeState, error)) {
	t.Helper()
	old := startRuntimeHook
	startRuntimeHook = fn
	t.Cleanup(func() { startRuntimeHook = old })
}

func setRestartRuntimeHook(t *testing.T, fn func(*session.Instance, map[string]string) (statedb.RuntimeState, error)) {
	t.Helper()
	old := restartRuntimeHook
	restartRuntimeHook = fn
	t.Cleanup(func() { restartRuntimeHook = old })
}

// reconcileSucceeds stands in for durability reconciliation of an injected
// publication failure, which has no binding plan to replay.
func reconcileSucceeds(t *testing.T) {
	t.Helper()
	old := reconcileRestartHook
	reconcileRestartHook = func(*session.Instance, error) error { return nil }
	t.Cleanup(func() { reconcileRestartHook = old })
}

var errInjectedPublication = errors.New("injected runtime publication failure")

// publicationFailed reports a completed physical runtime the way a failed
// durability publication does: the pane is live, only persistence failed.
func publicationFailed(inst *session.Instance, runtime statedb.RuntimeState, err error) (statedb.RuntimeState, error) {
	if err != nil {
		return runtime, err
	}
	return runtime, &session.RestartPartialSuccessError{
		InstanceID: inst.ID, Runtime: runtime, NeedsReconciliation: true, Err: errInjectedPublication,
	}
}

// stopLiveSessionsAtCleanup kills whatever the test left running in profile.
func stopLiveSessionsAtCleanup(t *testing.T, profile string) {
	t.Helper()
	t.Cleanup(func() {
		storage, err := session.NewStorageWithProfile(profile)
		if err != nil {
			t.Error(err)
			return
		}
		defer storage.Close()
		instances, _, err := storage.LoadWithGroups()
		if err != nil {
			t.Error(err)
			return
		}
		for _, inst := range instances {
			if inst.Exists() {
				if err := inst.KillCaptured(inst.CaptureRuntimeSelection()); err != nil {
					t.Errorf("stop %s: %v", inst.Title, err)
				}
			}
		}
	})
}

func recordEvents(ctx context.Context) (context.Context, *[]Event) {
	var events []Event
	return WithObserver(ctx, func(ev Event) { events = append(events, ev) }), &events
}

func requireRuntimeWarning(t *testing.T, events []Event, id, warning string) {
	t.Helper()
	for _, ev := range events {
		if ev.Kind == EventRuntimeWarning && ev.ID == id && ev.Message == warning {
			return
		}
	}
	t.Fatalf("no %s event for %s with %q in %+v", EventRuntimeWarning, id, warning, events)
}

func durableRuntime(t *testing.T, profile, id string) statedb.RuntimeState {
	t.Helper()
	storage, err := session.NewStorageWithProfile(profile)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	state, found, err := storage.GetDB().ReadRuntimeState(id)
	if err != nil || !found {
		t.Fatalf("runtime of %s: found=%v err=%v", id, found, err)
	}
	return state
}

func TestSessionStartPartialSuccessIsAWarning(t *testing.T) {
	requireTmux(t)
	const profile = "_core_start_partial"
	inst := session.NewInstance("alpha", t.TempDir())
	seedStore(t, profile, nil, inst)
	stopLiveSessionsAtCleanup(t, profile)
	reconcileSucceeds(t)
	setStartRuntimeHook(t, func(inst *session.Instance, message string) (statedb.RuntimeState, error) {
		runtime, err := inst.StartRuntime()
		return publicationFailed(inst, runtime, err)
	})

	ctx, events := recordEvents(context.Background())
	out, res := Invoke[SessionStartOut](ctx, testRegistry(t, Deps{}), IDSessionStart, SessionStartIn{Profile: profile, Session: "alpha", NoWait: true})
	if res.Err != nil {
		t.Fatalf("a live pane with a failed publication must not fail the start: %v", res.Err)
	}
	if out.Status != StartStatusStarted || !strings.Contains(out.Warning, errInjectedPublication.Error()) {
		t.Fatalf("out = %+v, want started with the publication warning", out)
	}
	if !slices.Contains(res.Warnings, out.Warning) {
		t.Fatalf("envelope warnings = %q, want %q", res.Warnings, out.Warning)
	}
	requireRuntimeWarning(t, *events, inst.ID, out.Warning)
}

// F6: a start whose pane is live but whose initial message never reached it
// succeeds, and says the message is still pending instead of sent.
func TestSessionStartUndeliveredMessageIsPending(t *testing.T) {
	requireTmux(t)
	const profile = "_core_start_undelivered"
	inst := session.NewInstance("alpha", t.TempDir())
	seedStore(t, profile, nil, inst)
	stopLiveSessionsAtCleanup(t, profile)
	setStartRuntimeHook(t, func(inst *session.Instance, message string) (statedb.RuntimeState, error) {
		if message != "hello" {
			t.Errorf("start message = %q, want hello", message)
		}
		runtime, err := inst.StartRuntime()
		if err != nil {
			return runtime, err
		}
		return runtime, &session.RestartPartialSuccessError{
			InstanceID: inst.ID, Runtime: runtime, MessageUndelivered: true,
			Err: errors.New("timeout waiting for agent to be ready"),
		}
	})

	out, res := Invoke[SessionStartOut](context.Background(), testRegistry(t, Deps{}), IDSessionStart, SessionStartIn{Profile: profile, Session: "alpha", Message: "hello", NoWait: true})
	if res.Err != nil {
		t.Fatalf("an undelivered message must not fail the start: %v", res.Err)
	}
	if out.Message != "hello" || !out.MessagePending || !strings.Contains(out.Warning, "initial message was not delivered") {
		t.Fatalf("out = %+v, want the message pending with a delivery warning", out)
	}
}

// #2099 + F3: a spawn that dies before verification is durably errored; a
// snapshot save would drop the runtime-owned status. Whether the pane died
// before or after its generation was committed decides which tuple carries
// the verdict, so this real spawn checks only the verdict itself.
func TestSessionStartSpawnFailurePersistsErrorStatus(t *testing.T) {
	requireTmux(t)
	const profile = "_core_start_spawn_failure"
	inst := session.NewInstanceWithTool("dies", t.TempDir(), "customfail2099")
	inst.Command = "sh -c 'exit 3'"
	seedStore(t, profile, nil, inst)
	stopLiveSessionsAtCleanup(t, profile)

	res := testRegistry(t, Deps{}).Run(context.Background(), IDSessionStart, SessionStartIn{Profile: profile, Session: "dies", NoWait: true})
	wantCode(t, res.Err, CodeSpawnFailed, "")
	sf, ok := AsError(res.Err).Data.(*SpawnFailure)
	if !ok || sf.StatusErr != nil || sf.SaveErr != nil {
		t.Fatalf("spawn failure = %+v", AsError(res.Err).Data)
	}
	if got := durableRuntime(t, profile, inst.ID); got.Status != string(session.StatusError) {
		t.Fatalf("durable runtime = %+v, want status error", got)
	}
}

// A pane that died before its generation was committed leaves the instance
// holding that uncommitted candidate. The error verdict must land on the
// durable runtime the failed start began from, one revision later.
func TestSessionStartSpawnDiedBeforeCommitErrorsDurableRuntime(t *testing.T) {
	requireTmux(t)
	const profile = "_core_start_died_before_commit"
	inst := session.NewInstance("dies", t.TempDir())
	seedStore(t, profile, nil, inst)
	before := durableRuntime(t, profile, inst.ID)
	setStartRuntimeHook(t, func(inst *session.Instance, message string) (statedb.RuntimeState, error) {
		// What start returns when commitPhysicalRuntime finds no live pane:
		// the never-committed successor as a partial success.
		candidate := inst.RuntimeState()
		candidate.Generation++
		candidate.StatusRevision = 0
		candidate.Status = string(session.StatusStarting)
		candidate.LastStartedAt = time.Now().UTC()
		return candidate, &session.RestartPartialSuccessError{
			InstanceID: inst.ID, Runtime: candidate, NeedsReconciliation: true,
			Err: fmt.Errorf("physical runtime %s was not verified live", candidate.TmuxSession),
		}
	})

	res := testRegistry(t, Deps{}).Run(context.Background(), IDSessionStart, SessionStartIn{Profile: profile, Session: "dies", NoWait: true})
	wantCode(t, res.Err, CodeSpawnFailed, "")
	if sf, ok := AsError(res.Err).Data.(*SpawnFailure); !ok || sf.StatusErr != nil || sf.SaveErr != nil {
		t.Fatalf("spawn failure = %+v", AsError(res.Err).Data)
	}
	want := before
	want.Status = string(session.StatusError)
	want.StatusRevision++
	if got := durableRuntime(t, profile, inst.ID); got != want {
		t.Fatalf("durable runtime = %+v, want %+v", got, want)
	}
}

// A spawn failure whose error verdict loses its status CAS to a concurrent
// writer reaches every client: the legacy CLI shape prints
// SpawnFailure.StatusErr, and envelope and daemon clients read the warning.
func TestSessionStartLostErrorVerdictIsAWarning(t *testing.T) {
	requireTmux(t)
	const profile = "_core_start_lost_error_verdict"
	inst := session.NewInstance("dies", t.TempDir())
	seedStore(t, profile, nil, inst)
	var concurrent statedb.RuntimeState
	setStartRuntimeHook(t, func(inst *session.Instance, message string) (statedb.RuntimeState, error) {
		// No pane is spawned, so verification fails; meanwhile another
		// writer moves the status revision the verdict would CAS on.
		current := inst.RuntimeState()
		storage, err := session.NewStorageWithProfile(profile)
		if err != nil {
			return statedb.RuntimeState{}, err
		}
		defer storage.Close()
		applied, err := storage.GetDB().WriteStatusIfVersion(inst.ID, inst.PersistenceIncarnation(),
			current.Generation, current.StatusRevision, string(session.StatusStopped))
		if err != nil || !applied {
			return statedb.RuntimeState{}, fmt.Errorf("concurrent status write: applied=%v err=%v", applied, err)
		}
		concurrent = current
		concurrent.Status = string(session.StatusStopped)
		concurrent.StatusRevision++
		return current, nil
	})

	res := testRegistry(t, Deps{}).Run(context.Background(), IDSessionStart, SessionStartIn{Profile: profile, Session: "dies", NoWait: true})
	wantCode(t, res.Err, CodeSpawnFailed, "")
	sf, ok := AsError(res.Err).Data.(*SpawnFailure)
	if !ok || sf.StatusErr == nil || sf.SaveErr != nil {
		t.Fatalf("spawn failure = %+v, want a lost error verdict", AsError(res.Err).Data)
	}
	if want := []string{"failed to save session error status: " + sf.StatusErr.Error()}; !slices.Equal(res.Warnings, want) {
		t.Fatalf("envelope warnings = %q, want %q", res.Warnings, want)
	}
	if got := durableRuntime(t, profile, inst.ID); got != concurrent {
		t.Fatalf("durable runtime = %+v, want the concurrent verdict %+v kept", got, concurrent)
	}
}

// The session state saved after a spawn failure is the same kind of loss as
// the error verdict: envelope and daemon clients must learn it as a warning.
func TestSessionStartLostStateSaveIsAWarning(t *testing.T) {
	requireTmux(t)
	const profile = "_core_start_lost_state_save"
	inst := session.NewInstance("dies", t.TempDir())
	seedStore(t, profile, nil, inst)
	// Every snapshot save upserts the profile's groups; the status CAS
	// writes none, so only the state save fails.
	storage, err := session.NewStorageWithProfile(profile)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"INSERT", "UPDATE"} {
		if _, err := storage.GetDB().DB().Exec(`CREATE TRIGGER core_test_fail_group_` + strings.ToLower(op) +
			` BEFORE ` + op + ` ON groups BEGIN SELECT RAISE(ABORT, 'injected group write failure'); END`); err != nil {
			t.Fatal(err)
		}
	}
	storage.Close()
	setStartRuntimeHook(t, func(inst *session.Instance, message string) (statedb.RuntimeState, error) {
		// No pane is spawned, so verification fails.
		return inst.RuntimeState(), nil
	})

	res := testRegistry(t, Deps{}).Run(context.Background(), IDSessionStart, SessionStartIn{Profile: profile, Session: "dies", NoWait: true})
	wantCode(t, res.Err, CodeSpawnFailed, "")
	sf, ok := AsError(res.Err).Data.(*SpawnFailure)
	if !ok || sf.SaveErr == nil || sf.StatusErr != nil || !strings.Contains(sf.SaveErr.Error(), "injected group write failure") {
		t.Fatalf("spawn failure = %+v, want only a lost state save", AsError(res.Err).Data)
	}
	if want := []string{"failed to save session state: " + sf.SaveErr.Error()}; !slices.Equal(res.Warnings, want) {
		t.Fatalf("envelope warnings = %q, want %q", res.Warnings, want)
	}
	if got := durableRuntime(t, profile, inst.ID); got.Status != string(session.StatusError) {
		t.Fatalf("durable runtime = %+v, want the error verdict saved", got)
	}
}

func TestSessionRestartPartialSuccessIsAWarning(t *testing.T) {
	requireTmux(t)
	const profile = "_core_restart_partial"
	inst := session.NewInstance("alpha", t.TempDir())
	seedStore(t, profile, nil, inst)
	stopLiveSessionsAtCleanup(t, profile)
	r := testRegistry(t, Deps{})
	if _, res := Invoke[SessionStartOut](context.Background(), r, IDSessionStart, SessionStartIn{Profile: profile, Session: "alpha", NoWait: true}); res.Err != nil {
		t.Fatal(res.Err)
	}
	reconcileSucceeds(t)
	setRestartRuntimeHook(t, func(inst *session.Instance, env map[string]string) (statedb.RuntimeState, error) {
		runtime, err := inst.RestartWithEnvRuntime(env)
		return publicationFailed(inst, runtime, err)
	})

	ctx, events := recordEvents(context.Background())
	out, res := Invoke[SessionRestartOut](ctx, r, IDSessionRestart, SessionRestartIn{Profile: profile, Session: "alpha", Force: true})
	if res.Err != nil {
		t.Fatalf("a live pane with a failed publication must not fail the restart: %v", res.Err)
	}
	if out.Skipped || !strings.Contains(out.Warning, errInjectedPublication.Error()) {
		t.Fatalf("out = %+v, want a restart with the publication warning", out)
	}
	if !slices.Contains(res.Warnings, out.Warning) {
		t.Fatalf("envelope warnings = %q, want %q", res.Warnings, out.Warning)
	}
	var warned bool
	for _, ev := range *events {
		warned = warned || (ev.Kind == EventRestartWarning && ev.Message == out.Warning)
	}
	if !warned {
		t.Fatalf("no restart warning event in %+v", *events)
	}
}

func TestSessionRestartAllPartialSuccessCountsAsRestarted(t *testing.T) {
	requireTmux(t)
	const profile = "_core_restart_all_partial"
	inst := session.NewInstance("alpha", t.TempDir())
	seedStore(t, profile, nil, inst)
	stopLiveSessionsAtCleanup(t, profile)
	r := testRegistry(t, Deps{})
	if _, res := Invoke[SessionStartOut](context.Background(), r, IDSessionStart, SessionStartIn{Profile: profile, Session: "alpha", NoWait: true}); res.Err != nil {
		t.Fatal(res.Err)
	}
	reconcileSucceeds(t)
	setRestartRuntimeHook(t, func(inst *session.Instance, env map[string]string) (statedb.RuntimeState, error) {
		runtime, err := inst.RestartWithEnvRuntime(env)
		return publicationFailed(inst, runtime, err)
	})

	out, res := Invoke[SessionRestartOut](context.Background(), r, IDSessionRestart, SessionRestartIn{Profile: profile, All: true})
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	all := out.All
	if all == nil || all.Restarted != 1 || all.Failed != 0 || !all.OK() || len(all.Sessions) != 1 {
		t.Fatalf("sweep = %+v, want one restarted session and no failures", all)
	}
	row := all.Sessions[0]
	if row.Success == nil || !*row.Success || row.Error != "" || !strings.Contains(row.Warning, errInjectedPublication.Error()) {
		t.Fatalf("row = %+v, want success with the publication warning", row)
	}
	if !slices.Contains(res.Warnings, row.Warning) {
		t.Fatalf("envelope warnings = %q, want %q", res.Warnings, row.Warning)
	}
}

// A stop that drains the queue surfaces the drained start's durability
// warning exactly like a start does.
func TestSessionStopDrainWarningIsSurfaced(t *testing.T) {
	requireTmux(t)
	const profile = "_core_stop_drain_warning"
	dir := t.TempDir()
	busy := session.NewInstanceWithGroup("busy", dir, "serial")
	next := session.NewInstanceWithGroup("next", dir, "serial")
	next.Status = session.StatusQueued
	seedStore(t, profile, []*session.GroupData{{Name: "serial", Path: "serial", MaxConcurrent: 1}}, busy, next)
	stopLiveSessionsAtCleanup(t, profile)
	r := testRegistry(t, Deps{})
	if out, res := Invoke[SessionStartOut](context.Background(), r, IDSessionStart, SessionStartIn{Profile: profile, Session: "busy", NoWait: true}); res.Err != nil || out.Status != StartStatusStarted {
		t.Fatalf("start busy: %+v %v", out, res.Err)
	}
	reconcileSucceeds(t)
	setStartRuntimeHook(t, func(inst *session.Instance, message string) (statedb.RuntimeState, error) {
		runtime, err := inst.StartRuntime()
		return publicationFailed(inst, runtime, err)
	})

	ctx, events := recordEvents(context.Background())
	out, res := Invoke[SessionStopOut](ctx, r, IDSessionStop, SessionStopIn{Profile: profile, Session: "busy"})
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if out.Drained != next.ID || !strings.Contains(out.Warning, errInjectedPublication.Error()) {
		t.Fatalf("out = %+v, want %s drained with the publication warning", out, next.ID)
	}
	if !slices.Contains(res.Warnings, out.Warning) {
		t.Fatalf("envelope warnings = %q, want %q", res.Warnings, out.Warning)
	}
	requireRuntimeWarning(t, *events, next.ID, out.Warning)
	res.Finish()
}

// loadForDrain seeds one queued session in a serial group and returns the
// loaded store, the way sessionStop hands it to drainGroupQueue.
func loadForDrain(t *testing.T, profile string) (*session.Storage, []*session.Instance, []*session.GroupData, *session.Instance) {
	t.Helper()
	queued := session.NewInstanceWithGroup("q", t.TempDir(), "g")
	queued.Status = session.StatusQueued
	seedStore(t, profile, []*session.GroupData{{Name: "g", Path: "g", MaxConcurrent: 1}}, queued)
	storage, err := session.NewStorageWithProfile(profile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	instances, groups, err := storage.LoadWithGroups()
	if err != nil {
		t.Fatal(err)
	}
	loaded := findByTitle(instances, "q")
	if loaded == nil || loaded.Status != session.StatusQueued {
		t.Fatalf("seeded queued session = %+v", loaded)
	}
	return storage, instances, groups, loaded
}

// F5: a queued session whose start fails is durably errored, so the next stop
// does not retry it ahead of the rest of the queue.
func TestDrainGroupQueueFailurePersistsErrorStatus(t *testing.T) {
	const profile = "_core_drain_failure"
	storage, instances, groups, queued := loadForDrain(t, profile)
	before := queued.RuntimeState()
	startErr := errors.New("injected start failure")
	setStartRuntimeHook(t, func(*session.Instance, string) (statedb.RuntimeState, error) {
		return statedb.RuntimeState{}, startErr
	})

	res := &runState{}
	ctx, events := recordEvents(withRunState(context.Background(), res))
	drained, warning := drainGroupQueue(ctx, storage, "g", instances, groups)
	if drained != nil || warning != "" {
		t.Fatalf("drained %v (%q) after a failed start", drained, warning)
	}
	if len(*events) != 1 || (*events)[0].Kind != EventQueueDrainFailed || (*events)[0].ID != queued.ID ||
		!errors.Is((*events)[0].Err, startErr) || (*events)[0].Message != "" || len(res.warnings) != 0 {
		t.Fatalf("events = %+v, warnings = %q", *events, res.warnings)
	}
	want := before
	want.Status = string(session.StatusError)
	want.StatusRevision++
	if got := durableRuntime(t, profile, queued.ID); got != want {
		t.Fatalf("durable runtime = %+v, want %+v", got, want)
	}
	if got := session.FindNextQueued(loadStore(t, profile), "g"); got != nil {
		t.Fatalf("failed session %s is still queued", got.Title)
	}
}

// The drain's error verdict is a CAS on the generation and revision it read:
// a concurrent status writer wins, and the lost verdict is a warning.
func TestDrainGroupQueueFailureKeepsConcurrentStatus(t *testing.T) {
	const profile = "_core_drain_failure_race"
	storage, instances, groups, queued := loadForDrain(t, profile)
	before := queued.RuntimeState()
	applied, err := storage.GetDB().WriteStatusIfVersion(queued.ID, queued.PersistenceIncarnation(), before.Generation, before.StatusRevision, string(session.StatusStopped))
	if err != nil || !applied {
		t.Fatalf("concurrent status write: applied=%v err=%v", applied, err)
	}
	setStartRuntimeHook(t, func(*session.Instance, string) (statedb.RuntimeState, error) {
		return statedb.RuntimeState{}, errors.New("injected start failure")
	})

	res := &runState{}
	ctx, events := recordEvents(withRunState(context.Background(), res))
	if drained, _ := drainGroupQueue(ctx, storage, "g", instances, groups); drained != nil {
		t.Fatalf("drained %s after a failed start", drained.Title)
	}
	if len(*events) != 1 || !strings.Contains((*events)[0].Message, "failed to save session error status") ||
		!slices.Equal(res.warnings, []string{(*events)[0].Message}) {
		t.Fatalf("events = %+v, warnings = %q", *events, res.warnings)
	}
	if got := durableRuntime(t, profile, queued.ID); got.Status != string(session.StatusStopped) || got.StatusRevision != before.StatusRevision+1 {
		t.Fatalf("durable runtime = %+v, want the concurrent stopped verdict kept", got)
	}
}
