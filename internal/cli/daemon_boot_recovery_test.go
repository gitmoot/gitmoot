package cli

import (
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// TestRecoverForeignBootRunnersRequeuesRebootedJobRegardlessOfLease is the AC2
// fix: after a reboot a running job whose runtime-session lease is still in the
// future — the case #536's lease gate deliberately leaves "held" — must be
// requeued immediately, and its stranded runtime lock reclaimed, because the boot
// id proves the in-process worker died with the old boot.
func TestRecoverForeignBootRunnersRequeuesRebootedJobRegardlessOfLease(t *testing.T) {
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
	if job.State != string(workflow.JobQueued) {
		t.Fatalf("job state = %q, want queued (rebooted job must recover regardless of lease)", job.State)
	}
	if _, err := store.GetResourceLock(ctx, "runtime:codex:reboot-session"); err == nil {
		t.Fatal("prior-boot runtime lock was not reclaimed")
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
	if err := recoverRunningJobsBefore(ctx, store, io.Discard, now.Add(time.Hour)); err != nil {
		t.Fatalf("recoverRunningJobsBefore returned error: %v", err)
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
