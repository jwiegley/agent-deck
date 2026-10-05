package tmux

import (
	"fmt"
	"strconv"
	"strings"
)

// RuntimeCandidate is one live tmux pane that claims an Agent Deck logical
// instance. GenerationKnown is deliberately separate from Generation: zero is
// a valid legacy value, but an absent or malformed stamp is not ownership
// evidence and must never authorize adoption or cleanup.
type RuntimeCandidate struct {
	SessionID           string
	SessionName         string
	SocketName          string
	PaneID              string
	InstanceID          string
	Generation          uint64
	GenerationKnown     bool
	StatusRevision      uint64
	Status              string
	LastStartedUnixNano int64
	StateKnown          bool
	BindingKind         string
	BindingValue        string
	BindingKnown        bool
	PanePID             int
	ProofError          string
}

// RuntimeCandidateSocketSnapshot is one socket-complete inventory indexed by
// logical instance ID. Err makes an indeterminate socket explicit so callers
// preserve candidates instead of treating a failed probe as absence.
type RuntimeCandidateSocketSnapshot struct {
	CandidatesByInstance map[string][]RuntimeCandidate
	Err                  error
}

// RuntimeCandidateSnapshot contains one inventory for every requested socket.
type RuntimeCandidateSnapshot map[string]RuntimeCandidateSocketSnapshot

var runtimeCandidateSnapshotOutputFn = runBoundedOutput

const runtimeCandidateFormatFields = 12

func runtimeCandidateFormat() string {
	// The binding value is deliberately last: it is the only free-text field,
	// so SplitN preserves an embedded tmuxFieldSep byte. The effective
	// instance option before it only pre-filters unprefixed claims (see
	// admitStampedUnprefixedRuntimeCandidates); it is never authority.
	return tmuxFmt(
		"#{session_id}",
		"#{session_name}",
		"#{pane_id}",
		"#{E:AGENTDECK_INSTANCE_ID}",
		"#{E:AGENTDECK_RUNTIME_GENERATION}",
		"#{E:AGENTDECK_RUNTIME_STATUS_REVISION}",
		"#{E:AGENTDECK_RUNTIME_STATUS}",
		"#{E:AGENTDECK_RUNTIME_STARTED_UNIX_NANO}",
		"#{E:AGENTDECK_RUNTIME_BINDING_KIND}",
		"#{pane_pid}",
		"#{"+runtimeCleanupInstanceOption+"}",
		"#{E:AGENTDECK_RUNTIME_BINDING_VALUE}",
	)
}

// SnapshotRuntimeCandidates inventories every requested socket once. Each
// distinct socket costs one formatted list-sessions subprocess, plus one
// bounded local-option batch when an unprefixed session's options name the
// instance its environment claims (admitStampedUnprefixedRuntimeCandidates).
// If that batch fails because a session closed after the listing, the socket
// is listed once more and the vanished session is absent; any other failure
// leaves the socket indeterminate. The result is indexed by instance ID so
// startup reconciliation never rescans a socket for every persisted instance.
func SnapshotRuntimeCandidates(socketNames []string) RuntimeCandidateSnapshot {
	snapshot := make(RuntimeCandidateSnapshot, len(socketNames))
	for _, socketName := range socketNames {
		if _, found := snapshot[socketName]; found {
			continue
		}
		byInstance, err := snapshotRuntimeCandidatesOnSocket(socketName)
		snapshot[socketName] = RuntimeCandidateSocketSnapshot{
			CandidatesByInstance: byInstance,
			Err:                  err,
		}
	}
	return snapshot
}

func snapshotRuntimeCandidatesOnSocket(socketName string) (map[string][]RuntimeCandidate, error) {
	byInstance, claims, err := listRuntimeCandidatesOnSocket(socketName)
	if err != nil {
		return nil, err
	}
	admitErr := admitStampedUnprefixedRuntimeCandidates(socketName, claims, byInstance)
	if admitErr == nil {
		return byInstance, nil
	}
	// tmux drops the rest of a command list after one command fails, and a
	// server exits with its last session: a claim that closed after the
	// listing can fail the batch for every other claim. List once more; when
	// a claim has vanished, the batch is read again for what remains.
	relisted, relistedClaims, err := listRuntimeCandidatesOnSocket(socketName)
	if err != nil || !runtimeClaimVanished(claims, relistedClaims) {
		return nil, admitErr
	}
	if err := admitStampedUnprefixedRuntimeCandidates(socketName, relistedClaims, relisted); err != nil {
		return nil, err
	}
	return relisted, nil
}

// listRuntimeCandidatesOnSocket parses one formatted listing into the
// prefixed candidates, indexed by instance, and the unprefixed claims whose
// effective instance option names the instance their environment claims.
func listRuntimeCandidatesOnSocket(socketName string) (map[string][]RuntimeCandidate, []RuntimeCandidate, error) {
	out, err := runtimeCandidateSnapshotOutputFn(socketName, "list-sessions", "-F", runtimeCandidateFormat())
	if err != nil {
		if isEmptyTmuxServerResult(err) {
			return map[string][]RuntimeCandidate{}, nil, nil
		}
		return nil, nil, fmt.Errorf("tmux: snapshot runtime candidates: %w", err)
	}

	byInstance := make(map[string][]RuntimeCandidate)
	var claims []RuntimeCandidate
	for _, line := range strings.Split(strings.TrimSuffix(string(out), "\n"), "\n") {
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, tmuxFieldSep, runtimeCandidateFormatFields)
		if len(fields) != runtimeCandidateFormatFields {
			return nil, nil, fmt.Errorf("tmux: malformed runtime candidate record on socket %q", socketName)
		}
		sessionID, name := strings.TrimSpace(fields[0]), strings.TrimSpace(fields[1])
		paneID, instanceID := strings.TrimSpace(fields[2]), strings.TrimSpace(fields[3])
		if name == "" || instanceID == "" {
			continue
		}
		if !validTmuxStableID(sessionID, '$') || !validTmuxStableID(paneID, '%') {
			return nil, nil, fmt.Errorf("tmux: invalid stable runtime candidate identity on socket %q", socketName)
		}
		candidate := runtimeCandidateFromFields(socketName, fields)
		if !strings.HasPrefix(name, SessionPrefix) {
			if fields[10] == instanceID {
				claims = append(claims, candidate)
			}
			continue
		}
		byInstance[instanceID] = append(byInstance[instanceID], candidate)
	}
	return byInstance, claims, nil
}

func runtimeClaimVanished(listed, relisted []RuntimeCandidate) bool {
	remaining := make(map[string]bool, len(relisted))
	for _, claim := range relisted {
		remaining[claim.SessionID] = true
	}
	for _, claim := range listed {
		if !remaining[claim.SessionID] {
			return true
		}
	}
	return false
}

// admitStampedUnprefixedRuntimeCandidates admits a session whose name lacks
// SessionPrefix only on Agent Deck's session-local cleanup stamp for the same
// instance. Agent Deck starts a persisted runtime under whatever tmux name the
// instance carries (an imported or fixture name included) and stamps it, so
// the name is no evidence either way. Its #{E:} fields are not evidence
// either: tmux falls back to the server's global environment, which a server
// started from inside an Agent Deck pane inherits wholesale. The local stamp
// is the authority the cleanup inventory and every conditional mutation
// already require. Its batch covers only the claims whose effective instance
// option already names the claimed instance: a session whose own options
// carry that stamp always expands it, so no other claim can be admitted,
// however many user sessions an inherited global environment turns into
// claims.
func admitStampedUnprefixedRuntimeCandidates(socketName string, claims []RuntimeCandidate, byInstance map[string][]RuntimeCandidate) error {
	if len(claims) == 0 {
		return nil
	}
	identities := make([]RuntimeBindingCandidate, 0, len(claims))
	for _, claim := range claims {
		identities = append(identities, RuntimeBindingCandidate{SessionID: claim.SessionID})
	}
	localOptions, err := runtimeCleanupLocalOptionsFn(socketName, identities)
	if err != nil {
		return fmt.Errorf("tmux: snapshot runtime candidates: %w", err)
	}
	for _, claim := range claims {
		options := localOptions[claim.SessionID]
		stamped := true
		for _, name := range runtimeCleanupOptionNames {
			if !options[name].present {
				stamped = false
				break
			}
		}
		if stamped && options[runtimeCleanupInstanceOption].value == claim.InstanceID {
			byInstance[claim.InstanceID] = append(byInstance[claim.InstanceID], claim)
		}
	}
	return nil
}

func runtimeCandidateFromFields(socketName string, fields []string) RuntimeCandidate {
	candidate := RuntimeCandidate{
		SessionID:    strings.TrimSpace(fields[0]),
		SessionName:  strings.TrimSpace(fields[1]),
		SocketName:   socketName,
		PaneID:       strings.TrimSpace(fields[2]),
		InstanceID:   strings.TrimSpace(fields[3]),
		Status:       fields[6],
		BindingKind:  fields[8],
		BindingValue: fields[11],
	}
	if generation, err := strconv.ParseUint(strings.TrimSpace(fields[4]), 10, 64); err != nil {
		candidate.ProofError = "missing or invalid AGENTDECK_RUNTIME_GENERATION"
	} else {
		candidate.Generation = generation
		candidate.GenerationKnown = true
	}
	revision, revisionErr := strconv.ParseUint(strings.TrimSpace(fields[5]), 10, 64)
	started, startedErr := strconv.ParseInt(strings.TrimSpace(fields[7]), 10, 64)
	if revisionErr == nil {
		candidate.StatusRevision = revision
	}
	if startedErr == nil && started > 0 {
		candidate.LastStartedUnixNano = started
	}
	candidate.StateKnown = revisionErr == nil && candidate.Status != "" && startedErr == nil && started > 0
	// A formatted tmux expansion cannot distinguish an unset variable from a
	// deliberately empty binding kind. The selected pre-CAS candidate is
	// revalidated with show-environment before adoption, which recovers that
	// distinction for tools without a durable conversation binding.
	candidate.BindingKnown = candidate.BindingKind != ""
	if pid, err := strconv.Atoi(strings.TrimSpace(fields[9])); err != nil || pid <= 0 {
		candidate.ProofError = appendRuntimeProofError(candidate.ProofError, "invalid pane pid")
	} else {
		candidate.PanePID = pid
	}
	return candidate
}

// Candidates returns candidates for one logical instance across the requested
// sockets. A socket omitted from or failed in the snapshot is indeterminate,
// never an empty inventory.
func (s RuntimeCandidateSnapshot) Candidates(instanceID string, socketNames ...string) ([]RuntimeCandidate, error) {
	if instanceID == "" {
		return nil, fmt.Errorf("tmux: empty runtime instance id")
	}
	seenSocket := make(map[string]bool, len(socketNames))
	seenCandidate := make(map[string]bool)
	var candidates []RuntimeCandidate
	for _, socketName := range socketNames {
		if seenSocket[socketName] {
			continue
		}
		seenSocket[socketName] = true
		socketSnapshot, found := s[socketName]
		if !found {
			return nil, fmt.Errorf("tmux: socket %q was not included in runtime snapshot", socketName)
		}
		if socketSnapshot.Err != nil {
			return nil, socketSnapshot.Err
		}
		for _, candidate := range socketSnapshot.CandidatesByInstance[instanceID] {
			key := candidate.SocketName + "\x00" + candidate.SessionID + "\x00" + candidate.PaneID
			if seenCandidate[key] {
				continue
			}
			seenCandidate[key] = true
			candidates = append(candidates, candidate)
		}
	}
	return candidates, nil
}

// RevalidateRuntimeCandidate re-probes only a selected adoption target. It is
// intentionally not used while building the socket snapshot. It reads the
// session's own environment (show-environment without -g), so it needs no
// name convention: the snapshot already admitted the candidate.
func RevalidateRuntimeCandidate(candidate RuntimeCandidate) (RuntimeCandidate, error) {
	if candidate.InstanceID == "" || candidate.SessionName == "" {
		return RuntimeCandidate{}, fmt.Errorf("tmux: invalid runtime candidate identity")
	}
	target := candidate.SessionName
	if candidate.SessionID != "" {
		if !validTmuxStableID(candidate.SessionID, '$') || !validTmuxStableID(candidate.PaneID, '%') {
			return RuntimeCandidate{}, fmt.Errorf("tmux: invalid stable runtime candidate identity")
		}
		target = candidate.SessionID
	}
	envOut, err := runBoundedOutput(candidate.SocketName, "show-environment", "-t", target)
	if err != nil {
		return RuntimeCandidate{}, fmt.Errorf("tmux: revalidate runtime candidate %s: %w", candidate.SessionName, err)
	}
	env := parseRuntimeEnvironment(string(envOut))
	if env["AGENTDECK_INSTANCE_ID"] != candidate.InstanceID {
		return RuntimeCandidate{}, fmt.Errorf("tmux: runtime candidate %s changed logical identity", candidate.SessionName)
	}
	verified := runtimeCandidateFromEnvironment(candidate.SocketName, candidate.SessionName, candidate.InstanceID, env)
	identityTarget := target
	if candidate.PaneID != "" {
		identityTarget = candidate.PaneID
	}
	pidOut, pidErr := runBoundedOutput(candidate.SocketName, "display-message", "-t", identityTarget, "-p",
		tmuxFmt("#{session_id}", "#{session_name}", "#{pane_id}", "#{pane_pid}"))
	if pidErr != nil {
		verified.ProofError = appendRuntimeProofError(verified.ProofError, "pane pid unavailable")
	} else {
		fields := strings.Split(strings.TrimSpace(string(pidOut)), tmuxFieldSep)
		if len(fields) != 4 || !validTmuxStableID(fields[0], '$') || !validTmuxStableID(fields[2], '%') {
			verified.ProofError = appendRuntimeProofError(verified.ProofError, "invalid stable tmux identity")
		} else if pid, parseErr := strconv.Atoi(strings.TrimSpace(fields[3])); parseErr != nil || pid <= 0 {
			verified.ProofError = appendRuntimeProofError(verified.ProofError, "invalid pane pid")
		} else {
			verified.SessionID = fields[0]
			verified.SessionName = fields[1]
			verified.PaneID = fields[2]
			verified.PanePID = pid
		}
	}
	return verified, nil
}

// ProbeExactSession asks the server on socketName whether the session named
// exactly sessionName answers has-session. It is probeSessionExistence with an
// "=" target, which disables tmux's prefix and pattern matching, so a
// similarly named neighbour never answers for it. The answer is tri-state:
// (true, nil) is presence, (false, nil) is absence proved by tmux's canonical
// missing-session or no-server answer, and a non-nil error is indeterminate.
// A missing socket file is indeterminate too (see isMissingTmuxSocketResult).
func ProbeExactSession(socketName, sessionName string) (bool, error) {
	if sessionName == "" {
		return false, fmt.Errorf("tmux: empty session name to probe")
	}
	switch state, err := probeSessionExistence(socketName, "="+sessionName); state {
	case sessionExistencePresent:
		return true, nil
	case sessionExistenceAbsent:
		return false, nil
	default:
		return false, fmt.Errorf("tmux: probe session %q on socket %q: %w", sessionName, socketName, err)
	}
}

// SelectedRuntimeSessionExists asks whether the exact tmux session a durable
// runtime tuple names still answers, for a destruction whose inventories hold
// no provable candidate. It is ProbeExactSession with one deliberate
// difference: a missing socket file also reads as absence, as it does for the
// inventories that just came back empty (isEmptyTmuxServerResult). Where no
// server has run since boot there is no socket file at all, and a stop or a
// delete after a reboot must still complete. The cost is the trade-off
// isMissingTmuxSocketResult records: a live server whose socket file was
// unlinked (macOS's /tmp cleaner) or that runs under another TMUX_TMPDIR reads
// as gone, so the destruction records stopped over processes this process
// cannot reach. Every other failure is indeterminate and returned as an
// error, so a destructive caller refuses rather than recording a live process
// stopped.
func SelectedRuntimeSessionExists(socketName, sessionName string) (bool, error) {
	live, err := ProbeExactSession(socketName, sessionName)
	if err != nil && isMissingTmuxSocketResult(err) {
		return false, nil
	}
	return live, err
}

// ListRuntimeCandidates inventories exact AGENTDECK_INSTANCE_ID matches on one
// socket. It is read-only: incomplete candidates are returned with ProofError
// so reconciliation can preserve and report them instead of treating a failed
// probe as absence.
func ListRuntimeCandidates(socketName, instanceID string) ([]RuntimeCandidate, error) {
	if instanceID == "" {
		return nil, fmt.Errorf("tmux: empty runtime instance id")
	}
	byInstance, err := snapshotRuntimeCandidatesOnSocket(socketName)
	if err != nil {
		return nil, err
	}
	return byInstance[instanceID], nil
}

func runtimeCandidateFromEnvironment(socketName, sessionName, instanceID string, env map[string]string) RuntimeCandidate {
	candidate := RuntimeCandidate{SessionName: sessionName, SocketName: socketName, InstanceID: instanceID}
	generationText, present := env["AGENTDECK_RUNTIME_GENERATION"]
	if !present || strings.TrimSpace(generationText) == "" {
		candidate.ProofError = "missing AGENTDECK_RUNTIME_GENERATION"
	} else if generation, err := strconv.ParseUint(strings.TrimSpace(generationText), 10, 64); err != nil {
		candidate.ProofError = "invalid AGENTDECK_RUNTIME_GENERATION"
	} else {
		candidate.Generation = generation
		candidate.GenerationKnown = true
	}
	revisionText, revisionKnown := env["AGENTDECK_RUNTIME_STATUS_REVISION"]
	status, statusKnown := env["AGENTDECK_RUNTIME_STATUS"]
	startedText, startedKnown := env["AGENTDECK_RUNTIME_STARTED_UNIX_NANO"]
	revision, revisionErr := strconv.ParseUint(strings.TrimSpace(revisionText), 10, 64)
	started, startedErr := strconv.ParseInt(strings.TrimSpace(startedText), 10, 64)
	if revisionKnown && revisionErr == nil {
		candidate.StatusRevision = revision
	}
	if startedKnown && startedErr == nil && started > 0 {
		candidate.LastStartedUnixNano = started
	}
	candidate.Status = status
	candidate.StateKnown = revisionKnown && revisionErr == nil && statusKnown && status != "" && startedKnown && startedErr == nil && started > 0
	candidate.BindingKind, candidate.BindingKnown = env["AGENTDECK_RUNTIME_BINDING_KIND"]
	if candidate.BindingKnown {
		var valueKnown bool
		candidate.BindingValue, valueKnown = env["AGENTDECK_RUNTIME_BINDING_VALUE"]
		if !valueKnown {
			candidate.ProofError = appendRuntimeProofError(candidate.ProofError, "missing runtime binding value")
		}
	}
	return candidate
}

func parseRuntimeEnvironment(output string) map[string]string {
	result := make(map[string]string)
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "-") {
			continue
		}
		if idx := strings.IndexByte(line, '='); idx > 0 {
			result[line[:idx]] = line[idx+1:]
		}
	}
	return result
}

func appendRuntimeProofError(current, next string) string {
	if current == "" {
		return next
	}
	return current + "; " + next
}
