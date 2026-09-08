package statedb

import (
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"
)

func TestRuntimeLifecycle_Migrate_InstanceIncarnationBackfillsLegacyParentsIdempotently(t *testing.T) {
	db := createV1SchemaDB(t)
	if _, err := db.DB().Exec(`
		INSERT INTO instances (id, title, project_path, group_path, sort_order, tool, status, created_at, tool_data)
		VALUES ('existing-2', 'Second Session', '/home/user/second', 'conductor', 1, 'pi', 'idle', ?, '{}')
	`, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}

	read := func() map[string]string {
		t.Helper()
		rows, err := db.DB().Query(`SELECT instance_id, incarnation FROM instance_incarnation ORDER BY instance_id`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		got := make(map[string]string)
		for rows.Next() {
			var id, incarnation string
			if err := rows.Scan(&id, &incarnation); err != nil {
				t.Fatal(err)
			}
			got[id] = incarnation
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return got
	}
	first := read()
	if len(first) != 2 || first["existing-1"] == "" || first["existing-2"] == "" ||
		first["existing-1"] == first["existing-2"] {
		t.Fatalf("legacy incarnations = %#v, want two distinct non-empty values", first)
	}
	if version, err := db.GetMeta("schema_version"); err != nil || version != strconv.Itoa(SchemaVersion) {
		t.Fatalf("schema version = %q err=%v, want %d", version, err, SchemaVersion)
	}
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	second := read()
	for id, incarnation := range first {
		if second[id] != incarnation {
			t.Fatalf("idempotent migration changed %s incarnation: %q -> %q", id, incarnation, second[id])
		}
	}
}

func TestRuntimeLifecycle_InstanceIncarnation_LegacyReplaceRotatesAndLegacyDeleteCleans(t *testing.T) {
	db := newRuntimeTestDB(t)
	row := &InstanceRow{
		ID: "legacy-writer", Title: "initial", ProjectPath: "/tmp/legacy",
		GroupPath: "my-sessions", Tool: "pi", Status: "idle",
		CreatedAt: time.Unix(100, 0).UTC(), ToolData: json.RawMessage(`{"notes":"initial"}`),
	}
	if err := db.SaveInstance(row); err != nil {
		t.Fatal(err)
	}
	first := row.Incarnation
	if first == "" {
		t.Fatal("SaveInstance did not capture an incarnation")
	}
	row.Title = "current upsert"
	if err := db.SaveInstance(row); err != nil {
		t.Fatal(err)
	}
	if row.Incarnation != first {
		t.Fatalf("ON CONFLICT update changed incarnation: %q -> %q", first, row.Incarnation)
	}

	if _, err := db.DB().Exec(`
		INSERT OR REPLACE INTO instances (
			id, title, project_path, group_path, sort_order,
			command, wrapper, tool, status, tmux_session, tmux_socket_name,
			created_at, last_accessed, parent_session_id, is_conductor,
			no_transition_notify, worktree_path, worktree_repo, worktree_branch,
			account, archived_at, tool_data, title_locked, auto_name,
			auto_name_description, pin
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.ID, "legacy replace", row.ProjectPath, row.GroupPath, 0,
		"", "", row.Tool, row.Status, "", "", row.CreatedAt.Unix(), 0,
		"", 0, 0, "", "", "", "", 0, `{}`, 0, 0, "", "",
	); err != nil {
		t.Fatal(err)
	}
	replaced, err := db.LoadInstanceByID(row.ID)
	if err != nil || replaced == nil || replaced.Incarnation == "" {
		t.Fatalf("load legacy replacement: row=%#v err=%v", replaced, err)
	}
	if replaced.Incarnation == first {
		t.Fatalf("legacy INSERT OR REPLACE preserved stale incarnation %q", first)
	}

	if _, err := db.DB().Exec(`DELETE FROM instances WHERE id = ?`, row.ID); err != nil {
		t.Fatal(err)
	}
	var tokens int
	if err := db.DB().QueryRow(`SELECT COUNT(*) FROM instance_incarnation WHERE instance_id = ?`, row.ID).Scan(&tokens); err != nil {
		t.Fatal(err)
	}
	if tokens != 0 {
		t.Fatalf("legacy DELETE left %d incarnation rows", tokens)
	}
}

func TestRuntimeLifecycle_GenericUpsertPreservesPreMintedIncarnation(t *testing.T) {
	db := newRuntimeTestDB(t)
	const incarnation = "pre-minted-generic-upsert"
	row := &InstanceRow{
		ID: "generic-upsert", Incarnation: incarnation, Title: "initial",
		ProjectPath: "/tmp/generic-upsert", GroupPath: "my-sessions",
		Tool: "pi", Status: "idle", CreatedAt: time.Unix(100, 0).UTC(),
	}
	if err := db.UpsertInstances([]*InstanceRow{row}); err != nil {
		t.Fatal(err)
	}
	if row.Incarnation != incarnation {
		t.Fatalf("inserted row incarnation = %q, want %q", row.Incarnation, incarnation)
	}
	loaded, err := db.LoadInstanceByID(row.ID)
	if err != nil || loaded == nil || loaded.Incarnation != incarnation {
		t.Fatalf("loaded inserted row = %#v err=%v, want incarnation %q", loaded, err, incarnation)
	}

	update := *row
	update.Title = "updated"
	update.Incarnation = "stale-caller-token"
	if err := db.UpsertInstances([]*InstanceRow{&update}); err != nil {
		t.Fatal(err)
	}
	if update.Incarnation != incarnation {
		t.Fatalf("existing-row upsert adopted caller token %q, want durable %q", update.Incarnation, incarnation)
	}
	loaded, err = db.LoadInstanceByID(row.ID)
	if err != nil || loaded == nil || loaded.Incarnation != incarnation || loaded.Title != "updated" {
		t.Fatalf("loaded updated row = %#v err=%v", loaded, err)
	}
}

func TestRuntimeLifecycle_GenericUpsertRequiresReservedPreParentIncarnation(t *testing.T) {
	const (
		instanceID = "reserved-pre-parent"
		reserved   = "reserved-pre-parent-incarnation"
	)
	initial := RuntimeState{
		InstanceID: instanceID, Generation: 1, StatusRevision: 2,
		TmuxSession: "reserved-runtime", TmuxSocketName: "isolated",
		Status: "starting", LastStartedAt: time.Unix(200, 123).UTC(),
	}

	for _, test := range []struct {
		name         string
		incarnation  string
		wantConflict bool
	}{
		{name: "exact token creates parent", incarnation: reserved},
		{name: "different token is rejected", incarnation: "competing-incarnation", wantConflict: true},
		{name: "blank token is rejected", wantConflict: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := newRuntimeTestDB(t)
			if got, err := db.EnsureRuntimeStateForNewInstance(initial, reserved); err != nil || got != initial {
				t.Fatalf("reserve pre-parent runtime = %#v err=%v, want %#v", got, err, initial)
			}
			row := &InstanceRow{
				ID: instanceID, Incarnation: test.incarnation, Title: "candidate",
				ProjectPath: "/tmp/reserved-pre-parent", GroupPath: "my-sessions",
				Tool: "pi", Status: initial.Status, TmuxSession: initial.TmuxSession,
				TmuxSocketName: initial.TmuxSocketName, RuntimeGeneration: initial.Generation,
				StatusRevision: initial.StatusRevision, LastStartedAt: initial.LastStartedAt,
				CreatedAt: time.Unix(100, 0).UTC(),
			}

			err := db.UpsertInstances([]*InstanceRow{row})
			if test.wantConflict {
				if !errors.Is(err, ErrInstanceParentConflict) {
					t.Fatalf("UpsertInstances error = %v, want parent conflict", err)
				}
				if parent, loadErr := db.LoadInstanceByID(instanceID); loadErr != nil || parent != nil {
					t.Fatalf("rejected contender created parent=%#v err=%v", parent, loadErr)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				parent, loadErr := db.LoadInstanceByID(instanceID)
				if loadErr != nil || parent == nil || parent.Incarnation != reserved {
					t.Fatalf("created parent=%#v err=%v, want incarnation %q", parent, loadErr, reserved)
				}
			}

			durable, found, readErr := db.ReadRuntimeState(instanceID)
			if readErr != nil || !found || durable != initial {
				t.Fatalf("reserved runtime=%#v found=%v err=%v, want unchanged %#v", durable, found, readErr, initial)
			}
			var durableIncarnation string
			if queryErr := db.DB().QueryRow(`SELECT incarnation FROM instance_incarnation WHERE instance_id = ?`, instanceID).
				Scan(&durableIncarnation); queryErr != nil || durableIncarnation != reserved {
				t.Fatalf("durable incarnation=%q err=%v, want %q", durableIncarnation, queryErr, reserved)
			}
		})
	}
}
