package main

import "testing"

// Upstream persists the live Codex identity at launch, output, archive and
// stop by writing tool_data (#2396, #2400). The fork rebuilds that key from
// the Codex runtime binding on every load and drops it on every save, so each
// of those paths must publish the runtime binding instead, and nothing that
// runs afterwards may lose it.

// A fake Codex that holds its thread before launch's identity wait ends is
// bound by launch itself, and the binding survives whatever the operator runs
// next.
func TestCodexLaunchPublishesTheLiveThreadAsItsRuntimeBinding(t *testing.T) {
	for _, c := range []struct {
		name string
		then [][]string // commands run after launch; "ID" is the session id
	}{
		{name: "archive", then: [][]string{{"session", "archive", "ID", "--json"}}},
		{name: "list then archive", then: [][]string{{"list", "--json"}, {"session", "archive", "ID", "--json"}}},
		{name: "output", then: [][]string{{"session", "output", "ID", "--json"}}},
		{name: "stop", then: [][]string{{"session", "stop", "ID", "--json"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := launchFakeCodex(t, false)
			if got := durableCodexBinding(t, f.home, f.id); got != codexFirstTurnThread {
				t.Fatalf("Codex binding after launch = %q, want the live thread %q", got, codexFirstTurnThread)
			}
			for _, command := range c.then {
				args := append([]string(nil), command...)
				for n, arg := range args {
					if arg == "ID" {
						args[n] = f.id
					}
				}
				f.run(t, args...)
				if got := durableCodexBinding(t, f.home, f.id); got != codexFirstTurnThread {
					t.Fatalf("Codex binding after %v = %q, want %q", command, got, codexFirstTurnThread)
				}
			}
		})
	}
}

// A composer that takes its thread only after launch stopped waiting has no
// pane CODEX_SESSION_ID for stop's tmux sync to read. Stop binds the live
// thread before the kill destroys the only evidence of it.
func TestCodexStopBindsAThreadTakenAfterLaunch(t *testing.T) {
	for _, c := range []struct {
		name string
		env  []string
	}{
		{name: "legacy stop", env: []string{envCoreRegistry + "=0"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := launchFakeCodex(t, true)
			f.openGate(t)

			f.runEnv(t, c.env, "session", "stop", f.id, "--json")

			if got := durableCodexBinding(t, f.home, f.id); got != codexFirstTurnThread {
				t.Fatalf("Codex binding after stop = %q, want the live thread %q", got, codexFirstTurnThread)
			}
		})
	}
}
