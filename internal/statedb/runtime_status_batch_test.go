package statedb

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestRuntimeLifecycle_StatusBatchCASMissRollsBackEveryRow(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"first", "stale"} {
		if err := db.SaveInstance(&InstanceRow{
			ID: id, Title: id, ProjectPath: "/tmp", Tool: "claude",
			Status: "error", CreatedAt: time.Unix(1000, 0).UTC(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	firstIncarnation := runtimeTestIncarnation(t, db, "first")
	staleIncarnation := runtimeTestIncarnation(t, db, "stale")
	applied, err := db.WriteStatusIfVersion("stale", staleIncarnation, 0, 0, "waiting")
	if err != nil || !applied {
		t.Fatalf("seed winner CAS = applied %v, err %v", applied, err)
	}

	err = db.PersistInstanceStatusesTx([]InstanceStatusUpdate{
		{ID: "first", Incarnation: firstIncarnation, Status: "running", Generation: 0, StatusRevision: 0, Versioned: true},
		{ID: "stale", Incarnation: staleIncarnation, Status: "running", Generation: 0, StatusRevision: 0, Versioned: true},
	})
	if !errors.Is(err, ErrStatusRevisionConflict) {
		t.Fatalf("PersistInstanceStatusesTx error = %v, want status revision conflict", err)
	}
	first, found, err := db.ReadRuntimeState("first")
	if err != nil || !found {
		t.Fatalf("read first = found %v, err %v", found, err)
	}
	if first.Status != "error" || first.StatusRevision != 0 {
		t.Fatalf("first row partially committed: %+v", first)
	}
	stale, found, err := db.ReadRuntimeState("stale")
	if err != nil || !found {
		t.Fatalf("read stale = found %v, err %v", found, err)
	}
	if stale.Status != "waiting" || stale.StatusRevision != 1 {
		t.Fatalf("winner row changed: %+v", stale)
	}
}

func TestRuntimeLifecycle_StatusBatchRejectsByteIdenticalABAAtomically(t *testing.T) {
	db, aba := newByteIdenticalRuntimeABA(t)
	valid := &InstanceRow{
		ID: "valid-batch-row", Incarnation: "valid-batch-incarnation",
		Title: "valid", ProjectPath: "/tmp/valid", GroupPath: "my-sessions",
		Tool: "claude", Status: "error", CreatedAt: time.Unix(1000, 0).UTC(),
	}
	if err := db.SaveInstance(valid); err != nil {
		t.Fatal(err)
	}

	err := db.PersistInstanceStatusesTx([]InstanceStatusUpdate{
		{
			ID: valid.ID, Incarnation: valid.Incarnation, Status: "running",
			Generation: 0, StatusRevision: 0, Versioned: true,
		},
		{
			ID: aba.instanceID, Incarnation: aba.incarnationA, Status: "waiting",
			Generation: aba.runtime.Generation, StatusRevision: aba.runtime.StatusRevision,
			Versioned: true,
		},
	})
	if !errors.Is(err, ErrInstanceParentConflict) {
		t.Fatalf("PersistInstanceStatusesTx error = %v, want parent conflict", err)
	}
	validState, found, err := db.ReadRuntimeState(valid.ID)
	if err != nil || !found || validState.Status != "error" || validState.StatusRevision != 0 {
		t.Fatalf("valid row partially committed: state=%#v found=%v err=%v", validState, found, err)
	}
	winner, found, err := db.ReadRuntimeState(aba.instanceID)
	if err != nil || !found || winner != aba.runtime {
		t.Fatalf("B runtime changed: state=%#v found=%v err=%v; want %#v", winner, found, err, aba.runtime)
	}
}
