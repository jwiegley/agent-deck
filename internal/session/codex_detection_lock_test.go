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
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

const codexDetectionLockThread = "019c9ffa-c9d6-7be1-9e1c-527080e68952"

// stageCodexDetectionProbe lets one Codex detection attempt run to completion
// without tmux, Docker or Codex, and returns the thread it must find. A fake
// tmux reports every session live, with a `sleep` child as the only pane and
// no peer bindings. Fake ps, pgrep and lsof describe that pane as a lone,
// non-Codex process, so the process probe settles on "absent" without the
// host's tools (the Nix runtime-lifecycle gate has none on PATH, and a probe
// that cannot run fails closed). A fake docker lists the rollout the
// container's Codex holds open. CODEX_HOME holds that rollout, scoped to
// projectPath, for the disk fallback to select.
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
	fakePs := fmt.Sprintf(`#!/bin/sh
case " $* " in
*" -eo pid=,ppid=,comm= "*) echo "%[1]d 1 sleep"; exit 0 ;;
*" -eo pid=,ppid= "*) echo "%[1]d 1"; exit 0 ;;
*" -o args= "*) echo "sleep 60"; exit 0 ;;
esac
exit 1
`, pane.Process.Pid)
	fakePgrep := "#!/bin/sh\nexit 1\n" // no children
	fakeLsof := "#!/bin/sh\nexit 0\n"  // holds no rollout open
	fakeDocker := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' %s\n", shellescape.Quote(rollout))
	for name, script := range map[string]string{
		"tmux": fakeTmux, "docker": fakeDocker, "ps": fakePs, "pgrep": fakePgrep, "lsof": fakeLsof,
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return codexDetectionLockThread
}

// newCodexDetectionLockInstance is a Codex instance at runtime generation 1
// whose tmux session and sandbox container the staged probe answers for.
func newCodexDetectionLockInstance(projectPath string, sandboxed bool, socket string) *Instance {
	inst := &Instance{
		ID: "codex-detection-lock", Title: "codex detection lock", ProjectPath: projectPath,
		GroupPath: "work", Tool: "codex", Command: "codex", Status: StatusStarting,
	}
	if sandboxed {
		inst.Sandbox = &SandboxConfig{Enabled: true}
		inst.SandboxContainer = "agentdeck-codex-lock"
	}
	inst.adoptRuntimeState(statedb.RuntimeState{
		InstanceID: inst.ID, Generation: 1, TmuxSession: "agentdeck_codex_lock",
		TmuxSocketName: socket, Status: string(StatusStarting),
	})
	return inst
}

// adoptNextCodexLockRuntime adopts the next runtime generation under i.mu, as
// a runtime commit or a reload of a newer one does, in a new tmux session when
// tmuxName is set.
func adoptNextCodexLockRuntime(inst *Instance, tmuxName string) {
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

// A detection attempt holds the spawn lock, which orders it after every start,
// restart and stop, but a TUI storage reload merges into the same Instance
// without that lock: it rewrites metadata and adopts a newer runtime, and with
// it a new tmux wrapper, under i.mu alone. So every Instance field one
// detection attempt reads must be read under i.mu. Each case runs one writer
// beside one attempt with nothing else ordering the two, so the race detector
// reports an unlocked read whichever runs first. The commit cases adopt a
// newer runtime as the reload does (adoptRuntimeStateLocked). The attempt
// reads the tmux wrapper and socket (process probe, peer exclusions,
// ownership claim), the sandbox and its container, the Codex home, project
// path and start time (disk fallback) and the tool and bound thread (subagent
// gate).
func TestRuntimeLifecycle_CodexDetectionReadsInstanceUnderLock(t *testing.T) {
	for n, c := range []struct {
		name      string
		sandboxed bool
		// write runs beside the attempt; reloaded is the row it merges.
		write func(inst, reloaded *Instance)
	}{
		{name: "restart commit adopts the runtime",
			write: func(inst, _ *Instance) { adoptNextCodexLockRuntime(inst, "") }},
		{name: "restart commit replaces the wrapper",
			write: func(inst, _ *Instance) { adoptNextCodexLockRuntime(inst, "agentdeck_codex_lock_next") }},
		{name: "metadata reload",
			write: func(inst, reloaded *Instance) { inst.MergeReloaded(reloaded) }},
		{name: "sandboxed restart commit replaces the wrapper", sandboxed: true,
			write: func(inst, _ *Instance) { adoptNextCodexLockRuntime(inst, "agentdeck_codex_lock_next") }},
		{name: "sandboxed metadata reload", sandboxed: true,
			write: func(inst, reloaded *Instance) { inst.MergeReloaded(reloaded) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			projectPath := t.TempDir()
			want := stageCodexDetectionProbe(t, projectPath)
			socket := fmt.Sprintf("codex-lock-%d", n)
			inst := newCodexDetectionLockInstance(projectPath, c.sandboxed, socket)
			reloaded := newCodexDetectionLockInstance(projectPath, c.sandboxed, socket)

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

// The #2394 live-thread evidence decides from the Instance whether the pane's
// rollout is local and which Codex home holds its writer lock: the tool, SSH
// host, sandbox and command. Detection attempts and status passes read that
// evidence beside a TUI storage reload, which rewrites those fields under i.mu
// without the spawn lock, so LiveCodexThreadID reads them under i.mu. Here the
// reload runs while the probe lists the pane's Codex processes, and only a
// directory the probe polls for orders the two, so the race detector reports
// an unlocked read that follows the reload.
func TestRuntimeLifecycle_CodexLiveThreadEvidenceReadsInstanceUnderLock(t *testing.T) {
	projectPath := t.TempDir()
	want := stageCodexDetectionProbe(t, projectPath)
	rollout := filepath.Join(os.Getenv("CODEX_HOME"), "sessions", "2026", "09", "25",
		"rollout-2026-09-25T00-00-00-"+want+".jsonl")
	inst := newCodexDetectionLockInstance(projectPath, false, "codex-live-thread")
	reloaded := newCodexDetectionLockInstance(projectPath, false, "codex-live-thread")
	stubCodexPaneOpenPaths(t, []string{rollout}, nil)

	merged := filepath.Join(t.TempDir(), "merged")
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	restore := codexPaneProcessPIDs
	t.Cleanup(func() { codexPaneProcessPIDs = restore })
	codexPaneProcessPIDs = func(*Instance) ([]int, error) {
		go func() {
			inst.MergeReloaded(reloaded)
			_ = os.Mkdir(merged, 0o700)
			<-done // stay alive (see runUnorderedAndAlive)
		}()
		deadline := time.Now().Add(30 * time.Second)
		for {
			if _, err := os.Stat(merged); err == nil {
				return []int{4242}, nil
			}
			if time.Now().After(deadline) {
				t.Fatal("the storage reload never finished")
			}
			time.Sleep(time.Millisecond)
		}
	}

	if got, live := inst.liveCodexBootstrapEvidence(); !live || got != want {
		t.Fatalf("live evidence = (%q, %v), want the thread the pane holds open (%q, true)", got, live, want)
	}
}

// installCodexDetectionSpawnLock stands held in for inst's spawn lock. Every
// acquisition for inst reports its number on entered, then runs before (when
// set) with that number, then takes held. Other instances keep the real lock.
func installCodexDetectionSpawnLock(t *testing.T, inst *Instance, held *sync.Mutex, before func(int)) <-chan int {
	t.Helper()
	entered := make(chan int, 8)
	calls := 0
	oldAcquire := instanceSpawnLockAcquireFn
	instanceSpawnLockAcquireFn = func(id string) (func(), error) {
		if id != inst.ID {
			return oldAcquire(id)
		}
		calls++
		entered <- calls
		if before != nil {
			before(calls)
		}
		held.Lock()
		var once sync.Once
		return func() { once.Do(held.Unlock) }, nil
	}
	t.Cleanup(func() { instanceSpawnLockAcquireFn = oldAcquire })
	return entered
}

// awaitCodexDetection returns once the detection goroutine has asked for the
// spawn lock for attempt want, and fails the test if detection ends first: an
// attempt that probes without the lock does not ask for it before it has read
// the Instance.
func awaitCodexDetection(t *testing.T, entered <-chan int, detected <-chan struct{}, want int) {
	t.Helper()
	select {
	case n := <-entered:
		if n != want {
			t.Fatalf("detection asked for the spawn lock %d times, want %d", n, want)
		}
	case <-detected:
		t.Fatalf("detection ended without asking for the spawn lock for attempt %d", want)
	case <-time.After(30 * time.Second):
		t.Fatalf("detection never asked for the spawn lock for attempt %d", want)
	}
}

// A start or restart launches detectCodexSessionAsync, which keeps retrying
// after the transition returns, and a stop or a fallback restart can begin in
// the meantime. Such a transition holds the spawn lock while it writes fields
// an attempt reads, without i.mu: restartWithTransition's fallback recreate
// replaces the tmux wrapper (recreateTmuxSession), and the relaunch stamps
// CodexStartedAt and the sandbox container. An attempt therefore takes the
// spawn lock before it reads the Instance. Here those writes run beside the
// detection goroutine while the lock is held, and nothing but that lock orders
// the two, so the race detector reports an attempt that probes first. Once the
// transition releases the lock, the attempt finds the staged thread.
func TestRuntimeLifecycle_CodexDetectionWaitsForTheTransitionHoldingTheSpawnLock(t *testing.T) {
	for n, sandboxed := range []bool{false, true} {
		t.Run(fmt.Sprintf("sandboxed=%v", sandboxed), func(t *testing.T) {
			projectPath := t.TempDir()
			want := stageCodexDetectionProbe(t, projectPath)
			socket := fmt.Sprintf("codex-wait-%d", n)
			inst := newCodexDetectionLockInstance(projectPath, sandboxed, socket)
			oldDelays := codexDetectionDelays
			codexDetectionDelays = []time.Duration{0}
			t.Cleanup(func() { codexDetectionDelays = oldDelays })
			var held sync.Mutex
			entered := installCodexDetectionSpawnLock(t, inst, &held, nil)

			held.Lock() // the transition's spawn lock
			replacement := tmux.NewSession(inst.Title, projectPath)
			replacement.SocketName, replacement.InstanceID = socket, inst.ID
			// An hour back, so the staged rollout still post-dates the start.
			startedAt := time.Now().Add(-time.Hour).UnixMilli()
			written, detected := make(chan struct{}), make(chan struct{})
			go func() {
				inst.tmuxSession = replacement
				inst.CodexStartedAt = startedAt
				inst.SandboxContainer = "agentdeck-codex-lock-next"
				close(written)
				// Stay alive while detection runs: the race detector can lose
				// an access made by a goroutine that has exited once a new
				// goroutine reuses its slot.
				<-detected
			}()
			go func() {
				defer close(detected)
				inst.detectCodexSessionAsync()
			}()
			<-written
			awaitCodexDetection(t, entered, detected, 1)
			held.Unlock()
			<-detected

			if got, _ := inst.currentRuntimeBinding("codex"); got != want {
				t.Fatalf("detection bound %q after the transition, want the staged thread %q", got, want)
			}
		})
	}
}

// Detection serves the runtime its launching transition committed. After a
// later transition has replaced that runtime, the next attempt stops without
// probing: the replacement's own start or restart detects for it. The thread
// staged for the replacement stays unbound.
func TestRuntimeLifecycle_CodexDetectionStopsOnceItsRuntimeIsReplaced(t *testing.T) {
	projectPath := t.TempDir()
	stageCodexDetectionProbe(t, projectPath)
	inst := newCodexDetectionLockInstance(projectPath, false, "codex-replaced")
	rollout := filepath.Join(os.Getenv("CODEX_HOME"), "sessions", "2026", "09", "25",
		"rollout-2026-09-25T00-00-00-"+codexDetectionLockThread+".jsonl")
	hidden := rollout + ".hidden"
	if err := os.Rename(rollout, hidden); err != nil {
		t.Fatal(err)
	}
	oldDelays := codexDetectionDelays
	codexDetectionDelays = []time.Duration{0, 0}
	t.Cleanup(func() { codexDetectionDelays = oldDelays })
	var held sync.Mutex
	replaced := make(chan struct{})
	entered := installCodexDetectionSpawnLock(t, inst, &held, func(n int) {
		if n == 2 {
			<-replaced
		}
	})

	detected := make(chan struct{})
	go func() {
		defer close(detected)
		inst.detectCodexSessionAsync()
	}()
	awaitCodexDetection(t, entered, detected, 1)
	// The first attempt found nothing. The second waits while a restart
	// commits the replacement, whose thread is then on disk.
	awaitCodexDetection(t, entered, detected, 2)
	adoptNextCodexLockRuntime(inst, "agentdeck_codex_replaced_next")
	if err := os.Rename(hidden, rollout); err != nil {
		t.Fatal(err)
	}
	close(replaced)
	<-detected

	if got, _ := inst.currentRuntimeBinding("codex"); got != "" {
		t.Fatalf("detection of the replaced runtime bound %q to its replacement, want nothing bound", got)
	}
}

// runUnorderedAndAlive runs each function in its own goroutine, ordered with
// the other by nothing, and keeps both goroutines alive until both functions
// have returned: the race detector can lose an access made by a goroutine
// that has exited once a new goroutine reuses its slot.
func runUnorderedAndAlive(fns ...func()) {
	var worked, exited sync.WaitGroup
	finished := make(chan struct{})
	worked.Add(len(fns))
	exited.Add(len(fns))
	for _, fn := range fns {
		go func() {
			defer exited.Done()
			fn()
			worked.Done()
			<-finished
		}()
	}
	worked.Wait()
	close(finished)
	exited.Wait()
}

// Every status pass observes the active binding under i.mu without the spawn
// lock (updateStatusWithEvidence → captureActiveRuntimeBindingObservation), so
// the TUI's status worker observes it while a restart the operator started
// sets or clears it under the spawn lock alone. Those writes therefore take
// i.mu. Each case runs one restart write beside one observation, ordered by
// nothing else, so the race detector reports an unlocked write whichever runs
// first. The fresh clear runs on a Claude instance because its Codex branch
// took i.mu for the pending warning right after clearing the thread, which
// orders an observation that comes later and would hide the race.
func TestRuntimeLifecycle_RestartWritesTheObservedBindingUnderLock(t *testing.T) {
	const stale = "019c9ffa-c9d6-7be1-9e1c-000000000000"
	for n, c := range []struct {
		name, tool, bound, kind, want string
		write                         func(*Instance)
	}{
		{name: "restart adopts the Codex thread", tool: "codex", kind: "codex",
			want:  codexDetectionLockThread,
			write: func(inst *Instance) { inst.adoptCodexSessionForRestart() }},
		{name: "relaunch drops a Codex thread without a rollout", tool: "codex", bound: stale, kind: "codex",
			write: func(inst *Instance) { inst.buildCodexCommand(inst.Command) }},
		{name: "fresh restart clears the binding", tool: "claude", bound: stale, kind: "claude",
			write: func(inst *Instance) { inst.clearSessionBindingForFreshStart() }},
	} {
		t.Run(c.name, func(t *testing.T) {
			projectPath := t.TempDir()
			stageCodexDetectionProbe(t, projectPath)
			inst := newCodexDetectionLockInstance(projectPath, false, fmt.Sprintf("restart-binding-%d", n))
			inst.Tool, inst.Command = c.tool, c.tool
			if c.bound != "" {
				inst.CodexSessionID, inst.ClaudeSessionID = c.bound, c.bound
			}

			runUnorderedAndAlive(
				func() { c.write(inst) },
				func() { inst.captureActiveRuntimeBindingObservation() })
			if got, _ := inst.currentRuntimeBinding(c.kind); got != c.want {
				t.Fatalf("%s binding after the restart write = %q, want %q", c.kind, got, c.want)
			}
		})
	}
}

// The status pass's Codex refresh holds the spawn lock but not i.mu, while a
// TUI storage reload rebinds the thread under i.mu without the spawn lock. The
// probe cadence asks whether a thread is bound, so it reads the binding under
// i.mu. Here the reload runs once the pass has taken its first snapshot and is
// waiting on tmux: a fake tmux leaves a marker, which the test polls for
// without synchronizing with the pass. Nothing else orders the reload's write
// and the cadence's read, so the race detector reports an unlocked read
// whichever runs first. A recent probe keeps the pass from probing, so the
// cadence check is its last read of the Instance.
func TestRuntimeLifecycle_CodexStatusPassReadsTheBindingUnderLock(t *testing.T) {
	projectPath := t.TempDir()
	marker := filepath.Join(t.TempDir(), "show-environment")
	bin := t.TempDir()
	fakeTmux := fmt.Sprintf("#!/bin/sh\ncase \" $* \" in\n*\" show-environment \"*) : > %s ;;\nesac\nexit 1\n",
		shellescape.Quote(marker))
	if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte(fakeTmux), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	inst := newCodexDetectionLockInstance(projectPath, false, "codex-status-pass")
	reloaded := newCodexDetectionLockInstance(projectPath, false, "codex-status-pass")
	inst.CodexSessionID = codexDetectionLockThread
	inst.lastCodexProbeAt = time.Now()

	passed, merged := make(chan struct{}), make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		inst.queryCodexSessionCandidateForPass(nil, false, codexDetectionLockThread, nil, nil)
		close(passed)
		<-merged // stay alive (see runUnorderedAndAlive)
	}()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the status pass never asked tmux for CODEX_SESSION_ID")
		}
		time.Sleep(time.Millisecond)
	}
	go func() {
		defer wg.Done()
		inst.MergeReloaded(reloaded)
		close(merged)
		<-passed
	}()
	wg.Wait()
	if got, _ := inst.currentRuntimeBinding("codex"); got != "" {
		t.Fatalf("codex binding after the reload = %q, want the reloaded row's empty binding", got)
	}
}
