package core

import (
	"context"
	"fmt"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/health"
	"github.com/asheshgoplani/agent-deck/internal/sendqueue"
	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/telemetry"
)

// SessionStopIn is the input of session.stop.
type SessionStopIn struct {
	Profile string `json:"profile" doc:"Profile whose store holds the session"`
	Session string `json:"session" doc:"Session id, id prefix, title or path"`
}

// SessionStopOut is the output of session.stop.
type SessionStopOut struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	Drained      string `json:"drained,omitempty" doc:"Id of the queued session started in the freed slot"`
	DrainedTitle string `json:"drained_title,omitempty"`
	Warning      string `json:"warning,omitempty" doc:"Runtime durability warning of the drained session's start"`
}

func (deps Deps) sessionStop(ctx context.Context, in SessionStopIn) (SessionStopOut, error) {
	d, err := loadSessionData(in.Profile)
	if err != nil {
		return SessionStopOut{}, err
	}
	inst, err := deps.resolve(in.Session, d.instances)
	if err != nil {
		return SessionStopOut{}, err
	}
	// Freeze the runtime the caller resolved before any probe: syncing ids
	// publishes bindings and can adopt a newer durable generation, which this
	// stop must never kill.
	selection := inst.CaptureRuntimeSelection()
	if !inst.Exists() {
		return SessionStopOut{}, Errorf(CodeNotRunning, "session '%s' is not running", inst.Title)
	}

	// Capture tool conversation ids before Kill: show-environment fails on
	// a dead tmux session.
	inst.SyncSessionIDsFromTmux()
	// A Codex thread the pane's process took after launch stopped waiting
	// for it is in no pane variable, and Kill destroys the only evidence of
	// it (#2400), so bind it from the live process as the legacy stop does.
	if _, err := inst.BindLiveCodexThread(0); err != nil {
		Warn(ctx, fmt.Sprintf("could not persist Codex session identity: %v", err))
	}
	// KillCaptured reserves the selected runtime before the kill and commits
	// stopped through the status authority, so another process's error
	// sample of this kill cannot win (defect 317).
	if err := inst.KillCaptured(selection); err != nil {
		return SessionStopOut{}, &Error{Code: CodeInvalid, Message: fmt.Sprintf("failed to stop session: %v", err), Cause: err}
	}
	inst.RecordTelemetryEnd(telemetry.EndStop)
	// Saved before the drain: a drained start waits for its agent to be ready.
	if err := d.saveOr("failed to save session state"); err != nil {
		return SessionStopOut{}, err
	}

	drained, warning := drainGroupQueue(ctx, d.storage, inst.GroupPath, d.instances, d.groups)
	if warning != "" {
		Warn(ctx, warning)
		Emit(ctx, Event{Kind: EventRuntimeWarning, ID: drained.ID, Title: drained.Title, Message: warning})
	}

	out := SessionStopOut{ID: inst.ID, Title: inst.Title, Warning: warning}
	if drained != nil {
		out.Drained = drained.ID
		out.DrainedTitle = drained.Title
	}
	// Journaled after the verdict: RecordSessionEvent writes synchronously
	// and a slow health volume must not delay the answer. session.end is
	// recorded after Kill above so the default CLI and daemon stops are counted like
	// the TUI, web and legacy CLI stops.
	AfterFunc(ctx, func() {
		session.RecordSessionEvent(in.Profile, inst.ID, health.KindStop, nil)
		session.RecallNotifyInstance(inst, health.KindStop)
	})
	return out, nil
}

// drainGroupQueue starts the oldest queued instance in groupPath when a slot
// is free and returns it with the start's runtime durability warning. It
// persists only that instance's row (session.Storage.StartQueued), which also
// puts its queued message on the send queue. Best-effort: a failed start is
// reported through EventQueueDrainFailed and durably marks that instance
// errored, so later stops do not retry it ahead of the rest of the queue. A
// session another drainer claimed first is left to it, and the next queued
// one is tried while the group is below its cap.
func drainGroupQueue(ctx context.Context, storage *session.Storage, groupPath string, instances []*session.Instance, groups []*session.GroupData) (*session.Instance, string) {
	tree := session.NewGroupTreeWithGroups(instances, groups)
	max := session.GroupMaxConcurrent(tree, groupPath)
	// The cap is checked on every pass, counting starting sessions too: a
	// claim lost to another drainer (or to a direct start) means that session
	// now holds a slot, and its stale copy here is marked starting.
	for !session.IsAtCap(session.CountOccupiedInGroup(instances, groupPath), max) {
		next := session.FindNextQueued(instances, groupPath)
		if next == nil {
			return nil, ""
		}
		started, err := storage.StartQueued(next)
		if started.Warning != "" {
			Warn(ctx, started.Warning)
		}
		if err != nil {
			Emit(ctx, Event{Kind: EventQueueDrainFailed, ID: next.ID, Title: next.Title, Err: err})
			return nil, ""
		}
		if started.Started {
			return next, ""
		}
		// Claimed by another drainer: this copy is stale.
		next.Status = session.StatusStarting
	}
	return nil, ""
}

func init() { session.SetQueuedMessageSink(EnqueueQueuedStartMessage, sendqueue.DefaultRetryBudget) }

// spawnSendWorker starts a target's send worker; tests replace it.
var spawnSendWorker = sendqueue.SpawnWorker

// SetSendWorkerSpawnForTest replaces the send-worker spawn of
// EnqueueQueuedStartMessage and returns a restore func.
func SetSendWorkerSpawnForTest(fn func(profile, sessionID string) error) func() {
	prev := spawnSendWorker
	spawnSendWorker = fn
	return func() { spawnSendWorker = prev }
}

// EnqueueQueuedStartMessage is the session queued-message sink: it puts a
// queued -m message on the durable send queue next to the profile's state.db
// (profileDir) and starts the target's send worker for that profile, which
// types it once the agent is ready. It returns at once: a drain runs under the profile mutation lock,
// and waiting there for readiness (up to send.DefaultAgentReadyTimeout) would
// block every other mutation.
func EnqueueQueuedStartMessage(profile, profileDir string, inst *session.Instance, message string) error {
	now := time.Now()
	// It is queued before the spawn: the worker waits for the pane
	// (WaitWhileStopped) and for the agent to be idle before typing.
	s := sendqueue.Send{
		SessionID: inst.ID, SessionTitle: inst.Title, Tool: inst.Tool, Running: true,
		Message: message, Sender: "cli", Deadline: now.Add(sendqueue.DefaultRetryBudget),
		WaitWhileStopped: true,
	}
	rec, err := sendqueue.Enqueue(profile, sendqueue.Dir(profileDir), s, now)
	if err != nil {
		return fmt.Errorf("queueing the queued message of %s: %w", inst.Title, err)
	}
	if err := spawnSendWorker(profile, rec.SessionID); err != nil {
		return fmt.Errorf("starting the send worker for %s (send %s stays queued): %w", inst.Title, rec.SendID, err)
	}
	return nil
}
