package session

import (
	"context"
	"errors"
	"sync"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
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

func (i *Instance) reloadStatusWinner(db *statedb.StateDB, incarnation string) (statedb.RuntimeState, error) {
	if db == nil {
		return i.runtimeStateSnapshot(), nil
	}
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

// UpdateStatusObserved is the single status authority. The caller captures the
// exact runtime tuple before probing; the candidate remains private until its
// durable CAS succeeds, and every loser adopts the durable winner.
func (i *Instance) UpdateStatusObserved(ctx context.Context, observed statedb.RuntimeState, incarnation string) (statedb.RuntimeState, error) {
	if ctx == nil {
		ctx = context.Background()
	}
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
