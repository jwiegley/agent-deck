package session

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// restartPersistFixture opens real storage on its own profile, registers it as
// the process-wide StateDB the way main.go and NewHome do, and returns one
// saved instance whose stored tmux name is the "old" one a restart will kill.
func restartPersistFixture(t *testing.T, profile, oldName string) (*Storage, *Instance) {
	t.Helper()

	storage, err := NewStorageWithProfile(profile)
	if err != nil {
		t.Fatalf("NewStorageWithProfile(%q): %v", profile, err)
	}
	t.Cleanup(func() { _ = storage.Close() })

	db := storage.GetDB()
	if db == nil {
		t.Fatal("storage has no state database")
	}
	prev := statedb.GetGlobal()
	statedb.SetGlobal(db)
	t.Cleanup(func() { statedb.SetGlobal(prev) })

	inst := NewInstanceWithTool("restart-persist", t.TempDir(), "shell")
	inst.Status = StatusRunning
	inst.GroupPath = DefaultGroupPath
	inst.tmuxSession = tmux.ReconnectSessionLazy(oldName, inst.ID, inst.ProjectPath, "shell", "running")

	instances := []*Instance{inst}
	if err := storage.InsertSessionAndVerify(inst, NewGroupTree(instances)); err != nil {
		t.Fatalf("seed insert: %v", err)
	}
	if got := storedTmuxName(t, storage, inst.ID); got != oldName {
		t.Fatalf("fixture: stored tmux name = %q, want %q", got, oldName)
	}
	return storage, inst
}

// storedTmuxName round-trips through storage and returns the tmux session name
// on disk for id -- what a reload, or any other process, would see.
func storedTmuxName(t *testing.T, storage *Storage, id string) string {
	t.Helper()
	instances, _, err := storage.LoadWithGroups()
	if err != nil {
		t.Fatalf("LoadWithGroups: %v", err)
	}
	for _, inst := range instances {
		if inst.ID != id {
			continue
		}
		if sess := inst.GetTmuxSession(); sess != nil {
			return sess.Name
		}
		return ""
	}
	t.Fatalf("instance %q missing from storage", id)
	return ""
}

// TestRestartRecordsWhatItProduced drives the real Instance.Restart against
// a real tmux server (TestMain's isolated TMUX_TMPDIR keeps the host's sessions
// untouched) and asserts the name it mints reaches the state DB.
//
// This is the defect from #1870 exactly as a user hits it: restart() kills the
// old tmux session and recreateTmuxSession mints a fresh name through
// tmux.NewSession, and until this fix nothing on the CLI paths ever wrote that
// name down. The stored name kept naming the killed session, so agent-deck
// polled a session that did not exist and showed `error` for a process running
// fine, while the live tmux session was orphaned.
//
// The old name is deliberately one no tmux server has: that is what a session
// looks like after a reboot or a tmux server restart, and it is what forces
// restart() down the fallback-recreate path instead of a respawn-in-place fast
// path (which keeps the name and so needs no write).
func TestRestartRecordsWhatItProduced(t *testing.T) {
	skipIfNoTmuxBinary(t)

	const oldName = "agentdeck_restartpersist_deadbeef"
	storage, inst := restartPersistFixture(t, "_test_restart_tmux_persist", oldName)

	if err := inst.Restart(); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	t.Cleanup(func() {
		if sess := inst.GetTmuxSession(); sess != nil {
			_ = sess.Kill()
		}
	})

	live := inst.GetTmuxSession()
	if live == nil {
		t.Fatal("restart left no tmux session")
	}
	if live.Name == oldName {
		t.Fatalf("restart reused the tmux name %q; this test needs the recreate path that mints "+
			"a new one, otherwise it proves nothing", oldName)
	}

	if got := storedTmuxName(t, storage, inst.ID); got != live.Name {
		t.Errorf("stored tmux name = %q, want the minted %q: nothing recorded the restart's new "+
			"name, so agent-deck reports this live session as errored and its tmux session is "+
			"orphaned", got, live.Name)
	}

	recorded, err := inst.RestartTmuxNameRecorded()
	if !recorded || err != nil {
		t.Errorf("RestartTmuxNameRecorded() = (%v, %v), want (true, nil)", recorded, err)
	}

	// The status the restart ended in travels with the name: a row naming a
	// live pane but still marked error misreports the session just as badly.
	rows, err := storage.GetDB().LoadInstances()
	if err != nil {
		t.Fatalf("LoadInstances: %v", err)
	}
	for _, r := range rows {
		if r.ID == inst.ID && r.Status != string(inst.Status) {
			t.Errorf("stored status = %q, want %q", r.Status, inst.Status)
		}
	}

	// The write has to be identifiable as ours, or a TUI reads its own restart
	// as an external change and abandons the save carrying the rest of it.
	stamps := inst.RestartRecordStamps()
	current, err := storage.GetDB().LastModified()
	if err != nil {
		t.Fatalf("LastModified: %v", err)
	}
	if !stamps.SoleWriterSince(stamps.Before, current) {
		t.Errorf("restart write is not recognisable as our own: stamps=%+v current=%d", stamps, current)
	}
}

// A loaded instance must write restart bookkeeping to its own profile even
// when another profile is registered as the process-global database.
func TestRestartOutcomeUsesOwningProfile(t *testing.T) {
	skipIfNoTmuxBinary(t)

	suffix := randomString(8)
	storage, _ := restartPersistFixture(t, "_test_restart_owner_"+suffix, "agentdeck_restartprofile_deadbeef")
	instances, _, err := storage.LoadWithGroups()
	if err != nil || len(instances) != 1 {
		t.Fatalf("reload owning profile: instances=%d err=%v", len(instances), err)
	}
	inst := instances[0]

	other, err := NewStorageWithProfile("_test_restart_other_" + suffix)
	if err != nil {
		t.Fatalf("NewStorageWithProfile(other): %v", err)
	}
	t.Cleanup(func() { _ = other.Close() })
	previous := statedb.GetGlobal()
	statedb.SetGlobal(other.GetDB())
	t.Cleanup(func() { statedb.SetGlobal(previous) })

	if err := inst.Restart(); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	t.Cleanup(func() {
		if sess := inst.GetTmuxSession(); sess != nil {
			_ = sess.Kill()
		}
	})

	if recorded, err := inst.RestartTmuxNameRecorded(); !recorded || err != nil {
		t.Fatalf("RestartTmuxNameRecorded() = (%v, %v), want owning-profile success", recorded, err)
	}
	live := inst.GetTmuxSession()
	if live == nil || storedTmuxName(t, storage, inst.ID) != live.Name {
		t.Fatalf("owning profile did not retain the restarted runtime: live=%v", live)
	}
	rows, err := other.GetDB().LoadInstances()
	if err != nil {
		t.Fatalf("LoadInstances(other): %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("global non-owning profile gained %d rows", len(rows))
	}
}

// TestRestartReportsAnUnrecordedTmuxName pins the honesty half. If the write
// cannot land -- here because the row was deleted while the restart ran -- the
// restart still succeeded and the process is live, so Restart must not fail.
// It must, however, leave the caller able to say the outcome is unknown rather
// than print a plain success for a session agent-deck can no longer find.
func TestRestartReportsAnUnrecordedTmuxName(t *testing.T) {
	skipIfNoTmuxBinary(t)

	const oldName = "agentdeck_restartunrecorded_deadbeef"
	storage, inst := restartPersistFixture(t, "_test_restart_tmux_unrecorded", oldName)

	// Another process removes the parent after the physical runtime and its
	// authoritative generation committed, but before the restart can emit its
	// acknowledgement stamp. Deleting before Restart would merely prove the
	// lifecycle precondition rejects a missing parent without spawning.
	deleted := false
	previous := restartOutcomeBeforeWriteFn
	restartOutcomeBeforeWriteFn = func() {
		deleted = true
		if err := storage.GetDB().DeleteInstance(inst.ID); err != nil {
			t.Fatalf("DeleteInstance in restart outcome window: %v", err)
		}
	}
	t.Cleanup(func() { restartOutcomeBeforeWriteFn = previous })

	if err := inst.Restart(); err != nil {
		t.Fatalf("Restart returned %v; a restart whose bookkeeping failed still started the "+
			"process, and reporting it as failed invites a retry that leaks another tmux session", err)
	}
	if !deleted {
		t.Fatal("restart never crossed the post-commit acknowledgement window")
	}
	t.Cleanup(func() {
		if sess := inst.GetTmuxSession(); sess != nil {
			_ = sess.Kill()
		}
	})

	recorded, err := inst.RestartTmuxNameRecorded()
	if recorded {
		t.Fatal("RestartTmuxNameRecorded() = true after the row was deleted: the caller would " +
			"report a durable restart that was never recorded")
	}
	if !errors.Is(err, statedb.ErrInstanceNotStored) {
		t.Errorf("reason = %v, want it to wrap ErrInstanceNotStored so the message names the cause", err)
	}
}

// The restoration verdict is tested directly: the fork refuses an uncertain
// predecessor termination before attempting a replacement.
func TestRestoreTmuxBindingAfterSpawnIsKilled(t *testing.T) {
	skipIfNoTmuxBinary(t)
	const oldName = "agentdeck_restartbinding_deadbeef"
	storage, inst := restartPersistFixture(t, "_test_restart_binding_restore", oldName)
	realTmux := seedRealTmuxSession(t, oldName)
	previous := inst.GetTmuxSession()
	inst.recreateTmuxSession()
	restartBindingShim(t, realTmux, `*" new-session "*`)
	if err := inst.GetTmuxSession().Start("sleep 60"); err == nil {
		t.Fatal("the replacement spawn did not reach the injected failure")
	}
	inst.restoreTmuxBindingOnFailure(previous, false)
	if inst.GetTmuxSession() != previous || !realTmuxHasSession(realTmux, oldName) {
		t.Fatal("failed spawn lost its surviving predecessor binding")
	}
	if err := storage.SaveWithGroups([]*Instance{inst}, NewGroupTree([]*Instance{inst})); err != nil {
		t.Fatal(err)
	}
	if got := storedTmuxName(t, storage, inst.ID); got != oldName {
		t.Fatalf("stored binding = %q, want %q", got, oldName)
	}
}

// TestRestartTmuxNameRecordedDefaultsToRecorded keeps the accessor from
// reporting a problem on an instance that has never restarted -- callers use it
// to decide whether to warn, and a warning about a restart that did not happen
// is noise.
func TestRestartTmuxNameRecordedDefaultsToRecorded(t *testing.T) {
	inst := NewInstanceWithTool("never-restarted", t.TempDir(), "shell")
	inst.CreatedAt = time.Now()

	recorded, err := inst.RestartTmuxNameRecorded()
	if !recorded || err != nil {
		t.Errorf("RestartTmuxNameRecorded() = (%v, %v) before any restart, want (true, nil)", recorded, err)
	}
}

// restartBindingShim puts a tmux shim first on PATH that SIGKILLs itself for
// every invocation matching one of the given shell case patterns (matched
// against " $* ") and hands everything else to the real binary. A
// signal-killed client is exactly the indeterminate outcome the restore
// verdict has to reason about.
func restartBindingShim(t *testing.T, realTmux string, killPatterns ...string) {
	t.Helper()
	script := "#!/bin/sh\ncase \" $* \" in\n"
	for _, p := range killPatterns {
		script += "  " + p + ") kill -9 $$ ;;\n"
	}
	script += "esac\nexec " + realTmux + " \"$@\"\n"
	shimDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(shimDir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatalf("write tmux shim: %v", err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// seedRealTmuxSession starts a detached session with the real binary and
// returns the tmux path.
func seedRealTmuxSession(t *testing.T, name string) string {
	t.Helper()
	realTmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skipf("tmux not on PATH: %v", err)
	}
	if out, err := exec.Command(realTmux, "new-session", "-d", "-s", name, "sleep 60").CombinedOutput(); err != nil {
		t.Fatalf("seed old session %q: %v: %s", name, err, out)
	}
	t.Cleanup(func() { _ = exec.Command(realTmux, "kill-session", "-t", "="+name).Run() })
	return realTmux
}

// killSessionsCreatedDuring snapshots the server's sessions now and, at
// cleanup, kills any session that was not there before, so a replacement the
// restart created cannot leak whichever binding the instance ends up with.
func killSessionsCreatedDuring(t *testing.T, realTmux string) {
	t.Helper()
	list := func() map[string]bool {
		out, _ := exec.Command(realTmux, "list-sessions", "-F", "#{session_name}").Output()
		names := map[string]bool{}
		for _, n := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if n != "" {
				names[n] = true
			}
		}
		return names
	}
	before := list()
	t.Cleanup(func() {
		for n := range list() {
			if !before[n] {
				_ = exec.Command(realTmux, "kill-session", "-t", "="+n).Run()
			}
		}
	})
}

func realTmuxHasSession(realTmux, name string) bool {
	return exec.Command(realTmux, "has-session", "-t", "="+name).Run() == nil
}

func TestRestoreTmuxBindingDoesNotRebindToKilledPredecessor(t *testing.T) {
	skipIfNoTmuxBinary(t)
	const oldName = "agentdeck_restartbinding_killedok"
	_, inst := restartPersistFixture(t, "_test_restart_binding_killed_ok", oldName)
	realTmux := seedRealTmuxSession(t, oldName)
	killSessionsCreatedDuring(t, realTmux)
	previous := inst.GetTmuxSession()
	if out, err := exec.Command(realTmux, "kill-session", "-t", "="+oldName).CombinedOutput(); err != nil {
		t.Fatalf("kill predecessor: %v: %s", err, out)
	}
	inst.recreateTmuxSession()
	replacement := inst.GetTmuxSession()
	if err := replacement.Start("sleep 60"); err != nil {
		t.Fatal(err)
	}
	restartBindingShim(t, realTmux, `*" has-session "*`+oldName+`*`)
	inst.restoreTmuxBindingOnFailure(previous, true)
	if inst.GetTmuxSession() != replacement || !realTmuxHasSession(realTmux, replacement.Name) || realTmuxHasSession(realTmux, oldName) {
		t.Fatal("restoration replaced the live replacement with a killed predecessor")
	}
}

func TestRestoreTmuxBindingOnIndeterminateProbe(t *testing.T) {
	skipIfNoTmuxBinary(t)
	const oldName = "agentdeck_restartbinding_indeterminate"
	_, inst := restartPersistFixture(t, "_test_restart_binding_indeterminate", oldName)
	realTmux := seedRealTmuxSession(t, oldName)
	previous := inst.GetTmuxSession()
	inst.recreateTmuxSession()
	restartBindingShim(t, realTmux, `*" has-session "*`+oldName+`*`, `*" new-session "*`)
	if err := inst.GetTmuxSession().Start("sleep 60"); err == nil {
		t.Fatal("the replacement spawn did not reach the injected failure")
	}
	inst.restoreTmuxBindingOnFailure(previous, false)
	if inst.GetTmuxSession() != previous || !realTmuxHasSession(realTmux, oldName) {
		t.Fatal("an inconclusive probe discarded the surviving predecessor")
	}
}

// TestRestartDoesNotRestoreAuthoritativelyAbsentBinding: the old name is
// provably gone (a completed has-session says "can't find session"), so a
// failed spawn must not point the instance back at it.
func TestRestartDoesNotRestoreAuthoritativelyAbsentBinding(t *testing.T) {
	skipIfNoTmuxBinary(t)

	const oldName = "agentdeck_restartbinding_absent"
	_, inst := restartPersistFixture(t, "_test_restart_binding_absent", oldName)
	realTmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skipf("tmux not on PATH: %v", err)
	}
	if realTmuxHasSession(realTmux, oldName) {
		t.Fatalf("fixture: %q unexpectedly exists", oldName)
	}
	killSessionsCreatedDuring(t, realTmux)

	restartBindingShim(t, realTmux, `*" new-session "*`)

	if err := inst.Restart(); err == nil {
		t.Fatal("restart must fail when its new-session child is killed")
	}

	if live := inst.GetTmuxSession(); live != nil && live.Name == oldName {
		t.Fatalf("binding after failed restart = %q: restored a session a completed probe proved gone", oldName)
	}
}

func TestRestoreTmuxBindingKeepsExistingReplacement(t *testing.T) {
	skipIfNoTmuxBinary(t)
	const oldName = "agentdeck_restartbinding_replacement"
	_, inst := restartPersistFixture(t, "_test_restart_binding_replacement", oldName)
	realTmux := seedRealTmuxSession(t, oldName)
	killSessionsCreatedDuring(t, realTmux)
	previous := inst.GetTmuxSession()
	inst.recreateTmuxSession()
	replacement := inst.GetTmuxSession()
	if err := replacement.Start("sleep 60"); err != nil {
		t.Fatal(err)
	}
	inst.restoreTmuxBindingOnFailure(previous, false)
	if inst.GetTmuxSession() != replacement || !realTmuxHasSession(realTmux, replacement.Name) || !realTmuxHasSession(realTmux, oldName) {
		t.Fatal("restoration abandoned a live replacement")
	}
}
