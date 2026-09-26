package session

import (
	"errors"
	"fmt"
	"os/exec"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// importTmuxSessionForTest runs the TUI import on a private configured socket:
// DiscoverExistingTmuxSessions, then the insert that persists each discovered
// instance. It returns the instance built around sessionName.
func importTmuxSessionForTest(t *testing.T, storage *Storage, socketName, sessionName string) *Instance {
	t.Helper()
	imported := discoveredSessionForTest(t, socketName, sessionName)
	if imported.TmuxSocketName != socketName || imported.GetTmuxSession().SocketName != socketName {
		t.Fatalf("imported runtime socket = %q (wrapper %q), want the socket it was found on %q",
			imported.TmuxSocketName, imported.GetTmuxSession().SocketName, socketName)
	}
	if err := storage.InsertSessionAndVerify(imported, nil); err != nil {
		t.Fatal(err)
	}
	return imported
}

// Importing a session is the user's grant of ownership. A live session Agent
// Deck never started, and a recovered orphan still stamped for the instance it
// lost, both used to be lifecycle-dead once imported: no inventory could prove
// them, so stop and delete refused with ErrRuntimeOwnershipUnproven. The
// import now stamps the session for its new instance, so stop and delete
// really end it and record the result.
func TestImportedRuntime_StopAndDeleteEndTheImportedSession(t *testing.T) {
	skipIfNoTmuxBinary(t)
	sessions := []struct {
		name    string
		session string
		prepare func(t *testing.T, socketName, sessionName string)
	}{
		{name: "session Agent Deck never started", session: "user-work",
			prepare: func(t *testing.T, socketName, sessionName string) {
				startUnownedTmuxSession(t, socketName, sessionName)
			}},
		{name: "recovered orphan stamped for a lost instance", session: tmux.SessionPrefix + "lost_1234abcd",
			prepare: func(t *testing.T, socketName, sessionName string) {
				startUnownedTmuxSession(t, socketName, sessionName)
				orphan := tmux.ReconnectSessionLazy(sessionName, "lost", t.TempDir(), "", string(StatusIdle))
				orphan.SocketName = socketName
				lost := statedb.RuntimeState{
					InstanceID: "lost-instance", Generation: 3, Status: string(StatusIdle),
					TmuxSession: sessionName, TmuxSocketName: socketName, LastStartedAt: time.Now(),
				}
				if err := stampRuntimeCandidate(orphan, lost, "", ""); err != nil {
					t.Fatalf("stamp orphan for its lost instance: %v", err)
				}
			}},
	}
	operations := []struct {
		name string
		run  func(*Instance) error
	}{
		{name: "stop", run: func(inst *Instance) error { return inst.KillCaptured(inst.CaptureRuntimeSelection()) }},
		{name: "delete", run: func(inst *Instance) error { return inst.DeleteCaptured(inst.CaptureRuntimeSelection()) }},
	}
	for _, session := range sessions {
		for _, operation := range operations {
			t.Run(session.name+"/"+operation.name, func(t *testing.T) {
				socketName := fmt.Sprintf("adtest-import-%d", time.Now().UnixNano())
				session.prepare(t, socketName, session.session)
				storage, err := NewStorageWithProfile("_test_import_" + operation.name)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = storage.Close() })
				inst := importTmuxSessionForTest(t, storage, socketName, session.session)

				if reconciled, err := inst.ReconcileRuntime(); err != nil || !reconciled.Live {
					t.Fatalf("imported runtime reconciliation = %+v err=%v, want it live", reconciled, err)
				}
				if err := operation.run(inst); err != nil {
					t.Fatalf("%s of the imported session: %v", operation.name, err)
				}
				if tmuxSessionAnswers(socketName, session.session) {
					t.Fatalf("%s left the imported session %q running", operation.name, session.session)
				}
				stored, found, err := storage.GetDB().ReadRuntimeState(inst.ID)
				if err != nil {
					t.Fatal(err)
				}
				switch operation.name {
				case "stop":
					if !found || stored.Status != string(StatusStopped) {
						t.Fatalf("stored runtime after stop = %#v found=%v, want stopped", stored, found)
					}
				case "delete":
					if found {
						t.Fatalf("stored runtime after delete = %#v, want the row gone", stored)
					}
				}
			})
		}
	}
}

// Restart proves the imported session is the predecessor it may replace: it
// ends that session and publishes the replacement as generation 1.
func TestImportedRuntime_RestartReplacesTheImportedSession(t *testing.T) {
	skipIfNoTmuxBinary(t)
	socketName := fmt.Sprintf("adtest-import-restart-%d", time.Now().UnixNano())
	startUnownedTmuxSession(t, socketName, "user-work")
	storage, err := NewStorageWithProfile("_test_import_restart")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	inst := importTmuxSessionForTest(t, storage, socketName, "user-work")

	runtime, err := inst.RestartRuntime()
	if err != nil {
		t.Fatalf("restart of the imported session: %v", err)
	}
	if tmuxSessionAnswers(socketName, "user-work") {
		t.Fatal("restart left the imported predecessor running")
	}
	durable, found, err := storage.GetDB().ReadRuntimeState(inst.ID)
	if err != nil || !found || durable.Generation != 1 || durable != runtime {
		t.Fatalf("stored runtime after restart = %#v found=%v err=%v, want generation 1 equal to %#v", durable, found, err, runtime)
	}
	if !tmuxSessionAnswers(socketName, durable.TmuxSession) {
		t.Fatalf("restart's replacement %q is not live", durable.TmuxSession)
	}
}

// discoveredSessionForTest runs only the discovery half of the import, without
// persisting anything, for checks of the stamp itself.
func discoveredSessionForTest(t *testing.T, socketName, sessionName string) *Instance {
	t.Helper()
	oldDefault := tmux.DefaultSocketName()
	tmux.SetDefaultSocketName(socketName)
	t.Cleanup(func() { tmux.SetDefaultSocketName(oldDefault) })
	discovered, err := DiscoverExistingTmuxSessions(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, inst := range discovered {
		if sess := inst.GetTmuxSession(); sess != nil && sess.Name == sessionName {
			return inst
		}
	}
	t.Fatalf("%q was not discovered", sessionName)
	return nil
}

// assertTmuxSessionUnstamped fails when anything stamped sessionName: a
// complete cleanup stamp for any instance, or the environment's instance ID.
func assertTmuxSessionUnstamped(t *testing.T, socketName, sessionName, when string) {
	t.Helper()
	stamped, err := tmux.ListRuntimeCleanupCandidates(socketName, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range stamped {
		if candidate.SessionName == sessionName {
			t.Fatalf("%s: %q carries a cleanup stamp for %q", when, sessionName, candidate.InstanceID)
		}
	}
	if out, err := exec.Command("tmux", "-L", socketName, "show-environment", "-t", "="+sessionName, "AGENTDECK_INSTANCE_ID").Output(); err == nil {
		t.Fatalf("%s: %q carries %q", when, sessionName, out)
	}
}

// The import's grant waits for the durable row. Discovery stamps nothing, so
// an import whose insert fails leaves no session stamped for an instance that
// was never persisted. The insert that commits stamps the instance's
// generation-zero identity into both inventories' evidence: the environment
// the reconcile snapshot reads and the session-local cleanup options a
// conditional kill compares.
func TestImportedRuntime_StampWaitsForTheCommittedInsert(t *testing.T) {
	skipIfNoTmuxBinary(t)
	socketName := fmt.Sprintf("adtest-import-stamp-%d", time.Now().UnixNano())
	startUnownedTmuxSession(t, socketName, "user-work")
	inst := discoveredSessionForTest(t, socketName, "user-work")
	assertTmuxSessionUnstamped(t, socketName, "user-work", "after discovery")

	// Another writer already owns the instance's ID, so the insert fails.
	lost, err := NewStorageWithProfile("_test_import_lost_insert")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lost.Close() })
	winner := NewInstance("same id", t.TempDir())
	winner.ID = inst.ID
	if err := lost.InsertSessionAndVerify(winner, nil); err != nil {
		t.Fatal(err)
	}
	if err := lost.InsertSessionAndVerify(inst, nil); !errors.Is(err, ErrSessionAlreadyExists) {
		t.Fatalf("insert over an existing ID = %v, want ErrSessionAlreadyExists", err)
	}
	assertTmuxSessionUnstamped(t, socketName, "user-work", "after a failed insert")

	storage, err := NewStorageWithProfile("_test_import_stamp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	if err := storage.InsertSessionAndVerify(inst, nil); err != nil {
		t.Fatal(err)
	}
	inventory, err := tmux.ListRuntimeGenerationCandidates(socketName, inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory) != 1 || inventory[0].SessionName != "user-work" || !inventory[0].GenerationKnown || inventory[0].Generation != 0 {
		t.Fatalf("cleanup inventory for the imported instance = %#v, want user-work at generation 0", inventory)
	}
	candidates, err := tmux.ListRuntimeCandidates(socketName, inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].ProofError != "" || !candidates[0].GenerationKnown || candidates[0].Generation != 0 {
		t.Fatalf("reconcile snapshot for the imported instance = %#v, want one proved generation-0 candidate", candidates)
	}
	if out, err := exec.Command("tmux", "-L", socketName, "show-environment", "-t", "=user-work", "AGENTDECK_RUNTIME_STARTED_UNIX_NANO").Output(); err != nil ||
		string(out) != "AGENTDECK_RUNTIME_STARTED_UNIX_NANO=0\n" {
		t.Fatalf("imported start stamp = %q err=%v, want 0 for an unknown start", out, err)
	}
}
