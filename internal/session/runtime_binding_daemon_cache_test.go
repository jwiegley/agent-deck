package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

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
