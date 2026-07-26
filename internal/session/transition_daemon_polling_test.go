package session

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

type hotPollStateSnapshot struct {
	tmuxSession                *tmux.Session
	lastOpenCodeScanAt         time.Time
	lastCodexScanAt            time.Time
	lastCodexProbeAt           time.Time
	lastPromptModTime          time.Time
	lastJSONLSize              int64
	lastJSONLPath              string
	cachedPrompt               string
	lastErrorCheck             time.Time
	lastIdleCheck              time.Time
	lastKnownActivity          int64
	tmuxFlipFromRunningPending bool
	lastSessionMetaSync        time.Time
	hermesGatewayCheckedAt     time.Time
	hermesGatewayOK            bool
}

func snapshotHotPollState(inst *Instance) hotPollStateSnapshot {
	return hotPollStateSnapshot{
		tmuxSession:                inst.tmuxSession,
		lastOpenCodeScanAt:         inst.lastOpenCodeScanAt,
		lastCodexScanAt:            inst.lastCodexScanAt,
		lastCodexProbeAt:           inst.lastCodexProbeAt,
		lastPromptModTime:          inst.lastPromptModTime,
		lastJSONLSize:              inst.lastJSONLSize,
		lastJSONLPath:              inst.lastJSONLPath,
		cachedPrompt:               inst.cachedPrompt,
		lastErrorCheck:             inst.lastErrorCheck,
		lastIdleCheck:              inst.lastIdleCheck,
		lastKnownActivity:          inst.lastKnownActivity,
		tmuxFlipFromRunningPending: inst.tmuxFlipFromRunningPending,
		lastSessionMetaSync:        inst.lastSessionMetaSync,
		hermesGatewayCheckedAt:     inst.hermesGatewayCheckedAt,
		hermesGatewayOK:            inst.hermesGatewayOK,
	}
}

func seedHotPollState(inst *Instance, now time.Time) {
	inst.lastOpenCodeScanAt = now.Add(-time.Second)
	inst.lastCodexScanAt = now.Add(-2 * time.Second)
	inst.lastCodexProbeAt = now.Add(-3 * time.Second)
	inst.lastPromptModTime = now.Add(-4 * time.Second)
	inst.lastJSONLSize = 42
	inst.lastJSONLPath = "/tmp/session.jsonl"
	inst.cachedPrompt = "cached prompt"
	inst.lastErrorCheck = now.Add(-5 * time.Second)
	inst.lastIdleCheck = now.Add(-6 * time.Second)
	inst.lastKnownActivity = 12345
	inst.tmuxFlipFromRunningPending = true
	inst.lastSessionMetaSync = now.Add(-7 * time.Second)
	inst.hermesGatewayCheckedAt = now.Add(-8 * time.Second)
	inst.hermesGatewayOK = true
}

func TestSyncProfile_PreservesTerminalPollThrottleAcrossReload(t *testing.T) {
	const profile = "_test_transition_terminal_poll_state"
	d, storage := bootstrapDaemonProfile(t, profile)

	instances := []*Instance{
		{
			ID:          "stopped-terminal",
			Title:       "stopped",
			ProjectPath: filepath.Join(os.Getenv("HOME"), "stopped-project"),
			GroupPath:   DefaultGroupPath,
			Tool:        "claude",
			Status:      StatusStopped,
			CreatedAt:   time.Now().Add(-time.Hour),
		},
		{
			ID:          "error-terminal",
			Title:       "error",
			ProjectPath: filepath.Join(os.Getenv("HOME"), "error-project"),
			GroupPath:   DefaultGroupPath,
			Tool:        "claude",
			Status:      StatusError,
			CreatedAt:   time.Now().Add(-time.Hour),
		},
	}
	for _, inst := range instances {
		if err := os.MkdirAll(inst.ProjectPath, 0o755); err != nil {
			t.Fatalf("mkdir project %s: %v", inst.ID, err)
		}
	}
	if err := storage.SaveWithGroups(instances, nil); err != nil {
		t.Fatalf("save terminal instances: %v", err)
	}

	stamps := map[string]time.Time{
		"stopped-terminal": time.Now().Add(-5 * time.Second),
		"error-terminal":   time.Now().Add(-7 * time.Second),
	}
	observed := make(map[string][]time.Time, len(stamps))
	var observedMu sync.Mutex

	originalProbe := updateInstanceStatus.Load().(statusProbeFunc)
	updateInstanceStatus.Store(statusProbeFunc(func(inst *Instance) error {
		inst.mu.Lock()
		defer inst.mu.Unlock()

		observedMu.Lock()
		observed[inst.ID] = append(observed[inst.ID], inst.lastErrorCheck)
		observedMu.Unlock()

		if inst.lastErrorCheck.IsZero() {
			inst.lastErrorCheck = stamps[inst.ID]
		}
		return nil
	}))
	t.Cleanup(func() { updateInstanceStatus.Store(originalProbe) })

	d.syncProfile(profile)
	d.syncProfile(profile)

	observedMu.Lock()
	defer observedMu.Unlock()
	for id, want := range stamps {
		got := observed[id]
		if len(got) != 2 {
			t.Fatalf("%s: expected two polls, got %d", id, len(got))
		}
		if !got[0].IsZero() {
			t.Fatalf("%s: first load unexpectedly had terminal poll state %s", id, got[0])
		}
		if !got[1].Equal(want) {
			t.Fatalf("%s: terminal poll state did not survive reload: want %s, got %s", id, want, got[1])
		}
	}
}

func TestSyncOnce_WarmsTmuxCachesOnceAcrossProfiles(t *testing.T) {
	const (
		profileA = "_test_transition_cache_a"
		profileB = "_test_transition_cache_b"
	)
	d, _ := bootstrapDaemonProfile(t, profileA)
	storageB, err := NewStorageWithProfile(profileB)
	if err != nil {
		t.Fatalf("NewStorageWithProfile(%s): %v", profileB, err)
	}
	t.Cleanup(func() { _ = storageB.Close() })
	d.storages[profileB] = storageB

	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "tmux-calls.log")
	fakeTmux := filepath.Join(binDir, "tmux")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$TMUX_TEST_LOG\"\n"
	if err := os.WriteFile(fakeTmux, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("TMUX_TEST_LOG", logPath)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	d.SyncOnce(context.Background())

	data, err := os.ReadFile(logPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read fake tmux log: %v", err)
	}
	var listWindows, listPanes int
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.Contains(line, "list-windows") {
			listWindows++
		}
		if strings.Contains(line, "list-panes") {
			listPanes++
		}
	}
	if listWindows != 1 || listPanes != 1 {
		t.Fatalf("one SyncOnce across two profiles must warm each tmux cache once; list-windows=%d list-panes=%d calls=%q",
			listWindows, listPanes, strings.TrimSpace(string(data)))
	}
}

func TestSyncProfile_PreservesHotPollStateAcrossReload(t *testing.T) {
	const profile = "_test_transition_hot_poll_state"
	d, storage := bootstrapDaemonProfile(t, profile)

	projectPath := filepath.Join(os.Getenv("HOME"), "hot-poll-project")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	inst := &Instance{
		ID:             "hot-poll-instance",
		Title:          "hot-poll",
		ProjectPath:    projectPath,
		GroupPath:      DefaultGroupPath,
		Command:        "codex",
		Tool:           "codex",
		Status:         StatusRunning,
		CreatedAt:      time.Now().Add(-time.Hour),
		CodexSessionID: "11111111-1111-1111-1111-111111111111",
		tmuxSession: &tmux.Session{
			Name:        "agentdeck_hot_poll",
			DisplayName: "hot-poll",
			WorkDir:     projectPath,
			Command:     "codex",
			InstanceID:  "hot-poll-instance",
		},
	}
	if err := storage.SaveWithGroups([]*Instance{inst}, nil); err != nil {
		t.Fatalf("save instance: %v", err)
	}

	originalProbe := updateInstanceStatus.Load().(statusProbeFunc)
	var probeMu sync.Mutex
	var first, second hotPollStateSnapshot
	var probes int
	updateInstanceStatus.Store(statusProbeFunc(func(inst *Instance) error {
		inst.mu.Lock()
		defer inst.mu.Unlock()
		probeMu.Lock()
		defer probeMu.Unlock()

		probes++
		if probes == 1 {
			seedHotPollState(inst, time.Now())
			first = snapshotHotPollState(inst)
		} else {
			second = snapshotHotPollState(inst)
		}
		return nil
	}))
	t.Cleanup(func() { updateInstanceStatus.Store(originalProbe) })

	d.syncProfile(profile)
	d.syncProfile(profile)

	probeMu.Lock()
	defer probeMu.Unlock()
	if probes != 2 {
		t.Fatalf("expected two probes, got %d", probes)
	}
	if second != first {
		t.Fatalf("hot polling state did not survive reload:\nfirst:  %+v\nsecond: %+v", first, second)
	}
}

func TestSyncProfile_ReusesTmuxRuntimeCachesAcrossReload(t *testing.T) {
	const profile = "_test_transition_tmux_runtime_cache"
	d, storage := bootstrapDaemonProfile(t, profile)

	projectPath := filepath.Join(os.Getenv("HOME"), "tmux-runtime-cache-project")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	inst := &Instance{
		ID:          "tmux-runtime-cache-instance",
		Title:       "tmux-runtime-cache",
		ProjectPath: projectPath,
		GroupPath:   DefaultGroupPath,
		Command:     "codex",
		Tool:        "codex",
		Status:      StatusIdle,
		CreatedAt:   time.Now().Add(-time.Hour),
		tmuxSession: &tmux.Session{
			Name:        "agentdeck_tmux_runtime_cache",
			DisplayName: "tmux-runtime-cache",
			WorkDir:     projectPath,
			Command:     "codex",
			InstanceID:  "tmux-runtime-cache-instance",
		},
	}
	if err := storage.SaveWithGroups([]*Instance{inst}, nil); err != nil {
		t.Fatalf("save instance: %v", err)
	}

	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "tmux-runtime-calls.log")
	fakeTmux := filepath.Join(binDir, "tmux")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$TMUX_TEST_LOG"
case " $* " in
  *" show-environment "*) printf '%s\n' 'CODEX_SESSION_ID=11111111-1111-1111-1111-111111111111' ;;
  *" capture-pane "*) printf '%s\n' 'cached pane content' ;;
  *' #{pane_dead} '*) printf '%s\n' '0' ;;
  *' #{window_activity} '*) printf '%s\n' '12345' ;;
esac
`
	if err := os.WriteFile(fakeTmux, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("TMUX_TEST_LOG", logPath)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	originalProbe := updateInstanceStatus.Load().(statusProbeFunc)
	updateInstanceStatus.Store(statusProbeFunc(func(inst *Instance) error {
		if _, err := inst.tmuxSession.GetEnvironment("CODEX_SESSION_ID"); err != nil {
			return err
		}
		_, err := inst.tmuxSession.GetStatus()
		return err
	}))
	t.Cleanup(func() { updateInstanceStatus.Store(originalProbe) })

	d.syncProfile(profile)
	// Let CapturePane's 500ms content cache expire. The second pass can avoid a
	// fresh capture only if the reloaded Instance retained the tmux status
	// tracker, which knows the window activity timestamp is unchanged.
	time.Sleep(1100 * time.Millisecond)
	d.syncProfile(profile)

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read fake tmux log: %v", err)
	}
	var showEnvironment, capturePane int
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.Contains(line, "show-environment") {
			showEnvironment++
		}
		if strings.Contains(line, "capture-pane") {
			capturePane++
		}
	}
	if showEnvironment != 1 || capturePane != 1 {
		t.Fatalf("tmux runtime caches must survive storage reload; show-environment=%d capture-pane=%d calls=%q",
			showEnvironment, capturePane, strings.TrimSpace(string(data)))
	}
}

func TestRestorePollingState_InvalidatesRestartAndRebind(t *testing.T) {
	createdAt := time.Unix(1_700_000_000, 0).UTC()
	startedAt := createdAt.Add(time.Hour)
	oldWrapper := &tmux.Session{
		Name:        "agentdeck_poll_identity",
		DisplayName: "poll-identity",
		WorkDir:     "/tmp/poll-identity",
		Command:     "codex",
		InstanceID:  "poll-identity",
	}
	old := &Instance{
		ID:             "poll-identity",
		Title:          "poll-identity",
		ProjectPath:    "/tmp/poll-identity",
		Command:        "codex",
		Tool:           "codex",
		CreatedAt:      createdAt,
		LastStartedAt:  startedAt,
		CodexSessionID: "11111111-1111-1111-1111-111111111111",
		tmuxSession:    oldWrapper,
	}
	seedHotPollState(old, startedAt.Add(time.Minute))
	state := old.pollingState()

	for _, tc := range []struct {
		name          string
		lastStartedAt time.Time
		sessionID     string
	}{
		{
			name:          "restart generation changed",
			lastStartedAt: startedAt.Add(time.Minute),
			sessionID:     old.CodexSessionID,
		},
		{
			name:          "durable binding changed",
			lastStartedAt: startedAt,
			sessionID:     "22222222-2222-2222-2222-222222222222",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			freshWrapper := &tmux.Session{
				Name:        oldWrapper.Name,
				DisplayName: oldWrapper.DisplayName,
				WorkDir:     oldWrapper.WorkDir,
				Command:     oldWrapper.Command,
				InstanceID:  oldWrapper.InstanceID,
			}
			current := &Instance{
				ID:             old.ID,
				Title:          old.Title,
				ProjectPath:    old.ProjectPath,
				Command:        old.Command,
				Tool:           old.Tool,
				CreatedAt:      old.CreatedAt,
				LastStartedAt:  tc.lastStartedAt,
				CodexSessionID: tc.sessionID,
				tmuxSession:    freshWrapper,
			}

			current.restorePollingState(state)

			if current.tmuxSession != freshWrapper {
				t.Fatal("stale tmux wrapper crossed a restart/rebind identity boundary")
			}
			if !current.lastCodexProbeAt.IsZero() || !current.lastSessionMetaSync.IsZero() || current.tmuxFlipFromRunningPending {
				t.Fatalf("stale poll backoff crossed a restart/rebind identity boundary: %+v", snapshotHotPollState(current))
			}
		})
	}
}

func TestStorage_RoundTripsPollingRuntimeGeneration(t *testing.T) {
	const profile = "_test_transition_runtime_generation"
	_, storage := bootstrapDaemonProfile(t, profile)
	startedAt := time.Unix(1_800_000_000, 123_456_789).UTC()
	inst := &Instance{
		ID:            "runtime-generation",
		Title:         "runtime-generation",
		ProjectPath:   t.TempDir(),
		GroupPath:     DefaultGroupPath,
		Command:       "codex",
		Tool:          "codex",
		Status:        StatusRunning,
		CreatedAt:     startedAt.Add(-time.Hour),
		LastStartedAt: startedAt,
	}
	if err := storage.SaveWithGroups([]*Instance{inst}, nil); err != nil {
		t.Fatalf("save instance: %v", err)
	}

	loaded, _, err := storage.LoadWithGroups()
	if err != nil {
		t.Fatalf("reload instance: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("loaded %d instances, want 1", len(loaded))
	}
	if !loaded[0].LastStartedAt.Equal(startedAt) {
		t.Fatalf("LastStartedAt did not survive storage reload: got %s, want %s", loaded[0].LastStartedAt, startedAt)
	}
}

func TestUpdateStatus_KnownCodexSkipsFleetEnumeration(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("AGENT_DECK_HOME", "")
	t.Setenv("AGENT_DECK_PROFILE", "")
	ClearUserConfigCache()
	t.Cleanup(ClearUserConfigCache)

	const (
		name      = "agentdeck_known_codex_budget"
		sessionID = "11111111-1111-1111-1111-111111111111"
	)
	projectPath := filepath.Join(home, "known-codex-budget")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	inst := &Instance{
		ID:               "known-codex-budget",
		Title:            "known-codex-budget",
		ProjectPath:      projectPath,
		GroupPath:        DefaultGroupPath,
		Command:          "codex",
		Tool:             "codex",
		Status:           StatusRunning,
		CreatedAt:        time.Now().Add(-time.Hour),
		CodexSessionID:   sessionID,
		lastCodexProbeAt: time.Now(),
		tmuxSession: &tmux.Session{
			Name:        name,
			DisplayName: "known-codex-budget",
			WorkDir:     projectPath,
			Command:     "codex",
			InstanceID:  "known-codex-budget",
		},
	}
	tmux.SeedPaneInfoCacheForTest(t, map[string]tmux.PaneInfo{
		name:                                 {Title: "⠋ Working", CurrentCommand: "codex"},
		"agentdeck_unknown_codex_budget_two": {Title: "⠋ Working", CurrentCommand: "codex"},
	})

	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "known-codex-budget.log")
	fakeTmux := filepath.Join(binDir, "tmux")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$TMUX_TEST_LOG"
case " $* " in
  *" list-sessions "*) printf '%s\n' 'agentdeck_known_codex_budget' 'agentdeck_other_codex' ;;
  *" show-environment "*) printf '%s\n' 'CODEX_SESSION_ID=11111111-1111-1111-1111-111111111111' ;;
esac
`
	if err := os.WriteFile(fakeTmux, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("TMUX_TEST_LOG", logPath)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if err := inst.UpdateStatus(); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read fake tmux log: %v", err)
	}
	var listSessions int
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.Contains(line, "list-sessions") {
			listSessions++
		}
	}
	if listSessions != 0 {
		t.Fatalf("known Codex status refresh must not enumerate the fleet; list-sessions=%d calls=%q",
			listSessions, strings.TrimSpace(string(data)))
	}
}

func TestUpdateStatus_UnknownCodexFastPollSubprocessBudget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("AGENT_DECK_HOME", "")
	t.Setenv("AGENT_DECK_PROFILE", "")
	ClearUserConfigCache()
	t.Cleanup(ClearUserConfigCache)

	const name = "agentdeck_unknown_codex_budget"
	projectPath := filepath.Join(home, "unknown-codex-budget")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	inst := &Instance{
		ID:              "unknown-codex-budget",
		Title:           "unknown-codex-budget",
		ProjectPath:     projectPath,
		GroupPath:       DefaultGroupPath,
		Command:         "codex",
		Tool:            "codex",
		Status:          StatusRunning,
		CreatedAt:       time.Now().Add(-time.Hour),
		lastCodexScanAt: time.Now(),
		tmuxSession: &tmux.Session{
			Name:        name,
			DisplayName: "unknown-codex-budget",
			WorkDir:     projectPath,
			Command:     "codex",
			InstanceID:  "unknown-codex-budget",
		},
	}
	tmux.SeedPaneInfoCacheForTest(t, map[string]tmux.PaneInfo{
		name: {Title: "⠋ Working", CurrentCommand: "codex"},
	})

	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "unknown-codex-budget.log")
	fakeTmux := filepath.Join(binDir, "tmux")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$TMUX_TEST_LOG"
case " $* " in
  *" has-session "*) exit 0 ;;
	*" list-sessions "*) printf '%s\n' 'agentdeck_unknown_codex_budget' 'agentdeck_unknown_codex_budget_two' 'agentdeck_other_codex_a' 'agentdeck_other_codex_b' ;;
  *" show-environment -t agentdeck_other_codex_a "*) printf '%s\n' 'CODEX_SESSION_ID=11111111-1111-1111-1111-111111111111' ;;
  *" show-environment -t agentdeck_other_codex_b "*) printf '%s\n' 'CODEX_SESSION_ID=22222222-2222-2222-2222-222222222222' ;;
  *" list-panes "*) printf '%s\n' '999999' ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(fakeTmux, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("TMUX_TEST_LOG", logPath)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	for poll := 0; poll < 2; poll++ {
		inst.lastSessionMetaSync = time.Time{}
		if err := inst.UpdateStatus(); err != nil {
			t.Fatalf("UpdateStatus poll %d: %v", poll+1, err)
		}
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read fake tmux log: %v", err)
	}
	var otherEnvReads int
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.Contains(line, "show-environment -t agentdeck_other_codex_") {
			otherEnvReads++
		}
	}
	if otherEnvReads != 0 {
		t.Fatalf("unknown Codex fast polls must defer fleet exclusions until the fallback scan is due; other-env reads=%d calls=%q",
			otherEnvReads, strings.TrimSpace(string(data)))
	}

	second := &Instance{
		ID:          "unknown-codex-budget-two",
		Title:       "unknown-codex-budget-two",
		ProjectPath: projectPath,
		GroupPath:   DefaultGroupPath,
		Command:     "codex",
		Tool:        "codex",
		Status:      StatusRunning,
		CreatedAt:   time.Now().Add(-time.Hour),
		tmuxSession: &tmux.Session{
			Name:        "agentdeck_unknown_codex_budget_two",
			DisplayName: "unknown-codex-budget-two",
			WorkDir:     projectPath,
			Command:     "codex",
			InstanceID:  "unknown-codex-budget-two",
		},
	}
	inst.lastCodexScanAt = time.Time{}
	inst.lastCodexProbeAt = time.Now()
	inst.lastSessionMetaSync = time.Time{}
	second.lastCodexProbeAt = time.Now()
	if err := inst.UpdateStatus(); err != nil {
		t.Fatalf("UpdateStatus first due fallback: %v", err)
	}
	if err := second.UpdateStatus(); err != nil {
		t.Fatalf("UpdateStatus second due fallback: %v", err)
	}

	data, err = os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read fake tmux log after due fallbacks: %v", err)
	}
	var ownershipSnapshots int
	otherEnvReads = 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.Contains(line, "list-sessions") {
			ownershipSnapshots++
		}
		if strings.Contains(line, "show-environment -t agentdeck_other_codex_") {
			otherEnvReads++
		}
	}
	if ownershipSnapshots != 1 || otherEnvReads != 2 {
		t.Fatalf("same-pass Codex fallbacks must share one ownership snapshot; snapshots=%d other-env reads=%d calls=%q",
			ownershipSnapshots, otherEnvReads, strings.TrimSpace(string(data)))
	}
}

func TestShouldRunCodexProcessProbeSteadyStateBackoff(t *testing.T) {
	t.Run("bootstrap keeps fast interval while ID unknown", func(t *testing.T) {
		inst := &Instance{}
		inst.lastCodexProbeAt = time.Now().Add(-codexBootstrapScanInterval - time.Second)
		if !inst.shouldRunCodexProcessProbe(false) {
			t.Fatal("expected probe to run at fast cadence while session ID is unknown")
		}
	})

	t.Run("known ID backs off to rotation interval", func(t *testing.T) {
		inst := &Instance{CodexSessionID: "11111111-1111-1111-1111-111111111111"}
		inst.lastCodexProbeAt = time.Now().Add(-codexBootstrapScanInterval - time.Second)
		if inst.shouldRunCodexProcessProbe(false) {
			t.Fatal("known Codex ID should suppress the process-file probe until the rotation interval")
		}

		inst.lastCodexProbeAt = time.Now().Add(-codexRotationScanInterval - time.Second)
		if !inst.shouldRunCodexProcessProbe(false) {
			t.Fatal("past the rotation interval, the process-file probe should run")
		}
	})

	t.Run("force bypasses backoff", func(t *testing.T) {
		inst := &Instance{CodexSessionID: "11111111-1111-1111-1111-111111111111"}
		inst.lastCodexProbeAt = time.Now()
		if !inst.shouldRunCodexProcessProbe(true) {
			t.Fatal("force=true must always probe")
		}
	})
}
