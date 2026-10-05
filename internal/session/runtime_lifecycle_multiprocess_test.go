package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

const runtimeLifecycleSessionHelperEnv = "AGENTDECK_RUNTIME_LIFECYCLE_SESSION_HELPER"

const (
	runtimeCrashStageEnv     = "AGENTDECK_RUNTIME_CRASH_STAGE"
	runtimeCrashDBEnv        = "AGENTDECK_RUNTIME_CRASH_DB"
	runtimeCrashInventoryEnv = "AGENTDECK_RUNTIME_CRASH_INVENTORY"
)

func TestRuntimeLifecycle_Multiprocess(t *testing.T) {
	path := os.Getenv(runtimeLifecycleSessionHelperEnv)
	if path != "" {
		db, err := statedb.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if err := db.Migrate(); err != nil {
			t.Fatal(err)
		}
		row, err := db.LoadInstanceByID("one")
		if err != nil || row == nil || row.Incarnation == "" {
			t.Fatalf("load child incarnation: row=%#v err=%v", row, err)
		}
		if err := db.CommitRuntimeTransition(0, row.Incarnation, statedb.RuntimeState{
			InstanceID: "one", Generation: 1, TmuxSession: "child-runtime",
			TmuxSocketName: "isolated", Status: "running", LastStartedAt: time.Unix(200, 0).UTC(),
		}); err != nil {
			t.Fatal(err)
		}
		return
	}

	path = filepath.Join(t.TempDir(), "state.db")
	db, err := statedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveInstance(&statedb.InstanceRow{
		ID: "one", Title: "one", ProjectPath: "/tmp/one", GroupPath: "my-sessions",
		Tool: "pi", Status: "idle", TmuxSession: "parent-runtime", CreatedAt: time.Unix(1, 0),
		ToolData: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestRuntimeLifecycle_Multiprocess$")
	cmd.Env = append(os.Environ(), runtimeLifecycleSessionHelperEnv+"="+path)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		t.Fatalf("runtime transition helper failed: %v\n%s", err, output.String())
	}

	state, found, err := db.ReadRuntimeState("one")
	if err != nil || !found || state.Generation != 1 || state.TmuxSession != "child-runtime" {
		t.Fatalf("child runtime state=%#v found=%v err=%v", state, found, err)
	}
	canonical := &Instance{ID: "one", Title: "canonical", Tool: "pi", Status: StatusIdle}
	if !canonical.ApplyRuntimeState(state) {
		t.Fatal("canonical instance rejected the child process runtime")
	}
	stale := &Instance{ID: "one", Title: "stale", Tool: "pi", Status: StatusError}
	if !canonical.MergeReloaded(stale) {
		t.Fatal("stale reload did not merge safe metadata")
	}
	if got := canonical.RuntimeState(); got.Generation != 1 || got.TmuxSession != "child-runtime" || got.Status != "running" {
		t.Fatalf("canonical runtime regressed: %#v", got)
	}
}

func runtimeCrashExitCode(stage RuntimeTransitionStage) int {
	switch stage {
	case RuntimeTransitionAfterRespawnBeforeStamp:
		return 69
	case RuntimeTransitionAfterStampBeforeCommit:
		return 70
	case RuntimeTransitionAfterCommitBeforeSweep:
		return 71
	case RuntimeDestructionAfterReserveBeforeTerminate:
		return 72
	case RuntimeDestructionAfterTerminateBeforeComplete:
		return 73
	default:
		return 2
	}
}

func readRuntimeCrashCandidates(path string) ([]tmux.RuntimeCandidate, error) {
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

func writeRuntimeCrashCandidates(path string, candidates []tmux.RuntimeCandidate) error {
	raw, err := json.Marshal(candidates)
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

func runtimeCrashInstance(t *testing.T, db *statedb.StateDB) *Instance {
	t.Helper()
	row, err := db.LoadInstanceByID("one")
	if err != nil || row == nil || row.Incarnation == "" {
		t.Fatalf("load crash instance incarnation: row=%#v err=%v", row, err)
	}
	inst := &Instance{
		ID: "one", Title: "one", ProjectPath: "/tmp/one", GroupPath: "my-sessions",
		Tool: "claude", Status: StatusIdle, TmuxSocketName: "isolated", owningDB: db,
		tmuxSession: &tmux.Session{Name: "runtime-g0", SocketName: "isolated", InstanceID: "one"},
	}
	inst.adoptPersistenceIncarnation(row.Incarnation)
	return inst
}

func runRuntimeCrashChild(t *testing.T, stage RuntimeTransitionStage) {
	t.Helper()
	installRuntimeLifecycleTestSeams(t)
	db, err := statedb.Open(os.Getenv(runtimeCrashDBEnv))
	if err != nil {
		t.Fatal(err)
	}
	inst := runtimeCrashInstance(t, db)
	inventoryPath := os.Getenv(runtimeCrashInventoryEnv)
	runtimeCandidateInventoryFn = func(socketName, instanceID string) ([]tmux.RuntimeCandidate, error) {
		candidates, err := readRuntimeCrashCandidates(inventoryPath)
		if err != nil {
			return nil, err
		}
		var matching []tmux.RuntimeCandidate
		for _, candidate := range candidates {
			if candidate.SocketName == socketName && candidate.InstanceID == instanceID {
				matching = append(matching, candidate)
			}
		}
		return matching, nil
	}
	runtimeGenerationCandidateInventoryFn = func(socketName, instanceID string) ([]tmux.RuntimeGenerationCandidate, error) {
		candidates, err := readRuntimeCrashCandidates(inventoryPath)
		if err != nil {
			return nil, err
		}
		var matching []tmux.RuntimeGenerationCandidate
		for _, candidate := range candidates {
			if candidate.SocketName == socketName && candidate.InstanceID == instanceID {
				matching = append(matching, tmux.RuntimeGenerationCandidate{
					SessionID: candidate.SessionID, SessionName: candidate.SessionName,
					SocketName: candidate.SocketName, PaneID: candidate.PaneID, PanePID: candidate.PanePID,
					InstanceID: candidate.InstanceID, Generation: candidate.Generation,
					GenerationKnown: candidate.GenerationKnown,
				})
			}
		}
		return matching, nil
	}
	if stage == RuntimeTransitionAfterRespawnBeforeStamp ||
		stage == RuntimeDestructionAfterReserveBeforeTerminate ||
		stage == RuntimeDestructionAfterTerminateBeforeComplete {
		current := tmux.RuntimeCandidate{
			SessionID: "$7", SessionName: "runtime-g0", SocketName: "isolated",
			PaneID: "%9", PanePID: 3131, InstanceID: "one",
			Generation: 0, GenerationKnown: true,
		}
		if err := writeRuntimeCrashCandidates(inventoryPath, []tmux.RuntimeCandidate{current}); err != nil {
			t.Fatal(err)
		}
	}
	if stage == RuntimeDestructionAfterReserveBeforeTerminate ||
		stage == RuntimeDestructionAfterTerminateBeforeComplete {
		discoverCapturedRuntimeChildrenFn = func(*Instance, tmux.RuntimeGenerationCandidate) ([]tmux.ProcessIdentity, error) {
			return nil, nil
		}
		if stage == RuntimeDestructionAfterReserveBeforeTerminate {
			runtimeDestructionReservedFn = func(statedb.RuntimeState) { os.Exit(runtimeCrashExitCode(stage)) }
		} else {
			terminateCapturedRuntimeFn = func(tmux.RuntimeGenerationCandidate, bool) error {
				if err := writeRuntimeCrashCandidates(inventoryPath, nil); err != nil {
					return err
				}
				os.Exit(runtimeCrashExitCode(stage))
				return nil
			}
		}
		err = inst.KillCaptured(inst.CaptureRuntimeSelection())
		t.Fatalf("crash stage %s was not reached: %v", stage, err)
	}
	authority, winner, err := inst.beginRuntimeTransition(false)
	if err != nil || winner != nil {
		t.Fatalf("begin winner=%v err=%v", winner, err)
	}
	if stage == RuntimeTransitionAfterRespawnBeforeStamp {
		runtimeCandidateRespawnFn = func(_ *tmux.Session, candidate tmux.RuntimeGenerationCandidate, _ string) error {
			if candidate.SessionID != "$7" || candidate.PaneID != "%9" || candidate.PanePID != 3131 {
				return fmt.Errorf("unexpected respawn candidate %#v", candidate)
			}
			candidates, err := readRuntimeCrashCandidates(inventoryPath)
			if err != nil {
				return err
			}
			if len(candidates) != 1 {
				return fmt.Errorf("respawn inventory has %d candidates", len(candidates))
			}
			candidates[0].GenerationKnown = false
			candidates[0].ProofError = "missing AGENTDECK_RUNTIME_GENERATION"
			candidates[0].PanePID = 4242
			return writeRuntimeCrashCandidates(inventoryPath, candidates)
		}
	} else {
		setRuntimeTestCandidate(inst, "runtime-g1")
	}
	runtimeCandidateStampFn = func(_ *tmux.Session, next statedb.RuntimeState, bindingKind, bindingValue string) error {
		return writeRuntimeCrashCandidates(inventoryPath, []tmux.RuntimeCandidate{{
			SessionName: next.TmuxSession, SocketName: next.TmuxSocketName,
			InstanceID: next.InstanceID, Generation: next.Generation, GenerationKnown: true,
			StatusRevision: next.StatusRevision, Status: next.Status,
			LastStartedUnixNano: next.LastStartedAt.UnixNano(), StateKnown: true,
			BindingKind: bindingKind, BindingValue: bindingValue, BindingKnown: true,
			PanePID: 4242,
		}})
	}
	runtimeTransitionFaultFn = func(observed RuntimeTransitionStage, _ statedb.RuntimeState) error {
		if observed == stage {
			os.Exit(runtimeCrashExitCode(stage))
		}
		return nil
	}
	if stage == RuntimeTransitionAfterRespawnBeforeStamp {
		err = inst.respawnRuntimePane(authority, "resume-command")
	} else {
		_, _, _, err = inst.commitPhysicalRuntime(authority)
	}
	authority.close()
	t.Fatalf("crash stage %s was not reached: %v", stage, err)
}

func seedRuntimeCrashDB(t *testing.T) (string, *statedb.StateDB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := statedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveInstance(&statedb.InstanceRow{
		ID: "one", Title: "one", ProjectPath: "/tmp/one", GroupPath: "my-sessions",
		Tool: "claude", Status: "idle", TmuxSession: "runtime-g0", TmuxSocketName: "isolated",
		CreatedAt: time.Unix(1, 0).UTC(), ToolData: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	return path, db
}

func runRuntimeCrashProcess(t *testing.T, stage RuntimeTransitionStage, dbPath, inventoryPath string) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestRuntimeLifecycle_MultiprocessCrashWindows$")
	cmd.Env = append(os.Environ(),
		"AGENTDECK_RUNTIME_LIFECYCLE_ONLY=1",
		"TMPDIR="+filepath.Dir(inventoryPath),
		runtimeCrashStageEnv+"="+string(stage),
		runtimeCrashDBEnv+"="+dbPath,
		runtimeCrashInventoryEnv+"="+inventoryPath,
	)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	err = cmd.Run()
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != runtimeCrashExitCode(stage) {
		t.Fatalf("crash helper stage %s exited with %v, want %d\n%s", stage, err, runtimeCrashExitCode(stage), output.String())
	}
}

type runtimeCrashObserver struct {
	stampCalls int
	sweepCalls int
	killed     []string
	err        error
}

func installRuntimeCrashParentSeams(t *testing.T, inventoryPath string) *runtimeCrashObserver {
	t.Helper()
	installRuntimeLifecycleTestSeams(t)
	observer := &runtimeCrashObserver{}
	runtimeCandidateInventoryFn = func(socketName, instanceID string) ([]tmux.RuntimeCandidate, error) {
		candidates, err := readRuntimeCrashCandidates(inventoryPath)
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
	runtimeCandidateStampFn = func(*tmux.Session, statedb.RuntimeState, string, string) error {
		observer.stampCalls++
		return nil
	}
	runtimeDuplicateSweepFn = func(inst *Instance, _ ...string) {
		observer.sweepCalls++
		candidates, err := readRuntimeCrashCandidates(inventoryPath)
		if err != nil {
			observer.err = err
			return
		}
		state := inst.runtimeStateSnapshot()
		survivors := make([]tmux.RuntimeCandidate, 0, len(candidates))
		for _, candidate := range candidates {
			provedLower := candidate.InstanceID == inst.ID && candidate.GenerationKnown &&
				candidate.PanePID > 0 && candidate.ProofError == "" && candidate.Generation < state.Generation
			if provedLower {
				observer.killed = append(observer.killed, candidate.SessionName)
				continue
			}
			survivors = append(survivors, candidate)
		}
		observer.err = writeRuntimeCrashCandidates(inventoryPath, survivors)
	}
	return observer
}

func TestRuntimeLifecycle_MultiprocessCrashWindows(t *testing.T) {
	if stageText := os.Getenv(runtimeCrashStageEnv); stageText != "" {
		stage := RuntimeTransitionStage(stageText)
		if stage != RuntimeTransitionAfterRespawnBeforeStamp &&
			stage != RuntimeTransitionAfterStampBeforeCommit && stage != RuntimeTransitionAfterCommitBeforeSweep &&
			stage != RuntimeDestructionAfterReserveBeforeTerminate &&
			stage != RuntimeDestructionAfterTerminateBeforeComplete {
			t.Fatalf("unknown crash stage %q", stageText)
		}
		runRuntimeCrashChild(t, stage)
		return
	}

	t.Run("destruction reservation crash recovers live runtime", func(t *testing.T) {
		dbPath, db := seedRuntimeCrashDB(t)
		inventoryPath := filepath.Join(t.TempDir(), "candidates.json")
		runRuntimeCrashProcess(t, RuntimeDestructionAfterReserveBeforeTerminate, dbPath, inventoryPath)
		reserved, found, err := db.ReadRuntimeState("one")
		if err != nil || !found || !statedb.IsRuntimeDestructionReserved(reserved) {
			t.Fatalf("reserved state=%#v found=%v err=%v", reserved, found, err)
		}
		installRuntimeCrashParentSeams(t, inventoryPath)
		result, err := runtimeCrashInstance(t, db).ReconcileRuntime()
		if err != nil {
			t.Fatal(err)
		}
		if !result.Live || result.Adopted || result.State.Status != string(StatusIdle) ||
			result.State.StatusRevision != reserved.StatusRevision+1 {
			t.Fatalf("live reservation recovery result=%#v", result)
		}
		if row, err := db.LoadInstanceByID("one"); err != nil || row == nil || row.Status != string(StatusIdle) {
			t.Fatalf("live recovery removed row: row=%#v err=%v", row, err)
		}
	})

	t.Run("destruction termination crash recovers stopped runtime", func(t *testing.T) {
		dbPath, db := seedRuntimeCrashDB(t)
		inventoryPath := filepath.Join(t.TempDir(), "candidates.json")
		runRuntimeCrashProcess(t, RuntimeDestructionAfterTerminateBeforeComplete, dbPath, inventoryPath)
		reserved, found, err := db.ReadRuntimeState("one")
		if err != nil || !found || !statedb.IsRuntimeDestructionReserved(reserved) {
			t.Fatalf("reserved state=%#v found=%v err=%v", reserved, found, err)
		}
		installRuntimeCrashParentSeams(t, inventoryPath)
		result, err := runtimeCrashInstance(t, db).ReconcileRuntime()
		if err != nil {
			t.Fatal(err)
		}
		if result.Live || result.Adopted || result.State.Status != string(StatusStopped) ||
			result.State.StatusRevision != reserved.StatusRevision+1 {
			t.Fatalf("stopped reservation recovery result=%#v", result)
		}
		if row, err := db.LoadInstanceByID("one"); err != nil || row == nil {
			t.Fatalf("stopped recovery removed row: row=%#v err=%v", row, err)
		}
	})

	t.Run("mid-respawn crash preserves incomplete candidate", func(t *testing.T) {
		dbPath, db := seedRuntimeCrashDB(t)
		inventoryPath := filepath.Join(t.TempDir(), "candidates.json")
		runRuntimeCrashProcess(t, RuntimeTransitionAfterRespawnBeforeStamp, dbPath, inventoryPath)
		before, found, err := db.ReadRuntimeState("one")
		if err != nil || !found || before.Generation != 0 || before.TmuxSession != "runtime-g0" {
			t.Fatalf("mid-respawn durable state=%#v found=%v err=%v", before, found, err)
		}
		observer := installRuntimeCrashParentSeams(t, inventoryPath)
		restarted := runtimeCrashInstance(t, db)
		_, err = restarted.ReconcileRuntime()
		var ambiguity *RuntimeReconciliationAmbiguityError
		if !errors.As(err, &ambiguity) || len(ambiguity.Candidates) != 1 {
			t.Fatalf("mid-respawn reconciliation error = %v", err)
		}
		after, found, readErr := db.ReadRuntimeState("one")
		remaining, inventoryErr := readRuntimeCrashCandidates(inventoryPath)
		if readErr != nil || !found || after != before || inventoryErr != nil ||
			observer.err != nil || observer.stampCalls != 0 || observer.sweepCalls != 0 || len(observer.killed) != 0 ||
			len(remaining) != 1 || remaining[0].GenerationKnown || remaining[0].PanePID != 4242 {
			t.Fatalf("durable=%#v found=%v readErr=%v stamp=%d sweep=%d killed=%v remaining=%#v observerErr=%v inventoryErr=%v",
				after, found, readErr, observer.stampCalls, observer.sweepCalls, observer.killed, remaining, observer.err, inventoryErr)
		}
	})

	t.Run("precommit unique candidate is adopted", func(t *testing.T) {
		dbPath, db := seedRuntimeCrashDB(t)
		inventoryPath := filepath.Join(t.TempDir(), "candidates.json")
		runRuntimeCrashProcess(t, RuntimeTransitionAfterStampBeforeCommit, dbPath, inventoryPath)
		before, found, err := db.ReadRuntimeState("one")
		if err != nil || !found || before.Generation != 0 {
			t.Fatalf("precommit durable state=%#v found=%v err=%v", before, found, err)
		}
		observer := installRuntimeCrashParentSeams(t, inventoryPath)
		restarted := runtimeCrashInstance(t, db)
		result, err := restarted.ReconcileRuntime()
		if err != nil {
			t.Fatal(err)
		}
		after, found, readErr := db.ReadRuntimeState("one")
		candidates, inventoryErr := readRuntimeCrashCandidates(inventoryPath)
		if readErr != nil || !found || after.Generation != 1 || after.TmuxSession != "runtime-g1" ||
			!result.Live || !result.Adopted || result.State.Generation != 1 {
			t.Fatalf("result=%#v durable=%#v found=%v err=%v", result, after, found, readErr)
		}
		if observer.err != nil || inventoryErr != nil || observer.stampCalls != 0 || len(observer.killed) != 0 || len(candidates) != 1 {
			t.Fatalf("stamp=%d sweep=%d killed=%v candidates=%v observerErr=%v inventoryErr=%v",
				observer.stampCalls, observer.sweepCalls, observer.killed, candidates, observer.err, inventoryErr)
		}
	})

	t.Run("postcommit winner retains only proved lower sweep", func(t *testing.T) {
		dbPath, db := seedRuntimeCrashDB(t)
		inventoryPath := filepath.Join(t.TempDir(), "candidates.json")
		runRuntimeCrashProcess(t, RuntimeTransitionAfterCommitBeforeSweep, dbPath, inventoryPath)
		candidates, err := readRuntimeCrashCandidates(inventoryPath)
		if err != nil || len(candidates) != 1 {
			t.Fatalf("read committed candidate: %v, %v", candidates, err)
		}
		candidates = append(candidates, tmux.RuntimeCandidate{
			SessionName: "runtime-g0", SocketName: "isolated", InstanceID: "one",
			Generation: 0, GenerationKnown: true, PanePID: 3131,
		})
		if err := writeRuntimeCrashCandidates(inventoryPath, candidates); err != nil {
			t.Fatal(err)
		}
		observer := installRuntimeCrashParentSeams(t, inventoryPath)
		restarted := runtimeCrashInstance(t, db)
		result, err := restarted.ReconcileRuntime()
		if err != nil {
			t.Fatal(err)
		}
		after, found, readErr := db.ReadRuntimeState("one")
		remaining, inventoryErr := readRuntimeCrashCandidates(inventoryPath)
		if readErr != nil || !found || after.Generation != 1 || after.TmuxSession != "runtime-g1" ||
			!result.Live || result.Adopted || result.State.Generation != 1 {
			t.Fatalf("result=%#v durable=%#v found=%v err=%v", result, after, found, readErr)
		}
		if observer.err != nil || inventoryErr != nil || observer.stampCalls != 0 || observer.sweepCalls != 1 ||
			len(observer.killed) != 1 || observer.killed[0] != "runtime-g0" || len(remaining) != 1 || remaining[0].SessionName != "runtime-g1" {
			t.Fatalf("stamp=%d sweep=%d killed=%v remaining=%v observerErr=%v inventoryErr=%v",
				observer.stampCalls, observer.sweepCalls, observer.killed, remaining, observer.err, inventoryErr)
		}
	})

	t.Run("precommit ambiguity is preserved", func(t *testing.T) {
		dbPath, db := seedRuntimeCrashDB(t)
		inventoryPath := filepath.Join(t.TempDir(), "candidates.json")
		runRuntimeCrashProcess(t, RuntimeTransitionAfterStampBeforeCommit, dbPath, inventoryPath)
		candidates, err := readRuntimeCrashCandidates(inventoryPath)
		if err != nil || len(candidates) != 1 {
			t.Fatalf("read precommit candidate: %v, %v", candidates, err)
		}
		competing := candidates[0]
		competing.SessionName = "runtime-g1-competing"
		competing.PanePID = 5252
		candidates = append(candidates, competing)
		if err := writeRuntimeCrashCandidates(inventoryPath, candidates); err != nil {
			t.Fatal(err)
		}
		observer := installRuntimeCrashParentSeams(t, inventoryPath)
		restarted := runtimeCrashInstance(t, db)
		_, err = restarted.ReconcileRuntime()
		var ambiguity *RuntimeReconciliationAmbiguityError
		if !errors.As(err, &ambiguity) || len(ambiguity.Candidates) != 2 {
			t.Fatalf("error=%v", err)
		}
		after, found, readErr := db.ReadRuntimeState("one")
		remaining, inventoryErr := readRuntimeCrashCandidates(inventoryPath)
		if readErr != nil || !found || after.Generation != 0 || observer.err != nil || inventoryErr != nil ||
			observer.stampCalls != 0 || observer.sweepCalls != 0 || len(observer.killed) != 0 || len(remaining) != 2 {
			t.Fatalf("durable=%#v found=%v readErr=%v stamp=%d sweep=%d killed=%v remaining=%v observerErr=%v inventoryErr=%v",
				after, found, readErr, observer.stampCalls, observer.sweepCalls, observer.killed, remaining, observer.err, inventoryErr)
		}
	})
}
