package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// Tests of `conductor teardown` against agents and files that are not Agent
// Deck's. The conductor they tear down has only a meta.json: with no session row
// there is no runtime action, no tmux and no service-unit path to take.

// externalTeardownHome points HOME at a new temp dir, makes the conductor ops in
// it, and returns the dir.
func externalTeardownHome(t *testing.T, heartbeat bool) string {
	t.Helper()
	sandboxConductorHome(t)
	if err := session.SaveConductorMeta(&session.ConductorMeta{Name: "ops", Profile: "default", HeartbeatEnabled: heartbeat}); err != nil {
		t.Fatal(err)
	}
	return os.Getenv("HOME")
}

// teardownAgent is a service that teardown handles: its label as the host's
// service manager knows it, the file that defines it, and the mutating calls
// that stopping it makes when Agent Deck owns it.
type teardownAgent struct {
	label, file string
	stop        []string
}

// teardownAgents returns the bridge, the transition notifier and the heartbeat
// of the conductor name on this host: launchd agents on macOS, systemd units on
// Linux. Call it after externalTeardownHome, so that the session path functions
// resolve under the test's HOME.
func teardownAgents(t *testing.T, name string) (bridge, notifier, heartbeat teardownAgent) {
	t.Helper()
	must := func(path string, err error) string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return path
	}
	switch runtime.GOOS {
	case "darwin":
		bridge = teardownAgent{label: session.LaunchdPlistName, file: must(session.LaunchdPlistPath())}
		notifier = teardownAgent{label: session.TransitionNotifierLaunchdPlistName, file: must(session.TransitionNotifierLaunchdPlistPath())}
		heartbeat = teardownAgent{label: session.HeartbeatPlistLabel(name), file: must(session.HeartbeatPlistPath(name))}
		for _, a := range []*teardownAgent{&bridge, &notifier} {
			a.stop = []string{"launchctl unload " + a.file}
		}
	case "linux":
		bridge = teardownAgent{label: "agent-deck-conductor-bridge.service", file: must(session.SystemdBridgeServicePath())}
		notifier = teardownAgent{label: "agent-deck-transition-notifier.service", file: must(session.SystemdTransitionNotifierServicePath())}
		heartbeat = teardownAgent{label: session.SystemdHeartbeatTimerName(name), file: must(session.SystemdHeartbeatTimerPath(name))}
		for _, a := range []*teardownAgent{&bridge, &notifier} {
			a.stop = []string{"systemctl --user disable --now " + a.label, "systemctl --user daemon-reload"}
		}
	default:
		t.Skipf("conductor teardown stops launchd agents or systemd units; %s has neither", runtime.GOOS)
	}
	return bridge, notifier, heartbeat
}

// teardownFileState describes what is at path: "missing" when nothing is, else
// its mode and its bytes. A file that is in the same state after a run was kept.
func teardownFileState(t *testing.T, path string) string {
	t.Helper()
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return "missing"
	}
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return fmt.Sprintf("%v %q", info.Mode(), content)
}

// plantTeardownAgent writes the definition of agent a at mode, as another
// manager leaves it, and returns its state. The binary looks for it at a.file,
// where the helper must have put it.
func plantTeardownAgent(t *testing.T, home string, a teardownAgent, mode os.FileMode) string {
	t.Helper()
	writeReadOnlyServiceFixtures(t, home, mode, a.label)
	state := teardownFileState(t, a.file)
	if state == "missing" {
		t.Fatalf("the fixture of %s is not at %s, where the binary looks for it", a.label, a.file)
	}
	return state
}

// teardownAllWithFakes runs `conductor teardown --all` with the recording
// launchctl and systemctl of pathDir first on PATH, and fails the test unless it
// exits 0.
func teardownAllWithFakes(t *testing.T, home, pathDir string) (stdout, stderr string) {
	t.Helper()
	env := []string{"PATH=" + pathDir + string(os.PathListSeparator) + os.Getenv("PATH")}
	stdout, stderr, code := runAgentDeckEnv(t, home, "", env, "conductor", "teardown", "--all")
	if code != 0 {
		t.Fatalf("teardown --all: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	return stdout, stderr
}

// teardown --all stops the bridge daemon and the transition notifier daemon. One
// whose plist or unit another manager owns (read-only here, as home-manager
// leaves it) is left as it is and reported [skip], not "stopped and removed";
// the other is stopped as it always was.
func TestConductorTeardownExternal_AllSkipsExternalAgents(t *testing.T) {
	const (
		bridgeOK   = "[ok] Daemon stopped and removed"
		notifierOK = "[ok] Transition notifier daemon stopped and removed"
	)
	skip := func(a teardownAgent) string { return "[skip] " + a.label + ": managed externally (read-only)" }
	rows := []struct {
		name             string
		bridge, notifier os.FileMode
		lines            func(bridge, notifier teardownAgent) []string // whole stdout lines the run prints
		stopped          int                                           // agents it reports "stopped and removed"
		calls            func(bridge, notifier teardownAgent) []string // the mutating service calls it makes
	}{
		{
			name: "both-external", bridge: 0o444, notifier: 0o444,
			lines:   func(bridge, notifier teardownAgent) []string { return []string{skip(bridge), skip(notifier)} },
			stopped: 0,
			calls:   func(bridge, notifier teardownAgent) []string { return nil },
		},
		{
			name: "mixed-ownership", bridge: 0o444, notifier: 0o644,
			lines:   func(bridge, notifier teardownAgent) []string { return []string{skip(bridge), notifierOK} },
			stopped: 1,
			calls:   func(bridge, notifier teardownAgent) []string { return notifier.stop },
		},
		{
			name: "reverse-mix", bridge: 0o644, notifier: 0o444,
			lines:   func(bridge, notifier teardownAgent) []string { return []string{bridgeOK, skip(notifier)} },
			stopped: 1,
			calls:   func(bridge, notifier teardownAgent) []string { return bridge.stop },
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			home := externalTeardownHome(t, false)
			bridge, notifier, _ := teardownAgents(t, "ops")
			pathDir, logPath := installRecordingServiceFakes(t, true)
			agents := []teardownAgent{bridge, notifier}
			modes := []os.FileMode{row.bridge, row.notifier}
			before := make([]string, len(agents))
			for i, a := range agents {
				before[i] = plantTeardownAgent(t, home, a, modes[i])
			}

			stdout, _ := teardownAllWithFakes(t, home, pathDir)

			lines := strings.Split(stdout, "\n")
			for _, want := range row.lines(bridge, notifier) {
				if !slices.Contains(lines, want) {
					t.Errorf("missing line %q in:\n%s", want, stdout)
				}
			}
			if got := strings.Count(stdout, "stopped and removed"); got != row.stopped {
				t.Errorf("%d agents reported stopped and removed, want %d", got, row.stopped)
			}
			if got, want := mutatingServiceCalls(t, logPath), row.calls(bridge, notifier); !slices.Equal(got, want) {
				t.Errorf("mutating service calls = %q, want %q", got, want)
			}
			for i, a := range agents {
				after := teardownFileState(t, a.file)
				switch {
				case modes[i] == 0o444 && after != before[i]:
					t.Errorf("%s is not Agent Deck's, but it changed:\n got: %s\nwant: %s", a.label, after, before[i])
				case modes[i] == 0o644 && after != "missing":
					t.Errorf("%s is Agent Deck's, and must be removed: it is %s", a.label, after)
				}
			}
		})
	}
}

// A stop that fails is a warning on stderr, never "stopped and removed". Here
// each agent's file is a directory that holds something, which no manager owns
// and agent-deck will not take for a service file: on both platforms the stop
// fails before any launchctl or systemctl call.
func TestConductorTeardownExternal_AllWarnsWhenStopFails(t *testing.T) {
	home := externalTeardownHome(t, false)
	bridge, notifier, _ := teardownAgents(t, "ops")
	pathDir, logPath := installRecordingServiceFakes(t, true)
	for _, a := range []teardownAgent{bridge, notifier} {
		if err := os.MkdirAll(filepath.Join(a.file, "keep"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	stdout, stderr := teardownAllWithFakes(t, home, pathDir)

	if got := strings.Count(stdout, "stopped and removed"); got != 0 {
		t.Errorf("%d agents reported stopped and removed, want 0", got)
	}
	if calls := mutatingServiceCalls(t, logPath); len(calls) != 0 {
		t.Errorf("a stop that failed still changed a service manager: %q", calls)
	}
	for _, want := range []string{
		"Warning: failed to stop bridge daemon: refusing to manage " + bridge.label,
		"Warning: failed to stop transition notifier daemon: refusing to manage " + notifier.label,
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
}

// Turning off the heartbeat of a conductor whose heartbeat agent another manager
// owns is not a failure: the agent stays, the teardown says so, and meta.json
// records heartbeat_enabled=false. With --json the skip is not printed, so
// stdout stays one JSON object.
func TestConductorTeardownExternal_HeartbeatSkipTurnsOffMeta(t *testing.T) {
	home := externalTeardownHome(t, true)
	_, _, heartbeat := teardownAgents(t, "ops")
	pathDir, logPath := installRecordingServiceFakes(t, false)
	// On Linux the timer alone: one external unit makes the pair external.
	before := plantTeardownAgent(t, home, heartbeat, 0o444)
	env := []string{"PATH=" + pathDir + string(os.PathListSeparator) + os.Getenv("PATH")}

	want := "  [skip] " + heartbeat.label + ": managed externally (read-only); heartbeat turned off in meta.json"
	stdout, stderr, code := runAgentDeckEnv(t, home, "", env, "conductor", "teardown", "ops")
	if code != 0 || !slices.Contains(strings.Split(stdout, "\n"), want) {
		t.Fatalf("teardown ops: exit=%d, want line %q\nstdout=%q stderr=%q", code, want, stdout, stderr)
	}
	meta, err := session.LoadConductorMeta("ops")
	if err != nil || meta.HeartbeatEnabled {
		t.Fatalf("heartbeat must be off in meta.json: meta=%+v err=%v", meta, err)
	}

	stdout, stderr, code = runAgentDeckEnv(t, home, "", env, "conductor", "teardown", "ops", "--json")
	var result struct {
		Success bool `json:"success"`
	}
	if err := json.Unmarshal([]byte(stdout), &result); code != 0 || err != nil || !result.Success {
		t.Fatalf("teardown ops --json: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

	if after := teardownFileState(t, heartbeat.file); after != before {
		t.Errorf("%s is not Agent Deck's, but it changed:\n got: %s\nwant: %s", heartbeat.label, after, before)
	}
	if calls := mutatingServiceCalls(t, logPath); len(calls) != 0 {
		t.Errorf("teardown changed a service manager for an external heartbeat: %q", calls)
	}
}

// An owned heartbeat agent that cannot be stopped is a failure, not a skip: the
// teardown exits 1 with the error before it turns the heartbeat off or removes
// anything. These runs have no PATH fakes, so TestMain's failing launchctl and
// systemctl make the strict unload or `disable --now` fail on both platforms.
// The --remove --json run is where a teardown that went on after the failure
// would do harm: it would delete the directory of a conductor whose heartbeat
// may still be running, and report success.
func TestConductorTeardownExternal_HeartbeatStopFailureExits(t *testing.T) {
	rows := []struct {
		name string
		args []string
	}{
		{"text", []string{"conductor", "teardown", "ops"}},
		{"remove-json", []string{"conductor", "teardown", "ops", "--remove", "--json"}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			home := externalTeardownHome(t, true)
			_, _, heartbeat := teardownAgents(t, "ops")
			before := plantTeardownAgent(t, home, heartbeat, 0o644)
			dir, err := session.ConductorNameDir("ops")
			if err != nil {
				t.Fatal(err)
			}

			stdout, stderr, code := runAgentDeck(t, home, row.args...)

			if code != 1 {
				t.Errorf("teardown %s: exit=%d, want 1\nstdout=%q stderr=%q", strings.Join(row.args[2:], " "), code, stdout, stderr)
			}
			if want := "Error disabling heartbeat for ops:"; !strings.Contains(stderr, want) {
				t.Errorf("stderr lacks %q:\n%s", want, stderr)
			}
			if meta, err := session.LoadConductorMeta("ops"); err != nil || !meta.HeartbeatEnabled {
				t.Errorf("a heartbeat that could not be stopped must stay on in meta.json: meta=%+v err=%v", meta, err)
			}
			if after := teardownFileState(t, heartbeat.file); after != before {
				t.Errorf("%s could not be stopped, but it changed:\n got: %s\nwant: %s", heartbeat.label, after, before)
			}
			if _, err := os.Stat(dir); err != nil {
				t.Errorf("the conductor directory must still exist: %v", err)
			}
		})
	}
}

// teardown --all --remove deletes bridge.py, with its stamp and
// .bridge.py.previous, only when agent-deck owns the bridge: it is the shipped
// one or still matches its stamp. A bridge the user changed, or a link to one,
// is kept and the teardown says so; bridge.py.backup is never touched. A removal
// that fails part way is a warning on stderr, and the exit is still 0. These runs
// have no PATH fakes: TestMain's failing launchctl and systemctl make the bridge
// daemon read as not running, so the --all stop never runs.
func TestConductorTeardownExternal_RemoveKeepsCustomizedBridge(t *testing.T) {
	const (
		custom      = "#!/usr/bin/env python3\n# hand-edited bridge\n"
		backup      = "# the user's own bridge\n"
		skipChanged = "[skip] bridge.py has local changes; kept"
		skipLink    = "[skip] bridge.py is a symlink; kept"
		warning     = "Warning: failed to remove bridge.py:"
	)
	stamp := fmt.Sprintf("%x\n", sha256.Sum256([]byte(custom)))
	type file struct{ name, content string }
	backupFile := file{"bridge.py.backup", backup}
	rows := []struct {
		name  string
		files []file   // written to the conductor directory
		link  bool     // and bridge.py is a link to <home>/my-bridge.py, which holds custom
		dirs  []string // and each of these is a directory holding a file, which os.Remove cannot remove
		line  string   // the [skip] line the run prints; "" for neither
		warns bool     // whether it warns on stderr that it failed to remove bridge.py
		gone  []string // the files the run removes; every other file is left as it was
	}{
		{
			name:  "customized",
			files: []file{{"bridge.py", custom}, backupFile},
			line:  skipChanged,
		},
		{
			name:  "stamped",
			files: []file{{"bridge.py", custom}, backupFile, {".bridge.py.sha256", stamp}, {".bridge.py.previous", "# previous bridge\n"}},
			gone:  []string{"bridge.py", ".bridge.py.sha256", ".bridge.py.previous"},
		},
		{
			name:  "symlink",
			files: []file{backupFile, {".bridge.py.sha256", stamp}},
			link:  true,
			line:  skipLink,
		},
		{
			name:  "absent",
			files: []file{backupFile},
		},
		{
			// bridge.py and the stamp go, then removing .bridge.py.previous fails.
			name:  "previous cannot be removed",
			files: []file{{"bridge.py", custom}, backupFile, {".bridge.py.sha256", stamp}},
			dirs:  []string{".bridge.py.previous"},
			warns: true,
			gone:  []string{"bridge.py", ".bridge.py.sha256"},
		},
	}
	// held is what path holds now: the target of a link, else its bytes.
	held := func(path string) (string, error) {
		if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
			return os.Readlink(path)
		}
		content, err := os.ReadFile(path)
		return string(content), err
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			home := externalTeardownHome(t, false)
			dir, err := session.ConductorDir()
			if err != nil {
				t.Fatal(err)
			}
			write := func(path, content string) {
				t.Helper()
				if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			// What the run must leave as it is, and what each path holds: its bytes,
			// or for a link its target.
			kept := map[string]string{}
			for _, f := range row.files {
				write(filepath.Join(dir, f.name), f.content)
				if !slices.Contains(row.gone, f.name) {
					kept[filepath.Join(dir, f.name)] = f.content
				}
			}
			if row.link {
				mine, link := filepath.Join(home, "my-bridge.py"), filepath.Join(dir, "bridge.py")
				write(mine, custom)
				if err := os.Symlink(mine, link); err != nil {
					t.Fatal(err)
				}
				kept[mine], kept[link] = custom, mine
			}
			for _, name := range row.dirs {
				if err := os.MkdirAll(filepath.Join(dir, name, "keep"), 0o755); err != nil {
					t.Fatal(err)
				}
			}

			stdout, stderr, code := runAgentDeck(t, home, "conductor", "teardown", "--all", "--remove")
			if code != 0 {
				t.Fatalf("teardown --all --remove: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
			}

			lines := strings.Split(stdout, "\n")
			for _, line := range []string{skipChanged, skipLink} {
				if printed, want := slices.Contains(lines, line), line == row.line; printed != want {
					t.Errorf("%q printed=%v, want %v", line, printed, want)
				}
			}
			if warned := strings.Contains(stderr, warning); warned != row.warns {
				t.Errorf("stderr has %q = %v, want %v:\n%s", warning, warned, row.warns, stderr)
			}
			for _, name := range row.gone {
				if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
					t.Errorf("%s must be removed: err=%v", name, err)
				}
			}
			for path, want := range kept {
				if got, err := held(path); err != nil || got != want {
					t.Errorf("%s must be left unchanged: %q err=%v", filepath.Base(path), got, err)
				}
			}
		})
	}
}
