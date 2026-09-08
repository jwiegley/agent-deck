package statedb

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func registerLegacyWriterAfterDestructionReservation(t *testing.T, db *StateDB) {
	t.Helper()
	legacyPID := os.Getpid()
	// Model a separate current-version caller observing this live legacy
	// process, rather than relying on the self-PID special case in the fence.
	if db.pid == legacyPID {
		db.pid = legacyPID + 1
	}
	now := time.Now().Unix()
	if _, err := db.DB().Exec(`INSERT OR REPLACE INTO instance_heartbeats
		(pid, started, heartbeat, is_primary) VALUES (?, ?, ?, 0)`, legacyPID, now, now); err != nil {
		t.Fatalf("register late legacy writer: %v", err)
	}
	if err := db.RequireRuntimeWriterCompatibility(); !errors.Is(err, ErrIncompatibleWriterSchema) {
		t.Fatalf("writer compatibility error = %v, want ErrIncompatibleWriterSchema", err)
	}
}

func TestRuntimeLifecycle_LateLegacyWriterDoesNotBlockReservedCompletion(t *testing.T) {
	db := newRuntimeTestDB(t)
	parent := &InstanceRow{
		ID: "late-writer-stop", Title: "late writer stop", ProjectPath: "/tmp/late-writer-stop",
		Tool: "pi", Status: "running", TmuxSession: "runtime-stop", TmuxSocketName: "isolated",
		CreatedAt: time.Unix(1, 0).UTC(), RuntimeBindings: map[string]RuntimeBinding{},
	}
	if err := db.SaveInstance(parent); err != nil {
		t.Fatal(err)
	}
	expected, found, err := db.ReadRuntimeState(parent.ID)
	if err != nil || !found {
		t.Fatalf("ReadRuntimeState = found %v, err %v", found, err)
	}
	claimed, err := db.ReserveRuntimeDestruction(expected, parent.Incarnation)
	if err != nil {
		t.Fatalf("ReserveRuntimeDestruction: %v", err)
	}
	if _, err := db.CompleteRuntimeDestruction(claimed, parent.Incarnation, ""); !errors.Is(err, ErrStatusRevisionConflict) {
		t.Fatalf("empty terminal status error = %v, want ErrStatusRevisionConflict", err)
	}
	if got, found, err := db.ReadRuntimeState(parent.ID); err != nil || !found || got != claimed {
		t.Fatalf("empty terminal status changed reservation: got=%#v found=%v err=%v want=%#v", got, found, err, claimed)
	}

	registerLegacyWriterAfterDestructionReservation(t, db)
	completed, err := db.CompleteRuntimeDestruction(claimed, parent.Incarnation, "stopped")
	if err != nil {
		t.Fatalf("CompleteRuntimeDestruction: %v", err)
	}
	if completed.Status != "stopped" || completed.StatusRevision != claimed.StatusRevision+1 {
		t.Fatalf("completed runtime = %#v, want stopped revision %d", completed, claimed.StatusRevision+1)
	}
	got, found, err := db.ReadRuntimeState(parent.ID)
	if err != nil || !found || got != completed {
		t.Fatalf("durable runtime = %#v, found %v, err %v; want %#v", got, found, err, completed)
	}
	row, err := db.LoadInstanceByID(parent.ID)
	if err != nil || row == nil || row.Status != "stopped" {
		t.Fatalf("completed parent projection = %#v, err %v", row, err)
	}
	var provenanceRows int
	if err := db.DB().QueryRow(`SELECT COUNT(*) FROM instance_runtime_destruction WHERE instance_id = ?`, parent.ID).Scan(&provenanceRows); err != nil || provenanceRows != 0 {
		t.Fatalf("completion left destruction provenance rows = %d, err %v", provenanceRows, err)
	}
}

func TestRuntimeLifecycle_MetadataProjectionKeepsDestructionReservationPrivate(t *testing.T) {
	db := newRuntimeTestDB(t)
	parent := &InstanceRow{
		ID: "reserved-metadata", Title: "before", ProjectPath: "/tmp/reserved-metadata",
		Tool: "pi", Status: "running", TmuxSession: "runtime", TmuxSocketName: "isolated",
		CreatedAt: time.Unix(1, 0).UTC(), RuntimeBindings: map[string]RuntimeBinding{},
	}
	if err := db.SaveInstance(parent); err != nil {
		t.Fatal(err)
	}
	before, found, err := db.ReadRuntimeState(parent.ID)
	if err != nil || !found {
		t.Fatalf("ReadRuntimeState = %#v, found=%v err=%v", before, found, err)
	}
	claimed, err := db.ReserveRuntimeDestruction(before, parent.Incarnation)
	if err != nil {
		t.Fatal(err)
	}

	parent.Title = "single-save"
	parent.Status = "stale-metadata-status"
	if err := db.SaveInstance(parent); err != nil {
		t.Fatalf("SaveInstance during reservation: %v", err)
	}
	parent.Title = "batch-save"
	if err := db.UpdateInstances([]*InstanceRow{parent}); err != nil {
		t.Fatalf("UpdateInstances during reservation: %v", err)
	}

	got, found, err := db.ReadRuntimeState(parent.ID)
	if err != nil || !found || got != claimed {
		t.Fatalf("metadata save changed reservation: got=%#v found=%v err=%v want=%#v", got, found, err, claimed)
	}
	var projectedStatus, projectedTitle string
	if err := db.DB().QueryRow(`SELECT status, title FROM instances WHERE id = ?`, parent.ID).
		Scan(&projectedStatus, &projectedTitle); err != nil {
		t.Fatal(err)
	}
	if projectedStatus != before.Status || projectedTitle != "batch-save" {
		t.Fatalf("legacy projection status=%q title=%q, want prior=%q and latest metadata",
			projectedStatus, projectedTitle, before.Status)
	}
	rows, err := db.LoadInstances()
	if err != nil || len(rows) != 1 || rows[0].Status != before.Status {
		t.Fatalf("LoadInstances exposed reservation: rows=%#v err=%v", rows, err)
	}
	row, err := db.LoadInstanceByID(parent.ID)
	if err != nil || row == nil || row.Status != before.Status {
		t.Fatalf("LoadInstanceByID exposed reservation: row=%#v err=%v", row, err)
	}
	statuses, err := db.ReadAllStatuses()
	if err != nil || statuses[parent.ID].Status != before.Status {
		t.Fatalf("ReadAllStatuses exposed reservation: statuses=%#v err=%v", statuses, err)
	}
	snapshot, err := db.LoadMigrationSnapshot(parent.ID)
	if err != nil || snapshot == nil || snapshot.Instance == nil || snapshot.Runtime == nil {
		t.Fatalf("LoadMigrationSnapshot = %#v, err=%v", snapshot, err)
	}
	if snapshot.Instance.Status != before.Status || snapshot.Runtime.Status != runtimeDestructionStatus {
		t.Fatalf("migration projection instance=%q runtime=%q, want prior=%q and private marker",
			snapshot.Instance.Status, snapshot.Runtime.Status, before.Status)
	}

	if _, err := db.DB().Exec(`UPDATE metadata SET value = ? WHERE key = 'schema_version'`, SchemaVersion-1); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(); err != nil {
		t.Fatalf("migration rejected proved reservation: %v", err)
	}
	got, found, err = db.ReadRuntimeState(parent.ID)
	if err != nil || !found || got != claimed {
		t.Fatalf("migration changed reservation: got=%#v found=%v err=%v", got, found, err)
	}
	completed, err := db.CompleteRuntimeDestruction(claimed, parent.Incarnation, "stopped")
	if err != nil || completed.Status != "stopped" {
		t.Fatalf("CompleteRuntimeDestruction = %#v, err=%v", completed, err)
	}
}

func TestRuntimeLifecycle_MigrateRejectsUnprovenDestructionReservation(t *testing.T) {
	db := newRuntimeTestDB(t)
	parent := &InstanceRow{
		ID: "orphaned-reservation", Title: "orphaned reservation",
		ProjectPath: "/tmp/orphaned-reservation", Tool: "pi", Status: "idle",
		TmuxSession: "runtime-orphaned", TmuxSocketName: "isolated",
		CreatedAt: time.Unix(1, 0).UTC(), RuntimeBindings: map[string]RuntimeBinding{},
	}
	if err := db.SaveInstance(parent); err != nil {
		t.Fatal(err)
	}
	state, found, err := db.ReadRuntimeState(parent.ID)
	if err != nil || !found {
		t.Fatalf("read runtime: state=%#v found=%v err=%v", state, found, err)
	}
	claimed, err := db.ReserveRuntimeDestruction(state, parent.Incarnation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().Exec(`DELETE FROM instance_runtime_destruction WHERE instance_id = ?`, parent.ID); err != nil {
		t.Fatal(err)
	}
	if rows, err := db.LoadInstances(); err == nil {
		t.Fatalf("LoadInstances accepted unproved reservation: rows=%#v", rows)
	}
	if row, err := db.LoadInstanceByID(parent.ID); err == nil {
		t.Fatalf("LoadInstanceByID accepted unproved reservation: row=%#v", row)
	}
	if statuses, err := db.ReadAllStatuses(); err == nil {
		t.Fatalf("ReadAllStatuses accepted unproved reservation: statuses=%#v", statuses)
	}
	if snapshot, err := db.LoadMigrationSnapshot(parent.ID); !errors.Is(err, ErrStatusRevisionConflict) {
		t.Fatalf("LoadMigrationSnapshot snapshot=%#v err=%v, want ErrStatusRevisionConflict", snapshot, err)
	}
	if _, err := db.DB().Exec(`UPDATE metadata SET value = ? WHERE key = 'schema_version'`, SchemaVersion-1); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(); !errors.Is(err, ErrStatusRevisionConflict) {
		t.Fatalf("Migrate error=%v, want ErrStatusRevisionConflict", err)
	}
	version, err := db.GetMeta("schema_version")
	if err != nil || version != "17" {
		t.Fatalf("schema version=%q err=%v, want rolled-back 17", version, err)
	}
	got, found, err := db.ReadRuntimeState(parent.ID)
	if err != nil || !found || got != claimed {
		t.Fatalf("migration changed orphaned reservation: got=%#v found=%v err=%v want=%#v",
			got, found, err, claimed)
	}
}

func TestRuntimeLifecycle_DestructionMarkerCannotBeSeededByGenericCreators(t *testing.T) {
	marker := func(id string) RuntimeState {
		return RuntimeState{
			InstanceID: id, Generation: 3, StatusRevision: 7,
			TmuxSession: "runtime-" + id, TmuxSocketName: "isolated",
			Status: runtimeDestructionStatus, LastStartedAt: time.Unix(20, 0).UTC(),
		}
	}
	rowFor := func(state RuntimeState, incarnation string) *InstanceRow {
		return &InstanceRow{
			ID: state.InstanceID, Incarnation: incarnation, Title: state.InstanceID,
			ProjectPath: "/tmp/" + state.InstanceID, GroupPath: "my-sessions", Tool: "pi",
			Status: state.Status, TmuxSession: state.TmuxSession, TmuxSocketName: state.TmuxSocketName,
			RuntimeGeneration: state.Generation, StatusRevision: state.StatusRevision,
			LastStartedAt: state.LastStartedAt, CreatedAt: time.Unix(1, 0).UTC(),
			RuntimeBindings: map[string]RuntimeBinding{},
		}
	}
	assertNoRuntime := func(t *testing.T, db *StateDB, id string) {
		t.Helper()
		if state, found, err := db.ReadRuntimeState(id); err != nil || found {
			t.Fatalf("runtime was seeded: state=%#v found=%v err=%v", state, found, err)
		}
	}
	seedNormal := func(t *testing.T, db *StateDB, id string) (*InstanceRow, RuntimeState) {
		t.Helper()
		parent := &InstanceRow{
			ID: id, Title: id, ProjectPath: "/tmp/" + id, GroupPath: "my-sessions",
			Tool: "pi", Status: "running", TmuxSession: "runtime-" + id,
			TmuxSocketName: "isolated", CreatedAt: time.Unix(1, 0).UTC(),
			RuntimeBindings: map[string]RuntimeBinding{},
		}
		if err := db.SaveInstance(parent); err != nil {
			t.Fatal(err)
		}
		state, found, err := db.ReadRuntimeState(id)
		if err != nil || !found {
			t.Fatalf("read normal runtime: state=%#v found=%v err=%v", state, found, err)
		}
		return parent, state
	}
	assertUnchanged := func(t *testing.T, db *StateDB, want RuntimeState) {
		t.Helper()
		got, found, err := db.ReadRuntimeState(want.InstanceID)
		if err != nil || !found || got != want {
			t.Fatalf("runtime changed: got=%#v found=%v err=%v want=%#v", got, found, err, want)
		}
	}

	t.Run("SaveInstance replacement", func(t *testing.T) {
		db := newRuntimeTestDB(t)
		original := &InstanceRow{
			ID: "save", Title: "original", ProjectPath: "/tmp/save", GroupPath: "my-sessions",
			Tool: "pi", Status: "running", TmuxSession: "runtime-save", TmuxSocketName: "isolated",
			CreatedAt: time.Unix(1, 0).UTC(), RuntimeBindings: map[string]RuntimeBinding{},
		}
		if err := db.SaveInstance(original); err != nil {
			t.Fatal(err)
		}
		state, found, err := db.ReadRuntimeState(original.ID)
		if err != nil || !found {
			t.Fatalf("read original runtime: state=%#v found=%v err=%v", state, found, err)
		}
		claimed, err := db.ReserveRuntimeDestruction(state, original.Incarnation)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.DeleteInstance(original.ID); err != nil {
			t.Fatal(err)
		}
		if err := db.SaveInstance(rowFor(claimed, "replacement-incarnation")); !errors.Is(err, ErrStatusRevisionConflict) {
			t.Fatalf("SaveInstance marker error=%v, want ErrStatusRevisionConflict", err)
		}
		if row, err := db.LoadInstanceByID(original.ID); err != nil || row != nil {
			t.Fatalf("replacement parent survived rollback: row=%#v err=%v", row, err)
		}
		assertNoRuntime(t, db, original.ID)
	})

	t.Run("InsertInstanceIfAbsent", func(t *testing.T) {
		db := newRuntimeTestDB(t)
		state := marker("insert")
		if _, inserted, err := db.InsertInstanceIfAbsent(rowFor(state, "insert-incarnation")); !errors.Is(err, ErrStatusRevisionConflict) || inserted {
			t.Fatalf("InsertInstanceIfAbsent inserted=%v err=%v", inserted, err)
		}
		if row, err := db.LoadInstanceByID(state.InstanceID); err != nil || row != nil {
			t.Fatalf("insert parent survived rollback: row=%#v err=%v", row, err)
		}
		assertNoRuntime(t, db, state.InstanceID)
	})

	t.Run("EnsureRuntimeState", func(t *testing.T) {
		db := newRuntimeTestDB(t)
		parent := &InstanceRow{
			ID: "ensure", Title: "ensure", ProjectPath: "/tmp/ensure", GroupPath: "my-sessions",
			Tool: "pi", Status: "idle", CreatedAt: time.Unix(1, 0).UTC(),
			RuntimeBindings: map[string]RuntimeBinding{},
		}
		if err := db.SaveInstance(parent); err != nil {
			t.Fatal(err)
		}
		if _, err := db.DB().Exec(`DELETE FROM instance_runtime_state WHERE instance_id = ?`, parent.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.EnsureRuntimeState(marker(parent.ID), parent.Incarnation); !errors.Is(err, ErrStatusRevisionConflict) {
			t.Fatalf("EnsureRuntimeState marker error=%v, want ErrStatusRevisionConflict", err)
		}
		assertNoRuntime(t, db, parent.ID)
	})

	t.Run("EnsureRuntimeStateForNewInstance", func(t *testing.T) {
		db := newRuntimeTestDB(t)
		state := marker("ensure-new")
		if _, err := db.EnsureRuntimeStateForNewInstance(state, "ensure-new-incarnation"); !errors.Is(err, ErrStatusRevisionConflict) {
			t.Fatalf("EnsureRuntimeStateForNewInstance marker error=%v, want ErrStatusRevisionConflict", err)
		}
		assertNoRuntime(t, db, state.InstanceID)
		var tokens int
		if err := db.DB().QueryRow(`SELECT COUNT(*) FROM instance_incarnation WHERE instance_id = ?`, state.InstanceID).Scan(&tokens); err != nil || tokens != 0 {
			t.Fatalf("new-instance marker left incarnation rows=%d err=%v", tokens, err)
		}
	})

	t.Run("WriteStatusIfVersion", func(t *testing.T) {
		db := newRuntimeTestDB(t)
		parent, before := seedNormal(t, db, "versioned-status")
		if applied, err := db.WriteStatusIfVersion(parent.ID, parent.Incarnation,
			before.Generation, before.StatusRevision, runtimeDestructionStatus); !errors.Is(err, ErrStatusRevisionConflict) || applied {
			t.Fatalf("WriteStatusIfVersion applied=%v err=%v", applied, err)
		}
		assertUnchanged(t, db, before)
	})

	t.Run("WriteStatus", func(t *testing.T) {
		db := newRuntimeTestDB(t)
		parent, before := seedNormal(t, db, "compat-status")
		if err := db.WriteStatus(parent.ID, runtimeDestructionStatus, parent.Tool); !errors.Is(err, ErrStatusRevisionConflict) {
			t.Fatalf("WriteStatus marker error=%v", err)
		}
		assertUnchanged(t, db, before)
	})

	t.Run("PersistInstanceStatusesTx", func(t *testing.T) {
		db := newRuntimeTestDB(t)
		parent, before := seedNormal(t, db, "batch-status")
		err := db.PersistInstanceStatusesTx([]InstanceStatusUpdate{{
			ID: parent.ID, Incarnation: parent.Incarnation, Status: runtimeDestructionStatus,
			Generation: before.Generation, StatusRevision: before.StatusRevision, Versioned: true,
		}})
		if !errors.Is(err, ErrStatusRevisionConflict) {
			t.Fatalf("PersistInstanceStatusesTx marker error=%v", err)
		}
		assertUnchanged(t, db, before)
	})

	t.Run("CommitRuntimeTransitionWithBindingPlan", func(t *testing.T) {
		db := newRuntimeTestDB(t)
		parent, before := seedNormal(t, db, "transition-status")
		next := before
		next.Generation++
		next.StatusRevision = 0
		next.Status = runtimeDestructionStatus
		if err := db.CommitRuntimeTransitionWithBindingPlan(before.Generation, parent.Incarnation, next, nil); !errors.Is(err, ErrStatusRevisionConflict) {
			t.Fatalf("CommitRuntimeTransitionWithBindingPlan marker error=%v", err)
		}
		assertUnchanged(t, db, before)
	})

	t.Run("CommitRuntimeTransitionCannotReplaceReservation", func(t *testing.T) {
		db := newRuntimeTestDB(t)
		parent, before := seedNormal(t, db, "transition-reserved")
		binding, applied, err := db.WriteRuntimeBindingIfVersion(
			parent.ID, parent.Incarnation, before.Generation, "claude", 0,
			"conversation-before", time.Unix(10, 0).UTC())
		if err != nil || !applied {
			t.Fatalf("seed binding=%#v applied=%v err=%v", binding, applied, err)
		}
		claimed, err := db.ReserveRuntimeDestruction(before, parent.Incarnation)
		if err != nil {
			t.Fatal(err)
		}
		next := before
		next.Generation++
		next.StatusRevision = 0
		next.TmuxSession = "replacement"
		plan := []RuntimeBindingTransition{{
			Kind: "claude", ExpectedRevision: binding.Revision,
			NextValue: "conversation-replacement", DetectedAt: time.Unix(20, 0).UTC(),
		}}
		if err := db.CommitRuntimeTransitionWithBindingPlan(before.Generation, parent.Incarnation, next, plan); !errors.Is(err, ErrStatusRevisionConflict) {
			t.Fatalf("CommitRuntimeTransitionWithBindingPlan reservation error=%v", err)
		}
		got, found, err := db.ReadRuntimeState(parent.ID)
		if err != nil || !found || got != claimed {
			t.Fatalf("commit replaced reservation: got=%#v found=%v err=%v want=%#v", got, found, err, claimed)
		}
		var provenanceRows int
		if err := db.DB().QueryRow(`SELECT COUNT(*) FROM instance_runtime_destruction WHERE instance_id = ?`, parent.ID).
			Scan(&provenanceRows); err != nil || provenanceRows != 1 {
			t.Fatalf("commit changed destruction provenance: rows=%d err=%v", provenanceRows, err)
		}
		gotBinding, found, err := db.ReadRuntimeBinding(parent.ID, "claude")
		if err != nil || !found || gotBinding != binding {
			t.Fatalf("commit changed binding: got=%#v found=%v err=%v want=%#v",
				gotBinding, found, err, binding)
		}
		row, err := db.LoadInstanceByID(parent.ID)
		if err != nil || row == nil || row.Status != before.Status ||
			row.RuntimeGeneration != claimed.Generation || row.StatusRevision != claimed.StatusRevision {
			t.Fatalf("commit changed compatibility projection: row=%#v err=%v", row, err)
		}
	})

	t.Run("InsertInstanceRowForMigration", func(t *testing.T) {
		sourceDB := newRuntimeTestDB(t)
		parent, _ := seedNormal(t, sourceDB, "migration-status")
		snapshot, err := sourceDB.LoadMigrationSnapshot(parent.ID)
		if err != nil || snapshot == nil || snapshot.Runtime == nil {
			t.Fatalf("LoadMigrationSnapshot snapshot=%#v err=%v", snapshot, err)
		}
		snapshot.Runtime.Status = runtimeDestructionStatus
		destinationDB := newRuntimeTestDB(t)
		if _, err := destinationDB.InsertInstanceRowForMigration(snapshot); !errors.Is(err, ErrStatusRevisionConflict) {
			t.Fatalf("InsertInstanceRowForMigration marker error=%v", err)
		}
		if row, err := destinationDB.LoadInstanceByID(parent.ID); err != nil || row != nil {
			t.Fatalf("migration marker parent=%#v err=%v", row, err)
		}
		assertNoRuntime(t, destinationDB, parent.ID)
	})
}

func TestRuntimeLifecycle_LateLegacyWriterDoesNotBlockReservedDelete(t *testing.T) {
	db := newRuntimeTestDB(t)
	parents := make(map[string]*InstanceRow)
	states := make(map[string]RuntimeState)
	for _, id := range []string{"reserved-delete", "unreserved-delete"} {
		binding := RuntimeBinding{
			InstanceID: id, Kind: "claude", Revision: 1, Value: "conversation-" + id,
		}
		parent := &InstanceRow{
			ID: id, Title: id, ProjectPath: "/tmp/" + id, Tool: "pi",
			Status: "stopped", TmuxSession: "runtime-" + id, TmuxSocketName: "isolated",
			CreatedAt: time.Unix(1, 0).UTC(), RuntimeBindings: map[string]RuntimeBinding{"claude": binding},
		}
		if err := db.SaveInstance(parent); err != nil {
			t.Fatalf("SaveInstance(%s): %v", id, err)
		}
		state, found, err := db.ReadRuntimeState(id)
		if err != nil || !found {
			t.Fatalf("ReadRuntimeState(%s) = found %v, err %v", id, found, err)
		}
		parents[id] = parent
		states[id] = state
	}
	claimed, err := db.ReserveRuntimeDestruction(states["reserved-delete"], parents["reserved-delete"].Incarnation)
	if err != nil {
		t.Fatalf("ReserveRuntimeDestruction: %v", err)
	}

	registerLegacyWriterAfterDestructionReservation(t, db)
	if err := db.DeleteInstanceIfRuntime(claimed, parents["reserved-delete"].Incarnation); err != nil {
		t.Fatalf("delete reserved runtime: %v", err)
	}
	if row, err := db.LoadInstanceByID("reserved-delete"); err != nil || row != nil {
		t.Fatalf("reserved parent after delete = %#v, err %v", row, err)
	}
	if runtime, found, err := db.ReadRuntimeState("reserved-delete"); err != nil || found {
		t.Fatalf("reserved runtime after delete = %#v, found %v, err %v", runtime, found, err)
	}
	if binding, found, err := db.ReadRuntimeBinding("reserved-delete", "claude"); err != nil || found {
		t.Fatalf("reserved binding after delete = %#v, found %v, err %v", binding, found, err)
	}
	var incarnationRows int
	if err := db.DB().QueryRow(`SELECT COUNT(*) FROM instance_incarnation WHERE instance_id = ?`, "reserved-delete").Scan(&incarnationRows); err != nil || incarnationRows != 0 {
		t.Fatalf("reserved incarnation rows after delete = %d, err %v", incarnationRows, err)
	}
	var provenanceRows int
	if err := db.DB().QueryRow(`SELECT COUNT(*) FROM instance_runtime_destruction WHERE instance_id = ?`, "reserved-delete").Scan(&provenanceRows); err != nil || provenanceRows != 0 {
		t.Fatalf("reserved destruction provenance rows after delete = %d, err %v", provenanceRows, err)
	}

	if _, err := db.ReserveRuntimeDestruction(states["unreserved-delete"], parents["unreserved-delete"].Incarnation); !errors.Is(err, ErrIncompatibleWriterSchema) {
		t.Fatalf("reserve unreserved runtime error = %v, want ErrIncompatibleWriterSchema", err)
	}
	err = db.DeleteInstanceIfRuntime(states["unreserved-delete"], parents["unreserved-delete"].Incarnation)
	if !errors.Is(err, ErrIncompatibleWriterSchema) {
		t.Fatalf("delete unreserved runtime error = %v, want ErrIncompatibleWriterSchema", err)
	}
	if row, err := db.LoadInstanceByID("unreserved-delete"); err != nil || row == nil {
		t.Fatalf("unreserved parent after rejected delete = %#v, err %v", row, err)
	}
	if got, found, err := db.ReadRuntimeState("unreserved-delete"); err != nil || !found || got != states["unreserved-delete"] {
		t.Fatalf("unreserved runtime after rejected delete = %#v, found %v, err %v", got, found, err)
	}
	if binding, found, err := db.ReadRuntimeBinding("unreserved-delete", "claude"); err != nil || !found || binding.Value != "conversation-unreserved-delete" {
		t.Fatalf("unreserved binding after rejected delete = %#v, found %v, err %v", binding, found, err)
	}
}

func TestRuntimeLifecycle_ReservedDestructionRejectsBatchStatusPublisher(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	parent := &InstanceRow{
		ID: "one", Title: "one", ProjectPath: "/tmp/one", Tool: "pi",
		Status: "running", TmuxSession: "runtime-g0", TmuxSocketName: "isolated",
		CreatedAt: time.Unix(1, 0).UTC(), RuntimeBindings: map[string]RuntimeBinding{},
	}
	if err := db.SaveInstance(parent); err != nil {
		t.Fatal(err)
	}
	expected, found, err := db.ReadRuntimeState("one")
	if err != nil || !found {
		t.Fatalf("ReadRuntimeState = found %v, err %v", found, err)
	}
	claimed, err := db.ReserveRuntimeDestruction(expected, parent.Incarnation)
	if err != nil {
		t.Fatal(err)
	}

	// An unversioned batch reads the reserved revision itself. The sentinel
	// predicate must still prevent that observation from becoming a write.
	err = db.PersistInstanceStatusesTx([]InstanceStatusUpdate{{
		ID: "one", Incarnation: parent.Incarnation, Status: "waiting",
	}})
	if !errors.Is(err, ErrStatusRevisionConflict) {
		t.Fatalf("PersistInstanceStatusesTx error = %v, want status revision conflict", err)
	}
	got, found, err := db.ReadRuntimeState("one")
	if err != nil || !found {
		t.Fatalf("ReadRuntimeState after batch = found %v, err %v", found, err)
	}
	if got != claimed {
		t.Fatalf("runtime after rejected batch = %#v, want claimed %#v", got, claimed)
	}
}

func TestRuntimeLifecycle_DeleteInstanceIfRuntimeSignalsChangeAndPreservesUnrelated(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}

	parents := make(map[string]*InstanceRow)
	for _, id := range []string{"delete-me", "keep-me"} {
		parent := &InstanceRow{
			ID: id, Title: id, ProjectPath: "/tmp/" + id, Tool: "pi",
			Status: "stopped", TmuxSession: "runtime-" + id, TmuxSocketName: "isolated",
			CreatedAt: time.Unix(1, 0).UTC(), RuntimeBindings: map[string]RuntimeBinding{},
		}
		if err := db.SaveInstance(parent); err != nil {
			t.Fatalf("SaveInstance(%s): %v", id, err)
		}
		parents[id] = parent
	}
	expected, found, err := db.ReadRuntimeState("delete-me")
	if err != nil || !found {
		t.Fatalf("ReadRuntimeState(delete-me) = found %v, err %v", found, err)
	}
	claimed, err := db.ReserveRuntimeDestruction(expected, parents["delete-me"].Incarnation)
	if err != nil {
		t.Fatalf("ReserveRuntimeDestruction: %v", err)
	}
	if err := db.SetMeta("last_modified", "1"); err != nil {
		t.Fatalf("SetMeta(last_modified): %v", err)
	}

	if err := db.DeleteInstanceIfRuntime(claimed, parents["delete-me"].Incarnation); err != nil {
		t.Fatalf("DeleteInstanceIfRuntime: %v", err)
	}
	after, err := db.LastModified()
	if err != nil {
		t.Fatalf("LastModified: %v", err)
	}
	if after <= 1 {
		t.Fatalf("last_modified = %d, want a deletion change signal newer than 1", after)
	}
	if row, err := db.LoadInstanceByID("delete-me"); err != nil || row != nil {
		t.Fatalf("deleted instance row = %#v, err %v", row, err)
	}
	if runtime, found, err := db.ReadRuntimeState("delete-me"); err != nil || found {
		t.Fatalf("deleted runtime row = %#v, found %v, err %v", runtime, found, err)
	}
	if row, err := db.LoadInstanceByID("keep-me"); err != nil || row == nil {
		t.Fatalf("unrelated instance row = %#v, err %v", row, err)
	}
	if runtime, found, err := db.ReadRuntimeState("keep-me"); err != nil || !found {
		t.Fatalf("unrelated runtime row = %#v, found %v, err %v", runtime, found, err)
	}
}

func TestRuntimeLifecycle_DestructionRejectsByteIdenticalReinsert(t *testing.T) {
	newParent := func(id, status string, revision uint64) *InstanceRow {
		return &InstanceRow{
			ID: id, Title: "byte identical", ProjectPath: "/tmp/" + id,
			GroupPath: "my-sessions", Tool: "pi", Status: status,
			TmuxSession: "same-runtime", TmuxSocketName: "same-socket",
			StatusRevision: revision, CreatedAt: time.Unix(1, 0).UTC(),
			RuntimeBindings: map[string]RuntimeBinding{},
		}
	}

	t.Run("reserve", func(t *testing.T) {
		db := newRuntimeTestDB(t)
		first := newParent("reserve-aba", "running", 0)
		if err := db.SaveInstance(first); err != nil {
			t.Fatal(err)
		}
		expected, found, err := db.ReadRuntimeState(first.ID)
		if err != nil || !found {
			t.Fatalf("read original runtime: %#v found=%v err=%v", expected, found, err)
		}
		if err := db.DeleteInstance(first.ID); err != nil {
			t.Fatal(err)
		}
		winner := newParent(first.ID, "running", 0)
		if err := db.SaveInstance(winner); err != nil {
			t.Fatal(err)
		}
		if winner.Incarnation == first.Incarnation {
			t.Fatalf("reinsert reused incarnation %q", winner.Incarnation)
		}
		if err := db.ValidateInstanceIncarnation(first.ID, first.Incarnation); !errors.Is(err, ErrInstanceParentConflict) {
			t.Fatalf("validate old incarnation error = %v, want parent conflict", err)
		}
		if err := db.ValidateInstanceIncarnation(winner.ID, winner.Incarnation); err != nil {
			t.Fatalf("validate winner incarnation: %v", err)
		}
		if _, err := db.ReserveRuntimeDestruction(expected, first.Incarnation); !errors.Is(err, ErrInstanceParentConflict) {
			t.Fatalf("reserve error = %v, want parent conflict", err)
		}
		if got, found, err := db.ReadRuntimeState(first.ID); err != nil || !found || got != expected {
			t.Fatalf("winner runtime = %#v found=%v err=%v; want %#v", got, found, err, expected)
		}
	})

	t.Run("complete", func(t *testing.T) {
		db := newRuntimeTestDB(t)
		first := newParent("complete-aba", "running", 0)
		if err := db.SaveInstance(first); err != nil {
			t.Fatal(err)
		}
		expected, found, err := db.ReadRuntimeState(first.ID)
		if err != nil || !found {
			t.Fatalf("read original runtime: %#v found=%v err=%v", expected, found, err)
		}
		claimed, err := db.ReserveRuntimeDestruction(expected, first.Incarnation)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.DeleteInstance(first.ID); err != nil {
			t.Fatal(err)
		}
		winner := newParent(first.ID, "running", 0)
		if err := db.SaveInstance(winner); err != nil {
			t.Fatal(err)
		}
		winnerState, found, err := db.ReadRuntimeState(winner.ID)
		if err != nil || !found {
			t.Fatalf("read winner runtime: %#v found=%v err=%v", winnerState, found, err)
		}
		winnerClaimed, err := db.ReserveRuntimeDestruction(winnerState, winner.Incarnation)
		if err != nil {
			t.Fatalf("reserve winner runtime: %v", err)
		}
		if _, err := db.CompleteRuntimeDestruction(claimed, first.Incarnation, "stopped"); !errors.Is(err, ErrInstanceParentConflict) {
			t.Fatalf("complete error = %v, want parent conflict", err)
		}
		if got, found, err := db.ReadRuntimeState(first.ID); err != nil || !found || got != winnerClaimed {
			t.Fatalf("winner runtime = %#v found=%v err=%v; want %#v", got, found, err, winnerClaimed)
		}
	})

	t.Run("delete", func(t *testing.T) {
		db := newRuntimeTestDB(t)
		first := newParent("delete-aba", "stopped", 0)
		if err := db.SaveInstance(first); err != nil {
			t.Fatal(err)
		}
		expected, found, err := db.ReadRuntimeState(first.ID)
		if err != nil || !found {
			t.Fatalf("read original runtime: %#v found=%v err=%v", expected, found, err)
		}
		if err := db.DeleteInstance(first.ID); err != nil {
			t.Fatal(err)
		}
		winner := newParent(first.ID, "stopped", 0)
		if err := db.SaveInstance(winner); err != nil {
			t.Fatal(err)
		}
		if err := db.DeleteInstanceIfRuntime(expected, first.Incarnation); !errors.Is(err, ErrInstanceParentConflict) {
			t.Fatalf("delete error = %v, want parent conflict", err)
		}
		row, err := db.LoadInstanceByID(first.ID)
		if err != nil || row == nil || row.Incarnation != winner.Incarnation {
			t.Fatalf("winner parent = %#v err=%v", row, err)
		}
		if got, found, err := db.ReadRuntimeState(first.ID); err != nil || !found || got != expected {
			t.Fatalf("winner runtime = %#v found=%v err=%v; want %#v", got, found, err, expected)
		}
	})
}
