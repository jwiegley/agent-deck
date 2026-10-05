package statedb

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

var migrationRetryBusy = errors.New("database is locked (SQLITE_BUSY)")

func openMigrationRetryTestDB(t *testing.T) *StateDB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestRuntimeLifecycle_InsertCostEventRowForMigration_RetryClearsInsertedToken(t *testing.T) {
	db := openMigrationRetryTestDB(t)
	event := &CostEventRow{
		ID: "retry-cost", SessionID: "session", Timestamp: "2026-08-07T12:00:00Z",
		Model: "model", CostMicrodollars: 42,
	}
	attempts := 0
	commit := func(tx *sql.Tx) error {
		attempts++
		if attempts > 1 {
			return tx.Commit()
		}
		if err := tx.Rollback(); err != nil {
			return err
		}
		if err := db.InsertCostEventRow(event); err != nil {
			return err
		}
		return migrationRetryBusy
	}

	inserted, err := db.insertCostEventRowForMigration(event, commit)
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("commit attempts = %d, want 2", attempts)
	}
	if inserted != nil {
		t.Fatalf("retry returned stale inserted token: %+v", inserted)
	}
	rows, err := db.LoadCostEventsForSession(event.SessionID)
	if err != nil || len(rows) != 1 || !sameCostEvent(rows[0], event) {
		t.Fatalf("foreign cost row = %+v, err=%v", rows, err)
	}
}

func TestRuntimeLifecycle_InsertWatcherEventRowForMigration_RetryClearsInsertedToken(t *testing.T) {
	db := openMigrationRetryTestDB(t)
	const watcherID = "retry-watcher"
	if err := db.SaveWatcher(&WatcherRow{
		ID: watcherID, Name: "retry watcher", Type: "github", ConfigPath: "/tmp/retry.toml",
		Status: "stopped", CreatedAt: time.Unix(1700000000, 0), UpdatedAt: time.Unix(1700000000, 0),
	}); err != nil {
		t.Fatal(err)
	}
	event := &WatcherEventRow{
		WatcherID: watcherID, DedupKey: "retry-dedup", SessionID: "session",
		Body: "body", CreatedAt: time.Unix(1700000001, 0),
	}
	attempts := 0
	commit := func(tx *sql.Tx) error {
		attempts++
		if attempts > 1 {
			return tx.Commit()
		}
		if err := tx.Rollback(); err != nil {
			return err
		}
		if err := db.InsertWatcherEventRow(event); err != nil {
			return err
		}
		return migrationRetryBusy
	}

	inserted, err := db.insertWatcherEventRowForMigration(event, commit)
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("commit attempts = %d, want 2", attempts)
	}
	if inserted != nil {
		t.Fatalf("retry returned stale inserted token: %+v", inserted)
	}
	rows, err := db.LoadWatcherEventsForSession(event.SessionID)
	if err != nil || len(rows) != 1 || !sameWatcherEventPayload(rows[0], event) {
		t.Fatalf("foreign watcher row = %+v, err=%v", rows, err)
	}
}

func TestRuntimeLifecycle_WithMigrationTargetLeaseHoldsDestinationThroughSourceDelete(t *testing.T) {
	tmp := t.TempDir()
	open := func(name string) *StateDB {
		t.Helper()
		db, err := Open(filepath.Join(tmp, name))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		if err := db.Migrate(); err != nil {
			t.Fatal(err)
		}
		return db
	}

	src := open("source.db")
	dst := open("target.db")
	contender := open("target.db")
	row := &InstanceRow{
		ID: "leased-migration", Title: "leased migration", ProjectPath: "/tmp/project",
		GroupPath: "my-sessions", Tool: "pi", Status: "idle", CreatedAt: time.Unix(1700000000, 0),
	}
	if err := src.SaveInstance(row); err != nil {
		t.Fatal(err)
	}
	snapshot, err := src.LoadMigrationSnapshot(row.ID)
	if err != nil {
		t.Fatal(err)
	}
	inserted, err := dst.InsertInstanceRowForMigration(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if inserted.incarnation == "" || inserted.incarnation == snapshot.incarnation ||
		inserted.origin != snapshot.incarnation {
		t.Fatalf("migration identity: source=%q target=%q origin=%q",
			snapshot.incarnation, inserted.incarnation, inserted.origin)
	}

	ctx := context.Background()
	conn, err := contender.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "PRAGMA busy_timeout = 1"); err != nil {
		t.Fatal(err)
	}

	if err := dst.WithMigrationTargetLease(snapshot, func() error {
		// A second destination writer cannot enter while the source CAS runs.
		if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); !isSQLiteBusy(err) {
			return errors.New("destination writer acquired the migration lease")
		}
		if err := src.DeleteMigrationSource(snapshot); err != nil {
			return err
		}
		remaining, err := src.LoadInstanceByID(row.ID)
		if err != nil {
			return err
		}
		if remaining != nil {
			return errors.New("source row survived migration deletion")
		}
		// The destination must still be protected after source deletion.
		if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); !isSQLiteBusy(err) {
			return errors.New("destination lease ended before source deletion returned")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// The writer slot is released when WithMigrationTargetLease returns.
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("destination writer remained blocked after lease release: %v", err)
	}
	if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	target, err := dst.LoadInstanceByID(row.ID)
	if err != nil || target == nil {
		t.Fatalf("migration target missing after protected source deletion: row=%+v err=%v", target, err)
	}
}

func TestRuntimeLifecycle_RollbackMigrationTargetRejectsByteIdenticalReinsert(t *testing.T) {
	src := openMigrationRetryTestDB(t)
	dst := openMigrationRetryTestDB(t)
	row := &InstanceRow{
		ID: "migration-aba", Title: "byte identical", ProjectPath: "/tmp/migration-aba",
		GroupPath: "my-sessions", Tool: "pi", Status: "idle",
		CreatedAt: time.Unix(1700000000, 0),
	}
	if err := src.SaveInstance(row); err != nil {
		t.Fatal(err)
	}
	source, err := src.LoadMigrationSnapshot(row.ID)
	if err != nil {
		t.Fatal(err)
	}
	first, err := dst.InsertInstanceRowForMigration(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := dst.DeleteInstance(row.ID); err != nil {
		t.Fatal(err)
	}
	second, err := dst.InsertInstanceRowForMigration(source)
	if err != nil {
		t.Fatal(err)
	}
	if first.incarnation == second.incarnation || first.origin != source.incarnation || second.origin != source.incarnation {
		t.Fatalf("migration reinsert identities: source=%q first=%q/%q second=%q/%q",
			source.incarnation, first.incarnation, first.origin, second.incarnation, second.origin)
	}
	if err := dst.RollbackMigrationTarget(first, nil, nil); !errors.Is(err, ErrMigrationSnapshotConflict) {
		t.Fatalf("old rollback error = %v, want snapshot conflict", err)
	}
	current, err := dst.LoadMigrationSnapshot(row.ID)
	if err != nil || !sameMigrationCore(current, second) {
		t.Fatalf("replacement after stale rollback = %#v err=%v; want %#v", current, err, second)
	}
}

func TestRuntimeLifecycle_WithMigrationTargetLeaseDistinguishesTargetConflict(t *testing.T) {
	src := openMigrationRetryTestDB(t)
	dst := openMigrationRetryTestDB(t)
	row := &InstanceRow{
		ID: "missing-target", Title: "missing target", ProjectPath: "/tmp/project",
		GroupPath: "my-sessions", Tool: "pi", Status: "idle", CreatedAt: time.Unix(1700000000, 0),
	}
	if err := src.SaveInstance(row); err != nil {
		t.Fatal(err)
	}
	snapshot, err := src.LoadMigrationSnapshot(row.ID)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	err = dst.WithMigrationTargetLease(snapshot, func() error {
		called = true
		return nil
	})
	if !errors.Is(err, ErrMigrationTargetConflict) {
		t.Fatalf("WithMigrationTargetLease error = %v, want target conflict", err)
	}
	if called {
		t.Fatal("source operation ran after target validation conflict")
	}
}
