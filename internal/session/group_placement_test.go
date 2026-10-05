package session

import "testing"

func TestCaptureSessionPlacementRestoresMoveAndOnlyDropsItsOwnGroup(t *testing.T) {
	first := &Instance{ID: "first", GroupPath: DefaultGroupPath, Order: 0}
	mover := &Instance{ID: "mover", GroupPath: DefaultGroupPath, Order: 5}
	last := &Instance{ID: "last", GroupPath: DefaultGroupPath, Order: 9}
	tree := NewGroupTree([]*Instance{first, mover, last})
	// Empty and unsaved, like a TUI create still awaiting its save: a restore
	// must not mistake it for a group the retracted move created.
	tree.CreateGroup("Existing")

	assertRestored := func(t *testing.T) {
		t.Helper()
		if mover.GroupPath != DefaultGroupPath || mover.Order != 5 || tree.SessionPosition(mover) != 1 {
			t.Fatalf("restored to group=%q order=%d slot=%d, want %q/5/1",
				mover.GroupPath, mover.Order, tree.SessionPosition(mover), DefaultGroupPath)
		}
	}

	restore := tree.CaptureSessionPlacement(mover)
	tree.MoveSessionToGroup(mover, tree.ResolveMoveTargetGroup("root"))
	restore()
	assertRestored(t)

	restore = tree.CaptureSessionPlacement(mover)
	tree.MoveSessionToGroup(mover, tree.ResolveMoveTargetGroup("existing"))
	restore()
	assertRestored(t)
	if _, ok := tree.Groups["Existing"]; !ok {
		t.Fatal("restore dropped a group that existed before the move")
	}

	restore = tree.CaptureSessionPlacement(mover)
	tree.MoveSessionToGroup(mover, tree.ResolveMoveTargetGroup("Fresh"))
	restore()
	assertRestored(t)
	if _, ok := tree.Groups["Fresh"]; ok {
		t.Fatal("restore kept the group the retracted move created")
	}
	for _, group := range tree.GroupList {
		if group.Path == "Fresh" {
			t.Fatal("restore left the created group in GroupList")
		}
	}
}
