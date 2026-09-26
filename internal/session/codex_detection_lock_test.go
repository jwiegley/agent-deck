package session

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"al.essio.dev/pkg/shellescape"
	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

const codexDetectionLockThread = "019c9ffa-c9d6-7be1-9e1c-527080e68952"

// stageCodexDetectionProbe lets one Codex detection attempt run to completion
// without tmux, Docker or Codex, and returns the thread it must find. A fake
// tmux reports every session live, with a `sleep` child as the only pane and
// no peer bindings. A fake docker lists the rollout the container's Codex
// holds open. CODEX_HOME holds that rollout, scoped to projectPath, for the
// disk fallback to select.
func stageCodexDetectionProbe(t *testing.T, projectPath string) string {
	t.Helper()
	pane := exec.Command("sleep", "60")
	if err := pane.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = pane.Process.Kill()
		_ = pane.Wait()
	})

	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	dir := filepath.Join(codexHome, "sessions", "2026", "09", "25")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	rollout := filepath.Join(dir, "rollout-2026-09-25T00-00-00-"+codexDetectionLockThread+".jsonl")
	meta := fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"cwd":%q,"thread_source":"user"}}`+"\n",
		codexDetectionLockThread, projectPath)
	if err := os.WriteFile(rollout, []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}

	bin := t.TempDir()
	fakeTmux := fmt.Sprintf(`#!/bin/sh
case " $* " in
*" has-session "*) exit 0 ;;
*" list-panes "*) echo %d; exit 0 ;;
*" list-sessions "*) exit 0 ;;
esac
exit 1
`, pane.Process.Pid)
	fakeDocker := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' %s\n", shellescape.Quote(rollout))
	for name, script := range map[string]string{"tmux": fakeTmux, "docker": fakeDocker} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return codexDetectionLockThread
}

// A start or restart launches detectCodexSessionAsync before the runtime
// commit that adopts its runtime: restartWithTransition runs
// commitPhysicalRuntime deferred, after the goroutine is started. A TUI
// storage reload can also merge into the same Instance while the detection
// retries. Both write under i.mu, so every Instance field one detection
// attempt reads must be read under i.mu as well. Each case runs one writer
// beside one attempt with nothing else ordering the two, so the race detector
// reports an unlocked read whichever runs first. The attempt reads the tmux
// wrapper and socket (process probe, peer exclusions, ownership claim), the
// sandbox and its container, the Codex home, project path and start time
// (disk fallback) and the tool and bound thread (subagent gate).
func TestRuntimeLifecycle_CodexDetectionReadsInstanceUnderLock(t *testing.T) {
	commit := func(tmuxName string) func(*Instance) {
		return func(inst *Instance) {
			next := inst.RuntimeState()
			next.Generation++
			next.StatusRevision = 0
			next.Status = string(StatusWaiting)
			next.LastStartedAt = time.Now().UTC()
			if tmuxName != "" {
				next.TmuxSession = tmuxName
			}
			inst.adoptRuntimeState(next)
		}
	}
	for n, c := range []struct {
		name      string
		sandboxed bool
		// write runs beside the attempt; reloaded is the row it merges.
		write func(inst, reloaded *Instance)
	}{
		{name: "restart commit adopts the runtime",
			write: func(inst, _ *Instance) { commit("")(inst) }},
		{name: "restart commit replaces the wrapper",
			write: func(inst, _ *Instance) { commit("agentdeck_codex_lock_next")(inst) }},
		{name: "metadata reload",
			write: func(inst, reloaded *Instance) { inst.MergeReloaded(reloaded) }},
		{name: "sandboxed restart commit replaces the wrapper", sandboxed: true,
			write: func(inst, _ *Instance) { commit("agentdeck_codex_lock_next")(inst) }},
		{name: "sandboxed metadata reload", sandboxed: true,
			write: func(inst, reloaded *Instance) { inst.MergeReloaded(reloaded) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			projectPath := t.TempDir()
			want := stageCodexDetectionProbe(t, projectPath)
			build := func() *Instance {
				inst := &Instance{
					ID: "codex-detection-lock", Title: "codex detection lock", ProjectPath: projectPath,
					GroupPath: "work", Tool: "codex", Command: "codex", Status: StatusStarting,
				}
				if c.sandboxed {
					inst.Sandbox = &SandboxConfig{Enabled: true}
					inst.SandboxContainer = "agentdeck-codex-lock"
				}
				inst.adoptRuntimeState(statedb.RuntimeState{
					InstanceID: inst.ID, Generation: 1, TmuxSession: "agentdeck_codex_lock",
					TmuxSocketName: fmt.Sprintf("codex-lock-%d", n), Status: string(StatusStarting),
				})
				return inst
			}
			inst, reloaded := build(), build()

			var got string
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				c.write(inst, reloaded)
			}()
			go func() {
				defer wg.Done()
				sessionID, _, probeErr := inst.queryCodexSessionFromProcessFiles()
				got = inst.resolveCodexDetectionCandidate(sessionID, probeErr)
			}()
			wg.Wait()
			if got != want {
				t.Fatalf("detection attempt found %q, want the staged thread %q", got, want)
			}
		})
	}
}
