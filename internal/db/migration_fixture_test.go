package db

import (
	"strings"
	"testing"
)

// migrationsBefore returns every migration up to but excluding the one whose SQL
// contains marker.
//
// Fixtures MUST locate their target migration BY CONTENT, never by tail index.
// migrations is an append-only positional slice shared with every in-flight
// branch, so migrations[:len(migrations)-1] stops omitting the migration a test
// is about the moment anything else is appended -- the migration-tail-collision
// class (see AGENTS.local.md, the repo-backfill locator in store_test.go, and
// TestTaskDisposalMigrationIsAppendOnlyTail, which records the same lesson after
// #1407's append tripped it).
//
// Found again by g7-review during #1550: four tests whose fixtures used
// migrations[:len(migrations)-1] had their advertised migration ALREADY APPLIED,
// because the canary sits at index 38, autonomy_policy at 24, worktree_path at 25
// and root_killed at 34 of 115. They passed while exercising nothing, and were
// invisible to a mutation that removed real Open from the whole package.
//
// Fail-closed on a missing OR ambiguous marker: a fixture that silently omits the
// wrong migration is precisely the defect this helper replaces, so it must never
// guess.
func migrationsBefore(t *testing.T, marker string) []string {
	t.Helper()
	found := -1
	for i, migration := range migrations {
		if !strings.Contains(migration, marker) {
			continue
		}
		if found != -1 {
			t.Fatalf("marker %q matches migrations %d and %d: choose a marker unique to the target migration", marker, found, i)
		}
		found = i
	}
	if found == -1 {
		t.Fatalf("no migration contains %q: the target migration was renamed or removed", marker)
	}
	return migrations[:found]
}
