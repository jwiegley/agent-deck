package session

import (
	"errors"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

func TestRuntimeLifecycle_NonDurableRespawnRequiresExactStableCandidate(t *testing.T) {
	oldGlobal := statedb.GetGlobal()
	oldInventory := runtimeCandidateInventoryFn
	oldRevalidate := runtimeCandidateRevalidateFn
	oldRespawn := runtimeCandidateRespawnFn
	t.Cleanup(func() {
		statedb.SetGlobal(oldGlobal)
		runtimeCandidateInventoryFn = oldInventory
		runtimeCandidateRevalidateFn = oldRevalidate
		runtimeCandidateRespawnFn = oldRespawn
	})
	statedb.SetGlobal(nil)

	exact := tmux.RuntimeCandidate{
		SessionID: "$7", SessionName: "agentdeck_local", SocketName: "isolated",
		PaneID: "%9", PanePID: 4242, InstanceID: "local-one",
		Generation: 3, GenerationKnown: true,
		StatusRevision: 2, Status: "waiting",
		LastStartedUnixNano: time.Unix(100, 0).UnixNano(), StateKnown: true,
		BindingKind: "", BindingValue: "", BindingKnown: true,
	}

	tests := []struct {
		name        string
		candidates  []tmux.RuntimeCandidate
		wantBegin   bool
		wantRespawn bool
	}{
		{name: "exact", candidates: []tmux.RuntimeCandidate{exact}, wantBegin: true, wantRespawn: true},
		{name: "absent", wantBegin: true},
		{name: "ambiguous", candidates: []tmux.RuntimeCandidate{exact, func() tmux.RuntimeCandidate {
			other := exact
			other.SessionID, other.SessionName, other.PaneID, other.PanePID = "$8", "agentdeck_other", "%10", 4343
			return other
		}()}},
		{name: "mismatched_generation", candidates: []tmux.RuntimeCandidate{func() tmux.RuntimeCandidate {
			other := exact
			other.Generation = 2
			return other
		}()}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			instance := &Instance{
				ID: "local-one", Title: test.name, Tool: "cursor", Status: StatusWaiting,
				RuntimeGeneration: 3, StatusRevision: 2, LastStartedAt: time.Unix(100, 0).UTC(),
				TmuxSocketName: "isolated",
				tmuxSession:    &tmux.Session{Name: "agentdeck_local", SocketName: "isolated", InstanceID: "local-one"},
			}
			runtimeCandidateInventoryFn = func(socketName, instanceID string) ([]tmux.RuntimeCandidate, error) {
				if socketName == "isolated" && instanceID == instance.ID {
					return append([]tmux.RuntimeCandidate(nil), test.candidates...), nil
				}
				return nil, nil
			}
			runtimeCandidateRevalidateFn = func(candidate tmux.RuntimeCandidate) (tmux.RuntimeCandidate, error) {
				return candidate, nil
			}
			respawns := 0
			runtimeCandidateRespawnFn = func(*tmux.Session, tmux.RuntimeGenerationCandidate, string) error {
				respawns++
				return nil
			}

			authority, winner, err := instance.beginRuntimeTransition(false)
			if !test.wantBegin {
				var ambiguity *RuntimeReconciliationAmbiguityError
				if !errors.As(err, &ambiguity) || authority != nil || winner != nil {
					t.Fatalf("begin authority=%v winner=%v err=%v, want fail-closed ambiguity", authority, winner, err)
				}
				if respawns != 0 {
					t.Fatalf("ambiguous candidate caused %d respawns", respawns)
				}
				return
			}
			if err != nil || winner != nil || authority == nil {
				t.Fatalf("begin authority=%v winner=%v err=%v", authority, winner, err)
			}
			defer authority.close()
			err = instance.respawnRuntimePane(authority, "resume-command")
			if test.wantRespawn {
				if err != nil || respawns != 1 {
					t.Fatalf("exact candidate respawn err=%v count=%d", err, respawns)
				}
				return
			}
			if !errors.Is(err, statedb.ErrRuntimeGenerationConflict) || respawns != 0 {
				t.Fatalf("absent candidate respawn err=%v count=%d, want generation conflict and no mutation", err, respawns)
			}
		})
	}
}
