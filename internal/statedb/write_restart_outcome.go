package statedb

import (
	"errors"
	"fmt"
	"time"
)

// ErrInstanceNotStored reports that a targeted UPDATE matched no row, so the
// value the caller asked to record was not recorded. SQLite reports an UPDATE
// that matches nothing as success, which would let a caller announce a durable
// write that never happened.
var ErrInstanceNotStored = errors.New("instance row not found")

// WriteStamps are the metadata.last_modified values a targeted write replaced
// and wrote.
//
// They exist so a process that is both a writer and a reader of this database
// can recognise its own bump. The TUI decides whether to save by comparing the
// database's last_modified against the value it captured when it last loaded;
// a targeted write of its own moves last_modified, so without a way to identify
// that bump the TUI reads its own write as somebody else's change. Before tells
// it whether anything ELSE changed since it loaded, and After identifies the
// value its own write produced. last_modified is a UnixNano stamp, so equality
// is an exact identity test rather than a heuristic.
type WriteStamps struct {
	Before int64
	After  int64
}

// SoleWriterSince reports whether this write was the only change to the
// database since the caller observed loadedAt, and produced exactly currentAt.
//
// Both halves are needed. Before <= loadedAt says nothing landed between the
// caller's load and this write; After == currentAt says nothing landed after
// it. A caller that checked only the second half would treat a database that
// had already moved on as up to date, and then overwrite whatever moved it.
func (w WriteStamps) SoleWriterSince(loadedAt, currentAt int64) bool {
	return w.After != 0 && w.Before <= loadedAt && w.After == currentAt
}

// WriteRestartOutcome acknowledges the exact physical runtime a restart
// already committed and emits the change-detection stamp peers use to reload
// it.
//
// A restart mints a NEW tmux session name — tmux.NewSession appends a fresh
// short id unconditionally — and that name exists only on the in-memory
// Instance until something writes the tmux_session column. Four CLI --restart
// paths never wrote it at all (#1870), so the stored name kept naming the
// session the restart had just killed: the TUI polled a tmux session that no
// longer existed and reported `error` for a process that was running fine,
// while the live tmux session was orphaned because nothing knew its name.
//
// It is a targeted compare-and-update, and specifically NOT a snapshot save.
// The alternative — pushing a whole preloaded instance list back through
// SaveWithGroups — makes a process that loaded its rows before a slow restart
// revert every unrelated change anything else made in between: an archive, a
// rename, a group move. That lost-update shape is the one behind this
// repository's data-loss incidents, and it is a far worse failure than the
// stale tmux name it would be curing.
//
// The authoritative runtime transition writes the name, socket, status, and
// generation together before this method runs. Rewriting those fields here
// would either race that transition or bypass its generation CAS. Instead this
// method proves that the same generation and physical identity are still
// current, updates only the legacy acknowledgement projection, and bumps
// last_modified in the same immediate transaction. Concurrent metadata edits
// (including a tool change) are left untouched. A concurrent status update
// is allowed: it does not change physical identity, and its earlier stamp is
// retained in Before so the TUI cannot mistake the combined history for its
// restart write alone.
//
// A zero-row UPDATE returns ErrInstanceNotStored rather than nil, so a caller
// can tell "recorded" from "silently dropped" (the instance was never saved, or
// another process deleted it mid-restart).
func (s *StateDB) WriteRestartOutcome(expected RuntimeState, expectedIncarnation string) (WriteStamps, error) {
	if expected.InstanceID == "" || expected.TmuxSession == "" {
		return WriteStamps{}, ErrRuntimeGenerationConflict
	}
	var stamps WriteStamps
	err := withBusyRetry(func() error {
		stamps = WriteStamps{}
		return s.withImmediateTransaction(func(tx *immediateTransaction) error {
			if err := s.requireRuntimeWriterCompatibility(tx, time.Now()); err != nil {
				return err
			}
			var parentExists int
			if err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM instances WHERE id = ?)`, expected.InstanceID).Scan(&parentExists); err != nil {
				return err
			}
			if parentExists == 0 {
				return fmt.Errorf("record restart outcome for %q: %w", expected.InstanceID, ErrInstanceNotStored)
			}
			if err := requireRuntimeIncarnation(tx, expected.InstanceID, expectedIncarnation); err != nil {
				return err
			}

			result, err := tx.Exec(`UPDATE instances
			SET acknowledged = CASE WHEN (
			        SELECT status FROM instance_runtime_state WHERE instance_id = ?
			    ) = 'running' THEN 0 ELSE acknowledged END
			WHERE id = ? AND EXISTS (
			    SELECT 1 FROM instance_runtime_state
			    WHERE instance_id = ? AND runtime_generation = ?
			      AND tmux_session = ? AND tmux_socket_name = ?
			      AND last_started_at = ?
			)`,
				expected.InstanceID, expected.InstanceID, expected.InstanceID,
				expected.Generation, expected.TmuxSession, expected.TmuxSocketName,
				runtimeUnix(expected.LastStartedAt))
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

			var beforeText string
			if err := tx.QueryRow(`SELECT COALESCE((
			    SELECT value FROM metadata WHERE key = 'last_modified'
			), '')`).Scan(&beforeText); err != nil {
				return err
			}
			var before int64
			if beforeText != "" {
				if _, err := fmt.Sscan(beforeText, &before); err != nil {
					return err
				}
			}
			after := time.Now().UnixNano()
			if after <= before {
				after = before + 1
			}
			if _, err := tx.Exec(`INSERT OR REPLACE INTO metadata (key, value)
			VALUES ('last_modified', ?)`, fmt.Sprintf("%d", after)); err != nil {
				return err
			}
			if err := tx.Commit(); err != nil {
				return err
			}
			stamps = WriteStamps{Before: before, After: after}
			return nil
		})
	})
	if err != nil {
		return WriteStamps{}, err
	}
	return stamps, nil
}
