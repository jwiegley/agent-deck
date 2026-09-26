package tmux

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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

// runtimeCandidateRecordForTest formats one snapshot row: generation 7,
// revision 3, running, a start stamp, the codex binding and pane pid 4242.
func runtimeCandidateRecordForTest(sessionID, name, paneID, instanceID string) string {
	return strings.Join([]string{
		sessionID, name, paneID, instanceID, "7", "3", "running", "1000000000", "codex", "4242", "value",
	}, tmuxFieldSep)
}

// A runtime Agent Deck started under an unprefixed tmux name (an imported or
// fixture name) is still Agent Deck's once its session-local cleanup stamp
// names the instance. Inherited #{E:} values without that stamp, a partial
// stamp, or a stamp for another instance never admit one.
func TestRuntimeLifecycle_RuntimeCandidateSnapshotAdmitsStampedUnprefixedSession(t *testing.T) {
	oldOutput := runtimeCandidateSnapshotOutputFn
	oldLocal := runtimeCleanupLocalOptionsFn
	t.Cleanup(func() {
		runtimeCandidateSnapshotOutputFn = oldOutput
		runtimeCleanupLocalOptionsFn = oldLocal
	})
	runtimeCandidateSnapshotOutputFn = func(socketName string, args ...string) ([]byte, error) {
		if socketName != "isolated" || len(args) != 3 || args[0] != "list-sessions" {
			t.Fatalf("snapshot command = %q %q", socketName, args)
		}
		rows := []string{
			runtimeCandidateRecordForTest("$1", "agentdeck_prefixed", "%1", "prefixed"),
			runtimeCandidateRecordForTest("$2", "ad-golden-sess-shell", "%2", "stamped"),
			runtimeCandidateRecordForTest("$3", "user-work", "%3", "inherited"),
			runtimeCandidateRecordForTest("$4", "partial", "%4", "partial"),
			runtimeCandidateRecordForTest("$5", "other-stamp", "%5", "claimed"),
			runtimeCandidateRecordForTest("$6", "plain-shell", "%6", ""),
		}
		return []byte(strings.Join(rows, "\n") + "\n"), nil
	}
	partial := runtimeCleanupLocalOptionsForTest("partial", 7, "CODEX_SESSION_ID", "value")
	delete(partial, runtimeCleanupGenerationOption)
	bySession := map[string]map[string]runtimeCleanupLocalOption{
		"$2": runtimeCleanupLocalOptionsForTest("stamped", 7, "CODEX_SESSION_ID", "value"),
		"$4": partial,
		"$5": runtimeCleanupLocalOptionsForTest("someone-else", 7, "CODEX_SESSION_ID", "value"),
	}
	var localReads [][]string
	runtimeCleanupLocalOptionsFn = func(socketName string, candidates []RuntimeBindingCandidate) (map[string]map[string]runtimeCleanupLocalOption, error) {
		if socketName != "isolated" {
			t.Fatalf("local stamp socket = %q", socketName)
		}
		ids := make([]string, 0, len(candidates))
		result := make(map[string]map[string]runtimeCleanupLocalOption, len(candidates))
		for _, candidate := range candidates {
			ids = append(ids, candidate.SessionID)
			result[candidate.SessionID] = bySession[candidate.SessionID]
		}
		localReads = append(localReads, ids)
		return result, nil
	}

	snapshot := SnapshotRuntimeCandidates([]string{"isolated"})
	if want := [][]string{{"$2", "$3", "$4", "$5"}}; !reflect.DeepEqual(localReads, want) {
		t.Fatalf("local stamp reads = %q, want one batch for the unprefixed claims %q", localReads, want)
	}
	for instanceID, wantSession := range map[string]string{
		"prefixed": "agentdeck_prefixed", "stamped": "ad-golden-sess-shell",
		"inherited": "", "partial": "", "claimed": "", "someone-else": "",
	} {
		candidates, err := snapshot.Candidates(instanceID, "isolated")
		if err != nil {
			t.Fatalf("candidates for %q: %v", instanceID, err)
		}
		if wantSession == "" {
			if len(candidates) != 0 {
				t.Fatalf("candidates for %q = %#v, want none", instanceID, candidates)
			}
			continue
		}
		if len(candidates) != 1 || candidates[0].SessionName != wantSession || !candidates[0].GenerationKnown ||
			candidates[0].Generation != 7 || candidates[0].PanePID != 4242 {
			t.Fatalf("candidates for %q = %#v, want %q", instanceID, candidates, wantSession)
		}
	}
}

// A failed local-stamp read leaves the socket indeterminate; it never silently
// drops a runtime that may be stamped.
func TestRuntimeLifecycle_RuntimeCandidateSnapshotUnprefixedStampReadFailureIsIndeterminate(t *testing.T) {
	oldOutput := runtimeCandidateSnapshotOutputFn
	oldLocal := runtimeCleanupLocalOptionsFn
	t.Cleanup(func() {
		runtimeCandidateSnapshotOutputFn = oldOutput
		runtimeCleanupLocalOptionsFn = oldLocal
	})
	runtimeCandidateSnapshotOutputFn = func(string, ...string) ([]byte, error) {
		return []byte(runtimeCandidateRecordForTest("$2", "ad-golden-sess-shell", "%2", "stamped") + "\n"), nil
	}
	blocked := errors.New("blocked local stamp read")
	runtimeCleanupLocalOptionsFn = func(string, []RuntimeBindingCandidate) (map[string]map[string]runtimeCleanupLocalOption, error) {
		return nil, blocked
	}

	if _, err := ListRuntimeCandidates("isolated", "stamped"); !errors.Is(err, blocked) {
		t.Fatalf("inventory error = %v, want the failed local stamp read", err)
	}
}

// SelectedRuntimeSessionExists asks for exactly the selected name on its own
// socket and reads only tmux's canonical absence answers as absence.
func TestRuntimeLifecycle_SelectedRuntimeSessionExistsClassifiesExactProbe(t *testing.T) {
	tests := []struct {
		name        string
		stderr      string
		exit        int
		wantPresent bool
		wantErr     bool
	}{
		{name: "answers", wantPresent: true},
		{name: "missing session", stderr: "can't find session: ad-golden-sess-shell", exit: 1},
		{name: "no server", stderr: "no server running on /tmp/tmux-501/isolated", exit: 1},
		{name: "no socket file", stderr: "error connecting to /tmp/tmux-501/isolated (No such file or directory)", exit: 1},
		{name: "another session missing", stderr: "can't find session: ad-golden", exit: 1, wantErr: true},
		{name: "permission", stderr: "error connecting to /tmp/tmux-501/isolated (Permission denied)", exit: 1, wantErr: true},
		{name: "protocol", stderr: "protocol version mismatch", exit: 1, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			binDir := t.TempDir()
			argsFile := filepath.Join(binDir, "args")
			script := "#!/bin/sh\nfor arg in \"$@\"; do printf '%s\\n' \"$arg\"; done > " + argsFile + "\n"
			if tt.stderr != "" {
				script += fmt.Sprintf("printf '%%s\\n' %q >&2\n", tt.stderr)
			}
			script += fmt.Sprintf("exit %d\n", tt.exit)
			if err := os.WriteFile(filepath.Join(binDir, "tmux"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", binDir)

			present, err := SelectedRuntimeSessionExists("isolated", "ad-golden-sess-shell")
			if present != tt.wantPresent || (err != nil) != tt.wantErr {
				t.Fatalf("probe = (%v, %v), want present=%v error=%v", present, err, tt.wantPresent, tt.wantErr)
			}
			raw, readErr := os.ReadFile(argsFile)
			if readErr != nil {
				t.Fatal(readErr)
			}
			args := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
			want := []string{"-L", "isolated", "has-session", "-t", "=ad-golden-sess-shell"}
			if len(args) < len(want) || !reflect.DeepEqual(args[len(args)-len(want):], want) {
				t.Fatalf("tmux args = %q, want to end with %q", args, want)
			}
		})
	}
}
