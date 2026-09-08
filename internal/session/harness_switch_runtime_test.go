package session

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
