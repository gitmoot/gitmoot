package db

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestLatestAdvancementMarker(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	if err := store.CreateJob(ctx, Job{ID: "j1", Agent: "w", Type: "implement", State: "succeeded", Payload: "{}"}); err != nil {
		t.Fatal(err)
	}
	if k, err := store.LatestAdvancementMarker(ctx, "j1"); err != nil || k != "" {
		t.Fatalf("no markers yet = %q, %v; want empty", k, err)
	}
	// A non-advancement event (queued) must be ignored; advance_retry is the latest
	// advancement marker.
	for _, kind := range []string{"advance_started", "queued", "advance_retry"} {
		if err := store.AddJobEvent(ctx, JobEvent{JobID: "j1", Kind: kind, Message: "m"}); err != nil {
			t.Fatal(err)
		}
	}
	if k, err := store.LatestAdvancementMarker(ctx, "j1"); err != nil || k != "advance_retry" {
		t.Fatalf("latest = %q, %v; want advance_retry", k, err)
	}
	// A subsequent terminal resolution wins over the earlier advance_retry.
	if err := store.AddJobEvent(ctx, JobEvent{JobID: "j1", Kind: "advance_retried", Message: "done"}); err != nil {
		t.Fatal(err)
	}
	if k, err := store.LatestAdvancementMarker(ctx, "j1"); err != nil || k != "advance_retried" {
		t.Fatalf("latest after resolve = %q, %v; want advance_retried", k, err)
	}
}

// TestAdvanceRetryCollapseMigration proves the one-time heal migration collapses a
// job's runaway advance_retry history (the pre-fix per-tick emission bug) down to a
// single row while leaving the job a valid pending-advance-retry candidate.
func TestAdvanceRetryCollapseMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gitmoot.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.CreateJob(ctx, Job{ID: "stuck", Agent: "w", Type: "implement", State: "succeeded", Payload: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := store.AddJobEvent(ctx, JobEvent{JobID: "stuck", Kind: "advance_started", Message: "s"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		if err := store.AddJobEvent(ctx, JobEvent{JobID: "stuck", Kind: "advance_retry", Message: "fail"}); err != nil {
			t.Fatal(err)
		}
	}
	// Rewind the collapse migration by identity and re-open so it re-applies over
	// the seeded duplicates.
	collapseVersion := 0
	for i, m := range migrations {
		if strings.Contains(m, "DELETE FROM job_events") && strings.Contains(m, "advance_retry") {
			collapseVersion = i + 1
		}
	}
	if collapseVersion == 0 {
		t.Fatal("advance_retry collapse migration not found")
	}
	if _, err := store.db.Exec(`DELETE FROM schema_migrations WHERE version = ?`, collapseVersion); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store2, err := Open(path)
	if err != nil {
		t.Fatalf("re-open (re-apply collapse migration): %v", err)
	}
	defer store2.Close()

	events, err := store2.ListJobEvents(ctx, "stuck")
	if err != nil {
		t.Fatal(err)
	}
	retries := 0
	for _, e := range events {
		if e.Kind == "advance_retry" {
			retries++
		}
	}
	if retries != 1 {
		t.Fatalf("advance_retry rows after collapse = %d, want 1", retries)
	}
	// Candidate semantics unchanged: the job is still surfaced as pending advance
	// retry (last-one-wins over the surviving markers).
	ids, err := store2.JobIDsWithPendingAdvanceRetry(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "stuck" {
		t.Fatalf("pending advance-retry candidates = %v, want [stuck]", ids)
	}
}

func TestRefreshLatestAdvanceRetryKeepsWhyStuckCurrent(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	if err := store.CreateJob(ctx, Job{ID: "j-stuck", Agent: "w", Type: "implement", State: "succeeded", Payload: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := store.AddJobEvent(ctx, JobEvent{JobID: "j-stuck", Kind: "advance_retry", Message: "github unavailable"}); err != nil {
		t.Fatal(err)
	}
	updated, err := store.RefreshLatestAdvanceRetry(ctx, "j-stuck", "merge conflict")
	if err != nil || !updated {
		t.Fatalf("refresh with new message = %v, %v; want updated", updated, err)
	}
	// Unchanged message must cost zero writes.
	updated, err = store.RefreshLatestAdvanceRetry(ctx, "j-stuck", "merge conflict")
	if err != nil || updated {
		t.Fatalf("refresh with same message = %v, %v; want no-op", updated, err)
	}
	// No advance_retry row at all: no-op, no error.
	if err := store.CreateJob(ctx, Job{ID: "j-clean", Agent: "w", Type: "implement", State: "succeeded", Payload: "{}"}); err != nil {
		t.Fatal(err)
	}
	if updated, err = store.RefreshLatestAdvanceRetry(ctx, "j-clean", "anything"); err != nil || updated {
		t.Fatalf("refresh with no row = %v, %v; want no-op", updated, err)
	}
	events, err := store.ListJobEvents(ctx, "j-stuck")
	if err != nil {
		t.Fatal(err)
	}
	retries := 0
	last := ""
	for _, ev := range events {
		if ev.Kind == "advance_retry" {
			retries++
			last = ev.Message
		}
	}
	if retries != 1 || last != "merge conflict" {
		t.Fatalf("rows=%d last=%q; want exactly one row carrying the latest message", retries, last)
	}
}
