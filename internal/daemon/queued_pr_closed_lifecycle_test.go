package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// TestPollOnceSupersedesAJobWhoseLifecycleWasRequeued pins that the sweep settles
// the generation it OBSERVED rather than a hardcoded zero.
//
// The settlement is anchored on jobs.lifecycle_generation, and that counter is
// bumped by every transition INTO queued. A job that ran once and was re-queued
// therefore sits at generation 1 while a fresh row sits at 0. If the candidate
// listing does not project the column, every observation reads 0, the
// compare-and-swap loses on exactly the retried population, and the sweep
// silently stops terminating the legs it exists for.
//
// MUTATION PROOF: drop lifecycle_generation from listQueuedJobsForRepoSQL (or
// scan it into a discard) and this job stays queued forever.
func TestPollOnceSupersedesAJobWhoseLifecycleWasRequeued(t *testing.T) {
	ctx := context.Background()
	store, repo, client := closedPullRequestSweepFixture(t)
	seedQueuedJob(t, store, "retried-review", "audit", "review", workflow.JobPayload{
		Repo: repo.FullName(), Branch: "task-7", PullRequest: 7, TaskID: "task-7",
		LeadAgent: "lead", Sender: "github",
	})
	// One completed lifecycle, then back to queued: the shape `gitmoot job retry`
	// leaves behind, and the shape the ABA guard is about.
	for _, state := range []workflow.JobState{workflow.JobRunning, workflow.JobSucceeded, workflow.JobQueued} {
		if err := store.UpdateJobState(ctx, "retried-review", string(state)); err != nil {
			t.Fatalf("UpdateJobState(%s): %v", state, err)
		}
	}
	requeued, err := store.GetJob(ctx, "retried-review")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if requeued.LifecycleGeneration == 0 {
		t.Fatalf("fixture did not advance the lifecycle generation: %+v", requeued)
	}

	engine := workflow.Engine{Store: store}
	daemon := Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}
	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}

	job, err := store.GetJob(ctx, "retried-review")
	if err != nil {
		t.Fatalf("GetJob after poll: %v", err)
	}
	if job.State != string(workflow.JobCancelled) {
		t.Fatalf("re-queued leg = %q, want cancelled: the sweep must anchor on the OBSERVED generation %d", job.State, requeued.LifecycleGeneration)
	}
}

// TestPollOnceRecoversASupersededChildWhoseFinalizationFailed drives the strand the
// pending-finalization marker exists for.
//
// The terminal write moves a superseded child OUT of queued, and the closed-PR
// sweep selects only queued jobs. So a failure anywhere after that write — the
// cleanups, the synthetic result, the parent advance — used to leave a child no
// later poll could rediscover and a coordinator that waited forever.
//
// The injected fault aborts the child's PAYLOAD write, which is the step that
// stamps the synthetic result the parent's advanceDelegations requires. The state
// transition is a state-only update and still commits, so the poll ends with a
// child that is terminal, out of `queued`, and owes both a result and a parent
// advance — the exact shape that used to be unreachable by every later sweep.
// Once the fault clears, a later poll must pay the whole debt.
//
// MUTATION PROOF: stop writing the pending marker inside the transition (or drop
// the completePendingSupersedeFinalizations pass) and the continuation is never
// enqueued.
func TestPollOnceRecoversASupersededChildWhoseFinalizationFailed(t *testing.T) {
	ctx := context.Background()
	store, repo, client := closedPullRequestSweepFixture(t)
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-7", RepoFullName: repo.FullName(), GoalID: "goal-1", Title: "Task 7",
		State: string(workflow.TaskReviewing), Branch: "task-7",
	}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	const coordinator = "review-coordinator/task-7/review-1"
	// `continue` rather than the default block_parent: releasing the coordinator
	// then shows up as a CONTINUATION job, an observable no other reconciler in the
	// poll can produce. Task state cannot serve here — reconcileClosedReviewingTasks
	// drives a reviewing task with a closed PR to blocked on its own.
	coordinatorPayload, err := json.Marshal(workflow.JobPayload{
		Repo: repo.FullName(), Branch: "task-7", PullRequest: 7, TaskID: "task-7", LeadAgent: "lead",
		Result: &workflow.AgentResult{
			Decision: "approved",
			Summary:  "fan out",
			Delegations: []workflow.Delegation{
				{ID: "correctness", Agent: "audit", Action: "review", Prompt: "review correctness", FailurePolicy: "continue"},
			},
		},
	})
	if err != nil {
		t.Fatalf("json.Marshal(coordinator): %v", err)
	}
	if err := store.CreateJob(ctx, db.Job{
		ID: coordinator, Agent: "lead", Type: "ask", State: string(workflow.JobSucceeded), Payload: string(coordinatorPayload),
	}); err != nil {
		t.Fatalf("CreateJob(coordinator): %v", err)
	}
	const child = coordinator + "/delegation/correctness"
	seedQueuedJob(t, store, child, "audit", "review", workflow.JobPayload{
		Repo: repo.FullName(), Branch: "task-7", PullRequest: 7, TaskID: "task-7", LeadAgent: "lead",
		ParentJobID: coordinator, DelegationID: "correctness",
	})

	raw, err := sql.Open("sqlite", store.DatabasePath())
	if err != nil {
		t.Fatalf("open trigger connection: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	if _, err := raw.Exec(`
CREATE TRIGGER fail_supersede_finalization
BEFORE UPDATE ON jobs
WHEN NEW.id = 'review-coordinator/task-7/review-1/delegation/correctness'
 AND OLD.payload <> NEW.payload
BEGIN
  SELECT RAISE(ABORT, 'injected finalization failure');
END;`); err != nil {
		t.Fatalf("create finalization failure trigger: %v", err)
	}

	engine := workflow.Engine{Store: store}
	daemon := Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}
	if err := daemon.PollOnce(ctx); err == nil {
		t.Fatal("PollOnce hid the injected finalization failure")
	}

	failed, err := store.GetJob(ctx, child)
	if err != nil {
		t.Fatalf("GetJob(child): %v", err)
	}
	if failed.State != string(workflow.JobFailed) {
		t.Fatalf("child state = %q, want failed: the transition itself must have committed", failed.State)
	}
	if countJobEventKind(t, store, child, workflow.JobEventSupersedeFinalizePending) != 1 {
		t.Fatalf("child events = %+v, want exactly one pending-finalization marker", mustJobEvents(t, store, child))
	}
	if countJobEventKind(t, store, child, workflow.JobEventSupersedeFinalizeCompleted) != 0 {
		t.Fatal("finalization was recorded complete while its own steps failed")
	}
	if countJobEventKind(t, store, child, "advance_completed") != 0 {
		t.Fatal("the parent advance completed even though the synthetic result was never written")
	}

	if _, err := raw.Exec(`DROP TRIGGER fail_supersede_finalization`); err != nil {
		t.Fatalf("drop finalization failure trigger: %v", err)
	}
	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("recovery PollOnce: %v", err)
	}

	if countJobEventKind(t, store, child, workflow.JobEventSupersedeFinalizeCompleted) != 1 {
		t.Fatalf("child events = %+v, want the debt recorded paid exactly once", mustJobEvents(t, store, child))
	}
	if countJobEventKind(t, store, child, "advance_completed") != 1 {
		t.Fatalf("child events = %+v, want the parent advance completed exactly once after recovery", mustJobEvents(t, store, child))
	}
	// Idempotent: a third poll finds no outstanding debt and does nothing new.
	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("third PollOnce: %v", err)
	}
	if got := countJobEventKind(t, store, child, workflow.JobEventSupersedeFinalizeCompleted); got != 1 {
		t.Fatalf("completed markers = %d after an extra poll, want 1", got)
	}
}

func mustJobEvents(t *testing.T, store *db.Store, jobID string) []db.JobEvent {
	t.Helper()
	events, err := store.ListJobEvents(context.Background(), jobID)
	if err != nil {
		t.Fatalf("ListJobEvents(%s): %v", jobID, err)
	}
	return events
}

func countJobEventKind(t *testing.T, store *db.Store, jobID string, kind string) int {
	t.Helper()
	count := 0
	for _, event := range mustJobEvents(t, store, jobID) {
		if event.Kind == kind {
			count++
		}
	}
	return count
}
