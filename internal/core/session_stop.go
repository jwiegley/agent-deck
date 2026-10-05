package core

import (
	"context"
	"fmt"

	"github.com/asheshgoplani/agent-deck/internal/health"
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
	if err := inst.KillCaptured(selection); err != nil {
		return SessionStopOut{}, &Error{Code: CodeInvalid, Message: fmt.Sprintf("failed to stop session: %v", err), Cause: err}
	}
	inst.RecordTelemetryEnd(telemetry.EndStop)

	drained, warning := drainGroupQueue(ctx, d.storage, inst.GroupPath, d.instances, d.groups)
	if warning != "" {
		Warn(ctx, warning)
		Emit(ctx, Event{Kind: EventRuntimeWarning, ID: drained.ID, Title: drained.Title, Message: warning})
	}
	if err := d.saveOr("failed to save session state"); err != nil {
		return SessionStopOut{}, err
	}

	out := SessionStopOut{ID: inst.ID, Title: inst.Title, Warning: warning}
	if drained != nil {
		out.Drained = drained.ID
		out.DrainedTitle = drained.Title
	}
	// Journaled after the verdict: RecordSessionEvent writes synchronously
	// and a slow health volume must not delay the answer.
	AfterFunc(ctx, func() {
		session.RecordSessionEvent(in.Profile, inst.ID, health.KindStop, nil)
		session.RecallNotifyInstance(inst, health.KindStop)
	})
	return out, nil
}

// drainGroupQueue starts the oldest queued instance in groupPath when a slot
// is free and returns it with the start's runtime durability warning.
// Best-effort: a failed start is reported through EventQueueDrainFailed and
// durably marks that instance errored (when storage is given), so later stops
// do not retry it ahead of the rest of the queue.
func drainGroupQueue(ctx context.Context, storage *session.Storage, groupPath string, instances []*session.Instance, groups []*session.GroupData) (*session.Instance, string) {
	tree := session.NewGroupTreeWithGroups(instances, groups)
	max := session.GroupMaxConcurrent(tree, groupPath)
	if session.IsAtCap(session.CountRunningInGroup(instances, groupPath), max) {
		return nil, ""
	}
	next := session.FindNextQueued(instances, groupPath)
	if next == nil {
		return nil, ""
	}
	runtime, err := startRuntime(next, "")
	err, warning := consumeRuntime(next, runtime, err)
	if err != nil {
		ev := Event{Kind: EventQueueDrainFailed, ID: next.ID, Title: next.Title, Err: err}
		if storage != nil {
			if statusErr := session.PersistSelectedStatus(storage, next, session.StatusError); statusErr != nil {
				ev.Message = fmt.Sprintf("failed to save session error status: %v", statusErr)
				Warn(ctx, ev.Message)
			}
		}
		Emit(ctx, ev)
		return nil, ""
	}
	return next, warning
}
