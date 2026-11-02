package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// Tests of `conductor migrate-dir` against bridge.py and agents that are not
// Agent Deck's. Each runs the binary with HOME in a temp dir and the recording
// launchctl and systemctl of installRecordingServiceFakes first on PATH, or, for
// a run that reaches no service manager, with TestMain's failing stubs.

// writeMigrateDirFile writes a fixture file at 0o644, creating its directory.
func writeMigrateDirFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, body, 0o644)
}

// runMigrateDirWithFakes runs `conductor migrate-dir args...` with the fakes of
// pathDir first on PATH. Pass the target first and --from with an "=": the
// positional scan takes the first argument that is not a flag as the target, so
// a bare `--from /old /new` swaps the two.
func runMigrateDirWithFakes(t *testing.T, home, pathDir string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	env := []string{"PATH=" + pathDir + string(os.PathListSeparator) + os.Getenv("PATH")}
	return runAgentDeckEnv(t, home, "", env, append([]string{"conductor", "migrate-dir"}, args...)...)
}

// The bridge agent hard-codes the directory a move would leave. With a bridge
// agent that another manager owns (read-only here, as home-manager leaves it),
// the dry-run lists that as a blocker and says the plan would be refused, and
// --apply refuses and exits 1.
func TestConductorMigrateDir_ExternalBridgeAgentRefuses(t *testing.T) {
	home := t.TempDir()
	from := filepath.Join(home, "old-conductor")
	to := filepath.Join(home, "new-conductor")
	// The probe below runs in this process, so it must read the binary's files.
	t.Setenv("HOME", home)
	writeReadOnlyServiceFixtures(t, home, 0o444, session.LaunchdPlistName, setupCLIBridgeUnit)
	br := session.BridgeDaemonExternal()
	if br == nil {
		t.Fatal("the probe reports no external bridge agent for the 0444 fixtures")
	}
	writeMigrateDirFile(t, filepath.Join(from, "bridge.py"), "# hand-edited bridge\n")
	pathDir, logPath := installRecordingServiceFakes(t, false)

	stdout, stderr, code := runMigrateDirWithFakes(t, home, pathDir, to, "--from="+from)
	if code != 0 {
		t.Fatalf("dry-run: exit=%d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	assertStdoutHasLine(t, stdout, "    - "+br.Label+" is managed externally and points at "+from+"; change its manager's configuration first")
	assertStdoutHasLine(t, stdout, "  Note: this plan would be REFUSED as-is — resolve the blockers above first.")

	stdout, stderr, code = runMigrateDirWithFakes(t, home, pathDir, to, "--from="+from, "--apply")
	if code != 1 {
		t.Fatalf("--apply: exit=%d, want 1\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	assertStdoutHasLine(t, stdout, "REFUSED — nothing was moved and [conductor].dir was NOT changed.")

	if calls := mutatingServiceCalls(t, logPath); len(calls) != 0 {
		t.Errorf("migrate-dir changed a service manager: %q", calls)
	}
}

// A no-op (the source is the target) strands nothing, so its dry-run lists no
// blocker and does not call the plan refused, and its apply commits and reloads
// the daemons. The reload leaves an agent that another manager owns alone and
// says so with a [skip] line, not a warning: the heartbeat agent here is
// read-only, and the bridge agent is read-only or a symlink, a dangling one
// included. The apply also reports the bridge.py it kept.
func TestConductorMigrateDir_NoOpApplyReportsKeptBridgeAndExternalAgents(t *testing.T) {
	rows := []struct {
		name   string
		bridge func(t *testing.T, home string) // makes the bridge agent's units external
		reason string                          // why the probe calls the bridge agent external
	}{
		{
			name: "read-only",
			bridge: func(t *testing.T, home string) {
				writeReadOnlyServiceFixtures(t, home, 0o444, session.LaunchdPlistName, setupCLIBridgeUnit)
			},
			reason: "read-only",
		},
		{
			// os.Stat follows a dangling link and finds nothing, so the reload used
			// to skip such a bridge agent without a word.
			name: "dangling",
			bridge: func(t *testing.T, home string) {
				for _, label := range []string{session.LaunchdPlistName, setupCLIBridgeUnit} {
					path := serviceFixturePath(home, label)
					if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(filepath.Join(home, "gone"), path); err != nil {
						t.Fatal(err)
					}
				}
			},
			reason: "symlink",
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			home := t.TempDir()
			// The probes below run in this process, so they must read the binary's files.
			t.Setenv("HOME", home)
			row.bridge(t, home)
			writeReadOnlyServiceFixtures(t, home, 0o444,
				session.HeartbeatPlistLabel("alpha"), session.SystemdHeartbeatServiceName("alpha"), session.SystemdHeartbeatTimerName("alpha"))
			br := session.BridgeDaemonExternal()
			hb := session.HeartbeatDaemonExternal("alpha")
			if br == nil || hb == nil {
				t.Fatalf("the probes report no external agent for the fixtures: bridge=%v heartbeat=%v", br, hb)
			}
			if br.Reason != row.reason {
				t.Fatalf("the bridge agent is external as %q, want %q", br.Reason, row.reason)
			}
			dir := filepath.Join(home, "conductor")
			writeMigrateDirFile(t, filepath.Join(dir, "alpha", "meta.json"),
				`{"name":"alpha","profile":"default","heartbeat_enabled":true,"heartbeat_interval":15}`)
			writeMigrateDirFile(t, filepath.Join(dir, "bridge.py"), "# hand-edited bridge\n")
			pathDir, logPath := installRecordingServiceFakes(t, false)

			// The dry-run of a no-op lists no blocker: nothing moves, so nothing is
			// stranded, and --apply accepts it, as the runs below show.
			stdout, stderr, code := runMigrateDirWithFakes(t, home, pathDir, dir, "--from="+dir)
			if code != 0 {
				t.Fatalf("dry-run: exit=%d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
			}
			for _, banned := range []string{"is managed externally and points at", "would be REFUSED"} {
				if strings.Contains(stdout, banned) {
					t.Errorf("dry-run stdout contains %q for a no-op:\n%s", banned, stdout)
				}
			}

			stdout, stderr, code = runMigrateDirWithFakes(t, home, pathDir, dir, "--from="+dir, "--apply")
			if code != 0 {
				t.Fatalf("exit=%d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
			}
			if strings.Contains(stderr, "failed to reload") {
				t.Errorf("stderr reports a failed reload of an agent that is managed externally:\n%s", stderr)
			}
			for _, line := range []string{"[skip] " + hb.Error(), "[skip] " + br.Error(), "  " + session.BridgeKeptNotice} {
				assertStdoutHasLine(t, stdout, line)
			}
			// What was left alone is neither reinstalled nor reloaded.
			for _, banned := range []string{"[ok] bridge.py reinstalled at new base", "[ok] heartbeat daemons reloaded", "[ok] bridge daemon reloaded"} {
				if strings.Contains(stdout, banned) {
					t.Errorf("stdout reports %q, but that was left alone:\n%s", banned, stdout)
				}
			}

			// Under --json stdout stays one JSON document, so the [skip] lines go to stderr.
			stdout, stderr, code = runMigrateDirWithFakes(t, home, pathDir, dir, "--from="+dir, "--apply", "--json")
			if code != 0 {
				t.Fatalf("--json: exit=%d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
			}
			if got := string(setupCLIJSON(t, stdout)["bridge_script"]); got != `"kept"` {
				t.Errorf("bridge_script = %s, want %q", got, "kept")
			}
			for _, line := range []string{"[skip] " + hb.Error(), "[skip] " + br.Error()} {
				if !strings.Contains(stderr, line) {
					t.Errorf("--json stderr lacks %q\nstderr: %s", line, stderr)
				}
			}

			if calls := mutatingServiceCalls(t, logPath); len(calls) != 0 {
				t.Errorf("migrate-dir changed a service manager: %q", calls)
			}
		})
	}
}

// A dry-run whose only blockers are destinations that already exist keeps the
// note that --apply --force merges them. A dry-run reloads nothing, whatever
// the plan holds, so it reaches no service manager; TestMain's failing stubs are
// only a backstop.
func TestConductorMigrateDir_SkipOnlyDryRunKeepsForceNote(t *testing.T) {
	home := t.TempDir()
	from := filepath.Join(home, "old-conductor")
	to := filepath.Join(home, "new-conductor")
	for _, base := range []string{from, to} {
		writeMigrateDirFile(t, filepath.Join(base, "alpha", "meta.json"), `{"name":"alpha","profile":"default"}`)
	}

	stdout, stderr, code := runAgentDeck(t, home, "conductor", "migrate-dir", to, "--from="+from)
	if code != 0 {
		t.Fatalf("exit=%d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	assertStdoutHasLine(t, stdout, "  Note: some destinations already exist; re-run with --apply --force to merge them.")
	if strings.Contains(stdout, "would be REFUSED") {
		t.Errorf("stdout calls a skip-only plan REFUSED:\n%s", stdout)
	}
}

// A dry-run whose blockers are not all destinations that already exist says the
// plan would be REFUSED: --force merges a destination that exists and clears no
// other blocker, so the note that offers --apply --force would mislead. Here
// alpha exists at both ends, a skip-exists blocker, and its heartbeat agent is
// read-only, a second blocker for a home whose action is not a move. The
// recording fakes see no mutating call, since a dry-run reloads nothing.
func TestConductorMigrateDir_SkipAndExternalDryRunSaysRefused(t *testing.T) {
	home := t.TempDir()
	from := filepath.Join(home, "old-conductor")
	to := filepath.Join(home, "new-conductor")
	// The probe below runs in this process, so it must read the binary's files.
	t.Setenv("HOME", home)
	writeReadOnlyServiceFixtures(t, home, 0o444,
		session.HeartbeatPlistLabel("alpha"), session.SystemdHeartbeatServiceName("alpha"), session.SystemdHeartbeatTimerName("alpha"))
	hb := session.HeartbeatDaemonExternal("alpha")
	if hb == nil {
		t.Fatal("the probe reports no external heartbeat agent for the 0444 fixtures")
	}
	for _, base := range []string{from, to} {
		writeMigrateDirFile(t, filepath.Join(base, "alpha", "meta.json"), `{"name":"alpha","profile":"default"}`)
	}
	pathDir, logPath := installRecordingServiceFakes(t, false)

	stdout, stderr, code := runMigrateDirWithFakes(t, home, pathDir, to, "--from="+from)
	if code != 0 {
		t.Fatalf("exit=%d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	for _, line := range []string{
		"    - alpha (conductor): destination already exists — re-run with --force to merge, or move the destination aside",
		"    - " + hb.Label + " is managed externally and points at " + from + "; change its manager's configuration first",
		"  Note: this plan would be REFUSED as-is — resolve the blockers above first.",
	} {
		assertStdoutHasLine(t, stdout, line)
	}
	if strings.Contains(stdout, "re-run with --apply --force") {
		t.Errorf("stdout offers --apply --force for a plan that --force does not clear:\n%s", stdout)
	}
	if calls := mutatingServiceCalls(t, logPath); len(calls) != 0 {
		t.Errorf("migrate-dir changed a service manager: %q", calls)
	}
}
