package ui

import (
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

// hooks-fd-macos.yml overlays this file, together with
// hook_cleanup_async_test.go, onto pinned upstream revisions that predate
// runtime authority. Keep it to helpers that compile there; fixtures that need
// the runtime tuple belong in runtime_status_fixture_test.go.

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
