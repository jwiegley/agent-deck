package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// TestStatusJSONShapeDocsMatchOutput: conductors triage with the compact
// `status --json` counts, so every place that spells out that shape for them
// must list exactly the keys the command prints, in its order. When status
// probes started keeping queued sessions queued, the output gained a queued
// count that the generated conductor instructions, the shipped conductor
// prompts and the capability checklist all went on omitting.
func TestStatusJSONShapeDocsMatchOutput(t *testing.T) {
	bin := goldensBinary(t)
	home, env := goldensSandbox(t)

	stdout, stderr, exit := runGoldensStreamsIn(t, bin, env, home, []string{"-p", goldensProfile, "status", "--json"})
	if exit != 0 {
		t.Fatalf("status --json: exit %d\n%s\n%s", exit, stdout, stderr)
	}
	keys := orderedJSONObjectKeys(t, stdout)
	quoted := make([]string, len(keys))
	for i, key := range keys {
		quoted[i] = fmt.Sprintf("%q: N", key)
	}
	shape := "{" + strings.Join(quoted, ", ") + "}"

	documented := map[string]string{}
	root := filepath.Join("..", "..")
	for _, rel := range []string{
		"conductor/conductor-claude.md",
		"conductor/conductor-hermes.md",
		"internal/agents/testdata/conductor/CLAUDE.md",
	} {
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		documented[rel] = string(data)
	}
	// The generated shared instructions, as a conductor setup writes them.
	if err := session.InstallSharedConductorInstructions(session.ConductorAgentClaude, ""); err != nil {
		t.Fatal(err)
	}
	dir, err := session.ConductorDir()
	if err != nil {
		t.Fatal(err)
	}
	generated, err := os.ReadFile(filepath.Join(dir, "CLAUDE.md"))
	if err != nil {
		t.Fatal(err)
	}
	documented["generated shared conductor instructions"] = string(generated)

	shapes := regexp.MustCompile(`\{"waiting": N[^}]*\}`)
	for where, text := range documented {
		found := shapes.FindAllString(text, -1)
		if len(found) == 0 {
			t.Errorf("%s no longer documents the status --json shape", where)
		}
		for _, got := range found {
			if got != shape {
				t.Errorf("%s documents status --json as %s, but it prints %s", where, got, shape)
			}
		}
	}

	checklist, err := os.ReadFile(filepath.Join(root, "docs", "verification", "CAPABILITY-CHECKLIST.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := "--json {" + strings.Join(keys, ",") + "}"
	if got := regexp.MustCompile(`--json \{waiting,[a-z,]*\}`).FindString(string(checklist)); got != want {
		t.Errorf("CAPABILITY-CHECKLIST.json notes status as %q, want %q", got, want)
	}
}

// orderedJSONObjectKeys returns the top-level keys of one JSON object in the
// order they appear.
func orderedJSONObjectKeys(t *testing.T, raw string) []string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("status --json is not an object: %v %v\n%s", tok, err, raw)
	}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("status --json: %v\n%s", err, raw)
		}
		keys = append(keys, tok.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatalf("status --json: %v\n%s", err, raw)
		}
	}
	return keys
}
