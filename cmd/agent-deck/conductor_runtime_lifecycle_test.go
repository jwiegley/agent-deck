package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

type fakeConductorRuntimeTarget struct {
	selection session.RuntimeSelection
	killed    []session.RuntimeSelection
	deleted   []session.RuntimeSelection
}

func TestRuntimeLifecycle_ConductorRemovalFencesDirectoryTeardown(t *testing.T) {
	raw, err := os.ReadFile("conductor_cmd.go")
	if err != nil {
		t.Fatal(err)
	}
	body := extractFuncBody(string(raw), "handleConductorTeardown")
	actionAt := strings.Index(body, "applyConductorRuntimeAction(")
	teardownAt := strings.Index(body, "session.TeardownConductor(")
	if actionAt < 0 || teardownAt < 0 || actionAt >= teardownAt {
		t.Fatalf("conditional runtime action must precede directory teardown")
	}
	if strings.Contains(body, "RemoveSessionAndVerify(") {
		t.Fatalf("conductor teardown must not issue an unconditional second delete")
	}
}

func (f *fakeConductorRuntimeTarget) CaptureRuntimeSelection() session.RuntimeSelection {
	return f.selection
}

func (f *fakeConductorRuntimeTarget) KillAndWaitCaptured(selection session.RuntimeSelection) error {
	f.killed = append(f.killed, selection)
	return nil
}

func (f *fakeConductorRuntimeTarget) DeleteAndWaitCaptured(selection session.RuntimeSelection) error {
	f.deleted = append(f.deleted, selection)
	return nil
}

func TestRuntimeLifecycle_ConductorActionUsesCapturedSelection(t *testing.T) {
	selection := session.RuntimeSelection{State: statedb.RuntimeState{
		InstanceID: "conductor", Generation: 7, StatusRevision: 3,
		TmuxSession: "conductor-g7", TmuxSocketName: "isolated", Status: "running",
	}}

	stop := &fakeConductorRuntimeTarget{selection: selection}
	if err := applyConductorRuntimeAction(stop, stop.CaptureRuntimeSelection(), false); err != nil {
		t.Fatal(err)
	}
	if len(stop.killed) != 1 || stop.killed[0] != selection || len(stop.deleted) != 0 {
		t.Fatalf("stop action = killed %#v, deleted %#v", stop.killed, stop.deleted)
	}

	remove := &fakeConductorRuntimeTarget{selection: selection}
	if err := applyConductorRuntimeAction(remove, remove.CaptureRuntimeSelection(), true); err != nil {
		t.Fatal(err)
	}
	if len(remove.deleted) != 1 || remove.deleted[0] != selection || len(remove.killed) != 0 {
		t.Fatalf("remove action = killed %#v, deleted %#v", remove.killed, remove.deleted)
	}
}

// teardownTestConductor creates conductor name in the default profile with its
// heartbeat on and its session row present, and returns the isolated home.
func teardownTestConductor(t *testing.T, name string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	project := filepath.Join(home, "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := session.SaveConductorMeta(&session.ConductorMeta{
		Name: name, Profile: "default", HeartbeatEnabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	out, stderr, code := runAgentDeck(t, home, "-p", "default", "add", "-t", session.ConductorSessionTitle(name), "-c", "claude", "--no-parent", "--json", project)
	if code != 0 {
		t.Fatalf("add conductor session: exit=%d stdout=%q stderr=%q", code, out, stderr)
	}
	return home
}

func teardownTestStateDBPath(home string) string {
	return filepath.Join(home, ".local", "share", "agent-deck", "profiles", "default", "state.db")
}

func execTeardownTestStateDB(t *testing.T, home, statement string, args ...any) {
	t.Helper()
	db, err := statedb.Open(teardownTestStateDBPath(home))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.DB().Exec(statement, args...); err != nil {
		t.Fatalf("%s: %v", statement, err)
	}
}

// registerLiveLegacyWriter registers this test process as a live writer from
// an older schema epoch, like an old binary still running during an upgrade,
// so every runtime action the CLI takes in the profile aborts.
func registerLiveLegacyWriter(t *testing.T, home string) {
	t.Helper()
	now := time.Now().Unix()
	execTeardownTestStateDB(t, home, `INSERT OR REPLACE INTO instance_heartbeats
		(pid, started, heartbeat, is_primary) VALUES (?, ?, ?, 0)`, os.Getpid(), now, now)
}

// failGroupWrites makes every write to the profile's groups fail, so
// SaveGroupsOnly fails after the conditional delete has committed.
func failGroupWrites(t *testing.T, home string) {
	t.Helper()
	for _, op := range []string{"INSERT", "UPDATE", "DELETE"} {
		execTeardownTestStateDB(t, home, `CREATE TRIGGER teardown_test_fail_group_`+strings.ToLower(op)+
			` BEFORE `+op+` ON groups BEGIN SELECT RAISE(ABORT, 'injected group write failure'); END`)
	}
}

// corruptProfileStore replaces the profile's state.db with bytes SQLite
// rejects, so the profile cannot be opened at all.
func corruptProfileStore(t *testing.T, home string) {
	t.Helper()
	dbPath := teardownTestStateDBPath(home)
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(dbPath + suffix); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(dbPath, []byte(strings.Repeat("not a database\n", 512)), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A zero exit, "success": true or "Teardown complete." tells the caller the
// heartbeat is off (upstream ee026742), which a skipped conductor does not
// guarantee. Every skipped target must fail the command with its reason.
func TestRuntimeLifecycle_ConductorTeardownReportsAbortedTarget(t *testing.T) {
	for _, tc := range []struct {
		name   string
		remove bool
		inject func(*testing.T, string)
		reason string
		// heartbeat is the heartbeat_enabled flag the skipped conductor keeps.
		heartbeat bool
	}{
		// A refused runtime action leaves the heartbeat and directory alone.
		{"runtime-action", false, registerLiveLegacyWriter, "runtime action aborted for conductor-abort-ops: " + statedb.ErrIncompatibleWriterSchema.Error(), true},
		{"runtime-action-remove", true, registerLiveLegacyWriter, "runtime action aborted for conductor-abort-ops: " + statedb.ErrIncompatibleWriterSchema.Error(), true},
		// An unopenable store leaves the runtime unobserved and unstopped.
		{"store-open", true, corruptProfileStore, "failed to open profile default: ", true},
		// The runtime row is already deleted, so its heartbeat goes with it;
		// only the directory stays for the rerun.
		{"group-save", true, failGroupWrites, "failed to save groups in default: ", false},
	} {
		for _, jsonOutput := range []bool{true, false} {
			mode := "human"
			if jsonOutput {
				mode = "json"
			}
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				const name = "abort-ops"
				home := teardownTestConductor(t, name)
				tc.inject(t, home)
				args := []string{"conductor", "teardown", name}
				if tc.remove {
					args = append(args, "--remove")
				}
				if jsonOutput {
					args = append(args, "--json")
				}
				out, stderr, code := runAgentDeck(t, home, args...)
				if code != 1 {
					t.Fatalf("teardown with a skipped conductor must exit 1: exit=%d stdout=%q stderr=%q", code, out, stderr)
				}
				if jsonOutput {
					var result struct {
						Success  *bool    `json:"success"`
						Removed  bool     `json:"removed"`
						Teardown []string `json:"teardown"`
						Aborted  []struct {
							Name    string `json:"name"`
							Profile string `json:"profile"`
							Reason  string `json:"reason"`
						} `json:"aborted"`
					}
					if err := json.Unmarshal([]byte(out), &result); err != nil {
						t.Fatalf("decode teardown: %v: %q", err, out)
					}
					if result.Success == nil || *result.Success || result.Removed != tc.remove || result.Teardown == nil || len(result.Teardown) != 0 {
						t.Fatalf("teardown must report failure and an empty teardown list: %s", out)
					}
					if len(result.Aborted) != 1 || result.Aborted[0].Name != name || result.Aborted[0].Profile != "default" ||
						!strings.Contains(result.Aborted[0].Reason, tc.reason) {
						t.Fatalf("aborted = %+v, want %s with reason %q", result.Aborted, name, tc.reason)
					}
				} else {
					if strings.Contains(out, "Teardown complete.") {
						t.Fatalf("human teardown must not claim completion: stdout=%q", out)
					}
					summary := "Teardown incomplete: 1 of 1 conductor(s) not torn down:\n  " + name + " (profile: default): " + tc.reason
					if !strings.Contains(stderr, summary) {
						t.Fatalf("stderr must name the skipped conductor and why, want %q in %q", summary, stderr)
					}
				}
				meta, err := session.LoadConductorMeta(name)
				if err != nil {
					t.Fatalf("a skipped conductor must keep its directory: %v", err)
				}
				if meta.HeartbeatEnabled != tc.heartbeat {
					t.Fatalf("heartbeat_enabled = %v, want %v", meta.HeartbeatEnabled, tc.heartbeat)
				}
			})
		}
	}
}

// The success path keeps upstream's report: exit 0, no "aborted" key or
// incomplete summary, and the heartbeat off or the conductor removed.
func TestRuntimeLifecycle_ConductorTeardownSuccessReportsComplete(t *testing.T) {
	for _, tc := range []struct {
		name       string
		remove     bool
		jsonOutput bool
	}{{"keep/json", false, true}, {"remove/json", true, true}, {"remove/human", true, false}} {
		t.Run(tc.name, func(t *testing.T) {
			const name = "done-ops"
			home := teardownTestConductor(t, name)
			args := []string{"conductor", "teardown", name}
			if tc.remove {
				args = append(args, "--remove")
			}
			if tc.jsonOutput {
				args = append(args, "--json")
			}
			out, stderr, code := runAgentDeck(t, home, args...)
			if code != 0 {
				t.Fatalf("teardown exited %d: stdout=%q stderr=%q", code, out, stderr)
			}
			if tc.jsonOutput {
				var result map[string]json.RawMessage
				if err := json.Unmarshal([]byte(out), &result); err != nil {
					t.Fatalf("decode teardown: %v: %q", err, out)
				}
				var teardown []string
				if err := json.Unmarshal(result["teardown"], &teardown); err != nil {
					t.Fatalf("decode teardown list: %v: %q", err, out)
				}
				if _, ok := result["aborted"]; ok || string(result["success"]) != "true" || len(teardown) != 1 || teardown[0] != name {
					t.Fatalf("teardown must report complete success: %s", out)
				}
			} else if !strings.Contains(out, "Teardown complete.") || strings.Contains(stderr, "Teardown incomplete") {
				t.Fatalf("human teardown must report completion: stdout=%q stderr=%q", out, stderr)
			}
			if !tc.remove {
				meta, err := session.LoadConductorMeta(name)
				if err != nil || meta.HeartbeatEnabled {
					t.Fatalf("heartbeat after teardown: meta=%+v err=%v", meta, err)
				}
				return
			}
			if _, err := session.LoadConductorMeta(name); err == nil {
				t.Fatal("teardown --remove must remove the conductor directory")
			}
			listed, stderr, code := runAgentDeck(t, home, "-p", "default", "list", "--json")
			if code != 0 || strings.Contains(listed, session.ConductorSessionTitle(name)) {
				t.Fatalf("teardown --remove must remove the session row: exit=%d stdout=%q stderr=%q", code, listed, stderr)
			}
		})
	}
}
