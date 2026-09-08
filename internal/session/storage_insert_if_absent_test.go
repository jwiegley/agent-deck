package session

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

func newInsertIfAbsentStorage(t *testing.T) *Storage {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "state.db")
	db, err := statedb.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &Storage{db: db, dbPath: dbPath, profile: "_insert_if_absent"}
}

func TestRuntimeLifecycle_InsertSessionIfAbsentDoesNotClobberSameIDWinner(t *testing.T) {
	storage := newInsertIfAbsentStorage(t)
	winner := &Instance{
		ID: "same-id", Title: "concurrent winner", ProjectPath: "/tmp/winner",
		GroupPath: DefaultGroupPath, Tool: "pi", Status: StatusStopped,
		CreatedAt: time.Unix(2, 0).UTC(), Notes: "winner metadata",
	}
	if err := storage.InsertSessionAndVerify(winner, nil); err != nil {
		t.Fatalf("seed winner: %v", err)
	}
	winnerRuntime, found, err := storage.db.ReadRuntimeState(winner.ID)
	if err != nil || !found {
		t.Fatalf("winner runtime = %#v, found=%v err=%v", winnerRuntime, found, err)
	}
	winnerRow, err := storage.db.LoadInstanceByID(winner.ID)
	if err != nil || winnerRow == nil || winnerRow.Incarnation == "" {
		t.Fatalf("winner incarnation row = %#v, err=%v", winnerRow, err)
	}
	winnerIncarnation := winnerRow.Incarnation

	stale := &Instance{
		ID: winner.ID, Title: "stale deleted snapshot", ProjectPath: "/tmp/stale",
		GroupPath: DefaultGroupPath, Tool: "claude", Status: StatusError,
		CreatedAt: time.Unix(1, 0).UTC(), Notes: "must not win",
	}
	if _, err := storage.InsertSessionIfAbsent(stale); !errors.Is(err, ErrSessionAlreadyExists) {
		t.Fatalf("InsertSessionIfAbsent error = %v, want session already exists", err)
	}
	stale.mu.RLock()
	staleOwner := stale.owningDB
	stale.mu.RUnlock()
	if staleOwner != storage.db {
		t.Fatalf("losing restore owningDB = %p, want storage db %p", staleOwner, storage.db)
	}
	row, err := storage.db.LoadInstanceByID(winner.ID)
	if err != nil || row == nil {
		t.Fatalf("winner row = %#v, err=%v", row, err)
	}
	if row.Title != winner.Title || row.ProjectPath != winner.ProjectPath || row.Tool != winner.Tool {
		t.Fatalf("same-ID winner was clobbered: %#v", row)
	}
	if row.Incarnation != winnerIncarnation {
		t.Fatalf("winner incarnation changed: got %q, want %q", row.Incarnation, winnerIncarnation)
	}
	afterRuntime, found, err := storage.db.ReadRuntimeState(winner.ID)
	if err != nil || !found || afterRuntime != winnerRuntime {
		t.Fatalf("winner runtime after stale restore = %#v, found=%v err=%v; want %#v",
			afterRuntime, found, err, winnerRuntime)
	}
}

func TestRuntimeLifecycle_InsertSessionIfAbsentCreatesOnlyMissingLogicalRow(t *testing.T) {
	storage := newInsertIfAbsentStorage(t)
	restored := &Instance{
		ID: "restored", Title: "restored", ProjectPath: "/tmp/restored",
		GroupPath: DefaultGroupPath, Tool: "pi", Status: StatusStopped,
		CreatedAt: time.Unix(1, 0).UTC(),
	}
	seed, err := storage.InsertSessionIfAbsent(restored)
	if err != nil {
		t.Fatalf("InsertSessionIfAbsent: %v", err)
	}
	if row, err := storage.db.LoadInstanceByID(restored.ID); err != nil || row == nil {
		t.Fatalf("restored logical row = %#v, err=%v", row, err)
	}
	if seed.Incarnation == "" || restored.PersistenceIncarnation() != seed.Incarnation {
		t.Fatalf("in-memory incarnation = %q, seed = %q", restored.PersistenceIncarnation(), seed.Incarnation)
	}
	if runtime, found, err := storage.db.ReadRuntimeState(restored.ID); err != nil || !found || runtime != seed.Runtime {
		t.Fatalf("atomic seed runtime = %#v, found=%v err=%v; want %#v", runtime, found, err, seed.Runtime)
	}
	loaded, err := storage.Load()
	if err != nil || len(loaded) != 1 || loaded[0].PersistenceIncarnation() != seed.Incarnation {
		t.Fatalf("loaded incarnation: instances=%#v err=%v; want %q", loaded, err, seed.Incarnation)
	}
}

func TestRuntimeLifecycle_ReinsertDeletedSessionRotatesIncarnation(t *testing.T) {
	storage := newInsertIfAbsentStorage(t)
	inst := &Instance{
		ID: "undo-reinsert", Title: "undo reinsert", ProjectPath: "/tmp/undo-reinsert",
		GroupPath: DefaultGroupPath, Tool: "pi", Status: StatusStopped,
		CreatedAt: time.Unix(1, 0).UTC(),
	}
	first, err := storage.InsertSessionIfAbsent(inst)
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.DeleteInstance(inst.ID); err != nil {
		t.Fatal(err)
	}

	reinserted, err := storage.ReinsertDeletedSession(inst)
	if err != nil {
		t.Fatal(err)
	}
	if reinserted.Incarnation == "" || reinserted.Incarnation == first.Incarnation ||
		inst.PersistenceIncarnation() != reinserted.Incarnation {
		t.Fatalf("reinsert incarnation=%q instance=%q deleted=%q, want a fresh shared token",
			reinserted.Incarnation, inst.PersistenceIncarnation(), first.Incarnation)
	}

	applied, err := storage.db.WriteStatusIfVersion(
		inst.ID, first.Incarnation, reinserted.Runtime.Generation,
		reinserted.Runtime.StatusRevision, string(StatusRunning))
	if !errors.Is(err, statedb.ErrInstanceParentConflict) || applied {
		t.Fatalf("stale status write applied=%v err=%v, want parent conflict", applied, err)
	}
	durable, found, readErr := storage.db.ReadRuntimeState(inst.ID)
	if readErr != nil || !found || durable != reinserted.Runtime {
		t.Fatalf("reinserted runtime=%#v found=%v err=%v, want unchanged %#v",
			durable, found, readErr, reinserted.Runtime)
	}
}

func TestRuntimeLifecycle_RollbackSessionSeedPreservesRuntimeAndParentWinners(t *testing.T) {
	t.Run("unchanged seed is removed", func(t *testing.T) {
		storage := newInsertIfAbsentStorage(t)
		inst := &Instance{
			ID: "unchanged", Title: "unchanged", ProjectPath: "/tmp/unchanged",
			GroupPath: DefaultGroupPath, Tool: "pi", Status: StatusStopped,
			CreatedAt: time.Unix(1, 0).UTC(),
		}
		seed, err := storage.InsertSessionIfAbsent(inst)
		if err != nil {
			t.Fatal(err)
		}
		if err := storage.RollbackSessionSeed(seed); err != nil {
			t.Fatal(err)
		}
		if row, err := storage.db.LoadInstanceByID(inst.ID); err != nil || row != nil {
			t.Fatalf("parent after rollback = %#v, err=%v", row, err)
		}
	})

	t.Run("runtime transition wins", func(t *testing.T) {
		storage := newInsertIfAbsentStorage(t)
		inst := &Instance{
			ID: "runtime-winner", Title: "seed", ProjectPath: "/tmp/runtime",
			GroupPath: DefaultGroupPath, Tool: "pi", Status: StatusStopped,
			CreatedAt: time.Unix(1, 0).UTC(),
		}
		seed, err := storage.InsertSessionIfAbsent(inst)
		if err != nil {
			t.Fatal(err)
		}
		winner := seed.Runtime
		winner.Generation++
		winner.StatusRevision = 0
		winner.TmuxSession = "winner"
		winner.Status = string(StatusRunning)
		if err := storage.db.CommitRuntimeTransition(seed.Runtime.Generation, seed.Incarnation, winner); err != nil {
			t.Fatal(err)
		}
		if err := storage.RollbackSessionSeed(seed); !errors.Is(err, statedb.ErrRuntimeGenerationConflict) {
			t.Fatalf("rollback error = %v, want generation conflict", err)
		}
		if row, err := storage.db.LoadInstanceByID(inst.ID); err != nil || row == nil {
			t.Fatalf("winner parent = %#v, err=%v", row, err)
		}
		if got, found, err := storage.db.ReadRuntimeState(inst.ID); err != nil || !found || got != winner {
			t.Fatalf("winner runtime = %#v, found=%v err=%v; want %#v", got, found, err, winner)
		}
	})

	t.Run("binding update wins", func(t *testing.T) {
		storage := newInsertIfAbsentStorage(t)
		inst := &Instance{
			ID: "binding-winner", Title: "seed", ProjectPath: "/tmp/binding",
			GroupPath: DefaultGroupPath, Tool: "claude", Status: StatusStopped,
			CreatedAt: time.Unix(1, 0).UTC(),
		}
		seed, err := storage.InsertSessionIfAbsent(inst)
		if err != nil {
			t.Fatal(err)
		}
		binding, err := storage.db.CommitRuntimeBinding(inst.ID, seed.Incarnation, seed.Runtime.Generation, "claude", 0, "winner-binding")
		if err != nil {
			t.Fatal(err)
		}
		if err := storage.RollbackSessionSeed(seed); !errors.Is(err, statedb.ErrRuntimeGenerationConflict) {
			t.Fatalf("rollback error = %v, want runtime conflict", err)
		}
		if got, found, err := storage.db.ReadRuntimeBinding(inst.ID, "claude"); err != nil || !found || got != binding {
			t.Fatalf("winner binding = %#v found=%v err=%v; want %#v", got, found, err, binding)
		}
		if row, err := storage.db.LoadInstanceByID(inst.ID); err != nil || row == nil {
			t.Fatalf("winner parent = %#v err=%v", row, err)
		}
	})

	t.Run("parent update wins", func(t *testing.T) {
		storage := newInsertIfAbsentStorage(t)
		inst := &Instance{
			ID: "parent-winner", Title: "seed", ProjectPath: "/tmp/parent",
			GroupPath: DefaultGroupPath, Tool: "pi", Status: StatusStopped,
			CreatedAt: time.Unix(1, 0).UTC(),
		}
		seed, err := storage.InsertSessionIfAbsent(inst)
		if err != nil {
			t.Fatal(err)
		}
		row, err := instanceToRow(inst)
		if err != nil {
			t.Fatal(err)
		}
		row.Title = "concurrent winner"
		if err := storage.db.SaveInstance(row); err != nil {
			t.Fatal(err)
		}
		if err := storage.RollbackSessionSeed(seed); !errors.Is(err, statedb.ErrInstanceParentConflict) {
			t.Fatalf("rollback error = %v, want parent conflict", err)
		}
		got, err := storage.db.LoadInstanceByID(inst.ID)
		if err != nil || got == nil || got.Title != row.Title {
			t.Fatalf("winner parent = %#v, err=%v", got, err)
		}
	})

	t.Run("byte-identical delete and reinsert wins", func(t *testing.T) {
		storage := newInsertIfAbsentStorage(t)
		newInstance := func() *Instance {
			return &Instance{
				ID: "parent-aba", Title: "identical", ProjectPath: "/tmp/parent-aba",
				GroupPath: DefaultGroupPath, Tool: "pi", Status: StatusStopped,
				CreatedAt: time.Unix(10, 0).UTC(), LastAccessedAt: time.Unix(20, 0).UTC(),
				Notes: "byte-identical metadata",
			}
		}
		original := newInstance()
		originalSeed, err := storage.InsertSessionIfAbsent(original)
		if err != nil {
			t.Fatal(err)
		}
		if err := storage.DeleteInstance(original.ID); err != nil {
			t.Fatal(err)
		}
		winner := newInstance()
		winnerSeed, err := storage.InsertSessionIfAbsent(winner)
		if err != nil {
			t.Fatal(err)
		}
		if winnerSeed.Incarnation == originalSeed.Incarnation {
			t.Fatalf("reinsert reused incarnation %q", winnerSeed.Incarnation)
		}
		if err := storage.RollbackSessionSeed(originalSeed); !errors.Is(err, statedb.ErrInstanceParentConflict) {
			t.Fatalf("old rollback error = %v, want parent conflict", err)
		}
		row, err := storage.db.LoadInstanceByID(winner.ID)
		if err != nil || row == nil || row.Incarnation != winnerSeed.Incarnation {
			t.Fatalf("byte-identical winner row = %#v, err=%v", row, err)
		}
		runtime, found, err := storage.db.ReadRuntimeState(winner.ID)
		if err != nil || !found || runtime != winnerSeed.Runtime {
			t.Fatalf("byte-identical winner runtime = %#v, found=%v err=%v; want %#v", runtime, found, err, winnerSeed.Runtime)
		}
	})
}

func TestRuntimeLifecycle_InsertSessionAndVerifyDoesNotResurrectPostInsertDelete(t *testing.T) {
	storage := newInsertIfAbsentStorage(t)
	const id = "delete-after-insert"
	if _, err := storage.db.DB().Exec(`
		CREATE TABLE insert_attempts (instance_id TEXT NOT NULL);
		CREATE TRIGGER count_target_insert AFTER INSERT ON instances
		WHEN NEW.id = 'delete-after-insert'
		BEGIN
			INSERT INTO insert_attempts(instance_id) VALUES (NEW.id);
		END;
		CREATE TRIGGER delete_target_after_group AFTER INSERT ON groups
		WHEN NEW.path = 'delete-trigger'
		BEGIN
			DELETE FROM instance_runtime_binding WHERE instance_id = 'delete-after-insert';
			DELETE FROM instance_runtime_state WHERE instance_id = 'delete-after-insert';
			DELETE FROM instances WHERE id = 'delete-after-insert';
		END;
	`); err != nil {
		t.Fatal(err)
	}
	inst := &Instance{
		ID: id, Title: "deleted winner", ProjectPath: "/tmp/delete-after-insert",
		GroupPath: "delete-trigger", Tool: "pi", Status: StatusStopped,
		CreatedAt: time.Unix(1, 0).UTC(),
	}
	if err := storage.InsertSessionAndVerify(inst, NewGroupTree([]*Instance{inst})); err != nil {
		t.Fatalf("InsertSessionAndVerify: %v", err)
	}
	var attempts int
	if err := storage.db.DB().QueryRow(`SELECT COUNT(*) FROM insert_attempts WHERE instance_id = ?`, id).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 {
		t.Fatalf("parent insert attempts = %d, want exactly 1", attempts)
	}
	if row, err := storage.db.LoadInstanceByID(id); err != nil || row != nil {
		t.Fatalf("parent after later delete = %#v, err=%v", row, err)
	}
	if runtime, found, err := storage.db.ReadRuntimeState(id); err != nil || found {
		t.Fatalf("runtime after later delete = %#v, found=%v err=%v", runtime, found, err)
	}
}
