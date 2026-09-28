package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCodexHookStatusOwnership(t *testing.T) {
	skipIfNoTmuxBinary(t)
	for _, mode := range []string{"cold", "cached", "rejected-cache"} {
		for _, source := range []string{"subagent", "unbacked", "user"} {
			t.Run(mode+"/"+source, func(t *testing.T) {
				inst, codexHome := newCodexGateInstance(t)
				root, candidate := uniqueSID(t), uniqueSID(t)
				seedCodexRolloutWithMeta(t, codexHome, root, "user", "", false)
				parent := ""
				if source == "subagent" {
					parent = root
				}
				if source != "unbacked" {
					seedCodexRolloutWithMeta(t, codexHome, candidate, source, parent, false)
				}
				inst.CodexSessionID = root
				inst.Status = StatusRunning
				if err := inst.tmuxSession.Start("sleep 300"); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = inst.tmuxSession.Kill() })
				// The fixture keeps a real pane alive without launching an agent.
				inst.tmuxSession.Command = "codex"
				if err := inst.tmuxSession.SetEnvironment("CODEX_SESSION_ID", root); err != nil {
					t.Fatal(err)
				}
				now := time.Now()
				event := "UserPromptSubmit"
				if source == "unbacked" {
					event = "agent-turn-complete"
				}
				hook := &HookStatus{Status: "running", SessionID: candidate, Event: event, UpdatedAt: now}
				if mode == "cold" {
					data, err := json.Marshal(map[string]any{
						"status": hook.Status, "session_id": candidate, "event": hook.Event, "ts": now.Unix(),
					})
					if err != nil {
						t.Fatal(err)
					}
					if err := os.MkdirAll(GetHooksDir(), 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(GetHooksDir(), inst.ID+".json"), data, 0o600); err != nil {
						t.Fatal(err)
					}
				} else {
					inst.hookStatus, inst.hookEvent = hook.Status, hook.Event
					inst.hookLastUpdate, inst.hookSessionID = now, candidate
					if mode == "rejected-cache" {
						inst.UpdateHookStatus(hook)
					}
				}
				if err := inst.UpdateStatus(); err != nil {
					t.Fatal(err)
				}
				want := candidate
				if source != "user" {
					want = root
				}
				if inst.CodexSessionID != want {
					t.Fatalf("%s hook changed owning conversation: got %q, want %q", source, inst.CodexSessionID, want)
				}
				if source != "user" && inst.hookSessionID == candidate {
					t.Fatal("rejected foreign thread remains cached for the next status pass")
				}
			})
		}
	}
}

// The cached-evidence recheck must not discard the pane's own turn-end: the
// bound thread is never foreign, even when no rollout for it is found under
// this Codex home.
func TestCodexHookStatusKeepsBoundThreadTurnEnd(t *testing.T) {
	skipIfNoTmuxBinary(t)
	inst, _ := newCodexGateInstance(t)
	bound := uniqueSID(t)
	inst.CodexSessionID = bound
	inst.Status = StatusRunning
	if err := inst.tmuxSession.Start("sleep 300"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inst.tmuxSession.Kill() })
	inst.tmuxSession.Command = "codex"
	if err := inst.tmuxSession.SetEnvironment("CODEX_SESSION_ID", bound); err != nil {
		t.Fatal(err)
	}
	inst.hookStatus, inst.hookEvent = "waiting", "agent-turn-complete"
	inst.hookLastUpdate, inst.hookSessionID = time.Now(), bound
	if err := inst.UpdateStatus(); err != nil {
		t.Fatal(err)
	}
	if inst.CodexSessionID != bound || inst.hookSessionID != bound {
		t.Fatalf("bound thread's turn-end was discarded: binding=%q cached=%q, want %q",
			inst.CodexSessionID, inst.hookSessionID, bound)
	}
	if got := inst.GetStatusThreadSafe(); got != StatusWaiting {
		t.Fatalf("status = %q, want %q from the bound thread's turn-end", got, StatusWaiting)
	}
}
