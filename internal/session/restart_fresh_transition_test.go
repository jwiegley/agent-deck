package session

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

type restartFreshFixture struct {
	tool         string
	stubName     string
	stubPreamble string
	bindingKind  string
	bindingID    string
	configure    func(t *testing.T, inst *Instance, stubPath, projectDir string)
}

func exerciseRestartFresh(t *testing.T, fixture restartFreshFixture) (*Instance, string) {
	t.Helper()
	skipIfNoTmuxBinary(t)
	isolatedHomeDir(t)

	previousDB := statedb.GetGlobal()
	statedb.SetGlobal(nil)
	t.Cleanup(func() { statedb.SetGlobal(previousDB) })

	projectDir := t.TempDir()
	argvLog := filepath.Join(t.TempDir(), fixture.tool+"-argv")
	stubPath := filepath.Join(t.TempDir(), fixture.stubName)
	stub := "#!/bin/sh\n" + fixture.stubPreamble +
		fmt.Sprintf("printf '%%s\\n' \"$*\" > %q\nsleep 30\n", argvLog)
	if err := os.WriteFile(stubPath, []byte(stub), 0o755); err != nil {
		t.Fatalf("write %s stub: %v", fixture.tool, err)
	}

	instanceID := fmt.Sprintf("restart-fresh-%s-instance-%d", fixture.tool, time.Now().UnixNano())
	oldName := fmt.Sprintf("%srestart-fresh-%s-%d", tmux.SessionPrefix, fixture.tool, time.Now().UnixNano())
	if output, err := exec.Command("tmux", "new-session", "-d", "-s", oldName, "-c", projectDir, "sleep 30").CombinedOutput(); err != nil {
		t.Fatalf("start predecessor tmux session: %v (%s)", err, strings.TrimSpace(string(output)))
	}
	oldSession := tmux.ReconnectSessionLazy(oldName, instanceID, projectDir, fixture.tool, "waiting")
	t.Cleanup(func() { _ = oldSession.Kill() })

	inst := NewInstanceWithTool("restart-fresh-"+fixture.tool, projectDir, fixture.tool)
	inst.ID = instanceID
	inst.Command = stubPath
	inst.tmuxSession = oldSession
	inst.TmuxSocketName = oldSession.SocketName
	inst.Status = StatusWaiting
	if fixture.configure != nil {
		fixture.configure(t, inst, stubPath, projectDir)
	}
	if err := stampRuntimeCandidate(oldSession, inst.runtimeStateSnapshot(), fixture.bindingKind, fixture.bindingID); err != nil {
		t.Fatalf("stamp predecessor runtime: %v", err)
	}

	if err := inst.RestartFresh(); err != nil {
		t.Fatalf("RestartFresh: %v", err)
	}
	newSession := inst.GetTmuxSession()
	if newSession == nil {
		t.Fatal("RestartFresh left no replacement tmux session")
	}
	t.Cleanup(func() { _ = newSession.Kill() })
	if newSession.Name == oldName {
		t.Fatalf("RestartFresh respawned predecessor %q instead of recreating it", oldName)
	}
	requireTmuxReplacement(t, oldSession, newSession)

	deadline := time.Now().Add(5 * time.Second)
	var argv []byte
	var err error
	for time.Now().Before(deadline) {
		argv, err = os.ReadFile(argvLog)
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("fresh %s stub did not record argv: %v", fixture.tool, err)
	}
	return inst, strings.TrimSpace(string(argv))
}

func requireTmuxReplacement(t *testing.T, predecessor, replacement *tmux.Session) {
	t.Helper()
	if predecessor.SocketName != replacement.SocketName {
		t.Fatalf("RestartFresh changed tmux sockets from %q to %q", predecessor.SocketName, replacement.SocketName)
	}
	args := []string{"-u"}
	if socket := strings.TrimSpace(predecessor.SocketName); socket != "" {
		args = append(args, "-L", socket)
	}
	args = append(args, "list-sessions", "-F", "#{session_name}")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tmux", args...).Output()
	if ctx.Err() != nil {
		t.Fatalf("tmux list-sessions did not complete while checking fresh replacement: %v", ctx.Err())
	}
	if err != nil {
		t.Fatalf("tmux list-sessions could not check fresh replacement: %v", err)
	}
	foundReplacement := false
	for _, name := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		switch strings.TrimSpace(name) {
		case predecessor.Name:
			t.Fatalf("RestartFresh left predecessor %q alive", predecessor.Name)
		case replacement.Name:
			foundReplacement = true
		}
	}
	if !foundReplacement {
		t.Fatalf("RestartFresh replacement %q is absent from the live tmux inventory", replacement.Name)
	}
}

func TestRestartFreshRecreatesWithoutResuming(t *testing.T) {
	const oldID = "old-gemini-session"
	inst, argv := exerciseRestartFresh(t, restartFreshFixture{
		tool:        "gemini",
		stubName:    "gemini",
		bindingKind: "gemini",
		bindingID:   oldID,
		configure: func(t *testing.T, inst *Instance, stubPath, _ string) {
			withConfig(t, &UserConfig{Gemini: GeminiSettings{Command: stubPath}})
			inst.Command = "gemini"
			inst.GeminiSessionID = oldID
			inst.GeminiDetectedAt = time.Now()
		},
	})
	if inst.GeminiSessionID != "" {
		t.Fatalf("RestartFresh retained Gemini binding %q", inst.GeminiSessionID)
	}
	for _, arg := range strings.Fields(argv) {
		if arg == "--resume" || arg == oldID {
			t.Fatalf("RestartFresh resumed the predecessor: argv=%q", argv)
		}
	}
}

func TestRestartFreshCursorDoesNotContinue(t *testing.T) {
	_, argv := exerciseRestartFresh(t, restartFreshFixture{
		tool:     "cursor",
		stubName: "agent",
	})
	if strings.Contains(argv, "--continue") {
		t.Fatalf("RestartFresh continued the predecessor: argv=%q", argv)
	}
}

func TestRestartFreshHermesDoesNotRediscoverSession(t *testing.T) {
	const oldID = "20260720_145826_0e92e7"
	preamble := "if [ \"$1\" = sessions ]; then\n" +
		"  printf 'Title Workspace LastActive ID\\n--- --- --- ---\\nFoo work 1m " + oldID + "\\n'\n" +
		"  exit 0\n" +
		"fi\n"
	inst, argv := exerciseRestartFresh(t, restartFreshFixture{
		tool:         "hermes",
		stubName:     "hermes",
		stubPreamble: preamble,
		configure: func(t *testing.T, inst *Instance, stubPath, _ string) {
			withConfig(t, &UserConfig{Hermes: HermesSettings{Command: stubPath}})
			inst.Command = "hermes"
			inst.HermesSessionID = oldID
		},
	})
	if inst.HermesSessionID != "" {
		t.Fatalf("RestartFresh retained Hermes binding %q", inst.HermesSessionID)
	}
	if strings.Contains(argv, "--resume") || strings.Contains(argv, oldID) {
		t.Fatalf("RestartFresh rediscovered the predecessor: argv=%q", argv)
	}
}

func TestRestartFreshDeepSeekDoesNotRediscoverSession(t *testing.T) {
	const oldID = "session-old"
	inst, argv := exerciseRestartFresh(t, restartFreshFixture{
		tool:     "deepseek",
		stubName: "dsh",
		configure: func(t *testing.T, inst *Instance, stubPath, projectDir string) {
			dshHome := writeDshHome(t, projectDir, []string{oldID}, []string{oldID})
			withConfig(t, &UserConfig{DeepSeek: DeepSeekSettings{
				Command:    stubPath,
				ConfigDir:  dshHome,
				Profile:    "web",
				ResumeFlag: "--resume",
			}})
			inst.Command = "deepseek"
			inst.DeepSeekSessionID = oldID
		},
	})
	if inst.DeepSeekSessionID != "" {
		t.Fatalf("RestartFresh retained DeepSeek binding %q", inst.DeepSeekSessionID)
	}
	if strings.Contains(argv, "--resume") || strings.Contains(argv, oldID) {
		t.Fatalf("RestartFresh rediscovered the predecessor: argv=%q", argv)
	}
}
