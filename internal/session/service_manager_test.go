package session

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/platform"
)

// The classifier fails closed: only the listed query verbs of launchctl and
// systemctl are non-mutating, and only when every option before the verb is one
// of those in serviceFlagsWithoutValue. An option that takes no value but is
// not listed (systemctl's --all, --failed, --full) is refused too, and every
// other tool has no allow-list at all.
func TestIsMutatingServiceVerb(t *testing.T) {
	tests := []struct {
		name     string
		mutating bool
		cmdlines []string
	}{
		{"queries", false, []string{
			"launchctl list com.agentdeck.conductor-bridge",
			"launchctl print gui/501/com.agentdeck.conductor-bridge",
			"launchctl print-disabled gui/501",
			"launchctl managername",
			"systemctl --user is-active --quiet x.service",
			"systemctl --user is-enabled x.timer",
			"systemctl --user show -p MainPID --value x.scope",
			"systemctl cat x.service",
			"systemctl --user status x.service",
			"systemctl --user list-timers",
			"systemctl --user --no-pager status x.service", // a listed option may precede the verb
			"systemctl --system --global --quiet -q --no-pager --no-block --no-legend --now status x.service", // every listed option but --user
		}},
		{"everything else", true, []string{
			"launchctl load /p/x.plist",
			"launchctl unload /p/x.plist",
			"launchctl kill SIGTERM gui/501/x",
			"launchctl setenv K V",
			"systemctl --user enable --now x.timer",
			"systemctl --user daemon-reload",
			"systemctl --user reset-failed x.service",
			"systemctl --user set-environment K=V",
			"systemctl --user",   // no verb
			"loginctl show-user", // no other tool has an allow-list
			// Only the options in serviceFlagsWithoutValue may precede a query verb. -p and
			// -H take the next argument as their value, which serviceVerb would read as the
			// verb; any other option, such as --property=... or a bare --, is refused
			// because it is not listed.
			"systemctl --user -p status stop x.service",       // -p takes a value: the verb is stop
			"systemctl -H host status x.service",              // -H takes a value
			"systemctl --property=ActiveState show x.service", // an option outside the no-value set
			"systemctl -- status x.service",                   // a bare --
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, cmdline := range tt.cmdlines {
				f := strings.Fields(cmdline)
				if got := isMutatingServiceVerb(f[0], f[1:]); got != tt.mutating {
					t.Errorf("isMutatingServiceVerb(%q) = %v, want %v", cmdline, got, tt.mutating)
				}
			}
		})
	}
}

// A temp HOME does not isolate launchd, so in a test binary a mutating verb
// may only reach a tool that lives under serviceManagerFakeRoot(). The rows run
// in order and never start a process: they read the command's Path and Args.
func TestServiceManagerCommand_GuardsMutatingVerbsInTests(t *testing.T) {
	bin, elsewhere := t.TempDir(), t.TempDir()
	script := []byte("#!/bin/sh\nexit 0\n")
	if err := os.WriteFile(filepath.Join(bin, "launchctl"), script, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(elsewhere, "systemctl"), script, 0o755); err != nil {
		t.Fatal(err)
	}
	// A fake that sits under the root only by name: it resolves outside it.
	if err := os.Symlink(filepath.Join(elsewhere, "systemctl"), filepath.Join(bin, "systemctl")); err != nil {
		t.Fatal(err)
	}
	// Another launchctl, on the PATH that replaces bin once the guard has looked
	// the tool up.
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "launchctl"), script, 0o755); err != nil {
		t.Fatal(err)
	}
	origRoot := serviceManagerFakeRoot
	t.Cleanup(func() { serviceManagerFakeRoot = origRoot })

	const plist = "/nonexistent/com.agentdeck.conductor-bridge.plist"
	binThenOld := bin + string(os.PathListSeparator) + os.Getenv("PATH")
	tests := []struct {
		name            string
		path            string
		root            string
		pathAfterLookup string // PATH from the moment the guard has looked the tool up; "" keeps path
		argv            []string
		wantPanic       []string // substrings of the panic; none: the call must not panic
		wantPath        string   // the command's Path when it must not panic; "" skips the check
	}{
		{
			name: "mutating verb, tool outside the root",
			path: binThenOld, root: elsewhere,
			argv:      []string{"launchctl", "unload", plist},
			wantPanic: []string{"launchctl unload " + plist, "installServiceManagerFake(t)"},
		},
		{
			name: "query verb is not guarded",
			path: binThenOld, root: elsewhere,
			argv: []string{"launchctl", "list", "com.agentdeck.conductor-bridge"},
		},
		{
			name: "mutating verb, tool not on PATH",
			path: t.TempDir(), root: elsewhere,
			argv:      []string{"systemctl", "--user", "daemon-reload"},
			wantPanic: []string{"systemctl --user daemon-reload"},
		},
		{
			name: "mutating verb, fake under the root",
			path: bin, root: bin,
			argv:     []string{"launchctl", "unload", plist},
			wantPath: filepath.Join(bin, "launchctl"),
		},
		{
			name: "mutating verb, fake resolving outside the root",
			path: bin, root: bin,
			argv:      []string{"systemctl", "--user", "daemon-reload"},
			wantPanic: []string{"systemctl --user daemon-reload"},
		},
		{
			// The command is the binary the guard checked, not whatever a second
			// PATH lookup would find.
			name: "mutating verb, PATH changed after the lookup",
			path: bin, root: bin, pathAfterLookup: other,
			argv:     []string{"launchctl", "unload", plist},
			wantPath: filepath.Join(bin, "launchctl"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PATH", tt.path)
			// The guard asks for the root after its LookPath and before it builds
			// the command, so this is where another goroutine's os.Setenv("PATH")
			// would land.
			serviceManagerFakeRoot = func() string {
				if tt.pathAfterLookup != "" {
					t.Setenv("PATH", tt.pathAfterLookup)
				}
				return tt.root
			}
			var gotArgs []string
			got, msg := callRecover(func() string {
				cmd := serviceManagerCommand(tt.argv[0], tt.argv[1:]...)
				gotArgs = cmd.Args
				return cmd.Path
			})
			if len(tt.wantPanic) == 0 {
				if msg != "" {
					t.Fatalf("serviceManagerCommand(%q) panicked: %s", tt.argv, msg)
				}
				if tt.wantPath != "" && got != tt.wantPath {
					t.Fatalf("serviceManagerCommand(%q).Path = %q, want %q", tt.argv, got, tt.wantPath)
				}
				// argv[0] stays the tool's name, not the path that was run.
				if !slices.Equal(gotArgs, tt.argv) {
					t.Fatalf("serviceManagerCommand(%q).Args = %q, want %q", tt.argv, gotArgs, tt.argv)
				}
				return
			}
			if msg == "" {
				t.Fatalf("serviceManagerCommand(%q) did not panic (Path %q)", tt.argv, got)
			}
			for _, want := range tt.wantPanic {
				if !strings.Contains(msg, want) {
					t.Errorf("panic %q does not contain %q", msg, want)
				}
			}
		})
	}
}

// The fake answers every call without a process, records them all (failed ones
// included), and gives the seams back when the test ends.
func TestInstallServiceManagerFake_RecordsCallsAndRestoresSeams(t *testing.T) {
	seams := func() [3]uintptr {
		return [3]uintptr{
			reflect.ValueOf(runLaunchctl).Pointer(),
			reflect.ValueOf(launchctlOutput).Pointer(),
			reflect.ValueOf(runSystemctl).Pointer(),
		}
	}
	orig := seams()

	t.Run("fake", func(t *testing.T) {
		fake := installServiceManagerFake(t)
		for i, got := range seams() {
			if got == orig[i] {
				t.Errorf("seam %d still points at the production implementation", i)
			}
		}
		fake.SetLoaded(LaunchdPlistName, true)
		fake.SetLoaded("agent-deck-conductor-bridge.service", true)
		fake.FailOn("launchctl", "unload", errors.New("unload refused"))
		fake.FailNext("launchctl", "load", errors.New("load refused"))

		const plist = "/abs/com.agentdeck.conductor-bridge.plist"
		launchctl := func(args ...string) func() ([]byte, error) {
			return func() ([]byte, error) { return nil, runLaunchctl(args...) }
		}
		launchctlOut := func(args ...string) func() ([]byte, error) {
			return func() ([]byte, error) { return launchctlOutput(args...) }
		}
		systemctl := func(args ...string) func() ([]byte, error) {
			return func() ([]byte, error) { return nil, runSystemctl(args...) }
		}
		steps := []struct {
			name    string
			do      func() ([]byte, error)
			wantOut string
			wantErr string // "" for nil
			line    string // the command line the fake must have recorded
		}{
			{"unload fails (FailOn)", launchctl("unload", plist), "", "unload refused",
				"launchctl unload " + plist},
			{"unload fails again", launchctl("unload", plist), "", "unload refused",
				"launchctl unload " + plist},
			{"load fails once (FailNext)", launchctl("load", plist), "", "load refused",
				"launchctl load " + plist},
			{"load then succeeds", launchctl("load", plist), "", "",
				"launchctl load " + plist},
			{"list of a loaded label", launchctlOut("list", LaunchdPlistName), "com.agentdeck.conductor-bridge\n", "",
				"launchctl list com.agentdeck.conductor-bridge"},
			{"list of an unloaded label", launchctlOut("list", TransitionNotifierLaunchdPlistName), "",
				"launchctl list com.agentdeck.transition-notifier: not loaded",
				"launchctl list com.agentdeck.transition-notifier"},
			{"managername", launchctlOut("managername"), "Aqua\n", "",
				"launchctl managername"},
			{"is-active of a loaded unit",
				systemctl("--user", "is-active", "--quiet", "agent-deck-conductor-bridge.service"), "", "",
				"systemctl --user is-active --quiet agent-deck-conductor-bridge.service"},
			{"is-active of an unloaded unit",
				systemctl("--user", "is-active", "--quiet", "agent-deck-transition-notifier.service"), "",
				"agent-deck-transition-notifier.service inactive",
				"systemctl --user is-active --quiet agent-deck-transition-notifier.service"},
			{"enable", systemctl("--user", "enable", "--now", "agent-deck-conductor-heartbeat-ops.timer"), "", "",
				"systemctl --user enable --now agent-deck-conductor-heartbeat-ops.timer"},
		}
		wantCalls := make([]string, len(steps))
		for i, s := range steps {
			wantCalls[i] = s.line
			out, err := s.do()
			if gotErr := errText(err); gotErr != s.wantErr {
				t.Errorf("%s: error %q, want %q", s.name, gotErr, s.wantErr)
			}
			if string(out) != s.wantOut {
				t.Errorf("%s: output %q, want %q", s.name, out, s.wantOut)
			}
		}
		if got := fake.Calls(); !slices.Equal(got, wantCalls) {
			t.Errorf("Calls() = %q, want %q", got, wantCalls)
		}
		wantMutating := []string{wantCalls[0], wantCalls[1], wantCalls[2], wantCalls[3], wantCalls[9]}
		if got := fake.MutatingCalls(); !slices.Equal(got, wantMutating) {
			t.Errorf("MutatingCalls() = %q, want %q", got, wantMutating)
		}
	})

	if got := seams(); got != orig {
		t.Fatalf("the seams were not restored when the test ended: %v, want %v", got, orig)
	}
}

// The running checks, the launchd domain query and systemdUserAvailable's
// daemon-reload fallback all reach the service managers through the seams. PATH
// holds no tool, so a call that still execs one directly finds nothing and the
// fake records nothing.
func TestServiceManagerSeams_RouteDaemonCalls(t *testing.T) {
	switch platform.Detect() {
	case platform.PlatformMacOS, platform.PlatformLinux, platform.PlatformWSL2:
	default:
		t.Skip("no launchd or systemd branch on this platform")
	}
	t.Setenv("PATH", t.TempDir())
	// With no loginctl on PATH, systemdUserAvailable takes its daemon-reload fallback.
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	fake := installServiceManagerFake(t)

	if IsBridgeDaemonRunning() || IsTransitionNotifierDaemonRunning() {
		t.Fatalf("nothing loaded: running checks must be false; calls %q", fake.Calls())
	}
	for _, label := range []string{
		LaunchdPlistName, TransitionNotifierLaunchdPlistName,
		systemdBridgeServiceName, systemdTransitionNotifierServiceName,
	} {
		fake.SetLoaded(label, true)
	}
	if !IsBridgeDaemonRunning() || !IsTransitionNotifierDaemonRunning() ||
		launchctlManagerName() != "Aqua" || !systemdUserAvailable() {
		t.Fatalf("running checks, managername and the daemon-reload fallback must go through the seams; calls %q", fake.Calls())
	}
	got := fake.MutatingCalls()
	if len(fake.Calls()) != 6 || !slices.Equal(got, []string{"systemctl --user daemon-reload"}) {
		t.Fatalf("want four running checks, managername and one daemon-reload; got calls %q, mutating %q", fake.Calls(), got)
	}
}

// Each seam runs the tool it is named after, through its production closure and
// without the fake. PATH holds only a recording launchctl and systemctl, which
// use shell builtins alone because nothing else is on PATH. The guard admits
// their mutating verbs because t.TempDir() lies under serviceManagerFakeRoot().
func TestServiceManagerSeams_RunTheNamedTool(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "record")
	script := func(after string) []byte {
		return []byte("#!/bin/sh\necho \"${0##*/}\" \"$@\" >> '" + record + "'\n" + after)
	}
	for name, content := range map[string][]byte{
		"launchctl": script("echo Aqua\n"),
		"systemctl": script(""),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)

	tests := []struct {
		name    string
		call    func() (string, error)
		wantOut string
		want    string // the line the tool recorded
	}{
		{"runLaunchctl", func() (string, error) {
			return "", runLaunchctl("unload", "x.plist")
		}, "", "launchctl unload x.plist"},
		{"launchctlOutput", func() (string, error) {
			out, err := launchctlOutput("managername")
			return string(out), err
		}, "Aqua\n", "launchctl managername"},
		{"launchctlManagerName", func() (string, error) {
			return launchctlManagerName(), nil
		}, "Aqua", "launchctl managername"},
		{"runSystemctl", func() (string, error) {
			return "", runSystemctl("--user", "daemon-reload")
		}, "", "systemctl --user daemon-reload"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := os.Remove(record); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			var out string
			var err error
			if _, msg := callRecover(func() string { out, err = tt.call(); return "" }); msg != "" {
				t.Fatalf("%s panicked: %s", tt.name, msg)
			}
			if err != nil || out != tt.wantOut {
				t.Fatalf("%s = %q, %v; want %q, nil", tt.name, out, err, tt.wantOut)
			}
			got, err := os.ReadFile(record)
			if err != nil || string(got) != tt.want+"\n" {
				t.Fatalf("the tool recorded %q (read error %v), want %q", got, err, tt.want+"\n")
			}
		})
	}
}
