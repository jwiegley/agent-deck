package session

import (
	"encoding/json"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

// A launch record belongs to the exact runtime, not merely a tmux name that a
// same-name respawn or a deleted/reinserted session can reuse.
func writeLaunchDecisionToToolData(td json.RawMessage, state statedb.RuntimeState, incarnation, tool string, exported bool) (json.RawMessage, error) {
	m := map[string]json.RawMessage{}
	if len(td) > 0 {
		if err := json.Unmarshal(td, &m); err != nil {
			return nil, err
		}
	}
	raw, err := json.Marshal(statedb.NewRuntimeLaunchRecord(state, incarnation,
		statedb.RuntimeLaunchDecision{Tool: tool, PiSocketExported: exported}))
	if err != nil {
		return nil, err
	}
	var record map[string]json.RawMessage
	if err := json.Unmarshal(raw, &record); err != nil {
		return nil, err
	}
	for key, value := range record {
		m[key] = value
	}
	return json.Marshal(m)
}

// ReadLaunchDecisionFromToolData rejects stale and legacy name-only records.
func ReadLaunchDecisionFromToolData(td json.RawMessage, state statedb.RuntimeState, incarnation string) (exported bool, tool string) {
	var record statedb.RuntimeLaunchRecord
	if json.Unmarshal(td, &record) != nil || !record.Matches(state, incarnation) {
		return false, ""
	}
	return record.PiSocketExported, record.Tool
}
