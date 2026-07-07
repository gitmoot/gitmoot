package db

import (
	"context"
	"path/filepath"
	"testing"
)

func openResultCheckStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestRecordAndListResultCheckFailures(t *testing.T) {
	ctx := context.Background()
	store := openResultCheckStore(t)

	checks := []ResultCheckFailure{
		{CheckID: "implement-changes-listed", Question: "made?", Explanation: "changes_made empty"},
		{CheckID: "implement-tests-listed", Question: "tested?", Explanation: "tests_run empty"},
	}
	if err := store.RecordResultCheckFailures(ctx, "job-1", "root-1", "implement", checks); err != nil {
		t.Fatalf("RecordResultCheckFailures: %v", err)
	}

	rows, err := store.ListResultCheckFailures(ctx, "job-1")
	if err != nil {
		t.Fatalf("ListResultCheckFailures: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want 2", rows)
	}
	if rows[0].CheckID != "implement-changes-listed" || rows[1].CheckID != "implement-tests-listed" {
		t.Fatalf("rows out of insertion order: %+v", rows)
	}
	if rows[0].JobID != "job-1" || rows[0].RootID != "root-1" || rows[0].Action != "implement" {
		t.Fatalf("row context wrong: %+v", rows[0])
	}
	if rows[0].CreatedAt == "" {
		t.Fatalf("row created_at should be populated: %+v", rows[0])
	}

	// A different job has no rows.
	other, err := store.ListResultCheckFailures(ctx, "job-2")
	if err != nil {
		t.Fatalf("ListResultCheckFailures(job-2): %v", err)
	}
	if len(other) != 0 {
		t.Fatalf("unrelated job rows = %+v, want none", other)
	}
}

func TestRecordResultCheckFailuresEmptyIsNoop(t *testing.T) {
	ctx := context.Background()
	store := openResultCheckStore(t)
	if err := store.RecordResultCheckFailures(ctx, "job-1", "root-1", "ask", nil); err != nil {
		t.Fatalf("RecordResultCheckFailures(empty): %v", err)
	}
	rows, err := store.ListResultCheckFailures(ctx, "job-1")
	if err != nil {
		t.Fatalf("ListResultCheckFailures: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("empty record must write nothing, got %+v", rows)
	}
}
