package statedb

import (
	"encoding/json"
)

// RuntimeLaunchDecision is the immutable launch intent carried through publication
// and reconciliation after the physical spawn has completed.
type RuntimeLaunchDecision struct {
	Tool             string
	PiSocketExported bool
	BindingKind      string
}

// RuntimePublication identifies launch metadata committed with a runtime.
type RuntimePublication struct {
	LaunchRecorded bool
	Stamps         WriteStamps
}

// RuntimeLaunchRecord binds the decision to one logical and physical runtime.
type RuntimeLaunchRecord struct {
	InstanceID       string `json:"launch_instance_id"`
	Incarnation      string `json:"launch_incarnation"`
	Generation       uint64 `json:"launch_runtime_generation"`
	TmuxSession      string `json:"launch_tmux_session"`
	TmuxSocketName   string `json:"launch_tmux_socket_name"`
	LastStartedAt    int64  `json:"launch_last_started_at"`
	Tool             string `json:"launched_tool"`
	PiSocketExported bool   `json:"pi_socket_exported"`
}

func NewRuntimeLaunchRecord(state RuntimeState, incarnation string, decision RuntimeLaunchDecision) RuntimeLaunchRecord {
	return RuntimeLaunchRecord{
		InstanceID: state.InstanceID, Incarnation: incarnation, Generation: state.Generation,
		TmuxSession: state.TmuxSession, TmuxSocketName: state.TmuxSocketName,
		LastStartedAt: runtimeUnix(state.LastStartedAt),
		Tool:          decision.Tool, PiSocketExported: decision.PiSocketExported,
	}
}

func (r RuntimeLaunchRecord) Matches(state RuntimeState, incarnation string) bool {
	return r.Tool != "" && r.InstanceID == state.InstanceID && r.Incarnation == incarnation &&
		r.Generation == state.Generation && r.TmuxSession == state.TmuxSession &&
		r.TmuxSocketName == state.TmuxSocketName && r.LastStartedAt == runtimeUnix(state.LastStartedAt)
}

func writeRuntimeLaunchDecisionTx(tx *immediateTransaction, state RuntimeState, incarnation string, decision RuntimeLaunchDecision) (bool, error) {
	raw, err := json.Marshal(NewRuntimeLaunchRecord(state, incarnation, decision))
	if err != nil {
		return false, err
	}
	result, err := tx.Exec(`UPDATE instances SET tool_data = json_patch(
	    COALESCE(NULLIF(tool_data, ''), '{}'), ?) WHERE id = ?`, string(raw), state.InstanceID)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected == 1, err
}
