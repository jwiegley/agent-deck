package statedb

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

func saveRuntimeBindingTestRow(t *testing.T, db *StateDB, id, toolData string) {
	t.Helper()
	if err := db.SaveInstance(&InstanceRow{
		ID: id, Title: id, ProjectPath: "/tmp/" + id, GroupPath: "my-sessions",
		Tool: "pi", Status: "idle", TmuxSession: id + "-g0", CreatedAt: time.Unix(1, 0),
		ToolData: json.RawMessage(toolData),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeLifecycle_FirstSavePreservesStartedRuntime(t *testing.T) {
	db := newRuntimeTestDB(t)
	startedAt := time.Unix(200, 0).UTC()
	detectedAt := time.Unix(190, 0).UTC()
	row := &InstanceRow{
		ID: "new", Title: "new", ProjectPath: "/tmp/new", GroupPath: "my-sessions",
		Tool: "pi", Status: "starting", TmuxSession: "new-g1", TmuxSocketName: "isolated",
		RuntimeGeneration: 1, StatusRevision: 3, LastStartedAt: startedAt,
		RuntimeBindings: map[string]RuntimeBinding{
			"claude": {
				InstanceID: "new", Kind: "claude", Generation: 1, Revision: 4,
				Value: "conversation-g1", DetectedAt: detectedAt,
			},
		},
		CreatedAt: time.Unix(1, 0),
		ToolData:  json.RawMessage(`{"claude_session_id":"stale","notes":"keep"}`),
	}
	if err := db.SaveInstance(row); err != nil {
		t.Fatal(err)
	}

	state, found, err := db.ReadRuntimeState("new")
	if err != nil || !found {
		t.Fatalf("ReadRuntimeState found=%v err=%v", found, err)
	}
	if state.Generation != 1 || state.StatusRevision != 3 || state.TmuxSession != "new-g1" ||
		state.TmuxSocketName != "isolated" || state.Status != "starting" || !state.LastStartedAt.Equal(startedAt) {
		t.Fatalf("first save lost started runtime: %#v", state)
	}
	binding, found, err := db.ReadRuntimeBinding("new", "claude")
	if err != nil || !found {
		t.Fatalf("ReadRuntimeBinding found=%v err=%v", found, err)
	}
	if binding.Generation != 1 || binding.Revision != 4 || binding.Value != "conversation-g1" || !binding.DetectedAt.Equal(detectedAt) {
		t.Fatalf("first save lost started binding: %#v", binding)
	}
	loaded, err := db.LoadInstances()
	if err != nil || len(loaded) != 1 || loaded[0].RuntimeGeneration != 1 || loaded[0].RuntimeBindings["claude"].Revision != 4 {
		t.Fatalf("LoadInstances = %#v, err=%v", loaded, err)
	}
}

func TestRuntimeLifecycle_TransitionBeforeMetadataProjectsOnFirstSave(t *testing.T) {
	db := newRuntimeTestDB(t)
	const incarnation = "pre-parent-transition"
	if _, err := db.EnsureRuntimeStateForNewInstance(RuntimeState{
		InstanceID: "new", TmuxSession: "runtime-g0", TmuxSocketName: "isolated", Status: "idle",
	}, incarnation); err != nil {
		t.Fatal(err)
	}
	next := RuntimeState{
		InstanceID: "new", Generation: 1, TmuxSession: "runtime-g1", TmuxSocketName: "isolated",
		Status: "starting", LastStartedAt: time.Unix(200, 0).UTC(),
	}
	if err := db.CommitRuntimeTransitionWithBindingPlan(0, incarnation, next, []RuntimeBindingTransition{{
		Kind: "claude", ExpectedRevision: 0, NextValue: "conversation", DetectedAt: time.Unix(190, 0).UTC(),
	}}); err != nil {
		t.Fatalf("transition before metadata row: %v", err)
	}
	if err := db.SaveInstance(&InstanceRow{
		ID: "new", Title: "metadata", ProjectPath: "/tmp/new", GroupPath: "my-sessions",
		Incarnation: incarnation,
		Tool:        "claude", Status: "idle", TmuxSession: "runtime-g0", TmuxSocketName: "stale-socket",
		CreatedAt: time.Unix(1, 0), ToolData: json.RawMessage(`{"claude_session_id":"stale","notes":"keep"}`),
	}); err != nil {
		t.Fatal(err)
	}
	state, found, err := db.ReadRuntimeState("new")
	if err != nil || !found || state.Generation != 1 || state.TmuxSession != "runtime-g1" || state.Status != "starting" {
		t.Fatalf("first metadata save changed runtime: state=%#v found=%v err=%v", state, found, err)
	}
	binding, found, err := db.ReadRuntimeBinding("new", "claude")
	if err != nil || !found || binding.Generation != 1 || binding.Revision != 1 || binding.Value != "conversation" {
		t.Fatalf("first metadata save changed binding: binding=%#v found=%v err=%v", binding, found, err)
	}
	var legacyStatus, legacyTmux, legacySocket, raw string
	if err := db.DB().QueryRow(`SELECT status, tmux_session, tmux_socket_name, tool_data FROM instances WHERE id = 'new'`).
		Scan(&legacyStatus, &legacyTmux, &legacySocket, &raw); err != nil {
		t.Fatal(err)
	}
	if legacyStatus != "starting" || legacyTmux != "runtime-g1" || legacySocket != "isolated" {
		t.Fatalf("legacy runtime projection = status %q tmux %q socket %q", legacyStatus, legacyTmux, legacySocket)
	}
	var projected map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &projected); err != nil {
		t.Fatal(err)
	}
	if string(projected["claude_session_id"]) != `"conversation"` || string(projected["notes"]) != `"keep"` {
		t.Fatalf("first metadata projection = %s", raw)
	}
}

func TestRuntimeLifecycle_AuthoritativeTimestampsDistinguishSameSecond(t *testing.T) {
	db := newRuntimeTestDB(t)
	saveRuntimeBindingTestRow(t, db, "one", `{"claude_session_id":"conversation"}`)
	incarnation := runtimeTestIncarnation(t, db, "one")
	base := time.Unix(2_000_000_000, 0).UTC()
	first, second := base.Add(111*time.Nanosecond), base.Add(222*time.Nanosecond)
	if err := db.CommitRuntimeTransitionWithBindingPlan(0, incarnation, RuntimeState{
		InstanceID: "one", Generation: 1, TmuxSession: "runtime-g1", Status: "starting", LastStartedAt: first,
	}, []RuntimeBindingTransition{{
		Kind: "claude", ExpectedRevision: 0, NextValue: "conversation", DetectedAt: first,
	}}); err != nil {
		t.Fatal(err)
	}
	state, _, err := db.ReadRuntimeState("one")
	if err != nil || !state.LastStartedAt.Equal(first) {
		t.Fatalf("first runtime timestamp = %s err=%v", state.LastStartedAt, err)
	}
	binding, _, err := db.ReadRuntimeBinding("one", "claude")
	if err != nil || !binding.DetectedAt.Equal(first) {
		t.Fatalf("first binding timestamp = %s err=%v", binding.DetectedAt, err)
	}
	if err := db.CommitRuntimeTransitionWithBindingPlan(1, incarnation, RuntimeState{
		InstanceID: "one", Generation: 2, TmuxSession: "runtime-g2", Status: "starting", LastStartedAt: second,
	}, []RuntimeBindingTransition{{
		Kind: "claude", ExpectedRevision: 1, NextValue: "conversation", DetectedAt: second,
	}}); err != nil {
		t.Fatal(err)
	}
	state, _, err = db.ReadRuntimeState("one")
	binding, _, bindingErr := db.ReadRuntimeBinding("one", "claude")
	if err != nil || bindingErr != nil || !state.LastStartedAt.Equal(second) || !binding.DetectedAt.Equal(second) {
		t.Fatalf("second timestamps state=%s binding=%s err=%v bindingErr=%v", state.LastStartedAt, binding.DetectedAt, err, bindingErr)
	}
	if state.LastStartedAt.Equal(first) || binding.DetectedAt.Equal(first) {
		t.Fatal("same-second authoritative timestamps collapsed")
	}
	var raw string
	if err := db.DB().QueryRow(`SELECT tool_data FROM instances WHERE id = 'one'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var projected map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &projected); err != nil {
		t.Fatal(err)
	}
	if string(projected["last_started_at"]) != fmt.Sprintf("%d", second.Unix()) ||
		string(projected["claude_detected_at"]) != fmt.Sprintf("%d", second.Unix()) {
		t.Fatalf("legacy timestamp projection = %s", raw)
	}
}

func TestRuntimeLifecycle_BindingPlanRejectsStaleRevisionAtomically(t *testing.T) {
	db := newRuntimeTestDB(t)
	saveRuntimeBindingTestRow(t, db, "one", `{"claude_session_id":"conversation"}`)
	incarnation := runtimeTestIncarnation(t, db, "one")
	next := RuntimeState{InstanceID: "one", Generation: 1, TmuxSession: "one-g1", Status: "starting"}
	err := db.CommitRuntimeTransitionWithBindingPlan(0, incarnation, next, []RuntimeBindingTransition{
		{Kind: "claude", ExpectedRevision: 1, NextValue: "conversation"},
	})
	if !errors.Is(err, ErrBindingRevisionConflict) {
		t.Fatalf("error = %v, want ErrBindingRevisionConflict", err)
	}
	state, _, err := db.ReadRuntimeState("one")
	if err != nil || state.Generation != 0 || state.TmuxSession != "one-g0" {
		t.Fatalf("stale plan changed runtime: %#v err=%v", state, err)
	}
	binding, _, err := db.ReadRuntimeBinding("one", "claude")
	if err != nil || binding.Generation != 0 || binding.Revision != 0 || binding.Value != "conversation" {
		t.Fatalf("stale plan changed binding: %#v err=%v", binding, err)
	}
}

func TestRuntimeLifecycle_BindingPlanCarriesAndReleasesAtomically(t *testing.T) {
	db := newRuntimeTestDB(t)
	saveRuntimeBindingTestRow(t, db, "one", `{"claude_session_id":"claude-g0","codex_session_id":"codex-g0"}`)
	incarnation := runtimeTestIncarnation(t, db, "one")
	detectedAt := time.Unix(300, 0).UTC()
	err := db.CommitRuntimeTransitionWithBindingPlan(0, incarnation,
		RuntimeState{InstanceID: "one", Generation: 1, TmuxSession: "one-g1", Status: "starting"},
		[]RuntimeBindingTransition{
			{Kind: "claude", ExpectedRevision: 0, NextValue: "claude-g0", DetectedAt: detectedAt},
			{Kind: "codex", ExpectedRevision: 0, NextValue: ""},
		})
	if err != nil {
		t.Fatal(err)
	}
	for kind, wantValue := range map[string]string{"claude": "claude-g0", "codex": ""} {
		binding, found, err := db.ReadRuntimeBinding("one", kind)
		if err != nil || !found || binding.Generation != 1 || binding.Revision != 1 || binding.Value != wantValue {
			t.Fatalf("%s binding = %#v found=%v err=%v", kind, binding, found, err)
		}
	}
	var raw string
	if err := db.DB().QueryRow(`SELECT tool_data FROM instances WHERE id = 'one'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var projected map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &projected); err != nil {
		t.Fatal(err)
	}
	if string(projected["claude_session_id"]) != `"claude-g0"` {
		t.Fatalf("claude projection = %s", raw)
	}
	if _, exists := projected["codex_session_id"]; exists {
		t.Fatalf("released codex binding remained projected: %s", raw)
	}
}

func TestRuntimeLifecycle_BindingPlanFailureRollsBackPriorRelease(t *testing.T) {
	db := newRuntimeTestDB(t)
	saveRuntimeBindingTestRow(t, db, "one", `{"claude_session_id":"claude-one","codex_session_id":"codex-one"}`)
	saveRuntimeBindingTestRow(t, db, "two", `{"codex_session_id":"codex-two"}`)
	incarnation := runtimeTestIncarnation(t, db, "one")
	err := db.CommitRuntimeTransitionWithBindingPlan(0, incarnation,
		RuntimeState{InstanceID: "one", Generation: 1, TmuxSession: "one-g1", Status: "starting"},
		[]RuntimeBindingTransition{
			{Kind: "claude", ExpectedRevision: 0, NextValue: ""},
			{Kind: "codex", ExpectedRevision: 0, NextValue: "codex-two"},
		})
	if err == nil {
		t.Fatal("binding ownership conflict unexpectedly committed")
	}
	state, _, err := db.ReadRuntimeState("one")
	if err != nil || state.Generation != 0 || state.TmuxSession != "one-g0" {
		t.Fatalf("failed plan changed runtime: %#v err=%v", state, err)
	}
	claude, _, err := db.ReadRuntimeBinding("one", "claude")
	if err != nil || claude.Generation != 0 || claude.Revision != 0 || claude.Value != "claude-one" {
		t.Fatalf("failed plan did not roll back release: %#v err=%v", claude, err)
	}
}

func TestRuntimeLifecycle_BindingPlanRejectsImplicitCarry(t *testing.T) {
	db := newRuntimeTestDB(t)
	saveRuntimeBindingTestRow(t, db, "one", `{"claude_session_id":"conversation"}`)
	incarnation := runtimeTestIncarnation(t, db, "one")
	err := db.CommitRuntimeTransitionWithBindingPlan(0, incarnation,
		RuntimeState{InstanceID: "one", Generation: 1, TmuxSession: "one-g1", Status: "starting"}, nil)
	if !errors.Is(err, ErrBindingPlanIncomplete) {
		t.Fatalf("error = %v, want ErrBindingPlanIncomplete", err)
	}
	err = db.CommitRuntimeTransition(0, incarnation,
		RuntimeState{InstanceID: "one", Generation: 1, TmuxSession: "one-g1", Status: "starting"})
	if !errors.Is(err, ErrBindingPlanIncomplete) {
		t.Fatalf("no-plan compatibility API error = %v, want ErrBindingPlanIncomplete", err)
	}
}

func TestRuntimeLifecycle_LoadProjectionStripsAbsentBindings(t *testing.T) {
	raw := json.RawMessage(`{
		"notes":"keep",
		"claude_session_id":"stale","claude_detected_at":1,
		"copilot_session_id":"stale","copilot_detected_at":2,
		"codex_session_id":"stale","codex_detected_at":3,
		"gemini_session_id":"stale","gemini_detected_at":4,
		"opencode_session_id":"stale","opencode_detected_at":5
	}`)
	projected := overlayBindings(raw, nil)
	var values map[string]json.RawMessage
	if err := json.Unmarshal(projected, &values); err != nil {
		t.Fatal(err)
	}
	if string(values["notes"]) != `"keep"` {
		t.Fatalf("unrelated metadata changed: %s", projected)
	}
	for kind, key := range bindingJSONKeys {
		if _, found := values[key]; found {
			t.Errorf("stale %s binding survived: %s", kind, projected)
		}
		if _, found := values[kind+"_detected_at"]; found {
			t.Errorf("stale %s detection time survived: %s", kind, projected)
		}
	}

	bindings := make(map[string]RuntimeBinding, len(bindingKinds))
	for index, kind := range bindingKinds {
		bindings[kind] = RuntimeBinding{
			Kind: kind, Generation: 4, Revision: 2, Value: "current-" + kind,
			DetectedAt: time.Unix(int64(100+index), 0).UTC(),
		}
	}
	projected = overlayBindings(raw, bindings)
	if err := json.Unmarshal(projected, &values); err != nil {
		t.Fatal(err)
	}
	for index, kind := range bindingKinds {
		key := bindingJSONKeys[kind]
		if string(values[key]) != `"current-`+kind+`"` {
			t.Errorf("%s binding projection = %s", kind, projected)
		}
		if string(values[kind+"_detected_at"]) != fmt.Sprintf("%d", 100+index) {
			t.Errorf("%s detection projection = %s", kind, projected)
		}
	}
}

func TestRuntimeLifecycle_RuntimeCASRejectsLiveLegacyWriter(t *testing.T) {
	db := newRuntimeTestDB(t)
	saveRuntimeBindingTestRow(t, db, "one", `{}`)
	incarnation := runtimeTestIncarnation(t, db, "one")
	legacyPID := os.Getpid()
	// Model a distinct, still-live legacy writer. A row using db.pid is the
	// pre-registration residue of an earlier process lifetime and is ignored.
	if db.pid == legacyPID {
		db.pid = legacyPID + 1
	}
	now := time.Now().Unix()
	if _, err := db.DB().Exec(`INSERT INTO instance_heartbeats
		(pid, started, heartbeat, is_primary, writer_schema_version)
		VALUES (?, ?, ?, 0, 0)`, legacyPID, now, now-3600); err != nil {
		t.Fatal(err)
	}

	if err := db.CommitRuntimeTransition(0, incarnation, RuntimeState{
		InstanceID: "one", Generation: 1, TmuxSession: "one-g1", Status: "starting",
	}); !errors.Is(err, ErrIncompatibleWriterSchema) {
		t.Fatalf("transition error = %v, want ErrIncompatibleWriterSchema", err)
	}
	if _, err := db.WriteStatusIfVersion("one", incarnation, 0, 0, "running"); !errors.Is(err, ErrIncompatibleWriterSchema) {
		t.Fatalf("status error = %v, want ErrIncompatibleWriterSchema", err)
	}
	if err := db.WriteStatus("one", "running", "pi"); !errors.Is(err, ErrIncompatibleWriterSchema) {
		t.Fatalf("compatibility status error = %v, want ErrIncompatibleWriterSchema", err)
	}
	if _, err := db.CommitRuntimeBinding("one", incarnation, 0, "claude", 0, "conversation"); !errors.Is(err, ErrIncompatibleWriterSchema) {
		t.Fatalf("binding error = %v, want ErrIncompatibleWriterSchema", err)
	}

	state, found, err := db.ReadRuntimeState("one")
	if err != nil || !found || state.Generation != 0 || state.Status != "idle" {
		t.Fatalf("fenced mutations changed runtime: state=%#v found=%v err=%v", state, found, err)
	}
	if _, found, err := db.ReadRuntimeBinding("one", "claude"); err != nil || found {
		t.Fatalf("fenced binding found=%v err=%v", found, err)
	}
}

func TestRuntimeLifecycle_WriteStatusCompatibilityTransaction(t *testing.T) {
	db := newRuntimeTestDB(t)
	saveRuntimeBindingTestRow(t, db, "one", `{}`)
	if err := db.WriteStatus("one", "running", "shell"); err != nil {
		t.Fatal(err)
	}
	state, found, err := db.ReadRuntimeState("one")
	if err != nil || !found || state.Status != "running" || state.StatusRevision != 1 {
		t.Fatalf("runtime state = %#v found=%v err=%v", state, found, err)
	}
	var legacyStatus, legacyTool string
	if err := db.DB().QueryRow(`SELECT status, tool FROM instances WHERE id = 'one'`).
		Scan(&legacyStatus, &legacyTool); err != nil {
		t.Fatal(err)
	}
	if legacyStatus != "running" || legacyTool != "shell" {
		t.Fatalf("legacy projection = status %q tool %q", legacyStatus, legacyTool)
	}
	if _, err := db.DB().Exec(`UPDATE instance_runtime_state SET status = ? WHERE instance_id = 'one'`, runtimeDestructionStatus); err != nil {
		t.Fatal(err)
	}
	if err := db.WriteStatus("one", "waiting", "pi"); !errors.Is(err, ErrStatusRevisionConflict) {
		t.Fatalf("destruction overwrite error = %v, want ErrStatusRevisionConflict", err)
	}
	state, _, err = db.ReadRuntimeState("one")
	if err != nil || state.Status != runtimeDestructionStatus || state.StatusRevision != 1 {
		t.Fatalf("destruction sentinel changed: %#v err=%v", state, err)
	}
}

func TestRuntimeLifecycle_BindingCASRejectsOldGenerationRow(t *testing.T) {
	db := newRuntimeTestDB(t)
	saveRuntimeBindingTestRow(t, db, "one", `{"claude_session_id":"old"}`)
	incarnation := runtimeTestIncarnation(t, db, "one")
	if _, err := db.DB().Exec(`UPDATE instance_runtime_state SET runtime_generation = 1 WHERE instance_id = 'one'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CommitRuntimeBinding("one", incarnation, 1, "claude", 0, "resurrected"); !errors.Is(err, ErrBindingRevisionConflict) {
		t.Fatalf("binding error = %v, want ErrBindingRevisionConflict", err)
	}
	binding, found, err := db.ReadRuntimeBinding("one", "claude")
	if err != nil || !found || binding.Generation != 0 || binding.Value != "old" {
		t.Fatalf("old-generation binding changed: binding=%#v found=%v err=%v", binding, found, err)
	}
}
