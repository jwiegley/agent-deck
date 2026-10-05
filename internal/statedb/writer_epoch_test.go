package statedb

import (
	"errors"
	"os"
	"testing"
	"time"
)

func TestRuntimeLifecycle_WriterCompatibilitySamePIDAndDeadRows(t *testing.T) {
	tests := []struct {
		name    string
		pid     func(*StateDB) int
		version int
		wantErr bool
	}{
		{name: "current writer", pid: func(db *StateDB) int { return db.pid }, version: SchemaVersion},
		{name: "same pid legacy row", pid: func(db *StateDB) int { return db.pid }, version: 0},
		{name: "same pid future writer", pid: func(db *StateDB) int { return db.pid }, version: SchemaVersion + 1, wantErr: true},
		{name: "dead legacy writer", pid: func(*StateDB) int { return 1 << 30 }, version: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := newRuntimeTestDB(t)
			now := time.Now().Unix()
			if _, err := db.DB().Exec(`INSERT INTO instance_heartbeats
				(pid, started, heartbeat, is_primary, writer_schema_version)
				VALUES (?, ?, ?, 0, ?)`, tt.pid(db), now, now, tt.version); err != nil {
				t.Fatal(err)
			}
			err := db.RequireRuntimeWriterCompatibility()
			if errors.Is(err, ErrIncompatibleWriterSchema) != tt.wantErr {
				t.Fatalf("RequireRuntimeWriterCompatibility() error = %v, want incompatible=%v", err, tt.wantErr)
			}
		})
	}
}

func TestRuntimeLifecycle_WriterCompatibilityUsesProcessStartToken(t *testing.T) {
	currentToken, err := durableWriterProcessStartToken(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		token   string
		wantErr bool
	}{
		{name: "matching token fences", token: currentToken, wantErr: true},
		{name: "mismatched token is stale", token: currentToken + "-old"},
		{name: "tokenless live writer fences", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := newRuntimeTestDB(t)
			db.pid = 0 // Make the test process a foreign writer to this handle.
			now := time.Now().Unix()
			if _, err := db.DB().Exec(`INSERT INTO instance_heartbeats
				(pid, started, heartbeat, is_primary, writer_schema_version, writer_process_start_token)
				VALUES (?, ?, ?, 0, 0, ?)`, os.Getpid(), now, now, tt.token); err != nil {
				t.Fatal(err)
			}
			err := db.RequireRuntimeWriterCompatibility()
			if errors.Is(err, ErrIncompatibleWriterSchema) != tt.wantErr {
				t.Fatalf("RequireRuntimeWriterCompatibility() error = %v, want incompatible=%v", err, tt.wantErr)
			}
		})
	}
}

func TestRuntimeLifecycle_LinuxWriterTokenIncludesBootID(t *testing.T) {
	got, err := qualifyWriterProcessStartToken("linux", "boot-123", "456")
	if err != nil {
		t.Fatal(err)
	}
	if got != "boot-123:456" {
		t.Fatalf("qualified Linux writer token = %q, want %q", got, "boot-123:456")
	}
	if _, err := qualifyWriterProcessStartToken("linux", "", "456"); err == nil {
		t.Fatal("empty Linux boot ID unexpectedly accepted")
	}
	got, err = qualifyWriterProcessStartToken("darwin", "", "123:456")
	if err != nil || got != "123:456" {
		t.Fatalf("Darwin writer token = %q, error = %v", got, err)
	}
}

func TestRuntimeLifecycle_WriterCompatibilityIdentityProbeErrorFences(t *testing.T) {
	db := newRuntimeTestDB(t)
	db.pid = 0 // Make the test process a foreign writer to this handle.
	now := time.Now().Unix()
	if _, err := db.DB().Exec(`INSERT INTO instance_heartbeats
		(pid, started, heartbeat, is_primary, writer_schema_version, writer_process_start_token)
		VALUES (?, ?, ?, 0, 0, 'recorded')`, os.Getpid(), now, now); err != nil {
		t.Fatal(err)
	}

	originalRead := readWriterProcessStartToken
	readWriterProcessStartToken = func(int) (string, error) {
		return "", errors.New("identity probe failed")
	}
	t.Cleanup(func() { readWriterProcessStartToken = originalRead })

	if err := db.RequireRuntimeWriterCompatibility(); !errors.Is(err, ErrIncompatibleWriterSchema) {
		t.Fatalf("RequireRuntimeWriterCompatibility() error = %v, want ErrIncompatibleWriterSchema", err)
	}
}

func TestRuntimeLifecycle_WriterCompatibilityMissingTokenOwnerIsStale(t *testing.T) {
	db := newRuntimeTestDB(t)
	now := time.Now().Unix()
	if _, err := db.DB().Exec(`INSERT INTO instance_heartbeats
		(pid, started, heartbeat, is_primary, writer_schema_version, writer_process_start_token)
		VALUES (?, ?, ?, 0, 0, 'old-boot:123')`, 1<<30, now, now); err != nil {
		t.Fatal(err)
	}
	if err := db.RequireRuntimeWriterCompatibility(); err != nil {
		t.Fatalf("dead token-bearing writer must be stale: %v", err)
	}
}

func TestRuntimeLifecycle_LegacyWriterOmissionRecordsEpochZero(t *testing.T) {
	db := newRuntimeTestDB(t)
	now := time.Now().Unix()
	if _, err := db.DB().Exec(`INSERT OR REPLACE INTO instance_heartbeats
		(pid, started, heartbeat, is_primary) VALUES (?, ?, ?, 0)`, db.pid, now, now); err != nil {
		t.Fatal(err)
	}
	var version int
	if err := db.DB().QueryRow(`SELECT writer_schema_version FROM instance_heartbeats WHERE pid = ?`, db.pid).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 0 {
		t.Fatalf("legacy writer epoch = %d, want 0", version)
	}
	if err := db.RequireRuntimeWriterCompatibility(); err != nil {
		t.Fatalf("same-PID pre-registration row must be stale: %v", err)
	}
}

func TestRuntimeLifecycle_RegisterHeartbeatRecordsWriterIdentity(t *testing.T) {
	db := newRuntimeTestDB(t)
	if err := db.RegisterInstance(false); err != nil {
		t.Fatal(err)
	}
	var registeredToken string
	if err := db.DB().QueryRow(`SELECT writer_process_start_token
		FROM instance_heartbeats WHERE pid = ?`, db.pid).Scan(&registeredToken); err != nil {
		t.Fatal(err)
	}
	if registeredToken == "" {
		t.Fatal("RegisterInstance stored an empty writer process start token")
	}
	if _, err := db.DB().Exec(`UPDATE instance_heartbeats
		SET writer_schema_version = 0, writer_process_start_token = '' WHERE pid = ?`, db.pid); err != nil {
		t.Fatal(err)
	}
	if err := db.Heartbeat(); err != nil {
		t.Fatal(err)
	}
	var version int
	var startToken string
	if err := db.DB().QueryRow(`SELECT writer_schema_version, writer_process_start_token
		FROM instance_heartbeats WHERE pid = ?`, db.pid).Scan(&version, &startToken); err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersion {
		t.Fatalf("writer schema = %d, want %d", version, SchemaVersion)
	}
	identity, err := durableWriterProcessStartToken(db.pid)
	if err != nil {
		t.Fatal(err)
	}
	if startToken != identity {
		t.Fatalf("writer start token = %q, want %q", startToken, identity)
	}
	if startToken != registeredToken {
		t.Fatalf("Heartbeat token = %q, want registered token %q", startToken, registeredToken)
	}
}

func TestRuntimeLifecycle_HeartbeatRepairsMissingRegistration(t *testing.T) {
	db := newRuntimeTestDB(t)
	if err := db.RegisterInstance(false); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().Exec(`DELETE FROM instance_heartbeats WHERE pid = ?`, db.pid); err != nil {
		t.Fatal(err)
	}
	if err := db.Heartbeat(); err != nil {
		t.Fatal(err)
	}
	var primary, version int
	var startToken string
	if err := db.DB().QueryRow(`SELECT is_primary, writer_schema_version, writer_process_start_token
		FROM instance_heartbeats WHERE pid = ?`, db.pid).Scan(&primary, &version, &startToken); err != nil {
		t.Fatal(err)
	}
	if primary != 0 || version != SchemaVersion || startToken == "" {
		t.Fatalf("repaired heartbeat primary=%d schema=%d start token=%q", primary, version, startToken)
	}
}

func TestRuntimeLifecycle_MigrateAddsWriterProcessStartToken(t *testing.T) {
	db := createV1SchemaDB(t)
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	if _, err := db.DB().Exec(`INSERT INTO instance_heartbeats
		(pid, started, heartbeat, is_primary) VALUES (?, ?, ?, 0)`, 1<<30, now, now); err != nil {
		t.Fatal(err)
	}
	var token string
	if err := db.DB().QueryRow(`SELECT writer_process_start_token
		FROM instance_heartbeats WHERE pid = ?`, 1<<30).Scan(&token); err != nil {
		t.Fatal(err)
	}
	if token != "" {
		t.Fatalf("legacy writer start token = %q, want empty additive default", token)
	}
}

func TestRuntimeLifecycle_CleanupRetainsStaleLiveWriter(t *testing.T) {
	db := newRuntimeTestDB(t)
	now := time.Now().Unix()
	if _, err := db.DB().Exec(`INSERT INTO instance_heartbeats
		(pid, started, heartbeat, is_primary, writer_schema_version)
		VALUES (?, ?, ?, 0, 0)`, db.pid, now-3600, now-3600); err != nil {
		t.Fatal(err)
	}
	if err := db.CleanDeadInstances(30 * time.Second); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := db.DB().QueryRow(`SELECT COUNT(*) FROM instance_heartbeats WHERE pid = ?`, db.pid).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatal("cleanup deleted a stale but live writer")
	}
}

func TestRuntimeLifecycle_RegisterRetriesLockedDatabase(t *testing.T) {
	db := newRuntimeTestDB(t)
	db.DB().SetMaxOpenConns(1)
	if _, err := db.DB().Exec(`PRAGMA busy_timeout = 1`); err != nil {
		t.Fatal(err)
	}
	blocker, err := Open(db.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blocker.Close() })
	tx, err := blocker.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE metadata SET value = value WHERE key = 'schema_version'`); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := db.RegisterInstance(false); !isSQLiteBusy(err) {
		_ = tx.Rollback()
		t.Fatalf("RegisterInstance error = %v, want SQLITE_BUSY under sustained lock", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := db.RegisterInstance(false); err != nil {
		t.Fatalf("RegisterInstance after unlock: %v", err)
	}
}

func TestRuntimeLifecycle_MigrateRejectsFreshLegacyWriterBeforeRuntimeTables(t *testing.T) {
	db := newRuntimeTestDB(t)
	if _, err := db.DB().Exec(`DROP TABLE instance_runtime_binding`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().Exec(`DROP TABLE instance_runtime_state`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().Exec(`UPDATE metadata SET value = ? WHERE key = 'schema_version'`, SchemaVersion-2); err != nil {
		t.Fatal(err)
	}
	db.pid = 0 // Make the live test process a foreign, legacy writer.
	now := time.Now().Unix()
	if _, err := db.DB().Exec(`INSERT INTO instance_heartbeats
		(pid, started, heartbeat, is_primary, writer_schema_version)
		VALUES (?, ?, ?, 0, 0)`, os.Getpid(), now, now); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(); !errors.Is(err, ErrIncompatibleWriterSchema) {
		t.Fatalf("Migrate() error = %v, want ErrIncompatibleWriterSchema", err)
	}
	var runtimeTables int
	if err := db.DB().QueryRow(`SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name IN ('instance_runtime_state', 'instance_runtime_binding')`).Scan(&runtimeTables); err != nil {
		t.Fatal(err)
	}
	if runtimeTables != 0 {
		t.Fatalf("Migrate() created %d runtime tables despite incompatible writer", runtimeTables)
	}
}
