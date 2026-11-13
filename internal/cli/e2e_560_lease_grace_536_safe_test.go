package cli

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// TestE2E560ExpiredLeaseFailsStuckWorker proves crash recovery still works
// after the grace is added: a genuinely stuck/crashed worker whose runtime lease
// (sized jobTimeout+grace, as run() sizes it) has FULLY elapsed is promptly
// failed (running -> failed) and its expired lease reaped — well before the
// coarse 30m stale window. This is the #560 recovery path; without it a crashed
// worker's owner would strand in 'running' until the coarse backstop.
func TestE2E560ExpiredLeaseFailsStuckWorker(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "job-stuck", Agent: "audit", Action: "ask", Repo: "owner/repo",
		Branch: "main", PullRequest: 1, JobTimeout: "10m",
	})
	if err := store.UpdateJobState(ctx, "job-stuck", string(workflow.JobRunning)); err != nil {
		t.Fatalf("UpdateJobState returned error: %v", err)
	}
	now := time.Now().UTC()
	// The lease was sized exactly as run() sizes it (jobTimeout+grace) but the worker
	// crashed and the whole lease has since elapsed — it expired a minute ago.
	leaseTTL := 10*time.Minute + runtimeLeaseTeardownGrace
	acquiredAt := now.Add(-(leaseTTL + time.Minute))
	const lockKey = "runtime:codex:session-stuck"
	acquired, err := store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey:   lockKey,
		OwnerJobID:    "job-stuck",
		OwnerToken:    "token-stuck",
		OwnerPID:      deadPID(t),
		OwnerHostname: thisHostname(t),
		ExpiresAt:     acquiredAt.Add(leaseTTL).Format(time.RFC3339Nano),
	}, acquiredAt)
	if err != nil || !acquired {
		t.Fatalf("AcquireResourceLock returned acquired=%v err=%v", acquired, err)
	}

	// A recovery tick "now" — far short of the 30m coarse window (the job went
	// running ~now) — must still recover the crashed worker via the expired-lease
	// reaper.
	recovery := defaultJobWorker(store, io.Discard)
	if err := runDaemonWorkerTickTracked(ctx, store, recovery, 0, false, "owner/repo", "", io.Discard, now, nil, nil); err != nil {
		t.Fatalf("recovery tick returned error: %v", err)
	}
	job, err := store.GetJob(ctx, "job-stuck")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobFailed) {
		t.Fatalf("job state = %q, want failed; a crashed worker with a fully-expired lease must settle", job.State)
	}
	if _, err := store.GetResourceLock(ctx, lockKey); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetResourceLock after expired-lease reap = %v, want sql.ErrNoRows", err)
	}
}

// TestE2E560SubFloorStaleWindowRejected pins the env floor (#560 finding 3):
// GITMOOT_STALE_RUNNING_AFTER is one crash-detection leg, not a timeout, so a
// sub-floor value must be rejected in favor of the default window rather than
// turned into an aggressive killer. Even after the age leg matures, missing
// transcript and process evidence must leave the job running.
func TestE2E560SubFloorStaleWindowRejected(t *testing.T) {
	t.Setenv("GITMOOT_STALE_RUNNING_AFTER", "1s") // below the 1m floor
	if got := configuredDaemonRunningJobStaleAfter(io.Discard); got != defaultDaemonRunningJobStaleAfter {
		t.Fatalf("sub-floor stale window = %s, want default %s", got, defaultDaemonRunningJobStaleAfter)
	}

	seed := func(t *testing.T) (*db.Store, jobWorker, time.Time) {
		store := daemonWorkerStore(t)
		seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
		// A shell runtime has no runtime-session lease; the new liveness predicate
		// must therefore stay fail-closed when transcript/PID evidence is absent.
		seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
		enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
			ID: "job-x", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1,
		})
		if err := store.UpdateJobState(context.Background(), "job-x", string(workflow.JobRunning)); err != nil {
			t.Fatalf("UpdateJobState returned error: %v", err)
		}
		return store, defaultJobWorker(store, io.Discard), time.Now().UTC()
	}

	// A 5m-old running job is older than the rejected 1s value but younger than
	// the enforced 30m default, so it remains outside the age candidate set.
	t.Run("sub_floor_rejected_uses_default_window", func(t *testing.T) {
		ctx := context.Background()
		store, worker, now := seed(t)
		if err := runDaemonWorkerTickTracked(ctx, store, worker, 0, false, "owner/repo", "", io.Discard, now.Add(5*time.Minute), nil, nil); err != nil {
			t.Fatalf("recovery tick returned error: %v", err)
		}
		job, err := store.GetJob(ctx, "job-x")
		if err != nil {
			t.Fatalf("GetJob returned error: %v", err)
		}
		if job.State != string(workflow.JobRunning) {
			t.Fatalf("job state at +5m = %q, want running; the sub-floor 1s window was honored (should fall back to the 30m default)", job.State)
		}
	})

	// Past the enforced 30m default, age alone is still insufficient: the new
	// same-boot sweep requires frozen transcript bytes and process evidence too.
	t.Run("default_window_is_not_an_age_only_killer", func(t *testing.T) {
		ctx := context.Background()
		store, worker, now := seed(t)
		if err := runDaemonWorkerTickTracked(ctx, store, worker, 0, false, "owner/repo", "", io.Discard, now.Add(31*time.Minute), nil, nil); err != nil {
			t.Fatalf("recovery tick returned error: %v", err)
		}
		job, err := store.GetJob(ctx, "job-x")
		if err != nil {
			t.Fatalf("GetJob returned error: %v", err)
		}
		if job.State != string(workflow.JobRunning) {
			t.Fatalf("job state at +31m = %q, want running without transcript and process evidence", job.State)
		}
	})
}
