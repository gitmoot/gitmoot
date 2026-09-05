package cli

import (
	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
	"testing"
)

// Test helpers shared with UNTAGGED tests, kept out of the e2e build tag
// (#1760 step 3). Tagging the e2e file beside this one removes it from the
// default build; these symbols are used by tests that are not e2e, so they
// would have stopped compiling. Types are moved WITH their methods.

// heartbeatLoopE2EHome builds an isolated home with an Initialized config + an
// open Store on that home's DB, so the write-side CLI (`agent heartbeat add`,
// which edits the config file), the production heartbeat enqueuer
// (newHeartbeatEnqueuer(store, home)), and the daemon worker tick all share the
// SAME config + store — exactly as the live daemon wires them. Never touches a
// real home.
func heartbeatLoopE2EHome(t *testing.T) (string, config.Paths, *db.Store) {
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

// heartbeatShellResultScript is the SHELL-runtime session body the worker runs as
// `sh -c <script> gitmoot <prompt>`. It ignores its input and echoes a valid
// gitmoot_result with decision "approved" so the ask job runs to a TERMINAL
// succeeded state with NO LLM and NO network — fully deterministic offline.
const heartbeatShellResultScript = `printf '%s' '{"gitmoot_result":{"decision":"approved","summary":"heartbeat ran","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}'`
