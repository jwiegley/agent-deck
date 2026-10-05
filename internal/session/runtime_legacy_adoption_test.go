package session

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

func legacyRuntimeAdoptionFixture(t *testing.T) (*statedb.StateDB, *Instance, statedb.RuntimeState, tmux.RuntimeCandidate) {
	t.Helper()
	db, err := statedb.Open(filepath.Join(t.TempDir(), "legacy-adoption.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}

	started := time.Unix(1_700_000_000, 123).UTC()
	parent := &statedb.InstanceRow{
		ID: "legacy-one", Title: "legacy one", ProjectPath: "/tmp/legacy-one",
		GroupPath: "my-sessions", Tool: "pi", Status: "waiting",
		TmuxSession: "agentdeck_legacy_one", TmuxSocketName: "legacy-socket",
		CreatedAt: time.Unix(1, 0).UTC(), LastStartedAt: started,
		RuntimeBindings: map[string]statedb.RuntimeBinding{},
	}
	if err := db.SaveInstance(parent); err != nil {
		t.Fatal(err)
	}
	if parent.Incarnation == "" {
		t.Fatal("fixture parent has no incarnation")
	}
	if _, err := db.DB().Exec(`
		INSERT INTO instance_legacy_runtime_adoption
			(instance_id, incarnation, tmux_session, tmux_socket_name)
		VALUES (?, ?, ?, ?)`,
		parent.ID, parent.Incarnation, parent.TmuxSession, parent.TmuxSocketName); err != nil {
		t.Fatal(err)
	}
	state, found, err := db.ReadRuntimeState(parent.ID)
	if err != nil || !found {
		t.Fatalf("ReadRuntimeState = %#v, found=%v, err=%v", state, found, err)
	}
	inst := &Instance{
		ID: parent.ID, Title: parent.Title, ProjectPath: parent.ProjectPath,
		GroupPath: parent.GroupPath, Tool: parent.Tool, Status: Status(parent.Status),
		RuntimeGeneration: state.Generation, StatusRevision: state.StatusRevision,
		TmuxSocketName: state.TmuxSocketName, LastStartedAt: state.LastStartedAt,
		tmuxSession: &tmux.Session{
			Name: state.TmuxSession, SocketName: state.TmuxSocketName, InstanceID: state.InstanceID,
		},
	}
	inst.setOwningDB(db)
	inst.adoptPersistenceIncarnation(parent.Incarnation)
	candidate := tmux.RuntimeCandidate{
		SessionID: "$7", SessionName: state.TmuxSession, SocketName: state.TmuxSocketName,
		PaneID: "%9", PanePID: 4242, InstanceID: state.InstanceID,
		ProofError: "missing AGENTDECK_RUNTIME_GENERATION",
	}
	return db, inst, state, candidate
}

func installLegacyRuntimeAdoptionSeams(t *testing.T, candidates *[]tmux.RuntimeCandidate) *int {
	t.Helper()
	installRuntimeLifecycleTestSeams(t)
	oldAdopt := adoptLegacyRuntimeCandidateFn
	oldValidate := validateLegacyRuntimeCandidateStampFn
	oldValidateWithIdentity := validateLegacyRuntimeCandidateStampWithIdentityFn
	oldCaptureIdentity := captureLegacyRuntimeProcessIdentityFn
	oldIdentityMatches := legacyRuntimeProcessIdentityMatchesFn
	oldBeforeConsume := legacyRuntimeAdoptionBeforeConsumeFn
	adoptCalls := 0
	t.Cleanup(func() {
		adoptLegacyRuntimeCandidateFn = oldAdopt
		validateLegacyRuntimeCandidateStampFn = oldValidate
		validateLegacyRuntimeCandidateStampWithIdentityFn = oldValidateWithIdentity
		captureLegacyRuntimeProcessIdentityFn = oldCaptureIdentity
		legacyRuntimeProcessIdentityMatchesFn = oldIdentityMatches
		legacyRuntimeAdoptionBeforeConsumeFn = oldBeforeConsume
	})
	runtimeCandidateInventoryFn = func(socketName, instanceID string) ([]tmux.RuntimeCandidate, error) {
		if socketName != "legacy-socket" || instanceID != "legacy-one" {
			return nil, nil
		}
		return append([]tmux.RuntimeCandidate(nil), (*candidates)...), nil
	}
	runtimeCandidateRevalidateFn = func(candidate tmux.RuntimeCandidate) (tmux.RuntimeCandidate, error) {
		return candidate, nil
	}
	adoptLegacyRuntimeCandidateFn = func(tmux.LegacyRuntimeAdoption) error {
		adoptCalls++
		return nil
	}
	validateLegacyRuntimeCandidateStampFn = func(tmux.LegacyRuntimeAdoption) error { return nil }
	validateLegacyRuntimeCandidateStampWithIdentityFn = func(adoption tmux.LegacyRuntimeAdoption, _ tmux.ProcessIdentity) error {
		return validateLegacyRuntimeCandidateStampFn(adoption)
	}
	captureLegacyRuntimeProcessIdentityFn = func(pid int) (tmux.ProcessIdentity, error) {
		return tmux.ProcessIdentity{PID: pid, StartToken: "legacy-test-birth"}, nil
	}
	legacyRuntimeProcessIdentityMatchesFn = func(tmux.ProcessIdentity) bool { return true }
	legacyRuntimeAdoptionBeforeConsumeFn = func() {}
	return &adoptCalls
}

func stampedLegacyRuntimeCandidate(state statedb.RuntimeState, candidate tmux.RuntimeCandidate) tmux.RuntimeCandidate {
	candidate.Generation = state.Generation
	candidate.GenerationKnown = true
	candidate.StatusRevision = state.StatusRevision
	candidate.Status = state.Status
	candidate.LastStartedUnixNano = state.LastStartedAt.UnixNano()
	candidate.StateKnown = true
	candidate.BindingKind = ""
	candidate.BindingValue = ""
	candidate.BindingKnown = true
	candidate.ProofError = ""
	return candidate
}

func requireLegacyAdoptionMarker(t *testing.T, db *statedb.StateDB, instanceID string, want bool) {
	t.Helper()
	_, found, err := db.ReadLegacyRuntimeAdoption(instanceID)
	if err != nil || found != want {
		t.Fatalf("legacy adoption marker found=%v, err=%v; want found=%v", found, err, want)
	}
}

func TestRuntimeLifecycle_LegacyAdoptionPlanIsReadOnly(t *testing.T) {
	db, inst, state, candidate := legacyRuntimeAdoptionFixture(t)
	candidates := []tmux.RuntimeCandidate{candidate}
	adoptCalls := installLegacyRuntimeAdoptionSeams(t, &candidates)

	plan, err := inst.PlanLegacyRuntimeAdoption()
	if err != nil {
		t.Fatal(err)
	}
	if plan.State != state || plan.Candidate != candidate || plan.AlreadyStamped {
		t.Fatalf("legacy adoption plan = %#v", plan)
	}
	if *adoptCalls != 0 {
		t.Fatalf("dry-run performed %d tmux mutations", *adoptCalls)
	}
	requireLegacyAdoptionMarker(t, db, inst.ID, true)
	got, found, err := db.ReadRuntimeState(inst.ID)
	if err != nil || !found || got != state {
		t.Fatalf("dry-run runtime state = %#v, found=%v, err=%v; want %#v", got, found, err, state)
	}
}

func TestRuntimeLifecycle_LegacyAdoptionStampsUniqueCandidateAndConsumesMarker(t *testing.T) {
	db, inst, state, candidate := legacyRuntimeAdoptionFixture(t)
	candidates := []tmux.RuntimeCandidate{candidate}
	adoptCalls := installLegacyRuntimeAdoptionSeams(t, &candidates)
	adoptLegacyRuntimeCandidateFn = func(adoption tmux.LegacyRuntimeAdoption) error {
		*adoptCalls++
		if adoption.Candidate != candidate || adoption.StatusRevision != state.StatusRevision ||
			adoption.Status != state.Status || adoption.StartedUnixNano != state.LastStartedAt.UnixNano() ||
			adoption.BindingKind != "" || adoption.BindingValue != "" {
			t.Fatalf("legacy tmux adoption = %#v", adoption)
		}
		candidates = []tmux.RuntimeCandidate{stampedLegacyRuntimeCandidate(state, candidate)}
		return nil
	}

	result, err := inst.AdoptLegacyRuntime()
	if err != nil {
		t.Fatal(err)
	}
	if *adoptCalls != 1 || !result.Live || result.State != state {
		t.Fatalf("legacy adoption calls=%d result=%#v", *adoptCalls, result)
	}
	requireLegacyAdoptionMarker(t, db, inst.ID, false)
}

func TestRuntimeLifecycle_LegacyAdoptionResumesAfterStampBeforeConsume(t *testing.T) {
	db, inst, state, candidate := legacyRuntimeAdoptionFixture(t)
	candidates := []tmux.RuntimeCandidate{stampedLegacyRuntimeCandidate(state, candidate)}
	adoptCalls := installLegacyRuntimeAdoptionSeams(t, &candidates)

	plan, err := inst.PlanLegacyRuntimeAdoption()
	if err != nil || !plan.AlreadyStamped {
		t.Fatalf("already-stamped plan = %#v, err=%v", plan, err)
	}
	result, err := inst.AdoptLegacyRuntime()
	if err != nil {
		t.Fatal(err)
	}
	if *adoptCalls != 0 || !result.Live || result.State != state {
		t.Fatalf("already-stamped adoption calls=%d result=%#v", *adoptCalls, result)
	}
	requireLegacyAdoptionMarker(t, db, inst.ID, false)
}

func TestRuntimeLifecycle_LegacyAdoptionRefusesAmbiguousCandidates(t *testing.T) {
	db, inst, _, candidate := legacyRuntimeAdoptionFixture(t)
	replacement := candidate
	replacement.SessionID = "$8"
	replacement.PaneID = "%10"
	replacement.PanePID = 4343
	candidates := []tmux.RuntimeCandidate{candidate, replacement}
	adoptCalls := installLegacyRuntimeAdoptionSeams(t, &candidates)

	_, err := inst.AdoptLegacyRuntime()
	var ambiguity *RuntimeReconciliationAmbiguityError
	if !errors.As(err, &ambiguity) || len(ambiguity.Candidates) != 2 {
		t.Fatalf("ambiguous adoption error = %v", err)
	}
	if *adoptCalls != 0 {
		t.Fatalf("ambiguous adoption performed %d tmux mutations", *adoptCalls)
	}
	requireLegacyAdoptionMarker(t, db, inst.ID, true)
}

func TestRuntimeLifecycle_LegacyAdoptionRefusesChangedCandidate(t *testing.T) {
	db, inst, _, candidate := legacyRuntimeAdoptionFixture(t)
	candidates := []tmux.RuntimeCandidate{candidate}
	adoptCalls := installLegacyRuntimeAdoptionSeams(t, &candidates)
	runtimeCandidateRevalidateFn = func(got tmux.RuntimeCandidate) (tmux.RuntimeCandidate, error) {
		got.PanePID++
		return got, nil
	}

	_, err := inst.AdoptLegacyRuntime()
	var ambiguity *RuntimeReconciliationAmbiguityError
	if !errors.As(err, &ambiguity) || len(ambiguity.Candidates) != 2 {
		t.Fatalf("changed-candidate adoption error = %v", err)
	}
	if *adoptCalls != 0 {
		t.Fatalf("changed-candidate adoption performed %d tmux mutations", *adoptCalls)
	}
	requireLegacyAdoptionMarker(t, db, inst.ID, true)
}

func TestRuntimeLifecycle_CurrentGenerationZeroWithoutMigrationMarkerIsNeverAdopted(t *testing.T) {
	db, inst, _, candidate := legacyRuntimeAdoptionFixture(t)
	if _, err := db.DB().Exec(`DELETE FROM instance_legacy_runtime_adoption WHERE instance_id = ?`, inst.ID); err != nil {
		t.Fatal(err)
	}
	candidates := []tmux.RuntimeCandidate{candidate}
	adoptCalls := installLegacyRuntimeAdoptionSeams(t, &candidates)
	inventoryCalls := 0
	runtimeCandidateInventoryFn = func(string, string) ([]tmux.RuntimeCandidate, error) {
		inventoryCalls++
		return candidates, nil
	}

	_, err := inst.AdoptLegacyRuntime()
	if err == nil {
		t.Fatal("unmarked generation-zero runtime was adopted")
	}
	if inventoryCalls != 0 || *adoptCalls != 0 {
		t.Fatalf("unmarked runtime inventory calls=%d adoption calls=%d", inventoryCalls, *adoptCalls)
	}
	requireLegacyAdoptionMarker(t, db, inst.ID, false)
}

func TestRuntimeLifecycle_StaleLegacyAdoptionMarkerIsRejectedBeforeInventory(t *testing.T) {
	db, inst, _, candidate := legacyRuntimeAdoptionFixture(t)
	if _, err := db.DB().Exec(`
		UPDATE instance_legacy_runtime_adoption SET incarnation = 'stale-incarnation'
		WHERE instance_id = ?`, inst.ID); err != nil {
		t.Fatal(err)
	}
	candidates := []tmux.RuntimeCandidate{candidate}
	adoptCalls := installLegacyRuntimeAdoptionSeams(t, &candidates)
	inventoryCalls := 0
	runtimeCandidateInventoryFn = func(string, string) ([]tmux.RuntimeCandidate, error) {
		inventoryCalls++
		return candidates, nil
	}

	_, err := inst.AdoptLegacyRuntime()
	if !errors.Is(err, statedb.ErrInstanceParentConflict) {
		t.Fatalf("stale marker error = %v, want parent conflict", err)
	}
	if inventoryCalls != 0 || *adoptCalls != 0 {
		t.Fatalf("stale marker inventory calls=%d adoption calls=%d", inventoryCalls, *adoptCalls)
	}
	requireLegacyAdoptionMarker(t, db, inst.ID, true)
}

func TestRuntimeLifecycle_LegacyAdoptionRespawnBetweenFinalProbeAndConsumePreservesMarker(t *testing.T) {
	db, inst, state, candidate := legacyRuntimeAdoptionFixture(t)
	candidates := []tmux.RuntimeCandidate{candidate}
	adoptCalls := installLegacyRuntimeAdoptionSeams(t, &candidates)
	adoptLegacyRuntimeCandidateFn = func(tmux.LegacyRuntimeAdoption) error {
		*adoptCalls++
		candidates = []tmux.RuntimeCandidate{stampedLegacyRuntimeCandidate(state, candidate)}
		return nil
	}
	validateCalls := 0
	validateLegacyRuntimeCandidateStampFn = func(adoption tmux.LegacyRuntimeAdoption) error {
		validateCalls++
		if adoption.Candidate.PanePID != candidate.PanePID {
			t.Fatalf("final proof pane pid = %d, want captured pid %d", adoption.Candidate.PanePID, candidate.PanePID)
		}
		if validateCalls == 1 {
			// The standalone proof succeeds, then the pane respawns before the
			// writer-reserved consume fence performs its own proof.
			replacement := stampedLegacyRuntimeCandidate(state, candidate)
			replacement.PanePID++
			candidates = []tmux.RuntimeCandidate{replacement}
			return nil
		}
		return tmux.ErrLegacyRuntimeCandidateChanged
	}

	_, err := inst.AdoptLegacyRuntime()
	var ambiguity *RuntimeReconciliationAmbiguityError
	if !errors.As(err, &ambiguity) || validateCalls != 2 || *adoptCalls != 1 {
		t.Fatalf("final proof error=%v validations=%d adoptions=%d", err, validateCalls, *adoptCalls)
	}
	requireLegacyAdoptionMarker(t, db, inst.ID, true)
}

func TestRuntimeLifecycle_LegacyAdoptionSamePIDNewBirthBetweenValidationAndConsumePreservesMarker(t *testing.T) {
	db, inst, state, candidate := legacyRuntimeAdoptionFixture(t)
	candidates := []tmux.RuntimeCandidate{candidate}
	adoptCalls := installLegacyRuntimeAdoptionSeams(t, &candidates)
	adoptLegacyRuntimeCandidateFn = func(tmux.LegacyRuntimeAdoption) error {
		*adoptCalls++
		candidates = []tmux.RuntimeCandidate{stampedLegacyRuntimeCandidate(state, candidate)}
		return nil
	}
	currentBirth := "birth-one"
	captures := 0
	captureLegacyRuntimeProcessIdentityFn = func(pid int) (tmux.ProcessIdentity, error) {
		captures++
		return tmux.ProcessIdentity{PID: pid, StartToken: currentBirth}, nil
	}
	legacyRuntimeProcessIdentityMatchesFn = func(identity tmux.ProcessIdentity) bool {
		return identity.PID == candidate.PanePID && identity.StartToken == currentBirth
	}
	validateCalls := 0
	validateLegacyRuntimeCandidateStampFn = func(adoption tmux.LegacyRuntimeAdoption) error {
		validateCalls++
		if adoption.Candidate.PanePID != candidate.PanePID {
			t.Fatalf("same-PID fence candidate = %d, want %d", adoption.Candidate.PanePID, candidate.PanePID)
		}
		return nil
	}
	gapHookCalls := 0
	legacyRuntimeAdoptionBeforeConsumeFn = func() {
		gapHookCalls++
		// The successful proof still names PID 4242, but that PID is replaced
		// before BEGIN IMMEDIATE invokes its first fence callback. Recapturing
		// here would incorrectly turn the replacement into the new authority.
		currentBirth = "birth-two"
	}

	_, err := inst.AdoptLegacyRuntime()
	var ambiguity *RuntimeReconciliationAmbiguityError
	if !errors.As(err, &ambiguity) || validateCalls != 1 || captures != 1 || gapHookCalls != 1 || *adoptCalls != 1 {
		t.Fatalf("same-PID fence error=%v validations=%d captures=%d gap-hooks=%d adoptions=%d",
			err, validateCalls, captures, gapHookCalls, *adoptCalls)
	}
	requireLegacyAdoptionMarker(t, db, inst.ID, true)
}

func TestRuntimeLifecycle_LegacyAdoptionFinalProofRejectsSameNameReplacement(t *testing.T) {
	db, inst, state, candidate := legacyRuntimeAdoptionFixture(t)
	candidates := []tmux.RuntimeCandidate{candidate}
	adoptCalls := installLegacyRuntimeAdoptionSeams(t, &candidates)
	adoptLegacyRuntimeCandidateFn = func(tmux.LegacyRuntimeAdoption) error {
		*adoptCalls++
		replacement := stampedLegacyRuntimeCandidate(state, candidate)
		replacement.SessionID = "$8"
		replacement.PaneID = "%10"
		replacement.PanePID++
		candidates = []tmux.RuntimeCandidate{replacement}
		return nil
	}

	_, err := inst.AdoptLegacyRuntime()
	var ambiguity *RuntimeReconciliationAmbiguityError
	if !errors.As(err, &ambiguity) || *adoptCalls != 1 {
		t.Fatalf("same-name replacement error=%v adoptions=%d", err, *adoptCalls)
	}
	requireLegacyAdoptionMarker(t, db, inst.ID, true)
}

func TestRuntimeLifecycle_AlreadyStampedLegacyAdoptionRequiresExactStampBeforeRecovery(t *testing.T) {
	db, inst, state, candidate := legacyRuntimeAdoptionFixture(t)
	candidates := []tmux.RuntimeCandidate{stampedLegacyRuntimeCandidate(state, candidate)}
	adoptCalls := installLegacyRuntimeAdoptionSeams(t, &candidates)
	validateLegacyRuntimeCandidateStampFn = func(tmux.LegacyRuntimeAdoption) error {
		return tmux.ErrLegacyRuntimeCandidateChanged
	}

	_, err := inst.AdoptLegacyRuntime()
	var ambiguity *RuntimeReconciliationAmbiguityError
	if !errors.As(err, &ambiguity) {
		t.Fatalf("already-stamped corruption error = %v, want ambiguity", err)
	}
	if *adoptCalls != 0 {
		t.Fatalf("corrupt already-stamped candidate performed %d mutations", *adoptCalls)
	}
	requireLegacyAdoptionMarker(t, db, inst.ID, true)
}

func TestRuntimeLifecycle_ModernRespawnCrashCannotReuseLegacyMarker(t *testing.T) {
	db, inst, state, candidate := legacyRuntimeAdoptionFixture(t)
	candidates := []tmux.RuntimeCandidate{stampedLegacyRuntimeCandidate(state, candidate)}
	installLegacyRuntimeAdoptionSeams(t, &candidates)
	crashErr := errors.New("simulated crash before final generation stamp")
	runtimeCandidateRespawnFn = func(_ *tmux.Session, got tmux.RuntimeGenerationCandidate, _ string) error {
		if got.SessionID != candidate.SessionID || got.PaneID != candidate.PaneID {
			t.Fatalf("respawn candidate = %#v", got)
		}
		stranded := stampedLegacyRuntimeCandidate(state, candidate)
		stranded.GenerationKnown = false
		stranded.ProofError = "missing AGENTDECK_RUNTIME_GENERATION"
		candidates = []tmux.RuntimeCandidate{stranded}
		return nil
	}
	runtimeTransitionFaultFn = func(stage RuntimeTransitionStage, _ statedb.RuntimeState) error {
		if stage == RuntimeTransitionAfterRespawnBeforeStamp {
			return crashErr
		}
		return nil
	}

	authority, winner, err := inst.beginRuntimeTransition(false)
	if err != nil || winner != nil || authority == nil {
		t.Fatalf("beginRuntimeTransition authority=%#v winner=%#v err=%v", authority, winner, err)
	}
	if err := inst.respawnRuntimePane(authority, "resume-command"); !errors.Is(err, crashErr) {
		t.Fatalf("respawn crash error = %v, want %v", err, crashErr)
	}
	authority.close()
	requireLegacyAdoptionMarker(t, db, inst.ID, false)
	if _, err := inst.PlanLegacyRuntimeAdoption(); err == nil {
		t.Fatal("crash-stranded modern pane reused a retired migration marker")
	}
}

func TestRuntimeLifecycle_AmbiguousModernTransitionPreservesLegacyMarker(t *testing.T) {
	db, inst, _, candidate := legacyRuntimeAdoptionFixture(t)
	candidates := []tmux.RuntimeCandidate{candidate}
	installLegacyRuntimeAdoptionSeams(t, &candidates)

	authority, winner, err := inst.beginRuntimeTransition(false)
	var ambiguity *RuntimeReconciliationAmbiguityError
	if authority != nil || winner != nil || !errors.As(err, &ambiguity) {
		t.Fatalf("ambiguous begin authority=%#v winner=%#v err=%v", authority, winner, err)
	}
	requireLegacyAdoptionMarker(t, db, inst.ID, true)
}
