package statedb

import (
	"errors"
	"fmt"
	"time"
)

// ErrInstanceNotStored reports that a targeted UPDATE matched no parent row.
var ErrInstanceNotStored = errors.New("instance row not found")

// WriteStamps identify the metadata.last_modified values a targeted write
// replaced and wrote, so a writer can recognise its own change-detection bump.
type WriteStamps struct {
	Before int64
	After  int64
}

// SoleWriterSince requires both no intervening writer and no later writer.
func (w WriteStamps) SoleWriterSince(loadedAt, currentAt int64) bool {
	return w.After != 0 && w.Before <= loadedAt && w.After == currentAt
}

// touchStamp serializes the read and write with every other database writer.
func (s *StateDB) touchStamp() (WriteStamps, error) {
	var stamps WriteStamps
	err := withBusyRetry(func() error {
		return s.withImmediateTransaction(func(tx *immediateTransaction) error {
			var err error
			stamps, err = writeModificationStamp(tx)
			if err != nil {
				return err
			}
			return tx.Commit()
		})
	})
	if err != nil {
		return WriteStamps{}, err
	}
	return stamps, nil
}

// WriteRestartOutcome acknowledges the exact physical runtime already committed
// by transition authority. It never rewrites runtime identity or metadata edits.
func (s *StateDB) WriteRestartOutcome(expected RuntimeState, incarnation string) (WriteStamps, error) {
	return s.writeRuntimeLaunchRecord(expected, incarnation, true, nil)
}

// WriteRestartOutcomeWithLaunch also records the tool and Pi socket decision
// for that same runtime, in the acknowledgement transaction.
func (s *StateDB) WriteRestartOutcomeWithLaunch(expected RuntimeState, incarnation, tool string, exported bool) (WriteStamps, error) {
	return s.writeRuntimeLaunchRecord(expected, incarnation, true, &RuntimeLaunchDecision{Tool: tool, PiSocketExported: exported})
}

// WriteLaunchDecision records a successful launch without changing runtime
// state. The generation and parent incarnation fence same-name respawns and ABA.
func (s *StateDB) WriteLaunchDecision(expected RuntimeState, incarnation, tool string, exported bool) (WriteStamps, error) {
	return s.writeRuntimeLaunchRecord(expected, incarnation, false, &RuntimeLaunchDecision{Tool: tool, PiSocketExported: exported})
}

func (s *StateDB) writeRuntimeLaunchRecord(expected RuntimeState, incarnation string, acknowledge bool, launch *RuntimeLaunchDecision) (WriteStamps, error) {
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
				return fmt.Errorf("record launch for %q: %w", expected.InstanceID, ErrInstanceNotStored)
			}
			if err := requireRuntimeIncarnation(tx, expected.InstanceID, incarnation); err != nil {
				return err
			}
			result, err := tx.Exec(`UPDATE instances
			SET acknowledged = CASE WHEN ? AND (
			        SELECT status FROM instance_runtime_state WHERE instance_id = ?
			    ) = 'running' THEN 0 ELSE acknowledged END,
			    acknowledged_at = CASE WHEN ? AND (
			        SELECT status FROM instance_runtime_state WHERE instance_id = ?
			    ) = 'running' THEN 0 ELSE acknowledged_at END
			WHERE id = ? AND EXISTS (
			    SELECT 1 FROM instance_runtime_state
			    WHERE instance_id = ? AND runtime_generation = ?
			      AND tmux_session = ? AND tmux_socket_name = ?
			      AND last_started_at = ?
			)`,
				acknowledge, expected.InstanceID, acknowledge, expected.InstanceID,
				expected.InstanceID, expected.InstanceID, expected.Generation,
				expected.TmuxSession, expected.TmuxSocketName, runtimeUnix(expected.LastStartedAt))
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
			if launch != nil {
				if _, err := writeRuntimeLaunchDecisionTx(tx, expected, incarnation, *launch); err != nil {
					return err
				}
			}
			stamps, err = writeModificationStamp(tx)
			if err != nil {
				return err
			}
			return tx.Commit()
		})
	})
	if err != nil {
		return WriteStamps{}, err
	}
	return stamps, nil
}

// writeModificationStamp records a writer's exact change within its transaction.
func writeModificationStamp(tx *immediateTransaction) (WriteStamps, error) {
	var beforeText string
	if err := tx.QueryRow(`SELECT COALESCE((
	    SELECT value FROM metadata WHERE key = 'last_modified'
	), '')`).Scan(&beforeText); err != nil {
		return WriteStamps{}, err
	}
	var before int64
	if beforeText != "" {
		if _, err := fmt.Sscan(beforeText, &before); err != nil {
			return WriteStamps{}, err
		}
	}
	after := time.Now().UnixNano()
	if after <= before {
		after = before + 1
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO metadata (key, value)
	VALUES ('last_modified', ?)`, fmt.Sprintf("%d", after)); err != nil {
		return WriteStamps{}, err
	}
	return WriteStamps{Before: before, After: after}, nil
}
