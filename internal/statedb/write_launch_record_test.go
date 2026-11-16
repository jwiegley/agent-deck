package statedb

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestLaunchRecordRequiresCurrentRuntimeAuthority(t *testing.T) {
	for name, write := range map[string]func(*StateDB, RuntimeState, string, string, bool) (WriteStamps, error){
		"start":   (*StateDB).WriteLaunchDecision,
		"restart": (*StateDB).WriteRestartOutcomeWithLaunch,
	} {
		t.Run(name, func(t *testing.T) {
			db := newTestDB(t)
			expected, incarnation := committedRestartOutcome(t, db, "launch-record")
			if _, err := write(db, expected, incarnation, "pi", true); err != nil {
				t.Fatal(err)
			}
			// Status samples do not replace the physical launch.
			if ok, err := db.WriteStatusIfVersion(expected.InstanceID, incarnation, expected.Generation, expected.StatusRevision, "running"); err != nil || !ok {
				t.Fatalf("status sample: ok=%v err=%v", ok, err)
			}
			if _, err := write(db, expected, incarnation, "pi", false); err != nil {
				t.Fatalf("status-only update rejected launch record: %v", err)
			}
			if _, err := write(db, expected, incarnation+"-stale", "claude", false); !errors.Is(err, ErrInstanceParentConflict) {
				t.Fatalf("stale parent error = %v", err)
			}
			next, found, err := db.ReadRuntimeState(expected.InstanceID)
			if err != nil || !found {
				t.Fatalf("read runtime: found=%v err=%v", found, err)
			}
			next.Generation++ // respawn-pane keeps the same tmux name
			next.StatusRevision = 0
			if err := db.CommitRuntimeTransition(expected.Generation, incarnation, next); err != nil {
				t.Fatal(err)
			}
			before, err := db.LastModified()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := write(db, expected, incarnation, "claude", true); !errors.Is(err, ErrRuntimeGenerationConflict) {
				t.Fatalf("stale same-name launch error = %v", err)
			}
			after, err := db.LastModified()
			if err != nil || after != before {
				t.Fatalf("rejected write changed stamp: before=%d after=%d err=%v", before, after, err)
			}
			rows, err := db.LoadInstances()
			if err != nil || len(rows) != 1 {
				t.Fatalf("load records: rows=%d err=%v", len(rows), err)
			}
			var data map[string]any
			if err := json.Unmarshal(rows[0].ToolData, &data); err != nil {
				t.Fatal(err)
			}
			if data["launched_tool"] != "pi" || data["pi_socket_exported"] != false {
				t.Fatalf("rejected write changed launch record: %v", data)
			}
			if rows[0].RuntimeGeneration != next.Generation || rows[0].Tool != "claude" {
				t.Fatalf("launch record changed runtime or metadata: %+v", rows[0])
			}
		})
	}
}

func TestLaunchRecordFailureRollsBackRuntimePublication(t *testing.T) {
	db := newTestDB(t)
	previous, incarnation := committedRestartOutcome(t, db, "atomic-launch")
	if _, err := db.WriteLaunchDecision(previous, incarnation, "pi", true); err != nil {
		t.Fatal(err)
	}
	before, err := db.LoadInstanceByID(previous.InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	stamp, err := db.LastModified()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`CREATE TRIGGER reject_launch BEFORE UPDATE OF tool_data ON instances
	    WHEN json_extract(NEW.tool_data, '$.launched_tool') = 'claude'
	    BEGIN SELECT RAISE(ABORT, 'injected launch failure'); END`); err != nil {
		t.Fatal(err)
	}
	next := previous
	next.Generation++
	publication, err := db.CommitRuntimeTransitionWithLaunch(previous.Generation, incarnation, next, nil, &RuntimeLaunchDecision{Tool: "claude"})
	if err == nil || publication != (RuntimePublication{}) {
		t.Fatalf("rejected launch publication=%+v err=%v", publication, err)
	}
	state, found, err := db.ReadRuntimeState(previous.InstanceID)
	if err != nil || !found || state != previous {
		t.Fatalf("failed launch advanced runtime: %+v found=%v err=%v", state, found, err)
	}
	after, err := db.LoadInstanceByID(previous.InstanceID)
	if err != nil || string(after.ToolData) != string(before.ToolData) {
		t.Fatalf("failed launch changed stored metadata: %v", err)
	}
	if current, err := db.LastModified(); err != nil || current != stamp {
		t.Fatalf("failed launch changed stamp: %d/%d err=%v", stamp, current, err)
	}
	if _, err := db.db.Exec(`DROP TRIGGER reject_launch`); err != nil {
		t.Fatal(err)
	}
	publication, err = db.CommitRuntimeTransitionWithLaunch(previous.Generation, incarnation, next, nil, &RuntimeLaunchDecision{Tool: "claude"})
	if err != nil || !publication.LaunchRecorded || publication.Stamps.After <= stamp {
		t.Fatalf("retried publication=%+v err=%v", publication, err)
	}
}
