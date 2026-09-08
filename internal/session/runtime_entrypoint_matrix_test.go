package session

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

func TestRuntimeLifecycle_PublicPhysicalEntrypointsAdoptOneFakeSpawn(t *testing.T) {
	entrypoints := []struct {
		name string
		call func(*Instance) (statedb.RuntimeState, error)
	}{
		{"Start", (*Instance).StartRuntime},
		{"StartWithMessage", func(inst *Instance) (statedb.RuntimeState, error) {
			return inst.StartWithMessageRuntime("hello")
		}},
		{"restart/fallback", (*Instance).RestartRuntime},
		{"restart with environment", func(inst *Instance) (statedb.RuntimeState, error) {
			return inst.RestartWithEnvRuntime(map[string]string{"MATRIX": "1"})
		}},
		{"fresh recovery", (*Instance).RestartFreshRuntime},
	}

	for _, entrypoint := range entrypoints {
		t.Run(entrypoint.name, func(t *testing.T) {
			installRuntimeLifecycleTestSeams(t)
			t.Setenv("HOME", t.TempDir())
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			db, inst := runtimeLifecycleTestDB(t, "pi", nil)
			initial, found, err := db.ReadRuntimeState(inst.ID)
			if err != nil || !found {
				t.Fatalf("initial runtime: found=%v err=%v", found, err)
			}

			want := initial
			want.Generation++
			want.StatusRevision = 0
			want.TmuxSession = "runtime-g1"
			want.Status = string(StatusStarting)
			want.LastStartedAt = time.Unix(200, 123).UTC()
			fakeSpawns := 0
			var fakeSpawnErr error
			runtimeTransitionObservedFn = func() {
				fakeSpawns++
				fakeSpawnErr = db.CommitRuntimeTransition(initial.Generation, inst.PersistenceIncarnation(), want)
			}

			got, err := entrypoint.call(inst)
			if fakeSpawnErr != nil {
				t.Fatalf("fake backend spawn: %v", fakeSpawnErr)
			}
			if err != nil {
				t.Fatal(err)
			}
			if fakeSpawns != 1 {
				t.Fatalf("fake backend spawns = %d, want 1", fakeSpawns)
			}
			if got != want {
				t.Fatalf("returned runtime = %+v, want %+v", got, want)
			}
			durable, found, err := db.ReadRuntimeState(inst.ID)
			if err != nil || !found || durable != want {
				t.Fatalf("durable runtime = %+v, found=%v err=%v; want %+v", durable, found, err, want)
			}
		})
	}
}

func TestRuntimeLifecycle_StaleDestructiveAndTerminalResultsCannotTouchNextGeneration(t *testing.T) {
	destructive := []struct {
		name string
		call func(*Instance, RuntimeSelection) error
	}{
		{"stop", (*Instance).KillCaptured},
		{"close", (*Instance).KillAndWaitCaptured},
		{"delete", (*Instance).DeleteCaptured},
		{"delete and wait", (*Instance).DeleteAndWaitCaptured},
		{"remove", (*Instance).RemoveCaptured},
	}

	for _, operation := range destructive {
		t.Run(operation.name, func(t *testing.T) {
			inst, db, current := newRuntimeDeleteTestInstance(t)
			terminated := 0
			stubRuntimeDeletionPhysicalWork(t, current, func(tmux.RuntimeGenerationCandidate, bool) error {
				terminated++
				return nil
			})
			selection := inst.CaptureRuntimeSelection()
			next := current
			next.Generation++
			next.StatusRevision = 0
			next.TmuxSession = "runtime-g2"
			next.LastStartedAt = current.LastStartedAt.Add(time.Second)
			if err := db.CommitRuntimeTransition(current.Generation, selection.Incarnation, next); err != nil {
				t.Fatal(err)
			}
			inst.ApplyRuntimeState(next)

			if err := operation.call(inst, selection); !errors.Is(err, statedb.ErrRuntimeGenerationConflict) {
				t.Fatalf("stale %s error = %v, want generation conflict", operation.name, err)
			}
			if terminated != 0 {
				t.Fatalf("stale %s terminated %d runtimes", operation.name, terminated)
			}
			got, found, err := db.ReadRuntimeState(inst.ID)
			if err != nil || !found || got != next {
				t.Fatalf("runtime after stale %s = %+v, found=%v err=%v; want %+v", operation.name, got, found, err, next)
			}
		})
	}

	t.Run("terminal status", func(t *testing.T) {
		inst, db, current := newRuntimeDeleteTestInstance(t)
		incarnation := inst.PersistenceIncarnation()
		next := current
		next.Generation++
		next.StatusRevision = 0
		next.TmuxSession = "runtime-g2"
		if err := db.CommitRuntimeTransition(current.Generation, incarnation, next); err != nil {
			t.Fatal(err)
		}
		applied, err := db.WriteStatusIfVersion(inst.ID, incarnation, current.Generation, current.StatusRevision, string(StatusStopped))
		if err != nil || applied {
			t.Fatalf("stale terminal status applied=%v err=%v", applied, err)
		}
		got, found, err := db.ReadRuntimeState(inst.ID)
		if err != nil || !found || got != next {
			t.Fatalf("runtime after stale terminal status = %+v, found=%v err=%v; want %+v", got, found, err, next)
		}
	})
}

func TestRuntimeLifecycle_PhysicalEntrypointRoutingContract(t *testing.T) {
	contracts := []struct {
		surface, path, function string
		calls                   []string
	}{
		{"Start result", "internal/session/restart_result.go", "StartRuntime", []string{"start"}},
		{"StartWithMessage result", "internal/session/restart_result.go", "StartWithMessageRuntime", []string{"startWithMessage"}},
		{"restart result", "internal/session/restart_result.go", "RestartRuntime", []string{"restart"}},
		{"restart-with-env result", "internal/session/restart_result.go", "RestartWithEnvRuntime", []string{"restartWithEnv"}},
		{"fresh-recovery result", "internal/session/restart_result.go", "RestartFreshRuntime", []string{"restartFresh"}},
		{"Start core", "internal/session/instance.go", "start", []string{"beginRuntimeTransition", "commitPhysicalRuntime"}},
		{"StartWithMessage core", "internal/session/instance.go", "startWithMessage", []string{"beginRuntimeTransition", "commitPhysicalRuntime"}},
		{"fallback authority", "internal/session/instance.go", "restart", []string{"beginRuntimeTransition", "restartWithTransition"}},
		{"fallback core", "internal/session/instance.go", "restartWithTransition", []string{"commitPhysicalRuntime"}},
		{"CLI start", "cmd/agent-deck/session_cmd.go", "handleSessionStart", []string{"StartRuntime", "StartWithMessageRuntime", "consumeRuntimeResult"}},
		{"CLI launch", "cmd/agent-deck/launch_cmd.go", "handleLaunch", []string{"StartRuntime", "StartWithMessageRuntime", "consumeRuntimeResult"}},
		{"CLI restart", "cmd/agent-deck/session_cmd.go", "handleSessionRestart", []string{"RestartWithEnvRuntime", "consumeRuntimeResult"}},
		{"CLI queued recovery", "cmd/agent-deck/session_cmd.go", "drainGroupQueue", []string{"StartRuntime", "consumeRuntimeResult"}},
		{"CLI fork", "cmd/agent-deck/session_cmd.go", "handleSessionFork", []string{"StartRuntime", "consumeRuntimeResult"}},
		{"CLI plugin mutation", "cmd/agent-deck/plugin_cmd.go", "pluginAttachOrDetach", []string{"RestartRuntime", "consumeRuntimeResult"}},
		{"CLI skill mutation", "cmd/agent-deck/skill_cmd.go", "restartProjectSkillsSession", []string{"RestartRuntime", "consumeRuntimeResult"}},
		{"CLI move", "cmd/agent-deck/session_move.go", "handleSessionMove", []string{"RestartRuntime", "consumeRuntimeResult"}},
		{"CLI try", "cmd/agent-deck/try_cmd.go", "handleTry", []string{"StartRuntime", "consumeRuntimeResult"}},
		{"CLI add attach", "cmd/agent-deck/main.go", "handleAdd", []string{"StartRuntime", "consumeRuntimeResult"}},
		{"CLI switch account", "cmd/agent-deck/session_switch_account.go", "handleSessionSwitchAccount", []string{"SwitchAccount"}},
		{"account switch core", "internal/session/account_switch.go", "SwitchAccount", []string{"SwitchAccountRuntime", "ConsumePhysicalRuntimeResult"}},
		{"MCP attach", "cmd/agent-deck/mcp_cmd.go", "handleMCPAttach", []string{"RestartRuntime", "consumeRuntimeResult"}},
		{"MCP detach", "cmd/agent-deck/mcp_cmd.go", "handleMCPDetach", []string{"RestartRuntime", "consumeRuntimeResult"}},
		{"web start seam", "internal/ui/web_mutator.go", "startRuntime", []string{"StartRuntime"}},
		{"web restart seam", "internal/ui/web_mutator.go", "restartRuntime", []string{"RestartRuntime"}},
		{"web create consumer", "internal/ui/web_mutator.go", "CreateSession", []string{"startRuntime", "consumeRuntime"}},
		{"web start consumer", "internal/ui/web_mutator.go", "StartSession", []string{"startRuntime", "consumeRuntime"}},
		{"web restart consumer", "internal/ui/web_mutator.go", "RestartSession", []string{"restartRuntime", "consumeRuntime"}},
		{"web recovery consumer", "internal/ui/web_mutator.go", "UndoDelete", []string{"restartRuntime", "consumeRuntime"}},
		{"web fork consumer", "internal/ui/web_mutator.go", "ForkSession", []string{"startRuntime", "consumeRuntime"}},
		{"TUI start seam", "internal/ui/home.go", "startRuntime", []string{"StartRuntime"}},
		{"TUI restart seam", "internal/ui/home.go", "restartRuntime", []string{"RestartRuntime"}},
		{"TUI fresh seam", "internal/ui/home.go", "restartFreshRuntime", []string{"RestartFreshRuntime"}},
		{"TUI imported-session consumer", "internal/ui/home.go", "createSessionFromGlobalSearch", []string{"startRuntime", "consumePhysicalRuntimeResult"}},
		{"TUI create consumer", "internal/ui/home.go", "createSessionInGroupWithWorktreeAndOptions", []string{"startRuntime", "consumePhysicalRuntimeResult"}},
		{"TUI fork consumer", "internal/ui/home.go", "completeForkRuntime", []string{"startRuntime", "consumeResult"}},
		{"TUI restart consumer", "internal/ui/home.go", "restartSession", []string{"restartRuntime", "consumePhysicalRuntimeResult"}},
		{"TUI fresh-recovery consumer", "internal/ui/home.go", "restartSessionFreshRuntimeWith", []string{"restartFresh", "consumePhysicalRuntimeResult"}},
		{"fleet seam", "internal/fleet/recover.go", "NewRecoverer", []string{"RestartRuntime"}},
		{"fleet consumer", "internal/fleet/recover.go", "Recover", []string{"restart", "consumeRestartRuntime"}},
	}

	for _, contract := range contracts {
		t.Run(contract.surface, func(t *testing.T) {
			calls := runtimeLifecycleFunctionCalls(t, contract.path, contract.function)
			for _, want := range contract.calls {
				if calls[want] == 0 {
					t.Fatalf("%s.%s does not call %s", contract.path, contract.function, want)
				}
			}
		})
	}
}

func runtimeLifecycleFunctionCalls(t *testing.T, path, function string) map[string]int {
	t.Helper()
	root := runtimeLifecycleSourceRoot(t)
	parsed, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, path), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var body *ast.BlockStmt
	for _, declaration := range parsed.Decls {
		decl, ok := declaration.(*ast.FuncDecl)
		if ok && decl.Name.Name == function {
			body = decl.Body
			break
		}
	}
	if body == nil {
		t.Fatalf("function %s not found in %s", function, path)
	}
	calls := make(map[string]int)
	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch target := call.Fun.(type) {
		case *ast.Ident:
			calls[target.Name]++
		case *ast.SelectorExpr:
			calls[target.Sel.Name]++
		}
		return true
	})
	return calls
}

func runtimeLifecycleSourceRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		info, statErr := os.Stat(filepath.Join(dir, "go.mod"))
		if statErr == nil && !info.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("cannot find go.mod above %s", dir)
		}
		dir = parent
	}
}
