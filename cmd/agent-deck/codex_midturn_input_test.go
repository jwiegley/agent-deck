package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

func appendCodexRolloutLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range lines {
		if _, err := f.WriteString(line + "\n"); err != nil {
			_ = f.Close()
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func codexRolloutStamp(at time.Time) string {
	return at.UTC().Format("2006-01-02T15:04:05.000Z")
}

// A send that lands while a turn runs starts no turn: Codex records it as one
// more user message of the running turn. Before this fix the unchanged
// generation wedged every later send to the session behind the marker.
func TestCodexAcceptanceGuardReconcilesInputTheRunningTurnTookUp(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex"))
	path := filepath.Join(home, "codex", "sessions", "2026", "10", "21", "rollout-test-thread-midturn.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	started := time.Now().Add(-time.Minute)
	appendCodexRolloutLines(t, path,
		`{"timestamp":"`+codexRolloutStamp(started)+`","type":"event_msg","payload":{"type":"task_started","turn_id":"turn-running"}}`,
		`{"timestamp":"`+codexRolloutStamp(started.Add(250*time.Millisecond))+`","type":"event_msg","payload":{"type":"item_completed","turn_id":"turn-running","item":{"type":"UserMessage","id":"item-opening"}}}`,
	)
	inst := &session.Instance{ID: "instance-midturn", Tool: "codex", CodexSessionID: "thread-midturn"}

	sender, err := acquireCodexAcceptanceGuard(inst, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	sentAt := time.Now()
	if err := sender.Prepare(inst.ID, sentAt); err != nil {
		t.Fatal(err)
	}
	if err := sender.RecordTransportOutcome(deliveryNoEvidence, sentAt.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	sender.Release()

	// Until the running turn takes the input up, delivery is still unknown.
	if guard, err := acquireCodexAcceptanceGuard(inst, time.Second); err == nil {
		guard.Release()
		t.Fatal("a later send crossed an attempt the running turn has not taken up")
	} else if !strings.Contains(err.Error(), "manually remove") {
		t.Fatalf("unresolved refusal omitted recovery path: %v", err)
	}

	appendCodexRolloutLines(t, path,
		`{"timestamp":"`+codexRolloutStamp(sentAt.Add(14*time.Second))+`","type":"event_msg","payload":{"type":"item_completed","turn_id":"turn-running","item":{"type":"UserMessage","id":"item-midturn"}}}`,
		`{"timestamp":"`+codexRolloutStamp(sentAt.Add(3*time.Minute))+`","type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-running","last_agent_message":"done"}}`,
	)
	next, err := acquireCodexAcceptanceGuard(inst, time.Second)
	if err != nil {
		t.Fatalf("input the running turn took up did not reconcile: %v", err)
	}
	defer next.Release()
	if next.fence.priorTurnGeneration != "thread-midturn:turn-running" {
		t.Fatalf("next fence = %q, want the unchanged running turn", next.fence.priorTurnGeneration)
	}
	if n := codexSubmissionMarkerCount(t); n != 0 {
		t.Fatalf("reconciliation left %d submission markers", n)
	}
}

func codexSubmissionMarkerCount(t *testing.T) int {
	t.Helper()
	root, err := session.GetAgentDeckDir()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "locks", "codex-submissions"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") {
			count++
		}
	}
	return count
}
