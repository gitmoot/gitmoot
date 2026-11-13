package cli

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestFinishQueuedReviewFailureWakesExactHeadWaiter(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	key, err := db.ReviewVerdictSubjectKey("acme/widget", 2165, "head-preflight")
	if err != nil {
		t.Fatal(err)
	}
	fact, _, err := store.SubscribeAwaitedFact(ctx, db.AwaitedFactSubscription{
		WaiterRole: "lane", SubjectKind: db.AwaitedFactSubjectReviewVerdict,
		SubjectKey: key, Deadline: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	payload := mustJobPayload(t, workflow.JobPayload{
		Repo: "acme/widget", PullRequest: 2165, HeadSHA: "head-preflight",
		WorkflowID: "preflight-review",
	})
	if err := store.CreateJobWithEvent(ctx, db.Job{
		ID: "review-preflight", Agent: "reviewer", Type: "review",
		State: string(workflow.JobQueued), Payload: payload,
	}, db.JobEvent{Kind: string(workflow.JobQueued), Message: "queued"}); err != nil {
		t.Fatal(err)
	}
	job, err := store.GetJob(ctx, "review-preflight")
	if err != nil {
		t.Fatal(err)
	}
	worker := jobWorker{Store: store, Stdout: io.Discard}
	if err := worker.finishQueuedJob(ctx, job, workflow.JobFailed, errors.New("adapter preflight failed")); err != nil {
		t.Fatal(err)
	}
	gotFact, err := store.GetAwaitedFact(ctx, fact.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotFact.State != db.AwaitedFactStateWaiting {
		t.Fatalf("awaited fact state = %q, want waiting", gotFact.State)
	}
	outbox, err := store.ListWakeOutbox(ctx, db.WakeOutboxStatePending)
	if err != nil {
		t.Fatal(err)
	}
	if len(outbox) != 1 || outbox[0].TargetRole != "lane" ||
		!strings.Contains(outbox[0].SourceID, `"state":"review_failed"`) {
		t.Fatalf("preflight failure wake = %+v", outbox)
	}
}

type deadlineBlockingAdapter struct{}

func (deadlineBlockingAdapter) Deliver(ctx context.Context, _ runtime.Agent, _ runtime.Job) (runtime.Result, error) {
	<-ctx.Done()
	return runtime.Result{
		SessionDiag: &runtime.SessionDiag{Stderr: "runner stderr deadline-tail"},
	}, ctx.Err()
}

func TestRunDeadlinePersistsTopLevelTimeoutEvidence(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "job-timeout", Agent: "audit", Action: "ask", Repo: "owner/repo",
		Branch: "main", JobTimeout: "2s",
	})
	job, err := store.GetJob(ctx, "job-timeout")
	if err != nil {
		t.Fatal(err)
	}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return deadlineBlockingAdapter{}, nil
	}

	if err := worker.run(ctx, job); err != nil {
		t.Fatalf("worker.run returned error: %v", err)
	}
	stored, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != string(workflow.JobFailed) {
		t.Fatalf("stored job state = %q, want failed", stored.State)
	}
	events, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	var timeoutMessage string
	for _, event := range events {
		if event.Kind == "job_timeout" {
			timeoutMessage = event.Message
		}
	}
	if timeoutMessage == "" {
		t.Fatalf("stored events = %+v, want job_timeout", events)
	}
	for _, want := range []string{`"deadline":`, `"elapsed":`, "deadline-tail"} {
		if !strings.Contains(timeoutMessage, want) {
			t.Fatalf("stored job_timeout = %q, want %q", timeoutMessage, want)
		}
	}
}

func TestRecoverKillPendingJobFailsInsteadOfRequeueing(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	for _, id := range []string{"job-witnessed", "job-no-witness"} {
		enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: id, Agent: "audit", Action: "review", Repo: "owner/repo"})
		if claimed, err := store.ClaimRunningJob(ctx, id, string(workflow.JobQueued), string(workflow.JobRunning), db.JobEvent{Kind: string(workflow.JobRunning)}, 1, "boot"); err != nil || !claimed {
			t.Fatalf("ClaimRunningJob(%s) claimed=%v err=%v", id, claimed, err)
		}
	}
	// A prior attempt's terminal event must not mask a later attempt's fresh kill
	// witness; recovery is ordered relative to the newest job_kill_pending event.
	if err := store.AddJobEvent(ctx, db.JobEvent{JobID: "job-witnessed", Kind: "job_kill_pending", Message: "prior deadline"}); err != nil {
		t.Fatal(err)
	}
	if err := store.AddJobEvent(ctx, db.JobEvent{JobID: "job-witnessed", Kind: string(workflow.JobFailed), Message: "prior attempt"}); err != nil {
		t.Fatal(err)
	}
	stop := armJobKillPending(store, "job-witnessed", time.Now().Add(time.Second))
	defer stop()
	deadline := time.Now().Add(time.Second)
	for {
		if event, found, err := store.GetLatestJobEventByKind(ctx, "job-witnessed", "job_kill_pending"); err != nil {
			t.Fatal(err)
		} else if found && strings.Contains(event.Message, "deadline=") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("job_kill_pending was not journaled before the deadline")
		}
		time.Sleep(time.Millisecond)
	}
	if err := recoverKillPendingJobs(ctx, store, io.Discard); err != nil {
		t.Fatal(err)
	}
	witnessed, _ := store.GetJob(ctx, "job-witnessed")
	if witnessed.State != string(workflow.JobFailed) {
		t.Fatalf("witnessed job state = %q, want failed", witnessed.State)
	}
	events, _ := store.ListJobEvents(ctx, witnessed.ID)
	if events[len(events)-1].Kind != jobRecoveryFailedEvent || !strings.Contains(events[len(events)-1].Message, "daemon died mid-kill") || !strings.Contains(events[len(events)-1].Message, "killed-by-deadline-unwitnessed") {
		t.Fatalf("witnessed job events = %+v, want terminal unwitnessed-deadline reason", events)
	}
	noWitness, _ := store.GetJob(ctx, "job-no-witness")
	if noWitness.State != string(workflow.JobRunning) {
		t.Fatalf("job without kill witness state = %q, want running", noWitness.State)
	}
}

// observedJobLifecycleForTest reads the row a test just seeded and returns its lifecycle, so a
// test settles the run it actually created rather than a hard-coded generation.
func observedJobLifecycleForTest(t *testing.T, store *db.Store, jobID string) jobLifecycle {
	t.Helper()
	job, err := store.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatalf("GetJob(%s) returned error: %v", jobID, err)
	}
	return observedJobLifecycle(job)
}
