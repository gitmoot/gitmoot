package cli

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestRecoverForeignBootRunnersContinuesPastVanishedCandidate(t *testing.T) {
	if db.BootID() == "" {
		t.Skip("kernel boot id unavailable on this platform")
	}
	ctx := context.Background()
	store := daemonWorkerStore(t)
	for _, id := range []string{"job-a-vanished", "job-b-survivor"} {
		enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: id, Agent: "audit", Action: "ask", Repo: "owner/repo"})
		if claimed, err := store.ClaimRunningJob(ctx, id, string(workflow.JobQueued), string(workflow.JobRunning), db.JobEvent{Kind: string(workflow.JobRunning)}, 1, "prior-boot"); err != nil || !claimed {
			t.Fatalf("ClaimRunningJob(%s) claimed=%v err=%v", id, claimed, err)
		}
	}

	getJob := func(ctx context.Context, id string) (db.Job, error) {
		if id == "job-a-vanished" {
			return db.Job{}, sql.ErrNoRows
		}
		return store.GetJob(ctx, id)
	}

	var output bytes.Buffer
	if err := recoverForeignBootRunnersWithGet(ctx, store, &output, getJob); err != nil {
		t.Fatalf("recoverForeignBootRunners returned error: %v", err)
	}
	vanished, _ := store.GetJob(ctx, "job-a-vanished")
	survivor, _ := store.GetJob(ctx, "job-b-survivor")
	if vanished.State != string(workflow.JobRunning) || survivor.State != string(workflow.JobFailed) {
		t.Fatalf("states after per-item lookup race = vanished:%q survivor:%q, want running/failed", vanished.State, survivor.State)
	}
	if !strings.Contains(output.String(), "skipping candidate job-a-vanished") || !strings.Contains(output.String(), "sql: no rows") {
		t.Fatalf("recovery output = %q, want recorded vanished-candidate error", output.String())
	}
}

// TestRecoverForeignBootRunnersFailsRebootedJobRegardlessOfLease is the AC2
// fix: after a reboot a running job whose runtime-session lease is still in the
// future — the case #536's lease gate deliberately leaves "held" — must be
// failed immediately with evidence, and its stranded runtime lock reclaimed,
// because the boot id proves the in-process worker died with the old boot.
func TestRecoverForeignBootRunnersFailsRebootedJobRegardlessOfLease(t *testing.T) {
	if db.BootID() == "" {
		t.Skip("kernel boot id unavailable on this platform")
	}
	ctx := context.Background()
	store := daemonWorkerStore(t)
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-reboot", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1, JobTimeout: "4h"})

	priorBoot := "prior-boot-651"
	if claimed, err := store.ClaimRunningJob(ctx, "job-reboot", string(workflow.JobQueued), string(workflow.JobRunning), db.JobEvent{Kind: string(workflow.JobRunning)}, 4321, priorBoot); err != nil || !claimed {
		t.Fatalf("ClaimRunningJob claimed=%v err=%v", claimed, err)
	}
	now := time.Now().UTC()
	// Unexpired lease (future) AND a live-looking owner pid: the lease/PID gate
	// would keep this "held" forever — only the boot signal recovers it.
	if acquired, err := store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey:   "runtime:codex:reboot-session",
		OwnerJobID:    "job-reboot",
		OwnerToken:    "tok-reboot",
		OwnerPID:      int64(os.Getpid()),
		OwnerHostname: thisHostname(t),
		OwnerBootID:   priorBoot,
		ExpiresAt:     now.Add(4 * time.Hour).Format(time.RFC3339Nano),
	}, now); err != nil || !acquired {
		t.Fatalf("AcquireResourceLock acquired=%v err=%v", acquired, err)
	}

	if err := recoverForeignBootRunners(ctx, store, io.Discard); err != nil {
		t.Fatalf("recoverForeignBootRunners returned error: %v", err)
	}

	job, err := store.GetJob(ctx, "job-reboot")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobFailed) {
		t.Fatalf("job state = %q, want failed (rebooted job must settle regardless of lease)", job.State)
	}
	events, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	last := events[len(events)-1]
	if last.Kind != jobRecoveryFailedEvent || !strings.Contains(last.Message, "previous host boot") {
		t.Fatalf("recovery event = %+v, want loud previous-boot failure", last)
	}
	if _, err := store.GetResourceLock(ctx, "runtime:codex:reboot-session"); err == nil {
		t.Fatal("prior-boot runtime lock was not reclaimed")
	}
}

func TestDaemonRecoveryNeverTouchesSessionRecordedJobs(t *testing.T) {
	if db.BootID() == "" {
		t.Skip("kernel boot id unavailable on this platform")
	}
	ctx := context.Background()
	store := daemonWorkerStore(t)
	id := "session-review-outside-daemon"
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: id, Agent: "audit", Action: "review", Repo: "owner/repo"})
	if claimed, err := store.ClaimRunningJob(ctx, id, string(workflow.JobQueued), string(workflow.JobRunning), db.JobEvent{Kind: string(workflow.JobRunning)}, 1, "prior-boot"); err != nil || !claimed {
		t.Fatalf("ClaimRunningJob claimed=%v err=%v", claimed, err)
	}
	now := time.Now().UTC()
	backdateJobUpdatedAt(t, store.DatabasePath(), id, now.Add(-4*time.Hour))
	if acquired, err := store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey: "runtime:codex:session-recorded", OwnerJobID: id, OwnerToken: "tok", OwnerBootID: "prior-boot",
		ExpiresAt: now.Add(-time.Hour).Format(time.RFC3339Nano),
	}, now.Add(-2*time.Hour)); err != nil || !acquired {
		t.Fatalf("AcquireResourceLock acquired=%v err=%v", acquired, err)
	}
	if err := store.AddJobEvent(ctx, db.JobEvent{JobID: id, Kind: "job_kill_pending", Message: "deadline witness"}); err != nil {
		t.Fatal(err)
	}
	if err := recoverForeignBootRunners(ctx, store, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := recoverExpiredRuntimeSessionLocks(ctx, store, io.Discard, now); err != nil {
		t.Fatal(err)
	}
	if err := recoverRunningJobsBeforeForRepo(ctx, store, io.Discard, now, now.Add(time.Second), "", ""); err != nil {
		t.Fatal(err)
	}
	if err := recoverKillPendingJobs(ctx, store, io.Discard); err != nil {
		t.Fatal(err)
	}
	job, err := store.GetJob(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != string(workflow.JobRunning) {
		t.Fatalf("session-recorded job state = %q, want running", job.State)
	}
	if _, err := store.GetResourceLock(ctx, "runtime:codex:session-recorded"); err != nil {
		t.Fatalf("session-recorded job lock was touched: %v", err)
	}
	if transitioned, err := store.TransitionJobStateWithEvent(ctx, id, string(workflow.JobRunning), string(workflow.JobCancelled), db.JobEvent{Kind: string(workflow.JobCancelled)}); err != nil || !transitioned {
		t.Fatalf("terminal session transition=%v err=%v", transitioned, err)
	}
	if err := recoverExpiredRuntimeSessionLocks(ctx, store, io.Discard, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetResourceLock(ctx, "runtime:codex:session-recorded"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("terminal session lock error = %v, want reclaimed", err)
	}
}

// TestRecoverForeignBootRunnersProtectsSameBootJob is the #536 regression guard:
// a job claimed on the CURRENT boot with an unexpired lease must NOT be touched by
// the cross-boot pass — nor by the lease-gated coarse recovery — exactly as today.
func TestRecoverForeignBootRunnersProtectsSameBootJob(t *testing.T) {
	if db.BootID() == "" {
		t.Skip("kernel boot id unavailable on this platform")
	}
	ctx := context.Background()
	store := daemonWorkerStore(t)
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-live", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1, JobTimeout: "4h"})

	if claimed, err := store.ClaimRunningJob(ctx, "job-live", string(workflow.JobQueued), string(workflow.JobRunning), db.JobEvent{Kind: string(workflow.JobRunning)}, os.Getpid(), db.BootID()); err != nil || !claimed {
		t.Fatalf("ClaimRunningJob claimed=%v err=%v", claimed, err)
	}
	now := time.Now().UTC()
	if acquired, err := store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey:   "runtime:codex:live-session",
		OwnerJobID:    "job-live",
		OwnerToken:    "tok-live",
		OwnerPID:      int64(os.Getpid()),
		OwnerHostname: thisHostname(t),
		OwnerBootID:   db.BootID(),
		ExpiresAt:     now.Add(4 * time.Hour).Format(time.RFC3339Nano),
	}, now); err != nil || !acquired {
		t.Fatalf("AcquireResourceLock acquired=%v err=%v", acquired, err)
	}

	if err := recoverForeignBootRunners(ctx, store, io.Discard); err != nil {
		t.Fatalf("recoverForeignBootRunners returned error: %v", err)
	}
	// Also drive the coarse lease-gated recovery well past the staleness threshold:
	// the unexpired lease must still protect the live same-boot job.
	if err := recoverRunningJobsBeforeForRepo(ctx, store, io.Discard, now, now.Add(time.Hour), "", ""); err != nil {
		t.Fatalf("recoverRunningJobsBeforeForRepo returned error: %v", err)
	}

	job, err := store.GetJob(ctx, "job-live")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobRunning) {
		t.Fatalf("job state = %q, want running (same-boot unexpired-lease job must stay protected)", job.State)
	}
	if _, err := store.GetResourceLock(ctx, "runtime:codex:live-session"); err != nil {
		t.Fatalf("same-boot runtime lock was wrongly reclaimed: %v", err)
	}
}
