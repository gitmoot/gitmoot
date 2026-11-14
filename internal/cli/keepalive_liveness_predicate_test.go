package cli

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// seedTTLLivenessJob creates a job in the given state.
func seedTTLLivenessJob(t *testing.T, store *db.Store, id, state string) {
	t.Helper()
	encoded, err := json.Marshal(workflow.JobPayload{})
	if err != nil {
		t.Fatalf("Marshal(payload) returned error: %v", err)
	}
	if err := store.CreateJob(context.Background(), db.Job{
		ID: id, Type: "review", Agent: "reviewer", State: state, Payload: string(encoded),
	}); err != nil {
		t.Fatalf("CreateJob(%q) returned error: %v", id, err)
	}
}

// TestShouldRenewSandboxTTLStopsForFinishedJobs pins the predicate that the
// whole #1539 amplification fix depends on.
//
// The keepalive tests inject a bool; this asserts the real function computes the
// RIGHT bool from job state. Without it the fix is only as good as my assumption
// about what the store says — the wrong-depth error this work has already hit
// three times.
func TestShouldRenewSandboxTTLStopsForFinishedJobs(t *testing.T) {
	t.Run("running job renews", func(t *testing.T) {
		store := openExecBackendLedgerTestStore(t)
		seedTTLLivenessJob(t, store, "job-live", string(workflow.JobRunning))
		if !(jobWorker{Store: store}).shouldRenewSandboxTTL("job-live") {
			t.Fatal("a running job must keep its sandbox alive")
		}
	})

	t.Run("job that left running does not renew", func(t *testing.T) {
		for _, state := range []string{
			string(workflow.JobFailed), string(workflow.JobSucceeded), string(workflow.JobCancelled),
		} {
			store := openExecBackendLedgerTestStore(t)
			seedTTLLivenessJob(t, store, "job-"+state, state)
			if (jobWorker{Store: store}).shouldRenewSandboxTTL("job-" + state) {
				t.Fatalf("state %q still renewed: nothing is waiting on that sandbox", state)
			}
		}
	})
}

// TestShouldRenewSandboxTTLFailsOpen pins the deliberate choice that an
// unanswerable store renews.
//
// Cutting a healthy review's sandbox short on a transient read error is worse
// than a few extra billed minutes, and the requested TTL still bounds the total.
// Round 1 review of #2229 measured the worst case as one extra ~50-minute cycle.
func TestShouldRenewSandboxTTLFailsOpen(t *testing.T) {
	t.Run("no store", func(t *testing.T) {
		if !(jobWorker{}).shouldRenewSandboxTTL("anything") {
			t.Fatal("a worker with no store refused to renew; it must fail open")
		}
	})

	t.Run("unknown job", func(t *testing.T) {
		store := openExecBackendLedgerTestStore(t)
		if !(jobWorker{Store: store}).shouldRenewSandboxTTL("job-that-does-not-exist") {
			t.Fatal("an unreadable job refused to renew; it must fail open")
		}
	})
}
