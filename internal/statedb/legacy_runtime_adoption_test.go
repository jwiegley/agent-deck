package statedb

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newV13LegacyRuntimeAdoptionDB(t *testing.T) *StateDB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "state-v13-adoption.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	installV13SchemaFixture(t, db.DB())
	seedV13SchemaFixture(t, db.DB())
	if err := db.Migrate(); err != nil {
		t.Fatalf("migrate v13 fixture: %v", err)
	}
	return db
}

func requireLegacyRuntimeAdoption(t *testing.T, db *StateDB, instanceID string) LegacyRuntimeAdoption {
	t.Helper()
	record, found, err := db.ReadLegacyRuntimeAdoption(instanceID)
	if err != nil || !found {
		t.Fatalf("ReadLegacyRuntimeAdoption(%q) = %#v, %v, %v", instanceID, record, found, err)
	}
	return record
}

func legacyRuntimeBindingSnapshotForTest(t *testing.T, db *StateDB, state RuntimeState) map[string]RuntimeBinding {
	t.Helper()
	bindings := make(map[string]RuntimeBinding)
	for _, kind := range bindingKinds {
		binding, found, err := db.ReadRuntimeBinding(state.InstanceID, kind)
		if err != nil {
			t.Fatal(err)
		}
		if found && binding.Generation == state.Generation {
			bindings[kind] = binding
		}
	}
	return bindings
}

func TestRuntimeLifecycle_LegacyRuntimeAdoptionV13MigrationOnly(t *testing.T) {
	db := newV13LegacyRuntimeAdoptionDB(t)
	record := requireLegacyRuntimeAdoption(t, db, "v13-instance")
	if record != (LegacyRuntimeAdoption{
		InstanceID: "v13-instance", TmuxSession: "legacy-tmux", TmuxSocketName: "legacy-socket",
	}) {
		t.Fatalf("legacy runtime adoption = %#v", record)
	}

	if _, err := db.DB().Exec(`
		INSERT INTO instances (id, title, project_path, created_at)
		VALUES ('current-instance', 'current', '/tmp/current', 1)`); err != nil {
		t.Fatal(err)
	}
	if record, found, err := db.ReadLegacyRuntimeAdoption("current-instance"); err != nil || found {
		t.Fatalf("current instance adoption = %#v, found=%v, err=%v", record, found, err)
	}
}

func TestRuntimeLifecycle_LegacyRuntimeAdoptionIncompleteStampIsIneligible(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state-v13-incomplete-adoption.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	installV13SchemaFixture(t, db.DB())
	seedV13SchemaFixture(t, db.DB())
	if _, err := db.DB().Exec(`
		INSERT INTO instances
			(id, title, project_path, status, tmux_session, tmux_socket_name, created_at, tool_data)
		VALUES
			('zero-started', 'zero started', '/tmp/zero-started', 'running', 'zero-tmux', 'zero-socket', 1, '{}'),
			('empty-status', 'empty status', '/tmp/empty-status', '', 'empty-status-tmux', 'empty-status-socket', 1,
			 '{"last_started_at":1700000000}')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(); err != nil {
		t.Fatalf("migrate incomplete v13 stamps: %v", err)
	}
	for _, instanceID := range []string{"zero-started", "empty-status"} {
		if record, found, err := db.ReadLegacyRuntimeAdoption(instanceID); err != nil || found {
			t.Errorf("%s adoption = %#v, found=%v, err=%v", instanceID, record, found, err)
		}
		if _, found, err := db.ReadRuntimeState(instanceID); err != nil || !found {
			t.Errorf("%s ordinary runtime migration found=%v, err=%v", instanceID, found, err)
		}
	}
}

func TestRuntimeLifecycle_LegacyRuntimeAdoptionFreshAndCurrentGenerationZeroAreIneligible(t *testing.T) {
	db := newRuntimeTestDB(t)
	if err := db.SaveInstance(&InstanceRow{
		ID: "fresh", Title: "fresh", ProjectPath: "/tmp/fresh", Status: "running",
		TmuxSession: "fresh-tmux", TmuxSocketName: "fresh-socket",
	}); err != nil {
		t.Fatal(err)
	}
	if record, found, err := db.ReadLegacyRuntimeAdoption("fresh"); err != nil || found {
		t.Fatalf("fresh adoption = %#v, found=%v, err=%v", record, found, err)
	}

	const midSpawnIncarnation = "current-generation-zero-mid-spawn"
	if _, err := db.EnsureRuntimeStateForNewInstance(RuntimeState{
		InstanceID: "mid-spawn", TmuxSession: "mid-spawn-tmux",
		TmuxSocketName: "mid-spawn-socket", Status: "starting",
	}, midSpawnIncarnation); err != nil {
		t.Fatal(err)
	}
	if record, found, err := db.ReadLegacyRuntimeAdoption("mid-spawn"); err != nil || found {
		t.Fatalf("mid-spawn adoption = %#v, found=%v, err=%v", record, found, err)
	}
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	if record, found, err := db.ReadLegacyRuntimeAdoption("mid-spawn"); err != nil || found {
		t.Fatalf("mid-spawn adoption after current-schema rerun = %#v, found=%v, err=%v", record, found, err)
	}
}

func TestRuntimeLifecycle_LegacyRuntimeAdoptionConsumeRequiresExactAuthority(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*StateDB, *RuntimeState, *string)
		wantParent bool
	}{
		{
			name: "expected generation",
			mutate: func(_ *StateDB, state *RuntimeState, _ *string) {
				state.Generation = 1
			},
		},
		{
			name: "durable generation",
			mutate: func(db *StateDB, _ *RuntimeState, _ *string) {
				if _, err := db.DB().Exec(`UPDATE instance_runtime_state SET runtime_generation = 1 WHERE instance_id = 'v13-instance'`); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "incarnation",
			mutate: func(_ *StateDB, _ *RuntimeState, incarnation *string) {
				*incarnation = "different-incarnation"
			},
			wantParent: true,
		},
		{
			name: "tmux session",
			mutate: func(_ *StateDB, state *RuntimeState, _ *string) {
				state.TmuxSession = "different-session"
			},
		},
		{
			name: "tmux socket",
			mutate: func(_ *StateDB, state *RuntimeState, _ *string) {
				state.TmuxSocketName = "different-socket"
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := newV13LegacyRuntimeAdoptionDB(t)
			state, found, err := db.ReadRuntimeState("v13-instance")
			if err != nil || !found {
				t.Fatalf("ReadRuntimeState = %#v, found=%v, err=%v", state, found, err)
			}
			incarnation := runtimeTestIncarnation(t, db, "v13-instance")
			tt.mutate(db, &state, &incarnation)
			err = db.ConsumeLegacyRuntimeAdoption(state, incarnation)
			if tt.wantParent {
				if !errors.Is(err, ErrInstanceParentConflict) {
					t.Fatalf("ConsumeLegacyRuntimeAdoption error = %v, want parent conflict", err)
				}
			} else if !errors.Is(err, ErrRuntimeGenerationConflict) {
				t.Fatalf("ConsumeLegacyRuntimeAdoption error = %v, want runtime conflict", err)
			}
			requireLegacyRuntimeAdoption(t, db, "v13-instance")
		})
	}
}

func TestRuntimeLifecycle_LegacyRuntimeAdoptionExactConsumeIsOneShot(t *testing.T) {
	db := newV13LegacyRuntimeAdoptionDB(t)
	state, found, err := db.ReadRuntimeState("v13-instance")
	if err != nil || !found {
		t.Fatalf("ReadRuntimeState = %#v, found=%v, err=%v", state, found, err)
	}
	incarnation := runtimeTestIncarnation(t, db, "v13-instance")
	if err := db.ConsumeLegacyRuntimeAdoption(state, incarnation); err != nil {
		t.Fatalf("exact consume: %v", err)
	}
	if record, found, err := db.ReadLegacyRuntimeAdoption("v13-instance"); err != nil || found {
		t.Fatalf("consumed adoption = %#v, found=%v, err=%v", record, found, err)
	}
	if err := db.Migrate(); err != nil {
		t.Fatalf("current-schema rerun: %v", err)
	}
	if record, found, err := db.ReadLegacyRuntimeAdoption("v13-instance"); err != nil || found {
		t.Fatalf("rerun recreated adoption = %#v, found=%v, err=%v", record, found, err)
	}
	if err := db.ConsumeLegacyRuntimeAdoption(state, incarnation); !errors.Is(err, ErrRuntimeGenerationConflict) {
		t.Fatalf("second consume error = %v, want runtime conflict", err)
	}
}

func TestRuntimeLifecycle_LegacyRuntimeAdoptionCommitFenceRollbackRetainsMarker(t *testing.T) {
	db := newV13LegacyRuntimeAdoptionDB(t)
	state, found, err := db.ReadRuntimeState("v13-instance")
	if err != nil || !found {
		t.Fatalf("ReadRuntimeState = %#v, found=%v, err=%v", state, found, err)
	}
	incarnation := runtimeTestIncarnation(t, db, state.InstanceID)
	fenceErr := errors.New("tmux authority changed after staged deletion")
	var stages []string
	err = db.ConsumeLegacyRuntimeAdoptionWithCommitFence(
		state, incarnation, legacyRuntimeBindingSnapshotForTest(t, db, state),
		func() error {
			stages = append(stages, "before-delete")
			return nil
		},
		func() error {
			stages = append(stages, "before-commit")
			return fenceErr
		},
	)
	if !errors.Is(err, fenceErr) {
		t.Fatalf("commit fence error = %v, want %v", err, fenceErr)
	}
	if got := strings.Join(stages, ","); got != "before-delete,before-commit" {
		t.Fatalf("commit fence stages = %q", got)
	}
	requireLegacyRuntimeAdoption(t, db, state.InstanceID)
}

func TestRuntimeLifecycle_LegacyRuntimeAdoptionCommitFenceRejectsBindingDrift(t *testing.T) {
	db := newV13LegacyRuntimeAdoptionDB(t)
	state, found, err := db.ReadRuntimeState("v13-instance")
	if err != nil || !found {
		t.Fatalf("ReadRuntimeState = %#v, found=%v, err=%v", state, found, err)
	}
	bindings := legacyRuntimeBindingSnapshotForTest(t, db, state)
	if _, err := db.DB().Exec(`UPDATE instance_runtime_binding
		SET binding_revision = binding_revision + 1, binding_value = 'replacement'
		WHERE instance_id = ? AND binding_kind = 'claude'`, state.InstanceID); err != nil {
		t.Fatal(err)
	}
	fenceCalls := 0
	err = db.ConsumeLegacyRuntimeAdoptionWithCommitFence(
		state, runtimeTestIncarnation(t, db, state.InstanceID), bindings,
		func() error { fenceCalls++; return nil },
		func() error { fenceCalls++; return nil },
	)
	if !errors.Is(err, ErrRuntimeGenerationConflict) {
		t.Fatalf("binding drift error = %v, want runtime conflict", err)
	}
	if fenceCalls != 0 {
		t.Fatalf("binding drift reached tmux fence %d times", fenceCalls)
	}
	requireLegacyRuntimeAdoption(t, db, state.InstanceID)
}

func TestRuntimeLifecycle_LegacyRuntimeAdoptionValidationRejectsDeleteReinsertABA(t *testing.T) {
	db := newV13LegacyRuntimeAdoptionDB(t)
	oldState, found, err := db.ReadRuntimeState("v13-instance")
	if err != nil || !found {
		t.Fatalf("ReadRuntimeState = %#v, found=%v, err=%v", oldState, found, err)
	}
	oldIncarnation := runtimeTestIncarnation(t, db, "v13-instance")
	if err := db.ValidateLegacyRuntimeAdoption(oldState, oldIncarnation); err != nil {
		t.Fatalf("validate original adoption: %v", err)
	}

	if err := db.DeleteInstance("v13-instance"); err != nil {
		t.Fatal(err)
	}
	if record, found, err := db.ReadLegacyRuntimeAdoption("v13-instance"); err != nil || found {
		t.Fatalf("deleted instance adoption = %#v, found=%v, err=%v", record, found, err)
	}
	if err := db.SaveInstance(&InstanceRow{
		ID: "v13-instance", Title: "replacement", ProjectPath: "/tmp/replacement",
		Status: "waiting", TmuxSession: oldState.TmuxSession,
		TmuxSocketName: oldState.TmuxSocketName,
	}); err != nil {
		t.Fatal(err)
	}
	replacementState, found, err := db.ReadRuntimeState("v13-instance")
	if err != nil || !found {
		t.Fatalf("replacement ReadRuntimeState = %#v, found=%v, err=%v", replacementState, found, err)
	}
	replacementIncarnation := runtimeTestIncarnation(t, db, "v13-instance")
	if replacementIncarnation == oldIncarnation {
		t.Fatal("delete/reinsert did not rotate incarnation")
	}
	if err := db.ValidateLegacyRuntimeAdoption(oldState, oldIncarnation); !errors.Is(err, ErrInstanceParentConflict) {
		t.Fatalf("old authority validation error = %v, want parent conflict", err)
	}
	if err := db.ValidateLegacyRuntimeAdoption(replacementState, replacementIncarnation); !errors.Is(err, ErrRuntimeGenerationConflict) {
		t.Fatalf("replacement authority validation error = %v, want missing-marker runtime conflict", err)
	}
	if record, found, err := db.ReadLegacyRuntimeAdoption("v13-instance"); err != nil || found {
		t.Fatalf("reinserted instance adoption = %#v, found=%v, err=%v", record, found, err)
	}
}

func TestRuntimeLifecycle_LegacyRuntimeAdoptionBlocksStatusCASUntilConsumed(t *testing.T) {
	db := newV13LegacyRuntimeAdoptionDB(t)
	state, found, err := db.ReadRuntimeState("v13-instance")
	if err != nil || !found {
		t.Fatalf("ReadRuntimeState = %#v, found=%v, err=%v", state, found, err)
	}
	incarnation := runtimeTestIncarnation(t, db, state.InstanceID)
	applied, err := db.WriteStatusIfVersion(
		state.InstanceID, incarnation, state.Generation, state.StatusRevision, "running")
	if err != nil || applied {
		t.Fatalf("status CAS while adoption marker exists = applied %v, err %v", applied, err)
	}
	unchanged, found, err := db.ReadRuntimeState(state.InstanceID)
	if err != nil || !found || unchanged != state {
		t.Fatalf("blocked status CAS changed tuple: state=%#v found=%v err=%v; want %#v", unchanged, found, err, state)
	}
	if err := db.ValidateLegacyRuntimeAdoption(state, incarnation); err != nil {
		t.Fatalf("blocked CAS invalidated adoption: %v", err)
	}
	if err := db.ConsumeLegacyRuntimeAdoption(state, incarnation); err != nil {
		t.Fatalf("consume adoption: %v", err)
	}
	applied, err = db.WriteStatusIfVersion(
		state.InstanceID, incarnation, state.Generation, state.StatusRevision, "running")
	if err != nil || !applied {
		t.Fatalf("status CAS after consume = applied %v, err %v", applied, err)
	}
	current, found, err := db.ReadRuntimeState(state.InstanceID)
	if err != nil || !found || current.Status != "running" || current.StatusRevision != state.StatusRevision+1 {
		t.Fatalf("post-consume status state=%#v found=%v err=%v", current, found, err)
	}
}

func TestRuntimeLifecycle_LegacyRuntimeAdoptionRejectsCompleteTupleDrift(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *StateDB, string)
	}{
		{
			name: "status revision",
			mutate: func(t *testing.T, db *StateDB, instanceID string) {
				t.Helper()
				if _, err := db.DB().Exec(`UPDATE instance_runtime_state
					SET status_revision = status_revision + 1 WHERE instance_id = ?`, instanceID); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "status",
			mutate: func(t *testing.T, db *StateDB, instanceID string) {
				t.Helper()
				if _, err := db.DB().Exec(`UPDATE instance_runtime_state
					SET status = 'running' WHERE instance_id = ?`, instanceID); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "last started",
			mutate: func(t *testing.T, db *StateDB, instanceID string) {
				t.Helper()
				if _, err := db.DB().Exec(`UPDATE instance_runtime_state
					SET last_started_at = last_started_at + 1 WHERE instance_id = ?`, instanceID); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := newV13LegacyRuntimeAdoptionDB(t)
			expected, found, err := db.ReadRuntimeState("v13-instance")
			if err != nil || !found {
				t.Fatalf("ReadRuntimeState = %#v, found=%v, err=%v", expected, found, err)
			}
			incarnation := runtimeTestIncarnation(t, db, expected.InstanceID)
			tt.mutate(t, db, expected.InstanceID)
			if err := db.ValidateLegacyRuntimeAdoption(expected, incarnation); !errors.Is(err, ErrRuntimeGenerationConflict) {
				t.Fatalf("validation after %s drift error = %v, want runtime conflict", tt.name, err)
			}
			if err := db.ConsumeLegacyRuntimeAdoption(expected, incarnation); !errors.Is(err, ErrRuntimeGenerationConflict) {
				t.Fatalf("consume after %s drift error = %v, want runtime conflict", tt.name, err)
			}
			requireLegacyRuntimeAdoption(t, db, expected.InstanceID)
		})
	}
}

func TestRuntimeLifecycle_LegacyRuntimeAdoptionNormalizesExpectedStartTime(t *testing.T) {
	db := newV13LegacyRuntimeAdoptionDB(t)
	expected, found, err := db.ReadRuntimeState("v13-instance")
	if err != nil || !found {
		t.Fatalf("ReadRuntimeState = %#v, found=%v, err=%v", expected, found, err)
	}
	_, offset := expected.LastStartedAt.Zone()
	expected.LastStartedAt = expected.LastStartedAt.In(time.FixedZone("equivalent", offset))
	if err := db.ValidateLegacyRuntimeAdoption(expected, runtimeTestIncarnation(t, db, expected.InstanceID)); err != nil {
		t.Fatalf("equivalent start time failed validation: %v", err)
	}
}

func TestRuntimeLifecycle_LegacyRuntimeAdoptionStatusGuardMatchesExactMarkerOnly(t *testing.T) {
	for _, test := range []struct {
		name       string
		mutate     func(*testing.T, *StateDB)
		generation uint64
	}{
		{
			name: "foreign incarnation",
			mutate: func(t *testing.T, db *StateDB) {
				if _, err := db.DB().Exec(`UPDATE instance_legacy_runtime_adoption SET incarnation = 'foreign'`); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "different tmux identity",
			mutate: func(t *testing.T, db *StateDB) {
				if _, err := db.DB().Exec(`UPDATE instance_legacy_runtime_adoption SET tmux_session = 'other'`); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "modern generation",
			mutate: func(t *testing.T, db *StateDB) {
				if _, err := db.DB().Exec(`UPDATE instance_runtime_state SET runtime_generation = 1`); err != nil {
					t.Fatal(err)
				}
			},
			generation: 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := newV13LegacyRuntimeAdoptionDB(t)
			incarnation := runtimeTestIncarnation(t, db, "v13-instance")
			test.mutate(t, db)
			applied, err := db.WriteStatusIfVersion("v13-instance", incarnation, test.generation, 0, "running")
			if err != nil || !applied {
				t.Fatalf("status CAS with nonmatching marker = applied %v, err %v", applied, err)
			}
		})
	}
}

func TestRuntimeLifecycle_ModernTransitionSupersedesLegacyAdoptionAndUnfreezesStatus(t *testing.T) {
	db := newV13LegacyRuntimeAdoptionDB(t)
	state, found, err := db.ReadRuntimeState("v13-instance")
	if err != nil || !found {
		t.Fatalf("ReadRuntimeState = %#v, found=%v, err=%v", state, found, err)
	}
	incarnation := runtimeTestIncarnation(t, db, state.InstanceID)
	plan := make([]RuntimeBindingTransition, 0, len(bindingKinds))
	for _, kind := range bindingKinds {
		binding, found, err := db.ReadRuntimeBinding(state.InstanceID, kind)
		if err != nil || !found {
			t.Fatalf("ReadRuntimeBinding(%s) = %#v, found=%v, err=%v", kind, binding, found, err)
		}
		plan = append(plan, RuntimeBindingTransition{
			Kind: kind, ExpectedRevision: binding.Revision,
			NextValue: binding.Value, DetectedAt: binding.DetectedAt,
		})
	}
	next := state
	next.Generation++
	next.StatusRevision = 0
	next.TmuxSession = "modern-tmux"
	next.Status = "waiting"
	next.LastStartedAt = state.LastStartedAt.Add(time.Second)
	if err := db.CommitRuntimeTransitionWithBindingPlan(state.Generation, incarnation, next, plan); err != nil {
		t.Fatalf("CommitRuntimeTransitionWithBindingPlan: %v", err)
	}
	if record, found, err := db.ReadLegacyRuntimeAdoption(state.InstanceID); err != nil || found {
		t.Fatalf("post-transition marker = %#v, found=%v, err=%v", record, found, err)
	}
	applied, err := db.WriteStatusIfVersion(state.InstanceID, incarnation, next.Generation, 0, "running")
	if err != nil || !applied {
		t.Fatalf("post-transition status CAS = applied %v, err %v", applied, err)
	}
}

func TestRuntimeLifecycle_LegacyAdoptionSupersessionRequiresExactRuntime(t *testing.T) {
	db := newV13LegacyRuntimeAdoptionDB(t)
	state, found, err := db.ReadRuntimeState("v13-instance")
	if err != nil || !found {
		t.Fatalf("ReadRuntimeState = %#v, found=%v, err=%v", state, found, err)
	}
	incarnation := runtimeTestIncarnation(t, db, state.InstanceID)
	stale := state
	stale.StatusRevision++
	if err := db.SupersedeLegacyRuntimeAdoption(stale, incarnation); !errors.Is(err, ErrRuntimeGenerationConflict) {
		t.Fatalf("stale supersession error = %v, want runtime conflict", err)
	}
	requireLegacyRuntimeAdoption(t, db, state.InstanceID)
	if err := db.SupersedeLegacyRuntimeAdoption(state, incarnation); err != nil {
		t.Fatalf("exact supersession: %v", err)
	}
	if record, found, err := db.ReadLegacyRuntimeAdoption(state.InstanceID); err != nil || found {
		t.Fatalf("superseded marker = %#v, found=%v, err=%v", record, found, err)
	}
	applied, err := db.WriteStatusIfVersion(state.InstanceID, incarnation, state.Generation, state.StatusRevision, "running")
	if err != nil || !applied {
		t.Fatalf("status CAS after supersession = applied %v, err %v", applied, err)
	}
}

func TestRuntimeLifecycle_RuntimeDeleteReservationSupersedesLegacyAdoption(t *testing.T) {
	db := newV13LegacyRuntimeAdoptionDB(t)
	state, found, err := db.ReadRuntimeState("v13-instance")
	if err != nil || !found {
		t.Fatalf("ReadRuntimeState = %#v, found=%v, err=%v", state, found, err)
	}
	claimed, err := db.ReserveRuntimeDestruction(state, runtimeTestIncarnation(t, db, state.InstanceID))
	if err != nil || claimed.Status != runtimeDestructionStatus {
		t.Fatalf("ReserveRuntimeDestruction = %#v, err=%v", claimed, err)
	}
	if record, found, err := db.ReadLegacyRuntimeAdoption(state.InstanceID); err != nil || found {
		t.Fatalf("reserved delete marker = %#v, found=%v, err=%v", record, found, err)
	}
}

func TestRuntimeLifecycle_LegacyParentReinsertSupersedesAdoptionMarker(t *testing.T) {
	db := newV13LegacyRuntimeAdoptionDB(t)
	if _, err := db.DB().Exec(`
		INSERT OR REPLACE INTO instances (id, title, project_path, created_at)
		VALUES ('v13-instance', 'replacement', '/replacement', 2)`); err != nil {
		t.Fatal(err)
	}
	if record, found, err := db.ReadLegacyRuntimeAdoption("v13-instance"); err != nil || found {
		t.Fatalf("reinserted parent marker = %#v, found=%v, err=%v", record, found, err)
	}
}
