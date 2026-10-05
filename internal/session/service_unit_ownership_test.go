// Instance-level guards for the service-mode unit ownership lease
// (issue #1721). The lease itself is exercised in internal/tmux; what
// matters here is that the remove path can never hand the gate an
// ownership record that names somebody else's unit.
package session

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// TestInstanceServiceUnitOwnership_NoTmuxSession_YieldsSkipRecord: an
// instance with no tmux session (never started, or loaded from the DB
// without one) must produce the always-skip zero record. Sanitizing an
// empty name would otherwise resolve to the "session" fallback and stop
// agentdeck-tmux-session.service — a unit that belongs to somebody else.
func TestInstanceServiceUnitOwnership_NoTmuxSession_YieldsSkipRecord(t *testing.T) {
	inst := &Instance{}

	own := inst.ServiceUnitOwnership()
	require.Equal(t, tmux.ServiceUnitOwnership{}, own,
		"a session-less instance must not name any tmux session or socket")

	dec := inst.RetireServiceUnit(own)
	assert.False(t, dec.Stopped, "no unit may be stopped for a session-less instance")
	assert.Equal(t, tmux.UnitStopSkipNoTmuxSession, dec.Reason)
	assert.Empty(t, dec.Unit, "no unit name may be derived from an empty session name")
}

// TestInstanceServiceUnitOwnership_CarriesSessionAndSocket: the record the
// remove path snapshots before teardown must identify both the session
// (which unit) and the socket (which server's occupancy proves ownership).
func TestInstanceServiceUnitOwnership_CarriesSessionAndSocket(t *testing.T) {
	sess := tmux.NewSession("ownership-record", "/tmp")
	sess.SocketName = "ad-test-1721"
	inst := &Instance{tmuxSession: sess, TmuxSocketName: sess.SocketName}

	own := inst.ServiceUnitOwnership()
	assert.Equal(t, sess.Name, own.SessionName)
	assert.Equal(t, "ad-test-1721", own.SocketName)
}

func TestRuntimeLifecycle_ParentDeleteRetiresServiceOnlyAfterSuccess(t *testing.T) {
	originalCapture := captureParentDeleteServiceOwnershipFn
	originalRetire := retireParentDeleteServiceUnitFn
	t.Cleanup(func() {
		captureParentDeleteServiceOwnershipFn = originalCapture
		retireParentDeleteServiceUnitFn = originalRetire
	})

	var events []string
	captureParentDeleteServiceOwnershipFn = func(*Instance, RuntimeSelection) tmux.ServiceUnitOwnership {
		events = append(events, "capture")
		return tmux.ServiceUnitOwnership{SessionName: "selected"}
	}
	retireParentDeleteServiceUnitFn = func(_ *Instance, ownership tmux.ServiceUnitOwnership) {
		events = append(events, "retire:"+ownership.SessionName)
	}

	inst := &Instance{ID: "selected"}
	selection := RuntimeSelection{}
	wantErr := errors.New("delete rejected")
	if err := inst.deleteCapturedAndRetireService(selection, func() error {
		events = append(events, "delete-failed")
		return wantErr
	}); !errors.Is(err, wantErr) {
		t.Fatalf("failed deletion error = %v, want %v", err, wantErr)
	}
	require.Equal(t, []string{"capture", "delete-failed"}, events)

	events = nil
	if err := inst.deleteCapturedAndRetireService(selection, func() error {
		events = append(events, "delete-succeeded")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	require.Equal(t, []string{"capture", "delete-succeeded", "retire:selected"}, events)
}
