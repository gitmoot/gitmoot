package cli

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// TestWorkerSweepSkipsBusyRepoAndReachesTheRest is #2135's cadence test, and the
// property is CADENCE, NOT COMPLETION. Asserting "the job eventually dispatches"
// passes today, with the bug, at ~78 minutes: the sweep blocks on the busy repo's
// lock and every repo behind it waits out the poll. So this asserts the sweep
// COMPLETES while a repo's lock is held by someone else - which it cannot do
// while the acquisition is a blocking Lock().
func TestWorkerSweepSkipsBusyRepoAndReachesTheRest(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	for _, name := range []string{"aaa-busy", "zzz-free"} {
		if err := store.UpsertRepo(ctx, db.Repo{Owner: "owner", Name: name, CheckoutPath: t.TempDir(), Enabled: true}); err != nil {
			t.Fatalf("UpsertRepo %s: %v", name, err)
		}
	}
	// The busy repo sorts FIRST, so a blocking acquisition stalls before the free
	// repo is ever considered - the ordering is the whole point of the fixture.
	locks := &repoCheckoutLocks{}
	busy := locks.For("owner/aaa-busy")
	busy.Lock()
	defer busy.Unlock()
	clearRepoTickSkip("owner/aaa-busy")
	defer clearRepoTickSkip("owner/aaa-busy")

	var out bytes.Buffer
	worker := jobWorker{Store: store, Stdout: &out}
	done := make(chan error, 1)
	go func() {
		done <- runEnabledRepoWorkerTicksTracked(ctx, store, worker, 1, "", &out, time.Now().UTC(), locks, nil)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("sweep returned %v, want it to complete past the busy repo", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("sweep did not finish while another holder had one repo's checkout lock: it is waiting on that lock, so every repo behind the busy one gets no dispatch this sweep")
	}

	if !strings.Contains(out.String(), "owner/aaa-busy: worker tick skipped, checkout busy") {
		t.Fatalf("busy repo did not report a skip, so a starving repo would not name itself in the log; output=%q", out.String())
	}
}

// TestRepoTickSkipForcesLockAfterThreshold covers the STARVATION VALVE, which is
// the arm that would otherwise ship untested - and an untested safety valve is
// the corpus defect this campaign keeps finding. Without it the fix trades a
// visible fleet-wide stall for one repo silently late forever.
func TestRepoTickSkipForcesLockAfterThreshold(t *testing.T) {
	const repo = "owner/starving"
	clearRepoTickSkip(repo)
	defer clearRepoTickSkip(repo)

	if skippedRepoTickNeedsLock(repo) {
		t.Fatal("a repo with no skips already demands a blocking acquisition; every contended sweep would stall")
	}
	for i := 1; i < repoTickSkipForceAfter; i++ {
		if got := recordRepoTickSkip(repo); got != i {
			t.Fatalf("skip %d recorded as %d", i, got)
		}
		if skippedRepoTickNeedsLock(repo) {
			t.Fatalf("forced a blocking acquisition after %d skips, before the %d threshold", i, repoTickSkipForceAfter)
		}
	}
	if got := recordRepoTickSkip(repo); got != repoTickSkipForceAfter {
		t.Fatalf("threshold skip recorded as %d, want %d", got, repoTickSkipForceAfter)
	}
	if !skippedRepoTickNeedsLock(repo) {
		t.Fatalf("after %d consecutive skips the repo still does not force the lock, so it can be starved indefinitely", repoTickSkipForceAfter)
	}

	// THE STREAK MUST MEASURE CONSECUTIVE MISSES, NOT LIFETIME MISSES. A tick that
	// runs clears it, so an occasionally-contended repo never accumulates its way
	// into forcing - otherwise every repo eventually blocks and the fix undoes
	// itself over time.
	clearRepoTickSkip(repo)
	if skippedRepoTickNeedsLock(repo) {
		t.Fatal("a repo whose tick ran still demands a blocking acquisition; the counter is measuring lifetime skips rather than consecutive ones")
	}
}

// TestWorkerSweepReachesTheSelectorOnRepoBehindABusyRepo is the strong form of
// the cadence property. "The sweep completed" is weaker than what #2135 claims:
// it does not prove the SELECTOR ran for the repo behind the busy one, which is
// the thing that was never happening.
//
// The observable is #2117's own instrument, used as a probe. Two jobs on repo B
// contend for one checkout key, so the selector must DECLINE the second and
// write a dispatch_declined event. That event can only exist if the selector was
// reached for repo B in this sweep - which is exactly what a blocking
// acquisition at repo A prevents.
func TestWorkerSweepReachesTheSelectorOnRepoBehindABusyRepo(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	for _, name := range []string{"aaa-busy", "zzz-free"} {
		if err := store.UpsertRepo(ctx, db.Repo{Owner: "owner", Name: name, CheckoutPath: t.TempDir(), Enabled: true}); err != nil {
			t.Fatalf("UpsertRepo %s: %v", name, err)
		}
	}
	uniq := t.Name()
	// Two jobs on the FREE repo sharing one runtime session, so the second is
	// declined rather than admitted - a decline is the observable, and it needs
	// the selector to have run.
	for i := 0; i < 2; i++ {
		agent := fmt.Sprintf("lead-%d-%s", i, uniq)
		task := fmt.Sprintf("task-%d-%s", i, uniq)
		seedDaemonWorkerAgent(t, store, agent, runtime.CodexRuntime, "session-shared", []string{"implement"}, "owner/zzz-free")
		if err := store.UpsertTask(ctx, db.Task{ID: task, RepoFullName: "owner/zzz-free", State: string(workflow.TaskImplementing), Branch: task, WorktreePath: "/tmp/gitmoot/" + task}); err != nil {
			t.Fatalf("UpsertTask: %v", err)
		}
		enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: fmt.Sprintf("job-%d-%s", i, uniq), Agent: agent, Action: "implement", Repo: "owner/zzz-free", Branch: task, TaskID: task})
	}

	locks := &repoCheckoutLocks{}
	busy := locks.For("owner/aaa-busy")
	busy.Lock()
	defer busy.Unlock()
	clearRepoTickSkip("owner/aaa-busy")
	defer clearRepoTickSkip("owner/aaa-busy")

	var out bytes.Buffer
	worker := jobWorker{Store: store, Stdout: &out}
	done := make(chan error, 1)
	go func() {
		done <- runEnabledRepoWorkerTicksTracked(ctx, store, worker, 8, "", &out, time.Now().UTC(), locks, nil)
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("sweep blocked on the busy repo's lock, so the selector was never reached for the repo behind it")
	}

	// THE OBSERVABLE IS A STATE CHANGE ON REPO B'S JOBS. They can only leave
	// `queued` if the sweep reached repo B's dispatch, which is precisely what a
	// blocking acquisition at repo A prevents.
	//
	// My first attempt asserted a dispatch_declined event instead, on the theory
	// that two jobs sharing a runtime session would make the selector decline the
	// second. It did not: both were ADMITTED and then failed downstream on a
	// checkout precondition, so no decline was ever written. The failure output
	// proved the property while the assertion denied it - the observable was
	// wrong, not the fix.
	queued, err := store.ListQueuedJobs(ctx)
	if err != nil {
		t.Fatalf("ListQueuedJobs: %v", err)
	}
	stillQueued := 0
	for _, job := range queued {
		if strings.Contains(job.ID, uniq) {
			stillQueued++
		}
	}
	if stillQueued == 2 {
		t.Fatalf("both of the free repo's jobs are still queued: the sweep never reached repo B's dispatch, which is the defect #2135 describes; output=%q", out.String())
	}
}

// TestWorkerSweepDefersTheForcedTurnToTheEnd pins the residual that the deferred
// valve exists to remove. Taking the forced blocking acquisition IN PLACE would
// reintroduce head-of-line blocking at a 1-in-N duty cycle: on that sweep every
// repo behind the starved one waits exactly as it does today. Deferring keeps the
// guarantee and removes the residual, so the forced turn must be reported AFTER
// the free repo's tick rather than before it.
func TestWorkerSweepDefersTheForcedTurnToTheEnd(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	for _, name := range []string{"aaa-busy", "zzz-free"} {
		if err := store.UpsertRepo(ctx, db.Repo{Owner: "owner", Name: name, CheckoutPath: t.TempDir(), Enabled: true}); err != nil {
			t.Fatalf("UpsertRepo %s: %v", name, err)
		}
	}
	locks := &repoCheckoutLocks{}
	busy := locks.For("owner/aaa-busy")
	busy.Lock()
	clearRepoTickSkip("owner/aaa-busy")
	defer clearRepoTickSkip("owner/aaa-busy")
	// Put the busy repo one skip below the threshold so THIS sweep forces it.
	for i := 0; i < repoTickSkipForceAfter; i++ {
		recordRepoTickSkip("owner/aaa-busy")
	}

	var out bytes.Buffer
	worker := jobWorker{Store: store, Stdout: &out}
	done := make(chan error, 1)
	go func() {
		done <- runEnabledRepoWorkerTicksTracked(ctx, store, worker, 1, "", &out, time.Now().UTC(), locks, nil)
	}()
	// The forced turn must WAIT for the lock, so the sweep cannot finish while it
	// is held - and crucially it must not finish EARLY either, which would mean
	// the guarantee was dropped rather than deferred.
	select {
	case <-done:
		t.Fatal("sweep finished while the forced repo's lock was still held: the guaranteed turn was skipped, not deferred")
	case <-time.After(2 * time.Second):
	}
	busy.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("sweep returned %v after the forced turn became available", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("sweep did not finish after the forced repo's lock was released")
	}
	if !strings.Contains(out.String(), "owner/aaa-busy: worker tick taking a forced turn") {
		t.Fatalf("forced turn was not reported, so a starving repo's guaranteed turn is invisible; output=%q", out.String())
	}
}
