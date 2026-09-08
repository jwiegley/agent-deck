package statedb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const packagedRuntimeBarrierEnv = "AGENTDECK_RUNTIME_HELPER_BARRIER"
const packagedRuntimeRetryBarrierEnv = "AGENTDECK_RUNTIME_HELPER_RETRY_BARRIER"

var packagedRuntimeStateDBModuleCache = func() string {
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

type packagedRuntimeProcess struct {
	cmd          *exec.Cmd
	cancel       context.CancelFunc
	stdout       bytes.Buffer
	stderr       bytes.Buffer
	ready        *os.File
	release      *os.File
	retryReady   *os.File
	retryRelease *os.File
	waited       bool
}

func buildPackagedRuntimeHelper(t *testing.T) string {
	t.Helper()
	root := runtimeLifecycleRepoRoot(t)
	buildRoot := t.TempDir()
	binary := filepath.Join(buildRoot, "runtime-lifecycle-helper")
	for _, dir := range []string{"home", "config", "data", "cache", "state", "tmp", "go-build"} {
		if err := os.MkdirAll(filepath.Join(buildRoot, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, runtimeLifecycleGo(t), "build",
		"-tags", "runtime_lifecycle_helper", "-o", binary, "./cmd/runtime-lifecycle-helper")
	cmd.Dir = root
	cmd.Env = packagedRuntimeStateDBBuildEnv(buildRoot)
	cmd.WaitDelay = 5 * time.Second
	configurePackagedRuntimeProcessGroup(cmd)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build packaged runtime helper: %v\n%s", err, output)
	}
	return binary
}

func configurePackagedRuntimeProcessGroup(cmd *exec.Cmd) {
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

func killPackagedRuntimeProcessGroup(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

func packagedRuntimeStateDBBuildEnv(root string) []string {
	drop := map[string]bool{
		"HOME": true, "XDG_CONFIG_HOME": true, "XDG_DATA_HOME": true,
		"XDG_CACHE_HOME": true, "XDG_STATE_HOME": true, "TMPDIR": true,
		"GOCACHE": true, "GOMODCACHE": true, "GOPROXY": true,
		"GOTOOLCHAIN": true, "GOENV": true,
	}
	env := make([]string, 0, len(os.Environ())+11)
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
		"GOCACHE="+filepath.Join(root, "go-build"),
		"GOMODCACHE="+packagedRuntimeStateDBModuleCache,
		"GOPROXY=off", "GOTOOLCHAIN=local", "GOENV=off",
	)
}

func runtimeLifecycleRepoRoot(t *testing.T) string {
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

func runtimeLifecycleGo(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("go")
	if err != nil {
		t.Fatal("go executable not found")
	}
	return path
}

func startPackagedRuntimeProcess(
	t *testing.T, helper string, env []string, args ...string,
) *packagedRuntimeProcess {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	child := &packagedRuntimeProcess{cancel: cancel}
	child.cmd = exec.CommandContext(ctx, helper, args...)
	processRoot := t.TempDir()
	for _, dir := range []string{"home", "config", "data", "cache", "state", "tmp", "tmux"} {
		if err := os.MkdirAll(filepath.Join(processRoot, dir), 0o700); err != nil {
			cancel()
			t.Fatal(err)
		}
	}
	child.cmd.Env = packagedRuntimeHelperEnv(processRoot, env)
	child.cmd.Stdout = &child.stdout
	child.cmd.Stderr = &child.stderr
	child.cmd.WaitDelay = 2 * time.Second
	configurePackagedRuntimeProcessGroup(child.cmd)

	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	preStartFiles := []*os.File{readyRead, readyWrite}
	started := false
	t.Cleanup(func() {
		if started {
			return
		}
		cancel()
		for _, file := range preStartFiles {
			_ = file.Close()
		}
	})
	releaseRead, releaseWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	preStartFiles = append(preStartFiles, releaseRead, releaseWrite)
	child.ready, child.release = readyRead, releaseWrite
	child.cmd.ExtraFiles = []*os.File{readyWrite, releaseRead}
	var retryReadyWrite, retryReleaseRead *os.File
	for _, item := range env {
		if item != packagedRuntimeRetryBarrierEnv+"=1" {
			continue
		}
		child.retryReady, retryReadyWrite, err = os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		preStartFiles = append(preStartFiles, child.retryReady, retryReadyWrite)
		retryReleaseRead, child.retryRelease, err = os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		preStartFiles = append(preStartFiles, retryReleaseRead, child.retryRelease)
		child.cmd.ExtraFiles = append(child.cmd.ExtraFiles, retryReadyWrite, retryReleaseRead)
	}
	if err := child.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	started = true
	_ = readyWrite.Close()
	_ = releaseRead.Close()
	if retryReadyWrite != nil {
		_ = retryReadyWrite.Close()
		_ = retryReleaseRead.Close()
	}
	t.Cleanup(func() {
		if !child.waited && child.cmd.Process != nil {
			killPackagedRuntimeProcessGroup(child.cmd)
			_ = child.cmd.Wait()
		}
		child.cancel()
		_ = child.ready.Close()
		if child.release != nil {
			_ = child.release.Close()
		}
		if child.retryReady != nil {
			_ = child.retryReady.Close()
		}
		if child.retryRelease != nil {
			_ = child.retryRelease.Close()
		}
	})
	return child
}

func packagedRuntimeHelperEnv(root string, extra []string) []string {
	drop := map[string]bool{
		packagedRuntimeBarrierEnv:               true,
		"AGENTDECK_RUNTIME_HELPER_BUSY_TIMEOUT": true,
		packagedRuntimeRetryBarrierEnv:          true,
		"HOME":                                  true, "XDG_CONFIG_HOME": true, "XDG_DATA_HOME": true,
		"XDG_CACHE_HOME": true, "XDG_STATE_HOME": true, "TMPDIR": true,
		"TMUX": true, "TMUX_PANE": true, "TMUX_TMPDIR": true,
	}
	env := make([]string, 0, len(os.Environ())+len(extra)+9)
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if !drop[key] {
			env = append(env, item)
		}
	}
	env = append(env,
		packagedRuntimeBarrierEnv+"=1",
		"HOME="+filepath.Join(root, "home"),
		"XDG_CONFIG_HOME="+filepath.Join(root, "config"),
		"XDG_DATA_HOME="+filepath.Join(root, "data"),
		"XDG_CACHE_HOME="+filepath.Join(root, "cache"),
		"XDG_STATE_HOME="+filepath.Join(root, "state"),
		"TMPDIR="+filepath.Join(root, "tmp"),
		"TMUX_TMPDIR="+filepath.Join(root, "tmux"),
	)
	return append(env, extra...)
}

func (p *packagedRuntimeProcess) waitReady(t *testing.T) {
	t.Helper()
	var signal [1]byte
	if _, err := io.ReadFull(p.ready, signal[:]); err != nil {
		p.waited = true
		waitErr := p.cmd.Wait()
		killPackagedRuntimeProcessGroup(p.cmd)
		p.cancel()
		t.Fatalf("packaged helper did not reach barrier: %v (wait=%v)\nstdout: %s\nstderr: %s",
			err, waitErr, p.stdout.String(), p.stderr.String())
	}
}

func (p *packagedRuntimeProcess) releaseBarrier(t *testing.T) {
	t.Helper()
	if _, err := p.release.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	_ = p.release.Close()
	p.release = nil
}

func (p *packagedRuntimeProcess) waitRetryReady(t *testing.T) {
	t.Helper()
	if p.retryReady == nil {
		t.Fatal("packaged helper retry barrier is absent")
	}
	var signal [1]byte
	if _, err := io.ReadFull(p.retryReady, signal[:]); err != nil {
		t.Fatalf("packaged helper did not reach retry barrier: %v", err)
	}
}

func (p *packagedRuntimeProcess) releaseRetryBarrier(t *testing.T) {
	t.Helper()
	if _, err := p.retryRelease.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	_ = p.retryRelease.Close()
	p.retryRelease = nil
}

func (p *packagedRuntimeProcess) wait(t *testing.T) string {
	t.Helper()
	err := p.cmd.Wait()
	p.waited = true
	killPackagedRuntimeProcessGroup(p.cmd)
	p.cancel()
	if err != nil {
		t.Fatalf("packaged helper failed: %v\nstdout: %s\nstderr: %s", err, p.stdout.String(), p.stderr.String())
	}
	return strings.TrimSpace(p.stdout.String())
}

func (p *packagedRuntimeProcess) waitFailure(t *testing.T) string {
	t.Helper()
	err := p.cmd.Wait()
	p.waited = true
	killPackagedRuntimeProcessGroup(p.cmd)
	p.cancel()
	if err == nil {
		t.Fatalf("packaged helper unexpectedly succeeded\nstdout: %s\nstderr: %s", p.stdout.String(), p.stderr.String())
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("packaged helper failure = %v, want exit 1\nstdout: %s\nstderr: %s",
			err, p.stdout.String(), p.stderr.String())
	}
	return p.stderr.String()
}

func runPackagedRuntimeGroup(t *testing.T, children ...*packagedRuntimeProcess) []string {
	t.Helper()
	for _, child := range children {
		child.waitReady(t)
	}
	for _, child := range children {
		child.releaseBarrier(t)
	}
	results := make([]string, len(children))
	for index, child := range children {
		results[index] = child.wait(t)
	}
	return results
}

func packagedRuntimeContender(
	t *testing.T, helper string, args ...string,
) *packagedRuntimeProcess {
	t.Helper()
	return startPackagedRuntimeProcess(t, helper, []string{
		"AGENTDECK_RUNTIME_HELPER_BUSY_TIMEOUT=0",
		packagedRuntimeRetryBarrierEnv + "=1",
	}, args...)
}

func runPackagedContendedCAS(
	t *testing.T, helper, path string, children ...*packagedRuntimeProcess,
) []string {
	t.Helper()
	for _, child := range children {
		child.waitReady(t)
	}
	lock := startPackagedRuntimeProcess(t, helper, nil, "lock", path)
	lock.waitReady(t)
	for _, child := range children {
		child.releaseBarrier(t)
	}
	// Each public operation has reached its own first SQLITE_BUSY retry while
	// the external writer lock is still held. The contenders therefore overlap
	// inside the production operation, not merely before invocation.
	for _, child := range children {
		child.waitRetryReady(t)
	}
	lock.releaseBarrier(t)
	if outcome := lock.wait(t); outcome != "unlocked" {
		t.Fatalf("lock helper = %q", outcome)
	}
	for _, child := range children {
		child.releaseRetryBarrier(t)
	}
	results := make([]string, len(children))
	for index, child := range children {
		results[index] = child.wait(t)
	}
	return results
}

func requireOnePackagedWinner(t *testing.T, outcomes []string, values ...string) string {
	t.Helper()
	winner, conflicts := "", 0
	for _, outcome := range outcomes {
		if strings.HasPrefix(outcome, "won:") {
			winner = strings.TrimPrefix(outcome, "won:")
		} else if outcome == "conflict" {
			conflicts++
		}
	}
	allowed := false
	for _, value := range values {
		allowed = allowed || winner == value
	}
	if winner == "" || !allowed || conflicts != len(outcomes)-1 {
		t.Fatalf("packaged CAS outcomes = %v", outcomes)
	}
	return winner
}

func TestRuntimeLifecycle_PackagedHelper(t *testing.T) {
	helper := buildPackagedRuntimeHelper(t)

	t.Run("concurrent idempotent migration", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "state.db")
		seedRuntimeLifecycleLegacyV13(t, path)
		children := make([]*packagedRuntimeProcess, 4)
		for index := range children {
			children[index] = startPackagedRuntimeProcess(t, helper, nil, "migrate", path)
		}
		succeeded := 0
		for index, outcome := range runPackagedRuntimeGroup(t, children...) {
			switch outcome {
			case "ok":
				succeeded++
			case "busy":
			default:
				t.Fatalf("migration helper %d = %q", index, outcome)
			}
		}
		if succeeded == 0 {
			t.Fatal("no concurrent packaged migrator completed")
		}
		if outcome := runPackagedRuntimeGroup(t,
			startPackagedRuntimeProcess(t, helper, nil, "migrate", path),
		)[0]; outcome != "ok" {
			t.Fatalf("idempotent packaged migration = %q", outcome)
		}

		db, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var version int
		if err := db.DB().QueryRow(`SELECT CAST(value AS INTEGER) FROM metadata WHERE key = 'schema_version'`).Scan(&version); err != nil {
			t.Fatal(err)
		}
		state, found, err := db.ReadRuntimeState("one")
		if err != nil || !found || version != SchemaVersion || state.Generation != 0 || state.TmuxSession != "tmux-g0" {
			t.Fatalf("migrated version=%d state=%#v found=%v err=%v", version, state, found, err)
		}
		binding, found, err := db.ReadRuntimeBinding("one", "claude")
		if err != nil || !found || binding.Generation != 0 || binding.Value != "claude-legacy" {
			t.Fatalf("migrated binding=%#v found=%v err=%v", binding, found, err)
		}
	})

	t.Run("transition binding status CAS and stale writers", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "state.db")
		db := seedRuntimeLifecycleRow(t, path, json.RawMessage(`{"notes":"original"}`))
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}

		transitionWinner := requireOnePackagedWinner(t, runPackagedContendedCAS(t, helper, path,
			packagedRuntimeContender(t, helper, "transition", path, "0", "tmux-a"),
			packagedRuntimeContender(t, helper, "transition", path, "0", "tmux-b"),
		), "tmux-a", "tmux-b")
		bindingWinner := requireOnePackagedWinner(t, runPackagedContendedCAS(t, helper, path,
			packagedRuntimeContender(t, helper, "binding", path, "1", "0", "binding-a"),
			packagedRuntimeContender(t, helper, "binding", path, "1", "0", "binding-b"),
		), "binding-a", "binding-b")
		statusWinner := requireOnePackagedWinner(t, runPackagedContendedCAS(t, helper, path,
			packagedRuntimeContender(t, helper, "status", path, "1", "0", "waiting"),
			packagedRuntimeContender(t, helper, "status", path, "1", "0", "error"),
		), "waiting", "error")

		stale := runPackagedRuntimeGroup(t,
			startPackagedRuntimeProcess(t, helper, nil, "stale-save", path),
			startPackagedRuntimeProcess(t, helper, nil, "status", path, "0", "0", "stale-status"),
			startPackagedRuntimeProcess(t, helper, nil, "binding", path, "1", "0", "stale-binding"),
		)
		if fmt.Sprint(stale) != "[metadata conflict conflict]" {
			t.Fatalf("stale helper outcomes = %v", stale)
		}

		verify, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer verify.Close()
		state, found, err := verify.ReadRuntimeState("one")
		if err != nil || !found || state.Generation != 1 || state.StatusRevision != 1 ||
			state.TmuxSession != transitionWinner || state.Status != statusWinner {
			t.Fatalf("runtime state=%#v found=%v err=%v", state, found, err)
		}
		binding, found, err := verify.ReadRuntimeBinding("one", "claude")
		if err != nil || !found || binding.Generation != 1 || binding.Revision != 1 || binding.Value != bindingWinner {
			t.Fatalf("binding=%#v found=%v err=%v", binding, found, err)
		}
		rows, err := verify.LoadInstances()
		if err != nil || len(rows) != 1 || rows[0].Title != "metadata-won" {
			t.Fatalf("metadata rows=%#v err=%v", rows, err)
		}
		var toolData map[string]json.RawMessage
		if err := json.Unmarshal(rows[0].ToolData, &toolData); err != nil {
			t.Fatal(err)
		}
		if string(toolData["claude_session_id"]) != fmt.Sprintf("%q", bindingWinner) ||
			string(toolData["notes"]) != `"metadata-won"` {
			t.Fatalf("tool_data=%s", rows[0].ToolData)
		}
	})

	t.Run("SQLITE_BUSY same call recovers after an external lock", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "state.db")
		db := seedRuntimeLifecycleRow(t, path, json.RawMessage(`{}`))
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}

		writer := packagedRuntimeContender(t, helper, "status", path, "0", "0", "waiting")
		writer.waitReady(t)
		lock := startPackagedRuntimeProcess(t, helper, nil, "lock", path)
		lock.waitReady(t)
		writer.releaseBarrier(t)
		writer.waitRetryReady(t)
		lock.releaseBarrier(t)
		if outcome := lock.wait(t); outcome != "unlocked" {
			t.Fatalf("lock helper = %q", outcome)
		}
		writer.releaseRetryBarrier(t)
		if outcome := writer.wait(t); outcome != "won:waiting" {
			t.Fatalf("writer helper = %q", outcome)
		}
		if !strings.Contains(writer.stderr.String(), "SQLITE_BUSY retry") ||
			strings.Contains(writer.stderr.String(), "SQLITE_BUSY exhausted retries") {
			t.Fatalf("same-call recovery log:\n%s", writer.stderr.String())
		}

		verify, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer verify.Close()
		state, found, err := verify.ReadRuntimeState("one")
		if err != nil || !found || state.Generation != 0 || state.StatusRevision != 1 || state.Status != "waiting" {
			t.Fatalf("retry state=%#v found=%v err=%v", state, found, err)
		}
	})

	t.Run("SQLITE_BUSY exhaustion is surfaced", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "state.db")
		db := seedRuntimeLifecycleRow(t, path, json.RawMessage(`{}`))
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}

		writer := startPackagedRuntimeProcess(t, helper,
			[]string{"AGENTDECK_RUNTIME_HELPER_BUSY_TIMEOUT=0"},
			"status", path, "0", "0", "must-not-land")
		writer.waitReady(t)
		lock := startPackagedRuntimeProcess(t, helper, nil, "lock", path)
		lock.waitReady(t)
		writer.releaseBarrier(t)
		stderr := writer.waitFailure(t)
		for _, text := range []string{"SQLITE_BUSY retry", "SQLITE_BUSY exhausted retries"} {
			if !strings.Contains(stderr, text) {
				t.Fatalf("exhaustion stderr lacks %q:\n%s", text, stderr)
			}
		}
		lock.releaseBarrier(t)
		if outcome := lock.wait(t); outcome != "unlocked" {
			t.Fatalf("lock helper = %q", outcome)
		}

		verify, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer verify.Close()
		state, found, err := verify.ReadRuntimeState("one")
		if err != nil || !found || state.Generation != 0 || state.StatusRevision != 0 || state.Status != "idle" {
			t.Fatalf("exhausted state=%#v found=%v err=%v", state, found, err)
		}
	})
}
