package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"al.essio.dev/pkg/shellescape"
)

// Fixtures for the tests that run the agent-deck binary through `conductor
// setup`, `teardown` and `migrate-dir`: service definitions another manager
// owns, and launchctl and systemctl fakes that record what the binary asked of
// them. The binary runs with HOME in a temp dir, but a temp HOME does not
// isolate launchd or systemd --user, so no test may leave a real launchctl or
// systemctl on its PATH.

// serviceQueryVerbs lists, per tool, the verbs that only read service-manager
// state. With serviceFlagsWithoutValue it copies the rule of internal/session's
// isMutatingServiceVerb, which is unexported there, so package main cannot call
// it. Keep the copy in step with internal/session/service_manager.go.
// mutatingServiceCalls treats every other call as a change.
var serviceQueryVerbs = map[string][]string{
	"launchctl": {"list", "print", "print-disabled", "managername"},
	"systemctl": {"is-active", "is-enabled", "is-failed", "is-system-running", "show", "cat", "status", "list-units", "list-unit-files", "list-timers"},
}

// serviceFlagsWithoutValue lists the options, none taking a value, that a query
// may have before its verb. Any other argument there makes the call a change:
// -p or -H may take the next argument as its value, which would then be read as
// the verb, and anything unlisted (--property=..., a bare --) fails closed.
var serviceFlagsWithoutValue = []string{"--user", "--system", "--global", "--quiet", "-q", "--no-pager", "--no-block", "--no-legend", "--now"}

// serviceFixturePath returns where the binary, run with HOME=home, keeps the
// definition of the service called label. A label ending in .service or .timer
// is a systemd unit in <home>/.config/systemd/user, and any other label is a
// launchd label whose plist is in <home>/Library/LaunchAgents.
func serviceFixturePath(home, label string) string {
	if strings.HasSuffix(label, ".service") || strings.HasSuffix(label, ".timer") {
		return filepath.Join(home, ".config", "systemd", "user", label)
	}
	return filepath.Join(home, "Library", "LaunchAgents", label+".plist")
}

// writeReadOnlyServiceFixtures writes a placeholder service definition for each
// label under home, at its serviceFixturePath, and sets its mode, as
// home-manager leaves a launch agent (0444). Callers pass the labels of both
// platforms: the binary reads those of its host. It returns the paths in label
// order.
func writeReadOnlyServiceFixtures(t *testing.T, home string, mode os.FileMode, labels ...string) []string {
	t.Helper()
	paths := make([]string, 0, len(labels))
	for _, label := range labels {
		path := serviceFixturePath(home, label)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("creating the directory of the %s fixture: %v", label, err)
		}
		if err := os.WriteFile(path, []byte("fixture\n"), 0o644); err != nil {
			t.Fatalf("writing the %s fixture: %v", label, err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatalf("setting the mode of the %s fixture: %v", label, err)
		}
		paths = append(paths, path)
	}
	return paths
}

// serviceFakeTemplate is the /bin/sh script of one fake service manager. It
// takes the tool name, the log path (already shell-quoted), what to do for a
// query that asks whether a service is loaded or active, and the case arm, empty
// or ending in a newline, that makes some verbs fail.
const serviceFakeTemplate = `#!/bin/sh
printf '%%s\n' "%[1]s $*" >> %[2]s
verb=
for arg in "$@"; do
	case "$arg" in
	-*) ;;
	*) verb=$arg; break ;;
	esac
done
case "$verb" in
%[4]slist | is-active)
	%[3]s
	;;
esac
exit 0
`

// installRecordingServiceFakes writes a launchctl and a systemctl to a new temp
// dir, pathDir, and returns it with the log they append to. Each call is one
// line, "<tool> <arguments>". A call whose verb (its first argument that does
// not start with "-") is one of failVerbs exits 1, as the tools do when they
// refuse it. Otherwise a call whose verb is list or is-active prints its
// arguments and exits 0 when listLoaded, else exits 1, as the tools do for a
// service that is or is not loaded; any other verb exits 0. With listLoaded and
// the failVerbs "load" and "enable", a service is running that cannot be
// installed again.
//
// Callers put pathDir first on the PATH of the binary they run:
//
//	"PATH=" + pathDir + string(os.PathListSeparator) + os.Getenv("PATH")
//
// agentDeckTestEnv appends extraEnv last and os/exec keeps the last duplicate
// key, so the fakes shadow the failing stubs TestMain puts on PATH
// (stubServiceManagers). Behind the fakes are those stubs, never a real tool.
func installRecordingServiceFakes(t *testing.T, listLoaded bool, failVerbs ...string) (pathDir, logPath string) {
	t.Helper()
	pathDir = t.TempDir()
	logPath = filepath.Join(t.TempDir(), "service.log")
	loaded := "exit 1"
	if listLoaded {
		loaded = `printf '%s\n' "$*"; exit 0`
	}
	failing := ""
	if len(failVerbs) > 0 {
		failing = strings.Join(failVerbs, " | ") + ")\n\texit 1\n\t;;\n"
	}
	for _, tool := range serviceManagerCommands {
		script := fmt.Sprintf(serviceFakeTemplate, tool, shellescape.Quote(logPath), loaded, failing)
		if err := os.WriteFile(filepath.Join(pathDir, tool), []byte(script), 0o755); err != nil {
			t.Fatalf("writing the %s fake: %v", tool, err)
		}
	}
	return pathDir, logPath
}

// serviceCallLines returns the non-empty lines of the log that
// installRecordingServiceFakes made: nil when nothing was called.
func serviceCallLines(t *testing.T, logPath string) []string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("reading the service call log: %v", err)
	}
	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// mutatingServiceCalls returns the logged calls that could change a service
// manager. A call is a query only when its verb (its first argument that does
// not start with "-") is one of the tool's serviceQueryVerbs and every argument
// before the verb is in serviceFlagsWithoutValue. It fails closed: an unknown
// tool, a call without a verb, an unknown verb and an option that may take a
// value before the verb (`-p status stop x` would otherwise read as the query
// status) all count as changes.
func mutatingServiceCalls(t *testing.T, logPath string) []string {
	t.Helper()
	var mutating []string
	for _, line := range serviceCallLines(t, logPath) {
		if !isServiceQuery(line) {
			mutating = append(mutating, line)
		}
	}
	return mutating
}

// isServiceQuery reports whether one logged call, "<tool> <arguments>", only
// reads service-manager state.
func isServiceQuery(line string) bool {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return false
	}
	queries, ok := serviceQueryVerbs[fields[0]]
	if !ok {
		return false
	}
	args := fields[1:]
	verb := slices.IndexFunc(args, func(arg string) bool { return !strings.HasPrefix(arg, "-") })
	if verb < 0 || !slices.Contains(queries, args[verb]) {
		return false
	}
	for _, arg := range args[:verb] {
		if !slices.Contains(serviceFlagsWithoutValue, arg) {
			return false
		}
	}
	return true
}

func TestMutatingServiceCalls_Classifies(t *testing.T) {
	rows := []struct {
		line     string
		mutating bool
	}{
		{"launchctl list x", false},
		{"systemctl --user is-active --quiet x", false},
		// -p takes the next argument as its value, so it must not be read as a
		// query verb: this call stops x.
		{"systemctl --user -p status stop x", true},
		{"launchctl unload p", true},
		{"systemctl --user enable --now x", true},
	}
	var lines, want []string
	for _, row := range rows {
		lines = append(lines, row.line)
		if row.mutating {
			want = append(want, row.line)
		}
	}
	logPath := filepath.Join(t.TempDir(), "service.log")
	if err := os.WriteFile(logPath, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := mutatingServiceCalls(t, logPath); !slices.Equal(got, want) {
		t.Errorf("mutating calls = %q, want %q", got, want)
	}
}
