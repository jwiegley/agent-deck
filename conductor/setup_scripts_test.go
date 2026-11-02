package conductor

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/testutil"
)

// The checkout scripts conductor/setup.sh and conductor/teardown.sh leave a
// customized bridge.py and a symlinked or read-only plist or unit alone, as the
// Go side (agent-deck conductor setup, update, teardown) does. These tests drive
// their functions the way setup_register_test.go drives register_session:
// extractShellFunc pulls each function out of its script, and a small driver
// script defines it and calls it.
//
// A temporary HOME does not isolate launchd: `launchctl unload` of a plist
// rendered from the template acts on the live com.agentdeck.conductor-bridge
// agent whatever HOME is. Nothing isolates this package, so every driver puts a
// recording launchctl first on PATH and refuses to run unless `command -v
// launchctl` finds that fake.

const canonicalBridge, olderBridge, customBridge = "canonical v2\n", "shipped v1\n", "my Q&A hook\n"

// stampOf is what the stamp of a bridge.py holding s reads: the sha256 as 64
// lower-case hex digits and a newline, the format of the Go installer's stamp.
func stampOf(s string) string { return fmt.Sprintf("%x\n", sha256.Sum256([]byte(s))) }

// testBash is the bash every script of this file runs under: macOS's /bin/bash
// (3.2.57 on hera), the oldest bash the scripts must work with, else the one on
// PATH.
func testBash() string {
	if runtime.GOOS == "darwin" {
		if _, err := os.Stat("/bin/bash"); err == nil {
			return "/bin/bash"
		}
	}
	return "bash"
}

// mustWrite writes content to path, creating its directory, with permissions of
// exactly perm: the umask trims the mode a file is created with (0464 would
// become 0444), and Chmod is not trimmed.
func mustWrite(t *testing.T, path, content string, perm os.FileMode) {
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

// mustSymlink makes link a symlink to target, creating link's directory.
func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// readOr returns what path holds, read through symlinks, or "" when it cannot
// be read.
func readOr(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

// fakeLaunchctl is the launchctl every driver runs: it logs each call to
// $FAKE_LAUNCHCTL_LOG, answers `list` with the contents of $FAKE_LAUNCHCTL_LIST_FILE
// when that is set and with $FAKE_LAUNCHCTL_LIST otherwise, and succeeds. The file
// is read by `exec cat`, so that a reader that stops early kills the fake with
// SIGPIPE as it kills the real launchctl, a single process; a plain `cat` followed
// by the script's `exit 0` would hide the signal.
const fakeLaunchctl = `#!/bin/sh
echo "launchctl $*" >> "$FAKE_LAUNCHCTL_LOG"
if [ "$1" = list ]; then
    if [ -n "${FAKE_LAUNCHCTL_LIST_FILE:-}" ]; then exec cat "$FAKE_LAUNCHCTL_LIST_FILE"; fi
    printf '%s\n' "${FAKE_LAUNCHCTL_LIST:-}"
fi
exit 0
`

// driverPreamble starts every driver script with setup.sh's colour variables and
// logging functions, minus the colour. teardown.sh defines the same except fail,
// so a function of teardown.sh never calls fail.
const driverPreamble = `#!/usr/bin/env bash
set -euo pipefail
RED=''; GREEN=''; YELLOW=''; CYAN=''; NC=''
info() { echo "[info] $*"; }
ok() { echo "[ok] $*"; }
warn() { echo "[warn] $*"; }
fail() { echo "[fail] $*"; exit 1; }
`

// scriptFixture is a fake HOME, a fake checkout and a fake launchctl, all under
// one temporary root.
type scriptFixture struct {
	root, home, conductorDir, log string
	env                           []string
}

// newScriptFixture builds the fixture. Its env sets every variable the scripts
// read, HOME, CONDUCTOR_DIR and SCRIPT_DIR, and puts the fake launchctl first
// on PATH.
func newScriptFixture(t *testing.T) *scriptFixture {
	t.Helper()
	root := t.TempDir()
	f := &scriptFixture{
		root:         root,
		home:         filepath.Join(root, "home"),
		conductorDir: filepath.Join(root, "home", ".agent-deck", "conductor"),
		log:          filepath.Join(root, "launchctl.log"),
	}
	// setup.sh makes the conductor dir before install_bridge_script, whose mktemp
	// needs it.
	for _, dir := range []string{filepath.Join(f.home, "Library", "LaunchAgents"), f.conductorDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	scriptDir := filepath.Join(root, "checkout", "conductor")
	plist, err := os.ReadFile("com.agentdeck.conductor-bridge.plist")
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(scriptDir, "com.agentdeck.conductor-bridge.plist"), string(plist), 0o644)
	mustWrite(t, f.canonicalPath(), canonicalBridge, 0o644)
	binDir := filepath.Join(root, "bin")
	mustWrite(t, filepath.Join(binDir, "launchctl"), fakeLaunchctl, 0o755)
	f.env = append(os.Environ(),
		"HOME="+f.home,
		"CONDUCTOR_DIR="+f.conductorDir,
		"SCRIPT_DIR="+scriptDir,
		"FAKE_LAUNCHCTL_LOG="+f.log,
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	return f
}

// conductorPath is the path of name inside the fixture's conductor directory.
func (f *scriptFixture) conductorPath(name string) string {
	return filepath.Join(f.conductorDir, name)
}

// canonicalPath is the checkout's canonical bridge, where the scripts look for it.
func (f *scriptFixture) canonicalPath() string {
	return filepath.Join(f.root, "checkout", "internal", "session", "conductor_bridge.py")
}

// run defines funcs, as extracted from script, in a driver script and runs call
// with it, under testBash() and f.env plus env, in f.root. It returns the
// combined output and the exit error.
func (f *scriptFixture) run(t *testing.T, script string, funcs []string, call string, env ...string) (string, error) {
	t.Helper()
	var b strings.Builder
	b.WriteString(driverPreamble)
	// The guard. A fake without its exec bit would be skipped by the PATH search,
	// and the driver would fall through to /bin/launchctl.
	b.WriteString(`[[ "$(command -v launchctl)" == "` + filepath.Join(f.root, "bin", "launchctl") + `" ]] || fail "launchctl is not the fake"` + "\n")
	if script == "teardown.sh" {
		// teardown.sh defines no fail: a function of it that called fail would pass
		// here and exit 127 in the real script.
		b.WriteString("unset -f fail\n")
	}
	for _, name := range funcs {
		b.WriteString(extractShellFunc(t, script, name) + "\n")
	}
	b.WriteString(call + "\n")
	drive := filepath.Join(f.root, "drive.sh")
	mustWrite(t, drive, b.String(), 0o755)
	cmd := exec.Command(testBash(), drive)
	// Not the checkout: a relative path in a function cannot write into the tree.
	cmd.Dir = f.root
	cmd.Env = append(append([]string(nil), f.env...), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// wantCalls reports an error unless the launchctl calls the fake logged are
// exactly want, in order, with <plist> in each replaced by plist. A nil want
// means no call at all, not even `list`.
func (f *scriptFixture) wantCalls(t *testing.T, plist string, want []string) {
	t.Helper()
	var got, wantLines []string
	if log := strings.TrimSuffix(readOr(f.log), "\n"); log != "" {
		got = strings.Split(log, "\n")
	}
	for _, w := range want {
		wantLines = append(wantLines, strings.ReplaceAll(w, "<plist>", plist))
	}
	if strings.Join(got, "\n") != strings.Join(wantLines, "\n") {
		t.Errorf("launchctl calls = %q, want %q", got, wantLines)
	}
}

// TestShellServiceFileExternal pins service_file_external, the shell form of
// the Go rule that a symlinked or read-only service file belongs to another
// manager. The Shell prefix keeps it apart from the Go test of the same rule.
func TestShellServiceFileExternal(t *testing.T) {
	f := newScriptFixture(t)
	target := filepath.Join(f.root, "target.plist")
	mustWrite(t, target, "x\n", 0o644)

	tests := []struct {
		name     string
		link     string      // P is a symlink to this path
		mode     os.FileMode // P is a file with this mode
		rootSkip string      // skip as root, for this reason
		want     string
	}{
		{name: "symlink", link: target, want: "symlink"},
		{name: "dangling symlink", link: filepath.Join(f.root, "gone.plist"), want: "symlink"},
		// No write bit at all: read-only for every uid, root included.
		{name: "no write bit", mode: 0o444, want: "read-only"},
		// A write bit for the group only: the owner is denied by access(2), which
		// root passes.
		{name: "owner bits deny write", mode: 0o464, rootSkip: "root passes access(2) W_OK whatever the owner bits say", want: "read-only"},
		{name: "writable", mode: 0o644},
		{name: "missing"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.rootSkip != "" {
				testutil.SkipIfRoot(t, tc.rootSkip)
			}
			p := filepath.Join(t.TempDir(), "agent.plist")
			switch {
			case tc.link != "":
				mustSymlink(t, tc.link, p)
			case tc.mode != 0:
				mustWrite(t, p, "x\n", tc.mode)
			}
			out, err := f.run(t, "setup.sh", []string{"service_file_external"}, `service_file_external "$P"`, "P="+p)
			if strings.TrimSpace(out) != tc.want || (err == nil) != (tc.want != "") {
				t.Fatalf("service_file_external printed %q, err = %v; want %q and exit 0 exactly when that is not empty", out, err, tc.want)
			}
		})
	}
}

// TestScriptHelpers_SameInBothScripts: each script runs standalone from a
// checkout, so the helpers they share are copies, and must stay identical.
func TestScriptHelpers_SameInBothScripts(t *testing.T) {
	for _, name := range []string{"sha256_of", "service_file_external"} {
		t.Run(name, func(t *testing.T) {
			setup, teardown := extractShellFunc(t, "setup.sh", name), extractShellFunc(t, "teardown.sh", name)
			if setup != teardown {
				t.Errorf("%s differs between setup.sh and teardown.sh:\nsetup.sh:\n%s\nteardown.sh:\n%s", name, setup, teardown)
			}
		})
	}
}

// bridgeState is how a test leaves bridge.py and its stamp before a script runs.
type bridgeState struct {
	bridge     string // bridge.py's content; "" means no bridge.py
	bridgeLink bool   // bridge.py is a symlink to a file outside the conductor dir that holds bridge
	bridgeDir  bool   // bridge.py is a directory
	stamp      string // .bridge.py.sha256's content; "" means no stamp
	stampLink  bool   // the stamp is a symlink to a file outside the conductor dir that holds stamp
}

// arrangeBridge puts the conductor dir in the state s describes. Files get 0644.
func (f *scriptFixture) arrangeBridge(t *testing.T, s bridgeState) {
	t.Helper()
	bridge, stamp := f.conductorPath("bridge.py"), f.conductorPath(".bridge.py.sha256")
	switch {
	case s.bridgeDir:
		if err := os.MkdirAll(bridge, 0o755); err != nil {
			t.Fatal(err)
		}
	case s.bridgeLink:
		elsewhere := filepath.Join(f.root, "elsewhere.py")
		mustWrite(t, elsewhere, s.bridge, 0o644)
		mustSymlink(t, elsewhere, bridge)
	case s.bridge != "":
		mustWrite(t, bridge, s.bridge, 0o644)
	}
	switch {
	case s.stamp == "":
	case s.stampLink:
		store := filepath.Join(f.root, "stamp.txt")
		mustWrite(t, store, s.stamp, 0o644)
		mustSymlink(t, store, stamp)
	default:
		mustWrite(t, stamp, s.stamp, 0o644)
	}
}

// fakeFailingCp is a cp that always fails: a row that puts it first on PATH makes
// the staging copy of install_bridge_script fail for every user, root included,
// which a canonical file that cannot be read does not. The driven run calls cp
// once, for that copy, so the fake has no real cp to pass other calls to.
const fakeFailingCp = `#!/bin/sh
echo "cp: $*: the test's cp always fails" >&2
exit 1
`

// TestInstallBridgeScript pins install_bridge_script: the canonical bridge goes
// over bridge.py only while bridge.py is missing, identical, or still the file
// setup.sh stamped, the stamp it leaves is the one the Go installer reads, and a
// failure before bridge.py is replaced leaves bridge.py and its stamp as they
// were, and no temp file behind.
func TestInstallBridgeScript(t *testing.T) {
	const kept = "[skip] bridge.py has local changes; left unchanged. To take the shipped bridge, move it aside and re-run conductor/setup.sh."
	tests := []struct {
		name          string
		before        bridgeState
		noCanonical   bool   // the checkout's canonical bridge is removed before the run
		failingCp     bool   // a cp that always fails is first on PATH, so the staging copy fails
		previousDir   bool   // .bridge.py.previous is a directory with mode 0555, so the hard link into it fails
		rootSkip      string // skip as root, for this reason
		wantOut       string
		wantErr       bool   // the script exits non-zero
		wantBridge    string // what bridge.py, the stamp and .bridge.py.previous read afterwards; "" means absent
		wantStamp     string
		wantPrevious  string
		wantStampLink bool // the stamp is still a symlink
		wantExec      bool // bridge.py has mode 0755
	}{
		{
			name:       "missing",
			wantOut:    "[ok] bridge.py installed",
			wantBridge: canonicalBridge, wantStamp: stampOf(canonicalBridge), wantExec: true,
		},
		{
			name:       "identical, no stamp",
			before:     bridgeState{bridge: canonicalBridge},
			wantOut:    "[ok] bridge.py unchanged",
			wantBridge: canonicalBridge, wantStamp: stampOf(canonicalBridge),
		},
		{
			name:       "stamped older",
			before:     bridgeState{bridge: olderBridge, stamp: stampOf(olderBridge)},
			wantOut:    "[ok] bridge.py updated",
			wantBridge: canonicalBridge, wantStamp: stampOf(canonicalBridge), wantPrevious: olderBridge, wantExec: true,
		},
		{
			name:       "customized, no stamp",
			before:     bridgeState{bridge: customBridge},
			wantOut:    kept,
			wantBridge: customBridge,
		},
		{
			name:       "customized, stale stamp",
			before:     bridgeState{bridge: customBridge, stamp: stampOf(olderBridge)},
			wantOut:    kept,
			wantBridge: customBridge, wantStamp: stampOf(olderBridge),
		},
		{
			name:       "stamp link",
			before:     bridgeState{bridge: olderBridge, stamp: stampOf(olderBridge), stampLink: true},
			wantOut:    kept,
			wantBridge: olderBridge, wantStamp: stampOf(olderBridge), wantStampLink: true,
		},
		{
			// The stamp is replaced by a file of its own: writing through the link
			// would change a file that is not ours.
			name:       "identical, stamp link",
			before:     bridgeState{bridge: canonicalBridge, stamp: stampOf(olderBridge), stampLink: true},
			wantOut:    "[ok] bridge.py unchanged",
			wantBridge: canonicalBridge, wantStamp: stampOf(canonicalBridge),
		},
		{
			name:       "stamp without newline",
			before:     bridgeState{bridge: olderBridge, stamp: strings.TrimSuffix(stampOf(olderBridge), "\n")},
			wantOut:    kept,
			wantBridge: olderBridge, wantStamp: strings.TrimSuffix(stampOf(olderBridge), "\n"),
		},
		{
			name:       "symlink",
			before:     bridgeState{bridge: customBridge, bridgeLink: true},
			wantOut:    "[skip] bridge.py is a symlink; left unchanged.",
			wantBridge: customBridge,
		},
		{
			name:    "directory",
			before:  bridgeState{bridgeDir: true},
			wantOut: "[fail] refusing to replace",
			wantErr: true,
		},
		{
			// Without the canonical file there is nothing to install, and a bridge
			// that is not stamped is not "local changes" either.
			name:        "canonical bridge missing",
			before:      bridgeState{bridge: olderBridge, stamp: stampOf(olderBridge)},
			noCanonical: true,
			wantOut:     "[fail] canonical bridge not found",
			wantErr:     true,
			wantBridge:  olderBridge, wantStamp: stampOf(olderBridge),
		},
		{
			name:        "canonical bridge missing, customized",
			before:      bridgeState{bridge: customBridge},
			noCanonical: true,
			wantOut:     "[fail] canonical bridge not found",
			wantErr:     true,
			wantBridge:  customBridge,
		},
		{
			// The copy to the temp file fails, whoever runs the script, and the temp
			// file goes with it.
			name:       "staging copy fails",
			before:     bridgeState{bridge: olderBridge, stamp: stampOf(olderBridge)},
			failingCp:  true,
			wantOut:    "[fail] could not stage",
			wantErr:    true,
			wantBridge: olderBridge, wantStamp: stampOf(olderBridge),
		},
		{
			// Keeping the displaced bridge fails, so nothing is replaced and the
			// temp file goes.
			name:        "previous copy cannot be kept",
			before:      bridgeState{bridge: olderBridge, stamp: stampOf(olderBridge)},
			previousDir: true,
			rootSkip:    "root creates links in a read-only directory",
			wantOut:     "[fail] could not keep",
			wantErr:     true,
			wantBridge:  olderBridge, wantStamp: stampOf(olderBridge),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.rootSkip != "" {
				testutil.SkipIfRoot(t, tc.rootSkip)
			}
			f := newScriptFixture(t)
			f.arrangeBridge(t, tc.before)
			if tc.noCanonical {
				if err := os.Remove(f.canonicalPath()); err != nil {
					t.Fatal(err)
				}
			}
			if tc.failingCp {
				// In this fixture's own bin dir, first on its PATH: no other row has it.
				mustWrite(t, filepath.Join(f.root, "bin", "cp"), fakeFailingCp, 0o755)
			}
			if tc.previousDir {
				// ln -f into a directory makes the link inside it, and that fails here.
				dir := f.conductorPath(".bridge.py.previous")
				if err := os.Mkdir(dir, 0o555); err != nil {
					t.Fatal(err)
				}
				// The temp dir is removed from below, and an unwritable directory stops that.
				t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
			}
			out, err := f.run(t, "setup.sh", []string{"sha256_of", "install_bridge_script"}, "install_bridge_script")
			if !strings.Contains(out, tc.wantOut) || (err != nil) != tc.wantErr {
				t.Fatalf("install_bridge_script printed %q, err = %v; want %q and an error exactly when the row says so (%t)", out, err, tc.wantOut, tc.wantErr)
			}
			want := map[string]string{
				"bridge.py":           tc.wantBridge,
				".bridge.py.sha256":   tc.wantStamp,
				".bridge.py.previous": tc.wantPrevious,
			}
			for name, content := range want {
				if got := readOr(f.conductorPath(name)); got != content {
					t.Errorf("%s reads %q, want %q", name, got, content)
				}
			}
			entries, err := os.ReadDir(f.conductorDir)
			if err != nil {
				t.Fatal(err)
			}
			// Besides the files wanted, only the directories the row made may be there.
			allowed := map[string]bool{"bridge.py": tc.before.bridgeDir, ".bridge.py.previous": tc.previousDir}
			for name, content := range want {
				allowed[name] = allowed[name] || content != ""
			}
			for _, e := range entries {
				if !allowed[e.Name()] {
					t.Errorf("the run left %s in the conductor dir", e.Name())
				}
			}
			if tc.before.stampLink {
				if got := readOr(filepath.Join(f.root, "stamp.txt")); got != tc.before.stamp {
					t.Errorf("the file the stamp linked to reads %q, want it untouched (%q)", got, tc.before.stamp)
				}
				info, err := os.Lstat(f.conductorPath(".bridge.py.sha256"))
				if err != nil || (info.Mode()&os.ModeSymlink != 0) != tc.wantStampLink {
					t.Errorf("the stamp Lstat = %v, %v; want a symlink exactly when %t", info, err, tc.wantStampLink)
				}
			}
			if info, err := os.Stat(f.conductorPath("bridge.py")); tc.wantExec && err == nil && info.Mode().Perm() != 0o755 {
				t.Errorf("bridge.py has mode %v, want 0755", info.Mode().Perm())
			}
		})
	}
}

// TestRemoveBridgeScript pins remove_bridge_script: teardown.sh removes
// bridge.py, its stamp and .bridge.py.previous only when bridge.py is a regular
// file that equals the checkout's canonical bridge or matches its stamp, and
// never touches bridge.py.backup.
func TestRemoveBridgeScript(t *testing.T) {
	const (
		kept        = "[skip] bridge.py has local changes; kept"
		keptSymlink = "[skip] bridge.py is a symlink; kept"
	)
	tests := []struct {
		name    string
		before  bridgeState
		removed bool
		printed string // the line a kept bridge prints
	}{
		{name: "customized", before: bridgeState{bridge: customBridge}, printed: kept},
		{name: "customized, stale stamp", before: bridgeState{bridge: customBridge, stamp: stampOf(olderBridge)}, printed: kept},
		{name: "stamp link", before: bridgeState{bridge: olderBridge, stamp: stampOf(olderBridge), stampLink: true}, printed: kept},
		{name: "stamp without newline", before: bridgeState{bridge: olderBridge, stamp: strings.TrimSuffix(stampOf(olderBridge), "\n")}, printed: kept},
		{name: "stamped", before: bridgeState{bridge: olderBridge, stamp: stampOf(olderBridge)}, removed: true},
		{name: "identical", before: bridgeState{bridge: canonicalBridge}, removed: true},
		{name: "symlink", before: bridgeState{bridge: canonicalBridge, bridgeLink: true, stamp: stampOf(canonicalBridge)}, printed: keptSymlink},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newScriptFixture(t)
			f.arrangeBridge(t, tc.before)
			mustWrite(t, f.conductorPath(".bridge.py.previous"), "prev\n", 0o644)
			mustWrite(t, f.conductorPath("bridge.py.backup"), "backup\n", 0o644)
			out, err := f.run(t, "teardown.sh", []string{"sha256_of", "remove_bridge_script"}, "remove_bridge_script")
			if err != nil {
				t.Fatalf("remove_bridge_script exited with %v:\n%s", err, out)
			}
			if got := readOr(f.conductorPath("bridge.py.backup")); got != "backup\n" {
				t.Errorf("bridge.py.backup reads %q, want it untouched", got)
			}
			if tc.removed {
				for _, name := range []string{"bridge.py", ".bridge.py.sha256", ".bridge.py.previous"} {
					if _, err := os.Lstat(f.conductorPath(name)); !os.IsNotExist(err) {
						t.Errorf("%s is still there (Lstat err = %v)", name, err)
					}
				}
				if strings.Contains(out, "[skip]") {
					t.Errorf("a removed bridge printed a [skip] line:\n%s", out)
				}
				return
			}
			for name, want := range map[string]string{
				"bridge.py":           tc.before.bridge,
				".bridge.py.sha256":   tc.before.stamp,
				".bridge.py.previous": "prev\n",
			} {
				if got := readOr(f.conductorPath(name)); got != want {
					t.Errorf("%s reads %q, want %q (kept)", name, got, want)
				}
			}
			if info, err := os.Lstat(f.conductorPath("bridge.py")); err != nil || (info.Mode()&os.ModeSymlink != 0) != tc.before.bridgeLink {
				t.Errorf("bridge.py Lstat = %v, %v; want a symlink exactly when it was one", info, err)
			}
			if !strings.Contains(out, tc.printed) {
				t.Errorf("output lacks %q:\n%s", tc.printed, out)
			}
		})
	}
}

// plistPath is where the fixture's HOME keeps the plist of label.
func (f *scriptFixture) plistPath(label string) string {
	return filepath.Join(f.home, "Library", "LaunchAgents", label+".plist")
}

// arrangePlist leaves "old\n" at plist: as a file with mode, or when link is set
// at f.root/store.plist (0644) with plist a symlink to it. With neither, there is
// no plist.
func (f *scriptFixture) arrangePlist(t *testing.T, plist string, mode os.FileMode, link bool) {
	t.Helper()
	switch {
	case link:
		store := filepath.Join(f.root, "store.plist")
		mustWrite(t, store, "old\n", 0o644)
		mustSymlink(t, store, plist)
	case mode != 0:
		mustWrite(t, plist, "old\n", mode)
	}
}

// longLaunchctlList is what `launchctl list` prints when many jobs are loaded:
// the label on the first line, then more than 128 KiB, which is more than a pipe
// holds.
func longLaunchctlList(label string) string {
	return "-\t0\t" + label + "\n" + strings.Repeat("-\t0\tcom.example.filler\n", 12000)
}

// TestInstallLaunchAgent pins install_launch_agent: a plist that is a symlink or
// read-only belongs to another manager and gets neither a write nor a launchctl
// call, and one that agent-deck owns keeps the steps it always had, in order: the
// write comes first, so that a failed write stops the script before any launchctl
// call.
func TestInstallLaunchAgent(t *testing.T) {
	const label = "com.agentdeck.conductor-bridge"
	tests := []struct {
		name        string
		mode        os.FileMode // the plist is a file with this mode
		link        bool        // the plist is a symlink to a file
		readOnlyDir bool        // the LaunchAgents dir has mode 0555, and there is no plist
		longList    bool        // launchctl list prints longLaunchctlList
		rootSkip    string      // skip as root, for this reason
		wantOut     string
		wantErr     bool // the script exits non-zero
		calls       []string
		external    bool // the plist must still read "old\n"
	}{
		{name: "read-only plist", mode: 0o444, wantOut: "[skip] " + label + ": managed externally (read-only)", external: true},
		{name: "symlinked plist", link: true, wantOut: "[skip] " + label + ": managed externally (symlink)", external: true},
		{name: "owned plist", mode: 0o644, wantOut: "[ok] " + label + " loaded", calls: []string{"launchctl list", "launchctl unload <plist>", "launchctl load <plist>"}},
		// grep must read the whole list: one that stops at its first match makes
		// launchctl die of SIGPIPE, which pipefail turns into "not loaded", and the
		// reload skips the unload.
		{name: "owned plist, long list", mode: 0o644, longList: true, wantOut: "[ok] " + label + " loaded", calls: []string{"launchctl list", "launchctl unload <plist>", "launchctl load <plist>"}},
		{name: "plist write fails", readOnlyDir: true, rootSkip: "root writes into a read-only directory", wantOut: "Permission denied", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.rootSkip != "" {
				testutil.SkipIfRoot(t, tc.rootSkip)
			}
			f := newScriptFixture(t)
			p := f.plistPath(label)
			f.arrangePlist(t, p, tc.mode, tc.link)
			if tc.readOnlyDir {
				dir := filepath.Dir(p)
				if err := os.Chmod(dir, 0o555); err != nil {
					t.Fatal(err)
				}
				// The temp dir is removed from below, and an unwritable directory stops that.
				t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
			}
			env := []string{"P=" + p, "PLIST_NAME=" + label, "PYTHON3_PATH=/opt/test/python3"}
			if tc.longList {
				list := filepath.Join(f.root, "list.txt")
				mustWrite(t, list, longLaunchctlList(label), 0o644)
				env = append(env, "FAKE_LAUNCHCTL_LIST_FILE="+list)
			} else {
				env = append(env, "FAKE_LAUNCHCTL_LIST="+label)
			}
			out, err := f.run(t, "setup.sh", []string{"service_file_external", "render_bridge_plist", "install_launch_agent"},
				`install_launch_agent "`+label+`" render_bridge_plist "$P"`, env...)
			if (err != nil) != tc.wantErr || !strings.Contains(out, tc.wantOut) {
				t.Fatalf("install_launch_agent printed %q, err = %v; want %q and an error exactly when the row says so (%t)", out, err, tc.wantOut, tc.wantErr)
			}
			f.wantCalls(t, p, tc.calls)
			switch {
			case tc.wantErr:
				if _, err := os.Lstat(p); !os.IsNotExist(err) {
					t.Errorf("a plist was written although the write failed (Lstat err = %v)", err)
				}
			case tc.external:
				if got := readOr(p); got != "old\n" {
					t.Errorf("the plist reads %q, want it untouched", got)
				}
			default:
				if want := "<string>" + f.conductorDir + "/bridge.py</string>"; !strings.Contains(readOr(p), want) {
					t.Errorf("the plist lacks %q:\n%s", want, readOr(p))
				}
			}
		})
	}
}

// TestInstallGhwSystemdUnits pins install_ghw_systemd_units: the gh-watcher's
// service and timer are one unit of ownership, so one of them being another
// manager's keeps both alone.
func TestInstallGhwSystemdUnits(t *testing.T) {
	ghwSrc, err := filepath.Abs("gh-watcher")
	if err != nil {
		t.Fatal(err)
	}
	for _, symlinked := range []bool{true, false} {
		t.Run(fmt.Sprintf("timer symlinked=%t", symlinked), func(t *testing.T) {
			f := newScriptFixture(t)
			unitDir := filepath.Join(f.home, ".config", "systemd", "user")
			service, timer := filepath.Join(unitDir, "agent-deck-gh-watcher.service"), filepath.Join(unitDir, "agent-deck-gh-watcher.timer")
			ghwHome := filepath.Join(f.conductorDir, "gh-watcher")
			if symlinked {
				store := filepath.Join(f.root, "store.timer")
				mustWrite(t, store, "nix\n", 0o644)
				mustSymlink(t, store, timer)
			}
			out, err := f.run(t, "setup.sh", []string{"service_file_external", "install_ghw_systemd_units"},
				`mkdir -p "$U"; install_ghw_systemd_units "$U"`,
				"U="+unitDir, "GHW_SRC="+ghwSrc, "GHW_HOME="+ghwHome, "GHW_INTERVAL=45")
			if err != nil {
				t.Fatalf("install_ghw_systemd_units failed: %v\n%s", err, out)
			}
			written := "[ok] gh-watcher systemd units written to " + unitDir
			if symlinked {
				if want := "[skip] agent-deck-gh-watcher.timer: managed externally (symlink)"; !strings.Contains(out, want) {
					t.Errorf("output lacks %q:\n%s", want, out)
				}
				if strings.Contains(out, written) {
					t.Errorf("the units were reported written beside an external timer:\n%s", out)
				}
				if _, err := os.Lstat(service); !os.IsNotExist(err) {
					t.Errorf("the service was written beside an external timer (Lstat err = %v)", err)
				}
				if got := readOr(timer); got != "nix\n" {
					t.Errorf("the external timer reads %q, want it untouched", got)
				}
				return
			}
			if !strings.Contains(out, written) || strings.Contains(out, "[skip]") {
				t.Errorf("want %q and no [skip] line:\n%s", written, out)
			}
			if want := "ExecStart=/bin/bash " + ghwHome + "/scripts/tick.sh"; !strings.Contains(readOr(service), want) {
				t.Errorf("the service lacks %q:\n%s", want, readOr(service))
			}
			if want := "OnUnitActiveSec=45"; !strings.Contains(readOr(timer), want) {
				t.Errorf("the timer lacks %q:\n%s", want, readOr(timer))
			}
		})
	}
}

// TestShellRemoveLaunchAgent pins remove_launch_agent: teardown.sh neither
// unloads nor deletes a plist that is a symlink or read-only.
func TestShellRemoveLaunchAgent(t *testing.T) {
	const label = "com.agentdeck.conductor-bridge"
	tests := []struct {
		name    string
		mode    os.FileMode // the plist is a file with this mode; 0 means no plist
		wantOut string
		calls   []string
		kept    bool // the plist must still read "old\n"
	}{
		{name: "read-only plist", mode: 0o444, wantOut: "[skip] " + label + ": managed externally (read-only)", kept: true},
		{name: "owned plist", mode: 0o644, wantOut: "[ok] " + label + " stopped and plist removed", calls: []string{"launchctl unload <plist>"}},
		{name: "missing", wantOut: "[info] No " + label + " plist found (already removed)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newScriptFixture(t)
			p := f.plistPath(label)
			f.arrangePlist(t, p, tc.mode, false)
			out, err := f.run(t, "teardown.sh", []string{"service_file_external", "remove_launch_agent"},
				`remove_launch_agent "`+label+`" "$P"`, "P="+p)
			if err != nil || !strings.Contains(out, tc.wantOut) {
				t.Fatalf("remove_launch_agent printed %q, err = %v; want exit 0 and %q", out, err, tc.wantOut)
			}
			f.wantCalls(t, p, tc.calls)
			if got := readOr(p) == "old\n"; got != tc.kept {
				t.Errorf("the plist reads %q; it should be kept exactly when the row says so (kept = %t)", readOr(p), tc.kept)
			}
		})
	}
}

// TestConductorScripts_CallTheGuardedSteps pins that each script runs the
// guarded functions, which the tests above exercise, and no longer the plain
// commands they replaced. A call is a line of its own, below the definition of
// the function it names. The gh-watcher unload in teardown.sh stays inline, so
// only this test checks it, by its text: /usr/bin/trash is an absolute path that
// no PATH fake intercepts, and a test would move a file to the real Trash.
func TestConductorScripts_CallTheGuardedSteps(t *testing.T) {
	tests := []struct {
		script string
		calls  []string // lines the script must run, each on a line of its own
		gone   []string // text the script must no longer contain
	}{
		{
			script: "setup.sh",
			calls: []string{
				`install_bridge_script`,
				`install_launch_agent "${PLIST_NAME}" render_bridge_plist "${PLIST_PATH}"`,
				`install_launch_agent "${GHW_PLIST_NAME}" render_ghw_plist "${GHW_PLIST_PATH}"`,
				`install_ghw_systemd_units "${GHW_UNIT_DIR}"`,
			},
			gone: []string{
				`> "${PLIST_PATH}"`,
				`> "${GHW_PLIST_PATH}"`,
				`> "${GHW_UNIT_DIR}/${unit}"`,
				`cp "${SCRIPT_DIR}/../internal/session/conductor_bridge.py" "${CONDUCTOR_DIR}/bridge.py"`,
			},
		},
		{
			script: "teardown.sh",
			calls: []string{
				`SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"`,
				`remove_launch_agent "${PLIST_NAME}" "${PLIST_PATH}"`,
				`remove_bridge_script`,
				`if GHW_REASON="$(service_file_external "${GHW_PLIST_PATH}")"; then`,
				`echo "[skip] com.agentdeck.gh-watcher: managed externally (${GHW_REASON})"`,
			},
			gone: []string{
				`rm -f "${PLIST_PATH}"`,
				`rm -f "${CONDUCTOR_DIR}/bridge.py"`,
			},
		},
	}
	for _, tc := range tests {
		if out, err := exec.Command(testBash(), "-n", tc.script).CombinedOutput(); err != nil {
			t.Fatalf("%s does not parse under %s: %v\n%s", tc.script, testBash(), err, out)
		}
		raw, err := os.ReadFile(tc.script)
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		firstLine := map[string]int{} // each trimmed line, with the first line number it occurs on
		for i, line := range strings.Split(text, "\n") {
			if _, seen := firstLine[strings.TrimSpace(line)]; !seen {
				firstLine[strings.TrimSpace(line)] = i + 1
			}
		}
		for _, call := range tc.calls {
			at, ok := firstLine[call]
			if !ok {
				t.Errorf("%s does not run %q as a line of its own", tc.script, call)
				continue
			}
			if defined, ok := firstLine[strings.Fields(call)[0]+"() {"]; ok && defined > at {
				t.Errorf("%s runs %q at line %d but defines it at line %d", tc.script, call, at, defined)
			}
		}
		for _, gone := range tc.gone {
			if strings.Contains(text, gone) {
				t.Errorf("%s still has the unguarded %q", tc.script, gone)
			}
		}
	}
	// render_ghw_plist is an argument of a call, not the first field of one.
	extractShellFunc(t, "setup.sh", "render_ghw_plist")
}
