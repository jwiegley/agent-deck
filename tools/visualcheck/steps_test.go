package main

import (
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// TestCycleRestorePressesReturnsToDefault pins the restore presses in
// stepStateVisibility to the real mode counts. A hard-coded count went stale
// when the time filter gained a fifth mode, leaving "Last 30 days" engaged
// and leaking its legend into every later golden frame.
func TestCycleRestorePressesReturnsToDefault(t *testing.T) {
	for _, c := range []struct {
		key   string
		modes int
	}{
		{"*", session.TimeFilterModeCount},
		{"t", session.GroupViewModeCount},
	} {
		// One press captures the frame; the restore presses must complete
		// exactly one full cycle back to mode 0, the default.
		got := cycleRestorePresses(c.key)
		if mode := (1 + got) % c.modes; mode != 0 || got >= c.modes {
			t.Errorf("key %q: %d restore presses over %d modes ends on mode %d, want %d presses back to the default",
				c.key, got, c.modes, mode, c.modes-1)
		}
	}
}

// TestShellPromptReady accepts the sandbox shell's prompt whether it runs as
// a regular user ($) or as root (#), so the attach step works in a root
// container. Only the last non-empty line counts.
func TestShellPromptReady(t *testing.T) {
	for _, c := range []struct {
		pane string
		want bool
	}{
		{"host:shell-live user$ \n\n", true},
		{"host:shell-live root# \n", true},
		{"$ ", true},
		{"# ", true},
		{"$ clear\n", false},
		{"", false},
		{"\n\n", false},
	} {
		if got := shellPromptReady(c.pane); got != c.want {
			t.Errorf("shellPromptReady(%q) = %v, want %v", c.pane, got, c.want)
		}
	}
}

// TestAttachedPaneBody pins the signal stepAttachShell waits on. The step once
// took the list's absence for the attach having landed and captured the notice
// agent-deck prints before its TUI starts, which the terminal shows again until
// the attached client draws.
func TestAttachedPaneBody(t *testing.T) {
	const name = "agentdeck_shell-live_342dbbf3"
	status := "[" + name + "] 0:sh*    ctrl+q detach · ctrl+s switch │ 📁 shell-live | shell-live"
	for _, c := range []struct {
		label, pane string
		attached    bool
		body        string
	}{
		{"list", " ●●○ Agent Deck\nSESSIONS   │ PREVIEW\n   └─ ○ shell-live shell\n", false, ""},
		{"main screen before the client draws", "agent-deck: new installs now start with safer defaults.\nThis notice is shown once.\n\n\n", false, ""},
		{"another session's client", "$\n\n[agentdeck_other_1234abcd] 0:sh*\n", false, ""},
		{"attached, launch line still shown", "$ export AGENTDECK_INSTANCE_ID=x; exec sh\n$\n\n" + status + "\n", true, "$ export AGENTDECK_INSTANCE_ID=x; exec sh\n$\n"},
		{"attached and cleared", "$\n\n\n" + status + "\n\n", true, "$\n\n"},
	} {
		body, attached := attachedPaneBody(c.pane, name)
		if attached != c.attached || body != c.body {
			t.Errorf("%s: attachedPaneBody = (%q, %v), want (%q, %v)", c.label, body, attached, c.body, c.attached)
		}
	}
	// The body of an attached, cleared pane is what the step's prompt check reads.
	if body, _ := attachedPaneBody("$\n\n\n"+status+"\n", name); !shellPromptReady(body) {
		t.Errorf("cleared attached pane body %q must read as a ready prompt", body)
	}
}
