package tmux

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRuntimeLifecycle_RuntimeBindingAuthorityEnvironmentIsNeverWrittenGlobally(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	for _, tree := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, tree), func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, parseErr := parser.ParseFile(fset, path, nil, 0)
			if parseErr != nil {
				return parseErr
			}
			ast.Inspect(file, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				literals := make(map[string]bool)
				for _, arg := range call.Args {
					ast.Inspect(arg, func(inner ast.Node) bool {
						literal, ok := inner.(*ast.BasicLit)
						if ok && literal.Kind == token.STRING {
							if value, err := strconv.Unquote(literal.Value); err == nil {
								literals[value] = true
							}
						}
						return true
					})
				}
				if literals["set-environment"] && literals["-g"] {
					t.Errorf("production global tmux environment write at %s; runtime authority stamps must remain session-only", fset.Position(call.Pos()))
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestRuntimeLifecycle_RuntimeCleanupInventoryUsesOnlySessionOptionAuthority(t *testing.T) {
	format := runtimeCleanupCandidateFormat()
	for _, forbidden := range []string{
		"#{E:", "AGENTDECK_INSTANCE_ID", "AGENTDECK_RUNTIME_GENERATION",
		"CLAUDE_SESSION_ID", "COPILOT_SESSION_ID", "CODEX_SESSION_ID",
		"GEMINI_SESSION_ID", "OPENCODE_SESSION_ID",
		runtimeCleanupInstanceOption, runtimeCleanupGenerationOption,
		runtimeCleanupBindingKeyOption, runtimeCleanupBindingValOption,
	} {
		if strings.Contains(format, forbidden) {
			t.Fatalf("stable cleanup inventory format %q reads inherited authority %q", format, forbidden)
		}
	}
}

func runtimeBindingCandidateForTest() RuntimeBindingCandidate {
	return RuntimeBindingCandidate{
		SessionName: "agentdeck_peer", SessionID: "$7", SocketName: "isolated",
		PaneID: "%9", PanePID: 4242,
		InstanceID: "peer-instance", InstanceKnown: true,
		Generation: 3, GenerationKnown: true,
		BindingKey: "CLAUDE_SESSION_ID", BindingValue: "conversation-id",
	}
}

func legacyRuntimeAdoptionForTest() LegacyRuntimeAdoption {
	return LegacyRuntimeAdoption{
		Candidate: RuntimeCandidate{
			SessionID: "$7", SessionName: "agentdeck_legacy", SocketName: "isolated",
			PaneID: "%9", PanePID: 4242, InstanceID: "legacy-instance",
			ProofError: "missing AGENTDECK_RUNTIME_GENERATION",
		},
		StatusRevision: 11, Status: "running", StartedUnixNano: 1234,
		BindingKind: "claude", BindingValue: "conversation-id",
	}
}

func stampedLegacyRuntimeAdoptionForTest() LegacyRuntimeAdoption {
	adoption := legacyRuntimeAdoptionForTest()
	adoption.Candidate.GenerationKnown = true
	adoption.Candidate.StatusRevision = adoption.StatusRevision
	adoption.Candidate.Status = adoption.Status
	adoption.Candidate.LastStartedUnixNano = adoption.StartedUnixNano
	adoption.Candidate.StateKnown = true
	adoption.Candidate.BindingKind = adoption.BindingKind
	adoption.Candidate.BindingValue = adoption.BindingValue
	adoption.Candidate.BindingKnown = true
	adoption.Candidate.ProofError = ""
	return adoption
}

func stubLegacyRuntimeStampConditionalForTest(t *testing.T) *[][]string {
	t.Helper()
	oldMutation := runtimeBindingConditionalKillFn
	var calls [][]string
	runtimeBindingConditionalKillFn = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		return nil, nil
	}
	t.Cleanup(func() { runtimeBindingConditionalKillFn = oldMutation })
	return &calls
}

func stubLegacyRuntimeCandidateStampProbesForTest(t *testing.T) {
	t.Helper()
	oldRevalidate := legacyRuntimeCandidateRevalidateFn
	oldCapture := processCaptureIdentityFn
	oldAlive := processIdentityAliveFn
	legacyRuntimeCandidateRevalidateFn = func(candidate RuntimeCandidate) (RuntimeCandidate, error) {
		return candidate, nil
	}
	processCaptureIdentityFn = func(pid int) (ProcessIdentity, error) {
		return ProcessIdentity{PID: pid, StartToken: fmt.Sprintf("legacy-start-%d", pid)}, nil
	}
	processIdentityAliveFn = func(ProcessIdentity) (bool, error) { return true, nil }
	t.Cleanup(func() {
		legacyRuntimeCandidateRevalidateFn = oldRevalidate
		processCaptureIdentityFn = oldCapture
		processIdentityAliveFn = oldAlive
	})
}

func TestRuntimeLifecycle_AdoptLegacyRuntimeCandidateUsesOneExactStableIDConditional(t *testing.T) {
	oldMutation := runtimeBindingConditionalKillFn
	t.Cleanup(func() { runtimeBindingConditionalKillFn = oldMutation })
	adoption := legacyRuntimeAdoptionForTest()
	var gotSocket string
	var got []string
	runtimeBindingConditionalKillFn = func(_ context.Context, socketName string, args ...string) ([]byte, error) {
		gotSocket = socketName
		got = append([]string(nil), args...)
		return nil, nil
	}

	if err := AdoptLegacyRuntimeCandidate(adoption); err != nil {
		t.Fatal(err)
	}
	if gotSocket != "isolated" {
		t.Fatalf("adoption socket = %q, want isolated", gotSocket)
	}
	wantCondition := "#{&&:#{&&:#{==:#{session_id},#{l:$7}},#{&&:#{==:#{session_name},#{l:agentdeck_legacy}},#{&&:#{==:#{pane_id},#{l:%9}},#{&&:#{==:#{pane_pid},#{l:4242}},#{==:#{E:AGENTDECK_INSTANCE_ID},#{l:legacy-instance}}}}}},#{&&:#{==:#{E:AGENTDECK_RUNTIME_GENERATION},},#{==:#{@agentdeck_runtime_generation},}}}"
	wantCommand := strings.Join([]string{
		"'set-environment' '-t' '$7' 'AGENTDECK_INSTANCE_ID' 'legacy-instance'",
		"'set-environment' '-t' '$7' 'AGENTDECK_RUNTIME_STATUS_REVISION' '11'",
		"'set-environment' '-t' '$7' 'AGENTDECK_RUNTIME_STATUS' 'running'",
		"'set-environment' '-t' '$7' 'AGENTDECK_RUNTIME_STARTED_UNIX_NANO' '1234'",
		"'set-environment' '-t' '$7' 'AGENTDECK_RUNTIME_BINDING_KIND' 'claude'",
		"'set-environment' '-t' '$7' 'AGENTDECK_RUNTIME_BINDING_VALUE' 'conversation-id'",
		"'set-option' '-t' '$7' '@agentdeck_runtime_instance_id' 'legacy-instance'",
		"'set-option' '-t' '$7' '@agentdeck_runtime_binding_key' 'CLAUDE_SESSION_ID'",
		"'set-option' '-t' '$7' '@agentdeck_runtime_binding_value' 'conversation-id'",
		"'set-option' '-t' '$7' '@agentdeck_runtime_generation' '0'",
		"'set-environment' '-t' '$7' 'AGENTDECK_RUNTIME_GENERATION' '0'",
	}, " ; ")
	want := []string{
		"if-shell", "-F", "-t", "%9", wantCondition, wantCommand,
		"display-message -p agent-deck-legacy-runtime-candidate-changed",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("legacy adoption argv = %#v, want %#v", got, want)
	}
	if strings.Contains(got[5], adoption.Candidate.SessionName) || strings.Contains(got[5], "'-g'") {
		t.Fatalf("legacy adoption mutates by name or globally: %q", got[5])
	}
}

func TestRuntimeLifecycle_AdoptLegacyRuntimeCandidateClassifiesMismatch(t *testing.T) {
	oldMutation := runtimeBindingConditionalKillFn
	t.Cleanup(func() { runtimeBindingConditionalKillFn = oldMutation })
	adoption := legacyRuntimeAdoptionForTest()
	for _, test := range []struct {
		name      string
		output    []byte
		invokeErr error
	}{
		{name: "false branch", output: []byte("agent-deck-legacy-runtime-candidate-changed\n")},
		{name: "conditional failure", invokeErr: errors.New("tmux unavailable")},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtimeBindingConditionalKillFn = func(context.Context, string, ...string) ([]byte, error) {
				return test.output, test.invokeErr
			}
			err := AdoptLegacyRuntimeCandidate(adoption)
			if !errors.Is(err, ErrLegacyRuntimeCandidateChanged) {
				t.Fatalf("legacy mismatch error = %v, want ErrLegacyRuntimeCandidateChanged", err)
			}
		})
	}
}

func TestRuntimeLifecycle_AdoptLegacyRuntimeCandidateRejectsKnownOrMalformedGeneration(t *testing.T) {
	oldMutation := runtimeBindingConditionalKillFn
	t.Cleanup(func() { runtimeBindingConditionalKillFn = oldMutation })
	calls := 0
	runtimeBindingConditionalKillFn = func(context.Context, string, ...string) ([]byte, error) {
		calls++
		return nil, nil
	}
	base := legacyRuntimeAdoptionForTest()
	if !IsUnstampedLegacyRuntimeCandidate(base.Candidate) {
		t.Fatal("exact missing-generation candidate was not recognized")
	}
	for _, test := range []struct {
		name          string
		wantUnstamped bool
		mutate        func(*RuntimeCandidate)
	}{
		{name: "known zero", mutate: func(candidate *RuntimeCandidate) { candidate.GenerationKnown = true }},
		{name: "known nonzero", mutate: func(candidate *RuntimeCandidate) { candidate.Generation, candidate.GenerationKnown = 3, true }},
		{name: "unknown nonzero", wantUnstamped: true, mutate: func(candidate *RuntimeCandidate) { candidate.Generation = 3 }},
		{name: "malformed", mutate: func(candidate *RuntimeCandidate) { candidate.ProofError = "invalid AGENTDECK_RUNTIME_GENERATION" }},
		{name: "ambiguous", mutate: func(candidate *RuntimeCandidate) {
			candidate.ProofError = "missing or invalid AGENTDECK_RUNTIME_GENERATION"
		}},
		{name: "compound", mutate: func(candidate *RuntimeCandidate) { candidate.ProofError += "; invalid pane pid" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			adoption := base
			test.mutate(&adoption.Candidate)
			if got := IsUnstampedLegacyRuntimeCandidate(adoption.Candidate); got != test.wantUnstamped {
				t.Fatalf("IsUnstampedLegacyRuntimeCandidate(%s) = %v, want %v", test.name, got, test.wantUnstamped)
			}
			if err := AdoptLegacyRuntimeCandidate(adoption); err == nil {
				t.Fatalf("%s generation evidence was accepted", test.name)
			}
		})
	}
	if calls != 0 {
		t.Fatalf("rejected generation evidence reached tmux %d times", calls)
	}
}

func TestRuntimeLifecycle_AdoptLegacyRuntimeCandidateRejectsUnsafeIdentity(t *testing.T) {
	oldMutation := runtimeBindingConditionalKillFn
	t.Cleanup(func() { runtimeBindingConditionalKillFn = oldMutation })
	calls := 0
	runtimeBindingConditionalKillFn = func(context.Context, string, ...string) ([]byte, error) {
		calls++
		return nil, nil
	}
	base := legacyRuntimeAdoptionForTest()
	for _, test := range []struct {
		name   string
		mutate func(*LegacyRuntimeAdoption)
	}{
		{name: "mutable name format", mutate: func(adoption *LegacyRuntimeAdoption) { adoption.Candidate.SessionName = "agentdeck_bad#name" }},
		{name: "session id", mutate: func(adoption *LegacyRuntimeAdoption) { adoption.Candidate.SessionID = "agentdeck_legacy" }},
		{name: "pane id", mutate: func(adoption *LegacyRuntimeAdoption) { adoption.Candidate.PaneID = "pane-9" }},
		{name: "pane pid", mutate: func(adoption *LegacyRuntimeAdoption) { adoption.Candidate.PanePID = 0 }},
		{name: "instance format", mutate: func(adoption *LegacyRuntimeAdoption) { adoption.Candidate.InstanceID = "bad#instance" }},
		{name: "binding format", mutate: func(adoption *LegacyRuntimeAdoption) { adoption.BindingValue = "bad#binding" }},
		{name: "zero start is not a complete tuple", mutate: func(adoption *LegacyRuntimeAdoption) { adoption.StartedUnixNano = 0 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			adoption := base
			test.mutate(&adoption)
			if err := AdoptLegacyRuntimeCandidate(adoption); err == nil {
				t.Fatalf("unsafe %s was accepted", test.name)
			}
		})
	}
	if calls != 0 {
		t.Fatalf("unsafe identities reached tmux %d times", calls)
	}
}

func TestRuntimeLifecycle_ValidateLegacyRuntimeCandidateStampRequiresExactEnvironment(t *testing.T) {
	base := stampedLegacyRuntimeAdoptionForTest()
	stubLegacyRuntimeCandidateStampProbesForTest(t)
	conditionalCalls := stubLegacyRuntimeStampConditionalForTest(t)
	stubRuntimeCleanupLocalOptions(t, map[string]map[string]runtimeCleanupLocalOption{
		base.Candidate.SessionID: runtimeCleanupLocalOptionsForTest(
			base.Candidate.InstanceID, 0, "CLAUDE_SESSION_ID", base.BindingValue),
	})
	if err := ValidateLegacyRuntimeCandidateStamp(base); err != nil {
		t.Fatalf("exact legacy stamp: %v", err)
	}
	if len(*conditionalCalls) != 1 {
		t.Fatalf("exact legacy stamp conditionals = %d, want 1", len(*conditionalCalls))
	}
	args := (*conditionalCalls)[0]
	if len(args) != 7 || args[0] != "if-shell" || args[1] != "-F" || args[2] != "-t" ||
		args[3] != base.Candidate.PaneID ||
		args[5] != "display-message agent-deck-legacy-runtime-stamp-valid" ||
		args[6] != "display-message -p agent-deck-legacy-runtime-candidate-changed" {
		t.Fatalf("exact legacy stamp conditional = %#v", args)
	}
	for _, proof := range []string{
		"#{==:#{session_id},#{l:$7}}",
		"#{==:#{session_name},#{l:agentdeck_legacy}}",
		"#{==:#{pane_id},#{l:%9}}",
		"#{==:#{pane_pid},#{l:4242}}",
		"#{==:#{E:AGENTDECK_INSTANCE_ID},#{l:legacy-instance}}",
		"#{==:#{E:AGENTDECK_RUNTIME_GENERATION},#{l:0}}",
		"#{==:#{E:AGENTDECK_RUNTIME_STATUS_REVISION},#{l:11}}",
		"#{==:#{E:AGENTDECK_RUNTIME_STATUS},#{l:running}}",
		"#{==:#{E:AGENTDECK_RUNTIME_STARTED_UNIX_NANO},#{l:1234}}",
		"#{==:#{E:AGENTDECK_RUNTIME_BINDING_KIND},#{l:claude}}",
		"#{==:#{E:AGENTDECK_RUNTIME_BINDING_VALUE},#{l:conversation-id}}",
		"#{==:#{@agentdeck_runtime_instance_id},#{l:legacy-instance}}",
		"#{==:#{@agentdeck_runtime_generation},#{l:0}}",
		"#{==:#{@agentdeck_runtime_binding_key},#{l:CLAUDE_SESSION_ID}}",
		"#{==:#{@agentdeck_runtime_binding_value},#{l:conversation-id}}",
	} {
		if !strings.Contains(args[4], proof) {
			t.Errorf("final legacy stamp condition %q lacks %q", args[4], proof)
		}
	}

	for _, test := range []struct {
		name   string
		mutate func(*LegacyRuntimeAdoption)
	}{
		{name: "instance", mutate: func(a *LegacyRuntimeAdoption) { a.Candidate.InstanceID = "other" }},
		{name: "generation unknown", mutate: func(a *LegacyRuntimeAdoption) { a.Candidate.GenerationKnown = false }},
		{name: "generation", mutate: func(a *LegacyRuntimeAdoption) { a.Candidate.Generation = 1 }},
		{name: "proof error", mutate: func(a *LegacyRuntimeAdoption) { a.Candidate.ProofError = "partial" }},
		{name: "state incomplete", mutate: func(a *LegacyRuntimeAdoption) { a.Candidate.StateKnown = false }},
		{name: "status revision", mutate: func(a *LegacyRuntimeAdoption) { a.Candidate.StatusRevision++ }},
		{name: "status", mutate: func(a *LegacyRuntimeAdoption) { a.Candidate.Status = "waiting" }},
		{name: "start", mutate: func(a *LegacyRuntimeAdoption) { a.Candidate.LastStartedUnixNano++ }},
		{name: "binding unknown", mutate: func(a *LegacyRuntimeAdoption) { a.Candidate.BindingKnown = false }},
		{name: "binding kind", mutate: func(a *LegacyRuntimeAdoption) { a.Candidate.BindingKind = "codex" }},
		{name: "binding value", mutate: func(a *LegacyRuntimeAdoption) { a.Candidate.BindingValue = "other" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			adoption := base
			test.mutate(&adoption)
			if err := ValidateLegacyRuntimeCandidateStamp(adoption); !errors.Is(err, ErrLegacyRuntimeCandidateChanged) {
				t.Fatalf("environment mismatch error = %v, want ErrLegacyRuntimeCandidateChanged", err)
			}
		})
	}
}

func TestRuntimeLifecycle_ValidateLegacyRuntimeCandidateStampRequiresEveryLocalOption(t *testing.T) {
	base := stampedLegacyRuntimeAdoptionForTest()
	stubLegacyRuntimeCandidateStampProbesForTest(t)
	stubLegacyRuntimeStampConditionalForTest(t)
	exact := runtimeCleanupLocalOptionsForTest(base.Candidate.InstanceID, 0, "CLAUDE_SESSION_ID", base.BindingValue)
	for _, option := range runtimeCleanupOptionNames {
		for _, test := range []struct {
			name   string
			mutate func(map[string]runtimeCleanupLocalOption)
		}{
			{name: "missing", mutate: func(options map[string]runtimeCleanupLocalOption) { delete(options, option) }},
			{name: "changed", mutate: func(options map[string]runtimeCleanupLocalOption) {
				value := options[option]
				value.value += "-changed"
				options[option] = value
			}},
		} {
			t.Run(option+"/"+test.name, func(t *testing.T) {
				options := make(map[string]runtimeCleanupLocalOption, len(exact))
				for name, value := range exact {
					options[name] = value
				}
				test.mutate(options)
				stubRuntimeCleanupLocalOptions(t, map[string]map[string]runtimeCleanupLocalOption{
					base.Candidate.SessionID: options,
				})
				if err := ValidateLegacyRuntimeCandidateStamp(base); !errors.Is(err, ErrLegacyRuntimeCandidateChanged) {
					t.Fatalf("local option mismatch error = %v, want ErrLegacyRuntimeCandidateChanged", err)
				}
			})
		}
	}
}

func TestRuntimeLifecycle_ValidateLegacyRuntimeCandidateStampRejectsRespawnAfterProbes(t *testing.T) {
	base := stampedLegacyRuntimeAdoptionForTest()
	stubLegacyRuntimeCandidateStampProbesForTest(t)
	stubRuntimeCleanupLocalOptions(t, map[string]map[string]runtimeCleanupLocalOption{
		base.Candidate.SessionID: runtimeCleanupLocalOptionsForTest(
			base.Candidate.InstanceID, 0, "CLAUDE_SESSION_ID", base.BindingValue),
	})
	oldMutation := runtimeBindingConditionalKillFn
	t.Cleanup(func() { runtimeBindingConditionalKillFn = oldMutation })
	runtimeBindingConditionalKillFn = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if len(args) != 7 || !strings.Contains(args[4], "#{==:#{pane_pid},#{l:4242}}") {
			t.Fatalf("final respawn guard = %#v", args)
		}
		// Simulate tmux observing a replacement pane process after the
		// userspace environment and local-option probes completed.
		return []byte("agent-deck-legacy-runtime-candidate-changed\n"), nil
	}

	if err := ValidateLegacyRuntimeCandidateStamp(base); !errors.Is(err, ErrLegacyRuntimeCandidateChanged) {
		t.Fatalf("respawned final candidate error = %v, want ErrLegacyRuntimeCandidateChanged", err)
	}
}

func TestRuntimeLifecycle_ValidateLegacyRuntimeCandidateStampRejectsSamePIDNewBirth(t *testing.T) {
	base := stampedLegacyRuntimeAdoptionForTest()
	stubRuntimeCleanupLocalOptions(t, map[string]map[string]runtimeCleanupLocalOption{
		base.Candidate.SessionID: runtimeCleanupLocalOptionsForTest(
			base.Candidate.InstanceID, 0, "CLAUDE_SESSION_ID", base.BindingValue),
	})
	oldRevalidate := legacyRuntimeCandidateRevalidateFn
	oldCapture := processCaptureIdentityFn
	oldAlive := processIdentityAliveFn
	oldMutation := runtimeBindingConditionalKillFn
	t.Cleanup(func() {
		legacyRuntimeCandidateRevalidateFn = oldRevalidate
		processCaptureIdentityFn = oldCapture
		processIdentityAliveFn = oldAlive
		runtimeBindingConditionalKillFn = oldMutation
	})
	legacyRuntimeCandidateRevalidateFn = func(candidate RuntimeCandidate) (RuntimeCandidate, error) {
		return candidate, nil
	}
	currentBirth := "birth-one"
	processCaptureIdentityFn = func(pid int) (ProcessIdentity, error) {
		return ProcessIdentity{PID: pid, StartToken: currentBirth}, nil
	}
	processIdentityAliveFn = func(identity ProcessIdentity) (bool, error) {
		return identity.PID == base.Candidate.PanePID && identity.StartToken == currentBirth, nil
	}
	runtimeBindingConditionalKillFn = func(context.Context, string, ...string) ([]byte, error) {
		// The pane still reports PID 4242, but that numeric PID now belongs to
		// another process lifetime.
		currentBirth = "birth-two"
		return nil, nil
	}

	if err := ValidateLegacyRuntimeCandidateStamp(base); !errors.Is(err, ErrLegacyRuntimeCandidateChanged) {
		t.Fatalf("same-PID replacement error = %v, want ErrLegacyRuntimeCandidateChanged", err)
	}
}

func TestRuntimeLifecycle_ValidateLegacyRuntimeCandidateStampDistinguishesEmptyFromUnset(t *testing.T) {
	base := stampedLegacyRuntimeAdoptionForTest()
	base.BindingKind, base.BindingValue = "", ""
	base.Candidate.BindingKind, base.Candidate.BindingValue = "", ""
	base.Candidate.BindingKnown = true

	t.Run("environment", func(t *testing.T) {
		stubLegacyRuntimeCandidateStampProbesForTest(t)
		stubLegacyRuntimeStampConditionalForTest(t)
		stubRuntimeCleanupLocalOptions(t, map[string]map[string]runtimeCleanupLocalOption{
			base.Candidate.SessionID: runtimeCleanupLocalOptionsForTest(base.Candidate.InstanceID, 0, "", ""),
		})
		probes := 0
		legacyRuntimeCandidateRevalidateFn = func(candidate RuntimeCandidate) (RuntimeCandidate, error) {
			probes++
			if probes == 2 {
				candidate.BindingKnown = false
			}
			return candidate, nil
		}
		if err := ValidateLegacyRuntimeCandidateStamp(base); !errors.Is(err, ErrLegacyRuntimeCandidateChanged) {
			t.Fatalf("empty-to-unset environment error = %v, want ErrLegacyRuntimeCandidateChanged", err)
		}
		if probes != 2 {
			t.Fatalf("presence-sensitive environment probes = %d, want 2", probes)
		}
	})

	t.Run("local option", func(t *testing.T) {
		stubLegacyRuntimeCandidateStampProbesForTest(t)
		stubLegacyRuntimeStampConditionalForTest(t)
		oldLocal := runtimeCleanupLocalOptionsFn
		t.Cleanup(func() { runtimeCleanupLocalOptionsFn = oldLocal })
		probes := 0
		runtimeCleanupLocalOptionsFn = func(_ string, candidates []RuntimeBindingCandidate) (map[string]map[string]runtimeCleanupLocalOption, error) {
			probes++
			options := runtimeCleanupLocalOptionsForTest(base.Candidate.InstanceID, 0, "", "")
			if probes == 2 {
				delete(options, runtimeCleanupBindingValOption)
			}
			return map[string]map[string]runtimeCleanupLocalOption{
				candidates[0].SessionID: options,
			}, nil
		}
		if err := ValidateLegacyRuntimeCandidateStamp(base); !errors.Is(err, ErrLegacyRuntimeCandidateChanged) {
			t.Fatalf("empty-to-unset local option error = %v, want ErrLegacyRuntimeCandidateChanged", err)
		}
		if probes != 2 {
			t.Fatalf("presence-sensitive local-option probes = %d, want 2", probes)
		}
	})
}

func stubProcessStartIdentityForTest(t *testing.T) {
	t.Helper()
	oldIdentity := processStartIdentityFn
	oldCapture := processCaptureIdentityFn
	oldAlive := processIdentityAliveFn
	t.Cleanup(func() {
		processStartIdentityFn = oldIdentity
		processCaptureIdentityFn = oldCapture
		processIdentityAliveFn = oldAlive
	})
	processStartIdentityFn = func(pid int) (string, error) {
		return fmt.Sprintf("start-%d", pid), nil
	}
	processCaptureIdentityFn = func(pid int) (ProcessIdentity, error) {
		return ProcessIdentity{PID: pid, StartToken: fmt.Sprintf("start-%d", pid)}, nil
	}
	processIdentityAliveFn = func(ProcessIdentity) (bool, error) { return true, nil }
}

func stubRetainedIdentityCaptureForTest(t *testing.T) map[int]int {
	t.Helper()
	oldCapture := processCaptureIdentityFn
	oldAlive := processIdentityAliveFn
	closed := make(map[int]int)
	processCaptureIdentityFn = func(pid int) (ProcessIdentity, error) {
		return ProcessIdentity{
			PID: pid, StartToken: fmt.Sprintf("start-%d", pid),
			handle: &processIdentityHandle{closeFn: func() error {
				closed[pid]++
				return nil
			}},
		}, nil
	}
	processIdentityAliveFn = func(ProcessIdentity) (bool, error) { return true, nil }
	t.Cleanup(func() {
		processCaptureIdentityFn = oldCapture
		processIdentityAliveFn = oldAlive
	})
	return closed
}

func stubRuntimeBindingCandidateProcessTreeForTest(t *testing.T, candidate RuntimeBindingCandidate) {
	t.Helper()
	stubProcessStartIdentityForTest(t)
	oldTree := runtimeGenerationProcessTreeFn
	oldEnsure := runtimeGenerationEnsurePIDsDeadFn
	runtimeGenerationProcessTreeFn = func(socketName, paneID string) ([]int, error) {
		if socketName != candidate.SocketName || paneID != candidate.PaneID {
			t.Fatalf("binding process tree target = %q/%q", socketName, paneID)
		}
		return []int{candidate.PanePID}, nil
	}
	runtimeGenerationEnsurePIDsDeadFn = func(identities []ProcessIdentity, _ time.Duration) {
		CloseProcessIdentities(identities)
	}
	t.Cleanup(func() {
		runtimeGenerationProcessTreeFn = oldTree
		runtimeGenerationEnsurePIDsDeadFn = oldEnsure
	})
}

func processIdentityPIDsForTest(identities []ProcessIdentity) []int {
	pids := make([]int, 0, len(identities))
	for _, identity := range identities {
		pids = append(pids, identity.PID)
	}
	return pids
}

func finalRuntimeCleanupCondition(t *testing.T, args []string, candidate RuntimeGenerationCandidate) string {
	t.Helper()
	wantPrefix := []string{"if-shell", "-F", "-t", candidate.PaneID}
	if len(args) != 7 || !reflect.DeepEqual(args[:4], wantPrefix) {
		t.Fatalf("conditional command = %#v, want prefix %#v and seven arguments", args, wantPrefix)
	}
	return args[4]
}

func runtimeCleanupLocalOptionsForTest(instanceID string, generation uint64, bindingKey, bindingValue string) map[string]runtimeCleanupLocalOption {
	return map[string]runtimeCleanupLocalOption{
		runtimeCleanupInstanceOption:   {value: instanceID, present: true},
		runtimeCleanupGenerationOption: {value: strconv.FormatUint(generation, 10), present: true},
		runtimeCleanupBindingKeyOption: {value: bindingKey, present: true},
		runtimeCleanupBindingValOption: {value: bindingValue, present: true},
	}
}

func runtimeCleanupLocalOutputForTest(candidates []RuntimeBindingCandidate, bySession map[string]map[string]runtimeCleanupLocalOption) []byte {
	var output strings.Builder
	for _, candidate := range candidates {
		for _, option := range runtimeCleanupOptionNames {
			fmt.Fprintln(&output, runtimeCleanupLocalOptionMarker(candidate.SessionID, option))
			if local := bySession[candidate.SessionID][option]; local.present {
				fmt.Fprintln(&output, local.value)
			}
		}
	}
	return []byte(output.String())
}

func stubRuntimeCleanupLocalOptions(t *testing.T, bySession map[string]map[string]runtimeCleanupLocalOption) {
	t.Helper()
	oldLocal := runtimeCleanupLocalOptionsFn
	t.Cleanup(func() { runtimeCleanupLocalOptionsFn = oldLocal })
	runtimeCleanupLocalOptionsFn = func(_ string, candidates []RuntimeBindingCandidate) (map[string]map[string]runtimeCleanupLocalOption, error) {
		result := make(map[string]map[string]runtimeCleanupLocalOption, len(candidates))
		for _, candidate := range candidates {
			result[candidate.SessionID] = bySession[candidate.SessionID]
		}
		return result, nil
	}
}

func stubCompleteRuntimeCleanupLocalOptions(t *testing.T, candidate RuntimeBindingCandidate) {
	t.Helper()
	stubRuntimeCleanupLocalOptions(t, map[string]map[string]runtimeCleanupLocalOption{
		candidate.SessionID: runtimeCleanupLocalOptionsForTest(
			candidate.InstanceID, candidate.Generation, candidate.BindingKey, candidate.BindingValue,
		),
	})
}

func TestRuntimeLifecycle_StampRuntimeCleanupIdentityInvalidatesGenerationUntilComplete(t *testing.T) {
	oldStamp := runtimeCleanupStampFn
	t.Cleanup(func() { runtimeCleanupStampFn = oldStamp })
	session := &Session{Name: "agentdeck_stamp", SocketName: "isolated"}
	var got []string
	runtimeCleanupStampFn = func(target *Session, args ...string) error {
		if target != session {
			t.Fatalf("stamp target = %#v, want %#v", target, session)
		}
		got = append([]string(nil), args...)
		return nil
	}

	if err := StampRuntimeCleanupIdentity(session, "one", 3, "CLAUDE_SESSION_ID", "conversation"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"set-option", "-u", "-t", "agentdeck_stamp", runtimeCleanupGenerationOption,
		";", "set-option", "-t", "agentdeck_stamp", runtimeCleanupInstanceOption, "one",
		";", "set-option", "-t", "agentdeck_stamp", runtimeCleanupBindingKeyOption, "CLAUDE_SESSION_ID",
		";", "set-option", "-t", "agentdeck_stamp", runtimeCleanupBindingValOption, "conversation",
		";", "set-option", "-t", "agentdeck_stamp", runtimeCleanupGenerationOption, "3",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stamp argv = %#v, want %#v", got, want)
	}
}

func TestRuntimeLifecycle_RuntimeCleanupIdentityRejectsReservedMarkerValues(t *testing.T) {
	oldStamp := runtimeCleanupStampFn
	t.Cleanup(func() { runtimeCleanupStampFn = oldStamp })
	stampCalls := 0
	runtimeCleanupStampFn = func(*Session, ...string) error {
		stampCalls++
		return nil
	}
	session := &Session{Name: "agentdeck_stamp", SocketName: "isolated"}
	reserved := runtimeCleanupLocalOptionMarkerPrefix + "user-value"
	for _, test := range []struct {
		name         string
		instanceID   string
		bindingValue string
	}{
		{name: "instance id", instanceID: reserved, bindingValue: "conversation"},
		{name: "binding value", instanceID: "one", bindingValue: reserved},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := StampRuntimeCleanupIdentity(session, test.instanceID, 3, "CLAUDE_SESSION_ID", test.bindingValue); err == nil {
				t.Fatalf("reserved marker value was accepted: instance=%q binding=%q", test.instanceID, test.bindingValue)
			}
		})
	}
	if stampCalls != 0 {
		t.Fatalf("invalid marker values reached tmux stamp %d times", stampCalls)
	}

	generation := runtimeGenerationCandidateFromBinding(runtimeBindingCandidateForTest())
	generation.InstanceID = reserved
	if _, err := runtimeGenerationCandidateCondition(generation); err == nil {
		t.Fatal("reserved marker instance id was accepted as cleanup authority")
	}
	binding := runtimeBindingCandidateForTest()
	binding.BindingValue = reserved
	if _, err := runtimeBindingCandidateCondition(binding); err == nil {
		t.Fatal("reserved marker binding value was accepted as cleanup authority")
	}
}

func TestRuntimeLifecycle_RuntimeCleanupLocalOptionParserRejectsReservedMarkerPrefixValue(t *testing.T) {
	oldOutput := runtimeBindingCandidateOutputFn
	t.Cleanup(func() { runtimeBindingCandidateOutputFn = oldOutput })
	candidate := RuntimeBindingCandidate{SessionID: "$7"}
	local := runtimeCleanupLocalOptionsForTest(
		runtimeCleanupLocalOptionMarkerPrefix+"legacy-value",
		3,
		"CLAUDE_SESSION_ID",
		"conversation",
	)
	runtimeBindingCandidateOutputFn = func(socketName string, args ...string) ([]byte, error) {
		if socketName != "isolated" || len(args) == 0 || args[0] != "display-message" {
			t.Fatalf("unexpected local-option read: socket=%q args=%q", socketName, args)
		}
		return runtimeCleanupLocalOutputForTest(
			[]RuntimeBindingCandidate{candidate},
			map[string]map[string]runtimeCleanupLocalOption{candidate.SessionID: local},
		), nil
	}

	if _, err := readRuntimeCleanupLocalOptions("isolated", []RuntimeBindingCandidate{candidate}); err == nil {
		t.Fatal("reserved marker-prefix value did not make legacy local-option inventory fail closed")
	}
}

func TestRuntimeLifecycle_RuntimeCleanupLocalOptionParserPreservesOpaqueWhitespace(t *testing.T) {
	oldOutput := runtimeBindingCandidateOutputFn
	t.Cleanup(func() { runtimeBindingCandidateOutputFn = oldOutput })
	candidate := RuntimeBindingCandidate{SessionID: "$7"}

	for _, instanceID := range []string{" peer-instance ", "   "} {
		t.Run(strconv.Quote(instanceID), func(t *testing.T) {
			local := runtimeCleanupLocalOptionsForTest(instanceID, 3, "", "")
			runtimeBindingCandidateOutputFn = func(socketName string, args ...string) ([]byte, error) {
				if socketName != "isolated" || len(args) == 0 || args[0] != "display-message" {
					t.Fatalf("unexpected local-option read: socket=%q args=%q", socketName, args)
				}
				return runtimeCleanupLocalOutputForTest(
					[]RuntimeBindingCandidate{candidate},
					map[string]map[string]runtimeCleanupLocalOption{candidate.SessionID: local},
				), nil
			}

			options, err := readRuntimeCleanupLocalOptions("isolated", []RuntimeBindingCandidate{candidate})
			if err != nil {
				t.Fatal(err)
			}
			got := options[candidate.SessionID][runtimeCleanupInstanceOption]
			if !got.present || got.value != instanceID {
				t.Fatalf("parsed instance option = %#v, want exact opaque value %q", got, instanceID)
			}
		})
	}
}

func TestRuntimeLifecycle_InvalidateRuntimeIdentityClearsBothGenerationMarkers(t *testing.T) {
	oldMutation := runtimeBindingConditionalKillFn
	t.Cleanup(func() { runtimeBindingConditionalKillFn = oldMutation })
	binding := runtimeBindingCandidateForTest()
	candidate := runtimeGenerationCandidateFromBinding(binding)
	stubCompleteRuntimeCleanupLocalOptions(t, binding)
	var got []string
	runtimeBindingConditionalKillFn = func(_ context.Context, socketName string, args ...string) ([]byte, error) {
		if socketName != candidate.SocketName {
			t.Fatalf("mutation socket = %q, want %q", socketName, candidate.SocketName)
		}
		got = append([]string(nil), args...)
		return nil, nil
	}

	if err := InvalidateRuntimeGenerationCandidate(candidate); err != nil {
		t.Fatal(err)
	}
	condition := finalRuntimeCleanupCondition(t, got, candidate)
	for _, check := range []string{
		"#{==:#{session_id},#{l:$7}}",
		"#{==:#{session_name},#{l:agentdeck_peer}}",
		"#{==:#{pane_id},#{l:%9}}",
		"#{==:#{pane_pid},#{l:4242}}",
		"#{==:#{@agentdeck_runtime_instance_id},#{l:peer-instance}}",
		"#{==:#{@agentdeck_runtime_generation},#{l:3}}",
		"#{==:#{AGENTDECK_RUNTIME_GENERATION},#{l:3}}",
	} {
		if !strings.Contains(condition, check) {
			t.Errorf("condition %q lacks %q", condition, check)
		}
	}
	wantMutation := runtimeGenerationMutationCommand(candidate)
	if got[5] != wantMutation {
		t.Fatalf("generation invalidation = %q, want %q", got[5], wantMutation)
	}
	for _, want := range []string{
		"'set-environment' '-u' '-t' '$7' 'AGENTDECK_RUNTIME_GENERATION'",
		"'set-option' '-u' '-t' '$7' '@agentdeck_runtime_generation'",
	} {
		if !strings.Contains(got[5], want) {
			t.Errorf("generation invalidation %q lacks %q", got[5], want)
		}
	}
}

func TestRuntimeLifecycle_RespawnSameNameReplacementIsUntouched(t *testing.T) {
	stubProcessStartIdentityForTest(t)
	oldMutation := runtimeBindingConditionalKillFn
	oldTree := runtimeGenerationProcessTreeFn
	t.Cleanup(func() {
		runtimeBindingConditionalKillFn = oldMutation
		runtimeGenerationProcessTreeFn = oldTree
	})
	binding := runtimeBindingCandidateForTest()
	candidate := runtimeGenerationCandidateFromBinding(binding)
	stubCompleteRuntimeCleanupLocalOptions(t, binding)
	treeCalls := 0
	runtimeGenerationProcessTreeFn = func(socketName, paneID string) ([]int, error) {
		treeCalls++
		if socketName != candidate.SocketName || paneID != candidate.PaneID {
			t.Fatalf("process tree target = %q/%q, want %q/%q", socketName, paneID, candidate.SocketName, candidate.PaneID)
		}
		return []int{candidate.PanePID}, nil
	}
	mutationCalls := 0
	runtimeBindingConditionalKillFn = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		mutationCalls++
		condition := finalRuntimeCleanupCondition(t, args, candidate)
		for _, check := range []string{
			"#{==:#{@agentdeck_runtime_instance_id},#{l:peer-instance}}",
			"#{==:#{@agentdeck_runtime_generation},#{l:3}}",
			"#{==:#{AGENTDECK_RUNTIME_GENERATION},#{l:3}}",
		} {
			if !strings.Contains(condition, check) {
				t.Errorf("condition %q lacks logical identity check %q", condition, check)
			}
		}
		environmentUnset := strings.Index(args[5], "'set-environment' '-u'")
		optionUnset := strings.Index(args[5], "'set-option' '-u'")
		clearHistory := strings.Index(args[5], "'clear-history' '-t' '%9'")
		respawn := strings.Index(args[5], "'respawn-pane' '-k' '-t' '%9'")
		if environmentUnset < 0 || optionUnset <= environmentUnset ||
			clearHistory <= optionUnset || respawn <= clearHistory {
			t.Fatalf("conditional generation invalidation, clear, and respawn order = %q", args[5])
		}
		if strings.Contains(args[5], "agentdeck_peer:") {
			t.Fatalf("conditional branch targets mutable name: %q", args[5])
		}
		// tmux reports the false branch when the name now identifies a
		// replacement with different stable IDs/PID. The true branch, including
		// both marker unsets and respawn, is therefore never executed.
		return []byte("agent-deck-runtime-generation-candidate-changed\n"), nil
	}
	session := &Session{
		Name: candidate.SessionName, SocketName: candidate.SocketName,
		InstanceID: candidate.InstanceID, clearOnRestart: true,
	}
	if err := RespawnRuntimeGenerationCandidate(session, candidate, ""); !errors.Is(err, ErrRuntimeGenerationCandidateChanged) {
		t.Fatalf("same-name replacement error = %v, want candidate changed", err)
	}
	if mutationCalls != 1 || treeCalls != 2 {
		t.Fatalf("conditional calls=%d tree calls=%d, want capture plus pre-mutation revalidation", mutationCalls, treeCalls)
	}
}

func TestRuntimeLifecycle_RuntimeBindingCandidatesCaptureStableSessionAndPaneIdentity(t *testing.T) {
	oldOutput := runtimeBindingCandidateOutputFn
	t.Cleanup(func() { runtimeBindingCandidateOutputFn = oldOutput })
	var calls [][]string
	runtimeBindingCandidateOutputFn = func(socketName string, args ...string) ([]byte, error) {
		if socketName != "isolated" {
			t.Errorf("socket = %q, want isolated", socketName)
		}
		calls = append(calls, append([]string(nil), args...))
		switch args[0] {
		case "list-sessions":
			if len(args) != 3 || args[1] != "-F" || args[2] != runtimeCleanupCandidateFormat() {
				t.Fatalf("unexpected tmux command: %q", args)
			}
			return []byte(tmuxFmt("$7", "agentdeck_peer", "%9", "4242") + "\n" +
				tmuxFmt("$8", "ordinary", "%10", "4343") + "\n"), nil
		case "display-message":
			candidate := runtimeBindingCandidateForTest()
			return runtimeCleanupLocalOutputForTest(
				[]RuntimeBindingCandidate{candidate},
				map[string]map[string]runtimeCleanupLocalOption{
					candidate.SessionID: runtimeCleanupLocalOptionsForTest(
						candidate.InstanceID, candidate.Generation, candidate.BindingKey, candidate.BindingValue,
					),
				},
			), nil
		default:
			t.Fatalf("unexpected tmux command: %q", args)
			return nil, nil
		}
	}

	candidates, err := ListRuntimeBindingCandidates("isolated", "CLAUDE_SESSION_ID", "conversation-id")
	if err != nil {
		t.Fatal(err)
	}
	want := []RuntimeBindingCandidate{runtimeBindingCandidateForTest()}
	if !reflect.DeepEqual(candidates, want) {
		t.Fatalf("candidates = %#v, want %#v", candidates, want)
	}
	if len(calls) != 2 || !reflect.DeepEqual(calls[0], []string{"list-sessions", "-F", runtimeCleanupCandidateFormat()}) {
		t.Fatalf("tmux calls = %#v, want stable inventory followed by one local-option batch", calls)
	}
	for _, forbidden := range []string{"-A", "-g"} {
		if slices.Contains(calls[1], forbidden) {
			t.Fatalf("local option batch %q contains inherited/global flag %q", calls[1], forbidden)
		}
	}
}

func TestRuntimeLifecycle_RuntimeCleanupInventoryPreservesWhitespaceInstanceID(t *testing.T) {
	oldOutput := runtimeBindingCandidateOutputFn
	t.Cleanup(func() { runtimeBindingCandidateOutputFn = oldOutput })
	rawInstanceID := " peer-instance "
	identity := RuntimeBindingCandidate{
		SessionName: "agentdeck_peer", SessionID: "$7", SocketName: "isolated",
		PaneID: "%9", PanePID: 4242,
	}
	stubRuntimeCleanupLocalOptions(t, map[string]map[string]runtimeCleanupLocalOption{
		identity.SessionID: runtimeCleanupLocalOptionsForTest(rawInstanceID, 3, "", ""),
	})
	runtimeBindingCandidateOutputFn = func(socketName string, args ...string) ([]byte, error) {
		if socketName != "isolated" || len(args) != 3 || args[0] != "list-sessions" ||
			args[1] != "-F" || args[2] != runtimeCleanupCandidateFormat() {
			t.Fatalf("unexpected inventory call: socket=%q args=%q", socketName, args)
		}
		return []byte(tmuxFmt(identity.SessionID, identity.SessionName, identity.PaneID, strconv.Itoa(identity.PanePID)) + "\n"), nil
	}

	candidates, err := ListRuntimeGenerationCandidates("isolated", rawInstanceID)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].InstanceID != rawInstanceID {
		t.Fatalf("whitespace-bearing instance inventory = %#v, want exact id %q", candidates, rawInstanceID)
	}
	trimmed, err := ListRuntimeGenerationCandidates("isolated", strings.TrimSpace(rawInstanceID))
	if err != nil {
		t.Fatal(err)
	}
	if len(trimmed) != 0 {
		t.Fatalf("trimmed identity matched raw cleanup authority: %#v", trimmed)
	}
}

func TestRuntimeLifecycle_RuntimeCleanupCandidateInventoryScalesTwoCallsForHundredSessions(t *testing.T) {
	oldOutput := runtimeBindingCandidateOutputFn
	t.Cleanup(func() { runtimeBindingCandidateOutputFn = oldOutput })

	calls := 0
	runtimeBindingCandidateOutputFn = func(socketName string, args ...string) ([]byte, error) {
		calls++
		if socketName != "isolated" {
			t.Fatalf("inventory socket = %q", socketName)
		}
		candidates := make([]RuntimeBindingCandidate, 0, 100)
		bySession := make(map[string]map[string]runtimeCleanupLocalOption, 100)
		for index := 0; index < 100; index++ {
			candidate := RuntimeBindingCandidate{SessionID: fmt.Sprintf("$%d", index+1)}
			candidates = append(candidates, candidate)
			bySession[candidate.SessionID] = runtimeCleanupLocalOptionsForTest(
				fmt.Sprintf("instance-%03d", index), uint64(index+1),
				"CLAUDE_SESSION_ID", fmt.Sprintf("conversation-%03d", index),
			)
		}
		if args[0] == "display-message" {
			return runtimeCleanupLocalOutputForTest(candidates, bySession), nil
		}
		if len(args) != 3 || args[0] != "list-sessions" || args[1] != "-F" || args[2] != runtimeCleanupCandidateFormat() {
			t.Fatalf("inventory call args = %q", args)
		}
		var output strings.Builder
		for index := range candidates {
			fmt.Fprintln(&output, tmuxFmt(
				candidates[index].SessionID,
				fmt.Sprintf("agentdeck_session_%03d", index),
				fmt.Sprintf("%%%d", index+1),
				strconv.Itoa(4000+index),
			))
		}
		return []byte(output.String()), nil
	}

	candidates, err := ListRuntimeCleanupCandidates("isolated", "CLAUDE_SESSION_ID")
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 100 || calls != 2 {
		t.Fatalf("inventory candidates=%d subprocesses=%d, want 100 and 2", len(candidates), calls)
	}
}

func TestRuntimeLifecycle_RuntimeCleanupInventoryRejectsInheritedGlobalsAndPartialLocalStamps(t *testing.T) {
	oldOutput := runtimeBindingCandidateOutputFn
	oldLocal := runtimeCleanupLocalOptionsFn
	t.Cleanup(func() {
		runtimeBindingCandidateOutputFn = oldOutput
		runtimeCleanupLocalOptionsFn = oldLocal
	})
	runtimeCleanupLocalOptionsFn = readRuntimeCleanupLocalOptions

	identity := RuntimeBindingCandidate{
		SessionName: "agentdeck_peer", SessionID: "$7", SocketName: "isolated",
		PaneID: "%9", PanePID: 4242,
	}
	for _, test := range []struct {
		name  string
		local map[string]runtimeCleanupLocalOption
	}{
		{
			name: "matching global values are not local authority",
			// All four effective values may exist globally; show-options without
			// -A emits none of them for this session.
			local: map[string]runtimeCleanupLocalOption{},
		},
		{
			name: "partial local stamp cannot fall back to matching global value",
			local: map[string]runtimeCleanupLocalOption{
				runtimeCleanupInstanceOption:   {value: "peer-instance", present: true},
				runtimeCleanupGenerationOption: {value: "3", present: true},
				runtimeCleanupBindingKeyOption: {value: "CLAUDE_SESSION_ID", present: true},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			runtimeBindingCandidateOutputFn = func(socketName string, args ...string) ([]byte, error) {
				calls++
				if socketName != "isolated" {
					t.Fatalf("socket = %q", socketName)
				}
				if args[0] == "list-sessions" {
					if strings.Contains(args[2], runtimeCleanupInstanceOption) {
						t.Fatalf("stable inventory reads effective user options: %q", args[2])
					}
					return []byte(tmuxFmt("$7", "agentdeck_peer", "%9", "4242") + "\n"), nil
				}
				if slices.Contains(args, "-A") || slices.Contains(args, "-g") {
					t.Fatalf("local-only query uses inherited/global flags: %q", args)
				}
				return runtimeCleanupLocalOutputForTest(
					[]RuntimeBindingCandidate{identity},
					map[string]map[string]runtimeCleanupLocalOption{identity.SessionID: test.local},
				), nil
			}

			candidates, err := ListRuntimeCleanupCandidates("isolated", "CLAUDE_SESSION_ID")
			if err != nil {
				t.Fatal(err)
			}
			if len(candidates) != 0 || calls != 2 {
				t.Fatalf("candidates=%#v subprocesses=%d, want no authority and two bounded calls", candidates, calls)
			}
		})
	}
}

func TestRuntimeLifecycle_RuntimeGenerationCandidatePreservesNativeDefaultSocketThroughKill(t *testing.T) {
	stubProcessStartIdentityForTest(t)
	oldDefault := DefaultSocketName()
	oldOutput := runtimeBindingCandidateOutputFn
	oldTree := runtimeGenerationProcessTreeFn
	oldKill := runtimeBindingConditionalKillFn
	oldEnsure := runtimeGenerationEnsurePIDsDeadFn
	t.Cleanup(func() {
		SetDefaultSocketName(oldDefault)
		runtimeBindingCandidateOutputFn = oldOutput
		runtimeGenerationProcessTreeFn = oldTree
		runtimeBindingConditionalKillFn = oldKill
		runtimeGenerationEnsurePIDsDeadFn = oldEnsure
	})
	SetDefaultSocketName("configured-socket")
	native := RuntimeBindingCandidate{
		SessionName: "agentdeck_native", SessionID: "$7", SocketName: "",
		PaneID: "%9", PanePID: 4242, InstanceID: "native-instance", InstanceKnown: true,
		Generation: 2, GenerationKnown: true,
	}
	stubCompleteRuntimeCleanupLocalOptions(t, native)

	runtimeBindingCandidateOutputFn = func(socketName string, args ...string) ([]byte, error) {
		if socketName != "" {
			t.Fatalf("inventory socket = %q, want native default empty", socketName)
		}
		if len(args) != 3 || args[2] != runtimeCleanupCandidateFormat() {
			t.Fatalf("inventory args = %q", args)
		}
		return []byte(tmuxFmt("$7", "agentdeck_native", "%9", "4242") + "\n"), nil
	}
	candidates, err := ListRuntimeGenerationCandidates("", "native-instance")
	if err != nil || len(candidates) != 1 {
		t.Fatalf("native candidates=%#v err=%v", candidates, err)
	}
	candidate := candidates[0]
	if candidate.SocketName != "" {
		t.Fatalf("candidate socket = %q, want native default empty", candidate.SocketName)
	}

	runtimeGenerationProcessTreeFn = func(socketName, paneID string) ([]int, error) {
		if socketName != "" || paneID != candidate.PaneID {
			t.Fatalf("process-tree target = (%q, %q), want native default and %q", socketName, paneID, candidate.PaneID)
		}
		return []int{candidate.PanePID, 5252}, nil
	}
	runtimeBindingConditionalKillFn = func(_ context.Context, socketName string, _ ...string) ([]byte, error) {
		if socketName != "" {
			t.Fatalf("conditional-kill socket = %q, want native default empty", socketName)
		}
		return nil, nil
	}
	reaped := false
	runtimeGenerationEnsurePIDsDeadFn = func(identities []ProcessIdentity, _ time.Duration) {
		reaped = reflect.DeepEqual(processIdentityPIDsForTest(identities), []int{candidate.PanePID, 5252})
	}
	if err := KillRuntimeGenerationCandidate(candidate, true); err != nil {
		t.Fatal(err)
	}
	if !reaped {
		t.Fatal("native-default process tree was not reaped after conditional kill")
	}
}

func TestRuntimeLifecycle_KillRuntimeBindingCandidateRevalidatesLocalStampAndKillsStableID(t *testing.T) {
	oldKill := runtimeBindingConditionalKillFn
	t.Cleanup(func() { runtimeBindingConditionalKillFn = oldKill })
	candidate := runtimeBindingCandidateForTest()
	stubRuntimeBindingCandidateProcessTreeForTest(t, candidate)
	stubCompleteRuntimeCleanupLocalOptions(t, candidate)
	called := false
	runtimeBindingConditionalKillFn = func(ctx context.Context, socketName string, args ...string) ([]byte, error) {
		called = true
		if socketName != candidate.SocketName {
			t.Errorf("socket = %q, want %q", socketName, candidate.SocketName)
		}
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("conditional kill has no deadline")
		}
		remaining := time.Until(deadline)
		if remaining <= 0 || remaining > runtimeBindingCandidateKillTimeout {
			t.Fatalf("conditional kill deadline remaining = %s", remaining)
		}
		condition := finalRuntimeCleanupCondition(t, args, runtimeGenerationCandidateFromBinding(candidate))
		for _, check := range []string{
			"#{==:#{session_id},#{l:$7}}",
			"#{==:#{session_name},#{l:agentdeck_peer}}",
			"#{==:#{pane_id},#{l:%9}}",
			"#{==:#{pane_pid},#{l:4242}}",
			"#{==:#{@agentdeck_runtime_instance_id},#{l:peer-instance}}",
			"#{==:#{@agentdeck_runtime_generation},#{l:3}}",
			"#{==:#{AGENTDECK_RUNTIME_GENERATION},#{l:3}}",
			"#{==:#{@agentdeck_runtime_binding_key},#{l:CLAUDE_SESSION_ID}}",
			"#{==:#{@agentdeck_runtime_binding_value},#{l:conversation-id}}",
		} {
			if !strings.Contains(condition, check) {
				t.Errorf("condition %q lacks %q", condition, check)
			}
		}
		if strings.Count(condition, "#{&&:") != 8 {
			t.Errorf("condition is not a nine-check binary conjunction: %q", condition)
		}
		if args[5] != "kill-session -t '$7'" || strings.Contains(args[5], candidate.SessionName) {
			t.Errorf("kill branch = %q, want stable session id only", args[5])
		}
		if args[6] != "display-message -p agent-deck-runtime-binding-candidate-changed" {
			t.Errorf("mismatch branch = %q", args[6])
		}
		return nil, nil
	}

	if err := KillRuntimeBindingCandidate(candidate); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("conditional kill was not invoked")
	}
}

func TestRuntimeLifecycle_KillRuntimeBindingCandidateMismatchAndUnsafeIdentityFailClosed(t *testing.T) {
	oldKill := runtimeBindingConditionalKillFn
	t.Cleanup(func() { runtimeBindingConditionalKillFn = oldKill })
	candidate := runtimeBindingCandidateForTest()
	stubRuntimeBindingCandidateProcessTreeForTest(t, candidate)
	stubCompleteRuntimeCleanupLocalOptions(t, candidate)
	calls := 0
	runtimeBindingConditionalKillFn = func(context.Context, string, ...string) ([]byte, error) {
		calls++
		return []byte("agent-deck-runtime-binding-candidate-changed\n"), nil
	}
	if err := KillRuntimeBindingCandidate(candidate); !errors.Is(err, ErrRuntimeBindingCandidateChanged) {
		t.Fatalf("mismatch error = %v, want ErrRuntimeBindingCandidateChanged", err)
	}

	candidate.BindingValue = "unsafe#format"
	if err := KillRuntimeBindingCandidate(candidate); err == nil {
		t.Fatal("unsafe identity was accepted")
	}
	if calls != 1 {
		t.Fatalf("conditional kill calls = %d, want only the proved mismatch call", calls)
	}
}

func TestRuntimeLifecycle_FinalConditionalKillBindingValueMayEqualMismatchMarker(t *testing.T) {
	oldKill := runtimeBindingConditionalKillFn
	t.Cleanup(func() { runtimeBindingConditionalKillFn = oldKill })
	candidate := runtimeBindingCandidateForTest()
	stubRuntimeBindingCandidateProcessTreeForTest(t, candidate)
	candidate.BindingValue = "agent-deck-runtime-binding-candidate-changed"
	stubCompleteRuntimeCleanupLocalOptions(t, candidate)
	runtimeBindingConditionalKillFn = func(context.Context, string, ...string) ([]byte, error) {
		return nil, nil
	}

	if err := KillRuntimeBindingCandidate(candidate); err != nil {
		t.Fatalf("binding value equal to mismatch marker was rejected: %v", err)
	}
}

func TestRuntimeLifecycle_FinalConditionalKillRejectsInheritedGlobalsAndPartialLocalStamp(t *testing.T) {
	candidate := runtimeBindingCandidateForTest()
	complete := runtimeCleanupLocalOptionsForTest(
		candidate.InstanceID, candidate.Generation, candidate.BindingKey, candidate.BindingValue,
	)
	partial := runtimeCleanupLocalOptionsForTest(
		candidate.InstanceID, candidate.Generation, candidate.BindingKey, candidate.BindingValue,
	)
	delete(partial, runtimeCleanupBindingValOption)
	changed := runtimeCleanupLocalOptionsForTest(
		candidate.InstanceID, candidate.Generation, candidate.BindingKey, "replacement-conversation",
	)
	for _, test := range []struct {
		name  string
		local map[string]runtimeCleanupLocalOption
	}{
		{name: "matching globals only", local: map[string]runtimeCleanupLocalOption{}},
		{name: "partial local stamp", local: partial},
		{name: "changed local stamp", local: changed},
		{name: "complete local stamp", local: complete},
	} {
		t.Run(test.name, func(t *testing.T) {
			stubRuntimeBindingCandidateProcessTreeForTest(t, candidate)
			oldKill := runtimeBindingConditionalKillFn
			t.Cleanup(func() { runtimeBindingConditionalKillFn = oldKill })
			stubRuntimeCleanupLocalOptions(t, map[string]map[string]runtimeCleanupLocalOption{
				candidate.SessionID: test.local,
			})
			conditionalReached := false
			runtimeBindingConditionalKillFn = func(_ context.Context, _ string, args ...string) ([]byte, error) {
				conditionalReached = true
				condition := finalRuntimeCleanupCondition(t, args, runtimeGenerationCandidateFromBinding(candidate))
				for _, check := range []string{
					"#{==:#{@agentdeck_runtime_instance_id},#{l:peer-instance}}",
					"#{==:#{@agentdeck_runtime_generation},#{l:3}}",
					"#{==:#{AGENTDECK_RUNTIME_GENERATION},#{l:3}}",
					"#{==:#{@agentdeck_runtime_binding_key},#{l:CLAUDE_SESSION_ID}}",
					"#{==:#{@agentdeck_runtime_binding_value},#{l:conversation-id}}",
				} {
					if !strings.Contains(condition, check) {
						t.Fatalf("final condition %q lacks %q", condition, check)
					}
				}
				return nil, nil
			}

			err := KillRuntimeBindingCandidate(candidate)
			wantErr := test.name != "complete local stamp"
			if wantErr != errors.Is(err, ErrRuntimeBindingCandidateChanged) {
				t.Fatalf("final local gate error=%v, want changed=%v", err, wantErr)
			}
			if conditionalReached == wantErr {
				t.Fatalf("final conditional reached=%v, want %v", conditionalReached, !wantErr)
			}
		})
	}
}

func TestRuntimeLifecycle_KillLowerGenerationSessionsReplacementOrNameReuseFailsClosed(t *testing.T) {
	oldOutput := runtimeBindingCandidateOutputFn
	oldTree := runtimeGenerationProcessTreeFn
	oldKill := runtimeBindingConditionalKillFn
	oldEnsure := runtimeGenerationEnsurePIDsDeadFn
	t.Cleanup(func() {
		runtimeBindingCandidateOutputFn = oldOutput
		runtimeGenerationProcessTreeFn = oldTree
		runtimeBindingConditionalKillFn = oldKill
		runtimeGenerationEnsurePIDsDeadFn = oldEnsure
	})
	stubProcessStartIdentityForTest(t)
	stubRuntimeCleanupLocalOptions(t, map[string]map[string]runtimeCleanupLocalOption{
		"$7": runtimeCleanupLocalOptionsForTest("peer-instance", 2, "", ""),
		"$8": runtimeCleanupLocalOptionsForTest("peer-instance", 3, "", ""),
	})

	inventoryCalls := 0
	runtimeBindingCandidateOutputFn = func(socketName string, args ...string) ([]byte, error) {
		inventoryCalls++
		if socketName != "isolated" {
			t.Errorf("socket = %q, want isolated", socketName)
		}
		if len(args) != 3 || args[0] != "list-sessions" || args[1] != "-F" || args[2] != runtimeCleanupCandidateFormat() {
			t.Fatalf("unexpected tmux inventory command: %q", args)
		}
		return []byte(tmuxFmt("$7", "agentdeck_reused", "%9", "4242") + "\n" +
			tmuxFmt("$8", "agentdeck_current", "%10", "4343") + "\n"), nil
	}
	runtimeGenerationProcessTreeFn = func(socketName, paneID string) ([]int, error) {
		if socketName != "isolated" || paneID != "%9" {
			t.Fatalf("process tree target = %q/%q", socketName, paneID)
		}
		return []int{4242, 5252}, nil
	}
	reapCalls := 0
	runtimeGenerationEnsurePIDsDeadFn = func([]ProcessIdentity, time.Duration) { reapCalls++ }

	conditionalCalls := 0
	runtimeBindingConditionalKillFn = func(ctx context.Context, socketName string, args ...string) ([]byte, error) {
		conditionalCalls++
		if socketName != "isolated" {
			t.Errorf("conditional socket = %q, want isolated", socketName)
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > time.Second {
			t.Fatalf("conditional kill deadline = %v, ok=%v", deadline, ok)
		}
		generation := RuntimeGenerationCandidate{
			SessionName: "agentdeck_reused", SessionID: "$7", SocketName: "isolated",
			PaneID: "%9", PanePID: 4242, InstanceID: "peer-instance",
			Generation: 2, GenerationKnown: true,
		}
		condition := finalRuntimeCleanupCondition(t, args, generation)
		for _, check := range []string{
			"#{==:#{session_id},#{l:$7}}",
			"#{==:#{session_name},#{l:agentdeck_reused}}",
			"#{==:#{pane_id},#{l:%9}}",
			"#{==:#{pane_pid},#{l:4242}}",
			"#{==:#{@agentdeck_runtime_instance_id},#{l:peer-instance}}",
			"#{==:#{@agentdeck_runtime_generation},#{l:2}}",
			"#{==:#{AGENTDECK_RUNTIME_GENERATION},#{l:2}}",
		} {
			if !strings.Contains(condition, check) {
				t.Errorf("condition %q lacks %q", condition, check)
			}
		}
		if args[5] != "kill-session -t '$7'" || strings.Contains(args[5], "agentdeck_reused") {
			t.Errorf("kill branch = %q, want stable session id only", args[5])
		}
		// Model a replacement that reused the mutable name after inventory.
		// The tmux-server conditional observes the stable-ID mismatch and takes
		// the preservation branch instead of killing the replacement.
		return []byte("agent-deck-runtime-generation-candidate-changed\n"), nil
	}

	err := KillLowerGenerationSessions("isolated", "peer-instance", "agentdeck_current", 3)
	if err == nil {
		t.Fatal("replacement/name reuse mismatch was reported as a successful kill")
	}
	if inventoryCalls != 1 {
		t.Fatalf("lower-generation cleanup inventory calls = %d, want 1", inventoryCalls)
	}
	if conditionalCalls != 1 {
		t.Fatalf("conditional kill calls = %d, want 1", conditionalCalls)
	}
	if reapCalls != 0 {
		t.Fatalf("rejected conditional reaped %d process trees, want 0", reapCalls)
	}
}

func TestRuntimeLifecycle_KillLowerGenerationSessionsIncompleteIdentityPreservesCandidate(t *testing.T) {
	oldOutput := runtimeBindingCandidateOutputFn
	oldKill := runtimeBindingConditionalKillFn
	t.Cleanup(func() {
		runtimeBindingCandidateOutputFn = oldOutput
		runtimeBindingConditionalKillFn = oldKill
	})
	stubRuntimeCleanupLocalOptions(t, map[string]map[string]runtimeCleanupLocalOption{
		"$7": {
			runtimeCleanupInstanceOption:   {value: "peer-instance", present: true},
			runtimeCleanupGenerationOption: {value: "", present: true},
			runtimeCleanupBindingKeyOption: {value: "", present: true},
			runtimeCleanupBindingValOption: {value: "", present: true},
		},
	})

	inventoryCalls := 0
	runtimeBindingCandidateOutputFn = func(socketName string, args ...string) ([]byte, error) {
		inventoryCalls++
		if socketName != "isolated" {
			t.Errorf("socket = %q, want isolated", socketName)
		}
		if len(args) != 3 || args[0] != "list-sessions" || args[1] != "-F" || args[2] != runtimeCleanupCandidateFormat() {
			t.Fatalf("unexpected tmux inventory command: %q", args)
		}
		// Exact instance, but no generation: this is not kill authority.
		return []byte(tmuxFmt("$7", "agentdeck_incomplete", "%9", "4242") + "\n"), nil
	}

	conditionalCalls := 0
	runtimeBindingConditionalKillFn = func(context.Context, string, ...string) ([]byte, error) {
		conditionalCalls++
		return nil, nil
	}

	if err := KillLowerGenerationSessions("isolated", "peer-instance", "agentdeck_current", 3); err != nil {
		t.Fatalf("incomplete candidate cleanup = %v, want preservation without a kill", err)
	}
	if inventoryCalls != 1 {
		t.Fatalf("lower-generation cleanup inventory calls = %d, want 1", inventoryCalls)
	}
	if conditionalCalls != 0 {
		t.Fatalf("incomplete candidate conditional kill calls = %d, want 0", conditionalCalls)
	}
}

func TestRuntimeLifecycle_KillRuntimeGenerationCandidateWaitRejectsChangedPaneProcess(t *testing.T) {
	oldTree := runtimeGenerationProcessTreeFn
	oldKill := runtimeBindingConditionalKillFn
	oldEnsure := runtimeGenerationEnsurePIDsDeadFn
	t.Cleanup(func() {
		runtimeGenerationProcessTreeFn = oldTree
		runtimeBindingConditionalKillFn = oldKill
		runtimeGenerationEnsurePIDsDeadFn = oldEnsure
	})

	binding := runtimeBindingCandidateForTest()
	stubCompleteRuntimeCleanupLocalOptions(t, binding)
	candidate := runtimeGenerationCandidateFromBinding(binding)
	runtimeGenerationProcessTreeFn = func(socketName, paneID string) ([]int, error) {
		if socketName != candidate.SocketName || paneID != candidate.PaneID {
			t.Fatalf("process-tree target = (%q, %q), want (%q, %q)", socketName, paneID, candidate.SocketName, candidate.PaneID)
		}
		return []int{candidate.PanePID + 1, 5252}, nil
	}
	conditionalCalls := 0
	runtimeBindingConditionalKillFn = func(context.Context, string, ...string) ([]byte, error) {
		conditionalCalls++
		return nil, nil
	}
	reapCalls := 0
	runtimeGenerationEnsurePIDsDeadFn = func([]ProcessIdentity, time.Duration) { reapCalls++ }

	err := KillRuntimeGenerationCandidate(candidate, true)
	if !errors.Is(err, ErrRuntimeGenerationCandidateChanged) {
		t.Fatalf("changed pane process error = %v, want fail-closed mismatch", err)
	}
	if conditionalCalls != 0 || reapCalls != 0 {
		t.Fatalf("changed pane process invoked conditional=%d reap=%d, want neither", conditionalCalls, reapCalls)
	}
}

func TestRuntimeLifecycle_KillRuntimeGenerationCandidateWaitCapturesKillsThenReaps(t *testing.T) {
	stubProcessStartIdentityForTest(t)
	oldTree := runtimeGenerationProcessTreeFn
	oldKill := runtimeBindingConditionalKillFn
	oldEnsure := runtimeGenerationEnsurePIDsDeadFn
	t.Cleanup(func() {
		runtimeGenerationProcessTreeFn = oldTree
		runtimeBindingConditionalKillFn = oldKill
		runtimeGenerationEnsurePIDsDeadFn = oldEnsure
	})

	binding := runtimeBindingCandidateForTest()
	stubCompleteRuntimeCleanupLocalOptions(t, binding)
	candidate := runtimeGenerationCandidateFromBinding(binding)
	events := []string{}
	runtimeGenerationProcessTreeFn = func(socketName, paneID string) ([]int, error) {
		events = append(events, "capture:"+socketName+":"+paneID)
		return []int{candidate.PanePID, 5252}, nil
	}
	runtimeBindingConditionalKillFn = func(ctx context.Context, socketName string, args ...string) ([]byte, error) {
		events = append(events, "conditional")
		if socketName != candidate.SocketName || len(args) != 7 || args[3] != candidate.PaneID ||
			args[5] != "kill-session -t '$7'" ||
			args[6] != "display-message -p agent-deck-runtime-generation-candidate-changed" {
			t.Fatalf("conditional = socket %q args %#v", socketName, args)
		}
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) <= 0 || time.Until(deadline) > time.Second {
			t.Fatalf("conditional deadline = %v, ok=%v", deadline, ok)
		}
		return nil, nil
	}
	runtimeGenerationEnsurePIDsDeadFn = func(identities []ProcessIdentity, timeout time.Duration) {
		events = append(events, "reap")
		pids := processIdentityPIDsForTest(identities)
		if !reflect.DeepEqual(pids, []int{candidate.PanePID, 5252}) || timeout != 3*time.Second {
			t.Fatalf("reap = pids %v timeout %s", pids, timeout)
		}
	}

	if err := KillRuntimeGenerationCandidate(candidate, true); err != nil {
		t.Fatal(err)
	}
	want := []string{"capture:isolated:%9", "capture:isolated:%9", "conditional", "reap"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestRuntimeLifecycle_KillRuntimeGenerationCandidateWaitMismatchNeverReaps(t *testing.T) {
	closed := stubRetainedIdentityCaptureForTest(t)
	oldTree := runtimeGenerationProcessTreeFn
	oldKill := runtimeBindingConditionalKillFn
	oldEnsure := runtimeGenerationEnsurePIDsDeadFn
	t.Cleanup(func() {
		runtimeGenerationProcessTreeFn = oldTree
		runtimeBindingConditionalKillFn = oldKill
		runtimeGenerationEnsurePIDsDeadFn = oldEnsure
	})

	binding := runtimeBindingCandidateForTest()
	stubCompleteRuntimeCleanupLocalOptions(t, binding)
	candidate := runtimeGenerationCandidateFromBinding(binding)
	runtimeGenerationProcessTreeFn = func(string, string) ([]int, error) {
		return []int{candidate.PanePID, 5252}, nil
	}
	runtimeBindingConditionalKillFn = func(context.Context, string, ...string) ([]byte, error) {
		return []byte("agent-deck-runtime-generation-candidate-changed\n"), nil
	}
	reapCalls := 0
	runtimeGenerationEnsurePIDsDeadFn = func([]ProcessIdentity, time.Duration) { reapCalls++ }

	err := KillRuntimeGenerationCandidate(candidate, true)
	if !errors.Is(err, ErrRuntimeGenerationCandidateChanged) {
		t.Fatalf("mismatch error = %v, want ErrRuntimeGenerationCandidateChanged", err)
	}
	if reapCalls != 0 {
		t.Fatalf("mismatch reaped %d process trees, want 0", reapCalls)
	}
	if !reflect.DeepEqual(closed, map[int]int{candidate.PanePID: 1, 5252: 1}) {
		t.Fatalf("failed conditional closed handles = %v, want each retained handle once", closed)
	}
}

func TestRuntimeLifecycle_KillRuntimeGenerationCandidateRejectsTreeSubstitutionAndClosesHandles(t *testing.T) {
	closed := stubRetainedIdentityCaptureForTest(t)
	oldTree := runtimeGenerationProcessTreeFn
	oldKill := runtimeBindingConditionalKillFn
	t.Cleanup(func() {
		runtimeGenerationProcessTreeFn = oldTree
		runtimeBindingConditionalKillFn = oldKill
	})

	binding := runtimeBindingCandidateForTest()
	stubCompleteRuntimeCleanupLocalOptions(t, binding)
	candidate := runtimeGenerationCandidateFromBinding(binding)
	probes := 0
	runtimeGenerationProcessTreeFn = func(string, string) ([]int, error) {
		probes++
		if probes == 1 {
			return []int{candidate.PanePID, 5252}, nil
		}
		return []int{candidate.PanePID, 6262}, nil
	}
	conditionalCalls := 0
	runtimeBindingConditionalKillFn = func(context.Context, string, ...string) ([]byte, error) {
		conditionalCalls++
		return nil, nil
	}

	err := KillRuntimeGenerationCandidate(candidate, true)
	if !errors.Is(err, ErrRuntimeGenerationCandidateChanged) {
		t.Fatalf("tree substitution error = %v, want candidate changed", err)
	}
	if conditionalCalls != 0 {
		t.Fatalf("tree substitution reached tmux mutation %d times", conditionalCalls)
	}
	if !reflect.DeepEqual(closed, map[int]int{candidate.PanePID: 1, 5252: 1}) {
		t.Fatalf("tree substitution closed handles = %v, want each captured handle once", closed)
	}
}

func TestRuntimeLifecycle_KillRuntimeGenerationCandidateAllowsProvedLegacyGenerationZero(t *testing.T) {
	stubProcessStartIdentityForTest(t)
	oldKill := runtimeBindingConditionalKillFn
	oldTree := runtimeGenerationProcessTreeFn
	oldEnsure := runtimeGenerationEnsurePIDsDeadFn
	t.Cleanup(func() {
		runtimeBindingConditionalKillFn = oldKill
		runtimeGenerationProcessTreeFn = oldTree
		runtimeGenerationEnsurePIDsDeadFn = oldEnsure
	})
	binding := runtimeBindingCandidateForTest()
	binding.Generation = 0
	stubCompleteRuntimeCleanupLocalOptions(t, binding)
	candidate := runtimeGenerationCandidateFromBinding(binding)
	events := make(chan string, 4)
	runtimeGenerationProcessTreeFn = func(socketName, paneID string) ([]int, error) {
		if socketName != candidate.SocketName || paneID != candidate.PaneID {
			t.Fatalf("process tree target = %q/%q", socketName, paneID)
		}
		events <- "capture"
		return []int{candidate.PanePID, 5252}, nil
	}
	runtimeBindingConditionalKillFn = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		events <- "conditional"
		condition := finalRuntimeCleanupCondition(t, args, candidate)
		for _, check := range []string{
			"#{==:#{@agentdeck_runtime_instance_id},#{l:peer-instance}}",
			"#{==:#{@agentdeck_runtime_generation},#{l:0}}",
			"#{==:#{AGENTDECK_RUNTIME_GENERATION},#{l:0}}",
		} {
			if !strings.Contains(condition, check) {
				t.Fatalf("generation-zero condition %q lacks %q", condition, check)
			}
		}
		return nil, nil
	}
	runtimeGenerationEnsurePIDsDeadFn = func(identities []ProcessIdentity, timeout time.Duration) {
		if got := processIdentityPIDsForTest(identities); !reflect.DeepEqual(got, []int{candidate.PanePID, 5252}) {
			t.Errorf("async reap identities = %v", got)
		}
		if timeout != 3*time.Second {
			t.Errorf("async reap timeout = %s", timeout)
		}
		events <- "reap"
	}

	if err := KillRuntimeGenerationCandidate(candidate, false); err != nil {
		t.Fatalf("proved generation zero was rejected: %v", err)
	}
	for index, want := range []string{"capture", "capture", "conditional", "reap"} {
		select {
		case got := <-events:
			if got != want {
				t.Fatalf("event %d = %q, want %q", index, got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for %q", want)
		}
	}
}
