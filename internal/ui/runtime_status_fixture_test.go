package ui

import (
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// publishStatusThroughAuthority commits status the way UpdateStatusObserved
// commits a probe result: a revision CAS on the durable runtime tuple, then the
// in-memory copy. Fixtures without a tmux session use it where production
// reaches the status probe; assigning Instance.Status would bypass authority,
// and snapshot saves drop runtime-owned status.
func publishStatusThroughAuthority(t *testing.T, storage *session.Storage, inst *session.Instance, status session.Status) {
	t.Helper()
	db := storage.GetDB()
	observed, found, err := db.ReadRuntimeState(inst.ID)
	if err != nil || !found {
		t.Fatalf("read runtime for %s: found=%v err=%v", inst.ID, found, err)
	}
	applied, err := db.WriteStatusIfVersion(
		inst.ID, inst.PersistenceIncarnation(), observed.Generation, observed.StatusRevision, string(status))
	if err != nil || !applied {
		t.Fatalf("publish %s for %s: applied=%v err=%v", status, inst.ID, applied, err)
	}
	committed, found, err := db.ReadRuntimeState(inst.ID)
	if err != nil || !found || !inst.ApplyRuntimeState(committed) {
		t.Fatalf("adopt committed runtime for %s: %#v found=%v err=%v", inst.ID, committed, found, err)
	}
}
