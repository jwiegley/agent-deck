package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

// Upstream's Codex and Gemini hook binders wrote their binding with
// Write{Codex,Gemini}SessionBinding, which also recorded the authoritative
// recall link in session_links ("Recall phase 1"). The fork publishes hook
// bindings through the runtime binding CAS instead, so confirmHookSessionLink
// records the link once the publish succeeds. Without it a hook-bound Codex or
// Gemini identity never reached the recall index.

// newHookRecallLinkInstance seeds one hook-bound instance whose durable
// tool_data carries toolData, with no tmux handle: these tests exercise only
// the durable binding and its link.
func newHookRecallLinkInstance(t *testing.T, db *statedb.StateDB, title, tool, toolData string) *Instance {
	t.Helper()
	projectPath := filepath.Join(os.Getenv("HOME"), "project")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	inst := NewInstanceWithTool(title, projectPath, tool)
	inst.tmuxSession = nil
	saveHookBindingTestInstance(t, db, inst, &statedb.InstanceRow{
		ID: inst.ID, Title: inst.Title, ProjectPath: inst.ProjectPath, GroupPath: inst.GroupPath,
		Command: inst.Command, Tool: tool, Status: "idle", CreatedAt: time.Now(),
		ToolData: json.RawMessage(toolData),
	})
	return inst
}

func isolateHookRecallLinkHome(t *testing.T) *statedb.StateDB {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	ClearUserConfigCache()
	t.Cleanup(ClearUserConfigCache)
	return withTempGlobalStateDB(t)
}

// requireRecallLinks fails unless the instance's session_links rows are
// exactly want (native id -> authoritative) for harness.
func requireRecallLinks(t *testing.T, db *statedb.StateDB, instanceID, harness string, want map[string]bool) {
	t.Helper()
	links, err := db.ListSessionLinks(instanceID)
	if err != nil {
		t.Fatalf("ListSessionLinks: %v", err)
	}
	got := make(map[string]bool, len(links))
	for _, link := range links {
		if link.Harness != harness {
			t.Fatalf("link %+v has harness %q, want %q", link, link.Harness, harness)
		}
		got[link.NativeID] = link.Authoritative
	}
	if len(got) != len(want) {
		t.Fatalf("links = %+v, want %v", links, want)
	}
	for id, authoritative := range want {
		if a, ok := got[id]; !ok || a != authoritative {
			t.Fatalf("links = %+v, want %v", links, want)
		}
	}
}

func TestCodexHookBindAndRebindWriteRecallLink(t *testing.T) {
	db := isolateHookRecallLinkHome(t)
	inst := newHookRecallLinkInstance(t, db, "hook-codex-recall-link", "codex", `{}`)
	const first = "7a3b9c10-0000-0000-0000-000000000d01"
	const second = "7a3b9c10-0000-0000-0000-000000000d02"

	inst.UpdateHookStatus(&HookStatus{Status: "running", SessionID: first, Event: "SessionStart", UpdatedAt: time.Now()})
	if inst.CodexSessionID != first {
		t.Fatalf("cold start did not bind %s (got %q)", first, inst.CodexSessionID)
	}
	requireRecallLinks(t, db, inst.ID, "codex", map[string]bool{first: true})

	inst.UpdateHookStatus(&HookStatus{Status: "running", SessionID: second, Event: "UserPromptSubmit", UpdatedAt: time.Now().Add(time.Second)})
	if inst.CodexSessionID != second {
		t.Fatalf("rebind did not bind %s (got %q)", second, inst.CodexSessionID)
	}
	requireRecallLinks(t, db, inst.ID, "codex", map[string]bool{first: false, second: true})
}

func TestGeminiHookBindAndRebindWriteRecallLink(t *testing.T) {
	db := isolateHookRecallLinkHome(t)
	inst := newHookRecallLinkInstance(t, db, "hook-gemini-recall-link", "gemini", `{}`)
	const first = "5ea244ce-0000-0000-0000-000000000e01"
	const second = "2266314c-0000-0000-0000-000000000e02"

	inst.UpdateHookStatus(&HookStatus{Status: "running", SessionID: first, Event: "SessionStart", UpdatedAt: time.Now()})
	if inst.GeminiSessionID != first {
		t.Fatalf("cold start did not bind %s (got %q)", first, inst.GeminiSessionID)
	}
	requireRecallLinks(t, db, inst.ID, "gemini", map[string]bool{first: true})

	// The Gemini rebind gate needs the candidate's conversation on disk.
	seedGeminiSessionFile(t, inst.ProjectPath, second)
	inst.UpdateHookStatus(&HookStatus{Status: "running", SessionID: second, Event: "UserPromptSubmit", UpdatedAt: time.Now().Add(time.Second)})
	if inst.GeminiSessionID != second {
		t.Fatalf("rebind did not bind %s (got %q)", second, inst.GeminiSessionID)
	}
	requireRecallLinks(t, db, inst.ID, "gemini", map[string]bool{first: false, second: true})
}

// The "already linked" mark must name the binding it was set for, not a bare
// id. A peer (the web server, a CLI status refresh) that rebinds A to B links
// B and demotes A. This process adopts B when its own publish of B loses the
// CAS, and when the hook returns to A (codex resume) its rebind must link A
// again: the mark its first link of A left says nothing about the link of the
// A bound now. Trusting it left the binding at A and the authoritative link
// at B, and every later equality tick trusted it too.
func TestHookRebindBackToAPeerDemotedIdRelinksIt(t *testing.T) {
	db := isolateHookRecallLinkHome(t)
	inst := newHookRecallLinkInstance(t, db, "hook-codex-relink", "codex", `{}`)
	const a = "7a3b9c10-0000-0000-0000-000000000b01"
	const b = "7a3b9c10-0000-0000-0000-000000000b02"
	at := time.Now()
	hook := func(id, event string) {
		t.Helper()
		at = at.Add(time.Second)
		inst.UpdateHookStatus(&HookStatus{Status: "running", SessionID: id, Event: event, UpdatedAt: at})
	}

	hook(a, "SessionStart")
	requireRecallLinks(t, db, inst.ID, "codex", map[string]bool{a: true})

	// The peer wins the hook rebind to B and links it.
	durable, found, err := db.ReadRuntimeBinding(inst.ID, "codex")
	if err != nil || !found || durable.Value != a {
		t.Fatalf("durable codex binding = %+v found=%v err=%v, want %s", durable, found, err, a)
	}
	incarnation := inst.PersistenceIncarnation()
	if _, err := db.CommitRuntimeBinding(inst.ID, incarnation, durable.Generation, "codex", durable.Revision, b); err != nil {
		t.Fatalf("peer rebind to %s: %v", b, err)
	}
	if linked, err := db.LinkRuntimeBinding(inst.ID, incarnation, "codex", b); err != nil || !linked {
		t.Fatalf("peer link of %s = %v, %v", b, linked, err)
	}
	requireRecallLinks(t, db, inst.ID, "codex", map[string]bool{a: false, b: true})

	// This process sees the same hook, loses the CAS and adopts B.
	hook(b, "UserPromptSubmit")
	if inst.CodexSessionID != b {
		t.Fatalf("losing publisher did not adopt the peer's %s (got %q)", b, inst.CodexSessionID)
	}

	hook(a, "UserPromptSubmit")
	if inst.CodexSessionID != a || readCodexSessionIDFromDB(t, db, inst.ID) != a {
		t.Fatalf("rebind back = memory %q durable %q, want %s",
			inst.CodexSessionID, readCodexSessionIDFromDB(t, db, inst.ID), a)
	}
	requireRecallLinks(t, db, inst.ID, "codex", map[string]bool{a: true, b: false})

	// The equality ticks that follow keep it that way.
	hook(a, "Stop")
	requireRecallLinks(t, db, inst.ID, "codex", map[string]bool{a: true, b: false})
}

// An identity bound by some other path (here the legacy tool_data a row was
// saved with) reaches its first hook already equal. As for a minted Claude id,
// that hook's vouch is what records the missing link.
func TestHookConfirmingABoundIdentityWritesItsRecallLink(t *testing.T) {
	for _, tc := range []struct {
		tool, key, id string
		bound         func(*Instance) string
	}{
		{"codex", "codex_session_id", "5ea244ce-0000-0000-0000-000000000f01", func(i *Instance) string { return i.CodexSessionID }},
		{"gemini", "gemini_session_id", "5ea244ce-0000-0000-0000-000000000f02", func(i *Instance) string { return i.GeminiSessionID }},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			db := isolateHookRecallLinkHome(t)
			inst := newHookRecallLinkInstance(t, db, "hook-confirm-"+tc.tool, tc.tool, `{"`+tc.key+`":"`+tc.id+`"}`)
			if tc.tool == "codex" {
				inst.CodexSessionID = tc.id
			} else {
				inst.GeminiSessionID = tc.id
			}
			requireRecallLinks(t, db, inst.ID, tc.tool, map[string]bool{})

			for n, event := range []string{"SessionStart", "UserPromptSubmit"} {
				inst.UpdateHookStatus(&HookStatus{Status: "running", SessionID: tc.id, Event: event, UpdatedAt: time.Now().Add(time.Duration(n) * time.Second)})
			}
			if got := tc.bound(inst); got != tc.id {
				t.Fatalf("bound id = %q, want %q", got, tc.id)
			}
			requireRecallLinks(t, db, inst.ID, tc.tool, map[string]bool{tc.id: true})
		})
	}
}

// The Claude candidate retraction clears its link mark both inside
// UpdateHookStatus, which holds i.mu, and from UpdateClaudeSession, which
// does not, while hooks set marks under i.mu. The marks must therefore be
// safe to touch without the instance lock (run under -race).
func TestRecallLinkMarksTolerateRetractionOutsideTheInstanceLock(t *testing.T) {
	db := isolateHookRecallLinkHome(t)
	const bound = "11111111-1111-4111-8111-11111111aa01"
	const candidate = "22222222-2222-4222-8222-22222222aa02"
	inst := newHookRecallLinkInstance(t, db, "hook-claude-link-marks", "claude", `{"claude_session_id":"`+bound+`"}`)
	inst.ClaudeSessionID = bound
	// A hook vouching for the bound id gives it the binding version the
	// marks are keyed on; without one, confirmHookSessionLink never marks.
	inst.UpdateHookStatus(&HookStatus{Status: "running", SessionID: bound, Event: "SessionStart", UpdatedAt: time.Now()})
	inst.mu.RLock()
	version, versioned := inst.recallLinkVersionLocked("claude", bound)
	inst.mu.RUnlock()
	if !versioned {
		t.Fatalf("hook left no in-memory binding version for %s: %+v", bound, inst.RuntimeBindings)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // UpdateClaudeSession's zombie rejection: no i.mu
		defer wg.Done()
		for n := 0; n < 100; n++ {
			inst.retractClaudeCandidateLink(candidate)
		}
	}()
	go func() { // UpdateHookStatus: marks set and cleared under i.mu
		defer wg.Done()
		for n := 0; n < 100; n++ {
			inst.mu.Lock()
			inst.retractClaudeCandidateLink(bound)
			inst.confirmHookSessionLink("claude", bound, "hook_payload")
			inst.mu.Unlock()
		}
	}()
	wg.Wait()
	requireRecallLinks(t, db, inst.ID, "claude", map[string]bool{bound: true})
	if !inst.linkedBindings.has("claude", version) {
		t.Fatalf("the last confirmation of %s left no mark", bound)
	}
}

// A link write that fails leaves the published binding in place. The next
// hook confirming that id retries the link instead of trusting a marker set
// for a write that never landed.
func TestFailedHookRecallLinkIsRetriedByTheNextHook(t *testing.T) {
	db := isolateHookRecallLinkHome(t)
	inst := newHookRecallLinkInstance(t, db, "hook-codex-link-retry", "codex", `{}`)
	const id = "7a3b9c10-0000-0000-0000-000000000a11"
	if _, err := db.DB().Exec(`CREATE TRIGGER hook_link_test_fail BEFORE INSERT ON session_links
		BEGIN SELECT RAISE(ABORT, 'injected recall link failure'); END`); err != nil {
		t.Fatal(err)
	}

	inst.UpdateHookStatus(&HookStatus{Status: "running", SessionID: id, Event: "SessionStart", UpdatedAt: time.Now()})
	if inst.CodexSessionID != id || readCodexSessionIDFromDB(t, db, inst.ID) != id {
		t.Fatalf("binding = memory %q durable %q, want %q published despite the link failure",
			inst.CodexSessionID, readCodexSessionIDFromDB(t, db, inst.ID), id)
	}
	requireRecallLinks(t, db, inst.ID, "codex", map[string]bool{})

	if _, err := db.DB().Exec(`DROP TRIGGER hook_link_test_fail`); err != nil {
		t.Fatal(err)
	}
	inst.UpdateHookStatus(&HookStatus{Status: "running", SessionID: id, Event: "UserPromptSubmit", UpdatedAt: time.Now().Add(time.Second)})
	requireRecallLinks(t, db, inst.ID, "codex", map[string]bool{id: true})
}
