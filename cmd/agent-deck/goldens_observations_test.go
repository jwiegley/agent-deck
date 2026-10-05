package main

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// Fork deviation (testdata/goldens/README.md, "Fork deviations"): a one-pass
// CLI read (`list --json`, `session show`, `status`, `group list`) durably
// commits the status it observes through the single status authority,
// session.Instance.UpdateStatusObserved. Upstream's CLI keeps those
// observations in memory. TestCLIGoldens/safe runs every spec against one
// seeded sandbox, so without a warm-up the first observing spec would commit
// the fixture's tmux-less sessions as error and every later spec's bytes
// would depend on which specs ran before it: a filtered run such as
// -run 'TestCLIGoldens/safe/fleet_status$' would print other bytes than the
// full run.

// goldensObservationWarmup is the read that commits the seeded sandbox's
// observations once, before any golden spec runs. It is not itself a golden.
var goldensObservationWarmup = []string{"-p", goldensProfile, "list", "--json"}

// commitGoldensObservations runs the warm-up against the freshly seeded
// sandbox and then holds the safe specs to the store it leaves: when the
// parent test finishes, the committed runtime statuses must be unchanged.
// A spec that commits a new observation after the warm-up would make the
// goldens order-dependent again, so it fails here instead of in whichever
// golden happens to run next.
func commitGoldensObservations(t *testing.T, bin string, env []string, home string) {
	t.Helper()
	stdout, stderr, exit := runGoldensStreamsIn(t, bin, env, home, goldensObservationWarmup)
	if exit != 0 {
		t.Fatalf("goldens warm-up %v: exit %d\nstdout:\n%s\nstderr:\n%s", goldensObservationWarmup, exit, stdout, stderr)
	}
	committed := goldensCommittedStatuses(t)
	t.Cleanup(func() {
		if got := goldensCommittedStatuses(t); got != committed {
			t.Errorf("a safe spec committed a status observation after the goldens warm-up, so the goldens depend on subtest order:\n--- after warm-up ---\n%s--- after the safe specs ---\n%s", committed, got)
		}
	})
}

// goldensCommittedStatuses renders every durable runtime row of the goldens
// profile (status, generation, status revision), ordered by instance id.
func goldensCommittedStatuses(t *testing.T) string {
	t.Helper()
	profileDir, err := session.GetProfileDir(goldensProfile)
	if err != nil {
		t.Fatalf("resolving goldens profile dir: %v", err)
	}
	db, err := sql.Open("sqlite", filepath.Join(profileDir, "state.db")+"?_pragma=busy_timeout(2000)")
	if err != nil {
		t.Fatalf("opening goldens state.db: %v", err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT instance_id, status, runtime_generation, status_revision
		FROM instance_runtime_state ORDER BY instance_id`)
	if err != nil {
		t.Fatalf("reading goldens runtime state: %v", err)
	}
	defer rows.Close()
	var sb strings.Builder
	for rows.Next() {
		var id, status string
		var generation, revision uint64
		if err := rows.Scan(&id, &status, &generation, &revision); err != nil {
			t.Fatalf("scanning goldens runtime state: %v", err)
		}
		fmt.Fprintf(&sb, "%s status=%s generation=%d revision=%d\n", id, status, generation, revision)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading goldens runtime state: %v", err)
	}
	return sb.String()
}
