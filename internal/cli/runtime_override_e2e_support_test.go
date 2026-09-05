package cli

import (
	"context"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"testing"
)

// Test helpers shared with UNTAGGED tests, kept out of the e2e build tag
// (#1760 step 3). Tagging the e2e file beside this one removes it from the
// default build; these symbols are used by tests that are not e2e, so they
// would have stopped compiling. Types are moved WITH their methods.

func runtimeOverrideE2EHome(t *testing.T) (string, *db.Store, string) {
	t.Helper()
	home, _, store := heartbeatLoopE2EHome(t)
	checkout := createDaemonWorkerGitCheckout(t, "main")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	// Default runtime codex with a stored model: the override must use neither.
	if err := store.UpsertAgent(context.Background(), db.Agent{
		Name:           "maintainer",
		Role:           "worker",
		Runtime:        runtime.CodexRuntime,
		RuntimeRef:     runtimeOverrideCodexRef,
		RepoScope:      "owner/repo",
		Capabilities:   []string{"ask"},
		AutonomyPolicy: runtime.AutonomyPolicyAuto,
		Model:          "gpt-5.5-codex",
		HealthStatus:   "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent: %v", err)
	}
	return home, store, checkout
}

const runtimeOverrideCodexRef = "codex-session-never-invoked"
