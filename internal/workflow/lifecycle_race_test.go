package workflow

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	dbtest "github.com/gitmoot/gitmoot/internal/db/dbtest"
)

// TestSupersedeClosedPullRequestJobRefusesAStaleObservedGeneration stands in the ABA
// window the state-only compare-and-swap could not see.
//
// The sweep forms its verdict — this pull request is closed, so this leg is
// pointless — about the run it LISTED. Between that listing and the write the job
// can complete and be re-queued, and a state-only CAS accepts the new run because
// it is `queued` too, cancelling work the verdict was never about.
//
// The test occupies exactly that window: it takes the observation, lets the job
// complete and re-queue, and only then settles with the stale row.
//
// MUTATION PROOF: swap TransitionJobStateWithEventAtGeneration for
// TransitionJobStateWithEvent and the newer lifecycle is cancelled.
func TestSupersedeClosedPullRequestJobRefusesAStaleObservedGeneration(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "impl", []string{"implement"}, "gitmoot/gitmoot")
	insertQueuedJob(t, store, db.Job{ID: "workflow-aba", Agent: "impl", Type: "implement"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-7", PullRequest: 7, TaskID: "task-7", LeadAgent: "impl",
	})
	stale := mustJob(t, store, "workflow-aba")

	for _, state := range []JobState{JobRunning, JobSucceeded, JobQueued} {
		if err := store.UpdateJobState(ctx, "workflow-aba", string(state)); err != nil {
			t.Fatalf("UpdateJobState(%s): %v", state, err)
		}
	}
	current := mustJob(t, store, "workflow-aba")
	if current.LifecycleGeneration == stale.LifecycleGeneration {
		t.Fatalf("fixture did not advance the generation: stale=%d current=%d", stale.LifecycleGeneration, current.LifecycleGeneration)
	}

	job, superseded, err := SupersedeClosedPullRequestJob(ctx, store, stale, "pr closed")
	if err != nil {
		t.Fatalf("SupersedeClosedPullRequestJob returned error: %v", err)
	}
	if superseded {
		t.Fatal("a stale observation cancelled a newer lifecycle")
	}
	if job.State != string(JobQueued) {
		t.Fatalf("state = %q, want the re-queued run left alone", job.State)
	}
	for _, event := range mustJobEventKinds(t, store, "workflow-aba") {
		if event == JobEventSupersededPullRequestClosed || event == JobEventSupersedeFinalizePending {
			t.Fatalf("stale settlement wrote %q on the live run", event)
		}
	}

	// The CURRENT observation still settles: the anchor rejects staleness, not the
	// sweep. Without this arm a guard that refused everything would pass above.
	_, superseded, err = SupersedeClosedPullRequestJob(ctx, store, current, "pr closed")
	if err != nil {
		t.Fatalf("current-generation supersede returned error: %v", err)
	}
	if !superseded {
		t.Fatal("the observed generation was refused; the sweep can no longer terminate anything")
	}
}

// TestFinalizeClosedPullRequestDelegationChildRefusesAStaleObservedGeneration is the
// same window on the child path, where the cost is worse: the stale settlement
// would drive a synthetic `failed` result into a run that is alive.
func TestFinalizeClosedPullRequestDelegationChildRefusesAStaleObservedGeneration(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "gitmoot/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-7", PullRequest: 7, TaskID: "task-7", LeadAgent: "coord",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "fan out",
			Delegations: []Delegation{
				{ID: "api", Agent: "api", Action: "review", Prompt: "review api"},
			},
		},
	})
	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob(parent-job) returned error: %v", err)
	}
	const child = "parent-job/delegation/api"
	stale := mustJob(t, store, child)
	for _, state := range []JobState{JobRunning, JobSucceeded, JobQueued} {
		if err := store.UpdateJobState(ctx, child, string(state)); err != nil {
			t.Fatalf("UpdateJobState(%s): %v", state, err)
		}
	}
	if mustJob(t, store, child).LifecycleGeneration == stale.LifecycleGeneration {
		t.Fatal("fixture did not advance the child's generation")
	}

	finalized, err := engine.FinalizeClosedPullRequestDelegationChild(ctx, stale, "pr closed")
	if err != nil {
		t.Fatalf("FinalizeClosedPullRequestDelegationChild returned error: %v", err)
	}
	if finalized {
		t.Fatal("a stale observation finalized a newer child lifecycle")
	}
	live := mustJob(t, store, child)
	if live.State != string(JobQueued) {
		t.Fatalf("child state = %q, want the re-queued run left alone", live.State)
	}
	payload, err := unmarshalPayload(live.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload: %v", err)
	}
	if payload.Result != nil {
		t.Fatalf("a synthetic result was stamped on a live run: %+v", payload.Result)
	}
}

// TestCompletePendingSupersedeFinalizationVoidsADebtFromAnOlderLifecycle is the
// recovery side of the same ABA problem, and it bites in both directions.
//
// The debt is recorded for the run that incurred it. `gitmoot job retry` then puts
// the job back in queued at a NEWER generation, and:
//
//	if that newer run FAILS, paying the old debt stamps it with the PR-closed
//	synthetic failure and advances the parent on a run that failed for its own
//	reasons;
//
//	if that newer run SUCCEEDS, the debt can never be paid at all, so the job stays
//	a candidate on every poll forever.
//
// Both are answered the same way: the debt is VOIDED — closed, with no work — and
// the newer run settles through its own path.
//
// MUTATION PROOF: drop the generation from the pending marker (or compare only the
// state) and the newer-failed arm stamps a result and advances the parent, while
// the newer-succeeded arm stays pending.
func TestCompletePendingSupersedeFinalizationVoidsADebtFromAnOlderLifecycle(t *testing.T) {
	for _, newer := range []JobState{JobFailed, JobSucceeded} {
		t.Run("newer "+string(newer), func(t *testing.T) {
			ctx := context.Background()
			store := openEngineStore(t)
			seedAgent(t, store, "coord", []string{"ask"}, "gitmoot/gitmoot")
			seedAgent(t, store, "api", []string{"review"}, "gitmoot/gitmoot")
			engine := testEngine(store)
			insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
				Repo: "gitmoot/gitmoot", Branch: "task-7", PullRequest: 7, TaskID: "task-7", LeadAgent: "coord",
				Result: &AgentResult{
					Decision: "approved",
					Summary:  "fan out",
					Delegations: []Delegation{
						{ID: "api", Agent: "api", Action: "review", Prompt: "review api"},
					},
				},
			})
			if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
				t.Fatalf("AdvanceJob(parent-job): %v", err)
			}
			const child = "parent-job/delegation/api"
			superseded := mustJob(t, store, child)
			// The supersession records its debt for THIS generation.
			if _, err := engine.FinalizeClosedPullRequestDelegationChild(ctx, superseded, "pr closed"); err != nil && !isDelegationPolicyOutcome(err) {
				t.Fatalf("FinalizeClosedPullRequestDelegationChild: %v", err)
			}
			// PRODUCTION must anchor the marker itself: the recovery below can only
			// tell lifecycles apart if the generation is in the row the sweep wrote.
			// The NET debt is already settled at this point — block_parent is a policy
			// outcome, which pays it — so the marker event is what to inspect.
			markedGeneration, markedReason, anchored := pendingDebtMarker(t, store, child)
			if !anchored || markedGeneration != superseded.LifecycleGeneration {
				t.Fatalf("pending marker generation = %d anchored=%v, want the observed generation %d", markedGeneration, anchored, superseded.LifecycleGeneration)
			}
			// A retry starts a new lifecycle, and that run reaches its own outcome.
			if _, err := RetryJob(ctx, store, child); err != nil {
				t.Fatalf("RetryJob: %v", err)
			}
			retried := mustJob(t, store, child)
			if retried.State != string(JobQueued) {
				t.Fatalf("retried child = %q, want queued", retried.State)
			}
			if retried.LifecycleGeneration == superseded.LifecycleGeneration {
				t.Fatal("RetryJob did not start a new lifecycle generation")
			}
			// Re-arm the debt as an OUTSTANDING one against the new lifecycle: this is
			// the state a poll would find if the original finalization had failed. It
			// reuses the generation PRODUCTION recorded above, not an invented one.
			if err := store.AddJobEvent(ctx, db.JobEvent{
				JobID: child, Kind: JobEventSupersedeFinalizePending,
				Message: formatSupersedeFinalizeDebt(markedGeneration, markedReason),
			}); err != nil {
				t.Fatalf("re-arm pending debt: %v", err)
			}
			if err := clearChildResult(ctx, store, child); err != nil {
				t.Fatalf("clear child result: %v", err)
			}
			if err := store.UpdateJobState(ctx, child, string(newer)); err != nil {
				t.Fatalf("UpdateJobState(%s): %v", newer, err)
			}
			taskBefore := mustTaskState(t, store, "task-7")

			handled, err := engine.CompletePendingSupersedeFinalization(ctx, child)
			if err != nil {
				t.Fatalf("CompletePendingSupersedeFinalization: %v", err)
			}
			if !handled {
				t.Fatal("the outstanding debt was not handled at all; it would stay a candidate forever")
			}
			// Voided: closed for the candidate query, with no synthetic result and no
			// parent advance against the newer run.
			pending, err := store.JobIDsWithPendingSupersedeFinalization(ctx)
			if err != nil {
				t.Fatalf("JobIDsWithPendingSupersedeFinalization: %v", err)
			}
			for _, id := range pending {
				if id == child {
					t.Fatal("the debt is still outstanding: this job is a candidate on every future poll")
				}
			}
			live := mustJob(t, store, child)
			payload, err := unmarshalPayload(live.Payload)
			if err != nil {
				t.Fatalf("unmarshalPayload: %v", err)
			}
			if payload.Result != nil {
				t.Fatalf("the old debt stamped a result on lifecycle %d: %+v", live.LifecycleGeneration, payload.Result)
			}
			if got := mustTaskState(t, store, "task-7"); got != taskBefore {
				t.Fatalf("task state moved %q -> %q: the parent was advanced on a newer lifecycle", taskBefore, got)
			}
		})
	}
}

// clearChildResult removes a synthetic result so the retried lifecycle starts from
// the same shape a real re-dispatch would: queued work with no verdict yet.
func clearChildResult(ctx context.Context, store *db.Store, jobID string) error {
	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		return err
	}
	payload, err := unmarshalPayload(job.Payload)
	if err != nil {
		return err
	}
	payload.Result = nil
	encoded, err := marshalPayload(payload)
	if err != nil {
		return err
	}
	return store.UpdateJobPayload(ctx, jobID, encoded)
}

// pendingDebtMarker returns the generation, reason and anchored flag of the LAST
// pending-debt marker a job carries. It reads the marker EVENT rather than the net
// debt on purpose: a supersession that ends in a policy outcome pays its debt
// immediately, so the net debt is empty while the marker's contents are still the
// thing production has to get right.
func pendingDebtMarker(t *testing.T, store *db.Store, jobID string) (int64, string, bool) {
	t.Helper()
	events, err := store.ListJobEvents(context.Background(), jobID)
	if err != nil {
		t.Fatalf("ListJobEvents(%s): %v", jobID, err)
	}
	generation, reason, anchored := int64(0), "", false
	found := false
	for _, event := range events {
		if event.Kind != JobEventSupersedeFinalizePending {
			continue
		}
		found = true
		generation, reason, anchored = parseSupersedeFinalizeDebt(event.Message)
	}
	if !found {
		t.Fatalf("job %s carries no %s marker", jobID, JobEventSupersedeFinalizePending)
	}
	return generation, reason, anchored
}

// TestPersistTaskStateRefusesADisposalWrittenAfterTheRead pins the third race.
//
// Every caller checks for a disposed task with a pre-read, and a pre-read is
// exactly what a concurrent disposal invalidates: an operator dismisses the task
// between the read and the write, and automation then resurrects it into
// `reviewing`, `blocked` or `implementing`. The exclusion has to be part of the
// statement that writes.
//
// MUTATION PROOF: drop the disposed states from the forbidden list (leaving only
// the merged guard) and every disposed arm below writes over the disposition.
func TestPersistTaskStateRefusesADisposalWrittenAfterTheRead(t *testing.T) {
	for _, disposed := range []TaskState{TaskDismissed, TaskSuperseded, TaskStranded} {
		for _, target := range []TaskState{TaskImplementing, TaskReviewing, TaskBlocked, TaskMerged} {
			t.Run(string(disposed)+"->"+string(target), func(t *testing.T) {
				store, err := dbtest.Open(t, filepath.Join(t.TempDir(), "gitmoot.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				ctx := context.Background()
				const repo, branch = "owner/repo", "feature/one"
				if err := store.UpsertTask(ctx, db.Task{
					ID: "task-1", RepoFullName: repo, State: string(TaskReviewing), Branch: branch,
				}); err != nil {
					t.Fatal(err)
				}
				// The caller's snapshot, taken while the task was still live.
				snapshot, err := store.GetTask(ctx, "task-1")
				if err != nil {
					t.Fatal(err)
				}
				// A second writer disposes of it in the window.
				disposal := snapshot
				disposal.State = string(disposed)
				if err := store.UpsertTask(ctx, disposal); err != nil {
					t.Fatal(err)
				}

				written, err := PersistTaskState(ctx, store, snapshot, target)
				if written {
					t.Fatalf("wrote %s over a %s task", target, disposed)
				}
				if err == nil || !strings.Contains(err.Error(), string(disposed)) {
					t.Fatalf("error = %v, want one naming %s", err, disposed)
				}
				task, err := store.GetTask(ctx, "task-1")
				if err != nil {
					t.Fatal(err)
				}
				if task.State != string(disposed) {
					t.Fatalf("task state = %q, want the disposition preserved", task.State)
				}
			})
		}
	}
}

// TestPersistTaskStateStillWritesEveryNonDisposedState is the other half: the
// atomic exclusions must not freeze ordinary advancement. A guard that refused
// everything would satisfy the race tests above and break the workflow.
func TestPersistTaskStateStillWritesEveryNonDisposedState(t *testing.T) {
	for _, target := range []TaskState{
		TaskPlanned, TaskImplementing, TaskPullRequestOpen, TaskReviewing, TaskChangesRequested,
		TaskReadyToMerge, TaskAwaitingHuman, TaskAwaitingHumanMerge, TaskMerged,
		TaskDismissed, TaskSuperseded, TaskStranded,
	} {
		t.Run(string(target), func(t *testing.T) {
			store, err := dbtest.Open(t, filepath.Join(t.TempDir(), "gitmoot.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			ctx := context.Background()
			if err := store.UpsertTask(ctx, db.Task{
				ID: "task-1", RepoFullName: "owner/repo", State: string(TaskImplementing), Branch: "feature/one",
			}); err != nil {
				t.Fatal(err)
			}
			task, err := store.GetTask(ctx, "task-1")
			if err != nil {
				t.Fatal(err)
			}
			written, err := PersistTaskState(ctx, store, task, target)
			if err != nil || !written {
				t.Fatalf("PersistTaskState(%s) written=%v err=%v", target, written, err)
			}
			if got := mustTaskState(t, store, "task-1"); got != string(target) {
				t.Fatalf("task state = %q, want %s", got, target)
			}
			// Idempotent: writing the same state again is permitted, including for a
			// disposed target, so a repeated disposal is not an error.
			written, err = PersistTaskState(ctx, store, task, target)
			if err != nil || !written {
				t.Fatalf("repeated PersistTaskState(%s) written=%v err=%v", target, written, err)
			}
		})
	}
}

func mustTaskState(t *testing.T, store *db.Store, taskID string) string {
	t.Helper()
	task, err := store.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("GetTask(%s): %v", taskID, err)
	}
	return task.State
}

func mustJobEventKinds(t *testing.T, store *db.Store, jobID string) []string {
	t.Helper()
	events, err := store.ListJobEvents(context.Background(), jobID)
	if err != nil {
		t.Fatalf("ListJobEvents(%s): %v", jobID, err)
	}
	kinds := make([]string, 0, len(events))
	for _, event := range events {
		kinds = append(kinds, event.Kind)
	}
	return kinds
}
