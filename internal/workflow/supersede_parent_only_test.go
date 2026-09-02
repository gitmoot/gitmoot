package workflow

import (
	"context"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// TestSupersedeAdvanceDoesNotDispatchTheDeadChildsOwnDelegations pins the guarantee the
// merge of #1731 with #1763 dropped.
//
// The two safety mechanisms on this path protect DIFFERENT things and neither implies
// the other:
//
//   - the supersede ownership anchor refuses parent effects from a lifecycle that has
//     been superseded;
//   - parent-DAG-only advancement refuses to dispatch the terminal child's OWN
//     delegations.
//
// The anchor cannot cover the second. When the recovery runs for the lifecycle it
// actually claimed, dispatching that child's delegations is legitimate by the anchor's
// own test, so every barrier passes and the grandchildren spawn anyway - from a recovery
// pass with no validated checkout, which is what #1763 closed.
//
// #1731 was written before the parent-only operation existed and called the full
// AdvanceJob here, so this is a merge-time regression rather than a defect in either
// change alone.
//
// THE FIXTURE HAS TO ROUTE, and the first version of this test did not: seeding the
// child as `succeeded` made advanceSupersededChildAtGeneration return (false, nil)
// before it ever claimed ownership, so both the fixed and the full-advance versions
// "passed" while advancing nothing. The shape below is the production one - a child
// terminalized by a supersession, carrying a result - and its result FANS OUT, which is
// the adversarial part: a full advance reads those delegations and dispatches them.
func TestSupersedeAdvanceDoesNotDispatchTheDeadChildsOwnDelegations(t *testing.T) {
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
	stampFanningOutResult(t, store, child, "pr closed")

	advanced, err := engine.advanceSupersededChildAtGeneration(ctx, child, observed.LifecycleGeneration)
	if err != nil && !isDelegationPolicyOutcome(err) {
		t.Fatalf("advanceSupersededChildAtGeneration: %v", err)
	}
	// The advance must actually RUN, otherwise the observable below is vacuous - this is
	// the assertion whose absence let the first version of this test pass against the
	// defect.
	if !advanced {
		t.Fatal("the advance was refused, so this test never reached the dispatch decision it exists to constrain")
	}

	// THE OBSERVABLE. A full advance reads the child's result and dispatches this row;
	// the parent-only operation cannot reach the code that creates it.
	if grandchild, err := store.GetJob(ctx, child+"/delegation/grandchild"); err == nil {
		t.Fatalf("the dead child's own delegation was dispatched (%s, state %q): the recovery ran a FULL advance, "+
			"so a pass with no validated checkout spawned work #1763 forbids",
			grandchild.ID, grandchild.State)
	}
}

// stampFanningOutResult is stampSyntheticResult's adversarial sibling: the result it
// writes carries a delegation, so a caller that runs the FULL advance dispatches a
// grandchild and a caller that advances only the parent DAG cannot.
func stampFanningOutResult(t *testing.T, store *db.Store, jobID string, summary string) {
	t.Helper()
	job := mustJob(t, store, jobID)
	payload, err := unmarshalPayload(job.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload: %v", err)
	}
	payload.Result = &AgentResult{
		Decision: "approved",
		Summary:  summary,
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
