package session

import (
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// mcpReapGracePeriod is how long we wait after SIGTERM before escalating
// to SIGKILL on a tracked MCP child. Stdio MCP children generally exit
// immediately when their stdio is closed, so anything that survives this
// window is almost certainly stuck and needs the harder signal.
//
// Raised from 500ms → 1s in the issue #1086 fix: on CI runners under
// the race detector, scheduler latency between the SIGTERM and the
// child's libc signal handler can exceed 500ms, causing a spurious
// SIGKILL escalation that races with the child's already-in-flight
// exit.
const mcpReapGracePeriod = 1 * time.Second

// mcpReapMinDepth returns the shallowest depth below the pane PID at which a
// process is an MCP child rather than the tool itself, given the pane leader's
// command name.
//
// The two spawn shapes put the tool at different depths:
//
//	shell-led pane:  sh (0) -> claude (1) -> MCP server (2)
//	agent-led pane:  claude (0)           -> MCP server (1)
//
// agent-deck execs the agent wherever it can, which makes the agent the pane
// leader and leaves nothing at depth 1 but its own children. Tools that are
// not exec'd (or panes whose leader is a login shell) keep the older shape,
// where depth 1 is the tool and must not be pre-empted. Assuming a single
// depth silently drops one of the two: hardcoding 2 leaves agent-led panes
// with no #965 defence at all, and hardcoding 1 would SIGTERM the tool.
//
// An unknown or unreadable leader falls back to 2, which is the conservative
// direction: it can under-register, never signal the tool by mistake.
func mcpReapMinDepth(paneLeaderComm string) int {
	if paneLeaderComm != "" && !isShellBinary(paneLeaderComm) {
		return 1
	}
	return 2
}

// mcpReapVerifyTimeout is how long we wait AFTER SIGKILL for the child
// to actually be gone (reaped by init or exit'd by the kernel). Without
// this verification, killInternal returns while children are still
// transitioning, which leaves a window where callers (and tests) can
// observe live PIDs even though the kernel has accepted SIGKILL. See
// issue #1086.
const mcpReapVerifyTimeout = 2 * time.Second

var (
	mcpCaptureProcessIdentityFn   = tmux.CaptureProcessIdentity
	mcpCaptureProcessIdentitiesFn = tmux.CaptureProcessIdentities
	mcpCloseProcessIdentitiesFn   = tmux.CloseProcessIdentities
	mcpProcessIdentityMatchesFn   = tmux.ProcessIdentityMatches
	mcpReapProcessIdentitiesFn    = tmux.ReapProcessIdentities
	mcpReadPanePIDFn              = func(i *Instance) int { return i.readPanePID() }
	mcpProcessTableSnapshotFn     = func() ([]byte, error) {
		return exec.Command("ps", "-eo", "pid=,ppid=,comm=").Output()
	}
)

// RegisterMCPChild records the OS PID of a stdio MCP child spawned for
// this session. Session stop iterates these PIDs and signals each
// (SIGTERM → SIGKILL) to prevent the issue-#965 orphan accumulation
// where MCP children get reparented to PID 1.
//
// Safe to call concurrently. Passing pid <= 0 is a no-op.
func (i *Instance) RegisterMCPChild(pid int) {
	if pid <= 0 {
		return
	}
	identity, err := mcpCaptureProcessIdentityFn(pid)
	if err != nil {
		mcpLog.Debug("mcp_child_identity_capture_failed",
			slog.Int("pid", pid), slog.Any("error", err))
		return
	}
	i.RegisterMCPChildIdentity(identity)
}

// RegisterMCPChildIdentity consumes an identity captured while the PID was
// still known to be a descendant of this session. It intentionally does not
// recapture by PID: callers carrying children across tmux teardown transfer
// the retained pre-mutation handle. Invalid identities are also consumed.
func (i *Instance) RegisterMCPChildIdentity(identity tmux.ProcessIdentity) {
	if !identity.Valid() {
		_ = identity.Close()
		return
	}
	i.mcpPIDsMu.Lock()
	defer i.mcpPIDsMu.Unlock()
	found := false
	for _, existing := range i.TrackedMCPPIDs {
		if existing == identity.PID {
			found = true
			break
		}
	}
	if !found {
		i.TrackedMCPPIDs = append(i.TrackedMCPPIDs, identity.PID)
	}
	if i.trackedMCPChildIdentities == nil {
		i.trackedMCPChildIdentities = make(map[int]tmux.ProcessIdentity)
	}
	if existing, ok := i.trackedMCPChildIdentities[identity.PID]; ok && existing != identity {
		_ = existing.Close()
	}
	i.trackedMCPChildIdentities[identity.PID] = identity
}

// TrackedMCPChildIdentities returns the identities captured for this
// instance's tracked children. Persisted raw PIDs without an in-process birth
// token are omitted and therefore can never authorize a signal.
func (i *Instance) TrackedMCPChildIdentities() []tmux.ProcessIdentity {
	i.mcpPIDsMu.Lock()
	defer i.mcpPIDsMu.Unlock()
	identities := make([]tmux.ProcessIdentity, 0, len(i.trackedMCPChildIdentities))
	for _, pid := range i.TrackedMCPPIDs {
		if identity, ok := i.trackedMCPChildIdentities[pid]; ok {
			// Observers receive metadata, not a shared close capability. Handle
			// ownership remains with the instance until unregister or reap.
			identities = append(identities, tmux.ProcessIdentity{
				PID: identity.PID, StartToken: identity.StartToken,
			})
		}
	}
	return identities
}

// UnregisterMCPChild removes a previously registered MCP child PID,
// e.g. when the child has been observed exiting cleanly.
func (i *Instance) UnregisterMCPChild(pid int) {
	if pid <= 0 {
		return
	}
	i.mcpPIDsMu.Lock()
	defer i.mcpPIDsMu.Unlock()
	out := i.TrackedMCPPIDs[:0]
	for _, p := range i.TrackedMCPPIDs {
		if p != pid {
			out = append(out, p)
		}
	}
	i.TrackedMCPPIDs = out
	if identity, ok := i.trackedMCPChildIdentities[pid]; ok {
		_ = identity.Close()
	}
	delete(i.trackedMCPChildIdentities, pid)
	if len(i.trackedMCPChildIdentities) == 0 {
		i.trackedMCPChildIdentities = nil
	}
}

// captureMCPChildrenFromPaneTree retains depth >= 2 descendants after two
// matching process-tree snapshots. Pane and direct tool-process PIDs remain
// owned by tmux teardown; deeper stdio MCP helpers may escape that process group.
// The caller owns returned handles.
func (i *Instance) captureMCPChildrenFromPaneTree() ([]tmux.ProcessIdentity, error) {
	if i.tmuxSession == nil {
		return nil, nil
	}
	panePID, children, err := i.mcpDescendantSnapshot()
	if err != nil {
		return nil, err
	}
	if len(children) == 0 {
		return nil, nil
	}
	identities, err := mcpCaptureProcessIdentitiesFn(children)
	if err != nil {
		return nil, err
	}
	owned := true
	defer func() {
		if owned {
			mcpCloseProcessIdentitiesFn(identities)
		}
	}()
	if !identityPIDsMatch(identities, children) {
		return nil, errors.New("MCP process tree changed during identity capture")
	}
	afterPanePID, afterChildren, err := i.mcpDescendantSnapshot()
	if err != nil {
		return nil, err
	}
	if panePID != afterPanePID || !sameProcessPIDSet(children, afterChildren) {
		return nil, errors.New("MCP process tree changed during identity capture")
	}
	for _, identity := range identities {
		if !mcpProcessIdentityMatchesFn(identity) {
			return nil, errors.New("MCP process identity exited during tree revalidation")
		}
	}
	owned = false
	return identities, nil
}

func (i *Instance) mcpDescendantSnapshot() (int, []int, error) {
	panePID := mcpReadPanePIDFn(i)
	if panePID <= 0 {
		return 0, nil, errors.New("MCP pane PID unavailable")
	}
	procTable, err := mcpProcessTableSnapshotFn()
	if err != nil {
		return 0, nil, fmt.Errorf("snapshot MCP process tree: %w", err)
	}
	if len(procTable) == 0 {
		return 0, nil, errors.New("empty MCP process table")
	}
	childrenByParent, parseErr := parsePSParentChildMap(procTable)
	if parseErr != nil {
		return 0, nil, fmt.Errorf("parse MCP process table: %w", parseErr)
	}
	// Same snapshot, so the leader identity cannot disagree with the tree.
	minDepth := mcpReapMinDepth(parsePSCommandNames(procTable)[panePID])

	// BFS from pane PID with depth tracking. Single snapshot — every
	// classification decision (skip below minDepth vs register at or
	// past it, and the pane-leader read that sets minDepth) is
	// against the same ps output, removing the two-snapshot race the
	// previous implementation had between collectTmuxPaneProcessTreePIDs
	// and the per-pid parent lookup.
	type queued struct {
		pid   int
		depth int
	}
	seen := map[int]bool{panePID: true}
	queue := []queued{{pid: panePID, depth: 0}}
	var descendants []int
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]

		for _, child := range childrenByParent[node.pid] {
			if child <= 0 || seen[child] {
				continue
			}
			seen[child] = true
			childDepth := node.depth + 1
			if childDepth >= minDepth {
				descendants = append(descendants, child)
			}
			queue = append(queue, queued{pid: child, depth: childDepth})
		}
	}
	return panePID, descendants, nil
}

func identityPIDsMatch(identities []tmux.ProcessIdentity, pids []int) bool {
	if len(identities) != len(pids) {
		return false
	}
	got := make(map[int]struct{}, len(identities))
	for _, identity := range identities {
		got[identity.PID] = struct{}{}
	}
	for _, pid := range pids {
		if _, ok := got[pid]; !ok {
			return false
		}
	}
	return len(got) == len(pids)
}

func sameProcessPIDSet(left, right []int) bool {
	if len(left) != len(right) {
		return false
	}
	pids := make(map[int]struct{}, len(left))
	for _, pid := range left {
		pids[pid] = struct{}{}
	}
	for _, pid := range right {
		if _, ok := pids[pid]; !ok {
			return false
		}
	}
	return len(pids) == len(left)
}

// readPanePID returns the pane PID for this Instance's tmux session,
// or 0 if it cannot be determined. Extracted so discovery can take a
// single ps snapshot rather than indirecting through
// collectTmuxPaneProcessTreePIDs (which also takes its own snapshot).
func (i *Instance) readPanePID() int {
	target := i.tmuxSession.Name + ":"
	// Bounded — see tmux.OutputBounded. This runs on the MCP child-reap path;
	// an unbounded probe would leave orphaned children unreaped indefinitely.
	out, err := tmux.OutputBounded(i.TmuxSocketName, "list-panes", "-t", target, "-F", "#{pane_pid}")
	if err != nil {
		// Returning 0 no-ops MCP child discovery in killInternal, which is
		// single-shot with no retry — so a timeout here silently resurrects the
		// setsid-MCP orphan leak (#965) for that kill. Log it rather than
		// letting the reap quietly do nothing.
		slog.Warn("pane_pid_probe_failed",
			slog.String("session", i.Title),
			slog.String("error", err.Error()))
		return 0
	}
	pidStr := strings.TrimSpace(string(out))
	if idx := strings.IndexByte(pidStr, '\n'); idx >= 0 {
		pidStr = pidStr[:idx]
	}
	pid, err := strconv.Atoi(pidStr)
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

// reapTrackedMCPChildren SIGTERMs every PID in TrackedMCPPIDs, waits a
// short grace window, then SIGKILLs any that are still alive. The list
// is cleared on return so a subsequent stop is a no-op.
//
// Errors signaling a single PID are logged and swallowed: a missing
// child (ESRCH) is the success case, and we never want a single stuck
// PID to block tmux teardown.
//
// Issue #1086: after SIGKILL, this function now blocks until every PID
// has actually been reaped (ESRCH or zombie) or mcpReapVerifyTimeout
// elapses. Previously it returned immediately after sending SIGKILL,
// which allowed callers (and the issue #965 regression test) to
// observe live PIDs in the brief window before the kernel reaped
// them — flaky on CI runners under -race.
func (i *Instance) reapTrackedMCPChildren() {
	i.mcpPIDsMu.Lock()
	pids := append([]int(nil), i.TrackedMCPPIDs...)
	i.TrackedMCPPIDs = nil
	identities := make([]tmux.ProcessIdentity, 0, len(i.trackedMCPChildIdentities))
	for _, pid := range pids {
		if identity, ok := i.trackedMCPChildIdentities[pid]; ok {
			identities = append(identities, identity)
			delete(i.trackedMCPChildIdentities, pid)
		}
	}
	// Preserve handle ownership even if an inconsistent persisted PID slice
	// omitted an in-memory identity. Clearing the map must not leak it.
	for _, identity := range i.trackedMCPChildIdentities {
		identities = append(identities, identity)
	}
	i.trackedMCPChildIdentities = nil
	i.mcpPIDsMu.Unlock()

	if len(identities) == 0 {
		return
	}
	mcpReapProcessIdentitiesFn(identities, tmux.ProcessReapTiming{
		TermGrace:    mcpReapGracePeriod,
		KillWait:     mcpReapVerifyTimeout,
		PollInterval: 20 * time.Millisecond,
	})
}
