package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// #2618 layer 3: a running row this daemon claimed with no live worker is
// failed after the grace period; a row with a live worker, or one another
// process claimed, is never touched.
func TestOrphanedClaimSweepFailsOnlyWorkerlessOwnClaims(t *testing.T) {
	self := db.CurrentJobRunner()
	if self.BootID == "" {
		t.Skip("kernel boot id unavailable on this platform")
	}
	ctx := context.Background()
	store := daemonWorkerStore(t)
	claims := map[string]db.JobRunner{
		"orphan-2618":  self,
		"live-2618":    self,
		"foreign-2618": {PID: self.PID + 1, BootID: self.BootID},
	}
	for id, runner := range claims {
		enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: id, Agent: "audit", Action: "ask", Repo: "owner/repo"})
		if claimed, err := store.ClaimRunningJob(ctx, id, string(workflow.JobQueued), string(workflow.JobRunning), db.JobEvent{Kind: string(workflow.JobRunning)}, runner.PID, runner.BootID); err != nil || !claimed {
			t.Fatalf("ClaimRunningJob(%s) claimed=%v err=%v", id, claimed, err)
		}
	}
	live := beginWorkerExitGuard("live-2618")
	defer liveJobWorkers.leave(live.jobID)

	var sweep orphanedClaimSweep
	start := time.Now().UTC()
	states := func() map[string]string {
		got := map[string]string{}
		for id := range claims {
			job, err := store.GetJob(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			got[id] = job.State
		}
		return got
	}
	sweep.sweep(ctx, store, io.Discard, start)
	sweep.sweep(ctx, store, io.Discard, start.Add(orphanedClaimGrace-time.Second))
	for id, state := range states() {
		if state != string(workflow.JobRunning) {
			t.Fatalf("%s = %s inside the grace period, want running", id, state)
		}
	}
	sweep.sweep(ctx, store, io.Discard, start.Add(orphanedClaimGrace+orphanedClaimSweepInterval))
	want := map[string]string{"orphan-2618": string(workflow.JobFailed), "live-2618": string(workflow.JobRunning), "foreign-2618": string(workflow.JobRunning)}
	if got := states(); got["orphan-2618"] != want["orphan-2618"] || got["live-2618"] != want["live-2618"] || got["foreign-2618"] != want["foreign-2618"] {
		t.Fatalf("states after grace = %v, want %v", got, want)
	}
}

// #2618 layer 1: the exit guard fails a run it claimed that is still running,
// naming the panic or error, and leaves alone a run it never claimed.
func TestWorkerExitGuardSettlesOnlyItsOwnClaim(t *testing.T) {
	self := db.CurrentJobRunner()
	ctx := context.Background()
	store := daemonWorkerStore(t)
	worker := jobWorker{Store: store, Stdout: io.Discard}
	for _, tc := range []struct {
		id       string
		claimant db.JobRunner
		unarmed  bool
		panicked any
		want     string
	}{
		{id: "panicked-2618", claimant: self, panicked: "boom", want: string(workflow.JobFailed)},
		{id: "not-claimed-2618", claimant: self, unarmed: true, want: string(workflow.JobRunning)},
		{id: "foreign-claim-2618", claimant: db.JobRunner{PID: self.PID + 1, BootID: self.BootID}, want: string(workflow.JobRunning)},
	} {
		enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: tc.id, Agent: "audit", Action: "ask", Repo: "owner/repo"})
		claimSettleTestJob(t, store, tc.id, tc.claimant)
		guard := beginWorkerExitGuard(tc.id)
		guard.armed, guard.generation = !tc.unarmed, 0
		worker.settleWorkerExit(ctx, guard, nil, tc.panicked)
		if n := liveJobWorkers.count(tc.id); n != 0 {
			t.Fatalf("%s live workers after exit = %d, want 0", tc.id, n)
		}
		job, err := store.GetJob(ctx, tc.id)
		if err != nil || job.State != tc.want {
			t.Fatalf("%s = %s (err %v), want %s", tc.id, job.State, err, tc.want)
		}
		if tc.panicked != nil {
			events, err := store.ListJobEvents(ctx, tc.id)
			if err != nil {
				t.Fatal(err)
			}
			last := events[len(events)-1]
			if last.Kind != jobRecoveryFailedEvent || !strings.Contains(last.Message, "worker panicked: boom") {
				t.Fatalf("last event = %+v, want %s naming the panic", last, jobRecoveryFailedEvent)
			}
			// A top-level job owes no parent advancement.
			if owed, err := worker.jobNeedsAdvanceRetry(ctx, tc.id); err != nil || owed {
				t.Fatalf("%s advance retry owed = %v (err %v), want false for a top-level job", tc.id, owed, err)
			}
		}
	}
}

func claimSettleTestJob(t *testing.T, store *db.Store, id string, runner db.JobRunner) {
	t.Helper()
	if claimed, err := store.ClaimRunningJob(context.Background(), id, string(workflow.JobQueued), string(workflow.JobRunning), db.JobEvent{Kind: string(workflow.JobRunning)}, runner.PID, runner.BootID); err != nil || !claimed {
		t.Fatalf("ClaimRunningJob(%s) claimed=%v err=%v", id, claimed, err)
	}
}

// PR #2626 review, P1. An old worker's guard claimed generation 0 and passed
// every in-process check; meanwhile the job finished, an operator retried it,
// and this same daemon claimed generation 1 for a healthy successor. The old
// guard must not fail the successor: it owns generation 0 only.
func TestReview2626OldGuardCannotSettleLiveSuccessor(t *testing.T) {
	self := db.CurrentJobRunner()
	ctx := context.Background()
	store := daemonWorkerStore(t)
	const id = "successor-2626"
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: id, Agent: "audit", Action: "ask", Repo: "owner/repo"})
	claimSettleTestJob(t, store, id, self)
	old := beginWorkerExitGuard(id)
	old.armed, old.generation = true, 0

	if ok, err := store.TransitionJobStateWithEvent(ctx, id, string(workflow.JobRunning), string(workflow.JobFailed), db.JobEvent{Kind: string(workflow.JobFailed)}); err != nil || !ok {
		t.Fatalf("finish generation 0: %v %v", ok, err)
	}
	if ok, err := store.TransitionJobStateWithEvent(ctx, id, string(workflow.JobFailed), string(workflow.JobQueued), db.JobEvent{Kind: "retry_queued"}); err != nil || !ok {
		t.Fatalf("operator retry: %v %v", ok, err)
	}
	claimSettleTestJob(t, store, id, self)

	jobWorker{Store: store, Stdout: io.Discard}.settleWorkerExit(ctx, old, nil, nil)
	job, err := store.GetJob(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != string(workflow.JobRunning) || job.LifecycleGeneration != 1 {
		t.Fatalf("successor = state %s generation %d, want running at generation 1 (the old guard owns generation 0 only)", job.State, job.LifecycleGeneration)
	}
}

// PR #2626 review, P2. A delegation child the daemon fails on its worker's
// behalf has not advanced its parent's DAG; the advance-retry pass must be able
// to find that debt, or a single-child coordinator strands.
func TestReview2626GuardSettlementLeavesAdvanceDebtDiscoverable(t *testing.T) {
	self := db.CurrentJobRunner()
	ctx := context.Background()
	store := daemonWorkerStore(t)
	const id = "child-2626"
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: id, Agent: "audit", Action: "ask", Repo: "owner/repo"})
	queued, err := store.GetJob(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := workflow.ParseJobPayload(queued.Payload)
	if err != nil {
		t.Fatal(err)
	}
	payload.ParentJobID = "parent-2626"
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateJobPayload(ctx, id, string(encoded)); err != nil {
		t.Fatal(err)
	}
	claimSettleTestJob(t, store, id, self)
	guard := beginWorkerExitGuard(id)
	guard.armed, guard.generation = true, 0
	worker := jobWorker{Store: store, Stdout: io.Discard}
	worker.settleWorkerExit(ctx, guard, nil, nil)

	job, err := store.GetJob(ctx, id)
	if err != nil || job.State != string(workflow.JobFailed) {
		t.Fatalf("child = %s (err %v), want failed", job.State, err)
	}
	owed, err := worker.jobNeedsAdvanceRetry(ctx, id)
	if err != nil || !owed {
		events, _ := store.ListJobEvents(ctx, id)
		t.Fatalf("advance retry owed = %v (err %v), want true; events %+v", owed, err, events)
	}
}

type failingDeliveryAdapter struct{}

func (failingDeliveryAdapter) Deliver(context.Context, runtime.Agent, runtime.Job) (runtime.Result, error) {
	return runtime.Result{}, errors.New("runtime died before a result")
}

// #2618 on the engine-claimed path: Mailbox.Run's claim reports its generation
// to run()'s guard, so a local job whose terminal writes were lost is still
// settled; a produced result is recorded instead of lost.
func TestLocalWorkerExitSettlesEngineClaimedRun(t *testing.T) {
	for _, tc := range []struct {
		name      string
		adapter   workflow.DeliveryAdapter
		lose      int // Mailbox.fail writes twice: with the payload, then state only
		wantState string
		wantKind  string
	}{
		{name: "no_result", adapter: failingDeliveryAdapter{}, lose: 2, wantState: string(workflow.JobFailed), wantKind: jobRecoveryFailedEvent},
		{name: "result_produced", adapter: &timeoutCaptureAdapter{}, lose: 1, wantState: string(workflow.JobSucceeded), wantKind: string(workflow.JobSucceeded)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := daemonWorkerStore(t)
			id := "local-2618-" + tc.name
			seedDaemonWorkerAgent(t, store, "plain", runtime.ShellRuntime, "", []string{"ask"}, "owner/repo")
			enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: id, Agent: "plain", Action: "ask", Repo: "owner/repo", Branch: "main"})
			job, err := store.GetJob(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.ExecForTest(ctx, fmt.Sprintf(`CREATE TRIGGER lose_terminal_writes BEFORE UPDATE OF state ON jobs
				WHEN OLD.state = 'running' AND NEW.state IN ('succeeded', 'failed', 'blocked')
				AND (SELECT COUNT(*) FROM job_events WHERE job_id = OLD.id AND kind = 'test_terminal_write_lost') < %d
				BEGIN
					INSERT INTO job_events (job_id, kind, message) VALUES (OLD.id, 'test_terminal_write_lost', NEW.state);
					SELECT RAISE(IGNORE);
				END`, tc.lose)); err != nil {
				t.Fatal(err)
			}
			worker := defaultJobWorker(store, io.Discard, t.TempDir())
			worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
				return t.TempDir(), nil
			}
			worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) { return tc.adapter, nil }
			runErr := worker.run(ctx, job)

			finished, err := store.GetJob(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			events, err := store.ListJobEvents(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			lost, sawKind := 0, false
			var trail []string
			for _, event := range events {
				trail = append(trail, event.Kind)
				if event.Kind == "test_terminal_write_lost" {
					lost++
				}
				sawKind = sawKind || event.Kind == tc.wantKind
			}
			if lost != tc.lose || finished.State != tc.wantState || !sawKind {
				t.Fatalf("lost=%d state=%s (run err %v), want lost=%d state=%s with a %s event; events %v", lost, finished.State, runErr, tc.lose, tc.wantState, tc.wantKind, trail)
			}
		})
	}
}
