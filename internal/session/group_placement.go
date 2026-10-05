package session

// CaptureSessionPlacement records where inst sits in the tree so a caller that
// applies a move before saving it (the web move endpoint, #2368) can retract
// the move exactly when the save fails. The returned restore puts inst back in
// its prior group at its prior slot and Order, and drops the group inst was
// moved into when that group did not exist at capture and is still empty and
// unsaved: a failed move then leaves nothing for a later save to persist, and
// queues no deletion for a row that was never written.
func (t *GroupTree) CaptureSessionPlacement(inst *Instance) (restore func()) {
	prior, order, slot := inst.GroupPath, inst.Order, t.SessionPosition(inst)
	existed := make(map[string]bool, len(t.Groups))
	for path := range t.Groups {
		existed[path] = true
	}
	return func() {
		// Even a move into the same group re-appended inst with a new Order.
		moved := inst.GroupPath
		t.MoveSessionToGroup(inst, prior)
		inst.Order = order
		if group := t.Groups[prior]; group != nil {
			// MoveSessionToGroup appended inst; slide it back into its slot.
			if last := len(group.Sessions) - 1; slot >= 0 && slot < last && group.Sessions[last] == inst {
				copy(group.Sessions[slot+1:], group.Sessions[slot:last])
				group.Sessions[slot] = inst
			}
		}
		group := t.Groups[moved]
		if group == nil || existed[moved] || len(group.Sessions) > 0 {
			return
		}
		if snapshot := group.storageSnapshot.Load(); snapshot == nil || snapshot.dbPath == "" {
			delete(t.Groups, moved)
			delete(t.Expanded, moved)
			t.rebuildGroupList()
		}
	}
}
