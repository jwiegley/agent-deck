package session

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

type statusCommitFenceKey struct{}

type statusCommitFence struct {
	mu         sync.Mutex
	canceled   bool
	committing bool
}

func (f *statusCommitFence) begin() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.canceled {
		return false
	}
	f.committing = true
	return true
}

// cancel returns true when no commit has started and publication is fenced.
// A false result means the caller must wait for the in-flight commit before it
// reports a timeout, so publication can never occur after that report.
func (f *statusCommitFence) cancel() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.committing {
		return false
	}
	f.canceled = true
	return true
}

func beginStatusCommit(ctx context.Context) bool {
	if err := ctx.Err(); err != nil {
		return false
	}
	if fence, ok := ctx.Value(statusCommitFenceKey{}).(*statusCommitFence); ok {
		return fence.begin()
	}
	return true
}

func statusCommitContextError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return context.Canceled
}

type statusProbeEvidenceKey struct{}

// statusProbeEvidence reports how one probe settled its candidate. paneSampled
// is set only when the candidate came from a live tmux pane sample, not from a
// fast path, a skip, a debounce hold, or an absent session; only such a sample
// may refresh tool identity and session metadata after the commit.
//
// noVerdict is set when the probe took no observation at all: a session this
// process cannot see from inside another tmux server, a skip that trusts the
// verdict an earlier sample settled, or an exit that keeps the last-known
// status without looking. The tmux grace window looks at nothing while the
// session does not exist yet, so every status it leaves unchanged is such a
// keep: running, idle and queued, and starting, which the window would only
// rewrite to itself. A stopped or queued session with no tmux session keeps
// its operator intent. Such a pass is a no-op, as upstream's early return is:
// nothing is committed or finalized, and no metadata refresh follows. The
// grace window forms a verdict only when it changes the status to starting.
// The other exits settle the status from what they found and are verdicts:
// idle for a session this process added but never started, which it knows
// has no tmux session, and a death's classification.
type statusProbeEvidence struct {
	paneSampled atomic.Bool
	noVerdict   atomic.Bool
}

func withStatusProbeEvidence(ctx context.Context) (context.Context, *statusProbeEvidence) {
	evidence := &statusProbeEvidence{}
	return context.WithValue(ctx, statusProbeEvidenceKey{}, evidence), evidence
}

// statusProbeEvidenceFor returns the pass evidence ctx carries, installing one
// for callers that bring none (the notify daemon), so every probe can report
// that it formed no verdict.
func statusProbeEvidenceFor(ctx context.Context) (context.Context, *statusProbeEvidence) {
	if evidence, ok := ctx.Value(statusProbeEvidenceKey{}).(*statusProbeEvidence); ok {
		return ctx, evidence
	}
	return withStatusProbeEvidence(ctx)
}

func recordStatusPaneSample(ctx context.Context) {
	if evidence, ok := ctx.Value(statusProbeEvidenceKey{}).(*statusProbeEvidence); ok {
		evidence.paneSampled.Store(true)
	}
}

func recordStatusNoVerdict(ctx context.Context) {
	if evidence, ok := ctx.Value(statusProbeEvidenceKey{}).(*statusProbeEvidence); ok {
		evidence.noVerdict.Store(true)
	}
}

// Test seams surround candidate calculation and the DB-before-memory boundary.
// Production leaves the candidate override nil.
var (
	statusProbeCandidateOverride func(context.Context, *Instance, statedb.RuntimeState) (Status, error)
	statusBeforeMemoryPublishFn  = func(*Instance, statedb.RuntimeState) {}
)

func sameStatusRuntime(left, right statedb.RuntimeState) bool {
	return left.InstanceID == right.InstanceID &&
		left.Generation == right.Generation &&
		left.StatusRevision == right.StatusRevision &&
		left.TmuxSession == right.TmuxSession &&
		left.TmuxSocketName == right.TmuxSocketName &&
		left.Status == right.Status &&
		left.LastStartedAt.Equal(right.LastStartedAt)
}

func statusRuntimeConflict(observed, current statedb.RuntimeState) error {
	if observed.InstanceID != current.InstanceID ||
		observed.Generation != current.Generation ||
		observed.TmuxSession != current.TmuxSession ||
		observed.TmuxSocketName != current.TmuxSocketName ||
		!observed.LastStartedAt.Equal(current.LastStartedAt) {
		return statedb.ErrRuntimeGenerationConflict
	}
	return statedb.ErrStatusRevisionConflict
}

func (i *Instance) runtimeStateLocked() statedb.RuntimeState {
	name := ""
	if i.tmuxSession != nil {
		name = i.tmuxSession.Name
	}
	return statedb.RuntimeState{
		InstanceID:     i.ID,
		Generation:     i.RuntimeGeneration,
		StatusRevision: i.StatusRevision,
		TmuxSession:    name,
		TmuxSocketName: i.TmuxSocketName,
		Status:         string(i.Status),
		LastStartedAt:  i.LastStartedAt,
	}
}

func (i *Instance) statusProbeCurrentLocked(ctx context.Context, observed statedb.RuntimeState) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	current := i.runtimeStateLocked()
	if !sameStatusRuntime(observed, current) {
		return statusRuntimeConflict(observed, current)
	}
	return nil
}

// statusAbsenceIsForeignServerFn is the foreign-server guard the status probe
// consults; tests replace it to stage a nested process without a second server.
var statusAbsenceIsForeignServerFn = (*tmux.Session).AbsenceIsForeignServer

// probeAbsenceIsForeignServer is called with i.mu held and returns with it
// held. From inside another tmux server the guard lists the default server,
// so, as in probeTmuxExists, status readers must not wait behind it, and a
// runtime replacement or cancellation meanwhile discards the answer.
func (i *Instance) probeAbsenceIsForeignServer(ctx context.Context, observed statedb.RuntimeState) (bool, error) {
	s := i.tmuxSession
	i.mu.Unlock()
	foreign := statusAbsenceIsForeignServerFn(s)
	i.mu.Lock()
	if err := i.statusProbeCurrentLocked(ctx, observed); err != nil {
		return false, err
	}
	return foreign, nil
}

func (i *Instance) reloadStatusWinner(db *statedb.StateDB, incarnation string) (statedb.RuntimeState, error) {
	if db == nil {
		return i.runtimeStateSnapshot(), nil
	}
	// A physical replacement can be live before its generation is committed.
	// Defer reconciliation while a transition owns the runtime: waiting here
	// would also exceed the status probe's deadline. A later poll can reread the
	// durable winner once the transition has released authority.
	release, acquired, err := tryAcquireInstanceSpawnLock(i.ID)
	if err != nil {
		return i.runtimeStateSnapshot(), err
	}
	if !acquired {
		return i.runtimeStateSnapshot(), statedb.ErrRuntimeGenerationConflict
	}
	defer release()
	if err := db.ValidateInstanceIncarnation(i.ID, incarnation); err != nil {
		return i.runtimeStateSnapshot(), err
	}
	current, found, err := db.ReadRuntimeState(i.ID)
	if err != nil {
		return i.runtimeStateSnapshot(), err
	}
	if !found {
		return i.runtimeStateSnapshot(), statedb.ErrRuntimeGenerationConflict
	}
	i.adoptRuntimeState(current)
	return current, nil
}

func (i *Instance) publishStatusIfCurrent(observed, next statedb.RuntimeState) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if !sameStatusRuntime(observed, i.runtimeStateLocked()) {
		return false
	}
	i.Status = Status(next.Status)
	i.StatusRevision = next.StatusRevision
	return true
}

func (i *Instance) finalizeCommittedStatus(state statedb.RuntimeState) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if sameStatusRuntime(state, i.runtimeStateLocked()) {
		i.statusSampledLive = true
		i.releaseAuthHoldIfHealthyLocked()
	}
}

func (i *Instance) acquireStatusProbe(ctx context.Context) (func(), error) {
	i.mu.Lock()
	if i.statusProbeGate == nil {
		i.statusProbeGate = make(chan struct{}, 1)
	}
	gate := i.statusProbeGate
	i.mu.Unlock()
	select {
	case gate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-gate
			return nil, err
		}
		return func() { <-gate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// PersistSelectedStatus durably marks exactly the runtime generation and status
// revision inst holds now with an operator-decided status (queued, or error
// after a failed spawn or queue drain). Status is runtime-owned, so snapshot
// saves drop it; this CAS is the only way such a verdict reaches the store. A
// concurrent replacement or status observation wins: the write is refused with
// statedb.ErrStatusRevisionConflict and inst is left untouched.
func PersistSelectedStatus(storage *Storage, inst *Instance, status Status) error {
	if storage == nil || storage.GetDB() == nil {
		return errors.New("session: persist status: storage unavailable")
	}
	selection := inst.CaptureRuntimeSelection()
	applied, err := storage.GetDB().WriteStatusIfVersion(
		inst.ID, selection.Incarnation, selection.State.Generation,
		selection.State.StatusRevision, string(status),
	)
	if err != nil {
		return err
	}
	if !applied {
		return statedb.ErrStatusRevisionConflict
	}
	next := selection.State
	next.Status = string(status)
	next.StatusRevision++
	inst.ApplyRuntimeState(next)
	return nil
}

// PersistSpawnFailureStatus durably marks a start or restart whose spawn
// verification failed as errored. A partial success whose generation was
// never committed leaves inst holding that uncommitted successor of the
// durable generation. The verdict then belongs to the canonical runtime, so
// inst re-derives it first. Reconciliation may prove a runtime live there: a
// replacement, or the very spawn verification gave up on (a probe that timed
// out), which reconciliation has just adopted and committed. Neither inherits
// the error; the refusal says a live runtime was kept. A committed generation
// goes straight to PersistSelectedStatus.
func PersistSpawnFailureStatus(storage *Storage, inst *Instance) error {
	if storage == nil || storage.GetDB() == nil {
		return errors.New("session: persist status: storage unavailable")
	}
	durable, found, err := storage.GetDB().ReadRuntimeState(inst.ID)
	if err != nil {
		return err
	}
	if found && durable.Generation+1 == inst.CaptureRuntimeSelection().State.Generation {
		reconciled, err := inst.ReconcileRuntime()
		if err != nil {
			return err
		}
		if reconciled.Live {
			return fmt.Errorf("a live runtime (generation %d, tmux session %q) was found; not marking it errored: %w",
				reconciled.State.Generation, reconciled.State.TmuxSession, statedb.ErrRuntimeGenerationConflict)
		}
	}
	return PersistSelectedStatus(storage, inst, StatusError)
}

// UpdateStatusObserved is the single status authority. The caller captures the
// exact runtime tuple before probing; the candidate remains private until its
// durable CAS succeeds. Losers adopt the durable winner unless a physical
// transition is still in flight, in which case reconciliation waits for a later poll.
// A probe that forms no verdict (statusProbeEvidence.noVerdict) returns the
// observed tuple untouched.
func (i *Instance) UpdateStatusObserved(ctx context.Context, observed statedb.RuntimeState, incarnation string) (statedb.RuntimeState, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, evidence := statusProbeEvidenceFor(ctx)
	release, err := i.acquireStatusProbe(ctx)
	if err != nil {
		return i.runtimeStateSnapshot(), err
	}
	defer release()

	current := i.runtimeStateSnapshot()
	if !sameStatusRuntime(observed, current) {
		conflictErr := statusRuntimeConflict(observed, current)
		winner, reloadErr := i.reloadStatusWinner(i.restartPersistenceDB(), incarnation)
		return winner, errors.Join(conflictErr, reloadErr)
	}

	db := i.restartPersistenceDB()
	if db != nil {
		if err := db.ValidateInstanceIncarnation(i.ID, incarnation); err != nil {
			return current, err
		}
		durable, found, err := db.ReadRuntimeState(i.ID)
		if err != nil {
			return current, err
		}
		if !found || !sameStatusRuntime(observed, durable) {
			var conflictErr error = statedb.ErrRuntimeGenerationConflict
			if found {
				conflictErr = statusRuntimeConflict(observed, durable)
			}
			winner, reloadErr := i.reloadStatusWinner(db, incarnation)
			return winner, errors.Join(conflictErr, reloadErr)
		}
	}

	candidate, probeErr := i.probeStatusCandidate(ctx, observed)
	if err := ctx.Err(); err != nil {
		return i.runtimeStateSnapshot(), err
	}
	current = i.runtimeStateSnapshot()
	if !sameStatusRuntime(observed, current) {
		conflictErr := statusRuntimeConflict(observed, current)
		winner, reloadErr := i.reloadStatusWinner(db, incarnation)
		return winner, errors.Join(probeErr, conflictErr, reloadErr)
	}
	if evidence.noVerdict.Load() {
		// Nothing was observed, so nothing is confirmed: finalizing would mark
		// the status sampled live and release an auth hold on no evidence.
		return observed, probeErr
	}

	next := observed
	next.Status = string(candidate)
	if candidate == Status(observed.Status) {
		if db != nil {
			if err := db.ValidateInstanceIncarnation(i.ID, incarnation); err != nil {
				return current, errors.Join(probeErr, err)
			}
			durable, found, err := db.ReadRuntimeState(i.ID)
			if err != nil {
				return current, errors.Join(probeErr, err)
			}
			if !found || !sameStatusRuntime(observed, durable) {
				var conflictErr error = statedb.ErrRuntimeGenerationConflict
				if found {
					conflictErr = statusRuntimeConflict(observed, durable)
				}
				winner, reloadErr := i.reloadStatusWinner(db, incarnation)
				return winner, errors.Join(probeErr, conflictErr, reloadErr)
			}
		}
		i.finalizeCommittedStatus(observed)
		return observed, probeErr
	}

	if db == nil {
		if !beginStatusCommit(ctx) {
			return i.runtimeStateSnapshot(), statusCommitContextError(ctx)
		}
		if !i.publishStatusIfCurrent(observed, next) {
			current := i.runtimeStateSnapshot()
			return current, errors.Join(probeErr, statusRuntimeConflict(observed, current))
		}
		i.finalizeCommittedStatus(next)
		return next, probeErr
	}

	// Re-read the indivisible tuple immediately before the revision CAS. The CAS
	// closes the remaining cross-process window by matching generation+revision.
	durable, found, err := db.ReadRuntimeState(i.ID)
	if err != nil {
		return current, errors.Join(probeErr, err)
	}
	if !found || !sameStatusRuntime(observed, durable) {
		var conflictErr error = statedb.ErrRuntimeGenerationConflict
		if found {
			conflictErr = statusRuntimeConflict(observed, durable)
		}
		winner, reloadErr := i.reloadStatusWinner(db, incarnation)
		return winner, errors.Join(probeErr, conflictErr, reloadErr)
	}
	if !beginStatusCommit(ctx) {
		return i.runtimeStateSnapshot(), statusCommitContextError(ctx)
	}
	applied, err := db.WriteStatusIfVersion(i.ID, incarnation, observed.Generation, observed.StatusRevision, next.Status)
	if err != nil {
		return current, errors.Join(probeErr, err)
	}
	if !applied {
		winner, reloadErr := i.reloadStatusWinner(db, incarnation)
		return winner, errors.Join(probeErr, statusRuntimeConflict(observed, winner), reloadErr)
	}
	next.StatusRevision++
	statusBeforeMemoryPublishFn(i, next)
	if !i.publishStatusIfCurrent(observed, next) {
		winner, reloadErr := i.reloadStatusWinner(db, incarnation)
		return winner, errors.Join(probeErr, statusRuntimeConflict(observed, winner), reloadErr)
	}
	i.finalizeCommittedStatus(next)
	return next, probeErr
}
