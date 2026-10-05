package main

import (
	"database/sql"
	"testing"
)

// alignRuntimeStartTimes copies each raced instance's runtime-owned
// last_started_at onto the serial fixture. The fork keeps LastStartedAt in
// instance_runtime_state (nanoseconds) as well as in the tool_data projection
// alignStorageTimes already aligns, so the same wall-clock start time lives in
// both places. Generation, status revision, tmux identity and status stay
// compared byte for byte.
func alignRuntimeStartTimes(t *testing.T, raced, serial *sql.DB) {
	t.Helper()
	rows, err := raced.Query("SELECT instance_id, last_started_at FROM instance_runtime_state ORDER BY instance_id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var started int64
		if err := rows.Scan(&id, &started); err != nil {
			t.Fatal(err)
		}
		if _, err := serial.Exec("UPDATE instance_runtime_state SET last_started_at=? WHERE instance_id=?", started, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}
