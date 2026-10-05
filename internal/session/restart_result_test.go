package session

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestInitialMessageUndelivered(t *testing.T) {
	sendErr := errors.New("timeout waiting for agent to be ready")
	undelivered := &RestartPartialSuccessError{InstanceID: "one", MessageUndelivered: true, Err: sendErr}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"undelivered", undelivered, true},
		{"wrapped", fmt.Errorf("start: %w", undelivered), true},
		{"publication failure", &RestartPartialSuccessError{InstanceID: "one", NeedsReconciliation: true, Err: sendErr}, false},
		{"plain failure", sendErr, false},
		{"success", nil, false},
	}
	for _, c := range cases {
		if got := InitialMessageUndelivered(c.err); got != c.want {
			t.Errorf("%s: InitialMessageUndelivered = %v, want %v", c.name, got, c.want)
		}
	}
	// The warning a start reports must name the undelivered message, not a
	// restart cleanup.
	if msg := undelivered.Error(); !strings.Contains(msg, "initial message was not delivered") || strings.Contains(msg, "restart") {
		t.Fatalf("warning = %q", msg)
	}
}

// Reconciliation rebuilds the partial-success error when it cannot prove the
// live runtime; the undelivered message must survive that rebuild or the
// start would report its message as sent.
func TestReconcileRestartResultKeepsUndeliveredMessage(t *testing.T) {
	installRuntimeLifecycleTestSeams(t)
	_, inst := runtimeLifecycleTestDB(t, "pi", nil)
	partial := &RestartPartialSuccessError{
		InstanceID: inst.ID, Runtime: inst.RuntimeState(),
		MessageUndelivered: true, Err: errors.New("timeout waiting for agent to be ready"),
	}
	err := inst.ReconcileRestartResult(partial)
	if err == nil {
		t.Fatal("reconciliation proved a live runtime the fixture never spawned")
	}
	if !InitialMessageUndelivered(err) {
		t.Fatalf("rebuilt error lost the undelivered message: %v", err)
	}
}

// A partial success names the call that produced it; one built without an
// operation reads as a restart, as every such result did before.
func TestRestartPartialSuccessErrorNamesOperation(t *testing.T) {
	cause := errors.New("stamp failed")
	cases := []struct {
		partial *RestartPartialSuccessError
		want    string
	}{
		{&RestartPartialSuccessError{InstanceID: "one", Operation: "start", NeedsReconciliation: true, Err: cause},
			"start completed for one but runtime generation persistence failed: stamp failed"},
		{&RestartPartialSuccessError{InstanceID: "one", Operation: "restart", NeedsReconciliation: true, Err: cause},
			"restart completed for one but runtime generation persistence failed: stamp failed"},
		{&RestartPartialSuccessError{InstanceID: "one", NeedsReconciliation: true, Err: cause},
			"restart completed for one but runtime generation persistence failed: stamp failed"},
		{&RestartPartialSuccessError{InstanceID: "one", Operation: "start", Err: cause},
			"start completed for one but post-commit cleanup was interrupted: stamp failed"},
	}
	for _, c := range cases {
		if got := c.partial.Error(); got != c.want {
			t.Errorf("Error() = %q, want %q", got, c.want)
		}
	}
}

// Reconciliation rebuilds a partial success it could not settle; the rebuilt
// warning must still name a start as a start.
func TestReconcileRestartResultKeepsOperation(t *testing.T) {
	installRuntimeLifecycleTestSeams(t)
	_, inst := runtimeLifecycleTestDB(t, "pi", nil)
	partial := &RestartPartialSuccessError{
		InstanceID: inst.ID, Operation: "start", Runtime: inst.RuntimeState(),
		NeedsReconciliation: true, Err: errors.New("stamp failed"),
	}
	err := inst.ReconcileRestartResult(partial)
	var rebuilt *RestartPartialSuccessError
	if !errors.As(err, &rebuilt) {
		t.Fatalf("reconciliation of an unpublished runtime = %v, want a rebuilt partial success", err)
	}
	if rebuilt.Operation != "start" || !strings.HasPrefix(err.Error(), "start completed for ") {
		t.Fatalf("rebuilt warning = %q (operation %q), want it to name the start", err, rebuilt.Operation)
	}
}

func TestMergeRestartWarnings(t *testing.T) {
	cases := []struct{ first, second, want string }{
		{"", "", ""},
		{"codex", "", "codex"},
		{"", "durability", "durability"},
		{"codex", "durability", "codex; durability"},
	}
	for _, c := range cases {
		if got := MergeRestartWarnings(c.first, c.second); got != c.want {
			t.Errorf("MergeRestartWarnings(%q, %q) = %q, want %q", c.first, c.second, got, c.want)
		}
	}
}
