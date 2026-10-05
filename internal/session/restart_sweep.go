package session

// Issue #666: cross-tmux duplicate-session sweep for the respawn-pane
// restart path.
//
// Background: the fallback restart branch at instance.go already calls
// tmux.KillSessionsWithEnvValue after recreating its tmux session to kill
// any OTHER agentdeck tmux session that holds the same Claude session id
// (issue #596 guard against double `claude --resume` on one conversation).
// The primary respawn-pane branches did not run that sweep, so a user
// who ended up with two agentdeck tmux sessions referencing the same
// tool session id (fork-then-edit path, or manual `session set
// claude-session-id` collision) could restart one while the other's
// claude process kept running — compounding the telegram 409 conflict
// users were hitting on conductor hosts.
//
// The hook var makes the sweep testable without a live tmux server.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// killDuplicateSessionsFn retains the historical test seam. Durable production
// cleanup uses the generation- and binding-aware paths below.
var killDuplicateSessionsFn = tmux.KillSessionsWithEnvValue

var (
	runtimeBindingSweepObservedFn    = func() {}
	runtimeBindingSweepLockFn        = defaultAcquireRuntimeBindingSweepLock
	runtimeCleanupCandidatesFn       = tmux.ListRuntimeCleanupCandidates
	runtimeBindingTargetLockFn       = tryAcquireInstanceSpawnLock
	killRuntimeGenerationCandidateFn = tmux.KillRuntimeGenerationCandidate
	killRuntimeBindingCandidateFn    = tmux.KillRuntimeBindingCandidate
	legacyDuplicateSweepForTests     = false
	runtimeBindingSweepReportFn      = func(instanceID, kind, reason string, err error) {
		attrs := []any{
			slog.String("instance_id", instanceID),
			slog.String("binding_kind", kind),
			slog.String("reason", reason),
		}
		if err != nil {
			attrs = append(attrs, slog.String("error", err.Error()))
		}
		sessionLog.Warn("runtime binding sweep preserved conflicting sessions", attrs...)
	}
)

func defaultAcquireRuntimeBindingSweepLock(kind, value string) (func(), error) {
	digest := sha256.Sum256([]byte(kind + "\x00" + value))
	key := "binding-" + kind + "-" + hex.EncodeToString(digest[:16])
	return defaultAcquireInstanceSpawnLock(key)
}

func runtimeBindingEnvironmentKey(kind string) string {
	switch kind {
	case "claude":
		return "CLAUDE_SESSION_ID"
	case "copilot":
		return "COPILOT_SESSION_ID"
	case "codex":
		return "CODEX_SESSION_ID"
	case "gemini":
		return "GEMINI_SESSION_ID"
	case "opencode":
		return "OPENCODE_SESSION_ID"
	default:
		return ""
	}
}

// sweepDuplicateToolSessions kills agentdeck tmux sessions (other than
// this instance's) that duplicate this instance. It runs up to two sweeps:
//
//  1. Tool-session-id sweep (issue #596/#666 guard). Kills sessions
//     sharing the same CLAUDE_/GEMINI_/OPENCODE_/CODEX_SESSION_ID, so a
//     fork-then-edit collision doesn't leave two `claude --resume`
//     processes fighting over one conversation.
//
//  2. Instance-id sweep (issue #678 guard). Kills sessions sharing the
//     same AGENTDECK_INSTANCE_ID. This covers shell / placeholder
//     sessions that have no tool-level session id — the tool-sweep
//     above is a no-op for them, and without the instance-id sweep
//     every SSH respawn race on Linux+systemd accumulated a new
//     duplicate tmux session (10 observed after a 2-week run with 30
//     shell projects in @bautrey's v0.28.3 fork).
//
// Running both is safe: the second sweep is a no-op if the first already
// killed the stale session. Both exclude only our exact socket-and-session
// identity, so a same-named lower generation on another socket remains a
// cleanup target while this instance is never one.
func (i *Instance) sweepDuplicateToolSessions(observedSockets ...string) {
	selection := i.CaptureRuntimeSelection()
	if selection.State.TmuxSession == "" {
		return
	}
	keepName := selection.State.TmuxSession
	state := selection.State
	db := i.restartPersistenceDB()
	if db != nil {
		durable, found, err := db.ReadRuntimeState(i.ID)
		if err != nil {
			return
		}
		if found {
			if durable.Generation == 0 {
				return
			}
			if durable.Generation != state.Generation ||
				durable.TmuxSession != state.TmuxSession ||
				durable.TmuxSocketName != state.TmuxSocketName {
				return
			}
			// A tool binding broadens cleanup beyond this logical instance. Foreign
			// kills require its durable owner lease; same-instance lower-generation
			// cleanup remains authorized by this exact durable runtime tuple.
			sockets := append(i.runtimeCandidateSocketNames(state), observedSockets...)
			if i.sweepCrossInstanceToolBinding(db, selection, keepName, sockets) {
				return
			}
			candidates, inventoryErr := inventoryRuntimeCleanupCandidates(sockets, "")
			if inventoryErr != nil {
				runtimeBindingSweepReportFn(i.ID, "", "runtime candidate inventory failed", inventoryErr)
				return
			}
			if err := i.killLowerGenerationCandidates(db, selection, keepName, candidates); err != nil {
				runtimeBindingSweepReportFn(i.ID, "", "same-instance generation cleanup failed", err)
			}
			return
		}
	}
	if !legacyDuplicateSweepForTests {
		return
	}

	// Historical unit-test compatibility only. Production callers without a
	// durable runtime never have enough ownership evidence to destroy peers.
	switch {
	case IsClaudeCompatible(i.Tool) && i.ClaudeSessionID != "":
		killDuplicateSessionsFn("CLAUDE_SESSION_ID", i.ClaudeSessionID, keepName)
	case i.Tool == "gemini" && i.GeminiSessionID != "":
		killDuplicateSessionsFn("GEMINI_SESSION_ID", i.GeminiSessionID, keepName)
	case i.Tool == "opencode" && i.OpenCodeSessionID != "":
		killDuplicateSessionsFn("OPENCODE_SESSION_ID", i.OpenCodeSessionID, keepName)
	case i.Tool == "codex" && i.CodexSessionID != "":
		killDuplicateSessionsFn("CODEX_SESSION_ID", i.CodexSessionID, keepName)
	}

	if i.ID != "" {
		killDuplicateSessionsFn("AGENTDECK_INSTANCE_ID", i.ID, keepName)
	}
}

// sweepCrossInstanceToolBinding reports whether this runtime has a non-empty
// tool binding. When it does, the function owns all duplicate cleanup so no
// kill can happen before the final durable ownership check.
func (i *Instance) sweepCrossInstanceToolBinding(db *statedb.StateDB, selection RuntimeSelection, keepName string, sockets []string) bool {
	state := selection.State
	kind := activeRuntimeBindingKind(i)
	envKey := runtimeBindingEnvironmentKey(kind)
	value, _ := i.currentRuntimeBinding(kind)
	if kind == "" || envKey == "" || value == "" {
		return false
	}
	if db == nil || state.Generation == 0 {
		runtimeBindingSweepReportFn(i.ID, kind, "binding cleanup lacks durable runtime authority", nil)
		return true
	}
	i.mu.RLock()
	binding, found := i.RuntimeBindings[kind]
	i.mu.RUnlock()
	if !found || binding.InstanceID != i.ID || binding.Generation != state.Generation || binding.Value != value {
		runtimeBindingSweepReportFn(i.ID, kind, "binding is not authoritatively owned by the current runtime", nil)
		return true
	}

	// Tests use this barrier to commit a competing binding observation after
	// the initial snapshot. The per-candidate owner leases below must then
	// preserve every foreign peer.
	runtimeBindingSweepObservedFn()
	release, err := runtimeBindingSweepLockFn(kind, value)
	if err != nil {
		runtimeBindingSweepReportFn(i.ID, kind, "binding lock unavailable", err)
		return true
	}
	defer release()

	// Inventory may involve several tmux probes. Keep it outside SQLite's
	// BEGIN IMMEDIATE owner lease so it cannot block unrelated state writers.
	allCandidates, err := inventoryRuntimeCleanupCandidates(sockets, envKey)
	if err != nil {
		runtimeBindingSweepReportFn(i.ID, kind, "binding candidate inventory failed", err)
		return true
	}
	candidates := make([]tmux.RuntimeBindingCandidate, 0, len(allCandidates))
	for _, candidate := range allCandidates {
		if candidate.BindingKey == envKey && candidate.BindingValue == value {
			candidates = append(candidates, candidate)
		}
	}
	for _, candidate := range candidates {
		if isKeptRuntimeCandidate(candidate, state, keepName) {
			continue
		}
		if !candidate.InstanceKnown || candidate.InstanceID == "" ||
			!candidate.GenerationKnown || candidate.Generation == 0 ||
			candidate.SessionID == "" || candidate.PaneID == "" || candidate.PanePID <= 0 ||
			candidate.BindingKey != envKey || candidate.BindingValue != value {
			runtimeBindingSweepReportFn(i.ID, kind, "binding candidate identity, generation, or binding is ambiguous", nil)
			return true
		}
	}

	// This instance's transition lock is still held by the caller, so its own
	// lower generations can be swept without broadening authority to a peer.
	if err := i.killLowerGenerationCandidates(db, selection, keepName, allCandidates); err != nil {
		runtimeBindingSweepReportFn(i.ID, kind, "same-instance generation cleanup failed", err)
		return true
	}

	for _, candidate := range candidates {
		if isKeptRuntimeCandidate(candidate, state, keepName) || candidate.InstanceID == i.ID {
			continue
		}
		// Never wait while holding the source and binding locks: opposite
		// direction sweeps would otherwise form a lock cycle.
		releaseTarget, acquired, lockErr := runtimeBindingTargetLockFn(candidate.InstanceID)
		if lockErr != nil {
			runtimeBindingSweepReportFn(i.ID, kind, "target instance lock unavailable", lockErr)
			return true
		}
		if !acquired {
			runtimeBindingSweepReportFn(i.ID, kind, "target instance transition is busy", nil)
			return true
		}

		var killErr error
		target := statedb.RuntimeState{
			InstanceID: candidate.InstanceID, Generation: candidate.Generation,
			TmuxSession: candidate.SessionName, TmuxSocketName: candidate.SocketName,
		}
		cleaned, leaseErr := db.WithRuntimeBindingCleanupLease(state, selection.Incarnation, binding, target, func() {
			// While the source, binding, and target locks remain held,
			// KillRuntimeBindingCandidate first reads the exact session-local
			// cleanup stamp and then conditionally kills the stable tmux identity.
			// Both tmux clients are independently bounded.
			killErr = killRuntimeBindingCandidateFn(candidate)
		})
		releaseTarget()
		if leaseErr != nil {
			runtimeBindingSweepReportFn(i.ID, kind, "durable owner recheck failed", leaseErr)
			return true
		}
		if !cleaned {
			runtimeBindingSweepReportFn(i.ID, kind, "durable binding ownership changed or is ambiguous", nil)
			return true
		}
		if killErr != nil {
			runtimeBindingSweepReportFn(i.ID, kind, "binding candidate changed or cleanup failed", killErr)
			return true
		}
	}
	return true
}

func inventoryRuntimeCleanupCandidates(socketNames []string, envKey string) ([]tmux.RuntimeBindingCandidate, error) {
	seenSockets := make(map[string]bool, len(socketNames))
	seenCandidates := make(map[string]bool)
	var candidates []tmux.RuntimeBindingCandidate
	for _, socketName := range socketNames {
		if seenSockets[socketName] {
			continue
		}
		seenSockets[socketName] = true
		found, err := runtimeCleanupCandidatesFn(socketName, envKey)
		if err != nil {
			return nil, err
		}
		for _, candidate := range found {
			if candidate.SocketName != socketName {
				return nil, fmt.Errorf("runtime cleanup candidate socket %q differs from inventoried socket %q", candidate.SocketName, socketName)
			}
			key := candidate.SocketName + "\x00" + candidate.SessionID + "\x00" + candidate.PaneID
			if seenCandidates[key] {
				continue
			}
			seenCandidates[key] = true
			candidates = append(candidates, candidate)
		}
	}
	return candidates, nil
}

func isKeptRuntimeCandidate(candidate tmux.RuntimeBindingCandidate, state statedb.RuntimeState, keepName string) bool {
	return candidate.SocketName == state.TmuxSocketName && candidate.SessionName == keepName
}

func (i *Instance) killLowerGenerationCandidates(db *statedb.StateDB, selection RuntimeSelection, keepName string, candidates []tmux.RuntimeBindingCandidate) error {
	state := selection.State
	lower := make([]tmux.RuntimeGenerationCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if !candidate.InstanceKnown || candidate.InstanceID != i.ID ||
			isKeptRuntimeCandidate(candidate, state, keepName) ||
			!candidate.GenerationKnown || candidate.Generation >= state.Generation {
			continue
		}
		lower = append(lower, tmux.RuntimeGenerationCandidate{
			SessionName: candidate.SessionName, SessionID: candidate.SessionID,
			SocketName: candidate.SocketName, PaneID: candidate.PaneID, PanePID: candidate.PanePID,
			InstanceID: candidate.InstanceID, Generation: candidate.Generation,
			GenerationKnown: candidate.GenerationKnown,
		})
	}
	if len(lower) == 0 {
		return nil
	}
	if db == nil {
		return statedb.ErrInstanceParentConflict
	}
	var killErr error
	leased, err := db.WithRuntimeOwnerLease(state, selection.Incarnation, func() {
		for _, generation := range lower {
			if killErr = killRuntimeGenerationCandidateFn(generation, false); killErr != nil {
				return
			}
		}
	})
	if err != nil {
		return err
	}
	if !leased {
		return statedb.ErrRuntimeGenerationConflict
	}
	return killErr
}
