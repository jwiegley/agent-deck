package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/testutil"
)

// testAgentLabel is the launchd label the service-file tests give the agent
// whose plist they replace or remove. No real agent has it.
const testAgentLabel = "com.agentdeck.test-agent"

// writeServiceFixture writes content to path and sets its mode to exactly perm,
// creating the parent directory first. WriteFile applies perm only to a file it
// creates, and the umask trims it; the Chmod makes the mode exact.
func writeServiceFixture(t *testing.T, path, content string, perm os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatal(err)
	}
}

func TestExternalServiceError_Error(t *testing.T) {
	err := &ExternalServiceError{Label: LaunchdPlistName, Path: "/p.plist", Reason: "read-only"}
	want := "com.agentdeck.conductor-bridge: managed externally (read-only)"
	if got := err.Error(); got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}

func TestServiceFileExternal(t *testing.T) {
	dir := t.TempDir()
	writeServiceFixture(t, filepath.Join(dir, "writable.plist"), "owned", 0o644)
	writeServiceFixture(t, filepath.Join(dir, "readonly.plist"), "managed by nix", 0o444)
	if err := os.Symlink("writable.plist", filepath.Join(dir, "link.plist")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("gone.plist", filepath.Join(dir, "dangling.plist")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "dir.plist"), 0o755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		file     string
		writable func(string) bool // stands in for serviceFileWritable; nil keeps the real one
		want     string            // the Reason of the result; "" when the file is not external
		wantErr  bool
	}{
		{name: "missing", file: "absent.plist"},
		{name: "symlink", file: "link.plist", want: "symlink"},
		{name: "dangling symlink", file: "dangling.plist", want: "symlink"},
		{name: "0444 file", file: "readonly.plist", want: "read-only"},
		{name: "writable file", file: "writable.plist"},
		{name: "root-owned 0644", file: "writable.plist", writable: func(string) bool { return false }, want: "read-only"},
		// Root can write a 0444 file; the mode bits still decide.
		{name: "root run on 0444", file: "readonly.plist", writable: func(string) bool { return true }, want: "read-only"},
		{name: "directory", file: "dir.plist", wantErr: true},
		// Lstat fails with ENOTDIR, not "does not exist": the file cannot be
		// classified, which must not read as "not external".
		{name: "parent is a file", file: "writable.plist/x.plist", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.writable != nil {
				orig := serviceFileWritable
				t.Cleanup(func() { serviceFileWritable = orig })
				serviceFileWritable = tc.writable
			}
			path := filepath.Join(dir, tc.file)
			ext, err := serviceFileExternal(testAgentLabel, path)
			if (err != nil) != tc.wantErr {
				t.Fatalf("serviceFileExternal(%q) error = %v, wantErr %v", path, err, tc.wantErr)
			}
			if tc.want == "" {
				if ext != nil {
					t.Fatalf("serviceFileExternal(%q) = %+v, want nil", path, *ext)
				}
				return
			}
			want := ExternalServiceError{Label: testAgentLabel, Path: path, Reason: tc.want}
			if ext == nil || *ext != want {
				t.Fatalf("serviceFileExternal(%q) = %v, want %+v", path, ext, want)
			}
		})
	}
}

func TestRequireOwnedServiceFile(t *testing.T) {
	tests := []struct {
		name    string
		fixture func(t *testing.T, p string) // nil: nothing at p
		reason  string                       // the Reason of the *ExternalServiceError expected; "" for none
		errText string                       // text of the other error expected; "" for none
	}{
		{name: "missing"},
		{name: "owned 0644", fixture: func(t *testing.T, p string) { writeServiceFixture(t, p, "old", 0o644) }},
		{name: "0444", fixture: func(t *testing.T, p string) { writeServiceFixture(t, p, "managed by nix", 0o444) }, reason: "read-only"},
		{name: "directory", fixture: func(t *testing.T, p string) {
			if err := os.Mkdir(p, 0o755); err != nil {
				t.Fatal(err)
			}
		}, errText: "is not a regular file"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), testAgentLabel+".plist")
			if tc.fixture != nil {
				tc.fixture(t, p)
			}

			err := requireOwnedServiceFile(testAgentLabel, p)

			switch {
			case tc.reason != "":
				wantExternal(t, err, testAgentLabel, p, tc.reason)
				if _, ok := err.(*ExternalServiceError); !ok {
					t.Fatalf("requireOwnedServiceFile() = %T, want the *ExternalServiceError itself", err)
				}
			case tc.errText != "":
				var ext *ExternalServiceError
				if err == nil || errors.As(err, &ext) || !strings.Contains(err.Error(), tc.errText) {
					t.Fatalf("requireOwnedServiceFile() = %v, want an error that is no *ExternalServiceError and contains %q", err, tc.errText)
				}
			case err != nil:
				// %#v shows a typed nil, which is not == nil, as (*ExternalServiceError)(nil).
				t.Fatalf("requireOwnedServiceFile() = %#v, want nil", err)
			}
		})
	}
}

// testPlist returns the path of the agent's plist in a directory of its own,
// holding old at perm, or not there at all when old is "".
func testPlist(t *testing.T, old string, perm os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), testAgentLabel+".plist")
	if old != "" {
		writeServiceFixture(t, p, old, perm)
	}
	return p
}

func wantFileBytes(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if string(got) != want {
		t.Fatalf("%s holds %q, want %q", path, got, want)
	}
}

func wantPerm(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s has mode %v, want %v", path, got, want)
	}
}

// wantNoTemps fails when a temp file of writeServiceTemp is left in dir.
func wantNoTemps(t *testing.T, dir string) {
	t.Helper()
	temps, err := filepath.Glob(filepath.Join(dir, ".*.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(temps) != 0 {
		t.Fatalf("temp files left in %s: %q", dir, temps)
	}
}

// wantCalls compares the recorded service-manager calls with want; no want
// asserts that there were none.
func wantCalls(t *testing.T, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("service-manager calls = %q, want %q", got, want)
	}
}

// wantExternal fails unless err is (or wraps) an *ExternalServiceError with
// exactly these fields. A typed nil *ExternalServiceError stored in err, which
// errors.As accepts, fails it as well instead of panicking on the dereference.
func wantExternal(t *testing.T, err error, label, path, reason string) {
	t.Helper()
	var ext *ExternalServiceError
	if !errors.As(err, &ext) || ext == nil || *ext != (ExternalServiceError{Label: label, Path: path, Reason: reason}) {
		t.Fatalf("err = %v, want *ExternalServiceError{%s %s %s}", err, label, path, reason)
	}
}

// serviceEntry condenses everything a write, chmod, removal or replaced link
// could change at path into one string: the Lstat and Stat modes, the link
// target and the bytes read through the path. Each is its zero value on error,
// so a dangling link and a missing file differ by their Lstat mode.
func serviceEntry(path string) string {
	var lmode, smode os.FileMode
	if info, err := os.Lstat(path); err == nil {
		lmode = info.Mode()
	}
	if info, err := os.Stat(path); err == nil {
		smode = info.Mode()
	}
	target, _ := os.Readlink(path)
	data, _ := os.ReadFile(path)
	return fmt.Sprintf("lstat=%v stat=%v link=%q bytes=%q", lmode, smode, target, data)
}

// observeLaunchctl wraps the runLaunchctl that installServiceManagerFake
// installed, so call it after installServiceManagerFake. Before each call it
// records the verb, what the plist holds at that moment ("" when it is
// missing) and how many of writeServiceTemp's temp files sit beside it, so a
// test can see what state the real launchctl would have met.
func observeLaunchctl(t *testing.T, plist string) *[]string {
	t.Helper()
	var seen []string
	inner := runLaunchctl
	t.Cleanup(func() { runLaunchctl = inner })
	runLaunchctl = func(args ...string) error {
		data, _ := os.ReadFile(plist)
		temps, _ := filepath.Glob(filepath.Join(filepath.Dir(plist), ".*.tmp-*"))
		seen = append(seen, fmt.Sprintf("%s %s temps=%d", args[0], data, len(temps)))
		return inner(args...)
	}
	return &seen
}

// externalServiceKinds lists the ways a plist can belong to another manager.
// Each setup creates the plist at p (and its directory) in that state.
var externalServiceKinds = []struct {
	name, reason string
	setup        func(t *testing.T, p string)
}{
	{"0444", "read-only", func(t *testing.T, p string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		writeServiceFixture(t, p, "managed by nix", 0o444)
	}},
	{"symlink", "symlink", func(t *testing.T, p string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(t.TempDir(), "target")
		writeServiceFixture(t, target, "managed by nix", 0o644)
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
	}},
	{"dangling symlink", "symlink", func(t *testing.T, p string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(filepath.Dir(p), "gone"), p); err != nil {
			t.Fatal(err)
		}
	}},
	{"root-owned 0644", "read-only", func(t *testing.T, p string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		writeServiceFixture(t, p, "managed by nix", 0o644)
		orig := serviceFileWritable
		t.Cleanup(func() { serviceFileWritable = orig })
		serviceFileWritable = func(string) bool { return false }
	}},
}

func TestReplaceLaunchAgent_ExternalIsUntouched(t *testing.T) {
	for _, kind := range externalServiceKinds {
		t.Run(kind.name, func(t *testing.T) {
			fake := installServiceManagerFake(t)
			p := filepath.Join(t.TempDir(), testAgentLabel+".plist")
			kind.setup(t, p)
			before := serviceEntry(p)

			wantExternal(t, replaceLaunchAgent(testAgentLabel, p, []byte("new")), testAgentLabel, p, kind.reason)
			wantCalls(t, fake.Calls())
			if got := serviceEntry(p); got != before {
				t.Fatalf("the plist changed:\n got %s\nwant %s", got, before)
			}
			wantNoTemps(t, filepath.Dir(p))
		})
	}
}

func TestReplaceLaunchAgent_OwnedPlist(t *testing.T) {
	tests := []struct {
		name string
		old  string // the plist before; "" for none
		perm os.FileMode
	}{
		// Neither CreateTemp's 0o600 nor the 0o644 of a new plist, so a missing
		// Chmod shows.
		{name: "keeps the mode of the old plist", old: "old", perm: 0o640},
		{name: "first install", old: "", perm: 0o644},
		{name: "same bytes are still reloaded", old: "new", perm: 0o644},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := installServiceManagerFake(t)
			p := testPlist(t, tc.old, tc.perm)
			seen := observeLaunchctl(t, p)

			if err := replaceLaunchAgent(testAgentLabel, p, []byte("new")); err != nil {
				t.Fatalf("replaceLaunchAgent() = %v, want nil", err)
			}
			wantCalls(t, fake.MutatingCalls(), "launchctl unload "+p, "launchctl load "+p)
			// The temp is written before the unload and renamed before the load.
			wantCalls(t, *seen, "unload "+tc.old+" temps=1", "load new temps=0")
			wantFileBytes(t, p, "new")
			wantPerm(t, p, tc.perm)
			wantNoTemps(t, filepath.Dir(p))
		})
	}
}

func TestReplaceLaunchAgent_TempWriteFailureUnloadsNothing(t *testing.T) {
	fake := installServiceManagerFake(t)
	testutil.SkipIfRoot(t, "root can create files in a 0555 directory")
	p := testPlist(t, "old", 0o644)
	dir := filepath.Dir(p)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	err := replaceLaunchAgent(testAgentLabel, p, []byte("new"))
	if err == nil || !strings.Contains(err.Error(), "writing com.agentdeck.test-agent plist") {
		t.Fatalf("replaceLaunchAgent() = %v, want an error containing %q", err, "writing com.agentdeck.test-agent plist")
	}
	wantCalls(t, fake.Calls())
	wantFileBytes(t, p, "old")
}

func TestReplaceLaunchAgent_Failures(t *testing.T) {
	// The mode of every old plist: neither CreateTemp's 0o600 nor the 0o644 of a
	// new plist, so a restore at either shows.
	const oldPerm os.FileMode = 0o640
	tests := []struct {
		name   string
		old    string   // the plist before; "" for none
		rename int      // the call of renameServiceFile that fails (1 or 2); 0 for none
		load   string   // "ok", "first" (only the first load fails) or "every"
		verbs  []string // the launchctl verbs issued that change state, in order
		after  string   // the plist afterwards; "" for none
		want   string   // text the error contains; {P} stands for the plist path
	}{
		{
			name: "rename refused", old: "old", rename: 1, load: "ok",
			verbs: []string{"unload", "load"}, after: "old",
			want: "replacing com.agentdeck.test-agent plist: rename refused; reloaded the previous plist",
		},
		{
			name: "rename refused, old will not load", old: "old", rename: 1, load: "every",
			verbs: []string{"unload", "load"}, after: "old",
			want: "the previous plist did not load: run 'launchctl load {P}'",
		},
		{
			name: "rename refused, first install", old: "", rename: 1, load: "ok",
			verbs: []string{"unload"}, after: "",
			want: "replacing com.agentdeck.test-agent plist: rename refused",
		},
		{
			name: "new plist rejected", old: "old", rename: 0, load: "first",
			verbs: []string{"unload", "load", "load"}, after: "old",
			want: "loading com.agentdeck.test-agent: load refused; restored and reloaded the previous plist",
		},
		{
			name: "old plist rejected too", old: "old", rename: 0, load: "every",
			verbs: []string{"unload", "load", "load"}, after: "old",
			want: "restored the previous plist, but it did not load: run 'launchctl load {P}'",
		},
		{
			name: "restore fails", old: "old", rename: 2, load: "every",
			verbs: []string{"unload", "load"}, after: "new",
			want: "restoring the previous plist failed",
		},
		{
			name: "first install rejected", old: "", rename: 0, load: "every",
			verbs: []string{"unload", "load"}, after: "new",
			want: "plist written but failed to load com.agentdeck.test-agent",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := installServiceManagerFake(t)
			p := testPlist(t, tc.old, oldPerm)
			if tc.rename > 0 {
				orig := renameServiceFile
				t.Cleanup(func() { renameServiceFile = orig })
				renames := 0
				renameServiceFile = func(from, to string) error {
					renames++
					if renames == tc.rename {
						return errors.New("rename refused")
					}
					return os.Rename(from, to)
				}
			}
			switch tc.load {
			case "first":
				fake.FailNext("launchctl", "load", errors.New("load refused"))
			case "every":
				fake.FailOn("launchctl", "load", errors.New("load refused"))
			}

			err := replaceLaunchAgent(testAgentLabel, p, []byte("new"))

			calls := make([]string, len(tc.verbs))
			for i, verb := range tc.verbs {
				calls[i] = "launchctl " + verb + " " + p
			}
			wantCalls(t, fake.MutatingCalls(), calls...)
			if tc.after == "" {
				if _, statErr := os.Lstat(p); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("the plist should be absent, Lstat gave %v", statErr)
				}
			} else {
				wantFileBytes(t, p, tc.after)
			}
			if tc.after == "old" {
				wantPerm(t, p, oldPerm)
			}
			if want := strings.ReplaceAll(tc.want, "{P}", p); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("replaceLaunchAgent() = %v, want an error containing %q", err, want)
			}
			wantNoTemps(t, filepath.Dir(p))
		})
	}
}

func TestRemoveLaunchAgent(t *testing.T) {
	type removeCase struct {
		name        string
		fixture     func(t *testing.T, p string) // nil: nothing at p
		strict      bool
		unloadFails bool
		external    string // the Reason of the *ExternalServiceError expected; "" for none
		errText     string // text the error contains; "" for a nil error
		unloads     bool   // `launchctl unload P` is expected
		removed     bool   // p is gone afterwards; otherwise it is unchanged
	}
	owned := func(t *testing.T, p string) { writeServiceFixture(t, p, "old", 0o644) }
	tests := []removeCase{
		{name: "missing", strict: true, removed: true},
		{name: "owned", fixture: owned, strict: true, unloads: true, removed: true},
		{name: "owned, lenient", fixture: owned, unloadFails: true, unloads: true, removed: true},
		{name: "owned, strict", fixture: owned, strict: true, unloadFails: true, errText: "unloading com.agentdeck.test-agent", unloads: true},
		// Neither unloaded nor removed: it cannot be classified.
		{name: "directory", fixture: func(t *testing.T, p string) {
			if err := os.Mkdir(p, 0o755); err != nil {
				t.Fatal(err)
			}
		}, errText: "refusing to manage com.agentdeck.test-agent"},
	}
	for _, kind := range externalServiceKinds {
		tests = append(tests, removeCase{name: "external/" + kind.name, fixture: kind.setup, external: kind.reason})
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := installServiceManagerFake(t)
			p := filepath.Join(t.TempDir(), testAgentLabel+".plist")
			if tc.fixture != nil {
				tc.fixture(t, p)
			}
			before := serviceEntry(p)
			if tc.unloadFails {
				fake.FailOn("launchctl", "unload", errors.New("unload refused"))
			}

			err := removeLaunchAgent(testAgentLabel, p, tc.strict)

			switch {
			case tc.external != "":
				wantExternal(t, err, testAgentLabel, p, tc.external)
			case tc.errText != "":
				if err == nil || !strings.Contains(err.Error(), tc.errText) {
					t.Fatalf("removeLaunchAgent() = %v, want an error containing %q", err, tc.errText)
				}
			case err != nil:
				t.Fatalf("removeLaunchAgent() = %v, want nil", err)
			}
			var calls []string
			if tc.unloads {
				calls = []string{"launchctl unload " + p}
			}
			wantCalls(t, fake.Calls(), calls...)
			if tc.removed {
				if _, statErr := os.Lstat(p); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("the plist should be gone, Lstat gave %v", statErr)
				}
			} else if got := serviceEntry(p); got != before {
				t.Fatalf("the plist changed:\n got %s\nwant %s", got, before)
			}
		})
	}
}

// TestLaunchAgentWiring_ExternalPlistIsLeftAlone drives the six launchd
// functions the way `conductor setup` and `conductor teardown` do. An external
// plist must be reported and left exactly as it was, with no launchctl call,
// and without generating a replacement first (PATH holds no python3 and no
// agent-deck, which the generators need).
func TestLaunchAgentWiring_ExternalPlistIsLeftAlone(t *testing.T) {
	agents := []struct {
		name      string
		label     string
		path      func() (string, error)
		install   func() error
		uninstall func() error
	}{
		{
			name: "bridge", label: LaunchdPlistName, path: LaunchdPlistPath,
			install:   func() error { _, err := installBridgeDaemonLaunchd(); return err },
			uninstall: uninstallBridgeDaemonLaunchd,
		},
		{
			name: "notifier", label: TransitionNotifierLaunchdPlistName, path: TransitionNotifierLaunchdPlistPath,
			install:   func() error { _, err := installTransitionNotifierLaunchd(); return err },
			uninstall: uninstallTransitionNotifierLaunchd,
		},
		{
			name: "heartbeat", label: HeartbeatPlistLabel("ops"),
			path:      func() (string, error) { return HeartbeatPlistPath("ops") },
			install:   func() error { return installHeartbeatDaemonLaunchd("ops", 15) },
			uninstall: func() error { return uninstallHeartbeatDaemonLaunchd("ops") },
		},
	}
	for _, agent := range agents {
		operations := []struct {
			name string
			call func() error
		}{
			{"install", agent.install},
			{"uninstall", agent.uninstall},
		}
		for _, op := range operations {
			for _, kind := range externalServiceKinds {
				t.Run(op.name+" "+agent.name+"/"+kind.name, func(t *testing.T) {
					// The plist paths hang off HOME; the fixture must not land in the
					// HOME that TestMain gives the whole package.
					t.Setenv("HOME", t.TempDir())
					t.Setenv("PATH", t.TempDir())
					fake := installServiceManagerFake(t)
					p, err := agent.path()
					if err != nil {
						t.Fatal(err)
					}
					kind.setup(t, p)
					before := serviceEntry(p)

					wantExternal(t, op.call(), agent.label, p, kind.reason)
					wantCalls(t, fake.Calls()) // not even managername or list
					if got := serviceEntry(p); got != before {
						t.Fatalf("the plist changed:\n got %s\nwant %s", got, before)
					}
					wantNoTemps(t, filepath.Dir(p))
				})
			}
		}
	}
}

// The bridge's and the notifier's uninstalls are lenient: a failed unload does
// not keep the plist. `conductor teardown` discards their errors, so a strict
// flag here would leave the file behind unnoticed. (The heartbeat's uninstall is
// strict, which TestLaunchdHeartbeatUninstallRefusesFailedUnload pins.)
func TestLaunchAgentWiring_LenientUninstallRemovesOwnedPlist(t *testing.T) {
	agents := []struct {
		name      string
		path      func() (string, error)
		uninstall func() error
	}{
		{"bridge", LaunchdPlistPath, uninstallBridgeDaemonLaunchd},
		{"notifier", TransitionNotifierLaunchdPlistPath, uninstallTransitionNotifierLaunchd},
	}
	for _, agent := range agents {
		t.Run(agent.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			fake := installServiceManagerFake(t)
			p, err := agent.path()
			if err != nil {
				t.Fatal(err)
			}
			writeServiceFixture(t, p, "old", 0o644)
			fake.FailOn("launchctl", "unload", errors.New("unload refused"))

			if err := agent.uninstall(); err != nil {
				t.Fatalf("uninstall = %v, want nil", err)
			}
			wantCalls(t, fake.MutatingCalls(), "launchctl unload "+p)
			if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the plist should be gone, Lstat gave %v", err)
			}
		})
	}
}

// The Background-domain guard skips the install of a running notifier and
// returns nil; an external plist is reported first, so the skip cannot hide it.
func TestInstallTransitionNotifierLaunchd_ExternalPrecedesBackgroundGuard(t *testing.T) {
	withGuardSeams(t, "Background", true)
	fake := installServiceManagerFake(t)
	p, err := TransitionNotifierLaunchdPlistPath()
	if err != nil {
		t.Fatal(err)
	}
	writeServiceFixture(t, p, "managed by nix", 0o444)

	_, err = installTransitionNotifierLaunchd()

	wantExternal(t, err, TransitionNotifierLaunchdPlistName, p, "read-only")
	wantCalls(t, fake.Calls())
	wantFileBytes(t, p, "managed by nix")
}
