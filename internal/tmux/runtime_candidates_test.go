package tmux

import (
	"fmt"
	"strings"
	"testing"
)

func TestRuntimeLifecycle_RuntimeCandidateSnapshotScalesWithSocketsNotInstances(t *testing.T) {
	oldOutput := runtimeCandidateSnapshotOutputFn
	t.Cleanup(func() { runtimeCandidateSnapshotOutputFn = oldOutput })

	sockets := []string{"socket-a", "socket-b", "socket-c"}
	calls := make(map[string]int)
	runtimeCandidateSnapshotOutputFn = func(socketName string, args ...string) ([]byte, error) {
		calls[socketName]++
		if len(args) != 3 || args[0] != "list-sessions" || args[1] != "-F" || args[2] != runtimeCandidateFormat() {
			t.Fatalf("snapshot command for %q = %q", socketName, args)
		}
		var output strings.Builder
		for index := 0; index < 200; index++ {
			instanceID := fmt.Sprintf("%s-instance-%03d", socketName, index)
			fmt.Fprintln(&output, strings.Join([]string{
				fmt.Sprintf("$%d", index+1), SessionPrefix + instanceID, fmt.Sprintf("%%%d", index+1),
				instanceID, "7", "3", "running", "1000000000", "codex", "4242", "binding|value",
			}, tmuxFieldSep))
		}
		return []byte(output.String()), nil
	}

	snapshot := SnapshotRuntimeCandidates(append(sockets, "socket-a"))
	if len(calls) != len(sockets) {
		t.Fatalf("snapshot subprocess sockets = %v, want %v", calls, sockets)
	}
	for _, socketName := range sockets {
		if calls[socketName] != 1 {
			t.Fatalf("snapshot subprocesses for %q = %d, want 1", socketName, calls[socketName])
		}
		for index := 0; index < 200; index++ {
			instanceID := fmt.Sprintf("%s-instance-%03d", socketName, index)
			candidates, err := snapshot.Candidates(instanceID, sockets...)
			if err != nil {
				t.Fatalf("candidates for %q: %v", instanceID, err)
			}
			if len(candidates) != 1 || candidates[0].InstanceID != instanceID ||
				candidates[0].SessionID == "" || candidates[0].PaneID == "" || candidates[0].BindingValue != "binding|value" {
				t.Fatalf("candidates for %q = %#v", instanceID, candidates)
			}
		}
	}
	if len(calls) != len(sockets) {
		t.Fatalf("instance lookups spawned more subprocesses: %v", calls)
	}
}

func TestRuntimeLifecycle_RuntimeCandidateSnapshotDoesNotTreatFailedSocketAsEmpty(t *testing.T) {
	wantErr := fmt.Errorf("blocked tmux inventory")
	snapshot := RuntimeCandidateSnapshot{
		"blocked": {Err: wantErr},
	}
	if _, err := snapshot.Candidates("one", "blocked"); err != wantErr {
		t.Fatalf("failed socket error = %v, want %v", err, wantErr)
	}
	if _, err := snapshot.Candidates("one", "not-inventoried"); err == nil {
		t.Fatal("omitted socket was treated as an empty inventory")
	}
}

func TestRuntimeLifecycle_ListRuntimeCandidatesAcceptsEmptyInventory(t *testing.T) {
	oldOutput := runtimeCandidateSnapshotOutputFn
	runtimeCandidateSnapshotOutputFn = func(string, ...string) ([]byte, error) { return nil, nil }
	t.Cleanup(func() { runtimeCandidateSnapshotOutputFn = oldOutput })

	candidates, err := ListRuntimeCandidates("", "instance")
	if err != nil || len(candidates) != 0 {
		t.Fatalf("empty inventory = %#v, err=%v", candidates, err)
	}
}
