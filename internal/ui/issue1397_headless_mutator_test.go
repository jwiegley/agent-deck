package ui

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

// newHeadlessHomeForTest builds a Home backed by a real (sandboxed, _test
// profile) storage but WITHOUT booting bubbletea — exactly the `web --no-tui`
// shape that issue #1397 is about. The in-memory instances/groupTree start
// empty, mirroring a freshly-constructed headless server.
func newHeadlessHomeForTest(t *testing.T, profile string) (*Home, *session.Storage) {
	t.Helper()
	storage, err := session.NewStorageWithProfile(profile)
	if err != nil {
		t.Fatalf("NewStorageWithProfile: %v", err)
	}
	t.Cleanup(func() { _ = storage.Close() })

	h := &Home{
		profile:      storage.Profile(),
		storage:      storage,
		instanceByID: make(map[string]*session.Instance),
		groupTree:    session.NewGroupTree(nil),
		headless:     true,
	}
	return h, storage
}

func seedSession(t *testing.T, storage *session.Storage, existing []*session.Instance, id, title string) *session.Instance {
	t.Helper()
	inst := &session.Instance{
		ID:          id,
		Title:       title,
		ProjectPath: "/tmp/issue1397-proj",
		GroupPath:   session.DefaultGroupPath,
		Command:     "bash",
		Tool:        "bash",
		Status:      session.StatusStopped,
		CreatedAt:   time.Now(),
	}
	all := append(append([]*session.Instance{}, existing...), inst)
	if err := storage.InsertSessionAndVerify(inst, session.NewGroupTree(all)); err != nil {
		t.Fatalf("seed InsertSessionAndVerify: %v", err)
	}
	return inst
}

func newLiveHomeForTest(t *testing.T, profile string) (*Home, *session.Storage) {
	t.Helper()
	previousGlobal := statedb.GetGlobal()
	h := NewHomeWithProfileAndMode(profile)
	if h.storage == nil {
		t.Fatal("live Home storage unavailable")
	}
	h.feedbackDialog = nil
	h.startupReconcileOnce.Do(func() {})
	t.Cleanup(func() {
		h.cancel()
		_ = h.storage.Close()
		statedb.SetGlobal(previousGlobal)
	})
	return h, h.storage
}

func hasDurableDeleteTombstone(h *Home, id string) bool {
	h.durableDeleteMu.Lock()
	defer h.durableDeleteMu.Unlock()
	_, ok := h.durableDeleteTombstones[id]
	return ok
}

// TestIssue1397_HeadlessDeleteFindsExistingSession verifies that a WebMutator
// backed by a headless Home (empty in-memory list) can delete a session that
// exists in storage. Pre-fix this returned "session not found" because the
// mutator only consulted the never-populated h.instanceByID.
func TestIssue1397_HeadlessDeleteFindsExistingSession(t *testing.T) {
	h, storage := newHeadlessHomeForTest(t, "_test_1397_delete")
	s1 := seedSession(t, storage, nil, "issue1397-del-001", "existing1")
	_ = seedSession(t, storage, []*session.Instance{s1}, "issue1397-del-002", "existing2")

	m := NewWebMutator(h)

	// The Home's in-memory map is empty — this is the headless precondition.
	if len(h.instanceByID) != 0 {
		t.Fatalf("precondition: headless Home should start with empty instanceByID, got %d", len(h.instanceByID))
	}

	if err := m.DeleteSession("issue1397-del-001"); err != nil {
		t.Fatalf("DeleteSession on existing session must succeed in headless mode, got: %v", err)
	}

	// Verify the row is actually gone from storage.
	remaining, _, err := storage.LoadWithGroups()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	for _, r := range remaining {
		if r.ID == "issue1397-del-001" {
			t.Fatalf("deleted session still present in storage")
		}
	}
	if len(remaining) != 1 {
		t.Fatalf("expected 1 remaining session, got %d", len(remaining))
	}
}

func TestRuntimeLifecycle_WebDeleteEvictsCanonicalStateBeforeLaterPersist(t *testing.T) {
	h, storage := newHeadlessHomeForTest(t, "_test_web_delete_persist")
	remove := seedSession(t, storage, nil, "web-delete-remove", "remove")
	_ = seedSession(t, storage, []*session.Instance{remove}, "web-delete-keep", "keep")
	h.search = NewSearch()
	m := NewWebMutator(h)

	if err := m.DeleteSession("web-delete-remove"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if h.instanceByID["web-delete-remove"] != nil {
		t.Fatal("deleted session remains in instanceByID")
	}
	for _, inst := range h.instances {
		if inst.ID == "web-delete-remove" {
			t.Fatal("deleted session remains in instances")
		}
	}
	for _, inst := range h.search.allItems {
		if inst.ID == "web-delete-remove" {
			t.Fatal("deleted session remains in search")
		}
	}
	for _, item := range h.groupTree.Flatten() {
		if item.Session != nil && item.Session.ID == "web-delete-remove" {
			t.Fatal("deleted session remains in group tree")
		}
	}
	for _, item := range h.flatItems {
		if item.Session != nil && item.Session.ID == "web-delete-remove" {
			t.Fatal("deleted session remains in flat items")
		}
	}

	// Reproduce the later Home save that used to reinsert the stale canonical
	// object after the targeted delete had already removed its database rows.
	if err := m.persistAllInstances(); err != nil {
		t.Fatalf("persistAllInstances: %v", err)
	}
	if row, err := storage.GetDB().LoadInstanceByID("web-delete-remove"); err != nil || row != nil {
		t.Fatalf("deleted instance after persist = %#v, err %v", row, err)
	}
	if runtime, found, err := storage.GetDB().ReadRuntimeState("web-delete-remove"); err != nil || found {
		t.Fatalf("deleted runtime after persist = %#v, found %v, err %v", runtime, found, err)
	}
	if row, err := storage.GetDB().LoadInstanceByID("web-delete-keep"); err != nil || row == nil {
		t.Fatalf("unrelated instance after persist = %#v, err %v", row, err)
	}
	if runtime, found, err := storage.GetDB().ReadRuntimeState("web-delete-keep"); err != nil || !found {
		t.Fatalf("unrelated runtime after persist = %#v, found %v, err %v", runtime, found, err)
	}
}

func TestRuntimeLifecycle_WebDeleteLiveModeLeavesTeaOwnedCanonicalStateUntouched(t *testing.T) {
	h, storage := newHeadlessHomeForTest(t, "_test_web_delete_live_owner")
	remove := seedSession(t, storage, nil, "web-live-remove", "remove")
	_ = seedSession(t, storage, []*session.Instance{remove}, "web-live-keep", "keep")
	h.headless = false
	h.search = NewSearch()
	if err := h.HydrateInstancesFromStorage(); err != nil {
		t.Fatalf("HydrateInstancesFromStorage: %v", err)
	}
	h.search.SetItems(h.instances)
	h.rebuildFlatItems()
	canonical := h.instanceByID["web-live-remove"]
	if canonical == nil {
		t.Fatal("missing live canonical fixture")
	}
	if err := storage.GetDB().SetMeta("last_modified", "1"); err != nil {
		t.Fatalf("SetMeta(last_modified): %v", err)
	}

	if err := NewWebMutator(h).DeleteSession(canonical.ID); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if h.instanceByID[canonical.ID] != canonical {
		t.Fatal("live web delete directly changed Tea-owned instanceByID")
	}
	for name, items := range map[string][]*session.Instance{
		"instances": h.instances,
		"search":    h.search.allItems,
	} {
		found := false
		for _, inst := range items {
			found = found || inst == canonical
		}
		if !found {
			t.Fatalf("live web delete directly removed canonical from %s", name)
		}
	}
	for _, item := range h.groupTree.Flatten() {
		if item.Session == canonical {
			goto foundGroup
		}
	}
	t.Fatal("live web delete directly removed canonical from group tree")

foundGroup:
	for _, item := range h.flatItems {
		if item.Session == canonical {
			goto foundFlat
		}
	}
	t.Fatal("live web delete directly removed canonical from flat items")

foundFlat:
	// A background completion can force-save before the watcher reload reaches
	// the Tea loop. The stale canonical row must remain excluded durably.
	h.forceSaveInstances()
	if row, err := storage.GetDB().LoadInstanceByID(canonical.ID); err != nil || row != nil {
		t.Fatalf("durable deleted row after force-save = %#v, err=%v", row, err)
	}
	if modified, err := storage.GetDB().LastModified(); err != nil || modified <= 1 {
		t.Fatalf("last_modified = %d, err=%v; watcher cannot observe delete", modified, err)
	}
}

func TestRuntimeLifecycle_WebDeleteLiveReloadTombstoneRejectsStaleThenClearsOnFresh(t *testing.T) {
	h, storage := newLiveHomeForTest(t, "_test_web_delete_live_reload")
	remove := seedSession(t, storage, nil, "web-live-reload-remove", "remove")
	_ = seedSession(t, storage, []*session.Instance{remove}, "web-live-reload-keep", "keep")
	staleInstances, staleGroups, err := storage.LoadWithGroups()
	if err != nil {
		t.Fatalf("load stale snapshot: %v", err)
	}
	if err := h.HydrateInstancesFromStorage(); err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	h.search.SetItems(h.instances)
	h.rebuildFlatItems()

	if err := NewWebMutator(h).DeleteSession(remove.ID); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if !hasDurableDeleteTombstone(h, remove.ID) {
		t.Fatal("successful live delete did not install tombstone")
	}

	staleVersion := h.beginReload()
	h.Update(loadSessionsMsg{
		instances: staleInstances,
		groups:    staleGroups, reloadVersion: staleVersion,
	})
	if h.instanceByID[remove.ID] != nil {
		t.Fatal("stale reload republished tombstoned instance")
	}
	if !hasDurableDeleteTombstone(h, remove.ID) {
		t.Fatal("stale reload cleared tombstone while deleted row was present")
	}
	h.forceSaveInstances()
	if row, err := storage.GetDB().LoadInstanceByID(remove.ID); err != nil || row != nil {
		t.Fatalf("stale reload/force-save resurrected row: %#v, err=%v", row, err)
	}

	freshCeiling := h.captureDurableDeleteSeq()
	freshInstances, freshGroups, err := storage.LoadWithGroups()
	if err != nil {
		t.Fatalf("load fresh snapshot: %v", err)
	}
	freshVersion := h.beginReload()
	h.Update(loadSessionsMsg{
		instances: freshInstances,
		groups:    freshGroups, reloadVersion: freshVersion, deleteSeqCeiling: freshCeiling,
	})
	if hasDurableDeleteTombstone(h, remove.ID) {
		t.Fatal("fresh reload observing durable absence did not clear tombstone")
	}
	if h.instanceByID[remove.ID] != nil {
		t.Fatal("fresh reload republished deleted instance")
	}
}

func TestRuntimeLifecycle_WebDeleteTombstoneDoesNotHideSameIDReplacement(t *testing.T) {
	h, storage := newLiveHomeForTest(t, "_test_web_delete_replacement_incarnation")
	removed := seedSession(t, storage, nil, "web-live-replacement", "removed")
	if err := h.HydrateInstancesFromStorage(); err != nil {
		t.Fatal(err)
	}
	if err := NewWebMutator(h).DeleteSession(removed.ID); err != nil {
		t.Fatal(err)
	}

	replacement := seedSession(t, storage, nil, removed.ID, "replacement")
	if replacement.PersistenceIncarnation() == removed.PersistenceIncarnation() {
		t.Fatal("same-ID replacement reused the deleted incarnation")
	}
	ceiling := h.captureDurableDeleteSeq()
	instances, groups, err := storage.LoadWithGroups()
	if err != nil {
		t.Fatal(err)
	}
	reloadVersion := h.beginReload()
	h.Update(loadSessionsMsg{
		instances: instances, groups: groups, reloadVersion: reloadVersion,
		deleteSeqCeiling: ceiling,
	})

	got := h.instanceByID[removed.ID]
	if got == nil || got.Title != "replacement" ||
		!got.MatchesPersistenceIncarnation(replacement.PersistenceIncarnation()) {
		t.Fatalf("same-ID replacement was hidden by old tombstone: %#v", got)
	}
	if hasDurableDeleteTombstone(h, removed.ID) {
		t.Fatal("reload of a same-ID replacement did not retire the old tombstone")
	}
}

func TestRuntimeLifecycle_OldAbsenceReloadCannotClearNewerDeleteToken(t *testing.T) {
	h, storage := newLiveHomeForTest(t, "_test_web_delete_reload_token_aba")
	remove := seedSession(t, storage, nil, "web-live-reload-token-aba", "remove")
	if err := h.HydrateInstancesFromStorage(); err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	if err := NewWebMutator(h).DeleteSession(remove.ID); err != nil {
		t.Fatalf("first DeleteSession: %v", err)
	}

	// This is the newest token the absence snapshot below can prove. A second
	// delete token installed after the load must survive publication of that
	// older snapshot even though the row is absent from it.
	oldCeiling := h.captureDurableDeleteSeq()
	instances, groups, err := storage.LoadWithGroups()
	if err != nil {
		t.Fatalf("load absence snapshot: %v", err)
	}
	h.durableDeleteMu.Lock()
	h.durableDeleteSeq++
	newerToken := h.durableDeleteSeq
	h.durableDeleteTombstones[remove.ID] = durableDeleteTombstone{
		sequence: newerToken, incarnation: remove.PersistenceIncarnation(),
	}
	h.durableDeleteMu.Unlock()

	reloadVersion := h.beginReload()
	h.Update(loadSessionsMsg{
		instances: instances, groups: groups, reloadVersion: reloadVersion,
		deleteSeqCeiling: oldCeiling,
	})

	h.durableDeleteMu.Lock()
	got, ok := h.durableDeleteTombstones[remove.ID]
	h.durableDeleteMu.Unlock()
	if !ok || got.sequence != newerToken {
		t.Fatalf("old reload cleared newer delete token: got=%d present=%v want=%d", got.sequence, ok, newerToken)
	}
}

func TestRuntimeLifecycle_WebDeleteLiveUndoTombstoneLifecycle(t *testing.T) {
	t.Run("successful undo clears", func(t *testing.T) {
		h, storage := newHeadlessHomeForTest(t, "_test_web_undo_live_success")
		remove := seedSession(t, storage, nil, "web-live-undo-success", "restore")
		h.headless = false
		if err := h.HydrateInstancesFromStorage(); err != nil {
			t.Fatalf("hydrate: %v", err)
		}
		canonical := h.instanceByID[remove.ID]
		m := NewWebMutator(h)
		reloadRequests := 0
		m.requestReloadFn = func() { reloadRequests++ }
		if err := m.DeleteSession(remove.ID); err != nil {
			t.Fatalf("DeleteSession: %v", err)
		}
		m.restartRuntimeFn = func(inst *session.Instance) (statedb.RuntimeState, error) {
			return storage.GetDB().EnsureRuntimeState(
				inst.RuntimeState(), inst.PersistenceIncarnation())
		}
		if _, err := m.UndoDelete(); err != nil {
			t.Fatalf("UndoDelete: %v", err)
		}
		if hasDurableDeleteTombstone(h, remove.ID) {
			t.Fatal("successful live undo did not clear tombstone")
		}
		if reloadRequests != 1 {
			t.Fatalf("successful live undo requested %d reloads, want 1", reloadRequests)
		}
		if h.instanceByID[remove.ID] != canonical {
			t.Fatal("live undo mutated Tea-owned canonical state")
		}
	})

	t.Run("failed undo retains", func(t *testing.T) {
		h, storage := newHeadlessHomeForTest(t, "_test_web_undo_live_failure")
		remove := seedSession(t, storage, nil, "web-live-undo-failure", "restore")
		h.headless = false
		if err := h.HydrateInstancesFromStorage(); err != nil {
			t.Fatalf("hydrate: %v", err)
		}
		m := NewWebMutator(h)
		if err := m.DeleteSession(remove.ID); err != nil {
			t.Fatalf("DeleteSession: %v", err)
		}
		m.restartRuntimeFn = func(*session.Instance) (statedb.RuntimeState, error) {
			return statedb.RuntimeState{}, errors.New("restart failed")
		}
		if _, err := m.UndoDelete(); err == nil {
			t.Fatal("UndoDelete unexpectedly succeeded")
		}
		if !hasDurableDeleteTombstone(h, remove.ID) {
			t.Fatal("failed live undo cleared tombstone")
		}
		if row, err := storage.GetDB().LoadInstanceByID(remove.ID); err != nil || row != nil {
			t.Fatalf("failed undo rollback row = %#v, err=%v", row, err)
		}
	})
}

func TestRuntimeLifecycle_WebDeleteLiveUndoConflictClearsOnlyCapturedTokenAndReloads(t *testing.T) {
	for _, tc := range []struct {
		name          string
		newerDelete   bool
		wantTombstone bool
	}{
		{name: "captured delete", wantTombstone: false},
		{name: "newer delete", newerDelete: true, wantTombstone: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, storage := newHeadlessHomeForTest(t, "_test_web_live_undo_conflict_"+strings.ReplaceAll(tc.name, " ", "_"))
			remove := seedSession(t, storage, nil, "web-live-conflict", "deleted")
			h.headless = false
			if err := h.HydrateInstancesFromStorage(); err != nil {
				t.Fatal(err)
			}
			m := NewWebMutator(h)
			if err := m.DeleteSession(remove.ID); err != nil {
				t.Fatal(err)
			}
			captured := m.undoStack[len(m.undoStack)-1].deleteToken
			if tc.newerDelete {
				h.durableDeleteMu.Lock()
				h.durableDeleteSeq++
				h.durableDeleteTombstones[remove.ID] = durableDeleteTombstone{
					sequence: h.durableDeleteSeq, incarnation: remove.PersistenceIncarnation(),
				}
				h.durableDeleteMu.Unlock()
			}

			winner := &session.Instance{
				ID: remove.ID, Title: "winner", ProjectPath: "/tmp/winner",
				GroupPath: session.DefaultGroupPath, Tool: "pi", Status: session.StatusStopped,
				CreatedAt: time.Unix(9, 0).UTC(),
			}
			m.beforeUndoInsertFn = func() {
				if err := storage.InsertSessionAndVerify(winner, nil); err != nil {
					t.Fatal(err)
				}
			}
			reloadRequests := 0
			m.requestReloadFn = func() { reloadRequests++ }

			if _, err := m.UndoDelete(); !errors.Is(err, session.ErrSessionAlreadyExists) {
				t.Fatalf("UndoDelete error = %v, want session already exists", err)
			}
			if reloadRequests != 1 {
				t.Fatalf("conflicting undo requested %d reloads, want 1", reloadRequests)
			}
			if got := hasDurableDeleteTombstone(h, remove.ID); got != tc.wantTombstone {
				t.Fatalf("tombstone present=%v, want %v (captured token %d)", got, tc.wantTombstone, captured)
			}
			row, err := storage.GetDB().LoadInstanceByID(remove.ID)
			if err != nil || row == nil || row.Title != winner.Title {
				t.Fatalf("winner row = %#v, err=%v", row, err)
			}
		})
	}
}

func TestRuntimeLifecycle_WebUndoRotatesIncarnationAndRestoresCanonicalState(t *testing.T) {
	h, storage := newHeadlessHomeForTest(t, "_test_web_undo_headless")
	remove := seedSession(t, storage, nil, "web-undo-restore", "restore")
	deletedIncarnation := remove.PersistenceIncarnation()
	_ = seedSession(t, storage, []*session.Instance{remove}, "web-undo-keep", "keep")
	h.search = NewSearch()
	m := NewWebMutator(h)
	if err := m.DeleteSession(remove.ID); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	m.restartRuntimeFn = func(inst *session.Instance) (statedb.RuntimeState, error) {
		current, err := storage.GetDB().EnsureRuntimeState(
			inst.RuntimeState(), inst.PersistenceIncarnation())
		if err != nil {
			return statedb.RuntimeState{}, err
		}
		next := current
		next.Generation++
		next.StatusRevision = 0
		next.TmuxSession = "web-undo-runtime"
		next.TmuxSocketName = "isolated"
		next.Status = string(session.StatusRunning)
		next.LastStartedAt = time.Unix(5, 0).UTC()
		if err := storage.GetDB().CommitRuntimeTransition(
			current.Generation, inst.PersistenceIncarnation(), next); err != nil {
			return statedb.RuntimeState{}, err
		}
		return next, nil
	}

	id, err := m.UndoDelete()
	if err != nil || id != remove.ID {
		t.Fatalf("UndoDelete = id %q, err %v", id, err)
	}
	canonical := h.instanceByID[id]
	if canonical == nil {
		t.Fatal("restored session missing from instanceByID")
	}
	for name, items := range map[string][]*session.Instance{
		"instances": h.instances,
		"search":    h.search.allItems,
	} {
		count := 0
		for _, inst := range items {
			if inst == canonical {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("canonical restored %d times in %s, want once", count, name)
		}
	}
	groupRefs, flatRefs := 0, 0
	for _, item := range h.groupTree.Flatten() {
		if item.Session == canonical {
			groupRefs++
		}
	}
	for _, item := range h.flatItems {
		if item.Session == canonical {
			flatRefs++
		}
	}
	if groupRefs != 1 || flatRefs != 1 {
		t.Fatalf("restored canonical refs: group=%d flat=%d, want one each", groupRefs, flatRefs)
	}
	if row, err := storage.GetDB().LoadInstanceByID(id); err != nil || row == nil {
		t.Fatalf("restored durable row = %#v, err=%v", row, err)
	}
	if runtime, found, err := storage.GetDB().ReadRuntimeState(id); err != nil || !found || runtime.Generation != 1 {
		t.Fatalf("restored runtime = %#v, found=%v err=%v", runtime, found, err)
	}
	restoredIncarnation := canonical.PersistenceIncarnation()
	if restoredIncarnation == "" || restoredIncarnation == deletedIncarnation {
		t.Fatalf("restored incarnation=%q, want fresh token different from deleted %q",
			restoredIncarnation, deletedIncarnation)
	}
	durable, found, err := storage.GetDB().ReadRuntimeState(id)
	if err != nil || !found {
		t.Fatalf("read restored runtime = %#v, found=%v err=%v", durable, found, err)
	}
	applied, err := storage.GetDB().WriteStatusIfVersion(
		id, deletedIncarnation, durable.Generation, durable.StatusRevision,
		string(session.StatusError))
	if !errors.Is(err, statedb.ErrInstanceParentConflict) || applied {
		t.Fatalf("stale pre-delete status write applied=%v err=%v, want parent conflict", applied, err)
	}
	afterStaleWrite, found, err := storage.GetDB().ReadRuntimeState(id)
	if err != nil || !found || afterStaleWrite != durable {
		t.Fatalf("runtime after stale write=%#v found=%v err=%v, want unchanged %#v",
			afterStaleWrite, found, err, durable)
	}
}

func TestRuntimeLifecycle_WebUndoDoesNotClobberConcurrentSameIDWinner(t *testing.T) {
	h, storage := newHeadlessHomeForTest(t, "_test_web_undo_same_id")
	remove := seedSession(t, storage, nil, "web-undo-same-id", "deleted snapshot")
	_ = seedSession(t, storage, []*session.Instance{remove}, "web-undo-survivor", "survivor")
	m := NewWebMutator(h)
	if err := m.DeleteSession(remove.ID); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}

	winner := &session.Instance{
		ID: remove.ID, Title: "concurrent winner", ProjectPath: "/tmp/winner",
		GroupPath: session.DefaultGroupPath, Tool: "pi", Status: session.StatusStopped,
		CreatedAt: time.Unix(9, 0).UTC(),
	}
	var winnerRuntime statedb.RuntimeState
	m.beforeUndoInsertFn = func() {
		if err := storage.InsertSessionAndVerify(winner, nil); err != nil {
			t.Fatalf("seed same-ID winner: %v", err)
		}
		var found bool
		var err error
		winnerRuntime, found, err = storage.GetDB().ReadRuntimeState(winner.ID)
		if err != nil || !found {
			t.Fatalf("winner runtime = %#v, found=%v err=%v", winnerRuntime, found, err)
		}
	}
	restartCalls := 0
	m.restartRuntimeFn = func(*session.Instance) (statedb.RuntimeState, error) {
		restartCalls++
		return statedb.RuntimeState{}, errors.New("stale undo must not restart")
	}

	if _, err := m.UndoDelete(); !errors.Is(err, session.ErrSessionAlreadyExists) {
		t.Fatalf("UndoDelete error = %v, want session already exists", err)
	}
	if restartCalls != 0 {
		t.Fatalf("stale undo attempted %d restarts", restartCalls)
	}
	row, err := storage.GetDB().LoadInstanceByID(winner.ID)
	if err != nil || row == nil || row.Title != winner.Title || row.ProjectPath != winner.ProjectPath {
		t.Fatalf("same-ID winner was clobbered: row=%#v err=%v", row, err)
	}
	afterRuntime, found, err := storage.GetDB().ReadRuntimeState(winner.ID)
	if err != nil || !found || afterRuntime != winnerRuntime {
		t.Fatalf("same-ID winner runtime = %#v, found=%v err=%v; want %#v",
			afterRuntime, found, err, winnerRuntime)
	}
}

func TestRuntimeLifecycle_WebCreateRollbackPreservesCompetingRuntimeTransition(t *testing.T) {
	h, storage := newHeadlessHomeForTest(t, "_test_web_create_rollback_transition")
	m := NewWebMutator(h)
	createdID := ""
	m.startRuntimeFn = func(inst *session.Instance) (statedb.RuntimeState, error) {
		createdID = inst.ID
		initial, found, err := storage.GetDB().ReadRuntimeState(inst.ID)
		if err != nil || !found {
			t.Fatalf("initial runtime = %#v, found=%v err=%v", initial, found, err)
		}
		winner := initial
		winner.Generation++
		winner.StatusRevision = 0
		winner.TmuxSession = "competing-winner"
		winner.Status = string(session.StatusRunning)
		if err := storage.GetDB().CommitRuntimeTransition(
			initial.Generation, inst.PersistenceIncarnation(), winner); err != nil {
			t.Fatal(err)
		}
		return statedb.RuntimeState{}, errors.New("original start failed after competing transition")
	}

	if _, err := m.CreateSession("seed", "shell", t.TempDir(), "work", "", ""); !errors.Is(err, statedb.ErrRuntimeGenerationConflict) {
		t.Fatalf("CreateSession error = %v, want rollback generation conflict", err)
	}
	if createdID == "" {
		t.Fatal("start callback did not observe created session")
	}
	if row, err := storage.GetDB().LoadInstanceByID(createdID); err != nil || row == nil {
		t.Fatalf("competing winner parent = %#v, err=%v", row, err)
	}
	if runtime, found, err := storage.GetDB().ReadRuntimeState(createdID); err != nil || !found ||
		runtime.Generation != 1 || runtime.TmuxSession != "competing-winner" {
		t.Fatalf("competing winner runtime = %#v, found=%v err=%v", runtime, found, err)
	}
}

// TestIssue1397_HeadlessDeleteUnknownStillErrors guards the inverse: deleting a
// genuinely non-existent id still fails (hydration must not paper over real
// not-found errors).
func TestIssue1397_HeadlessDeleteUnknownStillErrors(t *testing.T) {
	h, storage := newHeadlessHomeForTest(t, "_test_1397_unknown")
	_ = seedSession(t, storage, nil, "issue1397-keep-001", "keepme")

	m := NewWebMutator(h)
	if err := m.DeleteSession("does-not-exist"); err == nil {
		t.Fatal("deleting a non-existent session must still error")
	}
}

// TestIssue1397_HeadlessCreateGroupDoesNotTripGuard verifies that creating a
// group while sessions exist in storage does not trip the empty-SaveInstances
// data-loss guard. Pre-fix, persistAllInstances/SaveWithGroups ran with the
// empty in-memory list and returned 500 from ErrRefusingEmptySweep.
func TestIssue1397_HeadlessCreateGroupDoesNotTripGuard(t *testing.T) {
	h, storage := newHeadlessHomeForTest(t, "_test_1397_group")
	_ = seedSession(t, storage, nil, "issue1397-grp-001", "existing1")

	m := NewWebMutator(h)
	if _, err := m.CreateGroup("newgrp", ""); err != nil {
		t.Fatalf("CreateGroup must succeed in headless mode with existing sessions, got: %v", err)
	}

	// The pre-existing session must survive the group creation (not wiped).
	remaining, groups, err := storage.LoadWithGroups()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(remaining) != 1 {
		t.Fatalf("existing session must survive group creation, got %d sessions", len(remaining))
	}
	found := false
	for _, g := range groups {
		if g.Name == "newgrp" {
			found = true
		}
	}
	if !found {
		t.Fatalf("created group not persisted; groups=%+v", groups)
	}
}

// TestIssue1397_LiveModeDoesNotHydrate ensures the hydration path is gated on
// headless: in live-TUI mode (headless=false) beginHeadlessTx must be a no-op so
// it never races the bubbletea loop that owns the in-memory state.
func TestIssue1397_LiveModeDoesNotHydrate(t *testing.T) {
	storage, err := session.NewStorageWithProfile("_test_1397_live")
	if err != nil {
		t.Fatalf("NewStorageWithProfile: %v", err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	_ = seedSession(t, storage, nil, "issue1397-live-001", "onlyondisk")

	h := &Home{
		profile:      storage.Profile(),
		storage:      storage,
		instanceByID: make(map[string]*session.Instance),
		groupTree:    session.NewGroupTree(nil),
		headless:     false, // live TUI mode
	}
	m := NewWebMutator(h)

	// In live mode, beginHeadlessTx must NOT load from storage, so the on-disk
	// session stays invisible to the (empty) in-memory map.
	unlock, err := m.beginHeadlessTx()
	if err != nil {
		t.Fatalf("beginHeadlessTx should be a no-op in live mode, got: %v", err)
	}
	unlock()
	if len(h.instanceByID) != 0 {
		t.Fatalf("live mode must not hydrate; instanceByID has %d entries", len(h.instanceByID))
	}
}

// TestIssue1397_HeadlessConcurrentMutationsNoRace fires concurrent headless
// mutations (group creates + a delete) and asserts no data race and no lost
// data: the pre-existing session survives and the deleted one is gone. Run with
// -race to exercise the hydrate->mutate->persist serialization (#1397, Codex
// review point on concurrency).
func TestIssue1397_HeadlessConcurrentMutationsNoRace(t *testing.T) {
	h, storage := newHeadlessHomeForTest(t, "_test_1397_concurrent")
	keep := seedSession(t, storage, nil, "issue1397-keep-001", "keepme")
	_ = seedSession(t, storage, []*session.Instance{keep}, "issue1397-doomed-001", "doomed")

	m := NewWebMutator(h)

	const groups = 8
	var wg sync.WaitGroup
	errs := make(chan error, groups+1)

	for i := 0; i < groups; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			if _, err := m.CreateGroup(fmt.Sprintf("g%d", n), ""); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := m.DeleteSession("issue1397-doomed-001"); err != nil {
			errs <- err
		}
	}()

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent mutation error: %v", err)
	}

	remaining, grpList, err := storage.LoadWithGroups()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	// The kept session must survive all the churn; the doomed one must be gone.
	var sawKeep, sawDoomed bool
	for _, r := range remaining {
		switch r.ID {
		case "issue1397-keep-001":
			sawKeep = true
		case "issue1397-doomed-001":
			sawDoomed = true
		}
	}
	if !sawKeep {
		t.Error("kept session was lost under concurrent mutations")
	}
	if sawDoomed {
		t.Error("doomed session should have been deleted")
	}
	// Serialization guarantees no lost updates: ALL concurrently-created groups
	// must persist (a weaker >0 check would not catch a lost-update regression).
	created := 0
	for _, g := range grpList {
		if strings.HasPrefix(g.Name, "g") {
			created++
		}
	}
	if created != groups {
		t.Errorf("expected all %d concurrent groups to persist, got %d", groups, created)
	}
}
