package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/asheshgoplani/agent-deck/internal/testutil"
)

// Issues #2396 and #2400 (reproduced live with codex-cli 0.147): `launch`
// never persisted the Codex identity (PostStartSync skips Codex and the async
// detection dies with the launch process), so the registry row had no
// codex_session_id until a follow-up send hydrated it. `session show` derived
// the identity from the live pane, which hid the gap: first-turn output fell
// back to unbound pane text (#2396), and archiving killed the only evidence,
// losing codex_session_id and transcript_path for good (#2400).
//
// The fork keeps the identity in the Codex runtime binding, which is what
// these tests read back. They drive the real CLI in subprocesses: the
// archive and output handlers exit the process on a refusal, which would
// abort this whole test binary if they ran in it.

const codexFirstTurnThread = "01a0e047-dedd-7981-805a-ef829ec6f478"

// codexLaunchFixture is a Codex session that `agent-deck launch` started in a
// private profile store and tmux server.
type codexLaunchFixture struct {
	home, tmuxDir, id, gate, ready string
	env                            []string
}

// launchFakeCodex launches a fake Codex that holds codexFirstTurnThread's
// writer lock and rollout open, like a real Codex after its launch turn.
// Launch starts it through the runtime authority, so the captured stop,
// archive and kill can prove they own the pane, and after it takes the thread
// its process tree stays fixed, as those stops require. With gated set, the
// fake waits on a FIFO (see openGate) before it takes the thread, so launch's
// identity wait ends first and the row is left the way #2396 and #2400 found
// it: without a Codex identity.
func launchFakeCodex(t *testing.T, gated bool) *codexLaunchFixture {
	t.Helper()
	f := &codexLaunchFixture{home: t.TempDir(), tmuxDir: shortTempDir(t, "adcx")}
	t.Cleanup(func() { testutil.KillTmuxServersUnder(f.tmuxDir) })
	codexHome := filepath.Join(f.home, "codex")
	project := filepath.Join(f.home, "project")
	rollout := filepath.Join(codexHome, "sessions", "2026", "09", "27", "rollout-2026-09-27T00-33-21-"+codexFirstTurnThread+".jsonl")
	for _, dir := range []string{project, filepath.Join(codexHome, "thread-writer-locks"), filepath.Dir(rollout), filepath.Join(f.home, "bin")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	records := `{"timestamp":"2026-09-27T00:33:21Z","type":"session_meta","payload":{"session_id":"` + codexFirstTurnThread + `","id":"` + codexFirstTurnThread + `","cwd":"` + project + `","thread_source":"user"}}` + "\n" +
		`{"timestamp":"2026-09-27T00:33:22Z","type":"event_msg","payload":{"type":"task_started","turn_id":"turn-launch"}}` + "\n" +
		`{"timestamp":"2026-09-27T00:33:30Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-launch","last_agent_message":"FIRST-TURN"}}` + "\n"
	if err := os.WriteFile(rollout, []byte(records), 0o600); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(codexHome, "thread-writer-locks", codexFirstTurnThread+".lock")
	f.ready = filepath.Join(f.home, "codex-ready")
	script := "#!/bin/sh\n"
	if gated {
		f.gate = filepath.Join(f.home, "codex-gate")
		if err := syscall.Mkfifo(f.gate, 0o600); err != nil {
			t.Fatal(err)
		}
		// read is a shell builtin: the tree stays one process while it waits.
		script += "read _ < '" + f.gate + "'\n"
	}
	script += "exec 9>>'" + lock + "' 8<'" + rollout + "'\n: > '" + f.ready + "'\nsleep 2147483647\n"
	fakeCodex := filepath.Join(f.home, "bin", "codex")
	if err := os.WriteFile(fakeCodex, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	f.env = []string{"CODEX_HOME=" + codexHome, "TMUX_TMPDIR=" + f.tmuxDir}
	var launched struct {
		ID   string `json:"session_id"`
		Tool string `json:"tool"`
	}
	if err := json.Unmarshal([]byte(f.run(t, "launch", project, "-t", "codex-first-turn", "-c", fakeCodex, "--no-parent", "--json")), &launched); err != nil {
		t.Fatalf("decode launch output: %v", err)
	}
	if launched.ID == "" || launched.Tool != "codex" {
		t.Fatalf("launch started %+v, want a codex session", launched)
	}
	f.id = launched.ID
	if gated {
		if got := durableCodexBinding(t, f.home, f.id); got != "" {
			t.Fatalf("precondition: Codex binding after launch = %q, want none", got)
		}
	} else {
		f.awaitThread(t)
	}
	return f
}

// run runs the CLI against the fixture's store and tmux server and returns
// its stdout, failing the test when it exits non-zero.
func (f *codexLaunchFixture) run(t *testing.T, args ...string) string {
	t.Helper()
	return f.runEnv(t, nil, args...)
}

func (f *codexLaunchFixture) runEnv(t *testing.T, extraEnv []string, args ...string) string {
	t.Helper()
	stdout, stderr, code := runAgentDeckEnv(t, f.home, "", append(append([]string(nil), f.env...), extraEnv...), args...)
	if code != 0 {
		t.Fatalf("agent-deck %s: exit %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), code, stdout, stderr)
	}
	return stdout
}

// openGate lets a gated fake Codex take its thread and waits until it has.
func (f *codexLaunchFixture) openGate(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		// Non-blocking, so a fake that is not reading yet fails with ENXIO
		// instead of leaving the test stuck in open.
		gate, err := os.OpenFile(f.gate, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			_, err = gate.WriteString("go\n")
			if closeErr := gate.Close(); err == nil {
				err = closeErr
			}
			if err != nil {
				t.Fatalf("open the fake Codex's gate: %v", err)
			}
			break
		}
		if !errors.Is(err, syscall.ENXIO) || time.Now().After(deadline) {
			t.Fatalf("open the fake Codex's gate: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	f.awaitThread(t)
}

// awaitThread waits until the fake Codex holds its thread open.
func (f *codexLaunchFixture) awaitThread(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(f.ready); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the fake Codex never held its thread open")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// tmuxSessionAlive reports whether the session's pane still exists on the
// fixture's tmux server.
func (f *codexLaunchFixture) tmuxSessionAlive(t *testing.T, name, socket string) bool {
	t.Helper()
	args := []string{"has-session", "-t", "=" + name}
	if socket != "" {
		args = append([]string{"-L", socket}, args...)
	}
	cmd := exec.Command("tmux", args...)
	cmd.Env = append(tmuxEnvForIssue1031(), "TMUX_TMPDIR="+f.tmuxDir)
	return cmd.Run() == nil
}

// durableCodexBinding returns the instance's Codex runtime binding as the
// store holds it, "" when it has none, after checking that a reload projects
// the same identity.
func durableCodexBinding(t *testing.T, home, id string) string {
	t.Helper()
	db, err := statedb.Open(stateDBPath(t, home))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	binding, found, err := db.ReadRuntimeBinding(id, "codex")
	if err != nil {
		t.Fatalf("read Codex runtime binding: %v", err)
	}
	rows, err := db.LoadInstances()
	if err != nil {
		t.Fatalf("reload instances: %v", err)
	}
	for _, row := range rows {
		if row.ID != id {
			continue
		}
		var projected struct {
			CodexSessionID string `json:"codex_session_id"`
		}
		if len(row.ToolData) > 0 {
			if err := json.Unmarshal(row.ToolData, &projected); err != nil {
				t.Fatalf("decode reloaded tool_data: %v", err)
			}
		}
		if reloaded := row.RuntimeBindings["codex"].Value; reloaded != binding.Value || projected.CodexSessionID != binding.Value {
			t.Fatalf("reload projects Codex identity %q (tool_data %q), but the runtime binding row holds %q (found=%v)",
				reloaded, projected.CodexSessionID, binding.Value, found)
		}
		if !found {
			return ""
		}
		return binding.Value
	}
	t.Fatalf("instance %q was not persisted", id)
	return ""
}

func TestIssue2400_ArchiveKeepsLiveCodexIdentity(t *testing.T) {
	f := launchFakeCodex(t, true)
	db, err := statedb.Open(stateDBPath(t, f.home))
	if err != nil {
		t.Fatal(err)
	}
	state, found, err := db.ReadRuntimeState(f.id)
	_ = db.Close()
	if err != nil || !found || state.TmuxSession == "" {
		t.Fatalf("runtime state after launch: %+v found=%v err=%v", state, found, err)
	}
	f.openGate(t)

	f.run(t, "session", "archive", f.id, "--json")

	if f.tmuxSessionAlive(t, state.TmuxSession, state.TmuxSocketName) {
		t.Fatal("archive did not stop the session")
	}
	if got := durableCodexBinding(t, f.home, f.id); got != codexFirstTurnThread {
		t.Fatalf("persisted identity after archive = %q, want %q", got, codexFirstTurnThread)
	}
}

func TestIssue2396_FirstTurnOutputIsBoundToItsConversation(t *testing.T) {
	f := launchFakeCodex(t, true)
	f.openGate(t)

	raw := f.run(t, "session", "output", f.id, "--json")
	var got map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &got); err != nil {
		t.Fatalf("decode output %q: %v", raw, err)
	}
	if got["content"] != "FIRST-TURN" || got["conversation_id"] != codexFirstTurnThread ||
		got["codex_turn_generation"] != codexFirstTurnThread+":turn-launch" || got["timestamp"] == "" {
		t.Fatalf("first-turn output is not bound to its conversation: %v", got)
	}
	if persisted := durableCodexBinding(t, f.home, f.id); persisted != codexFirstTurnThread {
		t.Fatalf("persisted identity after output = %q, want %q", persisted, codexFirstTurnThread)
	}
}
