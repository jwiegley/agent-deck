package main

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

// archivedFlag parses `list --json` and returns the archived flag for the
// session with the given id. Fails the test if the id is absent.
func archivedFlag(t *testing.T, home, id string) bool {
	t.Helper()
	listJSON := readSessionsJSON(t, home)
	var sessions []struct {
		ID       string `json:"id"`
		Archived bool   `json:"archived"`
	}
	if err := json.Unmarshal([]byte(listJSON), &sessions); err != nil {
		t.Fatalf("parse list --json: %v\njson: %s", err, listJSON)
	}
	for _, s := range sessions {
		if s.ID == id {
			return s.Archived
		}
	}
	t.Fatalf("session %s not found in list; json:\n%s", id, listJSON)
	return false
}

// TestSessionArchive_MarksArchived is the happy path: archiving a stopped
// session flags it archived without removing it from the registry.
func TestSessionArchive_MarksArchived(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess CLI test skipped in short mode")
	}
	home := t.TempDir()
	workPath := filepath.Join(home, "proj")
	id := addTestSession(t, home, workPath, "archive-basic")

	if archivedFlag(t, home, id) {
		t.Fatalf("session %s archived before archive command ran", id)
	}

	stdout, stderr, code := runAgentDeck(t, home, "session", "archive", id, "--json")
	if code != 0 {
		t.Fatalf("session archive failed (exit %d)\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !archivedFlag(t, home, id) {
		t.Errorf("session %s not archived after archive command", id)
	}
}

// TestSessionUnarchive_ClearsArchived confirms unarchive reverses archive.
func TestSessionUnarchive_ClearsArchived(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess CLI test skipped in short mode")
	}
	home := t.TempDir()
	workPath := filepath.Join(home, "proj")
	id := addTestSession(t, home, workPath, "unarchive-basic")

	if _, stderr, code := runAgentDeck(t, home, "session", "archive", id, "--json"); code != 0 {
		t.Fatalf("archive setup failed (exit %d): %s", code, stderr)
	}
	if !archivedFlag(t, home, id) {
		t.Fatalf("archive setup did not take effect for %s", id)
	}

	stdout, stderr, code := runAgentDeck(t, home, "session", "unarchive", id, "--json")
	if code != 0 {
		t.Fatalf("session unarchive failed (exit %d)\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if archivedFlag(t, home, id) {
		t.Errorf("session %s still archived after unarchive command", id)
	}
}

// TestSessionArchive_NotFound_Exit2 mirrors other resolve-by-id commands:
// an unknown session id exits 2.
func TestSessionArchive_NotFound_Exit2(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess CLI test skipped in short mode")
	}
	home := t.TempDir()
	// Seed one real session so storage exists but the target id is absent.
	addTestSession(t, home, filepath.Join(home, "proj"), "archive-notfound")

	_, _, code := runAgentDeck(t, home, "session", "archive", "does-not-exist", "--json")
	if code != 2 {
		t.Fatalf("expected exit 2 for unknown session, got %d", code)
	}
}

// TestSessionUnarchive_NotArchived_Rejected: unarchiving a session that is not
// archived is an error (mirrors WebMutator.UnarchiveSession).
func TestSessionUnarchive_NotArchived_Rejected(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess CLI test skipped in short mode")
	}
	home := t.TempDir()
	id := addTestSession(t, home, filepath.Join(home, "proj"), "unarchive-noop")

	_, _, code := runAgentDeck(t, home, "session", "unarchive", id, "--json")
	if code != 1 {
		t.Fatalf("expected exit 1 (INVALID_OPERATION) unarchiving a non-archived session, got %d", code)
	}
}

// TestSessionArchive_AlreadyArchived_Rejected: archiving twice is an error so
// the caller notices the no-op rather than silently re-stamping.
func TestSessionArchive_AlreadyArchived_Rejected(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess CLI test skipped in short mode")
	}
	home := t.TempDir()
	id := addTestSession(t, home, filepath.Join(home, "proj"), "archive-twice")

	if _, stderr, code := runAgentDeck(t, home, "session", "archive", id, "--json"); code != 0 {
		t.Fatalf("first archive failed (exit %d): %s", code, stderr)
	}
	_, _, code := runAgentDeck(t, home, "session", "archive", id, "--json")
	if code != 1 {
		t.Fatalf("expected exit 1 (INVALID_OPERATION) archiving an already-archived session, got %d", code)
	}
}

// A missing <id|title> is a usage error (exit 1), distinct from the NOT_FOUND
// exit 2 reserved for a genuinely unknown session.
func TestSessionArchive_MissingArg_Exit1(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess CLI test skipped in short mode")
	}
	home := t.TempDir()
	addTestSession(t, home, filepath.Join(home, "proj"), "archive-missing-arg")

	_, _, code := runAgentDeck(t, home, "session", "archive", "--json")
	if code != 1 {
		t.Fatalf("expected exit 1 for archive with no id, got %d", code)
	}
}

func TestSessionUnarchive_MissingArg_Exit1(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess CLI test skipped in short mode")
	}
	home := t.TempDir()
	addTestSession(t, home, filepath.Join(home, "proj"), "unarchive-missing-arg")

	_, _, code := runAgentDeck(t, home, "session", "unarchive", "--json")
	if code != 1 {
		t.Fatalf("expected exit 1 for unarchive with no id, got %d", code)
	}
}

func TestRuntimeLifecycle_PersistArchivedCLIRejectsReplacementRuntime(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	session.ClearUserConfigCache()
	t.Cleanup(session.ClearUserConfigCache)

	storage, err := session.NewStorageWithProfile("_test_archive_runtime_fence")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	inst := session.NewInstanceWithGroupAndTool("archive", filepath.Join(home, "project"), "work", "pi")
	if err := storage.InsertSessionAndVerify(inst, nil); err != nil {
		t.Fatal(err)
	}
	killed := statedb.RuntimeState{
		InstanceID: inst.ID, Generation: 1, StatusRevision: 2,
		TmuxSession: "runtime-g1", TmuxSocketName: "isolated", Status: "stopped",
		LastStartedAt: time.Unix(2, 0).UTC(),
	}
	db := storage.GetDB()
	if err := db.CommitRuntimeTransition(0, inst.PersistenceIncarnation(), killed); err != nil {
		t.Fatal(err)
	}
	replacement := killed
	replacement.Generation++
	replacement.StatusRevision = 0
	replacement.TmuxSession = "runtime-g2"
	replacement.Status = "running"
	replacement.LastStartedAt = time.Unix(3, 0).UTC()
	if err := db.CommitRuntimeTransition(
		killed.Generation, inst.PersistenceIncarnation(), replacement); err != nil {
		t.Fatal(err)
	}
	inst.ArchivedAt = time.Unix(4, 0).UTC()
	if err := persistArchivedCLI(
		storage, inst, killed, inst.PersistenceIncarnation()); !errors.Is(err, statedb.ErrRuntimeGenerationConflict) {
		t.Fatalf("stale CLI archive error = %v, want generation conflict", err)
	}
	row, err := db.LoadInstanceByID(inst.ID)
	if err != nil || row == nil || !row.ArchivedAt.IsZero() {
		t.Fatalf("replacement archive row=%#v err=%v", row, err)
	}
}

func TestRuntimeLifecycle_PersistUnarchiveCLIRejectsReplacementIncarnation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	session.ClearUserConfigCache()
	t.Cleanup(session.ClearUserConfigCache)

	storage, err := session.NewStorageWithProfile("_test_unarchive_incarnation_fence")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	inst := session.NewInstanceWithGroupAndTool("unarchive", filepath.Join(home, "project"), "work", "pi")
	if err := storage.InsertSessionAndVerify(inst, nil); err != nil {
		t.Fatal(err)
	}
	db := storage.GetDB()
	archivedAt := time.Unix(4, 0).UTC()
	if err := db.SetArchivedIfIncarnation(inst.ID, inst.PersistenceIncarnation(), archivedAt); err != nil {
		t.Fatal(err)
	}
	replacement, err := db.LoadInstanceByID(inst.ID)
	if err != nil || replacement == nil {
		t.Fatalf("load A: row=%#v err=%v", replacement, err)
	}
	if err := db.DeleteInstance(inst.ID); err != nil {
		t.Fatal(err)
	}
	replacement.Incarnation = "cli-unarchive-replacement"
	replacement.ArchivedAt = archivedAt
	if _, inserted, err := db.InsertInstanceIfAbsent(replacement); err != nil || !inserted {
		t.Fatalf("insert B: inserted=%v err=%v", inserted, err)
	}

	inst.ArchivedAt = time.Time{}
	if err := persistArchivedCLI(
		storage, inst, statedb.RuntimeState{}, inst.PersistenceIncarnation(),
	); !errors.Is(err, statedb.ErrInstanceParentConflict) {
		t.Fatalf("stale CLI unarchive error = %v, want parent conflict", err)
	}
	row, err := db.LoadInstanceByID(inst.ID)
	if err != nil || row == nil || !row.ArchivedAt.Equal(archivedAt) || row.Incarnation != replacement.Incarnation {
		t.Fatalf("replacement unarchive row=%#v err=%v", row, err)
	}
}
