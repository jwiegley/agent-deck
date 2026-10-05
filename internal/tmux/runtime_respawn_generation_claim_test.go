package tmux

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// The #1892 generation claim for the restart path production actually uses.
// Every Instance restart reaches the pane through respawnRuntimePane, which
// calls RespawnRuntimeGenerationCandidate on the Instance's own Session, not
// Session.RespawnPane. The upstream #1892/#2361 generation tests exercise only
// RespawnPane, so these drive the candidate respawn against the same races on
// a real pane. Needing a live tmux server, they stay out of the SQLite-only
// TestRuntimeLifecycle_ gate, whose tmux is a stub that always reports no
// server. The lock-scope unit tests, which the gate does run, live in
// runtime_binding_candidates_test.go with the stub-safe helpers both files use.

const runtimeRespawnClaimGeneration = 4

// startRuntimeGenerationSession starts an inert pane stamped with complete
// generation authority, in the order stampRuntimeCandidate publishes it: the
// session-local cleanup options, then the generation environment marker.
func startRuntimeGenerationSession(t *testing.T, name string) *Session {
	t.Helper()
	skipIfNoTmuxBinary(t)
	// Neither the respawn's escalation nor the kill may still be reaping when
	// this test returns: either would be reading the process-identity seams
	// that stub-based tests replace. Cleanup runs KillAndWait, which reaps
	// synchronously, then waits for every escalation this test's respawns
	// started.
	trackRuntimeGenerationEscalationsForTest(t)
	s := NewSession(name, t.TempDir())
	s.InstanceID = "instance-" + name
	if err := s.Start("sleep 300"); err != nil {
		t.Fatalf("start inert pane: %v", err)
	}
	t.Cleanup(func() { _ = s.KillAndWait() })
	if err := StampRuntimeCleanupIdentity(s, s.InstanceID, runtimeRespawnClaimGeneration, "", ""); err != nil {
		t.Fatalf("stamp runtime cleanup identity: %v", err)
	}
	if err := s.SetEnvironment(runtimeGenerationEnvironment, strconv.Itoa(runtimeRespawnClaimGeneration)); err != nil {
		t.Fatalf("stamp generation environment: %v", err)
	}
	return s
}

// exactRuntimeGenerationCandidate waits for the pane's process tree to settle
// (its foreground command is the final sleep, whether the pane runs the agent
// stand-in or the timeout hold) and returns the exact candidate inventory
// reports for it, as respawnRuntimePane receives it after revalidation.
func exactRuntimeGenerationCandidate(t *testing.T, s *Session) RuntimeGenerationCandidate {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		out, err := s.tmuxCmd("display-message", "-p", "-t", "="+s.primaryWindowTarget(), "#{pane_current_command}").Output()
		if err == nil && strings.TrimSpace(string(out)) == "sleep" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pane never settled on its sleep: %q, %v", out, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	candidates, err := ListRuntimeGenerationCandidates(s.SocketName, s.InstanceID)
	if err != nil {
		t.Fatalf("list runtime generation candidates: %v", err)
	}
	if len(candidates) != 1 || candidates[0].SessionName != s.Name ||
		!candidates[0].GenerationKnown || candidates[0].Generation != runtimeRespawnClaimGeneration {
		t.Fatalf("runtime generation candidates = %#v, want exactly %s at generation %d",
			candidates, s.Name, runtimeRespawnClaimGeneration)
	}
	return candidates[0]
}

func expireStartupClockForTest(s *Session) {
	s.mu.Lock()
	s.startupAt = time.Now().Add(-startupStateWindow - time.Second)
	s.lastStableStatus = "starting"
	s.mu.Unlock()
}

// TestCandidateRespawn_ReleasesClaimedStartupTimeout restarts a session whose
// startup timeout was already claimed, in the same process (the TUI restart).
// The claim belonged to the pane generation the respawn ended; left set,
// GetStatus skips the #2361 alive probe and reports the healthy replacement as
// "error" on every poll, and MarkInteractiveAt drops the replacement's hook
// evidence.
func TestCandidateRespawn_ReleasesClaimedStartupTimeout(t *testing.T) {
	s := startRuntimeGenerationSession(t, "candidate-claim-release")
	expireStartupClockForTest(s)
	if status, err := s.GetStatus(); err != nil || status != "error" {
		t.Fatalf("precondition: expired startup status = %q, %v; want the claimed timeout", status, err)
	}

	candidate := exactRuntimeGenerationCandidate(t, s)
	if err := RespawnRuntimeGenerationCandidate(s, candidate, "sleep 300"); err != nil {
		t.Fatalf("restart the timed-out generation: %v", err)
	}
	status, err := s.GetStatus()
	if err != nil {
		t.Fatalf("GetStatus for the restarted generation: %v", err)
	}
	if status == "error" {
		t.Fatal("restarted generation still reports the previous generation's startup timeout")
	}

	s.mu.Lock()
	startupAt := s.startupAt
	s.mu.Unlock()
	if startupAt.IsZero() {
		t.Fatal("precondition: restarted generation has no startup clock for hook evidence to end")
	}
	s.MarkInteractiveAt(time.Now())
	s.mu.Lock()
	startupAt = s.startupAt
	s.mu.Unlock()
	if !startupAt.IsZero() {
		t.Fatal("hook evidence from the restarted generation did not end its startup")
	}
}

// TestCandidateRespawn_CannotLeaveTimeoutClaimCurrent replaces the pane
// between a poll's timeout claim and its generation validation, the
// afterStartupTimeoutClaim window. The poll must describe the replacement, not
// return the ended generation's timeout for it.
func TestCandidateRespawn_CannotLeaveTimeoutClaimCurrent(t *testing.T) {
	s := startRuntimeGenerationSession(t, "candidate-claim-race")
	expireStartupClockForTest(s)

	var respawnErr error
	replaced := false
	s.afterStartupTimeoutClaim = func() {
		candidate := exactRuntimeGenerationCandidate(t, s)
		respawnErr = RespawnRuntimeGenerationCandidate(s, candidate, "sleep 300")
		replaced = true
	}

	status, err := s.GetStatus()
	if err != nil {
		t.Fatalf("GetStatus across replacement generation: %v", err)
	}
	if !replaced {
		t.Fatal("precondition: GetStatus never claimed the expired startup")
	}
	if respawnErr != nil {
		t.Fatalf("replace timed-out pane generation: %v", respawnErr)
	}
	if status == "error" {
		t.Fatal("GetStatus returned the expired generation's timeout for its candidate replacement")
	}
}

// TestCandidateRespawn_CannotCrossTimeoutGenerationClaim holds session.mu as
// expireStartupHandover does while it claims a generation and respawns the
// timeout hold. Replacing the pane inside that window would let the claim
// land on (and the hold respawn over) the freshly restarted agent; the
// candidate respawn must wait for the claim instead.
func TestCandidateRespawn_CannotCrossTimeoutGenerationClaim(t *testing.T) {
	// Installed before the fixture so its seam is restored only after the
	// fixture's cleanup has waited out the respawn's escalation.
	captured := signalCandidateCaptureForTest(t)
	s := startRuntimeGenerationSession(t, "candidate-claim-serialized")
	candidate := exactRuntimeGenerationCandidate(t, s)

	s.mu.Lock() // models expireStartupHandover's claimed generation
	done := make(chan error, 1)
	go func() { done <- RespawnRuntimeGenerationCandidate(s, candidate, "sleep 300") }()
	if err := awaitRespawnParkedOnClaim(captured); err != nil {
		s.mu.Unlock()
		<-done
		t.Fatal(err)
	}
	pid, err := s.PanePID()
	if err != nil {
		s.mu.Unlock()
		<-done
		t.Fatal(err)
	}
	if pid != candidate.PanePID {
		s.mu.Unlock()
		<-done
		t.Fatalf("candidate respawn replaced pane while timeout generation claim was held: pid %d -> %d", candidate.PanePID, pid)
	}
	s.mu.Unlock()
	if err := <-done; err != nil {
		t.Fatalf("candidate respawn after generation claim released: %v", err)
	}
	if pid, err := s.PanePID(); err != nil || pid == candidate.PanePID {
		t.Fatalf("pane pid after respawn = %d, %v; want a replacement for %d", pid, err, candidate.PanePID)
	}
}
