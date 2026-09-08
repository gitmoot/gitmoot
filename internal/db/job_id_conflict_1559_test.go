package db

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// #1559: a job insert that loses a primary-key race surfaced one journal line,
// `constraint failed: UNIQUE constraint failed: jobs.id (1555)`, naming no id,
// no agent and no table context beyond the column. 1555 is
// SQLITE_CONSTRAINT_PRIMARYKEY, so the row was silently not written.
//
// The observed instance cost nothing: all four dispatched jobs existed and no
// leg failed. That is the argument FOR naming it, not against. The same shape on
// a top-level review means the reviewer does not exist while its coordinator
// still records a verdict, which is #1557's hollow panel.
//
// I hit this shape myself while fixing #1522: the deterministic fix-leg id
// derives from task and review round, so two objections in one round mint the
// SAME id and the second advance dies here. That collision is currently the only
// thing preventing a duplicate writer on the branch, and attributing it took
// hours precisely because the error named no id.
func TestJobInsertConflictNamesTheIDItFailedToWrite(t *testing.T) {
	ctx := context.Background()
	store, err := openCachedTestStore(t, filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	job := Job{ID: "local-review-g7-review-18ccc773a42446e8", Agent: "g7-review", Type: "review", State: "queued", Payload: "{}"}
	if err := store.CreateJob(ctx, job); err != nil {
		t.Fatalf("first insert: %v", err)
	}

	err = store.CreateJob(ctx, job)
	if !errors.Is(err, ErrJobIDConflict) {
		t.Fatalf("second insert error = %v, want ErrJobIDConflict so a caller can tell a collision from any other failure", err)
	}
	if !strings.Contains(err.Error(), job.ID) {
		t.Fatalf("conflict error %q does not name the id it failed to write; diagnosing it needs an inventory of everything dispatched in that second", err)
	}

	// The transactional twin must behave identically, or the naming is installed
	// on one insert path and absent on the next, which is the shape this campaign
	// keeps finding.
	err = store.CreateJobWithEvent(ctx, job, JobEvent{Kind: "queued", Message: "queued"})
	if !errors.Is(err, ErrJobIDConflict) || !strings.Contains(err.Error(), job.ID) {
		t.Fatalf("CreateJobWithEvent conflict = %v, want a named ErrJobIDConflict", err)
	}

	// A non-conflict failure must NOT be relabelled: a caller that retries on
	// ErrJobIDConflict would otherwise retry a permanent error forever.
	if err := store.CreateJob(ctx, Job{ID: "", Agent: "a", Type: "review", State: "queued", Payload: "{}"}); errors.Is(err, ErrJobIDConflict) {
		t.Fatalf("a non-conflict insert failure was labelled a conflict: %v", err)
	}
}

// The runtime column must travel on the transactional insert too (#1534). It did
// not: AddJobEvent carried it and createJobWithEventTx dropped it, so a job
// created together with its runtime-selection event lost the structured value
// and fell back to the registry default. That is the misattribution #1534 exists
// to stop, reintroduced through the call site next door, and it is a gap in my
// own merged change.
func TestCreateJobWithEventCarriesTheStructuredRuntime(t *testing.T) {
	ctx := context.Background()
	store, err := openCachedTestStore(t, filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	job := Job{ID: "job-1", Agent: "lead", Type: "implement", State: "queued", Payload: "{}"}
	if err := store.CreateJobWithEvent(ctx, job,
		JobEvent{Kind: "runtime_override", Message: "job runs on runtime codex (agent default claude)", Runtime: "codex"},
		JobEvent{Kind: "queued", Message: "queued"},
	); err != nil {
		t.Fatalf("CreateJobWithEvent: %v", err)
	}

	recorded, err := store.JobRecordedRuntime(ctx, "job-1", "lead")
	if err != nil {
		t.Fatalf("JobRecordedRuntime: %v", err)
	}
	if recorded != "codex" {
		t.Fatalf("recorded runtime = %q, want %q; the structured value was dropped by the transactional insert", recorded, "codex")
	}

	// The sibling event carries no runtime and must not shadow the one that does.
	events, err := store.ListJobEvents(ctx, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}
}
