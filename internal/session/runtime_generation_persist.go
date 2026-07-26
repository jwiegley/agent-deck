package session

import (
	"encoding/json"
	"time"
)

const toolDataLastStartedAtKey = "last_started_at"

// writeLastStartedAtToToolData keeps the durable runtime generation in the
// extensible tool_data blob, avoiding a schema migration for an additive field.
// A zero timestamp removes the key so legacy rows retain their old shape.
func writeLastStartedAtToToolData(td json.RawMessage, at time.Time) json.RawMessage {
	m := map[string]json.RawMessage{}
	if len(td) > 0 {
		_ = json.Unmarshal(td, &m)
	}
	if at.IsZero() {
		delete(m, toolDataLastStartedAtKey)
	} else {
		raw, _ := json.Marshal(at.UTC())
		m[toolDataLastStartedAtKey] = raw
	}
	out, _ := json.Marshal(m)
	return out
}

// readLastStartedAtFromToolData returns zero for legacy or malformed rows.
func readLastStartedAtFromToolData(td json.RawMessage) time.Time {
	if len(td) == 0 {
		return time.Time{}
	}
	var blob struct {
		LastStartedAt time.Time `json:"last_started_at"`
	}
	if err := json.Unmarshal(td, &blob); err != nil {
		return time.Time{}
	}
	return blob.LastStartedAt
}
