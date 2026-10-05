package core

import (
	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

// Runtime authority for the registry's lifecycle commands. Physical starts and
// restarts go through the result-carrying *Runtime variants, and every result
// is consumed by session.ConsumePhysicalRuntimeResult: a partial success (a
// live pane whose durability publication or post-start step failed) is adopted
// and reported as a warning, never as a failure a caller would retry against
// the live pane.

// EventRuntimeWarning: a start, or the queue drain a stop triggered, completed
// with a runtime durability warning in Message.
const EventRuntimeWarning EventKind = "runtime.warning"

// Test seams around the physical lifecycle calls and restart reconciliation.
// Production leaves them nil.
var (
	startRuntimeHook     func(inst *session.Instance, message string) (statedb.RuntimeState, error)
	restartRuntimeHook   func(inst *session.Instance, env map[string]string) (statedb.RuntimeState, error)
	reconcileRestartHook func(inst *session.Instance, err error) error
)

// startRuntime starts inst, delivering message once the agent is ready when
// one is given.
func startRuntime(inst *session.Instance, message string) (statedb.RuntimeState, error) {
	if startRuntimeHook != nil {
		return startRuntimeHook(inst, message)
	}
	if message != "" {
		return inst.StartWithMessageRuntime(message)
	}
	return inst.StartRuntime()
}

// restartRuntime restarts inst with one-shot environment overrides.
func restartRuntime(inst *session.Instance, env map[string]string) (statedb.RuntimeState, error) {
	if restartRuntimeHook != nil {
		return restartRuntimeHook(inst, env)
	}
	return inst.RestartWithEnvRuntime(env)
}

// consumeRuntime applies a physical lifecycle result to inst. failure is nil
// on a full or partial success; a partial success returns its operator-visible
// warning instead.
func consumeRuntime(inst *session.Instance, runtime statedb.RuntimeState, err error) (failure error, warning string) {
	_, failure, warning = session.ConsumePhysicalRuntimeResult(inst, runtime, err, reconcileRestartHook)
	return failure, warning
}
