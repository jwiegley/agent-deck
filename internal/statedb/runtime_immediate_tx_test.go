package statedb

import (
	"database/sql"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRuntimeLifecycle_ImmediateTransactionsExcludeCompetingWriter(t *testing.T) {
	tests := []struct {
		name   string
		run    func(*StateDB, RuntimeState, string) error
		verify func(*testing.T, *StateDB)
	}{
		{
			name: "transition",
			run: func(db *StateDB, expected RuntimeState, incarnation string) error {
				return db.CommitRuntimeTransition(expected.Generation, incarnation, RuntimeState{
					InstanceID: "one", Generation: expected.Generation + 1,
					TmuxSession: "runtime-next", TmuxSocketName: "socket-next",
					Status: "starting", LastStartedAt: time.Unix(2, 3).UTC(),
				})
			},
			verify: func(t *testing.T, db *StateDB) {
				state, found, err := db.ReadRuntimeState("one")
				if err != nil || !found || state.Generation != 1 || state.TmuxSession != "runtime-next" {
					t.Fatalf("transition state=%#v found=%v err=%v", state, found, err)
				}
			},
		},
		{
			name: "status",
			run: func(db *StateDB, expected RuntimeState, incarnation string) error {
				applied, err := db.WriteStatusIfVersion("one", incarnation, expected.Generation, expected.StatusRevision, "running")
				if err == nil && !applied {
					return fmt.Errorf("status CAS was not applied")
				}
				return err
			},
			verify: func(t *testing.T, db *StateDB) {
				state, found, err := db.ReadRuntimeState("one")
				if err != nil || !found || state.Status != "running" || state.StatusRevision != 1 {
					t.Fatalf("status state=%#v found=%v err=%v", state, found, err)
				}
			},
		},
		{
			name: "binding",
			run: func(db *StateDB, expected RuntimeState, incarnation string) error {
				_, err := db.CommitRuntimeBinding("one", incarnation, expected.Generation, "claude", 0, "conversation")
				return err
			},
			verify: func(t *testing.T, db *StateDB) {
				binding, found, err := db.ReadRuntimeBinding("one", "claude")
				if err != nil || !found || binding.Revision != 1 || binding.Value != "conversation" {
					t.Fatalf("binding=%#v found=%v err=%v", binding, found, err)
				}
			},
		},
		{
			name: "archive",
			run: func(db *StateDB, expected RuntimeState, incarnation string) error {
				return db.SetArchivedIfRuntime(expected, incarnation, time.Unix(9, 0).UTC())
			},
			verify: func(t *testing.T, db *StateDB) {
				row, err := db.LoadInstanceByID("one")
				if err != nil || row == nil || !row.ArchivedAt.Equal(time.Unix(9, 0).UTC()) {
					t.Fatalf("archived row=%#v err=%v", row, err)
				}
			},
		},
		{
			name: "delete",
			run: func(db *StateDB, expected RuntimeState, incarnation string) error {
				return db.DeleteInstanceIfRuntime(expected, incarnation)
			},
			verify: func(t *testing.T, db *StateDB) {
				row, err := db.LoadInstanceByID("one")
				if err != nil || row != nil {
					t.Fatalf("deleted row=%#v err=%v", row, err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := newRuntimeTestDB(t)
			parent := &InstanceRow{
				ID: "one", Title: "one", ProjectPath: "/tmp/one", GroupPath: "my-sessions",
				Tool: "claude", Status: "idle", CreatedAt: time.Unix(1, 0).UTC(),
			}
			if err := db.SaveInstance(parent); err != nil {
				t.Fatal(err)
			}
			expected, found, err := db.ReadRuntimeState("one")
			if err != nil || !found {
				t.Fatalf("initial runtime found=%v err=%v", found, err)
			}

			competitor, err := sql.Open("sqlite", db.path+"?_pragma=busy_timeout(0)&_pragma=foreign_keys(on)")
			if err != nil {
				t.Fatal(err)
			}
			defer competitor.Close()

			began := make(chan struct{})
			release := make(chan struct{})
			released := false
			defer func() {
				if !released {
					close(release)
				}
			}()
			var barrier sync.Once
			var attempts atomic.Int32
			db.testAfterImmediateBegin = func() {
				attempts.Add(1)
				barrier.Do(func() {
					close(began)
					<-release
				})
			}

			done := make(chan error, 1)
			go func() { done <- tt.run(db, expected, parent.Incarnation) }()
			select {
			case <-began:
			case err := <-done:
				t.Fatalf("target returned before BEGIN IMMEDIATE barrier: %v", err)
			}
			if _, err := competitor.Exec(`UPDATE metadata SET value = value WHERE key = 'schema_version'`); !isSQLiteBusy(err) {
				t.Fatalf("competing writer error = %v, want SQLITE_BUSY", err)
			}
			close(release)
			released = true
			if err := <-done; err != nil {
				t.Fatalf("target transaction: %v", err)
			}
			db.testAfterImmediateBegin = nil
			if got := attempts.Load(); got != 1 {
				t.Fatalf("transaction attempts = %d, want 1", got)
			}
			tt.verify(t, db)
		})
	}
}
