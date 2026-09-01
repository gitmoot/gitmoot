package workflow

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
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

// TestRetryIsRefusedInsideASupersedeAdvance pins the PREVENTION half: while the
// advance holds its claim, `gitmoot job retry` cannot roll the lifecycle over at
// all. That is what makes "no stale parent effect" a property of the system rather
// than of the barrier placement.
//
// MUTATION PROOF: remove the JobHasLiveSupersedeAdvanceClaim check from RetryJob and
// the retry succeeds mid-advance.
func TestRetryIsRefusedInsideASupersedeAdvance(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "impl", []string{"implement"}, "gitmoot/gitmoot")
	const child = "workflow-advance-claim"
	insertQueuedJob(t, store, db.Job{ID: child, Agent: "impl", Type: "implement"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-7", PullRequest: 7, TaskID: "task-7", LeadAgent: "impl",
	})
	observed := mustJob(t, store, child)
	if _, err := store.TransitionJobStateWithEventAtGeneration(ctx, child, observed.State, observed.LifecycleGeneration, string(JobFailed),
		db.JobEvent{JobID: child, Kind: JobEventSupersededPullRequestClosed, Message: "pr closed"}); err != nil {
		t.Fatalf("supersede: %v", err)
	}
	// The claim an in-flight advance holds.
	if written, err := store.AddJobEventAtGeneration(ctx, db.JobEvent{
		JobID: child, Kind: JobEventSupersedeAdvanceClaimed,
		Message: formatSupersedeFinalizeDebt(observed.LifecycleGeneration),
	}, observed.LifecycleGeneration); err != nil || !written {
		t.Fatalf("claim written=%v err=%v", written, err)
	}

	if _, err := RetryJob(ctx, store, child); err == nil {
		t.Fatal("a retry rolled the lifecycle over while the parent advance was in flight")
	} else if !errorMentions(err, "supersession parent-advance") {
		t.Fatalf("retry error = %v, want one naming the in-flight advance", err)
	}
	if state := mustJob(t, store, child).State; state != string(JobFailed) {
		t.Fatalf("child state = %q, want the claimed lifecycle untouched", state)
	}

	// Once the advance settles, the retry is allowed again.
	if written, err := store.AddJobEventAtGeneration(ctx, db.JobEvent{
		JobID: child, Kind: JobEventSupersedeAdvanceConfirmed,
		Message: formatSupersedeFinalizeDebt(observed.LifecycleGeneration),
	}, observed.LifecycleGeneration); err != nil || !written {
		t.Fatalf("confirm written=%v err=%v", written, err)
	}
	if _, err := RetryJob(ctx, store, child); err != nil {
		t.Fatalf("retry after the advance settled: %v", err)
	}
}

// TestSupersedeAdvanceClaimStopsBlockingRetriesAfterItsTTL keeps the prevention from
// becoming a wedge: a pass that died mid-advance leaves a claim behind, and an
// operator must not be locked out of retrying forever. The claim row is BACKDATED,
// which is what a crashed pass looks like an hour later.
func TestSupersedeAdvanceClaimStopsBlockingRetriesAfterItsTTL(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gitmoot.db")
	store := openEngineStoreAt(t, path)
	seedAgent(t, store, "impl", []string{"implement"}, "gitmoot/gitmoot")
	const child = "workflow-stale-claim"
	insertQueuedJob(t, store, db.Job{ID: child, Agent: "impl", Type: "implement"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-7", PullRequest: 7, TaskID: "task-7", LeadAgent: "impl",
	})
	observed := mustJob(t, store, child)
	if _, err := store.TransitionJobStateWithEventAtGeneration(ctx, child, observed.State, observed.LifecycleGeneration, string(JobFailed),
		db.JobEvent{JobID: child, Kind: JobEventSupersedeAdvanceClaimed, Message: formatSupersedeFinalizeDebt(observed.LifecycleGeneration)}); err != nil {
		t.Fatalf("supersede with claim: %v", err)
	}
	live, err := store.JobHasLiveSupersedeAdvanceClaim(ctx, child, SupersedeAdvanceClaimTTL)
	if err != nil {
		t.Fatalf("JobHasLiveSupersedeAdvanceClaim: %v", err)
	}
	if !live {
		t.Fatal("a fresh claim was not seen as live")
	}
	if _, err := RetryJob(ctx, store, child); err == nil {
		t.Fatal("a fresh claim did not block the retry")
	}

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open backdating connection: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	if _, err := raw.Exec(`UPDATE job_events SET created_at = datetime('now', '-1 hour')
		WHERE job_id = ? AND kind = 'supersede_advance_claimed'`, child); err != nil {
		t.Fatalf("backdate the claim: %v", err)
	}

	stale, err := store.JobHasLiveSupersedeAdvanceClaim(ctx, child, SupersedeAdvanceClaimTTL)
	if err != nil {
		t.Fatalf("JobHasLiveSupersedeAdvanceClaim: %v", err)
	}
	if stale {
		t.Fatal("a claim older than its TTL still blocks retries; a crashed advance would wedge the job")
	}
	if _, err := RetryJob(ctx, store, child); err != nil {
		t.Fatalf("retry after the claim went stale: %v", err)
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
