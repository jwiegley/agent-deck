package statedb

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

const runtimeDestructionStatus = "__agentdeck_stopping"

func rejectRuntimeDestructionStatus(status string) error {
	if status == runtimeDestructionStatus {
		return ErrStatusRevisionConflict
	}
	return nil
}

// IsRuntimeDestructionReserved reports whether a durable runtime is frozen at
// the crash boundary between its final identity check and physical teardown.
func IsRuntimeDestructionReserved(state RuntimeState) bool {
	return state.Status == runtimeDestructionStatus
}

func sameRuntimeForDestruction(current, expected RuntimeState) error {
	if current.InstanceID != expected.InstanceID ||
		current.Generation != expected.Generation ||
		current.TmuxSession != expected.TmuxSession ||
		current.TmuxSocketName != expected.TmuxSocketName {
		return ErrRuntimeGenerationConflict
	}
	if current.StatusRevision != expected.StatusRevision || current.Status != expected.Status {
		return ErrStatusRevisionConflict
	}
	return nil
}

func requireInstanceIncarnation(tx interface{ QueryRow(string, ...any) *sql.Row }, instanceID, expected string) error {
	if instanceID == "" || expected == "" {
		return ErrInstanceParentConflict
	}
	var current string
	err := tx.QueryRow(`
		SELECT token.incarnation
		FROM instances parent
		JOIN instance_incarnation token ON token.instance_id = parent.id
		WHERE parent.id = ?`, instanceID).Scan(&current)
	if err == sql.ErrNoRows {
		return ErrInstanceParentConflict
	}
	if err != nil {
		return err
	}
	if current != expected {
		return ErrInstanceParentConflict
	}
	return nil
}

func runtimeDestructionPriorStatus(
	tx interface{ QueryRow(string, ...any) *sql.Row },
	claimed RuntimeState,
	expectedIncarnation string,
) (string, error) {
	var prior string
	err := tx.QueryRow(`
		SELECT prior_status
		FROM instance_runtime_destruction
		WHERE instance_id = ? AND incarnation = ?
		  AND runtime_generation = ? AND claimed_status_revision = ?`,
		claimed.InstanceID, expectedIncarnation, claimed.Generation, claimed.StatusRevision,
	).Scan(&prior)
	if err == sql.ErrNoRows {
		return "", ErrStatusRevisionConflict
	}
	if err != nil {
		return "", err
	}
	if prior == "" || prior == runtimeDestructionStatus {
		return "", ErrStatusRevisionConflict
	}
	return prior, nil
}

// ValidateInstanceIncarnation verifies that instanceID still names the exact
// durable parent insertion captured by expected. It is an early fail-closed
// check for callers before external reconciliation; destructive commits must
// still use the incarnation-aware BEGIN IMMEDIATE APIs below.
func (s *StateDB) ValidateInstanceIncarnation(instanceID, expected string) error {
	tx, err := s.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireInstanceIncarnation(tx, instanceID, expected); err != nil {
		return err
	}
	return tx.Commit()
}

// ReserveRuntimeDestruction prevents ordinary status publishers from
// acquiring a newer revision between the final tuple check and process kill.
// Re-selecting an already-reserved tuple is idempotent for crash recovery.
func (s *StateDB) ReserveRuntimeDestruction(expected RuntimeState, expectedIncarnation string) (RuntimeState, error) {
	if expected.InstanceID == "" || expectedIncarnation == "" {
		if expected.InstanceID != "" {
			return RuntimeState{}, ErrInstanceParentConflict
		}
		return RuntimeState{}, ErrRuntimeGenerationConflict
	}
	var claimed RuntimeState
	err := withBusyRetry(func() error {
		return s.withImmediateTransaction(func(tx *immediateTransaction) error {
			if err := s.requireRuntimeWriterCompatibility(tx, time.Now()); err != nil {
				return err
			}
			if err := requireInstanceIncarnation(tx, expected.InstanceID, expectedIncarnation); err != nil {
				return err
			}
			current, found, err := runtimeStateFromScanner(tx.QueryRow(`
			SELECT instance_id, runtime_generation, status_revision,
			       tmux_session, tmux_socket_name, status, last_started_at
			FROM instance_runtime_state WHERE instance_id = ?`, expected.InstanceID))
			if err != nil {
				return err
			}
			if !found {
				return ErrRuntimeGenerationConflict
			}
			if err := sameRuntimeForDestruction(current, expected); err != nil {
				return err
			}
			if _, err := tx.Exec(`DELETE FROM instance_legacy_runtime_adoption WHERE instance_id = ?`, expected.InstanceID); err != nil {
				return err
			}
			if current.Status == runtimeDestructionStatus {
				if _, err := runtimeDestructionPriorStatus(tx, current, expectedIncarnation); err != nil {
					return err
				}
				claimed = current
				return tx.Commit()
			}
			if err := rejectRuntimeDestructionStatus(expected.Status); err != nil {
				return err
			}
			if _, err := tx.Exec(`INSERT INTO instance_runtime_destruction
				(instance_id, incarnation, runtime_generation, claimed_status_revision, prior_status)
				VALUES (?, ?, ?, ?, ?)`, expected.InstanceID, expectedIncarnation,
				expected.Generation, expected.StatusRevision+1, expected.Status); err != nil {
				return err
			}
			result, err := tx.Exec(`UPDATE instance_runtime_state
			SET status = ?, status_revision = status_revision + 1
			WHERE instance_id = ? AND runtime_generation = ? AND status_revision = ?
			  AND tmux_session = ? AND tmux_socket_name = ? AND status = ?`,
				runtimeDestructionStatus, expected.InstanceID, expected.Generation,
				expected.StatusRevision, expected.TmuxSession, expected.TmuxSocketName, expected.Status)
			if err != nil {
				return err
			}
			rows, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if rows != 1 {
				return ErrStatusRevisionConflict
			}
			if err := tx.Commit(); err != nil {
				return err
			}
			claimed = expected
			claimed.Status = runtimeDestructionStatus
			claimed.StatusRevision++
			return nil
		})
	})
	return claimed, err
}

// CompleteRuntimeDestruction releases a reserved tuple to its terminal status.
func (s *StateDB) CompleteRuntimeDestruction(claimed RuntimeState, expectedIncarnation, status string) (RuntimeState, error) {
	return s.completeRuntimeDestruction(claimed, expectedIncarnation, status, false)
}

// RestoreRuntimeDestruction releases a crash-stranded reservation back to the
// exact status captured by ReserveRuntimeDestruction. It is used only when a
// complete inventory proves that the reserved physical runtime is still live.
func (s *StateDB) RestoreRuntimeDestruction(claimed RuntimeState, expectedIncarnation string) (RuntimeState, error) {
	return s.completeRuntimeDestruction(claimed, expectedIncarnation, "", true)
}

func (s *StateDB) completeRuntimeDestruction(claimed RuntimeState, expectedIncarnation, status string, restorePrior bool) (RuntimeState, error) {
	if claimed.Status != runtimeDestructionStatus {
		return RuntimeState{}, ErrStatusRevisionConflict
	}
	if expectedIncarnation == "" {
		return RuntimeState{}, ErrInstanceParentConflict
	}
	if !restorePrior {
		if status == "" {
			return RuntimeState{}, ErrStatusRevisionConflict
		}
		if err := rejectRuntimeDestructionStatus(status); err != nil {
			return RuntimeState{}, err
		}
	}
	completed := RuntimeState{}
	err := withBusyRetry(func() error {
		return s.withImmediateTransaction(func(tx *immediateTransaction) error {
			// ReserveRuntimeDestruction established writer compatibility before it
			// durably published runtimeDestructionStatus. A legacy writer may
			// register after that reservation and before the external teardown
			// finishes; do not strand the exact reserved tuple in that case.
			if err := requireInstanceIncarnation(tx, claimed.InstanceID, expectedIncarnation); err != nil {
				return err
			}
			priorStatus, err := runtimeDestructionPriorStatus(tx, claimed, expectedIncarnation)
			if err != nil {
				return err
			}
			if restorePrior {
				status = priorStatus
			}
			if _, err := tx.Exec(`DELETE FROM instance_legacy_runtime_adoption WHERE instance_id = ?`, claimed.InstanceID); err != nil {
				return err
			}
			result, err := tx.Exec(`UPDATE instance_runtime_state
			SET status = ?, status_revision = status_revision + 1
			WHERE instance_id = ? AND runtime_generation = ? AND status_revision = ?
			  AND tmux_session = ? AND tmux_socket_name = ? AND status = ?`,
				status, claimed.InstanceID, claimed.Generation, claimed.StatusRevision,
				claimed.TmuxSession, claimed.TmuxSocketName, runtimeDestructionStatus)
			if err != nil {
				return err
			}
			rows, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if rows != 1 {
				return ErrStatusRevisionConflict
			}
			if _, err := tx.Exec(`UPDATE instances SET status = ? WHERE id = ?`, status, claimed.InstanceID); err != nil {
				return err
			}
			result, err = tx.Exec(`DELETE FROM instance_runtime_destruction
				WHERE instance_id = ? AND incarnation = ?
				  AND runtime_generation = ? AND claimed_status_revision = ?`,
				claimed.InstanceID, expectedIncarnation, claimed.Generation, claimed.StatusRevision)
			if err != nil {
				return err
			}
			rows, err = result.RowsAffected()
			if err != nil {
				return err
			}
			if rows != 1 {
				return ErrStatusRevisionConflict
			}
			if err := tx.Commit(); err != nil {
				return err
			}
			completed = claimed
			completed.Status = status
			completed.StatusRevision++
			return nil
		})
	})
	return completed, err
}

// DeleteInstanceIfRuntime deletes one logical instance only while the complete
// captured physical-runtime identity is still authoritative. The runtime row,
// its bindings, and the compatibility instance row are removed atomically.
func (s *StateDB) DeleteInstanceIfRuntime(expected RuntimeState, expectedIncarnation string) error {
	if expected.InstanceID == "" {
		return ErrRuntimeGenerationConflict
	}
	if expectedIncarnation == "" {
		return ErrInstanceParentConflict
	}
	return withBusyRetry(func() error {
		return s.withImmediateTransaction(func(tx *immediateTransaction) error {
			// A reserved tuple already crossed the writer-compatibility fence in
			// ReserveRuntimeDestruction. Let its exact deletion finish even if a
			// legacy writer registered while the physical teardown was in flight.
			// Non-reserved deletion remains fenced.
			if expected.Status != runtimeDestructionStatus {
				if err := s.requireRuntimeWriterCompatibility(tx, time.Now()); err != nil {
					return err
				}
			}
			if err := requireInstanceIncarnation(tx, expected.InstanceID, expectedIncarnation); err != nil {
				return err
			}

			current, found, err := runtimeStateFromScanner(tx.QueryRow(`
			SELECT instance_id, runtime_generation, status_revision,
			       tmux_session, tmux_socket_name, status, last_started_at
			FROM instance_runtime_state WHERE instance_id = ?`, expected.InstanceID))
			if err != nil {
				return err
			}
			if !found {
				return ErrRuntimeGenerationConflict
			}
			if err := sameRuntimeForDestruction(current, expected); err != nil {
				return err
			}
			if expected.Status == runtimeDestructionStatus {
				if _, err := runtimeDestructionPriorStatus(tx, expected, expectedIncarnation); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(`DELETE FROM instance_legacy_runtime_adoption WHERE instance_id = ?`, expected.InstanceID); err != nil {
				return err
			}

			result, err := tx.Exec(`DELETE FROM instance_runtime_state
			WHERE instance_id = ? AND runtime_generation = ? AND status_revision = ?
			  AND tmux_session = ? AND tmux_socket_name = ? AND status = ?`,
				expected.InstanceID, expected.Generation, expected.StatusRevision,
				expected.TmuxSession, expected.TmuxSocketName, expected.Status)
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
			if _, err := tx.Exec(`DELETE FROM instance_runtime_binding WHERE instance_id = ?`, expected.InstanceID); err != nil {
				return err
			}
			result, err = tx.Exec(`DELETE FROM instance_incarnation WHERE instance_id = ? AND incarnation = ?`, expected.InstanceID, expectedIncarnation)
			if err != nil {
				return err
			}
			if rows, err := result.RowsAffected(); err != nil || rows != 1 {
				if err != nil {
					return err
				}
				return ErrInstanceParentConflict
			}
			result, err = tx.Exec(`DELETE FROM instances WHERE id = ?`, expected.InstanceID)
			if err != nil {
				return err
			}
			if rows, err := result.RowsAffected(); err != nil || rows != 1 {
				if err != nil {
					return err
				}
				return ErrInstanceParentConflict
			}
			if _, err := tx.Exec(`INSERT OR REPLACE INTO metadata (key, value) VALUES ('last_modified', ?)`,
				fmt.Sprintf("%d", time.Now().UnixNano())); err != nil {
				return err
			}
			return tx.Commit()
		})
	})
}

func instanceSeedParentFromScanner(scanner interface{ Scan(...any) error }) (instanceSeedParent, bool, error) {
	var parent instanceSeedParent
	err := scanner.Scan(
		&parent.id, &parent.title, &parent.projectPath, &parent.groupPath, &parent.order,
		&parent.command, &parent.wrapper, &parent.tool, &parent.status,
		&parent.tmuxSession, &parent.tmuxSocketName, &parent.createdAt, &parent.lastAccessed,
		&parent.parentSessionID, &parent.isConductor, &parent.noTransitionNotify,
		&parent.worktreePath, &parent.worktreeRepo, &parent.worktreeBranch, &parent.account,
		&parent.archivedAt, &parent.toolData, &parent.titleLocked, &parent.autoName,
		&parent.autoNameDescription, &parent.pin, &parent.lastSentAt, &parent.acknowledged,
	)
	if err == sql.ErrNoRows {
		return instanceSeedParent{}, false, nil
	}
	if err != nil {
		return instanceSeedParent{}, false, err
	}
	return parent, true, nil
}

// DeleteInstanceSeedIfUnchanged rolls back only the exact parent/runtime pair
// created by InsertInstanceIfAbsent. Any intervening runtime or parent change
// wins and is preserved.
func (s *StateDB) DeleteInstanceSeedIfUnchanged(seed InstanceSeedToken) error {
	if seed.Runtime.InstanceID == "" || seed.parent.id != seed.Runtime.InstanceID || seed.Incarnation == "" {
		return ErrInstanceParentConflict
	}
	return withBusyRetry(func() error {
		return s.withImmediateTransaction(func(tx *immediateTransaction) error {
			if err := s.requireRuntimeWriterCompatibility(tx, time.Now()); err != nil {
				return err
			}
			var currentIncarnation string
			if err := tx.QueryRow(`SELECT incarnation FROM instance_incarnation WHERE instance_id = ?`, seed.Runtime.InstanceID).Scan(&currentIncarnation); err != nil {
				if err == sql.ErrNoRows {
					return ErrInstanceParentConflict
				}
				return err
			}
			if currentIncarnation != seed.Incarnation {
				return ErrInstanceParentConflict
			}
			currentRuntime, found, err := runtimeStateFromScanner(tx.QueryRow(`
				SELECT instance_id, runtime_generation, status_revision,
				       tmux_session, tmux_socket_name, status, last_started_at
				FROM instance_runtime_state WHERE instance_id = ?`, seed.Runtime.InstanceID))
			if err != nil {
				return err
			}
			if !found {
				return ErrRuntimeGenerationConflict
			}
			if err := sameRuntimeForDestruction(currentRuntime, seed.Runtime); err != nil {
				return err
			}
			if !currentRuntime.LastStartedAt.Equal(seed.Runtime.LastStartedAt) {
				return ErrRuntimeGenerationConflict
			}
			currentBindings, _, err := loadRuntimeBindingSnapshot(tx, seed.Runtime.InstanceID, currentRuntime.Generation)
			if err != nil {
				return err
			}
			if !sameSeedBindingSnapshot(currentBindings, seed.bindings) {
				return ErrRuntimeGenerationConflict
			}

			currentParent, found, err := instanceSeedParentFromScanner(tx.QueryRow(`
				SELECT id, title, project_path, group_path, sort_order,
				       command, wrapper, tool, status, tmux_session, tmux_socket_name,
				       created_at, last_accessed, parent_session_id, is_conductor,
				       no_transition_notify, worktree_path, worktree_repo, worktree_branch,
				       account, archived_at, tool_data, title_locked, auto_name,
				       auto_name_description, pin, last_sent_at, acknowledged
				FROM instances WHERE id = ?`, seed.Runtime.InstanceID))
			if err != nil {
				return err
			}
			if !found || currentParent != seed.parent {
				return ErrInstanceParentConflict
			}
			if _, err := tx.Exec(`DELETE FROM instance_legacy_runtime_adoption WHERE instance_id = ?`, seed.Runtime.InstanceID); err != nil {
				return err
			}

			result, err := tx.Exec(`DELETE FROM instance_runtime_state
				WHERE instance_id = ? AND runtime_generation = ? AND status_revision = ?
				  AND tmux_session = ? AND tmux_socket_name = ? AND status = ?`,
				seed.Runtime.InstanceID, seed.Runtime.Generation, seed.Runtime.StatusRevision,
				seed.Runtime.TmuxSession, seed.Runtime.TmuxSocketName, seed.Runtime.Status)
			if err != nil {
				return err
			}
			if rows, err := result.RowsAffected(); err != nil || rows != 1 {
				if err != nil {
					return err
				}
				return ErrRuntimeGenerationConflict
			}
			if _, err := tx.Exec(`DELETE FROM instance_runtime_binding WHERE instance_id = ?`, seed.Runtime.InstanceID); err != nil {
				return err
			}
			result, err = tx.Exec(`DELETE FROM instance_incarnation WHERE instance_id = ? AND incarnation = ?`, seed.Runtime.InstanceID, seed.Incarnation)
			if err != nil {
				return err
			}
			if rows, err := result.RowsAffected(); err != nil || rows != 1 {
				if err != nil {
					return err
				}
				return ErrInstanceParentConflict
			}
			result, err = tx.Exec(`DELETE FROM instances WHERE id = ?`, seed.Runtime.InstanceID)
			if err != nil {
				return err
			}
			if rows, err := result.RowsAffected(); err != nil || rows != 1 {
				if err != nil {
					return err
				}
				return ErrInstanceParentConflict
			}
			if _, err := tx.Exec(`INSERT OR REPLACE INTO metadata (key, value) VALUES ('last_modified', ?)`,
				fmt.Sprintf("%d", time.Now().UnixNano())); err != nil {
				return err
			}
			return tx.Commit()
		})
	})
}

func sameSeedBindingSnapshot(left, right []RuntimeBinding) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].InstanceID != right[index].InstanceID ||
			left[index].Kind != right[index].Kind ||
			left[index].Generation != right[index].Generation ||
			left[index].Revision != right[index].Revision ||
			left[index].Value != right[index].Value ||
			left[index].DetectedAt.UnixNano() != right[index].DetectedAt.UnixNano() {
			return false
		}
	}
	return true
}
