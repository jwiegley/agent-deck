package main

import (
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// F5 on the legacy rollback path (AGENT_DECK_CORE_REGISTRY=0): a queued
// session whose start fails is durably errored one status revision after the
// runtime the drain read, so later stops do not retry it ahead of the rest of
// the queue. An in-memory status would be dropped by the snapshot save.
func TestLegacyDrainGroupQueueFailurePersistsErrorStatus(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, key := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME"} {
		t.Setenv(key, "")
	}
	session.ClearUserConfigCache()
	t.Cleanup(session.ClearUserConfigCache)

	const profile = "_test_legacy_drain_failure"
	queued := session.NewInstanceWithGroup("q", t.TempDir(), "g")
	queued.Status = session.StatusQueued
	// A named account slot with no configuration fails StartRuntime
	// deterministically, before any tmux work.
	queued.Account = "unconfigured-drain-slot"
	groups := []*session.GroupData{{Name: "g", Path: "g", MaxConcurrent: 1}}
	seed, err := session.NewStorageWithProfile(profile)
	if err != nil {
		t.Fatal(err)
	}
	seeded := []*session.Instance{queued}
	if err := seed.SaveWithGroups(seeded, session.NewGroupTreeWithGroups(seeded, groups)); err != nil {
		t.Fatal(err)
	}
	_ = seed.Close()

	storage, err := session.NewStorageWithProfile(profile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	instances, loadedGroups, err := storage.LoadWithGroups()
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != 1 || instances[0].ID != queued.ID || instances[0].Status != session.StatusQueued {
		t.Fatalf("seeded store = %+v, want the one queued session", instances)
	}
	before := instances[0].RuntimeState()

	drained, warning := drainGroupQueue(storage, "g", instances, loadedGroups)
	if drained != nil || warning != "" {
		t.Fatalf("drained %v (%q) after a failed start", drained, warning)
	}
	// handleSessionStop's snapshot save follows the drain.
	if err := saveSessionData(storage, instances, loadedGroups); err != nil {
		t.Fatal(err)
	}
	want := before
	want.Status = string(session.StatusError)
	want.StatusRevision++
	got, found, err := storage.GetDB().ReadRuntimeState(queued.ID)
	if err != nil || !found || got != want {
		t.Fatalf("durable runtime = %+v, found=%v err=%v; want %+v", got, found, err, want)
	}
	reloaded, _, err := storage.LoadWithGroups()
	if err != nil {
		t.Fatal(err)
	}
	if next := session.FindNextQueued(reloaded, "g"); next != nil {
		t.Fatalf("failed session %s is still queued", next.Title)
	}
}
