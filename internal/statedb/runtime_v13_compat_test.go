package statedb

import (
	"database/sql"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// v13SchemaFixtureSQL is the complete schema emitted by SchemaVersion 13,
// copied from that revision's Migrate implementation. In particular, it has
// neither the v15 writer epoch column nor either authoritative runtime table.
const v13SchemaFixtureSQL = `
CREATE TABLE metadata (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE instances (
	id                    TEXT PRIMARY KEY,
	title                 TEXT NOT NULL,
	project_path          TEXT NOT NULL,
	group_path            TEXT NOT NULL DEFAULT 'my-sessions',
	sort_order            INTEGER NOT NULL DEFAULT 0,
	command               TEXT NOT NULL DEFAULT '',
	wrapper               TEXT NOT NULL DEFAULT '',
	tool                  TEXT NOT NULL DEFAULT 'shell',
	status                TEXT NOT NULL DEFAULT 'error',
	tmux_session          TEXT NOT NULL DEFAULT '',
	tmux_socket_name      TEXT NOT NULL DEFAULT '',
	created_at            INTEGER NOT NULL,
	last_accessed         INTEGER NOT NULL DEFAULT 0,
	parent_session_id     TEXT NOT NULL DEFAULT '',
	is_conductor          INTEGER NOT NULL DEFAULT 0,
	no_transition_notify  INTEGER NOT NULL DEFAULT 0,
	title_locked          INTEGER NOT NULL DEFAULT 0,
	worktree_path         TEXT NOT NULL DEFAULT '',
	worktree_repo         TEXT NOT NULL DEFAULT '',
	worktree_branch       TEXT NOT NULL DEFAULT '',
	account               TEXT NOT NULL DEFAULT '',
	archived_at           INTEGER NOT NULL DEFAULT 0,
	auto_name             INTEGER NOT NULL DEFAULT 0,
	auto_name_description TEXT NOT NULL DEFAULT '',
	pin                   TEXT NOT NULL DEFAULT '',
	last_sent_at          INTEGER NOT NULL DEFAULT 0,
	tool_data             TEXT NOT NULL DEFAULT '{}',
	acknowledged          INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE groups (
	path           TEXT PRIMARY KEY,
	name           TEXT NOT NULL,
	expanded       INTEGER NOT NULL DEFAULT 1,
	sort_order     INTEGER NOT NULL DEFAULT 0,
	default_path   TEXT NOT NULL DEFAULT '',
	max_concurrent INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE instance_heartbeats (
	pid        INTEGER PRIMARY KEY,
	started    INTEGER NOT NULL,
	heartbeat  INTEGER NOT NULL,
	is_primary INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE session_claims (
	session_id  TEXT PRIMARY KEY,
	owner_pid   INTEGER NOT NULL,
	owner_token TEXT NOT NULL DEFAULT '',
	claimed_at  INTEGER NOT NULL,
	heartbeat   INTEGER NOT NULL,
	scope       TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_session_claims_owner ON session_claims(owner_token);
CREATE TABLE recent_sessions (
	id              TEXT PRIMARY KEY,
	title           TEXT NOT NULL,
	project_path    TEXT NOT NULL,
	group_path      TEXT NOT NULL DEFAULT '',
	command         TEXT NOT NULL DEFAULT '',
	wrapper         TEXT NOT NULL DEFAULT '',
	tool            TEXT NOT NULL DEFAULT '',
	tool_options    TEXT NOT NULL DEFAULT '{}',
	sandbox_enabled INTEGER NOT NULL DEFAULT 0,
	gemini_yolo     INTEGER,
	deleted_at      INTEGER NOT NULL
);
CREATE TABLE cost_events (
	id                    TEXT PRIMARY KEY,
	session_id            TEXT NOT NULL,
	timestamp             DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	model                 TEXT NOT NULL,
	input_tokens          INTEGER NOT NULL DEFAULT 0,
	output_tokens         INTEGER NOT NULL DEFAULT 0,
	cache_read_tokens     INTEGER NOT NULL DEFAULT 0,
	cache_write_tokens    INTEGER NOT NULL DEFAULT 0,
	cost_microdollars     INTEGER NOT NULL DEFAULT 0,
	budget_stop_triggered INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_cost_events_session ON cost_events(session_id);
CREATE INDEX idx_cost_events_timestamp ON cost_events(timestamp);
CREATE TABLE watchers (
	id          TEXT PRIMARY KEY,
	name        TEXT UNIQUE NOT NULL,
	type        TEXT NOT NULL,
	config_path TEXT NOT NULL,
	status      TEXT NOT NULL DEFAULT 'stopped',
	conductor   TEXT NOT NULL DEFAULT '',
	created_at  INTEGER NOT NULL,
	updated_at  INTEGER NOT NULL
);
CREATE TABLE watcher_events (
	id                INTEGER PRIMARY KEY AUTOINCREMENT,
	watcher_id        TEXT NOT NULL REFERENCES watchers(id),
	dedup_key         TEXT NOT NULL,
	sender            TEXT NOT NULL DEFAULT '',
	subject           TEXT NOT NULL DEFAULT '',
	routed_to         TEXT NOT NULL DEFAULT '',
	session_id        TEXT NOT NULL DEFAULT '',
	triage_session_id TEXT NOT NULL DEFAULT '',
	body              TEXT NOT NULL DEFAULT '',
	created_at        INTEGER NOT NULL,
	UNIQUE(watcher_id, dedup_key)
);
CREATE INDEX idx_watcher_events_watcher_created
	ON watcher_events(watcher_id, created_at DESC);
`

const v13LegacyToolData = `{
  "unrelated": {"spacing": "must remain byte-for-byte", "unicode": "π"},
  "last_started_at": 1700000000,
  "claude_session_id": "legacy-claude",
  "claude_detected_at": 1700000011,
  "copilot_session_id": "legacy-copilot",
  "copilot_detected_at": 1700000012,
  "codex_session_id": "legacy-codex",
  "codex_detected_at": 1700000013,
  "gemini_session_id": "legacy-gemini",
  "gemini_detected_at": 1700000014,
  "opencode_session_id": "legacy-opencode",
  "opencode_detected_at": 1700000015
}`

func TestRuntimeLifecycle_V13FullSchemaMigrationCompatibility(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state-v13.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	installV13SchemaFixture(t, db.DB())
	seedV13SchemaFixture(t, db.DB())
	before := snapshotV13Payload(t, db.DB())

	if err := db.Migrate(); err != nil {
		t.Fatalf("migrate complete v13 fixture: %v", err)
	}
	if got := snapshotV13Payload(t, db.DB()); got != before {
		t.Fatalf("v13 payload changed during migration\nbefore:\n%s\nafter:\n%s", before, got)
	}
	assertV13ReadQueries(t, db.DB())

	var schemaVersion string
	if err := db.DB().QueryRow(`SELECT value FROM metadata WHERE key = 'schema_version'`).Scan(&schemaVersion); err != nil {
		t.Fatal(err)
	}
	if schemaVersion != strconv.Itoa(SchemaVersion) {
		t.Fatalf("schema version = %q, want %d", schemaVersion, SchemaVersion)
	}
	var writerVersion int
	if err := db.DB().QueryRow(`SELECT writer_schema_version FROM instance_heartbeats WHERE pid = 2147483647`).Scan(&writerVersion); err != nil {
		t.Fatal(err)
	}
	if writerVersion != 0 {
		t.Fatalf("legacy heartbeat writer version = %d, want 0", writerVersion)
	}

	legacyState := RuntimeState{
		InstanceID: "v13-instance", TmuxSession: "legacy-tmux",
		TmuxSocketName: "legacy-socket", Status: "waiting",
		LastStartedAt: time.Unix(1700000000, 0).UTC(),
	}
	assertV13RuntimeState(t, db, legacyState)
	for i, kind := range bindingKinds {
		binding, found, err := db.ReadRuntimeBinding("v13-instance", kind)
		if err != nil {
			t.Fatal(err)
		}
		if !found || binding.Generation != 0 || binding.Revision != 0 ||
			binding.Value != "legacy-"+kind ||
			!binding.DetectedAt.Equal(time.Unix(int64(1700000011+i), 0).UTC()) {
			t.Fatalf("imported %s binding = %#v, found=%v", kind, binding, found)
		}
	}

	// Advance both forms of authoritative runtime revision beyond anything a
	// v13 writer knew how to represent.
	nextState := RuntimeState{
		InstanceID: "v13-instance", Generation: 1, StatusRevision: 41,
		TmuxSession: "runtime-g1", TmuxSocketName: "runtime-socket-g1",
		Status: "running", LastStartedAt: time.Unix(1800000000, 987654321).UTC(),
	}
	plan := make([]RuntimeBindingTransition, 0, len(bindingKinds))
	for i, kind := range bindingKinds {
		plan = append(plan, RuntimeBindingTransition{
			Kind: kind, ExpectedRevision: 0, NextValue: "current-" + kind,
			DetectedAt: time.Unix(int64(1800000011+i), 123).UTC(),
		})
	}
	incarnation := runtimeTestIncarnation(t, db, "v13-instance")
	if err := db.CommitRuntimeTransitionWithBindingPlan(0, incarnation, nextState, plan); err != nil {
		t.Fatalf("advance migrated runtime: %v", err)
	}
	newestBindings := make(map[string]string, len(bindingKinds))
	for _, kind := range bindingKinds {
		newestBindings[kind] = "newest-" + kind
		binding, err := db.CommitRuntimeBinding("v13-instance", incarnation, 1, kind, 1, newestBindings[kind])
		if err != nil {
			t.Fatalf("advance %s binding: %v", kind, err)
		}
		if binding.Generation != 1 || binding.Revision != 2 || binding.Value != newestBindings[kind] {
			t.Fatalf("advanced %s binding = %#v", kind, binding)
		}
	}

	// Idempotent migration must never use the compatibility projection to
	// reset an already-newer authoritative runtime or binding row.
	if err := db.Migrate(); err != nil {
		t.Fatalf("rerun migration: %v", err)
	}
	assertV13RuntimeState(t, db, nextState)
	assertV13Bindings(t, db, 1, 2, newestBindings)

	// This is the exact 26-column INSERT OR REPLACE issued by a v13 full save.
	// It deliberately carries stale, non-empty values for every binding.
	staleToolData := `{
		"claude_session_id":"stale-claude",
		"copilot_session_id":"stale-copilot",
		"codex_session_id":"stale-codex",
		"gemini_session_id":"stale-gemini",
		"opencode_session_id":"stale-opencode",
		"last_started_at":1,
		"unrelated":"written-by-v13"
	}`
	if _, err := db.DB().Exec(`
		INSERT OR REPLACE INTO instances (
			id, title, project_path, group_path, sort_order,
			command, wrapper, tool, status, tmux_session, tmux_socket_name,
			created_at, last_accessed,
			parent_session_id, is_conductor, no_transition_notify,
			worktree_path, worktree_repo, worktree_branch, account,
			archived_at, tool_data, title_locked, auto_name, auto_name_description, pin
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"v13-instance", "stale v13 title", "/stale/project", "v13/group", 91,
		"stale command", "stale wrapper", "claude", "error", "stale-tmux", "stale-socket",
		901, 902, "stale-parent", 0, 0, "/stale/worktree", "/stale/repo", "stale-branch", "stale-account",
		903, staleToolData, 0, 0, "stale description", "bottom",
	); err != nil {
		t.Fatalf("v13 stale full save: %v", err)
	}

	assertV13RuntimeState(t, db, nextState)
	assertV13Bindings(t, db, 1, 2, newestBindings)
	loaded, err := db.LoadInstances()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 {
		t.Fatalf("LoadInstances returned %d rows, want 1", len(loaded))
	}
	if loaded[0].Title != "stale v13 title" || loaded[0].Status != nextState.Status ||
		loaded[0].TmuxSession != nextState.TmuxSession ||
		loaded[0].TmuxSocketName != nextState.TmuxSocketName ||
		loaded[0].RuntimeGeneration != nextState.Generation ||
		loaded[0].StatusRevision != nextState.StatusRevision {
		t.Fatalf("current load did not overlay authoritative runtime: %#v", loaded[0])
	}
	for _, kind := range bindingKinds {
		if got := loaded[0].RuntimeBindings[kind]; got.Value != newestBindings[kind] || got.Revision != 2 {
			t.Fatalf("loaded %s binding = %#v", kind, got)
		}
	}
}

func installV13SchemaFixture(t *testing.T, db *sql.DB) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, statement := range strings.Split(v13SchemaFixtureSQL, ";") {
		statement = strings.TrimSpace(statement)
		if statement == "" {
			continue
		}
		if _, err := tx.Exec(statement); err != nil {
			t.Fatalf("install v13 schema with %q: %v", statement, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func seedV13SchemaFixture(t *testing.T, db *sql.DB) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	mustExec := func(query string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(query, args...); err != nil {
			t.Fatalf("seed v13 fixture: %v\nquery: %s", err, query)
		}
	}

	mustExec(`INSERT INTO metadata(key, value) VALUES ('schema_version', '13')`)
	// SQLite retains the bound BLOB storage class despite the column's TEXT
	// affinity. This catches accidental text decoding or normalization.
	mustExec(`INSERT INTO metadata(key, value) VALUES ('unrelated-bytes', ?)`, []byte{0x00, 0x01, 0x7f, 0x80, 0xfe, 0xff})
	mustExec(`
		INSERT INTO instances (
			id, title, project_path, group_path, sort_order, command, wrapper, tool,
			status, tmux_session, tmux_socket_name, created_at, last_accessed,
			parent_session_id, is_conductor, no_transition_notify, title_locked,
			worktree_path, worktree_repo, worktree_branch, account, archived_at,
			auto_name, auto_name_description, pin, last_sent_at, tool_data, acknowledged
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		          ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"v13-instance", "v13 title", "/project/☃", "v13/group", 17,
		"printf 'legacy'", "env V13=1", "claude", "waiting", "legacy-tmux", "legacy-socket",
		101, 102, "v13-parent", 1, 1, 1, "/v13/worktree", "/v13/repo", "v13-branch",
		"v13-account", 103, 1, "v13 automatic description", "top", 104, v13LegacyToolData, 1,
	)
	mustExec(`INSERT INTO groups(path, name, expanded, sort_order, default_path, max_concurrent)
		VALUES ('v13/group', 'V13 group', 0, 21, '/v13/default', 7)`)
	// The maximum signed 32-bit PID is deliberately dead, so the v15 mixed-
	// writer fence sees and preserves the legacy epoch-zero row without
	// treating it as an active incompatible process.
	mustExec(`INSERT INTO instance_heartbeats(pid, started, heartbeat, is_primary)
		VALUES (2147483647, 201, 202, 1)`)
	mustExec(`INSERT INTO session_claims(session_id, owner_pid, owner_token, claimed_at, heartbeat, scope)
		VALUES ('v13-instance', 2147483647, 'v13-owner-token', 301, 302, 'v13/group')`)
	mustExec(`INSERT INTO recent_sessions(
		id, title, project_path, group_path, command, wrapper, tool, tool_options,
		sandbox_enabled, gemini_yolo, deleted_at)
		VALUES ('recent-v13', 'Recent v13', '/recent/project', 'v13/group', 'recent command',
		'recent wrapper', 'gemini', '{"legacy_option":"keep"}', 1, 0, 401)`)
	mustExec(`INSERT INTO cost_events(
		id, session_id, timestamp, model, input_tokens, output_tokens, cache_read_tokens,
		cache_write_tokens, cost_microdollars, budget_stop_triggered)
		VALUES ('cost-v13', 'v13-instance', '2026-01-02 03:04:05.678901+00:00',
		'legacy-model', 501, 502, 503, 504, 505, 1)`)
	mustExec(`INSERT INTO watchers(id, name, type, config_path, status, conductor, created_at, updated_at)
		VALUES ('watcher-v13', 'Watcher v13', 'mail', '/v13/watcher.json', 'running',
		'v13-instance', 601, 602)`)
	mustExec(`INSERT INTO watcher_events(
		id, watcher_id, dedup_key, sender, subject, routed_to, session_id,
		triage_session_id, body, created_at)
		VALUES (7, 'watcher-v13', 'dedup-v13', 'sender-v13', 'subject-v13',
		'route-v13', 'v13-instance', 'triage-v13', 'body v13 π', 701)`)

	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// snapshotV13Payload reads every v13 application table through an explicit
// v13 column projection. The schema version and the new trailing heartbeat
// column are the only fields expected to differ after migration, so neither is
// included. Values are encoded with their SQLite/driver type and raw bytes.
func snapshotV13Payload(t *testing.T, db *sql.DB) string {
	t.Helper()
	queries := []struct {
		name  string
		query string
	}{
		{"metadata", `SELECT key, value FROM metadata WHERE key <> 'schema_version' ORDER BY key`},
		{"instances", `SELECT id, title, project_path, group_path, sort_order, command, wrapper, tool,
			status, tmux_session, tmux_socket_name, created_at, last_accessed, parent_session_id,
			is_conductor, no_transition_notify, title_locked, worktree_path, worktree_repo,
			worktree_branch, account, archived_at, auto_name, auto_name_description, pin,
			last_sent_at, tool_data, acknowledged FROM instances ORDER BY id`},
		{"groups", `SELECT path, name, expanded, sort_order, default_path, max_concurrent FROM groups ORDER BY path`},
		{"instance_heartbeats", `SELECT pid, started, heartbeat, is_primary FROM instance_heartbeats ORDER BY pid`},
		{"session_claims", `SELECT session_id, owner_pid, owner_token, claimed_at, heartbeat, scope FROM session_claims ORDER BY session_id`},
		{"recent_sessions", `SELECT id, title, project_path, group_path, command, wrapper, tool, tool_options,
			sandbox_enabled, gemini_yolo, deleted_at FROM recent_sessions ORDER BY id`},
		{"cost_events", `SELECT id, session_id, timestamp, model, input_tokens, output_tokens,
			cache_read_tokens, cache_write_tokens, cost_microdollars, budget_stop_triggered
			FROM cost_events ORDER BY id`},
		{"watchers", `SELECT id, name, type, config_path, status, conductor, created_at, updated_at FROM watchers ORDER BY id`},
		{"watcher_events", `SELECT id, watcher_id, dedup_key, sender, subject, routed_to, session_id,
			triage_session_id, body, created_at FROM watcher_events ORDER BY id`},
		{"sqlite_sequence", `SELECT name, seq FROM sqlite_sequence ORDER BY name`},
	}
	var out strings.Builder
	for _, item := range queries {
		encoded, rows := snapshotSQLRows(t, db, item.query)
		if rows == 0 {
			t.Fatalf("v13 fixture table %s was not seeded", item.name)
		}
		fmt.Fprintf(&out, "%s\n%s", item.name, encoded)
	}
	return out.String()
}

// assertV13ReadQueries executes the principal SELECTs compiled into the v13
// binary after the additive migration. Keeping these literal catches an
// ostensibly additive schema change that nevertheless breaks an old reader.
func assertV13ReadQueries(t *testing.T, db *sql.DB) {
	t.Helper()
	queries := []struct {
		name  string
		query string
		args  []any
	}{
		{"LoadInstances", `SELECT id, title, project_path, group_path, sort_order,
			command, wrapper, tool, status, tmux_session, tmux_socket_name,
			created_at, last_accessed, parent_session_id, is_conductor, no_transition_notify,
			worktree_path, worktree_repo, worktree_branch, account, archived_at, tool_data,
			title_locked, auto_name, auto_name_description, pin FROM instances ORDER BY sort_order`, nil},
		{"ReadAllStatuses", `SELECT id, status, tool, acknowledged FROM instances`, nil},
		{"ReadLastSentAt", `SELECT last_sent_at FROM instances WHERE id = ?`, []any{"v13-instance"}},
		{"InstanceExists", `SELECT 1 FROM instances WHERE id = ? LIMIT 1`, []any{"v13-instance"}},
		{"SaveInstance tool_data prefetch", `SELECT tool_data FROM instances WHERE id = ?`, []any{"v13-instance"}},
		{"SaveInstance auto-name prefetch", `SELECT auto_name, auto_name_description FROM instances WHERE id = ?`, []any{"v13-instance"}},
		{"LoadGroups", `SELECT path, name, expanded, sort_order, default_path, max_concurrent FROM groups ORDER BY sort_order`, nil},
		{"AliveInstanceCount", `SELECT COUNT(*) FROM instance_heartbeats WHERE heartbeat >= ?`, []any{0}},
		{"ElectPrimary", `SELECT pid FROM instance_heartbeats WHERE is_primary = 1 AND heartbeat >= ? LIMIT 1`, []any{0}},
		{"LoadClaims", `SELECT session_id, owner_pid, owner_token, scope, heartbeat FROM session_claims`, nil},
		{"LoadRecentSessions", `SELECT id, title, project_path, group_path, command, wrapper, tool,
			tool_options, sandbox_enabled, gemini_yolo, deleted_at FROM recent_sessions ORDER BY deleted_at DESC`, nil},
		{"LoadCostEventsForSession", `SELECT id, session_id, timestamp, model, input_tokens, output_tokens,
			cache_read_tokens, cache_write_tokens, cost_microdollars, budget_stop_triggered
			FROM cost_events WHERE session_id = ? ORDER BY timestamp`, []any{"v13-instance"}},
		{"LoadWatchers", `SELECT id, name, type, config_path, status, conductor, created_at, updated_at FROM watchers ORDER BY name`, nil},
		{"LoadWatcherEvents", `SELECT id, watcher_id, dedup_key, sender, subject, routed_to, session_id, body, created_at
			FROM watcher_events WHERE watcher_id = ? ORDER BY created_at DESC LIMIT ?`, []any{"watcher-v13", 10}},
	}
	for _, item := range queries {
		if _, rows := snapshotSQLRows(t, db, item.query, item.args...); rows == 0 {
			t.Errorf("v13 %s SELECT returned no rows", item.name)
		}
	}
}

func snapshotSQLRows(t *testing.T, db *sql.DB, query string, args ...any) (string, int) {
	t.Helper()
	rows, err := db.Query(query, args...)
	if err != nil {
		t.Fatalf("legacy SELECT failed: %v\nquery: %s", err, query)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	fmt.Fprintf(&out, "columns=%q\n", columns)
	rowCount := 0
	for rows.Next() {
		values := make([]any, len(columns))
		dest := make([]any, len(columns))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			t.Fatalf("scan legacy SELECT: %v\nquery: %s", err, query)
		}
		rowCount++
		for i, value := range values {
			if i > 0 {
				out.WriteByte('|')
			}
			out.WriteString(encodeSQLValue(value))
		}
		out.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate legacy SELECT: %v\nquery: %s", err, query)
	}
	return out.String(), rowCount
}

func encodeSQLValue(value any) string {
	switch value := value.(type) {
	case nil:
		return "nil"
	case int64:
		return "int:" + strconv.FormatInt(value, 10)
	case float64:
		return "float:" + strconv.FormatFloat(value, 'g', -1, 64)
	case bool:
		return "bool:" + strconv.FormatBool(value)
	case []byte:
		return "bytes:" + hex.EncodeToString(value)
	case string:
		return "string:" + hex.EncodeToString([]byte(value))
	case time.Time:
		return "time:" + value.UTC().Format(time.RFC3339Nano)
	default:
		return fmt.Sprintf("%T:%v", value, value)
	}
}

func assertV13RuntimeState(t *testing.T, db *StateDB, want RuntimeState) {
	t.Helper()
	got, found, err := db.ReadRuntimeState(want.InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	if !found || got.InstanceID != want.InstanceID || got.Generation != want.Generation ||
		got.StatusRevision != want.StatusRevision || got.TmuxSession != want.TmuxSession ||
		got.TmuxSocketName != want.TmuxSocketName || got.Status != want.Status ||
		!got.LastStartedAt.Equal(want.LastStartedAt) {
		t.Fatalf("runtime state = %#v, found=%v; want %#v", got, found, want)
	}
}

func assertV13Bindings(t *testing.T, db *StateDB, generation, revision uint64, values map[string]string) {
	t.Helper()
	for _, kind := range bindingKinds {
		got, found, err := db.ReadRuntimeBinding("v13-instance", kind)
		if err != nil {
			t.Fatal(err)
		}
		if !found || got.Generation != generation || got.Revision != revision || got.Value != values[kind] || got.Value == "" {
			t.Fatalf("%s binding = %#v, found=%v; want generation=%d revision=%d value=%q",
				kind, got, found, generation, revision, values[kind])
		}
	}
}
