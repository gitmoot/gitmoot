package cli

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// seedTTLLivenessJob creates a job in the given state, optionally carrying a
// RootJobID in its payload.
func seedTTLLivenessJob(t *testing.T, store *db.Store, id, state, rootJobID string) {
	t.Helper()
	payload := workflow.JobPayload{RootJobID: rootJobID}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal(payload) returned error: %v", err)
	}
	if err := store.CreateJob(context.Background(), db.Job{
		ID: id, Type: "review", Agent: "reviewer", State: state, Payload: string(encoded),
	}); err != nil {
		t.Fatalf("CreateJob(%q) returned error: %v", id, err)
	}
}

// TestShouldRenewSandboxTTLStopsForKilledAndFinishedJobs pins the predicate that
// the whole #1539 amplification fix depends on.
//
// The keepalive tests inject a bool; this asserts the real function computes the
// RIGHT bool from job state. Without it the fix is only as good as my assumption
// about what the store says — the wrong-depth error this work has already hit
// three times.
func TestShouldRenewSandboxTTLStopsForKilledAndFinishedJobs(t *testing.T) {
	ctx := context.Background()

	t.Run("running job with a live root renews", func(t *testing.T) {
		store := openExecBackendLedgerTestStore(t)
		seedTTLLivenessJob(t, store, "job-live", string(workflow.JobRunning), "")
		if !(jobWorker{Store: store}).shouldRenewSandboxTTL("job-live") {
			t.Fatal("a running job with an unkilled root must keep its sandbox alive")
		}
	})

	t.Run("job that left running does not renew", func(t *testing.T) {
		for _, state := range []string{
			string(workflow.JobFailed), string(workflow.JobSucceeded), string(workflow.JobCancelled),
		} {
			store := openExecBackendLedgerTestStore(t)
			seedTTLLivenessJob(t, store, "job-"+state, state, "")
			if (jobWorker{Store: store}).shouldRenewSandboxTTL("job-" + state) {
				t.Fatalf("state %q still renewed: nothing is waiting on that sandbox", state)
			}
		}
	})

	t.Run("running job whose own root is killed does not renew", func(t *testing.T) {
		store := openExecBackendLedgerTestStore(t)
		seedTTLLivenessJob(t, store, "job-self-root", string(workflow.JobRunning), "")
		if err := store.SetRootJobKilled(ctx, "job-self-root"); err != nil {
			t.Fatalf("SetRootJobKilled returned error: %v", err)
		}
		if (jobWorker{Store: store}).shouldRenewSandboxTTL("job-self-root") {
			t.Fatal("a killed root still renewed: kill must stop buying provider time")
		}
	})

	// THE CASE THE FIX EXISTS FOR. KillDelegationTree marks the ROOT, so a child
	// must consult its root rather than itself. If the resolution disagreed, a
	// killed tree's child would keep renewing and the fix would do nothing for
	// exactly the situation it targets.
	t.Run("running child of a killed root does not renew", func(t *testing.T) {
		store := openExecBackendLedgerTestStore(t)
		seedTTLLivenessJob(t, store, "root-job", string(workflow.JobRunning), "")
		seedTTLLivenessJob(t, store, "child-job", string(workflow.JobRunning), "root-job")
		if err := store.SetRootJobKilled(ctx, "root-job"); err != nil {
			t.Fatalf("SetRootJobKilled returned error: %v", err)
		}
		if (jobWorker{Store: store}).shouldRenewSandboxTTL("child-job") {
			t.Fatal("a child of a killed root still renewed: the root's flag governs the tree")
		}
	})

	t.Run("child of a live root renews", func(t *testing.T) {
		store := openExecBackendLedgerTestStore(t)
		seedTTLLivenessJob(t, store, "root-live", string(workflow.JobRunning), "")
		seedTTLLivenessJob(t, store, "child-live", string(workflow.JobRunning), "root-live")
		if !(jobWorker{Store: store}).shouldRenewSandboxTTL("child-live") {
			t.Fatal("a child of a live root must keep renewing")
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

// TestRootJobIDForTTLLivenessPrefersThePayloadRoot pins the resolution itself,
// including the fallback for a job that is its own root.
func TestRootJobIDForTTLLivenessPrefersThePayloadRoot(t *testing.T) {
	withRoot, err := json.Marshal(workflow.JobPayload{RootJobID: "the-root"})
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}
	if got := rootJobIDForTTLLiveness(db.Job{ID: "child", Payload: string(withRoot)}); got != "the-root" {
		t.Fatalf("root = %q, want %q", got, "the-root")
	}

	bare, err := json.Marshal(workflow.JobPayload{})
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}
	if got := rootJobIDForTTLLiveness(db.Job{ID: "solo", Payload: string(bare)}); got != "solo" {
		t.Fatalf("root = %q, want the job itself when it carries no RootJobID", got)
	}

	// An unparseable payload must not silently resolve to some other job's root.
	if got := rootJobIDForTTLLiveness(db.Job{ID: "broken", Payload: "{not json"}); got != "broken" {
		t.Fatalf("root = %q, want the job itself when the payload cannot be read", got)
	}
}
