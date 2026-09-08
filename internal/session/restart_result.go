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
	Runtime    statedb.RuntimeState
	// BindingPlan is the exact CAS intent captured before the physical spawn.
	// Durability reconciliation reuses it verbatim and never spawns again.
	BindingPlan []statedb.RuntimeBindingTransition
	// NeedsReconciliation is true only when publication of Runtime failed.
	// Post-start actions such as initial message delivery may also return this
	// error type, but must not rewrite an already durable generation.
	NeedsReconciliation bool
	Err                 error
}

func (e *RestartPartialSuccessError) Error() string {
	if e.NeedsReconciliation {
		return fmt.Sprintf("restart completed for %s but runtime generation persistence failed: %v", e.InstanceID, e.Err)
	}
	return fmt.Sprintf("restart completed for %s but post-commit cleanup was interrupted: %v", e.InstanceID, e.Err)
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
	err = i.restart(nil, false, &runtime)
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
