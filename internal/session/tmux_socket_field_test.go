package session

import (
	"errors"
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// Moving an existing session to another tmux socket is refused, stopped or
// not. Upstream changes the stored socket and the tmux handle in memory, and
// the CLI then saves with SaveWithGroups. Here the socket is part of the
// durable runtime (instance_runtime_state.tmux_socket_name): the snapshot save
// never writes it and the next start re-adopts the stored socket, so a
// "moved" session would start on the old server after the CLI said
// "Updated".
func TestSetField_TmuxSocket_RefusesMovingAnExistingSession(t *testing.T) {
	for _, target := range []string{"deck36", ""} {
		inst := &Instance{ID: "w1", TmuxSocketName: "old", tmuxSession: &tmux.Session{Name: "agentdeck_w1", SocketName: "old"}}
		_, _, err := SetField(inst, FieldTmuxSocket, " "+target+" ", nil)
		var me *MutationError
		if !errors.As(err, &me) || !strings.Contains(me.Msg, "--tmux-socket") {
			t.Fatalf("move to %q: want a MutationError pointing at --tmux-socket, got %v", target, err)
		}
		if inst.TmuxSocketName != "old" || inst.GetTmuxSession().SocketName != "old" {
			t.Fatalf("move to %q changed the socket on refusal: %q / %q", target, inst.TmuxSocketName, inst.GetTmuxSession().SocketName)
		}
	}
}

// Setting the socket a session already has is a no-op, not a refusal.
func TestSetField_TmuxSocket_SameSocketIsANoOp(t *testing.T) {
	inst := &Instance{ID: "w2", TmuxSocketName: "old", tmuxSession: &tmux.Session{Name: "agentdeck_w2", SocketName: "old"}}
	if old, _, err := SetField(inst, FieldTmuxSocket, "old", nil); err != nil || old != "old" {
		t.Fatalf("same socket: old=%q err=%v", old, err)
	}
}

func TestSetField_TmuxSocket_RejectsPathsAndJunk(t *testing.T) {
	for _, bad := range []string{"../x", "/tmp/sock", "a b", "-L", "x;y"} {
		inst := &Instance{ID: "w3"}
		if _, _, err := SetField(inst, FieldTmuxSocket, bad, nil); err == nil {
			t.Errorf("socket %q accepted", bad)
		}
		if inst.TmuxSocketName != "" {
			t.Errorf("socket %q stored", bad)
		}
	}
}

func TestValidMutableFields_IncludesTmuxSocket(t *testing.T) {
	for _, f := range ValidMutableFields {
		if f == FieldTmuxSocket {
			return
		}
	}
	t.Errorf("FieldTmuxSocket missing from ValidMutableFields")
}
