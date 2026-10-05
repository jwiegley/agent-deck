package session

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestUpdateOpenCodeSession_DoesNotHoldLockAcrossSubprocess is half of the
// regression guard for the TUI-freeze bug: it verifies that calling
// updateOpenCodeSession() does not hold i.mu across the opencode CLI
// subprocess. If a future maintainer adds `i.mu.Lock(); defer i.mu.Unlock()`
// inside updateOpenCodeSession (the most natural way to reintroduce the bug),
// this test fails.
//
// It does NOT verify that UpdateStatus's call site drops i.mu around the call;
// see TestUpdateStatus_DropsLockAroundOpencode (source assertion) for that.
func TestUpdateOpenCodeSession_DoesNotHoldLockAcrossSubprocess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stub not portable to windows")
	}

	stubDir := t.TempDir()
	stubPath := filepath.Join(stubDir, "opencode")
	startedFile := filepath.Join(stubDir, "started")
	script := "#!/usr/bin/env bash\n" +
		"touch " + startedFile + "\n" +
		"sleep 0.6\n" +
		"echo '[]'\n"
	if err := os.WriteFile(stubPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	inst := &Instance{
		ID:          "test-instance",
		Tool:        "opencode",
		ProjectPath: t.TempDir(),
	}

	done := make(chan struct{})
	go func() {
		inst.updateOpenCodeSession(true)
		close(done)
	}()

	// Wait up to 10s for the stub to actually enter. Race instrumentation on
	// this large package can delay initial goroutine/process scheduling by a
	// few seconds; the file remains the deterministic handshake.
	waitDeadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(waitDeadline) {
		if _, err := os.Stat(startedFile); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(startedFile); err != nil {
		<-done
		t.Fatalf("opencode stub never ran (PATH issue?): %v", err)
	}

	// If updateOpenCodeSession holds i.mu (write lock) across the
	// subprocess, TryLock fails here.
	if !inst.mu.TryLock() {
		<-done
		t.Fatal("i.mu is held while the opencode subprocess is running; " +
			"updateOpenCodeSession must release i.mu across the subprocess.")
	}
	inst.mu.Unlock()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("updateOpenCodeSession did not return within 5s")
	}
}

// TestUpdateStatus_DropsLockAroundOpencode is the other half of the regression
// guard: it parses the post-authority metadata source and asserts that status
// refresh releases i.mu before delegating to UpdateOpenCodeSession. The
// callee's subprocess lock boundary is tested above.
//
// This is a source-level test because standing up a realistic tmux session
// that reaches the metadata-sync block at UpdateStatus:~2950 requires
// bypassing a 2-minute startup window and a content-based status detector —
// neither is reasonable in a unit test.
func TestUpdateStatus_DropsLockAroundOpencode(t *testing.T) {
	src, err := os.ReadFile("instance.go")
	if err != nil {
		t.Fatalf("read instance.go: %v", err)
	}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "instance.go", src, 0)
	if err != nil {
		t.Fatalf("parse instance.go: %v", err)
	}

	var metadataRefresh *ast.FuncDecl
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "refreshStatusMetadataIfCurrent" {
			continue
		}
		if fn.Recv == nil || len(fn.Recv.List) == 0 {
			continue
		}
		metadataRefresh = fn
		break
	}
	if metadataRefresh == nil {
		t.Fatal("refreshStatusMetadataIfCurrent function not declared in instance.go")
	}

	// Serialize the function body and scan for the required pattern.
	// Extract the raw source range of the function body.
	start := fset.Position(metadataRefresh.Body.Pos()).Offset
	end := fset.Position(metadataRefresh.Body.End()).Offset
	body := string(src[start:end])

	// Anchor the search on the lock-managing public refresh call. Publication
	// and the subprocess both happen after refreshStatusMetadataIfCurrent has
	// left its instance critical section.
	const window = 400 // chars
	callIdx := strings.Index(body, "i.UpdateOpenCodeSession()")
	if callIdx == -1 {
		t.Fatal("metadata refresh does not refresh OpenCode at all")
	}

	beforeStart := callIdx - window
	if beforeStart < 0 {
		beforeStart = 0
	}
	before := body[beforeStart:callIdx]
	if !strings.Contains(before, "i.mu.Unlock()") {
		t.Fatalf("metadata refresh does not call i.mu.Unlock() within %d chars before the OpenCode refresh — "+
			"freeze regression: i.mu is held across the opencode subprocess.\nContext:\n%s", window, before)
	}
}
