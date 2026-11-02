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

	"al.essio.dev/pkg/shellescape"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

const (
	// setupCLIDiscordConfig configures one chat channel, so setup installs the
	// bridge: a non-empty bot_token is all discordConfigured checks.
	setupCLIDiscordConfig = "[conductor.discord]\nbot_token = \"dummy\"\n"
	// The systemd units of the bridge and the transition notifier. session keeps
	// their names unexported.
	setupCLIBridgeUnit   = "agent-deck-conductor-bridge.service"
	setupCLINotifierUnit = "agent-deck-transition-notifier.service"
)

// setupCLIServiceLabels returns the labels of the bridge and notifier agents on
// this host, as `conductor setup` prints them: launchd labels on macOS, systemd
// units elsewhere.
func setupCLIServiceLabels() (bridge, notifier string) {
	if runtime.GOOS == "darwin" {
		return session.LaunchdPlistName, session.TransitionNotifierLaunchdPlistName
	}
	return setupCLIBridgeUnit, setupCLINotifierUnit
}

// setupCLIModes are the two output modes of `conductor setup`: text and --json.
var setupCLIModes = []struct {
	name string
	json bool
}{{"text", false}, {"json", true}}

// setupCLIFixture is a temp HOME to run `conductor setup` in as a subprocess,
// with nothing a real tool could answer: the venv python that setup prefers logs
// and, unless a test says otherwise (writeVenvPython), fails, so no pip runs on
// the host, and recording launchctl and systemctl fakes come first on PATH.
type setupCLIFixture struct {
	home       string
	condDir    string // the conductor base directory under home
	pythonLog  string // one line per run of the venv python
	serviceLog string // one line per launchctl or systemctl call
	env        []string
	want       map[string]string // path -> the bytes it must still hold
}

// newSetupCLIFixture makes a fixture whose config.toml holds config.
func newSetupCLIFixture(t *testing.T, config string) *setupCLIFixture {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("conductor setup installs launchd agents or systemd units; this platform has neither")
	}
	home := t.TempDir()
	writeAddUserConfig(t, home, config)
	// ConductorDir lies under agentDeckTestEnv's XDG_DATA_HOME. agent-deck keeps
	// its state root private and says on stderr when it finds it looser, so the
	// fixture creates it as agent-deck does, 0700.
	dataDir := filepath.Join(home, ".local", "share", "agent-deck")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}

	pathDir, serviceLog := installRecordingServiceFakes(t, false)
	fx := &setupCLIFixture{
		home:       home,
		condDir:    filepath.Join(dataDir, "conductor"),
		pythonLog:  filepath.Join(t.TempDir(), "python.log"),
		serviceLog: serviceLog,
		env:        []string{"PATH=" + pathDir + string(os.PathListSeparator) + os.Getenv("PATH")},
		want:       map[string]string{},
	}
	fx.writeVenvPython(t, 1)
	return fx
}

// writeVenvPython writes the venv python3, which findPython3 prefers to the
// host's: it appends its arguments to pythonLog and exits with code, so setup
// sees pip fail (1) or succeed (0) and no real interpreter runs.
func (fx *setupCLIFixture) writeVenvPython(t *testing.T, code int) {
	t.Helper()
	venvPython := filepath.Join(fx.condDir, "venv", "bin", "python3")
	if err := os.MkdirAll(filepath.Dir(venvPython), 0o755); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> %s\nexit %d\n", shellescape.Quote(fx.pythonLog), code)
	if err := os.WriteFile(venvPython, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// keep records the current bytes of each file, for assertUnchanged.
func (fx *setupCLIFixture) keep(t *testing.T, paths ...string) {
	t.Helper()
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s to keep it: %v", path, err)
		}
		fx.want[path] = string(data)
	}
}

// writeUserFile writes an executable file of the user's and keeps it.
func (fx *setupCLIFixture) writeUserFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	fx.keep(t, path)
}

// assertUnchanged fails for each kept file that no longer holds its bytes.
func (fx *setupCLIFixture) assertUnchanged(t *testing.T) {
	t.Helper()
	for path, want := range fx.want {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("reading %s: %v", path, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%s changed:\n got: %q\nwant: %q", path, got, want)
		}
	}
}

// run runs the agent-deck binary with the fixture's HOME and PATH, and fails the
// test unless it exits 0.
func (fx *setupCLIFixture) run(t *testing.T, args ...string) (stdout, stderr string) {
	t.Helper()
	stdout, stderr, code := runAgentDeckEnv(t, fx.home, "", fx.env, args...)
	if code != 0 {
		t.Fatalf("agent-deck %s: exit %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), code, stdout, stderr)
	}
	return stdout, stderr
}

// assertStdoutHasLine fails unless stdout has line as a whole line.
func assertStdoutHasLine(t *testing.T, stdout, line string) {
	t.Helper()
	if !slices.Contains(strings.Split(stdout, "\n"), line) {
		t.Errorf("stdout lacks the line %q\nstdout:\n%s", line, stdout)
	}
}

// setupCLIJSON decodes the stdout of a --json run into its top-level keys.
func setupCLIJSON(t *testing.T, stdout string) map[string]json.RawMessage {
	t.Helper()
	var out map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("stdout is not one JSON object: %v\nstdout: %s", err, stdout)
	}
	return out
}

// hera's case: the bridge is the director's own, and home-manager owns the
// read-only bridge and notifier agents. Setup must change none of it, run no
// pip for the bridge, and say what it left alone.
func TestConductorSetupCLI_ExternalBridgeAndAgentsUntouched(t *testing.T) {
	bridgeLabel, notifierLabel := setupCLIServiceLabels()
	for _, mode := range setupCLIModes {
		t.Run(mode.name, func(t *testing.T) {
			fx := newSetupCLIFixture(t, setupCLIDiscordConfig)
			fx.writeUserFile(t, filepath.Join(fx.condDir, "bridge.py"), "# bridge.py with the director's Discord Q&A hook\n")
			fx.keep(t, writeReadOnlyServiceFixtures(t, fx.home, 0o444,
				session.LaunchdPlistName, session.TransitionNotifierLaunchdPlistName,
				setupCLIBridgeUnit, setupCLINotifierUnit)...)

			args := []string{"conductor", "setup", "steward-probe", "--no-heartbeat"}
			if mode.json {
				args = append(args, "--json")
			}
			stdout, stderr := fx.run(t, args...)

			fx.assertUnchanged(t)
			if python, err := os.ReadFile(fx.pythonLog); err == nil && len(python) > 0 {
				t.Errorf("setup ran the venv python (pip) for an external bridge agent: %q", python)
			} else if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			for _, banned := range []string{"Warning: failed", "Tip: "} {
				if strings.Contains(stderr, banned) {
					t.Errorf("stderr contains %q:\n%s", banned, stderr)
				}
			}

			if mode.json {
				// Only the notifier_daemon_running query reaches a service manager.
				if calls := mutatingServiceCalls(t, fx.serviceLog); len(calls) != 0 {
					t.Errorf("setup changed a service manager: %q", calls)
				}
				out := setupCLIJSON(t, stdout)
				if got := string(out["bridge_script"]); got != `"kept"` {
					t.Errorf("bridge_script = %s, want %q", got, "kept")
				}
				var external []string
				wantExternal := []string{bridgeLabel, notifierLabel}
				if err := json.Unmarshal(out["external"], &external); err != nil || !slices.Equal(external, wantExternal) {
					t.Errorf("external = %s, want %q", out["external"], wantExternal)
				}
				for _, key := range []string{"daemon", "notifier_daemon"} {
					if got, ok := out[key]; ok {
						t.Errorf("JSON has %q = %s, but no daemon was installed", key, got)
					}
				}
			} else {
				// Every external check, the notifier's included, comes before any
				// launchctl or systemctl call.
				if calls := serviceCallLines(t, fx.serviceLog); len(calls) != 0 {
					t.Errorf("setup called the service managers: %q", calls)
				}
				for _, line := range []string{
					"[skip] bridge daemon managed externally; Python deps left to its manager",
					session.BridgeKeptNotice,
					"[skip] " + bridgeLabel + ": managed externally (read-only)",
					"[skip] " + notifierLabel + ": managed externally (read-only)",
				} {
					assertStdoutHasLine(t, stdout, line)
				}
				for _, banned := range []string{"[ok] bridge.py", "[ok] Bridge daemon loaded", "Bridge daemon NOT installed", "[ok] Transition notifier daemon installed"} {
					if strings.Contains(stdout, banned) {
						t.Errorf("stdout contains %q:\n%s", banned, stdout)
					}
				}
			}
		})
	}
}

// A fresh setup writes bridge.py and stamps it, and a rerun says what it did:
// bridge.py installed, unchanged, updated from an older bridge agent-deck wrote,
// or kept as a symlink. The rows share one conductor directory and run in order.
func TestConductorSetupCLI_FreshBridgeInstalledAndStamped(t *testing.T) {
	fx := newSetupCLIFixture(t, setupCLIDiscordConfig)
	bridge := filepath.Join(fx.condDir, "bridge.py")
	stamp := filepath.Join(fx.condDir, ".bridge.py.sha256")
	previous := filepath.Join(fx.condDir, ".bridge.py.previous")
	mine := filepath.Join(fx.home, "my-bridge.py")
	const older = "# a bridge an earlier agent-deck wrote\n"

	stampOf := func(content string) string { return fmt.Sprintf("%x\n", sha256.Sum256([]byte(content))) }
	read := func(t *testing.T, path string) string {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	write := func(t *testing.T, path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	remove := func(t *testing.T, paths ...string) {
		t.Helper()
		for _, path := range paths {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
		}
	}
	// The stamp records the sha256 of the bridge.py that is there.
	assertStamped := func(t *testing.T) {
		t.Helper()
		if got, want := read(t, stamp), stampOf(read(t, bridge)); got != want {
			t.Errorf("stamp = %q, want %q", got, want)
		}
	}

	var shipped, keptStamp string
	rows := []struct {
		name    string
		prep    func(t *testing.T)
		line    string // the text mode line
		outcome string // the bridge_script of the JSON mode
		check   func(t *testing.T)
	}{
		{
			name:    "installed",
			prep:    func(t *testing.T) { remove(t, bridge, stamp) },
			line:    "[ok] bridge.py installed",
			outcome: "installed",
			check: func(t *testing.T) {
				assertStamped(t)
				shipped = read(t, bridge)
			},
		},
		{
			name:    "unchanged",
			line:    "[ok] bridge.py unchanged",
			outcome: "unchanged",
			check: func(t *testing.T) {
				if got := read(t, bridge); got != shipped {
					t.Errorf("bridge.py changed:\n got: %q\nwant: %q", got, shipped)
				}
				assertStamped(t)
			},
		},
		{
			name: "updated",
			prep: func(t *testing.T) {
				write(t, bridge, older)
				write(t, stamp, stampOf(older))
			},
			line:    "[ok] bridge.py updated",
			outcome: "updated",
			check: func(t *testing.T) {
				if got := read(t, bridge); got != shipped {
					t.Errorf("bridge.py = %q, want the shipped bridge %q", got, shipped)
				}
				assertStamped(t)
				copies, err := filepath.Glob(filepath.Join(fx.condDir, ".bridge.py.previous*"))
				if err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(copies, []string{previous}) {
					t.Errorf("copies of the replaced bridge = %q, want only %q", copies, previous)
				} else if got := read(t, previous); got != older {
					t.Errorf("%s = %q, want the replaced bridge %q", previous, got, older)
				}
			},
		},
		{
			name: "symlink",
			prep: func(t *testing.T) {
				remove(t, bridge)
				write(t, mine, "# mine\n")
				if err := os.Symlink(mine, bridge); err != nil {
					t.Fatal(err)
				}
				keptStamp = read(t, stamp)
			},
			line:    session.BridgeSymlinkNotice,
			outcome: "kept",
			check: func(t *testing.T) {
				if target, err := os.Readlink(bridge); err != nil || target != mine {
					t.Errorf("bridge.py is a link to %q (%v), want the link to %q kept", target, err, mine)
				}
				if got := read(t, mine); got != "# mine\n" {
					t.Errorf("the link's target = %q, want it unchanged", got)
				}
				if got := read(t, stamp); got != keptStamp {
					t.Errorf("stamp = %q, want it unchanged: %q", got, keptStamp)
				}
			},
		},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			for _, mode := range setupCLIModes {
				t.Run(mode.name, func(t *testing.T) {
					if row.prep != nil {
						row.prep(t)
					}
					args := []string{"conductor", "setup", "fresh", "--no-heartbeat"}
					if mode.json {
						args = append(args, "--json")
					}
					stdout, _ := fx.run(t, args...)

					if mode.json {
						out := setupCLIJSON(t, stdout)
						if got := string(out["bridge_script"]); got != fmt.Sprintf("%q", row.outcome) {
							t.Errorf("bridge_script = %s, want %q", got, row.outcome)
						}
						if got := string(out["external"]); got != "[]" {
							t.Errorf("external = %q, want [] (never null)", got)
						}
					} else {
						assertStdoutHasLine(t, stdout, row.line)
					}
					row.check(t)
				})
			}
		})
	}
}

// A rerun of setup with the heartbeat on keeps the heartbeat.sh the director
// wrote, and leaves the heartbeat agent, read-only under home-manager, alone.
// The rerun is run twice, as text and as --json.
func TestConductorSetupCLI_RerunKeepsCustomHeartbeatScript(t *testing.T) {
	const name = "hb-custom"
	fx := newSetupCLIFixture(t, "")
	// TestMain's failing stubs stand in for the service managers on this run.
	runConductorSetupCLI(t, fx.home, name)
	fx.writeUserFile(t, filepath.Join(cliConductorDir(t, fx.home, name), "heartbeat.sh"), "#!/bin/sh\n# the director's own heartbeat\nexit 0\n")
	fx.keep(t, writeReadOnlyServiceFixtures(t, fx.home, 0o444,
		session.HeartbeatPlistLabel(name), session.SystemdHeartbeatServiceName(name), session.SystemdHeartbeatTimerName(name))...)
	hbLabel := session.SystemdHeartbeatTimerName(name)
	if runtime.GOOS == "darwin" {
		hbLabel = session.HeartbeatPlistLabel(name)
	}

	stdout, stderr := fx.run(t, "conductor", "setup", name)

	fx.assertUnchanged(t)
	assertStdoutHasLine(t, stdout, "  [skip] heartbeat.sh has local changes; left unchanged")
	assertStdoutHasLine(t, stdout, "  [skip] "+hbLabel+": managed externally (read-only)")
	if strings.Contains(stderr, "failed to install heartbeat") {
		t.Errorf("stderr reports a failed heartbeat install:\n%s", stderr)
	}

	// No chat channel is configured, so setup has no bridge step and its JSON has
	// no bridge_script; the heartbeat agent is the one agent it left alone.
	jsonOut, _ := fx.run(t, "conductor", "setup", name, "--json")

	fx.assertUnchanged(t)
	out := setupCLIJSON(t, jsonOut)
	if got, ok := out["bridge_script"]; ok {
		t.Errorf("JSON has bridge_script = %s, but no chat channel is configured", got)
	}
	var external []string
	wantExternal := []string{hbLabel}
	if err := json.Unmarshal(out["external"], &external); err != nil || !slices.Equal(external, wantExternal) {
		t.Errorf("external = %s, want %q", out["external"], wantExternal)
	}

	// Neither run changed the heartbeat agent: the calls that remain are the
	// notifier's.
	for _, call := range mutatingServiceCalls(t, fx.serviceLog) {
		if strings.Contains(call, "conductor-heartbeat") {
			t.Errorf("setup changed the external heartbeat agent: %q", call)
		}
	}
}

// A heartbeat.sh that cannot be read is neither replaced nor taken for the
// user's: setup warns that it did not install the script, and installs no
// heartbeat agent for it. The script here is a directory, which no uid can read
// as a file.
func TestConductorSetupCLI_UnreadableHeartbeatScriptWarnsAndInstallsNoDaemon(t *testing.T) {
	const name = "hb-unreadable"
	fx := newSetupCLIFixture(t, "")
	// TestMain's failing stubs stand in for the service managers on this run.
	runConductorSetupCLI(t, fx.home, name)
	script := filepath.Join(cliConductorDir(t, fx.home, name), "heartbeat.sh")
	if err := os.Remove(script); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Mkdir(script, 0o755); err != nil {
		t.Fatal(err)
	}

	_, stderr := fx.run(t, "conductor", "setup", name)

	for _, want := range []string{"Warning: failed to install heartbeat script:", "reading existing heartbeat.sh"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
	for _, call := range mutatingServiceCalls(t, fx.serviceLog) {
		if strings.Contains(call, "conductor-heartbeat") {
			t.Errorf("setup went on to the heartbeat agent: %q", call)
		}
	}
	// The agent's files are checked as well as the calls: with no systemd user
	// session an installed unit is written but never enabled, so it would leave
	// no call.
	for _, label := range []string{session.HeartbeatPlistLabel(name), session.SystemdHeartbeatServiceName(name), session.SystemdHeartbeatTimerName(name)} {
		if _, err := os.Lstat(serviceFixturePath(fx.home, label)); !os.IsNotExist(err) {
			t.Errorf("heartbeat agent %s exists after setup (Lstat: %v)", label, err)
		}
	}
	if info, err := os.Stat(script); err != nil {
		t.Errorf("heartbeat.sh: %v", err)
	} else if !info.IsDir() {
		t.Errorf("heartbeat.sh is no longer a directory (mode %v)", info.Mode())
	}
}

// A bridge agent that no other manager owns is installed: pip, the venv python,
// succeeds, so setup goes on to write the agent and load it.
func TestConductorSetupCLI_OwnedBridgeAgentInstalled(t *testing.T) {
	fx := newSetupCLIFixture(t, setupCLIDiscordConfig)
	fx.writeVenvPython(t, 0)
	bridgeLabel, _ := setupCLIServiceLabels()
	agentFile := serviceFixturePath(fx.home, bridgeLabel)

	stdout, _ := fx.run(t, "conductor", "setup", "owned", "--no-heartbeat")

	assertStdoutHasLine(t, stdout, "[ok] bridge.py installed")
	for _, banned := range []string{"[skip] bridge daemon managed externally", "Bridge daemon NOT installed"} {
		if strings.Contains(stdout, banned) {
			t.Errorf("stdout contains %q:\n%s", banned, stdout)
		}
	}
	if _, err := os.Stat(agentFile); err != nil {
		t.Errorf("setup wrote no bridge agent: %v", err)
	}
	// On Linux whether setup also enables the unit depends on the machine's
	// systemd user session, which a container may lack, so only the lines above
	// are asserted there. On macOS the launchctl fake answers the same everywhere,
	// so the load is asserted too.
	if runtime.GOOS == "darwin" {
		assertStdoutHasLine(t, stdout, "[ok] Bridge daemon loaded")
		if load := "launchctl load " + agentFile; !slices.Contains(mutatingServiceCalls(t, fx.serviceLog), load) {
			t.Errorf("service calls lack %q: %q", load, serviceCallLines(t, fx.serviceLog))
		}
	}
}
