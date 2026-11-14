package daemon

import (
	"context"
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
