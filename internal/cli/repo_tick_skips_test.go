package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
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
