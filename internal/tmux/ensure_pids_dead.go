// Package tmux provides synchronous, identity-bound process reaping for
// short-lived CLI callers that cannot leave cleanup to a background goroutine.
package tmux

import (
	"errors"
	"fmt"
	"os"
	"time"
)

// EnsurePIDsDead captures process identities and synchronously reaps them.
// Lifecycle callers that mutate tmux first must capture identities before the
// mutation and pass them to EnsureProcessIdentitiesDead instead.
func EnsurePIDsDead(pids []int, timeout time.Duration) error {
	identities, err := CaptureProcessIdentities(pids)
	if err != nil {
		return err
	}
	return EnsureProcessIdentitiesDead(identities, timeout)
}

// EnsureProcessIdentitiesDead takes ownership of the captured identities.
// Unsupported platforms never signal a raw PID and return an error if the
// original process's death cannot be verified.
func EnsureProcessIdentitiesDead(identities []ProcessIdentity, timeout time.Duration) error {
	if len(identities) == 0 {
		return nil
	}
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	initialGrace := min(250*time.Millisecond, timeout)
	remaining := timeout - initialGrace
	termGrace := min(750*time.Millisecond, remaining)
	remaining -= termGrace
	return ReapProcessIdentities(identities, ProcessReapTiming{
		InitialGrace: initialGrace,
		TermGrace:    termGrace,
		KillWait:     remaining,
		PollInterval: 50 * time.Millisecond,
	})
}

func processReapResult(alive []int) error {
	if len(alive) == 0 {
		return nil
	}
	return fmt.Errorf("process reaping timed out; death unverified for captured PIDs %v", alive)
}

func killAndWaitResult(killErr, reapErr error, sessionExists bool) error {
	if !sessionExists {
		killErr = nil
	}
	return errors.Join(killErr, reapErr)
}

// KillAndWait synchronously kills the captured tmux session and verifies its
// original process tree is dead. A same-named replacement is never targeted.
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
	// kill in one tmux-server command queue on the captured socket.
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

	identitiesOwned = false
	reapErr := EnsureProcessIdentitiesDead(oldIdentities, 3*time.Second)
	return killAndWaitResult(killErr, reapErr, killErr == nil)
}
