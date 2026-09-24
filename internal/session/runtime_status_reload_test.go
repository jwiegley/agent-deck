package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

func TestRuntimeLifecycle_StatusReloadDefersDuringPhysicalReplacement(t *testing.T) {
	for _, duringProbe := range []bool{false, true} {
		name := "before-probe"
		if duringProbe {
			name = "after-probe"
		}
		t.Run(name, func(t *testing.T) {
			installRuntimeLifecycleTestSeams(t)
			// Use the real shared lock for both transition and nonblocking probe.
			instanceSpawnLockAcquireFn = defaultAcquireInstanceSpawnLock
			db, inst := runtimeLifecycleTestDB(t, "pi", nil)
			observed := inst.RuntimeState()
			incarnation := inst.PersistenceIncarnation()
			probeEntered, releaseProbe := make(chan struct{}), make(chan struct{})
			setStatusProbeOverride(t, func(context.Context, *Instance, statedb.RuntimeState) (Status, error) {
				close(probeEntered)
				<-releaseProbe
				return StatusStopped, nil
			})
			type result struct {
				state statedb.RuntimeState
				err   error
			}
			statusDone := make(chan result, 1)
			finished := make(chan struct{})
			started := false
			probe := func() {
				defer close(finished)
				state, err := inst.UpdateStatusObserved(context.Background(), observed, incarnation)
				statusDone <- result{state, err}
			}
			t.Cleanup(func() {
				select {
				case <-releaseProbe:
				default:
					close(releaseProbe)
				}
				if started {
					select {
					case <-finished:
					case <-time.After(5 * time.Second):
						t.Error("status probe did not finish")
					}
				}
			})
			if duringProbe {
				started = true
				go probe()
				<-probeEntered
			}
			authority, winner, err := inst.beginRuntimeTransition(false)
			if err != nil || winner != nil {
				t.Fatalf("begin transition: winner=%v err=%v", winner, err)
			}
			defer authority.close()
			// Fallback recreation has succeeded, but the new pane has not yet
			// been stamped or committed. The old durable tmux name is absent.
			setRuntimeTestCandidate(inst, "replacement")
			runtimeCandidateExistsFn = func(s *tmux.Session) bool { return s.Name == "replacement" }
			stamped := ""
			runtimeCandidateStampFn = func(s *tmux.Session, _ statedb.RuntimeState, _, _ string) error {
				stamped = s.Name
				return nil
			}
			if duringProbe {
				close(releaseProbe)
			} else {
				started = true
				go probe()
			}
			select {
			case got := <-statusDone:
				if !errors.Is(got.err, statedb.ErrRuntimeGenerationConflict) || got.state.TmuxSession != "replacement" {
					t.Fatalf("status recovery restored predecessor during spawn: state=%+v err=%v", got.state, got.err)
				}
			case <-time.After(time.Second):
				t.Fatal("status recovery blocked behind physical transition")
			}
			next, _, committed, err := inst.commitPhysicalRuntime(authority)
			if err != nil || !committed || stamped != "replacement" {
				t.Fatalf("physical publication: stamped=%q committed=%v err=%v", stamped, committed, err)
			}
			authority.close()
			recovered, err := inst.UpdateStatusObserved(context.Background(), observed, incarnation)
			if !errors.Is(err, statedb.ErrRuntimeGenerationConflict) || !sameStatusRuntime(recovered, next) {
				t.Fatalf("status recovery: state=%+v err=%v; want committed replacement %+v", recovered, err, next)
			}
			durable, found, err := db.ReadRuntimeState(inst.ID)
			if err != nil || !found || !sameStatusRuntime(durable, next) || !sameStatusRuntime(inst.RuntimeState(), next) {
				t.Fatalf("replacement lost: durable=%+v memory=%+v err=%v", durable, inst.RuntimeState(), err)
			}
		})
	}
}

func TestRuntimeLifecycle_ReloadToolMetadataDiscardsOldDetection(t *testing.T) {
	for _, deferred := range []bool{false, true} {
		name := "reload"
		if deferred {
			name = "deferred"
		}
		t.Run(name, func(t *testing.T) {
			canonical := &Instance{
				ID: "one", Tool: "claude", Command: "claude", Status: StatusStopped,
				RuntimeGeneration: 2, StatusRevision: 4, TmuxSocketName: "isolated",
				tmuxSession: tmux.ReconnectSessionLazy("runtime", "one", "", "claude", "stopped"),
				hookStatus:  "running", hookEvent: "UserPromptSubmit", hookSessionID: "old-claude-id",
				hookLastUpdate: time.Now(),
			}
			canonical.tmuxSession.SocketName = "isolated"
			if got := canonical.tmuxSession.DetectTool(); got != "claude" {
				t.Fatalf("prime detection: %q", got)
			}
			state := canonical.RuntimeState()
			loaded := &Instance{ID: "one", Tool: "codex", Command: "codex"}
			loaded.adoptRuntimeState(state)
			if deferred {
				canonical.MergeDeferredReload(loaded)
			} else {
				canonical.MergeReloaded(loaded)
			}
			canonical.refreshStatusMetadataIfCurrent(state, RuntimeBindingObservation{}, nil)
			if canonical.Tool != "codex" || canonical.tmuxSession.Command != "codex" {
				t.Fatalf("status poll restored old metadata: tool=%q command=%q", canonical.Tool, canonical.tmuxSession.Command)
			}
			if canonical.hookStatus != "" || canonical.hookEvent != "" || canonical.hookSessionID != "" || !canonical.hookLastUpdate.IsZero() {
				t.Fatalf("old hook survived tool change: status=%q event=%q session=%q", canonical.hookStatus, canonical.hookEvent, canonical.hookSessionID)
			}
			if !sameStatusRuntime(canonical.RuntimeState(), state) {
				t.Fatalf("metadata reload changed physical runtime: %+v", canonical.RuntimeState())
			}
		})
	}
}

func TestRuntimeLifecycle_ReloadUnchangedToolPreservesDetection(t *testing.T) {
	canonical := &Instance{
		ID: "one", Tool: "claude", Command: "claude", Status: StatusStopped,
		tmuxSession: tmux.ReconnectSessionLazy("runtime", "one", "", "claude", "stopped"),
		hookStatus:  "waiting", hookSessionID: "conversation",
	}
	prior := canonical.tmuxSession
	loaded := &Instance{ID: "one", Tool: "claude", Command: "claude", Title: "renamed"}
	loaded.adoptRuntimeState(canonical.RuntimeState())
	canonical.MergeReloaded(loaded)
	if canonical.tmuxSession != prior || canonical.hookStatus != "waiting" || canonical.hookSessionID != "conversation" {
		t.Fatal("ordinary metadata reload discarded runtime caches")
	}
}

func TestRuntimeLifecycle_ReloadRejectsInFlightToolDetection(t *testing.T) {
	dir := t.TempDir()
	entered, release := filepath.Join(dir, "entered"), filepath.Join(dir, "release")
	t.Setenv("DETECTION_ENTERED", entered)
	t.Setenv("DETECTION_RELEASE", release)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(`#!/bin/sh
case "$*" in
  *capture-pane*)
    : > "$DETECTION_ENTERED"
    while [ ! -e "$DETECTION_RELEASE" ]; do sleep 0.01; done
    printf 'Claude Code\n'
    ;;
esac
`), 0o755); err != nil {
		t.Fatal(err)
	}
	canonical := &Instance{
		ID: "one", Tool: "claude", Command: "wrapper", Status: StatusStopped,
		tmuxSession: tmux.ReconnectSessionLazy("runtime", "one", "", "wrapper", "stopped"),
	}
	state := canonical.RuntimeState()
	oldWrapper := canonical.tmuxSession
	done := make(chan struct{})
	go func() {
		canonical.refreshStatusMetadataIfCurrent(state, RuntimeBindingObservation{}, nil)
		close(done)
	}()
	t.Cleanup(func() {
		_ = os.WriteFile(release, nil, 0o600)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("tool detector did not finish")
		}
	})
	// Pause the actual detector's pane read after it captured the old wrapper.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(entered); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("old tool detector did not enter capture-pane")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-done:
		t.Fatal("old detector completed before metadata reload")
	default:
	}
	loaded := &Instance{ID: "one", Tool: "codex", Command: "codex"}
	loaded.adoptRuntimeState(state)
	canonical.MergeReloaded(loaded)
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	<-done
	if got := oldWrapper.DetectTool(); got != "claude" {
		t.Fatalf("old wrapper detected %q, want claude", got)
	}
	if canonical.Tool != "codex" {
		t.Fatalf("in-flight old detector restored tool %q after reload", canonical.Tool)
	}
}
