//go:build e2e

package cli

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
)

// countHeartbeatJobs returns every persisted job whose id carries the
// heartbeatJobID prefix, i.e. jobs the heartbeat scan actually enqueued (not the
// recording-fake jobs the unit tests use). The full-chain assertions count these
// to prove "exactly one enqueue per due window".
func countHeartbeatJobs(t *testing.T, store *db.Store) []db.Job {
	t.Helper()
	jobs, err := store.ListJobs(context.Background())
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	out := make([]db.Job, 0, len(jobs))
	for _, j := range jobs {
		if strings.HasPrefix(j.ID, "heartbeat-") {
			out = append(out, j)
		}
	}
	return out
}

// TestHeartbeatLoopFullChainE2E is the full-chain, NO-LLM, deterministic E2E for
// agent heartbeat schedules (#533/#558 MVP + #564 finalize). The existing
// daemon_heartbeat_test.go calls runHeartbeatScanOnce DIRECTLY with a recording
// fake enqueuer; nothing drives the LIVE daemon chain end-to-end. This drives the
// REAL chain a daemon supervisor iteration runs:
//
//	write-side CLI (`agent heartbeat add`) writes the config section
//	  -> runHeartbeatScanOnce with the PRODUCTION enqueuer (real Mailbox) ENQUEUES a real queued job
//	    -> the REAL worker tick (runEnabledRepoWorkerTicks -> runQueuedJobsForRepo -> worker.run)
//	       CLAIMS + RUNS the job through the REAL shell adapter to a TERMINAL succeeded state
//	      -> heartbeat_state.next_due advances + last_status is recorded
//	        -> a daemon RESTART (fresh in-memory scan/enqueuer, SAME persisted store)
//	           does NOT re-fire the same due window (restart-safe dedup lives in the persisted next_due)
//
// The clock is INJECTED (now is the scan's parameter) so "due" is deterministic;
// the shell runtime keeps it offline (no real LLM / GitHub). It MUST go red if any
// link breaks: scan-not-enqueued, worker-didn't-run, next_due-not-advanced, or a
// restart double-fire.
func TestHeartbeatLoopOffByDefaultE2E(t *testing.T) {
	ctx := context.Background()
	home, paths, store := heartbeatLoopE2EHome(t)
	checkout := createDaemonWorkerGitCheckout(t, "main")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "maintainer", runtime.ShellRuntime, heartbeatShellResultScript, []string{"ask"}, "owner/repo")
	// NOTE: no `agent heartbeat add` — the config has zero heartbeat sections.

	now := time.Date(2026, 6, 30, 9, 0, 0, 0, time.UTC)
	enqueue := newHeartbeatEnqueuer(store, home)
	if err := runHeartbeatScanOnce(ctx, paths, store, enqueue, now); err != nil {
		t.Fatalf("off-by-default scan: %v", err)
	}
	worker := defaultJobWorker(store, io.Discard, home)
	if err := runEnabledRepoWorkerTicksTracked(ctx, store, worker, 1, "", io.Discard, now, nil, nil); err != nil {
		t.Fatalf("off-by-default worker tick: %v", err)
	}
	if jobs := countHeartbeatJobs(t, store); len(jobs) != 0 {
		t.Fatalf("off-by-default enqueued %d heartbeat jobs, want 0: %+v", len(jobs), jobs)
	}
	if _, found, err := store.GetHeartbeatState(ctx, "maintainer", "beat"); err != nil || found {
		t.Fatalf("off-by-default wrote heartbeat_state: found=%v err=%v", found, err)
	}
}
