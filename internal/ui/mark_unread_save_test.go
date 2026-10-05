package ui

import (
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// Pressing `u` (mark unread) clears the acknowledged flag with a targeted
// write, recomputes the status to waiting, and saves.
//
// The targeted write moves last_modified. The save's external-change guard
// compares last_modified against the value captured at load, so without
// adoptOwnWrite the guard fires on this TUI's OWN bump: the save aborts and
// schedules a reload instead.
//
// Upstream's rationale, where the save persisted status: the reload rebuilt
// acknowledged from the stored status -- still "idle", because the aborted
// save was the one that would have written "waiting" -- so the row went
// straight back to gray, however many times the key was pressed.
//
// Under runtime authority that half no longer applies: the handler's
// UpdateStatus commits waiting through the status CAS before the save, and
// snapshot saves drop runtime-owned status, so the stored status is the same
// whether the save commits or aborts. The false positive still costs the
// save itself: the snapshot it carries is discarded, and the TUI schedules a
// reload for a change it made itself. The fixture publishes the probe result
// the way the handler does and checks that the save committed.
//
// This exercises the same sequence as the `u` case in updateInner, minus the
// tmux session it needs to reach that code.
func TestMarkUnread_SaveIsNotAbortedByItsOwnWrite(t *testing.T) {
	h, storage := newHeadlessHomeForTest(t, "_test_mark_unread_save")

	inst := &session.Instance{
		ID:          "mark-unread-001",
		Title:       "worker",
		ProjectPath: "/tmp/mark-unread-proj",
		GroupPath:   session.DefaultGroupPath,
		Command:     "claude",
		Tool:        "claude",
		Status:      session.StatusIdle,
		CreatedAt:   time.Now(),
	}
	all := []*session.Instance{inst}
	if err := storage.SaveWithGroups(all, session.NewGroupTree(all)); err != nil {
		t.Fatalf("seed SaveWithGroups: %v", err)
	}
	h.instances = all
	h.instanceByID[inst.ID] = inst
	h.groupTree = session.NewGroupTree(all)

	// The TUI's freshness marker as of its last load.
	loaded, err := storage.GetFileMtime()
	if err != nil {
		t.Fatalf("GetFileMtime: %v", err)
	}
	h.lastLoadMtime = loaded

	// --- what the `u` key does ---
	stamps, err := storage.GetDB().SetAcknowledgedStamped(inst.ID, false)
	if err != nil {
		t.Fatalf("SetAcknowledgedStamped: %v", err)
	}
	if !h.adoptOwnWrite(stamps, "mark_unread") {
		t.Fatal("adoptOwnWrite refused our own write: nothing else touched this database")
	}
	publishStatusThroughAuthority(t, storage, inst, session.StatusWaiting)
	h.saveInstances()
	// --- end ---

	// Status is runtime-owned, so the save above no longer carries it and the
	// stored status cannot tell a committed save from an aborted one. A
	// committed save stamps last_modified past the acknowledged write and
	// advances this TUI's freshness marker to it; an abort leaves the marker
	// behind and schedules a reload instead.
	current, err := storage.GetFileMtime()
	if err != nil {
		t.Fatalf("GetFileMtime after save: %v", err)
	}
	if !current.After(time.Unix(0, stamps.After)) || !h.lastLoadMtime.Equal(current) {
		t.Errorf("save aborted on this TUI's own acknowledged write: last_modified=%d marker=%d "+
			"acknowledged-write=%d", current.UnixNano(), h.lastLoadMtime.UnixNano(), stamps.After)
	}
}

// The guard must still fire for a real external write. Adopting unconditionally
// would trade a lost mark-unread for a lost update: this TUI would overwrite
// whatever the other process changed while it was stale.
func TestMarkUnread_ForeignWriteStillAbortsTheSave(t *testing.T) {
	h, storage := newHeadlessHomeForTest(t, "_test_mark_unread_foreign")

	inst := &session.Instance{
		ID:          "mark-unread-002",
		Title:       "worker",
		ProjectPath: "/tmp/mark-unread-proj",
		GroupPath:   session.DefaultGroupPath,
		Command:     "claude",
		Tool:        "claude",
		Status:      session.StatusIdle,
		CreatedAt:   time.Now(),
	}
	all := []*session.Instance{inst}
	if err := storage.SaveWithGroups(all, session.NewGroupTree(all)); err != nil {
		t.Fatalf("seed SaveWithGroups: %v", err)
	}
	h.instances = all
	h.instanceByID[inst.ID] = inst
	h.groupTree = session.NewGroupTree(all)

	loaded, err := storage.GetFileMtime()
	if err != nil {
		t.Fatalf("GetFileMtime: %v", err)
	}
	h.lastLoadMtime = loaded

	// Another process writes after we loaded.
	if err := storage.GetDB().Touch(); err != nil {
		t.Fatalf("Touch: %v", err)
	}

	stamps, err := storage.GetDB().SetAcknowledgedStamped(inst.ID, false)
	if err != nil {
		t.Fatalf("SetAcknowledgedStamped: %v", err)
	}
	if h.adoptOwnWrite(stamps, "mark_unread") {
		t.Error("adopted the marker past another process's write: the next save would revert " +
			"whatever that process changed")
	}
}

// RemoteSession parity for the `u` action: mark-unread is local-only by
// construction, so there is no remote path to cover.
//
// Everything the handler does is local state a remote row does not have. It
// clears the acknowledged flag on the local tmux.Session, writes the local
// SQLite `acknowledged` column, and reads the recomputed local status back.
// RemoteSessionInfo (internal/session/ssh.go) carries no acknowledged field --
// it ships id/title/path/group/tool/status/substate/archived/last-activity and
// nothing about whether the viewer has seen the session -- and the SSH surface
// exposes no acknowledge or unread verb to forward the action to.
//
// Routing `u` to a remote would mean designing that protocol first, which is
// well outside a fix for the local save aborting on its own write. Recorded as
// a documented skip rather than a silent omission, per the repository's
// RemoteSession parity guideline.
func TestMarkUnread_RemoteSessionIsOutOfScope(t *testing.T) {
	t.Skip("mark-unread is local-only: RemoteSessionInfo has no acknowledged field and the " +
		"remote surface has no acknowledge/unread operation to route the action to")
}
