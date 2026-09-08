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

const runtimeCandidateFormatFields = 11

func runtimeCandidateFormat() string {
	// The binding value is deliberately last: it is the only free-text field,
	// so SplitN preserves an embedded tmuxFieldSep byte.
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
		"#{E:AGENTDECK_RUNTIME_BINDING_VALUE}",
	)
}

// SnapshotRuntimeCandidates inventories every requested socket with one
// formatted list-sessions subprocess per distinct socket. The result is
// indexed by instance ID so startup reconciliation never rescans a socket for
// every persisted instance.
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
	out, err := runtimeCandidateSnapshotOutputFn(socketName, "list-sessions", "-F", runtimeCandidateFormat())
	if err != nil {
		if isEmptyTmuxServerResult(err) {
			return map[string][]RuntimeCandidate{}, nil
		}
		return nil, fmt.Errorf("tmux: snapshot runtime candidates: %w", err)
	}

	byInstance := make(map[string][]RuntimeCandidate)
	for _, line := range strings.Split(strings.TrimSuffix(string(out), "\n"), "\n") {
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, tmuxFieldSep, runtimeCandidateFormatFields)
		if len(fields) != runtimeCandidateFormatFields {
			return nil, fmt.Errorf("tmux: malformed runtime candidate record on socket %q", socketName)
		}
		sessionID, name := strings.TrimSpace(fields[0]), strings.TrimSpace(fields[1])
		paneID, instanceID := strings.TrimSpace(fields[2]), strings.TrimSpace(fields[3])
		if name == "" || !strings.HasPrefix(name, SessionPrefix) || instanceID == "" {
			continue
		}
		if !validTmuxStableID(sessionID, '$') || !validTmuxStableID(paneID, '%') {
			return nil, fmt.Errorf("tmux: invalid stable runtime candidate identity on socket %q", socketName)
		}
		candidate := runtimeCandidateFromFields(socketName, fields)
		byInstance[instanceID] = append(byInstance[instanceID], candidate)
	}
	return byInstance, nil
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
		BindingValue: fields[10],
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
// intentionally not used while building the socket snapshot.
func RevalidateRuntimeCandidate(candidate RuntimeCandidate) (RuntimeCandidate, error) {
	if candidate.InstanceID == "" || candidate.SessionName == "" || !strings.HasPrefix(candidate.SessionName, SessionPrefix) {
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
