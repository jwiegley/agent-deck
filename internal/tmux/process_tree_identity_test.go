package tmux

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func installPgrepForProcessTreeTest(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "pgrep")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func TestRuntimeLifecycle_ProcessTreeTreatsPgrepNoMatchAsLeaf(t *testing.T) {
	installPgrepForProcessTreeTest(t, "exit 1")
	pids, err := processTreeFromPanePID(4242)
	if err != nil || !reflect.DeepEqual(pids, []int{4242}) {
		t.Fatalf("leaf process tree = %v, err=%v; want [4242]", pids, err)
	}
}

func TestRuntimeLifecycle_ProcessTreeFailsClosedOnPgrepFailure(t *testing.T) {
	installPgrepForProcessTreeTest(t, "exit 2")
	if pids, err := processTreeFromPanePID(4242); err == nil || pids != nil {
		t.Fatalf("failed process inventory = %v, err=%v; want explicit failure", pids, err)
	}
}

func TestRuntimeLifecycle_ProcessTreeFailsClosedOnMalformedPgrepOutput(t *testing.T) {
	installPgrepForProcessTreeTest(t, "printf 'not-a-pid\\n'")
	if pids, err := processTreeFromPanePID(4242); err == nil || pids != nil {
		t.Fatalf("malformed process inventory = %v, err=%v; want explicit failure", pids, err)
	}
}
