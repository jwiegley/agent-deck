package session

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

// UpdateClaudeSession updates the Claude session ID from tmux environment.
// The capture-resume pattern (used in Start/Fork/Restart) sets CLAUDE_SESSION_ID
// in the tmux environment, making this the single authoritative source.
//
// No file scanning fallback - we rely on the consistent capture-resume pattern.
func (i *Instance) UpdateClaudeSession(excludeIDs map[string]bool) {
	observation := i.CaptureRuntimeBindingObservation("claude")
	i.mu.RLock()
	tool := i.Tool
	i.mu.RUnlock()
	if !IsClaudeCompatible(tool) {
		return
	}

	// Read from tmux environment (set by capture-resume pattern)
	if sessionID := i.GetSessionIDFromTmux(); sessionID != "" {
		rejected := false
		if observation.value != sessionID {
			// Quality gate: don't adopt a zombie ID from tmux env when current has real data
			if observation.value != "" {
				currentHasData := sessionHasConversationData(i, observation.value)
				candidateHasData := sessionHasConversationData(i, sessionID)
				if currentHasData && !candidateHasData {
					sessionLog.Debug("claude_session_tmux_rejected_zombie",
						slog.String("current_id", observation.value),
						slog.String("zombie_id", sessionID),
						slog.String("reason", "tmux_env_has_zombie_id"),
					)
					_ = WriteSessionIDLifecycleEvent(SessionIDLifecycleEvent{
						InstanceID: i.ID, Tool: tool, Action: "reject",
						Source: "tmux_env", OldID: observation.value, Candidate: sessionID,
						Reason: "zombie_id_no_conversation_data",
					})
					i.retractClaudeCandidateLink(sessionID)
					// Don't adopt the zombie; skip the update but still refresh prompt below
					rejected = true
					sessionID = observation.value
				}
			}
			if !rejected {
				action := "bind"
				if observation.value != "" {
					action = "rebind"
				}
				_ = WriteSessionIDLifecycleEvent(SessionIDLifecycleEvent{
					InstanceID: i.ID, Tool: tool, Action: action,
					Source: "tmux_env", OldID: observation.value, NewID: sessionID,
				})
			}
		}
		if !rejected {
			if err := i.PublishRuntimeBindingObservation(observation, sessionID, time.Now()); err != nil {
				sessionLog.Debug("claude_tmux_binding_rejected",
					slog.String("session_id", sessionID), slog.String("error", err.Error()))
			} else {
				// #1815: own pane env is a weak vouch, applied only after the
				// durable owner accepted the observation.
				i.noteClaudeSessionIDFromOwnPane()
			}
		}
	}

	// Update latest prompt from JSONL file (tail-read with size caching)
	if current, _ := i.currentRuntimeBinding("claude"); current != "" {
		jsonlPath := i.GetJSONLPath()
		if jsonlPath != "" {
			if prompt := i.readJSONLTail(jsonlPath); prompt != "" {
				i.LatestPrompt = prompt
			}
		}
	}
}

// syncClaudeSessionFromDisk is a legacy shim kept for compatibility.
// Disk scan is intentionally NOT authoritative for session identity.
// Session ID binding must come from tmux env and/or hook session anchor.
func (i *Instance) syncClaudeSessionFromDisk() {
	if !IsClaudeCompatible(i.Tool) {
		return
	}
	sessionLog.Debug("claude_session_disk_scan_disabled",
		slog.String("instance", i.ID),
		slog.String("reason", "disk_scan_not_authoritative"),
	)
	_ = WriteSessionIDLifecycleEvent(SessionIDLifecycleEvent{
		InstanceID: i.ID, Tool: i.Tool, Action: "scan_disabled",
		Source: "disk_scan", Reason: "disk_scan_not_authoritative",
	})
}

// UpdateHookStatus updates the instance's hook-based status fields.
// Called by StatusFileWatcher when a hook status file changes.
func (i *Instance) UpdateHookStatus(status *HookStatus) {
	if status == nil {
		return
	}

	observation := i.captureActiveRuntimeBindingObservation()
	i.mu.Lock()
	// Issue #1846: whatever hookLastUpdate ends up COMMITTED when this call
	// returns is agent-activity evidence — fold it into the durable record.
	// The reject branches below restore the pre-event value first, so a
	// foreign ephemeral's timestamp is never credited to this instance.
	// Defer order (LIFO): the fold runs under the lock, then the lock is
	// released, then the throttled SQLite flush runs OUTSIDE it — the DB
	// write can stall on SQLITE_BUSY and must not hold up i.mu readers.
	defer i.persistLastActivity(false)
	defer i.mu.Unlock()
	defer func() { i.noteAgentActivityLocked(i.hookLastUpdate) }()

	// Snapshot the prior hook-status fields so a candidate that fails the
	// ownership check below can RESTORE them rather than leaving its status
	// applied. This closes the `claude -p` env-pollution flip: a foreign
	// ephemeral session (a `claude -p` child that inherited our
	// AGENTDECK_INSTANCE_ID and fired hooks under our id) has no conversation
	// data, so the bind is rejected — but previously the running/waiting status
	// it carried had already been written here and stuck, flipping this
	// instance's status. See the candidate_has_no_conversation_data branch.
	prevHookStatus, prevHookEvent, prevHookLastUpdate := i.hookStatus, i.hookEvent, i.hookLastUpdate
	prevStartedGen, prevCompletedGen := i.codexStartedGeneration, i.codexCompletedGeneration
	prevStartedSID, prevCompletedSID := i.codexStartedSessionID, i.codexCompletedSessionID
	prevInvalidatingGen := i.codexInvalidatingGeneration
	prevHookFingerprint := i.hookFingerprint
	// rejected tracks whether this event was rolled back by restoreHook below.
	// A rejected candidate must never be read as evidence that the agent is
	// interactive — see the disarm condition further down.
	rejected := false
	restoreHook := func() {
		rejected = true
		i.hookStatus, i.hookEvent, i.hookLastUpdate = prevHookStatus, prevHookEvent, prevHookLastUpdate
		i.codexStartedGeneration, i.codexCompletedGeneration = prevStartedGen, prevCompletedGen
		i.codexStartedSessionID, i.codexCompletedSessionID = prevStartedSID, prevCompletedSID
		i.codexInvalidatingGeneration = prevInvalidatingGen
		i.hookFingerprint = prevHookFingerprint
	}

	// Detect whether this is genuinely new data. Parsed hook files use their
	// exact content fingerprint because their timestamps have only whole-second
	// granularity; synthetic callers without a fingerprint retain the timestamp
	// fallback.
	// Only reset acknowledgment on new events — not on re-application of the same
	// stale hook file, which would undo the user's intentional acknowledge.
	isNewEvent := status.UpdatedAt.After(i.hookLastUpdate)
	if status.Fingerprint.valid() {
		isNewEvent = status.Fingerprint != i.hookFingerprint
	}

	i.hookStatus = status.Status
	i.hookEvent = status.Event
	i.hookLastUpdate = status.UpdatedAt
	i.setCodexGenerationEvidence(status)
	i.hookFingerprint = status.Fingerprint

	// Permission-type events are always attention-needed, even if the user
	// previously acknowledged this session. A mid-task permission block is new
	// activity that the user must respond to — unlike Stop (task complete) which
	// can stay grey if already seen.
	// Handles both PermissionRequest events and Notification/permission_prompt.
	if isNewEvent && status.Status == "waiting" && i.tmuxSession != nil {
		if status.Event == "PermissionRequest" || status.Event == "Notification" {
			i.tmuxSession.ResetAcknowledged()
		}
	}

	// A live lifecycle hook proves the agent became interactive. The hook fast
	// path in UpdateStatus skips tmux.GetStatus, which is otherwise the only
	// place the startup clock is cleared, so end the startup phase here or the
	// first hook-quiet poll expires a healthy pane (#2361). Deferred so it only
	// counts hooks that survive the ownership checks below: a rejected foreign
	// ephemeral (rejected set true by restoreHook, above) must not disarm the
	// watchdog. Registered after the Unlock defer, so it runs with i.mu still
	// held.
	//
	// Not gated on isNewEvent: the COLD LOAD branch in updateStatus reads the
	// SessionStart hook file straight off disk and stamps i.hookLastUpdate
	// from it, outside this function, before the watcher ever calls
	// UpdateHookStatus with that same event. When the watcher's feed arrives,
	// its UpdatedAt equals what cold-load already recorded, so isNewEvent
	// would be false and the disarm would never fire — the pane then gets
	// killed by the startup watchdog despite a real hook on file. Re-feeding
	// an already-accepted hook is harmless here: MarkInteractiveAt is
	// idempotent (clearing an already-zero startupAt is a no-op) and
	// generation-guarded (a timestamp older than the current pane's startupAt
	// is ignored), and this is the only path that recovers from that
	// cold-load race.
	//
	// agentdeck_spawn_seed (item #2) is excluded: it is a synthetic seed
	// agent-deck itself writes when handing a pane to Hermes, not evidence
	// that an agent became interactive, so it must not disarm the watchdog.
	defer func() {
		if i.tmuxSession != nil && !rejected && !isTerminalHookEvent(status.Event) &&
			(status.Status == "running" || status.Status == "waiting") &&
			status.Event != "agentdeck_spawn_seed" {
			i.tmuxSession.MarkInteractiveAt(status.UpdatedAt)
		}
	}()

	// Issue #1349 defense-in-depth #1: never bind a session id from a terminal
	// hook event (e.g. SessionEnd). The status/event/ack bookkeeping above still
	// applies, but a terminal payload's session_id is stale by definition and
	// must not become a bind source — that is exactly what re-binds a
	// stopped/removed session every poll cycle and collides session ids.
	//
	// Accepted tradeoff: if the daemon's first-ever observation of a session is
	// a SessionEnd (e.g. it was down during the SessionStart/UserPromptSubmit
	// edges and only the latest event survives in the hook file), the bind is
	// skipped and the prior ClaudeSessionID stays. That is the correct call —
	// the session is already gone, so binding its (possibly reused) id onto a
	// now-dead instance is the corruption we are preventing, not a feature.
	if isTerminalHookEvent(status.Event) {
		return
	}

	// Resolve session ID from hook payload first, then sidecar anchor.
	sessionID := strings.TrimSpace(status.SessionID)
	hookSource := "hook_payload"
	if sessionID == "" {
		sessionID = ReadHookSessionAnchor(i.ID)
		hookSource = "hook_anchor"
	}
	if sessionID == "" {
		return
	}

	switch {
	case i.Tool == "hermes":
		i.bindHermesSessionFromHook(sessionID, hookSource, status.Event, status.Platform, status.Cwd, status.UpdatedAt)
	case IsClaudeCompatible(i.Tool):
		if sessionID == i.ClaudeSessionID {
			// Review finding on #1830: same rationale as the sibling equality
			// branch above — a live hook payload reporting this instance's
			// own recorded id is the strongest available vouch, and must
			// clear any lingering disk-scan taint on it rather than being
			// treated as a no-op.
			if i.publishObservedRuntimeBindingLocked(observation, sessionID, status.Fingerprint) {
				i.markClaudeSessionIDVerified()
				i.confirmHookSessionLink("claude", sessionID, hookSource)
			}
			return
		}
		// Issue #1729 guard: a candidate whose hook-reported cwd is provably
		// outside every path this instance owns is a foreign ephemeral — the
		// classic case is a headless `claude -p --no-session-persistence`
		// worker (spawned by a plugin with cwd=$TMPDIR) that inherited our
		// AGENTDECK_INSTANCE_ID from the tmux pane environment. Such
		// candidates must never influence instance state: no bind (including
		// cold start, which previously accepted ANY first candidate), no
		// rebind, and no status flip (restore the pre-event fields). An empty
		// cwd (legacy hook files, agents that send none) is not evidence and
		// never blocks.
		if i.hookCwdIsForeign(status.Cwd) {
			restoreHook()
			_ = WriteSessionIDLifecycleEvent(SessionIDLifecycleEvent{
				InstanceID: i.ID, Tool: i.Tool, Action: "reject",
				Source: hookSource, OldID: i.ClaudeSessionID, Candidate: sessionID,
				HookEvent: status.Event, Reason: "candidate_cwd_outside_instance_paths",
			})
			i.retractClaudeCandidateLink(sessionID)
			return
		}
		// Cold start — no session bound yet. Accept the first candidate
		// unconditionally; there is nothing to protect.
		if i.ClaudeSessionID == "" {
			i.bindClaudeSessionFromHook(observation, sessionID, hookSource, status.Event, "bind", status.Fingerprint)
			return
		}
		// v1.7.7 guard: candidate must have any conversation data at all.
		if !sessionHasConversationData(i, sessionID) {
			// A different session id with NO conversation data on an established
			// instance is a foreign ephemeral (a `claude -p` child that inherited
			// our AGENTDECK_INSTANCE_ID) — it doesn't own this instance, so its
			// status must not stick either. Restore the pre-event status so the
			// foreign hook is a no-op, not a flip. (A real /clear or fork carries
			// conversation data and never reaches this branch.)
			restoreHook()
			_ = WriteSessionIDLifecycleEvent(SessionIDLifecycleEvent{
				InstanceID: i.ID, Tool: i.Tool, Action: "reject",
				Source: hookSource, OldID: i.ClaudeSessionID, Candidate: sessionID,
				HookEvent: status.Event, Reason: "candidate_has_no_conversation_data",
			})
			i.retractClaudeCandidateLink(sessionID)
			return
		}
		// v1.7.23 guard (issue #661): when BOTH current and candidate have
		// data, the candidate must have strictly MORE content to win. This
		// stops the UserPromptSubmit flap where a fresh 1-record jsonl
		// overwrites a rich hundreds-of-KB historic jsonl on every restart.
		// Byte size is a robust proxy for "how much history this session
		// holds" — immune to record-count ties and faster than re-scanning
		// the file.
		//
		// Issue #856: but a strict size-only rule rejects user-initiated
		// new sessions (e.g. /clear) indefinitely, since they're smaller by
		// definition. Mtime gap is the discriminator: in a flap the user
		// keeps typing into the rich session so its mtime stays fresh; in
		// /clear the user abandons the old session, so its mtime stales
		// while the new jsonl's mtime advances. If the candidate's jsonl
		// is significantly newer than the current's (clearRebindMtimeGrace),
		// treat it as a user-initiated new session and rebind regardless
		// of size.
		if sessionHasConversationData(i, i.ClaudeSessionID) {
			currentSize := sessionConversationByteSize(i, i.ClaudeSessionID)
			candidateSize := sessionConversationByteSize(i, sessionID)
			if candidateSize <= currentSize {
				currentMtime := sessionConversationMtime(i, i.ClaudeSessionID)
				candidateMtime := sessionConversationMtime(i, sessionID)
				clearRebind := !currentMtime.IsZero() && !candidateMtime.IsZero() &&
					candidateMtime.Sub(currentMtime) >= clearRebindMtimeGrace
				if !clearRebind {
					// This rejection does not call restoreHook, so the event's
					// status/event/hookLastUpdate stand — and so does the
					// startup-watchdog disarm registered above: this is still
					// a live hook from this instance's own session, just not
					// one that wins the rebind.
					_ = WriteSessionIDLifecycleEvent(SessionIDLifecycleEvent{
						InstanceID: i.ID, Tool: i.Tool, Action: "reject",
						Source: hookSource, OldID: i.ClaudeSessionID, Candidate: sessionID,
						HookEvent: status.Event, Reason: "candidate_has_less_conversation_data",
					})
					i.retractClaudeCandidateLink(sessionID)
					return
				}
			}
		}
		i.bindClaudeSessionFromHook(observation, sessionID, hookSource, status.Event, "rebind", status.Fingerprint)
	case IsCodexCompatible(i.Tool):
		if sessionID == i.CodexSessionID {
			// Like Claude's equality branch: a live hook vouching for the
			// bound id also records its recall link, which an id bound by
			// any other path (or a link write that failed) still lacks.
			if i.publishObservedRuntimeBindingLocked(observation, sessionID, status.Fingerprint) {
				i.confirmHookSessionLink("codex", sessionID, hookSource)
			}
			return
		}
		// Quality gate (incident 2026-07-15): codex subagent threads fire
		// the same agent-turn-complete notify as the main thread, and a
		// completing subagent's payload id would otherwise usurp the
		// binding. Restarting then resumes a finalized child thread, which
		// refuses turn/start and error-loops the session. See
		// codex_subagent_gate.go.
		// Both rejections below also restore the pre-event hook fields: a
		// rejected thread's turn-end is not this pane's turn-finished edge,
		// and its "waiting" must not release `session send --defer-if-busy`
		// into a main turn that is still running.
		if i.shouldRejectCodexSubagentRebind(sessionID) {
			restoreHook()
			i.clearRejectedCodexHookLocked(sessionID)
			_ = WriteSessionIDLifecycleEvent(SessionIDLifecycleEvent{
				InstanceID: i.ID, Tool: i.Tool, Action: "reject",
				Source: hookSource, OldID: i.CodexSessionID, Candidate: sessionID,
				HookEvent: status.Event, Reason: "candidate_is_subagent_thread",
			})
			sessionLog.Debug("codex_session_rebind_rejected_subagent",
				slog.String("old_id", i.CodexSessionID),
				slog.String("candidate", sessionID),
				slog.String("event", status.Event),
			)
			return
		}
		if i.shouldRejectCodexUnbackedTurnEnd(sessionID, status.Event) {
			restoreHook()
			_ = WriteSessionIDLifecycleEvent(SessionIDLifecycleEvent{
				InstanceID: i.ID, Tool: i.Tool, Action: "reject",
				Source: hookSource, OldID: i.CodexSessionID, Candidate: sessionID,
				HookEvent: status.Event, Reason: "candidate_turn_ended_without_rollout",
			})
			return
		}
		i.bindCodexSessionFromHook(observation, sessionID, hookSource, status.Event, status.Fingerprint)
	case i.Tool == "gemini":
		if sessionID == i.GeminiSessionID {
			if i.publishObservedRuntimeBindingLocked(observation, sessionID, status.Fingerprint) {
				i.confirmHookSessionLink("gemini", sessionID, hookSource)
			}
			return
		}
		// Quality gate: only accept when candidate session appears valid on disk,
		// OR when current session is empty (first detection/bootstrap).
		if i.GeminiSessionID == "" || geminiSessionHasConversationData(sessionID, i.ProjectPath) {
			i.bindGeminiSessionFromHook(observation, sessionID, hookSource, status.Event, status.Fingerprint)
		}
	}
}

// publishObservedRuntimeBindingLocked publishes a hook-observed tool session
// id through the runtime binding CAS, which commits the durable binding and
// its tool_data projection together and only then adopts the id in memory,
// so DB-direct consumers and peer processes see it at once (PERSIST-12,
// #1138/#1139). It reports whether the id is now this instance's binding.
// The recall link is not part of that write: callers record it with
// confirmHookSessionLink after a successful publish.
func (i *Instance) publishObservedRuntimeBindingLocked(observation RuntimeBindingObservation, sessionID string, fingerprint HookStatusFingerprint) bool {
	// UpdateHookStatus owns i.mu across its quality gates. Publication uses the
	// global lock order (spawn lock, then i.mu), so leave the instance critical
	// section before entering the single CAS authority.
	i.mu.Unlock()
	err := i.publishHookRuntimeBindingObservation(observation, sessionID, fingerprint, true)
	i.mu.Lock()
	if err != nil {
		sessionLog.Warn("runtime_binding_persist_failed",
			slog.String("instance_id", i.ID), slog.String("kind", observation.kind),
			slog.String("new_id", sessionID), slog.String("error", err.Error()))
		return false
	}
	return true
}

// bindCodexSessionFromHook is the Codex counterpart of
// bindClaudeSessionFromHook. It publishes the new binding durably first
// (publishObservedRuntimeBindingLocked), because none of the UpdateHookStatus
// callers (TUI tick, web refresh, CLI status refresh) save after a
// hook-triggered rebind (#1139); only a published id gets the bookkeeping
// that follows: debug log, ownership record, tmux env propagation and the
// authoritative recall link that upstream's WriteCodexSessionBinding wrote
// with the binding.
func (i *Instance) bindCodexSessionFromHook(observation RuntimeBindingObservation, sessionID, hookSource, hookEvent string, fingerprint HookStatusFingerprint) {
	oldID := i.CodexSessionID
	if !i.publishObservedRuntimeBindingLocked(observation, sessionID, fingerprint) {
		return
	}
	sessionLog.Debug("codex_session_update_from_hook",
		slog.String("old_id", oldID),
		slog.String("new_id", sessionID),
		slog.String("event", hookEvent),
	)
	i.recordCodexOwnershipLocked(sessionID)
	i.hookSessionID = sessionID

	if i.tmuxSession != nil && i.tmuxSession.Exists() {
		_ = i.tmuxSession.SetEnvironment("CODEX_SESSION_ID", sessionID)
	}
	i.confirmHookSessionLink("codex", sessionID, hookSource)
}

// bindGeminiSessionFromHook is the Gemini counterpart of
// bindCodexSessionFromHook: publish first, then the bookkeeping and the
// recall link upstream's WriteGeminiSessionBinding wrote. The quality gate
// (GeminiSessionID == "" || geminiSessionHasConversationData(...)) is
// enforced by the caller in UpdateHookStatus before this function is
// invoked, mirroring the invariant the inlined pre-#1139 code preserved.
func (i *Instance) bindGeminiSessionFromHook(observation RuntimeBindingObservation, sessionID, hookSource, hookEvent string, fingerprint HookStatusFingerprint) {
	oldID := i.GeminiSessionID
	if !i.publishObservedRuntimeBindingLocked(observation, sessionID, fingerprint) {
		return
	}
	sessionLog.Debug("gemini_session_update_from_hook",
		slog.String("old_id", oldID),
		slog.String("new_id", sessionID),
		slog.String("event", hookEvent),
	)
	i.hookSessionID = sessionID

	if i.tmuxSession != nil && i.tmuxSession.Exists() {
		_ = i.tmuxSession.SetEnvironment("GEMINI_SESSION_ID", sessionID)
	}
	i.confirmHookSessionLink("gemini", sessionID, hookSource)
}

// GetHookStatus returns the current hook-based status and its freshness.
// Freshness window is tool-specific.
// StatusEvidence is the externally fed evidence UpdateStatus turns into a
// rendered status: the latest hook sample and the latest OpenCode SSE sample.
// It changes only when a feed lands (UpdateHookStatus, UpdateOpenCodeSSEStatus)
// and reverts when a feed is rejected, so a poller can compare two readings to
// learn whether this instance has unapplied evidence.
type StatusEvidence struct {
	HookStatus string
	HookEvent  string
	HookAt     time.Time
	SSEStatus  string
	SSEAt      time.Time
}

// StatusEvidence returns the current evidence snapshot (read lock).
func (i *Instance) StatusEvidence() StatusEvidence {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return StatusEvidence{
		HookStatus: i.hookStatus,
		HookEvent:  i.hookEvent,
		HookAt:     i.hookLastUpdate,
		SSEStatus:  i.sseStatus,
		SSEAt:      i.sseLastUpdate,
	}
}

func (i *Instance) GetHookStatus() (string, bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()

	if i.hookStatus == "" {
		return "", false
	}
	fresh := time.Since(i.hookLastUpdate) < hookFastPathFreshnessForTool(i.Tool, i.hookStatus)
	return i.hookStatus, fresh
}

// LastHookActivityTime returns the on-disk hook status file's UpdatedAt
// timestamp for this instance, and true when a hook sample has been
// recorded. Unlike LastObservedActivity's process-local tmux tracker, this
// is populated even by a cold, short-lived CLI process: UpdateStatus's
// "COLD LOAD" branch reads the sidecar hook file straight off disk before
// this can be called. That makes it durable cross-process evidence of real
// activity, which status --stale needs (issue #1704 dual-review finding):
// a fresh CLI invocation never gets to observe LastObservedActivity, so
// without this a session that just finished real work and has no
// LastAccessedAt (never attached in the TUI) would misreport its
// last-activity age against CreatedAt instead of the moment it actually
// went idle/waiting.
func (i *Instance) LastHookActivityTime() (time.Time, bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()

	if i.hookStatus == "" || i.hookLastUpdate.IsZero() {
		return time.Time{}, false
	}
	return i.hookLastUpdate, true
}

// ClearHookStatus resets the hook-based status and removes the persisted hook
// record, forcing the next UpdateStatus() to fall through to polling. Used
// when the user manually overrides status (e.g., pressing 'u' to unacknowledge
// after an Escape interrupt where the Stop hook didn't fire).
func (i *Instance) ClearHookStatus() {
	i.mu.Lock()
	// #1846: the file removed below is the only durable record of this
	// session's last real activity, and the in-memory copy is zeroed right
	// after. Fold it into the durable last-activity record first.
	i.noteAgentActivityLocked(i.hookLastUpdate)
	i.hookStatus = ""
	i.hookLastUpdate = time.Time{}
	i.hookFingerprint = HookStatusFingerprint{}
	i.validatedHookBindings = nil
	i.mu.Unlock()
	// Flush BEFORE removing the file below, bypassing the write throttle —
	// this evidence has no other way to survive. Outside i.mu: the SQLite
	// write can stall on SQLITE_BUSY (see persistLastActivity).
	i.persistLastActivity(true)
	if i.Tool == "hermes" {
		i.clearHermesHookStatuses()
		return
	}

	// Remove the persisted status file. Sandbox sessions bridge a PER-INSTANCE
	// scoped subdir (…/hooks/sandbox/<id>/<id>.json) from the container, and the
	// watcher attributes that file to this instance by its OWNING SUBDIR, so the
	// scoped file is the one to clear. Non-sandbox sessions write the flat
	// …/hooks/<id>.json. We remove only the FILE here (not the subdir): this can
	// fire mid-session (attach-return / unacknowledge) while the container still
	// has the subdir bind-mounted, and unlinking the mount source would orphan
	// the live bridge. The subdir + its fsnotify watch are torn down at session
	// end (see killInternal).
	hookPath := filepath.Join(GetHooksDir(), i.ID+".json")
	if i.IsSandboxed() {
		hookPath = filepath.Join(GetHooksDir(), "sandbox", i.ID, i.ID+".json")
	}
	if err := os.Remove(hookPath); err != nil && !os.IsNotExist(err) {
		sessionLog.Debug("clear_hook_status_file_failed",
			slog.String("instance", i.ID),
			slog.String("path", hookPath),
			slog.String("error", err.Error()),
		)
	}
}

// SetGeminiYoloMode sets the YOLO mode for Gemini and syncs it to the tmux environment.
// This ensures the background status worker sees the correct state during restarts.
func (i *Instance) SetGeminiYoloMode(enabled bool) {
	if i.Tool != "gemini" {
		return
	}

	i.GeminiYoloMode = &enabled

	// Sync to tmux environment immediately if session exists
	// This ensures background detection (UpdateGeminiSession) sees the new value
	if i.tmuxSession != nil && i.tmuxSession.Exists() {
		val := "false"
		if enabled {
			val = "true"
		}
		_ = i.tmuxSession.SetEnvironment("GEMINI_YOLO_MODE", val)
	}
}

// UpdateGeminiSession updates the Gemini session ID, YOLO mode, analytics, and latest prompt.
// Delegates to focused helpers for each concern.
func (i *Instance) UpdateGeminiSession(excludeIDs map[string]bool) {
	i.mu.RLock()
	isGemini := i.Tool == "gemini"
	i.mu.RUnlock()
	if !isGemini {
		return
	}
	i.syncGeminiSessionFromTmux()
	i.syncGeminiSessionFromDisk()
	i.updateGeminiAnalytics()
	i.updateGeminiLatestPrompt()
}

// UpdateOpenCodeSession refreshes the OpenCode session ID from OpenCode state
// without stealing a different tab's session from the same project.
func (i *Instance) UpdateOpenCodeSession() {
	i.updateOpenCodeSession(false)
}

// updateOpenCodeSession self-manages i.mu: state reads/writes happen under the
// lock but queryOpenCodeSession I/O runs outside it, so a slow OpenCode server
// or compatibility CLI cannot starve render-path RLocks on this instance.
//
// Contract: callers MUST NOT hold i.mu when invoking this function.
func (i *Instance) updateOpenCodeSession(force bool) {
	i.mu.Lock()
	if i.Tool != "opencode" {
		i.mu.Unlock()
		return
	}
	now := time.Now()
	if !force && !i.lastOpenCodeScanAt.IsZero() && now.Sub(i.lastOpenCodeScanAt) < opencodeRotationScanInterval {
		i.mu.Unlock()
		return
	}
	i.lastOpenCodeScanAt = now
	observation := i.runtimeBindingObservationLocked("opencode")
	i.mu.Unlock()

	candidate := i.queryOpenCodeSession()
	i.publishOpenCodeSessionCandidate(observation, candidate, time.Now())
}

func (i *Instance) applyOpenCodeSessionCandidate(candidate string) bool {
	if candidate == "" {
		return false
	}
	observation := i.CaptureRuntimeBindingObservation("opencode")

	if candidate == observation.value {
		i.publishOpenCodeSessionCandidate(observation, candidate, time.Now())
		return false
	}

	if observation.value != "" {
		lastActivity := i.GetLastActivityTime()
		if !lastActivity.IsZero() && time.Since(lastActivity) <= opencodeRotationActivityWindow {
			sessionLog.Debug(
				"opencode_session_rebind_recent_activity",
				slog.String("old_id", observation.value),
				slog.String("new_id", candidate),
				slog.Time("last_activity", lastActivity),
			)
		}
	}

	sessionLog.Debug(
		"opencode_session_rebind",
		slog.String("old_id", observation.value),
		slog.String("new_id", candidate),
	)

	return i.publishOpenCodeSessionCandidate(observation, candidate, time.Now())
}

// queryGeminiSessionFromTmux reads candidates without mutating binding state.
func (i *Instance) queryGeminiSessionFromTmux() (string, *bool) {
	i.mu.RLock()
	tmuxSession := i.tmuxSession
	i.mu.RUnlock()
	if tmuxSession == nil {
		return "", nil
	}
	sessionID, _ := tmuxSession.GetEnvironment("GEMINI_SESSION_ID")
	var yolo *bool
	if yoloEnv, err := tmuxSession.GetEnvironment("GEMINI_YOLO_MODE"); err == nil && yoloEnv != "" {
		enabled := yoloEnv == "true"
		yolo = &enabled
	}
	return sessionID, yolo
}

// syncGeminiSessionFromTmux publishes the tmux observation after the query.
func (i *Instance) syncGeminiSessionFromTmux() {
	observation := i.CaptureRuntimeBindingObservation("gemini")
	i.mu.RLock()
	isGemini := i.Tool == "gemini"
	i.mu.RUnlock()
	if !isGemini {
		return
	}
	sessionID, yolo := i.queryGeminiSessionFromTmux()
	if sessionID != "" {
		if err := i.PublishRuntimeBindingObservation(observation, sessionID, time.Now()); err != nil {
			sessionLog.Debug("gemini_tmux_binding_rejected",
				slog.String("session_id", sessionID), slog.String("error", err.Error()))
		}
	}
	if yolo != nil {
		i.mu.Lock()
		i.GeminiYoloMode = yolo
		i.mu.Unlock()
	}
}

func (i *Instance) queryGeminiSessionFromDisk() string {
	i.mu.RLock()
	projectPath := i.ProjectPath
	i.mu.RUnlock()
	sessions, err := ListGeminiSessions(projectPath)
	if err != nil || len(sessions) == 0 {
		return ""
	}
	return sessions[0].SessionID
}

// syncGeminiSessionFromDisk scans the filesystem for the most recent session
// and publishes it through the same CAS authority as live observations.
func (i *Instance) syncGeminiSessionFromDisk() {
	observation := i.CaptureRuntimeBindingObservation("gemini")
	i.mu.RLock()
	isGemini := i.Tool == "gemini"
	i.mu.RUnlock()
	if !isGemini {
		return
	}
	sessionID := i.queryGeminiSessionFromDisk()
	if sessionID == "" {
		return
	}
	if sessionID != observation.value {
		sessionLog.Debug(
			"gemini_session_update",
			slog.String("old_id", observation.value),
			slog.String("new_id", sessionID),
		)
	}
	if err := i.PublishRuntimeBindingObservation(observation, sessionID, time.Now()); err != nil {
		sessionLog.Debug("gemini_disk_binding_rejected",
			slog.String("session_id", sessionID), slog.String("error", err.Error()))
		return
	}
	i.mu.RLock()
	tmuxSession := i.tmuxSession
	i.mu.RUnlock()
	if tmuxSession != nil && tmuxSession.Exists() {
		_ = tmuxSession.SetEnvironment("GEMINI_SESSION_ID", sessionID)
	}
}

// updateGeminiSessionForTransition is used only while restart holds physical
// transition authority. Its assignments are captured by that transition's
// binding plan and must not recursively acquire the spawn lock.
func (i *Instance) updateGeminiSessionForTransition() {
	if sessionID, yolo := i.queryGeminiSessionFromTmux(); sessionID != "" {
		i.GeminiSessionID, i.GeminiDetectedAt = sessionID, time.Now()
		if yolo != nil {
			i.GeminiYoloMode = yolo
		}
	}
	if sessionID := i.queryGeminiSessionFromDisk(); sessionID != "" {
		i.GeminiSessionID, i.GeminiDetectedAt = sessionID, time.Now()
	}
	i.updateGeminiAnalytics()
	i.updateGeminiLatestPrompt()
}

// updateGeminiAnalytics refreshes token counts, cost, and model from the session file.
// Syncs the detected model back to the instance's GeminiModel field.
func (i *Instance) updateGeminiAnalytics() {
	if i.GeminiSessionID == "" {
		return
	}
	if i.GeminiAnalytics == nil {
		i.GeminiAnalytics = &GeminiSessionAnalytics{}
	}
	// Non-blocking update (ignore errors, best effort)
	_ = UpdateGeminiAnalyticsFromDisk(i.ProjectPath, i.GeminiSessionID, i.GeminiAnalytics)

	// Sync detected model from analytics to instance (if not explicitly set by user)
	if i.GeminiModel == "" && i.GeminiAnalytics.Model != "" {
		i.GeminiModel = i.GeminiAnalytics.Model
	}
}

// updateGeminiLatestPrompt extracts the latest user prompt from the session file.
// Uses mtime caching to skip re-reading unchanged files (important for large session files).
func (i *Instance) updateGeminiLatestPrompt() {
	if i.GeminiSessionID == "" || len(i.GeminiSessionID) < 8 {
		return
	}

	sessionsDir := GetGeminiSessionsDir(i.ProjectPath)
	pattern := filepath.Join(sessionsDir, "session-*-"+i.GeminiSessionID[:8]+".json")
	filePath, fileMtime := findNewestFile(pattern)

	// Fallback: cross-project search
	if filePath == "" {
		filePath = findGeminiSessionInAllProjects(i.GeminiSessionID)
		if filePath != "" {
			if info, err := os.Stat(filePath); err == nil {
				fileMtime = info.ModTime()
			}
		}
	}

	if filePath == "" {
		return
	}

	// mtime cache: skip re-read if file hasn't changed since last read
	if !i.lastPromptModTime.IsZero() && !fileMtime.IsZero() && fileMtime.Equal(i.lastPromptModTime) {
		return
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		return
	}
	if prompt, err := parseGeminiLatestUserPrompt(data); err == nil && prompt != "" {
		i.LatestPrompt = prompt
	}
	i.lastPromptModTime = fileMtime
}

// SyncSessionIDsToTmux syncs session IDs from Instance to tmux environment.
// PERFORMANCE: This is called on-demand (e.g., first attach) rather than at load time
// to reduce subprocess overhead during TUI startup.
//
// Session IDs are needed in tmux environment for restart/resume operations that
// spawn new processes. Without this sync, R key wouldn't resume the correct session.
func (i *Instance) SyncSessionIDsToTmux() {
	if i.tmuxSession == nil || !i.tmuxSession.Exists() {
		return
	}

	// Sync ClaudeSessionID.
	//
	// #1815: never publish a disk-scanned id into the pane env. The env is
	// read back as an ownership signal, so publishing a suspect id would
	// launder it into a resumable one on the next poll — the guard's own
	// bypass. A suspect id is refused at spawn anyway, so the pane never
	// legitimately holds one.
	i.claudeSessionIDsFromDiskScanMu.Lock()
	tainted := i.claudeSessionIDsFromDiskScan[i.ClaudeSessionID]
	i.claudeSessionIDsFromDiskScanMu.Unlock()
	if i.ClaudeSessionID != "" && !tainted {
		_ = i.tmuxSession.SetEnvironment("CLAUDE_SESSION_ID", i.ClaudeSessionID)
	}

	// Sync GeminiSessionID
	if i.GeminiSessionID != "" {
		_ = i.tmuxSession.SetEnvironment("GEMINI_SESSION_ID", i.GeminiSessionID)
	}

	// Sync OpenCodeSessionID
	if i.OpenCodeSessionID != "" {
		_ = i.tmuxSession.SetEnvironment("OPENCODE_SESSION_ID", i.OpenCodeSessionID)
	}

	// Sync CodexSessionID
	if i.CodexSessionID != "" {
		_ = i.tmuxSession.SetEnvironment("CODEX_SESSION_ID", i.CodexSessionID)
	}

	// Sync CopilotSessionID
	if i.CopilotSessionID != "" {
		_ = i.tmuxSession.SetEnvironment("COPILOT_SESSION_ID", i.CopilotSessionID)
	}

	// Custom [tools.*] resume id: re-publish so a restart that still has the
	// old pane (or a respawn) sees the same id. The agent-deck-owned variable
	// is written too, so a tool that exposes no env var of its own is still
	// recoverable from the pane.
	//
	// Gated on the scope check for the same reason the read side is. The pane
	// env is read back as a live ownership signal, so publishing an id that
	// resume has refused would launder it into acceptance on the next poll --
	// the refusal has to hold on both sides of the loop or it holds on neither.
	if i.GenericSessionID != "" && i.genericSessionScopeMismatch() == "" {
		for _, envName := range i.genericSessionEnvNames() {
			_ = i.tmuxSession.SetEnvironment(envName, i.GenericSessionID)
		}
	}
}

// SyncSessionIDsFromTmux reads tool session IDs from the tmux environment
// into the Instance struct. This is the reverse of SyncSessionIDsToTmux.
// Used in the stop path to capture IDs that may not have been saved during
// start (e.g., if PostStartSync timed out but the tool started late).
// Only updates fields where the tmux env has a non-empty value; does not
// blank existing IDs if the tmux env is missing the variable.
func (i *Instance) SyncSessionIDsFromTmux() {
	bindings := []struct {
		kind, env string
	}{
		{"claude", "CLAUDE_SESSION_ID"},
		{"gemini", "GEMINI_SESSION_ID"},
		{"opencode", "OPENCODE_SESSION_ID"},
		{"codex", "CODEX_SESSION_ID"},
		{"copilot", "COPILOT_SESSION_ID"},
	}
	for _, item := range bindings {
		observation := i.CaptureRuntimeBindingObservation(item.kind)
		i.mu.RLock()
		tmuxSession := i.tmuxSession
		i.mu.RUnlock()
		if tmuxSession == nil || !tmuxSession.Exists() {
			return
		}
		id, err := tmuxSession.GetEnvironment(item.env)
		if err != nil || id == "" {
			continue
		}
		if err := i.PublishRuntimeBindingObservation(observation, id, time.Now()); err != nil {
			sessionLog.Debug("tmux_binding_rejected",
				slog.String("kind", item.kind), slog.String("session_id", id),
				slog.String("error", err.Error()))
			continue
		}
		if item.kind == "claude" {
			// #1815: own pane env is a weak vouch, after durable acceptance.
			i.noteClaudeSessionIDFromOwnPane()
		}
	}

	// Custom tools keep their conversation id outside the fixed runtime-binding
	// kinds. Pull it from the tool's own env var or agent-deck's fallback.
	i.mu.RLock()
	tmuxSession := i.tmuxSession
	i.mu.RUnlock()
	if tmuxSession == nil || !tmuxSession.Exists() {
		return
	}
	for _, envName := range i.genericSessionEnvNames() {
		id, err := tmuxSession.GetEnvironment(envName)
		if err != nil {
			continue
		}
		if id = strings.TrimSpace(id); id != "" {
			i.persistGenericSessionIDIfChanged(id)
			break
		}
	}
}

// GetSessionIDFromTmux reads Claude session ID from tmux environment
// This is the primary method for sessions started with the capture-resume pattern
func (i *Instance) GetSessionIDFromTmux() string {
	i.mu.RLock()
	tmuxSession := i.tmuxSession
	i.mu.RUnlock()
	if tmuxSession == nil {
		return ""
	}
	sessionID, err := tmuxSession.GetEnvironment("CLAUDE_SESSION_ID")
	if err != nil {
		return ""
	}
	return sessionID
}

// RefreshLiveSessionIDs re-reads tool-specific session identifiers from the
// live tmux environment and updates the instance's stored IDs when a newer
// non-empty value is found. Safe no-op when tmuxSession is nil or the tool
// has no live-env handle.
//
// Call this before reads that must reflect the CURRENT conversation (e.g.
// TUI cross-session send-output, issue #598). Reads that tolerate stale data
// (status polling) don't need it.
func (i *Instance) RefreshLiveSessionIDs() {
	i.mu.RLock()
	tool, tmuxSession := i.Tool, i.tmuxSession
	i.mu.RUnlock()
	if tmuxSession == nil {
		return
	}
	if IsClaudeCompatible(tool) {
		i.UpdateClaudeSession(nil)
	}
	if tool == "gemini" {
		i.syncGeminiSessionFromTmux()
	}
}

// bindClaudeSessionFromHook performs the common bookkeeping when
// UpdateHookStatus has decided a candidate session ID wins: log the
// lifecycle event, update the in-memory instance fields, and propagate
// the ID into the tmux environment so a future restart's
// capture-resume pattern picks it up. `action` is "bind" (cold start)
// or "rebind" (replacing an existing ID).
func (i *Instance) bindClaudeSessionFromHook(observation RuntimeBindingObservation, sessionID, hookSource, hookEvent, action string, fingerprint HookStatusFingerprint) {
	oldID := i.ClaudeSessionID
	if !i.publishObservedRuntimeBindingLocked(observation, sessionID, fingerprint) {
		return
	}
	sessionLog.Debug("claude_session_update_from_hook",
		slog.String("old_id", oldID),
		slog.String("new_id", sessionID),
		slog.String("event", hookEvent),
	)
	_ = WriteSessionIDLifecycleEvent(SessionIDLifecycleEvent{
		InstanceID: i.ID, Tool: i.Tool, Action: action,
		Source: hookSource, OldID: oldID, NewID: sessionID,
		HookEvent: hookEvent,
	})
	// #1815: a hook payload correlated to this instance identifies THIS
	// session, so the id is verified ownership.
	i.markClaudeSessionIDVerified()
	i.hookSessionID = sessionID

	if i.tmuxSession != nil && i.tmuxSession.Exists() {
		_ = i.tmuxSession.SetEnvironment("CLAUDE_SESSION_ID", sessionID)
	}

	// The publish above already persisted the rebind: none of the three
	// UpdateHookStatus callers (TUI tick, web refresh, CLI status refresh)
	// save after a rebind, and without a synchronous write the pre-/clear
	// UUID stayed in tool_data.claude_session_id for DB-direct consumers
	// while peers reloading the stale row kept producing fresh "rebind"
	// lifecycle entries. What remains is the recall link.
	i.confirmHookSessionLink("claude", sessionID, hookSource)
}

// confirmHookSessionLink records the authoritative recall session_links row
// for a harness id a live hook just bound (the bind/rebind paths) or
// confirmed as this instance's already-bound id (the equality branches).
// The binding itself changes only through the runtime binding CAS, so the
// link follows a successful publish rather than sharing its transaction as
// upstream's Write{Claude,Codex,Gemini}SessionBinding did. The equality
// branches matter as much as the binds: ids agent-deck mints at launch
// (--session-id) reach UpdateHookStatus already equal, and an id bound by a
// non-hook path, or whose earlier link write failed, has no link until a
// hook vouches for it. Failures are logged and retried on the next hook.
// The caller holds i.mu; recordRecallLinkLocked releases it around the
// write.
func (i *Instance) confirmHookSessionLink(kind, sessionID, hookSource string) {
	if err := i.recordRecallLinkLocked(kind, sessionID); err != nil {
		sessionLog.Warn("session_link_confirm_failed",
			slog.String("instance_id", i.ID),
			slog.String("kind", kind),
			slog.String("session_id", sessionID),
			slog.String("source", hookSource),
			slog.String("error", err.Error()))
	}
}

// linkRuntimeBindingFn is statedb's LinkRuntimeBinding. Test seam: it lets a
// test observe the link write, including the instance locks held around it.
var linkRuntimeBindingFn = (*statedb.StateDB).LinkRuntimeBinding

// recordRecallLinkLocked writes the authoritative session_links row for
// value while value is still this instance's durable binding of kind
// (statedb.LinkRuntimeBinding checks both in one transaction). The
// in-memory mark keeps it to one attempt per binding version per process
// once the link exists, not one per hook event. It names the version, not
// the id: a rebind, even back to an id linked earlier, is a new version,
// and a peer may have linked another id in between.
//
// The caller holds i.mu, which is released around the database call and
// retaken before returning, as publishObservedRuntimeBindingLocked does
// around the CAS: writing a missing link takes SQLite's writer reservation,
// and status readers must not queue on i.mu behind a peer's write. The mark
// set afterwards names the version read before the call, so a binding that
// moved on meanwhile simply misses it.
func (i *Instance) recordRecallLinkLocked(kind, value string) error {
	if value == "" {
		return nil
	}
	version, versioned := i.recallLinkVersionLocked(kind, value)
	if versioned && i.linkedBindings.has(kind, version) {
		return nil
	}
	owner, snapshot := i.owningDB, i.storageSnapshot
	instanceID, incarnation := i.ID, i.persistenceIncarnation
	i.mu.Unlock()
	// The owning store, as the binding itself was published to
	// (resolveOwningStateDB); none at all leaves nothing to link.
	db, release, err := i.resolveOwningStateDB(owner, snapshot)
	linked := false
	if err == nil && db != nil {
		linked, err = linkRuntimeBindingFn(db, instanceID, incarnation, kind, value)
	}
	release()
	i.mu.Lock()
	if err != nil || !linked {
		return err
	}
	if versioned {
		i.linkedBindings.set(kind, version)
	}
	return nil
}

// recallLinkVersionLocked returns the version of the in-memory binding of
// kind while it holds value. Every publish and every adoption of a peer's
// binding records one, so a hook-bound id always has a version; an id that
// reached memory without either has none, and no mark can vouch for its
// link. The caller holds i.mu.
func (i *Instance) recallLinkVersionLocked(kind, value string) (recallLinkVersion, bool) {
	binding, ok := i.RuntimeBindings[kind]
	if !ok || binding.Value != value {
		return recallLinkVersion{}, false
	}
	return recallLinkVersion{
		incarnation: i.persistenceIncarnation,
		generation:  binding.Generation,
		revision:    binding.Revision,
		value:       value,
	}, true
}

// RecordRecallLink records the authoritative recall link for the instance's
// current binding of kind when it lacks one. A published binding whose link
// write failed stays bound, and nothing about the binding itself will retry
// the link; an observer that owns the identity (legacy Codex hydration)
// calls this to repair it. A linked identity costs one read.
func (i *Instance) RecordRecallLink(kind string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	value := i.runtimeBindingValueLocked(kind)
	if err := i.recordRecallLinkLocked(kind, value); err != nil {
		sessionLog.Warn("session_link_record_failed",
			slog.String("instance_id", i.ID),
			slog.String("kind", kind),
			slog.String("session_id", value),
			slog.String("error", err.Error()))
		return err
	}
	return nil
}

// retractClaudeCandidateLink drops the recall session_links row of a
// candidate id the adoption arbitration rejected. A candidate that was bound
// earlier and lost a re-adoption (or turned out to be a zombie) must not
// linger as a link, or the recall index would bind its transcript to this
// instance. Rows are written by confirmHookSessionLink when a hook binds or
// confirms an id; this is the matching retraction.
func (i *Instance) retractClaudeCandidateLink(candidate string) {
	if candidate == "" {
		return
	}
	// UpdateHookStatus calls this holding i.mu and UpdateClaudeSession's
	// zombie rejection without it, so the owning handle is read without
	// taking i.mu (withOwningStateDBLocked) in both cases.
	if err := i.withOwningStateDBLocked(func(db *statedb.StateDB) error {
		_, err := db.RetractSessionLink(i.ID, "claude", candidate)
		return err
	}); err != nil && !errors.Is(err, errNoStateDB) {
		sessionLog.Warn("claude_session_link_retract_failed",
			slog.String("instance_id", i.ID),
			slog.String("candidate", candidate),
			slog.String("error", err.Error()))
		return
	}
	i.linkedBindings.clear("claude", candidate)
}

// UpdateClaudeSessionsWithDedup is retained for caller compatibility. Durable
// binding ownership is now decided by instance_runtime_binding's unique owner
// constraint; an age/path/disk guess must never authorize clearing a peer.
// Reload already projects each instance's durable winner, so no cross-instance
// mutation is required here.
func UpdateClaudeSessionsWithDedup(instances []*Instance) {
	_ = instances
}

// applyHookSessionIDLocked binds the session id carried by the latest hook
// sample to the tool's session id field (the hook fast path in UpdateStatus).
// Caller holds i.mu.
func (i *Instance) applyHookSessionIDLocked() {
	if i.hookSessionID == "" {
		return
	}
	switch {
	case IsClaudeCompatible(i.Tool):
		if i.claudeIDSuperseded(i.hookSessionID) {
			// #2683: stale hook from the pre-handoff process.
			break
		}
		if i.hookSessionID != i.ClaudeSessionID {
			i.ClaudeSessionID = i.hookSessionID
			// #1815: own hook anchor — verified ownership.
			i.markClaudeSessionIDVerified()
			i.ClaudeDetectedAt = time.Now()
		} else {
			// Review finding on #1830: the values already matching is
			// NOT a no-op case for the taint. A live hook confirming
			// the session's own id is the strongest available vouch
			// (the process itself, not a disk guess) — clear any
			// lingering disk-scan taint on it, or a solo custom-wrapper
			// session whose own id was once disk-scanned stays
			// permanently unresumable even though this hook just
			// proved it right.
			i.markClaudeSessionIDVerified()
		}
	case IsCodexCompatible(i.Tool):
		if i.hookSessionID != i.CodexSessionID {
			i.CodexSessionID = i.hookSessionID
			i.recordCodexOwnership(i.CodexSessionID)
			i.CodexDetectedAt = time.Now()
		}
	case i.Tool == "gemini":
		if i.hookSessionID != i.GeminiSessionID {
			i.GeminiSessionID = i.hookSessionID
			i.GeminiDetectedAt = time.Now()
		}
	}
}
