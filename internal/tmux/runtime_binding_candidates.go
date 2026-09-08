package tmux

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

// RuntimeGenerationCandidate is one exact physical runtime for an Agent Deck
// logical instance. SessionID and PaneID are immutable tmux-server identities;
// GenerationKnown distinguishes a proved legacy generation zero from missing or
// malformed generation evidence.
type RuntimeGenerationCandidate struct {
	SessionName     string
	SessionID       string
	SocketName      string
	PaneID          string
	PanePID         int
	InstanceID      string
	Generation      uint64
	GenerationKnown bool
}

// RuntimeBindingCandidate is one Agent Deck tmux session advertising a tool
// binding. The tmux and pane IDs are immutable server-side identities; cleanup
// rechecks them together with the mutable name, process, and session-local stamp
// before it kills anything.
type RuntimeBindingCandidate struct {
	SessionName     string
	SessionID       string
	SocketName      string
	PaneID          string
	PanePID         int
	InstanceID      string
	InstanceKnown   bool
	Generation      uint64
	GenerationKnown bool
	BindingKey      string
	BindingValue    string
}

// LegacyRuntimeAdoption is the complete generation-zero authority to publish
// onto one migration-selected, previously unstamped runtime. Candidate must be
// the exact stable identity captured by the migration inventory.
type LegacyRuntimeAdoption struct {
	Candidate       RuntimeCandidate
	StatusRevision  uint64
	Status          string
	StartedUnixNano int64
	BindingKind     string
	BindingValue    string
}

var ErrRuntimeGenerationCandidateChanged = errors.New("tmux: runtime generation candidate changed")

var ErrRuntimeBindingCandidateChanged = errors.New("tmux: runtime binding candidate changed")

var ErrLegacyRuntimeCandidateChanged = errors.New("tmux: legacy runtime candidate changed")

const runtimeBindingCandidateKillTimeout = 750 * time.Millisecond

const runtimeGenerationEnvironment = "AGENTDECK_RUNTIME_GENERATION"

const (
	runtimeCleanupInstanceOption   = "@agentdeck_runtime_instance_id"
	runtimeCleanupGenerationOption = "@agentdeck_runtime_generation"
	runtimeCleanupBindingKeyOption = "@agentdeck_runtime_binding_key"
	runtimeCleanupBindingValOption = "@agentdeck_runtime_binding_value"
)

var (
	runtimeBindingCandidateOutputFn    = runBoundedOutput
	runtimeCleanupLocalOptionsFn       = readRuntimeCleanupLocalOptions
	legacyRuntimeCandidateRevalidateFn = RevalidateRuntimeCandidate
	runtimeBindingConditionalKillFn    = func(ctx context.Context, socketName string, args ...string) ([]byte, error) {
		cmd := tmuxExecContext(ctx, socketName, args...)
		cmd.WaitDelay = 100 * time.Millisecond
		return cmd.Output()
	}
	runtimeGenerationProcessTreeFn = func(socketName, paneID string) ([]int, error) {
		_, pids, err := paneProcessTreeForPaneID(socketName, paneID)
		return pids, err
	}
	runtimeGenerationEnsurePIDsDeadFn = EnsureProcessIdentitiesDead
	runtimeCleanupStampFn             = func(session *Session, args ...string) error {
		return session.runBoundedMutation(args...)
	}
)

const runtimeCleanupCandidateFormatFields = 4

var runtimeCleanupOptionNames = [...]string{
	runtimeCleanupInstanceOption,
	runtimeCleanupGenerationOption,
	runtimeCleanupBindingKeyOption,
	runtimeCleanupBindingValOption,
}

const runtimeCleanupLocalOptionMarkerPrefix = "agent-deck-runtime-local-option:"

type runtimeCleanupLocalOption struct {
	value   string
	present bool
}

func runtimeCleanupCandidateFormat() string {
	return tmuxFmt(
		"#{session_id}",
		"#{session_name}",
		"#{pane_id}",
		"#{pane_pid}",
	)
}

// StampRuntimeCleanupIdentity publishes cleanup authority as session-local user
// options. Generation is unset first and restored last so a partial restamp
// remains incomplete and fail-closed.
func StampRuntimeCleanupIdentity(session *Session, instanceID string, generation uint64, bindingKey, bindingValue string) error {
	if session == nil || instanceID == "" || session.Name == "" {
		return fmt.Errorf("tmux: invalid runtime cleanup stamp target")
	}
	if bindingKey != "" && !validTmuxEnvironmentKey(bindingKey) {
		return fmt.Errorf("tmux: invalid runtime cleanup binding key")
	}
	if _, err := runtimeBindingFormatLiteral(instanceID); err != nil {
		return fmt.Errorf("tmux: invalid runtime cleanup instance id: %w", err)
	}
	if bindingValue != "" {
		if _, err := runtimeBindingFormatLiteral(bindingValue); err != nil {
			return fmt.Errorf("tmux: invalid runtime cleanup binding value: %w", err)
		}
	}
	return runtimeCleanupStampFn(session,
		"set-option", "-u", "-t", session.Name, runtimeCleanupGenerationOption,
		";", "set-option", "-t", session.Name, runtimeCleanupInstanceOption, instanceID,
		";", "set-option", "-t", session.Name, runtimeCleanupBindingKeyOption, bindingKey,
		";", "set-option", "-t", session.Name, runtimeCleanupBindingValOption, bindingValue,
		";", "set-option", "-t", session.Name, runtimeCleanupGenerationOption, strconv.FormatUint(generation, 10),
	)
}

// IsUnstampedLegacyRuntimeCandidate reports the one incomplete-generation
// state that migration may adopt. Invalid, ambiguous, and compound proof
// failures remain ineligible.
func IsUnstampedLegacyRuntimeCandidate(candidate RuntimeCandidate) bool {
	return !candidate.GenerationKnown && candidate.ProofError == "missing AGENTDECK_RUNTIME_GENERATION"
}

// AdoptLegacyRuntimeCandidate atomically publishes a complete generation-zero
// runtime tuple onto one explicitly migration-authorized legacy candidate. The
// caller must retain its migration/lifecycle authority from inventory through
// this call. The tmux-server conditional rechecks the captured physical and
// logical identity and both absent generation markers before any write. Every
// mutation targets the immutable session ID, and the environment generation is
// written last as the final completeness marker.
func AdoptLegacyRuntimeCandidate(adoption LegacyRuntimeAdoption) error {
	candidate := adoption.Candidate
	if !IsUnstampedLegacyRuntimeCandidate(candidate) || candidate.Generation != 0 {
		return fmt.Errorf("tmux: legacy runtime candidate generation is not exactly missing")
	}
	if candidate.SessionName == "" || !strings.HasPrefix(candidate.SessionName, SessionPrefix) ||
		!validTmuxStableID(candidate.SessionID, '$') || !validTmuxStableID(candidate.PaneID, '%') ||
		candidate.PanePID <= 0 || candidate.InstanceID == "" {
		return fmt.Errorf("tmux: invalid stable legacy runtime candidate identity")
	}
	if adoption.Status == "" || adoption.StartedUnixNano <= 0 {
		return fmt.Errorf("tmux: incomplete generation-zero runtime tuple")
	}
	bindingKey, err := legacyRuntimeBindingEnvironmentKey(adoption.BindingKind)
	if err != nil {
		return err
	}
	// A known tool may deliberately have no binding value (a release
	// decision). A value without a known kind is not a complete decision.
	if adoption.BindingKind == "" && adoption.BindingValue != "" {
		return fmt.Errorf("tmux: incomplete generation-zero binding decision")
	}
	for _, value := range []string{candidate.SessionName, candidate.InstanceID} {
		if _, err := runtimeBindingFormatLiteral(value); err != nil {
			return err
		}
	}
	if adoption.BindingValue != "" {
		if _, err := runtimeBindingFormatLiteral(adoption.BindingValue); err != nil {
			return fmt.Errorf("tmux: invalid generation-zero binding value: %w", err)
		}
	}

	condition, err := legacyRuntimeAdoptionCondition(candidate)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), runtimeBindingCandidateKillTimeout)
	defer cancel()
	const mismatchMessage = "agent-deck-legacy-runtime-candidate-changed"
	args := []string{
		"if-shell", "-F", "-t", candidate.PaneID, condition,
		legacyRuntimeAdoptionCommand(adoption, bindingKey),
		"display-message -p " + mismatchMessage,
	}
	out, err := runtimeBindingConditionalKillFn(ctx, candidate.SocketName, args...)
	if err != nil {
		if ctx.Err() != nil {
			return annotateDeadline(ctx.Err(), err)
		}
		return fmt.Errorf("%w: conditional legacy adoption failed: %v", ErrLegacyRuntimeCandidateChanged, err)
	}
	if len(out) != 0 {
		return ErrLegacyRuntimeCandidateChanged
	}
	return nil
}

// ValidateLegacyRuntimeCandidateStamp proves that a revalidated candidate has
// the exact complete generation-zero environment and session-local cleanup
// authority selected from the durable migration state. It retains one process
// identity across presence-sensitive probes on both sides of the tmux-server
// conditional, so PID reuse and present-empty-to-unset drift fail closed.
func ValidateLegacyRuntimeCandidateStamp(adoption LegacyRuntimeAdoption) error {
	candidate := adoption.Candidate
	if candidate.PanePID <= 0 {
		return ErrLegacyRuntimeCandidateChanged
	}
	identity, err := CaptureProcessIdentity(candidate.PanePID)
	if err != nil {
		return fmt.Errorf("%w: capture pane process identity: %v", ErrLegacyRuntimeCandidateChanged, err)
	}
	defer func() { _ = identity.Close() }()
	return ValidateLegacyRuntimeCandidateStampWithProcessIdentity(adoption, identity)
}

// ValidateLegacyRuntimeCandidateStampWithProcessIdentity performs the exact
// stamp proof against one process lifetime captured by the caller. It does not
// take ownership of identity, allowing a commit fence to retain the same
// handle through marker deletion and COMMIT.
func ValidateLegacyRuntimeCandidateStampWithProcessIdentity(adoption LegacyRuntimeAdoption, identity ProcessIdentity) error {
	candidate := adoption.Candidate
	bindingKey, err := legacyRuntimeBindingEnvironmentKey(adoption.BindingKind)
	if err != nil {
		return err
	}
	if adoption.BindingKind == "" && adoption.BindingValue != "" {
		return fmt.Errorf("tmux: incomplete generation-zero binding decision")
	}
	if !validTmuxStableID(candidate.SessionID, '$') || !validTmuxStableID(candidate.PaneID, '%') ||
		candidate.SessionName == "" || !strings.HasPrefix(candidate.SessionName, SessionPrefix) ||
		candidate.InstanceID == "" || candidate.PanePID <= 0 ||
		!identity.Valid() || identity.PID != candidate.PanePID {
		return ErrLegacyRuntimeCandidateChanged
	}
	if !candidate.GenerationKnown || candidate.Generation != 0 || candidate.ProofError != "" ||
		!candidate.StateKnown || candidate.StatusRevision != adoption.StatusRevision ||
		candidate.Status != adoption.Status || candidate.LastStartedUnixNano != adoption.StartedUnixNano ||
		!candidate.BindingKnown || candidate.BindingKind != adoption.BindingKind ||
		candidate.BindingValue != adoption.BindingValue {
		return ErrLegacyRuntimeCandidateChanged
	}
	if !ProcessIdentityMatches(identity) {
		return ErrLegacyRuntimeCandidateChanged
	}
	if err := validateLegacyRuntimeCandidateStampSnapshot(adoption, bindingKey); err != nil {
		return err
	}
	if !ProcessIdentityMatches(identity) {
		return ErrLegacyRuntimeCandidateChanged
	}
	return nil
}

func validateLegacyRuntimeCandidateStampSnapshot(adoption LegacyRuntimeAdoption, bindingKey string) error {
	candidate := adoption.Candidate
	validateExactSnapshot := func() error {
		verified, err := legacyRuntimeCandidateRevalidateFn(candidate)
		if err != nil {
			return fmt.Errorf("%w: revalidate exact legacy runtime stamp: %v", ErrLegacyRuntimeCandidateChanged, err)
		}
		// RuntimeCandidate records presence separately from value. Direct
		// equality therefore rejects an environment variable changing from a
		// deliberately present empty value to unset (or the reverse).
		if verified != candidate {
			return ErrLegacyRuntimeCandidateChanged
		}
		return validateRuntimeCleanupLocalIdentity(RuntimeGenerationCandidate{
			SessionName: candidate.SessionName, SessionID: candidate.SessionID,
			SocketName: candidate.SocketName, PaneID: candidate.PaneID, PanePID: candidate.PanePID,
			InstanceID: candidate.InstanceID, Generation: candidate.Generation,
			GenerationKnown: candidate.GenerationKnown,
		}, bindingKey, adoption.BindingValue, true, ErrLegacyRuntimeCandidateChanged)
	}
	if err := validateExactSnapshot(); err != nil {
		return err
	}

	condition, err := legacyRuntimeStampCondition(adoption, bindingKey)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), runtimeBindingCandidateKillTimeout)
	defer cancel()
	const mismatchMessage = "agent-deck-legacy-runtime-candidate-changed"
	args := []string{
		"if-shell", "-F", "-t", candidate.PaneID, condition,
		"display-message agent-deck-legacy-runtime-stamp-valid",
		"display-message -p " + mismatchMessage,
	}
	out, err := runtimeBindingConditionalKillFn(ctx, candidate.SocketName, args...)
	if err != nil {
		if ctx.Err() != nil {
			return annotateDeadline(ctx.Err(), err)
		}
		return fmt.Errorf("%w: conditional legacy stamp proof failed: %v", ErrLegacyRuntimeCandidateChanged, err)
	}
	if len(out) != 0 {
		return ErrLegacyRuntimeCandidateChanged
	}
	// Tmux format equality cannot distinguish an unset variable or option from
	// a present empty one. Repeat the presence-sensitive environment and local
	// option probes after the atomic identity conditional closes that gap.
	return validateExactSnapshot()
}

// ListRuntimeGenerationCandidates inventories exact logical-instance matches
// on one tmux socket. Stable session, pane, and process identity is captured
// before any cleanup decision; incomplete generation evidence remains explicit
// so callers preserve that candidate.
func ListRuntimeGenerationCandidates(socketName, instanceID string) ([]RuntimeGenerationCandidate, error) {
	if instanceID == "" {
		return nil, nil
	}
	inventory, err := ListRuntimeCleanupCandidates(socketName, "")
	if err != nil {
		return nil, err
	}
	candidates := make([]RuntimeGenerationCandidate, 0, len(inventory))
	for _, item := range inventory {
		if item.InstanceID == instanceID {
			candidates = append(candidates, runtimeGenerationCandidateFromBinding(item))
		}
	}
	return candidates, nil
}

// ListRuntimeBindingCandidates inventories one binding on one socket without
// making cleanup decisions. A failed or malformed formatted inventory is
// indeterminate, so callers preserve every candidate.
func ListRuntimeBindingCandidates(socketName, envKey, envValue string) ([]RuntimeBindingCandidate, error) {
	if envKey == "" || envValue == "" {
		return nil, nil
	}
	inventory, err := ListRuntimeCleanupCandidates(socketName, envKey)
	if err != nil {
		return nil, err
	}
	candidates := make([]RuntimeBindingCandidate, 0, len(inventory))
	for _, item := range inventory {
		if item.BindingKey == envKey && item.BindingValue == envValue {
			candidates = append(candidates, item)
		}
	}
	return candidates, nil
}

// ListRuntimeCleanupCandidates inventories every Agent Deck session on exactly
// one socket. It uses one stable-identity call and one bounded local-option
// batch, and preserves socketName verbatim: "" is the native tmux default
// socket, even when Agent Deck has a different configured DefaultSocketName.
func ListRuntimeCleanupCandidates(socketName, envKey string) ([]RuntimeBindingCandidate, error) {
	if envKey != "" && !validTmuxEnvironmentKey(envKey) {
		return nil, fmt.Errorf("tmux: invalid binding inventory environment key")
	}
	out, err := runtimeBindingCandidateOutputFn(socketName, "list-sessions", "-F", runtimeCleanupCandidateFormat())
	if err != nil {
		if isEmptyTmuxServerResult(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("tmux: list binding candidates: %w", err)
	}

	var identities []RuntimeBindingCandidate
	if strings.TrimSpace(string(out)) == "" {
		return nil, nil
	}
	for _, line := range strings.Split(strings.TrimSuffix(string(out), "\n"), "\n") {
		fields := strings.SplitN(line, tmuxFieldSep, runtimeCleanupCandidateFormatFields)
		if len(fields) != runtimeCleanupCandidateFormatFields {
			return nil, fmt.Errorf("tmux: malformed runtime cleanup candidate record on socket %q", socketName)
		}
		sessionID, name := strings.TrimSpace(fields[0]), strings.TrimSpace(fields[1])
		if name == "" || !strings.HasPrefix(name, SessionPrefix) {
			continue
		}
		if !validTmuxStableID(sessionID, '$') {
			return nil, fmt.Errorf("tmux: invalid binding candidate session id %q", sessionID)
		}
		paneID := strings.TrimSpace(fields[2])
		if !validTmuxStableID(paneID, '%') {
			return nil, fmt.Errorf("tmux: malformed binding candidate pane %q", paneID)
		}
		panePID, parseErr := strconv.Atoi(strings.TrimSpace(fields[3]))
		if parseErr != nil || panePID <= 0 {
			return nil, fmt.Errorf("tmux: invalid binding candidate pane pid %q", fields[3])
		}
		identities = append(identities, RuntimeBindingCandidate{
			SessionName: name, SessionID: sessionID, SocketName: socketName,
			PaneID: paneID, PanePID: panePID,
		})
	}

	localOptions, err := runtimeCleanupLocalOptionsFn(socketName, identities)
	if err != nil {
		return nil, err
	}
	candidates := make([]RuntimeBindingCandidate, 0, len(identities))
	for _, candidate := range identities {
		options := localOptions[candidate.SessionID]
		complete := true
		for _, name := range runtimeCleanupOptionNames {
			if !options[name].present {
				complete = false
				break
			}
		}
		if !complete {
			continue
		}
		candidate.InstanceID = options[runtimeCleanupInstanceOption].value
		candidate.InstanceKnown = candidate.InstanceID != ""
		if generation, parseErr := strconv.ParseUint(strings.TrimSpace(options[runtimeCleanupGenerationOption].value), 10, 64); parseErr == nil {
			candidate.Generation = generation
			candidate.GenerationKnown = true
		}
		candidate.BindingKey = options[runtimeCleanupBindingKeyOption].value
		candidate.BindingValue = options[runtimeCleanupBindingValOption].value
		candidates = append(candidates, candidate)
	}
	return candidates, nil
}

// readRuntimeCleanupLocalOptions reads only options set on each session. tmux
// format expansion returns inherited global values, while show-options without
// -A uses options_get_only and therefore provides the required local provenance.
// All commands share one bounded tmux client, independent of session count.
func readRuntimeCleanupLocalOptions(socketName string, candidates []RuntimeBindingCandidate) (map[string]map[string]runtimeCleanupLocalOption, error) {
	result := make(map[string]map[string]runtimeCleanupLocalOption, len(candidates))
	if len(candidates) == 0 {
		return result, nil
	}
	var args []string
	for _, candidate := range candidates {
		for _, option := range runtimeCleanupOptionNames {
			if len(args) != 0 {
				args = append(args, ";")
			}
			args = append(args,
				"display-message", "-p", "-t", candidate.SessionID,
				runtimeCleanupLocalOptionMarker(candidate.SessionID, option),
				";", "show-options", "-qv", "-t", candidate.SessionID, option,
			)
		}
	}
	out, err := runtimeBindingCandidateOutputFn(socketName, args...)
	if err != nil {
		return nil, fmt.Errorf("tmux: read local runtime cleanup options: %w", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	line := 0
	for _, candidate := range candidates {
		options := make(map[string]runtimeCleanupLocalOption, len(runtimeCleanupOptionNames))
		result[candidate.SessionID] = options
		for _, option := range runtimeCleanupOptionNames {
			marker := runtimeCleanupLocalOptionMarker(candidate.SessionID, option)
			if line >= len(lines) || lines[line] != marker {
				return nil, fmt.Errorf("tmux: malformed local runtime cleanup options on socket %q", socketName)
			}
			line++
			if line < len(lines) && !strings.HasPrefix(lines[line], runtimeCleanupLocalOptionMarkerPrefix) {
				options[option] = runtimeCleanupLocalOption{value: lines[line], present: true}
				line++
			}
		}
	}
	if line != len(lines) {
		return nil, fmt.Errorf("tmux: trailing local runtime cleanup options on socket %q", socketName)
	}
	return result, nil
}

func runtimeCleanupLocalOptionMarker(sessionID, option string) string {
	return runtimeCleanupLocalOptionMarkerPrefix + sessionID + ":" + option
}

// KillRuntimeGenerationCandidate executes one bounded tmux-server conditional:
// the exact logical instance, generation, stable tmux IDs, mutable name, and
// pane process must all still match before tmux kills the stable session ID.
// The exact pane's process tree and kernel birth identities are captured
// immediately before that conditional and reaped only after a successful
// conditional kill. wait selects synchronous versus background reaping. The
// caller must hold the logical instance's lifecycle lock from inventory through
// this call so Agent Deck cannot restamp the candidate between its local-option
// read and the tmux conditional.
func KillRuntimeGenerationCandidate(candidate RuntimeGenerationCandidate, wait bool) error {
	condition, err := runtimeGenerationCandidateCondition(candidate)
	if err != nil {
		return err
	}
	if err := validateRuntimeCleanupLocalIdentity(
		candidate, "", "", false, ErrRuntimeGenerationCandidateChanged,
	); err != nil {
		return err
	}
	identities, err := captureStableProcessTree(func() ([]int, error) {
		return runtimeGenerationProcessTreeFn(candidate.SocketName, candidate.PaneID)
	}, candidate.PanePID, ErrRuntimeGenerationCandidateChanged)
	if err != nil {
		return fmt.Errorf("tmux: capture runtime generation candidate process tree: %w", err)
	}
	owned := true
	defer func() {
		if owned {
			CloseProcessIdentities(identities)
		}
	}()
	if err := executeRuntimeCandidateConditionalKill(
		candidate, condition,
		"agent-deck-runtime-generation-candidate-changed",
		ErrRuntimeGenerationCandidateChanged,
	); err != nil {
		return err
	}
	ensureDead := runtimeGenerationEnsurePIDsDeadFn
	if wait {
		owned = false
		ensureDead(identities, 3*time.Second)
	} else {
		owned = false
		go func() {
			ensureDead(identities, 3*time.Second)
		}()
	}
	return nil
}

// KillRuntimeBindingCandidate revalidates the session-scoped stamped binding
// pair before applying the same exact built-in tmux identity condition used
// above. Inherited globals never become cleanup authority. The caller must hold
// both the binding-owner and target lifecycle locks from inventory through this
// call.
func KillRuntimeBindingCandidate(candidate RuntimeBindingCandidate) error {
	condition, err := runtimeBindingCandidateCondition(candidate)
	if err != nil {
		return err
	}
	generation := runtimeGenerationCandidateFromBinding(candidate)
	// Reject local-stamp mismatches before acquiring signal authority. The
	// conditional repeats this check after tree capture to close the restamp
	// window immediately before mutation.
	if err := validateRuntimeCleanupLocalIdentity(
		generation, candidate.BindingKey, candidate.BindingValue, true,
		ErrRuntimeBindingCandidateChanged,
	); err != nil {
		return err
	}
	identities, err := captureStableProcessTree(func() ([]int, error) {
		return runtimeGenerationProcessTreeFn(candidate.SocketName, candidate.PaneID)
	}, candidate.PanePID, ErrRuntimeBindingCandidateChanged)
	if err != nil {
		return fmt.Errorf("tmux: capture runtime binding candidate process tree: %w", err)
	}
	owned := true
	defer func() {
		if owned {
			CloseProcessIdentities(identities)
		}
	}()
	if err := executeRuntimeCandidateConditionalKill(
		generation, condition,
		"agent-deck-runtime-binding-candidate-changed",
		ErrRuntimeBindingCandidateChanged,
	); err != nil {
		return err
	}
	ensureDead := runtimeGenerationEnsurePIDsDeadFn
	owned = false
	go func() {
		ensureDead(identities, 3*time.Second)
	}()
	return nil
}

// InvalidateRuntimeGenerationCandidate removes both generation completeness
// markers, but only while the exact physical and logical candidate still
// matches. The two unsets execute in one tmux-server command list so a client
// crash cannot leave either marker authorizing a replacement pane on its own.
func InvalidateRuntimeGenerationCandidate(candidate RuntimeGenerationCandidate) error {
	condition, err := runtimeGenerationCandidateCondition(candidate)
	if err != nil {
		return err
	}
	if err := validateRuntimeCleanupLocalIdentity(candidate, "", "", false, ErrRuntimeGenerationCandidateChanged); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), tmuxMutationTimeout)
	defer cancel()
	const mismatchMessage = "agent-deck-runtime-generation-candidate-changed"
	args := []string{
		"if-shell", "-F", "-t", candidate.PaneID, condition,
		runtimeGenerationMutationCommand(candidate),
		"display-message -p " + mismatchMessage,
	}
	out, err := runtimeBindingConditionalKillFn(ctx, candidate.SocketName, args...)
	if err != nil {
		if ctx.Err() != nil {
			return annotateDeadline(ctx.Err(), err)
		}
		return fmt.Errorf("%w: conditional generation invalidation failed: %v", ErrRuntimeGenerationCandidateChanged, err)
	}
	if len(out) != 0 {
		return ErrRuntimeGenerationCandidateChanged
	}
	return nil
}

// RespawnRuntimeGenerationCandidate invalidates the exact candidate's
// completeness markers immediately before replacing its process. The physical
// mutation targets the immutable pane ID, never the reusable session name.
func RespawnRuntimeGenerationCandidate(session *Session, candidate RuntimeGenerationCandidate, command string) error {
	if session == nil || session.SocketName != candidate.SocketName ||
		session.Name != candidate.SessionName || session.InstanceID != candidate.InstanceID {
		return ErrRuntimeGenerationCandidateChanged
	}
	condition, err := runtimeGenerationCandidateCondition(candidate)
	if err != nil {
		return err
	}
	respawnArgs := []string{"respawn-pane", "-k", "-t", candidate.PaneID}
	if command != "" {
		wrapped, wrapErr := wrapRespawnCommand(command)
		if wrapErr != nil {
			return wrapErr
		}
		respawnArgs = append(respawnArgs, wrapped...)
	}
	if err := validateRuntimeCleanupLocalIdentity(candidate, "", "", false, ErrRuntimeGenerationCandidateChanged); err != nil {
		return err
	}

	session.invalidateCache()
	oldIdentities, err := captureStableProcessTree(func() ([]int, error) {
		return runtimeGenerationProcessTreeFn(candidate.SocketName, candidate.PaneID)
	}, candidate.PanePID, ErrRuntimeGenerationCandidateChanged)
	if err != nil {
		return fmt.Errorf("tmux: capture stable process tree before respawn: %w", err)
	}
	oldPIDs := processIdentityPIDs(oldIdentities)
	respawnLog.Info("pre_respawn_process_tree", slog.Any("pids", oldPIDs))
	identitiesOwned := true
	defer func() {
		if identitiesOwned {
			CloseProcessIdentities(oldIdentities)
		}
	}()
	mutations := make([][]string, 0, 2)
	if session.clearOnRestart {
		mutations = append(mutations, []string{"clear-history", "-t", candidate.PaneID})
	}
	mutations = append(mutations, respawnArgs)

	ctx, cancel := context.WithTimeout(context.Background(), tmuxMutationTimeout)
	defer cancel()
	const mismatchMessage = "agent-deck-runtime-generation-candidate-changed"
	args := []string{
		"if-shell", "-F", "-t", candidate.PaneID, condition,
		runtimeGenerationMutationCommand(candidate, mutations...),
		"display-message -p " + mismatchMessage,
	}
	respawnLog.Debug("respawn_pane_executing", slog.Any("args", args))
	out, err := runtimeBindingConditionalKillFn(ctx, candidate.SocketName, args...)
	if err != nil {
		if ctx.Err() != nil {
			return annotateDeadline(ctx.Err(), err)
		}
		return fmt.Errorf("%w: conditional respawn failed: %v", ErrRuntimeGenerationCandidateChanged, err)
	}
	if len(out) != 0 {
		return ErrRuntimeGenerationCandidateChanged
	}
	if session.clearOnRestart {
		respawnLog.Info("cleared_scrollback", slog.String("session", candidate.SessionName))
	}

	newPIDs, newTreeErr := runtimeGenerationProcessTreeFn(candidate.SocketName, candidate.PaneID)
	identitiesOwned = false
	go func() {
		session.escalateAfterRespawn(oldIdentities, newPIDs, newTreeErr)
	}()
	// A control-mode client is attached to the session, not to the pane process,
	// so respawn-pane does not require reconnecting it. More importantly, a
	// reconnect by mutable session name after the conditional could disconnect
	// or attach to a same-name replacement created in the meantime.
	session.mu.Lock()
	session.startupAt = time.Now()
	session.lastStableStatus = "waiting"
	session.stateTracker = nil
	session.cachedPromptDetector = nil
	session.cachedPromptDetectorTool = ""
	session.mu.Unlock()
	return nil
}

func processIdentityPIDs(identities []ProcessIdentity) []int {
	pids := make([]int, 0, len(identities))
	for _, identity := range identities {
		pids = append(pids, identity.PID)
	}
	return pids
}

func runtimeGenerationMutationCommand(candidate RuntimeGenerationCandidate, trailing ...[]string) string {
	commands := [][]string{
		{"set-environment", "-u", "-t", candidate.SessionID, runtimeGenerationEnvironment},
		{"set-option", "-u", "-t", candidate.SessionID, runtimeCleanupGenerationOption},
	}
	commands = append(commands, trailing...)
	parts := make([]string, 0, len(commands))
	for _, command := range commands {
		quoted := make([]string, len(command))
		for index, arg := range command {
			quoted[index] = tmuxQuote(arg)
		}
		parts = append(parts, strings.Join(quoted, " "))
	}
	return strings.Join(parts, " ; ")
}

func executeRuntimeCandidateConditionalKill(
	candidate RuntimeGenerationCandidate,
	condition, mismatchMessage string,
	mismatchErr error,
) error {

	// The caller's local-only read rejects inherited globals. The built-in tmux
	// conditional below compares the same logical stamps again together
	// with stable session, pane, name, and process identity, closing the restamp
	// race between the read and mutation.
	ctx, cancel := context.WithTimeout(context.Background(), runtimeBindingCandidateKillTimeout)
	defer cancel()
	args := []string{
		"if-shell", "-F", "-t", candidate.PaneID, condition,
		fmt.Sprintf("kill-session -t '%s'", candidate.SessionID),
		"display-message -p " + mismatchMessage,
	}
	out, err := runtimeBindingConditionalKillFn(ctx, candidate.SocketName, args...)
	if err != nil {
		if ctx.Err() != nil {
			return annotateDeadline(ctx.Err(), err)
		}
		return fmt.Errorf("%w: conditional kill failed: %v", mismatchErr, err)
	}
	if len(out) != 0 {
		return mismatchErr
	}
	return nil
}

func validateRuntimeCleanupLocalIdentity(
	candidate RuntimeGenerationCandidate,
	bindingKey, bindingValue string,
	compareBinding bool,
	mismatchErr error,
) error {
	// Read only options physically present on this session. Inherited server
	// globals are never sufficient mutation authority.
	localOptions, err := runtimeCleanupLocalOptionsFn(candidate.SocketName, []RuntimeBindingCandidate{{
		SessionID: candidate.SessionID,
	}})
	if err != nil {
		return fmt.Errorf("%w: read local runtime cleanup stamp: %v", mismatchErr, err)
	}
	options := localOptions[candidate.SessionID]
	for _, name := range runtimeCleanupOptionNames {
		if !options[name].present {
			return mismatchErr
		}
	}
	if options[runtimeCleanupInstanceOption].value != candidate.InstanceID ||
		options[runtimeCleanupGenerationOption].value != strconv.FormatUint(candidate.Generation, 10) {
		return mismatchErr
	}
	if compareBinding && (options[runtimeCleanupBindingKeyOption].value != bindingKey ||
		options[runtimeCleanupBindingValOption].value != bindingValue) {
		return mismatchErr
	}
	return nil
}

func runtimeGenerationCandidateFromBinding(candidate RuntimeBindingCandidate) RuntimeGenerationCandidate {
	return RuntimeGenerationCandidate{
		SessionName: candidate.SessionName, SessionID: candidate.SessionID,
		SocketName: candidate.SocketName, PaneID: candidate.PaneID, PanePID: candidate.PanePID,
		InstanceID: candidate.InstanceID, Generation: candidate.Generation,
		GenerationKnown: candidate.GenerationKnown,
	}
}

func runtimeGenerationCandidateCondition(candidate RuntimeGenerationCandidate) (string, error) {
	checks, err := runtimeGenerationCandidateChecks(candidate)
	if err != nil {
		return "", err
	}
	return runtimeCandidateCondition(checks)
}

func runtimeGenerationCandidateChecks(candidate RuntimeGenerationCandidate) ([][2]string, error) {
	if candidate.SessionName == "" || !strings.HasPrefix(candidate.SessionName, SessionPrefix) {
		return nil, fmt.Errorf("tmux: invalid runtime cleanup target %q", candidate.SessionName)
	}
	if !validTmuxStableID(candidate.SessionID, '$') || !validTmuxStableID(candidate.PaneID, '%') || candidate.PanePID <= 0 {
		return nil, fmt.Errorf("tmux: invalid stable runtime cleanup identity")
	}
	if candidate.InstanceID == "" || !candidate.GenerationKnown {
		return nil, fmt.Errorf("tmux: incomplete runtime cleanup identity")
	}
	generation := strconv.FormatUint(candidate.Generation, 10)
	return [][2]string{
		{"session_id", candidate.SessionID},
		{"session_name", candidate.SessionName},
		{"pane_id", candidate.PaneID},
		{"pane_pid", strconv.Itoa(candidate.PanePID)},
		{runtimeCleanupInstanceOption, candidate.InstanceID},
		{runtimeCleanupGenerationOption, generation},
		{runtimeGenerationEnvironment, generation},
	}, nil
}

func runtimeBindingCandidateCondition(candidate RuntimeBindingCandidate) (string, error) {
	// Preserve the stronger foreign-binding contract: unlike same-instance
	// legacy cleanup, a generation-zero or incomplete foreign identity is not
	// sufficient authority to cross an instance boundary.
	if !candidate.InstanceKnown || !candidate.GenerationKnown || candidate.Generation == 0 {
		return "", fmt.Errorf("tmux: incomplete binding cleanup identity")
	}
	if !validTmuxEnvironmentKey(candidate.BindingKey) || candidate.BindingValue == "" {
		return "", fmt.Errorf("tmux: invalid binding cleanup environment")
	}
	checks, err := runtimeGenerationCandidateChecks(runtimeGenerationCandidateFromBinding(candidate))
	if err != nil {
		return "", err
	}
	checks = append(checks,
		[2]string{runtimeCleanupBindingKeyOption, candidate.BindingKey},
		[2]string{runtimeCleanupBindingValOption, candidate.BindingValue},
	)
	return runtimeCandidateCondition(checks)
}

func legacyRuntimeAdoptionCondition(candidate RuntimeCandidate) (string, error) {
	identity, err := runtimeCandidateCondition([][2]string{
		{"session_id", candidate.SessionID},
		{"session_name", candidate.SessionName},
		{"pane_id", candidate.PaneID},
		{"pane_pid", strconv.Itoa(candidate.PanePID)},
		{"E:AGENTDECK_INSTANCE_ID", candidate.InstanceID},
	})
	if err != nil {
		return "", err
	}
	return "#{&&:" + identity +
		",#{&&:#{==:#{E:" + runtimeGenerationEnvironment + "},}," +
		"#{==:#{" + runtimeCleanupGenerationOption + "},}}}", nil
}

func legacyRuntimeStampCondition(adoption LegacyRuntimeAdoption, bindingKey string) (string, error) {
	candidate := adoption.Candidate
	checks := [][2]string{
		{"session_id", candidate.SessionID},
		{"session_name", candidate.SessionName},
		{"pane_id", candidate.PaneID},
		{"pane_pid", strconv.Itoa(candidate.PanePID)},
		{"E:AGENTDECK_INSTANCE_ID", candidate.InstanceID},
		{"E:" + runtimeGenerationEnvironment, "0"},
		{"E:AGENTDECK_RUNTIME_STATUS_REVISION", strconv.FormatUint(adoption.StatusRevision, 10)},
		{"E:AGENTDECK_RUNTIME_STATUS", adoption.Status},
		{"E:AGENTDECK_RUNTIME_STARTED_UNIX_NANO", strconv.FormatInt(adoption.StartedUnixNano, 10)},
		{"E:AGENTDECK_RUNTIME_BINDING_KIND", adoption.BindingKind},
		{"E:AGENTDECK_RUNTIME_BINDING_VALUE", adoption.BindingValue},
		{runtimeCleanupInstanceOption, candidate.InstanceID},
		{runtimeCleanupGenerationOption, "0"},
		{runtimeCleanupBindingKeyOption, bindingKey},
		{runtimeCleanupBindingValOption, adoption.BindingValue},
	}
	return runtimeCandidateConditionAllowEmpty(checks)
}

func legacyRuntimeAdoptionCommand(adoption LegacyRuntimeAdoption, bindingKey string) string {
	candidate := adoption.Candidate
	commands := [][]string{
		{"set-environment", "-t", candidate.SessionID, "AGENTDECK_INSTANCE_ID", candidate.InstanceID},
		{"set-environment", "-t", candidate.SessionID, "AGENTDECK_RUNTIME_STATUS_REVISION", strconv.FormatUint(adoption.StatusRevision, 10)},
		{"set-environment", "-t", candidate.SessionID, "AGENTDECK_RUNTIME_STATUS", adoption.Status},
		{"set-environment", "-t", candidate.SessionID, "AGENTDECK_RUNTIME_STARTED_UNIX_NANO", strconv.FormatInt(adoption.StartedUnixNano, 10)},
		{"set-environment", "-t", candidate.SessionID, "AGENTDECK_RUNTIME_BINDING_KIND", adoption.BindingKind},
		{"set-environment", "-t", candidate.SessionID, "AGENTDECK_RUNTIME_BINDING_VALUE", adoption.BindingValue},
		{"set-option", "-t", candidate.SessionID, runtimeCleanupInstanceOption, candidate.InstanceID},
		{"set-option", "-t", candidate.SessionID, runtimeCleanupBindingKeyOption, bindingKey},
		{"set-option", "-t", candidate.SessionID, runtimeCleanupBindingValOption, adoption.BindingValue},
		{"set-option", "-t", candidate.SessionID, runtimeCleanupGenerationOption, "0"},
		{"set-environment", "-t", candidate.SessionID, runtimeGenerationEnvironment, "0"},
	}
	parts := make([]string, 0, len(commands))
	for _, command := range commands {
		quoted := make([]string, len(command))
		for index, arg := range command {
			quoted[index] = tmuxQuote(arg)
		}
		parts = append(parts, strings.Join(quoted, " "))
	}
	return strings.Join(parts, " ; ")
}

func legacyRuntimeBindingEnvironmentKey(kind string) (string, error) {
	switch kind {
	case "":
		return "", nil
	case "claude":
		return "CLAUDE_SESSION_ID", nil
	case "copilot":
		return "COPILOT_SESSION_ID", nil
	case "codex":
		return "CODEX_SESSION_ID", nil
	case "gemini":
		return "GEMINI_SESSION_ID", nil
	case "opencode":
		return "OPENCODE_SESSION_ID", nil
	default:
		return "", fmt.Errorf("tmux: invalid generation-zero binding kind %q", kind)
	}
}

func runtimeCandidateCondition(checks [][2]string) (string, error) {
	return runtimeCandidateConditionWithEmpty(checks, false)
}

func runtimeCandidateConditionAllowEmpty(checks [][2]string) (string, error) {
	return runtimeCandidateConditionWithEmpty(checks, true)
}

func runtimeCandidateConditionWithEmpty(checks [][2]string, allowEmpty bool) (string, error) {
	if len(checks) == 0 {
		return "", fmt.Errorf("tmux: empty runtime candidate condition")
	}
	parts := make([]string, 0, len(checks))
	for _, check := range checks {
		if allowEmpty && check[1] == "" {
			parts = append(parts, fmt.Sprintf("#{==:#{%s},}", check[0]))
			continue
		}
		literal, literalErr := runtimeBindingFormatLiteral(check[1])
		if literalErr != nil {
			return "", literalErr
		}
		parts = append(parts, fmt.Sprintf("#{==:#{%s},%s}", check[0], literal))
	}
	condition := parts[len(parts)-1]
	for index := len(parts) - 2; index >= 0; index-- {
		condition = "#{&&:" + parts[index] + "," + condition + "}"
	}
	return condition, nil
}

func runtimeBindingFormatLiteral(value string) (string, error) {
	// l: prevents nested format expansion. Reject its structural bytes rather
	// than trying to quote an identity we are about to use as kill authority.
	// The local-option batch reserves its marker prefix as framing; accepting a
	// value with that prefix would make presence and value lines ambiguous.
	if value == "" || strings.HasPrefix(value, runtimeCleanupLocalOptionMarkerPrefix) ||
		strings.ContainsAny(value, "#,}") || strings.ContainsAny(value, "\r\n\x00") {
		return "", fmt.Errorf("tmux: unsafe binding cleanup identity")
	}
	return "#{l:" + value + "}", nil
}

func validTmuxStableID(value string, prefix byte) bool {
	if len(value) < 2 || value[0] != prefix {
		return false
	}
	for index := 1; index < len(value); index++ {
		if value[index] < '0' || value[index] > '9' {
			return false
		}
	}
	return true
}

func validTmuxEnvironmentKey(value string) bool {
	if value == "" {
		return false
	}
	for index := range value {
		char := value[index]
		if (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '_' {
			return false
		}
	}
	return true
}
