package cli

import (
	"context"
	"database/sql"
	"io"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestRuntimeSessionLockHeartbeatKeepsLiveJobSafe(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-live", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main"})
	if err := store.UpdateJobState(ctx, "job-live", string(workflow.JobRunning)); err != nil {
		t.Fatalf("UpdateJobState returned error: %v", err)
	}
	now := time.Now().UTC()
	// ttl is deliberately wide relative to the beat cadence: the assertion below
	// needs the LAST beat before stop() to still be within ttl of reapNow even on
	// a heavily loaded -race CI runner, so keep hundreds of ms of margin.
	ttl := 500 * time.Millisecond
	lockKey := "runtime:codex:session-live"
	acquired, err := store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey: lockKey,
		OwnerJobID:  "job-live",
		OwnerToken:  "token-live",
		ExpiresAt:   now.Add(ttl).Format(time.RFC3339Nano),
	}, now)
	if err != nil || !acquired {
		t.Fatalf("AcquireResourceLock returned acquired=%v err=%v", acquired, err)
	}

	stop := startRuntimeSessionLockHeartbeatWithCadence(ctx, store, lockKey, "token-live", ttl, 10*time.Millisecond, io.Discard)
	t.Cleanup(stop)
	time.Sleep(ttl + 10*time.Millisecond)
	stop()
	reapNow := time.Now().UTC()
	if !reapNow.After(now.Add(ttl)) {
		t.Fatalf("reap time %s did not pass original expiry %s", reapNow, now.Add(ttl))
	}
	if err := recoverExpiredRuntimeSessionLocks(ctx, store, io.Discard, reapNow); err != nil {
		t.Fatalf("recoverExpiredRuntimeSessionLocks returned error: %v", err)
	}
	if err := recoverRunningJobsBeforeForRepo(ctx, store, io.Discard, reapNow, reapNow.Add(time.Minute), "", ""); err != nil {
		t.Fatalf("recoverRunningJobsBeforeForRepo returned error: %v", err)
	}

	job, err := store.GetJob(ctx, "job-live")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobRunning) {
		t.Fatalf("job state = %q, want running", job.State)
	}
	if _, err := store.GetResourceLock(ctx, lockKey); err != nil {
		t.Fatalf("runtime lock missing after recovery passes: %v", err)
	}
}

func TestRuntimeSessionLockHeartbeatStoppedRunnerRecovers(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-dead", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main"})
	if err := store.UpdateJobState(ctx, "job-dead", string(workflow.JobRunning)); err != nil {
		t.Fatalf("UpdateJobState returned error: %v", err)
	}
	now := time.Now().UTC()
	ttl := time.Minute
	lockKey := "runtime:codex:session-dead"
	acquired, err := store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey: lockKey,
		OwnerJobID:  "job-dead",
		OwnerToken:  "token-dead",
		ExpiresAt:   now.Add(ttl).Format(time.RFC3339Nano),
	}, now)
	if err != nil || !acquired {
		t.Fatalf("AcquireResourceLock returned acquired=%v err=%v", acquired, err)
	}
	stop := startRuntimeSessionLockHeartbeatWithCadence(ctx, store, lockKey, "token-dead", ttl, time.Hour, io.Discard)
	stop()

	reapNow := now.Add(ttl + time.Second)
	if err := recoverExpiredRuntimeSessionLocks(ctx, store, io.Discard, reapNow); err != nil {
		t.Fatalf("recoverExpiredRuntimeSessionLocks returned error: %v", err)
	}
	if err := recoverRunningJobsBeforeForRepo(ctx, store, io.Discard, reapNow, reapNow.Add(time.Minute), "", ""); err != nil {
		t.Fatalf("recoverRunningJobsBeforeForRepo returned error: %v", err)
	}
	job, err := store.GetJob(ctx, "job-dead")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobQueued) {
		t.Fatalf("job state = %q, want queued", job.State)
	}
	if _, err := store.GetResourceLock(ctx, lockKey); err != sql.ErrNoRows {
		t.Fatalf("GetResourceLock after recovery error = %v, want sql.ErrNoRows", err)
	}
}

func TestRuntimeSessionLockHeartbeatOwnershipGuardStopsStaleOwner(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	now := time.Now().UTC()
	lockKey := "runtime:codex:session-reacquired"
	acquired, err := store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey: lockKey,
		OwnerJobID:  "job-old",
		OwnerToken:  "token-old",
		ExpiresAt:   now.Add(time.Minute).Format(time.RFC3339Nano),
	}, now)
	if err != nil || !acquired {
		t.Fatalf("old AcquireResourceLock returned acquired=%v err=%v", acquired, err)
	}
	stop := startRuntimeSessionLockHeartbeatWithCadence(ctx, store, lockKey, "token-old", time.Minute, time.Millisecond, io.Discard)
	t.Cleanup(stop)
	if released, err := store.ReleaseResourceLock(ctx, lockKey, "job-old", "token-old"); err != nil || !released {
		t.Fatalf("ReleaseResourceLock returned released=%v err=%v", released, err)
	}
	newExpiry := now.Add(2 * time.Minute)
	acquired, err = store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey: lockKey,
		OwnerJobID:  "job-new",
		OwnerToken:  "token-new",
		ExpiresAt:   newExpiry.Format(time.RFC3339Nano),
	}, now)
	if err != nil || !acquired {
		t.Fatalf("new AcquireResourceLock returned acquired=%v err=%v", acquired, err)
	}
	time.Sleep(10 * time.Millisecond)
	stop()
	lock, err := store.GetResourceLock(ctx, lockKey)
	if err != nil {
		t.Fatalf("GetResourceLock returned error: %v", err)
	}
	// Compare the reacquired lease by INSTANT, not by raw string: the store
	// persists times via a fixed-width layout (…".000000000Z", always 9
	// fractional digits) while time.RFC3339Nano trims trailing zeros, so a
	// direct string compare spuriously fails ~10% of the time — whenever the
	// captured `now` happens to have a trailing-zero nanosecond — even though the
	// stored expiry is the exact same instant. The real assertion is that a stale
	// token-old heartbeat did NOT change the reacquired token-new lease; the
	// explicit HeartbeatResourceLock check below covers the owner-token CAS.
	gotExpiry, err := time.Parse(time.RFC3339Nano, lock.ExpiresAt)
	if err != nil {
		t.Fatalf("parse reacquired lock expiry %q: %v", lock.ExpiresAt, err)
	}
	if lock.OwnerToken != "token-new" || !gotExpiry.Equal(newExpiry) {
		t.Fatalf("reacquired lock changed by stale heartbeat: %+v (want owner token-new, expiry %s)", lock, newExpiry.Format(time.RFC3339Nano))
	}
	updated, err := store.HeartbeatResourceLock(ctx, lockKey, "token-old", now.Add(3*time.Minute))
	if err != nil || updated {
		t.Fatalf("stale HeartbeatResourceLock returned updated=%v err=%v", updated, err)
	}
}

func TestRuntimeSessionLockHeartbeatLeavesShellRecoveryUnchanged(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-shell", Agent: "shell", Action: "ask", Repo: "owner/repo", Branch: "main"})
	if err := store.UpdateJobState(ctx, "job-shell", string(workflow.JobRunning)); err != nil {
		t.Fatalf("UpdateJobState returned error: %v", err)
	}
	stop := startRuntimeSessionLockHeartbeat(ctx, store, "", "", time.Minute)
	stop()
	now := time.Now().UTC()
	if err := recoverRunningJobsBeforeForRepo(ctx, store, io.Discard, now, now.Add(time.Minute), "", ""); err != nil {
		t.Fatalf("recoverRunningJobsBeforeForRepo returned error: %v", err)
	}
	job, err := store.GetJob(ctx, "job-shell")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobQueued) {
		t.Fatalf("job state = %q, want queued", job.State)
	}
}

func TestRuntimeSessionLockHeartbeatErrorsAreBestEffort(t *testing.T) {
	store := daemonWorkerStore(t)
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	stop := startRuntimeSessionLockHeartbeatWithCadence(context.Background(), store, "runtime:codex:closed", "token", time.Minute, time.Millisecond, io.Discard)
	time.Sleep(5 * time.Millisecond)
	stop()
}
