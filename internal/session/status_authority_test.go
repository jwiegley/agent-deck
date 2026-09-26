package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

func setStatusProbeOverride(t *testing.T, fn func(context.Context, *Instance, statedb.RuntimeState) (Status, error)) {
	t.Helper()
	old := statusProbeCandidateOverride
	statusProbeCandidateOverride = fn
	t.Cleanup(func() { statusProbeCandidateOverride = old })
}

func TestRuntimeLifecycle_StatusAuthority_DelayedGenerationCannotPublish(t *testing.T) {
	started := time.Unix(7000, 0).UTC()
	db, inst := runtimeLifecycleReviverFixture(t, StatusError, started)
	observed := inst.runtimeStateSnapshot()
	incarnation := inst.PersistenceIncarnation()
	entered, release := make(chan struct{}), make(chan struct{})
	setStatusProbeOverride(t, func(context.Context, *Instance, statedb.RuntimeState) (Status, error) {
		close(entered)
		<-release
		return StatusWaiting, nil
	})

	type result struct {
		state statedb.RuntimeState
		err   error
	}
	done := make(chan result, 1)
	go func() {
		state, err := inst.UpdateStatusObserved(context.Background(), observed, incarnation)
		done <- result{state: state, err: err}
	}()
	<-entered
	next := observed
	next.Generation++
	next.StatusRevision = 0
	next.TmuxSession = "replacement"
	next.TmuxSocketName = "replacement-socket"
	next.Status = string(StatusRunning)
	next.LastStartedAt = started.Add(time.Second)
	if err := db.CommitRuntimeTransition(observed.Generation, incarnation, next); err != nil {
		t.Fatal(err)
	}
	inst.adoptRuntimeState(next)
	close(release)
	got := <-done
	if !errors.Is(got.err, statedb.ErrRuntimeGenerationConflict) {
		t.Fatalf("error = %v, want generation conflict", got.err)
	}
	if !sameStatusRuntime(got.state, next) || !sameStatusRuntime(inst.runtimeStateSnapshot(), next) {
		t.Fatalf("late generation published: returned=%+v memory=%+v want=%+v", got.state, inst.runtimeStateSnapshot(), next)
	}
	durable, _, _ := db.ReadRuntimeState(inst.ID)
	if !sameStatusRuntime(durable, next) {
		t.Fatalf("durable runtime changed: got %+v want %+v", durable, next)
	}
}

func TestRuntimeLifecycle_StatusAuthority_UnchangedCandidateDoesNotBumpRevision(t *testing.T) {
	started := time.Unix(7100, 0).UTC()
	db, inst := runtimeLifecycleReviverFixture(t, StatusWaiting, started)
	observed := inst.runtimeStateSnapshot()
	incarnation := inst.PersistenceIncarnation()
	setStatusProbeOverride(t, func(context.Context, *Instance, statedb.RuntimeState) (Status, error) {
		return StatusWaiting, nil
	})
	got, err := inst.UpdateStatusObserved(context.Background(), observed, incarnation)
	if err != nil {
		t.Fatal(err)
	}
	durable, _, _ := db.ReadRuntimeState(inst.ID)
	if got.StatusRevision != observed.StatusRevision || durable.StatusRevision != observed.StatusRevision || inst.StatusRevision != observed.StatusRevision {
		t.Fatalf("unchanged observation bumped revision: returned=%d durable=%d memory=%d", got.StatusRevision, durable.StatusRevision, inst.StatusRevision)
	}
}

// A probe that forms no verdict leaves the authority a no-op even for a caller
// that brings no pass evidence of its own (the notify daemon): an unchanged
// verdict marks the status sampled live and releases a healthy session's auth
// hold, but a pass that observed nothing does neither.
func TestRuntimeLifecycle_StatusAuthority_NoVerdictFinalizesNothing(t *testing.T) {
	for _, c := range []struct {
		name      string
		noVerdict bool
	}{
		{"unchanged verdict", false},
		{"no verdict", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			db, inst := runtimeLifecycleReviverFixture(t, StatusRunning, time.Unix(7120, 0).UTC())
			t.Cleanup(func() { clearAuthHoldRecord(inst.ID) })
			if err := writeAuthHoldRecord(AuthHoldRecord{InstanceID: inst.ID, Reason: AuthHoldReasonDeath}); err != nil {
				t.Fatal(err)
			}
			observed := inst.runtimeStateSnapshot()
			setStatusProbeOverride(t, func(ctx context.Context, _ *Instance, observed statedb.RuntimeState) (Status, error) {
				if c.noVerdict {
					recordStatusNoVerdict(ctx)
				}
				return Status(observed.Status), nil
			})

			got, err := inst.UpdateStatusObserved(context.Background(), observed, inst.PersistenceIncarnation())
			if err != nil || got != observed {
				t.Fatalf("UpdateStatusObserved = %+v, %v; want the observed tuple %+v", got, err, observed)
			}
			durable, found, err := db.ReadRuntimeState(inst.ID)
			if err != nil || !found || durable != observed {
				t.Fatalf("durable runtime = %+v found=%v err=%v, want %+v", durable, found, err, observed)
			}
			inst.mu.RLock()
			sampledLive := inst.statusSampledLive
			inst.mu.RUnlock()
			held := inst.AuthHold() != nil
			if sampledLive != !c.noVerdict || held != c.noVerdict {
				t.Fatalf("sampled live = %v, auth hold kept = %v; want %v and %v", sampledLive, held, !c.noVerdict, c.noVerdict)
			}
		})
	}
}

// A probe exit that keeps the last-known status without looking forms no
// verdict: inside the tmux grace window a running, idle or queued status is
// kept while the spawn has no session yet, and with no tmux session at all a
// stopped or queued session keeps its operator intent. Finalizing such a keep
// marked the status sampled live and, for a healthy status, released the
// cross-process auth hold on no evidence. The exits that rewrite the status,
// starting inside the window and a death's classification, remain verdicts.
func TestRuntimeLifecycle_StatusProbe_UnobservedKeepsFormNoVerdict(t *testing.T) {
	for _, c := range []struct {
		name      string
		status    Status
		inGrace   bool
		want      Status
		noVerdict bool
	}{
		{"grace window keeps running", StatusRunning, true, StatusRunning, true},
		{"grace window keeps queued", StatusQueued, true, StatusQueued, true},
		{"no tmux session keeps stopped", StatusStopped, false, StatusStopped, true},
		{"no tmux session keeps queued", StatusQueued, false, StatusQueued, true},
		{"grace window reads starting (verdict)", StatusError, true, StatusStarting, false},
		{"no tmux session classifies the death (verdict)", StatusRunning, false, StatusError, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			db, inst := runtimeLifecycleReviverFixture(t, c.status, time.Unix(7300, 0).UTC())
			t.Cleanup(func() { clearAuthHoldRecord(inst.ID) })
			if err := writeAuthHoldRecord(AuthHoldRecord{InstanceID: inst.ID, Reason: AuthHoldReasonDeath}); err != nil {
				t.Fatal(err)
			}
			if c.inGrace {
				inst.mu.Lock()
				inst.CreatedAt = time.Now()
				inst.mu.Unlock()
			}
			before := inst.RuntimeState()

			evidence, err := inst.updateStatusWithEvidence(nil, true)
			if err != nil {
				t.Fatalf("status pass: %v", err)
			}
			if got := inst.GetStatusThreadSafe(); got != c.want {
				t.Fatalf("status = %q, want %q", got, c.want)
			}
			durable, found, err := db.ReadRuntimeState(inst.ID)
			if err != nil || !found || durable != inst.RuntimeState() {
				t.Fatalf("durable runtime = %+v found=%v err=%v, want memory's %+v", durable, found, err, inst.RuntimeState())
			}
			inst.mu.RLock()
			sampledLive := inst.statusSampledLive
			inst.mu.RUnlock()
			if sampledLive == c.noVerdict {
				t.Fatalf("sampled live = %v, want %v", sampledLive, !c.noVerdict)
			}
			if c.noVerdict {
				if got := inst.RuntimeState(); got != before {
					t.Fatalf("runtime = %+v, want the kept tuple %+v", got, before)
				}
				if inst.AuthHold() == nil {
					t.Fatal("a pass without a verdict released the auth-hold sidecar")
				}
			}
			if got := evidence.noVerdict.Load(); got != c.noVerdict {
				t.Fatalf("no verdict = %v, want %v", got, c.noVerdict)
			}
		})
	}
}

func TestRuntimeLifecycle_StatusAuthority_RejectsByteIdenticalIncarnationABA(t *testing.T) {
	started := time.Unix(7150, 123).UTC()
	db, stale := runtimeLifecycleReviverFixture(t, StatusWaiting, started)
	observed := stale.runtimeStateSnapshot()
	incarnationA := stale.PersistenceIncarnation()
	if err := db.DeleteInstance(stale.ID); err != nil {
		t.Fatal(err)
	}
	const incarnationB = "reviver-runtime-incarnation-b"
	winner := &statedb.InstanceRow{
		ID: stale.ID, Incarnation: incarnationB, Title: stale.Title,
		ProjectPath: stale.ProjectPath, Tool: stale.Tool, Status: string(StatusWaiting),
		CreatedAt: started.Add(-time.Minute), LastStartedAt: started,
	}
	if err := db.SaveInstance(winner); err != nil {
		t.Fatal(err)
	}
	if incarnationA == winner.Incarnation {
		t.Fatalf("byte-identical B reused A incarnation %q", incarnationA)
	}
	durable, found, err := db.ReadRuntimeState(stale.ID)
	if err != nil || !found || durable != observed {
		t.Fatalf("byte-identical B runtime = %#v found=%v err=%v; want %#v", durable, found, err, observed)
	}

	probes := 0
	setStatusProbeOverride(t, func(context.Context, *Instance, statedb.RuntimeState) (Status, error) {
		probes++
		return StatusRunning, nil
	})
	got, err := stale.UpdateStatusObserved(context.Background(), observed, incarnationA)
	if !errors.Is(err, statedb.ErrInstanceParentConflict) {
		t.Fatalf("stale A status error = %v, want parent conflict", err)
	}
	if probes != 0 || got != observed {
		t.Fatalf("stale A result = %#v probes=%d, want original observation without probe", got, probes)
	}
	durable, found, err = db.ReadRuntimeState(stale.ID)
	if err != nil || !found || durable != observed {
		t.Fatalf("stale A changed B runtime = %#v found=%v err=%v", durable, found, err)
	}
}

func TestRuntimeLifecycle_StatusAuthority_ExactTupleMismatchRejectsCandidate(t *testing.T) {
	started := time.Unix(7200, 0).UTC()
	_, inst := runtimeLifecycleReviverFixture(t, StatusError, started)
	observed := inst.runtimeStateSnapshot()
	incarnation := inst.PersistenceIncarnation()
	entered, release := make(chan struct{}), make(chan struct{})
	setStatusProbeOverride(t, func(context.Context, *Instance, statedb.RuntimeState) (Status, error) {
		close(entered)
		<-release
		return StatusWaiting, nil
	})
	done := make(chan error, 1)
	go func() {
		_, err := inst.UpdateStatusObserved(context.Background(), observed, incarnation)
		done <- err
	}()
	<-entered
	inst.mu.Lock()
	inst.LastStartedAt = started.Add(time.Second)
	inst.mu.Unlock()
	close(release)
	if err := <-done; !errors.Is(err, statedb.ErrRuntimeGenerationConflict) {
		t.Fatalf("error = %v, want exact-tuple generation conflict", err)
	}
	if inst.Status == StatusWaiting {
		t.Fatal("candidate published despite LastStartedAt mismatch")
	}
}

func TestRuntimeLifecycle_StatusAuthority_TimedOutLateProbeCannotPublish(t *testing.T) {
	started := time.Unix(7300, 0).UTC()
	db, inst := runtimeLifecycleReviverFixture(t, StatusError, started)
	observed := inst.runtimeStateSnapshot()
	incarnation := inst.PersistenceIncarnation()
	inst.hookSessionID = "late-tool-id"
	entered, release := make(chan struct{}), make(chan struct{})
	setStatusProbeOverride(t, func(context.Context, *Instance, statedb.RuntimeState) (Status, error) {
		close(entered)
		<-release
		return StatusWaiting, nil
	})
	timeout := make(chan time.Time)
	oldTimeout := statusProbeTimeoutFn
	statusProbeTimeoutFn = func(time.Duration) <-chan time.Time { return timeout }
	t.Cleanup(func() { statusProbeTimeoutFn = oldTimeout })

	d := NewTransitionDaemon()
	type boundedResult struct {
		state    statedb.RuntimeState
		timedOut bool
	}
	done := make(chan boundedResult, 1)
	go func() {
		state, timedOut := d.refreshInstanceStatusBounded("test", inst, observed, incarnation)
		done <- boundedResult{state: state, timedOut: timedOut}
	}()
	<-entered
	close(timeout)
	bounded := <-done
	if !bounded.timedOut || !sameStatusRuntime(bounded.state, observed) {
		t.Fatalf("timeout result = %+v", bounded)
	}

	next := observed
	next.Generation++
	next.StatusRevision = 0
	next.Status = string(StatusRunning)
	next.TmuxSession = "after-timeout"
	next.LastStartedAt = started.Add(time.Second)
	if err := db.CommitRuntimeTransition(observed.Generation, incarnation, next); err != nil {
		t.Fatal(err)
	}
	inst.adoptRuntimeState(next)
	close(release)

	// Acquiring the same gate is a deterministic completion barrier for the
	// detached probe: it cannot succeed until the canceled authority returns.
	barrierCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	releaseBarrier, err := inst.acquireStatusProbe(barrierCtx)
	if err != nil {
		t.Fatal(err)
	}
	releaseBarrier()
	if !sameStatusRuntime(inst.runtimeStateSnapshot(), next) {
		t.Fatalf("late timeout probe changed memory: got %+v want %+v", inst.runtimeStateSnapshot(), next)
	}
	if inst.ClaudeSessionID != "" {
		t.Fatalf("late timeout probe published tool id %q", inst.ClaudeSessionID)
	}
	durable, _, _ := db.ReadRuntimeState(inst.ID)
	if !sameStatusRuntime(durable, next) {
		t.Fatalf("late timeout probe changed DB: got %+v want %+v", durable, next)
	}
}
