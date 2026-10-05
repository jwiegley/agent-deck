package session

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"al.essio.dev/pkg/shellescape"

	"github.com/asheshgoplani/agent-deck/internal/procfd"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// codexRolloutExistsInHome reports whether Codex has flushed a rollout JSONL
// for the given session ID under codexHome/sessions. Used by buildCodexCommand
// to gate `codex resume <sid>` on a real on-disk rollout file (Issue #756).
//
// Codex layout: codexHome/sessions/YYYY/MM/DD/rollout-<ts>-<uuid>.jsonl
func codexRolloutExistsInHome(sessionID, codexHome string) bool {
	return codexRolloutPathInHome(sessionID, codexHome) != ""
}

// DetectCodexSession is the public wrapper for async Codex session detection
// Call this for restored sessions that don't have a session ID yet
func (i *Instance) DetectCodexSession() {
	i.detectCodexSessionAsync()
}

func (i *Instance) resolveCodexDetectionCandidate(sessionID string, probeErr error) string {
	sessionID = i.filterCodexProcessProbeCandidate(sessionID)
	if sessionID == "" && probeErr == nil {
		if threadID, live := i.liveCodexBootstrapEvidence(nil); live {
			i.recordCodexOwnership(threadID)
			return threadID
		}
		var pass StatusUpdatePass
		i.codexExclusions(&pass)
		codexBootstrapMu.Lock()
		defer codexBootstrapMu.Unlock()
		exclude := i.codexExclusions(&pass)
		if exclude == nil {
			return ""
		}
		sessionID = i.queryCodexSession(codexSessionExclusions{explicit: exclude}, true)
		i.recordCodexOwnership(sessionID)
		return sessionID
	}
	i.recordCodexOwnership(sessionID)
	return sessionID
}

// codexDetectionDelays is how long detectCodexSessionAsync waits before each
// attempt: a brief wait for Codex to initialize, then short retry delays.
var codexDetectionDelays = []time.Duration{time.Second, time.Second, 2 * time.Second}

// detectCodexSessionAsync detects the Codex session ID after startup. Codex
// stores sessions in ~/.codex/sessions/YYYY/MM/DD/*.jsonl, and the session ID
// is the UUID in the file name. Codex has no "session list" command, so the
// live-process probe and the filesystem scan find it.
//
// A start or restart launches it, and it outlives that transition. Each
// attempt therefore holds the instance spawn lock, as the status pass's Codex
// refresh does (updateCodexSessionForPass). A start, restart or stop holds
// that lock while it writes fields an attempt reads, without i.mu: the tmux
// wrapper (a fallback recreate replaces it, then configures and starts it),
// the sandbox container and CodexStartedAt among them. The first attempt runs
// once the launching transition has committed its runtime and released the
// lock, and it fixes the runtime generation detection serves. Once another
// transition has replaced that runtime, detection stops; the replacement's own
// start or restart detects for it.
func (i *Instance) detectCodexSessionAsync() {
	var served codexDetectionRuntime
	for attempt, delay := range codexDetectionDelays {
		time.Sleep(delay)
		if i.detectCodexSessionAttempt(attempt, &served) {
			return
		}
	}
	sessionLog.Warn("codex_detection_failed", slog.Int("attempts", len(codexDetectionDelays)))
}

// codexDetectionRuntime is the runtime generation detectCodexSessionAsync
// serves, fixed by its first attempt that holds the spawn lock.
type codexDetectionRuntime struct {
	generation uint64
	fixed      bool
}

// detectCodexSessionAttempt is one detection attempt, made under the instance
// spawn lock. It reports whether detection is over: the thread it found was
// published or refused, or the runtime it serves was replaced.
func (i *Instance) detectCodexSessionAttempt(attempt int, served *codexDetectionRuntime) bool {
	release, err := acquireInstanceSpawnLock(i.ID)
	if err != nil {
		sessionLog.Debug("codex_detection_lock_failed",
			slog.Int("attempt", attempt+1), slog.String("error", err.Error()))
		return false
	}
	defer release()

	observation := i.CaptureRuntimeBindingObservation("codex")
	if !served.fixed {
		served.generation, served.fixed = observation.generation, true
	} else if observation.generation != served.generation {
		sessionLog.Debug("codex_detection_superseded", slog.Int("attempt", attempt+1),
			slog.Uint64("generation", served.generation), slog.Uint64("current_generation", observation.generation))
		return true
	}
	sessionID, _, probeErr := i.queryCodexSessionFromProcessFiles(nil)
	sessionID = i.resolveCodexDetectionCandidate(sessionID, probeErr)
	if sessionID == "" {
		if probeErr != nil {
			sessionLog.Debug("codex_session_probe_undetermined", slog.Int("attempt", attempt+1), slog.Any("error", probeErr))
		} else {
			sessionLog.Debug("codex_session_not_found", slog.Int("attempt", attempt+1), slog.Int("total", len(codexDetectionDelays)))
		}
		return false
	}
	if err := i.publishRuntimeBindingObservationLocked(observation, sessionID, time.Now()); err != nil {
		sessionLog.Debug("codex_binding_rejected",
			slog.String("session_id", sessionID), slog.String("error", err.Error()))
		return true
	}

	// Store in tmux environment for restart
	i.mu.RLock()
	tmuxSession := i.tmuxSession
	i.mu.RUnlock()
	if tmuxSession != nil {
		if err := tmuxSession.SetEnvironment("CODEX_SESSION_ID", sessionID); err != nil {
			sessionLog.Warn("codex_set_env_failed", slog.String("error", err.Error()))
		}
	}

	sessionLog.Debug(
		"codex_session_detected",
		slog.String("session_id", sessionID),
		slog.Int("attempt", attempt+1),
	)
	return true
}

func getCodexHomeDir() string {
	return sandboxedToolDir("Codex home", getCodexHomeDirUnguarded())
}

func getCodexHomeDirUnguarded() string {
	if codexHome := strings.TrimSpace(os.Getenv("CODEX_HOME")); codexHome != "" {
		return ExpandPath(codexHome)
	}

	if cfg, err := LoadUserConfig(); err == nil && cfg != nil {
		profile := resolvedProcessProfile()
		if profileDir := cfg.GetProfileCodexConfigDir(profile); profileDir != "" {
			return profileDir
		}
		if cfg.Codex.ConfigDir != "" {
			return ExpandPath(cfg.Codex.ConfigDir)
		}
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), ".codex")
	}
	return filepath.Join(home, ".codex")
}

func isCodexHomeExplicit() bool {
	if strings.TrimSpace(os.Getenv("CODEX_HOME")) != "" {
		return true
	}
	cfg, err := LoadUserConfig()
	if err != nil || cfg == nil {
		return false
	}
	profile := resolvedProcessProfile()
	if cfg.GetProfileCodexConfigDir(profile) != "" {
		return true
	}
	return strings.TrimSpace(cfg.Codex.ConfigDir) != ""
}

// runWithTimeout runs op in a goroutine and waits up to timeout for it to
// complete. Returns true if op finished, false if it timed out. The
// abandoned goroutine continues running until op returns naturally; its
// effects on shared state after timeout are not consulted by callers, which
// must check the return value before reading any variables op may have
// written.
//
// Used to backstop FS operations under ~/.codex/sessions which can hang
// indefinitely on a stuck FS layer (kernel D-state during readdir on the
// WSL 9p path was observed on 2026-04-28; one thread held a dentry that
// the FS layer never released, blocking every agent-deck CLI command that
// transitively walked the codex sessions tree).
func runWithTimeout(timeout time.Duration, op func()) bool {
	done := make(chan struct{})
	go func() {
		defer close(done)
		op()
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// codexWalkDirTimeout caps the recursive walk over $CODEX_HOME/sessions/ in
// queryCodexSession. Healthy walks of a year's sessions complete in roughly
// one second; the bound is generous so a slow disk does not false-negative
// while still preventing indefinite hangs.
const codexWalkDirTimeout = 5 * time.Second

// codexSessionExclusions is a read-only view used by the fallback disk walk.
// Explicit caller maps preserve the public UpdateCodexSession contract. Shared
// ownership snapshots carry owner counts so the current tmux session's ID can
// be allowed without accidentally allowing a duplicate owned by a sibling.
type codexSessionExclusions struct {
	explicit map[string]bool
	ownedIDs map[string]int
	ownID    string
}

func (e codexSessionExclusions) contains(id string) bool {
	if e.explicit != nil {
		return e.explicit[id]
	}
	owners := e.ownedIDs[id]
	if id == e.ownID && owners == 1 {
		return false
	}
	return owners > 0
}

// queryCodexSession scans Codex sessions and returns the best candidate.
// Selection strategy:
//  1. Prefer sessions whose JSONL metadata matches this instance's project path.
//  2. Optionally allow unscoped fallback (no cwd metadata) for initial bootstrap.
//
// The caller must not hold i.mu. The inputs are read under it once, as in
// queryCodexSessionFromProcessFiles, and the walk, which can outlive its
// timeout, reads only that snapshot.
func (i *Instance) queryCodexSession(exclusions codexSessionExclusions, allowUnscoped bool) string {
	i.mu.RLock()
	codexHome, projectPath, codexStartedAt, instanceID := i.getCodexHomeDir(), i.ProjectPath, i.CodexStartedAt, i.ID
	i.mu.RUnlock()
	return queryCodexSessionWith(codexHome, projectPath, codexStartedAt, instanceID, exclusions, allowUnscoped)
}

// Test seam for a bounded walk that must not hold Instance.mu.
var codexWalkDir = filepath.WalkDir

func queryCodexSessionWith(codexHome, projectPath string, codexStartedAt int64, instanceID string, exclusions codexSessionExclusions, allowUnscoped bool) string {
	sessionsDir := filepath.Join(codexHome, "sessions")
	if _, err := os.Stat(sessionsDir); os.IsNotExist(err) {
		return ""
	}

	uuidPattern := uuidPatternRE

	var bestScopedID string
	var bestScopedTime time.Time
	var bestUnscopedID string
	var bestUnscopedTime time.Time

	normalizedProjectPath := normalizePath(projectPath)

	var walkErr error
	if !runWithTimeout(codexWalkDirTimeout, func() {
		walkErr = codexWalkDir(sessionsDir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil // Skip errors
			}

			if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
				return nil
			}

			sessionID := uuidPattern.FindString(d.Name())
			if sessionID == "" {
				return nil
			}
			if exclusions.contains(sessionID) {
				return nil
			}

			// Subagent-thread gate (incident 2026-07-15): never let the
			// bootstrap disk scan adopt a subagent rollout as the session's
			// main thread — codex refuses user turns on it. Skipping it here
			// (rather than after selection) lets the walk fall through to the
			// best user-sourced match instead of returning nothing when the
			// most-recent match happens to be a subagent. See
			// codex_subagent_gate.go.
			if CodexSubagentThread(sessionID, codexHome) {
				return nil
			}

			info, err := d.Info()
			if err != nil {
				return nil
			}

			// Only consider sessions created after we started this instance.
			if codexStartedAt > 0 {
				startTime := time.UnixMilli(codexStartedAt)
				if info.ModTime().Before(startTime) {
					return nil
				}
			}

			matchesProject, hasProjectMetadata := codexSessionMatchesProject(path, normalizedProjectPath)
			if matchesProject {
				if bestScopedID == "" || info.ModTime().After(bestScopedTime) {
					bestScopedID = sessionID
					bestScopedTime = info.ModTime()
				}
				return nil
			}

			// Use unscoped records only when bootstrapping and metadata is unavailable.
			if allowUnscoped && !hasProjectMetadata {
				if bestUnscopedID == "" || info.ModTime().After(bestUnscopedTime) {
					bestUnscopedID = sessionID
					bestUnscopedTime = info.ModTime()
				}
			}

			return nil
		})
	}) {
		// Walk did not complete in time. The most likely cause is a stuck FS
		// layer (e.g., WSL kernel D-state). Return without consulting the
		// best* variables since they may be partially populated; the caller
		// retries with backoff so a transient stall self-heals.
		sessionLog.Warn("codex_walkdir_timeout",
			slog.String("instance_id", instanceID),
			slog.String("sessions_dir", sessionsDir),
			slog.Duration("timeout", codexWalkDirTimeout))
		return ""
	}
	if walkErr != nil {
		sessionLog.Debug("codex_scan_error", slog.String("error", walkErr.Error()))
	}

	if bestScopedID != "" {
		return bestScopedID
	}
	if allowUnscoped {
		return bestUnscopedID
	}
	return ""
}

// codexSessionMatchesProject checks whether a Codex session file belongs to the
// current project by inspecting JSONL metadata fields (cwd/workdir/path).
// Returns:
//   - match=true if project path matches
//   - known=true if any project metadata field was found
func codexSessionMatchesProject(sessionFilePath, normalizedProjectPath string) (match bool, known bool) {
	if normalizedProjectPath == "" {
		return false, false
	}

	file, err := os.Open(sessionFilePath)
	if err != nil {
		return false, false
	}
	defer file.Close()

	const maxLines = 256

	scanner := bufio.NewScanner(file)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	lineCount := 0
	foundMetadata := false

	for scanner.Scan() {
		lineCount++
		cwd := extractCodexCWDFromJSONLine(scanner.Bytes())
		if cwd != "" {
			foundMetadata = true
			if normalizePath(cwd) == normalizedProjectPath {
				return true, true
			}
		}
		if lineCount >= maxLines {
			break
		}
	}

	return false, foundMetadata
}

// extractCodexCWDFromJSONLine extracts cwd-like project fields from one JSONL record.
func extractCodexCWDFromJSONLine(line []byte) string {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return ""
	}

	keys := []string{"cwd", "workdir", "working_dir", "directory", "path"}
	for _, key := range keys {
		if val := decodeJSONStringField(raw, key); val != "" {
			return val
		}
	}

	if payloadRaw, ok := raw["payload"]; ok && len(payloadRaw) > 0 {
		var payloadObj map[string]json.RawMessage
		if err := json.Unmarshal(payloadRaw, &payloadObj); err == nil {
			for _, key := range keys {
				if val := decodeJSONStringField(payloadObj, key); val != "" {
					return val
				}
			}
		}
	}
	if contextRaw, ok := raw["context"]; ok && len(contextRaw) > 0 {
		var contextObj map[string]json.RawMessage
		if err := json.Unmarshal(contextRaw, &contextObj); err == nil {
			for _, key := range keys {
				if val := decodeJSONStringField(contextObj, key); val != "" {
					return val
				}
			}
		}
	}

	return ""
}

func decodeJSONStringField(raw map[string]json.RawMessage, key string) string {
	value, ok := raw[key]
	if !ok || len(value) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(value, &s); err != nil {
		return ""
	}
	return strings.TrimSpace(s)
}

// collectOtherCodexSessionIDs enumerates other managed tmux sessions and returns
// the CODEX_SESSION_ID values they currently own.
func (i *Instance) collectOtherCodexSessionIDs() map[string]bool {
	return i.codexExclusions(nil)
}

func codexSessionScanInterval(allowUnscoped bool) time.Duration {
	if allowUnscoped {
		return codexBootstrapScanInterval
	}
	return codexRotationScanInterval
}

func (i *Instance) codexSessionScanDue(allowUnscoped bool) bool {
	return i.lastCodexScanAt.IsZero() ||
		time.Since(i.lastCodexScanAt) >= codexSessionScanInterval(allowUnscoped)
}

// shouldRunCodexProcessProbe returns whether we should run Codex process/file
// probing right now. The caller must not hold i.mu: it holds the spawn lock
// (the status pass, a restart), but a storage reload rebinds the thread under
// i.mu without that lock, so the binding is read under i.mu.
func (i *Instance) shouldRunCodexProcessProbe(force bool) bool {
	if force {
		i.lastCodexProbeAt = time.Now()
		return true
	}

	// Fast cadence only while bootstrapping (no session ID yet) — that phase is
	// bounded by discovery succeeding within seconds. Once an ID is known the
	// probe is just a rotation safety net behind the notify hook and tmux env,
	// so it backs off to the rotation cadence (issue #1552).
	i.mu.RLock()
	bound := i.CodexSessionID != ""
	i.mu.RUnlock()
	interval := codexBootstrapScanInterval
	if bound {
		interval = codexRotationScanInterval
	}

	if !i.lastCodexProbeAt.IsZero() && time.Since(i.lastCodexProbeAt) < interval {
		return false
	}

	i.lastCodexProbeAt = time.Now()
	return true
}

// PaneProcessTreePIDs exports collectTmuxPaneProcessTreePIDs for callers
// outside internal/session — the #2100 send path, which uses pane-tree
// membership to decide which of several Claude session records addresses
// THIS session. Same contract as the unexported method: a non-nil error
// means the forest may be incomplete.
func (i *Instance) PaneProcessTreePIDs() ([]int, error) {
	return i.collectTmuxPaneProcessTreePIDs(nil)
}

// collectTmuxPaneProcessTreePIDs returns every pane PID and descendant. A
// non-nil error means the returned process forest may be incomplete. Its
// error strings are prefixed "codex process probe" for historical reasons
// (its first caller); it is not codex-specific — see PaneProcessTreePIDs.
// A status pass shares one process-table snapshot across its rows; nil reads
// a fresh one.
//
// The caller must not hold i.mu. The wrapper and socket are read under it: a
// runtime commit adopts them under i.mu, and a storage reload replaces them
// under i.mu without the spawn lock that Codex detection and the status pass
// hold.
func (i *Instance) collectTmuxPaneProcessTreePIDs(pass *StatusUpdatePass) ([]int, error) {
	i.mu.RLock()
	tmuxSession, socketName := i.tmuxSession, i.TmuxSocketName
	i.mu.RUnlock()
	if tmuxSession == nil || !tmuxSession.Exists() {
		return nil, errors.New("codex process probe: tmux session is unavailable")
	}

	target := tmuxSession.TmuxName()
	// Target the same tmux server the session was created on (issue #687).
	// A session on an isolated agent-deck socket would return no panes from
	// the default server and we would mistakenly treat it as empty.
	// Bounded — see tmux.OutputBounded. Exec(...).Output() has no deadline, and
	// a tmux client that has exhausted its fd table never exits, so this probe
	// would hang the caller forever. -s makes list-panes cover every window in
	// the session rather than only the target's current window.
	out, err := tmux.OutputBounded(socketName, "list-panes", "-s", "-t", target, "-F", "#{pane_pid}")
	if err != nil {
		return nil, fmt.Errorf("codex process probe: list tmux panes: %w", err)
	}
	panePIDs, paneErr := parsePositivePIDs(out, "tmux pane")
	if len(panePIDs) == 0 {
		return nil, paneErr
	}

	// Single snapshot of the process table is substantially cheaper than
	// spawning pgrep once per node in deep process trees.
	treePIDs, psErr := pass.processTree(panePIDs)
	if psErr == nil {
		return treePIDs, paneErr
	}

	// Fallback path for environments where ps output is unavailable/unexpected.
	var (
		allPIDs  []int
		pgrepErr error
	)
	seen := make(map[int]bool)
	for _, panePID := range panePIDs {
		pids, err := collectProcessTreePIDsViaPgrep(panePID)
		pgrepErr = errors.Join(pgrepErr, err)
		for _, pid := range pids {
			if !seen[pid] {
				seen[pid] = true
				allPIDs = append(allPIDs, pid)
			}
		}
	}
	if pgrepErr != nil {
		pgrepErr = errors.Join(fmt.Errorf("codex process probe: ps: %w", psErr), pgrepErr)
	}
	return allPIDs, errors.Join(paneErr, pgrepErr)
}

func parsePositivePIDs(output []byte, source string) ([]int, error) {
	var (
		pids     []int
		parseErr error
	)
	seen := make(map[int]bool)
	scanner := bufio.NewScanner(bytes.NewReader(output))
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		pid, err := strconv.Atoi(text)
		if err != nil || pid <= 0 {
			parseErr = errors.Join(parseErr, fmt.Errorf("codex process probe: invalid %s pid on line %d: %q", source, line, text))
			continue
		}
		if !seen[pid] {
			seen[pid] = true
			pids = append(pids, pid)
		}
	}
	parseErr = errors.Join(parseErr, scanner.Err())
	if len(pids) == 0 && parseErr == nil {
		parseErr = fmt.Errorf("codex process probe: %s output contained no pids", source)
	}
	return pids, parseErr
}

func collectProcessTreePIDsFromTable(rootPIDs []int, procTable []byte) ([]int, error) {
	childrenByParent, parseErr := parsePSParentChildMap(procTable)
	return walkProcessTree(rootPIDs, childrenByParent, parseErr)
}

// walkProcessTree returns rootPIDs and their descendants in childrenByParent.
// parseErr (the table's own) is carried into the returned error.
func walkProcessTree(rootPIDs []int, childrenByParent map[int][]int, parseErr error) ([]int, error) {
	return walkProcessTreeIn(rootPIDs, childrenByParent, presentPIDs(childrenByParent), parseErr)
}

// presentPIDs returns every pid that childrenByParent lists as a child, i.e.
// every row of the ps table it was parsed from.
func presentPIDs(childrenByParent map[int][]int) map[int]bool {
	present := make(map[int]bool)
	for _, children := range childrenByParent {
		for _, pid := range children {
			present[pid] = true
		}
	}
	return present
}

// walkProcessTreeIn is walkProcessTree with present (presentPIDs of
// childrenByParent) already built, for a table shared by many walks.
func walkProcessTreeIn(rootPIDs []int, childrenByParent map[int][]int, present map[int]bool, parseErr error) ([]int, error) {
	var allPIDs []int
	seen := make(map[int]bool)
	queue := make([]int, 0, len(rootPIDs))
	for _, rootPID := range rootPIDs {
		if !present[rootPID] {
			parseErr = errors.Join(parseErr, fmt.Errorf("codex process probe: pane pid %d missing from ps process table", rootPID))
			continue
		}
		if !seen[rootPID] {
			seen[rootPID] = true
			queue = append(queue, rootPID)
		}
	}
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		allPIDs = append(allPIDs, parent)

		for _, childPID := range childrenByParent[parent] {
			if childPID <= 0 || seen[childPID] {
				continue
			}
			seen[childPID] = true
			queue = append(queue, childPID)
		}
	}
	return allPIDs, parseErr
}

func parsePSParentChildMap(procTable []byte) (map[int][]int, error) {
	childrenByParent := make(map[int][]int)
	var parseErr error
	scanner := bufio.NewScanner(bytes.NewReader(procTable))
	for line := 1; scanner.Scan(); line++ {
		text := scanner.Text()
		fields := strings.Fields(text)
		// Tolerant of TRAILING columns so one snapshot can also carry comm=
		// for callers that identify the pane leader (parsePSCommandNames).
		// A row with fewer than two fields is genuinely malformed, and #1687's
		// fail-closed intent is kept by recording it rather than skipping it.
		if len(fields) < 2 {
			parseErr = errors.Join(parseErr, fmt.Errorf("codex process probe: invalid ps row %d: %q", line, text))
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid <= 0 {
			parseErr = errors.Join(parseErr, fmt.Errorf("codex process probe: invalid ps pid on row %d: %q", line, fields[0]))
			continue
		}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil || ppid < 0 {
			parseErr = errors.Join(parseErr, fmt.Errorf("codex process probe: invalid ps parent pid on row %d: %q", line, fields[1]))
			continue
		}
		childrenByParent[ppid] = append(childrenByParent[ppid], pid)
	}
	return childrenByParent, errors.Join(parseErr, scanner.Err())
}

// parsePSCommandNames reads `pid=,ppid=,comm=` output into pid -> command
// name. Comm is the last column and may contain spaces, so everything from
// the third field on is joined. macOS reports comm as an absolute path while
// Linux reports the bare name, so the result is reduced to the base name to
// give callers one shape to compare against.
func parsePSCommandNames(procTable []byte) map[int]string {
	commByPID := make(map[int]string)
	scanner := bufio.NewScanner(bytes.NewReader(procTable))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid <= 0 {
			continue
		}
		commByPID[pid] = filepath.Base(strings.Join(fields[2:], " "))
	}
	return commByPID
}

func collectProcessTreePIDsViaPgrep(rootPID int) ([]int, error) {
	var (
		allPIDs  []int
		probeErr error
	)
	seen := map[int]bool{rootPID: true}
	queue := []int{rootPID}
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		allPIDs = append(allPIDs, parent)

		// #nosec G204 -- "pgrep" is a fixed binary name and the only argument is
		// strconv.Itoa(int), never reachable from external input.
		childrenRaw, err := exec.Command("pgrep", "-P", strconv.Itoa(parent)).Output()
		if err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
				continue // pgrep uses exit 1 when the process has no children.
			}
			probeErr = errors.Join(probeErr, fmt.Errorf("codex process probe: pgrep children of %d: %w", parent, err))
			continue
		}
		children, parseErr := parsePositivePIDs(childrenRaw, "pgrep")
		probeErr = errors.Join(probeErr, parseErr)
		for _, childPID := range children {
			if seen[childPID] {
				continue
			}
			seen[childPID] = true
			queue = append(queue, childPID)
		}
	}
	return allPIDs, probeErr
}

func isLikelyCodexProcessPID(pid int) (bool, error) {
	// #nosec G204 -- "ps" is a fixed binary; only arg is strconv.Itoa(int).
	argsOut, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "args=").Output()
	if err != nil {
		return false, fmt.Errorf("codex process probe: inspect pid %d: %w", pid, err)
	}
	return isCodexArgs(string(argsOut)), nil
}

func isCodexArgs(args string) bool {
	return strings.Contains(strings.ToLower(strings.TrimSpace(args)), "codex")
}

func (i *Instance) collectCodexProcessCandidates(pass *StatusUpdatePass) ([]int, error) {
	pids, probeErr := i.collectTmuxPaneProcessTreePIDs(pass)
	candidates := make([]int, 0, len(pids))
	for _, pid := range pids {
		likely, err := pass.isLikelyCodexProcess(pid)
		if err != nil {
			probeErr = errors.Join(probeErr, err)
			continue
		}
		if likely {
			candidates = append(candidates, pid)
		}
	}
	return candidates, probeErr
}

func extractCodexSessionIDFromPath(path string) string {
	normalized := strings.TrimSpace(path)
	normalized = strings.TrimSuffix(normalized, " (deleted)")
	matches := codexSessionIDPathPatternRE.FindStringSubmatch(normalized)
	if len(matches) < 2 {
		return ""
	}
	return matches[1]
}

func extractCodexSessionIDFromLsofOutput(output []byte) string {
	scanner := bufio.NewScanner(bytes.NewReader(output))
	for scanner.Scan() {
		if sessionID := extractCodexSessionIDFromPath(scanner.Text()); sessionID != "" {
			return sessionID
		}
	}
	return ""
}

func (i *Instance) extractAcceptedCodexSessionIDFromOutput(output []byte) string {
	seen := make(map[string]bool)
	scanner := bufio.NewScanner(bytes.NewReader(output))
	for scanner.Scan() {
		candidateID := extractCodexSessionIDFromPath(scanner.Text())
		if candidateID == "" || seen[candidateID] {
			continue
		}
		seen[candidateID] = true
		if sessionID := i.filterCodexProcessProbeCandidate(candidateID); sessionID != "" {
			return sessionID
		}
	}
	return ""
}

func (i *Instance) extractCodexSessionIDFromProcFD(pid int) (string, error) {
	fdDir := filepath.Join("/proc", strconv.Itoa(pid), "fd")
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		return "", fmt.Errorf("codex process probe: read %s: %w", fdDir, err)
	}

	var probeErr error
	for _, entry := range entries {
		targetPath := filepath.Join(fdDir, entry.Name())
		target, err := os.Readlink(targetPath)
		if err != nil {
			probeErr = errors.Join(probeErr, fmt.Errorf("codex process probe: readlink %s: %w", targetPath, err))
			continue
		}
		if candidateID := extractCodexSessionIDFromPath(target); candidateID != "" {
			if sessionID := i.filterCodexProcessProbeCandidate(candidateID); sessionID != "" {
				return sessionID, nil
			}
		}
	}
	return "", probeErr
}

func (i *Instance) queryCodexSessionFromHostProcFD(pass *StatusUpdatePass) (string, error) {
	candidates, probeErr := i.collectCodexProcessCandidates(pass)
	for _, pid := range candidates {
		sessionID, err := i.extractCodexSessionIDFromProcFD(pid)
		if sessionID != "" {
			return sessionID, nil
		}
		probeErr = errors.Join(probeErr, err)
	}
	return "", probeErr
}

func codexProcFDProbeScript(procRoot string) string {
	root := shellescape.Quote(strings.TrimRight(procRoot, "/"))
	return fmt.Sprintf(
		`command -v readlink >/dev/null 2>&1 || {
	echo %q
	exit 0
}
proc_root=%s
saw_process=0
incomplete=0
for p in "$proc_root"/[0-9]*; do
	if [ "$p" = "$proc_root/[0-9]*" ]; then
		break
	fi
	saw_process=1
	if [ ! -d "$p" ]; then
		incomplete=1
		continue
	fi
	d="$p/fd"
	if [ ! -d "$d" ] || [ ! -r "$d" ] || [ ! -x "$d" ]; then
		incomplete=1
		continue
	fi
	for f in "$d"/*; do
		if [ "$f" = "$d/*" ]; then
			incomplete=1
			break
		fi
		if ! t=$(readlink "$f" 2>/dev/null); then
			incomplete=1
			continue
		fi
		case "$t" in
			*/sessions/*rollout-*.jsonl*)
				printf '%%s\n' "$t"
				;;
		esac
	done
done
if [ "$saw_process" -eq 0 ]; then
	incomplete=1
fi
if [ "$incomplete" -ne 0 ]; then
	echo %q
fi`,
		codexProbeMissingSentinel,
		root,
		codexProbeIncompleteSentinel,
	)
}

func (i *Instance) queryCodexSessionFromDockerProcFD() (string, string, error) {
	i.mu.RLock()
	container := i.SandboxContainer
	i.mu.RUnlock()
	if strings.TrimSpace(container) == "" {
		return "", "", errors.New("codex process probe: sandbox container is unavailable")
	}

	script := codexProcFDProbeScript("/proc")
	// #nosec G204 -- "docker exec" with internal SandboxContainer name and a
	// hardcoded shell probe script (the sentinels are compile-time constants);
	// no external input flows here.
	out, err := exec.Command("docker", "exec", container, "sh", "-lc", script).Output()
	if sessionID := i.extractAcceptedCodexSessionIDFromOutput(out); sessionID != "" {
		return sessionID, "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("codex process probe: docker exec: %w", err)
	}
	if bytes.Contains(out, []byte(codexProbeMissingSentinel)) {
		return "", "readlink", errors.New("codex process probe: readlink is unavailable")
	}
	if bytes.Contains(out, []byte(codexProbeIncompleteSentinel)) {
		return "", "", errors.New("codex process probe: container fd scan is incomplete")
	}
	return "", "", nil
}

// procfdOpenVnodePaths is a test seam over procfd.OpenVnodePaths so the
// native-probe semantics (ID extraction, answered/lsof-fallback logic) stay
// testable on platforms where the real scan is unsupported.
var procfdOpenVnodePaths = procfd.OpenVnodePaths

// queryCodexSessionFromHostNative probes candidate PIDs with the in-process
// libproc FD scan (issue #1552: spawning lsof per PID on macOS walks every FD
// and resolves every vnode path, and polling it stalled whole machines). A
// non-nil error means the negative result is incomplete and requires fallback.
func (i *Instance) queryCodexSessionFromHostNative(pids []int) (string, error) {
	var probeErr error
	for _, pid := range pids {
		paths, err := procfdOpenVnodePaths(pid)
		for _, path := range paths {
			candidateID := extractCodexSessionIDFromPath(path)
			if sessionID := i.filterCodexProcessProbeCandidate(candidateID); sessionID != "" {
				return sessionID, nil
			}
		}
		if err != nil {
			if !errors.Is(err, procfd.ErrUnsupported) {
				sessionLog.Debug("codex_procfd_probe_failed", slog.Int("pid", pid), slog.Any("error", err))
			}
			probeErr = errors.Join(probeErr, fmt.Errorf("codex native probe pid %d: %w", pid, err))
		}
	}
	return "", probeErr
}

func (i *Instance) queryCodexSessionFromHostLsof(pids []int) (string, string, error) {
	if _, err := exec.LookPath("lsof"); err != nil {
		return "", "lsof", fmt.Errorf("codex process probe: find lsof: %w", err)
	}

	var probeErr error
	for _, pid := range pids {
		// -n -P disables reverse-DNS host and port-name resolution so a resolver
		// that drops PTR queries cannot stall the probe (issue #1581); the
		// context timeout bounds the call even if lsof hangs for any other reason.
		ctx, cancel := context.WithTimeout(context.Background(), codexLsofProbeTimeout)
		// #nosec G204 -- "lsof" is a fixed binary; only dynamic arg is strconv.Itoa(int).
		out, err := exec.CommandContext(ctx, "lsof", "-n", "-P", "-p", strconv.Itoa(pid)).Output()
		cancel()
		if sessionID := i.extractAcceptedCodexSessionIDFromOutput(out); sessionID != "" {
			return sessionID, "", nil
		}
		if err != nil {
			var execErr *exec.Error
			if errors.As(err, &execErr) && execErr.Err == exec.ErrNotFound {
				return "", "lsof", fmt.Errorf("codex process probe: run lsof: %w", err)
			}
			sessionLog.Debug("codex_lsof_probe_failed", slog.Int("pid", pid), slog.Any("error", err))
			probeErr = errors.Join(probeErr, fmt.Errorf("codex process probe: lsof pid %d: %w", pid, err))
			continue
		}
	}

	return "", "", probeErr
}

// queryCodexSessionFromProcessFiles inspects live Codex processes and returns
// the active session UUID inferred from open rollout JSONL files. Empty ID with
// nil error means definitely absent; a non-nil error means undetermined. The
// second return value names a missing dependency, when applicable.
// A status pass shares one process-table snapshot across its rows; nil reads
// a fresh one.
//
// The caller must not hold i.mu: every step reads the Instance under it,
// because a storage reload writes those fields under i.mu without the spawn
// lock its callers (a detection attempt, the status pass) hold.
func (i *Instance) queryCodexSessionFromProcessFiles(pass *StatusUpdatePass) (string, string, error) {
	i.mu.RLock()
	sandboxed := i.IsSandboxed()
	i.mu.RUnlock()
	// Sandboxed sessions run Codex inside Docker; probe container /proc.
	if sandboxed {
		return i.queryCodexSessionFromDockerProcFD()
	}

	// Linux/WSL: pure in-process /proc scanning (no lsof dependency).
	if runtime.GOOS == "linux" {
		sessionID, err := i.queryCodexSessionFromHostProcFD(pass)
		return sessionID, "", err
	}

	// Non-Linux (e.g. macOS): in-process libproc FD scan first, with lsof kept
	// only as a compatibility fallback when the native probe cannot answer.
	candidates, candidateErr := i.collectCodexProcessCandidates(pass)
	if sessionID, nativeErr := i.queryCodexSessionFromHostNative(candidates); sessionID != "" {
		return sessionID, "", nil
	} else if nativeErr == nil && candidateErr == nil {
		return "", "", nil
	}

	sessionID, missingDep, lsofErr := i.queryCodexSessionFromHostLsof(candidates)
	if sessionID != "" {
		return sessionID, "", nil
	}
	return "", missingDep, errors.Join(candidateErr, lsofErr)
}

// ConsumeCodexRestartWarning returns and clears any pending Codex restart warning.
func (i *Instance) ConsumeCodexRestartWarning() string {
	i.mu.Lock()
	defer i.mu.Unlock()
	warning := strings.TrimSpace(i.pendingCodexRestartWarning)
	i.pendingCodexRestartWarning = ""
	return warning
}

func codexProbeMissingWarning(missingDep string) string {
	missingDep = strings.TrimSpace(missingDep)
	if missingDep == "" {
		return ""
	}
	return fmt.Sprintf("Codex session detection fallback: %s is not available", missingDep)
}

// UpdateCodexSession updates the Codex session ID.
// Primary source: tmux environment.
// Fallback: project-aware filesystem scan.
func (i *Instance) UpdateCodexSession(excludeIDs map[string]bool) {
	// Preserve the explicit caller-supplied exclusion API: nil means no exclusions.
	// Status passes use the lazy ownership lookup through updateCodexSessionForPass.
	if excludeIDs == nil {
		excludeIDs = make(map[string]bool)
	}
	i.updateCodexSession(excludeIDs, false)
}

type codexSessionCandidate struct {
	id, envID, source, missingDependency string
}

// updateCodexSession refreshes Codex bindings through the versioned publisher.
func (i *Instance) updateCodexSession(excludeIDs map[string]bool, forceProbe bool) string {
	return i.updateCodexSessionForPass(excludeIDs, forceProbe, nil)
}

func (i *Instance) updateCodexSessionForPass(excludeIDs map[string]bool, forceProbe bool, pass *StatusUpdatePass) string {
	// Match restart's lock order: instance authority before bootstrap claims.
	release, err := acquireInstanceSpawnLock(i.ID)
	if err != nil {
		sessionLog.Debug("codex_binding_lock_failed", slog.String("error", err.Error()))
		return ""
	}
	defer release()
	observation := i.CaptureRuntimeBindingObservation("codex")
	if observation.value == "" && pass == nil {
		pass = &StatusUpdatePass{}
	}
	// The disk fallback takes the bootstrap claim just before it selects, and
	// it is held through publication below. The tmux environment read and the
	// process probe are authoritative and never wait for it.
	var bootstrap codexBootstrapClaim
	defer bootstrap.release()
	candidate := i.queryCodexSessionCandidateForPass(excludeIDs, forceProbe, observation.value, pass, &bootstrap)
	if candidate.id != "" {
		if err := i.publishRuntimeBindingObservationLocked(observation, candidate.id, time.Now()); err != nil {
			sessionLog.Debug("codex_binding_rejected",
				slog.String("session_id", candidate.id), slog.String("source", candidate.source),
				slog.String("error", err.Error()))
		} else {
			i.syncPublishedCodexCandidate(candidate, observation.value)
		}
	}
	return candidate.missingDependency
}

// adoptCodexSessionForRestart is a restart's Codex detection, run under the
// restart's transition authority. Like the status pass it takes the bootstrap
// claim lazily, on the way into the disk fallback, so it cannot select the
// rollout a peer bootstrapping in the same project is selecting from the same
// ownership snapshot. It publishes its selection to peers before releasing
// the claim: recordCodexOwnership for this process and the tmux environment
// for others, as the status pass's publication does. The durable binding
// follows with the restart's generation; holding the claim across the spawn
// would stall every peer's fallback behind it. The selection is written under
// i.mu: the restart holds the spawn lock, not i.mu, and every status pass
// observes the binding under i.mu alone (captureActiveRuntimeBindingObservation).
func (i *Instance) adoptCodexSessionForRestart() codexSessionCandidate {
	var bootstrap codexBootstrapClaim
	defer bootstrap.release()
	candidate := i.queryCodexSessionCandidateForPass(nil, true, i.CodexSessionID, &StatusUpdatePass{}, &bootstrap)
	if candidate.id != "" {
		i.mu.Lock()
		i.CodexSessionID = candidate.id
		i.CodexDetectedAt = time.Now()
		i.mu.Unlock()
		i.recordCodexOwnership(candidate.id)
		i.syncPublishedCodexCandidate(candidate, "")
	}
	return candidate
}

// codexBootstrapClaim holds codexBootstrapMu from the moment the disk fallback
// starts selecting until its caller has published the result, so peers sharing
// a project cannot claim one rollout from the same ownership snapshot. It is
// taken lazily, only on the way into the fallback: authoritative evidence is
// read without it, as codexBootstrapMu's contract requires. The zero value
// holds nothing, and release is idempotent.
type codexBootstrapClaim struct{ held bool }

func (c *codexBootstrapClaim) acquire() {
	if !c.held {
		codexBootstrapMu.Lock()
		c.held = true
	}
}

func (c *codexBootstrapClaim) release() {
	if c.held {
		c.held = false
		codexBootstrapMu.Unlock()
	}
}

// queryCodexSessionCandidateForPass reads the tmux environment and the process
// probe, then falls back to the disk scan. A non-nil bootstrap is acquired
// just before that scan and left held for the caller to release once it has
// published the candidate.
func (i *Instance) queryCodexSessionCandidateForPass(excludeIDs map[string]bool, forceProbe bool, currentID string, pass *StatusUpdatePass, bootstrap *codexBootstrapClaim) codexSessionCandidate {
	i.mu.RLock()
	tool := i.Tool
	tmuxSession := i.tmuxSession
	startedAt := i.CodexStartedAt
	hookID := i.hookSessionID
	i.mu.RUnlock()
	if !IsCodexCompatible(tool) {
		return codexSessionCandidate{}
	}

	candidate := codexSessionCandidate{}
	if tmuxSession != nil {
		if sessionID, err := tmuxSession.GetEnvironment("CODEX_SESSION_ID"); err == nil && sessionID != "" {
			candidate.id, candidate.envID, candidate.source = sessionID, sessionID, "tmux_env"
		}
	}

	if i.shouldRunCodexProcessProbe(forceProbe) {
		// Every ID the probe returns has passed its subagent gate
		// (filterCodexProcessProbeCandidate), which records the rejections.
		sessionID, missingDep, probeErr := i.queryCodexSessionFromProcessFiles(pass)
		if sessionID != "" {
			candidate.id, candidate.source = sessionID, "process_probe"
			return candidate
		}
		if missingDep != "" {
			candidate.missingDependency = missingDep
		}
		if probeErr != nil {
			sessionLog.Debug("codex_session_probe_undetermined", slog.Any("error", probeErr))
			return candidate
		}
	}
	if candidate.id != "" || currentID != "" {
		return candidate
	}

	allowUnscoped := candidate.envID == "" && startedAt > 0
	if !i.codexSessionScanDue(allowUnscoped) {
		return candidate
	}
	// Stamped before the live-process check as well as the disk scan: a live
	// Codex that owns no thread yet would otherwise walk the pane's process
	// tree and list its open files on every pass.
	i.lastCodexScanAt = time.Now()
	// When a Codex process runs in the pane, only the thread it holds open (its
	// rollout or thread writer lock) may bind: a child launched without a
	// message has no rollout yet, and the newest rollout in the project is a
	// sibling's (#2394). An incomplete probe binds nothing this pass. The caller
	// publishes the live thread through the runtime binding authority, which
	// records its ownership and the pane's CODEX_SESSION_ID.
	if threadID, live := i.liveCodexBootstrapEvidence(pass); live {
		if threadID != "" {
			candidate.id, candidate.source = threadID, "live_thread"
		}
		return candidate
	}
	if bootstrap != nil {
		if excludeIDs == nil {
			i.codexExclusions(pass) // Fetch peer ownership before the claim; no subprocess runs under it.
		}
		bootstrap.acquire()
	}
	if excludeIDs == nil {
		excludeIDs = i.codexExclusions(pass)
		if excludeIDs == nil {
			return candidate
		}
	}
	exclusions := codexSessionExclusions{explicit: excludeIDs}
	if sessionID := i.queryCodexSession(exclusions, allowUnscoped); sessionID != "" {
		i.mu.RLock()
		hookUnchanged := i.hookSessionID == hookID
		i.mu.RUnlock()
		if !hookUnchanged {
			return candidate
		}
		candidate.id, candidate.source = sessionID, "disk"
	}
	return candidate
}

func (i *Instance) syncPublishedCodexCandidate(candidate codexSessionCandidate, oldID string) {
	if candidate.id != oldID {
		sessionLog.Debug("codex_session_update",
			slog.String("old_id", oldID), slog.String("new_id", candidate.id),
			slog.String("source", candidate.source))
	}
	i.mu.RLock()
	tmuxSession := i.tmuxSession
	i.mu.RUnlock()
	if tmuxSession != nil && tmuxSession.Exists() && (candidate.id != oldID || candidate.envID == "") {
		_ = tmuxSession.SetEnvironment("CODEX_SESSION_ID", candidate.id)
	}
}
