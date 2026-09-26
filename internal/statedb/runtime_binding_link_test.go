package statedb

import (
	"testing"
	"time"
)

// A recall link records the instance's current harness identity, so it may
// only be written for the durable binding that still holds that identity:
// a writer that lost a rebind race must not make its superseded id
// authoritative again, and a replaced incarnation links nothing.
func TestRuntimeLifecycle_RecallLinkFollowsOnlyTheCurrentBinding(t *testing.T) {
	db := newRuntimeTestDB(t)
	if err := db.SaveInstance(&InstanceRow{ID: "one", Title: "one", ProjectPath: "/tmp/one", GroupPath: "my-sessions", Tool: "codex", Status: "idle", CreatedAt: time.Unix(1, 0)}); err != nil {
		t.Fatal(err)
	}
	incarnation := runtimeTestIncarnation(t, db, "one")
	links := func() map[string]bool {
		t.Helper()
		rows, err := db.ListSessionLinks("one")
		if err != nil {
			t.Fatal(err)
		}
		out := make(map[string]bool, len(rows))
		for _, row := range rows {
			if row.Harness != "codex" {
				t.Fatalf("link %+v has harness %q, want codex", row, row.Harness)
			}
			out[row.NativeID] = row.Authoritative
		}
		return out
	}
	link := func(value, withIncarnation string) bool {
		t.Helper()
		linked, err := db.LinkRuntimeBinding("one", withIncarnation, "codex", value)
		if err != nil {
			t.Fatalf("LinkRuntimeBinding(%s): %v", value, err)
		}
		return linked
	}

	if link("first", incarnation) || len(links()) != 0 {
		t.Fatalf("an unbound id was linked: %v", links())
	}
	if _, err := db.CommitRuntimeBinding("one", incarnation, 0, "codex", 0, "first"); err != nil {
		t.Fatal(err)
	}
	if link("first", "replaced-incarnation") || len(links()) != 0 {
		t.Fatalf("a stale incarnation linked the binding: %v", links())
	}
	if !link("first", incarnation) {
		t.Fatal("the current binding was not linked")
	}
	if got := links(); len(got) != 1 || !got["first"] {
		t.Fatalf("links = %v, want first authoritative", got)
	}

	// A peer rebinds before a slower writer records its link for the old id.
	if _, err := db.CommitRuntimeBinding("one", incarnation, 0, "codex", 1, "second"); err != nil {
		t.Fatal(err)
	}
	if !link("second", incarnation) {
		t.Fatal("the rebound binding was not linked")
	}
	if link("first", incarnation) {
		t.Fatal("a superseded id was linked")
	}
	if got := links(); len(got) != 2 || got["first"] || !got["second"] {
		t.Fatalf("links = %v, want second authoritative and first demoted", got)
	}

	// Re-confirming a linked identity is a read: with every write to
	// session_links refused, it still reports the link.
	for _, trigger := range []string{
		`CREATE TRIGGER link_test_no_insert BEFORE INSERT ON session_links
			BEGIN SELECT RAISE(ABORT, 'unexpected link insert'); END`,
		`CREATE TRIGGER link_test_no_update BEFORE UPDATE ON session_links
			BEGIN SELECT RAISE(ABORT, 'unexpected link update'); END`,
	} {
		if _, err := db.DB().Exec(trigger); err != nil {
			t.Fatal(err)
		}
	}
	if !link("second", incarnation) {
		t.Fatal("an existing authoritative link was not reported")
	}
}
