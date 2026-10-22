package session

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func codexMidturnStamp(at time.Time) string {
	return at.UTC().Format("2006-01-02T15:04:05.000Z")
}

func codexMidturnTurnStart(turnID string, at time.Time) string {
	return `{"timestamp":"` + codexMidturnStamp(at) + `","type":"event_msg","payload":{"type":"task_started","turn_id":"` + turnID + `"}}`
}

func codexMidturnUserItem(turnID string, at time.Time) string {
	return `{"timestamp":"` + codexMidturnStamp(at) + `","type":"event_msg","payload":{"type":"item_completed","turn_id":"` + turnID + `","item":{"type":"UserMessage","id":"item"}}}`
}

func codexMidturnLegacyUserMessage(at time.Time) string {
	return `{"timestamp":"` + codexMidturnStamp(at) + `","type":"event_msg","payload":{"type":"user_message","message":"text"}}`
}

func TestCodexTurnTookInputSinceFromRollout(t *testing.T) {
	start := time.Date(2026, 10, 21, 18, 0, 39, 871_000_000, time.UTC)
	since := start.Add(900 * time.Millisecond)
	later := since.Add(13 * time.Second)
	for _, tt := range []struct {
		name  string
		lines []string
		want  bool
	}{
		{
			name: "later input in the running turn",
			lines: []string{
				codexMidturnTurnStart("turn-running", start),
				codexMidturnUserItem("turn-running", start.Add(253*time.Millisecond)),
				`{"timestamp":"` + codexMidturnStamp(later) + `","type":"response_item","payload":{"type":"message","role":"user"}}`,
				codexMidturnUserItem("turn-running", later),
				`{"timestamp":"` + codexMidturnStamp(later.Add(time.Minute)) + `","type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-running"}}`,
			},
			want: true,
		},
		{
			name: "opening input recorded after the attempt began",
			lines: []string{
				codexMidturnTurnStart("turn-running", start),
				codexMidturnUserItem("turn-running", since.Add(time.Millisecond)),
			},
		},
		{
			name: "later input recorded before the attempt began",
			lines: []string{
				codexMidturnTurnStart("turn-running", start),
				codexMidturnUserItem("turn-running", start.Add(253*time.Millisecond)),
				codexMidturnUserItem("turn-running", since.Add(-time.Millisecond)),
			},
		},
		{
			name: "a newer turn started",
			lines: []string{
				codexMidturnTurnStart("turn-running", start),
				codexMidturnUserItem("turn-running", start.Add(253*time.Millisecond)),
				codexMidturnUserItem("turn-running", later),
				codexMidturnTurnStart("turn-next", later.Add(time.Second)),
				codexMidturnUserItem("turn-next", later.Add(2*time.Second)),
			},
		},
		{
			name: "later input attributed to another turn",
			lines: []string{
				codexMidturnTurnStart("turn-running", start),
				codexMidturnUserItem("turn-running", start.Add(253*time.Millisecond)),
				codexMidturnUserItem("turn-other", later),
			},
		},
		{
			name: "legacy user_message events",
			lines: []string{
				codexMidturnTurnStart("turn-running", start),
				codexMidturnLegacyUserMessage(start.Add(253 * time.Millisecond)),
				codexMidturnLegacyUserMessage(later),
			},
			want: true,
		},
		{
			name: "one input in each encoding is still only the opening input",
			lines: []string{
				codexMidturnTurnStart("turn-running", start),
				codexMidturnLegacyUserMessage(since.Add(time.Millisecond)),
				codexMidturnUserItem("turn-running", since.Add(2*time.Millisecond)),
			},
		},
		{
			name: "no turn start",
			lines: []string{
				codexMidturnUserItem("turn-running", start.Add(253*time.Millisecond)),
				codexMidturnUserItem("turn-running", later),
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "rollout.jsonl")
			if err := os.WriteFile(path, []byte(strings.Join(tt.lines, "\n")+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := codexTurnTookInputSinceFromRollout(path, "turn-running", since)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("took input = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCodexTurnTookInputSinceFailsClosedPastScanWindow(t *testing.T) {
	start := time.Date(2026, 10, 21, 18, 0, 39, 0, time.UTC)
	since := start.Add(time.Second)
	var b strings.Builder
	b.WriteString(codexMidturnTurnStart("turn-running", start) + "\n")
	b.WriteString(codexMidturnUserItem("turn-running", start.Add(250*time.Millisecond)) + "\n")
	padding := `{"type":"event_msg","payload":{"type":"agent_message","message":"` + strings.Repeat("x", 1024) + `"}}` + "\n"
	for int64(b.Len()) <= codexTurnGenerationScanMaxBytes {
		b.WriteString(padding)
	}
	b.WriteString(codexMidturnUserItem("turn-running", since.Add(time.Second)) + "\n")
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := codexTurnTookInputSinceFromRollout(path, "turn-running", since)
	if err != nil || got {
		t.Fatalf("turn start beyond the scan window: took input = %v, err = %v; want false", got, err)
	}
}

func TestCodexSubmissionMarkerReconcilesInputTheRunningTurnTookUp(t *testing.T) {
	isolateCodexSubmissionMarkers(t)
	createdAt := time.Date(2026, 10, 21, 18, 0, 40, 762_602_000, time.UTC)
	marker, err := PrepareCodexSubmissionMarker(
		"instance-midturn", "thread-midturn", "thread-midturn:turn-running", createdAt,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := marker.MarkTransportAmbiguous(createdAt.Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	path, err := codexSubmissionMarkerPath(marker.CodexSessionID)
	if err != nil {
		t.Fatal(err)
	}
	refuses := func(name string, tookInputSince func(string, time.Time) (bool, error)) error {
		t.Helper()
		accepted, err := ReconcileCodexSubmissionMarker(
			marker.InstanceID, marker.CodexSessionID, marker.PriorTurnGeneration, tookInputSince,
		)
		if err == nil || accepted != "" || !strings.Contains(err.Error(), "manually remove") {
			t.Fatalf("%s: accepted=%q err=%v, want the unresolved refusal", name, accepted, err)
		}
		if _, statErr := os.Stat(path); statErr != nil {
			t.Fatalf("%s: unresolved marker was removed: %v", name, statErr)
		}
		return err
	}

	refuses("no evidence source", nil)
	var gotGeneration string
	var gotSince time.Time
	refuses("no later input", func(generation string, since time.Time) (bool, error) {
		gotGeneration, gotSince = generation, since
		return false, nil
	})
	if gotGeneration != marker.PriorTurnGeneration || !gotSince.Equal(createdAt) {
		t.Fatalf("evidence asked for (%q, %v), want (%q, %v)",
			gotGeneration, gotSince, marker.PriorTurnGeneration, createdAt)
	}
	if err := refuses("unreadable rollout", func(string, time.Time) (bool, error) {
		return false, errors.New("rollout unreadable")
	}); !strings.Contains(err.Error(), "rollout unreadable") {
		t.Fatalf("refusal hid the evidence error: %v", err)
	}

	accepted, err := ReconcileCodexSubmissionMarker(
		marker.InstanceID, marker.CodexSessionID, marker.PriorTurnGeneration,
		func(string, time.Time) (bool, error) { return true, nil },
	)
	if err != nil || accepted != marker.PriorTurnGeneration {
		t.Fatalf("input the running turn took up was not reconciled: accepted=%q err=%v", accepted, err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("reconciled marker still exists: %v", statErr)
	}
}

func TestCodexSubmissionMarkerWithoutTurnIgnoresInputEvidence(t *testing.T) {
	isolateCodexSubmissionMarkers(t)
	marker, err := PrepareCodexSubmissionMarker("instance-fresh", "thread-fresh", "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := ReconcileCodexSubmissionMarker(
		marker.InstanceID, marker.CodexSessionID, "",
		func(string, time.Time) (bool, error) {
			t.Fatal("a thread with no turn has no turn to take input up")
			return true, nil
		},
	)
	if err == nil || accepted != "" {
		t.Fatalf("marker without a turn reconciled: accepted=%q err=%v", accepted, err)
	}
}
