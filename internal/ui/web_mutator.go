package ui

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/git"
	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/asheshgoplani/agent-deck/internal/vcs"
	"github.com/asheshgoplani/agent-deck/internal/vcsbackend"
	"github.com/asheshgoplani/agent-deck/internal/web"
)

// Compile-time check: WebMutator must implement web.SessionMutator.
var _ web.SessionMutator = (*WebMutator)(nil)

// WebMutator bridges the web HTTP handlers to the TUI session/group management
// methods. It wraps the Home model and implements web.SessionMutator.
//
// The undoStack/undoWindow fields support the web's Chrome-style undo of
// deletes (POST /api/sessions/undelete). The TUI maintains its own
// in-memory stack in Home; the web stack is kept here so that web
// deletes/undos don't race with the Tea Update goroutine.
type WebMutator struct {
	h                  *Home
	startRuntimeFn     func(*session.Instance) (statedb.RuntimeState, error)
	restartRuntimeFn   func(*session.Instance) (statedb.RuntimeState, error)
	reconcileRestartFn func(*session.Instance, error) error
	archiveAfterKillFn func(statedb.RuntimeState)
	beforeUndoInsertFn func()
	requestReloadFn    func()

	undoMu     sync.Mutex
	undoStack  []webDeletedEntry
	undoWindow time.Duration

	// headlessTxMu serializes the full hydrate -> mutate -> persist transaction
	// in headless (`web --no-tui`) mode (#1397). Without it, two concurrent HTTP
	// handlers could each hydrate (replacing h.instances/instanceByID/groupTree),
	// mutate a now-detached snapshot, and persist over each other — a lost
	// update. Only contended in headless mode; in live-TUI mode the Tea loop
	// owns that state and the mutator never hydrates, so this is uncontended.
	headlessTxMu sync.Mutex
}

type webDeletedEntry struct {
	instance    *session.Instance
	deletedAt   time.Time
	deleteToken uint64
}

// NewWebMutator returns a WebMutator backed by the given Home. The undo
// window defaults to web.DefaultUndoWindow (30s).
func NewWebMutator(h *Home) *WebMutator {
	return &WebMutator{h: h, undoWindow: web.DefaultUndoWindow}
}

func (m *WebMutator) startRuntime(inst *session.Instance) (statedb.RuntimeState, error) {
	if m.startRuntimeFn != nil {
		return m.startRuntimeFn(inst)
	}
	return inst.StartRuntime()
}

func (m *WebMutator) restartRuntime(inst *session.Instance) (statedb.RuntimeState, error) {
	if m.restartRuntimeFn != nil {
		return m.restartRuntimeFn(inst)
	}
	return inst.RestartRuntime()
}

func (m *WebMutator) consumeRuntime(inst *session.Instance, runtime statedb.RuntimeState, err error) (statedb.RuntimeState, error, string) {
	reconcile := m.reconcileRestartFn
	if reconcile == nil {
		reconcile = (*session.Instance).ReconcileRestartResult
	}
	return consumePhysicalRuntimeResult(inst, runtime, err, reconcile)
}

func rollbackWebSeed(storage *session.Storage, seed statedb.InstanceSeedToken, cause error) error {
	if err := storage.RollbackSessionSeed(seed); err != nil {
		return errors.Join(cause, fmt.Errorf("rollback session seed: %w", err))
	}
	return cause
}

func (m *WebMutator) requestReload() {
	if m.requestReloadFn != nil {
		m.requestReloadFn()
		return
	}
	if m.h != nil && m.h.storageWatcher != nil {
		m.h.storageWatcher.TriggerReload()
	}
}

func (m *WebMutator) clearDeleteTokenAndReload(id string, token uint64) {
	m.h.durableDeleteMu.Lock()
	if current, ok := m.h.durableDeleteTombstones[id]; ok && current.sequence == token {
		delete(m.h.durableDeleteTombstones, id)
	}
	m.h.durableDeleteMu.Unlock()
	m.requestReload()
}

// WithUndoWindow overrides the undo grace period (useful for tests that
// need to force expiry without sleeping).
func (m *WebMutator) WithUndoWindow(d time.Duration) *WebMutator {
	m.undoWindow = d
	return m
}

// beginHeadlessTx serializes and hydrates a headless mutation (#1397). It
// returns an unlock function the caller MUST defer.
//
// In `web --no-tui` mode no bubbletea loop ever populates
// h.instances/instanceByID/groupTree, so every lookup would miss pre-existing
// sessions and persistAllInstances([]) would trip the empty-sweep guard. This
// helper:
//
//  1. takes headlessTxMu so the whole hydrate -> mutate -> persist sequence runs
//     as one critical section (no concurrent handler can replace the in-memory
//     snapshot mid-mutation — prevents lost updates), and
//  2. reloads the registry from storage so the mutation sees the current state,
//     including out-of-band changes from a concurrent CLI add/rm.
//
// In live-TUI mode it is a pure no-op (returns a no-op unlock and does NOT
// hydrate): the Tea loop owns that state and re-reading/locking here would
// race it.
//
// Callers in live mode pay only a nil check and a closure; the mutex is never
// contended because hydration never runs there.
func (m *WebMutator) beginHeadlessTx() (unlock func(), err error) {
	if m.h == nil || !m.h.IsHeadless() {
		return func() {}, nil
	}
	m.headlessTxMu.Lock()
	if hErr := m.h.HydrateInstancesFromStorage(); hErr != nil {
		m.headlessTxMu.Unlock()
		return func() {}, hErr
	}
	return m.headlessTxMu.Unlock, nil
}

// CreateSession creates and starts a new session, persisting it to storage.
func (m *WebMutator) CreateSession(title, tool, projectPath, groupPath, modelID, reasoningEffort string) (string, error) {
	unlock, err := m.beginHeadlessTx()
	if err != nil {
		return "", err
	}
	defer unlock()
	// #1706: project_path is identity and must be absolute — the request may
	// carry a relative path, which tmux would resolve against the tmux server's
	// cwd rather than this process's.
	projectPath, err = session.ResolveProjectPath(projectPath)
	if err != nil {
		return "", err
	}
	var inst *session.Instance
	if groupPath != "" {
		inst = session.NewInstanceWithGroupAndTool(title, projectPath, groupPath, tool)
	} else {
		inst = session.NewInstanceWithTool(title, projectPath, tool)
	}
	if tool != "" && tool != "shell" {
		inst.Command = tool
	}

	if modelID = strings.TrimSpace(modelID); modelID != "" {
		if err := inst.ApplyLaunchModel(modelID); err != nil {
			return "", err
		}
	}
	if reasoningEffort = strings.TrimSpace(reasoningEffort); reasoningEffort != "" {
		if err := inst.ApplyLaunchReasoningEffort(reasoningEffort); err != nil {
			return "", err
		}
	}

	storage, err := session.NewStorageWithProfile(m.h.profile)
	if err != nil {
		return "", fmt.Errorf("open storage: %w", err)
	}
	defer storage.Close()
	token := m.h.beginSessionTransitionFor(inst)
	defer m.h.finishSessionTransition(inst.ID, token)
	seed, err := storage.InsertSessionIfAbsent(inst)
	if err != nil {
		return "", fmt.Errorf("seed session: %w", err)
	}
	runtime, startErr := m.startRuntime(inst)
	_, startErr, warning := m.consumeRuntime(inst, runtime, startErr)
	if startErr != nil {
		return "", rollbackWebSeed(storage, seed, fmt.Errorf("start session: %w", startErr))
	}
	if warning != "" {
		uiLog.Warn("web_create_partial_success", "warning", warning, "instance_id", inst.ID)
	}

	m.h.instancesMu.RLock()
	existing := make([]*session.Instance, len(m.h.instances))
	copy(existing, m.h.instances)
	m.h.instancesMu.RUnlock()

	allInstances := append(existing, inst) //nolint:gocritic
	if err := m.h.saveWithGroups(storage, allInstances, m.h.groupTree); err != nil {
		return "", fmt.Errorf("save session: %w", err)
	}
	return inst.ID, nil
}

// StartSession starts a stopped/idle session by ID.
func (m *WebMutator) StartSession(id string) error {
	unlock, err := m.beginHeadlessTx()
	if err != nil {
		return err
	}
	defer unlock()
	m.h.instancesMu.RLock()
	inst := m.h.instanceByID[id]
	m.h.instancesMu.RUnlock()
	if inst == nil {
		return fmt.Errorf("session not found: %s", id)
	}
	token := m.h.beginSessionTransitionFor(inst)
	defer m.h.finishSessionTransition(inst.ID, token)
	runtime, startErr := m.startRuntime(inst)
	_, startErr, warning := m.consumeRuntime(inst, runtime, startErr)
	if warning != "" {
		uiLog.Warn("web_start_partial_success", "warning", warning, "instance_id", inst.ID)
	}
	return startErr
}

// StopSession kills (stops) a running session by ID.
func (m *WebMutator) StopSession(id string) error {
	unlock, err := m.beginHeadlessTx()
	if err != nil {
		return err
	}
	defer unlock()
	m.h.instancesMu.RLock()
	inst := m.h.instanceByID[id]
	m.h.instancesMu.RUnlock()
	if inst == nil {
		return fmt.Errorf("session not found: %s", id)
	}
	return inst.KillCaptured(inst.CaptureRuntimeSelection())
}

// RestartSession restarts a session by ID.
func (m *WebMutator) RestartSession(id string) error {
	unlock, err := m.beginHeadlessTx()
	if err != nil {
		return err
	}
	defer unlock()
	m.h.instancesMu.RLock()
	inst := m.h.instanceByID[id]
	m.h.instancesMu.RUnlock()
	if inst == nil {
		return fmt.Errorf("session not found: %s", id)
	}
	token := m.h.beginSessionTransitionFor(inst)
	defer m.h.finishSessionTransition(inst.ID, token)
	runtime, restartErr := m.restartRuntime(inst)
	_, restartErr, warning := m.consumeRuntime(inst, runtime, restartErr)
	if warning != "" {
		uiLog.Warn("web_restart_partial_success", "warning", warning, "instance_id", inst.ID)
	}
	return restartErr
}

// DeleteSession kills a session and removes it from persistent storage.
// Before removal, the instance is pushed onto the web undo stack so a
// subsequent UndoDelete (POST /api/sessions/undelete) can restore it.
func (m *WebMutator) DeleteSession(id string) error {
	unlock, err := m.beginHeadlessTx()
	if err != nil {
		return err
	}
	defer unlock()
	m.h.instancesMu.RLock()
	inst := m.h.instanceByID[id]
	m.h.instancesMu.RUnlock()
	if inst == nil {
		return fmt.Errorf("session not found: %s", id)
	}

	selection := inst.CaptureRuntimeSelection()
	deletedIncarnation := inst.PersistenceIncarnation()
	var deleteToken uint64
	if m.h.IsHeadless() {
		if err := inst.DeleteCaptured(selection); err != nil {
			return err
		}
		m.h.removeFailedSeededInstance(id)
	} else {
		m.h.durableDeleteMu.Lock()
		if err := inst.DeleteCaptured(selection); err != nil {
			m.h.durableDeleteMu.Unlock()
			return err
		}
		if m.h.durableDeleteTombstones == nil {
			m.h.durableDeleteTombstones = make(map[string]durableDeleteTombstone)
		}
		m.h.durableDeleteSeq++
		deleteToken = m.h.durableDeleteSeq
		m.h.durableDeleteTombstones[id] = durableDeleteTombstone{
			sequence: deleteToken, incarnation: deletedIncarnation,
		}
		m.h.durableDeleteMu.Unlock()
	}
	m.pushUndo(inst, deleteToken)
	return nil
}

// CloseSession stops the session process but keeps its metadata in
// storage. Mirrors the TUI's Shift+D handler (internal/ui/home.go
// closeSession). Identical to StopSession at the session.Instance level
// — both call Kill() — but is kept distinct so the parity matrix and
// the front-end can express the user-visible intent ("close, but don't
// delete").
func (m *WebMutator) CloseSession(id string) error {
	unlock, err := m.beginHeadlessTx()
	if err != nil {
		return err
	}
	defer unlock()
	m.h.instancesMu.RLock()
	inst := m.h.instanceByID[id]
	m.h.instancesMu.RUnlock()
	if inst == nil {
		return fmt.Errorf("session not found: %s", id)
	}
	return inst.KillCaptured(inst.CaptureRuntimeSelection())
}

// ArchiveSession stops the session process and marks it archived so it
// is hidden from active lists but retained in storage.
func (m *WebMutator) ArchiveSession(id string) error {
	unlock, err := m.beginHeadlessTx()
	if err != nil {
		return err
	}
	defer unlock()
	m.h.instancesMu.RLock()
	inst := m.h.instanceByID[id]
	m.h.instancesMu.RUnlock()
	if inst == nil {
		return fmt.Errorf("session not found: %s", id)
	}
	selection := inst.CaptureRuntimeSelection()
	runtime, err := inst.KillCapturedRuntime(selection)
	if err != nil {
		return fmt.Errorf("failed to stop session: %w", err)
	}
	if m.archiveAfterKillFn != nil {
		m.archiveAfterKillFn(runtime)
	}
	archivedAt := time.Now().UTC()
	if err := m.h.persistArchivedIfRuntime(runtime, selection.Incarnation, archivedAt); err != nil {
		return fmt.Errorf("failed to persist archive: %w", err)
	}
	m.h.instancesMu.Lock()
	inst.ArchivedAt = archivedAt
	m.h.instancesMu.Unlock()
	return nil
}

// UnarchiveSession clears the archive flag without starting tmux.
func (m *WebMutator) UnarchiveSession(id string) error {
	unlock, err := m.beginHeadlessTx()
	if err != nil {
		return err
	}
	defer unlock()
	m.h.instancesMu.RLock()
	inst := m.h.instanceByID[id]
	m.h.instancesMu.RUnlock()
	if inst == nil {
		return fmt.Errorf("session not found: %s", id)
	}
	m.h.instancesMu.Lock()
	if !inst.IsArchived() {
		m.h.instancesMu.Unlock()
		return fmt.Errorf("session is not archived: %s", id)
	}
	m.h.instancesMu.Unlock()
	incarnation := inst.PersistenceIncarnation()
	if err := m.h.persistArchivedIncarnation(id, incarnation, time.Time{}); err != nil {
		return fmt.Errorf("failed to persist unarchive: %w", err)
	}
	m.h.instancesMu.Lock()
	if current := m.h.instanceByID[id]; current != nil && current.MatchesPersistenceIncarnation(incarnation) {
		current.ArchivedAt = time.Time{}
	}
	m.h.instancesMu.Unlock()
	return nil
}

func (m *WebMutator) persistAllInstances() error {
	storage, err := session.NewStorageWithProfile(m.h.profile)
	if err != nil {
		return fmt.Errorf("open storage: %w", err)
	}
	defer storage.Close()

	m.h.instancesMu.RLock()
	instances := make([]*session.Instance, len(m.h.instances))
	copy(instances, m.h.instances)
	m.h.instancesMu.RUnlock()

	if err := m.h.saveWithGroups(storage, instances, m.h.groupTree); err != nil {
		return fmt.Errorf("save sessions: %w", err)
	}
	return nil
}

// UndoDelete restores the most-recently deleted session if its delete
// timestamp is within the configured undo window. Returns the restored
// session id. Returns web.ErrUndoNothing if the stack is empty, or
// web.ErrUndoExpired if the most recent entry is older than the window.
func (m *WebMutator) UndoDelete() (string, error) {
	m.undoMu.Lock()
	if len(m.undoStack) == 0 {
		m.undoMu.Unlock()
		return "", web.ErrUndoNothing
	}
	entry := m.undoStack[len(m.undoStack)-1]
	m.undoStack = m.undoStack[:len(m.undoStack)-1]
	window := m.undoWindow
	m.undoMu.Unlock()

	if window == 0 {
		window = web.DefaultUndoWindow
	}
	if time.Since(entry.deletedAt) > window {
		return "", web.ErrUndoExpired
	}

	// #1397: hydrate + serialize before reading/persisting the in-memory list so
	// the restored row is appended to the CURRENT registry (in headless mode the
	// list would otherwise be empty, dropping every other session) and so this
	// re-persist does not race a concurrent mutation.
	unlock, err := m.beginHeadlessTx()
	if err != nil {
		return "", err
	}
	defer unlock()
	storage, err := session.NewStorageWithProfile(m.h.profile)
	if err != nil {
		return "", fmt.Errorf("open storage: %w", err)
	}
	defer storage.Close()
	token := m.h.beginSessionTransitionFor(entry.instance)
	defer m.h.finishSessionTransition(entry.instance.ID, token)
	if m.beforeUndoInsertFn != nil {
		m.beforeUndoInsertFn()
	}
	seed, err := storage.ReinsertDeletedSession(entry.instance)
	if err != nil {
		if !m.h.IsHeadless() && errors.Is(err, session.ErrSessionAlreadyExists) {
			m.clearDeleteTokenAndReload(entry.instance.ID, entry.deleteToken)
		}
		return "", fmt.Errorf("seed session: %w", err)
	}
	runtime, restartErr := m.restartRuntime(entry.instance)
	_, restartErr, warning := m.consumeRuntime(entry.instance, runtime, restartErr)
	if restartErr != nil {
		rollbackErr := rollbackWebSeed(storage, seed, fmt.Errorf("restart session: %w", restartErr))
		if !m.h.IsHeadless() && (errors.Is(rollbackErr, statedb.ErrRuntimeGenerationConflict) ||
			errors.Is(rollbackErr, statedb.ErrStatusRevisionConflict) ||
			errors.Is(rollbackErr, statedb.ErrInstanceParentConflict)) {
			m.clearDeleteTokenAndReload(entry.instance.ID, entry.deleteToken)
		}
		return "", rollbackErr
	}
	if warning != "" {
		uiLog.Warn("web_undo_restart_partial_success", "warning", warning, "instance_id", entry.instance.ID)
	}

	if m.h.IsHeadless() {
		canonical, added := m.h.publishCompletedInstance(entry.instance, runtime)
		if added && m.h.groupTree != nil {
			m.h.groupTree.AddSession(canonical)
		}
		if m.h.search != nil {
			m.h.search.SetItems(m.h.instances)
		}
		if m.h.groupTree != nil {
			m.h.rebuildFlatItems()
		}
	} else {
		m.clearDeleteTokenAndReload(entry.instance.ID, entry.deleteToken)
	}
	return entry.instance.ID, nil
}

// pushUndo records a freshly-deleted instance onto the web undo stack,
// capped at 10 entries (FIFO eviction) to bound memory.
func (m *WebMutator) pushUndo(inst *session.Instance, deleteToken uint64) {
	m.undoMu.Lock()
	defer m.undoMu.Unlock()
	m.undoStack = append(m.undoStack, webDeletedEntry{
		instance:    inst,
		deletedAt:   time.Now(),
		deleteToken: deleteToken,
	})
	if len(m.undoStack) > 10 {
		m.undoStack = m.undoStack[len(m.undoStack)-10:]
	}
}

// ForkSession forks an existing session using the proper tool-specific fork command.
func (m *WebMutator) ForkSession(id string) (string, error) {
	unlock, err := m.beginHeadlessTx()
	if err != nil {
		return "", err
	}
	defer unlock()
	m.h.instancesMu.RLock()
	parent := m.h.instanceByID[id]
	m.h.instancesMu.RUnlock()
	if parent == nil {
		return "", fmt.Errorf("session not found: %s", id)
	}

	forked, _, err := parent.CreateForkedInstanceForTool(parent.Title+" (fork)", parent.GroupPath, nil)
	if err != nil {
		return "", fmt.Errorf("fork session: %w", err)
	}

	storage, err := session.NewStorageWithProfile(m.h.profile)
	if err != nil {
		return "", fmt.Errorf("open storage: %w", err)
	}
	defer storage.Close()
	token := m.h.beginSessionTransitionFor(forked)
	defer m.h.finishSessionTransition(forked.ID, token)
	seed, err := storage.InsertSessionIfAbsent(forked)
	if err != nil {
		return "", fmt.Errorf("seed forked session: %w", err)
	}
	runtime, startErr := m.startRuntime(forked)
	_, startErr, warning := m.consumeRuntime(forked, runtime, startErr)
	if startErr != nil {
		return "", rollbackWebSeed(storage, seed, fmt.Errorf("start forked session: %w", startErr))
	}
	if warning != "" {
		uiLog.Warn("web_fork_partial_success", "warning", warning, "instance_id", forked.ID)
	}

	m.h.instancesMu.RLock()
	existing := make([]*session.Instance, len(m.h.instances))
	copy(existing, m.h.instances)
	m.h.instancesMu.RUnlock()

	allInstances := append(existing, forked) //nolint:gocritic
	if err := m.h.saveWithGroups(storage, allInstances, m.h.groupTree); err != nil {
		return "", fmt.Errorf("save forked session: %w", err)
	}
	return forked.ID, nil
}

// UpdateSession applies one or more field edits via session.SetField (the
// same path the TUI EditSessionDialog uses) and persists. Returns the list
// of fields that actually changed and whether any change requires a restart.
//
// instancesMu is held only across the SetField loop — postCommits and the
// storage flush run after unlock, mirroring the TUI's home.go edit handler
// so slow tmux subprocesses don't stall the status worker.
func (m *WebMutator) UpdateSession(id string, updates map[string]string) ([]string, bool, error) {
	if len(updates) == 0 {
		return nil, false, nil
	}
	unlock, err := m.beginHeadlessTx()
	if err != nil {
		return nil, false, err
	}
	defer unlock()
	m.h.instancesMu.RLock()
	inst := m.h.instanceByID[id]
	m.h.instancesMu.RUnlock()
	if inst == nil {
		return nil, false, fmt.Errorf("session not found: %s", id)
	}

	changed := make([]string, 0, len(updates))
	restartRequired := false
	var postCommits []func()

	m.h.instancesMu.Lock()
	for field, value := range updates {
		oldValue, postCommit, err := session.SetField(inst, field, value, nil)
		if err != nil {
			m.h.instancesMu.Unlock()
			return nil, false, err
		}
		// #1706: SetField canonicalizes a project path, so a request carrying
		// another spelling of the stored path is a no-op — compare what was
		// actually stored, not the raw request value, or it would be reported
		// as changed and restart-required.
		newValue := value
		if field == session.FieldPath {
			newValue = inst.ProjectPath
		}
		if oldValue == newValue {
			continue
		}
		changed = append(changed, field)
		if postCommit != nil {
			postCommits = append(postCommits, postCommit)
		}
		if session.RestartPolicyFor(field) == session.FieldRestartRequired {
			restartRequired = true
		}
	}
	m.h.instancesMu.Unlock()

	for _, fn := range postCommits {
		fn()
	}

	if len(changed) == 0 {
		return nil, false, nil
	}

	storage, err := session.NewStorageWithProfile(m.h.profile)
	if err != nil {
		return nil, false, fmt.Errorf("open storage: %w", err)
	}
	defer storage.Close()

	m.h.instancesMu.RLock()
	instances := make([]*session.Instance, len(m.h.instances))
	copy(instances, m.h.instances)
	m.h.instancesMu.RUnlock()

	if err := m.h.saveWithGroups(storage, instances, m.h.groupTree); err != nil {
		return nil, false, fmt.Errorf("save session: %w", err)
	}
	return changed, restartRequired, nil
}

// CreateGroup creates a new group (or subgroup if parentPath is non-empty) and
// persists the group tree to storage.
func (m *WebMutator) CreateGroup(name, parentPath string) (string, error) {
	unlock, err := m.beginHeadlessTx()
	if err != nil {
		return "", err
	}
	defer unlock()
	// Seed the new-group default from [group_defaults].max_concurrent.
	if cfg, _ := session.LoadUserConfig(); cfg != nil {
		m.h.groupTree.DefaultMaxConcurrent = cfg.GroupDefaults.MaxConcurrent
	}
	var grp *session.Group
	if parentPath != "" {
		grp = m.h.groupTree.CreateSubgroup(parentPath, name)
	} else {
		grp = m.h.groupTree.CreateGroup(name)
	}
	if grp == nil {
		return "", fmt.Errorf("failed to create group %q", name)
	}

	storage, err := session.NewStorageWithProfile(m.h.profile)
	if err != nil {
		return "", fmt.Errorf("open storage: %w", err)
	}
	defer storage.Close()

	m.h.instancesMu.RLock()
	instances := make([]*session.Instance, len(m.h.instances))
	copy(instances, m.h.instances)
	m.h.instancesMu.RUnlock()

	if err := m.h.saveWithGroups(storage, instances, m.h.groupTree); err != nil {
		return "", fmt.Errorf("save group: %w", err)
	}
	return grp.Path, nil
}

// RenameGroup renames a group identified by groupPath to newName and persists.
func (m *WebMutator) RenameGroup(groupPath, newName string) error {
	unlock, err := m.beginHeadlessTx()
	if err != nil {
		return err
	}
	defer unlock()
	if err := m.h.groupTree.RenameGroup(groupPath, newName); err != nil {
		return err
	}

	storage, err := session.NewStorageWithProfile(m.h.profile)
	if err != nil {
		return fmt.Errorf("open storage: %w", err)
	}
	defer storage.Close()

	m.h.instancesMu.RLock()
	instances := make([]*session.Instance, len(m.h.instances))
	copy(instances, m.h.instances)
	m.h.instancesMu.RUnlock()

	return m.h.saveWithGroups(storage, instances, m.h.groupTree)
}

// FinishWorktree merges (or skips), removes the worktree, optionally
// deletes the source branch, kills the tmux session, and removes the
// session from storage. Mirrors `agent-deck worktree finish` (see
// cmd/agent-deck/worktree_cmd.go handleWorktreeFinish) — the
// orchestration is duplicated rather than refactored to keep the
// fix minimally invasive (issue #1126).
func (m *WebMutator) FinishWorktree(id string, opts web.WorktreeFinishOptions) (web.WorktreeFinishResult, error) {
	unlock, err := m.beginHeadlessTx()
	if err != nil {
		return web.WorktreeFinishResult{}, err
	}
	defer unlock()
	m.h.instancesMu.RLock()
	inst := m.h.instanceByID[id]
	m.h.instancesMu.RUnlock()
	if inst == nil {
		return web.WorktreeFinishResult{}, web.ErrSessionNotFound
	}
	if !inst.IsWorktree() {
		return web.WorktreeFinishResult{}, web.ErrNotAWorktree
	}

	repoRoot := inst.WorktreeRepoRoot
	worktreePath := inst.WorktreePath
	worktreeBranch := inst.WorktreeBranch
	selection := inst.CaptureRuntimeSelection()

	backend, err := vcsbackend.Detect(repoRoot)
	if err != nil {
		return web.WorktreeFinishResult{}, fmt.Errorf("initialize VCS: %w", err)
	}

	if !opts.Force {
		dirty, dErr := git.HasUncommittedChanges(worktreePath)
		if dErr != nil {
			if _, statErr := os.Stat(worktreePath); os.IsNotExist(statErr) {
				dirty = false
			} else {
				return web.WorktreeFinishResult{}, fmt.Errorf("check worktree status: %w", dErr)
			}
		}
		if dirty {
			return web.WorktreeFinishResult{}, fmt.Errorf("worktree has uncommitted changes (set force=true to override)")
		}
	}

	targetBranch := opts.Into
	if targetBranch == "" && !opts.NoMerge {
		targetBranch, err = backend.GetDefaultBranch()
		if err != nil {
			return web.WorktreeFinishResult{}, fmt.Errorf("determine target branch: %w (set into=<branch>)", err)
		}
	}
	if !opts.NoMerge && targetBranch == worktreeBranch {
		return web.WorktreeFinishResult{}, fmt.Errorf("cannot merge branch %q into itself", worktreeBranch)
	}

	if !opts.NoMerge {
		// Checkout target in main repo, then merge.
		checkout := exec.Command("git", "-C", repoRoot, "checkout", targetBranch)
		if out, cErr := checkout.CombinedOutput(); cErr != nil {
			return web.WorktreeFinishResult{}, fmt.Errorf("checkout %s: %s", targetBranch, strings.TrimSpace(string(out)))
		}
		if mErr := backend.MergeBranch(worktreeBranch); mErr != nil {
			if backend.Type() == vcs.TypeGit {
				_ = exec.Command("git", "-C", repoRoot, "merge", "--abort").Run()
			}
			return web.WorktreeFinishResult{}, fmt.Errorf("merge failed (aborted): %w", mErr)
		}
	}

	if err := inst.DeleteCaptured(selection); err != nil {
		return web.WorktreeFinishResult{}, fmt.Errorf("session changed before finish: %w", err)
	}

	if _, statErr := os.Stat(worktreePath); !os.IsNotExist(statErr) {
		// Best-effort: log via error wrapping only if it bubbles. CLI
		// treats this as a warning; we mirror that by swallowing here so
		// the rest of cleanup proceeds.
		_ = backend.RemoveWorktree(worktreePath, opts.Force)
	}
	_ = backend.PruneWorktrees()

	branchDeleted := false
	if !opts.KeepBranch {
		if dErr := backend.DeleteBranch(worktreeBranch, opts.Force); dErr == nil {
			branchDeleted = true
		}
	}

	storage, err := session.NewStorageWithProfile(m.h.profile)
	if err != nil {
		return web.WorktreeFinishResult{}, fmt.Errorf("open storage: %w", err)
	}
	defer storage.Close()

	// The runtime-aware delete above already removed the row. Persist only group
	// ordering here; never issue an unconditional second delete.
	if sErr := storage.SaveGroupsOnly(m.h.groupTree); sErr != nil {
		return web.WorktreeFinishResult{}, fmt.Errorf("save session data: %w", sErr)
	}

	// Issue #1576: sweep transition-notifier state (inbox JSONL lines +
	// runtime/transition-notify-state.json dedup record) for the removed
	// session, mirroring the #910 cleanup on `agent-deck rm`. Best-effort —
	// never fails the finish.
	_, _ = session.SweepInboxesForChildSession(id)
	_, _ = session.RemoveNotifyStateRecord(id)

	mergedInto := targetBranch
	if opts.NoMerge {
		mergedInto = ""
	}
	return web.WorktreeFinishResult{
		SessionID:     id,
		Branch:        worktreeBranch,
		MergedInto:    mergedInto,
		Merged:        !opts.NoMerge,
		BranchDeleted: branchDeleted,
	}, nil
}

// DeleteGroup deletes a group (and its subgroups), moving sessions to the default
// group. Returns an error if groupPath is the default group.
func (m *WebMutator) DeleteGroup(groupPath string) error {
	if groupPath == session.DefaultGroupPath {
		return fmt.Errorf("cannot delete default group")
	}
	unlock, err := m.beginHeadlessTx()
	if err != nil {
		return err
	}
	defer unlock()

	m.h.groupTree.DeleteGroup(groupPath)

	storage, err := session.NewStorageWithProfile(m.h.profile)
	if err != nil {
		return fmt.Errorf("open storage: %w", err)
	}
	defer storage.Close()

	m.h.instancesMu.RLock()
	instances := make([]*session.Instance, len(m.h.instances))
	copy(instances, m.h.instances)
	m.h.instancesMu.RUnlock()

	return m.h.saveWithGroups(storage, instances, m.h.groupTree)
}
