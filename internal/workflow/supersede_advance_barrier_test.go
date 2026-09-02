package workflow

import (
	"context"
	"errors"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// TestSupersedeAdvanceBarriersBlockEveryStaleParentEffect is the B5 requirement:
// zero stale parent-side effects, proven at each effect class rather than inferred
// from a post-effect marker.
//
// A retry lands INSIDE AdvanceJob — after the child snapshot the pass reasons from,
// and before the failure policy, the dependent enqueue and the continuation. Each
// barrier re-asserts the anchored child lifecycle immediately before its effect, so
// the advance aborts rather than applying a policy, enqueueing a dependent or
// minting a continuation from a run that no longer exists.
//
// The retry is forced through the store, bypassing RetryJob's own refusal, because
// this test is about the barriers: the refusal is proven separately in
// TestRetryIsRefusedInsideASupersedeAdvance.
//
// MUTATION PROOF: delete any single assertSupersedeAdvanceAnchor call and the stage
// named by that barrier stops aborting — the continuation appears, or the parent
// task moves.
func TestSupersedeAdvanceBarriersBlockEveryStaleParentEffect(t *testing.T) {
	for _, barrier := range []string{
		supersedeAdvanceBarrierChildSnapshot,
		supersedeAdvanceBarrierFailurePolicy,
		supersedeAdvanceBarrierDependentEnqueue,
		supersedeAdvanceBarrierContinuation,
	} {
		t.Run(barrier, func(t *testing.T) {
			ctx := context.Background()
			store := openEngineStore(t)
			seedAgent(t, store, "coord", []string{"ask"}, "gitmoot/gitmoot")
			seedAgent(t, store, "api", []string{"review"}, "gitmoot/gitmoot")
			seedAgent(t, store, "ui", []string{"review"}, "gitmoot/gitmoot")
			engine := testEngine(store)
			if err := store.UpsertTask(ctx, db.Task{
				ID: "task-7", RepoFullName: "gitmoot/gitmoot", State: string(TaskImplementing), Branch: "task-7",
			}); err != nil {
				t.Fatalf("UpsertTask: %v", err)
			}
			// Two delegations, the second gated on the first, so the dependent-enqueue
			// and continuation effect classes are both reachable.
			insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
				Repo: "gitmoot/gitmoot", Branch: "task-7", PullRequest: 7, TaskID: "task-7", LeadAgent: "coord",
				Result: &AgentResult{
					Decision: "approved",
					Summary:  "fan out",
					Delegations: []Delegation{
						{ID: "api", Agent: "api", Action: "review", Prompt: "review api", FailurePolicy: "continue"},
						{ID: "ui", Agent: "ui", Action: "review", Prompt: "review ui", Deps: []string{"api"}},
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
			stampSyntheticResult(t, store, child, "pr closed")
			taskBefore := mustTaskState(t, store, "task-7")

			hit := 0
			supersedeAdvanceBarrierHook = func(hookCtx context.Context, at string) {
				if at != barrier || hit > 0 {
					return
				}
				hit++
				// The lifecycle rolls over mid-advance, payload preserved so nothing else
				// can be blamed for the abort.
				for _, state := range []JobState{JobQueued, JobRunning} {
					if err := store.UpdateJobState(hookCtx, child, string(state)); err != nil {
						t.Fatalf("UpdateJobState(%s): %v", state, err)
					}
				}
			}
			t.Cleanup(func() { supersedeAdvanceBarrierHook = nil })

			advanced, err := engine.advanceSupersededChildAtGeneration(ctx, child, observed.LifecycleGeneration)
			if err != nil {
				t.Fatalf("advanceSupersededChildAtGeneration: %v", err)
			}
			if hit != 1 {
				t.Fatalf("barrier %q was never reached; the effect class under test did not run", barrier)
			}
			if advanced {
				t.Fatalf("the advance reported success after the lifecycle rolled over at %q", barrier)
			}
			// No parent-side effect may be attributable to the superseded run.
			if got := mustTaskState(t, store, "task-7"); got != taskBefore {
				t.Fatalf("task state %q -> %q: a failure policy ran from a superseded lifecycle", taskBefore, got)
			}
			if _, err := store.GetJob(ctx, "parent-job/delegation/ui"); err == nil {
				t.Fatal("a dependent sibling was enqueued from a superseded lifecycle")
			}
			if _, err := store.GetJob(ctx, "parent-job/continuation"); err == nil {
				t.Fatal("a coordinator continuation was minted from a superseded lifecycle")
			}
			if got := countWorkflowJobEvents(t, store, child, JobEventSupersedeAdvanceConfirmed); got != 0 {
				t.Fatalf("%s events = %d, want 0", JobEventSupersedeAdvanceConfirmed, got)
			}
			if got := countWorkflowJobEvents(t, store, child, JobEventSupersedeAdvanceSuperseded); got != 1 {
				t.Fatalf("%s events = %d, want exactly 1 durable trace", JobEventSupersedeAdvanceSuperseded, got)
			}
			// The debt is still outstanding, so the next poll re-drives it.
			debt, derr := latestSupersedeFinalizeDebt(ctx, store, child)
			if derr != nil {
				t.Fatalf("latestSupersedeFinalizeDebt: %v", derr)
			}
			if !debt.pending {
				t.Fatal("the debt was closed although no advance ran")
			}
		})
	}
}

// TestSupersedeAdvanceBarriersAllowTheAnchoredLifecycle is the success-path control.
// Barriers that refused unconditionally would satisfy the test above while breaking
// every ordinary recovery, so the un-raced advance must complete and confirm.
func TestSupersedeAdvanceBarriersAllowTheAnchoredLifecycle(t *testing.T) {
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
	stampSyntheticResult(t, store, child, "pr closed")

	advanced, err := engine.advanceSupersededChildAtGeneration(ctx, child, observed.LifecycleGeneration)
	if err != nil && !isDelegationPolicyOutcome(err) {
		t.Fatalf("advanceSupersededChildAtGeneration: %v", err)
	}
	if !advanced {
		t.Fatal("an un-raced advance was refused; the barriers block the ordinary path")
	}
	if got := countWorkflowJobEvents(t, store, child, JobEventSupersedeAdvanceConfirmed); got != 1 {
		t.Fatalf("%s events = %d, want 1", JobEventSupersedeAdvanceConfirmed, got)
	}
}

// stampSyntheticResult puts the recovery into its already-finalized arm, where the
// only work left is the parent advance.
func stampSyntheticResult(t *testing.T, store *db.Store, jobID string, summary string) {
	t.Helper()
	job := mustJob(t, store, jobID)
	payload, err := unmarshalPayload(job.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload: %v", err)
	}
	payload.Result = &AgentResult{Decision: "failed", Summary: summary}
	encoded, err := marshalPayload(payload)
	if err != nil {
		t.Fatalf("marshalPayload: %v", err)
	}
	if err := store.UpdateJobPayload(context.Background(), jobID, encoded); err != nil {
		t.Fatalf("UpdateJobPayload: %v", err)
	}
}

func errorMentions(err error, want string) bool {
	if err == nil {
		return false
	}
	var rolled supersedeAdvanceRolledBackError
	if errors.As(err, &rolled) {
		return false
	}
	return len(want) > 0 && containsString(err.Error(), want)
}

func containsString(haystack string, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOfString(haystack, needle) >= 0)
}

func indexOfString(haystack string, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
