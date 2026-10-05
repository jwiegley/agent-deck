package statedb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

// Low-level helpers used by the cross-profile migration code in
// internal/session/profile_migrate.go (issue #928). Each writer is wrapped in
// withBusyRetry because cross-profile moves race against background
// heartbeat/status writers on the source DB.

// ErrMigrationSnapshotConflict means the source or rollback target changed
// after the migration captured it. No rows are deleted on this error.
var ErrMigrationSnapshotConflict = errors.New("statedb: migration snapshot conflict")

// ErrMigrationTargetConflict means the destination no longer contains the
// complete transferred source snapshot. It is distinct from a source CAS
// conflict so callers can report the profile that changed without hiding the
// source error needed to drive a safe rollback.
var ErrMigrationTargetConflict = errors.New("statedb: migration target conflict")

// MigrationSnapshot is the complete transferable state for one instance.
// Private fields retain exact database representations for deletion CAS.
type MigrationSnapshot struct {
	Instance      *InstanceRow
	Runtime       *RuntimeState
	CostEvents    []*CostEventRow
	WatcherEvents []*WatcherEventRow
	incarnation   string
	origin        string
	metadata      migrationInstanceSnapshot
	bindings      []RuntimeBinding
}

type migrationInstanceSnapshot struct {
	ID, Title, ProjectPath, GroupPath                      string
	Command, Wrapper, Tool, Status                         string
	TmuxSession, TmuxSocketName                            string
	ParentSessionID, WorktreePath, WorktreeRepo            string
	WorktreeBranch, Account, ToolData                      string
	AutoNameDescription, Pin                               string
	Order                                                  int
	CreatedAt, LastAccessed, ArchivedAt, LastSentAt        int64
	IsConductor, NoTransitionNotify, TitleLocked, AutoName int
	Acknowledged                                           int
}

type migrationQueryer interface {
	Query(string, ...any) (*sql.Rows, error)
	QueryRow(string, ...any) *sql.Row
}

type migrationCommitFunc func(*sql.Tx) error

func commitMigrationTransaction(tx *sql.Tx) error { return tx.Commit() }

// LoadInstanceByID returns the row with the given id, or (nil, nil) if it
// does not exist. Any other error (driver, schema, etc.) is returned as-is.
func (s *StateDB) LoadInstanceByID(id string) (*InstanceRow, error) {
	snapshot, err := s.loadMigrationCoreSnapshot(id)
	if err != nil || snapshot == nil {
		return nil, err
	}
	return snapshot.Instance, nil
}

// LoadMigrationSnapshot reads metadata, runtime, bindings, cost events, and
// watcher events from one SQLite snapshot.
func (s *StateDB) LoadMigrationSnapshot(id string) (*MigrationSnapshot, error) {
	tx, err := s.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	snapshot, err := loadMigrationSnapshot(tx, id)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (s *StateDB) loadMigrationCoreSnapshot(id string) (*MigrationSnapshot, error) {
	tx, err := s.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	snapshot, err := loadMigrationCore(tx, id)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func loadMigrationSnapshot(q migrationQueryer, id string) (*MigrationSnapshot, error) {
	snapshot, err := loadMigrationCore(q, id)
	if err != nil || snapshot == nil {
		return snapshot, err
	}
	snapshot.CostEvents, err = loadMigrationCostEvents(q, id)
	if err != nil {
		return nil, err
	}
	snapshot.WatcherEvents, err = loadMigrationWatcherEvents(q, id)
	if err != nil {
		return nil, err
	}
	return snapshot, nil
}

func loadMigrationCore(q migrationQueryer, id string) (*MigrationSnapshot, error) {
	var metadata migrationInstanceSnapshot
	err := q.QueryRow(`
		SELECT id, title, project_path, group_path, sort_order,
			command, wrapper, tool, status, tmux_session, tmux_socket_name,
			created_at, last_accessed, parent_session_id, is_conductor,
			no_transition_notify, worktree_path, worktree_repo, worktree_branch,
			account, archived_at, tool_data, title_locked, auto_name,
			auto_name_description, pin, last_sent_at, acknowledged
		FROM instances WHERE id = ?`, id).Scan(
		&metadata.ID, &metadata.Title, &metadata.ProjectPath, &metadata.GroupPath, &metadata.Order,
		&metadata.Command, &metadata.Wrapper, &metadata.Tool, &metadata.Status,
		&metadata.TmuxSession, &metadata.TmuxSocketName, &metadata.CreatedAt,
		&metadata.LastAccessed, &metadata.ParentSessionID, &metadata.IsConductor,
		&metadata.NoTransitionNotify, &metadata.WorktreePath, &metadata.WorktreeRepo,
		&metadata.WorktreeBranch, &metadata.Account, &metadata.ArchivedAt,
		&metadata.ToolData, &metadata.TitleLocked, &metadata.AutoName,
		&metadata.AutoNameDescription, &metadata.Pin, &metadata.LastSentAt,
		&metadata.Acknowledged,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var incarnation, origin string
	if err := q.QueryRow(`SELECT incarnation, origin_incarnation FROM instance_incarnation WHERE instance_id = ?`, id).Scan(&incarnation, &origin); err != nil {
		return nil, err
	}

	row := metadata.instanceRow()
	row.Incarnation = incarnation
	runtime, found, err := runtimeStateFromScanner(q.QueryRow(`
		SELECT instance_id, runtime_generation, status_revision,
		       tmux_session, tmux_socket_name, status, last_started_at
		FROM instance_runtime_state WHERE instance_id = ?`, id))
	if err != nil {
		return nil, err
	}
	var runtimePtr *RuntimeState
	if found {
		runtimePtr = &runtime
		row.RuntimeGeneration = runtime.Generation
		row.StatusRevision = runtime.StatusRevision
		row.TmuxSession = runtime.TmuxSession
		row.TmuxSocketName = runtime.TmuxSocketName
		if IsRuntimeDestructionReserved(runtime) {
			// Keep the private marker in MigrationSnapshot.Runtime for lifecycle
			// authority and migration rejection, while every metadata projection
			// exposes only the prior status proved in this same read snapshot.
			prior, priorErr := runtimeDestructionPriorStatus(q, runtime, incarnation)
			if priorErr != nil {
				return nil, priorErr
			}
			row.Status = prior
		} else {
			row.Status = runtime.Status
		}
		row.LastStartedAt = runtime.LastStartedAt
	}

	row.RuntimeBindings = make(map[string]RuntimeBinding)
	bindings, err := q.Query(`
		SELECT binding_kind, runtime_generation, binding_revision, binding_value, binding_detected_at
		FROM instance_runtime_binding WHERE instance_id = ? ORDER BY binding_kind`, id)
	if err != nil {
		return nil, err
	}
	var bindingSnapshots []RuntimeBinding
	for bindings.Next() {
		binding := RuntimeBinding{InstanceID: id}
		var generation, revision uint64
		var detectedAt int64
		if err := bindings.Scan(&binding.Kind, &generation, &revision, &binding.Value, &detectedAt); err != nil {
			_ = bindings.Close()
			return nil, err
		}
		binding.Generation = generation
		binding.Revision = revision
		if detectedAt > 0 {
			binding.DetectedAt = runtimeTime(detectedAt)
		}
		bindingSnapshots = append(bindingSnapshots, binding)
		if binding.Generation == row.RuntimeGeneration {
			row.RuntimeBindings[binding.Kind] = binding
		}
	}
	if err := bindings.Err(); err != nil {
		_ = bindings.Close()
		return nil, err
	}
	if err := bindings.Close(); err != nil {
		return nil, err
	}
	row.ToolData = overlayBindings(row.ToolData, row.RuntimeBindings)

	return &MigrationSnapshot{
		Instance: row, Runtime: runtimePtr, incarnation: incarnation, origin: origin,
		metadata: metadata, bindings: bindingSnapshots,
	}, nil
}

func (metadata migrationInstanceSnapshot) instanceRow() *InstanceRow {
	row := &InstanceRow{
		ID: metadata.ID, Title: metadata.Title, ProjectPath: metadata.ProjectPath,
		GroupPath: metadata.GroupPath, Order: metadata.Order, Command: metadata.Command,
		Wrapper: metadata.Wrapper, Tool: metadata.Tool, Status: metadata.Status,
		TmuxSession: metadata.TmuxSession, TmuxSocketName: metadata.TmuxSocketName,
		ParentSessionID: metadata.ParentSessionID, IsConductor: metadata.IsConductor != 0,
		NoTransitionNotify: metadata.NoTransitionNotify != 0, WorktreePath: metadata.WorktreePath,
		WorktreeRepo: metadata.WorktreeRepo, WorktreeBranch: metadata.WorktreeBranch,
		Account: metadata.Account, ToolData: json.RawMessage(metadata.ToolData),
		TitleLocked: metadata.TitleLocked != 0, AutoName: metadata.AutoName != 0,
		AutoNameDescription: metadata.AutoNameDescription, Pin: metadata.Pin,
		CreatedAt: time.Unix(metadata.CreatedAt, 0),
	}
	if metadata.LastAccessed > 0 {
		row.LastAccessed = time.Unix(metadata.LastAccessed, 0)
	}
	if metadata.ArchivedAt > 0 {
		row.ArchivedAt = time.Unix(metadata.ArchivedAt, 0).UTC()
	}
	return row
}

// LoadInstanceChildren returns rows whose parent_session_id matches the given id.
func (s *StateDB) LoadInstanceChildren(parentID string) ([]*InstanceRow, error) {
	all, err := s.LoadInstances()
	if err != nil {
		return nil, err
	}
	var out []*InstanceRow
	for _, row := range all {
		if row.ParentSessionID == parentID {
			out = append(out, row)
		}
	}
	return out, nil
}

// LoadInstancesByGroup returns rows whose group_path exactly matches the given path.
func (s *StateDB) LoadInstancesByGroup(groupPath string) ([]*InstanceRow, error) {
	all, err := s.LoadInstances()
	if err != nil {
		return nil, err
	}
	var out []*InstanceRow
	for _, row := range all {
		if row.GroupPath == groupPath {
			out = append(out, row)
		}
	}
	return out, nil
}

// InsertInstanceRow inserts a single instance row. Unlike SaveInstance it does
// not merge tool_data extras — cross-profile migration is a verbatim transfer
// and the caller has already prepared the row.
func (s *StateDB) InsertInstanceRow(inst *InstanceRow) error {
	snapshot, err := migrationSnapshotFromInstance(inst)
	if err != nil {
		return err
	}
	_, err = s.insertMigrationSnapshot(snapshot)
	return err
}

// InsertInstanceRowForMigration copies the complete captured metadata,
// runtime, and binding snapshot. It returns the exact target core so a later
// rollback can be compare-and-swap safe.
func (s *StateDB) InsertInstanceRowForMigration(source *MigrationSnapshot) (*MigrationSnapshot, error) {
	target, err := migrationTargetCore(source)
	if err != nil {
		return nil, err
	}
	return s.insertMigrationSnapshot(target)
}

func migrationTargetCore(source *MigrationSnapshot) (*MigrationSnapshot, error) {
	if source == nil || source.Instance == nil || source.Instance.ID == "" ||
		source.metadata.ID != source.Instance.ID {
		return nil, ErrMigrationSnapshotConflict
	}
	instance := *source.Instance
	instance.Incarnation = ""
	target := &MigrationSnapshot{
		Instance: &instance,
		origin:   source.incarnation,
		metadata: source.metadata,
		bindings: append([]RuntimeBinding(nil), source.bindings...),
	}
	if source.Runtime != nil {
		runtime := *source.Runtime
		target.Runtime = &runtime
	} else {
		// Legacy rows without authoritative runtime state become generation zero
		// at the destination, matching ordinary first-open migration behavior.
		target.Runtime = &RuntimeState{
			InstanceID:     source.Instance.ID,
			Generation:     source.Instance.RuntimeGeneration,
			StatusRevision: source.Instance.StatusRevision,
			TmuxSession:    source.Instance.TmuxSession,
			TmuxSocketName: source.Instance.TmuxSocketName,
			Status:         source.Instance.Status,
			LastStartedAt:  legacyLastStartedAt(json.RawMessage(source.metadata.ToolData)),
		}
	}
	return target, nil
}

func migrationSnapshotFromInstance(inst *InstanceRow) (*MigrationSnapshot, error) {
	if inst == nil || inst.ID == "" {
		return nil, fmt.Errorf("statedb: instance id is required")
	}
	toolData := inst.ToolData
	if len(toolData) == 0 {
		toolData = json.RawMessage("{}")
	}
	bindings := inst.RuntimeBindings
	if bindings == nil {
		bindings = make(map[string]RuntimeBinding)
		for kind, value := range legacyBindings(inst.ToolData) {
			bindings[kind] = RuntimeBinding{
				InstanceID: inst.ID, Kind: kind, Generation: inst.RuntimeGeneration,
				Value: value, DetectedAt: legacyTimestamp(inst.ToolData, kind+"_detected_at"),
			}
		}
	}
	bindingSnapshots := make([]RuntimeBinding, 0, len(bindings))
	for kind, binding := range bindings {
		if binding.InstanceID != "" && binding.InstanceID != inst.ID {
			return nil, fmt.Errorf("runtime binding %s belongs to %s, not %s", kind, binding.InstanceID, inst.ID)
		}
		if binding.Generation != inst.RuntimeGeneration {
			return nil, fmt.Errorf("runtime binding %s generation %d does not match runtime %d", kind, binding.Generation, inst.RuntimeGeneration)
		}
		binding.InstanceID = inst.ID
		binding.Kind = kind
		bindingSnapshots = append(bindingSnapshots, binding)
	}
	sort.Slice(bindingSnapshots, func(i, j int) bool { return bindingSnapshots[i].Kind < bindingSnapshots[j].Kind })
	lastStartedAt := inst.LastStartedAt
	if lastStartedAt.IsZero() {
		lastStartedAt = legacyLastStartedAt(inst.ToolData)
	}
	runtime := &RuntimeState{
		InstanceID:     inst.ID,
		Generation:     inst.RuntimeGeneration,
		StatusRevision: inst.StatusRevision,
		TmuxSession:    inst.TmuxSession,
		TmuxSocketName: inst.TmuxSocketName,
		Status:         inst.Status,
		LastStartedAt:  lastStartedAt,
	}
	return &MigrationSnapshot{
		Instance: inst, Runtime: runtime,
		origin:   inst.Incarnation,
		metadata: migrationMetadataFromInsertedInstance(inst, string(toolData)),
		bindings: bindingSnapshots,
	}, nil
}

func (s *StateDB) insertMigrationSnapshot(snapshot *MigrationSnapshot) (*MigrationSnapshot, error) {
	metadata := snapshot.metadata
	runtime := snapshot.Runtime
	if snapshot.Instance == nil || metadata.ID == "" || runtime == nil ||
		metadata.ID != snapshot.Instance.ID || runtime.InstanceID != metadata.ID {
		return nil, ErrMigrationSnapshotConflict
	}
	if err := rejectRuntimeDestructionStatus(runtime.Status); err != nil {
		return nil, err
	}
	bindings := append([]RuntimeBinding(nil), snapshot.bindings...)
	sort.Slice(bindings, func(i, j int) bool { return bindings[i].Kind < bindings[j].Kind })
	for _, binding := range bindings {
		if binding.InstanceID != metadata.ID || binding.Kind == "" {
			return nil, ErrMigrationSnapshotConflict
		}
	}
	var inserted *MigrationSnapshot
	err := withBusyRetry(func() error {
		inserted = nil
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.Exec(`
			INSERT INTO instances (
				id, title, project_path, group_path, sort_order,
				command, wrapper, tool, status, tmux_session, tmux_socket_name,
				created_at, last_accessed,
				parent_session_id, is_conductor, no_transition_notify,
				worktree_path, worktree_repo, worktree_branch, account,
				archived_at, title_locked, auto_name, auto_name_description, pin,
				last_sent_at, tool_data, acknowledged
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`,
			metadata.ID, metadata.Title, metadata.ProjectPath, metadata.GroupPath, metadata.Order,
			metadata.Command, metadata.Wrapper, metadata.Tool, metadata.Status,
			metadata.TmuxSession, metadata.TmuxSocketName, metadata.CreatedAt, metadata.LastAccessed,
			metadata.ParentSessionID, metadata.IsConductor, metadata.NoTransitionNotify,
			metadata.WorktreePath, metadata.WorktreeRepo, metadata.WorktreeBranch, metadata.Account,
			metadata.ArchivedAt, metadata.TitleLocked, metadata.AutoName,
			metadata.AutoNameDescription, metadata.Pin, metadata.LastSentAt,
			metadata.ToolData, metadata.Acknowledged,
		); err != nil {
			return err
		}
		// The trigger minted a fresh destination identity. Retain the source
		// identity only as lineage so a crash retry can recognize this transfer
		// without letting an old rollback token authorize a replacement.
		if snapshot.origin != "" {
			if _, err := tx.Exec(`UPDATE instance_incarnation SET origin_incarnation = ? WHERE instance_id = ?`, snapshot.origin, metadata.ID); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`INSERT INTO instance_runtime_state
			(instance_id, runtime_generation, status_revision, tmux_session, tmux_socket_name, status, last_started_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			runtime.InstanceID, runtime.Generation, runtime.StatusRevision, runtime.TmuxSession,
			runtime.TmuxSocketName, runtime.Status, runtimeUnix(runtime.LastStartedAt)); err != nil {
			return err
		}
		for _, binding := range bindings {
			if _, err := tx.Exec(`INSERT INTO instance_runtime_binding
				(instance_id, binding_kind, runtime_generation, binding_revision, binding_value, binding_detected_at)
				VALUES (?, ?, ?, ?, ?, ?)`, binding.InstanceID, binding.Kind, binding.Generation,
				binding.Revision, binding.Value, runtimeUnix(binding.DetectedAt)); err != nil {
				return err
			}
		}
		inserted, err = loadMigrationCore(tx, metadata.ID)
		if err != nil {
			return err
		}
		if inserted == nil {
			return ErrMigrationSnapshotConflict
		}
		if inserted.incarnation == "" {
			return ErrMigrationSnapshotConflict
		}
		return tx.Commit()
	})
	if err != nil {
		return nil, err
	}
	return inserted, nil
}

func migrationMetadataFromInsertedInstance(inst *InstanceRow, toolData string) migrationInstanceSnapshot {
	boolInt := func(value bool) int {
		if value {
			return 1
		}
		return 0
	}
	return migrationInstanceSnapshot{
		ID: inst.ID, Title: inst.Title, ProjectPath: inst.ProjectPath, GroupPath: inst.GroupPath,
		Order: inst.Order, Command: inst.Command, Wrapper: inst.Wrapper, Tool: inst.Tool,
		Status: inst.Status, TmuxSession: inst.TmuxSession, TmuxSocketName: inst.TmuxSocketName,
		CreatedAt: inst.CreatedAt.Unix(), LastAccessed: instLastAccessedUnix(inst),
		ParentSessionID: inst.ParentSessionID, IsConductor: boolInt(inst.IsConductor),
		NoTransitionNotify: boolInt(inst.NoTransitionNotify), WorktreePath: inst.WorktreePath,
		WorktreeRepo: inst.WorktreeRepo, WorktreeBranch: inst.WorktreeBranch, Account: inst.Account,
		ArchivedAt: archivedAtUnix(inst.ArchivedAt), ToolData: toolData,
		TitleLocked: boolInt(inst.TitleLocked), AutoName: boolInt(inst.AutoName),
		AutoNameDescription: inst.AutoNameDescription, Pin: inst.Pin,
		LastSentAt: 0, Acknowledged: 0,
	}
}

// DeleteMigrationSource atomically deletes a complete captured source only if
// metadata, runtime, bindings, and every associated event are still exact.
func (s *StateDB) DeleteMigrationSource(expected *MigrationSnapshot) error {
	if expected == nil || expected.Instance == nil || expected.Instance.ID == "" {
		return ErrMigrationSnapshotConflict
	}
	return withBusyRetry(func() error {
		return s.withImmediateTransaction(func(tx *immediateTransaction) error {
			if err := s.requireRuntimeWriterCompatibility(tx, time.Now()); err != nil {
				return err
			}
			current, err := loadMigrationSnapshot(tx, expected.Instance.ID)
			if err != nil {
				return err
			}
			if !sameMigrationSnapshot(current, expected) {
				return ErrMigrationSnapshotConflict
			}
			if err := deleteAllMigrationRows(tx, expected.Instance.ID); err != nil {
				return err
			}
			return tx.Commit()
		})
	})
}

// RollbackMigrationTarget removes only rows inserted by this migration. Extra
// pre-existing or concurrently appended target events are never selected.
func (s *StateDB) RollbackMigrationTarget(expected *MigrationSnapshot, insertedCosts []*CostEventRow, insertedWatchers []*WatcherEventRow) error {
	if expected != nil && (expected.Instance == nil || expected.Instance.ID == "") {
		return ErrMigrationSnapshotConflict
	}
	if expected == nil && len(insertedCosts) == 0 && len(insertedWatchers) == 0 {
		return nil
	}
	return withBusyRetry(func() error {
		return s.withImmediateTransaction(func(tx *immediateTransaction) error {
			if err := s.requireRuntimeWriterCompatibility(tx, time.Now()); err != nil {
				return err
			}
			if expected != nil {
				current, err := loadMigrationCore(tx, expected.Instance.ID)
				if err != nil {
					return err
				}
				if !sameMigrationCore(current, expected) {
					return ErrMigrationSnapshotConflict
				}
			}
			for _, event := range insertedCosts {
				if event == nil {
					continue
				}
				budgetStop := 0
				if event.BudgetStopTriggered {
					budgetStop = 1
				}
				result, err := tx.Exec(`DELETE FROM cost_events
					WHERE id = ? AND session_id = ? AND timestamp = ? AND model = ?
					  AND input_tokens = ? AND output_tokens = ? AND cache_read_tokens = ?
					  AND cache_write_tokens = ? AND cost_microdollars = ?
					  AND budget_stop_triggered = ?`, event.ID, event.SessionID, event.Timestamp,
					event.Model, event.InputTokens, event.OutputTokens, event.CacheReadTokens,
					event.CacheWriteTokens, event.CostMicrodollars, budgetStop)
				if err != nil {
					return err
				}
				if rows, err := result.RowsAffected(); err != nil || rows != 1 {
					if err != nil {
						return err
					}
					return ErrMigrationSnapshotConflict
				}
			}
			for _, event := range insertedWatchers {
				if event == nil {
					continue
				}
				result, err := tx.Exec(`DELETE FROM watcher_events
					WHERE id = ? AND watcher_id = ? AND dedup_key = ? AND sender = ?
					  AND subject = ? AND routed_to = ? AND session_id = ?
					  AND triage_session_id = ? AND body = ? AND created_at = ?`,
					event.ID, event.WatcherID, event.DedupKey, event.Sender, event.Subject,
					event.RoutedTo, event.SessionID, event.TriageSessionID, event.Body,
					event.CreatedAt.Unix())
				if err != nil {
					return err
				}
				if rows, err := result.RowsAffected(); err != nil || rows != 1 {
					if err != nil {
						return err
					}
					return ErrMigrationSnapshotConflict
				}
			}
			if expected != nil {
				if _, err := tx.Exec(`DELETE FROM instance_runtime_binding WHERE instance_id = ?`, expected.Instance.ID); err != nil {
					return err
				}
				if _, err := tx.Exec(`DELETE FROM instance_runtime_state WHERE instance_id = ?`, expected.Instance.ID); err != nil {
					return err
				}
				result, err := tx.Exec(`DELETE FROM instance_incarnation WHERE instance_id = ? AND incarnation = ?`, expected.Instance.ID, expected.incarnation)
				if err != nil {
					return err
				}
				if rows, err := result.RowsAffected(); err != nil || rows != 1 {
					if err != nil {
						return err
					}
					return ErrMigrationSnapshotConflict
				}
				result, err = tx.Exec(`DELETE FROM instances WHERE id = ?`, expected.Instance.ID)
				if err != nil {
					return err
				}
				if rows, err := result.RowsAffected(); err != nil || rows != 1 {
					if err != nil {
						return err
					}
					return ErrMigrationSnapshotConflict
				}
			}
			return tx.Commit()
		})
	})
}

func sameMigrationSnapshot(left, right *MigrationSnapshot) bool {
	return sameMigrationCore(left, right) &&
		sameCostEvents(left.CostEvents, right.CostEvents) &&
		sameWatcherEvents(left.WatcherEvents, right.WatcherEvents)
}

// MigrationCoreMatchesTransfer reports whether target is the exact core that
// inserting source would produce, including legacy generation-zero runtime
// normalization.
func MigrationCoreMatchesTransfer(source, target *MigrationSnapshot) bool {
	expected, err := migrationTargetCore(source)
	return err == nil && migrationTransferIncarnationMatches(source, target) &&
		sameMigrationTransferCore(expected, target)
}

// WithMigrationTargetLease holds the destination writer slot while verifying
// that it still contains the complete source snapshot and while op removes the
// source. The target transaction intentionally makes no writes: its rollback
// is the lease release, so a successful source deletion cannot be followed by
// a target commit failure. Concurrent target deletion or replacement must wait
// until op has completed.
func (s *StateDB) WithMigrationTargetLease(source *MigrationSnapshot, op func() error) error {
	if source == nil || source.Instance == nil || source.Instance.ID == "" || op == nil {
		return ErrMigrationTargetConflict
	}
	return withBusyRetry(func() error {
		return s.withImmediateTransaction(func(tx *immediateTransaction) error {
			if err := s.requireRuntimeWriterCompatibility(tx, time.Now()); err != nil {
				return err
			}
			current, err := loadMigrationSnapshot(tx, source.Instance.ID)
			if err != nil {
				return err
			}
			if !migrationTargetContainsSource(source, current) {
				return ErrMigrationTargetConflict
			}
			return op()
		})
	})
}

func migrationTargetContainsSource(source, target *MigrationSnapshot) bool {
	expectedCore, err := migrationTargetCore(source)
	if err != nil || !migrationTransferIncarnationMatches(source, target) ||
		!sameMigrationTransferCore(expectedCore, target) {
		return false
	}
	targetCosts := make(map[string]*CostEventRow, len(target.CostEvents))
	for _, event := range target.CostEvents {
		if event != nil {
			targetCosts[event.ID] = event
		}
	}
	for _, event := range source.CostEvents {
		if event == nil || !sameCostEvent(event, targetCosts[event.ID]) {
			return false
		}
	}

	type watcherKey struct{ watcherID, dedupKey string }
	targetWatchers := make(map[watcherKey]*WatcherEventRow, len(target.WatcherEvents))
	for _, event := range target.WatcherEvents {
		if event != nil {
			targetWatchers[watcherKey{event.WatcherID, event.DedupKey}] = event
		}
	}
	for _, event := range source.WatcherEvents {
		if event == nil || !sameWatcherEventPayload(event,
			targetWatchers[watcherKey{event.WatcherID, event.DedupKey}]) {
			return false
		}
	}
	return true
}

func sameMigrationCore(left, right *MigrationSnapshot) bool {
	return left != nil && right != nil && left.incarnation == right.incarnation && left.origin == right.origin &&
		sameMigrationTransferCore(left, right)
}

func migrationTransferIncarnationMatches(source, target *MigrationSnapshot) bool {
	return source != nil && target != nil && source.incarnation != "" &&
		target.incarnation != "" && target.incarnation != source.incarnation &&
		target.origin == source.incarnation
}

func sameMigrationTransferCore(left, right *MigrationSnapshot) bool {
	if left == nil || right == nil || left.metadata != right.metadata ||
		!sameMigrationRuntime(left.Runtime, right.Runtime) || len(left.bindings) != len(right.bindings) {
		return false
	}
	for index := range left.bindings {
		if !sameMigrationBinding(left.bindings[index], right.bindings[index]) {
			return false
		}
	}
	return true
}

func sameMigrationRuntime(left, right *RuntimeState) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.InstanceID == right.InstanceID && left.Generation == right.Generation &&
		left.StatusRevision == right.StatusRevision && left.TmuxSession == right.TmuxSession &&
		left.TmuxSocketName == right.TmuxSocketName && left.Status == right.Status &&
		left.LastStartedAt.UnixNano() == right.LastStartedAt.UnixNano()
}

func sameMigrationBinding(left, right RuntimeBinding) bool {
	return left.InstanceID == right.InstanceID && left.Kind == right.Kind &&
		left.Generation == right.Generation && left.Revision == right.Revision &&
		left.Value == right.Value && left.DetectedAt.UnixNano() == right.DetectedAt.UnixNano()
}

func sameCostEvents(left, right []*CostEventRow) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if !sameCostEvent(left[index], right[index]) {
			return false
		}
	}
	return true
}

func sameCostEvent(left, right *CostEventRow) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func sameWatcherEvents(left, right []*WatcherEventRow) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] == nil || right[index] == nil {
			if left[index] != nil || right[index] != nil {
				return false
			}
			continue
		}
		if left[index].ID != right[index].ID ||
			!sameWatcherEventPayload(left[index], right[index]) {
			return false
		}
	}
	return true
}

func sameWatcherEventPayload(left, right *WatcherEventRow) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.WatcherID == right.WatcherID && left.DedupKey == right.DedupKey &&
		left.Sender == right.Sender && left.Subject == right.Subject &&
		left.RoutedTo == right.RoutedTo && left.SessionID == right.SessionID &&
		left.TriageSessionID == right.TriageSessionID && left.Body == right.Body &&
		left.CreatedAt.Unix() == right.CreatedAt.Unix()
}

func deleteAllMigrationRows(tx *immediateTransaction, instanceID string) error {
	if _, err := tx.Exec(`DELETE FROM watcher_events WHERE session_id = ? OR triage_session_id = ?`, instanceID, instanceID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM cost_events WHERE session_id = ?`, instanceID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM instance_runtime_binding WHERE instance_id = ?`, instanceID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM instance_runtime_state WHERE instance_id = ?`, instanceID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM instance_incarnation WHERE instance_id = ?`, instanceID); err != nil {
		return err
	}
	result, err := tx.Exec(`DELETE FROM instances WHERE id = ?`, instanceID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrMigrationSnapshotConflict
	}
	return nil
}

// --- cost_events round-trip ---

// LoadCostEventsForSession returns every cost_events row matching session_id.
// Timestamp is preserved verbatim as TEXT — we can't reliably round-trip
// through time.Time without risking timezone drift.
func (s *StateDB) LoadCostEventsForSession(sessionID string) ([]*CostEventRow, error) {
	return loadMigrationCostEvents(s.db, sessionID)
}

func loadMigrationCostEvents(q migrationQueryer, sessionID string) ([]*CostEventRow, error) {
	rows, err := q.Query(`
		SELECT id, session_id, timestamp, model,
			input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
			cost_microdollars, budget_stop_triggered
		FROM cost_events WHERE session_id = ? ORDER BY id
	`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*CostEventRow
	for rows.Next() {
		r := &CostEventRow{}
		var budgetStop int
		if err := rows.Scan(
			&r.ID, &r.SessionID, &r.Timestamp, &r.Model,
			&r.InputTokens, &r.OutputTokens, &r.CacheReadTokens, &r.CacheWriteTokens,
			&r.CostMicrodollars, &budgetStop,
		); err != nil {
			return nil, err
		}
		r.BudgetStopTriggered = budgetStop != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// InsertCostEventRow inserts a single cost_events row verbatim, preserving id
// (which is also the dedup key in costs.WriteCostEvent — using INSERT OR
// IGNORE makes the migration safely retriable).
func (s *StateDB) InsertCostEventRow(ev *CostEventRow) error {
	budgetStop := 0
	if ev.BudgetStopTriggered {
		budgetStop = 1
	}
	return withBusyRetry(func() error {
		_, err := s.db.Exec(`INSERT OR IGNORE INTO cost_events (
			id, session_id, timestamp, model, input_tokens, output_tokens,
			cache_read_tokens, cache_write_tokens, cost_microdollars,
			budget_stop_triggered
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			ev.ID, ev.SessionID, ev.Timestamp, ev.Model, ev.InputTokens,
			ev.OutputTokens, ev.CacheReadTokens, ev.CacheWriteTokens,
			ev.CostMicrodollars, budgetStop)
		return err
	})
}

// InsertCostEventRowForMigration reports the exact row inserted by this call.
// A nil row means the destination already owned an identical primary key.
func (s *StateDB) InsertCostEventRowForMigration(ev *CostEventRow) (*CostEventRow, error) {
	return s.insertCostEventRowForMigration(ev, commitMigrationTransaction)
}

func (s *StateDB) insertCostEventRowForMigration(ev *CostEventRow, commit migrationCommitFunc) (*CostEventRow, error) {
	budgetStop := 0
	if ev.BudgetStopTriggered {
		budgetStop = 1
	}
	var inserted *CostEventRow
	err := withBusyRetry(func() error {
		inserted = nil
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		result, err := tx.Exec(`
			INSERT INTO cost_events (
				id, session_id, timestamp, model,
				input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
				cost_microdollars, budget_stop_triggered
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO NOTHING
		`,
			ev.ID, ev.SessionID, ev.Timestamp, ev.Model,
			ev.InputTokens, ev.OutputTokens, ev.CacheReadTokens, ev.CacheWriteTokens,
			ev.CostMicrodollars, budgetStop,
		)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if rows == 1 {
			copy := *ev
			inserted = &copy
			return commit(tx)
		}
		existing, err := loadCostEventByID(tx, ev.ID)
		if err != nil {
			return err
		}
		if !sameCostEvent(existing, ev) {
			return fmt.Errorf("%w: cost event %s differs at destination", ErrMigrationSnapshotConflict, ev.ID)
		}
		return commit(tx)
	})
	return inserted, err
}

func loadCostEventByID(q migrationQueryer, id string) (*CostEventRow, error) {
	row := &CostEventRow{}
	var budgetStop int
	err := q.QueryRow(`SELECT id, session_id, timestamp, model,
		input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
		cost_microdollars, budget_stop_triggered
		FROM cost_events WHERE id = ?`, id).Scan(
		&row.ID, &row.SessionID, &row.Timestamp, &row.Model,
		&row.InputTokens, &row.OutputTokens, &row.CacheReadTokens, &row.CacheWriteTokens,
		&row.CostMicrodollars, &budgetStop,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	row.BudgetStopTriggered = budgetStop != 0
	return row, nil
}

// --- watcher_events round-trip ---

// LoadWatcherEventsForSession returns every watcher_events row whose
// session_id OR triage_session_id matches sessionID. The dual match preserves
// triage links when migrating a triage target.
func (s *StateDB) LoadWatcherEventsForSession(sessionID string) ([]*WatcherEventRow, error) {
	return loadMigrationWatcherEvents(s.db, sessionID)
}

func loadMigrationWatcherEvents(q migrationQueryer, sessionID string) ([]*WatcherEventRow, error) {
	rows, err := q.Query(`
		SELECT id, watcher_id, dedup_key, sender, subject, routed_to,
			session_id, triage_session_id, body, created_at
		FROM watcher_events WHERE session_id = ? OR triage_session_id = ?
		ORDER BY id
	`, sessionID, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*WatcherEventRow
	for rows.Next() {
		var r WatcherEventRow
		var createdAt int64
		if err := rows.Scan(
			&r.ID, &r.WatcherID, &r.DedupKey, &r.Sender, &r.Subject, &r.RoutedTo,
			&r.SessionID, &r.TriageSessionID, &r.Body, &createdAt,
		); err != nil {
			return nil, err
		}
		r.CreatedAt = time.Unix(createdAt, 0)
		out = append(out, &r)
	}
	return out, rows.Err()
}

// InsertWatcherEventRow inserts a watcher_events row with INSERT OR IGNORE
// against the (watcher_id, dedup_key) UNIQUE constraint — safe to retry.
// Note: the source row's `id` (auto-increment) is intentionally NOT preserved;
// the unique constraint is what dedupes across DBs.
func (s *StateDB) InsertWatcherEventRow(ev *WatcherEventRow) error {
	createdAt := ev.CreatedAt.Unix()
	if createdAt <= 0 {
		createdAt = time.Now().Unix()
	}
	return withBusyRetry(func() error {
		_, err := s.db.Exec(`INSERT OR IGNORE INTO watcher_events (
			watcher_id, dedup_key, sender, subject, routed_to,
			session_id, triage_session_id, body, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			ev.WatcherID, ev.DedupKey, ev.Sender, ev.Subject, ev.RoutedTo,
			ev.SessionID, ev.TriageSessionID, ev.Body, createdAt)
		return err
	})
}

// InsertWatcherEventRowForMigration returns the destination row, including its
// generated ID, only when this call inserted it. A dedup collision succeeds
// only when every transferable field already matches.
func (s *StateDB) InsertWatcherEventRowForMigration(ev *WatcherEventRow) (*WatcherEventRow, error) {
	return s.insertWatcherEventRowForMigration(ev, commitMigrationTransaction)
}

func (s *StateDB) insertWatcherEventRowForMigration(ev *WatcherEventRow, commit migrationCommitFunc) (*WatcherEventRow, error) {
	createdAt := ev.CreatedAt.Unix()
	if createdAt <= 0 {
		createdAt = time.Now().Unix()
	}
	var inserted *WatcherEventRow
	err := withBusyRetry(func() error {
		inserted = nil
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		result, err := tx.Exec(`
			INSERT INTO watcher_events (
				watcher_id, dedup_key, sender, subject, routed_to,
				session_id, triage_session_id, body, created_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(watcher_id, dedup_key) DO NOTHING
		`,
			ev.WatcherID, ev.DedupKey, ev.Sender, ev.Subject, ev.RoutedTo,
			ev.SessionID, ev.TriageSessionID, ev.Body, createdAt,
		)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if rows == 1 {
			insertedID, err := result.LastInsertId()
			if err != nil {
				return err
			}
			copy := *ev
			copy.ID = insertedID
			copy.CreatedAt = time.Unix(createdAt, 0)
			inserted = &copy
			return commit(tx)
		}
		existing, err := loadWatcherEventByDedup(tx, ev.WatcherID, ev.DedupKey)
		if err != nil {
			return err
		}
		expected := *ev
		expected.CreatedAt = time.Unix(createdAt, 0)
		if !sameWatcherEventPayload(existing, &expected) {
			return fmt.Errorf("%w: watcher event %s/%s differs at destination",
				ErrMigrationSnapshotConflict, ev.WatcherID, ev.DedupKey)
		}
		return commit(tx)
	})
	return inserted, err
}

func loadWatcherEventByDedup(q migrationQueryer, watcherID, dedupKey string) (*WatcherEventRow, error) {
	row := &WatcherEventRow{}
	var createdAt int64
	err := q.QueryRow(`SELECT id, watcher_id, dedup_key, sender, subject,
		routed_to, session_id, triage_session_id, body, created_at
		FROM watcher_events WHERE watcher_id = ? AND dedup_key = ?`, watcherID, dedupKey).Scan(
		&row.ID, &row.WatcherID, &row.DedupKey, &row.Sender, &row.Subject,
		&row.RoutedTo, &row.SessionID, &row.TriageSessionID, &row.Body, &createdAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	row.CreatedAt = time.Unix(createdAt, 0)
	return row, nil
}

// LoadWatcherByID returns the watcher row with the given id, or (nil, nil)
// if absent. Used by cross-profile migration to copy referenced watcher rows
// from src to dst before inserting watcher_events (the events table FK-
// references watchers(id) with foreign_keys=on).
func (s *StateDB) LoadWatcherByID(id string) (*WatcherRow, error) {
	var w WatcherRow
	var createdAt, updatedAt int64
	err := s.db.QueryRow(`
		SELECT id, name, type, config_path, status, conductor, created_at, updated_at
		FROM watchers WHERE id = ?
	`, id).Scan(&w.ID, &w.Name, &w.Type, &w.ConfigPath, &w.Status, &w.Conductor, &createdAt, &updatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	w.CreatedAt = time.Unix(createdAt, 0)
	w.UpdatedAt = time.Unix(updatedAt, 0)
	return &w, nil
}

// --- group round-trip ---

// LoadGroup returns the row for the given path, or (nil, nil) if absent.
func (s *StateDB) LoadGroup(path string) (*GroupRow, error) {
	var g GroupRow
	var expanded int
	err := s.db.QueryRow(`
		SELECT path, name, expanded, sort_order, default_path
		FROM groups WHERE path = ?
	`, path).Scan(&g.Path, &g.Name, &expanded, &g.Order, &g.DefaultPath)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	g.Expanded = expanded != 0
	return &g, nil
}

// SaveGroup inserts or replaces a single group row.
func (s *StateDB) SaveGroup(g *GroupRow) error {
	if g == nil || g.Path == "" {
		return fmt.Errorf("statedb: SaveGroup requires non-empty path")
	}
	expanded := 0
	if g.Expanded {
		expanded = 1
	}
	return withBusyRetry(func() error {
		_, err := s.db.Exec(`
			INSERT OR REPLACE INTO groups (path, name, expanded, sort_order, default_path)
			VALUES (?, ?, ?, ?, ?)
		`, g.Path, g.Name, expanded, g.Order, g.DefaultPath)
		return err
	})
}

// instLastAccessedUnix returns LastAccessed as a unix timestamp, or 0 if zero.
// SaveInstance uses .Unix() directly which surfaces -6795364578871 for the
// zero time — avoid that for cross-profile migration so rows look natural in
// the destination DB.
func instLastAccessedUnix(inst *InstanceRow) int64 {
	if inst.LastAccessed.IsZero() {
		return 0
	}
	return inst.LastAccessed.Unix()
}
