package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Exercise the status poller's disk and cached-hook paths against a real
// SQLite binding and real Codex rollouts. Only the tmux/process probes are
// fixtures; no agent process is needed to reproduce the binding corruption.
func TestCodexHookPublication(t *testing.T) {
	for _, mode := range []string{"cold-child", "cached-child", "rejected-cached-child", "cold-root"} {
		for _, statusOnly := range []bool{false, true} {
			if statusOnly && mode == "cold-root" {
				continue // Status-only callers deliberately do not publish a new binding.
			}
			name := mode
			if statusOnly {
				name += "-status-only"
			}
			t.Run(name, func(t *testing.T) {
				t.Setenv("HOME", t.TempDir())
				ClearUserConfigCache()
				t.Cleanup(ClearUserConfigCache)
				codexHome, _ := statusPassFixture(t, 1)
				storage := runtimeBindingTestStorage(t)
				inst := passInstance(0, "")
				rootID, candidateID := uniqueSID(t), uniqueSID(t)
				inst.CodexSessionID = rootID
				seedCodexRolloutWithMeta(t, codexHome, rootID, "user", "", false)
				if mode == "cold-root" {
					seedCodexRolloutWithMeta(t, codexHome, candidateID, "user", "", false)
				} else {
					seedCodexRolloutWithMeta(t, codexHome, candidateID, "subagent", rootID, true)
				}
				if err := storage.InsertSessionAndVerify(inst, nil); err != nil {
					t.Fatal(err)
				}
				before, found, err := storage.db.ReadRuntimeBinding(inst.ID, "codex")
				if err != nil || !found || before.Value != rootID {
					t.Fatalf("initial binding = %+v, found=%v, err=%v", before, found, err)
				}

				hookDir := GetHooksDir()
				if err := os.MkdirAll(hookDir, 0o700); err != nil {
					t.Fatal(err)
				}
				hookStatus := "waiting"
				if mode == "cold-root" {
					hookStatus = "running"
				}
				body, err := json.Marshal(map[string]any{
					"status": hookStatus, "event": "agent-turn-complete",
					"session_id": candidateID, "ts": time.Now().Unix(),
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(hookDir, inst.ID+".json"), body, 0o600); err != nil {
					t.Fatal(err)
				}
				hs := readHookStatusFile(inst.ID)
				if hs == nil || hs.SessionID != candidateID || !hs.Fingerprint.valid() {
					t.Fatal("hook file did not load with its candidate and fingerprint")
				}

				if mode == "cached-child" || mode == "rejected-cached-child" {
					// A long-lived process may already have loaded this child before
					// the guard, or before its rollout metadata was flushed.
					inst.hookStatus, inst.hookEvent = hs.Status, hs.Event
					inst.hookLastUpdate, inst.hookFingerprint = hs.UpdatedAt, hs.Fingerprint
					inst.hookSessionID = candidateID
					if mode == "rejected-cached-child" {
						inst.UpdateHookStatus(hs)
					}
				}

				// Run the same status/publication sequence more than once: a rejected
				// child must not get another chance through the cached fast path.
				for pass := 0; pass < 2; pass++ {
					var err error
					if statusOnly {
						var statusPass StatusUpdatePass
						err = statusPass.UpdateStatusOnly(inst)
					} else {
						err = inst.UpdateStatus()
					}
					if err != nil {
						t.Fatal(err)
					}
					after, found, err := storage.db.ReadRuntimeBinding(inst.ID, "codex")
					if err != nil || !found {
						t.Fatalf("binding after status pass %d: found=%v err=%v", pass, found, err)
					}
					if mode == "cold-root" {
						if after.Value != candidateID || inst.CodexSessionID != candidateID || after.Revision != before.Revision+1 {
							t.Errorf("root hook did not bind once: memory=%q durable=%+v before=%+v", inst.CodexSessionID, after, before)
						}
					} else {
						if after != before || inst.CodexSessionID != rootID {
							t.Errorf("child hook replaced root after pass %d: memory=%q durable=%+v before=%+v", pass, inst.CodexSessionID, after, before)
						}
						if inst.hookSessionID == candidateID {
							t.Errorf("rejected child remains cached after pass %d", pass)
						}
					}
					if inst.Status != StatusRunning {
						t.Errorf("live parent status changed: %q", inst.Status)
					}
					if mode == "cold-root" && inst.hookLastUpdate != hs.UpdatedAt {
						t.Error("valid root activity was not preserved")
					}
					if mode != "cold-root" && (inst.hookStatus != "" || !inst.hookLastUpdate.IsZero()) {
						t.Error("rejected child activity remains cached")
					}
				}
			})
		}
	}
}

func TestCodexHookPublication_RechecksPreviouslyUnflushedCandidate(t *testing.T) {
	inst, codexHome := newCodexGateInstance(t)
	inst.tmuxSession = nil
	storage := runtimeBindingTestStorage(t)
	if err := storage.InsertSessionAndVerify(inst, nil); err != nil {
		t.Fatal(err)
	}
	childID := uniqueSID(t)
	// Codex may emit a thread's first hooks (thread start, prompt submit)
	// before flushing its rollout. Preserve the existing fail-open behavior for
	// them, including the fingerprint publication cache. A turn end cannot be
	// that early: a real thread's rollout is on disk from its turn start, so a
	// turn end without one is an ephemeral helper (CodexUnbackedTurnEnd).
	hs := &HookStatus{
		Status: "waiting", Event: "thread-started", SessionID: childID,
		UpdatedAt: time.Now(), Fingerprint: HookStatusFingerprint{1},
	}
	inst.UpdateHookStatus(hs)
	if inst.CodexSessionID != childID || inst.validatedHookBindings["codex"].value != childID {
		t.Fatal("unflushed candidate was not accepted and cached")
	}
	before, found, err := storage.db.ReadRuntimeBinding(inst.ID, "codex")
	if err != nil || !found {
		t.Fatalf("accepted binding: found=%v err=%v", found, err)
	}

	// A thread-title helper's turn end names a thread that never writes a
	// rollout. It must neither rebind nor displace the accepted candidate's
	// hook evidence or publication cache.
	inst.UpdateHookStatus(&HookStatus{
		Status: "waiting", Event: "agent-turn-complete", SessionID: uniqueSID(t),
		UpdatedAt: time.Now(), Fingerprint: HookStatusFingerprint{2},
	})
	if inst.CodexSessionID != childID || inst.hookSessionID != childID ||
		inst.hookEvent != hs.Event || inst.hookFingerprint != hs.Fingerprint ||
		inst.validatedHookBindings["codex"].value != childID {
		t.Fatalf("unbacked turn end displaced the accepted candidate: binding=%q hook=%q event=%q",
			inst.CodexSessionID, inst.hookSessionID, inst.hookEvent)
	}
	if current, found, err := storage.db.ReadRuntimeBinding(inst.ID, "codex"); err != nil || !found || current != before {
		t.Fatalf("unbacked turn end changed the durable binding: before=%+v after=%+v found=%v err=%v", before, current, found, err)
	}

	seedCodexRolloutWithMeta(t, codexHome, childID, "subagent", uniqueSID(t), true)
	err = inst.publishHookRuntimeBindingObservation(inst.captureActiveRuntimeBindingObservation(), childID, hs.Fingerprint)
	if err == nil {
		t.Fatal("unchanged fingerprint bypassed newly flushed subagent metadata")
	}
	if inst.hookSessionID != "" {
		t.Errorf("rejected child still cached: %q", inst.hookSessionID)
	}
	if _, ok := inst.validatedHookBindings["codex"]; ok {
		t.Error("rejected child remains in the validated hook cache")
	}
	after, found, err := storage.db.ReadRuntimeBinding(inst.ID, "codex")
	if err != nil || !found || after != before {
		t.Fatalf("rejection changed the existing durable binding: before=%+v after=%+v found=%v err=%v", before, after, found, err)
	}
}
