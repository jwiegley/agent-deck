package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
	"github.com/stretchr/testify/require"
)

func TestNativeHarnessSwitch_RuntimeAuthority(t *testing.T) {
	for _, outcome := range []string{"success", "partial", "target failure", "outcome write failure"} {
		t.Run(outcome, func(t *testing.T) {
			home := withTempAgentDeckHome(t, `
[profiles.personal.claude]
config_dir = "~/.claude-personal"
[profiles.work.claude]
config_dir = "~/.claude-work"
`)
			cfg, err := LoadUserConfig()
			require.NoError(t, err)
			project := filepath.Join(home, "project")
			require.NoError(t, os.MkdirAll(project, 0o700))
			const sid = "11111111-2222-3333-4444-555555555555"
			sourcePath := filepath.Join(home, ".claude-personal", "projects", ConvertToClaudeDirName(project), sid+".jsonl")
			initial := []byte(`{"sessionId":"` + sid + `","type":"user","message":"before stop"}` + "\n")
			final := append(append([]byte(nil), initial...), []byte(`{"sessionId":"`+sid+`","type":"assistant","message":"flushed on stop"}`+"\n")...)
			require.NoError(t, os.MkdirAll(filepath.Dir(sourcePath), 0o700))
			require.NoError(t, os.WriteFile(sourcePath, initial, 0o600))

			installRuntimeLifecycleTestSeams(t)
			storage := newTestStorage(t)
			inst := &Instance{ID: "native-runtime", Title: "source", ProjectPath: project, GroupPath: "test", Tool: "claude", Command: "claude", Account: "personal", ClaudeSessionID: sid, Status: StatusWaiting, CreatedAt: time.Now(), TmuxSocketName: "isolated", tmuxSession: &tmux.Session{Name: "runtime-g0", SocketName: "isolated", InstanceID: "native-runtime"}}
			require.NoError(t, storage.Save([]*Instance{inst}))
			selection := inst.CaptureRuntimeSelection()
			installRuntimeDeletionCandidateSeams(t, selection.State)
			oldTerminate, oldRunning, oldStart, oldWrite := terminateCapturedRuntimeFn, nativeSwitchRunning, nativeSwitchStart, harnessSwitchJournalWrite
			t.Cleanup(func() {
				terminateCapturedRuntimeFn, nativeSwitchRunning, nativeSwitchStart, harnessSwitchJournalWrite = oldTerminate, oldRunning, oldStart, oldWrite
			})
			killed, contenderObserved := make(chan struct{}), make(chan struct{})
			var killedOnce sync.Once
			var observations atomic.Int32
			runtimeTransitionObservedFn = func() {
				if observations.Add(1) == 2 {
					close(contenderObserved)
				}
			}
			running := true
			nativeSwitchRunning = func(*Instance) bool { return running }
			terminateCapturedRuntimeFn = func(candidate tmux.RuntimeGenerationCandidate, wait bool) error {
				require.True(t, wait, "final transcript export requires synchronous termination")
				require.True(t, runtimeGenerationCandidateMatchesState(candidate, selection.State))
				running = false
				require.NoError(t, os.WriteFile(sourcePath, final, 0o600))
				killedOnce.Do(func() { close(killed) })
				return nil
			}
			externalDone := make(chan error, 1)
			go func() {
				<-killed
				_, err := inst.RestartRuntime()
				externalDone <- err
			}()
			starts := 0
			nativeSwitchStart = func(i *Instance, authority *runtimeTransitionAuthority) error {
				starts++
				select {
				case <-contenderObserved:
				case <-time.After(5 * time.Second):
					t.Fatal("competing restart did not reach lifecycle lock")
				}
				select {
				case err := <-externalDone:
					t.Fatalf("restart interleaved before target spawn: %v", err)
				default:
				}
				stored, err := storage.db.LoadInstances()
				require.NoError(t, err)
				require.Equal(t, i.Account, stored[0].Account, "account must persist before either target or rollback spawn")
				if starts == 1 {
					require.Equal(t, "work", i.Account)
					destination := filepath.Join(home, ".claude-work", "projects", ConvertToClaudeDirName(project), sid+".jsonl")
					require.Equal(t, final, mustReadSwitchFixture(t, destination))
					if outcome == "target failure" {
						return errors.New("target spawn failed")
					}
				}
				setRuntimeTestCandidate(i, "runtime-g1")
				runtime, plan, committed, err := i.commitPhysicalRuntime(authority)
				require.NoError(t, err)
				require.True(t, committed)
				if outcome == "partial" {
					return &RestartPartialSuccessError{InstanceID: i.ID, Runtime: runtime, BindingPlan: plan, Err: errors.New("post-spawn cleanup failed")}
				}
				return nil
			}
			harnessSwitchJournalWrite = func(path string, journal *switchJournal) error {
				if outcome == "outcome write failure" && journal.State == switchCommitted {
					return errors.New("outcome receipt unavailable")
				}
				return oldWrite(path, journal)
			}

			result, err := ExecuteHarnessSwitch(cfg, inst, HarnessSwitchOptions{Target: SwitchPreviewTarget{Harness: "claude", Account: "work"}})
			select {
			case externalErr := <-externalDone:
				require.NoError(t, externalErr)
			case <-time.After(5 * time.Second):
				t.Fatal("competing restart remained blocked after switch")
			}
			if outcome == "target failure" {
				require.ErrorContains(t, err, "target spawn failed")
				require.Equal(t, 2, starts)
				require.Equal(t, "personal", inst.Account)
				return
			}
			require.Equal(t, 1, starts, "partial success must never respawn or roll back")
			require.Equal(t, "work", inst.Account)
			require.True(t, result.Committed)
			require.False(t, result.DestinationReady, "runtime publication is not native readiness")
			if outcome == "outcome write failure" {
				require.ErrorContains(t, err, "outcome receipt unavailable")
				_, retryErr := ExecuteHarnessSwitch(cfg, inst, HarnessSwitchOptions{Target: SwitchPreviewTarget{Harness: "claude", Account: "personal"}})
				require.ErrorContains(t, retryErr, "unresolved prior operation")
				require.Equal(t, 1, starts, "uncertain launch must not be replayed")
				return
			}
			require.NoError(t, err)
			if outcome == "partial" {
				require.NotEmpty(t, result.Warnings)
			}
			require.NoError(t, storage.CommitNativeHarnessSwitch(inst, result))
		})
	}
}

// A status observation is liveness, not identity (#2344). Whether the TUI
// poller commits it in-process or another process (the transition daemon, a
// one-pass CLI observation) commits it through the same revision CAS, it must
// not cancel a confirmed native account switch: not before the switch starts,
// which leaves the in-memory runtime trailing the durable row; not between the
// runtime capture and transition authority; not while the source is staged
// before its stop; and not between the stop's final read and its destruction
// reservation. A replaced physical runtime must still refuse.
func TestNativeHarnessSwitch_StatusObservationDoesNotCancelSwitch(t *testing.T) {
	const (
		beforeSwitch       = "before switch"
		beforeAuthority    = "before authority"
		beforeStop         = "before stop"
		atReservation      = "at reservation"
		replacedGeneration = "replaced generation"
	)
	for _, tc := range []struct {
		tool, window string
		poller       bool // the in-process TUI poller rather than another process
	}{
		{tool: "claude", window: beforeSwitch},
		{tool: "claude", window: beforeAuthority},
		{tool: "claude", window: beforeAuthority, poller: true},
		{tool: "claude", window: beforeStop},
		{tool: "claude", window: beforeStop, poller: true},
		{tool: "claude", window: atReservation},
		{tool: "codex", window: beforeSwitch},
		{tool: "codex", window: beforeStop, poller: true},
		{tool: "claude", window: replacedGeneration},
	} {
		source := "other process"
		if tc.poller {
			source = "tui poller"
		}
		t.Run(tc.tool+"/"+source+"/"+tc.window, func(t *testing.T) {
			home := withTempAgentDeckHome(t, `
[profiles.personal.claude]
config_dir = "~/.claude-personal"
[profiles.work.claude]
config_dir = "~/.claude-work"
[profiles.personal.codex]
config_dir = "~/.codex-personal"
[profiles.work.codex]
config_dir = "~/.codex-work"
`)
			cfg, err := LoadUserConfig()
			require.NoError(t, err)
			project := filepath.Join(home, "project")
			require.NoError(t, os.MkdirAll(project, 0o700))
			const sid = "11111111-2222-3333-4444-555555555555"
			inst := &Instance{ID: "native-status-tick", Title: "source", ProjectPath: project, GroupPath: "test", Tool: tc.tool, Command: tc.tool, Account: "personal", Status: StatusWaiting, CreatedAt: time.Now(), TmuxSocketName: "isolated", tmuxSession: &tmux.Session{Name: "runtime-g0", SocketName: "isolated", InstanceID: "native-status-tick"}}
			sourcePath := filepath.Join(home, ".claude-personal", "projects", ConvertToClaudeDirName(project), sid+".jsonl")
			transcript := []byte(`{"sessionId":"` + sid + `","type":"user","message":"hi"}` + "\n")
			inst.ClaudeSessionID = sid
			if tc.tool == "codex" {
				inst.ClaudeSessionID, inst.CodexSessionID = "", sid
				sourcePath = filepath.Join(home, ".codex-personal", "sessions", "2026", "09", "28", "rollout-2026-09-28T00-00-00-"+sid+".jsonl")
				transcript = []byte(`{"type":"session_meta","payload":{"id":"` + sid + `"}}` + "\n")
			}
			require.NoError(t, os.MkdirAll(filepath.Dir(sourcePath), 0o700))
			require.NoError(t, os.WriteFile(sourcePath, transcript, 0o600))

			installRuntimeLifecycleTestSeams(t)
			storage := newTestStorage(t)
			require.NoError(t, storage.Save([]*Instance{inst}))
			selection := inst.CaptureRuntimeSelection()
			installRuntimeDeletionCandidateSeams(t, selection.State)
			oldTerminate, oldRunning, oldStart, oldStop, oldInventory := terminateCapturedRuntimeFn, nativeSwitchRunning, nativeSwitchStart, nativeSwitchStop, runtimeGenerationCandidateInventoryFn
			t.Cleanup(func() {
				terminateCapturedRuntimeFn, nativeSwitchRunning, nativeSwitchStart, nativeSwitchStop, runtimeGenerationCandidateInventoryFn = oldTerminate, oldRunning, oldStart, oldStop, oldInventory
			})
			setStatusProbeOverride(t, func(context.Context, *Instance, statedb.RuntimeState) (Status, error) {
				return StatusIdle, nil
			})

			// One status commit through the CAS that the TUI poller, the
			// transition daemon and a one-pass CLI observation all use.
			ticks := 0
			tick := func() {
				if ticks > 0 {
					return
				}
				ticks++
				if tc.poller {
					observed := inst.runtimeStateSnapshot()
					next, err := inst.UpdateStatusObserved(context.Background(), observed, inst.PersistenceIncarnation())
					require.NoError(t, err)
					require.Equal(t, observed.StatusRevision+1, next.StatusRevision, "the poller tick did not commit")
					return
				}
				durable, found, err := storage.db.ReadRuntimeState(inst.ID)
				require.NoError(t, err)
				require.True(t, found)
				applied, err := storage.db.WriteStatusIfVersion(inst.ID, selection.Incarnation, durable.Generation, durable.StatusRevision, string(StatusIdle))
				require.NoError(t, err)
				require.True(t, applied, "the other process's tick did not commit")
			}
			stopping := false
			switch tc.window {
			case beforeSwitch:
				tick()
				require.Equal(t, selection.State, inst.CaptureRuntimeSelection().State, "memory must trail the durable row")
			case beforeAuthority:
				runtimeTransitionObservedFn = tick
			case beforeStop:
				nativeSwitchStop = func(i *Instance, a *runtimeTransitionAuthority) error {
					tick()
					return oldStop(i, a)
				}
			case atReservation:
				nativeSwitchStop = func(i *Instance, a *runtimeTransitionAuthority) error {
					stopping = true
					return oldStop(i, a)
				}
				inventory := runtimeGenerationCandidateInventoryFn
				runtimeGenerationCandidateInventoryFn = func(socketName, instanceID string) ([]tmux.RuntimeGenerationCandidate, error) {
					if stopping {
						tick()
					}
					return inventory(socketName, instanceID)
				}
			case replacedGeneration:
				// Another process restarted the source after it was loaded.
				binding, found, err := storage.db.ReadRuntimeBinding(inst.ID, "claude")
				require.NoError(t, err)
				require.True(t, found)
				next := selection.State
				next.Generation++
				next.StatusRevision = 0
				next.TmuxSession = "runtime-g1-elsewhere"
				next.Status = string(StatusRunning)
				require.NoError(t, storage.db.CommitRuntimeTransitionWithBindingPlan(selection.State.Generation, selection.Incarnation, next, []statedb.RuntimeBindingTransition{{
					Kind: "claude", ExpectedRevision: binding.Revision, NextValue: binding.Value, DetectedAt: binding.DetectedAt,
				}}))
				installRuntimeDeletionCandidateSeams(t, next)
			}

			running := true
			terminated := 0
			nativeSwitchRunning = func(*Instance) bool { return running }
			terminateCapturedRuntimeFn = func(candidate tmux.RuntimeGenerationCandidate, wait bool) error {
				require.True(t, runtimeGenerationCandidateMatchesState(candidate, selection.State))
				terminated++
				running = false
				return nil
			}
			nativeSwitchStart = func(i *Instance, authority *runtimeTransitionAuthority) error {
				setRuntimeTestCandidate(i, "runtime-g1")
				_, _, committed, err := i.commitPhysicalRuntime(authority)
				require.NoError(t, err)
				require.True(t, committed)
				return nil
			}

			result, err := ExecuteHarnessSwitch(cfg, inst, HarnessSwitchOptions{Target: SwitchPreviewTarget{Harness: tc.tool, Account: "work"}, Storage: storage})
			if tc.window == replacedGeneration {
				require.ErrorContains(t, err, "native switch source changed before runtime authority was acquired", "a replaced runtime is not the source the operator confirmed")
				require.Zero(t, terminated, "the replacement runtime must not be stopped")
				require.Equal(t, "personal", inst.Account)
				return
			}
			require.NoError(t, err, "a status observation cancelled the confirmed switch")
			require.Equal(t, 1, ticks, "the status observation was never committed")
			require.True(t, result.Committed)
			require.Equal(t, 1, terminated, "the source must be stopped exactly once")
			require.Equal(t, "work", inst.Account)
			stored, err := storage.db.LoadInstances()
			require.NoError(t, err)
			require.Equal(t, "work", stored[0].Account)
		})
	}
}

func TestCrossHarnessStorageAdapter_PreservesObservedRuntimeBinding(t *testing.T) {
	storage := newTestStorage(t)
	source := &Instance{ID: "source", Tool: "claude", ClaudeSessionID: "source-native", ProjectPath: t.TempDir(), Title: "source", GroupPath: "test"}
	target := &Instance{ID: "target", Tool: "codex", ProjectPath: source.ProjectPath, Title: "target", GroupPath: "test", ArchivedAt: time.Now(), Supersedes: source.ID}
	require.NoError(t, storage.Save([]*Instance{source}))
	adapter := StorageCrossHarnessTargetStore{Storage: storage}
	require.NoError(t, adapter.CreateTarget(target))
	observation := target.CaptureRuntimeBindingObservation("codex")
	require.NoError(t, target.PublishRuntimeBindingObservation(observation, "observed-target-native", time.Now()))
	target.Notes = "saved after target start"
	require.NoError(t, adapter.SaveTarget(target))
	loaded, err := adapter.LoadTarget(target.ID)
	require.NoError(t, err)
	require.Equal(t, "observed-target-native", loaded.CodexSessionID)
	require.Equal(t, target.Notes, loaded.Notes)
	require.NoError(t, adapter.FinalizeTargetHandoff(source, loaded))
	loaded, err = adapter.LoadTarget(target.ID)
	require.NoError(t, err)
	require.True(t, loaded.ArchivedAt.IsZero())
	require.Equal(t, "observed-target-native", loaded.CodexSessionID)
}

func TestNativeHarnessSwitch_RejectsRecreatedIncarnation(t *testing.T) {
	storage := newTestStorage(t)
	stale := &Instance{ID: "recreated-switch", Tool: "claude", Account: "personal", ClaudeSessionID: "native", ProjectPath: t.TempDir()}
	require.NoError(t, storage.InsertSessionAndVerify(stale, nil))
	result := nativeHarnessCommitResult(stale, "work")
	require.NoError(t, storage.db.DeleteInstance(stale.ID))
	winner := &Instance{ID: stale.ID, Tool: stale.Tool, Account: stale.Account, ClaudeSessionID: stale.ClaudeSessionID, ProjectPath: stale.ProjectPath}
	require.NoError(t, storage.InsertSessionAndVerify(winner, nil))
	require.ErrorContains(t, storage.CommitNativeHarnessSwitch(stale, result), "source identity conflict")
	result.nativeStorageAcknowledgement = true
	result.nativeTarget.Account = winner.Account
	require.ErrorContains(t, storage.CommitNativeHarnessSwitch(stale, result), "target identity conflict")
	rows, err := storage.db.LoadInstances()
	require.NoError(t, err)
	require.Equal(t, "personal", rows[0].Account)
}

func TestCrossHarnessStorageAdapter_RejectsRecreatedIncarnations(t *testing.T) {
	for _, recreated := range []string{"source", "target"} {
		t.Run(recreated, func(t *testing.T) {
			storage := newTestStorage(t)
			source := &Instance{ID: "source", Tool: "claude", ClaudeSessionID: "source-native", ProjectPath: t.TempDir()}
			target := &Instance{ID: "target", Tool: "codex", CodexSessionID: "target-native", ProjectPath: source.ProjectPath, ArchivedAt: time.Now(), Supersedes: source.ID}
			require.NoError(t, storage.Save([]*Instance{source, target}))
			stale := source
			if recreated == "target" {
				stale = target
			}
			require.NoError(t, storage.db.DeleteInstance(stale.ID))
			winner := &Instance{ID: stale.ID, Tool: stale.Tool, ClaudeSessionID: stale.ClaudeSessionID, CodexSessionID: stale.CodexSessionID, ProjectPath: stale.ProjectPath, ArchivedAt: stale.ArchivedAt, Supersedes: stale.Supersedes}
			require.NoError(t, storage.InsertSessionAndVerify(winner, nil))
			adapter := StorageCrossHarnessTargetStore{Storage: storage}
			require.ErrorContains(t, adapter.FinalizeTargetHandoff(source, target), recreated+" identity conflict")
			loadedSource, err := adapter.LoadTarget(source.ID)
			require.NoError(t, err)
			require.True(t, loadedSource.ArchivedAt.IsZero())
			loadedTarget, err := adapter.LoadTarget(target.ID)
			require.NoError(t, err)
			require.False(t, loadedTarget.ArchivedAt.IsZero())
		})
	}
}
