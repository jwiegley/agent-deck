package tmux

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// On Linux, EnsurePIDsDead must synchronously reap SIGHUP-immune children by
// the time it returns. Darwin has no supported identity-bound signal primitive,
// so the same auxiliary reap must fail closed and leave the child untouched.
//
// Observed 2026-04-22 on the maintainer's host: PID 321456, 33-hour
// orphan with AGENTDECK_INSTANCE_ID set, no corresponding agent-deck
// session record. Root cause #59.
func TestRuntimeLifecycle_EnsurePIDsDeadUsesPlatformIdentitySignalContract(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skipf("posix signal semantics only; GOOS=%s", runtime.GOOS)
	}

	// `trap '' HUP; sleep 30` emulates claude 2.1.27+ which ignores
	// SIGHUP. This is the real-world case that triggered the orphan
	// bug — tmux kill-session sends SIGHUP and the claude child
	// keeps running.
	cmd := exec.Command("sh", "-c", "trap '' HUP; sleep 30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start shell: %v", err)
	}
	pid := cmd.Process.Pid
	// Reap the process as soon as it exits so the kernel zombie entry
	// clears and kill(pid, 0) correctly returns ESRCH. Without this,
	// Go keeps the PID in its wait-for-me set and signal-0 reports
	// "alive" on a defunct-but-unreaped process — the classic zombie
	// pitfall that trips up every "is this pid dead" check.
	reaped := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(reaped)
	}()
	// Ensure we never leak the sleep, even if the assertion below fails.
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-reaped
	})

	// Give the shell a moment to install the HUP trap.
	time.Sleep(150 * time.Millisecond)

	// Sanity: child is alive.
	if err := syscall.Kill(pid, syscall.Signal(0)); err != nil {
		t.Fatalf("setup: pid %d not alive: %v", pid, err)
	}

	// Linux must complete the reap before returning. Darwin must return after
	// its bounded grace without signaling this PID through a racy raw handle.
	err := EnsurePIDsDead([]int{pid}, 3*time.Second)

	alive := syscall.Kill(pid, syscall.Signal(0)) == nil
	if runtime.GOOS == "darwin" {
		if !alive {
			t.Fatalf("Darwin auxiliary reap signaled pid %d without identity-bound signal support", pid)
		}
		if err == nil || !strings.Contains(err.Error(), "death unverified") {
			t.Fatalf("live Darwin child must report unverified death, got %v", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if alive {
		name, _ := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output()
		t.Errorf("pid %d (comm=%q) still alive after EnsurePIDsDead — must be synchronous",
			pid, strings.TrimSpace(string(name)))
	}
}

// TestKillAndWait_RoutesKillToSessionSocket ensures the synchronous stop path
// cannot kill a same-named session on the host/default server while its Exists
// check probes a custom socket. The shim records argv only; it starts no tmux
// server or process tree.
func TestKillAndWait_RoutesKillToSessionSocket(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "tmux-argv.log")
	t.Setenv("TMUX_KILL_LOG", logPath)
	writeFakeTmux(t, dir, `printf '%s\n' "$*" >> "$TMUX_KILL_LOG"`)
	oldCapture := captureStableSessionProcessTreeFn
	captureStableSessionProcessTreeFn = func(s *Session) (stableSessionTarget, []ProcessIdentity, error) {
		return stableSessionTargetForTest(s.Name, s.SocketName), nil, nil
	}
	t.Cleanup(func() { captureStableSessionProcessTreeFn = oldCapture })

	socket := "kill-and-wait-custom-socket"
	s := &Session{Name: "kill-and-wait-target", SocketName: socket}
	if err := s.KillAndWait(); err != nil {
		t.Fatalf("KillAndWait on captured session: %v", err)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read fake tmux argv log: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if !strings.HasPrefix(line, "-u -L "+socket+" ") {
			t.Fatalf("tmux argv %q did not target -L %q", line, socket)
		}
		if strings.Contains(line, "'kill-session' '-t' '$7'") {
			return
		}
	}
	t.Fatalf("fake tmux never received stable-ID kill-session; argv log: %q", data)
}

// A nil/empty PID list must be a no-op, returning immediately. Callers
// in the remove path often fetch `getPaneProcessTree()` which returns
// an empty slice for already-torn-down sessions; that must not block.
func TestEnsurePIDsDead_NoopOnEmptyPIDs(t *testing.T) {
	start := time.Now()
	EnsurePIDsDead(nil, 10*time.Second)
	EnsurePIDsDead([]int{}, 10*time.Second)
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Errorf("EnsurePIDsDead on empty PID list must return immediately; took %v", elapsed)
	}
}
