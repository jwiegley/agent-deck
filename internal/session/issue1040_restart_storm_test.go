package session

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const instanceSpawnLockCrashHelperEnv = "AGENTDECK_INSTANCE_SPAWN_LOCK_CRASH_HELPER"

const instanceSpawnLockCrashReadyFD = 3

func TestRuntimeLifecycle_InstanceSpawnTryLockUsesStableFlock(t *testing.T) {
	withTempLockDir(t)

	release, acquired, err := defaultTryAcquireInstanceSpawnLock("target")
	if err != nil || !acquired {
		t.Fatalf("first try acquire: acquired=%v err=%v", acquired, err)
	}
	path, err := instanceSpawnLockPath("target")
	if err != nil {
		release()
		t.Fatal(err)
	}
	if contenderRelease, contenderAcquired, err := defaultTryAcquireInstanceSpawnLock("target"); err != nil {
		release()
		t.Fatal(err)
	} else if contenderAcquired {
		contenderRelease()
		release()
		t.Fatal("nonblocking contender acquired a held flock")
	}

	release()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stable lockfile disappeared on release: %v", err)
	}
	reacquiredRelease, reacquired, err := defaultTryAcquireInstanceSpawnLock("target")
	if err != nil || !reacquired {
		t.Fatalf("try acquire after release: acquired=%v err=%v", reacquired, err)
	}
	reacquiredRelease()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stable lockfile disappeared after second release: %v", err)
	}
}

func TestRuntimeLifecycle_InstanceSpawnLockMarkerIsDiagnosticOnly(t *testing.T) {
	withTempLockDir(t)
	path, err := instanceSpawnLockPath("diagnostic")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("stale malformed marker"), 0o600); err != nil {
		t.Fatal(err)
	}

	release, acquired, err := defaultTryAcquireInstanceSpawnLock("diagnostic")
	if err != nil || !acquired {
		t.Fatalf("stale marker blocked free flock: acquired=%v err=%v", acquired, err)
	}
	marker, err := os.ReadFile(path)
	if err != nil {
		release()
		t.Fatal(err)
	}
	if string(marker) != strconv.Itoa(os.Getpid()) {
		release()
		t.Fatalf("diagnostic marker = %q, want current PID", marker)
	}
	// Marker contents do not participate in ownership; changing them cannot
	// make a second contender acquire the held flock.
	if err := os.WriteFile(path, []byte("externally changed"), 0o600); err != nil {
		release()
		t.Fatal(err)
	}
	if contenderRelease, contenderAcquired, err := defaultTryAcquireInstanceSpawnLock("diagnostic"); err != nil {
		release()
		t.Fatal(err)
	} else if contenderAcquired {
		contenderRelease()
		release()
		t.Fatal("marker replacement bypassed held flock")
	}
	release()
}

func TestRuntimeLifecycle_InstanceSpawnLockCrashReleasesFlock(t *testing.T) {
	if os.Getenv(instanceSpawnLockCrashHelperEnv) == "1" {
		agentDeckDirOverride = os.Getenv("AGENTDECK_INSTANCE_SPAWN_LOCK_ROOT")
		if _, err := defaultAcquireInstanceSpawnLock("crash-release"); err != nil {
			t.Fatal(err)
		}
		ready := os.NewFile(instanceSpawnLockCrashReadyFD, "spawn-lock-ready")
		if ready == nil {
			t.Fatal("spawn-lock readiness descriptor is unavailable")
		}
		if _, err := ready.Write([]byte{1}); err != nil {
			t.Fatal(err)
		}
		_ = ready.Close()
		select {}
	}

	withTempLockDir(t)
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = readyRead.Close() })
	cmd := exec.Command(os.Args[0], "-test.run=^TestRuntimeLifecycle_InstanceSpawnLockCrashReleasesFlock$")
	cmd.ExtraFiles = []*os.File{readyWrite}
	cmd.Env = append(os.Environ(),
		instanceSpawnLockCrashHelperEnv+"=1",
		"AGENTDECK_INSTANCE_SPAWN_LOCK_ROOT="+agentDeckDirOverride,
		"AGENTDECK_RUNTIME_LIFECYCLE_ONLY=1",
	)
	if err := cmd.Start(); err != nil {
		_ = readyWrite.Close()
		t.Fatal(err)
	}
	_ = readyWrite.Close()
	waited := false
	t.Cleanup(func() {
		if !waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	path, err := instanceSpawnLockPath("crash-release")
	if err != nil {
		t.Fatal(err)
	}
	if err := readyRead.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(readyRead, []byte{0}); err != nil {
		t.Fatalf("crash helper did not acquire flock: %v", err)
	}
	marker, err := os.ReadFile(path)
	if err != nil || string(marker) != strconv.Itoa(cmd.Process.Pid) {
		t.Fatalf("crash helper marker=%q err=%v, want PID %d", marker, err, cmd.Process.Pid)
	}
	if contenderRelease, acquired, err := defaultTryAcquireInstanceSpawnLock("crash-release"); err != nil {
		t.Fatal(err)
	} else if acquired {
		contenderRelease()
		t.Fatal("parent acquired flock while child held it")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	waited = true

	release, acquired, err := defaultTryAcquireInstanceSpawnLock("crash-release")
	if err != nil || !acquired {
		t.Fatalf("try acquire after owner crash: acquired=%v err=%v", acquired, err)
	}
	release()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stable lockfile disappeared after crash recovery: %v", err)
	}
}

// Issue #1040 regression suite — concurrent restart-storm prevention.
//
// Background: when a Claude session exited (task complete, context limit,
// API error) and a watcher / external poller called `agent-deck session
// start <id>` more than once in rapid succession, agent-deck v1.9.17 spawned
// 3–5 concurrent tmux sessions for the same instance. Each fell through the
// `Restart()` respawn-pane fast path (because the prior tmux session was
// gone) and into the recreateTmuxSession fallback, which mints a fresh
// random suffix per call. The #666 duplicate-sweep ran AFTER each spawn,
// so every spawn killed the previous one and the last writer wins on DB
// state — leaving a short-lived (8–15s) session as the "tracked" one.
//
// Fix shape: a per-instance advisory flock at ~/.agent-deck/locks/instance-
// spawn-<id>.lock serializing the spawn critical section, plus a sibling
// "already alive" gate inside the lock so the second waiter exits without
// re-spawning. The stable lockfile remains after release; flock state alone
// determines ownership.
//
// These tests exercise the lock primitive + the SpawnAttempt wrapper used
// by Restart() / Start(). They MUST fail on current main (functions do not
// exist) and pass after the fix.

// TestInstanceSpawnLock_SerializesConcurrentAcquires_RegressionFor1040
// drives the lock-only invariant: across N concurrent acquires for the
// same instance ID, at most one holder is inside the critical section at
// any moment. Without the lock, the observed-max-holders count saturates
// at N (the storm signature). With the lock it must be exactly 1.
func TestInstanceSpawnLock_SerializesConcurrentAcquires_RegressionFor1040(t *testing.T) {
	withTempLockDir(t)

	const goroutines = 8
	const holdDuration = 20 * time.Millisecond

	var (
		holders    atomic.Int32
		maxHolders atomic.Int32
	)

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			release, err := acquireInstanceSpawnLock("inst-1040")
			if err != nil {
				t.Errorf("acquire failed: %v", err)
				return
			}
			defer release()
			now := holders.Add(1)
			for {
				cur := maxHolders.Load()
				if now <= cur || maxHolders.CompareAndSwap(cur, now) {
					break
				}
			}
			time.Sleep(holdDuration)
			holders.Add(-1)
		}()
	}
	wg.Wait()

	if got := maxHolders.Load(); got != 1 {
		t.Fatalf("max concurrent holders = %d, want 1 (lock did not serialize)", got)
	}
}

// TestInstanceSpawnLock_DifferentInstancesDoNotSerialize verifies the lock
// key is scoped per instance ID — two different instances spawning at the
// same time must not block each other. Without this, a single hung restart
// would freeze every other session on the host.
func TestInstanceSpawnLock_DifferentInstancesDoNotSerialize(t *testing.T) {
	withTempLockDir(t)

	releaseA, err := acquireInstanceSpawnLock("inst-A")
	if err != nil {
		t.Fatalf("acquire A: %v", err)
	}
	defer releaseA()

	done := make(chan struct{})
	go func() {
		releaseB, err := acquireInstanceSpawnLock("inst-B")
		if err != nil {
			t.Errorf("acquire B: %v", err)
			return
		}
		releaseB()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("acquire of inst-B blocked on inst-A's lock — per-instance scoping broken")
	}
}

// TestSpawnAttempt_SingleFlight_OnConcurrentInvocation_RegressionFor1040
// is the core regression test for #1040. N goroutines each invoke a
// SpawnAttempt whose AlreadyAlive callback flips to true as soon as the
// first Spawn finishes. After serialization the spawn counter must read 1.
//
// On current main (no SpawnAttempt type) the test fails to compile. With
// the fix in place, exactly one spawn fires; the rest see AlreadyAlive=true
// and return nil.
func TestSpawnAttempt_SingleFlight_OnConcurrentInvocation_RegressionFor1040(t *testing.T) {
	withTempLockDir(t)

	const goroutines = 6

	var (
		spawnCount atomic.Int32
		spawned    atomic.Bool
	)

	attempt := SpawnAttempt{
		InstanceID: "inst-1040-storm",
		AlreadyAlive: func() bool {
			return spawned.Load()
		},
		Spawn: func() error {
			spawnCount.Add(1)
			// Simulate the spawn latency so concurrent waiters pile up
			// behind the lock — this is the storm-shape repro.
			time.Sleep(15 * time.Millisecond)
			spawned.Store(true)
			return nil
		},
	}

	var wg sync.WaitGroup
	wg.Add(goroutines)
	errs := make(chan error, goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			if err := attempt.Run(); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("Run returned error: %v", err)
	}

	if got := spawnCount.Load(); got != 1 {
		t.Fatalf("spawn count = %d, want 1 (single-flight broke under concurrency — storm reintroduced)", got)
	}
}

// TestSpawnAttempt_SkipsWhenAlreadyAlive verifies the gate-only path:
// when AlreadyAlive is true before any caller spawns, no spawn fires
// (matches the "sibling already produced a live session" case in
// Restart() / Start()).
func TestSpawnAttempt_SkipsWhenAlreadyAlive(t *testing.T) {
	withTempLockDir(t)

	var spawnCount atomic.Int32
	attempt := SpawnAttempt{
		InstanceID:   "inst-already-alive",
		AlreadyAlive: func() bool { return true },
		Spawn: func() error {
			spawnCount.Add(1)
			return nil
		},
	}

	if err := attempt.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := spawnCount.Load(); got != 0 {
		t.Fatalf("spawn count = %d, want 0 (gate failed to skip)", got)
	}
}

// TestSpawnAttempt_StormBurstDeduplicatesViaStamp is the direct
// reproduction of the v1.9.17 storm shape without an AlreadyAlive hook:
// N goroutines call Run() in quick succession, the first one stamps,
// the others see the stamp's mtime > their beforeLock and skip. This is
// the file-only path used by Restart() / Start() in production.
func TestSpawnAttempt_StormBurstDeduplicatesViaStamp(t *testing.T) {
	withTempLockDir(t)

	const goroutines = 6

	var spawnCount atomic.Int32
	attempt := SpawnAttempt{
		InstanceID: "inst-storm",
		// AlreadyAlive intentionally nil — exercise the stamp path only.
		Spawn: func() error {
			spawnCount.Add(1)
			time.Sleep(10 * time.Millisecond)
			return nil
		},
	}

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			if err := attempt.Run(); err != nil {
				t.Errorf("Run: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := spawnCount.Load(); got != 1 {
		t.Fatalf("storm burst: spawn count = %d, want 1 (stamp gate broken)", got)
	}
}

// TestSpawnAttempt_LegitimateRestartProceedsAfterPriorSpawn pins the
// other direction: a manual restart issued long after a prior spawn
// (i.e. stamp mtime older than the new beforeLock) must NOT be skipped.
// Without this, a single successful spawn would brick all subsequent
// restarts for the lifetime of the lockfile directory.
func TestSpawnAttempt_LegitimateRestartProceedsAfterPriorSpawn(t *testing.T) {
	withTempLockDir(t)

	var spawnCount atomic.Int32
	attempt := SpawnAttempt{
		InstanceID: "inst-rerun",
		Spawn: func() error {
			spawnCount.Add(1)
			return nil
		},
	}

	if err := attempt.Run(); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	// Sleep past the filesystem mtime granularity so the second
	// beforeLock is strictly after the stamp written by the first run.
	// 50ms is comfortable margin against any sub-second clock skew.
	time.Sleep(50 * time.Millisecond)
	if err := attempt.Run(); err != nil {
		t.Fatalf("second Run: %v", err)
	}

	if got := spawnCount.Load(); got != 2 {
		t.Fatalf("legitimate sequential restart: spawn count = %d, want 2 (stamp gate over-suppressed)", got)
	}
}

// TestSpawnAttempt_NilAlreadyAliveTreatedAsNotAlive — defensive default:
// passing a nil AlreadyAlive callback must NOT cause a nil deref. The
// caller intent in that case is "we have no sibling-detection logic,
// always spawn" (used by tests / callers that have already done the
// check externally).
func TestSpawnAttempt_NilAlreadyAliveTreatedAsNotAlive(t *testing.T) {
	withTempLockDir(t)

	var spawnCount atomic.Int32
	attempt := SpawnAttempt{
		InstanceID:   "inst-nil-alive",
		AlreadyAlive: nil,
		Spawn: func() error {
			spawnCount.Add(1)
			return nil
		},
	}

	if err := attempt.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := spawnCount.Load(); got != 1 {
		t.Fatalf("spawn count = %d, want 1 (nil AlreadyAlive should not skip)", got)
	}
}

// withTempLockDir points AGENT_DECK_DIR at a fresh tmp dir so the lock
// files don't leak between tests or pollute the user's real ~/.agent-deck.
// Returns nothing — registers cleanup via t.Cleanup.
func withTempLockDir(t *testing.T) {
	t.Helper()
	tmp := t.TempDir()
	prev := agentDeckDirOverride
	agentDeckDirOverride = filepath.Join(tmp, ".agent-deck")
	t.Cleanup(func() { agentDeckDirOverride = prev })
}
