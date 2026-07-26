package session

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

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
