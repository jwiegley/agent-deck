package main

import (
	"fmt"
	"os"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// codexLaunchIdentityWait bounds how long launch waits for a fresh Codex
// process to hold its thread open. A launch turn has already started by then;
// a composer without one may take longer, and its first send hydrates it.
const codexLaunchIdentityWait = 3 * time.Second

// persistLiveCodexIdentity binds the instance to the thread the pane's live
// Codex process holds open (its rollout or thread writer lock), through the
// runtime binding authority (Instance.BindLiveCodexThread): the fork rebuilds
// the Codex identity from the runtime binding on load, so upstream's targeted
// tool_data write would not survive. It waits up to wait for that evidence and
// returns the bound id, or "" when there is none. Launch can call it while
// startup detection still runs, since both publish through the same authority.
//
// Launch never persisted this binding for Codex (#2396, #2400): the row
// stayed unbound until a follow-up send, output fell back to pane text, and
// archive killed the only evidence of the identity.
func persistLiveCodexIdentity(storage *session.Storage, inst *session.Instance, wait time.Duration) string {
	if inst == nil || !session.IsCodexCompatible(inst.Tool) || storage == nil || storage.GetDB() == nil {
		return ""
	}
	id, err := inst.BindLiveCodexThread(wait)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not persist Codex session identity: %v\n", err)
		return ""
	}
	return id
}

// adoptLiveCodexIdentity binds an unbound Codex instance to its live thread.
// Bound instances and other tools are left alone.
func adoptLiveCodexIdentity(storage *session.Storage, inst *session.Instance) {
	persistLiveCodexIdentity(storage, inst, 0)
}
