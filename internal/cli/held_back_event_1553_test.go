package cli

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// #1553: a queued job the dispatcher looked at and refused to claim recorded the
// reason only on daemon stdout. `gitmoot job events <id>` showed the rows written
// at the INSTANT of dispatch and nothing about the interval before it, so a job
// waiting 120 minutes and a job about to run in one second read identically.
//
// The two arms below are the whole property. The refusal arm alone would pass
// against a version that recorded the event unconditionally, which is why the
// control arm dispatches a job that is NOT held back and requires silence.
func heldBackEvents(t *testing.T, store *db.Store, jobID string) []db.JobEvent {
	t.Helper()
	events, err := store.ListJobEvents(context.Background(), jobID)
	if err != nil {
		t.Fatalf("ListJobEvents(%s): %v", jobID, err)
	}
	var held []db.JobEvent
	for _, event := range events {
		if event.Kind == dispatchHeldBackEventKind {
			held = append(held, event)
		}
	}
	return held
}

func TestHeldBackJobRecordsItsReasonInJobEvents(t *testing.T) {
	resetHeldBackWarnState()
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "coder", runtime.CodexRuntime, "session-1", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-big", Agent: "coder", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})

	worker := poolSchedulerWorker(t, store, &cliWorkerFakeAdapter{output: poolSchedulerAskResult}, false)
	// Cap below the smallest estimate: the job is refused, and stays queued.
	worker.Admission = newAdmissionBudget(config.AdmissionPolicy{MaxMemoryGB: 0.1})
	tracker := newInflightJobTracker(ctx)

	if err := dispatchQueuedJobsTracked(ctx, worker, 2, 2, "owner/repo", "", tracker); err != nil {
		t.Fatalf("dispatchQueuedJobsTracked: %v", err)
	}
	if job, _ := store.GetJob(ctx, "job-big"); job.State != string(workflow.JobQueued) {
		t.Fatalf("job-big state = %q, want queued (the fixture must actually refuse it)", job.State)
	}
	held := heldBackEvents(t, store, "job-big")
	if len(held) != 1 {
		t.Fatalf("dispatch_held_back events = %d, want 1; a refused job must say why in its own record", len(held))
	}
	if !strings.Contains(held[0].Message, "NEVER fit") || !strings.Contains(held[0].Message, "max_memory_gb") {
		t.Fatalf("held-back message = %q, want the admission cap named; an event that does not carry the reason\nis the same silence in a new row", held[0].Message)
	}

	// THE BOUND, and it is why the event rides the existing log throttle rather
	// than every skip: an unthrottled per-pass append is what grew job_events to
	// ~1.8M rows. A second pass inside the window must add nothing.
	if err := dispatchQueuedJobsTracked(ctx, worker, 2, 2, "owner/repo", "", tracker); err != nil {
		t.Fatalf("dispatchQueuedJobsTracked (second pass): %v", err)
	}
	if held := heldBackEvents(t, store, "job-big"); len(held) != 1 {
		t.Fatalf("dispatch_held_back events after a second refusing pass = %d, want 1 (throttle regressed)", len(held))
	}
}

// The control. Without it the assertion above is satisfied by a version that
// records the event on every dispatch, which would restore the original defect
// in the opposite direction: an event that is always present distinguishes
// nothing either.
func TestDispatchedJobRecordsNoHeldBackEvent(t *testing.T) {
	resetHeldBackWarnState()
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "coder", runtime.CodexRuntime, "session-1", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-ok", Agent: "coder", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})

	worker := poolSchedulerWorker(t, store, &cliWorkerFakeAdapter{output: poolSchedulerAskResult}, false)
	tracker := newInflightJobTracker(ctx)

	if err := dispatchQueuedJobsTracked(ctx, worker, 2, 2, "owner/repo", "", tracker); err != nil {
		t.Fatalf("dispatchQueuedJobsTracked: %v", err)
	}
	tracker.drain(io.Discard, 10*time.Second)
	if job, _ := store.GetJob(ctx, "job-ok"); job.State == string(workflow.JobQueued) {
		t.Fatalf("job-ok is still queued; the control must dispatch, or its silence proves nothing")
	}
	if held := heldBackEvents(t, store, "job-ok"); len(held) != 0 {
		t.Fatalf("dispatch_held_back events on a job that ran = %d, want 0; msg=%q", len(held), held[0].Message)
	}
}

// The pool path (#1553 second half). A job the SELECTOR declines - it is scanned,
// it is runnable-looking, but an in-flight job holds the repo checkout - reaches
// the bottom of the dispatch loop through a bare `continue` and parks on the
// re-query bound. This is the shape behind the 120-minute worst case.
func TestPoolWaitRecordsTheHeldBackJob(t *testing.T) {
	resetHeldBackWarnState()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")

	adapter := newWedgeBlockingAdapter("job-a")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})
	worker := poolSchedulerWorker(t, store, adapter, false)
	worker.Stdout = io.Discard
	live := newDaemonReloadableConfig(30*time.Second, 2, false)
	var checkoutLock sync.Mutex
	tracker := newInflightJobTracker(ctx)
	t.Cleanup(func() { tracker.drain(io.Discard, 5*time.Second) })
	_ = startSingleRepoWorkerLoop(ctx, 5*time.Millisecond, store, worker, live, &checkoutLock, tracker, "owner/repo", "", io.Discard)

	if !waitForCondition(t, 5*time.Second, adapter.stillBlocked) {
		t.Fatalf("job-a never started delivering")
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-b", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 2})

	if !waitForCondition(t, 5*time.Second, func() bool { return len(heldBackEvents(t, store, "job-b")) > 0 }) {
		t.Fatalf("job-b waited behind job-a and its record never said so")
	}
	held := heldBackEvents(t, store, "job-b")
	t.Logf("held-back rows for job-b: %d; first message = %q", len(held), held[0].Message)
	if len(held) != 1 {
		t.Fatalf("dispatch_held_back rows = %d, want exactly 1; the named refusal and the wait record must not both file", len(held))
	}
	close(adapter.release)
	if got := waitForJobState(t, store, "job-b", string(workflow.JobSucceeded), 10*time.Second); got != string(workflow.JobSucceeded) {
		t.Fatalf("job-b state = %q after release, want succeeded", got)
	}
}

// The fallback arm. explainHeldBackJobs walked the declined jobs and returned
// silently for the two cases it could not attribute: a job with no runtime key,
// and one whose lock row will not read. Those jobs were declined by the selector
// - the wait is certain - so silence there is the defect, not caution.
func TestUnattributableHeldBackJobStillRecordsTheWait(t *testing.T) {
	resetHeldBackWarnState()
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-mute", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})

	worker := poolSchedulerWorker(t, store, &cliWorkerFakeAdapter{output: poolSchedulerAskResult}, false)
	worker.Stdout = io.Discard
	// No in-flight job holds anything, and no runtime session lock exists: both
	// attribution attempts miss, which is precisely the silent path.
	tracker := newInflightJobTracker(ctx)
	job, err := store.GetJob(ctx, "job-mute")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if holder := tracker.holderOf(queuedJobCheckoutKey(ctx, store, job)); holder != "" {
		t.Fatalf("fixture holds checkout %q; the unattributable arm would not be exercised", holder)
	}

	explainHeldBackJobs(ctx, worker, tracker, []db.Job{job})

	held := heldBackEvents(t, store, "job-mute")
	if len(held) != 1 {
		t.Fatalf("dispatch_held_back rows for an unattributable wait = %d, want 1", len(held))
	}
	if !strings.Contains(held[0].Message, "not selected this dispatch pass") {
		t.Fatalf("message = %q, want the fact of the wait recorded even without a cause", held[0].Message)
	}
}
