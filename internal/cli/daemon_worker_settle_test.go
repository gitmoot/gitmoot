package cli

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
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
// naming the panic or error, and leaves alone a run the engine never claimed.
func TestWorkerExitGuardSettlesOnlyItsOwnClaim(t *testing.T) {
	self := db.CurrentJobRunner()
	ctx := context.Background()
	store := daemonWorkerStore(t)
	worker := jobWorker{Store: store, Stdout: io.Discard}
	for _, tc := range []struct {
		id       string
		claimant db.JobRunner
		disarm   bool
		panicked any
		want     string
	}{
		{id: "panicked-2618", claimant: self, panicked: "boom", want: string(workflow.JobFailed)},
		{id: "not-claimed-2618", claimant: self, disarm: true, want: string(workflow.JobRunning)},
		{id: "foreign-claim-2618", claimant: db.JobRunner{PID: self.PID + 1, BootID: self.BootID}, want: string(workflow.JobRunning)},
	} {
		enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: tc.id, Agent: "audit", Action: "ask", Repo: "owner/repo"})
		if claimed, err := store.ClaimRunningJob(ctx, tc.id, string(workflow.JobQueued), string(workflow.JobRunning), db.JobEvent{Kind: string(workflow.JobRunning)}, tc.claimant.PID, tc.claimant.BootID); err != nil || !claimed {
			t.Fatalf("ClaimRunningJob(%s) claimed=%v err=%v", tc.id, claimed, err)
		}
		guard := beginWorkerExitGuard(tc.id)
		guard.claimed(0)
		if tc.disarm {
			guard.observeRunErr(workflow.JobNotClaimedError{JobID: tc.id, State: "running"})
		}
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
		}
	}
}
