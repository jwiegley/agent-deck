package session

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

var (
	ErrRuntimeBindingObservationStale = errors.New("session: runtime binding observation is stale")
	ErrRuntimeBindingOwnership        = errors.New("session: runtime binding is owned by another instance")

	// errRuntimeBindingSpawnLockBusy reports a publication that found the
	// instance spawn lock held and did not wait for it.
	errRuntimeBindingSpawnLockBusy = errors.New("session: instance spawn lock is held; binding not published")

	// Test seam: a successful durable change has committed, but the matching
	// in-memory projection has not been published yet.
	runtimeBindingBeforeMemoryPublishFn = func(*Instance, statedb.RuntimeBinding) {}
	// A binding restamp first removes the physical runtime's environment
	// completeness marker. The marker is restored only after the durable write
	// and session-local cleanup stamp both succeed.
	runtimeBindingCleanupInvalidateFn = func(session *tmux.Session) error {
		return session.UnsetEnvironment("AGENTDECK_RUNTIME_GENERATION")
	}
)

type runtimeBindingCleanupTarget struct {
	session      *tmux.Session
	instanceID   string
	generation   uint64
	bindingKey   string
	bindingValue string
}

// RuntimeBindingObservation identifies the runtime and binding revision that
// produced a tool-session observation. Its fields are deliberately opaque:
// callers may only capture it immediately before querying and pass it back to
// PublishRuntimeBindingObservation after the query completes.
type RuntimeBindingObservation struct {
	instanceID  string
	incarnation string
	kind        string
	generation  uint64
	revision    uint64
	value       string
}

type validatedHookRuntimeBinding struct {
	fingerprint HookStatusFingerprint
	incarnation string
	generation  uint64
	revision    uint64
	value       string
}

// RuntimeBindingPublishError reports a rejected binding observation without
// exposing a partially updated in-memory Instance.
type RuntimeBindingPublishError struct {
	InstanceID string
	Kind       string
	Generation uint64
	Revision   uint64
	Cause      error
}

func (e *RuntimeBindingPublishError) Error() string {
	return fmt.Sprintf("publish %s binding for %s at generation %d revision %d: %v",
		e.Kind, e.InstanceID, e.Generation, e.Revision, e.Cause)
}

func (e *RuntimeBindingPublishError) Unwrap() error { return e.Cause }

// CaptureRuntimeBindingObservation captures the authority token before an
// observer performs any tmux, process, or disk query.
func (i *Instance) CaptureRuntimeBindingObservation(kind string) RuntimeBindingObservation {
	i.mu.RLock()
	defer i.mu.RUnlock()
	observation := RuntimeBindingObservation{
		instanceID:  i.ID,
		incarnation: i.persistenceIncarnation,
		kind:        kind,
		generation:  i.RuntimeGeneration,
		value:       i.runtimeBindingValueLocked(kind),
	}
	if binding, ok := i.RuntimeBindings[kind]; ok && binding.Generation == observation.generation {
		observation.revision = binding.Revision
		observation.value = binding.Value
	}
	return observation
}

func (i *Instance) captureActiveRuntimeBindingObservation() RuntimeBindingObservation {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.runtimeBindingObservationLocked(activeRuntimeBindingKind(i))
}

// PublishRuntimeBindingObservation is the single post-commit observation
// boundary for tool session IDs. Callers must hold neither i.mu nor the
// instance spawn lock. The token is never recaptured after waiting: an
// observation made by an older physical runtime cannot bind its replacement.
func (i *Instance) PublishRuntimeBindingObservation(observation RuntimeBindingObservation, value string, detectedAt time.Time) error {
	return i.publishRuntimeBindingObservation(observation, value, detectedAt, true)
}

// publishRuntimeBindingObservation is PublishRuntimeBindingObservation that,
// unless wait is set, makes a single attempt at the instance spawn lock and
// returns errRuntimeBindingSpawnLockBusy while a start, restart or another
// publisher holds it, rather than wait out the lock's budget.
func (i *Instance) publishRuntimeBindingObservation(observation RuntimeBindingObservation, value string, detectedAt time.Time, wait bool) error {
	if observation.instanceID != i.ID {
		return i.runtimeBindingPublishError(observation.kind, observation.generation, observation.revision,
			ErrRuntimeBindingObservationStale)
	}
	if !runtimeBindingKindSupported(observation.kind) {
		return i.runtimeBindingPublishError(observation.kind, observation.generation, observation.revision,
			fmt.Errorf("unsupported runtime binding kind %q", observation.kind))
	}

	var release func()
	var err error
	if wait {
		release, err = acquireInstanceSpawnLock(observation.instanceID)
	} else {
		var acquired bool
		if release, acquired, err = tryAcquireInstanceSpawnLock(observation.instanceID); err == nil && !acquired {
			err = errRuntimeBindingSpawnLockBusy
		}
	}
	if err != nil {
		return i.runtimeBindingPublishError(observation.kind, observation.generation, observation.revision, err)
	}
	defer release()
	return i.publishRuntimeBindingObservationLocked(observation, value, detectedAt)
}

// publishRuntimeBindingObservationLocked requires the instance spawn lock, not i.mu.
func (i *Instance) publishRuntimeBindingObservationLocked(observation RuntimeBindingObservation, value string, detectedAt time.Time) error {

	i.mu.RLock()
	current := i.runtimeBindingObservationLocked(observation.kind)
	db := i.owningDB
	i.mu.RUnlock()
	if !current.sameVersion(observation) {
		return i.runtimeBindingPublishError(observation.kind, observation.generation, observation.revision,
			ErrRuntimeBindingObservationStale)
	}
	if db == nil {
		db = statedb.GetGlobal()
	}

	// New instances do not have a database until their first save. Preserve the
	// versioned shape locally so insertInitialRuntimeTx can publish it later.
	if db == nil {
		if current.value == value {
			return nil
		}
		binding := statedb.RuntimeBinding{
			InstanceID: i.ID, Kind: observation.kind, Generation: observation.generation,
			Revision: observation.revision + 1, Value: value, DetectedAt: detectedAt,
		}
		i.mu.Lock()
		defer i.mu.Unlock()
		if !i.runtimeBindingObservationLocked(observation.kind).sameVersion(observation) {
			return i.runtimeBindingPublishError(observation.kind, observation.generation, observation.revision,
				ErrRuntimeBindingObservationStale)
		}
		return i.applyRuntimeBindingLocked(binding)
	}

	cleanupTarget := i.runtimeBindingCleanupTarget(observation, value)

	// A repeated observation is a true no-op. Validate it against durable state
	// while serialized so a stale in-memory snapshot cannot conceal a peer's
	// winning revision, then retain the original revision and detection time.
	if current.value == value {
		if err := db.ValidateRuntimeIncarnation(i.ID, observation.incarnation); err != nil {
			return i.runtimeBindingPublishError(observation.kind, observation.generation, observation.revision, err)
		}
		durableState, stateFound, stateErr := db.ReadRuntimeState(i.ID)
		if stateErr != nil {
			return i.runtimeBindingPublishError(observation.kind, observation.generation, observation.revision, stateErr)
		}
		if !stateFound || durableState.Generation != observation.generation {
			return i.rejectRuntimeBindingConflictLocked(observation, statedb.ErrRuntimeGenerationConflict)
		}
		binding, found, readErr := db.ReadRuntimeBinding(i.ID, observation.kind)
		if readErr != nil {
			return i.runtimeBindingPublishError(observation.kind, observation.generation, observation.revision, readErr)
		}
		if !found && observation.revision == 0 && value == "" {
			return nil
		}
		if !found || binding.Generation != observation.generation ||
			binding.Revision != observation.revision || binding.Value != value {
			return i.rejectRuntimeBindingConflictLocked(observation, statedb.ErrBindingRevisionConflict)
		}
		i.mu.Lock()
		defer i.mu.Unlock()
		if !i.runtimeBindingObservationLocked(observation.kind).sameVersion(observation) {
			return i.runtimeBindingPublishError(observation.kind, observation.generation, observation.revision,
				ErrRuntimeBindingObservationStale)
		}
		return i.applyRuntimeBindingLocked(binding)
	}

	// The cleanup marker is invalidated only after SQLite has proved and staged
	// this exact incarnation, generation, and binding revision. Keeping that
	// bounded mutation inside the immediate transaction prevents an obsolete
	// peer from touching a newer physical runtime before its stale CAS fails.
	var beforeCommit func() error
	if cleanupTarget.session != nil {
		beforeCommit = func() error {
			if err := runtimeBindingCleanupInvalidateFn(cleanupTarget.session); err != nil {
				return fmt.Errorf("invalidate runtime cleanup binding: %w", err)
			}
			return nil
		}
	}
	binding, applied, err := db.WriteRuntimeBindingIfVersionWithCommitFence(
		i.ID, observation.incarnation, observation.generation, observation.kind, observation.revision, value, detectedAt, beforeCommit)
	if err != nil {
		cause := err
		if isRuntimeBindingOwnershipError(err) {
			cause = errors.Join(ErrRuntimeBindingOwnership, err)
		}
		return i.runtimeBindingPublishError(observation.kind, observation.generation, observation.revision, cause)
	}
	if !applied {
		cause := error(statedb.ErrBindingRevisionConflict)
		if durable, found, readErr := db.ReadRuntimeState(i.ID); readErr != nil {
			return i.runtimeBindingPublishError(observation.kind, observation.generation, observation.revision, readErr)
		} else if !found || durable.Generation != observation.generation {
			cause = statedb.ErrRuntimeGenerationConflict
		}
		return i.rejectRuntimeBindingConflictLocked(observation, cause)
	}

	if cleanupTarget.session != nil {
		if err := restampRuntimeBindingCleanup(cleanupTarget); err != nil {
			// The durable winner is already committed. Publish it locally before
			// returning the restamp failure, while leaving cleanup fail-closed.
			if adoptErr := i.adoptDurableRuntimeLocked(); adoptErr != nil {
				err = errors.Join(err, adoptErr)
			}
			return i.runtimeBindingPublishError(observation.kind, observation.generation, observation.revision,
				fmt.Errorf("restamp runtime cleanup binding: %w", err))
		}
	}

	runtimeBindingBeforeMemoryPublishFn(i, binding)
	i.mu.Lock()
	if !i.runtimeBindingObservationLocked(observation.kind).sameVersion(observation) {
		i.mu.Unlock()
		_ = i.adoptDurableRuntimeLocked()
		return i.runtimeBindingPublishError(observation.kind, observation.generation, observation.revision,
			ErrRuntimeBindingObservationStale)
	}
	err = i.applyRuntimeBindingLocked(binding)
	i.mu.Unlock()
	return err
}

// runtimeBindingCleanupTarget captures the current physical tmux target while
// the caller holds the instance lifecycle lock. Generation zero and runtimes
// without a tmux session have no physical cleanup authority to restamp.
func (i *Instance) runtimeBindingCleanupTarget(observation RuntimeBindingObservation, value string) runtimeBindingCleanupTarget {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if observation.generation == 0 || i.RuntimeGeneration != observation.generation ||
		observation.kind != activeRuntimeBindingKind(i) || i.tmuxSession == nil || i.tmuxSession.Name == "" {
		return runtimeBindingCleanupTarget{}
	}
	return runtimeBindingCleanupTarget{
		session: i.tmuxSession, instanceID: i.ID, generation: observation.generation,
		bindingKey: runtimeBindingEnvironmentKey(observation.kind), bindingValue: value,
	}
}

func restampRuntimeBindingCleanup(target runtimeBindingCleanupTarget) error {
	if err := runtimeCleanupIdentityStampFn(
		target.session, target.instanceID, target.generation, target.bindingKey, target.bindingValue,
	); err != nil {
		return err
	}
	// This is the final completeness marker checked by destructive cleanup.
	return runtimeCandidateSetEnvFn(
		target.session, "AGENTDECK_RUNTIME_GENERATION", strconv.FormatUint(target.generation, 10),
	)
}

// publishHookRuntimeBindingObservation avoids repeating durable binding checks
// for an unchanged cached hook file on every status tick. A changed source
// fingerprint or binding token deliberately misses this cache and takes the
// durable publisher above. Timestamp is intentionally excluded: hook payloads
// have whole-second granularity and distinct events can share one value.
// wait selects how the durable publisher takes the instance spawn lock (see
// publishRuntimeBindingObservation).
func (i *Instance) publishHookRuntimeBindingObservation(observation RuntimeBindingObservation, value string, fingerprint HookStatusFingerprint, wait bool) error {
	i.mu.Lock()
	// Cached hook IDs can predate their rollout metadata or the watcher's
	// ownership check. Recheck before even the unchanged-fingerprint fast path:
	// accepting a hook earlier does not make a subsequently identified child
	// thread an authoritative binding for its parent.
	if observation.kind == "codex" && i.shouldRejectCodexSubagentRebind(value) {
		i.clearRejectedCodexHookLocked(value)
		delete(i.validatedHookBindings, observation.kind)
		i.mu.Unlock()
		return i.runtimeBindingPublishError(observation.kind, observation.generation, observation.revision,
			errors.New("hook candidate is a Codex subagent thread"))
	}
	validated, ok := i.validatedHookBindings[observation.kind]
	current := i.runtimeBindingObservationLocked(observation.kind)
	db := i.owningDB
	i.mu.Unlock()
	if fingerprint.valid() && ok && validated.fingerprint == fingerprint &&
		validated.incarnation == observation.incarnation &&
		validated.generation == observation.generation && validated.revision == observation.revision &&
		validated.value == value && current.sameVersion(observation) && current.value == value {
		if db == nil {
			db = statedb.GetGlobal()
		}
		if db != nil {
			if err := db.ValidateRuntimeIncarnation(i.ID, observation.incarnation); err != nil {
				return i.runtimeBindingPublishError(
					observation.kind, observation.generation, observation.revision, err,
				)
			}
		}
		return nil
	}

	if err := i.publishRuntimeBindingObservation(observation, value, time.Now(), wait); err != nil {
		return err
	}

	expectedRevision := observation.revision
	if observation.value != value {
		expectedRevision++
	}
	i.mu.Lock()
	current = i.runtimeBindingObservationLocked(observation.kind)
	if fingerprint.valid() && current.generation == observation.generation && current.revision == expectedRevision && current.value == value {
		if i.validatedHookBindings == nil {
			i.validatedHookBindings = make(map[string]validatedHookRuntimeBinding)
		}
		i.validatedHookBindings[observation.kind] = validatedHookRuntimeBinding{
			fingerprint: fingerprint, incarnation: observation.incarnation,
			generation: current.generation, revision: current.revision, value: value,
		}
	}
	i.mu.Unlock()
	return nil
}

// clearRejectedCodexHookLocked discards only cached evidence from the rejected
// child. A valid parent's previously accepted hook must remain intact.
func (i *Instance) clearRejectedCodexHookLocked(sessionID string) {
	if i.hookSessionID != sessionID {
		return
	}
	i.hookStatus, i.hookEvent, i.hookSessionID = "", "", ""
	i.hookLastUpdate = time.Time{}
	i.hookFingerprint = HookStatusFingerprint{}
	i.codexStartedGeneration, i.codexCompletedGeneration = "", ""
	i.codexStartedSessionID, i.codexCompletedSessionID = "", ""
}

// publishRuntimeBinding is reserved for explicit operator mutations, whose
// observation starts at the call itself. Runtime observers must capture their
// token before querying and call PublishRuntimeBindingObservation directly.
func (i *Instance) publishRuntimeBinding(kind, value string, detectedAt time.Time) error {
	return i.PublishRuntimeBindingObservation(i.CaptureRuntimeBindingObservation(kind), value, detectedAt)
}

// NormalizeCodexSessionID validates a Codex identity exactly as
// SetField(FieldCodexSessionID) does, without publishing it. An observer that
// must vet a candidate before binding it validates here and publishes through
// PublishRuntimeBindingObservation only once the candidate is accepted:
// SetField's binding is durable before it returns, so a later refusal could
// not take it back.
func NormalizeCodexSessionID(value string) (string, error) {
	return normalizeToolSessionID(FieldCodexSessionID, value)
}

// rejectRuntimeBindingConflictLocked runs while PublishRuntimeBindingObservation
// holds the instance lifecycle lock.
func (i *Instance) rejectRuntimeBindingConflictLocked(observation RuntimeBindingObservation, cause error) error {
	if !errors.Is(cause, statedb.ErrInstanceParentConflict) {
		if err := i.adoptDurableRuntimeLocked(); err != nil {
			cause = errors.Join(cause, err)
		}
	}
	return i.runtimeBindingPublishError(observation.kind, observation.generation, observation.revision, cause)
}

func (observation RuntimeBindingObservation) sameVersion(other RuntimeBindingObservation) bool {
	return observation.instanceID == other.instanceID && observation.incarnation == other.incarnation && observation.kind == other.kind &&
		observation.generation == other.generation && observation.revision == other.revision
}

func (i *Instance) runtimeBindingObservationLocked(kind string) RuntimeBindingObservation {
	observation := RuntimeBindingObservation{
		instanceID:  i.ID,
		incarnation: i.persistenceIncarnation,
		kind:        kind,
		generation:  i.RuntimeGeneration,
		value:       i.runtimeBindingValueLocked(kind),
	}
	if binding, ok := i.RuntimeBindings[kind]; ok && binding.Generation == observation.generation {
		observation.revision = binding.Revision
		observation.value = binding.Value
	}
	return observation
}

func (i *Instance) runtimeBindingValueLocked(kind string) string {
	switch kind {
	case "claude":
		return i.ClaudeSessionID
	case "copilot":
		return i.CopilotSessionID
	case "codex":
		return i.CodexSessionID
	case "gemini":
		return i.GeminiSessionID
	case "opencode":
		return i.OpenCodeSessionID
	default:
		return ""
	}
}

func runtimeBindingKindSupported(kind string) bool {
	switch kind {
	case "claude", "copilot", "codex", "gemini", "opencode":
		return true
	default:
		return false
	}
}

func (i *Instance) runtimeBindingPublishError(kind string, generation, revision uint64, cause error) error {
	return &RuntimeBindingPublishError{
		InstanceID: i.ID, Kind: kind, Generation: generation, Revision: revision, Cause: cause,
	}
}

func (i *Instance) applyRuntimeBindingLocked(binding statedb.RuntimeBinding) error {
	switch binding.Kind {
	case "claude":
		i.ClaudeSessionID, i.ClaudeDetectedAt = binding.Value, binding.DetectedAt
	case "copilot":
		i.CopilotSessionID, i.CopilotDetectedAt = binding.Value, binding.DetectedAt
	case "codex":
		i.CodexSessionID, i.CodexDetectedAt = binding.Value, binding.DetectedAt
		i.recordCodexOwnershipLocked(binding.Value)
	case "gemini":
		i.GeminiSessionID, i.GeminiDetectedAt = binding.Value, binding.DetectedAt
	case "opencode":
		i.OpenCodeSessionID, i.OpenCodeDetectedAt = binding.Value, binding.DetectedAt
	default:
		return fmt.Errorf("unsupported runtime binding kind %q", binding.Kind)
	}
	if i.RuntimeBindings == nil {
		i.RuntimeBindings = make(map[string]statedb.RuntimeBinding)
	}
	i.RuntimeBindings[binding.Kind] = binding
	return nil
}

func isRuntimeBindingOwnershipError(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "unique constraint") &&
		(strings.Contains(message, "idx_runtime_binding_owner") ||
			strings.Contains(message, "instance_runtime_binding.binding_kind"))
}
