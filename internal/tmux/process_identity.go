package tmux

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"syscall"
	"time"
)

var errProcessIdentityNotFound = errors.New("process no longer exists")
var errProcessIdentitySignalUnsupported = errors.New("identity-bound process signaling is unsupported")
var errProcessIdentityClosed = errors.New("retained process identity handle is closed")

// ProcessIdentity identifies one lifetime of one numeric PID. On Linux the
// identity owns a retained pidfd opened during capture; value copies share
// that handle and Close is idempotent across every copy. StartToken remains a
// durable, kernel-derived description for persistence and diagnostics, but is
// never substituted for the retained handle when signaling or polling.
//
// The fields are exported because session lifecycle code must carry identities
// captured before a tmux mutation into the later orphan-reap phase. Callers
// should obtain values from CaptureProcessIdentity rather than constructing
// them.
type ProcessIdentity struct {
	PID        int
	StartToken string
	handle     *processIdentityHandle
}

// processIdentityHandle serializes operations with an idempotent close. The
// closures let the common ownership code stay platform-neutral while Linux
// binds every operation to one retained pidfd.
type processIdentityHandle struct {
	mu       sync.RWMutex
	closed   bool
	signalFn func(os.Signal) (bool, error)
	aliveFn  func() (bool, error)
	closeFn  func() error
}

func (handle *processIdentityHandle) signal(signal os.Signal) (bool, error) {
	if handle == nil {
		return false, errProcessIdentitySignalUnsupported
	}
	handle.mu.RLock()
	defer handle.mu.RUnlock()
	if handle.closed {
		return false, errProcessIdentityClosed
	}
	if handle.signalFn == nil {
		return false, errProcessIdentitySignalUnsupported
	}
	return handle.signalFn(signal)
}

func (handle *processIdentityHandle) alive() (bool, error) {
	if handle == nil {
		return false, errProcessIdentitySignalUnsupported
	}
	handle.mu.RLock()
	defer handle.mu.RUnlock()
	if handle.closed {
		return false, errProcessIdentityClosed
	}
	if handle.aliveFn == nil {
		return false, errProcessIdentitySignalUnsupported
	}
	return handle.aliveFn()
}

func (handle *processIdentityHandle) close() error {
	if handle == nil {
		return nil
	}
	handle.mu.Lock()
	defer handle.mu.Unlock()
	if handle.closed {
		return nil
	}
	handle.closed = true
	if handle.closeFn == nil {
		return nil
	}
	return handle.closeFn()
}

// Valid reports whether identity contains enough information to authorize a
// later signal. It does not inspect the current process table.
func (identity ProcessIdentity) Valid() bool {
	return identity.PID > 0 && identity.StartToken != ""
}

// Close releases the kernel handle retained by CaptureProcessIdentity.
// ProcessIdentity is intentionally copyable; all copies share one idempotent
// close operation.
func (identity ProcessIdentity) Close() error {
	return identity.handle.close()
}

// CloseProcessIdentities releases every retained handle still owned by the
// caller. It is safe to call on aliases and duplicate identities because
// Close is shared and idempotent. After ownership is transferred to a reaper,
// the former owner must not call this function: closing any alias closes the
// retained handle for every copy.
func CloseProcessIdentities(identities []ProcessIdentity) {
	for _, identity := range identities {
		_ = identity.Close()
	}
}

// ProcessReapTiming controls the three bounded waits in
// ReapProcessIdentities. A zero duration skips that wait, which is useful for
// callers that already supplied the corresponding grace period.
type ProcessReapTiming struct {
	InitialGrace time.Duration
	TermGrace    time.Duration
	KillWait     time.Duration
	PollInterval time.Duration
}

var (
	processStartIdentityFn   = readProcessStartIdentity
	processCaptureIdentityFn = captureProcessIdentity
	processSignalIdentityFn  = signalProcessIdentity
	processIdentityAliveFn   = processIdentityAlive
)

// signalProcessIfCurrent is the only destructive-signal gateway. The
// platform implementation must bind the signal to the captured process
// lifetime; a separate raw-PID check followed by kill is not sufficient.
func signalProcessIfCurrent(identity ProcessIdentity, signal os.Signal) (bool, error) {
	if !identity.Valid() {
		return false, nil
	}
	return processSignalIdentityFn(identity, signal)
}

// IsProcessIdentityNotFound reports whether capture failed because the process
// no longer exists. Other capture failures are indeterminate and must not be
// treated as proof that the process is dead.
func IsProcessIdentityNotFound(err error) bool {
	return errors.Is(err, errProcessIdentityNotFound)
}

// ReadProcessStartToken returns the durable kernel start token used for
// persistence and diagnostics. It deliberately retains no signal handle and
// therefore must never be used as destructive authority.
func ReadProcessStartToken(pid int) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("tmux: invalid process pid %d", pid)
	}
	token, err := processStartIdentityFn(pid)
	if err != nil {
		return "", fmt.Errorf("tmux: read process start token for pid %d: %w", pid, err)
	}
	if token == "" {
		return "", fmt.Errorf("tmux: empty process start token for pid %d", pid)
	}
	return token, nil
}

// CaptureProcessIdentity records a kernel-derived identity for pid. Linux
// capture retains one pidfd for the identity's entire destructive lifecycle;
// a raw PID plus start token is never Linux signal authority.
func CaptureProcessIdentity(pid int) (ProcessIdentity, error) {
	if pid <= 0 {
		return ProcessIdentity{}, fmt.Errorf("tmux: invalid process pid %d", pid)
	}
	identity, err := processCaptureIdentityFn(pid)
	if err != nil {
		return ProcessIdentity{}, fmt.Errorf("tmux: capture process identity for pid %d: %w", pid, err)
	}
	if identity.PID != pid || !identity.Valid() {
		_ = identity.Close()
		return ProcessIdentity{}, fmt.Errorf("tmux: incomplete process identity for pid %d", pid)
	}
	return identity, nil
}

// CaptureProcessIdentities captures one identity per still-existing PID,
// preserving input order. A process that disappeared during the snapshot is
// omitted; every other read failure aborts the capture so a caller cannot
// destructively mutate tmux and later discover that part of its process tree
// had only raw-PID authority.
func CaptureProcessIdentities(pids []int) ([]ProcessIdentity, error) {
	if len(pids) == 0 {
		return nil, nil
	}
	identities := make([]ProcessIdentity, 0, len(pids))
	seen := make(map[int]struct{}, len(pids))
	for _, pid := range pids {
		if _, duplicate := seen[pid]; duplicate {
			continue
		}
		identity, err := CaptureProcessIdentity(pid)
		if err != nil {
			if IsProcessIdentityNotFound(err) {
				continue
			}
			CloseProcessIdentities(identities)
			return nil, err
		}
		seen[pid] = struct{}{}
		identities = append(identities, identity)
	}
	return identities, nil
}

// captureStableProcessTree captures retained identities, then inventories the
// tree again before its caller mutates tmux. Exact set equality is required
// unless every inventoried process has already exited; that case needs only the
// tmux identity check and no process reap. Partial capture fails closed.
func captureStableProcessTree(
	probe func() ([]int, error),
	expectedRoot int,
	changed error,
) ([]ProcessIdentity, error) {
	before, err := probe()
	if err != nil {
		return nil, err
	}
	if expectedRoot <= 0 && len(before) > 0 {
		expectedRoot = before[0]
	}
	if !validProcessTree(before, expectedRoot) {
		return nil, changed
	}

	identities, err := CaptureProcessIdentities(before)
	if err != nil {
		return nil, err
	}
	owned := true
	defer func() {
		if owned {
			CloseProcessIdentities(identities)
		}
	}()
	if len(identities) != 0 && !identitiesExactlyCoverPIDs(identities, before) {
		return nil, changed
	}

	after, err := probe()
	if err != nil {
		return nil, err
	}
	if !validProcessTree(after, expectedRoot) || !samePIDSet(before, after) {
		return nil, changed
	}
	for _, identity := range identities {
		if !ProcessIdentityMatches(identity) {
			return nil, changed
		}
	}
	owned = false
	return identities, nil
}

func validProcessTree(pids []int, expectedRoot int) bool {
	if len(pids) == 0 || expectedRoot <= 0 || pids[0] != expectedRoot {
		return false
	}
	seen := make(map[int]struct{}, len(pids))
	for _, pid := range pids {
		if pid <= 0 {
			return false
		}
		if _, duplicate := seen[pid]; duplicate {
			return false
		}
		seen[pid] = struct{}{}
	}
	return true
}

func identitiesExactlyCoverPIDs(identities []ProcessIdentity, pids []int) bool {
	if len(identities) != len(pids) {
		return false
	}
	seen := make(map[int]struct{}, len(identities))
	for _, identity := range identities {
		if !identity.Valid() {
			return false
		}
		seen[identity.PID] = struct{}{}
	}
	for _, pid := range pids {
		if _, ok := seen[pid]; !ok {
			return false
		}
	}
	return len(seen) == len(pids)
}

func samePIDSet(left, right []int) bool {
	if len(left) != len(right) {
		return false
	}
	seen := make(map[int]struct{}, len(left))
	for _, pid := range left {
		seen[pid] = struct{}{}
	}
	for _, pid := range right {
		if _, ok := seen[pid]; !ok {
			return false
		}
	}
	return true
}

// ProcessIdentityMatches reports whether identity still names the same process
// lifetime. Any inability to read the current kernel identity fails closed.
func ProcessIdentityMatches(identity ProcessIdentity) bool {
	if !identity.Valid() {
		return false
	}
	alive, err := processIdentityAliveFn(identity)
	return err == nil && alive
}

// ReapProcessIdentities takes ownership of identities and escalates TERM to
// KILL while retaining the identity captured before the caller's destructive
// tmux mutation. A signal is sent
// only when the platform can bind it to that exact process lifetime. If the
// PID was recycled, identity is indeterminate, or identity-bound signaling is
// unavailable, no signal is sent.
func ReapProcessIdentities(identities []ProcessIdentity, timing ProcessReapTiming) {
	if len(identities) == 0 {
		return
	}
	defer CloseProcessIdentities(identities)
	if timing.InitialGrace > 0 {
		time.Sleep(timing.InitialGrace)
	}
	unsupportedSignalWarned := false
	logSignalError := func(event string, identity ProcessIdentity, err error) {
		if errors.Is(err, errProcessIdentitySignalUnsupported) {
			if !unsupportedSignalWarned {
				respawnLog.Warn("process_identity_signal_unsupported",
					slog.Int("process_count", len(identities)), slog.Any("error", err))
				unsupportedSignalWarned = true
			}
			return
		}
		if !errors.Is(err, syscall.ESRCH) {
			respawnLog.Debug(event,
				slog.Int("pid", identity.PID), slog.Any("error", err))
		}
	}

	termed := make([]ProcessIdentity, 0, len(identities))
	for _, identity := range identities {
		// Keep the identity check immediately adjacent to the destructive
		// signal. A command-name allowlist is intentionally not involved.
		signaled, err := signalProcessIfCurrent(identity, syscall.SIGTERM)
		if err != nil {
			logSignalError("process_sigterm_failed", identity, err)
			continue
		}
		if !signaled {
			continue
		}
		termed = append(termed, identity)
	}
	if len(termed) == 0 {
		return
	}
	if timing.TermGrace > 0 {
		time.Sleep(timing.TermGrace)
	}

	killed := make([]ProcessIdentity, 0, len(termed))
	for _, identity := range termed {
		// Revalidate independently of the TERM-stage result: the original
		// process can exit and its numeric PID can be reused during the grace
		// window.
		signaled, err := signalProcessIfCurrent(identity, syscall.SIGKILL)
		if err != nil {
			logSignalError("process_sigkill_failed", identity, err)
			continue
		}
		if !signaled {
			continue
		}
		killed = append(killed, identity)
	}
	if len(killed) == 0 || timing.KillWait <= 0 {
		return
	}

	pollInterval := timing.PollInterval
	if pollInterval <= 0 {
		pollInterval = 20 * time.Millisecond
	}
	deadline := time.Now().Add(timing.KillWait)
	for {
		remaining := killed[:0]
		for _, identity := range killed {
			if ProcessIdentityMatches(identity) {
				remaining = append(remaining, identity)
			}
		}
		if len(remaining) == 0 {
			return
		}
		killed = remaining
		if !time.Now().Before(deadline) {
			pids := make([]int, 0, len(killed))
			for _, identity := range killed {
				pids = append(pids, identity.PID)
			}
			respawnLog.Warn("process_sigkill_unverified",
				slog.Any("pids", pids), slog.Duration("waited", timing.KillWait))
			return
		}
		time.Sleep(pollInterval)
	}
}
