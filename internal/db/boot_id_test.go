package db

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBootIDStableAndCached(t *testing.T) {
	first := BootID()
	second := BootID()
	if first != second {
		t.Fatalf("BootID not stable: %q != %q", first, second)
	}
	// On a host that exposes the kernel boot id (Linux) it must be non-empty; on any
	// other platform the file is absent and "" is the documented sentinel.
	if _, err := os.Stat(bootIDPath); err == nil {
		if first == "" {
			t.Fatalf("BootID = %q, want non-empty on a host exposing %s", first, bootIDPath)
		}
		if got := readBootID(); got != first {
			t.Fatalf("readBootID = %q, want %q", got, first)
		}
	}
}

func openBootTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestMigrationAddsRunnerAndOwnerBootColumns(t *testing.T) {
	ctx := context.Background()
	store := openBootTestStore(t)
	// A targeted projection over the new columns errors if the ALTER TABLEs did not
	// run — a direct assertion the additive migration landed.
	if _, err := store.db.ExecContext(ctx, `SELECT runner_pid, runner_boot_id FROM jobs LIMIT 0`); err != nil {
		t.Fatalf("jobs missing runner_pid/runner_boot_id: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `SELECT owner_boot_id FROM resource_locks LIMIT 0`); err != nil {
		t.Fatalf("resource_locks missing owner_boot_id: %v", err)
	}
	// Migrate is idempotent (re-running never re-applies an appended migration).
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate returned error: %v", err)
	}
}

func seedBootQueuedJob(t *testing.T, store *Store, id string) {
	t.Helper()
	if err := store.CreateJobWithEvent(context.Background(), Job{
		ID:      id,
		Agent:   "audit",
		Type:    "ask",
		State:   "queued",
		Payload: `{"repo":"owner/repo"}`,
	}, JobEvent{JobID: id, Kind: "queued", Message: "job queued"}); err != nil {
		t.Fatalf("CreateJobWithEvent(%s) returned error: %v", id, err)
	}
}

func jobRunnerIdentity(t *testing.T, store *Store, id string) (int, string) {
	t.Helper()
	var pid int
	var boot string
	if err := store.db.QueryRowContext(context.Background(), `SELECT runner_pid, runner_boot_id FROM jobs WHERE id = ?`, id).Scan(&pid, &boot); err != nil {
		t.Fatalf("read runner identity for %s: %v", id, err)
	}
	return pid, boot
}

func TestClaimRunningJobStampsRunnerIdentity(t *testing.T) {
	ctx := context.Background()
	store := openBootTestStore(t)
	seedBootQueuedJob(t, store, "job-claim")

	claimed, err := store.ClaimRunningJob(ctx, "job-claim", "queued", "running", JobEvent{Kind: "running", Message: "job started"}, 4321, "boot-A")
	if err != nil {
		t.Fatalf("ClaimRunningJob returned error: %v", err)
	}
	if !claimed {
		t.Fatal("ClaimRunningJob did not claim a queued job")
	}
	job, err := store.GetJob(ctx, "job-claim")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != "running" {
		t.Fatalf("job state = %q, want running", job.State)
	}
	pid, boot := jobRunnerIdentity(t, store, "job-claim")
	if pid != 4321 || boot != "boot-A" {
		t.Fatalf("runner identity = (%d, %q), want (4321, boot-A)", pid, boot)
	}
	events, err := store.ListJobEvents(ctx, "job-claim")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	var sawRunning bool
	for _, e := range events {
		if e.Kind == "running" {
			sawRunning = true
		}
	}
	if !sawRunning {
		t.Fatalf("events = %+v, want a running claim event", events)
	}

	// A second claim on the now-running job transitions nothing and writes no event.
	claimed, err = store.ClaimRunningJob(ctx, "job-claim", "queued", "running", JobEvent{Kind: "running", Message: "job started"}, 9999, "boot-B")
	if err != nil {
		t.Fatalf("second ClaimRunningJob returned error: %v", err)
	}
	if claimed {
		t.Fatal("second ClaimRunningJob claimed an already-running job")
	}
	if pid, boot := jobRunnerIdentity(t, store, "job-claim"); pid != 4321 || boot != "boot-A" {
		t.Fatalf("runner identity after no-op claim = (%d, %q), want (4321, boot-A)", pid, boot)
	}
}

func TestRequeueRunningJobsFromForeignBoot(t *testing.T) {
	ctx := context.Background()
	store := openBootTestStore(t)
	// Three running jobs: one claimed on a foreign boot, one on the current boot,
	// one identity-less (legacy '' boot).
	for _, tc := range []struct {
		id   string
		boot string
	}{
		{"job-foreign", "old-boot"},
		{"job-current", "cur-boot"},
		{"job-legacy", ""},
	} {
		seedBootQueuedJob(t, store, tc.id)
		if claimed, err := store.ClaimRunningJob(ctx, tc.id, "queued", "running", JobEvent{Kind: "running"}, 1234, tc.boot); err != nil || !claimed {
			t.Fatalf("ClaimRunningJob(%s) claimed=%v err=%v", tc.id, claimed, err)
		}
	}

	requeued, err := store.RequeueRunningJobsFromForeignBoot(ctx, "cur-boot")
	if err != nil {
		t.Fatalf("RequeueRunningJobsFromForeignBoot returned error: %v", err)
	}
	if len(requeued) != 1 || requeued[0] != "job-foreign" {
		t.Fatalf("requeued = %v, want [job-foreign]", requeued)
	}
	assertState := func(id, want string) {
		t.Helper()
		job, err := store.GetJob(ctx, id)
		if err != nil {
			t.Fatalf("GetJob(%s) returned error: %v", id, err)
		}
		if job.State != want {
			t.Fatalf("job %s state = %q, want %q", id, job.State, want)
		}
	}
	assertState("job-foreign", "queued")
	assertState("job-current", "running")
	assertState("job-legacy", "running")

	// The foreign-boot requeue must leave an audit event.
	events, err := store.ListJobEvents(ctx, "job-foreign")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	var sawQueued bool
	for _, e := range events {
		if e.Kind == "queued" && e.Message == "recovered running job claimed on a previous boot (host rebooted)" {
			sawQueued = true
		}
	}
	if !sawQueued {
		t.Fatalf("events = %+v, want a previous-boot recovery event", events)
	}

	// An empty current boot id (non-Linux / unavailable) is a STRICT no-op: no job
	// with a non-empty recorded boot is ever swept just because it differs from "".
	if requeued, err := store.RequeueRunningJobsFromForeignBoot(ctx, ""); err != nil {
		t.Fatalf("RequeueRunningJobsFromForeignBoot(\"\") returned error: %v", err)
	} else if len(requeued) != 0 {
		t.Fatalf("empty-boot requeue = %v, want none", requeued)
	}
	assertState("job-current", "running")
}

func TestReleaseRuntimeSessionLocksFromForeignBoot(t *testing.T) {
	ctx := context.Background()
	store := openBootTestStore(t)
	now := time.Now().UTC()
	future := now.Add(4 * time.Hour).Format(time.RFC3339Nano)

	locks := []ResourceLock{
		// Foreign-boot runtime lock with an UNEXPIRED lease: must be reclaimed
		// regardless of the lease (the #651 cross-boot gap).
		{ResourceKey: "runtime:codex:s-old", OwnerJobID: "job-old", OwnerToken: "t1", OwnerBootID: "old-boot", ExpiresAt: future},
		// Current-boot runtime lock: must be kept.
		{ResourceKey: "runtime:codex:s-cur", OwnerJobID: "job-cur", OwnerToken: "t2", OwnerBootID: "cur-boot", ExpiresAt: future},
		// Identity-less runtime lock (legacy / non-Linux acquire): must be kept.
		{ResourceKey: "runtime:codex:s-legacy", OwnerJobID: "job-leg", OwnerToken: "t3", OwnerBootID: "", ExpiresAt: future},
		// Foreign-boot but NOT a runtime lock: must be kept (only runtime:% locks
		// are reclaimed cross-boot by the daemon).
		{ResourceKey: "skillopt-train-generation:s:i", OwnerJobID: "job-gen", OwnerToken: "t4", OwnerBootID: "old-boot", ExpiresAt: future},
	}
	for _, lock := range locks {
		if acquired, err := store.AcquireResourceLock(ctx, lock, now); err != nil || !acquired {
			t.Fatalf("AcquireResourceLock(%s) acquired=%v err=%v", lock.ResourceKey, acquired, err)
		}
	}

	released, err := store.ReleaseRuntimeSessionLocksFromForeignBoot(ctx, "cur-boot")
	if err != nil {
		t.Fatalf("ReleaseRuntimeSessionLocksFromForeignBoot returned error: %v", err)
	}
	if released != 1 {
		t.Fatalf("released = %d, want 1", released)
	}
	present := func(key string) bool {
		t.Helper()
		if _, err := store.GetResourceLock(ctx, key); err == nil {
			return true
		}
		return false
	}
	if present("runtime:codex:s-old") {
		t.Fatal("foreign-boot runtime lock was not reclaimed")
	}
	for _, key := range []string{"runtime:codex:s-cur", "runtime:codex:s-legacy", "skillopt-train-generation:s:i"} {
		if !present(key) {
			t.Fatalf("lock %s was wrongly reclaimed", key)
		}
	}

	// Empty current boot id is a STRICT no-op.
	if released, err := store.ReleaseRuntimeSessionLocksFromForeignBoot(ctx, ""); err != nil {
		t.Fatalf("ReleaseRuntimeSessionLocksFromForeignBoot(\"\") returned error: %v", err)
	} else if released != 0 {
		t.Fatalf("empty-boot release = %d, want 0", released)
	}
}

func TestAcquireResourceLockPersistsOwnerBootID(t *testing.T) {
	ctx := context.Background()
	store := openBootTestStore(t)
	now := time.Now().UTC()
	future := now.Add(time.Hour).Format(time.RFC3339Nano)

	if acquired, err := store.AcquireResourceLock(ctx, ResourceLock{
		ResourceKey: "runtime:codex:s1", OwnerJobID: "j1", OwnerToken: "tok", OwnerBootID: "boot-x", ExpiresAt: future,
	}, now); err != nil || !acquired {
		t.Fatalf("AcquireResourceLock acquired=%v err=%v", acquired, err)
	}
	boot, err := store.ResourceLockOwnerBootID(ctx, "runtime:codex:s1")
	if err != nil {
		t.Fatalf("ResourceLockOwnerBootID returned error: %v", err)
	}
	if boot != "boot-x" {
		t.Fatalf("owner boot id = %q, want boot-x", boot)
	}

	// A lock with no recorded boot reads "".
	if acquired, err := store.AcquireResourceLock(ctx, ResourceLock{
		ResourceKey: "runtime:codex:s2", OwnerJobID: "j2", OwnerToken: "tok2", ExpiresAt: future,
	}, now); err != nil || !acquired {
		t.Fatalf("AcquireResourceLock(no boot) acquired=%v err=%v", acquired, err)
	}
	if boot, err := store.ResourceLockOwnerBootID(ctx, "runtime:codex:s2"); err != nil || boot != "" {
		t.Fatalf("ResourceLockOwnerBootID(no boot) = (%q, %v), want (\"\", nil)", boot, err)
	}

	// An absent lock reads "" with no error.
	if boot, err := store.ResourceLockOwnerBootID(ctx, "runtime:codex:absent"); err != nil || boot != "" {
		t.Fatalf("ResourceLockOwnerBootID(absent) = (%q, %v), want (\"\", nil)", boot, err)
	}
}
