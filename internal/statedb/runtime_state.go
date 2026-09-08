package statedb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

var (
	ErrRuntimeGenerationConflict = errors.New("statedb: runtime generation conflict")
	ErrStatusRevisionConflict    = errors.New("statedb: status revision conflict")
	ErrBindingRevisionConflict   = errors.New("statedb: binding revision conflict")
	ErrBindingPlanIncomplete     = errors.New("statedb: runtime binding plan is incomplete")
	ErrInstanceParentConflict    = errors.New("statedb: instance parent conflict")
)

// requireRuntimeIncarnation validates the durable runtime authority token.
// Unlike requireInstanceIncarnation, it deliberately does not require the
// metadata parent: new-session startup persists this side token before the
// parent insert so every pre-insert runtime mutation is still ABA-safe.
func requireRuntimeIncarnation(q interface{ QueryRow(string, ...any) *sql.Row }, instanceID, expectedIncarnation string) error {
	if instanceID == "" || expectedIncarnation == "" {
		return ErrInstanceParentConflict
	}
	var current string
	err := q.QueryRow(`SELECT incarnation FROM instance_incarnation WHERE instance_id = ?`, instanceID).Scan(&current)
	if err == sql.ErrNoRows {
		return ErrInstanceParentConflict
	}
	if err != nil {
		return err
	}
	if current != expectedIncarnation {
		return ErrInstanceParentConflict
	}
	return nil
}

// ValidateRuntimeIncarnation verifies the side-token authority used by runtime
// writers. It also works during the deliberate pre-parent startup interval.
// Mutating callers must still use an incarnation-aware BEGIN IMMEDIATE API.
func (s *StateDB) ValidateRuntimeIncarnation(instanceID, expectedIncarnation string) error {
	tx, err := s.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireRuntimeIncarnation(tx, instanceID, expectedIncarnation); err != nil {
		return err
	}
	return tx.Commit()
}

// RuntimeState is the authoritative, indivisible identity of one physical
// runtime. The similarly named columns on instances are compatibility
// projections only; ordinary instance saves never update this tuple.
type RuntimeState struct {
	InstanceID     string
	Generation     uint64
	StatusRevision uint64
	TmuxSession    string
	TmuxSocketName string
	Status         string
	LastStartedAt  time.Time
}

// LegacyRuntimeAdoption records the exact tmux identity imported from a
// pre-runtime-schema instances row. Its durable incarnation is deliberately
// private: callers prove the current incarnation when consuming the record.
type LegacyRuntimeAdoption struct {
	InstanceID     string
	TmuxSession    string
	TmuxSocketName string
}

// InstanceSeedToken identifies the exact parent/runtime pair created by an
// insert-only session seed. The parent snapshot is intentionally opaque to
// callers; it is consumed only by DeleteInstanceSeedIfUnchanged.
type InstanceSeedToken struct {
	Runtime     RuntimeState
	Bindings    map[string]RuntimeBinding
	Incarnation string
	parent      instanceSeedParent
	bindings    []RuntimeBinding
}

type instanceSeedParent struct {
	id, title, projectPath, groupPath, command, wrapper, tool   string
	status, tmuxSession, tmuxSocketName                         string
	parentSessionID, worktreePath, worktreeRepo, worktreeBranch string
	account, toolData, autoNameDescription, pin                 string
	order, createdAt, lastAccessed, archivedAt                  int64
	isConductor, noTransitionNotify, titleLocked, autoName      int64
	lastSentAt, acknowledged                                    int64
}

func seedBoolInt(value bool) int64 {
	if value {
		return 1
	}
	return 0
}

func instanceSeedParentFromRow(inst *InstanceRow) instanceSeedParent {
	toolData := inst.ToolData
	if len(toolData) == 0 {
		toolData = json.RawMessage("{}")
	}
	return instanceSeedParent{
		id: inst.ID, title: inst.Title, projectPath: inst.ProjectPath,
		groupPath: inst.GroupPath, order: int64(inst.Order), command: inst.Command,
		wrapper: inst.Wrapper, tool: inst.Tool, status: inst.Status,
		tmuxSession: inst.TmuxSession, tmuxSocketName: inst.TmuxSocketName,
		createdAt: inst.CreatedAt.Unix(), lastAccessed: inst.LastAccessed.Unix(),
		parentSessionID: inst.ParentSessionID, isConductor: seedBoolInt(inst.IsConductor),
		noTransitionNotify: seedBoolInt(inst.NoTransitionNotify),
		worktreePath:       inst.WorktreePath, worktreeRepo: inst.WorktreeRepo,
		worktreeBranch: inst.WorktreeBranch, account: inst.Account,
		archivedAt: archivedAtUnix(inst.ArchivedAt), toolData: string(toolData),
		titleLocked: seedBoolInt(inst.TitleLocked), autoName: seedBoolInt(inst.AutoName),
		autoNameDescription: inst.AutoNameDescription, pin: inst.Pin,
	}
}

// RuntimeBinding assigns one tool conversation to one runtime generation.
// Revision is independent of the runtime status revision.
type RuntimeBinding struct {
	InstanceID string
	Kind       string
	Generation uint64
	Revision   uint64
	Value      string
	DetectedAt time.Time
}

type runtimeBindingQueryer interface {
	Query(string, ...any) (*sql.Rows, error)
}

// loadRuntimeBindingSnapshot returns every durable binding in stable order and
// the current-generation projection used by in-memory Instances.
func loadRuntimeBindingSnapshot(q runtimeBindingQueryer, instanceID string, generation uint64) ([]RuntimeBinding, map[string]RuntimeBinding, error) {
	rows, err := q.Query(`
		SELECT binding_kind, runtime_generation, binding_revision, binding_value, binding_detected_at
		FROM instance_runtime_binding WHERE instance_id = ? ORDER BY binding_kind`, instanceID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var all []RuntimeBinding
	current := make(map[string]RuntimeBinding)
	for rows.Next() {
		binding := RuntimeBinding{InstanceID: instanceID}
		var bindingGeneration, revision uint64
		var detectedAt int64
		if err := rows.Scan(&binding.Kind, &bindingGeneration, &revision, &binding.Value, &detectedAt); err != nil {
			return nil, nil, err
		}
		binding.Generation = bindingGeneration
		binding.Revision = revision
		if detectedAt > 0 {
			binding.DetectedAt = runtimeTime(detectedAt)
		}
		all = append(all, binding)
		if binding.Generation == generation {
			current[binding.Kind] = binding
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return all, current, nil
}

// RuntimeBindingTransition is one binding decision captured before a physical
// runtime replacement. NextValue carries or assigns a binding; empty releases
// it. ExpectedRevision makes that decision stale-safe.
type RuntimeBindingTransition struct {
	Kind             string
	ExpectedRevision uint64
	NextValue        string
	DetectedAt       time.Time
}

var bindingJSONKeys = map[string]string{
	"claude":   "claude_session_id",
	"copilot":  "copilot_session_id",
	"codex":    "codex_session_id",
	"gemini":   "gemini_session_id",
	"opencode": "opencode_session_id",
}

var bindingKinds = []string{"claude", "copilot", "codex", "gemini", "opencode"}

var runtimeToolDataKeys = map[string]bool{
	"last_started_at":      true,
	"claude_session_id":    true,
	"claude_detected_at":   true,
	"copilot_session_id":   true,
	"copilot_detected_at":  true,
	"codex_session_id":     true,
	"codex_detected_at":    true,
	"gemini_session_id":    true,
	"gemini_detected_at":   true,
	"opencode_session_id":  true,
	"opencode_detected_at": true,
}

func runtimeUnix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UTC().UnixNano()
}

// runtimeTime decodes the authoritative nanosecond format. The v15 migration
// converts v14 authoritative rows from seconds before any runtime read.
func runtimeTime(value int64) time.Time {
	if value <= 0 {
		return time.Time{}
	}
	return time.Unix(0, value).UTC()
}

func legacyRuntimeUnix(value int64) int64 {
	decoded := runtimeTime(value)
	if decoded.IsZero() {
		return 0
	}
	return decoded.Unix()
}

func legacyUnixTime(value int64) time.Time {
	if value <= 0 {
		return time.Time{}
	}
	return time.Unix(value, 0).UTC()
}

// mergeMetadataToolData lets ordinary saves update configuration keys while
// retaining every runtime-owned key from the durable row. In particular, a
// stale full Instance snapshot cannot replace a newer conversation binding.
func mergeMetadataToolData(oldToolData, newToolData json.RawMessage) json.RawMessage {
	merged := MergeToolDataExtras(oldToolData, newToolData)
	if len(oldToolData) == 0 {
		return merged
	}
	var oldMap, newMap map[string]json.RawMessage
	if json.Unmarshal(oldToolData, &oldMap) != nil || json.Unmarshal(merged, &newMap) != nil {
		return merged
	}
	if newMap == nil {
		newMap = make(map[string]json.RawMessage)
	}
	for key := range runtimeToolDataKeys {
		if value, ok := oldMap[key]; ok {
			newMap[key] = value
		} else {
			delete(newMap, key)
		}
	}
	out, err := json.Marshal(newMap)
	if err != nil {
		return merged
	}
	return out
}

func legacyTimestamp(toolData json.RawMessage, key string) time.Time {
	var values map[string]json.RawMessage
	if json.Unmarshal(toolData, &values) != nil {
		return time.Time{}
	}
	raw, ok := values[key]
	if !ok {
		return time.Time{}
	}
	var unix int64
	if json.Unmarshal(raw, &unix) == nil && unix > 0 {
		return legacyUnixTime(unix)
	}
	var text string
	if json.Unmarshal(raw, &text) != nil || text == "" {
		return time.Time{}
	}
	if unix, err := strconv.ParseInt(text, 10, 64); err == nil && unix > 0 {
		return legacyUnixTime(unix)
	}
	parsed, _ := time.Parse(time.RFC3339Nano, text)
	return parsed.UTC()
}

func legacyLastStartedAt(toolData json.RawMessage) time.Time {
	return legacyTimestamp(toolData, "last_started_at")
}

func legacyBindings(toolData json.RawMessage) map[string]string {
	var values map[string]json.RawMessage
	if json.Unmarshal(toolData, &values) != nil {
		return nil
	}
	result := make(map[string]string)
	for kind, key := range bindingJSONKeys {
		var value string
		if json.Unmarshal(values[key], &value) == nil && value != "" {
			result[kind] = value
		}
	}
	return result
}

// migrateRuntimeState creates the mixed-writer fence and imports legacy
// runtime values exactly once. It is additive and safe to repeat.
func migrateRuntimeState(tx *sql.Tx) error {
	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS instance_runtime_state (
			instance_id        TEXT PRIMARY KEY,
			runtime_generation INTEGER NOT NULL DEFAULT 0,
			status_revision    INTEGER NOT NULL DEFAULT 0,
			tmux_session       TEXT NOT NULL DEFAULT '',
			tmux_socket_name   TEXT NOT NULL DEFAULT '',
			status             TEXT NOT NULL DEFAULT 'error',
			last_started_at    INTEGER NOT NULL DEFAULT 0
		)`); err != nil {
		return fmt.Errorf("statedb: create runtime state: %w", err)
	}
	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS instance_runtime_binding (
			instance_id        TEXT NOT NULL,
			binding_kind       TEXT NOT NULL,
			runtime_generation INTEGER NOT NULL,
			binding_revision   INTEGER NOT NULL DEFAULT 0,
			binding_value      TEXT NOT NULL,
			binding_detected_at INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (instance_id, binding_kind)
		)`); err != nil {
		return fmt.Errorf("statedb: create runtime binding: %w", err)
	}
	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS instance_runtime_destruction (
			instance_id            TEXT PRIMARY KEY,
			incarnation           TEXT NOT NULL CHECK (incarnation <> ''),
			runtime_generation     INTEGER NOT NULL,
			claimed_status_revision INTEGER NOT NULL,
			prior_status           TEXT NOT NULL CHECK (prior_status <> '')
		)`); err != nil {
		return fmt.Errorf("statedb: create runtime destruction provenance: %w", err)
	}
	if _, err := tx.Exec(`
		CREATE TRIGGER IF NOT EXISTS runtime_state_delete_clears_destruction
		AFTER DELETE ON instance_runtime_state
		BEGIN
			DELETE FROM instance_runtime_destruction WHERE instance_id = OLD.instance_id;
		END`); err != nil {
		return fmt.Errorf("statedb: create runtime destruction delete trigger: %w", err)
	}
	if _, err := tx.Exec(`
		CREATE TRIGGER IF NOT EXISTS runtime_state_insert_clears_stale_destruction
		AFTER INSERT ON instance_runtime_state
		BEGIN
			DELETE FROM instance_runtime_destruction WHERE instance_id = NEW.instance_id;
		END`); err != nil {
		return fmt.Errorf("statedb: create runtime destruction insert trigger: %w", err)
	}
	// A pre-v18 database can contain a crash-stranded private marker but has
	// no lossless record of the prior status or reserving incarnation. Refuse
	// that migration instead of manufacturing provenance that could authorize
	// an incorrect recovery.
	var unprovenReservations int
	if err := tx.QueryRow(`
		SELECT COUNT(*)
		FROM instance_runtime_state runtime
		LEFT JOIN instance_runtime_destruction reservation
		  ON reservation.instance_id = runtime.instance_id
		 AND reservation.runtime_generation = runtime.runtime_generation
		 AND reservation.claimed_status_revision = runtime.status_revision
		LEFT JOIN instance_incarnation token
		  ON token.instance_id = runtime.instance_id
		WHERE runtime.status = ? AND (
		  reservation.instance_id IS NULL OR token.instance_id IS NULL OR
		  reservation.incarnation <> token.incarnation OR
		  reservation.prior_status = ?
		)`,
		runtimeDestructionStatus, runtimeDestructionStatus,
	).Scan(&unprovenReservations); err != nil {
		return fmt.Errorf("statedb: inspect runtime destruction provenance: %w", err)
	}
	if unprovenReservations != 0 {
		return fmt.Errorf("statedb: migrate %d unproven runtime destruction reservations: %w",
			unprovenReservations, ErrStatusRevisionConflict)
	}
	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS instance_legacy_runtime_adoption (
			instance_id      TEXT PRIMARY KEY,
			incarnation     TEXT NOT NULL CHECK (incarnation <> ''),
			tmux_session    TEXT NOT NULL CHECK (tmux_session <> ''),
			tmux_socket_name TEXT NOT NULL DEFAULT ''
		)`); err != nil {
		return fmt.Errorf("statedb: create legacy runtime adoption: %w", err)
	}
	// The marker belongs to one parent insertion. Clear it for every delete or
	// reinsert, including legacy SQL paths that know nothing about this table.
	if _, err := tx.Exec(`
		CREATE TRIGGER IF NOT EXISTS instances_remove_legacy_runtime_adoption
		AFTER DELETE ON instances
		BEGIN
			DELETE FROM instance_legacy_runtime_adoption WHERE instance_id = OLD.id;
		END`); err != nil {
		return fmt.Errorf("statedb: create legacy runtime adoption delete trigger: %w", err)
	}
	if _, err := tx.Exec(`
		CREATE TRIGGER IF NOT EXISTS instances_reinsert_supersedes_legacy_runtime_adoption
		AFTER INSERT ON instances
		BEGIN
			DELETE FROM instance_legacy_runtime_adoption WHERE instance_id = NEW.id;
		END`); err != nil {
		return fmt.Errorf("statedb: create legacy runtime adoption insert trigger: %w", err)
	}
	if _, err := tx.Exec(`ALTER TABLE instance_runtime_binding ADD COLUMN binding_detected_at INTEGER NOT NULL DEFAULT 0`); err != nil && !strings.Contains(err.Error(), "duplicate column") {
		return fmt.Errorf("statedb: add binding detected timestamp: %w", err)
	}
	// v14 stored authoritative timestamps as Unix seconds. Convert those rows
	// once, before importing any legacy instances into the nanosecond format.
	var priorVersionText string
	priorVersion := 0
	if err := tx.QueryRow(`SELECT value FROM metadata WHERE key = 'schema_version'`).Scan(&priorVersionText); err == nil {
		var parseErr error
		priorVersion, parseErr = strconv.Atoi(priorVersionText)
		if parseErr != nil {
			return fmt.Errorf("statedb: invalid schema version %q: %w", priorVersionText, parseErr)
		}
		if priorVersion == 14 {
			if _, err := tx.Exec(`UPDATE instance_runtime_state
				SET last_started_at = last_started_at * 1000000000
				WHERE last_started_at > 0`); err != nil {
				return fmt.Errorf("statedb: convert runtime timestamps to nanoseconds: %w", err)
			}
			if _, err := tx.Exec(`UPDATE instance_runtime_binding
				SET binding_detected_at = binding_detected_at * 1000000000
				WHERE binding_detected_at > 0`); err != nil {
				return fmt.Errorf("statedb: convert binding timestamps to nanoseconds: %w", err)
			}
		}
	} else if err != sql.ErrNoRows {
		return fmt.Errorf("statedb: read prior schema version: %w", err)
	}
	legacyAdoptionMigration := priorVersion > 0 && priorVersion < 14
	if legacyAdoptionMigration {
		// The adoption record must be tied to the same insertion identity as
		// the imported parent. This is also the ordinary v16 incarnation
		// backfill; the later migration-wide backfill remains an idempotent
		// safety net for every other upgrade path.
		if _, err := tx.Exec(`
			INSERT OR IGNORE INTO instance_incarnation (instance_id, incarnation)
			SELECT id, lower(hex(randomblob(16))) FROM instances
		`); err != nil {
			return fmt.Errorf("statedb: backfill legacy runtime incarnations: %w", err)
		}
	}
	if _, err := tx.Exec(`
		CREATE UNIQUE INDEX IF NOT EXISTS idx_runtime_binding_owner
		ON instance_runtime_binding(binding_kind, binding_value)
		WHERE binding_value <> ''`); err != nil {
		return fmt.Errorf("statedb: create runtime binding owner index: %w", err)
	}

	type legacyRow struct {
		id, status, tmuxSession, tmuxSocket string
		toolData                            string
	}
	rows, err := tx.Query(`SELECT id, status, tmux_session, tmux_socket_name, tool_data FROM instances ORDER BY id`)
	if err != nil {
		return fmt.Errorf("statedb: read legacy runtime state: %w", err)
	}
	var legacy []legacyRow
	for rows.Next() {
		var row legacyRow
		if err := rows.Scan(&row.id, &row.status, &row.tmuxSession, &row.tmuxSocket, &row.toolData); err != nil {
			_ = rows.Close()
			return fmt.Errorf("statedb: scan legacy runtime state: %w", err)
		}
		legacy = append(legacy, row)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("statedb: iterate legacy runtime state: %w", err)
	}
	_ = rows.Close()

	for _, row := range legacy {
		if row.status == runtimeDestructionStatus {
			return fmt.Errorf("statedb: migrate runtime state for %s: %w", row.id, ErrStatusRevisionConflict)
		}
		toolData := json.RawMessage(row.toolData)
		startedAt := legacyLastStartedAt(toolData)
		result, err := tx.Exec(`
			INSERT OR IGNORE INTO instance_runtime_state
				(instance_id, runtime_generation, status_revision, tmux_session, tmux_socket_name, status, last_started_at)
			VALUES (?, 0, 0, ?, ?, ?, ?)`,
			row.id, row.tmuxSession, row.tmuxSocket, row.status, runtimeUnix(startedAt),
		)
		if err != nil {
			return fmt.Errorf("statedb: migrate runtime state for %s: %w", row.id, err)
		}
		inserted, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("statedb: inspect migrated runtime state for %s: %w", row.id, err)
		}
		if legacyAdoptionMigration && inserted == 1 && row.tmuxSession != "" && row.status != "" && !startedAt.IsZero() {
			if _, err := tx.Exec(`
				INSERT OR IGNORE INTO instance_legacy_runtime_adoption
					(instance_id, incarnation, tmux_session, tmux_socket_name)
				SELECT ?, incarnation, ?, ?
				FROM instance_incarnation WHERE instance_id = ?`,
				row.id, row.tmuxSession, row.tmuxSocket, row.id); err != nil {
				return fmt.Errorf("statedb: mark legacy runtime adoption for %s: %w", row.id, err)
			}
		}
		bindings := legacyBindings(toolData)
		for _, kind := range bindingKinds {
			value := bindings[kind]
			if value == "" {
				continue
			}
			if _, err := tx.Exec(`
				INSERT INTO instance_runtime_binding
					(instance_id, binding_kind, runtime_generation, binding_revision, binding_value, binding_detected_at)
				VALUES (?, ?, 0, 0, ?, ?)
				ON CONFLICT(instance_id, binding_kind) DO NOTHING`,
				row.id, kind, value, runtimeUnix(legacyTimestamp(toolData, kind+"_detected_at"))); err != nil {
				return fmt.Errorf("statedb: legacy %s binding conflict for %s: %w", kind, row.id, err)
			}
		}
	}
	return nil
}

// ReadLegacyRuntimeAdoption returns the one-shot eligibility record created
// while importing a live pre-runtime-schema tmux identity.
func (s *StateDB) ReadLegacyRuntimeAdoption(instanceID string) (LegacyRuntimeAdoption, bool, error) {
	var adoption LegacyRuntimeAdoption
	err := s.db.QueryRow(`
		SELECT instance_id, tmux_session, tmux_socket_name
		FROM instance_legacy_runtime_adoption WHERE instance_id = ?`, instanceID).
		Scan(&adoption.InstanceID, &adoption.TmuxSession, &adoption.TmuxSocketName)
	if err == sql.ErrNoRows {
		return LegacyRuntimeAdoption{}, false, nil
	}
	if err != nil {
		return LegacyRuntimeAdoption{}, false, err
	}
	return adoption, true, nil
}

type legacyRuntimeAdoptionQueryer interface {
	QueryRow(string, ...any) *sql.Row
}

func sameRuntimeStateTuple(left, right RuntimeState) bool {
	return left.InstanceID == right.InstanceID && left.Generation == right.Generation &&
		left.StatusRevision == right.StatusRevision && left.TmuxSession == right.TmuxSession &&
		left.TmuxSocketName == right.TmuxSocketName && left.Status == right.Status &&
		left.LastStartedAt.Equal(right.LastStartedAt)
}

func sameRuntimeBindingTuple(left, right RuntimeBinding) bool {
	return left.InstanceID == right.InstanceID && left.Kind == right.Kind &&
		left.Generation == right.Generation && left.Revision == right.Revision &&
		left.Value == right.Value && left.DetectedAt.Equal(right.DetectedAt)
}

func validateLegacyRuntimeBindingSnapshot(q runtimeBindingQueryer, expected RuntimeState, bindings map[string]RuntimeBinding) error {
	all, current, err := loadRuntimeBindingSnapshot(q, expected.InstanceID, expected.Generation)
	if err != nil {
		return err
	}
	// A row from another generation is not part of the caller's projection,
	// but it is still durable binding state. Reject it rather than silently
	// treating an incomplete projection as the exact migration authority.
	if len(all) != len(bindings) || len(current) != len(bindings) {
		return ErrRuntimeGenerationConflict
	}
	for kind, binding := range bindings {
		observed, found := current[kind]
		if !found || binding.InstanceID != expected.InstanceID || binding.Kind != kind ||
			binding.Generation != expected.Generation || !sameRuntimeBindingTuple(observed, binding) {
			return ErrRuntimeGenerationConflict
		}
	}
	return nil
}

func validateLegacyRuntimeAdoption(q legacyRuntimeAdoptionQueryer, expected RuntimeState, incarnation string) error {
	if expected.InstanceID == "" || expected.Generation != 0 {
		return ErrRuntimeGenerationConflict
	}
	if err := requireInstanceIncarnation(q, expected.InstanceID, incarnation); err != nil {
		return err
	}

	var markerIncarnation, markerSession, markerSocket string
	err := q.QueryRow(`
		SELECT incarnation, tmux_session, tmux_socket_name
		FROM instance_legacy_runtime_adoption WHERE instance_id = ?`, expected.InstanceID).
		Scan(&markerIncarnation, &markerSession, &markerSocket)
	if err == sql.ErrNoRows {
		return ErrRuntimeGenerationConflict
	}
	if err != nil {
		return err
	}
	if markerIncarnation != incarnation {
		return ErrInstanceParentConflict
	}
	if markerSession != expected.TmuxSession || markerSocket != expected.TmuxSocketName {
		return ErrRuntimeGenerationConflict
	}

	current, found, err := runtimeStateFromScanner(q.QueryRow(`
		SELECT instance_id, runtime_generation, status_revision,
		       tmux_session, tmux_socket_name, status, last_started_at
		FROM instance_runtime_state WHERE instance_id = ?`, expected.InstanceID))
	if err != nil {
		return err
	}
	if !found {
		return ErrRuntimeGenerationConflict
	}
	if !sameRuntimeStateTuple(current, expected) {
		return ErrRuntimeGenerationConflict
	}
	return nil
}

// ValidateLegacyRuntimeAdoption verifies the current parent, marker, and
// authoritative generation-zero runtime in one read snapshot. Callers use it
// before touching tmux; ConsumeLegacyRuntimeAdoption repeats the same proof
// under a writer reservation before consuming the marker.
func (s *StateDB) ValidateLegacyRuntimeAdoption(expected RuntimeState, incarnation string) error {
	tx, err := s.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := validateLegacyRuntimeAdoption(tx, expected, incarnation); err != nil {
		return err
	}
	return tx.Commit()
}

// ConsumeLegacyRuntimeAdoption atomically consumes the migration-only marker
// iff it still names the current parent incarnation and complete authoritative
// generation-zero runtime tuple. A new generation-zero spawn is therefore
// never mistaken for a genuinely migrated live runtime.
func (s *StateDB) ConsumeLegacyRuntimeAdoption(expected RuntimeState, incarnation string) error {
	return s.consumeLegacyRuntimeAdoption(expected, incarnation, nil, nil, nil)
}

// ConsumeLegacyRuntimeAdoptionWithCommitFence consumes the marker only while
// the complete durable runtime and binding snapshots still match. beforeDelete
// runs after BEGIN IMMEDIATE has reserved the writer and before the marker is
// staged for deletion. beforeCommit runs after that deletion but before COMMIT.
// Either callback must be bounded; an error rolls the deletion back.
func (s *StateDB) ConsumeLegacyRuntimeAdoptionWithCommitFence(
	expected RuntimeState,
	incarnation string,
	bindings map[string]RuntimeBinding,
	beforeDelete func() error,
	beforeCommit func() error,
) error {
	if bindings == nil || beforeDelete == nil || beforeCommit == nil {
		return fmt.Errorf("statedb: incomplete legacy runtime adoption commit fence")
	}
	return s.consumeLegacyRuntimeAdoption(expected, incarnation, bindings, beforeDelete, beforeCommit)
}

func (s *StateDB) consumeLegacyRuntimeAdoption(
	expected RuntimeState,
	incarnation string,
	bindings map[string]RuntimeBinding,
	beforeDelete func() error,
	beforeCommit func() error,
) error {
	withTransaction := s.withImmediateTransaction
	if beforeDelete != nil || beforeCommit != nil {
		withTransaction = s.withRuntimeBindingCommitFence
	}
	return withBusyRetry(func() error {
		return withTransaction(func(tx *immediateTransaction) error {
			if err := s.requireRuntimeWriterCompatibility(tx, time.Now()); err != nil {
				return err
			}
			if err := validateLegacyRuntimeAdoption(tx, expected, incarnation); err != nil {
				return err
			}
			if bindings != nil {
				if err := validateLegacyRuntimeBindingSnapshot(tx, expected, bindings); err != nil {
					return err
				}
			}
			if beforeDelete != nil {
				if err := beforeDelete(); err != nil {
					return err
				}
			}

			result, err := tx.Exec(`
				DELETE FROM instance_legacy_runtime_adoption
				WHERE instance_id = ? AND incarnation = ?
				  AND tmux_session = ? AND tmux_socket_name = ?`,
				expected.InstanceID, incarnation, expected.TmuxSession, expected.TmuxSocketName)
			if err != nil {
				return err
			}
			deleted, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if deleted != 1 {
				return ErrRuntimeGenerationConflict
			}
			if beforeCommit != nil {
				if err := beforeCommit(); err != nil {
					return err
				}
			}
			return tx.Commit()
		})
	})
}

// SupersedeLegacyRuntimeAdoption retires migration-only recovery authority
// before a modern physical transition begins. The caller holds the instance
// lifecycle lock; BEGIN IMMEDIATE makes the complete runtime proof and marker
// deletion indivisible with respect to status and runtime writers.
func (s *StateDB) SupersedeLegacyRuntimeAdoption(expected RuntimeState, incarnation string) error {
	if expected.InstanceID == "" {
		return ErrRuntimeGenerationConflict
	}
	return withBusyRetry(func() error {
		return s.withImmediateTransaction(func(tx *immediateTransaction) error {
			if err := s.requireRuntimeWriterCompatibility(tx, time.Now()); err != nil {
				return err
			}
			if err := requireRuntimeIncarnation(tx, expected.InstanceID, incarnation); err != nil {
				return err
			}
			current, found, err := runtimeStateFromScanner(tx.QueryRow(`
				SELECT instance_id, runtime_generation, status_revision,
				       tmux_session, tmux_socket_name, status, last_started_at
				FROM instance_runtime_state WHERE instance_id = ?`, expected.InstanceID))
			if err != nil {
				return err
			}
			if !found || !sameRuntimeStateTuple(current, expected) {
				return ErrRuntimeGenerationConflict
			}
			if _, err := tx.Exec(`DELETE FROM instance_legacy_runtime_adoption WHERE instance_id = ?`, expected.InstanceID); err != nil {
				return err
			}
			return tx.Commit()
		})
	})
}

type runtimeStateExecutor interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func initialRuntimeState(inst *InstanceRow) RuntimeState {
	generation := inst.RuntimeGeneration
	statusRevision := inst.StatusRevision
	startedAt := inst.LastStartedAt
	bindings := inst.RuntimeBindings
	hasRuntimeSnapshot := generation != 0 || statusRevision != 0 || !startedAt.IsZero() || bindings != nil
	if !hasRuntimeSnapshot {
		startedAt = legacyLastStartedAt(inst.ToolData)
	}
	return RuntimeState{
		InstanceID: inst.ID, Generation: generation, StatusRevision: statusRevision,
		TmuxSession: inst.TmuxSession, TmuxSocketName: inst.TmuxSocketName,
		Status: inst.Status, LastStartedAt: startedAt,
	}
}

func hasExplicitRuntimeSnapshot(inst *InstanceRow) bool {
	return inst.RuntimeGeneration != 0 || inst.StatusRevision != 0 ||
		!inst.LastStartedAt.IsZero() || inst.RuntimeBindings != nil
}

func insertInitialRuntimeTx(tx runtimeStateExecutor, inst *InstanceRow) (bool, error) {
	initial := initialRuntimeState(inst)
	// This private marker proves an in-place ReserveRuntimeDestruction CAS.
	// Generic creation and save helpers must never originate or carry it.
	if IsRuntimeDestructionReserved(initial) {
		return false, ErrStatusRevisionConflict
	}
	bindings := inst.RuntimeBindings
	hasRuntimeSnapshot := hasExplicitRuntimeSnapshot(inst)
	result, err := tx.Exec(`
		INSERT OR IGNORE INTO instance_runtime_state
			(instance_id, runtime_generation, status_revision, tmux_session, tmux_socket_name, status, last_started_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		initial.InstanceID, initial.Generation, initial.StatusRevision, initial.TmuxSession,
		initial.TmuxSocketName, initial.Status, runtimeUnix(initial.LastStartedAt))
	if err != nil {
		return false, err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if inserted == 0 {
		return false, nil
	}
	if !hasRuntimeSnapshot {
		bindings = make(map[string]RuntimeBinding)
		for kind, value := range legacyBindings(inst.ToolData) {
			bindings[kind] = RuntimeBinding{InstanceID: inst.ID, Kind: kind, Value: value}
		}
	}
	for kind := range bindings {
		if _, ok := bindingJSONKeys[kind]; !ok {
			return false, fmt.Errorf("initial binding has unsupported kind %q", kind)
		}
	}
	for _, kind := range bindingKinds {
		binding, ok := bindings[kind]
		if !ok {
			continue
		}
		if binding.InstanceID != "" && binding.InstanceID != inst.ID {
			return false, fmt.Errorf("initial %s binding belongs to %s, not %s", kind, binding.InstanceID, inst.ID)
		}
		if binding.Kind != "" && binding.Kind != kind {
			return false, fmt.Errorf("initial binding map key %s disagrees with kind %s", kind, binding.Kind)
		}
		if binding.Generation != initial.Generation {
			return false, fmt.Errorf("initial %s binding generation %d does not match runtime %d", kind, binding.Generation, initial.Generation)
		}
		detectedAt := binding.DetectedAt
		if !hasRuntimeSnapshot {
			detectedAt = legacyTimestamp(inst.ToolData, kind+"_detected_at")
		}
		if _, err := tx.Exec(`
			INSERT INTO instance_runtime_binding
				(instance_id, binding_kind, runtime_generation, binding_revision, binding_value, binding_detected_at)
			VALUES (?, ?, ?, ?, ?, ?)`,
			inst.ID, kind, initial.Generation, binding.Revision, binding.Value, runtimeUnix(detectedAt)); err != nil {
			return false, fmt.Errorf("initial %s binding for %s: %w", kind, inst.ID, err)
		}
	}
	return true, nil
}

func runtimeStateFromScanner(scanner interface{ Scan(...any) error }) (RuntimeState, bool, error) {
	var state RuntimeState
	var generation, revision uint64
	var startedAt int64
	err := scanner.Scan(&state.InstanceID, &generation, &revision, &state.TmuxSession, &state.TmuxSocketName, &state.Status, &startedAt)
	if err == sql.ErrNoRows {
		return RuntimeState{}, false, nil
	}
	if err != nil {
		return RuntimeState{}, false, err
	}
	state.Generation = generation
	state.StatusRevision = revision
	if startedAt > 0 {
		state.LastStartedAt = runtimeTime(startedAt)
	}
	return state, true, nil
}

func (s *StateDB) ReadRuntimeState(instanceID string) (RuntimeState, bool, error) {
	return runtimeStateFromScanner(s.db.QueryRow(`
		SELECT instance_id, runtime_generation, status_revision,
		       tmux_session, tmux_socket_name, status, last_started_at
		FROM instance_runtime_state WHERE instance_id = ?`, instanceID))
}

// EnsureRuntimeState creates generation zero for the exact persisted logical
// instance identified by expectedIncarnation. Existing authoritative state is
// returned unchanged. The parent check and possible insert share the same
// BEGIN IMMEDIATE reservation, so a delete/reinsert cannot redirect a stale
// initializer to a byte-identical replacement.
func (s *StateDB) EnsureRuntimeState(initial RuntimeState, expectedIncarnation string) (RuntimeState, error) {
	if initial.InstanceID == "" {
		return RuntimeState{}, ErrRuntimeGenerationConflict
	}
	if IsRuntimeDestructionReserved(initial) {
		return RuntimeState{}, ErrStatusRevisionConflict
	}
	var state RuntimeState
	err := withBusyRetry(func() error {
		return s.withImmediateTransaction(func(tx *immediateTransaction) error {
			if err := s.requireRuntimeWriterCompatibility(tx, time.Now()); err != nil {
				return err
			}
			if err := requireInstanceIncarnation(tx, initial.InstanceID, expectedIncarnation); err != nil {
				return err
			}
			if _, err := tx.Exec(`
				INSERT OR IGNORE INTO instance_runtime_state
					(instance_id, runtime_generation, status_revision, tmux_session, tmux_socket_name, status, last_started_at)
				VALUES (?, ?, ?, ?, ?, ?, ?)`,
				initial.InstanceID, initial.Generation, initial.StatusRevision,
				initial.TmuxSession, initial.TmuxSocketName, initial.Status, runtimeUnix(initial.LastStartedAt)); err != nil {
				return err
			}
			var found bool
			var err error
			state, found, err = runtimeStateFromScanner(tx.QueryRow(`
				SELECT instance_id, runtime_generation, status_revision,
				       tmux_session, tmux_socket_name, status, last_started_at
				FROM instance_runtime_state WHERE instance_id = ?`, initial.InstanceID))
			if err != nil {
				return err
			}
			if !found {
				return fmt.Errorf("%w: runtime state for logical instance %s was not initialized",
					ErrRuntimeGenerationConflict, initial.InstanceID)
			}
			return tx.Commit()
		})
	})
	if err != nil {
		return RuntimeState{}, err
	}
	return state, nil
}

// EnsureRuntimeStateForNewInstance supports the narrow create ordering where a
// physical runtime is published before its first metadata row. The caller's
// pre-minted incarnation is persisted atomically with the runtime. A competing
// same-ID creator with a different token loses without changing either row.
func (s *StateDB) EnsureRuntimeStateForNewInstance(initial RuntimeState, expectedIncarnation string) (RuntimeState, error) {
	if initial.InstanceID == "" {
		return RuntimeState{}, ErrRuntimeGenerationConflict
	}
	if IsRuntimeDestructionReserved(initial) {
		return RuntimeState{}, ErrStatusRevisionConflict
	}
	if expectedIncarnation == "" {
		return RuntimeState{}, ErrInstanceParentConflict
	}
	var state RuntimeState
	err := withBusyRetry(func() error {
		return s.withImmediateTransaction(func(tx *immediateTransaction) error {
			if err := s.requireRuntimeWriterCompatibility(tx, time.Now()); err != nil {
				return err
			}
			var parentExists int
			if err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM instances WHERE id = ?)`, initial.InstanceID).Scan(&parentExists); err != nil {
				return err
			}
			if parentExists != 0 {
				return ErrInstanceParentConflict
			}
			if _, err := tx.Exec(`
				INSERT OR IGNORE INTO instance_incarnation (instance_id, incarnation)
				VALUES (?, ?)`, initial.InstanceID, expectedIncarnation); err != nil {
				return err
			}
			if err := requireRuntimeIncarnation(tx, initial.InstanceID, expectedIncarnation); err != nil {
				return err
			}
			if _, err := tx.Exec(`
				INSERT OR IGNORE INTO instance_runtime_state
					(instance_id, runtime_generation, status_revision, tmux_session, tmux_socket_name, status, last_started_at)
				VALUES (?, ?, ?, ?, ?, ?, ?)`,
				initial.InstanceID, initial.Generation, initial.StatusRevision,
				initial.TmuxSession, initial.TmuxSocketName, initial.Status, runtimeUnix(initial.LastStartedAt)); err != nil {
				return err
			}
			var found bool
			var err error
			state, found, err = runtimeStateFromScanner(tx.QueryRow(`
				SELECT instance_id, runtime_generation, status_revision,
				       tmux_session, tmux_socket_name, status, last_started_at
				FROM instance_runtime_state WHERE instance_id = ?`, initial.InstanceID))
			if err != nil {
				return err
			}
			if !found {
				return fmt.Errorf("%w: runtime state for new instance %s was not initialized",
					ErrRuntimeGenerationConflict, initial.InstanceID)
			}
			return tx.Commit()
		})
	})
	if err != nil {
		return RuntimeState{}, err
	}
	return state, nil
}

// InsertInstanceIfAbsent restores one logical instance and its initial runtime
// atomically without overwriting a same-ID winner created by another process.
func (s *StateDB) InsertInstanceIfAbsent(inst *InstanceRow) (InstanceSeedToken, bool, error) {
	if inst == nil || inst.ID == "" || inst.Incarnation == "" {
		return InstanceSeedToken{}, false, fmt.Errorf("statedb: invalid instance restore")
	}
	parent := instanceSeedParentFromRow(inst)
	var seed InstanceSeedToken
	inserted := false
	err := withBusyRetry(func() error {
		seed = InstanceSeedToken{}
		inserted = false
		return s.withImmediateTransaction(func(tx *immediateTransaction) error {
			if err := s.requireRuntimeWriterCompatibility(tx, time.Now()); err != nil {
				return err
			}
			var parentExists int
			if err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM instances WHERE id = ?)`, inst.ID).Scan(&parentExists); err != nil {
				return err
			}
			if parentExists != 0 {
				return tx.Commit()
			}
			if _, err := tx.Exec(`
				INSERT OR IGNORE INTO instance_incarnation (instance_id, incarnation)
				VALUES (?, ?)`, inst.ID, inst.Incarnation); err != nil {
				return err
			}
			if err := requireRuntimeIncarnation(tx, inst.ID, inst.Incarnation); err != nil {
				return err
			}
			result, err := tx.Exec(`
				INSERT INTO instances (
					id, title, project_path, group_path, sort_order,
					command, wrapper, tool, status, tmux_session, tmux_socket_name,
					created_at, last_accessed,
					parent_session_id, is_conductor, no_transition_notify,
					worktree_path, worktree_repo, worktree_branch, account,
					archived_at, tool_data, title_locked, auto_name, auto_name_description, pin
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(id) DO NOTHING`,
				parent.id, parent.title, parent.projectPath, parent.groupPath, parent.order,
				parent.command, parent.wrapper, parent.tool, parent.status, parent.tmuxSession, parent.tmuxSocketName,
				parent.createdAt, parent.lastAccessed,
				parent.parentSessionID, parent.isConductor, parent.noTransitionNotify,
				parent.worktreePath, parent.worktreeRepo, parent.worktreeBranch, parent.account,
				parent.archivedAt, parent.toolData, parent.titleLocked,
				parent.autoName, parent.autoNameDescription, parent.pin)
			if err != nil {
				return err
			}
			rows, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if rows == 0 {
				return ErrInstanceParentConflict
			}
			if _, err := tx.Exec(`UPDATE instance_incarnation SET incarnation = ? WHERE instance_id = ?`,
				inst.Incarnation, inst.ID); err != nil {
				return err
			}
			if err := requireInstanceIncarnation(tx, inst.ID, inst.Incarnation); err != nil {
				return err
			}

			// The CLI fork path deliberately publishes a physical runtime before
			// inserting its parent. Adopt that already-authoritative row and its
			// bindings verbatim; initialize runtime only when none exists.
			runtime, runtimeFound, err := runtimeStateFromScanner(tx.QueryRow(`
				SELECT instance_id, runtime_generation, status_revision,
				       tmux_session, tmux_socket_name, status, last_started_at
				FROM instance_runtime_state WHERE instance_id = ?`, inst.ID))
			if err != nil {
				return err
			}
			if runtimeFound && IsRuntimeDestructionReserved(runtime) {
				return ErrStatusRevisionConflict
			}
			if !runtimeFound {
				runtimeInserted, err := insertInitialRuntimeTx(tx, inst)
				if err != nil {
					return err
				}
				if !runtimeInserted {
					return ErrRuntimeGenerationConflict
				}
				runtime, runtimeFound, err = runtimeStateFromScanner(tx.QueryRow(`
					SELECT instance_id, runtime_generation, status_revision,
					       tmux_session, tmux_socket_name, status, last_started_at
					FROM instance_runtime_state WHERE instance_id = ?`, inst.ID))
				if err != nil {
					return err
				}
				if !runtimeFound {
					return ErrRuntimeGenerationConflict
				}
			}

			allBindings, currentBindings, err := loadRuntimeBindingSnapshot(tx, inst.ID, runtime.Generation)
			if err != nil {
				return err
			}
			if err := projectRuntimeToolDataTx(tx, inst.ID); err != nil {
				return fmt.Errorf("project runtime tool data for %s: %w", inst.ID, err)
			}

			var incarnation string
			if err := tx.QueryRow(`SELECT incarnation FROM instance_incarnation WHERE instance_id = ?`, inst.ID).Scan(&incarnation); err != nil {
				return fmt.Errorf("load instance incarnation for %s: %w", inst.ID, err)
			}
			insertedParent, found, err := instanceSeedParentFromScanner(tx.QueryRow(`
				SELECT id, title, project_path, group_path, sort_order,
				       command, wrapper, tool, status, tmux_session, tmux_socket_name,
				       created_at, last_accessed, parent_session_id, is_conductor,
				       no_transition_notify, worktree_path, worktree_repo, worktree_branch,
				       account, archived_at, tool_data, title_locked, auto_name,
				       auto_name_description, pin, last_sent_at, acknowledged
				FROM instances WHERE id = ?`, inst.ID))
			if err != nil {
				return err
			}
			if !found {
				return ErrInstanceParentConflict
			}
			if _, err := tx.Exec(`INSERT OR REPLACE INTO metadata (key, value) VALUES ('last_modified', ?)`,
				fmt.Sprintf("%d", time.Now().UnixNano())); err != nil {
				return err
			}
			if err := tx.Commit(); err != nil {
				return err
			}
			seed = InstanceSeedToken{
				Runtime: runtime, Bindings: currentBindings, Incarnation: incarnation,
				parent: insertedParent, bindings: allBindings,
			}
			inst.Incarnation = incarnation
			inserted = true
			return nil
		})
	})
	return seed, inserted, err
}

// CommitRuntimeTransition atomically publishes a complete next runtime tuple.
// The generation must advance by exactly one.
func (s *StateDB) CommitRuntimeTransition(expectedGeneration uint64, expectedIncarnation string, next RuntimeState) error {
	return s.CommitRuntimeTransitionWithBindingPlan(expectedGeneration, expectedIncarnation, next, nil)
}

// CommitRuntimeTransitionWithBindingPlan atomically advances the runtime and
// every binding captured for its current generation. Current bindings may not
// be omitted: each must be explicitly carried or released.
func (s *StateDB) CommitRuntimeTransitionWithBindingPlan(expectedGeneration uint64, expectedIncarnation string, next RuntimeState, plan []RuntimeBindingTransition) error {
	if next.InstanceID == "" || next.Generation != expectedGeneration+1 {
		return fmt.Errorf("%w: expected %d, next %d", ErrRuntimeGenerationConflict, expectedGeneration, next.Generation)
	}
	if err := rejectRuntimeDestructionStatus(next.Status); err != nil {
		return err
	}
	planned := make(map[string]RuntimeBindingTransition, len(plan))
	for _, item := range plan {
		if _, ok := bindingJSONKeys[item.Kind]; !ok {
			return fmt.Errorf("unsupported runtime binding kind %q", item.Kind)
		}
		if _, exists := planned[item.Kind]; exists {
			return fmt.Errorf("duplicate runtime binding plan for %q", item.Kind)
		}
		planned[item.Kind] = item
	}
	return withBusyRetry(func() error {
		return s.withImmediateTransaction(func(tx *immediateTransaction) error {
			if err := s.requireRuntimeWriterCompatibility(tx, time.Now()); err != nil {
				return err
			}
			if err := requireRuntimeIncarnation(tx, next.InstanceID, expectedIncarnation); err != nil {
				return err
			}

			var currentGeneration, currentStatusRevision uint64
			var currentStatus string
			if err := tx.QueryRow(`SELECT runtime_generation, status_revision, status
				FROM instance_runtime_state WHERE instance_id = ?`, next.InstanceID).
				Scan(&currentGeneration, &currentStatusRevision, &currentStatus); err != nil {
				return err
			}
			if currentGeneration != expectedGeneration {
				return ErrRuntimeGenerationConflict
			}
			if currentStatus == runtimeDestructionStatus {
				return ErrStatusRevisionConflict
			}

			type currentBinding struct {
				generation uint64
				revision   uint64
			}
			current := make(map[string]currentBinding)
			rows, err := tx.Query(`SELECT binding_kind, runtime_generation, binding_revision
			FROM instance_runtime_binding WHERE instance_id = ?`, next.InstanceID)
			if err != nil {
				return err
			}
			for rows.Next() {
				var kind string
				var generation, revision uint64
				if err := rows.Scan(&kind, &generation, &revision); err != nil {
					_ = rows.Close()
					return err
				}
				current[kind] = currentBinding{generation: generation, revision: revision}
			}
			if err := rows.Err(); err != nil {
				_ = rows.Close()
				return err
			}
			if err := rows.Close(); err != nil {
				return err
			}
			for kind, binding := range current {
				if binding.generation == expectedGeneration {
					if _, ok := planned[kind]; !ok {
						return fmt.Errorf("%w: missing %s revision %d", ErrBindingPlanIncomplete, kind, binding.revision)
					}
				}
			}
			for kind, item := range planned {
				binding, found := current[kind]
				switch {
				case !found && item.ExpectedRevision != 0:
					return ErrBindingRevisionConflict
				case found && (binding.generation != expectedGeneration || binding.revision != item.ExpectedRevision):
					return ErrBindingRevisionConflict
				}
			}

			result, err := tx.Exec(`
			UPDATE instance_runtime_state
			SET runtime_generation = ?, status_revision = ?, tmux_session = ?,
			    tmux_socket_name = ?, status = ?, last_started_at = ?
			WHERE instance_id = ? AND runtime_generation = ?
			  AND status_revision = ? AND status = ? AND status <> ?`,
				next.Generation, next.StatusRevision, next.TmuxSession,
				next.TmuxSocketName, next.Status, runtimeUnix(next.LastStartedAt),
				next.InstanceID, expectedGeneration, currentStatusRevision,
				currentStatus, runtimeDestructionStatus)
			if err != nil {
				return err
			}
			affected, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if affected != 1 {
				return ErrRuntimeGenerationConflict
			}

			for _, item := range plan {
				detectedAt := runtimeUnix(item.DetectedAt)
				if _, found := current[item.Kind]; !found {
					if item.NextValue == "" {
						continue
					}
					if _, err := tx.Exec(`INSERT INTO instance_runtime_binding
					(instance_id, binding_kind, runtime_generation, binding_revision, binding_value, binding_detected_at)
					VALUES (?, ?, ?, 1, ?, ?)`, next.InstanceID, item.Kind, next.Generation, item.NextValue, detectedAt); err != nil {
						return err
					}
					continue
				}
				bindingResult, err := tx.Exec(`UPDATE instance_runtime_binding
				SET runtime_generation = ?, binding_revision = binding_revision + 1,
				    binding_value = ?, binding_detected_at = ?
				WHERE instance_id = ? AND binding_kind = ?
				  AND runtime_generation = ? AND binding_revision = ?`,
					next.Generation, item.NextValue, detectedAt, next.InstanceID, item.Kind,
					expectedGeneration, item.ExpectedRevision)
				if err != nil {
					return err
				}
				bindingAffected, err := bindingResult.RowsAffected()
				if err != nil {
					return err
				}
				if bindingAffected != 1 {
					return ErrBindingRevisionConflict
				}
			}

			if _, err := tx.Exec(`UPDATE instances
			SET status = ?, tmux_session = ?, tmux_socket_name = ? WHERE id = ?`,
				next.Status, next.TmuxSession, next.TmuxSocketName, next.InstanceID); err != nil {
				return err
			}
			if err := projectRuntimeToolDataTx(tx, next.InstanceID); err != nil {
				return err
			}
			if _, err := tx.Exec(`DELETE FROM instance_legacy_runtime_adoption WHERE instance_id = ?`, next.InstanceID); err != nil {
				return err
			}
			return tx.Commit()
		})
	})
}

// WriteStatusIfVersion publishes an observation only if both its runtime and
// same-generation status revision are still current. A pending legacy runtime
// adoption marker freezes this tuple until validation and consumption finish.
func (s *StateDB) WriteStatusIfVersion(instanceID, expectedIncarnation string, generation, expectedRevision uint64, status string) (bool, error) {
	if err := rejectRuntimeDestructionStatus(status); err != nil {
		return false, err
	}
	applied := false
	err := withBusyRetry(func() error {
		return s.withImmediateTransaction(func(tx *immediateTransaction) error {
			if err := s.requireRuntimeWriterCompatibility(tx, time.Now()); err != nil {
				return err
			}
			if err := requireRuntimeIncarnation(tx, instanceID, expectedIncarnation); err != nil {
				return err
			}
			result, err := tx.Exec(`
			UPDATE instance_runtime_state
			SET status = ?, status_revision = status_revision + 1
			WHERE instance_id = ? AND runtime_generation = ? AND status_revision = ?
			  AND status <> ?
			  AND NOT (
			      instance_runtime_state.runtime_generation = 0 AND EXISTS (
			          SELECT 1 FROM instance_legacy_runtime_adoption adoption
			          WHERE adoption.instance_id = instance_runtime_state.instance_id
			            AND adoption.incarnation = ?
			            AND adoption.tmux_session = instance_runtime_state.tmux_session
			            AND adoption.tmux_socket_name = instance_runtime_state.tmux_socket_name
			      )
			  )`,
				status, instanceID, generation, expectedRevision, runtimeDestructionStatus, expectedIncarnation)
			if err != nil {
				return err
			}
			rows, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if rows == 0 {
				return ErrStatusRevisionConflict
			}
			if _, err := tx.Exec(`UPDATE instances
			SET status = ?, acknowledged = CASE WHEN ? = 'running' THEN 0 ELSE acknowledged END
			WHERE id = ?`, status, status, instanceID); err != nil {
				return err
			}
			if err := tx.Commit(); err != nil {
				return err
			}
			applied = true
			return nil
		})
	})
	if errors.Is(err, ErrStatusRevisionConflict) {
		return false, nil
	}
	return applied, err
}

// SetArchivedIfRuntime changes archive metadata only while expected is still
// the complete authoritative runtime tuple. This prevents a delayed archive
// completion from hiding a replacement runtime that started after the kill.
func (s *StateDB) SetArchivedIfRuntime(expected RuntimeState, expectedIncarnation string, at time.Time) error {
	if expected.InstanceID == "" {
		return ErrRuntimeGenerationConflict
	}
	return withBusyRetry(func() error {
		return s.withImmediateTransaction(func(tx *immediateTransaction) error {
			if err := s.requireRuntimeWriterCompatibility(tx, time.Now()); err != nil {
				return err
			}
			if err := requireRuntimeIncarnation(tx, expected.InstanceID, expectedIncarnation); err != nil {
				return err
			}
			result, err := tx.Exec(`UPDATE instances SET archived_at = ?
			WHERE id = ? AND EXISTS (
				SELECT 1 FROM instance_runtime_state
				WHERE instance_id = ? AND runtime_generation = ? AND status_revision = ?
				  AND tmux_session = ? AND tmux_socket_name = ? AND status = ?
				  AND last_started_at = ?
			)`,
				archivedAtUnix(at), expected.InstanceID, expected.InstanceID,
				expected.Generation, expected.StatusRevision, expected.TmuxSession,
				expected.TmuxSocketName, expected.Status, runtimeUnix(expected.LastStartedAt))
			if err != nil {
				return err
			}
			rows, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if rows != 1 {
				return ErrRuntimeGenerationConflict
			}
			if _, err := tx.Exec(`INSERT OR REPLACE INTO metadata (key, value) VALUES ('last_modified', ?)`,
				fmt.Sprintf("%d", time.Now().UnixNano())); err != nil {
				return err
			}
			return tx.Commit()
		})
	})
}

func (s *StateDB) ReadRuntimeBinding(instanceID, kind string) (RuntimeBinding, bool, error) {
	var binding RuntimeBinding
	var generation, revision uint64
	var detectedAt int64
	err := s.db.QueryRow(`
		SELECT instance_id, binding_kind, runtime_generation, binding_revision, binding_value, binding_detected_at
		FROM instance_runtime_binding WHERE instance_id = ? AND binding_kind = ?`, instanceID, kind).
		Scan(&binding.InstanceID, &binding.Kind, &generation, &revision, &binding.Value, &detectedAt)
	if err == sql.ErrNoRows {
		return RuntimeBinding{}, false, nil
	}
	if err != nil {
		return RuntimeBinding{}, false, err
	}
	binding.Generation = generation
	binding.Revision = revision
	if detectedAt > 0 {
		binding.DetectedAt = runtimeTime(detectedAt)
	}
	return binding, true, nil
}

// WithRuntimeOwnerLease runs action only while expected still identifies the
// durable physical runtime for the captured incarnation. BEGIN IMMEDIATE keeps
// a delete/reinsert or runtime transition from invalidating that final check
// until the external action returns.
func (s *StateDB) WithRuntimeOwnerLease(expected RuntimeState, expectedIncarnation string, action func()) (bool, error) {
	if expected.InstanceID == "" || expected.Generation == 0 || action == nil {
		return false, nil
	}
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return false, err
	}
	finished := false
	defer func() {
		if !finished {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()
	if err := s.requireRuntimeWriterCompatibility(conn, time.Now()); err != nil {
		return false, err
	}
	tx := &immediateTransaction{ctx: ctx, conn: conn}
	if err := requireRuntimeIncarnation(tx, expected.InstanceID, expectedIncarnation); err != nil {
		return false, err
	}
	current, found, err := runtimeStateFromScanner(tx.QueryRow(`
		SELECT instance_id, runtime_generation, status_revision,
		       tmux_session, tmux_socket_name, status, last_started_at
		FROM instance_runtime_state WHERE instance_id = ?`, expected.InstanceID))
	if err != nil {
		return false, err
	}
	if !found || current.Generation != expected.Generation ||
		current.TmuxSession != expected.TmuxSession ||
		current.TmuxSocketName != expected.TmuxSocketName {
		return false, nil
	}

	action()
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return false, err
	}
	finished = true
	return true, nil
}

// WithRuntimeBindingOwnerLease runs action only while expected is still the
// durable physical runtime and binding is its sole current-generation owner.
// BEGIN IMMEDIATE keeps any binding or runtime commit from invalidating that
// final ownership check until action returns.
func (s *StateDB) WithRuntimeBindingOwnerLease(expected RuntimeState, expectedIncarnation string, binding RuntimeBinding, action func()) (bool, error) {
	return s.withRuntimeBindingOwnerLease(expected, expectedIncarnation, binding, nil, action)
}

// WithRuntimeBindingCleanupLease extends the source owner lease with the
// inventoried target's physical runtime. If that tuple is still the target's
// current durable runtime, its current binding must still match the candidate
// stamp. A missing or different binding is ambiguous and preserves the target;
// a tuple that is no longer current remains eligible stale-runtime cleanup.
func (s *StateDB) WithRuntimeBindingCleanupLease(expected RuntimeState, expectedIncarnation string, binding RuntimeBinding, target RuntimeState, action func()) (bool, error) {
	if target.InstanceID == "" || target.InstanceID == expected.InstanceID || target.Generation == 0 || target.TmuxSession == "" {
		return false, nil
	}
	return s.withRuntimeBindingOwnerLease(expected, expectedIncarnation, binding, &target, action)
}

func (s *StateDB) withRuntimeBindingOwnerLease(expected RuntimeState, expectedIncarnation string, binding RuntimeBinding, target *RuntimeState, action func()) (bool, error) {
	if expected.InstanceID == "" || expected.Generation == 0 || binding.Value == "" ||
		binding.InstanceID != expected.InstanceID || binding.Generation != expected.Generation || action == nil {
		return false, nil
	}
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return false, err
	}
	finished := false
	defer func() {
		if !finished {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()
	if err := s.requireRuntimeWriterCompatibility(conn, time.Now()); err != nil {
		return false, err
	}
	parentTx := &immediateTransaction{ctx: ctx, conn: conn}
	if err := requireRuntimeIncarnation(parentTx, expected.InstanceID, expectedIncarnation); err != nil {
		return false, err
	}

	var owners, exact int
	err = conn.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(CASE
			WHEN b.instance_id = ? AND b.runtime_generation = ? AND b.binding_revision = ?
			 AND r.tmux_session = ? AND r.tmux_socket_name = ? THEN 1 ELSE 0 END), 0)
		FROM instance_runtime_binding b
		JOIN instance_runtime_state r
		  ON r.instance_id = b.instance_id AND r.runtime_generation = b.runtime_generation
		WHERE b.binding_kind = ? AND b.binding_value = ?`,
		expected.InstanceID, expected.Generation, binding.Revision,
		expected.TmuxSession, expected.TmuxSocketName, binding.Kind, binding.Value).
		Scan(&owners, &exact)
	if err != nil {
		return false, err
	}
	if owners != 1 || exact != 1 {
		return false, nil
	}
	if target != nil {
		// A target row that still names this exact physical candidate must prove
		// the same binding advertised by its cleanup stamp. NOT EXISTS makes a
		// missing binding fail closed as well as a different current value.
		var targetBindingMismatch int
		err = conn.QueryRowContext(ctx, `
			SELECT EXISTS(
				SELECT 1
				FROM instance_runtime_state r
				WHERE r.instance_id = ? AND r.runtime_generation = ?
				  AND r.tmux_session = ? AND r.tmux_socket_name = ?
				  AND NOT EXISTS(
					SELECT 1 FROM instance_runtime_binding b
					WHERE b.instance_id = r.instance_id
					  AND b.runtime_generation = r.runtime_generation
					  AND b.binding_kind = ? AND b.binding_value = ?
				  )
			)`,
			target.InstanceID, target.Generation, target.TmuxSession, target.TmuxSocketName,
			binding.Kind, binding.Value).Scan(&targetBindingMismatch)
		if err != nil {
			return false, err
		}
		if targetBindingMismatch != 0 {
			return false, nil
		}
	}

	action()
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return false, err
	}
	finished = true
	return true, nil
}

// CommitRuntimeBinding changes one binding only while its runtime generation
// and binding revision remain current. Non-empty values have one durable owner.
func (s *StateDB) CommitRuntimeBinding(instanceID, expectedIncarnation string, generation uint64, kind string, expectedRevision uint64, nextValue string) (RuntimeBinding, error) {
	return s.commitRuntimeBinding(instanceID, expectedIncarnation, generation, kind, expectedRevision, nextValue, time.Time{}, nil)
}

// WriteRuntimeBindingIfVersion publishes a detected tool binding only when
// the observing runtime generation and binding revision are still current.
// The authoritative row and legacy JSON projection commit together.
func (s *StateDB) WriteRuntimeBindingIfVersion(instanceID, expectedIncarnation string, generation uint64, kind string, expectedRevision uint64, nextValue string, detectedAt time.Time) (RuntimeBinding, bool, error) {
	return s.WriteRuntimeBindingIfVersionWithCommitFence(
		instanceID, expectedIncarnation, generation, kind, expectedRevision, nextValue, detectedAt, nil,
	)
}

// WriteRuntimeBindingIfVersionWithCommitFence runs beforeCommit after the exact
// binding update has been staged under BEGIN IMMEDIATE, but before SQLite makes
// it visible. The callback must be bounded. An error rolls the database change
// back; a process exit after the callback leaves any external cleanup authority
// invalidated before the durable binding can change.
func (s *StateDB) WriteRuntimeBindingIfVersionWithCommitFence(instanceID, expectedIncarnation string, generation uint64, kind string, expectedRevision uint64, nextValue string, detectedAt time.Time, beforeCommit func() error) (RuntimeBinding, bool, error) {
	binding, err := s.commitRuntimeBinding(
		instanceID, expectedIncarnation, generation, kind, expectedRevision, nextValue, detectedAt, beforeCommit,
	)
	if errors.Is(err, ErrRuntimeGenerationConflict) || errors.Is(err, ErrBindingRevisionConflict) {
		return RuntimeBinding{}, false, nil
	}
	return binding, err == nil, err
}

// withRuntimeBindingCommitFence is the bounded external-action counterpart to
// withImmediateTransaction. Runtime cleanup invalidation must remain inside the
// writer reservation that proved the binding CAS, so an obsolete process cannot
// mutate tmux after merely checking stale local state.
func (s *StateDB) withRuntimeBindingCommitFence(op func(*immediateTransaction) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), immediateTransactionTimeout)
	defer cancel()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	tx := &immediateTransaction{ctx: ctx, conn: conn}
	defer tx.rollback()
	if s.testAfterImmediateBegin != nil {
		s.testAfterImmediateBegin()
	}
	return op(tx)
}

func (s *StateDB) commitRuntimeBinding(instanceID, expectedIncarnation string, generation uint64, kind string, expectedRevision uint64, nextValue string, detectedAt time.Time, beforeCommit func() error) (RuntimeBinding, error) {
	if _, ok := bindingJSONKeys[kind]; !ok {
		return RuntimeBinding{}, fmt.Errorf("unsupported runtime binding kind %q", kind)
	}
	var committed RuntimeBinding
	withTransaction := s.withImmediateTransaction
	if beforeCommit != nil {
		withTransaction = s.withRuntimeBindingCommitFence
	}
	err := withBusyRetry(func() error {
		return withTransaction(func(tx *immediateTransaction) error {
			if err := s.requireRuntimeWriterCompatibility(tx, time.Now()); err != nil {
				return err
			}
			if err := requireRuntimeIncarnation(tx, instanceID, expectedIncarnation); err != nil {
				return err
			}
			var currentGeneration uint64
			if err := tx.QueryRow(`SELECT runtime_generation FROM instance_runtime_state WHERE instance_id = ?`, instanceID).Scan(&currentGeneration); err != nil {
				return err
			}
			if currentGeneration != generation {
				return ErrRuntimeGenerationConflict
			}
			var bindingGeneration, currentRevision uint64
			queryErr := tx.QueryRow(`SELECT runtime_generation, binding_revision
			FROM instance_runtime_binding WHERE instance_id = ? AND binding_kind = ?`, instanceID, kind).
				Scan(&bindingGeneration, &currentRevision)
			nextDetectedAt := runtimeUnix(detectedAt)
			switch {
			case queryErr == sql.ErrNoRows && expectedRevision == 0:
				_, queryErr = tx.Exec(`INSERT INTO instance_runtime_binding
				(instance_id, binding_kind, runtime_generation, binding_revision, binding_value, binding_detected_at)
				VALUES (?, ?, ?, 1, ?, ?)`, instanceID, kind, generation, nextValue, nextDetectedAt)
				currentRevision = 0
			case queryErr == sql.ErrNoRows:
				return ErrBindingRevisionConflict
			case queryErr != nil:
				return queryErr
			case bindingGeneration != generation:
				return ErrBindingRevisionConflict
			case currentRevision != expectedRevision:
				return ErrBindingRevisionConflict
			default:
				result, updateErr := tx.Exec(`UPDATE instance_runtime_binding
				SET runtime_generation = ?, binding_revision = binding_revision + 1,
				    binding_value = ?, binding_detected_at = ?
				WHERE instance_id = ? AND binding_kind = ?
				  AND runtime_generation = ? AND binding_revision = ?`,
					generation, nextValue, nextDetectedAt, instanceID, kind, generation, expectedRevision)
				if updateErr != nil {
					return updateErr
				}
				rows, rowsErr := result.RowsAffected()
				if rowsErr != nil {
					return rowsErr
				}
				if rows != 1 {
					return ErrBindingRevisionConflict
				}
			}
			if queryErr != nil {
				return queryErr
			}
			key := bindingJSONKeys[kind]
			detectedKey := kind + "_detected_at"
			if _, err := tx.Exec(`UPDATE instances SET tool_data = json_set(
			COALESCE(NULLIF(tool_data, ''), '{}'), '$.' || ?, ?, '$.' || ?, ?) WHERE id = ?`,
				key, nextValue, detectedKey, legacyRuntimeUnix(nextDetectedAt), instanceID); err != nil {
				return err
			}
			if beforeCommit != nil {
				if err := beforeCommit(); err != nil {
					return err
				}
			}
			if err := tx.Commit(); err != nil {
				return err
			}
			committed = RuntimeBinding{
				InstanceID: instanceID, Kind: kind, Generation: generation,
				Revision: expectedRevision + 1, Value: nextValue,
			}
			if nextDetectedAt > 0 {
				committed.DetectedAt = runtimeTime(nextDetectedAt)
			}
			return nil
		})
	})
	return committed, err
}

func overlayBindings(toolData json.RawMessage, bindings map[string]RuntimeBinding) json.RawMessage {
	var values map[string]json.RawMessage
	if json.Unmarshal(toolData, &values) != nil {
		return toolData
	}
	if values == nil {
		values = make(map[string]json.RawMessage)
	}
	for kind, key := range bindingJSONKeys {
		delete(values, key)
		delete(values, kind+"_detected_at")
	}
	for kind, binding := range bindings {
		key, ok := bindingJSONKeys[kind]
		if !ok || binding.Value == "" {
			continue
		}
		encoded, _ := json.Marshal(binding.Value)
		values[key] = encoded
		if !binding.DetectedAt.IsZero() {
			values[kind+"_detected_at"] = json.RawMessage(strconv.FormatInt(binding.DetectedAt.Unix(), 10))
		}
	}
	out, err := json.Marshal(values)
	if err != nil {
		return toolData
	}
	return out
}

// projectRuntimeToolDataTx rebuilds compatibility JSON exclusively from the
// authoritative runtime tables while the metadata save holds the same write
// transaction. A binding CAS can therefore commit before this snapshot or
// after this transaction, but can never be overwritten between prefetch and
// upsert by stale JSON.
type runtimeQueryExecutor interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

func projectRuntimeToolDataTx(tx runtimeQueryExecutor, instanceID string) error {
	var raw string
	if err := tx.QueryRow(`SELECT tool_data FROM instances WHERE id = ?`, instanceID).Scan(&raw); err == sql.ErrNoRows {
		// A physical start may commit before a new TUI/web instance has its
		// metadata row. The authoritative runtime remains valid; the first
		// metadata upsert below will project it.
		return nil
	} else if err != nil {
		return err
	}
	values := make(map[string]json.RawMessage)
	_ = json.Unmarshal([]byte(raw), &values)
	for key := range runtimeToolDataKeys {
		delete(values, key)
	}
	var lastStartedAt int64
	if err := tx.QueryRow(`SELECT last_started_at FROM instance_runtime_state WHERE instance_id = ?`, instanceID).Scan(&lastStartedAt); err != nil {
		return err
	}
	if lastStartedAt > 0 {
		values["last_started_at"] = json.RawMessage(strconv.FormatInt(legacyRuntimeUnix(lastStartedAt), 10))
	}
	rows, err := tx.Query(`
		SELECT b.binding_kind, b.binding_value, b.binding_detected_at
		FROM instance_runtime_binding b
		JOIN instance_runtime_state r ON r.instance_id = b.instance_id
		WHERE b.instance_id = ? AND b.runtime_generation = r.runtime_generation
		  AND b.binding_value <> ''
		ORDER BY binding_kind`, instanceID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, value string
		var detectedAt int64
		if err := rows.Scan(&kind, &value, &detectedAt); err != nil {
			return err
		}
		key, ok := bindingJSONKeys[kind]
		if !ok {
			continue
		}
		encoded, _ := json.Marshal(value)
		values[key] = encoded
		if detectedAt > 0 {
			values[kind+"_detected_at"] = json.RawMessage(strconv.FormatInt(legacyRuntimeUnix(detectedAt), 10))
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	projected, err := json.Marshal(values)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE instances
		SET tool_data = ?,
		    status = (
		      SELECT CASE WHEN runtime.status = ? THEN reservation.prior_status ELSE runtime.status END
		      FROM instance_runtime_state runtime
		      LEFT JOIN instance_incarnation token
		        ON token.instance_id = runtime.instance_id
		      LEFT JOIN instance_runtime_destruction reservation
		        ON reservation.instance_id = runtime.instance_id
		       AND reservation.incarnation = token.incarnation
		       AND reservation.runtime_generation = runtime.runtime_generation
		       AND reservation.claimed_status_revision = runtime.status_revision
		      WHERE runtime.instance_id = ?
		    ),
		    tmux_session = (SELECT tmux_session FROM instance_runtime_state WHERE instance_id = ?),
		    tmux_socket_name = (SELECT tmux_socket_name FROM instance_runtime_state WHERE instance_id = ?)
		WHERE id = ?`, string(projected), runtimeDestructionStatus,
		instanceID, instanceID, instanceID, instanceID)
	return err
}
