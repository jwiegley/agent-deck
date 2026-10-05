package main

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Status probes keep a queued session queued (operator intent) instead of
// reporting its absent tmux session as error, so the per-status counts need a
// queued bucket: without one, `status` and `group list` count the session in
// their totals and in no bucket. These tests run the built binary against the
// goldens fixture, whose golden-sess-6 is queued in group backend/api.

// TestStatusCountsEverySessionOnce: the `status --json` buckets and the
// `status -v` sections each add up to the total, with golden-sess-6 queued.
func TestStatusCountsEverySessionOnce(t *testing.T) {
	bin := goldensBinary(t)
	home, env := goldensSandbox(t)

	stdout, stderr, exit := runGoldensStreamsIn(t, bin, env, home, []string{"-p", goldensProfile, "status", "--json"})
	if exit != 0 {
		t.Fatalf("status --json: exit %d\n%s\n%s", exit, stdout, stderr)
	}
	var counts map[string]int
	if err := json.Unmarshal([]byte(stdout), &counts); err != nil {
		t.Fatalf("status --json: %v\n%s", err, stdout)
	}
	sum := 0
	for key, n := range counts {
		if key != "total" {
			sum += n
		}
	}
	if counts["queued"] != 1 || counts["total"] != 6 || sum != counts["total"] {
		t.Errorf("status --json = %s: want queued 1 and buckets summing to total 6 (sum %d)", strings.TrimSpace(stdout), sum)
	}

	stdout, stderr, exit = runGoldensStreamsIn(t, bin, env, home, []string{"-p", goldensProfile, "status", "-v"})
	if exit != 0 {
		t.Fatalf("status -v: exit %d\n%s\n%s", exit, stdout, stderr)
	}
	sections := regexp.MustCompile(`(?m)^([A-Z]+) \((\d+)\):$`).FindAllStringSubmatch(stdout, -1)
	listed := 0
	for _, s := range sections {
		n, _ := strconv.Atoi(s[2])
		listed += n
	}
	if !strings.Contains(stdout, "QUEUED (1):\n  ○ codex queued") || !strings.Contains(stdout, "Total: 6 sessions") || listed != 6 {
		t.Errorf("status -v sections list %d of 6 sessions, want a QUEUED section with codex queued:\n%s", listed, stdout)
	}
}

// TestQueuedGlyphAgreesBetweenStatusAndShow: `session show` draws a queued
// session with the same glyph as its row in the `status -v` QUEUED section,
// not StatusSymbol's "?" for an unknown status.
func TestQueuedGlyphAgreesBetweenStatusAndShow(t *testing.T) {
	bin := goldensBinary(t)
	home, env := goldensSandbox(t)

	stdout, stderr, exit := runGoldensStreamsIn(t, bin, env, home, []string{"-p", goldensProfile, "status", "-v"})
	if exit != 0 {
		t.Fatalf("status -v: exit %d\n%s\n%s", exit, stdout, stderr)
	}
	row := regexp.MustCompile(`(?m)^QUEUED \(1\):\n  (\S+) codex queued`).FindStringSubmatch(stdout)
	if row == nil {
		t.Fatalf("status -v has no QUEUED row for codex queued:\n%s", stdout)
	}
	// Both surfaces take the glyph from StatusSymbol, so agreement alone
	// would also hold if StatusSymbol lost its queued case and both drew
	// "?". Pin the glyph the QUEUED section has always drawn.
	if row[1] != "○" {
		t.Fatalf("status -v draws queued as %q, want %q", row[1], "○")
	}

	stdout, stderr, exit = runGoldensStreamsIn(t, bin, env, home, []string{"-p", goldensProfile, "session", "show", "golden-sess-6"})
	if exit != 0 {
		t.Fatalf("session show: exit %d\n%s\n%s", exit, stdout, stderr)
	}
	if want := "Status:  " + row[1] + " queued\n"; !strings.Contains(stdout, want) {
		t.Errorf("session show golden-sess-6 lacks %q (status -v draws queued as %q):\n%s", want, row[1], stdout)
	}
}

// TestGroupListCountsEverySessionOnce: every group's `group list --json`
// status buckets add up to its session_count, through the registry path and
// the legacy handler (whose builder the remote agent's change probe shares).
func TestGroupListCountsEverySessionOnce(t *testing.T) {
	bin := goldensBinary(t)
	home, env := goldensSandbox(t)

	type node struct {
		Path         string         `json:"path"`
		SessionCount int            `json:"session_count"`
		Status       map[string]int `json:"status"`
		Children     []node         `json:"children"`
	}
	for _, path := range []struct {
		name string
		env  []string
	}{
		{"registry", env},
		{"legacy", append(append([]string{}, env...), envCoreRegistry+"=0")},
	} {
		stdout, stderr, exit := runGoldensStreamsIn(t, bin, path.env, home, []string{"-p", goldensProfile, "group", "list", "--json"})
		if exit != 0 {
			t.Fatalf("%s group list --json: exit %d\n%s\n%s", path.name, exit, stdout, stderr)
		}
		var listed struct {
			Groups []node `json:"groups"`
		}
		if err := json.Unmarshal([]byte(stdout), &listed); err != nil {
			t.Fatalf("%s group list --json: %v\n%s", path.name, err, stdout)
		}
		queued := map[string]int{}
		var walk func([]node)
		walk = func(nodes []node) {
			for _, g := range nodes {
				sum := 0
				for _, n := range g.Status {
					sum += n
				}
				if sum != g.SessionCount {
					t.Errorf("%s: group %s buckets %v sum to %d, want session_count %d", path.name, g.Path, g.Status, sum, g.SessionCount)
				}
				queued[g.Path] = g.Status["queued"]
				walk(g.Children)
			}
		}
		walk(listed.Groups)
		if queued["backend"] != 1 || queued["backend/api"] != 1 || queued["my-sessions"] != 0 {
			t.Errorf("%s: queued counts %v, want golden-sess-6 in backend and backend/api only", path.name, queued)
		}
	}
}
