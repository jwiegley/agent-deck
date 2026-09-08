// Package tmux — synchronous process-tree reap primitives (issue #59,
// v1.7.68).
//
// Session.Kill always ran the SIGTERM→SIGKILL escalation in a
// background goroutine. In short-lived CLI processes (`agent-deck
// remove`, `agent-deck session remove --force`) the goroutine was
// aborted when the CLI exited, leaving any SIGHUP-immune child (e.g.
// claude 2.1.27+) running indefinitely. The orphan observed
// 2026-04-22 (PID 321456, 33 hours old, AGENTDECK_INSTANCE_ID set,
// registry row gone) is the production manifestation.
//
// EnsurePIDsDead is the synchronous companion on Linux, where pidfds bind
// auxiliary signals to the captured process lifetime. Platforms without an
// identity-bound signal primitive fail closed: they warn and return without
// sending TERM or KILL through a recycled numeric PID. Session.KillAndWait
// wraps that behavior at the tmux-session level.

package tmux

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"
)

// EnsurePIDsDead captures each original process identity and, where the
// platform supports identity-bound signaling, blocks until every process is
// gone or the timeout elapses while escalating SIGTERM → SIGKILL. On an
// unsupported platform it logs a warning and performs no auxiliary signal. A
// zero-length slice is a no-op.
//
// Callers in CLI processes should use this instead of scheduling
// ensureProcessesDead on a goroutine — see issue #59.
//
// This convenience entry point captures identities when called. Lifecycle
// callers that mutate tmux first must instead call CaptureProcessIdentities
// before that mutation and pass the result to EnsureProcessIdentitiesDead.
func EnsurePIDsDead(pids []int, timeout time.Duration) {
	identities, err := CaptureProcessIdentities(pids)
	if err != nil {
		respawnLog.Warn("ensure_pids_dead_identity_capture_failed", slog.Any("error", err))
		return
	}
	EnsureProcessIdentitiesDead(identities, timeout)
}

// EnsureProcessIdentitiesDead applies the synchronous tmux reap timing where
// identity-bound signaling is supported, retaining identities captured before
// the tmux mutation. Unsupported platforms warn and fail closed.
func EnsureProcessIdentitiesDead(identities []ProcessIdentity, timeout time.Duration) {
	if len(identities) == 0 {
		return
	}
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	initialGrace := min(250*time.Millisecond, timeout)
	remaining := timeout - initialGrace
	termGrace := min(750*time.Millisecond, remaining)
	remaining -= termGrace
	ReapProcessIdentities(identities, ProcessReapTiming{
		InitialGrace: initialGrace,
		TermGrace:    termGrace,
		KillWait:     remaining,
		PollInterval: 50 * time.Millisecond,
	})
}

// KillAndWait is the synchronous variant of Session.Kill. It runs tmux
// kill-session and, where identity-bound signaling is supported, verifies or
// reaps every pane process captured before the kill. On unsupported platforms,
// surviving children are left untouched after a warning. Intended for
// short-lived CLI processes where the goroutine scheduled by Kill would be
// aborted on exit.
//
// See issue #59 and the package-level docs above.
func (s *Session) KillAndWait() error {
	if pm := GetPipeManager(); pm != nil {
		pm.Disconnect(s.Name)
	}
	_ = os.Remove(s.LogFile())

	target, oldIdentities, identityErr := captureStableSessionProcessTreeFn(s)
	if identityErr != nil {
		captureErr := fmt.Errorf("tmux: capture process identities before kill: %w", identityErr)
		probeTarget := s.Name
		if target.SessionID != "" {
			probeTarget = target.SessionID
		}
		if absent, resolvedErr := sessionAbsentAfterFailure(s.SocketName, probeTarget, captureErr); absent {
			return nil
		} else {
			return resolvedErr
		}
	}
	identitiesOwned := true
	defer func() {
		if identitiesOwned {
			CloseProcessIdentities(oldIdentities)
		}
	}()

	// The stable-ID conditional is bounded and executes the process proof and
	// kill in one tmux-server command queue. A same-named replacement cannot
	// redirect it to another physical runtime.
	killErr := mutateStableSessionTarget(target, []string{"kill-session", "-t", target.SessionID})
	if killErr != nil {
		if !errors.Is(killErr, errStableSessionMutationIndeterminate) &&
			!errors.Is(killErr, errStableSessionTargetChanged) {
			return killErr
		}
		absent, resolvedErr := sessionAbsentAfterFailure(s.SocketName, target.SessionID, killErr)
		if !absent {
			return resolvedErr
		}
	}

	if len(oldIdentities) > 0 {
		identitiesOwned = false
		EnsureProcessIdentitiesDead(oldIdentities, 3*time.Second)
	}

	// Killing an already-dead session is success (see Session.Kill): tmux
	// `kill-session` exits non-zero for a session that no longer exists. CLI
	// callers (`agent-deck remove` of a stopped session) must not fail on that.
	return nil
}
