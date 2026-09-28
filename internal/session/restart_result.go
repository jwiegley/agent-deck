package session

import (
	"errors"
	"fmt"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

// RestartPartialSuccessError means the replacement pane/runtime is already
// live, but persisting its runtime generation failed. Retrying or rolling back
// the restart is destructive; callers should keep the completed runtime and
// surface this as a durability warning.
type RestartPartialSuccessError struct {
	InstanceID string
	// Operation is the physical call that completed: "start" or "restart".
	// Results built without it read as a restart.
	Operation string
	Runtime   statedb.RuntimeState
	// BindingPlan is the exact CAS intent captured before the physical spawn.
	// Durability reconciliation reuses it verbatim and never spawns again.
	BindingPlan []statedb.RuntimeBindingTransition
	// NeedsReconciliation is true only when publication of Runtime failed.
	// Post-start actions such as initial message delivery may also return this
	// error type, but must not rewrite an already durable generation.
	NeedsReconciliation bool
	// MessageUndelivered is true when StartWithMessage spawned the pane but
	// never delivered the initial message to it. Callers must not report the
	// message as sent.
	MessageUndelivered bool
	Err                error
}

func (e *RestartPartialSuccessError) Error() string {
	operation := e.Operation
	if operation == "" {
		operation = "restart"
	}
	if e.NeedsReconciliation {
		return fmt.Sprintf("%s completed for %s but runtime generation persistence failed: %v", operation, e.InstanceID, e.Err)
	}
	if e.MessageUndelivered && spawnedRuntimeGone(e.Err) {
		// Nothing started: the pane died before its generation was published.
		return fmt.Sprintf("session %s exited before its initial message could be delivered", e.InstanceID)
	}
	if e.MessageUndelivered {
		return fmt.Sprintf("session %s started but its initial message was not delivered: %v", e.InstanceID, e.Err)
	}
	return fmt.Sprintf("%s completed for %s but post-commit cleanup was interrupted: %v", operation, e.InstanceID, e.Err)
}

// errSpawnedRuntimeGone marks commitPhysicalRuntime's exact probe proving the
// runtime tmux just accepted already gone, before its generation could be
// published. An indeterminate probe is never this error.
var errSpawnedRuntimeGone = errors.New("the spawned runtime exited before its generation was published")

// spawnedRuntimeGone reports a start or restart whose spawn tmux accepted but
// whose runtime died before publication. Nothing was published and nothing
// is live, so it is neither a failure to retry nor a partial success to
// reconcile. The call completes as upstream's does once tmux accepted the
// spawn, and reports the canonical runtime its transition kept. The death
// belongs to spawn verification: the fast-death watcher records it,
// VerifySpawned reports it, and PersistSpawnFailureStatus marks that
// canonical runtime errored.
func spawnedRuntimeGone(err error) bool {
	return errors.Is(err, errSpawnedRuntimeGone)
}

// keepCanonicalRuntime gives i back the runtime its transition kept, after a
// spawn proved gone before publication. The spawn may have left i holding a
// tuple nobody committed: Start's starting status and start time, or a
// fallback recreate's new tmux name at the kept generation. Nothing was
// published, so i takes back the durable tuple and its bindings (the local
// tuple without a store), and a fresh restart gets back the bindings
// prepareFresh cleared. i.RuntimeState() then equals the runtime the call
// reports, which ConsumePhysicalRuntimeResult returns as canonical and
// PersistSpawnFailureStatus marks errored with a plain status CAS.
func (a *runtimeTransitionAuthority) keepCanonicalRuntime(i *Instance) {
	if a.durable {
		i.adoptRuntimeSnapshot(a.expected, a.bindings)
	} else {
		i.adoptRuntimeState(a.expected)
	}
	a.restoreFresh(i)
}

func (e *RestartPartialSuccessError) Unwrap() error { return e.Err }

func (e *RestartPartialSuccessError) RestartCompleted() bool { return true }

// IsRestartPartialSuccess recognizes both the production error and compatible
// wrappers used by orchestration layers/tests without coupling them to its
// concrete type.
func IsRestartPartialSuccess(err error) bool {
	if err == nil {
		return false
	}
	var completed interface{ RestartCompleted() bool }
	return errors.As(err, &completed) && completed.RestartCompleted()
}

// InitialMessageUndelivered reports whether a StartWithMessage result left the
// pane live without delivering its initial message. Evaluate it on the raw
// lifecycle error: ConsumePhysicalRuntimeResult turns that error into a warning.
func InitialMessageUndelivered(err error) bool {
	var partial *RestartPartialSuccessError
	return errors.As(err, &partial) && partial.MessageUndelivered
}

// MergeRestartWarnings joins two optional operator warnings (a Codex resume
// warning and a runtime durability warning) into the one line restart reports.
func MergeRestartWarnings(first, second string) string {
	if first == "" {
		return second
	}
	if second == "" {
		return first
	}
	return first + "; " + second
}

// RestartRuntimeCandidate extracts the physical runtime that completed even
// when its durability publication did not.
func RestartRuntimeCandidate(err error) (statedb.RuntimeState, bool) {
	var partial *RestartPartialSuccessError
	if !errors.As(err, &partial) {
		return statedb.RuntimeState{}, false
	}
	return partial.Runtime, partial.Runtime.InstanceID != ""
}

// ConsumePhysicalRuntimeResult is the single caller contract for physical
// start/restart results. A completed runtime is adopted exactly once; a
// partial success is reconciled as a durability warning and never enters a
// retry/rollback failure channel. The returned runtime is always the
// canonical tuple retained by inst after applying or reconciling the result.
//
// reconcile is injectable for orchestration tests. A nil callback uses the
// instance's production durability reconciler.
func ConsumePhysicalRuntimeResult(
	inst *Instance,
	runtime statedb.RuntimeState,
	err error,
	reconcile func(*Instance, error) error,
) (statedb.RuntimeState, error, string) {
	if err == nil {
		if runtime.InstanceID != "" {
			inst.ApplyRuntimeState(runtime)
		}
		return inst.RuntimeState(), nil, ""
	}
	if !IsRestartPartialSuccess(err) {
		return runtime, err, ""
	}

	if candidate, ok := RestartRuntimeCandidate(err); ok {
		runtime = candidate
	}
	if runtime.InstanceID != "" {
		inst.ApplyRuntimeState(runtime)
	}
	if reconcile == nil {
		reconcile = func(instance *Instance, resultErr error) error {
			return instance.ReconcileRestartResult(resultErr)
		}
	}

	warning := err.Error()
	if reconcileErr := reconcile(inst, err); reconcileErr != nil {
		warning = reconcileErr.Error()
	} else {
		warning += "; runtime durability reconciliation completed"
	}
	return inst.RuntimeState(), nil, warning
}

// captureRuntimeResult copies the transition result while the lifecycle core
// still owns it. Callers must never reconstruct this value from a later
// Instance snapshot: another writer may already have advanced the canonical
// object by the time the public method returns.
func captureRuntimeResult(dst *statedb.RuntimeState, state statedb.RuntimeState) {
	if dst != nil {
		*dst = state
	}
}

// The Runtime variants are the result-carrying lifecycle API. Error-only
// methods are adapters around the same internal operations and discard this
// exact transition tuple.
func (i *Instance) StartRuntime() (runtime statedb.RuntimeState, err error) {
	err = i.start(&runtime)
	return runtime, err
}

func (i *Instance) StartWithMessageRuntime(message string) (runtime statedb.RuntimeState, err error) {
	err = i.startWithMessage(message, &runtime)
	return runtime, err
}

func (i *Instance) RestartRuntime() (runtime statedb.RuntimeState, err error) {
	err = i.restartRecorded(nil, &runtime)
	return runtime, err
}

func (i *Instance) RestartWithEnvRuntime(env map[string]string) (runtime statedb.RuntimeState, err error) {
	err = i.restartWithEnv(env, &runtime)
	return runtime, err
}

func (i *Instance) RestartFreshRuntime() (runtime statedb.RuntimeState, err error) {
	err = i.restartFresh(&runtime)
	return runtime, err
}
