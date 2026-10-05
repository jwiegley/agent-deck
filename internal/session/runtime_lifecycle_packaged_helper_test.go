package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

var packagedRuntimeModuleCache = func() string {
	if cache := os.Getenv("GOMODCACHE"); cache != "" {
		return cache
	}
	goPath := os.Getenv("GOPATH")
	if goPath == "" {
		goPath = filepath.Join(os.Getenv("HOME"), "go")
	}
	if first, _, found := strings.Cut(goPath, string(os.PathListSeparator)); found {
		goPath = first
	}
	return filepath.Join(goPath, "pkg", "mod")
}()

func buildPackagedRuntimeCrashHelper(t *testing.T) string {
	t.Helper()
	root := packagedRuntimeRepoRoot(t)
	binary := filepath.Join(t.TempDir(), "runtime-lifecycle-crash-helper")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, packagedRuntimeGo(t), "build",
		"-tags", "runtime_lifecycle_helper", "-o", binary,
		"./cmd/runtime-lifecycle-crash-helper")
	cmd.Dir = root
	cmd.Env = packagedRuntimeBuildEnv(filepath.Join(t.TempDir(), "go-build"))
	cmd.WaitDelay = 5 * time.Second
	configurePackagedCrashProcessGroup(cmd)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build packaged runtime crash helper: %v\n%s", err, output)
	}
	return binary
}

func configurePackagedCrashProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}
}

func killPackagedCrashProcessGroup(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

func packagedRuntimeBuildEnv(goCache string) []string {
	drop := map[string]bool{
		"GOCACHE": true, "GOMODCACHE": true, "GOPROXY": true, "GOTOOLCHAIN": true,
		"GOENV": true,
	}
	env := make([]string, 0, len(os.Environ())+4)
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if !drop[key] {
			env = append(env, item)
		}
	}
	return append(env,
		"GOCACHE="+goCache,
		"GOMODCACHE="+packagedRuntimeModuleCache,
		"GOPROXY=off",
		"GOTOOLCHAIN=local",
		"GOENV=off",
	)
}

func packagedRuntimeRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

func packagedRuntimeGo(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("go")
	if err != nil {
		t.Fatal("go executable not found")
	}
	return path
}

func packagedRuntimeCrashEnv(root string) []string {
	drop := map[string]bool{
		"HOME": true, "XDG_CONFIG_HOME": true, "XDG_DATA_HOME": true,
		"XDG_CACHE_HOME": true, "XDG_STATE_HOME": true, "TMPDIR": true,
		"TMUX": true, "TMUX_PANE": true, "TMUX_TMPDIR": true, "PATH": true,
	}
	env := make([]string, 0, len(os.Environ())+6)
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if !drop[key] {
			env = append(env, item)
		}
	}
	return append(env,
		"HOME="+filepath.Join(root, "home"),
		"XDG_CONFIG_HOME="+filepath.Join(root, "config"),
		"XDG_DATA_HOME="+filepath.Join(root, "data"),
		"XDG_CACHE_HOME="+filepath.Join(root, "cache"),
		"XDG_STATE_HOME="+filepath.Join(root, "state"),
		"TMPDIR="+filepath.Join(root, "tmp"),
		"TMUX_TMPDIR="+filepath.Join(root, "tmux"),
		"PATH="+filepath.Join(root, "bin"),
	)
}

func runPackagedRuntimeCrash(
	t *testing.T, helper string, stage RuntimeTransitionStage,
	dbPath, inventoryPath, lockRoot, processRoot string,
) {
	t.Helper()
	for _, dir := range []string{
		filepath.Join(processRoot, "home"), filepath.Join(processRoot, "config"),
		filepath.Join(processRoot, "data"), filepath.Join(processRoot, "cache"),
		filepath.Join(processRoot, "state"), filepath.Join(processRoot, "tmp"),
		filepath.Join(processRoot, "tmux"), filepath.Join(processRoot, "bin"), lockRoot,
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, helper, string(stage), dbPath, inventoryPath, lockRoot)
	cmd.Env = packagedRuntimeCrashEnv(processRoot)
	cmd.WaitDelay = 2 * time.Second
	configurePackagedCrashProcessGroup(cmd)
	outputRead, outputWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = outputWrite, outputWrite
	if err := cmd.Start(); err != nil {
		_ = outputRead.Close()
		_ = outputWrite.Close()
		t.Fatal(err)
	}
	_ = outputWrite.Close()
	type readResult struct {
		data []byte
		err  error
	}
	readDone := make(chan readResult, 1)
	go func() {
		data, readErr := io.ReadAll(outputRead)
		readDone <- readResult{data: data, err: readErr}
	}()
	err = cmd.Wait()
	exitErr, ok := err.(*exec.ExitError)
	var captured readResult
	select {
	case captured = <-readDone:
	case <-time.After(2 * time.Second):
		killPackagedCrashProcessGroup(cmd)
		_ = outputRead.Close()
		<-readDone
		t.Fatalf("packaged crash helper stage %s left a descendant holding its output pipe", stage)
	}
	_ = outputRead.Close()
	killPackagedCrashProcessGroup(cmd)
	if captured.err != nil {
		t.Fatalf("read packaged crash helper output: %v", captured.err)
	}
	if !ok || exitErr.ExitCode() != runtimeCrashExitCode(stage) {
		t.Fatalf("packaged crash helper stage %s exited with %v, want %d\n%s",
			stage, err, runtimeCrashExitCode(stage), captured.data)
	}
	if ctx.Err() != nil {
		t.Fatalf("packaged crash helper stage %s timed out: %v", stage, ctx.Err())
	}
}

type packagedRuntimeContenderResult struct {
	Adopted      bool
	Runtime      statedb.RuntimeState
	LocalRuntime statedb.RuntimeState
	SpawnCalls   int
	StampCalls   int
	CommitCalls  int
	SweepCalls   int
}

type packagedRuntimeContenderProcess struct {
	cmd     *exec.Cmd
	cancel  context.CancelFunc
	stdout  bytes.Buffer
	stderr  bytes.Buffer
	ready   *os.File
	release *os.File
	waited  bool
}

func startPackagedRuntimeContender(
	t *testing.T, helper, dbPath, inventoryPath, lockRoot, sessionName string,
) *packagedRuntimeContenderProcess {
	t.Helper()
	processRoot := t.TempDir()
	for _, dir := range []string{
		filepath.Join(processRoot, "home"), filepath.Join(processRoot, "config"),
		filepath.Join(processRoot, "data"), filepath.Join(processRoot, "cache"),
		filepath.Join(processRoot, "state"), filepath.Join(processRoot, "tmp"),
		filepath.Join(processRoot, "tmux"), filepath.Join(processRoot, "bin"), lockRoot,
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	child := &packagedRuntimeContenderProcess{cancel: cancel}
	child.cmd = exec.CommandContext(ctx, helper, "contend", dbPath, inventoryPath, lockRoot, sessionName)
	child.cmd.Env = packagedRuntimeCrashEnv(processRoot)
	child.cmd.Stdout = &child.stdout
	child.cmd.Stderr = &child.stderr
	child.cmd.WaitDelay = 2 * time.Second
	configurePackagedCrashProcessGroup(child.cmd)

	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	releaseRead, releaseWrite, err := os.Pipe()
	if err != nil {
		cancel()
		_ = readyRead.Close()
		_ = readyWrite.Close()
		t.Fatal(err)
	}
	child.ready = readyRead
	child.release = releaseWrite
	child.cmd.ExtraFiles = []*os.File{readyWrite, releaseRead}
	if err := child.cmd.Start(); err != nil {
		cancel()
		_ = readyRead.Close()
		_ = readyWrite.Close()
		_ = releaseRead.Close()
		_ = releaseWrite.Close()
		t.Fatal(err)
	}
	_ = readyWrite.Close()
	_ = releaseRead.Close()
	t.Cleanup(func() {
		if !child.waited && child.cmd.Process != nil {
			killPackagedCrashProcessGroup(child.cmd)
			_ = child.cmd.Wait()
		}
		child.cancel()
		_ = child.ready.Close()
		if child.release != nil {
			_ = child.release.Close()
		}
	})
	return child
}

func (p *packagedRuntimeContenderProcess) waitReady(t *testing.T) {
	t.Helper()
	var signal [1]byte
	if _, err := io.ReadFull(p.ready, signal[:]); err != nil {
		p.waited = true
		waitErr := p.cmd.Wait()
		p.cancel()
		t.Fatalf("packaged contender did not reach observation barrier: %v (wait=%v)\nstdout: %s\nstderr: %s",
			err, waitErr, p.stdout.String(), p.stderr.String())
	}
}

func (p *packagedRuntimeContenderProcess) releaseBarrier(t *testing.T) {
	t.Helper()
	if _, err := p.release.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	_ = p.release.Close()
	p.release = nil
}

func (p *packagedRuntimeContenderProcess) waitResult(t *testing.T) packagedRuntimeContenderResult {
	t.Helper()
	err := p.cmd.Wait()
	p.waited = true
	killPackagedCrashProcessGroup(p.cmd)
	p.cancel()
	if err != nil {
		t.Fatalf("packaged contender failed: %v\nstdout: %s\nstderr: %s", err, p.stdout.String(), p.stderr.String())
	}
	var result packagedRuntimeContenderResult
	if err := json.Unmarshal(p.stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode packaged contender result: %v\nstdout: %s\nstderr: %s", err, p.stdout.String(), p.stderr.String())
	}
	return result
}

func installPackagedCrashParentSeams(
	t *testing.T, inventoryPath, lockRoot string,
) *runtimeCrashObserver {
	t.Helper()
	observer := installRuntimeCrashParentSeams(t, inventoryPath)
	oldRoot := agentDeckDirOverride
	agentDeckDirOverride = lockRoot
	instanceSpawnLockAcquireFn = defaultAcquireInstanceSpawnLock
	t.Cleanup(func() { agentDeckDirOverride = oldRoot })
	return observer
}

func requirePackagedCrashLockAvailable(t *testing.T, lockRoot string) {
	t.Helper()
	path := filepath.Join(lockRoot, "locks", "instance-spawn-one.lock")
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("crashed helper did not leave transition lock %s: %v", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		t.Fatalf("crashed helper still owns transition lock %s: %v", path, err)
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}

func packagedCrashState() statedb.RuntimeState {
	return statedb.RuntimeState{
		InstanceID: "one", Generation: 1, StatusRevision: 0,
		TmuxSession: "runtime-g1", TmuxSocketName: "isolated",
		Status: "starting", LastStartedAt: time.Unix(100, 123).UTC(),
	}
}

func requirePackagedCrashCandidate(t *testing.T, candidate tmux.RuntimeCandidate) {
	t.Helper()
	want := packagedCrashState()
	if candidate.SessionName != want.TmuxSession || candidate.SocketName != want.TmuxSocketName ||
		candidate.InstanceID != want.InstanceID || candidate.Generation != want.Generation || !candidate.GenerationKnown ||
		candidate.StatusRevision != want.StatusRevision || candidate.Status != want.Status || !candidate.StateKnown ||
		candidate.LastStartedUnixNano != want.LastStartedAt.UnixNano() ||
		candidate.BindingKind != "claude" || candidate.BindingValue != "" || !candidate.BindingKnown ||
		candidate.PanePID <= 0 || candidate.ProofError != "" {
		t.Fatalf("packaged crash candidate=%#v, want complete tuple %#v with explicit empty claude binding", candidate, want)
	}
}

func requireNoPackagedCrashBinding(t *testing.T, db *statedb.StateDB) {
	t.Helper()
	if binding, found, err := db.ReadRuntimeBinding("one", "claude"); err != nil || found {
		t.Fatalf("explicit empty binding produced row=%#v found=%v err=%v", binding, found, err)
	}
}

func TestRuntimeLifecycle_PackagedCrashHelper(t *testing.T) {
	helper := buildPackagedRuntimeCrashHelper(t)
	parentIsolation := t.TempDir()
	for _, dir := range []string{"bin", "tmux"} {
		if err := os.MkdirAll(filepath.Join(parentIsolation, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// An empty private PATH turns any missed tmux seam into a hard test failure.
	// The production file-lock path remains exercised under a private root.
	t.Setenv("PATH", filepath.Join(parentIsolation, "bin"))
	t.Setenv("TMUX_TMPDIR", filepath.Join(parentIsolation, "tmux"))
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")

	t.Run("separate process contenders spawn exactly one runtime", func(t *testing.T) {
		root := t.TempDir()
		dbPath, db := seedRuntimeCrashDB(t)
		inventoryPath := filepath.Join(root, "candidates.json")
		lockRoot := filepath.Join(root, "agent-deck")
		first := startPackagedRuntimeContender(t, helper, dbPath, inventoryPath, lockRoot, "runtime-first")
		second := startPackagedRuntimeContender(t, helper, dbPath, inventoryPath, lockRoot, "runtime-second")
		first.waitReady(t)
		second.waitReady(t)
		first.releaseBarrier(t)
		second.releaseBarrier(t)
		results := []packagedRuntimeContenderResult{first.waitResult(t), second.waitResult(t)}

		var winner, loser *packagedRuntimeContenderResult
		for index := range results {
			result := &results[index]
			if result.Adopted {
				if loser != nil {
					t.Fatalf("multiple lock losers: %#v", results)
				}
				loser = result
			} else {
				if winner != nil {
					t.Fatalf("multiple lock winners: %#v", results)
				}
				winner = result
			}
		}
		if winner == nil || loser == nil {
			t.Fatalf("packaged contender roles = %#v", results)
		}
		if winner.SpawnCalls != 1 || winner.StampCalls != 1 || winner.CommitCalls != 1 || winner.SweepCalls != 1 {
			t.Fatalf("packaged lock winner counters = %#v", *winner)
		}
		if loser.SpawnCalls != 0 || loser.StampCalls != 0 || loser.CommitCalls != 0 || loser.SweepCalls != 0 {
			t.Fatalf("packaged lock loser performed work = %#v", *loser)
		}
		if winner.Runtime != loser.Runtime || winner.LocalRuntime != winner.Runtime || loser.LocalRuntime != winner.Runtime ||
			winner.Runtime.Generation != 1 || winner.Runtime.StatusRevision != 0 ||
			winner.Runtime.TmuxSocketName != "isolated" || winner.Runtime.Status != string(StatusStarting) ||
			(winner.Runtime.TmuxSession != "runtime-first" && winner.Runtime.TmuxSession != "runtime-second") {
			t.Fatalf("packaged contender runtime mismatch: winner=%#v loser=%#v", *winner, *loser)
		}
		durable, found, err := db.ReadRuntimeState("one")
		if err != nil || !found || durable != winner.Runtime {
			t.Fatalf("packaged contender durable runtime=%#v found=%v err=%v want=%#v", durable, found, err, winner.Runtime)
		}
		candidates, err := readRuntimeCrashCandidates(inventoryPath)
		if err != nil || len(candidates) != 1 {
			t.Fatalf("packaged contender inventory=%#v err=%v", candidates, err)
		}
		candidate := candidates[0]
		if candidate.InstanceID != "one" || candidate.SessionName != winner.Runtime.TmuxSession ||
			candidate.SocketName != "isolated" || candidate.Generation != 1 || !candidate.GenerationKnown ||
			candidate.StatusRevision != winner.Runtime.StatusRevision || candidate.Status != winner.Runtime.Status ||
			candidate.LastStartedUnixNano != winner.Runtime.LastStartedAt.UnixNano() || !candidate.StateKnown ||
			candidate.BindingKind != "claude" || candidate.BindingValue != "" || !candidate.BindingKnown ||
			candidate.PanePID <= 0 || candidate.ProofError != "" {
			t.Fatalf("packaged contender candidate=%#v want runtime=%#v", candidate, winner.Runtime)
		}
		requireNoPackagedCrashBinding(t, db)
		requirePackagedCrashLockAvailable(t, lockRoot)
	})

	t.Run("destruction reservation crash releases lock and recovers live runtime", func(t *testing.T) {
		root := t.TempDir()
		dbPath, db := seedRuntimeCrashDB(t)
		inventoryPath := filepath.Join(root, "candidates.json")
		lockRoot := filepath.Join(root, "agent-deck")
		runPackagedRuntimeCrash(t, helper, RuntimeDestructionAfterReserveBeforeTerminate,
			dbPath, inventoryPath, lockRoot, filepath.Join(root, "process"))
		requirePackagedCrashLockAvailable(t, lockRoot)
		reserved, found, err := db.ReadRuntimeState("one")
		if err != nil || !found || !statedb.IsRuntimeDestructionReserved(reserved) {
			t.Fatalf("reserved state=%#v found=%v err=%v", reserved, found, err)
		}
		candidates, err := readRuntimeCrashCandidates(inventoryPath)
		if err != nil || len(candidates) != 1 || candidates[0].SessionName != reserved.TmuxSession {
			t.Fatalf("live destruction inventory=%#v err=%v", candidates, err)
		}
		installPackagedCrashParentSeams(t, inventoryPath, lockRoot)
		result, err := runtimeCrashInstance(t, db).ReconcileRuntime()
		if err != nil {
			t.Fatal(err)
		}
		if !result.Live || result.Adopted || result.State.Status != string(StatusIdle) ||
			result.State.StatusRevision != reserved.StatusRevision+1 {
			t.Fatalf("live reservation recovery result=%#v", result)
		}
		if row, err := db.LoadInstanceByID("one"); err != nil || row == nil || row.Status != string(StatusIdle) {
			t.Fatalf("live recovery removed row: row=%#v err=%v", row, err)
		}
		requirePackagedCrashLockAvailable(t, lockRoot)
	})

	t.Run("destruction termination crash releases lock and recovers stopped runtime", func(t *testing.T) {
		root := t.TempDir()
		dbPath, db := seedRuntimeCrashDB(t)
		inventoryPath := filepath.Join(root, "candidates.json")
		lockRoot := filepath.Join(root, "agent-deck")
		runPackagedRuntimeCrash(t, helper, RuntimeDestructionAfterTerminateBeforeComplete,
			dbPath, inventoryPath, lockRoot, filepath.Join(root, "process"))
		requirePackagedCrashLockAvailable(t, lockRoot)
		reserved, found, err := db.ReadRuntimeState("one")
		if err != nil || !found || !statedb.IsRuntimeDestructionReserved(reserved) {
			t.Fatalf("reserved state=%#v found=%v err=%v", reserved, found, err)
		}
		candidates, err := readRuntimeCrashCandidates(inventoryPath)
		if err != nil || len(candidates) != 0 {
			t.Fatalf("terminated destruction inventory=%#v err=%v", candidates, err)
		}
		installPackagedCrashParentSeams(t, inventoryPath, lockRoot)
		result, err := runtimeCrashInstance(t, db).ReconcileRuntime()
		if err != nil {
			t.Fatal(err)
		}
		if result.Live || result.Adopted || result.State.Status != string(StatusStopped) ||
			result.State.StatusRevision != reserved.StatusRevision+1 {
			t.Fatalf("stopped reservation recovery result=%#v", result)
		}
		if row, err := db.LoadInstanceByID("one"); err != nil || row == nil {
			t.Fatalf("stopped recovery removed row: row=%#v err=%v", row, err)
		}
		requirePackagedCrashLockAvailable(t, lockRoot)
	})

	t.Run("mid-respawn candidate remains incomplete and preserved", func(t *testing.T) {
		root := t.TempDir()
		dbPath, db := seedRuntimeCrashDB(t)
		seeded, found, err := db.ReadRuntimeState("one")
		if err != nil || !found {
			t.Fatalf("seeded state=%#v found=%v err=%v", seeded, found, err)
		}
		inventoryPath := filepath.Join(root, "candidates.json")
		lockRoot := filepath.Join(root, "agent-deck")
		runPackagedRuntimeCrash(t, helper, RuntimeTransitionAfterRespawnBeforeStamp,
			dbPath, inventoryPath, lockRoot, filepath.Join(root, "process"))
		requirePackagedCrashLockAvailable(t, lockRoot)
		before, found, err := db.ReadRuntimeState("one")
		if err != nil || !found || before != seeded {
			t.Fatalf("mid-respawn durable state=%#v found=%v err=%v", before, found, err)
		}
		incomplete, inventoryErr := readRuntimeCrashCandidates(inventoryPath)
		if inventoryErr != nil || len(incomplete) != 1 || incomplete[0].GenerationKnown ||
			incomplete[0].ProofError == "" || incomplete[0].PanePID <= 0 {
			t.Fatalf("mid-respawn inventory=%#v err=%v", incomplete, inventoryErr)
		}

		observer := installPackagedCrashParentSeams(t, inventoryPath, lockRoot)
		_, err = runtimeCrashInstance(t, db).ReconcileRuntime()
		var ambiguity *RuntimeReconciliationAmbiguityError
		if !errors.As(err, &ambiguity) || len(ambiguity.Candidates) != 1 {
			t.Fatalf("mid-respawn reconciliation error = %v", err)
		}
		after, found, readErr := db.ReadRuntimeState("one")
		remaining, inventoryErr := readRuntimeCrashCandidates(inventoryPath)
		if readErr != nil || !found || after != seeded || observer.err != nil || inventoryErr != nil ||
			observer.stampCalls != 0 || observer.sweepCalls != 0 || len(observer.killed) != 0 ||
			len(remaining) != 1 || remaining[0] != incomplete[0] {
			t.Fatalf("durable=%#v found=%v readErr=%v stamp=%d sweep=%d killed=%v remaining=%#v observerErr=%v inventoryErr=%v",
				after, found, readErr, observer.stampCalls, observer.sweepCalls, observer.killed, remaining, observer.err, inventoryErr)
		}
		requireNoPackagedCrashBinding(t, db)
		requirePackagedCrashLockAvailable(t, lockRoot)
	})

	t.Run("precommit unique candidate is adopted", func(t *testing.T) {
		root := t.TempDir()
		dbPath, db := seedRuntimeCrashDB(t)
		seeded, found, err := db.ReadRuntimeState("one")
		if err != nil || !found {
			t.Fatalf("seeded state=%#v found=%v err=%v", seeded, found, err)
		}
		inventoryPath := filepath.Join(root, "candidates.json")
		lockRoot := filepath.Join(root, "agent-deck")
		runPackagedRuntimeCrash(t, helper, RuntimeTransitionAfterStampBeforeCommit,
			dbPath, inventoryPath, lockRoot, filepath.Join(root, "process"))
		requirePackagedCrashLockAvailable(t, lockRoot)
		before, found, err := db.ReadRuntimeState("one")
		if err != nil || !found || before != seeded {
			t.Fatalf("precommit durable state=%#v found=%v err=%v", before, found, err)
		}
		stamped, inventoryErr := readRuntimeCrashCandidates(inventoryPath)
		if inventoryErr != nil || len(stamped) != 1 {
			t.Fatalf("precommit inventory=%#v err=%v", stamped, inventoryErr)
		}
		requirePackagedCrashCandidate(t, stamped[0])

		observer := installPackagedCrashParentSeams(t, inventoryPath, lockRoot)
		result, err := runtimeCrashInstance(t, db).ReconcileRuntime()
		if err != nil {
			t.Fatal(err)
		}
		after, found, readErr := db.ReadRuntimeState("one")
		remaining, inventoryErr := readRuntimeCrashCandidates(inventoryPath)
		want := packagedCrashState()
		if readErr != nil || !found || after != want || result.State != want ||
			!result.Live || !result.Adopted {
			t.Fatalf("result=%#v durable=%#v found=%v err=%v", result, after, found, readErr)
		}
		if observer.err != nil || inventoryErr != nil || observer.stampCalls != 0 ||
			observer.sweepCalls != 1 || len(observer.killed) != 0 || len(remaining) != 1 {
			t.Fatalf("stamp=%d sweep=%d killed=%v remaining=%v observerErr=%v inventoryErr=%v",
				observer.stampCalls, observer.sweepCalls, observer.killed, remaining, observer.err, inventoryErr)
		}
		requirePackagedCrashCandidate(t, remaining[0])
		requireNoPackagedCrashBinding(t, db)
		requirePackagedCrashLockAvailable(t, lockRoot)
	})

	t.Run("postcommit winner retains and sweeps only proved lower", func(t *testing.T) {
		root := t.TempDir()
		dbPath, db := seedRuntimeCrashDB(t)
		inventoryPath := filepath.Join(root, "candidates.json")
		lockRoot := filepath.Join(root, "agent-deck")
		lower := tmux.RuntimeCandidate{
			SessionName: "runtime-g0", SocketName: "isolated", SessionID: "$7", PaneID: "%9", InstanceID: "one",
			Generation: 0, GenerationKnown: true, PanePID: 3131,
		}
		if err := writeRuntimeCrashCandidates(inventoryPath, []tmux.RuntimeCandidate{lower}); err != nil {
			t.Fatal(err)
		}
		runPackagedRuntimeCrash(t, helper, RuntimeTransitionAfterCommitBeforeSweep,
			dbPath, inventoryPath, lockRoot, filepath.Join(root, "process"))
		requirePackagedCrashLockAvailable(t, lockRoot)
		committed, found, readErr := db.ReadRuntimeState("one")
		if readErr != nil || !found || committed != packagedCrashState() {
			t.Fatalf("postcommit durable state=%#v found=%v err=%v", committed, found, readErr)
		}
		requireNoPackagedCrashBinding(t, db)
		candidates, err := readRuntimeCrashCandidates(inventoryPath)
		if err != nil || len(candidates) != 2 {
			t.Fatalf("read committed candidate: %v, %v", candidates, err)
		}
		var sawLower, sawWinner bool
		for _, candidate := range candidates {
			switch candidate.SessionName {
			case "runtime-g0":
				sawLower = candidate == lower
			case "runtime-g1":
				requirePackagedCrashCandidate(t, candidate)
				sawWinner = true
			}
		}
		if !sawLower || !sawWinner {
			t.Fatalf("postcommit crash swept or lost candidate before reconciliation: %#v", candidates)
		}

		observer := installPackagedCrashParentSeams(t, inventoryPath, lockRoot)
		result, err := runtimeCrashInstance(t, db).ReconcileRuntime()
		if err != nil {
			t.Fatal(err)
		}
		after, found, readErr := db.ReadRuntimeState("one")
		remaining, inventoryErr := readRuntimeCrashCandidates(inventoryPath)
		want := packagedCrashState()
		if readErr != nil || !found || after != want || result.State != want ||
			!result.Live || result.Adopted {
			t.Fatalf("result=%#v durable=%#v found=%v err=%v", result, after, found, readErr)
		}
		if observer.err != nil || inventoryErr != nil || observer.stampCalls != 0 ||
			observer.sweepCalls != 1 || len(observer.killed) != 1 ||
			observer.killed[0] != "runtime-g0" || len(remaining) != 1 || remaining[0].SessionName != "runtime-g1" {
			t.Fatalf("stamp=%d sweep=%d killed=%v remaining=%v observerErr=%v inventoryErr=%v",
				observer.stampCalls, observer.sweepCalls, observer.killed, remaining, observer.err, inventoryErr)
		}
		requirePackagedCrashCandidate(t, remaining[0])
		requireNoPackagedCrashBinding(t, db)
		requirePackagedCrashLockAvailable(t, lockRoot)
	})

	t.Run("precommit ambiguity preserves every candidate", func(t *testing.T) {
		root := t.TempDir()
		dbPath, db := seedRuntimeCrashDB(t)
		seeded, found, err := db.ReadRuntimeState("one")
		if err != nil || !found {
			t.Fatalf("seeded state=%#v found=%v err=%v", seeded, found, err)
		}
		inventoryPath := filepath.Join(root, "candidates.json")
		lockRoot := filepath.Join(root, "agent-deck")
		runPackagedRuntimeCrash(t, helper, RuntimeTransitionAfterStampBeforeCommit,
			dbPath, inventoryPath, lockRoot, filepath.Join(root, "process"))
		requirePackagedCrashLockAvailable(t, lockRoot)
		candidates, err := readRuntimeCrashCandidates(inventoryPath)
		if err != nil || len(candidates) != 1 {
			t.Fatalf("read precommit candidate: %v, %v", candidates, err)
		}
		requirePackagedCrashCandidate(t, candidates[0])
		competing := candidates[0]
		competing.SessionName = "runtime-g1-competing"
		competing.PanePID++
		candidates = append(candidates, competing)
		if err := writeRuntimeCrashCandidates(inventoryPath, candidates); err != nil {
			t.Fatal(err)
		}

		observer := installPackagedCrashParentSeams(t, inventoryPath, lockRoot)
		_, err = runtimeCrashInstance(t, db).ReconcileRuntime()
		var ambiguity *RuntimeReconciliationAmbiguityError
		if !errors.As(err, &ambiguity) || len(ambiguity.Candidates) != 2 {
			t.Fatalf("error=%v", err)
		}
		after, found, readErr := db.ReadRuntimeState("one")
		remaining, inventoryErr := readRuntimeCrashCandidates(inventoryPath)
		if readErr != nil || !found || after != seeded || observer.err != nil || inventoryErr != nil ||
			observer.stampCalls != 0 || observer.sweepCalls != 0 || len(observer.killed) != 0 || len(remaining) != 2 {
			t.Fatalf("durable=%#v found=%v readErr=%v stamp=%d sweep=%d killed=%v remaining=%v observerErr=%v inventoryErr=%v",
				after, found, readErr, observer.stampCalls, observer.sweepCalls,
				observer.killed, remaining, observer.err, inventoryErr)
		}
		requireNoPackagedCrashBinding(t, db)
		requirePackagedCrashLockAvailable(t, lockRoot)
	})
}
