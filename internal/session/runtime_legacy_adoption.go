package session

import (
	"errors"
	"fmt"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// LegacyRuntimeAdoptionPlan describes the one migration-recorded generation-0
// runtime that an operator may explicitly bring under generation authority.
// AlreadyStamped is true only when a prior adoption completed the tmux mutation
// but stopped before consuming the one-time migration record.
type LegacyRuntimeAdoptionPlan struct {
	State          statedb.RuntimeState
	Candidate      tmux.RuntimeCandidate
	AlreadyStamped bool
}

var (
	adoptLegacyRuntimeCandidateFn                     = tmux.AdoptLegacyRuntimeCandidate
	validateLegacyRuntimeCandidateStampFn             = tmux.ValidateLegacyRuntimeCandidateStamp
	validateLegacyRuntimeCandidateStampWithIdentityFn = tmux.ValidateLegacyRuntimeCandidateStampWithProcessIdentity
	captureLegacyRuntimeProcessIdentityFn             = tmux.CaptureProcessIdentity
	legacyRuntimeProcessIdentityMatchesFn             = tmux.ProcessIdentityMatches
	legacyRuntimeAdoptionBeforeConsumeFn              = func() {}
)

func (i *Instance) legacyRuntimeAdoptionAuthority(state statedb.RuntimeState, candidate tmux.RuntimeCandidate, bindings map[string]statedb.RuntimeBinding) tmux.LegacyRuntimeAdoption {
	bindingKind := activeRuntimeBindingKind(i)
	bindingValue := ""
	if binding, found := bindings[bindingKind]; found {
		bindingValue = binding.Value
	}
	startedUnixNano := int64(0)
	if !state.LastStartedAt.IsZero() {
		startedUnixNano = state.LastStartedAt.UnixNano()
	}
	return tmux.LegacyRuntimeAdoption{
		Candidate: candidate, StatusRevision: state.StatusRevision,
		Status: state.Status, StartedUnixNano: startedUnixNano,
		BindingKind: bindingKind, BindingValue: bindingValue,
	}
}

// PlanLegacyRuntimeAdoption is the read-only half of legacy recovery. It never
// stamps, starts, stops, or kills a runtime. The migration record, durable
// generation-0 tuple, complete socket inventory, and immutable tmux identity
// must all agree before a plan is returned.
func (i *Instance) PlanLegacyRuntimeAdoption() (LegacyRuntimeAdoptionPlan, error) {
	release, err := acquireInstanceSpawnLock(i.ID)
	if err != nil {
		return LegacyRuntimeAdoptionPlan{}, err
	}
	defer release()
	return i.planLegacyRuntimeAdoptionLocked()
}

// AdoptLegacyRuntime executes a freshly recomputed legacy recovery plan. The
// caller's explicit confirmation is represented by choosing this mutating API;
// ordinary startup reconciliation never calls it.
func (i *Instance) AdoptLegacyRuntime() (RuntimeReconciliationResult, error) {
	release, err := acquireInstanceSpawnLock(i.ID)
	if err != nil {
		return RuntimeReconciliationResult{}, err
	}
	defer release()

	plan, err := i.planLegacyRuntimeAdoptionLocked()
	if err != nil {
		return RuntimeReconciliationResult{}, err
	}
	db := i.restartPersistenceDB()
	incarnation := i.persistenceIncarnationSnapshot()
	bindings, _, err := i.readRuntimeBindingPlan(db, plan.State, true)
	if err != nil {
		return RuntimeReconciliationResult{}, err
	}
	authority := i.legacyRuntimeAdoptionAuthority(plan.State, plan.Candidate, bindings)
	if !plan.AlreadyStamped {
		if err := adoptLegacyRuntimeCandidateFn(authority); err != nil {
			return RuntimeReconciliationResult{}, fmt.Errorf("adopt migrated legacy runtime: %w", err)
		}
	}

	candidates, err := i.inventoryRuntimeCandidates(plan.State)
	if err != nil {
		return RuntimeReconciliationResult{}, err
	}
	result, err := i.reconcileRuntimeCandidatesLocked(db, plan.State, incarnation, candidates, true)
	if err != nil {
		return result, err
	}
	if !result.Live || result.State != plan.State {
		return result, i.runtimeAmbiguity("legacy adoption did not prove the durable generation-zero winner", candidates)
	}
	// Reinventory and revalidate after stamping/reconciliation. Marker
	// consumption is permitted only while the complete environment, durable
	// binding decision, and every local cleanup stamp still agree exactly.
	postCandidates, err := i.inventoryRuntimeCandidates(plan.State)
	if err != nil {
		return result, err
	}
	if len(postCandidates) != 1 {
		return result, i.runtimeAmbiguity("legacy adoption final proof requires exactly one candidate", postCandidates)
	}
	postCandidate := postCandidates[0]
	if postCandidate.InstanceID != plan.State.InstanceID || postCandidate.SessionName != plan.State.TmuxSession ||
		postCandidate.SocketName != plan.State.TmuxSocketName ||
		postCandidate.SessionID != plan.Candidate.SessionID || postCandidate.PaneID != plan.Candidate.PaneID ||
		postCandidate.PanePID != plan.Candidate.PanePID || postCandidate.PanePID <= 0 {
		return result, i.runtimeAmbiguity("legacy adoption final candidate changed identity", postCandidates)
	}
	verified, err := runtimeCandidateRevalidateFn(postCandidate)
	if err != nil {
		return result, i.runtimeAmbiguity("legacy adoption final revalidation failed: "+err.Error(), postCandidates)
	}
	if !sameRuntimeCandidateSnapshot(postCandidate, verified) {
		observed := append(append([]tmux.RuntimeCandidate(nil), postCandidates...), verified)
		return result, i.runtimeAmbiguity("legacy adoption final candidate changed during revalidation", observed)
	}
	postBindings, _, err := i.readRuntimeBindingPlan(db, plan.State, true)
	if err != nil {
		return result, err
	}
	finalAuthority := i.legacyRuntimeAdoptionAuthority(plan.State, verified, postBindings)
	// Capture once before the successful pre-consume proof. The same retained
	// lifetime remains authoritative across every BEGIN IMMEDIATE retry, the
	// staged marker deletion, the final proof, and COMMIT.
	retainedIdentity, err := captureLegacyRuntimeProcessIdentityFn(finalAuthority.Candidate.PanePID)
	if err != nil {
		return result, i.runtimeAmbiguity("legacy adoption final process identity capture failed: "+err.Error(), postCandidates)
	}
	defer func() { _ = retainedIdentity.Close() }()
	validateRetainedAuthority := func() error {
		if !legacyRuntimeProcessIdentityMatchesFn(retainedIdentity) {
			return tmux.ErrLegacyRuntimeCandidateChanged
		}
		if err := validateLegacyRuntimeCandidateStampWithIdentityFn(finalAuthority, retainedIdentity); err != nil {
			return err
		}
		if !legacyRuntimeProcessIdentityMatchesFn(retainedIdentity) {
			return tmux.ErrLegacyRuntimeCandidateChanged
		}
		return nil
	}
	if err := validateRetainedAuthority(); err != nil {
		return result, i.runtimeAmbiguity("legacy adoption final stamp proof failed: "+err.Error(), postCandidates)
	}
	legacyRuntimeAdoptionBeforeConsumeFn()
	if err := db.ConsumeLegacyRuntimeAdoptionWithCommitFence(
		plan.State, incarnation, postBindings, validateRetainedAuthority, validateRetainedAuthority,
	); err != nil {
		if errors.Is(err, tmux.ErrLegacyRuntimeCandidateChanged) {
			return result, i.runtimeAmbiguity("legacy adoption commit fence failed: "+err.Error(), postCandidates)
		}
		return result, fmt.Errorf("consume migrated legacy runtime adoption: %w", err)
	}
	return result, nil
}

func (i *Instance) planLegacyRuntimeAdoptionLocked() (LegacyRuntimeAdoptionPlan, error) {
	db := i.restartPersistenceDB()
	if db == nil {
		return LegacyRuntimeAdoptionPlan{}, fmt.Errorf("runtime database unavailable")
	}
	incarnation := i.persistenceIncarnationSnapshot()
	if err := db.ValidateInstanceIncarnation(i.ID, incarnation); err != nil {
		return LegacyRuntimeAdoptionPlan{}, err
	}
	durable, found, err := db.ReadRuntimeState(i.ID)
	if err != nil {
		return LegacyRuntimeAdoptionPlan{}, err
	}
	durable, found, err = i.reconcileReservedRuntimeBeforeCompatibilityLocked(db, durable, found, incarnation)
	if err != nil {
		return LegacyRuntimeAdoptionPlan{}, err
	}
	if !found {
		return LegacyRuntimeAdoptionPlan{}, fmt.Errorf("runtime state for %s is missing", i.ID)
	}
	eligible, found, err := db.ReadLegacyRuntimeAdoption(i.ID)
	if err != nil {
		return LegacyRuntimeAdoptionPlan{}, err
	}
	if !found {
		return LegacyRuntimeAdoptionPlan{}, fmt.Errorf("runtime %s has no pending migrated legacy adoption", i.ID)
	}
	if durable.Generation != 0 || durable.TmuxSession == "" ||
		eligible.InstanceID != durable.InstanceID || eligible.TmuxSession != durable.TmuxSession ||
		eligible.TmuxSocketName != durable.TmuxSocketName {
		return LegacyRuntimeAdoptionPlan{}, fmt.Errorf("migrated legacy adoption no longer matches durable runtime %s", i.ID)
	}
	if err := db.ValidateLegacyRuntimeAdoption(durable, incarnation); err != nil {
		return LegacyRuntimeAdoptionPlan{}, fmt.Errorf("validate migrated legacy runtime authority: %w", err)
	}
	bindings, _, err := i.readRuntimeBindingPlan(db, durable, true)
	if err != nil {
		return LegacyRuntimeAdoptionPlan{}, err
	}

	candidates, err := i.inventoryRuntimeCandidates(durable)
	if err != nil {
		return LegacyRuntimeAdoptionPlan{}, err
	}
	if len(candidates) != 1 {
		return LegacyRuntimeAdoptionPlan{}, i.runtimeAmbiguity("migrated legacy adoption requires exactly one candidate", candidates)
	}
	candidate := candidates[0]
	if candidate.InstanceID != durable.InstanceID || candidate.SessionName != durable.TmuxSession ||
		candidate.SocketName != durable.TmuxSocketName || candidate.PanePID <= 0 {
		return LegacyRuntimeAdoptionPlan{}, i.runtimeAmbiguity("legacy candidate does not match the migrated durable identity", candidates)
	}
	verified, err := runtimeCandidateRevalidateFn(candidate)
	if err != nil {
		return LegacyRuntimeAdoptionPlan{}, i.runtimeAmbiguity("legacy candidate revalidation failed: "+err.Error(), candidates)
	}
	if !sameRuntimeCandidateSnapshot(candidate, verified) {
		observed := append(append([]tmux.RuntimeCandidate(nil), candidates...), verified)
		return LegacyRuntimeAdoptionPlan{}, i.runtimeAmbiguity("legacy candidate changed during revalidation", observed)
	}
	alreadyStamped := verified.GenerationKnown && verified.Generation == 0 && verified.ProofError == ""
	if alreadyStamped {
		if err := validateLegacyRuntimeCandidateStampFn(i.legacyRuntimeAdoptionAuthority(durable, verified, bindings)); err != nil {
			return LegacyRuntimeAdoptionPlan{}, i.runtimeAmbiguity("already-stamped legacy candidate lacks exact durable authority: "+err.Error(), candidates)
		}
	} else if !tmux.IsUnstampedLegacyRuntimeCandidate(verified) {
		return LegacyRuntimeAdoptionPlan{}, i.runtimeAmbiguity("legacy candidate is not an unstamped migrated runtime", candidates)
	}
	return LegacyRuntimeAdoptionPlan{State: durable, Candidate: verified, AlreadyStamped: alreadyStamped}, nil
}
