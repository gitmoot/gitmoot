package workflow

import (
	"context"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// TestSupersedeRecoveryNeverDispatchesTheDeadChildsOwnDelegations pins that a
// closed-PR-superseded child never fans out its own delegations during recovery, using
// the ONLY result shape this path can mint: decision "failed" with
// SupersededPullRequestClosed set. The sweep selects QUEUED jobs, which carry no result;
// the finalizer stamps the synthetic one; RetryJob clears it.
//
// WHAT THIS TEST DOES NOT DO, stated because the first version of it claimed otherwise
// and the claim was wrong. It does NOT discriminate the routing change from the full
// AdvanceJob. Review measured that on this shape the full advance also produces no
// grandchild, because it returns at the failed/blocked branch (engine_run_budgets.go)
// BEFORE reaching dispatchDelegations. My earlier version reached the dispatch only by
// stamping decision "approved", which the supersession cannot produce, so it pinned a
// hypothetical and I have removed it along with its helper.
//
// The routing to AdvanceParentDAGForTerminalChild is still the right change, for a
// reason this test cannot express: it makes the exclusion STRUCTURAL instead of
// depending on an early return inside a long function continuing to sit ahead of
// dispatchDelegations. This test is the behavioural floor for that - it fails if any
// future edit lets the failed-decision path fall through to a fan-out - and it is
// honest about being a change-detector rather than a mutant-killer for the routing.
func TestSupersedeRecoveryNeverDispatchesTheDeadChildsOwnDelegations(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "gitmoot/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-7", RepoFullName: "gitmoot/gitmoot", State: string(TaskImplementing), Branch: "task-7",
	}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-7", PullRequest: 7, TaskID: "task-7", LeadAgent: "coord",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "fan out",
			Delegations: []Delegation{
				{ID: "api", Agent: "api", Action: "review", Prompt: "review api", FailurePolicy: "continue"},
			},
		},
	})
	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob(parent-job): %v", err)
	}
	const child = "parent-job/delegation/api"
	observed := mustJob(t, store, child)
	superseded, err := store.TransitionJobStateWithEventAtGeneration(ctx, child, observed.State, observed.LifecycleGeneration, string(JobFailed),
		db.JobEvent{JobID: child, Kind: JobEventSupersededPullRequestClosed, Message: "pr closed"},
		db.JobEvent{JobID: child, Kind: JobEventSupersedeFinalizePending, Message: formatSupersedeFinalizeDebt(observed.LifecycleGeneration)})
	if err != nil || !superseded {
		t.Fatalf("supersede: transitioned=%v err=%v", superseded, err)
	}
	// The production result shape, carrying a delegation: if anything on this path ever
	// reaches dispatchDelegations, THIS is what it would fan out.
	stampSupersededResultWithFanout(t, store, child, "pr closed")

	advanced, err := engine.advanceSupersededChildAtGeneration(ctx, child, observed.LifecycleGeneration)
	if err != nil && !isDelegationPolicyOutcome(err) {
		t.Fatalf("advanceSupersededChildAtGeneration: %v", err)
	}
	if !advanced {
		t.Fatal("the advance was refused, so the dispatch decision this test constrains was never reached")
	}
	if grandchild, err := store.GetJob(ctx, child+"/delegation/grandchild"); err == nil {
		t.Fatalf("the dead child's own delegation was dispatched (%s, state %q) during recovery",
			grandchild.ID, grandchild.State)
	}
}

// stampSupersededResultWithFanout writes the production supersession result shape
// (failed + SupersededPullRequestClosed) but with a delegation attached, so a path that
// wrongly reached dispatchDelegations would have something to dispatch.
func stampSupersededResultWithFanout(t *testing.T, store *db.Store, jobID string, summary string) {
	t.Helper()
	job := mustJob(t, store, jobID)
	payload, err := unmarshalPayload(job.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload: %v", err)
	}
	payload.Result = &AgentResult{
		Decision:                    "failed",
		Summary:                     summary,
		SupersededPullRequestClosed: true,
		Delegations: []Delegation{
			{ID: "grandchild", Agent: "api", Action: "review", Prompt: "review more", FailurePolicy: "continue"},
		},
	}
	encoded, err := marshalPayload(payload)
	if err != nil {
		t.Fatalf("marshalPayload: %v", err)
	}
	if err := store.UpdateJobPayload(context.Background(), jobID, encoded); err != nil {
		t.Fatalf("UpdateJobPayload: %v", err)
	}
}
