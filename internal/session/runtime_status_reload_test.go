package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

func TestRuntimeLifecycle_StatusReloadDefersDuringPhysicalReplacement(t *testing.T) {
	for _, duringProbe := range []bool{false, true} {
		name := "before-probe"
		if duringProbe {
			name = "after-probe"
		}
		t.Run(name, func(t *testing.T) {
			installRuntimeLifecycleTestSeams(t)
			// Use the real shared lock for both transition and nonblocking probe.
			instanceSpawnLockAcquireFn = defaultAcquireInstanceSpawnLock
			db, inst := runtimeLifecycleTestDB(t, "pi", nil)
			observed := inst.RuntimeState()
			incarnation := inst.PersistenceIncarnation()
			probeEntered, releaseProbe := make(chan struct{}), make(chan struct{})
			setStatusProbeOverride(t, func(context.Context, *Instance, statedb.RuntimeState) (Status, error) {
				close(probeEntered)
				<-releaseProbe
				return StatusStopped, nil
			})
			type result struct {
				state statedb.RuntimeState
				err   error
			}
			statusDone := make(chan result, 1)
			finished := make(chan struct{})
			started := false
			probe := func() {
				defer close(finished)
				state, err := inst.UpdateStatusObserved(context.Background(), observed, incarnation)
				statusDone <- result{state, err}
			}
			t.Cleanup(func() {
				select {
				case <-releaseProbe:
				default:
					close(releaseProbe)
				}
				if started {
					select {
					case <-finished:
					case <-time.After(5 * time.Second):
						t.Error("status probe did not finish")
					}
				}
			})
			if duringProbe {
				started = true
				go probe()
				<-probeEntered
			}
			authority, winner, err := inst.beginRuntimeTransition(false)
			if err != nil || winner != nil {
				t.Fatalf("begin transition: winner=%v err=%v", winner, err)
			}
			defer authority.close()
			// Fallback recreation has succeeded, but the new pane has not yet
			// been stamped or committed. The old durable tmux name is absent.
			setRuntimeTestCandidate(inst, "replacement")
			runtimeCandidateExistsFn = func(s *tmux.Session) (bool, error) { return s.Name == "replacement", nil }
			stamped := ""
			runtimeCandidateStampFn = func(s *tmux.Session, _ statedb.RuntimeState, _, _ string) error {
				stamped = s.Name
				return nil
			}
			if duringProbe {
				close(releaseProbe)
			} else {
				started = true
				go probe()
			}
			select {
			case got := <-statusDone:
				if !errors.Is(got.err, statedb.ErrRuntimeGenerationConflict) || got.state.TmuxSession != "replacement" {
					t.Fatalf("status recovery restored predecessor during spawn: state=%+v err=%v", got.state, got.err)
				}
			case <-time.After(time.Second):
				t.Fatal("status recovery blocked behind physical transition")
			}
			next, _, committed, err := inst.commitPhysicalRuntime(authority)
			if err != nil || !committed || stamped != "replacement" {
				t.Fatalf("physical publication: stamped=%q committed=%v err=%v", stamped, committed, err)
			}
			authority.close()
			recovered, err := inst.UpdateStatusObserved(context.Background(), observed, incarnation)
			if !errors.Is(err, statedb.ErrRuntimeGenerationConflict) || !sameStatusRuntime(recovered, next) {
				t.Fatalf("status recovery: state=%+v err=%v; want committed replacement %+v", recovered, err, next)
			}
			durable, found, err := db.ReadRuntimeState(inst.ID)
			if err != nil || !found || !sameStatusRuntime(durable, next) || !sameStatusRuntime(inst.RuntimeState(), next) {
				t.Fatalf("replacement lost: durable=%+v memory=%+v err=%v", durable, inst.RuntimeState(), err)
			}
		})
	}
}

func TestRuntimeLifecycle_ReloadToolMetadataDiscardsOldDetection(t *testing.T) {
	for _, deferred := range []bool{false, true} {
		name := "reload"
		if deferred {
			name = "deferred"
		}
		t.Run(name, func(t *testing.T) {
			canonical := &Instance{
				ID: "one", Tool: "claude", Command: "claude", Status: StatusStopped,
				RuntimeGeneration: 2, StatusRevision: 4, TmuxSocketName: "isolated",
				tmuxSession: tmux.ReconnectSessionLazy("runtime", "one", "", "claude", "stopped"),
				hookStatus:  "running", hookEvent: "UserPromptSubmit", hookSessionID: "old-claude-id",
				hookLastUpdate: time.Now(),
			}
			canonical.tmuxSession.SocketName = "isolated"
			if got := canonical.tmuxSession.DetectTool(); got != "claude" {
				t.Fatalf("prime detection: %q", got)
			}
			state := canonical.RuntimeState()
			loaded := &Instance{ID: "one", Tool: "codex", Command: "codex"}
			loaded.adoptRuntimeState(state)
			if deferred {
				canonical.MergeDeferredReload(loaded)
			} else {
				canonical.MergeReloaded(loaded)
			}
			canonical.refreshStatusMetadataIfCurrent(state, RuntimeBindingObservation{}, true, true, nil)
			if canonical.Tool != "codex" || canonical.tmuxSession.Command != "codex" {
				t.Fatalf("status poll restored old metadata: tool=%q command=%q", canonical.Tool, canonical.tmuxSession.Command)
			}
			if canonical.hookStatus != "" || canonical.hookEvent != "" || canonical.hookSessionID != "" || !canonical.hookLastUpdate.IsZero() {
				t.Fatalf("old hook survived tool change: status=%q event=%q session=%q", canonical.hookStatus, canonical.hookEvent, canonical.hookSessionID)
			}
			if !sameStatusRuntime(canonical.RuntimeState(), state) {
				t.Fatalf("metadata reload changed physical runtime: %+v", canonical.RuntimeState())
			}
		})
	}
}

func TestRuntimeLifecycle_ReloadUnchangedToolPreservesDetection(t *testing.T) {
	canonical := &Instance{
		ID: "one", Tool: "claude", Command: "claude", Status: StatusStopped,
		tmuxSession: tmux.ReconnectSessionLazy("runtime", "one", "", "claude", "stopped"),
		hookStatus:  "waiting", hookSessionID: "conversation",
	}
	prior := canonical.tmuxSession
	loaded := &Instance{ID: "one", Tool: "claude", Command: "claude", Title: "renamed"}
	loaded.adoptRuntimeState(canonical.RuntimeState())
	canonical.MergeReloaded(loaded)
	if canonical.tmuxSession != prior || canonical.hookStatus != "waiting" || canonical.hookSessionID != "conversation" {
		t.Fatal("ordinary metadata reload discarded runtime caches")
	}
}

func TestRuntimeLifecycle_ReloadRejectsInFlightToolDetection(t *testing.T) {
	dir := t.TempDir()
	entered, release := filepath.Join(dir, "entered"), filepath.Join(dir, "release")
	t.Setenv("DETECTION_ENTERED", entered)
	t.Setenv("DETECTION_RELEASE", release)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(`#!/bin/sh
case "$*" in
  *capture-pane*)
    : > "$DETECTION_ENTERED"
    while [ ! -e "$DETECTION_RELEASE" ]; do sleep 0.01; done
    printf 'Claude Code\n'
    ;;
esac
`), 0o755); err != nil {
		t.Fatal(err)
	}
	canonical := &Instance{
		ID: "one", Tool: "claude", Command: "wrapper", Status: StatusStopped,
		tmuxSession: tmux.ReconnectSessionLazy("runtime", "one", "", "wrapper", "stopped"),
	}
	state := canonical.RuntimeState()
	oldWrapper := canonical.tmuxSession
	done := make(chan struct{})
	go func() {
		canonical.refreshStatusMetadataIfCurrent(state, RuntimeBindingObservation{}, true, true, nil)
		close(done)
	}()
	t.Cleanup(func() {
		_ = os.WriteFile(release, nil, 0o600)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("tool detector did not finish")
		}
	})
	// Pause the actual detector's pane read after it captured the old wrapper.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(entered); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("old tool detector did not enter capture-pane")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-done:
		t.Fatal("old detector completed before metadata reload")
	default:
	}
	loaded := &Instance{ID: "one", Tool: "codex", Command: "codex"}
	loaded.adoptRuntimeState(state)
	canonical.MergeReloaded(loaded)
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	<-done
	if got := oldWrapper.DetectTool(); got != "claude" {
		t.Fatalf("old wrapper detected %q, want claude", got)
	}
	if canonical.Tool != "codex" {
		t.Fatalf("in-flight old detector restored tool %q after reload", canonical.Tool)
	}
}

// Every construction of an Instance's tmux wrapper carries the configured
// per-session options and the Instance's group path, and a wrapper around a
// live session that no Start or Restart configures afterwards also carries the
// [tmux].options overrides and its tool's detection patterns. Runtime adoption
// (a reload that corrects the tool, or a durable winner under another tmux
// name) built its wrapper bare: the adopted session ignored
// inject_status_line, mouse, clear_on_restart, the Indic zero-width-mark
// opt-in (#2334) and the terminal-chrome setting, the next attach pushed
// agent-deck's status-bar defaults over the user's options and cleared
// @agentdeck_group_path, and status detection fell back to command-inferred
// patterns that ignore a [tools.<name>] definition. The adopting sites run a
// custom tool so that only a configured definition can supply its patterns.
// Discovery is covered with a real server by
// TestDiscoverExistingTmuxSessionsConfiguresImportedWrappers.
func TestRuntimeLifecycle_EveryTmuxWrapperConstructionAppliesSettings(t *testing.T) {
	home := configureNonDefaultTmuxWrapperSettings(t)
	type site struct {
		build func(t *testing.T) (*Instance, *tmux.Session)
		// adopts marks a wrapper around a live session that no Start or
		// Restart configures afterwards.
		adopts bool
	}
	sites := map[string]site{
		"NewInstance": {build: func(*testing.T) (*Instance, *tmux.Session) {
			inst := NewInstance("settings", home)
			return inst, inst.tmuxSession
		}},
		"NewInstanceWithTool": {build: func(*testing.T) (*Instance, *tmux.Session) {
			inst := NewInstanceWithTool("settings", home, "claude")
			return inst, inst.tmuxSession
		}},
		"recreateTmuxSession": {build: func(*testing.T) (*Instance, *tmux.Session) {
			inst := &Instance{ID: "settings", Title: "settings", ProjectPath: home, GroupPath: "work/settings", TmuxSocketName: "isolated"}
			inst.recreateTmuxSession()
			return inst, inst.tmuxSession
		}},
		"storage load": {adopts: true, build: func(t *testing.T) (*Instance, *tmux.Session) {
			instances, _, err := (&Storage{}).convertToInstances(&StorageData{Instances: []*InstanceData{{
				ID: "settings", Title: "settings", Tool: wrapperSettingsCustomTool, Command: wrapperSettingsCustomTool,
				ProjectPath: home, GroupPath: "work/settings", TmuxSession: "agentdeck_settings",
			}}})
			if err != nil {
				t.Fatal(err)
			}
			return instances[0], instances[0].tmuxSession
		}},
		"runtime adoption": {adopts: true, build: func(*testing.T) (*Instance, *tmux.Session) {
			inst := &Instance{ID: "settings", Title: "settings", ProjectPath: home, GroupPath: "work/settings",
				Tool: wrapperSettingsCustomTool, Command: wrapperSettingsCustomTool}
			inst.adoptRuntimeState(statedb.RuntimeState{
				InstanceID: "settings", Generation: 2, TmuxSession: "agentdeck_settings_g2", TmuxSocketName: "isolated",
				Status: string(StatusRunning),
			})
			return inst, inst.tmuxSession
		}},
		"tool-correcting reload": {adopts: true, build: func(t *testing.T) (*Instance, *tmux.Session) {
			inst := &Instance{ID: "settings", Title: "settings", ProjectPath: home, GroupPath: "work/old", Tool: "shell", Command: "bash",
				TmuxSocketName: "isolated", tmuxSession: &tmux.Session{Name: "agentdeck_settings", SocketName: "isolated"}}
			loaded := &Instance{ID: "settings", Title: "settings", ProjectPath: home, GroupPath: "work/settings",
				Tool: wrapperSettingsCustomTool, Command: wrapperSettingsCustomTool,
				TmuxSocketName: "isolated", tmuxSession: &tmux.Session{Name: "agentdeck_settings", SocketName: "isolated"}}
			if !inst.MergeReloaded(loaded) {
				t.Fatal("the reloaded row was not merged")
			}
			return inst, inst.GetTmuxSession()
		}},
	}
	for name, site := range sites {
		t.Run(name, func(t *testing.T) {
			inst, sess := site.build(t)
			assertTmuxWrapperConfigured(t, inst, sess, site.adopts)
		})
	}
}

// wrapperSettingsCustomTool is the [tools.<name>] entry
// configureNonDefaultTmuxWrapperSettings defines, with its own busy, prompt
// and detection patterns and no built-in defaults to fall back on.
const (
	wrapperSettingsCustomTool    = "wrapped-agent"
	wrapperSettingsBusyPattern   = "wrapped-agent is thinking"
	wrapperSettingsDetectPattern = "Wrapped Agent v"
)

// configureNonDefaultTmuxWrapperSettings writes a user config whose per-session
// tmux settings all differ from both a new wrapper's defaults and a bare
// wrapper's zero values, plus one [tmux].options override and the
// wrapperSettingsCustomTool definition, and returns the isolated HOME.
func configureNonDefaultTmuxWrapperSettings(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	ClearUserConfigCache()
	t.Cleanup(ClearUserConfigCache)
	inject, mouse, badge := false, false, true
	if err := SaveUserConfig(&UserConfig{
		Tmux: TmuxSettings{
			InjectStatusLine: &inject, Mouse: &mouse, IndicZeroWidthMarks: true, ClearOnRestart: true,
			Options: map[string]string{"status": "2"},
		},
		Terminal: TerminalSettings{ITermBadge: &badge},
		Tools: map[string]ToolDef{wrapperSettingsCustomTool: {
			Command:        wrapperSettingsCustomTool,
			BusyPatterns:   []string{wrapperSettingsBusyPattern},
			PromptPatterns: []string{"wrapped> "},
			DetectPatterns: []string{wrapperSettingsDetectPattern},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	return home
}

// assertTmuxWrapperConfigured checks sess against the settings
// configureNonDefaultTmuxWrapperSettings wrote. Every wrapper carries the
// per-session settings and inst's group path; an adopting wrapper also carries
// the option overrides and the detection patterns that Start would otherwise
// install, which for wrapperSettingsCustomTool come from its definition alone.
func assertTmuxWrapperConfigured(t *testing.T, inst *Instance, sess *tmux.Session, adopts bool) {
	t.Helper()
	if inst == nil || sess == nil {
		t.Fatalf("no tmux wrapper was constructed: instance=%v wrapper=%v", inst, sess)
	}
	if sess.GetMouse() {
		t.Error("mouse = true, want the configured false")
	}
	fields := reflect.ValueOf(sess).Elem()
	for field, want := range map[string]bool{
		"injectStatusLine":       false,
		"indicZeroWidthMarks":    true,
		"indicZeroWidthMarksSet": true,
		"clearOnRestart":         true,
		"terminalChromeEnabled":  true,
	} {
		if got := fields.FieldByName(field).Bool(); got != want {
			t.Errorf("%s = %v, want the configured %v", field, got, want)
		}
	}
	if got := sess.GetGroupPath(); got != inst.GroupPath {
		t.Errorf("wrapper group path = %q, want the instance's %q", got, inst.GroupPath)
	}
	if !adopts {
		return
	}
	if got := sess.OptionOverrides["status"]; got != "2" {
		t.Errorf("option overrides = %v, want the configured status=2", sess.OptionOverrides)
	}
	patterns := fields.FieldByName("resolvedPatterns")
	if patterns.IsNil() {
		t.Errorf("resolved patterns = nil, want %s's detection patterns", inst.Tool)
	}
	if inst.Tool != wrapperSettingsCustomTool {
		return
	}
	if !patterns.IsNil() {
		if busy := patterns.Elem().FieldByName("BusyStrings"); busy.Len() != 1 || busy.Index(0).String() != wrapperSettingsBusyPattern {
			t.Errorf("resolved busy strings = %v, want the definition's [%q]", busy, wrapperSettingsBusyPattern)
		}
	}
	if got := fields.FieldByName("customToolName").String(); got != inst.Tool {
		t.Errorf("custom tool name = %q, want %q", got, inst.Tool)
	}
	if detect := fields.FieldByName("customDetectPatterns"); detect.Len() != 1 || detect.Index(0).String() != wrapperSettingsDetectPattern {
		t.Errorf("detect patterns = %v, want the definition's [%q]", detect, wrapperSettingsDetectPattern)
	}
}

// A reload that keeps the wrapper (same tmux session, same tool) still brings
// it up to date with the metadata another process committed. After a
// cross-process `session move`, rename or sandbox change the kept wrapper
// carried the old group path, display name and remain-on-exit, and the next
// attach's EnsureConfigured wrote them back over the tmux options the
// committing process had published. A deferred reload merges the same
// metadata once the transition that deferred it has finished.
func TestRuntimeLifecycle_ReloadRefreshesKeptWrapperMetadata(t *testing.T) {
	configureNonDefaultTmuxWrapperSettings(t)
	for _, deferred := range []bool{false, true} {
		name := "reload"
		if deferred {
			name = "deferred"
		}
		t.Run(name, func(t *testing.T) {
			canonical := &Instance{ID: "kept", Title: "before", GroupPath: "work/old", Tool: "claude", Command: "claude",
				Status: StatusRunning, RuntimeGeneration: 2, StatusRevision: 1, TmuxSocketName: "isolated"}
			canonical.adoptRuntimeState(statedb.RuntimeState{
				InstanceID: "kept", Generation: 2, StatusRevision: 1, TmuxSession: "agentdeck_kept", TmuxSocketName: "isolated",
				Status: string(StatusRunning),
			})
			kept := canonical.GetTmuxSession()
			merge := func(title, group string, sandbox *SandboxConfig) {
				t.Helper()
				loaded := &Instance{ID: "kept", Title: title, GroupPath: group, Tool: "claude", Command: "claude", Sandbox: sandbox}
				loaded.adoptRuntimeState(canonical.RuntimeState())
				merged := canonical.MergeReloaded
				if deferred {
					merged = canonical.MergeDeferredReload
				}
				if !merged(loaded) {
					t.Fatal("the reloaded row was not merged")
				}
				if canonical.GetTmuxSession() != kept {
					t.Fatal("a metadata-only reload replaced the wrapper")
				}
				if got := kept.GetGroupPath(); got != group {
					t.Errorf("kept wrapper group path = %q, want the moved group %q", got, group)
				}
				if kept.DisplayName != title {
					t.Errorf("kept wrapper display name = %q, want the new title %q", kept.DisplayName, title)
				}
				if got := kept.OptionOverrides["status"]; got != "2" {
					t.Errorf("kept wrapper option overrides = %v, want the configured status=2 kept", kept.OptionOverrides)
				}
			}

			merge("after", "work/new", &SandboxConfig{Enabled: true})
			if got := kept.OptionOverrides["remain-on-exit"]; got != "on" {
				t.Errorf("sandboxed wrapper remain-on-exit = %q, want on", got)
			}
			merge("after", "work/new", nil)
			if got, ok := kept.OptionOverrides["remain-on-exit"]; ok {
				t.Errorf("unsandboxed wrapper remain-on-exit = %q, want it dropped", got)
			}
		})
	}
}

// A long-lived process keeps its canonical Instance across storage reloads
// and merges each reloaded row into it. The merge also installs the reloaded
// row as the next save's baseline, so a favourite it did not copy reads as
// this process's own edit and the next routine save reverts a `session set
// favorite false` another process made.
func TestRuntimeLifecycle_ReloadMergeKeepsFavoriteUnsetFromOtherProcess(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	openStorage := func() *Storage {
		db, err := statedb.Open(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Migrate(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return &Storage{db: db, dbPath: dbPath, profile: "_test"}
	}
	tui, cli := openStorage(), openStorage()
	seed := &Instance{
		ID: "favorite-reload", Title: "favorite", ProjectPath: t.TempDir(), GroupPath: "test",
		Command: "claude", Tool: "claude", Status: StatusIdle, CreatedAt: time.Now(), Favorite: true,
	}
	if err := tui.InsertSessionAndVerify(seed, nil); err != nil {
		t.Fatal(err)
	}
	instances, _, err := tui.LoadWithGroups()
	if err != nil || len(instances) != 1 || !instances[0].Favorite {
		t.Fatalf("load favourite: instances=%d err=%v", len(instances), err)
	}
	canonical := instances[0]

	// `agent-deck session set favorite-reload favorite false` in another process.
	cliInstances, cliGroups, err := cli.LoadWithGroups()
	if err != nil || len(cliInstances) != 1 {
		t.Fatalf("cli load: instances=%d err=%v", len(cliInstances), err)
	}
	if _, _, err := SetField(cliInstances[0], FieldFavorite, "false", nil); err != nil {
		t.Fatal(err)
	}
	if err := cli.SaveWithGroups(cliInstances, NewGroupTreeWithGroups(cliInstances, cliGroups)); err != nil {
		t.Fatal(err)
	}

	// The storage watcher's reload merges into the canonical object, then any
	// routine save (rename, reorder, another session's delete) writes it back.
	reloaded, _, err := tui.LoadWithGroups()
	if err != nil || len(reloaded) != 1 {
		t.Fatalf("reload: instances=%d err=%v", len(reloaded), err)
	}
	if !canonical.MergeReloaded(reloaded[0]) {
		t.Fatal("reload was not merged into the canonical instance")
	}
	if err := tui.SaveWithGroups([]*Instance{canonical}, nil); err != nil {
		t.Fatal(err)
	}

	row, err := tui.db.LoadInstanceByID(canonical.ID)
	if err != nil || row == nil {
		t.Fatalf("read row: row=%v err=%v", row, err)
	}
	if ReadFavoriteFromToolData(row.ToolData) {
		t.Fatalf("routine save reverted the other process's favourite unset: tool_data=%s", row.ToolData)
	}
	if canonical.Favorite {
		t.Fatal("canonical instance kept the stale favourite after the reload")
	}
}
