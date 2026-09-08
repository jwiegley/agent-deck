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
	runtimeApplyBeforePublishFn  = func(statedb.RuntimeState) {}
	runtimeReloadBeforePublishFn = func() {}
	runtimeStatusAfterVersionFn  = func() {}
)

// runtimeStateSnapshot captures one coherent runtime tuple from the Instance.
func (i *Instance) runtimeStateSnapshot() statedb.RuntimeState {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.runtimeStateSnapshotLocked()
}

func (i *Instance) runtimeStateSnapshotLocked() statedb.RuntimeState {
	name := ""
	if i.tmuxSession != nil {
		name = i.tmuxSession.Name
	}
	return statedb.RuntimeState{
		InstanceID:     i.ID,
		Generation:     i.RuntimeGeneration,
		StatusRevision: i.StatusRevision,
		TmuxSession:    name,
		TmuxSocketName: i.TmuxSocketName,
		Status:         string(i.Status),
		LastStartedAt:  i.LastStartedAt,
	}
}

// RuntimeState returns the instance's current coherent runtime tuple.
func (i *Instance) RuntimeState() statedb.RuntimeState { return i.runtimeStateSnapshot() }

func (i *Instance) RuntimeVersion() (uint64, uint64) {
	state := i.runtimeStateSnapshot()
	return state.Generation, state.StatusRevision
}

// ApplyStatusIfRuntimeVersion applies a shared status observation only while
// its generation and revision still identify the instance's current runtime.
// The check and write share one lock so a runtime transition cannot interleave.
func (i *Instance) ApplyStatusIfRuntimeVersion(generation, revision uint64, status Status) (applied, changed bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.RuntimeGeneration != generation || i.StatusRevision != revision {
		return false, false
	}
	runtimeStatusAfterVersionFn()
	changed = i.Status != status
	i.Status = status
	return true, changed
}

func (i *Instance) AcceptStatusRevision(generation, previousRevision uint64) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.RuntimeGeneration != generation || i.StatusRevision != previousRevision {
		return false
	}
	i.StatusRevision++
	return true
}

func (i *Instance) RuntimeStateIsCurrent(expected statedb.RuntimeState) bool {
	current, found, err := i.durableRuntimeState()
	if err != nil {
		return false
	}
	if !found {
		current = i.runtimeStateSnapshot()
	}
	return current.InstanceID == expected.InstanceID &&
		current.Generation == expected.Generation &&
		current.StatusRevision == expected.StatusRevision &&
		current.TmuxSession == expected.TmuxSession &&
		current.TmuxSocketName == expected.TmuxSocketName
}

// ApplyRuntimeState publishes a durable transition result onto a canonical UI
// object. Older generations are ignored.
func (i *Instance) ApplyRuntimeState(state statedb.RuntimeState) bool {
	runtimeApplyBeforePublishFn(state)
	i.mu.Lock()
	defer i.mu.Unlock()
	current := i.runtimeStateSnapshotLocked()
	if state.InstanceID != i.ID || state.Generation < current.Generation ||
		(state.Generation == current.Generation && state.StatusRevision < current.StatusRevision) ||
		(state.Generation == current.Generation && physicalRuntimeIdentityEstablished(current) &&
			!samePhysicalRuntime(current, state)) {
		return false
	}
	i.adoptRuntimeStateLocked(state)
	return true
}

// MergeReloaded keeps the canonical object identity used by UI commands while
// merging safe metadata from every snapshot. Lower-generation runtime state
// and bindings are ignored so they cannot be saved back later.
func (i *Instance) MergeReloaded(loaded *Instance) bool {
	return i.mergeReloaded(loaded, true, true)
}

// MergeReloadedRuntimeOnly applies only authoritative runtime state and
// bindings from a detached snapshot. Physical transitions use this while
// launch metadata is frozen on the canonical object.
func (i *Instance) MergeReloadedRuntimeOnly(loaded *Instance) bool {
	return i.mergeReloaded(loaded, false, true)
}

// MergeDeferredReload applies metadata that was read while a physical
// transition was in flight. The transition has completed before this method
// runs, so launch inputs may now advance, while transition-produced runtime,
// binding, container, and MCP state remain authoritative.
func (i *Instance) MergeDeferredReload(loaded *Instance) bool {
	return i.mergeReloaded(loaded, true, false)
}

func (i *Instance) mergeReloaded(loaded *Instance, mergeMetadata, mergeRuntime bool) bool {
	if loaded == nil || loaded.ID != i.ID {
		return false
	}
	if loaded == i {
		return true
	}
	loaded.mu.RLock()
	defer loaded.mu.RUnlock()
	incoming := loaded.runtimeStateSnapshotLocked()
	incomingIncarnation := loaded.persistenceIncarnation
	incomingStorageSnapshot := loaded.storageSnapshot
	runtimeReloadBeforePublishFn()
	i.mu.Lock()
	defer i.mu.Unlock()
	// An empty current token is the pre-migration/in-memory case and may adopt
	// the durable token on first reload. Once an object has a token, however,
	// an empty or different incoming token is a distinct insertion of the same
	// logical ID and must never be merged into this object.
	if i.persistenceIncarnation != "" && i.persistenceIncarnation != incomingIncarnation {
		return false
	}
	if i.persistenceIncarnation == "" && incomingIncarnation != "" {
		i.persistenceIncarnation = incomingIncarnation
	}
	current := i.runtimeStateSnapshotLocked()
	if mergeMetadata {
		currentSandboxContainer := i.SandboxContainer
		currentLoadedMCPNames := append([]string(nil), i.LoadedMCPNames...)
		i.mergeReloadedMetadataLocked(loaded)
		i.storageSnapshot = incomingStorageSnapshot
		if !mergeRuntime {
			i.SandboxContainer = currentSandboxContainer
			i.LoadedMCPNames = currentLoadedMCPNames
		}
	}
	if !mergeRuntime {
		return true
	}
	// A generation names one physical runtime. A detached equal-generation
	// snapshot can refresh status and bindings only when it names the same
	// runtime; otherwise it is a stale loser or predecessor.
	if incoming.Generation == current.Generation && physicalRuntimeIdentityEstablished(current) &&
		!samePhysicalRuntime(current, incoming) {
		return true
	}
	currentBindings := make(map[string]statedb.RuntimeBinding, len(i.RuntimeBindings))
	for kind, binding := range i.RuntimeBindings {
		currentBindings[kind] = binding
	}
	mergedBindings := mergeRuntimeBindingsForReload(
		current.Generation, incoming.Generation, currentBindings, loaded.RuntimeBindings)
	i.RuntimeBindings = make(map[string]statedb.RuntimeBinding, len(mergedBindings))
	for kind, binding := range mergedBindings {
		i.RuntimeBindings[kind] = binding
	}
	bindingValue := func(kind string) (string, time.Time) {
		binding, ok := mergedBindings[kind]
		if !ok {
			return "", time.Time{}
		}
		return binding.Value, binding.DetectedAt
	}
	i.ClaudeSessionID, i.ClaudeDetectedAt = bindingValue("claude")
	i.CopilotSessionID, i.CopilotDetectedAt = bindingValue("copilot")
	i.CodexSessionID, i.CodexDetectedAt = bindingValue("codex")
	i.GeminiSessionID, i.GeminiDetectedAt = bindingValue("gemini")
	i.OpenCodeSessionID, i.OpenCodeDetectedAt = bindingValue("opencode")
	if incoming.Generation < current.Generation ||
		(incoming.Generation == current.Generation && incoming.StatusRevision < current.StatusRevision) {
		return true
	}
	i.adoptRuntimeStateLocked(incoming)
	return true
}

func (i *Instance) mergeReloadedMetadataLocked(loaded *Instance) {
	i.Title = loaded.Title
	i.ProjectPath = loaded.ProjectPath
	i.GroupPath = loaded.GroupPath
	i.Order = loaded.Order
	i.Pin = loaded.Pin
	i.ParentSessionID = loaded.ParentSessionID
	i.ParentProjectPath = loaded.ParentProjectPath
	i.IsConductor = loaded.IsConductor
	i.NoTransitionNotify = loaded.NoTransitionNotify
	i.TitleLocked = loaded.TitleLocked
	i.AutoName = loaded.AutoName
	i.autoNameDescription = loaded.autoNameDescription
	i.WorktreePath = loaded.WorktreePath
	i.WorktreeRepoRoot = loaded.WorktreeRepoRoot
	i.WorktreeBranch = loaded.WorktreeBranch
	i.WorktreeType = loaded.WorktreeType
	i.Account = loaded.Account
	i.MultiRepoEnabled = loaded.MultiRepoEnabled
	i.AdditionalPaths = append([]string(nil), loaded.AdditionalPaths...)
	i.MultiRepoTempDir = loaded.MultiRepoTempDir
	i.MultiRepoWorktrees = append([]MultiRepoWorktree(nil), loaded.MultiRepoWorktrees...)
	i.Command = loaded.Command
	i.Wrapper = loaded.Wrapper
	i.Tool = loaded.Tool
	i.CreatedAt = loaded.CreatedAt
	i.LastAccessedAt = loaded.LastAccessedAt
	i.ArchivedAt = loaded.ArchivedAt
	i.GeminiYoloMode = loaded.GeminiYoloMode
	i.GeminiModel = loaded.GeminiModel
	i.CopilotModel = loaded.CopilotModel
	i.CopilotAllowAll = loaded.CopilotAllowAll
	i.LatestPrompt = loaded.LatestPrompt
	i.Notes = loaded.Notes
	i.Color = loaded.Color
	i.Sandbox = loaded.Sandbox
	i.SandboxContainer = loaded.SandboxContainer
	i.SSHHost = loaded.SSHHost
	i.SSHRemotePath = loaded.SSHRemotePath
	i.LoadedMCPNames = append([]string(nil), loaded.LoadedMCPNames...)
	i.Channels = append([]string(nil), loaded.Channels...)
	i.Plugins = append([]string(nil), loaded.Plugins...)
	i.PluginChannelLinkDisabled = loaded.PluginChannelLinkDisabled
	i.AutoLinkedChannels = append([]string(nil), loaded.AutoLinkedChannels...)
	i.IdleTimeoutSecs = loaded.IdleTimeoutSecs
	i.ExtraArgs = append([]string(nil), loaded.ExtraArgs...)
	i.ExitToShell = loaded.ExitToShell
	i.LaunchShell = loaded.LaunchShell
	i.ToolOptionsJSON = append([]byte(nil), loaded.ToolOptionsJSON...)
	if loaded.owningDB != nil {
		i.owningDB = loaded.owningDB
	}
}

func mergeRuntimeBindingsForReload(currentGeneration, incomingGeneration uint64, current, incoming map[string]statedb.RuntimeBinding) map[string]statedb.RuntimeBinding {
	merged := make(map[string]statedb.RuntimeBinding)
	if incomingGeneration <= currentGeneration {
		for kind, binding := range current {
			if binding.Generation == currentGeneration {
				merged[kind] = binding
			}
		}
	}
	if incomingGeneration < currentGeneration {
		return merged
	}
	for kind, binding := range incoming {
		if binding.Generation != incomingGeneration {
			continue
		}
		previous, found := merged[kind]
		if !found || binding.Revision > previous.Revision {
			merged[kind] = binding
		}
	}
	return merged
}

type RuntimeTransitionStage string

const (
	RuntimeTransitionAfterRespawnBeforeStamp       RuntimeTransitionStage = "after-respawn-before-stamp"
	RuntimeTransitionAfterStampBeforeCommit        RuntimeTransitionStage = "after-stamp-before-commit"
	RuntimeTransitionAfterCommitBeforeSweep        RuntimeTransitionStage = "after-commit-before-sweep"
	RuntimeDestructionAfterReserveBeforeTerminate  RuntimeTransitionStage = "after-destruction-reserve-before-terminate"
	RuntimeDestructionAfterTerminateBeforeComplete RuntimeTransitionStage = "after-destruction-terminate-before-complete"
)

var (
	runtimeTransitionObservedFn   = func() {}
	runtimeTransitionFaultFn      = func(RuntimeTransitionStage, statedb.RuntimeState) error { return nil }
	runtimeCandidateInventoryFn   = tmux.ListRuntimeCandidates
	runtimeCandidateSnapshotFn    = tmux.SnapshotRuntimeCandidates
	runtimeCandidateRevalidateFn  = tmux.RevalidateRuntimeCandidate
	runtimeCandidateExistsFn      = func(session *tmux.Session) bool { return session != nil && session.Exists() }
	runtimeCandidateStampFn       = stampRuntimeCandidate
	runtimeCandidateSetEnvFn      = func(session *tmux.Session, key, value string) error { return session.SetEnvironment(key, value) }
	runtimeCleanupIdentityStampFn = tmux.StampRuntimeCleanupIdentity
	runtimeCandidateRespawnFn     = tmux.RespawnRuntimeGenerationCandidate
	runtimeTransitionCommitFn     = func(db *statedb.StateDB, expected uint64, incarnation string, next statedb.RuntimeState, plan []statedb.RuntimeBindingTransition) error {
		return db.CommitRuntimeTransitionWithBindingPlan(expected, incarnation, next, plan)
	}
	runtimeDuplicateSweepFn = func(i *Instance, observedSockets ...string) { i.sweepDuplicateToolSessions(observedSockets...) }
)

var runtimeBindingKinds = []string{"claude", "copilot", "codex", "gemini", "opencode"}

type runtimeTransitionAuthority struct {
	expected         statedb.RuntimeState
	incarnation      string
	durable          bool
	db               *statedb.StateDB
	bindings         map[string]statedb.RuntimeBinding
	plan             []statedb.RuntimeBindingTransition
	fresh            bool
	freshSnapshot    freshRuntimeBindingSnapshot
	currentCandidate *tmux.RuntimeCandidate
	// predecessorTerminated is set by a composite transition that performs the
	// exact captured stop before entering restartWithTransition.
	predecessorTerminated bool
	release               func()
}

func (a *runtimeTransitionAuthority) close() {
	if a != nil && a.release != nil {
		a.release()
		a.release = nil
	}
}

// RuntimeReconciliationResult describes a read-only inventory or a completed
// recovery. Live is true only when one exact, generation-proved candidate is
// the durable winner. Ambiguous candidates are returned in the error instead.
type RuntimeReconciliationResult struct {
	State      statedb.RuntimeState
	Live       bool
	Adopted    bool
	Candidates []tmux.RuntimeCandidate
}

// StartupRuntimeReconciliation is one result from the socket-complete startup
// inventory. Results retain input order so the UI can apply them to the
// corresponding loaded instances before publishing its load message.
type StartupRuntimeReconciliation struct {
	InstanceID string
	Result     RuntimeReconciliationResult
	Err        error
}

// RuntimeReconciliationAmbiguityError preserves every candidate for operator
// triage. Reconciliation never kills or guesses when this error is returned.
type RuntimeReconciliationAmbiguityError struct {
	InstanceID string
	Reason     string
	Candidates []tmux.RuntimeCandidate
}

func (e *RuntimeReconciliationAmbiguityError) Error() string {
	details := make([]string, 0, len(e.Candidates))
	for _, candidate := range e.Candidates {
		generation := "unknown"
		if candidate.GenerationKnown {
			generation = strconv.FormatUint(candidate.Generation, 10)
		}
		proof := candidate.ProofError
		if proof == "" {
			proof = "none"
		}
		details = append(details, fmt.Sprintf(
			"socket=%q session=%q generation=%s pid=%d proof=%q",
			candidate.SocketName, candidate.SessionName, generation, candidate.PanePID, proof))
	}
	return fmt.Sprintf("runtime reconciliation for %s is ambiguous: %s (%d candidates preserved: %s)",
		e.InstanceID, e.Reason, len(e.Candidates), strings.Join(details, "; "))
}

// ReconcileRestartResult retries only the failed durability publication. It
// never stamps, starts, restarts, or kills the observed candidate.
func (i *Instance) ReconcileRestartResult(restartErr error) error {
	var partial *RestartPartialSuccessError
	if !errors.As(restartErr, &partial) {
		return restartErr
	}
	if !partial.NeedsReconciliation {
		result, err := i.ReconcileRuntime()
		if err == nil && result.Live {
			return nil
		}
		if err == nil {
			err = fmt.Errorf("no live durable runtime winner was proved")
		}
		return &RestartPartialSuccessError{
			InstanceID: partial.InstanceID, Runtime: partial.Runtime,
			BindingPlan:         append([]statedb.RuntimeBindingTransition(nil), partial.BindingPlan...),
			NeedsReconciliation: false,
			Err:                 errors.Join(partial.Err, fmt.Errorf("post-commit reconciliation failed: %w", err)),
		}
	}
	if err := i.reconcileRuntimeCandidate(partial.Runtime, partial.BindingPlan); err != nil {
		return &RestartPartialSuccessError{
			InstanceID: partial.InstanceID, Runtime: partial.Runtime,
			BindingPlan:         append([]statedb.RuntimeBindingTransition(nil), partial.BindingPlan...),
			NeedsReconciliation: true,
			Err:                 fmt.Errorf("%v; durability reconciliation failed: %w", partial.Err, err),
		}
	}
	return nil
}

func samePhysicalRuntime(left, right statedb.RuntimeState) bool {
	return left.InstanceID == right.InstanceID &&
		left.Generation == right.Generation &&
		left.TmuxSession == right.TmuxSession &&
		left.TmuxSocketName == right.TmuxSocketName
}

func physicalRuntimeIdentityEstablished(state statedb.RuntimeState) bool {
	return state.TmuxSession != "" || state.TmuxSocketName != ""
}

func (i *Instance) observeRuntimeState() (statedb.RuntimeState, bool, error) {
	db := i.restartPersistenceDB()
	if db == nil {
		return i.runtimeStateSnapshot(), false, nil
	}
	state, found, err := db.ReadRuntimeState(i.ID)
	if err != nil {
		return statedb.RuntimeState{}, false, err
	}
	// A reservation crossed the compatibility fence before it was published.
	// Let the lifecycle lock recover that exact tuple even if a legacy writer
	// registered afterward; fresh work remains fenced below.
	if found && statedb.IsRuntimeDestructionReserved(state) {
		return state, true, nil
	}
	if err := db.RequireRuntimeWriterCompatibility(); err != nil {
		return statedb.RuntimeState{}, false, err
	}
	return state, found, nil
}

// beginRuntimeTransition obtains physical-transition authority. A caller that
// observed an older generation before waiting on the lock is a lock loser: it
// receives winner and must return without changing any physical or durable
// state.
func (i *Instance) beginRuntimeTransition(fresh bool) (*runtimeTransitionAuthority, *statedb.RuntimeState, error) {
	observed, _, err := i.observeRuntimeState()
	if err != nil {
		return nil, nil, err
	}
	runtimeTransitionObservedFn()
	release, err := acquireInstanceSpawnLock(i.ID)
	if err != nil {
		return nil, nil, err
	}
	fail := func(err error) (*runtimeTransitionAuthority, *statedb.RuntimeState, error) {
		release()
		return nil, nil, err
	}

	incarnation := i.persistenceIncarnationSnapshot()
	current, durable, err := i.durableRuntimeStateWithIncarnation(incarnation)
	if err != nil {
		return fail(err)
	}
	db := i.restartPersistenceDB()
	if durable && db != nil {
		if err := db.ValidateRuntimeIncarnation(i.ID, incarnation); err != nil {
			return fail(err)
		}
	}
	requireBindingCompatibility := !(durable && statedb.IsRuntimeDestructionReserved(current))
	bindings, plan, err := i.readRuntimeBindingPlanWithCompatibility(
		db, current, durable, requireBindingCompatibility)
	if err != nil {
		return fail(err)
	}
	if current.Generation > observed.Generation {
		if durable {
			i.adoptRuntimeSnapshot(current, bindings)
		} else {
			i.adoptRuntimeState(current)
		}
		release()
		winner := current
		return nil, &winner, nil
	}
	var currentCandidate *tmux.RuntimeCandidate
	if durable {
		reconciled, reconcileErr := i.reconcileRuntimeLocked(db, current, incarnation)
		if reconcileErr != nil {
			return fail(reconcileErr)
		}
		if reconciled.Adopted {
			release()
			winner := reconciled.State
			return nil, &winner, nil
		}
		// Reconciliation may have recovered a crash-stranded destruction
		// reservation without advancing the generation. Continue from that exact
		// durable revision, never from the sentinel snapshot read above.
		current = reconciled.State
		bindings, plan, err = i.readRuntimeBindingPlan(db, current, true)
		if err != nil {
			return fail(err)
		}
		if reconciled.Live {
			currentCandidate = runtimeCandidateForState(reconciled.Candidates, reconciled.State)
			if currentCandidate == nil {
				return fail(fmt.Errorf("live runtime %s generation %d has no exact physical candidate",
					reconciled.State.TmuxSession, reconciled.State.Generation))
			}
		}
	} else {
		currentCandidate, err = i.selectNonDurableRuntimeCandidate(current)
		if err != nil {
			return fail(err)
		}
	}
	if durable && db != nil {
		// Reconciliation gets the first read-only decision: a still-live unstamped
		// legacy pane remains recoverable when it is rejected as ambiguous. Only a
		// successful modern transition authority retires the migration marker,
		// immediately before physical mutation can begin under this lifecycle lock.
		if err := db.SupersedeLegacyRuntimeAdoption(current, incarnation); err != nil {
			return fail(err)
		}
	}

	if durable {
		i.adoptRuntimeSnapshot(current, bindings)
	} else {
		// A non-durable snapshot has no authoritative binding rows to project.
		// Preserve local recovery markers such as an empty Claude id paired with
		// a non-zero detection time; command dispatch still needs that marker.
		i.adoptRuntimeState(current)
	}
	return &runtimeTransitionAuthority{
		expected:         current,
		incarnation:      incarnation,
		durable:          durable,
		db:               db,
		bindings:         bindings,
		plan:             plan,
		fresh:            fresh,
		freshSnapshot:    i.snapshotFreshRuntimeBindings(),
		currentCandidate: currentCandidate,
		release:          release,
	}, nil, nil
}

// selectNonDurableRuntimeCandidate proves the physical predecessor for local
// transitions that have no StateDB. The stamped tmux identity is still
// mandatory: local state is not permission to rediscover by mutable name or
// to choose among competing candidates.
func (i *Instance) selectNonDurableRuntimeCandidate(state statedb.RuntimeState) (*tmux.RuntimeCandidate, error) {
	candidates, err := i.inventoryRuntimeCandidates(state)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	if len(candidates) != 1 {
		return nil, i.runtimeAmbiguity("non-durable runtime has competing physical candidates", candidates)
	}
	selected := runtimeCandidateForState(candidates, state)
	if selected == nil || selected.PanePID <= 0 || selected.ProofError != "" {
		return nil, i.runtimeAmbiguity("non-durable runtime lacks an exact generation and process proof", candidates)
	}
	verified, verifyErr := runtimeCandidateRevalidateFn(*selected)
	if verifyErr != nil {
		return nil, i.runtimeAmbiguity("non-durable runtime candidate revalidation failed: "+verifyErr.Error(), candidates)
	}
	if !sameRuntimeCandidateSnapshot(*selected, verified) {
		observed := append(append([]tmux.RuntimeCandidate(nil), candidates...), verified)
		return nil, i.runtimeAmbiguity("non-durable runtime candidate changed during revalidation", observed)
	}
	return &verified, nil
}

func runtimeCandidateForState(candidates []tmux.RuntimeCandidate, state statedb.RuntimeState) *tmux.RuntimeCandidate {
	for index := range candidates {
		candidate := &candidates[index]
		if candidate.InstanceID == state.InstanceID &&
			candidate.GenerationKnown && candidate.Generation == state.Generation &&
			candidate.SessionName == state.TmuxSession && candidate.SocketName == state.TmuxSocketName {
			selected := *candidate
			return &selected
		}
	}
	return nil
}

func (i *Instance) durableRuntimeState() (statedb.RuntimeState, bool, error) {
	return i.durableRuntimeStateWithIncarnation(i.persistenceIncarnationSnapshot())
}

func (i *Instance) durableRuntimeStateWithIncarnation(incarnation string) (statedb.RuntimeState, bool, error) {
	db := i.restartPersistenceDB()
	if db == nil {
		return i.runtimeStateSnapshot(), false, nil
	}
	state, found, err := db.ReadRuntimeState(i.ID)
	if err != nil {
		return statedb.RuntimeState{}, false, err
	}
	if found && statedb.IsRuntimeDestructionReserved(state) {
		return state, true, nil
	}
	if err := db.RequireRuntimeWriterCompatibility(); err != nil {
		return statedb.RuntimeState{}, false, err
	}
	if !found {
		initial := i.runtimeStateSnapshot()
		i.mu.RLock()
		newUnsaved := i.addedThisProcess && i.owningDB == nil
		i.mu.RUnlock()
		if newUnsaved {
			state, err = db.EnsureRuntimeStateForNewInstance(initial, incarnation)
		} else {
			state, err = db.EnsureRuntimeState(initial, incarnation)
		}
		if err != nil {
			return statedb.RuntimeState{}, false, err
		}
		found = true
	}
	return state, found, nil
}

// adoptRuntimeState reconciles a lock loser or reload result to the durable
// winner without spawning, persisting, sweeping, or killing anything.
func (i *Instance) adoptRuntimeState(state statedb.RuntimeState) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.adoptRuntimeStateLocked(state)
}

func (i *Instance) adoptRuntimeStateLocked(state statedb.RuntimeState) {
	i.RuntimeGeneration = state.Generation
	i.StatusRevision = state.StatusRevision
	i.Status = Status(state.Status)
	i.LastStartedAt = state.LastStartedAt
	i.TmuxSocketName = state.TmuxSocketName
	if state.TmuxSession == "" {
		i.tmuxSession = nil
		return
	}
	if i.tmuxSession == nil || i.tmuxSession.Name != state.TmuxSession || i.tmuxSession.SocketName != state.TmuxSocketName {
		sess := tmux.ReconnectSessionLazy(state.TmuxSession, i.Title, i.EffectiveWorkingDir(), i.Command, statusToString(i.Status))
		sess.SocketName = state.TmuxSocketName
		sess.InstanceID = i.ID
		i.tmuxSession = sess
	}
}

func (i *Instance) adoptRuntimeSnapshot(state statedb.RuntimeState, bindings map[string]statedb.RuntimeBinding) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.adoptRuntimeStateLocked(state)
	i.adoptRuntimeBindingsLocked(bindings)
}

// reconcileReservedRuntimeBeforeCompatibilityLocked completes or restores an
// already-published destruction reservation before applying the writer fence.
// The caller must hold the instance lifecycle lock: reconciliation inventories
// and may mutate the exact physical runtime represented by state.
func (i *Instance) reconcileReservedRuntimeBeforeCompatibilityLocked(
	db *statedb.StateDB,
	state statedb.RuntimeState,
	found bool,
	incarnation string,
) (statedb.RuntimeState, bool, error) {
	if found && statedb.IsRuntimeDestructionReserved(state) {
		result, err := i.reconcileRuntimeLocked(db, state, incarnation)
		if err != nil {
			return statedb.RuntimeState{}, false, err
		}
		state = result.State
	}
	if err := db.RequireRuntimeWriterCompatibility(); err != nil {
		return statedb.RuntimeState{}, false, err
	}
	return state, found, nil
}

func (i *Instance) adoptDurableRuntime() error {
	release, err := acquireInstanceSpawnLock(i.ID)
	if err != nil {
		return err
	}
	defer release()
	return i.adoptDurableRuntimeLocked()
}

// adoptDurableRuntimeLocked projects the durable winner while the caller
// holds the instance lifecycle lock.
func (i *Instance) adoptDurableRuntimeLocked() error {
	incarnation := i.persistenceIncarnationSnapshot()
	db := i.restartPersistenceDB()
	if db == nil {
		return nil
	}
	state, found, err := i.durableRuntimeState()
	if err != nil {
		return err
	}
	state, found, err = i.reconcileReservedRuntimeBeforeCompatibilityLocked(db, state, found, incarnation)
	if err != nil {
		return err
	}
	if found {
		bindings, _, bindingErr := i.readRuntimeBindingPlan(db, state, true)
		if bindingErr != nil {
			return bindingErr
		}
		i.adoptRuntimeSnapshot(state, bindings)
	}
	return nil
}

func (i *Instance) readRuntimeBindingPlan(db *statedb.StateDB, state statedb.RuntimeState, durable bool) (map[string]statedb.RuntimeBinding, []statedb.RuntimeBindingTransition, error) {
	return i.readRuntimeBindingPlanWithCompatibility(db, state, durable, true)
}

func (i *Instance) readRuntimeBindingPlanWithCompatibility(
	db *statedb.StateDB,
	state statedb.RuntimeState,
	durable bool,
	requireWriterCompatibility bool,
) (map[string]statedb.RuntimeBinding, []statedb.RuntimeBindingTransition, error) {
	bindings := make(map[string]statedb.RuntimeBinding)
	if !durable {
		kind := activeRuntimeBindingKind(i)
		hadActiveBinding := false
		i.mu.RLock()
		for bindingKind, binding := range i.RuntimeBindings {
			if bindingKind == kind {
				hadActiveBinding = true
			}
			if binding.Generation == state.Generation {
				bindings[bindingKind] = binding
			}
		}
		i.mu.RUnlock()
		if _, found := bindings[kind]; kind != "" && !found && !hadActiveBinding {
			value, detectedAt := i.currentRuntimeBinding(kind)
			if value != "" {
				bindings[kind] = statedb.RuntimeBinding{
					InstanceID: i.ID, Kind: kind, Generation: state.Generation,
					Value: value, DetectedAt: detectedAt,
				}
			}
		}
	} else {
		if db == nil {
			return nil, nil, fmt.Errorf("runtime database unavailable")
		}
		if requireWriterCompatibility {
			if err := db.RequireRuntimeWriterCompatibility(); err != nil {
				return nil, nil, err
			}
		}
		for _, kind := range runtimeBindingKinds {
			binding, found, err := db.ReadRuntimeBinding(i.ID, kind)
			if err != nil {
				return nil, nil, err
			}
			if found && binding.Generation == state.Generation {
				bindings[kind] = binding
			}
		}
	}

	plan := make([]statedb.RuntimeBindingTransition, 0, len(bindings))
	for _, kind := range runtimeBindingKinds {
		binding, found := bindings[kind]
		if !found || binding.Generation != state.Generation {
			continue
		}
		plan = append(plan, statedb.RuntimeBindingTransition{
			Kind: kind, ExpectedRevision: binding.Revision,
			NextValue: binding.Value, DetectedAt: binding.DetectedAt,
		})
	}
	return bindings, plan, nil
}

func (i *Instance) adoptRuntimeBindings(bindings map[string]statedb.RuntimeBinding) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.adoptRuntimeBindingsLocked(bindings)
}

func (i *Instance) adoptRuntimeBindingsLocked(bindings map[string]statedb.RuntimeBinding) {
	i.RuntimeBindings = make(map[string]statedb.RuntimeBinding, len(bindings))
	for kind, binding := range bindings {
		i.RuntimeBindings[kind] = binding
	}
	set := func(kind string) (string, time.Time) {
		binding, ok := bindings[kind]
		if !ok {
			return "", time.Time{}
		}
		return binding.Value, binding.DetectedAt
	}
	i.ClaudeSessionID, i.ClaudeDetectedAt = set("claude")
	i.CopilotSessionID, i.CopilotDetectedAt = set("copilot")
	i.CodexSessionID, i.CodexDetectedAt = set("codex")
	i.GeminiSessionID, i.GeminiDetectedAt = set("gemini")
	i.OpenCodeSessionID, i.OpenCodeDetectedAt = set("opencode")
}

func activeRuntimeBindingKind(i *Instance) string {
	switch {
	case IsClaudeCompatible(i.Tool):
		return "claude"
	case i.Tool == "copilot":
		return "copilot"
	case IsCodexCompatible(i.Tool):
		return "codex"
	case i.Tool == "gemini":
		return "gemini"
	case i.Tool == "opencode":
		return "opencode"
	default:
		return ""
	}
}

func (i *Instance) currentRuntimeBinding(kind string) (string, time.Time) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	switch kind {
	case "claude":
		return i.ClaudeSessionID, i.ClaudeDetectedAt
	case "copilot":
		return i.CopilotSessionID, i.CopilotDetectedAt
	case "codex":
		return i.CodexSessionID, i.CodexDetectedAt
	case "gemini":
		return i.GeminiSessionID, i.GeminiDetectedAt
	case "opencode":
		return i.OpenCodeSessionID, i.OpenCodeDetectedAt
	default:
		return "", time.Time{}
	}
}

func (a *runtimeTransitionAuthority) bindingPlanForCommit(i *Instance) []statedb.RuntimeBindingTransition {
	planned := make(map[string]statedb.RuntimeBindingTransition, len(a.plan))
	for _, item := range a.plan {
		planned[item.Kind] = item
	}
	activeKind := activeRuntimeBindingKind(i)
	activeValue, activeDetectedAt := i.currentRuntimeBinding(activeKind)
	plan := make([]statedb.RuntimeBindingTransition, 0, len(runtimeBindingKinds))
	for _, kind := range runtimeBindingKinds {
		item := planned[kind]
		item.Kind = kind
		item.NextValue = ""
		item.DetectedAt = time.Time{}
		if kind == activeKind {
			item.NextValue = activeValue
			item.DetectedAt = activeDetectedAt
		}
		plan = append(plan, item)
	}
	return plan
}

type freshRuntimeBindingSnapshot struct {
	bindings                                           map[string]statedb.RuntimeBinding
	claude, copilot, codex, gemini, openCode           string
	claudeAt, copilotAt, codexAt, geminiAt, openCodeAt time.Time
	openCodeStarted, codexStarted, copilotStarted      int64
	lastOpenCodeScan, lastCodexScan                    time.Time
	pendingCodexWarning, hermes                        string
}

func (i *Instance) snapshotFreshRuntimeBindings() freshRuntimeBindingSnapshot {
	i.mu.RLock()
	defer i.mu.RUnlock()
	snapshot := freshRuntimeBindingSnapshot{
		bindings: make(map[string]statedb.RuntimeBinding, len(i.RuntimeBindings)),
		claude:   i.ClaudeSessionID, claudeAt: i.ClaudeDetectedAt,
		copilot: i.CopilotSessionID, copilotAt: i.CopilotDetectedAt,
		codex: i.CodexSessionID, codexAt: i.CodexDetectedAt,
		gemini: i.GeminiSessionID, geminiAt: i.GeminiDetectedAt,
		openCode: i.OpenCodeSessionID, openCodeAt: i.OpenCodeDetectedAt,
		openCodeStarted: i.OpenCodeStartedAt, codexStarted: i.CodexStartedAt,
		copilotStarted: i.CopilotStartedAt, lastOpenCodeScan: i.lastOpenCodeScanAt,
		lastCodexScan: i.lastCodexScanAt, pendingCodexWarning: i.pendingCodexRestartWarning,
		hermes: i.HermesSessionID,
	}
	for kind, binding := range i.RuntimeBindings {
		snapshot.bindings[kind] = binding
	}
	return snapshot
}

func (a *runtimeTransitionAuthority) prepareFresh(i *Instance) {
	if a != nil && a.fresh {
		i.clearSessionBindingForFreshStart()
	}
}

func (a *runtimeTransitionAuthority) restoreFresh(i *Instance) {
	if a == nil || !a.fresh {
		return
	}
	s := a.freshSnapshot
	i.mu.Lock()
	i.RuntimeBindings = make(map[string]statedb.RuntimeBinding, len(s.bindings))
	for kind, binding := range s.bindings {
		i.RuntimeBindings[kind] = binding
	}
	i.ClaudeSessionID, i.ClaudeDetectedAt = s.claude, s.claudeAt
	i.CopilotSessionID, i.CopilotDetectedAt = s.copilot, s.copilotAt
	i.CodexSessionID, i.CodexDetectedAt = s.codex, s.codexAt
	i.GeminiSessionID, i.GeminiDetectedAt = s.gemini, s.geminiAt
	i.OpenCodeSessionID, i.OpenCodeDetectedAt = s.openCode, s.openCodeAt
	i.OpenCodeStartedAt, i.CodexStartedAt, i.CopilotStartedAt = s.openCodeStarted, s.codexStarted, s.copilotStarted
	i.lastOpenCodeScanAt, i.lastCodexScanAt = s.lastOpenCodeScan, s.lastCodexScan
	i.pendingCodexRestartWarning, i.HermesSessionID = s.pendingCodexWarning, s.hermes
	i.mu.Unlock()
}

func stampRuntimeCandidate(session *tmux.Session, next statedb.RuntimeState, bindingKind, bindingValue string) error {
	if session == nil {
		return fmt.Errorf("tmux session not initialized")
	}
	stamps := [][2]string{
		{"AGENTDECK_INSTANCE_ID", next.InstanceID},
		{"AGENTDECK_RUNTIME_STATUS_REVISION", strconv.FormatUint(next.StatusRevision, 10)},
		{"AGENTDECK_RUNTIME_STATUS", next.Status},
		{"AGENTDECK_RUNTIME_STARTED_UNIX_NANO", strconv.FormatInt(next.LastStartedAt.UnixNano(), 10)},
		{"AGENTDECK_RUNTIME_BINDING_KIND", bindingKind},
		{"AGENTDECK_RUNTIME_BINDING_VALUE", bindingValue},
	}
	for _, stamp := range stamps {
		if err := runtimeCandidateSetEnvFn(session, stamp[0], stamp[1]); err != nil {
			return fmt.Errorf("stamp %s: %w", stamp[0], err)
		}
	}
	bindingKey := runtimeBindingEnvironmentKey(bindingKind)
	if err := runtimeCleanupIdentityStampFn(session, next.InstanceID, next.Generation, bindingKey, bindingValue); err != nil {
		return fmt.Errorf("stamp runtime cleanup identity: %w", err)
	}
	// Generation is the final environment completeness marker. Reconciliation
	// treats a candidate without it as ambiguous and preserves it.
	if err := runtimeCandidateSetEnvFn(session, "AGENTDECK_RUNTIME_GENERATION", strconv.FormatUint(next.Generation, 10)); err != nil {
		return fmt.Errorf("stamp AGENTDECK_RUNTIME_GENERATION: %w", err)
	}
	return nil
}

func (i *Instance) respawnRuntimePane(authority *runtimeTransitionAuthority, command string) error {
	if authority == nil || authority.currentCandidate == nil || authority.expected.InstanceID != i.ID {
		return fmt.Errorf("runtime respawn candidate unavailable: %w", statedb.ErrRuntimeGenerationConflict)
	}
	selected := *authority.currentCandidate
	verified, err := runtimeCandidateRevalidateFn(selected)
	if err != nil {
		return i.runtimeAmbiguity("runtime respawn candidate revalidation failed: "+err.Error(), []tmux.RuntimeCandidate{selected})
	}
	if !sameRuntimeCandidateSnapshot(selected, verified) {
		return i.runtimeAmbiguity("runtime respawn candidate changed before mutation", []tmux.RuntimeCandidate{selected, verified})
	}
	candidate := tmux.RuntimeGenerationCandidate{
		SessionName: verified.SessionName, SessionID: verified.SessionID,
		SocketName: verified.SocketName, PaneID: verified.PaneID, PanePID: verified.PanePID,
		InstanceID: verified.InstanceID, Generation: verified.Generation,
		GenerationKnown: verified.GenerationKnown,
	}
	if err := runtimeCandidateRespawnFn(i.tmuxSession, candidate, command); err != nil {
		return err
	}
	if err := runtimeTransitionFaultFn(RuntimeTransitionAfterRespawnBeforeStamp, authority.expected); err != nil {
		return err
	}
	return nil
}

// commitPhysicalRuntime is the only publication point after a successful
// physical start/restart. committed distinguishes a pre-CAS candidate from a
// post-CAS cleanup interruption; either error is a completed physical spawn.
func (i *Instance) commitPhysicalRuntime(authority *runtimeTransitionAuthority) (next statedb.RuntimeState, plan []statedb.RuntimeBindingTransition, committed bool, err error) {
	if authority == nil {
		return next, nil, false, fmt.Errorf("runtime transition authority unavailable")
	}
	next = i.runtimeStateSnapshot()
	next.Generation = authority.expected.Generation + 1
	next.StatusRevision = 0
	next.LastStartedAt = nowFn().UTC()
	if i.tmuxSession != nil {
		next.TmuxSession = i.tmuxSession.Name
		next.TmuxSocketName = i.tmuxSession.SocketName
	}
	if next.TmuxSession == "" {
		return next, nil, false, fmt.Errorf("physical runtime for %s has no tmux identity", i.ID)
	}
	if !runtimeCandidateExistsFn(i.tmuxSession) {
		return next, nil, false, fmt.Errorf("physical runtime %s was not verified live", next.TmuxSession)
	}
	plan = authority.bindingPlanForCommit(i)
	bindingKind := activeRuntimeBindingKind(i)
	bindingValue, _ := i.currentRuntimeBinding(bindingKind)
	if err := runtimeCandidateStampFn(i.tmuxSession, next, bindingKind, bindingValue); err != nil {
		return next, plan, false, fmt.Errorf("stamp runtime generation %d: %w", next.Generation, err)
	}
	if err := runtimeTransitionFaultFn(RuntimeTransitionAfterStampBeforeCommit, next); err != nil {
		return next, plan, false, err
	}
	if authority.durable {
		if authority.db == nil {
			return next, plan, false, fmt.Errorf("runtime database unavailable")
		}
		if err := authority.db.RequireRuntimeWriterCompatibility(); err != nil {
			return next, plan, false, err
		}
		if err := runtimeTransitionCommitFn(authority.db, authority.expected.Generation, authority.incarnation, next, plan); err != nil {
			if !errors.Is(err, statedb.ErrInstanceParentConflict) {
				winner, found, readErr := authority.db.ReadRuntimeState(i.ID)
				if readErr == nil && found && winner.Generation >= next.Generation {
					if bindings, _, bindingErr := i.readRuntimeBindingPlan(authority.db, winner, true); bindingErr == nil {
						i.adoptRuntimeSnapshot(winner, bindings)
					}
				}
			}
			return next, plan, false, err
		}
		bindings, _, readErr := i.readRuntimeBindingPlan(authority.db, next, true)
		if readErr != nil {
			return next, plan, true, readErr
		}
		i.adoptRuntimeSnapshot(next, bindings)
	} else {
		i.applyLocalRuntimeTransition(next, plan)
	}
	recordInstanceSpawn(i.ID)
	if err := runtimeTransitionFaultFn(RuntimeTransitionAfterCommitBeforeSweep, next); err != nil {
		return next, plan, true, err
	}
	return next, plan, true, nil
}

func (i *Instance) applyLocalRuntimeTransition(state statedb.RuntimeState, plan []statedb.RuntimeBindingTransition) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.adoptRuntimeStateLocked(state)
	if i.RuntimeBindings == nil {
		i.RuntimeBindings = make(map[string]statedb.RuntimeBinding)
	}
	for _, transition := range plan {
		if transition.NextValue == "" {
			if _, found := i.RuntimeBindings[transition.Kind]; !found {
				continue
			}
		}
		i.RuntimeBindings[transition.Kind] = statedb.RuntimeBinding{
			InstanceID: i.ID, Kind: transition.Kind, Generation: state.Generation,
			Revision: transition.ExpectedRevision + 1, Value: transition.NextValue,
			DetectedAt: transition.DetectedAt,
		}
	}
}

// ReconcileRuntime inventories exact logical-instance candidates under the
// same cross-process lock used by transitions. It adopts one uniquely proved
// pre-CAS candidate, or retains the durable post-CAS winner and permits only
// the existing lower-generation sweep. Ambiguity is always preserved.
func (i *Instance) ReconcileRuntime() (RuntimeReconciliationResult, error) {
	release, err := acquireInstanceSpawnLock(i.ID)
	if err != nil {
		return RuntimeReconciliationResult{}, err
	}
	defer release()
	incarnation := i.persistenceIncarnationSnapshot()

	db := i.restartPersistenceDB()
	if db == nil {
		return RuntimeReconciliationResult{State: i.runtimeStateSnapshot()}, nil
	}
	if err := db.ValidateInstanceIncarnation(i.ID, incarnation); err != nil {
		return RuntimeReconciliationResult{}, err
	}
	durable, found, err := db.ReadRuntimeState(i.ID)
	if err != nil {
		return RuntimeReconciliationResult{}, err
	}
	if !found {
		return RuntimeReconciliationResult{}, nil
	}
	if !statedb.IsRuntimeDestructionReserved(durable) {
		if err := db.RequireRuntimeWriterCompatibility(); err != nil {
			return RuntimeReconciliationResult{}, err
		}
	}
	return i.reconcileRuntimeLocked(db, durable, incarnation)
}

func (i *Instance) reconcileRuntimeLocked(db *statedb.StateDB, durable statedb.RuntimeState, incarnation string) (RuntimeReconciliationResult, error) {
	candidates, err := i.inventoryRuntimeCandidates(durable)
	if err != nil {
		return RuntimeReconciliationResult{}, err
	}
	return i.reconcileRuntimeCandidatesLocked(db, durable, incarnation, candidates, true)
}

// ReconcileRuntimeFromSnapshot preserves the normal per-instance transition
// lock and database CAS. The asynchronous socket-complete snapshot is a
// preflight only: a complete inventory is refreshed under the lock before any
// Live or adoption decision, then the selected immutable tmux identity is
// re-probed.
func (i *Instance) ReconcileRuntimeFromSnapshot(snapshot tmux.RuntimeCandidateSnapshot) (RuntimeReconciliationResult, error) {
	release, err := acquireInstanceSpawnLock(i.ID)
	if err != nil {
		return RuntimeReconciliationResult{}, err
	}
	defer release()
	incarnation := i.persistenceIncarnationSnapshot()

	db := i.restartPersistenceDB()
	if db == nil {
		return RuntimeReconciliationResult{State: i.runtimeStateSnapshot()}, nil
	}
	if err := db.ValidateInstanceIncarnation(i.ID, incarnation); err != nil {
		return RuntimeReconciliationResult{}, err
	}
	durable, found, err := db.ReadRuntimeState(i.ID)
	if err != nil {
		return RuntimeReconciliationResult{}, err
	}
	if !found {
		return RuntimeReconciliationResult{}, nil
	}
	if !statedb.IsRuntimeDestructionReserved(durable) {
		if err := db.RequireRuntimeWriterCompatibility(); err != nil {
			return RuntimeReconciliationResult{}, err
		}
	}
	if _, err := snapshot.Candidates(i.ID, i.runtimeCandidateSocketNames(durable)...); err != nil {
		return RuntimeReconciliationResult{}, err
	}
	candidates, err := i.inventoryRuntimeCandidates(durable)
	if err != nil {
		return RuntimeReconciliationResult{}, err
	}
	return i.reconcileRuntimeCandidatesLocked(db, durable, incarnation, candidates, true)
}

func (i *Instance) reconcileRuntimeCandidatesLocked(db *statedb.StateDB, durable statedb.RuntimeState, incarnation string, candidates []tmux.RuntimeCandidate, revalidateSelected bool) (RuntimeReconciliationResult, error) {
	result := RuntimeReconciliationResult{State: durable, Candidates: candidates}
	if len(candidates) == 0 {
		if statedb.IsRuntimeDestructionReserved(durable) {
			completed, err := db.CompleteRuntimeDestruction(durable, incarnation, string(StatusStopped))
			if err != nil {
				return result, err
			}
			result.State = completed
			i.adoptRuntimeState(completed)
			bindings, _, err := i.readRuntimeBindingPlanWithCompatibility(db, completed, true, false)
			if err != nil {
				return result, err
			}
			i.adoptRuntimeSnapshot(completed, bindings)
			return result, nil
		}
		i.adoptRuntimeState(durable)
		return result, nil
	}

	var current *tmux.RuntimeCandidate
	var next *tmux.RuntimeCandidate
	var lower []tmux.RuntimeCandidate
	for idx := range candidates {
		candidate := &candidates[idx]
		if !candidate.GenerationKnown || candidate.PanePID <= 0 || candidate.ProofError != "" {
			return result, i.runtimeAmbiguity("candidate lacks exact generation or process proof", candidates)
		}
		switch {
		case candidate.Generation < durable.Generation:
			lower = append(lower, *candidate)
		case candidate.Generation == durable.Generation:
			if candidate.SessionName != durable.TmuxSession || candidate.SocketName != durable.TmuxSocketName || current != nil {
				return result, i.runtimeAmbiguity("same-generation identity conflict", candidates)
			}
			current = candidate
		case candidate.Generation == durable.Generation+1:
			if next != nil {
				return result, i.runtimeAmbiguity("multiple pre-commit candidates", candidates)
			}
			next = candidate
		default:
			return result, i.runtimeAmbiguity("candidate generation is newer than the next admissible generation", candidates)
		}
	}

	selected := next
	if selected == nil {
		selected = current
	}
	if revalidateSelected && selected != nil {
		verified, verifyErr := runtimeCandidateRevalidateFn(*selected)
		if verifyErr != nil {
			return result, i.runtimeAmbiguity("selected runtime candidate revalidation failed: "+verifyErr.Error(), candidates)
		}
		if !sameRuntimeCandidateSnapshot(*selected, verified) {
			observed := append(append([]tmux.RuntimeCandidate(nil), candidates...), verified)
			return result, i.runtimeAmbiguity("selected runtime candidate changed during revalidation", observed)
		}
		*selected = verified
	}

	if statedb.IsRuntimeDestructionReserved(durable) {
		// A crash did not persist whether teardown had started or whether the
		// caller meant to stop or delete. Recover only the two states proved by a
		// complete inventory: the exact runtime is still live, or no runtime
		// remains. Every competing observation keeps the sentinel frozen.
		if current == nil || next != nil || len(lower) != 0 || len(candidates) != 1 {
			return result, i.runtimeAmbiguity("reserved runtime has competing or non-current candidates", candidates)
		}
		completed, err := db.RestoreRuntimeDestruction(durable, incarnation)
		if err != nil {
			return result, err
		}
		result.State = completed
		i.adoptRuntimeState(completed)
		bindings, _, err := i.readRuntimeBindingPlanWithCompatibility(db, completed, true, false)
		if err != nil {
			return result, err
		}
		i.adoptRuntimeSnapshot(completed, bindings)
		result.Live = true
		return result, nil
	}

	if next != nil {
		if !next.StateKnown {
			return result, i.runtimeAmbiguity("pre-commit candidate lacks a complete runtime tuple", candidates)
		}
		activeKind := activeRuntimeBindingKind(i)
		if !next.BindingKnown || next.BindingKind != activeKind {
			return result, i.runtimeAmbiguity("pre-commit candidate lacks its explicit binding decision", candidates)
		}
		bindings, plan, err := i.readRuntimeBindingPlan(db, durable, true)
		if err != nil {
			return result, err
		}
		_ = bindings
		plan = applyCandidateBindingDecision(plan, *next)
		adopted := statedb.RuntimeState{
			InstanceID: i.ID, Generation: next.Generation,
			StatusRevision: next.StatusRevision, TmuxSession: next.SessionName,
			TmuxSocketName: next.SocketName, Status: next.Status,
			LastStartedAt: time.Unix(0, next.LastStartedUnixNano).UTC(),
		}
		if err := runtimeTransitionCommitFn(db, durable.Generation, incarnation, adopted, plan); err != nil {
			if !errors.Is(err, statedb.ErrInstanceParentConflict) {
				winner, winnerFound, readErr := db.ReadRuntimeState(i.ID)
				if readErr == nil && winnerFound && winner.Generation >= adopted.Generation {
					i.adoptRuntimeState(winner)
				}
			}
			return result, err
		}
		durable = adopted
		result.State, result.Adopted = adopted, true
		current = next
	}

	if current == nil {
		return result, i.runtimeAmbiguity("durable winner is not live", candidates)
	}
	bindings, _, err := i.readRuntimeBindingPlan(db, durable, true)
	if err != nil {
		return result, err
	}
	i.adoptRuntimeSnapshot(durable, bindings)
	for _, candidate := range lower {
		generation := tmux.RuntimeGenerationCandidate{
			SessionName: candidate.SessionName, SessionID: candidate.SessionID,
			SocketName: candidate.SocketName, PaneID: candidate.PaneID, PanePID: candidate.PanePID,
			InstanceID: candidate.InstanceID, Generation: candidate.Generation,
			GenerationKnown: candidate.GenerationKnown,
		}
		if err := killRuntimeGenerationCandidateFn(generation, false); err != nil {
			return result, fmt.Errorf("clean lower runtime generation %d on socket %q session %q: %w",
				candidate.Generation, candidate.SocketName, candidate.SessionName, err)
		}
	}
	result.Live = true
	if result.Adopted || len(lower) > 0 {
		recordInstanceSpawn(i.ID)
		runtimeDuplicateSweepFn(i, runtimeCandidateObservedSocketNames(candidates)...)
	}
	return result, nil
}

func sameRuntimeCandidateSnapshot(snapshot, verified tmux.RuntimeCandidate) bool {
	return snapshot.SessionID == verified.SessionID &&
		snapshot.SessionName == verified.SessionName &&
		snapshot.SocketName == verified.SocketName &&
		snapshot.PaneID == verified.PaneID &&
		snapshot.InstanceID == verified.InstanceID &&
		snapshot.Generation == verified.Generation &&
		snapshot.GenerationKnown == verified.GenerationKnown &&
		snapshot.StatusRevision == verified.StatusRevision &&
		snapshot.Status == verified.Status &&
		snapshot.LastStartedUnixNano == verified.LastStartedUnixNano &&
		snapshot.StateKnown == verified.StateKnown &&
		snapshot.BindingKind == verified.BindingKind &&
		snapshot.BindingValue == verified.BindingValue &&
		(!snapshot.BindingKnown || verified.BindingKnown) &&
		snapshot.PanePID == verified.PanePID &&
		snapshot.ProofError == verified.ProofError
}

func (i *Instance) runtimeAmbiguity(reason string, candidates []tmux.RuntimeCandidate) error {
	return &RuntimeReconciliationAmbiguityError{
		InstanceID: i.ID, Reason: reason,
		Candidates: append([]tmux.RuntimeCandidate(nil), candidates...),
	}
}

func (i *Instance) inventoryRuntimeCandidates(durable statedb.RuntimeState) ([]tmux.RuntimeCandidate, error) {
	sockets := i.runtimeCandidateSocketNames(durable)
	seenSocket := make(map[string]bool)
	seenCandidate := make(map[string]bool)
	var candidates []tmux.RuntimeCandidate
	for _, socketName := range sockets {
		if seenSocket[socketName] {
			continue
		}
		seenSocket[socketName] = true
		found, err := runtimeCandidateInventoryFn(socketName, i.ID)
		if err != nil {
			return nil, err
		}
		for _, candidate := range found {
			key := candidate.SocketName + "\x00" + candidate.SessionID + "\x00" + candidate.PaneID + "\x00" + candidate.SessionName
			if seenCandidate[key] {
				continue
			}
			seenCandidate[key] = true
			candidates = append(candidates, candidate)
		}
	}
	return candidates, nil
}

func (i *Instance) runtimeCandidateSocketNames(durable statedb.RuntimeState) []string {
	// The empty string is tmux's native default socket, not an alias for the
	// currently configured Agent Deck socket. Always inventory it explicitly so
	// a later configuration change cannot hide a captured legacy runtime.
	return []string{durable.TmuxSocketName, i.TmuxSocketName, tmux.DefaultSocketName(), ""}
}

func runtimeCandidateObservedSocketNames(candidates []tmux.RuntimeCandidate) []string {
	sockets := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		sockets = append(sockets, candidate.SocketName)
	}
	return sockets
}

// ReconcileStartupRuntimes builds one all-runtime inventory per distinct
// socket, then reconciles each instance through its normal lock and CAS.
func ReconcileStartupRuntimes(instances []*Instance) []StartupRuntimeReconciliation {
	if len(instances) == 0 {
		return nil
	}
	seenSocket := make(map[string]bool)
	var sockets []string
	for _, instance := range instances {
		if instance == nil {
			continue
		}
		for _, socketName := range instance.runtimeCandidateSocketNames(instance.runtimeStateSnapshot()) {
			if seenSocket[socketName] {
				continue
			}
			seenSocket[socketName] = true
			sockets = append(sockets, socketName)
		}
	}
	snapshot := runtimeCandidateSnapshotFn(sockets)
	results := make([]StartupRuntimeReconciliation, 0, len(instances))
	for _, instance := range instances {
		if instance == nil {
			continue
		}
		result, err := instance.ReconcileRuntimeFromSnapshot(snapshot)
		results = append(results, StartupRuntimeReconciliation{InstanceID: instance.ID, Result: result, Err: err})
	}
	return results
}

func applyCandidateBindingDecision(plan []statedb.RuntimeBindingTransition, candidate tmux.RuntimeCandidate) []statedb.RuntimeBindingTransition {
	planned := make(map[string]statedb.RuntimeBindingTransition, len(plan))
	for _, item := range plan {
		planned[item.Kind] = item
	}
	explicit := make([]statedb.RuntimeBindingTransition, 0, len(runtimeBindingKinds))
	for _, kind := range runtimeBindingKinds {
		item := planned[kind]
		item.Kind = kind
		item.NextValue = ""
		item.DetectedAt = time.Time{}
		if kind == candidate.BindingKind {
			item.NextValue = candidate.BindingValue
		}
		explicit = append(explicit, item)
	}
	return explicit
}

func (i *Instance) reconcileRuntimeCandidate(candidate statedb.RuntimeState, plan []statedb.RuntimeBindingTransition) error {
	if candidate.InstanceID != i.ID || candidate.Generation == 0 || candidate.TmuxSession == "" {
		return fmt.Errorf("invalid runtime candidate for %s", i.ID)
	}
	release, err := acquireInstanceSpawnLock(i.ID)
	if err != nil {
		return err
	}
	defer release()
	incarnation := i.persistenceIncarnationSnapshot()
	db := i.restartPersistenceDB()
	if db == nil {
		return fmt.Errorf("runtime database unavailable")
	}
	if err := db.ValidateInstanceIncarnation(i.ID, incarnation); err != nil {
		return err
	}
	durable, found, err := db.ReadRuntimeState(i.ID)
	if err != nil {
		return err
	}
	durable, found, err = i.reconcileReservedRuntimeBeforeCompatibilityLocked(db, durable, found, incarnation)
	if err != nil {
		return err
	}
	if found && samePhysicalRuntime(durable, candidate) {
		result, reconcileErr := i.reconcileRuntimeLocked(db, durable, incarnation)
		if reconcileErr != nil {
			return reconcileErr
		}
		if !result.Live {
			return i.runtimeAmbiguity("durable partial-success winner is not uniquely proved live", result.Candidates)
		}
		return nil
	}
	if !found || durable.Generation+1 != candidate.Generation {
		if found && durable.Generation >= candidate.Generation {
			i.adoptRuntimeState(durable)
		}
		return statedb.ErrRuntimeGenerationConflict
	}
	candidates, err := i.inventoryRuntimeCandidates(durable)
	if err != nil {
		return err
	}
	if err := i.validatePartialRuntimeCandidate(durable, candidate, plan, candidates); err != nil {
		return err
	}
	if err := runtimeTransitionCommitFn(db, durable.Generation, incarnation, candidate, plan); err != nil {
		if !errors.Is(err, statedb.ErrInstanceParentConflict) {
			winner, winnerFound, readErr := db.ReadRuntimeState(i.ID)
			if readErr == nil && winnerFound && winner.Generation >= candidate.Generation {
				i.adoptRuntimeState(winner)
			}
		}
		return err
	}
	bindings, _, err := i.readRuntimeBindingPlan(db, candidate, true)
	if err != nil {
		return err
	}
	i.adoptRuntimeSnapshot(candidate, bindings)
	recordInstanceSpawn(i.ID)
	runtimeDuplicateSweepFn(i, runtimeCandidateObservedSocketNames(candidates)...)
	return nil
}

func (i *Instance) validatePartialRuntimeCandidate(durable, expected statedb.RuntimeState, plan []statedb.RuntimeBindingTransition, candidates []tmux.RuntimeCandidate) error {
	var currentCount int
	var selected *tmux.RuntimeCandidate
	for index := range candidates {
		candidate := &candidates[index]
		if candidate.InstanceID != i.ID || !candidate.GenerationKnown || candidate.PanePID <= 0 || candidate.ProofError != "" {
			return i.runtimeAmbiguity("partial-success inventory contains incomplete identity or process proof", candidates)
		}
		switch {
		case candidate.Generation < durable.Generation:
			continue
		case candidate.Generation == durable.Generation:
			currentCount++
			if currentCount > 1 || candidate.SessionName != durable.TmuxSession || candidate.SocketName != durable.TmuxSocketName {
				return i.runtimeAmbiguity("partial-success inventory contains a same-generation identity conflict", candidates)
			}
		case candidate.Generation == durable.Generation+1:
			if selected != nil || candidate.SessionName != expected.TmuxSession || candidate.SocketName != expected.TmuxSocketName {
				return i.runtimeAmbiguity("partial-success candidate is not uniquely identified", candidates)
			}
			selected = candidate
		default:
			return i.runtimeAmbiguity("partial-success inventory contains a newer inadmissible generation", candidates)
		}
	}
	if selected == nil {
		return i.runtimeAmbiguity("partial-success candidate is not uniquely proved live", candidates)
	}
	verified, err := runtimeCandidateRevalidateFn(*selected)
	if err != nil {
		return i.runtimeAmbiguity("partial-success candidate revalidation failed: "+err.Error(), candidates)
	}
	if !sameRuntimeCandidateSnapshot(*selected, verified) {
		observed := append(append([]tmux.RuntimeCandidate(nil), candidates...), verified)
		return i.runtimeAmbiguity("partial-success candidate changed during revalidation", observed)
	}
	if !selected.StateKnown || selected.StatusRevision != expected.StatusRevision ||
		selected.Status != expected.Status || selected.LastStartedUnixNano != expected.LastStartedAt.UnixNano() {
		return i.runtimeAmbiguity("partial-success candidate lacks the exact complete runtime tuple", candidates)
	}
	activeKind := activeRuntimeBindingKind(i)
	expectedBinding, bindingKnown := "", false
	for _, transition := range plan {
		if transition.Kind == activeKind {
			expectedBinding, bindingKnown = transition.NextValue, true
			break
		}
	}
	if !bindingKnown && activeKind == "" {
		bindingKnown = true
	}
	if !bindingKnown || !selected.BindingKnown || selected.BindingKind != activeKind || selected.BindingValue != expectedBinding {
		return i.runtimeAmbiguity("partial-success candidate lacks its exact binding decision", candidates)
	}
	return nil
}
