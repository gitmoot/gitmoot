package cli

import (
	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
	"testing"
)

// Test helpers shared by untagged and e2e daemon tests.

// daemonE2EHome builds an isolated home with an Initialized config + an open
// Store on that home's DB, so CLI config edits and the daemon worker tick share
// the SAME config + store, exactly as the live daemon wires them. Never touches
// a real home.
func daemonE2EHome(t *testing.T) (string, config.Paths, *db.Store) {
	t.Helper()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return home, paths, store
}

// shellApprovedResultScript is the SHELL-runtime session body the worker runs as
// `sh -c <script> gitmoot <prompt>`. It ignores its input and echoes a valid
// gitmoot_result with decision "approved" so the ask job runs to a TERMINAL
// succeeded state with NO LLM and NO network — fully deterministic offline.
const shellApprovedResultScript = `printf '%s' '{"gitmoot_result":{"decision":"approved","summary":"shell fixture ran","findings":[],"changes_made":[],"tests_run":[],"needs":[]}}'`
