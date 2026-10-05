package session

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

// errNoStateDB reports that this process has no state database open, so a
// restart's new tmux session name could not be recorded anywhere.
var errNoStateDB = errors.New("no state database open in this process")

// restartOutcomeBeforeWriteFn is a test seam for the only meaningful failure
// window: the runtime commit succeeded, then its logical parent disappeared
// before the acknowledgement write. Production leaves it as a no-op.
var restartOutcomeBeforeWriteFn = func() {}

// SetRestartOutcomeBeforeWriteForTest installs a hook at the post-commit,
// pre-acknowledgement boundary and returns a restore function. It lets command
// package tests exercise a peer deleting the logical parent in that exact
// window rather than deleting it before the lifecycle precondition runs.
func SetRestartOutcomeBeforeWriteForTest(fn func()) func() {
	previous := restartOutcomeBeforeWriteFn
	if fn == nil {
		fn = func() {}
	}
	restartOutcomeBeforeWriteFn = fn
	return func() { restartOutcomeBeforeWriteFn = previous }
}

// recordRestartOutcome acknowledges the exact runtime generation a restart
// just committed and emits the change-detection stamp used by other processes.
//
// restart()'s fallback-recreate path calls recreateTmuxSession, whose
// tmux.NewSession appends a fresh short id unconditionally, so the name that
// identifies the live process changes on every trip through that path. Until
// something writes the tmux_session column, that name exists only on this
// in-memory Instance: the stored one still names the session the restart
// killed, so the TUI polls a tmux session that does not exist and reports
// `error` for a healthy process, and the live tmux session is orphaned because
// nothing knows its name (#1870).
//
// Persisting here rather than at each call site is what makes the fix
// complete. The four CLI --restart paths in #1870 are the ones that were
// reported, but `session move --restart`, `session start`, `session restart`,
// the fleet recovery sweep and the web mutator all reach the same mint through
// Instance.Restart, and every one of them would need its own save otherwise.
//
// The write is a targeted compare-and-update, not a snapshot save. A CLI
// command loads its rows, restarts (seconds, sometimes more), and would then
// push that stale snapshot back over everything another process changed in the
// meantime — a far larger blast radius than the one column the restart
// actually invalidated.
//
// Failure is not returned to the caller, because the restart itself succeeded:
// the replacement process is running whether or not the name reached disk.
// Reporting it as a failed restart would invite the operator to restart again
// and leak another tmux session, which is the original bug's own failure mode.
// It is recorded instead, and RestartTmuxNameRecorded lets callers with no
// other save on their path say plainly that the outcome is unknown.
func (i *Instance) recordRestartOutcome(runtime statedb.RuntimeState, incarnation string) {
	restartOutcomeBeforeWriteFn()
	stamps, err := i.writeRestartOutcome(runtime, incarnation)

	i.restartTmuxRecordMu.Lock()
	i.restartTmuxRecordErr = err
	i.restartTmuxRecordStamps = stamps
	i.restartTmuxRecordMu.Unlock()

	if err != nil {
		sessionLog.Warn("restart_outcome_persist_failed",
			slog.String("instance_id", i.ID),
			slog.String("error", err.Error()))
	}
}

// writeRestartOutcome performs the targeted write and reports what stopped it.
func (i *Instance) writeRestartOutcome(runtime statedb.RuntimeState, incarnation string) (statedb.WriteStamps, error) {
	if runtime.TmuxSession == "" {
		return statedb.WriteStamps{}, errors.New("restart left no tmux session name to record")
	}

	// The instance's profile database is authoritative. The process-wide
	// fallback exists for unsaved/legacy callers, but must not redirect a
	// loaded instance when another profile happens to be globally active.
	db := i.restartPersistenceDB()
	if db == nil {
		return statedb.WriteStamps{}, errNoStateDB
	}
	stamps, err := db.WriteRestartOutcome(runtime, incarnation)
	if err != nil {
		return statedb.WriteStamps{}, fmt.Errorf("record restart outcome for tmux session %q: %w", runtime.TmuxSession, err)
	}
	return stamps, nil
}

// RestartTmuxNameRecorded reports whether the last restart of this instance
// managed to record what it produced, and if not, why.
//
// It answers a question a CLI command cannot otherwise answer: the restart
// returned nil, so the process is live, but is agent-deck able to find it
// again? A command that prints "restarted" without checking is claiming a
// durability it did not verify — and the failure it hides is precisely the one
// that makes a running session look broken.
//
// Returns (true, nil) before any restart on this Instance: there is no
// unrecorded restart to warn about.
func (i *Instance) RestartTmuxNameRecorded() (bool, error) {
	i.restartTmuxRecordMu.Lock()
	defer i.restartTmuxRecordMu.Unlock()
	return i.restartTmuxRecordErr == nil, i.restartTmuxRecordErr
}

// RestartRecordStamps returns the last_modified values the last restart's
// targeted write replaced and wrote, or the zero value if it recorded nothing.
//
// A process that both wrote and reads this database needs them to tell its own
// bump apart from another process's change. Without that, the TUI reads the
// write its own restart just made as an external change and abandons the save
// that would have persisted the rest of the restart.
func (i *Instance) RestartRecordStamps() statedb.WriteStamps {
	i.restartTmuxRecordMu.Lock()
	defer i.restartTmuxRecordMu.Unlock()
	if i.restartTmuxRecordErr != nil {
		return statedb.WriteStamps{}
	}
	return i.restartTmuxRecordStamps
}
