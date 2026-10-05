package session

import (
	"fmt"
	"log/slog"

	"github.com/asheshgoplani/agent-deck/internal/logging"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// grantPendingImportOwnership applies the ownership grant of an import once
// the imported instance's row has committed. DiscoverExistingTmuxSessions
// only marks the instances it builds (importOwnershipPending) and stamps
// nothing, so a discovery whose inserts never run, or an insert that fails,
// leaves no session stamped for an instance that does not exist. A failed
// insert keeps the mark, so a retried insert that commits still grants.
//
// A session the import may not take over, or cannot stamp, is still
// imported: the row is durable either way, and its lifecycle operations
// refuse with the manual remedy instead (ErrRuntimeOwnershipUnproven).
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
	sessionName := ""
	if sess := i.GetTmuxSession(); sess != nil {
		sessionName = sess.Name
	}
	claim, granted, err := i.grantImportedRuntimeOwnership(s)
	switch {
	case err != nil:
		sessionLog.Warn("import_ownership_stamp_failed",
			slog.String("instance_id", i.ID),
			slog.String("tmux_session", logging.SanitizeValue(sessionName)),
			slog.String("error", err.Error()))
	case !granted:
		sessionLog.Info("import_ownership_left_with_owner",
			slog.String("instance_id", i.ID),
			slog.String("tmux_session", logging.SanitizeValue(sessionName)),
			slog.String("owner_instance_id", logging.SanitizeValue(claim.instanceID)),
			slog.String("owner_profile", logging.SanitizeValue(claim.profile)))
	}
}

// importedRuntimeClaim is what a live tmux session's own state says about
// which Agent Deck instance owns it.
type importedRuntimeClaim struct {
	// instanceID is the instance the session's complete session-local
	// cleanup stamp names or, without one, the instance its session-scoped
	// AGENTDECK_INSTANCE_ID names: what every spawn writes first, and what a
	// pre-stamp runtime still carries for legacy adoption. Empty when the
	// session names no instance.
	instanceID string
	// profile is the session-scoped AGENTDECK_PROFILE, which Agent Deck sets
	// on every session it starts (ensureProfileEnv) and on every session an
	// import takes over.
	profile string
}

// readImportedRuntimeClaim reads the claim from the session's own options and
// environment, never from inherited server globals: a tmux server started
// inside an Agent Deck pane inherits that pane's environment wholesale.
func readImportedRuntimeClaim(session *tmux.Session) (importedRuntimeClaim, error) {
	var claim importedRuntimeClaim
	stamped, err := tmux.ListRuntimeCleanupCandidates(session.SocketName, "")
	if err != nil {
		return claim, err
	}
	for _, candidate := range stamped {
		if candidate.SessionName == session.Name {
			claim.instanceID = candidate.InstanceID
		}
	}
	if claim.instanceID == "" {
		if claim.instanceID, err = session.ReadEnvironment("AGENTDECK_INSTANCE_ID"); err != nil {
			return claim, err
		}
	}
	if claim.profile, err = session.ReadEnvironment("AGENTDECK_PROFILE"); err != nil {
		return claim, err
	}
	return claim, nil
}

// importMayTakeOwnership reports whether an import into this profile truly
// holds the grant for a session with this claim. Profiles share one tmux
// server, so every live runtime of another profile is untracked here and
// shows up in this profile's import. A session that names no instance is
// the user's to grant. A session that names one is taken over only when it
// belongs to this profile (its AGENTDECK_PROFILE is this process's profile)
// and that instance no longer exists here, as for a recovered orphan.
// Anything else stays with the instance that owns it, which keeps its stop,
// restart and delete.
func (s *Storage) importMayTakeOwnership(claim importedRuntimeClaim) (bool, error) {
	if claim.instanceID == "" {
		return true, nil
	}
	if claim.profile == "" || claim.profile != sessionProfileEnvValue() {
		return false, nil
	}
	exists, err := s.InstanceExists(claim.instanceID)
	if err != nil {
		return false, err
	}
	return !exists, nil
}

// grantImportedRuntimeOwnership makes a session the user imported provably
// this instance's runtime, when the import may take it over
// (importMayTakeOwnership). The explicit import is the ownership grant:
// DiscoverExistingTmuxSessions builds a new instance around a live tmux
// session Agent Deck never started, or one whose instance is gone, and until
// something stamps that session every runtime inventory ignores it, so stop,
// restart and delete refuse it (ErrRuntimeOwnershipUnproven).
//
// The stamp is the one a spawn publishes (stampRuntimeCandidate): the
// environment the reconcile snapshot reads, and the session-local cleanup
// options that the destructive inventory and every conditional kill require.
// It carries the generation-zero tuple the import's insert committed. A lost
// instance's stamp, such as a recovered orphan's, is replaced: importing the
// session transfers it to the new instance. The session is then marked as
// this profile's, as ensureProfileEnv marks every session Agent Deck starts.
// It reports the claim it read and whether it stamped the session.
func (i *Instance) grantImportedRuntimeOwnership(s *Storage) (importedRuntimeClaim, bool, error) {
	release, err := acquireInstanceSpawnLock(i.ID)
	if err != nil {
		return importedRuntimeClaim{}, false, err
	}
	defer release()
	i.mu.RLock()
	session := i.tmuxSession
	i.mu.RUnlock()
	if session == nil {
		return importedRuntimeClaim{}, false, fmt.Errorf("imported instance %s has no tmux session", i.ID)
	}
	claim, err := readImportedRuntimeClaim(session)
	if err != nil {
		return claim, false, fmt.Errorf("read the imported session's ownership: %w", err)
	}
	take, err := s.importMayTakeOwnership(claim)
	if err != nil || !take {
		return claim, false, err
	}
	kind := activeRuntimeBindingKind(i)
	value, _ := i.currentRuntimeBinding(kind)
	if err := runtimeCandidateStampFn(session, i.runtimeStateSnapshot(), kind, value); err != nil {
		return claim, false, err
	}
	if err := session.SetEnvironment("AGENTDECK_PROFILE", sessionProfileEnvValue()); err != nil {
		sessionLog.Warn("import_profile_mark_failed",
			slog.String("instance_id", i.ID),
			slog.String("tmux_session", logging.SanitizeValue(session.Name)),
			slog.String("error", err.Error()))
	}
	return claim, true, nil
}
