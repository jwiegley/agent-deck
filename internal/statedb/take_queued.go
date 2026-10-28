package statedb

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// TakeQueued takes a session off its group queue in one transaction: it
// removes the pending queued -m message from tool_data and, when the row is
// queued, moves it to starting, in the same one-row write. It returns the
// message it removed and the status it left the row at. Of concurrent takers exactly one gets the message, so
// a caller hands it off only after this returns and nothing can queue it
// twice.
//
// With claim set (a queue drain), the row must still be queued: taken is
// false, and nothing is written, when another process got there first. Without
// it (a session that was started or restarted another way), any pending
// message is taken whatever the status.
//
// The runtime row is the status authority (instance_runtime_state); the
// instances column only projects it. A row with a runtime moves queued to
// starting through that row, with a status revision of its own, and refuses
// while a writer from another schema epoch is live, like every other runtime
// write. Only a row without a runtime (none exists after migration, but a
// legacy writer can insert one) is moved through the projection alone.
func (s *StateDB) TakeQueued(id, tool string, claim bool) (message, status string, taken bool, err error) {
	var from, tmuxName string
	err = withBusyRetry(func() error {
		message, status, taken, from, tmuxName = "", "", false, "", ""
		return s.withImmediateTransaction(func(tx *immediateTransaction) error {
			var current string
			var raw []byte
			if err := tx.QueryRow(`SELECT status, tool_data, tmux_session FROM instances WHERE id = ?`, id).Scan(&current, &raw, &tmuxName); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return nil
				}
				return err
			}
			var runtimeStatus string
			hasRuntime := true
			if err := tx.QueryRow(`SELECT status FROM instance_runtime_state WHERE instance_id = ?`, id).Scan(&runtimeStatus); err != nil {
				if !errors.Is(err, sql.ErrNoRows) {
					return err
				}
				hasRuntime = false
			} else {
				current = runtimeStatus
			}
			if claim && current != "queued" {
				return nil
			}
			td := map[string]json.RawMessage{}
			if len(raw) > 0 {
				if err := json.Unmarshal(raw, &td); err != nil {
					return fmt.Errorf("reading tool_data of %s: %w", id, err)
				}
			}
			var msg string
			if v, ok := td["queued_message"]; ok {
				_ = json.Unmarshal(v, &msg)
				delete(td, "queued_message")
			}
			if !claim && msg == "" && current != "queued" {
				return nil // nothing to take
			}
			next := current
			if current == "queued" {
				next = "starting"
			}
			if hasRuntime && next != current {
				if err := s.requireRuntimeWriterCompatibility(tx, time.Now()); err != nil {
					return err
				}
				res, err := tx.Exec(`UPDATE instance_runtime_state
					SET status = ?, status_revision = status_revision + 1
					WHERE instance_id = ? AND status = ?`, next, id, current)
				if err != nil {
					return err
				}
				if n, err := res.RowsAffected(); err != nil {
					return err
				} else if n != 1 {
					return nil
				}
			}
			out, err := json.Marshal(td)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE instances SET status = ?, tool = ?, tool_data = ? WHERE id = ?`, next, tool, string(out), id); err != nil {
				return err
			}
			if err := tx.Commit(); err != nil {
				return err
			}
			message, taken, from, status = msg, true, current, next
			return nil
		})
	})
	if err == nil && taken && from == "queued" {
		if observe := loadStatusChangeObserver(); observe != nil {
			observe(StatusChange{DBPath: s.path, ID: id, TmuxSession: tmuxName, From: from, To: "starting", Tool: tool, At: time.Now()})
		}
	}
	return message, status, taken, err
}
