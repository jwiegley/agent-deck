package statedb

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const runtimeLifecycleDBHelperEnv = "AGENTDECK_RUNTIME_LIFECYCLE_DB_HELPER"

const runtimeLifecycleLegacyToolData = `{"last_started_at":"2026-08-07T12:34:56Z","notes":{"keep":"byte-exact"},"claude_session_id":"claude-legacy","claude_detected_at":101,"copilot_session_id":"copilot-legacy","copilot_detected_at":102,"codex_session_id":"codex-legacy","codex_detected_at":103,"gemini_session_id":"gemini-legacy","gemini_detected_at":104,"opencode_session_id":"opencode-legacy","opencode_detected_at":105}`

type runtimeLifecycleChild struct {
	cmd     *exec.Cmd
	output  bytes.Buffer
	ready   *os.File
	release *os.File
}

func startRuntimeLifecycleChild(t *testing.T, mode, dbPath, resultPath, value string, barrier bool) *runtimeLifecycleChild {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := &runtimeLifecycleChild{}
	child.cmd = exec.Command(executable, "-test.run=^TestRuntimeLifecycle_MultiprocessSQLite$")
	child.cmd.Env = append(os.Environ(),
		runtimeLifecycleDBHelperEnv+"="+mode,
		"AGENTDECK_RUNTIME_LIFECYCLE_DB_PATH="+dbPath,
		"AGENTDECK_RUNTIME_LIFECYCLE_RESULT="+resultPath,
		"AGENTDECK_RUNTIME_LIFECYCLE_VALUE="+value,
	)
	child.cmd.Stdout = &child.output
	child.cmd.Stderr = &child.output
	if barrier {
		readyRead, readyWrite, pipeErr := os.Pipe()
		if pipeErr != nil {
			t.Fatal(pipeErr)
		}
		releaseRead, releaseWrite, pipeErr := os.Pipe()
		if pipeErr != nil {
			_ = readyRead.Close()
			_ = readyWrite.Close()
			t.Fatal(pipeErr)
		}
		child.ready = readyRead
		child.release = releaseWrite
		child.cmd.ExtraFiles = []*os.File{readyWrite, releaseRead}
		if err := child.cmd.Start(); err != nil {
			t.Fatal(err)
		}
		_ = readyWrite.Close()
		_ = releaseRead.Close()
	} else if err := child.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if child.cmd.Process != nil {
			_ = child.cmd.Process.Kill()
		}
		if child.ready != nil {
			_ = child.ready.Close()
		}
		if child.release != nil {
			_ = child.release.Close()
		}
	})
	return child
}

func (c *runtimeLifecycleChild) waitReady(t *testing.T) {
	t.Helper()
	var signal [1]byte
	if _, err := io.ReadFull(c.ready, signal[:]); err != nil {
		_ = c.cmd.Wait()
		t.Fatalf("helper did not reach barrier: %v\n%s", err, c.output.String())
	}
}

func (c *runtimeLifecycleChild) releaseBarrier(t *testing.T) {
	t.Helper()
	if _, err := c.release.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	_ = c.release.Close()
	c.release = nil
}

func (c *runtimeLifecycleChild) wait(t *testing.T) {
	t.Helper()
	if err := c.cmd.Wait(); err != nil {
		t.Fatalf("helper failed: %v\n%s", err, c.output.String())
	}
}

func runtimeLifecycleChildBarrier(t *testing.T) {
	t.Helper()
	ready := os.NewFile(3, "runtime-lifecycle-ready")
	release := os.NewFile(4, "runtime-lifecycle-release")
	if ready == nil || release == nil {
		t.Fatal("runtime lifecycle helper barrier is absent")
	}
	defer ready.Close()
	defer release.Close()
	if _, err := ready.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	var signal [1]byte
	if _, err := io.ReadFull(release, signal[:]); err != nil {
		t.Fatal(err)
	}
}

func writeRuntimeLifecycleResult(t *testing.T, value string) {
	t.Helper()
	if err := os.WriteFile(os.Getenv("AGENTDECK_RUNTIME_LIFECYCLE_RESULT"), []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

func openRuntimeLifecycleDB(t *testing.T) *StateDB {
	t.Helper()
	db, err := Open(os.Getenv("AGENTDECK_RUNTIME_LIFECYCLE_DB_PATH"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func runRuntimeLifecycleDBHelper(t *testing.T, mode string) {
	t.Helper()
	switch mode {
	case "migrate":
		db := openRuntimeLifecycleDB(t)
		runtimeLifecycleChildBarrier(t)
		if err := db.Migrate(); err != nil {
			if isSQLiteBusy(err) {
				writeRuntimeLifecycleResult(t, "busy")
				return
			}
			t.Fatal(err)
		}
		writeRuntimeLifecycleResult(t, "ok")
	case "transition":
		db := openRuntimeLifecycleDB(t)
		incarnation := runtimeTestIncarnation(t, db, "one")
		runtimeLifecycleChildBarrier(t)
		value := os.Getenv("AGENTDECK_RUNTIME_LIFECYCLE_VALUE")
		err := db.CommitRuntimeTransition(0, incarnation, RuntimeState{
			InstanceID: "one", Generation: 1, TmuxSession: value,
			TmuxSocketName: "isolated", Status: "starting", LastStartedAt: time.Unix(200, 0).UTC(),
		})
		switch {
		case err == nil:
			writeRuntimeLifecycleResult(t, "won:"+value)
		case errors.Is(err, ErrRuntimeGenerationConflict):
			writeRuntimeLifecycleResult(t, "conflict")
		default:
			t.Fatal(err)
		}
	case "stale-metadata":
		db := openRuntimeLifecycleDB(t)
		runtimeLifecycleChildBarrier(t)
		err := db.UpsertInstances([]*InstanceRow{{
			ID: "one", Title: "metadata-won", ProjectPath: "/tmp/one", GroupPath: "my-sessions",
			Tool: "claude", Status: "error", TmuxSession: "stale-tmux", CreatedAt: time.Unix(1, 0),
			ToolData: json.RawMessage(`{"claude_session_id":"old","notes":"metadata-won"}`),
		}})
		if err != nil {
			t.Fatal(err)
		}
		writeRuntimeLifecycleResult(t, "metadata")
	case "stale-status":
		db := openRuntimeLifecycleDB(t)
		incarnation := runtimeTestIncarnation(t, db, "one")
		runtimeLifecycleChildBarrier(t)
		applied, err := db.WriteStatusIfVersion("one", incarnation, 0, 0, "error")
		if err != nil || applied {
			t.Fatalf("stale status applied=%v err=%v", applied, err)
		}
		writeRuntimeLifecycleResult(t, "status-rejected")
	case "stale-binding":
		db := openRuntimeLifecycleDB(t)
		incarnation := runtimeTestIncarnation(t, db, "one")
		runtimeLifecycleChildBarrier(t)
		_, applied, err := db.WriteRuntimeBindingIfVersion("one", incarnation, 1, "claude", 0, "stale-binding", time.Unix(300, 0))
		if err != nil || applied {
			t.Fatalf("stale binding applied=%v err=%v", applied, err)
		}
		writeRuntimeLifecycleResult(t, "binding-rejected")
	case "lock":
		db := openRawRuntimeLifecycleDB(t)
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`UPDATE metadata SET value = value WHERE key = 'schema_version'`); err != nil {
			t.Fatal(err)
		}
		runtimeLifecycleChildBarrier(t)
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		writeRuntimeLifecycleResult(t, "unlocked")
	case "busy-retry":
		db := openRawRuntimeLifecycleDB(t)
		attempts := 0
		err := withBusyRetry(func() error {
			attempts++
			_, err := db.Exec(`UPDATE metadata SET value = value WHERE key = 'schema_version'`)
			if attempts == 1 {
				if !isSQLiteBusy(err) {
					t.Fatalf("first write error = %v, want SQLITE_BUSY", err)
				}
				runtimeLifecycleChildBarrier(t)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		writeRuntimeLifecycleResult(t, strconv.Itoa(attempts))
	case "busy-exhaust":
		db := openRawRuntimeLifecycleDB(t)
		attempts := 0
		err := withBusyRetry(func() error {
			attempts++
			_, err := db.Exec(`UPDATE metadata SET value = value WHERE key = 'schema_version'`)
			return err
		})
		if !isSQLiteBusy(err) || attempts != 5 {
			t.Fatalf("busy exhaustion attempts=%d err=%v", attempts, err)
		}
		writeRuntimeLifecycleResult(t, strconv.Itoa(attempts))
	default:
		t.Fatalf("unknown runtime lifecycle helper mode %q", mode)
	}
}

func openRawRuntimeLifecycleDB(t *testing.T) *sql.DB {
	t.Helper()
	path := os.Getenv("AGENTDECK_RUNTIME_LIFECYCLE_DB_PATH")
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(0)&_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func readRuntimeLifecycleResult(t *testing.T, path string) string {
	t.Helper()
	value, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(value)
}

func seedRuntimeLifecycleRow(t *testing.T, path string, toolData json.RawMessage) *StateDB {
	t.Helper()
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.SaveInstance(&InstanceRow{
		ID: "one", Title: "one", ProjectPath: "/tmp/one", GroupPath: "my-sessions",
		Tool: "claude", Status: "idle", TmuxSession: "tmux-g0", CreatedAt: time.Unix(1, 0),
		ToolData: toolData,
	}); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	return db
}

func seedRuntimeLifecycleLegacyV13(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(0)&_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	statements := []string{
		`CREATE TABLE metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`INSERT INTO metadata(key, value) VALUES ('schema_version', '13')`,
		`CREATE TABLE instance_heartbeats (
			pid INTEGER PRIMARY KEY,
			started INTEGER NOT NULL,
			heartbeat INTEGER NOT NULL,
			is_primary INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE instances (
			id TEXT PRIMARY KEY,
			title TEXT NOT NULL,
			project_path TEXT NOT NULL,
			group_path TEXT NOT NULL DEFAULT 'my-sessions',
			sort_order INTEGER NOT NULL DEFAULT 0,
			command TEXT NOT NULL DEFAULT '',
			wrapper TEXT NOT NULL DEFAULT '',
			tool TEXT NOT NULL DEFAULT 'shell',
			status TEXT NOT NULL DEFAULT 'error',
			tmux_session TEXT NOT NULL DEFAULT '',
			tmux_socket_name TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			last_accessed INTEGER NOT NULL DEFAULT 0,
			parent_session_id TEXT NOT NULL DEFAULT '',
			is_conductor INTEGER NOT NULL DEFAULT 0,
			no_transition_notify INTEGER NOT NULL DEFAULT 0,
			title_locked INTEGER NOT NULL DEFAULT 0,
			worktree_path TEXT NOT NULL DEFAULT '',
			worktree_repo TEXT NOT NULL DEFAULT '',
			worktree_branch TEXT NOT NULL DEFAULT '',
			account TEXT NOT NULL DEFAULT '',
			archived_at INTEGER NOT NULL DEFAULT 0,
			auto_name INTEGER NOT NULL DEFAULT 0,
			auto_name_description TEXT NOT NULL DEFAULT '',
			pin TEXT NOT NULL DEFAULT '',
			last_sent_at INTEGER NOT NULL DEFAULT 0,
			tool_data TEXT NOT NULL DEFAULT '{}',
			acknowledged INTEGER NOT NULL DEFAULT 0
		)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO instances
		(id, title, project_path, group_path, tool, status, tmux_session, tmux_socket_name, created_at, tool_data)
		VALUES ('one', 'one', '/tmp/one', 'my-sessions', 'claude', 'idle', 'tmux-g0', 'isolated', 1, ?)`,
		runtimeLifecycleLegacyToolData); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO instances
		(id, title, project_path, group_path, tool, status, tmux_session, created_at, tool_data)
		VALUES ('unix', 'unix', '/tmp/unix', 'my-sessions', 'pi', 'waiting', 'unix-g0', 1,
		'{"last_started_at":123456789,"notes":"unix"}')`); err != nil {
		t.Fatal(err)
	}
}

func runRuntimeLifecycleBarrierGroup(t *testing.T, children ...*runtimeLifecycleChild) {
	t.Helper()
	for _, child := range children {
		child.waitReady(t)
	}
	for _, child := range children {
		child.releaseBarrier(t)
	}
	for _, child := range children {
		child.wait(t)
	}
}

func TestRuntimeLifecycle_MultiprocessSQLite(t *testing.T) {
	if mode := os.Getenv(runtimeLifecycleDBHelperEnv); mode != "" {
		runRuntimeLifecycleDBHelper(t, mode)
		return
	}

	t.Run("concurrent migration", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "state.db")
		seedRuntimeLifecycleLegacyV13(t, path)

		children := make([]*runtimeLifecycleChild, 4)
		results := make([]string, 4)
		for index := range children {
			results[index] = filepath.Join(dir, fmt.Sprintf("migration-%d", index))
			children[index] = startRuntimeLifecycleChild(t, "migrate", path, results[index], "", true)
		}
		runRuntimeLifecycleBarrierGroup(t, children...)
		succeeded := 0
		for _, result := range results {
			switch readRuntimeLifecycleResult(t, result) {
			case "ok":
				succeeded++
			case "busy":
				// A concurrent migrator may return a classified retryable lock.
			default:
				t.Fatalf("unexpected migration result in %s", result)
			}
		}
		if succeeded == 0 {
			t.Fatal("no concurrent migrator completed")
		}

		raw, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(0)&_pragma=foreign_keys(on)")
		if err != nil {
			t.Fatal(err)
		}
		defer raw.Close()
		var schemaVersion, runtimeTables, runtimeRows, bindingRows int
		if err := raw.QueryRow(`SELECT CAST(value AS INTEGER) FROM metadata WHERE key = 'schema_version'`).Scan(&schemaVersion); err != nil {
			t.Fatal(err)
		}
		if err := raw.QueryRow(`SELECT COUNT(*) FROM sqlite_master
					WHERE type = 'table' AND name IN (
						'instance_runtime_state', 'instance_runtime_binding', 'instance_runtime_destruction'
					)`).Scan(&runtimeTables); err != nil {
			t.Fatal(err)
		}
		if err := raw.QueryRow(`SELECT COUNT(*) FROM instance_runtime_state`).Scan(&runtimeRows); err != nil {
			t.Fatal(err)
		}
		if err := raw.QueryRow(`SELECT COUNT(*) FROM instance_runtime_binding`).Scan(&bindingRows); err != nil {
			t.Fatal(err)
		}
		if schemaVersion != SchemaVersion || runtimeTables != 3 || runtimeRows != 2 || bindingRows != len(bindingKinds) {
			t.Fatalf("raw migration schema=%d tables=%d runtimes=%d bindings=%d", schemaVersion, runtimeTables, runtimeRows, bindingRows)
		}
		var generation, rfcStarted, unixStarted int64
		var tmuxName, rawToolData string
		if err := raw.QueryRow(`SELECT runtime_generation, tmux_session, last_started_at
				FROM instance_runtime_state WHERE instance_id = 'one'`).Scan(&generation, &tmuxName, &rfcStarted); err != nil {
			t.Fatal(err)
		}
		if err := raw.QueryRow(`SELECT last_started_at FROM instance_runtime_state WHERE instance_id = 'unix'`).Scan(&unixStarted); err != nil {
			t.Fatal(err)
		}
		if err := raw.QueryRow(`SELECT tool_data FROM instances WHERE id = 'one'`).Scan(&rawToolData); err != nil {
			t.Fatal(err)
		}
		wantRFC := time.Date(2026, 8, 7, 12, 34, 56, 0, time.UTC).UnixNano()
		wantUnix := time.Unix(123456789, 0).UTC().UnixNano()
		if generation != 0 || tmuxName != "tmux-g0" || rfcStarted != wantRFC || unixStarted != wantUnix || rawToolData != runtimeLifecycleLegacyToolData {
			t.Fatalf("raw migration generation=%d tmux=%q rfc=%d unix=%d tool_data=%q", generation, tmuxName, rfcStarted, unixStarted, rawToolData)
		}
		for index, kind := range bindingKinds {
			var bindingGeneration, revision, detectedAt int64
			var value string
			if err := raw.QueryRow(`SELECT runtime_generation, binding_revision, binding_value, binding_detected_at
					FROM instance_runtime_binding WHERE instance_id = 'one' AND binding_kind = ?`, kind).
				Scan(&bindingGeneration, &revision, &value, &detectedAt); err != nil {
				t.Fatal(err)
			}
			wantDetectedAt := time.Unix(int64(101+index), 0).UTC().UnixNano()
			if bindingGeneration != 0 || revision != 0 || value != kind+"-legacy" || detectedAt != wantDetectedAt {
				t.Errorf("%s binding generation=%d revision=%d value=%q detected=%d", kind, bindingGeneration, revision, value, detectedAt)
			}
		}
		if err := raw.Close(); err != nil {
			t.Fatal(err)
		}

		verify, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer verify.Close()
		state, found, err := verify.ReadRuntimeState("one")
		if err != nil || !found || state.Generation != 0 || state.TmuxSession != "tmux-g0" ||
			!state.LastStartedAt.Equal(time.Date(2026, 8, 7, 12, 34, 56, 0, time.UTC)) {
			t.Fatalf("migrated state=%#v found=%v err=%v", state, found, err)
		}
		plan := make([]RuntimeBindingTransition, 0, len(bindingKinds))
		for _, kind := range bindingKinds {
			binding, found, err := verify.ReadRuntimeBinding("one", kind)
			if err != nil || !found || binding.Value != kind+"-legacy" || binding.Generation != 0 {
				t.Fatalf("migrated %s binding=%#v found=%v err=%v", kind, binding, found, err)
			}
			plan = append(plan, RuntimeBindingTransition{
				Kind: kind, ExpectedRevision: binding.Revision,
				NextValue: binding.Value, DetectedAt: binding.DetectedAt,
			})
		}
		incarnation := runtimeTestIncarnation(t, verify, "one")
		if err := verify.CommitRuntimeTransitionWithBindingPlan(0, incarnation, RuntimeState{
			InstanceID: "one", Generation: 1, TmuxSession: "tmux-g1",
			TmuxSocketName: "isolated", Status: "starting", LastStartedAt: time.Unix(200, 0),
		}, plan); err != nil {
			t.Fatalf("first migrated transition: %v", err)
		}
		if state, found, err := verify.ReadRuntimeState("one"); err != nil || !found || state.Generation != 1 || state.TmuxSession != "tmux-g1" {
			t.Fatalf("first transitioned state=%#v found=%v err=%v", state, found, err)
		}
	})

	t.Run("one winner transition CAS", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "state.db")
		db := seedRuntimeLifecycleRow(t, path, json.RawMessage(`{}`))
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		firstResult := filepath.Join(dir, "first")
		secondResult := filepath.Join(dir, "second")
		first := startRuntimeLifecycleChild(t, "transition", path, firstResult, "tmux-a", true)
		second := startRuntimeLifecycleChild(t, "transition", path, secondResult, "tmux-b", true)
		runRuntimeLifecycleBarrierGroup(t, first, second)
		outcomes := []string{readRuntimeLifecycleResult(t, firstResult), readRuntimeLifecycleResult(t, secondResult)}
		wins, conflicts := 0, 0
		for _, outcome := range outcomes {
			if strings.HasPrefix(outcome, "won:") {
				wins++
			} else if outcome == "conflict" {
				conflicts++
			}
		}
		if wins != 1 || conflicts != 1 {
			t.Fatalf("transition outcomes = %v", outcomes)
		}
		verify, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer verify.Close()
		state, found, err := verify.ReadRuntimeState("one")
		if err != nil || !found || state.Generation != 1 || (state.TmuxSession != "tmux-a" && state.TmuxSession != "tmux-b") {
			t.Fatalf("winner state=%#v found=%v err=%v", state, found, err)
		}
	})

	t.Run("stale writers", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "state.db")
		db := seedRuntimeLifecycleRow(t, path, json.RawMessage(`{"claude_session_id":"old","notes":"original"}`))
		incarnation := runtimeTestIncarnation(t, db, "one")
		if err := db.CommitRuntimeTransitionWithBindingPlan(0, incarnation, RuntimeState{
			InstanceID: "one", Generation: 1, TmuxSession: "tmux-g1",
			TmuxSocketName: "isolated", Status: "starting", LastStartedAt: time.Unix(200, 0),
		}, []RuntimeBindingTransition{
			{Kind: "claude", ExpectedRevision: 0, NextValue: "old"},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := db.CommitRuntimeBinding("one", incarnation, 1, "claude", 1, "new"); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}

		metadataResult := filepath.Join(dir, "metadata")
		statusResult := filepath.Join(dir, "status")
		bindingResult := filepath.Join(dir, "binding")
		metadata := startRuntimeLifecycleChild(t, "stale-metadata", path, metadataResult, "", true)
		status := startRuntimeLifecycleChild(t, "stale-status", path, statusResult, "", true)
		binding := startRuntimeLifecycleChild(t, "stale-binding", path, bindingResult, "", true)
		runRuntimeLifecycleBarrierGroup(t, metadata, status, binding)
		if got := readRuntimeLifecycleResult(t, metadataResult); got != "metadata" {
			t.Fatalf("metadata result = %q", got)
		}
		if got := readRuntimeLifecycleResult(t, statusResult); got != "status-rejected" {
			t.Fatalf("status result = %q", got)
		}
		if got := readRuntimeLifecycleResult(t, bindingResult); got != "binding-rejected" {
			t.Fatalf("binding result = %q", got)
		}

		verify, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer verify.Close()
		state, found, err := verify.ReadRuntimeState("one")
		if err != nil || !found || state.Generation != 1 || state.StatusRevision != 0 ||
			state.Status != "starting" || state.TmuxSession != "tmux-g1" {
			t.Fatalf("runtime state=%#v found=%v err=%v", state, found, err)
		}
		currentBinding, found, err := verify.ReadRuntimeBinding("one", "claude")
		if err != nil || !found || currentBinding.Revision != 2 || currentBinding.Value != "new" {
			t.Fatalf("binding=%#v found=%v err=%v", currentBinding, found, err)
		}
		rows, err := verify.LoadInstances()
		if err != nil || len(rows) != 1 || rows[0].Title != "metadata-won" {
			t.Fatalf("metadata rows=%#v err=%v", rows, err)
		}
		var toolData map[string]json.RawMessage
		if err := json.Unmarshal(rows[0].ToolData, &toolData); err != nil {
			t.Fatal(err)
		}
		if string(toolData["claude_session_id"]) != `"new"` || string(toolData["notes"]) != `"metadata-won"` {
			t.Fatalf("tool_data=%s", rows[0].ToolData)
		}
	})

	t.Run("SQLITE_BUSY retry and exhaustion", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "state.db")
		db := seedRuntimeLifecycleRow(t, path, json.RawMessage(`{}`))
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}

		lockResult := filepath.Join(dir, "lock-retry")
		lock := startRuntimeLifecycleChild(t, "lock", path, lockResult, "", true)
		lock.waitReady(t)
		retryResult := filepath.Join(dir, "retry")
		retry := startRuntimeLifecycleChild(t, "busy-retry", path, retryResult, "", true)
		retry.waitReady(t)
		lock.releaseBarrier(t)
		lock.wait(t)
		retry.releaseBarrier(t)
		retry.wait(t)
		if got := readRuntimeLifecycleResult(t, retryResult); got != "2" {
			t.Fatalf("retry attempts = %q, want 2", got)
		}

		exhaustLockResult := filepath.Join(dir, "lock-exhaust")
		exhaustLock := startRuntimeLifecycleChild(t, "lock", path, exhaustLockResult, "", true)
		exhaustLock.waitReady(t)
		exhaustResult := filepath.Join(dir, "exhaust")
		exhaust := startRuntimeLifecycleChild(t, "busy-exhaust", path, exhaustResult, "", false)
		exhaust.wait(t)
		exhaustLock.releaseBarrier(t)
		exhaustLock.wait(t)
		if got := readRuntimeLifecycleResult(t, exhaustResult); got != "5" {
			t.Fatalf("exhaustion attempts = %q, want 5", got)
		}
	})
}
