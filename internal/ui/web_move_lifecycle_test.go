package ui

import (
	"errors"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/web"
)

// newLiveWebMoveFixture seeds ids, in that Order, into a live-TUI shaped Home:
// the Tea loop owns h.instances, so a web mutation never re-hydrates them.
func newLiveWebMoveFixture(t *testing.T, profile string, ids ...string) (*Home, *session.Storage, *WebMutator) {
	t.Helper()
	h, storage := newHeadlessHomeForTest(t, profile)
	var seeded []*session.Instance
	for _, id := range ids {
		seeded = append(seeded, seedSession(t, storage, seeded, id, id))
	}
	for i, inst := range seeded {
		inst.Order = i
	}
	if err := storage.SaveWithGroups(seeded, session.NewGroupTree(seeded)); err != nil {
		t.Fatalf("order fixture sessions: %v", err)
	}
	h.headless = false
	if err := h.HydrateInstancesFromStorage(); err != nil {
		t.Fatalf("HydrateInstancesFromStorage: %v", err)
	}
	m := NewWebMutator(h)
	m.requestReloadFn = func() {}
	return h, storage, m
}

func storedGroupPath(t *testing.T, storage *session.Storage, id string) string {
	t.Helper()
	row, err := storage.GetDB().LoadInstanceByID(id)
	if err != nil || row == nil {
		t.Fatalf("stored row %s = %#v, err=%v", id, row, err)
	}
	return row.GroupPath
}

// A live web delete leaves its instance in h.instances behind a tombstone
// until a reload observes the row is gone. The v1.16.22 move endpoint must
// save through the tombstone-aware path, so moving another session in that
// window succeeds, and moving the deleted one is a 404, not a 200 that
// reports a group nothing stored.
func TestRuntimeLifecycle_WebMoveHonorsLiveDeleteTombstone(t *testing.T) {
	h, storage, m := newLiveWebMoveFixture(t, "_test_web_move_tombstone", "web-move-deleted", "web-move-kept")
	deleted := h.instanceByID["web-move-deleted"]
	if err := m.DeleteSession(deleted.ID); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}

	if _, _, err := m.MoveSessionToGroup(deleted.ID, "Work"); !errors.Is(err, web.ErrSessionNotFound) {
		t.Fatalf("move of a just-deleted session: err=%v, want ErrSessionNotFound", err)
	}
	if deleted.GroupPath != session.DefaultGroupPath {
		t.Fatalf("rejected move changed the deleted instance's group to %q", deleted.GroupPath)
	}
	if _, ok := h.groupTree.Groups["Work"]; ok {
		t.Fatal("rejected move created its target group")
	}

	movedTo, _, err := m.MoveSessionToGroup("web-move-kept", "Work")
	if err != nil || movedTo != "Work" {
		t.Fatalf("move beside a live delete: movedTo=%q err=%v", movedTo, err)
	}
	if got := storedGroupPath(t, storage, "web-move-kept"); got != "Work" {
		t.Fatalf("stored group after move = %q, want Work", got)
	}
	if row, err := storage.GetDB().LoadInstanceByID(deleted.ID); err != nil || row != nil {
		t.Fatalf("move resurrected the deleted row: %#v, err=%v", row, err)
	}
}
