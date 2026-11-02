package session

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/platform"
)

// stubSystemdUserAvailable makes the install paths ask f, not the host, whether
// a systemd user session exists, until the test ends.
func stubSystemdUserAvailable(t *testing.T, f func() bool) {
	t.Helper()
	orig := systemdUserAvailableCheck
	t.Cleanup(func() { systemdUserAvailableCheck = orig })
	systemdUserAvailableCheck = f
}

// forbidSystemdUserCheck fails the test when the install path asks whether a
// systemd user session exists: the path under test must stop before that.
func forbidSystemdUserCheck(t *testing.T) {
	t.Helper()
	stubSystemdUserAvailable(t, func() bool {
		t.Error("availability checked")
		return true
	})
}

// hbUnits returns a heartbeat's unit pair in dir, the service first, each
// holding "new <Name>".
func hbUnits(dir string) []systemdUnitFile {
	units := make([]systemdUnitFile, 0, 2)
	for _, name := range []string{"hb.service", "hb.timer"} {
		units = append(units, systemdUnitFile{Name: name, Path: filepath.Join(dir, name), Content: []byte("new " + name)})
	}
	return units
}

// snapshotUnitDir fingerprints every entry of dir, the temp files of
// writeServiceTemp included, by name. A regular file is "<perm> <bytes>", a
// symlink is "-> <target> <the bytes read through it, or missing>" and anything
// else is its file mode, so a write, a chmod, a removal and a replaced link all
// change the snapshot.
func snapshotUnitDir(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := make(map[string]string, len(entries))
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		switch mode := info.Mode(); {
		case mode&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				t.Fatal(err)
			}
			through := "missing"
			if data, err := os.ReadFile(path); err == nil {
				through = string(data)
			}
			snapshot[entry.Name()] = "-> " + target + " " + through
		case mode.IsRegular():
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			snapshot[entry.Name()] = fmt.Sprintf("%v %s", mode.Perm(), data)
		default:
			snapshot[entry.Name()] = mode.String()
		}
	}
	return snapshot
}

// linkUnit makes p a symlink to target, creating p's directory first.
func linkUnit(t *testing.T, target, p string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, p); err != nil {
		t.Fatal(err)
	}
}

// externalUnitKinds lists the ways a unit file can belong to another manager.
// Each setup puts the unit at p (and creates p's directory) in that state. The
// seam kind leaves every other path to the seam it replaced, so one test can
// give several units this kind and keep a pair's owned unit owned.
var externalUnitKinds = []struct {
	name, reason string
	setup        func(t *testing.T, p string)
}{
	{"0444", "read-only", func(t *testing.T, p string) {
		writeServiceFixture(t, p, "kept", 0o444)
	}},
	{"symlink", "symlink", func(t *testing.T, p string) {
		target := filepath.Join(t.TempDir(), "store.unit")
		writeServiceFixture(t, target, "kept", 0o644)
		linkUnit(t, target, p)
	}},
	{"dangling symlink", "symlink", func(t *testing.T, p string) {
		linkUnit(t, filepath.Join(t.TempDir(), "gone.unit"), p)
	}},
	{"unwritable via the seam", "read-only", func(t *testing.T, p string) {
		writeServiceFixture(t, p, "kept", 0o644)
		orig := serviceFileWritable
		t.Cleanup(func() { serviceFileWritable = orig })
		serviceFileWritable = func(path string) bool { return path != p && orig(path) }
	}},
}

// writeOwnedUnits writes every unit as an owned file holding "old <Name>".
func writeOwnedUnits(t *testing.T, units []systemdUnitFile) {
	t.Helper()
	for _, u := range units {
		writeServiceFixture(t, u.Path, "old "+u.Name, 0o644)
	}
}

func TestRequireOwnedSystemdUnits(t *testing.T) {
	tests := []struct {
		name    string
		fixture func(t *testing.T, units []systemdUnitFile) // nil: no unit present
		reason  string                                      // the Reason of the *ExternalServiceError expected; "" for none
		at      int                                         // the unit whose file that error names
		errText string                                      // text of the other error expected; "" for none
	}{
		{name: "no unit present"},
		{name: "both units owned", fixture: writeOwnedUnits},
		{name: "timer is 0444", fixture: func(t *testing.T, units []systemdUnitFile) {
			writeServiceFixture(t, units[0].Path, "old hb.service", 0o644)
			writeServiceFixture(t, units[1].Path, "managed by nix", 0o444)
		}, reason: "read-only", at: 1},
		// Two external units: the first, in slice order, is the one reported.
		{name: "both units external", fixture: func(t *testing.T, units []systemdUnitFile) {
			linkUnit(t, filepath.Join(t.TempDir(), "gone.unit"), units[0].Path)
			writeServiceFixture(t, units[1].Path, "managed by nix", 0o444)
		}, reason: "symlink", at: 0},
		{name: "timer is a directory", fixture: func(t *testing.T, units []systemdUnitFile) {
			writeServiceFixture(t, units[0].Path, "old hb.service", 0o644)
			if err := os.Mkdir(units[1].Path, 0o755); err != nil {
				t.Fatal(err)
			}
		}, errText: "is not a regular file"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			units := hbUnits(t.TempDir())
			if tc.fixture != nil {
				tc.fixture(t, units)
			}

			err := requireOwnedSystemdUnits("hb.timer", units)

			switch {
			case tc.reason != "":
				wantExternal(t, err, "hb.timer", units[tc.at].Path, tc.reason)
			case tc.errText != "":
				var ext *ExternalServiceError
				if err == nil || errors.As(err, &ext) || !strings.Contains(err.Error(), tc.errText) {
					t.Fatalf("requireOwnedSystemdUnits() = %v, want an error that is no *ExternalServiceError and contains %q", err, tc.errText)
				}
			case err != nil:
				// %#v shows a typed nil, which is not == nil, as (*ExternalServiceError)(nil).
				t.Fatalf("requireOwnedSystemdUnits() = %#v, want nil", err)
			}
		})
	}
}

func TestReplaceSystemdUnits_OneExternalUnitWritesNeither(t *testing.T) {
	for _, kind := range externalUnitKinds {
		t.Run(kind.name, func(t *testing.T) {
			fake := installServiceManagerFake(t)
			forbidSystemdUserCheck(t)
			dir := t.TempDir()
			units := hbUnits(dir)
			writeServiceFixture(t, units[0].Path, "old hb.service", 0o644)
			kind.setup(t, units[1].Path)
			before := snapshotUnitDir(t, dir)

			err := replaceSystemdUnits("hb.timer", units, "hb.timer")

			wantExternal(t, err, "hb.timer", units[1].Path, kind.reason)
			wantCalls(t, fake.Calls())
			if after := snapshotUnitDir(t, dir); !maps.Equal(after, before) {
				t.Fatalf("the unit directory changed:\n got %v\nwant %v", after, before)
			}
		})
	}
}

func TestReplaceSystemdUnits_OwnedPairRenamedThenOnlyEnabled(t *testing.T) {
	tests := []struct {
		name      string
		before    string // what each unit holds beforehand: "<before> <Name>"
		available bool   // whether a systemd user session exists
	}{
		{name: "old", before: "old", available: true},
		// Already the new bytes, and still renamed: the unit is always replaced.
		{name: "current", before: "new", available: true},
		{name: "unavailable", before: "old", available: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := installServiceManagerFake(t)
			dir := t.TempDir()
			units := hbUnits(dir)
			// What the units hold each time availability is asked. The check comes
			// after the renames, so it must see the new bytes.
			var seen []string
			stubSystemdUserAvailable(t, func() bool {
				for _, u := range units {
					data, _ := os.ReadFile(u.Path)
					seen = append(seen, string(data))
				}
				return tc.available
			})
			var preCall []os.FileInfo
			for _, u := range units {
				writeServiceFixture(t, u.Path, tc.before+" "+u.Name, 0o644)
				info, err := os.Stat(u.Path)
				if err != nil {
					t.Fatal(err)
				}
				preCall = append(preCall, info)
			}

			err := replaceSystemdUnits("hb.timer", units, "hb.timer")

			if tc.available && err != nil {
				t.Fatalf("replaceSystemdUnits() = %v, want nil", err)
			}
			// Compared with ==: the sentinel comes back as it is, for callers to wrap.
			if !tc.available && err != errSystemdUserUnavailable {
				t.Fatalf("replaceSystemdUnits() = %v, want errSystemdUserUnavailable itself", err)
			}
			if want := []string{"new hb.service", "new hb.timer"}; !slices.Equal(seen, want) {
				t.Fatalf("the units held %q when availability was asked, want %q", seen, want)
			}
			for i, u := range units {
				wantFileBytes(t, u.Path, "new "+u.Name)
				wantPerm(t, u.Path, 0o644)
				info, err := os.Stat(u.Path)
				if err != nil {
					t.Fatal(err)
				}
				if os.SameFile(preCall[i], info) {
					t.Fatalf("%s was written in place, want a new file renamed over it", u.Name)
				}
			}
			wantNoTemps(t, dir)
			var calls []string
			if tc.available {
				calls = []string{"systemctl --user enable --now hb.timer"}
			}
			wantCalls(t, fake.Calls(), calls...)
		})
	}
}

func TestReplaceSystemdUnits_Failures(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, units []systemdUnitFile) // writes the fixtures; may move a unit
		want  string                                      // text the error contains
		files []string                                    // what each unit holds afterwards; "" for not checked
	}{
		{
			name: "temp write fails",
			setup: func(t *testing.T, units []systemdUnitFile) {
				writeServiceFixture(t, units[0].Path, "old hb.service", 0o644)
				// The timer's directory is missing, so its temp cannot be created, and
				// only after the service's temp was written.
				units[1].Path = filepath.Join(filepath.Dir(units[1].Path), "missing", "hb.timer")
			},
			want:  "writing hb.timer",
			files: []string{"old hb.service", ""},
		},
		{
			name: "rename fails",
			setup: func(t *testing.T, units []systemdUnitFile) {
				writeOwnedUnits(t, units)
				orig := renameServiceFile
				t.Cleanup(func() { renameServiceFile = orig })
				renames := 0
				renameServiceFile = func(from, to string) error {
					renames++
					if renames == 2 {
						return errors.New("rename refused")
					}
					return os.Rename(from, to)
				}
			},
			want: "installing hb.timer: rename refused",
			// Nothing was stopped, so the service that was renamed is not put back.
			files: []string{"new hb.service", "old hb.timer"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := installServiceManagerFake(t)
			forbidSystemdUserCheck(t)
			dir := t.TempDir()
			units := hbUnits(dir)
			tc.setup(t, units)

			err := replaceSystemdUnits("hb.timer", units, "hb.timer")

			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("replaceSystemdUnits() = %v, want an error containing %q", err, tc.want)
			}
			for i, want := range tc.files {
				if want != "" {
					wantFileBytes(t, units[i].Path, want)
				}
			}
			wantNoTemps(t, dir)
			wantCalls(t, fake.Calls())
		})
	}
}

func TestRemoveSystemdUnits_Sequence(t *testing.T) {
	const (
		disable = "systemctl --user disable --now hb.timer"
		reload  = "systemctl --user daemon-reload"
	)
	type removeCase struct {
		name         string
		fixture      func(t *testing.T, units []systemdUnitFile) // nil: no unit file
		strict       bool
		disableFails bool
		external     string   // the Reason of the *ExternalServiceError expected; "" for none
		errText      string   // text the error contains; "" for a nil error
		calls        []string // the service-manager calls, in order
		left         []string // the files left in the directory, sorted, each as it was
	}
	service := func(t *testing.T, units []systemdUnitFile) {
		writeServiceFixture(t, units[0].Path, "old hb.service", 0o644)
	}
	tests := []removeCase{
		{name: "owned", fixture: writeOwnedUnits, strict: true, calls: []string{disable, reload}},
		{
			name: "strict failure", fixture: writeOwnedUnits, strict: true, disableFails: true,
			errText: "disabling hb.timer", calls: []string{disable}, left: []string{"hb.service", "hb.timer"},
		},
		{name: "lenient failure", fixture: writeOwnedUnits, disableFails: true, calls: []string{disable, reload}},
		// The timer is not there, so there is nothing to disable.
		{name: "service only", fixture: service, strict: true, calls: []string{reload}},
		{name: "missing", strict: true},
	}
	for _, kind := range externalUnitKinds {
		tests = append(tests, removeCase{
			name: "external/" + kind.name,
			fixture: func(t *testing.T, units []systemdUnitFile) {
				service(t, units)
				kind.setup(t, units[1].Path)
			},
			external: kind.reason, left: []string{"hb.service", "hb.timer"},
		})
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := installServiceManagerFake(t)
			dir := t.TempDir()
			units := hbUnits(dir)
			if tc.fixture != nil {
				tc.fixture(t, units)
			}
			before := snapshotUnitDir(t, dir)
			if tc.disableFails {
				fake.FailOn("systemctl", "disable", errors.New("exit status 1"))
			}

			err := removeSystemdUnits("hb.timer", units, "hb.timer", tc.strict)

			switch {
			case tc.external != "":
				wantExternal(t, err, "hb.timer", units[1].Path, tc.external)
			case tc.errText != "":
				if err == nil || !strings.Contains(err.Error(), tc.errText) {
					t.Fatalf("removeSystemdUnits() = %v, want an error containing %q", err, tc.errText)
				}
			case err != nil:
				t.Fatalf("removeSystemdUnits() = %v, want nil", err)
			}
			wantCalls(t, fake.Calls(), tc.calls...)
			after := snapshotUnitDir(t, dir)
			if left := slices.Sorted(maps.Keys(after)); !slices.Equal(left, tc.left) {
				t.Fatalf("files left = %q, want %q", left, tc.left)
			}
			for name, fingerprint := range after {
				if fingerprint != before[name] {
					t.Fatalf("%s changed: now %q, was %q", name, fingerprint, before[name])
				}
			}
		})
	}
}

// BridgeDaemonExternal and HeartbeatDaemonExternal look at the files the host's
// own service manager uses, so the rows are the host's: launchd's plists on
// macOS, systemd's units on Linux and WSL2.
func TestDaemonExternalProbes_HostPlatform(t *testing.T) {
	type probeRow struct {
		label string
		path  func() (string, error)
		probe func() *ExternalServiceError
	}
	heartbeat := func() *ExternalServiceError { return HeartbeatDaemonExternal("ops") }
	var rows []probeRow
	switch plat := platform.Detect(); plat {
	case platform.PlatformMacOS:
		rows = []probeRow{
			{"com.agentdeck.conductor-bridge", LaunchdPlistPath, BridgeDaemonExternal},
			{"com.agentdeck.conductor-heartbeat.ops", func() (string, error) { return HeartbeatPlistPath("ops") }, heartbeat},
		}
	case platform.PlatformLinux, platform.PlatformWSL2:
		rows = []probeRow{
			{"agent-deck-conductor-bridge.service", SystemdBridgeServicePath, BridgeDaemonExternal},
			// The heartbeat is one pair, named by its timer: either file keeps it.
			{"agent-deck-conductor-heartbeat-ops.timer", func() (string, error) { return SystemdHeartbeatServicePath("ops") }, heartbeat},
			{"agent-deck-conductor-heartbeat-ops.timer", func() (string, error) { return SystemdHeartbeatTimerPath("ops") }, heartbeat},
		}
	default:
		t.Skipf("no launchd or systemd probe on %s", plat)
	}
	for _, kind := range externalUnitKinds {
		t.Run(kind.name, func(t *testing.T) {
			useTempConductorHome(t)
			fake := installServiceManagerFake(t)
			for _, row := range rows {
				path, err := row.path()
				if err != nil {
					t.Fatal(err)
				}
				if ext := row.probe(); ext != nil {
					t.Fatalf("%s: probe with %s missing = %+v, want nil", row.label, path, *ext)
				}
				writeServiceFixture(t, path, "owned", 0o644)
				if ext := row.probe(); ext != nil {
					t.Fatalf("%s: probe with %s owned = %+v, want nil", row.label, path, *ext)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}

				kind.setup(t, path)
				want := ExternalServiceError{Label: row.label, Path: path, Reason: kind.reason}
				if ext := row.probe(); ext == nil || *ext != want {
					t.Fatalf("%s: probe with %s %s = %v, want %+v", row.label, path, kind.name, ext, want)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			wantCalls(t, fake.Calls())
		})
	}

	// A file that cannot be classified (a directory is neither a symlink nor a
	// regular file) is not reported as external: the install that follows reports it.
	t.Run("cannot be checked", func(t *testing.T) {
		useTempConductorHome(t)
		fake := installServiceManagerFake(t)
		for _, row := range rows {
			path, err := row.path()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(path, 0o755); err != nil {
				t.Fatal(err)
			}
			if ext := row.probe(); ext != nil {
				t.Fatalf("%s: probe with a directory at %s = %+v, want nil", row.label, path, *ext)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}
		wantCalls(t, fake.Calls())
	})
}

// A systemd user session is often missing in a container or a VM without
// lingering. The units are written all the same, and each installer keeps the
// error text it always had, with errSystemdUserUnavailable inside for errors.Is.
func TestSystemdDaemons_UnavailableKeepsTodaysErrorWithUnitsInPlace(t *testing.T) {
	useTempConductorHome(t)
	fake := installServiceManagerFake(t)
	stubSystemdUserAvailable(t, func() bool { return false })
	// The bridge's python3 comes from the conductor venv, so the host needs none.
	writeServiceFixture(t, filepath.Join(ConductorVenvDir(), "bin", "python3"), "#!/bin/sh\nexit 0\n", 0o755)
	condDir, err := ConductorDir()
	if err != nil {
		t.Fatal(err)
	}
	opsDir, err := ConductorNameDir("ops")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		install func() (string, error) // the heartbeat's returns no path
		want    string
	}{
		{"bridge", installBridgeDaemonSystemd, "systemd user session not available (common in containers/VMs without lingering); run manually: python3 " + condDir + "/bridge.py"},
		{"notifier", installTransitionNotifierSystemd, "systemd user session not available; run manually: agent-deck notify-daemon"},
		{"heartbeat", func() (string, error) { return "", installHeartbeatDaemonSystemd("ops", 15) }, "systemd user session not available; run heartbeat manually via cron or: bash " + opsDir + "/heartbeat.sh"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path, err := tc.install()
			if path != "" || !errors.Is(err, errSystemdUserUnavailable) || err.Error() != tc.want {
				t.Fatalf("install = (%q, %v), want (%q, an error that wraps errSystemdUserUnavailable and reads %q)", path, err, "", tc.want)
			}
		})
	}

	unitDir, err := SystemdUserDir()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"agent-deck-conductor-bridge.service",
		"agent-deck-conductor-heartbeat-ops.service",
		"agent-deck-conductor-heartbeat-ops.timer",
		"agent-deck-transition-notifier.service",
	}
	// No temp file among them: every unit was renamed into place.
	if got := slices.Sorted(maps.Keys(snapshotUnitDir(t, unitDir))); !slices.Equal(got, want) {
		t.Fatalf("%s holds %q, want %q", unitDir, got, want)
	}
	wantCalls(t, fake.Calls())
}

// TestSystemdDaemons_ExternalUnitsLeftAlone drives the six systemd functions the
// way `conductor setup` and `conductor teardown` do. A unit that belongs to
// another manager must be reported and left exactly as it was, with no
// systemctl call, and with nothing generated or created first: the conductor
// and log directories must not appear. PATH is empty and there is no venv
// python3, since an external unit needs no generating.
func TestSystemdDaemons_ExternalUnitsLeftAlone(t *testing.T) {
	for _, kind := range externalUnitKinds {
		t.Run(kind.name, func(t *testing.T) {
			useTempConductorHome(t)
			t.Setenv("PATH", t.TempDir())
			fake := installServiceManagerFake(t)
			forbidSystemdUserCheck(t)
			must := func(s string, err error) string {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
				return s
			}
			unitDir := must(SystemdUserDir())
			bridge := must(SystemdBridgeServicePath())
			notifier := must(SystemdTransitionNotifierServicePath())
			heartbeatService := must(SystemdHeartbeatServicePath("ops"))
			heartbeatTimer := must(SystemdHeartbeatTimerPath("ops"))
			for _, p := range []string{bridge, notifier, heartbeatTimer} {
				kind.setup(t, p)
			}
			// The heartbeat's service is owned: its timer alone keeps the pair.
			writeServiceFixture(t, heartbeatService, "kept", 0o644)
			before := snapshotUnitDir(t, unitDir)

			steps := []struct {
				name, label, path string
				call              func() error
			}{
				{"install bridge", "agent-deck-conductor-bridge.service", bridge, func() error { _, err := installBridgeDaemonSystemd(); return err }},
				{"install notifier", "agent-deck-transition-notifier.service", notifier, func() error { _, err := installTransitionNotifierSystemd(); return err }},
				{"install heartbeat", "agent-deck-conductor-heartbeat-ops.timer", heartbeatTimer, func() error { return installHeartbeatDaemonSystemd("ops", 15) }},
				{"uninstall bridge", "agent-deck-conductor-bridge.service", bridge, uninstallBridgeDaemonSystemd},
				{"uninstall notifier", "agent-deck-transition-notifier.service", notifier, uninstallTransitionNotifierSystemd},
				{"uninstall heartbeat", "agent-deck-conductor-heartbeat-ops.timer", heartbeatTimer, func() error { return uninstallHeartbeatDaemonSystemd("ops") }},
			}
			for _, step := range steps {
				t.Run(step.name, func(t *testing.T) {
					wantExternal(t, step.call(), step.label, step.path, kind.reason)
				})
			}

			wantCalls(t, fake.Calls())
			if after := snapshotUnitDir(t, unitDir); !maps.Equal(after, before) {
				t.Fatalf("the unit directory changed:\n got %v\nwant %v", after, before)
			}
			for _, dir := range []string{must(ConductorDir()), filepath.Dir(must(logDataPath("transition-notifier.log")))} {
				if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("%s exists (Lstat gave %v), want it never created", dir, err)
				}
			}
		})
	}
}

// The enable that ends an install and the disable that starts an uninstall name
// the unit each daemon is enabled by: the service for the bridge and the
// notifier, the timer for the heartbeat. A failed disable is ignored by the
// bridge's and the notifier's uninstall, which still removes the unit, and keeps
// the heartbeat's pair.
func TestSystemdDaemons_OwnedUnitsInstalledAndRemoved(t *testing.T) {
	tests := []struct {
		name      string
		install   func() error
		uninstall func() error
		unit      string            // what is enabled and disabled
		files     map[string]string // the unit files an install writes, each with a line it must hold
		strict    bool              // a failed disable keeps the files and is returned
	}{
		{
			name:      "bridge",
			install:   func() error { _, err := installBridgeDaemonSystemd(); return err },
			uninstall: uninstallBridgeDaemonSystemd,
			unit:      "agent-deck-conductor-bridge.service",
			files:     map[string]string{"agent-deck-conductor-bridge.service": "[Service]"},
		},
		{
			name:      "notifier",
			install:   func() error { _, err := installTransitionNotifierSystemd(); return err },
			uninstall: uninstallTransitionNotifierSystemd,
			unit:      "agent-deck-transition-notifier.service",
			files:     map[string]string{"agent-deck-transition-notifier.service": "[Service]"},
		},
		{
			name:      "heartbeat",
			install:   func() error { return installHeartbeatDaemonSystemd("ops", 15) },
			uninstall: func() error { return uninstallHeartbeatDaemonSystemd("ops") },
			unit:      "agent-deck-conductor-heartbeat-ops.timer",
			files: map[string]string{
				"agent-deck-conductor-heartbeat-ops.service": "[Service]",
				"agent-deck-conductor-heartbeat-ops.timer":   "[Timer]",
			},
			strict: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			useTempConductorHome(t)
			fake := installServiceManagerFake(t)
			stubSystemdUserAvailable(t, func() bool { return true })
			writeServiceFixture(t, filepath.Join(ConductorVenvDir(), "bin", "python3"), "#!/bin/sh\nexit 0\n", 0o755)
			unitDir, err := SystemdUserDir()
			if err != nil {
				t.Fatal(err)
			}
			enable := "systemctl --user enable --now " + tc.unit
			disable := "systemctl --user disable --now " + tc.unit

			if err := tc.install(); err != nil {
				t.Fatalf("install = %v, want nil", err)
			}
			wantCalls(t, fake.Calls(), enable)
			for name, line := range tc.files {
				data, err := os.ReadFile(filepath.Join(unitDir, name))
				if err != nil || !strings.Contains(string(data), line) {
					t.Fatalf("%s holds %q (read error %v), want a unit with %q", name, data, err, line)
				}
			}

			fake.FailOn("systemctl", "disable", errors.New("exit status 1"))
			err = tc.uninstall()

			left := slices.Sorted(maps.Keys(snapshotUnitDir(t, unitDir)))
			if tc.strict {
				if err == nil || !strings.Contains(err.Error(), "disabling "+tc.unit) {
					t.Fatalf("uninstall = %v, want an error containing %q", err, "disabling "+tc.unit)
				}
				wantCalls(t, fake.Calls(), enable, disable)
				if want := slices.Sorted(maps.Keys(tc.files)); !slices.Equal(left, want) {
					t.Fatalf("unit files left = %q, want every one of %q kept", left, want)
				}
				return
			}
			if err != nil {
				t.Fatalf("uninstall = %v, want nil", err)
			}
			wantCalls(t, fake.Calls(), enable, disable, "systemctl --user daemon-reload")
			if len(left) != 0 {
				t.Fatalf("unit files left = %q, want none", left)
			}
		})
	}
}
