package session

import (
	"fmt"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// RuntimeSelection is the physical-runtime identity selected by a destructive
// command before that command is queued or dispatched.
type RuntimeSelection struct {
	State       statedb.RuntimeState
	Incarnation string
}

// CaptureRuntimeSelection freezes the identity a destructive command is
// allowed to affect. Execution revalidates this tuple under the cross-process
// instance lock; it never substitutes a newer runtime.
func (i *Instance) CaptureRuntimeSelection() RuntimeSelection {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return RuntimeSelection{
		State:       i.runtimeStateSnapshotLocked(),
		Incarnation: i.persistenceIncarnation,
	}
}

func sameDestructiveRuntime(left, right statedb.RuntimeState) bool {
	return left.InstanceID == right.InstanceID &&
		left.Generation == right.Generation &&
		left.StatusRevision == right.StatusRevision &&
		left.TmuxSession == right.TmuxSession &&
		left.TmuxSocketName == right.TmuxSocketName &&
		left.Status == right.Status
}

func destructiveRuntimeConflict(current, expected statedb.RuntimeState) error {
	if current.InstanceID != expected.InstanceID ||
		current.Generation != expected.Generation ||
		current.TmuxSession != expected.TmuxSession ||
		current.TmuxSocketName != expected.TmuxSocketName {
		return statedb.ErrRuntimeGenerationConflict
	}
	return statedb.ErrStatusRevisionConflict
}

func (i *Instance) validateRuntimeSelection(db *statedb.StateDB, selection RuntimeSelection, durable bool) error {
	expected := selection.State
	if i.PersistenceIncarnation() != selection.Incarnation {
		return statedb.ErrInstanceParentConflict
	}
	if db != nil {
		current, found, err := db.ReadRuntimeState(expected.InstanceID)
		if err != nil {
			return err
		}
		if durable {
			if !found {
				return statedb.ErrRuntimeGenerationConflict
			}
			if err := db.ValidateInstanceIncarnation(expected.InstanceID, selection.Incarnation); err != nil {
				return err
			}
			if !sameDestructiveRuntime(current, expected) {
				return destructiveRuntimeConflict(current, expected)
			}
			return nil
		}
		if found {
			return statedb.ErrRuntimeGenerationConflict
		}
	}
	if current := i.runtimeStateSnapshot(); !sameDestructiveRuntime(current, expected) {
		return destructiveRuntimeConflict(current, expected)
	}
	return nil
}

var runtimeGenerationCandidateInventoryFn = tmux.ListRuntimeGenerationCandidates

var terminateCapturedRuntimeFn = tmux.KillRuntimeGenerationCandidate

// discoverCapturedRuntimeChildrenFn returns, but does not register, descendants
// of the stable tmux identity selected for destruction. The caller registers
// them only after the atomic conditional kill succeeds, so a replaced target
// can never turn this auxiliary cleanup into a kill of the replacement.
var discoverCapturedRuntimeChildrenFn = func(i *Instance, candidate tmux.RuntimeGenerationCandidate) ([]tmux.ProcessIdentity, error) {
	target := tmux.ReconnectSessionLazy(candidate.SessionID, candidate.SessionName, "", "", "")
	target.SocketName = candidate.SocketName
	target.InstanceID = candidate.InstanceID
	shadow := &Instance{
		ID:             candidate.InstanceID,
		Title:          i.Title,
		TmuxSocketName: candidate.SocketName,
		tmuxSession:    target,
	}
	return shadow.captureMCPChildrenFromPaneTree()
}

var runtimeDestructionReservedFn = func(statedb.RuntimeState) {}

var (
	captureParentDeleteServiceOwnershipFn = func(i *Instance, selection RuntimeSelection) tmux.ServiceUnitOwnership {
		state := selection.State
		if state.TmuxSession == "" {
			return tmux.ServiceUnitOwnership{}
		}
		target := tmux.ReconnectSessionLazy(
			state.TmuxSession, i.Title, i.EffectiveWorkingDir(), i.Command, state.Status,
		)
		target.SocketName = state.TmuxSocketName
		return target.ServiceUnitOwnership()
	}
	retireParentDeleteServiceUnitFn = func(i *Instance, ownership tmux.ServiceUnitOwnership) {
		_ = i.RetireServiceUnit(ownership)
	}
)

// deleteCapturedAndRetireService snapshots the service-unit ownership proof for
// the selected runtime before teardown, then retires that unit only after the
// exact parent deletion succeeds. Keeping this in the parent-deleting APIs
// prevents individual CLI, TUI, web, worktree, and conductor callers from
// forgetting the Restart=on-failure cleanup choreography.
func (i *Instance) deleteCapturedAndRetireService(selection RuntimeSelection, deleteParent func() error) error {
	ownership := captureParentDeleteServiceOwnershipFn(i, selection)
	if err := deleteParent(); err != nil {
		return err
	}
	retireParentDeleteServiceUnitFn(i, ownership)
	return nil
}

// captureDestructiveRuntimeCandidateLocked reconciles all logical-instance
// candidates before a destruction reservation, then freezes the stable tmux
// identity of the durable winner. The caller already holds the instance lock.
// A target that vanished between inventories is already stopped; a target that
// appeared or changed is preserved for a later reconciliation pass.
func (i *Instance) captureDestructiveRuntimeCandidateLocked(
	db *statedb.StateDB, expected statedb.RuntimeState, incarnation string, durable bool,
) (*tmux.RuntimeGenerationCandidate, error) {
	live := false
	if durable {
		reconciled, err := i.reconcileRuntimeLocked(db, expected, incarnation)
		if err != nil {
			return nil, err
		}
		if !sameDestructiveRuntime(reconciled.State, expected) {
			return nil, destructiveRuntimeConflict(reconciled.State, expected)
		}
		live = reconciled.Live
	}

	socketName := expected.TmuxSocketName
	candidates, err := runtimeGenerationCandidateInventoryFn(socketName, expected.InstanceID)
	if err != nil {
		return nil, err
	}

	var selected *tmux.RuntimeGenerationCandidate
	for idx := range candidates {
		candidate := candidates[idx]
		if candidate.InstanceID != expected.InstanceID ||
			candidate.SocketName != socketName || !candidate.GenerationKnown {
			return nil, fmt.Errorf("runtime destruction candidate has incomplete identity: %w", statedb.ErrRuntimeGenerationConflict)
		}
		switch {
		case candidate.Generation < expected.Generation:
			continue
		case candidate.Generation > expected.Generation:
			return nil, fmt.Errorf("newer runtime candidate appeared during destruction: %w", statedb.ErrRuntimeGenerationConflict)
		case candidate.SessionName != expected.TmuxSession:
			return nil, fmt.Errorf("same-generation runtime identity changed during destruction: %w", statedb.ErrRuntimeGenerationConflict)
		case selected != nil:
			return nil, fmt.Errorf("multiple stable identities claim the selected runtime: %w", statedb.ErrRuntimeGenerationConflict)
		default:
			captured := candidate
			selected = &captured
		}
	}

	if durable && !live && selected != nil {
		return nil, fmt.Errorf("runtime candidate appeared after reconciliation: %w", statedb.ErrRuntimeGenerationConflict)
	}
	return selected, nil
}

// terminateTransitionPredecessor stops the exact runtime captured when the
// caller acquired restart transition authority. The caller already holds the
// per-instance transition lock, so this helper performs the durable status
// reservation without trying to acquire that lock a second time.
func (i *Instance) terminateTransitionPredecessor(authority *runtimeTransitionAuthority) error {
	if authority == nil || authority.expected.InstanceID != i.ID {
		return statedb.ErrRuntimeGenerationConflict
	}
	selected := authority.expected
	if selected.TmuxSession == "" {
		return nil
	}
	if !authority.durable {
		if err := i.validateRuntimeSelection(authority.db, RuntimeSelection{State: selected, Incarnation: authority.incarnation}, false); err != nil {
			return err
		}
		candidate, err := i.captureDestructiveRuntimeCandidateLocked(
			authority.db, selected, authority.incarnation, false)
		if err != nil || candidate == nil {
			return err
		}
		return terminateCapturedRuntimeFn(*candidate, false)
	}
	if authority.db == nil {
		return fmt.Errorf("runtime database unavailable")
	}
	selection := RuntimeSelection{State: selected, Incarnation: authority.incarnation}
	if err := i.validateRuntimeSelection(authority.db, selection, true); err != nil {
		return err
	}
	candidate, err := i.captureDestructiveRuntimeCandidateLocked(
		authority.db, selected, authority.incarnation, true)
	if err != nil {
		return err
	}
	claimed, err := authority.db.ReserveRuntimeDestruction(selected, authority.incarnation)
	if err != nil {
		return err
	}
	runtimeDestructionReservedFn(claimed)
	release := func(status string) error {
		completed, completeErr := authority.db.CompleteRuntimeDestruction(claimed, authority.incarnation, status)
		if completeErr == nil {
			authority.expected = completed
			i.adoptRuntimeState(completed)
		}
		return completeErr
	}
	selection.State = claimed
	if err := i.validateRuntimeSelection(authority.db, selection, true); err != nil {
		if releaseErr := release(selected.Status); releaseErr != nil {
			return fmt.Errorf("runtime selection changed: %v; failed to release runtime reservation: %w", err, releaseErr)
		}
		return err
	}
	if candidate != nil {
		err = terminateCapturedRuntimeFn(*candidate, false)
	}
	if err != nil {
		if releaseErr := release(selected.Status); releaseErr != nil {
			return fmt.Errorf("failed to kill predecessor runtime: %v; failed to release runtime reservation: %w", err, releaseErr)
		}
		return err
	}
	return release(string(StatusStopped))
}

// KillCaptured stops exactly the selected runtime and retains its metadata.
func (i *Instance) KillCaptured(selection RuntimeSelection) error {
	return i.killInternal(selection, false, false, true, nil)
}

// KillCapturedRuntime stops exactly the selected runtime and returns the
// authoritative post-kill tuple before releasing lifecycle authority.
func (i *Instance) KillCapturedRuntime(selection RuntimeSelection) (runtime statedb.RuntimeState, err error) {
	err = i.killInternal(selection, false, false, true, &runtime)
	return runtime, err
}

// KillAndWaitCaptured is KillCaptured with synchronous process-tree reaping.
func (i *Instance) KillAndWaitCaptured(selection RuntimeSelection) error {
	return i.killInternal(selection, true, false, true, nil)
}

// DeleteCaptured stops and conditionally deletes exactly the selected runtime.
func (i *Instance) DeleteCaptured(selection RuntimeSelection) error {
	return i.deleteCapturedAndRetireService(selection, func() error {
		return i.killInternal(selection, false, true, true, nil)
	})
}

// DeleteCapturedWithCleanup is DeleteCaptured with a best-effort filesystem
// cleanup callback that runs after the exact runtime is stopped but before its
// parent row is removed. Keeping the row present during cleanup prevents a
// same-ID replacement from being inserted and then losing paths owned by the
// deleted incarnation.
func (i *Instance) DeleteCapturedWithCleanup(selection RuntimeSelection, cleanup func()) error {
	return i.deleteCapturedAndRetireService(selection, func() error {
		return i.killInternalWithCleanup(selection, false, true, true, nil, cleanup)
	})
}

// DeleteAndWaitCaptured is DeleteCaptured with synchronous process-tree reaping.
func (i *Instance) DeleteAndWaitCaptured(selection RuntimeSelection) error {
	return i.deleteCapturedAndRetireService(selection, func() error {
		return i.killInternal(selection, true, true, true, nil)
	})
}

// RemoveCaptured conditionally deletes a stopped/error selection without
// touching a physical process.
func (i *Instance) RemoveCaptured(selection RuntimeSelection) error {
	return i.deleteCapturedAndRetireService(selection, func() error {
		return i.killInternal(selection, false, true, false, nil)
	})
}
