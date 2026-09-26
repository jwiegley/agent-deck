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
