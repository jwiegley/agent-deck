package session

import (
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// Seams over the host service managers. Every launchctl and systemctl call of
// the daemon helpers goes through one of them, so a test swaps them for
// installServiceManagerFake and never reaches a real tool. That matters: a
// temporary HOME does not isolate launchd, and a real
// `launchctl unload <tmp>/com.agentdeck.conductor-bridge.plist` acts on the
// live agent with that label, wherever the plist file sits.
var (
	runLaunchctl    = func(args ...string) error { return serviceManagerCommand("launchctl", args...).Run() }
	launchctlOutput = func(args ...string) ([]byte, error) { return serviceManagerCommand("launchctl", args...).Output() }
	runSystemctl    = func(args ...string) error { return serviceManagerCommand("systemctl", args...).Run() }
)

// serviceManagerFakeRoot returns the directory a test binary's launchctl or
// systemctl must live under for serviceManagerCommand to run it with a
// mutating verb. The fakes that stand in for the real tools live there:
// cmd/agent-deck's stubServiceManagers (os.MkdirTemp("", ...)) and the
// t.TempDir() PATH fakes (Go 1.26 creates those under $GOTMPDIR when it is set;
// scripts/test.sh and CI leave it unset). A variable so the guard's own test
// can point it elsewhere.
var serviceManagerFakeRoot = os.TempDir

// serviceQueryVerbs lists, per tool, the verbs that only read service-manager
// state. isMutatingServiceVerb treats every other verb, and every other tool,
// as one that changes it.
var serviceQueryVerbs = map[string][]string{
	"launchctl": {"list", "print", "print-disabled", "managername"},
	"systemctl": {"is-active", "is-enabled", "is-failed", "is-system-running", "show", "cat", "status", "list-units", "list-unit-files", "list-timers"},
}

// serviceFlagsWithoutValue lists the options, none taking a value, that
// isMutatingServiceVerb accepts before a query verb. Any other argument there
// makes the call mutating: -p or -H may take the next argument as its value,
// which serviceVerb would read as the verb, and anything else unlisted
// (--property=..., a bare --) fails closed.
var serviceFlagsWithoutValue = []string{"--user", "--system", "--global", "--quiet", "-q", "--no-pager", "--no-block", "--no-legend", "--now"}

// serviceVerb returns the verb of a launchctl or systemctl command line: its
// first argument that is not a flag, or "" when it has none.
func serviceVerb(args []string) string {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return ""
}

// isMutatingServiceVerb reports whether running name with args could change a
// service manager. It fails closed: a call is a query only when its verb is on
// the tool's query allow-list and every argument before the verb is in
// serviceFlagsWithoutValue. So an unknown verb, a command line with no verb, an
// option that may take a value before the verb (`-p status stop x.service`
// would otherwise read as the query status) and a tool without an allow-list
// all are mutating.
func isMutatingServiceVerb(name string, args []string) bool {
	queries, ok := serviceQueryVerbs[name]
	if !ok {
		return true
	}
	verb := serviceVerb(args)
	if !slices.Contains(queries, verb) {
		return true
	}
	for _, a := range args[:slices.Index(args, verb)] {
		if !slices.Contains(serviceFlagsWithoutValue, a) {
			return true
		}
	}
	return false
}

// serviceManagerCommand builds the command for one launchctl or systemctl call.
// In a test binary (testing.Testing(), as internal/sandboxguard decides it) it
// refuses a call that isMutatingServiceVerb reports, unless the tool resolves,
// symlinks followed, to a fake under serviceManagerFakeRoot(); the panic names
// the call and where the tool really is. A call that passes runs the path that
// was checked. Anywhere else it is exec.Command.
func serviceManagerCommand(name string, args ...string) *exec.Cmd {
	if testing.Testing() && isMutatingServiceVerb(name, args) {
		resolved, err := exec.LookPath(name)
		if err != nil || !withinBase(resolved, serviceManagerFakeRoot()) {
			cmdline := strings.Join(append([]string{name}, args...), " ")
			panic(fmt.Sprintf("service manager guard: refusing to run %q in a test binary: %s resolves to %q, not a fake under %q; install a fake through the seam (installServiceManagerFake(t)) before reaching this path", cmdline, name, resolved, serviceManagerFakeRoot()))
		}
		// exec.Command(name, ...) would look PATH up a second time, and a PATH
		// that changed since the check could name a binary nobody checked.
		cmd := exec.Command(resolved, args...) //nolint:gosec // G702: resolved is exec.LookPath of the fixed tool name the seams pass (launchctl or systemctl), checked to lie under serviceManagerFakeRoot; no shell
		cmd.Args[0] = name
		return cmd
	}
	return exec.Command(name, args...) //nolint:gosec // G702: the seams pass the fixed name launchctl or systemctl, no shell; args are verbs, flags, plist paths, unit names and launchd labels
}
