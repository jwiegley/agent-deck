package statedb

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newRuntimeTestDB(t *testing.T) *StateDB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(); err != nil {
		db.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func runtimeTestIncarnation(t *testing.T, db *StateDB, id string) string {
	t.Helper()
	row, err := db.LoadInstanceByID(id)
	if err != nil || row == nil || row.Incarnation == "" {
		t.Fatalf("load incarnation for %s: row=%#v err=%v", id, row, err)
	}
	return row.Incarnation
}

type runtimeABASnapshot struct {
	instanceID   string
	incarnationA string
	incarnationB string
	runtime      RuntimeState
	binding      RuntimeBinding
}

func newByteIdenticalRuntimeABA(t *testing.T) (*StateDB, runtimeABASnapshot) {
	t.Helper()
	db := newRuntimeTestDB(t)
	const (
		instanceID   = "byte-identical-aba"
		incarnationA = "byte-identical-incarnation-a"
		incarnationB = "byte-identical-incarnation-b"
	)
	makeRow := func(incarnation string) *InstanceRow {
		return &InstanceRow{
			ID: instanceID, Incarnation: incarnation, Title: "same bytes",
			ProjectPath: "/tmp/byte-identical", GroupPath: "my-sessions",
			Tool: "claude", Status: "running", TmuxSession: "same-runtime-g1",
			TmuxSocketName: "isolated", CreatedAt: time.Unix(100, 0).UTC(),
			LastAccessed: time.Unix(101, 0).UTC(), RuntimeGeneration: 1,
			StatusRevision: 3, LastStartedAt: time.Unix(200, 123).UTC(),
			ToolData: json.RawMessage(`{"notes":"byte-identical"}`),
			RuntimeBindings: map[string]RuntimeBinding{
				"claude": {
					InstanceID: instanceID, Kind: "claude", Generation: 1,
					Revision: 2, Value: "same-conversation",
					DetectedAt: time.Unix(190, 456).UTC(),
				},
			},
		}
	}

	first, inserted, err := db.InsertInstanceIfAbsent(makeRow(incarnationA))
	if err != nil || !inserted {
		t.Fatalf("insert A: seed=%#v inserted=%v err=%v", first, inserted, err)
	}
	if err := db.DeleteInstance(instanceID); err != nil {
		t.Fatal(err)
	}
	second, inserted, err := db.InsertInstanceIfAbsent(makeRow(incarnationB))
	if err != nil || !inserted {
		t.Fatalf("insert byte-identical B: seed=%#v inserted=%v err=%v", second, inserted, err)
	}
	if first.parent != second.parent || first.Runtime != second.Runtime ||
		first.Bindings["claude"] != second.Bindings["claude"] {
		t.Fatalf("A/B persisted bytes differ: A=%#v B=%#v", first, second)
	}
	if first.Incarnation != incarnationA || second.Incarnation != incarnationB || first.Incarnation == second.Incarnation {
		t.Fatalf("A/B incarnations = %q/%q", first.Incarnation, second.Incarnation)
	}
	return db, runtimeABASnapshot{
		instanceID: instanceID, incarnationA: incarnationA, incarnationB: incarnationB,
		runtime: second.Runtime, binding: second.Bindings["claude"],
	}
}

func TestRuntimeLifecycle_EnsureRuntimeStateRequiresLogicalInstance(t *testing.T) {
	db := newRuntimeTestDB(t)
	initial := RuntimeState{
		InstanceID: "deleted-before-runtime", TmuxSession: "orphan-runtime",
		TmuxSocketName: "isolated", Status: "starting",
	}

	if _, err := db.EnsureRuntimeState(initial, "removed-parent-token"); !errors.Is(err, ErrInstanceParentConflict) {
		t.Fatalf("EnsureRuntimeState error = %v, want parent conflict", err)
	}
	if got, found, err := db.ReadRuntimeState(initial.InstanceID); err != nil || found {
		t.Fatalf("orphan runtime = %#v, found=%v err=%v", got, found, err)
	}
}

func TestRuntimeLifecycle_UpdateInstancesDoesNotInitializeMissingRuntime(t *testing.T) {
	db := newRuntimeTestDB(t)
	if _, err := db.DB().Exec(`
		INSERT INTO instances (id, title, project_path, group_path, tool, status, created_at)
		VALUES ('metadata-only', 'before', '/tmp/one', 'my-sessions', 'claude', 'idle', 100)`); err != nil {
		t.Fatal(err)
	}
	persisted, err := db.LoadInstanceByID("metadata-only")
	if err != nil || persisted == nil {
		t.Fatalf("load parent token: row=%#v err=%v", persisted, err)
	}
	row := &InstanceRow{
		ID: "metadata-only", Incarnation: persisted.Incarnation,
		Title: "after", ProjectPath: "/tmp/one", GroupPath: "my-sessions",
		Tool: "claude", Status: "running", CreatedAt: time.Unix(100, 0).UTC(),
		RuntimeGeneration: 9,
		RuntimeBindings: map[string]RuntimeBinding{
			"claude": {InstanceID: "metadata-only", Kind: "claude", Generation: 9, Value: "stale"},
		},
	}
	if err := db.UpdateInstances([]*InstanceRow{row}); err != nil {
		t.Fatal(err)
	}
	loaded, err := db.LoadInstanceByID(row.ID)
	if err != nil || loaded == nil || loaded.Title != "after" {
		t.Fatalf("updated parent = %#v, err=%v", loaded, err)
	}
	if runtime, found, err := db.ReadRuntimeState(row.ID); err != nil || found {
		t.Fatalf("runtime after update-only save = %#v, found=%v err=%v", runtime, found, err)
	}
	var bindings int
	if err := db.DB().QueryRow(`SELECT COUNT(*) FROM instance_runtime_binding WHERE instance_id = ?`, row.ID).Scan(&bindings); err != nil {
		t.Fatal(err)
	}
	if bindings != 0 {
		t.Fatalf("bindings after update-only save = %d, want 0", bindings)
	}
}

func TestRuntimeLifecycle_UpdateInstancesRejectsStaleIncarnationAfterDeleteReinsert(t *testing.T) {
	db := newRuntimeTestDB(t)
	created := time.Unix(100, 0).UTC()
	original := &InstanceRow{
		ID: "same-id-aba", Incarnation: "same-id-incarnation-a", Title: "same bytes", ProjectPath: "/tmp/aba",
		GroupPath: "my-sessions", Tool: "pi", Status: "stopped", CreatedAt: created,
		ToolData: json.RawMessage(`{"notes":"unchanged"}`),
	}
	firstSeed, inserted, err := db.InsertInstanceIfAbsent(original)
	if err != nil || !inserted {
		t.Fatalf("insert original: seed=%#v inserted=%v err=%v", firstSeed, inserted, err)
	}
	stale, err := db.LoadInstanceByID(original.ID)
	if err != nil || stale == nil || stale.Incarnation == "" {
		t.Fatalf("load stale row: row=%#v err=%v", stale, err)
	}
	firstIncarnation := stale.Incarnation

	if err := db.DeleteInstance(original.ID); err != nil {
		t.Fatal(err)
	}
	winner := &InstanceRow{
		ID: original.ID, Incarnation: "same-id-incarnation-b", Title: original.Title, ProjectPath: original.ProjectPath,
		GroupPath: original.GroupPath, Tool: original.Tool, Status: original.Status,
		CreatedAt: original.CreatedAt, ToolData: append(json.RawMessage(nil), original.ToolData...),
	}
	winnerSeed, inserted, err := db.InsertInstanceIfAbsent(winner)
	if err != nil || !inserted {
		t.Fatalf("insert byte-identical winner: seed=%#v inserted=%v err=%v", winnerSeed, inserted, err)
	}
	if winnerSeed.Incarnation == "" || winnerSeed.Incarnation == firstIncarnation {
		t.Fatalf("winner incarnation = %q, first = %q", winnerSeed.Incarnation, firstIncarnation)
	}

	stale.Title = "stale writer must not win"
	if err := db.UpdateInstances([]*InstanceRow{stale}); err != nil {
		t.Fatal(err)
	}
	got, err := db.LoadInstanceByID(original.ID)
	if err != nil || got == nil {
		t.Fatalf("load winner: row=%#v err=%v", got, err)
	}
	if got.Title != original.Title || got.Incarnation != winnerSeed.Incarnation {
		t.Fatalf("stale ABA update changed winner: %#v", got)
	}
	runtime, found, err := db.ReadRuntimeState(original.ID)
	if err != nil || !found || runtime != winnerSeed.Runtime {
		t.Fatalf("winner runtime = %#v found=%v err=%v; want %#v", runtime, found, err, winnerSeed.Runtime)
	}
}

func TestRuntimeLifecycle_InstanceIncarnationRejectsByteIdenticalABA(t *testing.T) {
	t.Run("transition", func(t *testing.T) {
		db, aba := newByteIdenticalRuntimeABA(t)
		next := aba.runtime
		next.Generation++
		next.TmuxSession = "stale-a-must-not-start"
		next.Status = "starting"
		plan := []RuntimeBindingTransition{{
			Kind: "claude", ExpectedRevision: aba.binding.Revision,
			NextValue: aba.binding.Value, DetectedAt: aba.binding.DetectedAt,
		}}
		if err := db.CommitRuntimeTransitionWithBindingPlan(
			aba.runtime.Generation, aba.incarnationA, next, plan,
		); !errors.Is(err, ErrInstanceParentConflict) {
			t.Fatalf("binding-plan transition error = %v, want parent conflict", err)
		}
		if err := db.CommitRuntimeTransition(
			aba.runtime.Generation, aba.incarnationA, next,
		); !errors.Is(err, ErrInstanceParentConflict) {
			t.Fatalf("plain transition error = %v, want parent conflict", err)
		}
		state, found, err := db.ReadRuntimeState(aba.instanceID)
		if err != nil || !found || state != aba.runtime {
			t.Fatalf("B runtime = %#v found=%v err=%v; want %#v", state, found, err, aba.runtime)
		}
		binding, found, err := db.ReadRuntimeBinding(aba.instanceID, "claude")
		if err != nil || !found || binding != aba.binding {
			t.Fatalf("B binding = %#v found=%v err=%v; want %#v", binding, found, err, aba.binding)
		}
	})

	t.Run("status", func(t *testing.T) {
		db, aba := newByteIdenticalRuntimeABA(t)
		applied, err := db.WriteStatusIfVersion(
			aba.instanceID, aba.incarnationA, aba.runtime.Generation,
			aba.runtime.StatusRevision, "waiting",
		)
		if applied || !errors.Is(err, ErrInstanceParentConflict) {
			t.Fatalf("stale A status applied=%v err=%v, want parent conflict", applied, err)
		}
		state, found, err := db.ReadRuntimeState(aba.instanceID)
		if err != nil || !found || state != aba.runtime {
			t.Fatalf("B runtime = %#v found=%v err=%v; want %#v", state, found, err, aba.runtime)
		}
	})

	t.Run("binding", func(t *testing.T) {
		db, aba := newByteIdenticalRuntimeABA(t)
		if _, err := db.CommitRuntimeBinding(
			aba.instanceID, aba.incarnationA, aba.runtime.Generation,
			"claude", aba.binding.Revision, "stale-a-binding",
		); !errors.Is(err, ErrInstanceParentConflict) {
			t.Fatalf("stale A binding commit error = %v, want parent conflict", err)
		}
		if _, applied, err := db.WriteRuntimeBindingIfVersion(
			aba.instanceID, aba.incarnationA, aba.runtime.Generation,
			"claude", aba.binding.Revision, "stale-a-observation", time.Unix(300, 0).UTC(),
		); applied || !errors.Is(err, ErrInstanceParentConflict) {
			t.Fatalf("stale A binding observation applied=%v err=%v, want parent conflict", applied, err)
		}
		binding, found, err := db.ReadRuntimeBinding(aba.instanceID, "claude")
		if err != nil || !found || binding != aba.binding {
			t.Fatalf("B binding = %#v found=%v err=%v; want %#v", binding, found, err, aba.binding)
		}
	})

	t.Run("archive", func(t *testing.T) {
		db, aba := newByteIdenticalRuntimeABA(t)
		if err := db.SetArchivedIfRuntime(
			aba.runtime, aba.incarnationA, time.Unix(400, 0).UTC(),
		); !errors.Is(err, ErrInstanceParentConflict) {
			t.Fatalf("stale A archive error = %v, want parent conflict", err)
		}
		row, err := db.LoadInstanceByID(aba.instanceID)
		if err != nil || row == nil || !row.ArchivedAt.IsZero() || row.Incarnation != aba.incarnationB {
			t.Fatalf("B parent after stale archive = %#v err=%v", row, err)
		}
	})

	t.Run("unarchive", func(t *testing.T) {
		db, aba := newByteIdenticalRuntimeABA(t)
		archivedAt := time.Unix(400, 0).UTC()
		if err := db.SetArchivedIfIncarnation(aba.instanceID, aba.incarnationB, archivedAt); err != nil {
			t.Fatal(err)
		}
		if err := db.SetArchivedIfIncarnation(
			aba.instanceID, aba.incarnationA, time.Time{},
		); !errors.Is(err, ErrInstanceParentConflict) {
			t.Fatalf("stale A unarchive error = %v, want parent conflict", err)
		}
		row, err := db.LoadInstanceByID(aba.instanceID)
		if err != nil || row == nil || !row.ArchivedAt.Equal(archivedAt) || row.Incarnation != aba.incarnationB {
			t.Fatalf("B parent after stale unarchive = %#v err=%v", row, err)
		}
	})

	t.Run("binding owner lease", func(t *testing.T) {
		db, aba := newByteIdenticalRuntimeABA(t)
		ran := false
		leased, err := db.WithRuntimeBindingOwnerLease(
			aba.runtime, aba.incarnationA, aba.binding, func() { ran = true },
		)
		if leased || ran || !errors.Is(err, ErrInstanceParentConflict) {
			t.Fatalf("stale A lease leased=%v ran=%v err=%v, want parent conflict", leased, ran, err)
		}
	})

	t.Run("runtime owner lease", func(t *testing.T) {
		db, aba := newByteIdenticalRuntimeABA(t)
		ran := false
		leased, err := db.WithRuntimeOwnerLease(
			aba.runtime, aba.incarnationA, func() { ran = true },
		)
		if leased || ran || !errors.Is(err, ErrInstanceParentConflict) {
			t.Fatalf("stale A lease leased=%v ran=%v err=%v, want parent conflict", leased, ran, err)
		}
		leased, err = db.WithRuntimeOwnerLease(
			aba.runtime, aba.incarnationB, func() { ran = true },
		)
		if err != nil || !leased || !ran {
			t.Fatalf("current B lease leased=%v ran=%v err=%v, want action", leased, ran, err)
		}
	})

	t.Run("ensure runtime", func(t *testing.T) {
		db, aba := newByteIdenticalRuntimeABA(t)
		if _, err := db.DB().Exec(`DELETE FROM instance_runtime_binding WHERE instance_id = ?`, aba.instanceID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.DB().Exec(`DELETE FROM instance_runtime_state WHERE instance_id = ?`, aba.instanceID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.EnsureRuntimeState(aba.runtime, aba.incarnationA); !errors.Is(err, ErrInstanceParentConflict) {
			t.Fatalf("stale A ensure error = %v, want parent conflict", err)
		}
		if state, found, err := db.ReadRuntimeState(aba.instanceID); err != nil || found {
			t.Fatalf("stale A initialized B runtime = %#v found=%v err=%v", state, found, err)
		}
		if state, err := db.EnsureRuntimeState(aba.runtime, aba.incarnationB); err != nil || state != aba.runtime {
			t.Fatalf("B ensure state=%#v err=%v, want %#v", state, err, aba.runtime)
		}
	})
}

func TestRuntimeLifecycle_InsertInstanceIfAbsentAdoptsPreParentRuntimeAndBindings(t *testing.T) {
	db := newRuntimeTestDB(t)
	started := time.Unix(200, 123).UTC()
	authoritative := RuntimeState{
		InstanceID: "pre-parent", Generation: 1, StatusRevision: 4,
		TmuxSession: "runtime-g1", TmuxSocketName: "socket-g1",
		Status: "running", LastStartedAt: started,
	}
	const preParentIncarnation = "pre-parent-incarnation"
	if got, err := db.EnsureRuntimeStateForNewInstance(authoritative, preParentIncarnation); err != nil || got != authoritative {
		t.Fatalf("publish pre-parent runtime = %#v err=%v; want %#v", got, err, authoritative)
	}
	if _, err := db.EnsureRuntimeStateForNewInstance(authoritative, "competing-pre-parent-incarnation"); !errors.Is(err, ErrInstanceParentConflict) {
		t.Fatalf("competing pre-parent publisher error = %v, want parent conflict", err)
	}
	if got, found, err := db.ReadRuntimeState(authoritative.InstanceID); err != nil || !found || got != authoritative {
		t.Fatalf("competing publisher changed runtime = %#v found=%v err=%v", got, found, err)
	}
	binding, err := db.CommitRuntimeBinding(authoritative.InstanceID, preParentIncarnation, authoritative.Generation, "claude", 0, "conversation-g1")
	if err != nil {
		t.Fatal(err)
	}
	candidate := &InstanceRow{
		ID: authoritative.InstanceID, Incarnation: preParentIncarnation, Title: "new parent", ProjectPath: "/tmp/pre-parent",
		GroupPath: "my-sessions", Tool: "claude", Status: "stopped",
		TmuxSession: "stale-candidate", CreatedAt: time.Unix(100, 0).UTC(),
		RuntimeBindings: map[string]RuntimeBinding{
			"claude": {InstanceID: authoritative.InstanceID, Kind: "claude", Value: "stale-candidate"},
		},
	}
	seed, inserted, err := db.InsertInstanceIfAbsent(candidate)
	if err != nil || !inserted {
		t.Fatalf("insert parent: seed=%#v inserted=%v err=%v", seed, inserted, err)
	}
	if seed.Runtime != authoritative {
		t.Fatalf("adopted runtime = %#v, want %#v", seed.Runtime, authoritative)
	}
	if got := seed.Bindings["claude"]; got != binding {
		t.Fatalf("adopted binding = %#v, want %#v", got, binding)
	}
	if got, found, err := db.ReadRuntimeState(candidate.ID); err != nil || !found || got != authoritative {
		t.Fatalf("durable runtime = %#v found=%v err=%v; want %#v", got, found, err, authoritative)
	}
	if got, found, err := db.ReadRuntimeBinding(candidate.ID, "claude"); err != nil || !found || got != binding {
		t.Fatalf("durable binding = %#v found=%v err=%v; want %#v", got, found, err, binding)
	}
	loaded, err := db.LoadInstanceByID(candidate.ID)
	if err != nil || loaded == nil {
		t.Fatalf("loaded adopted parent = %#v err=%v", loaded, err)
	}
	if seed.Incarnation != preParentIncarnation ||
		candidate.Incarnation != preParentIncarnation ||
		loaded.Incarnation != preParentIncarnation {
		t.Fatalf("pre-minted incarnation changed across parent insert: seed=%q candidate=%q loaded=%q",
			seed.Incarnation, candidate.Incarnation, loaded.Incarnation)
	}
	if loaded.RuntimeBindings["claude"] != binding {
		t.Fatalf("loaded adopted binding = %#v, want %#v", loaded.RuntimeBindings["claude"], binding)
	}
}

func TestRuntimeLifecycle_StaleMetadataSaveCannotRevertRuntimeOrBinding(t *testing.T) {
	db := newRuntimeTestDB(t)
	created := time.Unix(100, 0).UTC()
	row := &InstanceRow{
		ID: "one", Title: "before", ProjectPath: "/tmp/one", GroupPath: "my-sessions",
		Tool: "claude", Status: "idle", TmuxSession: "tmux-g0", CreatedAt: created,
		ToolData: json.RawMessage(`{"claude_session_id":"conversation-g0","notes":"before"}`),
	}
	if err := db.SaveInstance(row); err != nil {
		t.Fatal(err)
	}

	started := time.Unix(200, 123).UTC()
	next := RuntimeState{
		InstanceID: "one", Generation: 1, StatusRevision: 0,
		TmuxSession: "tmux-g1", TmuxSocketName: "isolated", Status: "starting", LastStartedAt: started,
	}
	incarnation := runtimeTestIncarnation(t, db, "one")
	if err := db.CommitRuntimeTransitionWithBindingPlan(0, incarnation, next, []RuntimeBindingTransition{
		{Kind: "claude", ExpectedRevision: 0, NextValue: "conversation-g0"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CommitRuntimeBinding("one", incarnation, 1, "claude", 1, "conversation-g1"); err != nil {
		t.Fatal(err)
	}

	// The stale snapshot changes only metadata but still carries every G0
	// runtime-looking field. Its ordinary save must not own those fields.
	row.Title = "after"
	row.Status = "error"
	row.TmuxSession = "tmux-g0"
	row.TmuxSocketName = ""
	row.ToolData = json.RawMessage(`{"claude_session_id":"conversation-g0","notes":"after"}`)
	if err := db.UpsertInstances([]*InstanceRow{row}); err != nil {
		t.Fatal(err)
	}

	loaded, err := db.LoadInstances()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 {
		t.Fatalf("loaded %d rows, want 1", len(loaded))
	}
	got := loaded[0]
	if got.Title != "after" || got.RuntimeGeneration != 1 || got.TmuxSession != "tmux-g1" ||
		got.TmuxSocketName != "isolated" || got.Status != "starting" || !got.LastStartedAt.Equal(started) {
		t.Fatalf("stale save changed runtime or lost metadata: %#v", got)
	}
	var toolData map[string]json.RawMessage
	if err := json.Unmarshal(got.ToolData, &toolData); err != nil {
		t.Fatal(err)
	}
	if string(toolData["claude_session_id"]) != `"conversation-g1"` || string(toolData["notes"]) != `"after"` {
		t.Fatalf("tool_data = %s", got.ToolData)
	}
}

func TestRuntimeLifecycle_BindingCASBeforeMetadataWriteTransactionWins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	if err := writer.Migrate(); err != nil {
		t.Fatal(err)
	}
	binder, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = binder.Close() })
	if err := binder.Migrate(); err != nil {
		t.Fatal(err)
	}
	stale := &InstanceRow{
		ID: "one", Title: "stale", ProjectPath: "/tmp/one", GroupPath: "my-sessions",
		Tool: "claude", Status: "idle", CreatedAt: time.Unix(1, 0),
		ToolData: json.RawMessage(`{"claude_session_id":"old","notes":"stale"}`),
	}
	if err := writer.SaveInstance(stale); err != nil {
		t.Fatal(err)
	}
	beforeTransaction := make(chan struct{})
	release := make(chan struct{})
	writer.testBeforeInstanceWriteTransaction = func() {
		close(beforeTransaction)
		<-release
	}
	saveDone := make(chan error, 1)
	go func() { saveDone <- writer.SaveInstance(stale) }()
	<-beforeTransaction
	detectedAt := time.Unix(44, 0).UTC()
	if _, applied, err := binder.WriteRuntimeBindingIfVersion("one", runtimeTestIncarnation(t, binder, "one"), 0, "claude", 0, "new", detectedAt); err != nil || !applied {
		t.Fatalf("binding CAS applied=%v err=%v", applied, err)
	}
	close(release)
	if err := <-saveDone; err != nil {
		t.Fatal(err)
	}
	writer.testBeforeInstanceWriteTransaction = nil
	var raw string
	if err := writer.DB().QueryRow(`SELECT tool_data FROM instances WHERE id = 'one'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var projected map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &projected); err != nil {
		t.Fatal(err)
	}
	if string(projected["claude_session_id"]) != `"new"` || string(projected["claude_detected_at"]) != "44" {
		t.Fatalf("legacy projection regressed: %s", raw)
	}
}

func TestRuntimeLifecycle_MetadataSavePreservesConcurrentToolDataExtra(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	if err := writer.Migrate(); err != nil {
		t.Fatal(err)
	}
	targeted, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = targeted.Close() })
	if err := targeted.Migrate(); err != nil {
		t.Fatal(err)
	}
	seed := &InstanceRow{
		ID: "extras", Title: "before", ProjectPath: "/tmp/extras", GroupPath: "my-sessions",
		Tool: "pi", Status: "idle", CreatedAt: time.Unix(1, 0),
		ToolData: json.RawMessage(`{"clear_on_compact":false,"notes":"before"}`),
	}
	if err := writer.SaveInstance(seed); err != nil {
		t.Fatal(err)
	}
	stale, err := writer.LoadInstanceByID(seed.ID)
	if err != nil || stale == nil {
		t.Fatalf("load stale row: row=%#v err=%v", stale, err)
	}
	stale.Title = "after"
	stale.ToolData = json.RawMessage(`{"notes":"after"}`)

	beforeTransaction := make(chan struct{})
	release := make(chan struct{})
	writer.testBeforeInstanceWriteTransaction = func() {
		close(beforeTransaction)
		<-release
	}
	saveDone := make(chan error, 1)
	go func() { saveDone <- writer.UpdateInstances([]*InstanceRow{stale}) }()
	<-beforeTransaction
	if _, err := targeted.DB().Exec(`
		UPDATE instances
		SET tool_data = json_set(tool_data, '$.clear_on_compact', json('true'))
		WHERE id = ?`, seed.ID); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-saveDone; err != nil {
		t.Fatal(err)
	}
	writer.testBeforeInstanceWriteTransaction = nil

	got, err := writer.LoadInstanceByID(seed.ID)
	if err != nil || got == nil {
		t.Fatalf("load saved row: row=%#v err=%v", got, err)
	}
	if got.Title != "after" {
		t.Fatalf("routine metadata did not save: title=%q", got.Title)
	}
	var toolData map[string]json.RawMessage
	if err := json.Unmarshal(got.ToolData, &toolData); err != nil {
		t.Fatal(err)
	}
	if string(toolData["clear_on_compact"]) != "true" || string(toolData["notes"]) != `"after"` {
		t.Fatalf("tool_data lost concurrent extra or metadata: %s", got.ToolData)
	}
}

func TestRuntimeLifecycle_GenerationAndStatusCAS(t *testing.T) {
	db := newRuntimeTestDB(t)
	row := &InstanceRow{ID: "one", Title: "one", ProjectPath: "/tmp/one", GroupPath: "my-sessions", Tool: "pi", Status: "idle", CreatedAt: time.Unix(1, 0)}
	if err := db.SaveInstance(row); err != nil {
		t.Fatal(err)
	}
	clock := time.Unix(300, 0).UTC()
	incarnation := runtimeTestIncarnation(t, db, "one")
	for generation := uint64(1); generation <= 2; generation++ {
		if err := db.CommitRuntimeTransition(generation-1, incarnation, RuntimeState{
			InstanceID: "one", Generation: generation, TmuxSession: "tmux", Status: "starting", LastStartedAt: clock,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if applied, err := db.WriteStatusIfVersion("one", incarnation, 1, 0, "error"); err != nil || applied {
		t.Fatalf("stale generation applied=%v err=%v", applied, err)
	}
	if applied, err := db.WriteStatusIfVersion("one", incarnation, 2, 0, "running"); err != nil || !applied {
		t.Fatalf("current status applied=%v err=%v", applied, err)
	}
	if applied, err := db.WriteStatusIfVersion("one", incarnation, 2, 0, "error"); err != nil || applied {
		t.Fatalf("stale revision applied=%v err=%v", applied, err)
	}
	state, found, err := db.ReadRuntimeState("one")
	if err != nil || !found || state.Generation != 2 || state.StatusRevision != 1 || state.Status != "running" {
		t.Fatalf("state=%#v found=%v err=%v", state, found, err)
	}
}

func TestRuntimeLifecycle_StatusCASClearsAcknowledgedOnActivity(t *testing.T) {
	db := newRuntimeTestDB(t)
	if err := db.SaveInstance(&InstanceRow{
		ID: "one", Title: "one", ProjectPath: "/tmp/one", GroupPath: "my-sessions",
		Tool: "pi", Status: "idle", CreatedAt: time.Unix(1, 0),
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetAcknowledged("one", true); err != nil {
		t.Fatal(err)
	}
	state, found, err := db.ReadRuntimeState("one")
	if err != nil || !found {
		t.Fatalf("ReadRuntimeState found=%v err=%v", found, err)
	}
	incarnation := runtimeTestIncarnation(t, db, "one")
	if applied, err := db.WriteStatusIfVersion("one", incarnation, state.Generation, state.StatusRevision, "running"); err != nil || !applied {
		t.Fatalf("running status applied=%v err=%v", applied, err)
	}
	statuses, err := db.ReadAllStatuses()
	if err != nil {
		t.Fatal(err)
	}
	if statuses["one"].Acknowledged {
		t.Fatal("running activity retained stale acknowledgment")
	}
	if applied, err := db.WriteStatusIfVersion("one", incarnation, state.Generation, state.StatusRevision+1, "waiting"); err != nil || !applied {
		t.Fatalf("waiting status applied=%v err=%v", applied, err)
	}
	statuses, err = db.ReadAllStatuses()
	if err != nil {
		t.Fatal(err)
	}
	if statuses["one"].Status != "waiting" || statuses["one"].Acknowledged {
		t.Fatalf("post-activity waiting status = %#v", statuses["one"])
	}
}

func TestRuntimeLifecycle_ArchiveCASRejectsReplacementRuntime(t *testing.T) {
	db := newRuntimeTestDB(t)
	if err := db.SaveInstance(&InstanceRow{
		ID: "one", Title: "one", ProjectPath: "/tmp/one", GroupPath: "my-sessions",
		Tool: "pi", Status: "idle", CreatedAt: time.Unix(1, 0),
	}); err != nil {
		t.Fatal(err)
	}
	killed := RuntimeState{
		InstanceID: "one", Generation: 1, StatusRevision: 2,
		TmuxSession: "runtime-g1", TmuxSocketName: "isolated", Status: "stopped",
		LastStartedAt: time.Unix(2, 123).UTC(),
	}
	incarnation := runtimeTestIncarnation(t, db, "one")
	if err := db.CommitRuntimeTransition(0, incarnation, killed); err != nil {
		t.Fatal(err)
	}
	replacement := RuntimeState{
		InstanceID: "one", Generation: 2,
		TmuxSession: "runtime-g2", TmuxSocketName: "isolated", Status: "running",
		LastStartedAt: time.Unix(3, 456).UTC(),
	}
	if err := db.CommitRuntimeTransition(killed.Generation, incarnation, replacement); err != nil {
		t.Fatal(err)
	}
	archivedAt := time.Unix(10, 0).UTC()
	if err := db.SetArchivedIfRuntime(killed, incarnation, archivedAt); !errors.Is(err, ErrRuntimeGenerationConflict) {
		t.Fatalf("stale archive error = %v, want generation conflict", err)
	}
	row, err := db.LoadInstanceByID("one")
	if err != nil || row == nil {
		t.Fatalf("LoadInstanceByID row=%#v err=%v", row, err)
	}
	if !row.ArchivedAt.IsZero() {
		t.Fatalf("replacement runtime was archived at %v", row.ArchivedAt)
	}
	if err := db.SetArchivedIfRuntime(replacement, incarnation, archivedAt); err != nil {
		t.Fatalf("current archive: %v", err)
	}
	row, err = db.LoadInstanceByID("one")
	if err != nil || row == nil || !row.ArchivedAt.Equal(archivedAt) {
		t.Fatalf("current archive row=%#v err=%v", row, err)
	}
}

func TestRuntimeLifecycle_BindingRevisionAndUniqueOwner(t *testing.T) {
	db := newRuntimeTestDB(t)
	for _, id := range []string{"one", "two"} {
		if err := db.SaveInstance(&InstanceRow{ID: id, Title: id, ProjectPath: "/tmp/" + id, GroupPath: "my-sessions", Tool: "claude", Status: "idle", CreatedAt: time.Unix(1, 0)}); err != nil {
			t.Fatal(err)
		}
	}
	oneIncarnation := runtimeTestIncarnation(t, db, "one")
	twoIncarnation := runtimeTestIncarnation(t, db, "two")
	if _, err := db.CommitRuntimeBinding("one", oneIncarnation, 0, "claude", 0, "conversation"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CommitRuntimeBinding("one", oneIncarnation, 0, "claude", 0, "stale"); !errors.Is(err, ErrBindingRevisionConflict) {
		t.Fatalf("stale revision err=%v", err)
	}
	if _, err := db.CommitRuntimeBinding("two", twoIncarnation, 0, "claude", 0, "conversation"); err == nil {
		t.Fatal("duplicate owner succeeded")
	}
	if _, err := db.CommitRuntimeBinding("one", oneIncarnation, 0, "claude", 1, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CommitRuntimeBinding("two", twoIncarnation, 0, "claude", 0, "conversation"); err != nil {
		t.Fatalf("claim after release: %v", err)
	}
}

func TestRuntimeLifecycle_FutureSchemaRejectedBeforeMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.DB().Exec(`CREATE TABLE metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().Exec(`INSERT INTO metadata(key, value) VALUES ('schema_version', ?)`, SchemaVersion+1); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(); !errors.Is(err, ErrFutureSchema) {
		t.Fatalf("Migrate error = %v, want ErrFutureSchema", err)
	}
	var created int
	if err := db.DB().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='instances'`).Scan(&created); err != nil {
		t.Fatal(err)
	}
	if created != 0 {
		t.Fatal("future-schema rejection mutated database")
	}
}

func TestRuntimeLifecycle_ConcurrentFutureSchemaCannotBeDowngraded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.DB().Exec(`CREATE TABLE metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().Exec(`INSERT INTO metadata(key, value) VALUES ('schema_version', ?)`, SchemaVersion-1); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	future, err := db.DB().Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer future.Close()
	if _, err := future.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = future.ExecContext(ctx, `ROLLBACK`)
		}
	}()

	beforeBegin := make(chan struct{})
	releaseBegin := make(chan struct{})
	var barrier sync.Once
	db.testBeforeMigrationBegin = func() {
		barrier.Do(func() {
			close(beforeBegin)
			<-releaseBegin
		})
	}
	migrationDone := make(chan error, 1)
	go func() { migrationDone <- db.Migrate() }()
	<-beforeBegin
	close(releaseBegin)

	if _, err := future.ExecContext(ctx, `UPDATE metadata SET value = ? WHERE key = 'schema_version'`, SchemaVersion+1); err != nil {
		t.Fatal(err)
	}
	if _, err := future.ExecContext(ctx, `CREATE TABLE future_only (value TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := future.ExecContext(ctx, `INSERT INTO future_only(value) VALUES ('preserve')`); err != nil {
		t.Fatal(err)
	}
	if _, err := future.ExecContext(ctx, `COMMIT`); err != nil {
		t.Fatal(err)
	}
	committed = true

	if err := <-migrationDone; !errors.Is(err, ErrFutureSchema) {
		t.Fatalf("Migrate error = %v, want ErrFutureSchema", err)
	}
	db.testBeforeMigrationBegin = nil

	var version int
	if err := db.DB().QueryRow(`SELECT CAST(value AS INTEGER) FROM metadata WHERE key = 'schema_version'`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersion+1 {
		t.Fatalf("schema version = %d, want %d", version, SchemaVersion+1)
	}
	var value string
	if err := db.DB().QueryRow(`SELECT value FROM future_only`).Scan(&value); err != nil {
		t.Fatal(err)
	}
	if value != "preserve" {
		t.Fatalf("future-only data = %q, want preserve", value)
	}
	var created int
	if err := db.DB().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='instances'`).Scan(&created); err != nil {
		t.Fatal(err)
	}
	if created != 0 {
		t.Fatal("older migration mutated future schema")
	}
}

func TestRuntimeLifecycle_CurrentSchemaFastPathSkipsMigrationBody(t *testing.T) {
	db := newRuntimeTestDB(t)
	if _, err := db.DB().Exec(`INSERT INTO instances
		(id, title, project_path, created_at)
		VALUES ('late', 'late', '/tmp/late', 1)`); err != nil {
		t.Fatal(err)
	}

	reachedDDL := false
	db.testBeforeMigrationDDL = func() { reachedDDL = true }
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	db.testBeforeMigrationDDL = nil
	if reachedDDL {
		t.Fatal("current-schema migration entered DDL/backfill body")
	}

	var runtimeRows int
	if err := db.DB().QueryRow(`SELECT COUNT(*) FROM instance_runtime_state WHERE instance_id = 'late'`).Scan(&runtimeRows); err != nil {
		t.Fatal(err)
	}
	if runtimeRows != 0 {
		t.Fatal("current-schema migration repeated per-instance runtime backfill")
	}
}

func TestRuntimeLifecycle_ReadAllStatusesUsesAuthoritativeRuntime(t *testing.T) {
	db := newRuntimeTestDB(t)
	row := &InstanceRow{ID: "one", Title: "one", ProjectPath: "/tmp/one", GroupPath: "my-sessions", Tool: "pi", Status: "idle", CreatedAt: time.Unix(1, 0)}
	if err := db.SaveInstance(row); err != nil {
		t.Fatal(err)
	}
	if err := db.CommitRuntimeTransition(0, runtimeTestIncarnation(t, db, "one"), RuntimeState{InstanceID: "one", Generation: 1, TmuxSession: "g1", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().Exec(`UPDATE instances SET status = 'error' WHERE id = 'one'`); err != nil {
		t.Fatal(err)
	}
	statuses, err := db.ReadAllStatuses()
	if err != nil {
		t.Fatal(err)
	}
	status := statuses["one"]
	if status.Status != "running" || status.Generation != 1 || status.StatusRevision != 0 {
		t.Fatalf("status = %#v, want authoritative running generation 1 revision 0", status)
	}
}

func TestRuntimeLifecycle_MigrationIdempotentAndReadsRFC3339(t *testing.T) {
	db := newRuntimeTestDB(t)
	if _, err := db.DB().Exec(`INSERT INTO instances
		(id, title, project_path, group_path, tool, status, tmux_session, created_at, tool_data)
		VALUES ('legacy', 'legacy', '/tmp/legacy', 'my-sessions', 'pi', 'waiting', 'legacy-tmux', 1,
		'{"last_started_at":"2026-08-07T12:34:56.123Z"}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().Exec(`DELETE FROM instance_runtime_state WHERE instance_id = 'legacy'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().Exec(`UPDATE metadata SET value = ? WHERE key = 'schema_version'`, SchemaVersion-2); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	state, found, err := db.ReadRuntimeState("legacy")
	want := time.Date(2026, 8, 7, 12, 34, 56, 123_000_000, time.UTC)
	if err != nil || !found || state.Generation != 0 || state.TmuxSession != "legacy-tmux" || !state.LastStartedAt.Equal(want) {
		t.Fatalf("state=%#v found=%v err=%v", state, found, err)
	}
}

func TestRuntimeLifecycle_V14SecondTimestampsMigrateOnce(t *testing.T) {
	db := newRuntimeTestDB(t)
	if err := db.SaveInstance(&InstanceRow{
		ID: "legacy-v14", Title: "legacy-v14", ProjectPath: "/tmp/legacy-v14", GroupPath: "my-sessions",
		Tool: "claude", Status: "idle", CreatedAt: time.Unix(1, 0),
		ToolData: json.RawMessage(`{"last_started_at":200,"claude_session_id":"conversation","claude_detected_at":190}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().Exec(`UPDATE instance_runtime_state SET last_started_at = 200 WHERE instance_id = 'legacy-v14'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().Exec(`UPDATE instance_runtime_binding SET binding_detected_at = 190 WHERE instance_id = 'legacy-v14'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().Exec(`UPDATE metadata SET value = '14' WHERE key = 'schema_version'`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := db.Migrate(); err != nil {
			t.Fatal(err)
		}
	}
	state, found, err := db.ReadRuntimeState("legacy-v14")
	if err != nil || !found || !state.LastStartedAt.Equal(time.Unix(200, 0).UTC()) {
		t.Fatalf("runtime state = %#v found=%v err=%v", state, found, err)
	}
	binding, found, err := db.ReadRuntimeBinding("legacy-v14", "claude")
	if err != nil || !found || !binding.DetectedAt.Equal(time.Unix(190, 0).UTC()) {
		t.Fatalf("runtime binding = %#v found=%v err=%v", binding, found, err)
	}
}

func TestRuntimeLifecycle_MigrationReportsDeterministicBindingOwnerConflict(t *testing.T) {
	db := newRuntimeTestDB(t)
	for _, id := range []string{"b", "a"} {
		if _, err := db.DB().Exec(`INSERT INTO instances
			(id, title, project_path, group_path, tool, status, created_at, tool_data)
			VALUES (?, ?, '/tmp', 'my-sessions', 'claude', 'idle', 1,
			'{"claude_session_id":"shared"}')`, id, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.DB().Exec(`DROP TABLE instance_runtime_binding`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().Exec(`DROP TABLE instance_runtime_state`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().Exec(`UPDATE metadata SET value = ? WHERE key = 'schema_version'`, SchemaVersion-2); err != nil {
		t.Fatal(err)
	}
	err := db.Migrate()
	if err == nil || !strings.Contains(err.Error(), "legacy claude binding conflict for b") {
		t.Fatalf("migration error = %v, want deterministic loser b", err)
	}
}
