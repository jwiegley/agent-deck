package session

import (
	"fmt"
	"log/slog"

	"github.com/asheshgoplani/agent-deck/internal/logging"
)

// grantPendingImportOwnership applies the ownership grant of an import once
// the imported instance's row has committed. DiscoverExistingTmuxSessions
// only marks the instances it builds (importOwnershipPending) and stamps
// nothing, so a discovery whose inserts never run, or an insert that fails,
// leaves no session stamped for an instance that does not exist. A failed
// insert keeps the mark, so a retried insert that commits still grants.
//
// A session that cannot be stamped is still imported: the row is durable
// either way, and its lifecycle operations refuse with the manual remedy
// instead (ErrRuntimeOwnershipUnproven).
func (s *Storage) grantPendingImportOwnership(i *Instance) {
	if i == nil {
		return
	}
	i.mu.Lock()
	pending := i.importOwnershipPending
	i.importOwnershipPending = false
	i.mu.Unlock()
	if !pending {
		return
	}
	if err := i.grantImportedRuntimeOwnership(); err != nil {
		sessionName := ""
		if sess := i.GetTmuxSession(); sess != nil {
			sessionName = sess.Name
		}
		sessionLog.Warn("import_ownership_stamp_failed",
			slog.String("instance_id", i.ID),
			slog.String("tmux_session", logging.SanitizeValue(sessionName)),
			slog.String("error", err.Error()))
	}
}

// grantImportedRuntimeOwnership makes a session the user imported provably
// this instance's runtime. The explicit import is the ownership grant:
// DiscoverExistingTmuxSessions builds a new instance around a live tmux
// session Agent Deck never started, or one whose instance is gone, and until
// something stamps that session every runtime inventory ignores it, so stop,
// restart and delete refuse it (ErrRuntimeOwnershipUnproven).
//
// The stamp is the one a spawn publishes (stampRuntimeCandidate): the
// environment the reconcile snapshot reads, and the session-local cleanup
// options that the destructive inventory and every conditional kill require.
// It carries the generation-zero tuple the import's insert committed. A
// previous stamp, such as a recovered orphan's for its lost instance, is
// replaced: importing the session transfers it to the new instance.
func (i *Instance) grantImportedRuntimeOwnership() error {
	release, err := acquireInstanceSpawnLock(i.ID)
	if err != nil {
		return err
	}
	defer release()
	i.mu.RLock()
	session := i.tmuxSession
	i.mu.RUnlock()
	if session == nil {
		return fmt.Errorf("imported instance %s has no tmux session", i.ID)
	}
	kind := activeRuntimeBindingKind(i)
	value, _ := i.currentRuntimeBinding(kind)
	return runtimeCandidateStampFn(session, i.runtimeStateSnapshot(), kind, value)
}
