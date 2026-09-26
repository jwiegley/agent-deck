package main

import (
	"fmt"

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

// startSuccess is the verdict of a start that completed: `session start` on
// the legacy and registry paths, and `launch`.
type startSuccess struct {
	// verb heads the success line: "Started" or "Launched".
	verb  string
	id    string
	title string
	// warning is the runtime durability warning of a partial success.
	warning string
	// tmux and claudeSessionID are the spawn receipt `session start` echoes.
	// launch leaves them empty and reports the committed row's instead
	// (addLaunchStateJSON).
	tmux            string
	claudeSessionID string
	message         string
	// messageUndelivered: the pane is live but the initial message never
	// reached it (session.InitialMessageUndelivered).
	messageUndelivered bool
	// messageDeferred: launch --no-wait, which reports the message pending
	// because it returns without waiting for the agent. launch sets it for
	// every --no-wait, even when the prompt rode the spawn command and is
	// already delivered, which keeps upstream's message_pending = *noWait.
	messageDeferred bool
}

// renderStartSuccess adds a completed start's fields to jsonData and returns
// its success line. It is the one rendering of an initial message's outcome,
// so no surface can report a message as sent that the start left undelivered.
func renderStartSuccess(s startSuccess, jsonData map[string]interface{}) string {
	jsonData["success"] = true
	jsonData["id"] = s.id
	jsonData["title"] = s.title
	if s.warning != "" {
		jsonData["warning"] = s.warning
	}
	if s.tmux != "" {
		jsonData["tmux"] = s.tmux
	}
	if s.claudeSessionID != "" {
		jsonData["claude_session_id"] = s.claudeSessionID
	}
	line := fmt.Sprintf("%s session: %s", s.verb, s.title)
	if s.message == "" {
		return line
	}
	jsonData["message"] = s.message
	jsonData["message_pending"] = s.messageUndelivered || s.messageDeferred
	switch {
	case s.messageUndelivered:
		return line + " (message not delivered)"
	case s.messageDeferred:
		return line + " (message sent with --no-wait)"
	default:
		return line + " (message sent)"
	}
}
