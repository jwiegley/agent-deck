package ui

import (
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

// detachGlobalStateDB clears the process-global state database for a fixture
// whose rows were never inserted through Storage. Lifecycle and status
// authority fall back to that database for such rows, so a Home left behind by
// an earlier test (often with its storage already closed) would decide every
// transition instead of the fixture's own in-memory runtime.
func detachGlobalStateDB(t *testing.T) {
	t.Helper()
	previous := statedb.GetGlobal()
	statedb.SetGlobal(nil)
	t.Cleanup(func() { statedb.SetGlobal(previous) })
}
