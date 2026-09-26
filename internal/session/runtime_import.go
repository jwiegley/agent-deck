package session

import "fmt"

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
// It carries the instance's generation-zero tuple, the generation the
// import's insert commits. A previous stamp, such as a recovered orphan's for
// its lost instance, is replaced: importing the session transfers it to the
// new instance.
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
