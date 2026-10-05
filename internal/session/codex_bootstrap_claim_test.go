package session

import (
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// holdCodexBootstrapForTest takes codexBootstrapMu as a peer mid-way through
// its disk-scan selection would, and returns an idempotent release that also
// runs at cleanup.
func holdCodexBootstrapForTest(t *testing.T) func() {
	t.Helper()
	codexBootstrapMu.Lock()
	var once sync.Once
	release := func() { once.Do(codexBootstrapMu.Unlock) }
	t.Cleanup(release)
	return release
}

func codexSessionIDForTest(inst *Instance) string {
	inst.mu.RLock()
	defer inst.mu.RUnlock()
	return inst.CodexSessionID
}

// codexBootstrapMu serializes only the disk fallback's selection with its
// publication. An unbound instance whose own tmux environment names its
// session publishes that authoritative binding while a peer holds the lock for
// its disk scan. Taking the lock before the environment read serialized every
// unbound instance's read and process probe behind one mutex, and the TUI's
// ten-worker status sweep outlived the ownership TTL and re-read the fleet
// (TestBackgroundStatusPassOwnershipLinear in internal/ui).
func TestCodexBootstrapClaimSkipsAuthoritativeEnvironmentRead(t *testing.T) {
	resetCodexOwnershipCache(t)
	codexCountingTmux(t, 1)
	// The process probe is not due, so the environment read, which names this
	// session's own ID, is the only evidence.
	inst := &Instance{ID: "codex-bootstrap-env", Tool: "codex", lastCodexProbeAt: time.Now(),
		tmuxSession: &tmux.Session{Name: "agentdeck_scan_0"}}
	release := holdCodexBootstrapForTest(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		inst.updateCodexSessionForPass(nil, false, &StatusUpdatePass{})
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		release()
		<-done
		t.Fatal("the authoritative tmux environment read waited for a peer's bootstrap selection")
	}
	release()
	if got := codexSessionIDForTest(inst); got != "id-agentdeck_scan_0" {
		t.Fatalf("codex binding = %q, want the environment's id-agentdeck_scan_0", got)
	}
}

// A restart's Codex fallback selects under the same claim as the status pass:
// it waits for a peer that is mid-selection, then claims the rollout it chose
// for its peers before releasing the lock. Scanning without the claim let a
// restart and a peer's status-pass bootstrap in one project pick the same
// rollout from one ownership snapshot.
func TestCodexRestartFallbackSelectsUnderBootstrapClaim(t *testing.T) {
	resetCodexOwnershipCache(t)
	codexCountingTmux(t, 1)
	t.Setenv("CODEX_SCAN_EMPTY", "1") // no tmux environment evidence anywhere
	t.Setenv("CODEX_HOME", t.TempDir())
	// The restart's forced process probe inspects a live pane running no Codex
	// process, so detection goes on to the disk fallback.
	pane := exec.Command("sleep", "30")
	if err := pane.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pane.Process.Kill(); _ = pane.Wait() })
	t.Setenv("CODEX_SCAN_PANE_PID", strconv.Itoa(pane.Process.Pid))
	sid := uniqueSID(t)
	seedCodexRolloutWithMeta(t, os.Getenv("CODEX_HOME"), sid, "", "", false)
	inst := &Instance{ID: "codex-restart-fallback", Tool: "codex", ProjectPath: "/tmp/project",
		CodexStartedAt: time.Now().Add(-time.Minute).UnixMilli(),
		tmuxSession:    &tmux.Session{Name: "agentdeck_scan_0"}}
	release := holdCodexBootstrapForTest(t)

	done := make(chan codexSessionCandidate, 1)
	go func() { done <- inst.adoptCodexSessionForRestart() }()
	select {
	case <-done:
		t.Fatal("the restart's disk fallback selected while a peer held the bootstrap claim")
	case <-time.After(300 * time.Millisecond):
	}
	release()
	var candidate codexSessionCandidate
	select {
	case candidate = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the restart's disk fallback never finished after the peer released its claim")
	}
	if candidate.id != sid || candidate.source != "disk" || codexSessionIDForTest(inst) != sid {
		t.Fatalf("restart selection = %+v (bound %q), want the rollout %s from disk", candidate, codexSessionIDForTest(inst), sid)
	}
	codexOwnershipCache.Lock()
	claims := codexOwnershipCache.bySocket[""].claims
	codexOwnershipCache.Unlock()
	if claims == nil {
		t.Fatal("the restart's selection left no ownership snapshot")
	}
	claims.Lock()
	claimed := claims.bySession["agentdeck_scan_0"]
	claims.Unlock()
	if claimed != sid {
		t.Fatalf("ownership claim for the restarting session = %q, want %s visible to peers", claimed, sid)
	}
	if !codexBootstrapMu.TryLock() {
		t.Fatal("the restart kept the bootstrap claim after publishing its selection")
	}
	codexBootstrapMu.Unlock()
}

// The disk fallback still selects under the claim: it waits for a peer's
// selection and publication, then binds, and it releases the claim when done.
func TestCodexBootstrapClaimSerializesDiskFallback(t *testing.T) {
	resetCodexOwnershipCache(t)
	codexCountingTmux(t, 1)
	t.Setenv("CODEX_SCAN_EMPTY", "1") // no tmux environment evidence anywhere
	t.Setenv("CODEX_HOME", t.TempDir())
	sid := uniqueSID(t)
	seedCodexRolloutWithMeta(t, os.Getenv("CODEX_HOME"), sid, "", "", false)
	inst := &Instance{ID: "codex-bootstrap-disk", Tool: "codex", ProjectPath: "/tmp/project",
		CodexStartedAt: time.Now().Add(-time.Minute).UnixMilli(), lastCodexProbeAt: time.Now(),
		tmuxSession: &tmux.Session{Name: "agentdeck_scan_0"}}
	release := holdCodexBootstrapForTest(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		inst.updateCodexSessionForPass(nil, false, &StatusUpdatePass{})
	}()
	select {
	case <-done:
		t.Fatal("the disk fallback selected while a peer held the bootstrap claim")
	case <-time.After(300 * time.Millisecond):
	}
	release()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the disk fallback never finished after the peer released its claim")
	}
	if got := codexSessionIDForTest(inst); got != sid {
		t.Fatalf("codex binding = %q, want the rollout %s", got, sid)
	}
	if !codexBootstrapMu.TryLock() {
		t.Fatal("the disk fallback kept the bootstrap claim after publishing")
	}
	codexBootstrapMu.Unlock()
}

// The #2394 live-process check runs on the bootstrap scan's cadence, as
// upstream's shouldScanCodexSession stamps it. While the pane's live Codex
// owns no thread (composer not up, an update prompt, a Codex older than 0.155
// idle before its first turn), each check walks the pane's process tree and
// lists its open files, so it must not repeat on every status pass.
func TestCodexLiveEvidenceCheckRunsOnTheBootstrapScanCadence(t *testing.T) {
	inst, _ := newUnboundCodexForBootstrap(t)
	var probes atomic.Int32
	restore := codexPaneProcessPIDs
	t.Cleanup(func() { codexPaneProcessPIDs = restore })
	codexPaneProcessPIDs = func(*Instance) ([]int, error) {
		probes.Add(1)
		return []int{4242}, nil
	}
	stubCodexPaneOpenPaths(t, []string{"/dev/null"}, nil)

	for n := 0; n < 5; n++ {
		inst.UpdateCodexSession(map[string]bool{})
	}
	if got := probes.Load(); got != 1 {
		t.Fatalf("five back-to-back passes ran the live-process check %d times, want once per %v",
			got, codexBootstrapScanInterval)
	}
}
