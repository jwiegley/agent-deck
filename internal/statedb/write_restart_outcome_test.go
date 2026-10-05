package statedb

import (
	"errors"
	"testing"
	"time"
)

// A restart mints a new tmux session name and that name is the only handle
// anything has on the live process. The authoritative runtime transition
// commits the name and status together; WriteRestartOutcome verifies that
// exact tuple, acknowledges it, and emits the peer-reload stamp (#1870).

func restartOutcomeRow(id string) *InstanceRow {
	return &InstanceRow{
		ID:             id,
		Incarnation:    id + "-incarnation",
		Title:          "target",
		ProjectPath:    "/tmp/proj",
		GroupPath:      "Ungrouped",
		Command:        "claude",
		Tool:           "claude",
		Status:         "error",
		TmuxSession:    "agentdeck_target_deadbeef",
		TmuxSocketName: "old-socket",
		CreatedAt:      time.Now(),
	}
}

func committedRestartOutcome(t *testing.T, db *StateDB, id string) (RuntimeState, string) {
	t.Helper()
	row := restartOutcomeRow(id)
	seed, inserted, err := db.InsertInstanceIfAbsent(row)
	if err != nil || !inserted {
		t.Fatalf("InsertInstanceIfAbsent: inserted=%v err=%v", inserted, err)
	}
	next := RuntimeState{
		InstanceID: id, Generation: seed.Runtime.Generation + 1,
		TmuxSession: "agentdeck_target_f00dcafe", TmuxSocketName: "restart-socket",
		Status: "waiting", LastStartedAt: time.Now().UTC(),
	}
	if err := db.CommitRuntimeTransition(seed.Runtime.Generation, row.Incarnation, next); err != nil {
		t.Fatalf("CommitRuntimeTransition: %v", err)
	}
	return next, row.Incarnation
}

func TestWriteRestartOutcome_UpdatesOnlyItsOwnColumns(t *testing.T) {
	db := newTestDB(t)
	expected, incarnation := committedRestartOutcome(t, db, "restart-target")

	if _, err := db.db.Exec(`UPDATE instances SET tool = 'codex' WHERE id = ?`, expected.InstanceID); err != nil {
		t.Fatalf("concurrent tool update: %v", err)
	}
	if _, err := db.WriteRestartOutcome(expected, incarnation); err != nil {
		t.Fatalf("WriteRestartOutcome: %v", err)
	}

	rows, err := db.LoadInstances()
	if err != nil {
		t.Fatalf("LoadInstances: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("LoadInstances returned %d rows, want 1", len(rows))
	}
	got := rows[0]
	if got.TmuxSession != "agentdeck_target_f00dcafe" {
		t.Errorf("tmux_session = %q, want the restart's new name: the stored name still points at "+
			"the session the restart killed, so agent-deck reports a live session as errored", got.TmuxSession)
	}
	if got.Status != "waiting" {
		t.Errorf("status = %q, want waiting: a row naming a live pane but still marked error "+
			"misreports the session just as badly as one naming a dead pane", got.Status)
	}
	if got.Tool != "codex" {
		t.Errorf("tool = %q, want concurrent metadata value codex", got.Tool)
	}
	// The point of a targeted write is that it leaves everything else alone. A
	// whole-row save from a stale snapshot is what this exists to avoid.
	if got.Title != "target" || got.ProjectPath != "/tmp/proj" || got.GroupPath != "Ungrouped" {
		t.Errorf("unrelated columns changed: title=%q path=%q group=%q",
			got.Title, got.ProjectPath, got.GroupPath)
	}
}

// TestWriteRestartOutcome_StampsIdentifyOurOwnBump pins the mechanism the TUI
// depends on. The write moves last_modified, and a TUI that cannot tell that
// bump apart from another process's change reads its own write as external and
// abandons the save that would have persisted the rest of the restart.
func TestWriteRestartOutcome_StampsIdentifyOurOwnBump(t *testing.T) {
	db := newTestDB(t)
	expected, incarnation := committedRestartOutcome(t, db, "stamped")
	loadedAt, err := db.LastModified()
	if err != nil {
		t.Fatalf("LastModified: %v", err)
	}

	stamps, err := db.WriteRestartOutcome(expected, incarnation)
	if err != nil {
		t.Fatalf("WriteRestartOutcome: %v", err)
	}
	current, err := db.LastModified()
	if err != nil {
		t.Fatalf("LastModified: %v", err)
	}
	if !stamps.SoleWriterSince(loadedAt, current) {
		t.Fatalf("SoleWriterSince(%d, %d) = false for stamps %+v: nothing else wrote, so this "+
			"write must be recognisable as our own", loadedAt, current, stamps)
	}

	// Somebody else writes afterwards: our stamp is no longer what the database
	// reads, so the change IS external and the caller must not claim otherwise.
	if err := db.WriteStatus("stamped", "running", "claude"); err != nil {
		t.Fatalf("WriteStatus: %v", err)
	}
	if err := db.Touch(); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	after, err := db.LastModified()
	if err != nil {
		t.Fatalf("LastModified: %v", err)
	}
	if stamps.SoleWriterSince(loadedAt, after) {
		t.Error("SoleWriterSince = true after another writer bumped last_modified")
	}
}

// TestWriteStamps_SoleWriterSince_RejectsAPriorExternalChange is the half that
// protects against the lost update. A change that landed BEFORE our write is
// still a change we have not seen, and treating our own bump as proof of
// freshness would let a stale snapshot save proceed and revert it.
func TestWriteStamps_SoleWriterSince_RejectsAPriorExternalChange(t *testing.T) {
	const loadedAt = 1000
	// Something wrote at 1500, then we wrote at 2000.
	stamps := WriteStamps{Before: 1500, After: 2000}

	if stamps.SoleWriterSince(loadedAt, 2000) {
		t.Error("SoleWriterSince = true though another process wrote between the load and our write: " +
			"the caller would treat its stale snapshot as current and overwrite that process's change")
	}
	if !(WriteStamps{Before: 1000, After: 2000}).SoleWriterSince(loadedAt, 2000) {
		t.Error("SoleWriterSince = false when our write was the only change since the load")
	}
	if (WriteStamps{}).SoleWriterSince(loadedAt, 0) {
		t.Error("a zero WriteStamps claims sole authorship; it recorded nothing")
	}
}

func TestWriteRestartOutcome_UnknownInstanceIsNotSilentlyDropped(t *testing.T) {
	db := newTestDB(t)

	_, err := db.WriteRestartOutcome(RuntimeState{
		InstanceID: "never-stored", Generation: 1,
		TmuxSession: "agentdeck_ghost_f00dcafe", TmuxSocketName: "ghost-socket",
		LastStartedAt: time.Now().UTC(),
	}, "missing-incarnation")
	if err == nil {
		t.Fatal("WriteRestartOutcome succeeded for an instance with no row: SQLite reports a " +
			"zero-row UPDATE as success, so the caller would announce a durable write that " +
			"never happened")
	}
	if !errors.Is(err, ErrInstanceNotStored) {
		t.Errorf("error = %v, want ErrInstanceNotStored so callers can tell it apart from an I/O failure", err)
	}
}

func TestWriteRestartOutcome_RejectsStalePhysicalRuntime(t *testing.T) {
	db := newTestDB(t)
	stale, incarnation := committedRestartOutcome(t, db, "stale-restart")
	winner := stale
	winner.Generation++
	winner.TmuxSession = "agentdeck_winner_cafebabe"
	winner.LastStartedAt = winner.LastStartedAt.Add(time.Second)
	if err := db.CommitRuntimeTransition(stale.Generation, incarnation, winner); err != nil {
		t.Fatalf("CommitRuntimeTransition winner: %v", err)
	}
	before, err := db.LastModified()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := db.WriteRestartOutcome(stale, incarnation); !errors.Is(err, ErrRuntimeGenerationConflict) {
		t.Fatalf("WriteRestartOutcome stale error = %v, want ErrRuntimeGenerationConflict", err)
	}
	after, err := db.LastModified()
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("last_modified changed on rejected stale outcome: before=%d after=%d", before, after)
	}
	got, found, err := db.ReadRuntimeState(stale.InstanceID)
	if err != nil || !found || got != winner {
		t.Fatalf("winner changed by stale outcome: got=%#v found=%v err=%v want=%#v", got, found, err, winner)
	}
}

func TestWriteRestartOutcome_AcknowledgementAndStampAreAtomic(t *testing.T) {
	db := newTestDB(t)
	expected, incarnation := committedRestartOutcome(t, db, "atomic-restart")
	if _, err := db.db.Exec(`UPDATE instance_runtime_state SET status = 'running' WHERE instance_id = ?`, expected.InstanceID); err != nil {
		t.Fatalf("set authoritative running status: %v", err)
	}
	if _, err := db.db.Exec(`UPDATE instances SET status = 'running', acknowledged = 1 WHERE id = ?`, expected.InstanceID); err != nil {
		t.Fatalf("set acknowledged fixture: %v", err)
	}
	if _, err := db.db.Exec(`CREATE TRIGGER fail_restart_stamp BEFORE INSERT ON metadata
		WHEN NEW.key = 'last_modified'
		BEGIN SELECT RAISE(ABORT, 'forced restart stamp failure'); END`); err != nil {
		t.Fatalf("create metadata failure trigger: %v", err)
	}

	if _, err := db.WriteRestartOutcome(expected, incarnation); err == nil {
		t.Fatal("WriteRestartOutcome succeeded though the reload stamp was rejected")
	}
	statuses, err := db.ReadAllStatuses()
	if err != nil {
		t.Fatalf("ReadAllStatuses: %v", err)
	}
	if !statuses[expected.InstanceID].Acknowledged {
		t.Fatal("acknowledgement cleared despite stamp failure: update and signal were not atomic")
	}
}
