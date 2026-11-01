package session

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
)

// serviceManagerFake answers launchctl and systemctl for the code under test
// without starting a process. It records every call, the failed ones too, so a
// test can assert exactly which service-manager commands a path issued, and it
// scripts the few queries the daemon helpers ask. The seams are package
// variables, so a test that installs it never calls t.Parallel().
type serviceManagerFake struct {
	mu       sync.Mutex
	calls    []serviceManagerCall
	failOn   map[string]error   // tool+" "+verb: the error of every matching call
	failNext map[string][]error // tool+" "+verb: the errors of the next matching calls, oldest first
	loaded   map[string]bool    // launchd labels and systemd units that read as loaded
}

type serviceManagerCall struct {
	tool string
	args []string
}

func (c serviceManagerCall) String() string {
	return strings.Join(append([]string{c.tool}, c.args...), " ")
}

// installServiceManagerFake points runLaunchctl, launchctlOutput and
// runSystemctl at a new fake until the test ends. A test that can reach one of
// them calls it first: serviceManagerCommand panics in a test binary when a
// mutating verb would reach a real tool.
func installServiceManagerFake(t *testing.T) *serviceManagerFake {
	t.Helper()
	f := &serviceManagerFake{
		failOn:   map[string]error{},
		failNext: map[string][]error{},
		loaded:   map[string]bool{},
	}
	origRunLaunchctl, origLaunchctlOutput, origRunSystemctl := runLaunchctl, launchctlOutput, runSystemctl
	t.Cleanup(func() {
		runLaunchctl, launchctlOutput, runSystemctl = origRunLaunchctl, origLaunchctlOutput, origRunSystemctl
	})
	runLaunchctl = func(args ...string) error {
		_, err := f.call("launchctl", args)
		return err
	}
	launchctlOutput = func(args ...string) ([]byte, error) { return f.call("launchctl", args) }
	runSystemctl = func(args ...string) error {
		_, err := f.call("systemctl", args)
		return err
	}
	return f
}

// call records the command, then answers it: a queued FailNext error for its
// verb (dequeued), else its FailOn error, else the scripted query answers, else
// success with no output.
func (f *serviceManagerFake) call(tool string, args []string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, serviceManagerCall{tool: tool, args: slices.Clone(args)})

	key := tool + " " + serviceVerb(args)
	if queue := f.failNext[key]; len(queue) > 0 {
		f.failNext[key] = queue[1:]
		return nil, queue[0]
	}
	if err, ok := f.failOn[key]; ok {
		return nil, err
	}
	switch key {
	case "launchctl managername":
		// The domain of a login terminal: not the Background domain of a
		// daemon-spawned process, which the notifier guard keys on.
		return []byte("Aqua\n"), nil
	case "launchctl list":
		if len(args) > 1 {
			label := args[len(args)-1]
			if !f.loaded[label] {
				return nil, fmt.Errorf("launchctl list %s: not loaded", label)
			}
			return []byte(label + "\n"), nil
		}
	case "systemctl is-active":
		unit := args[len(args)-1]
		if !f.loaded[unit] {
			return nil, fmt.Errorf("%s inactive", unit)
		}
	}
	return nil, nil
}

// FailOn makes every call of tool's verb (its first argument that is not a
// flag) return err. err must not be nil.
func (f *serviceManagerFake) FailOn(tool, verb string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failOn[tool+" "+verb] = err
}

// FailNext makes the next call of tool's verb return err, and only that one.
// Several FailNext calls for one verb fail that many calls, in order. err must
// not be nil.
func (f *serviceManagerFake) FailNext(tool, verb string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := tool + " " + verb
	f.failNext[key] = append(f.failNext[key], err)
}

// SetLoaded marks a launchd label or a systemd unit as loaded or not, which is
// what "launchctl list <label>" and "systemctl is-active <unit>" report.
func (f *serviceManagerFake) SetLoaded(label string, loaded bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loaded[label] = loaded
}

// Calls returns every command line the fake saw, in order.
func (f *serviceManagerFake) Calls() []string {
	return f.lines(func(serviceManagerCall) bool { return true })
}

// MutatingCalls returns the calls whose verb is outside the query allow-list
// (isMutatingServiceVerb): the ones that would change a real service manager.
func (f *serviceManagerFake) MutatingCalls() []string {
	return f.lines(func(c serviceManagerCall) bool { return isMutatingServiceVerb(c.tool, c.args) })
}

func (f *serviceManagerFake) lines(keep func(serviceManagerCall) bool) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		if keep(c) {
			out = append(out, c.String())
		}
	}
	return out
}
