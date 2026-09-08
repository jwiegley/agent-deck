package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func stableSessionTargetForTest(name, socket string) stableSessionTarget {
	return stableSessionTarget{
		SessionName: name,
		SessionID:   "$7",
		SocketName:  socket,
		PaneID:      "%9",
		PanePID:     4242,
	}
}

func installFailedLiveTmux(t *testing.T) {
	t.Helper()
	binDir := t.TempDir()
	fakeTmux := filepath.Join(binDir, "tmux")
	script := `#!/bin/sh
case "$*" in
  *"kill-session"*) exit 23 ;;
  *"has-session"*) exit 0 ;;
  *) exit 0 ;;
esac
`
	if err := os.WriteFile(fakeTmux, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestRuntimeLifecycle_KillAndWaitTargetsCapturedSocket(t *testing.T) {
	oldCapture := captureStableSessionProcessTreeFn
	captureStableSessionProcessTreeFn = func(s *Session) (stableSessionTarget, []ProcessIdentity, error) {
		return stableSessionTargetForTest(s.Name, s.SocketName), nil, nil
	}
	t.Cleanup(func() { captureStableSessionProcessTreeFn = oldCapture })
	binDir := t.TempDir()
	argsLog := filepath.Join(binDir, "args.log")
	fakeTmux := filepath.Join(binDir, "tmux")
	if err := os.WriteFile(fakeTmux, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$AGENT_DECK_TMUX_ARGS_LOG\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_DECK_TMUX_ARGS_LOG", argsLog)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	s := &Session{Name: "captured", SocketName: "runtime-socket"}
	if err := s.KillAndWait(); err != nil {
		raw, _ := os.ReadFile(argsLog)
		t.Fatalf("KillAndWait: %v; observed tmux commands: %q", err, raw)
	}
	raw, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	foundKill := false
	for _, line := range lines {
		if !strings.HasPrefix(line, "-u -L runtime-socket ") {
			t.Fatalf("tmux command targeted wrong socket: %q", line)
		}
		if strings.HasPrefix(line, "-u -L runtime-socket if-shell -F -t %9 ") &&
			strings.Contains(line, "'kill-session' '-t' '$7'") &&
			!strings.Contains(line, "kill-session -t captured") {
			foundKill = true
		}
	}
	if !foundKill {
		t.Fatalf("captured-socket kill not observed in %q", lines)
	}
}

func TestRuntimeLifecycle_KillFailureRetainsLivePane(t *testing.T) {
	installFailedLiveTmux(t)
	closed := 0
	identity := ProcessIdentity{
		PID: 4242, StartToken: "birth-a",
		handle: &processIdentityHandle{closeFn: func() error {
			closed++
			return nil
		}},
	}
	oldCapture := captureStableSessionProcessTreeFn
	captureStableSessionProcessTreeFn = func(s *Session) (stableSessionTarget, []ProcessIdentity, error) {
		return stableSessionTargetForTest(s.Name, s.SocketName), []ProcessIdentity{identity}, nil
	}
	t.Cleanup(func() { captureStableSessionProcessTreeFn = oldCapture })

	s := &Session{Name: "still-live", SocketName: "runtime-socket"}
	if err := s.Kill(); err == nil {
		t.Fatal("Kill succeeded even though kill-session failed and the session remained live")
	}
	if closed != 1 {
		t.Fatalf("retained handle closes before return = %d, want 1", closed)
	}
}

func TestRuntimeLifecycle_KillAndWaitFailureDoesNotSignalLivePane(t *testing.T) {
	installFailedLiveTmux(t)
	oldCapture := captureStableSessionProcessTreeFn
	captureStableSessionProcessTreeFn = func(s *Session) (stableSessionTarget, []ProcessIdentity, error) {
		return stableSessionTargetForTest(s.Name, s.SocketName), []ProcessIdentity{{PID: 4242, StartToken: "birth-a"}}, nil
	}
	t.Cleanup(func() { captureStableSessionProcessTreeFn = oldCapture })

	signals := 0
	stubProcessReapIO(t, func(int) (string, error) {
		return "birth-a", nil
	}, func(ProcessIdentity, os.Signal) (bool, error) {
		signals++
		return true, nil
	})

	s := &Session{Name: "still-live", SocketName: "runtime-socket"}
	if err := s.KillAndWait(); err == nil {
		t.Fatal("KillAndWait succeeded even though kill-session failed and the session remained live")
	}
	if signals != 0 {
		t.Fatalf("failed kill-session authorized %d auxiliary signals against a live pane", signals)
	}
}

func TestRuntimeLifecycle_GenericMutationRejectsSameNameReplacement(t *testing.T) {
	tests := []struct {
		name       string
		invoke     func(*Session) error
		wantBranch []string
	}{
		{
			name:       "kill",
			invoke:     func(s *Session) error { return s.Kill() },
			wantBranch: []string{"'kill-session' '-t' '$7'"},
		},
		{
			name:       "kill and wait",
			invoke:     func(s *Session) error { return s.KillAndWait() },
			wantBranch: []string{"'kill-session' '-t' '$7'"},
		},
		{
			name:   "respawn",
			invoke: func(s *Session) error { s.clearOnRestart = true; return s.RespawnPane("") },
			wantBranch: []string{
				"'clear-history' '-t' '%9'",
				"'respawn-pane' '-k' '-t' '%9'",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldCapture := captureStableSessionProcessTreeFn
			oldMutation := stableSessionConditionalMutationFn
			oldProbe := probeSessionExistenceFn
			t.Cleanup(func() {
				captureStableSessionProcessTreeFn = oldCapture
				stableSessionConditionalMutationFn = oldMutation
				probeSessionExistenceFn = oldProbe
			})

			closed := 0
			identity := ProcessIdentity{
				PID: 4242, StartToken: "birth-a",
				handle: &processIdentityHandle{closeFn: func() error {
					closed++
					return nil
				}},
			}
			captureStableSessionProcessTreeFn = func(s *Session) (stableSessionTarget, []ProcessIdentity, error) {
				return stableSessionTargetForTest(s.Name, s.SocketName), []ProcessIdentity{identity}, nil
			}
			probeSessionExistenceFn = func(socketName, target string) (sessionExistence, error) {
				if socketName != "isolated" || target != "$7" {
					t.Fatalf("existence probe target = %q/%q, want isolated/$7", socketName, target)
				}
				return sessionExistencePresent, nil
			}

			mutationCalls := 0
			stableSessionConditionalMutationFn = func(ctx context.Context, socketName string, args ...string) ([]byte, error) {
				mutationCalls++
				if socketName != "isolated" {
					t.Fatalf("conditional socket = %q, want isolated", socketName)
				}
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("conditional mutation has no deadline")
				}
				wantPrefix := []string{"if-shell", "-F", "-t", "%9"}
				if len(args) != 7 || !reflect.DeepEqual(args[:4], wantPrefix) {
					t.Fatalf("conditional args = %#v, want prefix %#v", args, wantPrefix)
				}
				for _, proof := range []string{
					"#{==:#{session_id},#{l:$7}}",
					"#{==:#{session_name},#{l:reused}}",
					"#{==:#{pane_id},#{l:%9}}",
					"#{==:#{pane_pid},#{l:4242}}",
				} {
					if !strings.Contains(args[4], proof) {
						t.Errorf("conditional proof %q lacks %q", args[4], proof)
					}
				}
				for _, command := range tt.wantBranch {
					if !strings.Contains(args[5], command) {
						t.Errorf("mutation branch %q lacks %q", args[5], command)
					}
				}
				if strings.Contains(args[5], "reused") {
					t.Errorf("mutation branch targets reusable session name: %q", args[5])
				}
				return []byte("agent-deck-stable-session-target-changed\n"), nil
			}

			s := &Session{Name: "reused", SocketName: "isolated"}
			err := tt.invoke(s)
			if !errors.Is(err, errStableSessionTargetChanged) {
				t.Fatalf("replacement error = %v, want %v", err, errStableSessionTargetChanged)
			}
			if mutationCalls != 1 {
				t.Fatalf("conditional mutation calls = %d, want 1", mutationCalls)
			}
			if closed != 1 {
				t.Fatalf("retained handle closes = %d, want 1", closed)
			}
		})
	}
}

func TestRuntimeLifecycle_IndeterminateAbsenceNeverAuthorizesReaping(t *testing.T) {
	tests := []struct {
		name   string
		invoke func(*Session) error
	}{
		{name: "kill", invoke: func(s *Session) error { return s.Kill() }},
		{name: "kill and wait", invoke: func(s *Session) error { return s.KillAndWait() }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldCapture := captureStableSessionProcessTreeFn
			oldMutation := stableSessionConditionalMutationFn
			oldProbe := probeSessionExistenceFn
			t.Cleanup(func() {
				captureStableSessionProcessTreeFn = oldCapture
				stableSessionConditionalMutationFn = oldMutation
				probeSessionExistenceFn = oldProbe
			})

			closed := 0
			identity := ProcessIdentity{
				PID: 4242, StartToken: "birth-a",
				handle: &processIdentityHandle{closeFn: func() error {
					closed++
					return nil
				}},
			}
			captureStableSessionProcessTreeFn = func(s *Session) (stableSessionTarget, []ProcessIdentity, error) {
				return stableSessionTargetForTest(s.Name, s.SocketName), []ProcessIdentity{identity}, nil
			}
			mutationErr := errors.New("tmux mutation failed")
			stableSessionConditionalMutationFn = func(context.Context, string, ...string) ([]byte, error) {
				return nil, mutationErr
			}
			probeSessionExistenceFn = func(string, string) (sessionExistence, error) {
				return sessionExistenceIndeterminate, errors.New("permission denied")
			}

			err := tt.invoke(&Session{Name: "still-live", SocketName: "isolated"})
			if !errors.Is(err, mutationErr) {
				t.Fatalf("error = %v, want mutation failure preserved", err)
			}
			if closed != 1 {
				t.Fatalf("retained handle closes = %d, want 1", closed)
			}
		})
	}
}

func TestRuntimeLifecycle_ProbeSessionExistence_FailuresAreIndeterminate(t *testing.T) {
	tests := []struct {
		name   string
		stderr string
		sleep  time.Duration
		noTmux bool
	}{
		{name: "permission", stderr: "permission denied"},
		{name: "socket", stderr: "error connecting to /tmp/tmux.sock (Permission denied)"},
		{name: "protocol", stderr: "protocol error"},
		{name: "timeout", sleep: time.Second},
		{name: "launch", noTmux: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			binDir := t.TempDir()
			if !tt.noTmux {
				fakeTmux := filepath.Join(binDir, "tmux")
				script := "#!/bin/sh\n"
				if tt.sleep > 0 {
					script += "/bin/sleep 1\n"
				}
				script += fmt.Sprintf("printf '%%s\\n' %q >&2\nexit 42\n", tt.stderr)
				if err := os.WriteFile(fakeTmux, []byte(script), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", binDir)
			oldTimeout := hasSessionProbeTimeout
			if tt.sleep > 0 {
				hasSessionProbeTimeout = 20 * time.Millisecond
			}
			t.Cleanup(func() { hasSessionProbeTimeout = oldTimeout })

			state, err := probeSessionExistence("isolated", "$7")
			if state != sessionExistenceIndeterminate || err == nil {
				t.Fatalf("probe = (%v, %v), want indeterminate error", state, err)
			}
		})
	}
}

func TestRuntimeLifecycle_ProbeSessionExistence_CanonicalMissingSessionIsAbsent(t *testing.T) {
	binDir := t.TempDir()
	fakeTmux := filepath.Join(binDir, "tmux")
	script := "#!/bin/sh\nprintf '%s\\n' \"can't find session: \\$7\" >&2\nexit 1\n"
	if err := os.WriteFile(fakeTmux, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)

	state, err := probeSessionExistence("isolated", "$7")
	if err != nil || state != sessionExistenceAbsent {
		t.Fatalf("probe = (%v, %v), want absent", state, err)
	}
}

func TestRuntimeLifecycle_StableSessionMutationCommandTargetsImmutableIDs(t *testing.T) {
	target := stableSessionTargetForTest("reused", "isolated")
	condition, err := stableSessionTargetCondition(target)
	if err != nil {
		t.Fatal(err)
	}
	wantProofs := []string{"$7", "reused", "%9", strconv.Itoa(target.PanePID)}
	for _, proof := range wantProofs {
		if !strings.Contains(condition, proof) {
			t.Errorf("condition %q lacks %q", condition, proof)
		}
	}
}
