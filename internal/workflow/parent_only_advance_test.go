package workflow

import (
	"context"
	"errors"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// TestParentOnlyAdvanceCannotRunTheChildsOwnAdvancement pins the CONTRACT that makes
// AdvanceParentDAGForTerminalChild safe to call without a validated checkout (#1673).
//
// The retry actuator reaches it for a child whose pull request closed, because that
// child can never satisfy the delivery preflight. The full AdvanceJob would, for the
// same job: dispatch the child's OWN delegations, normalize a high-risk lens verdict by
// rewriting the stored result and job state, and register worktree teardown. Under an
// unvalidated checkout those become database and remote actions nobody authorized.
//
// So this operation must advance the PARENT and nothing else. The assertions below are
// the observable form of "structurally cannot": no grandchild job, and the child's own
// stored result and state untouched.
//
// SEMANTIC REVERSION THIS KILLS: route the actuator (or this operation) through
// AdvanceJob and the child's delegation is dispatched.
func TestParentOnlyAdvanceCannotRunTheChildsOwnAdvancement(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "gitmoot/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)

	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo:      "gitmoot/gitmoot",
		Branch:    "task-7",
		TaskID:    "task-7",
		TaskTitle: "Parent",
		Sender:    "coord",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "fan out",
			Delegations: []Delegation{
				// Default failure policy (block_parent): the parent branch does NOT
				// short-circuit, so the full advance would continue on to dispatch the
				// child's own delegations - which is what makes the two paths distinguishable.
				{ID: "api", Agent: "api", Action: "review", Prompt: "review api"},
			},
		},
	})

	// A SETTLED CHILD THAT CARRIES ITS OWN DELEGATIONS. The full advance would dispatch
	// them; the parent-only operation must not.
	child := "parent-job/delegation/api"
	insertCompletedJob(t, store, db.Job{
		ID: child, Agent: "api", Type: "review", ParentJobID: "parent-job", DelegationID: "api",
	}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		Branch:      "task-7",
		TaskID:      "task-7",
		ParentJobID: "parent-job",
		Sender:      "coord",
		Result: &AgentResult{
			Decision: "failed",
			Summary:  "pull request #7 is no longer open",
			Delegations: []Delegation{
				{ID: "grandchild", Agent: "api", Action: "review", Prompt: "review deeper"},
			},
		},
	})
	beforeJob := mustJob(t, store, child)

	if err := engine.AdvanceParentDAGForTerminalChild(ctx, child); err != nil {
		var blocked BlockedError
		if !errors.As(err, &blocked) {
			t.Fatalf("AdvanceParentDAGForTerminalChild: %v", err)
		}
	}

	// THE CHILD'S OWN DELEGATION WAS NOT DISPATCHED.
	if _, err := store.GetJob(ctx, child+"/delegation/grandchild"); err == nil {
		t.Fatal("the child's own delegation was dispatched: this is not a parent-only advancement")
	}
	// AND THE CHILD ITSELF WAS NOT REWRITTEN - no lens normalization, no state change.
	afterJob := mustJob(t, store, child)
	if afterJob.State != beforeJob.State {
		t.Fatalf("child state %q -> %q: a parent-only advancement must not rewrite the child", beforeJob.State, afterJob.State)
	}
	if afterJob.Payload != beforeJob.Payload {
		t.Fatal("the child's stored payload was rewritten: a parent-only advancement must not normalize its result")
	}
}

// TestParentOnlyAdvanceRefusesANonTerminalChild is the guard's own success/failure
// boundary: the operation exists for SETTLED children, and applying it to a running one
// would skip validation that job still needs.
func TestParentOnlyAdvanceRefusesANonTerminalChild(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "gitmoot/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)

	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-7", TaskID: "task-7", Sender: "coord",
		Result: &AgentResult{Decision: "approved", Summary: "fan out",
			Delegations: []Delegation{{ID: "api", Agent: "api", Action: "review", Prompt: "review api"}}},
	})
	child := "parent-job/delegation/api"
	insertQueuedJob(t, store, db.Job{
		ID: child, Agent: "api", Type: "review", ParentJobID: "parent-job", DelegationID: "api",
	}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-7", TaskID: "task-7", ParentJobID: "parent-job",
		Sender: "coord", Result: &AgentResult{Decision: "approved", Summary: "still running"},
	})

	if err := engine.AdvanceParentDAGForTerminalChild(ctx, child); err == nil {
		t.Fatal("a queued child was accepted for parent-only advancement")
	}
}
