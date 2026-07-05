package db

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestExternallyDrivenDefaultsFalse proves the #657 column is additive: a job
// created via the normal CreateJob path (which never sets externally_driven) reads
// back false through both GetJob and ListJobs, so every engine-driven job is
// byte-identical.
func TestExternallyDrivenDefaultsFalse(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	if err := store.CreateJob(ctx, Job{ID: "j1", Agent: "a", Type: "ask", State: "queued"}); err != nil {
		t.Fatalf("CreateJob returned error: %v", err)
	}
	got, err := store.GetJob(ctx, "j1")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if got.ExternallyDriven {
		t.Fatalf("GetJob ExternallyDriven = true, want false")
	}
	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	if len(jobs) != 1 || jobs[0].ExternallyDriven {
		t.Fatalf("ListJobs = %+v, want one job with ExternallyDriven=false", jobs)
	}
}

// TestCreateExternallyDrivenJobWithEvent proves the session insert path stamps the
// flag and creates the job directly running with its clock-in event.
func TestCreateExternallyDrivenJobWithEvent(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	if err := store.CreateExternallyDrivenJobWithEvent(ctx, Job{
		ID:    "s1",
		Agent: "lead",
		Type:  "ask",
		State: "running",
	}, JobEvent{Kind: "running", Message: "job started"}); err != nil {
		t.Fatalf("CreateExternallyDrivenJobWithEvent returned error: %v", err)
	}
	got, err := store.GetJob(ctx, "s1")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if !got.ExternallyDriven || got.State != "running" {
		t.Fatalf("job = %+v, want running externally_driven", got)
	}
	evs, err := store.ListJobEvents(ctx, "s1")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if len(evs) != 1 || evs[0].Kind != "running" {
		t.Fatalf("events = %+v, want a single running event", evs)
	}
}

// TestListRunningJobsUpdatedBeforeSkipsExternallyDriven is the highest-risk
// correctness point (#657): the stuck-running reaper MUST NOT reclaim a session job
// even when it is stale, because the calling session may hold it open for minutes.
func TestListRunningJobsUpdatedBeforeSkipsExternallyDriven(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	// A normal running job (engine-driven) and a session running job, both stale.
	if err := store.CreateJob(ctx, Job{ID: "engine-run", Agent: "a", Type: "implement", State: "running"}); err != nil {
		t.Fatalf("CreateJob returned error: %v", err)
	}
	if err := store.CreateExternallyDrivenJobWithEvent(ctx, Job{ID: "session-run", Agent: "lead", Type: "ask", State: "running"}, JobEvent{Kind: "running", Message: "job started"}); err != nil {
		t.Fatalf("CreateExternallyDrivenJobWithEvent returned error: %v", err)
	}
	// Backdate both so they predate the reaper threshold.
	if _, err := store.db.ExecContext(ctx, `UPDATE jobs SET updated_at = '2026-01-01 00:00:00'`); err != nil {
		t.Fatalf("backdate returned error: %v", err)
	}

	before := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	got, err := store.ListRunningJobsUpdatedBefore(ctx, before)
	if err != nil {
		t.Fatalf("ListRunningJobsUpdatedBefore returned error: %v", err)
	}
	if len(got) != 1 || got[0].ID != "engine-run" {
		t.Fatalf("reaper candidates = %+v, want only engine-run (session job must be exempt)", got)
	}
}

// TestExternallyDrivenColumnMigratesOnPreExistingDB proves the migration is correct
// on a pre-existing DB: a job written on a first Open reads externally_driven=0 and
// a re-open (idempotent re-migration) neither errors nor changes it.
func TestExternallyDrivenColumnMigratesOnPreExistingDB(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gitmoot.db")

	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	if err := store.CreateJob(ctx, Job{ID: "old", Agent: "a", Type: "ask", State: "queued"}); err != nil {
		t.Fatalf("CreateJob returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("re-Open returned error: %v", err)
	}
	defer reopened.Close()
	got, err := reopened.GetJob(ctx, "old")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if got.ExternallyDriven {
		t.Fatalf("pre-existing row ExternallyDriven = true, want false (default 0)")
	}
}
