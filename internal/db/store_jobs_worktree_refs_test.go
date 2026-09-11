package db

import (
	"context"
	"testing"
)

// TestListJobWorktreeRefsReturnsOnlyPathBearingRows covers #2149's query.
//
// The reclaim guard used to load every job and decode every payload to read
// one field. This returns the field itself, and only for rows that have one,
// so the caller has nothing to decode.
func TestListJobWorktreeRefsReturnsOnlyPathBearingRows(t *testing.T) {
	store := openStoreOperationsTestStore(t)
	ctx := context.Background()

	insert := func(id, state, payload string) {
		t.Helper()
		if _, err := store.db.ExecContext(ctx,
			`INSERT INTO jobs (id, agent, type, state, payload, created_at, updated_at)
			 VALUES (?,?,?,?,?,?,?)`,
			id, "a", "review", state, payload, "2026-09-01 00:00:00", "2026-09-02 00:00:00"); err != nil {
			t.Fatal(err)
		}
	}
	insert("has-path", "succeeded", `{"worktree_path":"/w/one","prompt":"x"}`)
	insert("no-path", "succeeded", `{"prompt":"x"}`)
	insert("empty-path", "succeeded", `{"worktree_path":"","prompt":"x"}`)
	// A payload that is not JSON must be TOLERATED, not rejected. The Go code
	// this replaces skipped such a row; an unguarded expression index would
	// have failed this INSERT instead, turning a data oddity into a write
	// outage. This insert is the regression test for that.
	insert("not-json", "succeeded", `this is not json at all`)

	refs, err := store.ListJobWorktreeRefs(ctx)
	if err != nil {
		t.Fatalf("ListJobWorktreeRefs: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("got %d refs, want 1: %+v", len(refs), refs)
	}
	got := refs[0]
	if got.ID != "has-path" || got.WorktreePath != "/w/one" {
		t.Errorf("ref = %+v, want has-path at /w/one", got)
	}
	if got.State != "succeeded" || got.UpdatedAt == "" || got.CreatedAt == "" {
		t.Errorf("ref = %+v, want the finality and age fields the guard needs", got)
	}
}

// TestListJobWorktreeRefsUsesTheCoveringIndex pins the property the fix is
// for, not merely the result. Without the index this query re-parses every
// payload in SQLite, which is the cost being removed; measured on live-shaped
// data that difference was 53ms against 8.5ms, and it grows with the table.
//
// Asserting the PLAN rather than a duration, because a timing assertion on a
// shared runner is a flake generator.
func TestListJobWorktreeRefsUsesTheCoveringIndex(t *testing.T) {
	store := openStoreOperationsTestStore(t)
	ctx := context.Background()

	var id, parent, notused int
	var plan string
	if err := store.db.QueryRowContext(ctx, "EXPLAIN QUERY PLAN "+listJobWorktreeRefsSQL).
		Scan(&id, &parent, &notused, &plan); err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	if plan == "" {
		t.Fatal("empty query plan")
	}
	const want = "COVERING INDEX idx_jobs_worktree_path"
	if !contains(plan, want) {
		t.Errorf("plan = %q, want it to use %q. Without the covering index every row's payload is parsed again, which is the cost #2149 removes.", plan, want)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle ||
		len(needle) == 0 || indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
