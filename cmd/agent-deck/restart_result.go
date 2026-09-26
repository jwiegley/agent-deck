package main

import (
	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

// consumeRuntimeResult applies the exact runtime returned by a physical
// lifecycle call. A partial success is never returned as a failure: the pane
// is already live, so callers must not enter retry or rollback paths. Instead
// we retry only the durability publication and return an operator-visible
// warning, preserving whichever exact runtime reconciliation proves canonical.
func consumeRuntimeResult(inst *session.Instance, runtime statedb.RuntimeState, err error) (failure error, warning string) {
	_, failure, warning = session.ConsumePhysicalRuntimeResult(inst, runtime, err, nil)
	return failure, warning
}

// initialMessageOutcome is the parenthetical a start with an initial message
// reports. A partial success can leave the pane live without the message
// (session.InitialMessageUndelivered); that start must not claim it was sent.
func initialMessageOutcome(undelivered bool) string {
	if undelivered {
		return "(message not delivered)"
	}
	return "(message sent)"
}
