package session

import (
	"fmt"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

var switchAccountRestartWithTransitionFn = func(i *Instance, authority *runtimeTransitionAuthority, result *statedb.RuntimeState) error {
	return i.restartWithTransition(authority, nil, result)
}

// SwitchAccountRuntime keeps one per-instance authority across the captured
// stop, transcript/account commit, and optional replacement spawn.
func (i *Instance) SwitchAccountRuntime(selection RuntimeSelection, restart bool, commitAccount func() error) (runtime statedb.RuntimeState, err error) {
	if selection.State.InstanceID == "" || selection.State.InstanceID != i.ID {
		return runtime, statedb.ErrRuntimeGenerationConflict
	}
	if commitAccount == nil {
		return runtime, fmt.Errorf("switch-account commit is nil")
	}

	authority, winner, err := i.beginRuntimeTransition(false)
	if err != nil {
		return runtime, err
	}
	if winner != nil {
		return *winner, destructiveRuntimeConflict(*winner, selection.State)
	}
	defer authority.close()
	if !sameDestructiveRuntime(authority.expected, selection.State) {
		return authority.expected, destructiveRuntimeConflict(authority.expected, selection.State)
	}

	if err := i.killInternalLocked(selection, false, false, true, &runtime, nil); err != nil {
		return runtime, err
	}
	authority.expected = runtime
	authority.predecessorTerminated = true

	if err := commitAccount(); err != nil {
		return runtime, err
	}
	if !restart {
		return runtime, nil
	}
	err = switchAccountRestartWithTransitionFn(i, authority, &runtime)
	return runtime, err
}
