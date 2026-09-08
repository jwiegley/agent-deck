package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/experiments"
	"github.com/asheshgoplani/agent-deck/internal/session"
)

func TestRuntimeLifecycle_InsertTrySessionCreatesParentRuntimeAndGroup(t *testing.T) {
	storage, err := session.NewStorageWithProfile("_test_try_explicit_insert")
	if err != nil {
		t.Fatalf("NewStorageWithProfile: %v", err)
	}
	t.Cleanup(func() { _ = storage.Close() })

	inst := session.NewInstanceWithGroup("experiment", t.TempDir(), "experiments")
	inst.Command = "pi"
	inst.Tool = "pi"
	if err := insertTrySession(storage, inst, []*session.Instance{inst}, nil); err != nil {
		t.Fatalf("insertTrySession: %v", err)
	}
	if row, err := storage.GetDB().LoadInstanceByID(inst.ID); err != nil || row == nil {
		t.Fatalf("parent row=%#v err=%v", row, err)
	}
	if runtime, found, err := storage.GetDB().ReadRuntimeState(inst.ID); err != nil || !found {
		t.Fatalf("runtime=%#v found=%v err=%v", runtime, found, err)
	}
	groups, err := storage.GetDB().LoadGroups()
	if err != nil {
		t.Fatalf("LoadGroups: %v", err)
	}
	foundGroup := false
	for _, group := range groups {
		if group.Path == "experiments" {
			foundGroup = true
		}
	}
	if !foundGroup {
		t.Fatalf("experiments group not persisted: %#v", groups)
	}
}

func TestTryCommand_CreateExperiment(t *testing.T) {
	tmpDir := t.TempDir()

	// Create experiment
	exp, created, err := experiments.FindOrCreate(tmpDir, "test-project", true)
	if err != nil {
		t.Fatal(err)
	}

	if !created {
		t.Error("expected experiment to be created")
	}

	today := time.Now().Format("2006-01-02")
	if !strings.Contains(exp.Path, today) {
		t.Errorf("expected path to contain today's date %s, got %s", today, exp.Path)
	}

	// Verify directory exists
	if _, err := os.Stat(exp.Path); os.IsNotExist(err) {
		t.Error("experiment directory was not created")
	}
}

func TestTryCommand_FindExisting(t *testing.T) {
	tmpDir := t.TempDir()
	today := time.Now().Format("2006-01-02")

	// Pre-create an experiment folder
	existingPath := filepath.Join(tmpDir, today+"-my-project")
	if err := os.MkdirAll(existingPath, 0755); err != nil {
		t.Fatalf("failed to create experiment folder: %v", err)
	}

	// Try to find it
	exp, created, err := experiments.FindOrCreate(tmpDir, "my-project", true)
	if err != nil {
		t.Fatal(err)
	}

	if created {
		t.Error("expected to find existing experiment, not create new one")
	}

	if exp.Path != existingPath {
		t.Errorf("expected path %s, got %s", existingPath, exp.Path)
	}
}

func TestTryCommand_FuzzyMatch(t *testing.T) {
	tmpDir := t.TempDir()
	today := time.Now().Format("2006-01-02")

	// Create experiments
	if err := os.MkdirAll(filepath.Join(tmpDir, today+"-redis-cache"), 0755); err != nil {
		t.Fatalf("failed to create redis-cache folder: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(tmpDir, today+"-redis-server"), 0755); err != nil {
		t.Fatalf("failed to create redis-server folder: %v", err)
	}

	// Fuzzy search
	exps, _ := experiments.ListExperiments(tmpDir)
	matches := experiments.FuzzyFind(exps, "redis")

	if len(matches) != 2 {
		t.Errorf("expected 2 matches, got %d", len(matches))
	}

	// Partial match
	matches = experiments.FuzzyFind(exps, "rds-cch")
	if len(matches) == 0 {
		t.Error("expected fuzzy match for 'rds-cch'")
	}
}
