//go:build runtime_lifecycle_helper

package session

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// RuntimeLifecycleHelperConfig is available only to the separately built
// lifecycle crash helper. Normal Agent Deck builds do not compile this bridge.
type RuntimeLifecycleHelperConfig struct {
	DBPath        string
	InventoryPath string
	LockRoot      string
	Stage         RuntimeTransitionStage
	SessionName   string
	Ready         *os.File
	Release       *os.File
}

// RuntimeLifecycleContenderResult is emitted by an independently compiled
// helper process after contending for one physical runtime transition.
type RuntimeLifecycleContenderResult struct {
	Adopted      bool
	Runtime      statedb.RuntimeState
	LocalRuntime statedb.RuntimeState
	SpawnCalls   int
	StampCalls   int
	CommitCalls  int
	SweepCalls   int
}

// RunRuntimeLifecycleContenderHelper makes two separately compiled processes
// race through the real filesystem lock and SQLite transition commit. The
// supplied barrier is reached after the initial durable observation and before
// lock acquisition, proving that the eventual loser observed the old tuple.
func RunRuntimeLifecycleContenderHelper(config RuntimeLifecycleHelperConfig) (RuntimeLifecycleContenderResult, error) {
	result := RuntimeLifecycleContenderResult{}
	if config.DBPath == "" || config.InventoryPath == "" || config.LockRoot == "" ||
		config.SessionName == "" || config.Ready == nil || config.Release == nil {
		return result, fmt.Errorf("incomplete runtime lifecycle contender configuration")
	}
	db, err := statedb.Open(config.DBPath)
	if err != nil {
		return result, err
	}
	defer db.Close()
	if err := db.Migrate(); err != nil {
		return result, err
	}

	oldObserved := runtimeTransitionObservedFn
	oldInventory := runtimeCandidateInventoryFn
	oldExists := runtimeCandidateExistsFn
	oldStamp := runtimeCandidateStampFn
	oldCommit := runtimeTransitionCommitFn
	oldSweep := runtimeDuplicateSweepFn
	oldNow := nowFn
	oldRoot := agentDeckDirOverride
	defer func() {
		runtimeTransitionObservedFn = oldObserved
		runtimeCandidateInventoryFn = oldInventory
		runtimeCandidateExistsFn = oldExists
		runtimeCandidateStampFn = oldStamp
		runtimeTransitionCommitFn = oldCommit
		runtimeDuplicateSweepFn = oldSweep
		nowFn = oldNow
		agentDeckDirOverride = oldRoot
	}()

	agentDeckDirOverride = config.LockRoot
	runtimeTransitionObservedFn = func() {
		if _, err := config.Ready.Write([]byte{1}); err != nil {
			panic(fmt.Sprintf("signal runtime lifecycle contender barrier: %v", err))
		}
		var release [1]byte
		if _, err := io.ReadFull(config.Release, release[:]); err != nil {
			panic(fmt.Sprintf("await runtime lifecycle contender barrier: %v", err))
		}
	}
	runtimeCandidateExistsFn = func(*tmux.Session) bool { return true }
	runtimeCandidateInventoryFn = func(socketName, instanceID string) ([]tmux.RuntimeCandidate, error) {
		candidates, err := readRuntimeLifecycleInventory(config.InventoryPath)
		if err != nil {
			return nil, err
		}
		matching := make([]tmux.RuntimeCandidate, 0, len(candidates))
		for _, candidate := range candidates {
			if candidate.SocketName == socketName && candidate.InstanceID == instanceID {
				matching = append(matching, candidate)
			}
		}
		return matching, nil
	}
	runtimeCandidateStampFn = func(
		_ *tmux.Session, next statedb.RuntimeState, bindingKind, bindingValue string,
	) error {
		result.StampCalls++
		candidates, err := readRuntimeLifecycleInventory(config.InventoryPath)
		if err != nil {
			return err
		}
		candidates = append(candidates, tmux.RuntimeCandidate{
			SessionName: next.TmuxSession, SocketName: next.TmuxSocketName,
			SessionID: fmt.Sprintf("$%d", os.Getpid()), PaneID: fmt.Sprintf("%%%d", os.Getpid()),
			InstanceID: next.InstanceID, Generation: next.Generation, GenerationKnown: true,
			StatusRevision: next.StatusRevision, Status: next.Status,
			LastStartedUnixNano: next.LastStartedAt.UnixNano(), StateKnown: true,
			BindingKind: bindingKind, BindingValue: bindingValue, BindingKnown: true,
			PanePID: os.Getpid(),
		})
		return writeRuntimeLifecycleInventory(config.InventoryPath, candidates)
	}
	runtimeTransitionCommitFn = func(
		db *statedb.StateDB, expected uint64, incarnation string,
		next statedb.RuntimeState, plan []statedb.RuntimeBindingTransition,
	) error {
		result.CommitCalls++
		return db.CommitRuntimeTransitionWithBindingPlan(expected, incarnation, next, plan)
	}
	runtimeDuplicateSweepFn = func(*Instance, ...string) { result.SweepCalls++ }
	nowFn = func() time.Time { return time.Unix(100, 123).UTC() }

	row, err := db.LoadInstanceByID("one")
	if err != nil {
		return result, err
	}
	if row == nil || row.Incarnation == "" {
		return result, statedb.ErrInstanceParentConflict
	}
	inst := &Instance{
		ID: "one", Title: "one", ProjectPath: "/tmp/one", GroupPath: "my-sessions",
		Tool: "claude", Status: StatusIdle, TmuxSocketName: "isolated", owningDB: db,
		persistenceIncarnation: row.Incarnation,
		tmuxSession:            &tmux.Session{Name: "runtime-g0", SocketName: "isolated", InstanceID: "one"},
	}
	authority, winner, err := inst.beginRuntimeTransition(false)
	if err != nil {
		return result, err
	}
	if winner != nil {
		result.Adopted = true
		result.Runtime = *winner
		result.LocalRuntime = inst.runtimeStateSnapshot()
		if result.LocalRuntime != result.Runtime {
			return result, fmt.Errorf("lock loser local runtime %#v did not adopt winner %#v", result.LocalRuntime, result.Runtime)
		}
		return result, nil
	}
	defer authority.close()
	result.SpawnCalls++
	inst.mu.Lock()
	inst.tmuxSession = &tmux.Session{Name: config.SessionName, SocketName: "isolated", InstanceID: inst.ID}
	inst.TmuxSocketName = "isolated"
	inst.Status = StatusStarting
	inst.mu.Unlock()
	next, _, committed, err := inst.commitPhysicalRuntime(authority)
	if err != nil {
		return result, err
	}
	if !committed {
		return result, fmt.Errorf("runtime contender did not commit")
	}
	runtimeDuplicateSweepFn(inst, authority.expected.TmuxSocketName)
	result.Runtime = next
	result.LocalRuntime = inst.runtimeStateSnapshot()
	if result.LocalRuntime != result.Runtime {
		return result, fmt.Errorf("lock winner local runtime %#v differs from committed runtime %#v", result.LocalRuntime, result.Runtime)
	}
	return result, nil
}

// RunRuntimeLifecycleCrashHelper enters the real transition publication path
// with a JSON-backed fake tmux inventory, then exits at the requested fault
// boundary. It returns only when setup fails or the boundary is not reached.
func RunRuntimeLifecycleCrashHelper(config RuntimeLifecycleHelperConfig) error {
	if config.Stage != RuntimeTransitionAfterRespawnBeforeStamp &&
		config.Stage != RuntimeTransitionAfterStampBeforeCommit &&
		config.Stage != RuntimeTransitionAfterCommitBeforeSweep &&
		config.Stage != RuntimeDestructionAfterReserveBeforeTerminate &&
		config.Stage != RuntimeDestructionAfterTerminateBeforeComplete {
		return fmt.Errorf("unknown runtime transition stage %q", config.Stage)
	}
	db, err := statedb.Open(config.DBPath)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := db.Migrate(); err != nil {
		return err
	}

	oldObserved := runtimeTransitionObservedFn
	oldFault := runtimeTransitionFaultFn
	oldInventory := runtimeCandidateInventoryFn
	oldExists := runtimeCandidateExistsFn
	oldRevalidate := runtimeCandidateRevalidateFn
	oldStamp := runtimeCandidateStampFn
	oldRespawn := runtimeCandidateRespawnFn
	oldGenerationInventory := runtimeGenerationCandidateInventoryFn
	oldTerminate := terminateCapturedRuntimeFn
	oldDiscoverChildren := discoverCapturedRuntimeChildrenFn
	oldReserved := runtimeDestructionReservedFn
	oldNow := nowFn
	oldRoot := agentDeckDirOverride
	defer func() {
		runtimeTransitionObservedFn = oldObserved
		runtimeTransitionFaultFn = oldFault
		runtimeCandidateInventoryFn = oldInventory
		runtimeCandidateExistsFn = oldExists
		runtimeCandidateRevalidateFn = oldRevalidate
		runtimeCandidateStampFn = oldStamp
		runtimeCandidateRespawnFn = oldRespawn
		runtimeGenerationCandidateInventoryFn = oldGenerationInventory
		terminateCapturedRuntimeFn = oldTerminate
		discoverCapturedRuntimeChildrenFn = oldDiscoverChildren
		runtimeDestructionReservedFn = oldReserved
		nowFn = oldNow
		agentDeckDirOverride = oldRoot
	}()

	agentDeckDirOverride = config.LockRoot
	runtimeTransitionObservedFn = func() {}
	runtimeCandidateExistsFn = func(*tmux.Session) bool { return true }
	// The helper's JSON inventory is the physical oracle. Do not shell out to
	// the deliberately empty private PATH to revalidate its synthetic panes.
	runtimeCandidateRevalidateFn = func(candidate tmux.RuntimeCandidate) (tmux.RuntimeCandidate, error) {
		return candidate, nil
	}
	runtimeCandidateInventoryFn = func(socketName, instanceID string) ([]tmux.RuntimeCandidate, error) {
		candidates, err := readRuntimeLifecycleInventory(config.InventoryPath)
		if err != nil {
			return nil, err
		}
		matching := make([]tmux.RuntimeCandidate, 0, len(candidates))
		for _, candidate := range candidates {
			if candidate.SocketName == socketName && candidate.InstanceID == instanceID {
				matching = append(matching, candidate)
			}
		}
		return matching, nil
	}
	runtimeGenerationCandidateInventoryFn = func(socketName, instanceID string) ([]tmux.RuntimeGenerationCandidate, error) {
		candidates, err := readRuntimeLifecycleInventory(config.InventoryPath)
		if err != nil {
			return nil, err
		}
		matching := make([]tmux.RuntimeGenerationCandidate, 0, len(candidates))
		for _, candidate := range candidates {
			if candidate.SocketName != socketName || candidate.InstanceID != instanceID {
				continue
			}
			matching = append(matching, tmux.RuntimeGenerationCandidate{
				SessionID: candidate.SessionID, SessionName: candidate.SessionName,
				SocketName: candidate.SocketName, PaneID: candidate.PaneID, PanePID: candidate.PanePID,
				InstanceID: candidate.InstanceID, Generation: candidate.Generation,
				GenerationKnown: candidate.GenerationKnown,
			})
		}
		return matching, nil
	}
	discoverCapturedRuntimeChildrenFn = func(*Instance, tmux.RuntimeGenerationCandidate) ([]tmux.ProcessIdentity, error) {
		return nil, nil
	}
	if config.Stage == RuntimeTransitionAfterRespawnBeforeStamp {
		if err := writeRuntimeLifecycleInventory(config.InventoryPath, []tmux.RuntimeCandidate{{
			SessionID: "$7", SessionName: "runtime-g0", SocketName: "isolated",
			PaneID: "%9", PanePID: 3131, InstanceID: "one",
			Generation: 0, GenerationKnown: true,
		}}); err != nil {
			return err
		}
		runtimeCandidateRespawnFn = func(_ *tmux.Session, candidate tmux.RuntimeGenerationCandidate, _ string) error {
			if candidate.SessionID != "$7" || candidate.PaneID != "%9" || candidate.PanePID != 3131 {
				return fmt.Errorf("unexpected runtime respawn candidate %#v", candidate)
			}
			candidates, err := readRuntimeLifecycleInventory(config.InventoryPath)
			if err != nil {
				return err
			}
			if len(candidates) != 1 {
				return fmt.Errorf("runtime respawn inventory has %d candidates", len(candidates))
			}
			candidates[0].GenerationKnown = false
			candidates[0].ProofError = "missing AGENTDECK_RUNTIME_GENERATION"
			candidates[0].PanePID = os.Getpid()
			return writeRuntimeLifecycleInventory(config.InventoryPath, candidates)
		}
	}
	if config.Stage == RuntimeDestructionAfterReserveBeforeTerminate ||
		config.Stage == RuntimeDestructionAfterTerminateBeforeComplete {
		if err := writeRuntimeLifecycleInventory(config.InventoryPath, []tmux.RuntimeCandidate{{
			SessionID: "$7", SessionName: "runtime-g0", SocketName: "isolated",
			PaneID: "%9", PanePID: os.Getpid(), InstanceID: "one",
			Generation: 0, GenerationKnown: true,
		}}); err != nil {
			return err
		}
		if config.Stage == RuntimeDestructionAfterReserveBeforeTerminate {
			runtimeDestructionReservedFn = func(statedb.RuntimeState) { os.Exit(72) }
		} else {
			terminateCapturedRuntimeFn = func(tmux.RuntimeGenerationCandidate, bool) error {
				if err := writeRuntimeLifecycleInventory(config.InventoryPath, nil); err != nil {
					return err
				}
				os.Exit(73)
				return nil
			}
		}
	}
	runtimeCandidateStampFn = func(
		_ *tmux.Session, next statedb.RuntimeState, bindingKind, bindingValue string,
	) error {
		candidates, err := readRuntimeLifecycleInventory(config.InventoryPath)
		if err != nil {
			return err
		}
		candidates = append(candidates, tmux.RuntimeCandidate{
			SessionName: next.TmuxSession, SocketName: next.TmuxSocketName,
			SessionID: "$8", PaneID: "%10",
			InstanceID: next.InstanceID, Generation: next.Generation, GenerationKnown: true,
			StatusRevision: next.StatusRevision, Status: next.Status,
			LastStartedUnixNano: next.LastStartedAt.UnixNano(), StateKnown: true,
			BindingKind: bindingKind, BindingValue: bindingValue, BindingKnown: true,
			PanePID: os.Getpid(),
		})
		return writeRuntimeLifecycleInventory(config.InventoryPath, candidates)
	}
	runtimeTransitionFaultFn = func(observed RuntimeTransitionStage, _ statedb.RuntimeState) error {
		if observed == config.Stage {
			if observed == RuntimeTransitionAfterRespawnBeforeStamp {
				os.Exit(69)
			}
			if observed == RuntimeTransitionAfterStampBeforeCommit {
				os.Exit(70)
			}
			os.Exit(71)
		}
		return nil
	}
	nowFn = func() time.Time { return time.Unix(100, 123).UTC() }
	row, err := db.LoadInstanceByID("one")
	if err != nil {
		return err
	}
	if row == nil || row.Incarnation == "" {
		return statedb.ErrInstanceParentConflict
	}

	inst := &Instance{
		ID: "one", Title: "one", ProjectPath: "/tmp/one", GroupPath: "my-sessions",
		Tool: "claude", Status: StatusIdle, TmuxSocketName: "isolated", owningDB: db,
		persistenceIncarnation: row.Incarnation,
		tmuxSession:            &tmux.Session{Name: "runtime-g0", SocketName: "isolated", InstanceID: "one"},
	}
	if config.Stage == RuntimeDestructionAfterReserveBeforeTerminate ||
		config.Stage == RuntimeDestructionAfterTerminateBeforeComplete {
		err = inst.KillCaptured(inst.CaptureRuntimeSelection())
		return fmt.Errorf("runtime crash boundary %s was not reached: %w", config.Stage, err)
	}
	authority, winner, err := inst.beginRuntimeTransition(false)
	if err != nil {
		return err
	}
	if winner != nil {
		return fmt.Errorf("unexpected transition winner: %#v", *winner)
	}
	if config.Stage == RuntimeTransitionAfterRespawnBeforeStamp {
		err = inst.respawnRuntimePane(authority, "resume-command")
		authority.close()
		return fmt.Errorf("runtime crash boundary %s was not reached: %w", config.Stage, err)
	}
	inst.mu.Lock()
	inst.tmuxSession = &tmux.Session{Name: "runtime-g1", SocketName: "isolated", InstanceID: inst.ID}
	inst.TmuxSocketName = "isolated"
	inst.Status = StatusStarting
	inst.mu.Unlock()
	_, _, _, err = inst.commitPhysicalRuntime(authority)
	authority.close()
	return fmt.Errorf("runtime crash boundary %s was not reached: %w", config.Stage, err)
}

func readRuntimeLifecycleInventory(path string) ([]tmux.RuntimeCandidate, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var candidates []tmux.RuntimeCandidate
	if err := json.Unmarshal(raw, &candidates); err != nil {
		return nil, err
	}
	return candidates, nil
}

func writeRuntimeLifecycleInventory(path string, candidates []tmux.RuntimeCandidate) error {
	raw, err := json.Marshal(candidates)
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}
