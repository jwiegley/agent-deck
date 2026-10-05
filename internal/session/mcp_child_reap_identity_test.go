package session

import (
	"errors"
	"reflect"
	"syscall"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

func stubMCPIdentityReaper(t *testing.T, capture func(int) (tmux.ProcessIdentity, error), reap func([]tmux.ProcessIdentity, tmux.ProcessReapTiming)) {
	t.Helper()
	oldCapture := mcpCaptureProcessIdentityFn
	oldReap := mcpReapProcessIdentitiesFn
	t.Cleanup(func() {
		mcpCaptureProcessIdentityFn = oldCapture
		mcpReapProcessIdentitiesFn = oldReap
	})
	mcpCaptureProcessIdentityFn = capture
	mcpReapProcessIdentitiesFn = reap
}

func cleanupMCPIdentityState(t *testing.T, instance *Instance) {
	t.Helper()
	t.Cleanup(func() {
		instance.mcpPIDsMu.Lock()
		instance.trackedMCPChildIdentities = nil
		instance.mcpPIDsMu.Unlock()
	})
}

func TestRuntimeLifecycle_MCPChildReapCarriesCapturedBirthIdentity(t *testing.T) {
	instance := &Instance{ID: "mcp-identity-preserved"}
	cleanupMCPIdentityState(t, instance)
	want := tmux.ProcessIdentity{PID: 4242, StartToken: "birth-a"}
	var got []tmux.ProcessIdentity
	var gotTiming tmux.ProcessReapTiming
	stubMCPIdentityReaper(t,
		func(pid int) (tmux.ProcessIdentity, error) {
			if pid != want.PID {
				t.Fatalf("capture pid = %d, want %d", pid, want.PID)
			}
			return want, nil
		},
		func(identities []tmux.ProcessIdentity, timing tmux.ProcessReapTiming) {
			got = append([]tmux.ProcessIdentity(nil), identities...)
			gotTiming = timing
		},
	)

	instance.RegisterMCPChild(want.PID)
	instance.reapTrackedMCPChildren()
	if !reflect.DeepEqual(got, []tmux.ProcessIdentity{want}) {
		t.Fatalf("reaper identities = %#v, want exact captured identity %#v", got, want)
	}
	if gotTiming.TermGrace != mcpReapGracePeriod || gotTiming.KillWait != mcpReapVerifyTimeout {
		t.Fatalf("reaper timing = %#v", gotTiming)
	}
}

func TestRuntimeLifecycle_MCPChildRegistrationCaptureFailureFailsClosed(t *testing.T) {
	instance := &Instance{ID: "mcp-identity-capture-failure"}
	cleanupMCPIdentityState(t, instance)
	reapCalls := 0
	stubMCPIdentityReaper(t,
		func(int) (tmux.ProcessIdentity, error) {
			return tmux.ProcessIdentity{}, errors.New("kernel identity unavailable")
		},
		func([]tmux.ProcessIdentity, tmux.ProcessReapTiming) { reapCalls++ },
	)

	instance.RegisterMCPChild(4242)
	instance.reapTrackedMCPChildren()
	if len(instance.TrackedMCPPIDs) != 0 || len(instance.TrackedMCPChildIdentities()) != 0 || reapCalls != 0 {
		t.Fatalf("capture failure registered pids=%v identities=%v reapCalls=%d",
			instance.TrackedMCPPIDs, instance.TrackedMCPChildIdentities(), reapCalls)
	}
}

func TestRuntimeLifecycle_MCPChildReapPIDReuseBeforeTERMAndKILL(t *testing.T) {
	for _, test := range []struct {
		name        string
		reuseBefore syscall.Signal
		wantSignals []syscall.Signal
	}{
		{name: "before TERM", reuseBefore: syscall.SIGTERM},
		{name: "before KILL", reuseBefore: syscall.SIGKILL, wantSignals: []syscall.Signal{syscall.SIGTERM}},
	} {
		t.Run(test.name, func(t *testing.T) {
			instance := &Instance{ID: "mcp-identity-reuse-" + test.name}
			cleanupMCPIdentityState(t, instance)
			captured := tmux.ProcessIdentity{PID: 4242, StartToken: "birth-a"}
			currentToken := captured.StartToken
			var signals []syscall.Signal
			stubMCPIdentityReaper(t,
				func(int) (tmux.ProcessIdentity, error) { return captured, nil },
				func(identities []tmux.ProcessIdentity, _ tmux.ProcessReapTiming) {
					if len(identities) != 1 || identities[0] != captured {
						t.Fatalf("reaper lost captured identity: %#v", identities)
					}
					if test.reuseBefore == syscall.SIGTERM {
						currentToken = "birth-b"
					}
					if currentToken == identities[0].StartToken {
						signals = append(signals, syscall.SIGTERM)
					}
					if test.reuseBefore == syscall.SIGKILL {
						currentToken = "birth-b"
					}
					if currentToken == identities[0].StartToken {
						signals = append(signals, syscall.SIGKILL)
					}
				},
			)

			instance.RegisterMCPChild(captured.PID)
			instance.reapTrackedMCPChildren()
			if !reflect.DeepEqual(signals, test.wantSignals) {
				t.Fatalf("signals = %v, want %v", signals, test.wantSignals)
			}
		})
	}
}

func TestRuntimeLifecycle_RegisterMCPChildIdentityRefreshesSamePIDBirth(t *testing.T) {
	instance := &Instance{ID: "mcp-identity-refresh"}
	cleanupMCPIdentityState(t, instance)
	instance.RegisterMCPChildIdentity(tmux.ProcessIdentity{PID: 4242, StartToken: "birth-a"})
	instance.RegisterMCPChildIdentity(tmux.ProcessIdentity{PID: 4242, StartToken: "birth-b"})
	want := []tmux.ProcessIdentity{{PID: 4242, StartToken: "birth-b"}}
	if got := instance.TrackedMCPChildIdentities(); !reflect.DeepEqual(got, want) {
		t.Fatalf("refreshed identities = %#v, want %#v", got, want)
	}
}

func TestRuntimeLifecycle_MCPDiscoveryRejectsTreeAndLifetimeSubstitution(t *testing.T) {
	for _, test := range []struct {
		name              string
		afterProcessTable []byte
		identityAlive     bool
	}{
		{
			name:              "membership changed",
			afterProcessTable: []byte("100 1\n200 100\n301 200\n"),
			identityAlive:     true,
		},
		{
			name:              "same pid lifetime changed",
			afterProcessTable: []byte("100 1\n200 100\n300 200\n"),
			identityAlive:     false,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			oldCapture := mcpCaptureProcessIdentitiesFn
			oldClose := mcpCloseProcessIdentitiesFn
			oldMatches := mcpProcessIdentityMatchesFn
			oldPane := mcpReadPanePIDFn
			oldSnapshot := mcpProcessTableSnapshotFn
			t.Cleanup(func() {
				mcpCaptureProcessIdentitiesFn = oldCapture
				mcpCloseProcessIdentitiesFn = oldClose
				mcpProcessIdentityMatchesFn = oldMatches
				mcpReadPanePIDFn = oldPane
				mcpProcessTableSnapshotFn = oldSnapshot
			})

			mcpReadPanePIDFn = func(*Instance) int { return 100 }
			snapshots := 0
			mcpProcessTableSnapshotFn = func() ([]byte, error) {
				snapshots++
				if snapshots == 1 {
					return []byte("100 1\n200 100\n300 200\n"), nil
				}
				return test.afterProcessTable, nil
			}
			mcpCaptureProcessIdentitiesFn = func(pids []int) ([]tmux.ProcessIdentity, error) {
				if !reflect.DeepEqual(pids, []int{300}) {
					t.Fatalf("captured MCP descendants = %v, want [300]", pids)
				}
				return []tmux.ProcessIdentity{{PID: 300, StartToken: "birth-a"}}, nil
			}
			mcpProcessIdentityMatchesFn = func(tmux.ProcessIdentity) bool { return test.identityAlive }
			closeCalls := 0
			mcpCloseProcessIdentitiesFn = func(identities []tmux.ProcessIdentity) {
				closeCalls++
				if len(identities) != 1 || identities[0].PID != 300 {
					t.Fatalf("closed MCP identities = %#v", identities)
				}
			}

			instance := &Instance{tmuxSession: &tmux.Session{}}
			identities, err := instance.captureMCPChildrenFromPaneTree()
			if err == nil || identities != nil {
				t.Fatalf("substituted MCP tree = %#v, err=%v; want rejection", identities, err)
			}
			if snapshots != 2 || closeCalls != 1 {
				t.Fatalf("snapshots=%d closes=%d, want two snapshots and one cleanup", snapshots, closeCalls)
			}
		})
	}
}
