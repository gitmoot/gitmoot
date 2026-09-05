package cli

import (
	"context"
	"github.com/gitmoot/gitmoot/internal/db"
	"testing"
)

// Test helpers shared with UNTAGGED tests, kept out of the e2e build tag
// (#1760 step 3). Tagging the e2e file beside this one removes it from the
// default build; these symbols are used by tests that are not e2e, so they
// would have stopped compiling. Types are moved WITH their methods.

// terminalJobDecision returns a terminal job and its parsed gitmoot_result decision.
func terminalJobDecision(t *testing.T, ctx context.Context, store *db.Store, jobID string) (db.Job, string) {
	t.Helper()
	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob(%s): %v", jobID, err)
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatalf("payload(%s): %v", jobID, err)
	}
	decision := ""
	if payload.Result != nil {
		decision = payload.Result.Decision
	}
	return job, decision
}
