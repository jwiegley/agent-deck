package session

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

func runtimeRespawnCandidateForTest() tmux.RuntimeCandidate {
	return tmux.RuntimeCandidate{
		SessionID: "$7", SessionName: "agentdeck_runtime", SocketName: "isolated",
		PaneID: "%9", PanePID: 4242, InstanceID: "one",
		Generation: 3, GenerationKnown: true,
		StatusRevision: 2, Status: "waiting", LastStartedUnixNano: time.Unix(100, 0).UnixNano(), StateKnown: true,
		BindingKind: "claude", BindingValue: "conversation", BindingKnown: true,
	}
}

func runtimeRespawnAuthorityForTest(candidate tmux.RuntimeCandidate) *runtimeTransitionAuthority {
	return &runtimeTransitionAuthority{
		expected: statedb.RuntimeState{
			InstanceID: candidate.InstanceID, Generation: candidate.Generation,
			TmuxSession: candidate.SessionName, TmuxSocketName: candidate.SocketName,
		},
		currentCandidate: &candidate,
	}
}

func TestRuntimeLifecycle_RespawnRevalidatesStableCandidateImmediatelyBeforeMutation(t *testing.T) {
	oldRevalidate := runtimeCandidateRevalidateFn
	oldRespawn := runtimeCandidateRespawnFn
	oldFault := runtimeTransitionFaultFn
	t.Cleanup(func() {
		runtimeCandidateRevalidateFn = oldRevalidate
		runtimeCandidateRespawnFn = oldRespawn
		runtimeTransitionFaultFn = oldFault
	})
	candidate := runtimeRespawnCandidateForTest()
	authority := runtimeRespawnAuthorityForTest(candidate)
	instance := &Instance{
		ID: candidate.InstanceID,
		tmuxSession: &tmux.Session{
			Name: candidate.SessionName, SocketName: candidate.SocketName, InstanceID: candidate.InstanceID,
		},
	}
	var events []string
	runtimeCandidateRevalidateFn = func(got tmux.RuntimeCandidate) (tmux.RuntimeCandidate, error) {
		events = append(events, "revalidate")
		if got != candidate {
			t.Fatalf("revalidation candidate = %#v, want %#v", got, candidate)
		}
		return got, nil
	}
	runtimeCandidateRespawnFn = func(_ *tmux.Session, got tmux.RuntimeGenerationCandidate, command string) error {
		events = append(events, "conditional-invalidate-and-respawn")
		if got.SessionID != candidate.SessionID || got.SessionName != candidate.SessionName ||
			got.PaneID != candidate.PaneID || got.PanePID != candidate.PanePID ||
			got.SocketName != candidate.SocketName || command != "resume-command" {
			t.Fatalf("respawn candidate=%#v command=%q", got, command)
		}
		return nil
	}
	runtimeTransitionFaultFn = func(stage RuntimeTransitionStage, _ statedb.RuntimeState) error {
		events = append(events, string(stage))
		return nil
	}

	if err := instance.respawnRuntimePane(authority, "resume-command"); err != nil {
		t.Fatal(err)
	}
	want := "revalidate,conditional-invalidate-and-respawn,after-respawn-before-stamp"
	if got := strings.Join(events, ","); got != want {
		t.Fatalf("respawn events = %q, want %q", got, want)
	}
}

func TestRuntimeLifecycle_RespawnSameNameReplacementFailsBeforeMutation(t *testing.T) {
	oldRevalidate := runtimeCandidateRevalidateFn
	oldRespawn := runtimeCandidateRespawnFn
	oldFault := runtimeTransitionFaultFn
	t.Cleanup(func() {
		runtimeCandidateRevalidateFn = oldRevalidate
		runtimeCandidateRespawnFn = oldRespawn
		runtimeTransitionFaultFn = oldFault
	})
	candidate := runtimeRespawnCandidateForTest()
	authority := runtimeRespawnAuthorityForTest(candidate)
	instance := &Instance{
		ID: candidate.InstanceID,
		tmuxSession: &tmux.Session{
			Name: candidate.SessionName, SocketName: candidate.SocketName, InstanceID: candidate.InstanceID,
		},
	}
	replacement := candidate
	replacement.SessionID = "$8"
	replacement.PaneID = "%10"
	replacement.PanePID++
	runtimeCandidateRevalidateFn = func(tmux.RuntimeCandidate) (tmux.RuntimeCandidate, error) {
		return replacement, nil
	}
	mutationCalls := 0
	runtimeCandidateRespawnFn = func(*tmux.Session, tmux.RuntimeGenerationCandidate, string) error {
		mutationCalls++
		return nil
	}
	runtimeTransitionFaultFn = func(RuntimeTransitionStage, statedb.RuntimeState) error {
		t.Fatal("fault boundary reached after rejected replacement")
		return nil
	}

	err := instance.respawnRuntimePane(authority, "resume-command")
	var ambiguity *RuntimeReconciliationAmbiguityError
	if !errors.As(err, &ambiguity) || len(ambiguity.Candidates) != 2 {
		t.Fatalf("same-name replacement error = %v, want two-candidate ambiguity", err)
	}
	if mutationCalls != 0 {
		t.Fatalf("same-name replacement received %d mutations", mutationCalls)
	}
}

func TestRuntimeLifecycle_RuntimeStampPublishesEnvironmentGenerationLast(t *testing.T) {
	oldSetEnv := runtimeCandidateSetEnvFn
	oldCleanupStamp := runtimeCleanupIdentityStampFn
	t.Cleanup(func() {
		runtimeCandidateSetEnvFn = oldSetEnv
		runtimeCleanupIdentityStampFn = oldCleanupStamp
	})
	next := statedb.RuntimeState{
		InstanceID: "one", Generation: 4, StatusRevision: 0,
		TmuxSession: "agentdeck_runtime", TmuxSocketName: "isolated",
		Status: "waiting", LastStartedAt: time.Unix(200, 123).UTC(),
	}
	failureSteps := []string{
		"AGENTDECK_INSTANCE_ID",
		"AGENTDECK_RUNTIME_STATUS_REVISION",
		"AGENTDECK_RUNTIME_STATUS",
		"AGENTDECK_RUNTIME_STARTED_UNIX_NANO",
		"AGENTDECK_RUNTIME_BINDING_KIND",
		"AGENTDECK_RUNTIME_BINDING_VALUE",
		"cleanup-identity",
		"AGENTDECK_RUNTIME_GENERATION",
	}
	for _, failStep := range append(failureSteps, "") {
		name := failStep
		if name == "" {
			name = "success"
		}
		t.Run(name, func(t *testing.T) {
			environment := make(map[string]string)
			var order []string
			injected := errors.New("injected partial stamp")
			runtimeCandidateSetEnvFn = func(_ *tmux.Session, key, value string) error {
				order = append(order, key)
				if key == failStep {
					return injected
				}
				environment[key] = value
				return nil
			}
			runtimeCleanupIdentityStampFn = func(*tmux.Session, string, uint64, string, string) error {
				order = append(order, "cleanup-identity")
				if _, published := environment["AGENTDECK_RUNTIME_GENERATION"]; published {
					t.Fatal("environment generation was published before cleanup identity completed")
				}
				if failStep == "cleanup-identity" {
					return injected
				}
				return nil
			}

			err := stampRuntimeCandidate(&tmux.Session{Name: next.TmuxSession}, next, "claude", "conversation")
			if failStep != "" {
				if !errors.Is(err, injected) {
					t.Fatalf("partial stamp error = %v, want injected failure at %s", err, failStep)
				}
				if value, published := environment["AGENTDECK_RUNTIME_GENERATION"]; published {
					t.Fatalf("partial stamp published generation %q after failure at %s; order=%v", value, failStep, order)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := environment["AGENTDECK_RUNTIME_GENERATION"]; got != "4" {
				t.Fatalf("completed generation = %q, want 4", got)
			}
			if got := order[len(order)-1]; got != "AGENTDECK_RUNTIME_GENERATION" {
				t.Fatalf("final stamp step = %q, order=%v", got, order)
			}
		})
	}
}
