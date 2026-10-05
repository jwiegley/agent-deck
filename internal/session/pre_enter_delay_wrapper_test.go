package session

import "testing"

// A wrapper prepared for a live runtime (storage load, runtime adoption,
// discovery) gets the tool's pre-Enter delay (#3064) from
// configureTmuxWrapperLocked before it is installed, so the instance may have
// no wrapper yet. Setting it on i.tmuxSession instead dereferenced a nil
// wrapper on adoption and, had that been guarded, left the adopted wrapper
// with the transport default.
func TestNewTmuxWrapperLocked_SetsPreEnterDelayOnThePreparedWrapper(t *testing.T) {
	want := preEnterDelayForTool("devin")
	if want == 0 {
		t.Fatal("precondition: devin has a pre-Enter delay")
	}
	inst := &Instance{ID: "pre-enter-devin", Title: "pre-enter", Tool: "devin", ProjectPath: t.TempDir()}
	sess := inst.newTmuxWrapperLocked("agentdeck_pre_enter_devin", "")
	if got := sess.PreEnterDelay(); got != want {
		t.Fatalf("prepared wrapper pre-Enter delay = %v, want %v", got, want)
	}
	if inst.tmuxSession != nil {
		t.Fatal("preparing a wrapper installed it on the instance")
	}
}
