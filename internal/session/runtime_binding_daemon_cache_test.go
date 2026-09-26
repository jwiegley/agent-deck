package session

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/asheshgoplani/agent-deck/internal/testutil"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

func TestRuntimeLifecycle_TUIAliveDaemonCarriesValidatedHookBinding(t *testing.T) {
	// TestMain intentionally skips package-wide tmux setup for the focused
	// runtime-lifecycle gate. Isolate this test itself in that mode, while
	// retaining TestMain's shared isolated socket during an ordinary suite run.
	if os.Getenv(testutil.TestIsolationMarkerEnv) == "" {
		cleanupTmux := testutil.IsolateTmuxSocket()
		t.Cleanup(cleanupTmux)
	}
	if os.Getenv("AGENTDECK_RUNTIME_LIFECYCLE_ONLY") == "1" {
		installRuntimeLifecycleNoopTmux(t)
	} else {
		skipIfNoTmuxBinary(t)
	}
	const (
		profile   = "_test_runtime_hook_cache_tui"
		instance  = "runtime-hook-cache-tui"
		sessionID = "11111111-2222-3333-4444-555555555555"
	)
	d, storage := bootstrapDaemonProfile(t, profile)
	projectPath := filepath.Join(t.TempDir(), "project")
	tmuxName := tmux.SessionPrefix + instance + "-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if output, err := exec.Command("tmux", "new-session", "-d", "-s", tmuxName, "cat").CombinedOutput(); err != nil {
		t.Fatalf("create isolated tmux session: %v (%s)", err, output)
	}
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", tmuxName).Run() })
	inst := &Instance{
		ID: instance, Title: "cached hook", ProjectPath: projectPath,
		GroupPath: DefaultGroupPath, Command: "claude", Tool: "claude",
		Status: StatusRunning, CreatedAt: time.Unix(1300, 0).UTC(),
	}
	inst.SetTmuxSessionForTest(tmux.ReconnectSessionLazy(
		tmuxName, inst.Title, projectPath, inst.Command, "running"))
	if err := storage.InsertSessionAndVerify(inst, nil); err != nil {
		t.Fatal(err)
	}
	db := storage.GetDB()
	if err := db.RegisterInstance(false); err != nil {
		t.Fatal(err)
	}
	if err := db.Heartbeat(); err != nil {
		t.Fatal(err)
	}
	if alive, err := db.AliveInstanceCount(); err != nil || alive == 0 {
		t.Fatalf("TUI heartbeat not alive: count=%d err=%v", alive, err)
	}
	if err := db.WriteStatus(inst.ID, "running", inst.Tool); err != nil {
		t.Fatal(err)
	}
	seedHookStatusFile(t, inst.ID, "SessionStart", sessionID, "running")

	oldAcquire := instanceSpawnLockAcquireFn
	lockAcquisitions := 0
	instanceSpawnLockAcquireFn = func(string) (func(), error) {
		lockAcquisitions++
		return func() {}, nil
	}
	t.Cleanup(func() { instanceSpawnLockAcquireFn = oldAcquire })

	// The first pass validates and publishes the hook's new binding.
	loaded, _, loadErr := storage.LoadWithGroups()
	if loadErr != nil || len(loaded) != 1 {
		t.Fatalf("pre-sync load: count=%d err=%v", len(loaded), loadErr)
	}
	if !loaded[0].Exists() {
		t.Fatal("hermetic tmux fixture did not preserve the live-session precondition")
	}
	d.syncProfile(profile)
	if lockAcquisitions != 1 {
		t.Fatalf("first pass lock acquisitions = %d, want 1", lockAcquisitions)
	}
	state, ok := d.pollState[profile][inst.ID]
	if !ok || state.identity.claudeSessionID != sessionID {
		t.Fatalf("post-hook polling identity = %+v found=%v", state.identity, ok)
	}
	if cached, ok := state.validatedHookBindings["claude"]; !ok || cached.value != sessionID || !cached.fingerprint.valid() {
		t.Fatalf("validated hook binding = %+v found=%v", cached, ok)
	}

	// Every binding-validation SQLite read is downstream of the spawn lock.
	// No second lock acquisition therefore proves the unchanged hook returned
	// before both the filesystem lock and those durable reads.
	d.syncProfile(profile)
	if lockAcquisitions != 1 {
		t.Fatalf("unchanged second pass acquired binding lock %d times, want 1 total", lockAcquisitions)
	}
}

// The poll-state bridge carries the daemon's caches and throttles across its
// per-pass reload, never the running-flip debounce: livePrior alone carries
// that, and it is dropped while a TUI owns status. A flip held on the last
// no-TUI pass before a TUI started is hours stale when the TUI exits. Restored
// anyway, it skips the one-sample hold a cold instance gives a transient
// capture failure, so the first pass after the TUI exits committed
// running -> error for a pane that was never read as dead.
func TestRuntimeLifecycle_DaemonPollStateDoesNotCarryFlipAcrossTUILifetime(t *testing.T) {
	inboxTestHome(t)
	profile := "_test_runtime_poll_state_flip"
	storage, err := NewStorageWithProfile(profile)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	defer storage.Close()
	inst := &Instance{
		ID: "runtime-poll-state-flip", Title: "worker", ProjectPath: t.TempDir(), GroupPath: DefaultGroupPath,
		Tool: "codex", Status: StatusRunning, CreatedAt: time.Now(),
	}
	if err := storage.SaveWithGroups([]*Instance{inst}, nil); err != nil {
		t.Fatalf("save: %v", err)
	}
	db := storage.GetDB()
	if db == nil {
		t.Fatal("no state db")
	}

	// The candidate probe stands in for probeStatusCandidate's tmux path with
	// the instance's carried debounce state: a readable sample goes through
	// the live-prior debounce, and a capture failure through the hold on the
	// persisted status that the capture-error branch applies.
	sample, captureFails := StatusRunning, false
	orig := statusProbeCandidateOverride
	t.Cleanup(func() { statusProbeCandidateOverride = orig })
	statusProbeCandidateOverride = func(_ context.Context, inst *Instance, observed statedb.RuntimeState) (Status, error) {
		inst.mu.Lock()
		defer inst.mu.Unlock()
		prev := Status(observed.Status)
		livePrev := prev
		if !inst.statusSampledLive {
			livePrev = ""
		}
		inst.statusSampledLive = true
		if captureFails {
			apply, next, held := debounceFlipFromRunning(prev, StatusError, "", inst.hookStatus, inst.tmuxFlipFromRunningPending)
			inst.tmuxFlipFromRunningPending = next
			if held {
				return apply, nil
			}
			return StatusError, errors.New("capture-pane failed")
		}
		apply, next, _ := debounceFlipFromRunning(livePrev, sample, string(sample), inst.hookStatus, inst.tmuxFlipFromRunningPending)
		inst.tmuxFlipFromRunningPending = next
		return apply, nil
	}

	d := NewTransitionDaemon()
	d.turnLiveCheck = func(*Instance) bool { return false }
	d.syncProfile(profile) // no TUI: running observed live
	sample = StatusWaiting
	d.syncProfile(profile) // no TUI: the first waiting sample is held
	if got, prior := d.lastStatus[profile][inst.ID], d.livePrior[profile][inst.ID]; got != "running" || !prior.flipPending {
		t.Fatalf("precondition: status=%q prior=%+v, want a held running verdict with its flip pending", got, prior)
	}

	if err := db.RegisterInstance(false); err != nil {
		t.Fatalf("RegisterInstance: %v", err)
	}
	if err := db.Heartbeat(); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	d.syncProfile(profile) // a TUI owns status
	if err := db.UnregisterInstance(); err != nil {
		t.Fatalf("UnregisterInstance: %v", err)
	}

	captureFails = true
	d.syncProfile(profile) // the TUI exited; this pass's capture fails once
	if got := d.lastStatus[profile][inst.ID]; got != "running" {
		t.Fatalf("status after the TUI exited = %q, want running: a stale pending flip skipped the capture-failure hold", got)
	}
	durable, found, err := db.ReadRuntimeState(inst.ID)
	if err != nil || !found || durable.Status != string(StatusRunning) {
		t.Fatalf("durable status = %+v (found=%v err=%v), want running", durable, found, err)
	}
}

// A successful profile listing retires every per-profile map of a deleted
// profile. The carried flip priors are per-profile state like the poll-state
// bridge; left behind they leak for the notify daemon's lifetime, and a
// profile recreated under the same name would inherit the old verdicts.
func TestRuntimeLifecycle_DaemonPruneDeletedProfileDropsLivePriors(t *testing.T) {
	d := NewTransitionDaemon()
	d.livePrior["kept"] = map[string]liveStatusPrior{"worker": {status: StatusRunning, flipPending: true}}
	d.livePrior["deleted"] = map[string]liveStatusPrior{"worker": {status: StatusRunning, flipPending: true}}
	d.pollState["kept"] = map[string]instancePollingState{"worker": {}}
	d.pollState["deleted"] = map[string]instancePollingState{"worker": {}}

	d.pruneDeletedProfiles([]string{"kept"})

	if priors, ok := d.livePrior["deleted"]; ok {
		t.Fatalf("deleted profile kept its flip priors: %+v", priors)
	}
	if _, ok := d.pollState["deleted"]; ok {
		t.Fatal("deleted profile kept its poll state")
	}
	if prior := d.livePrior["kept"]["worker"]; prior.status != StatusRunning || !prior.flipPending {
		t.Fatalf("active profile lost its flip prior: %+v", prior)
	}
	if _, ok := d.pollState["kept"]["worker"]; !ok {
		t.Fatal("active profile lost its poll state")
	}
}

func installRuntimeLifecycleNoopTmux(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "tmux")
	script := `#!/bin/sh
while [ "$#" -gt 0 ]; do
	case "$1" in
	-u)
		shift
		;;
	-L|-S)
		shift
		[ "$#" -gt 0 ] || exit 64
		shift
		;;
	*)
		break
		;;
	esac
done
case "$1" in
new-session|has-session|kill-session)
	exit 0
	;;
*)
	echo "unexpected hermetic tmux command: $*" >&2
	exit 64
	;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write hermetic tmux: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}
