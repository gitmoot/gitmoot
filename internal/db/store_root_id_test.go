package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// rootIDOf reads the denormalized root_id column directly (GetJob/ListJobs do not
// project it), so tests can assert the index value independently of the readers.
func rootIDOf(t *testing.T, store *Store, id string) string {
	t.Helper()
	var rootID string
	if err := store.db.QueryRowContext(context.Background(), `SELECT root_id FROM jobs WHERE id = ?`, id).Scan(&rootID); err != nil {
		t.Fatalf("read root_id for %q returned error: %v", id, err)
	}
	return rootID
}

// TestCreateJobPopulatesRootID pins #420 at the insert path: a coordinator with no
// payload RootJobID self-roots to its own id, a child inherits the payload's
// root, and a malformed payload self-roots (it never errors the insert). This is
// load-bearing — an implementation that forgets to bind the empty-string check
// in COALESCE(NULLIF(...)) leaves root_id empty and fails here.
func TestCreateJobPopulatesRootID(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	// Coordinator: payload carries no root_job_id => self-roots.
	if err := store.CreateJob(ctx, Job{ID: "R", Agent: "coord", Type: "ask", State: "succeeded", Payload: `{"sender":"coord"}`}); err != nil {
		t.Fatalf("CreateJob(R) returned error: %v", err)
	}
	// Child: payload root_job_id points back at R.
	if err := store.CreateJobWithEvent(ctx, Job{ID: "R/c1", Agent: "w", Type: "ask", State: "succeeded", Payload: `{"root_job_id":"R"}`}, JobEvent{Kind: "succeeded", Message: "done"}); err != nil {
		t.Fatalf("CreateJobWithEvent(R/c1) returned error: %v", err)
	}
	// Empty-string root_job_id must NOT win: NULLIF('','') is NULL so it self-roots.
	if err := store.CreateJob(ctx, Job{ID: "R/c2", Agent: "w", Type: "ask", State: "succeeded", Payload: `{"root_job_id":""}`}); err != nil {
		t.Fatalf("CreateJob(R/c2) returned error: %v", err)
	}
	// Malformed payload must self-root and never error the insert.
	if err := store.CreateJob(ctx, Job{ID: "bad", Agent: "w", Type: "ask", State: "succeeded", Payload: `not json`}); err != nil {
		t.Fatalf("CreateJob(bad) with malformed payload returned error: %v", err)
	}

	for _, tc := range []struct{ id, want string }{
		{"R", "R"},
		{"R/c1", "R"},
		{"R/c2", "R/c2"},
		{"bad", "bad"},
	} {
		if got := rootIDOf(t, store, tc.id); got != tc.want {
			t.Fatalf("root_id(%q) = %q, want %q", tc.id, got, tc.want)
		}
	}
}

// TestListJobsByRootReturnsTree pins that ListJobsByRoot returns exactly the tree
// (self-rooted coordinator + every child) ordered by id, and excludes a sibling
// tree. Load-bearing: a naive ListJobsByRoot that filtered on parent_job_id or
// payload would not return this set.
func TestListJobsByRootReturnsTree(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	seed := func(id, payload string) {
		if err := store.CreateJob(ctx, Job{ID: id, Agent: "a", Type: "ask", State: "succeeded", Payload: payload}); err != nil {
			t.Fatalf("CreateJob(%q) returned error: %v", id, err)
		}
	}
	seed("R", `{"workflow_id":"wf-proof","repo":"owner/repo","pull_request":12,"result":{"decision":"approved"}}`)
	seed("R/a", `{"root_job_id":"R"}`)
	seed("R/b", `{"root_job_id":"R"}`)
	seed("S", `{}`) // unrelated root
	seed("S/a", `{"root_job_id":"S"}`)

	jobs, err := store.ListJobsByRoot(ctx, "R")
	if err != nil {
		t.Fatalf("ListJobsByRoot(R) returned error: %v", err)
	}
	gotIDs := make([]string, 0, len(jobs))
	for _, j := range jobs {
		gotIDs = append(gotIDs, j.ID)
		if j.RootID != "R" {
			t.Fatalf("ListJobsByRoot(R) returned job %q with RootID %q, want R", j.ID, j.RootID)
		}
		if j.ID == "R" && (j.WorkflowID != "wf-proof" || j.Repo != "owner/repo" || j.PullRequest != 12 || j.ResultHash == "" || j.CreatedAt == "") {
			t.Fatalf("ListJobsByRoot(R) proof projection incomplete: %+v", j)
		}
	}
	want := []string{"R", "R/a", "R/b"}
	if len(gotIDs) != len(want) {
		t.Fatalf("ListJobsByRoot(R) ids = %v, want %v", gotIDs, want)
	}
	for i := range want {
		if gotIDs[i] != want[i] {
			t.Fatalf("ListJobsByRoot(R) ids = %v, want %v (ORDER BY id)", gotIDs, want)
		}
	}
}

// TestCountAndSumJobsByRoot pins the pushed-into-SQL helpers: COUNT over the tree
// and SUM(input+output) over the tree, both keyed on the indexed root_id and both
// ignoring a sibling tree. The empty-tree SUM is COALESCEd to 0.
func TestCountAndSumJobsByRoot(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	mk := func(id, payload string, in, out int) {
		if err := store.CreateJob(ctx, Job{ID: id, Agent: "a", Type: "ask", State: "succeeded", Payload: payload}); err != nil {
			t.Fatalf("CreateJob(%q) returned error: %v", id, err)
		}
		if in != 0 || out != 0 {
			if err := store.UpdateJobUsage(ctx, id, in, out); err != nil {
				t.Fatalf("UpdateJobUsage(%q) returned error: %v", id, err)
			}
		}
	}
	mk("R", `{}`, 10, 5)
	mk("R/a", `{"root_job_id":"R"}`, 3, 7)
	mk("R/b", `{"root_job_id":"R"}`, 0, 0)
	mk("S", `{}`, 100, 100) // sibling, must not count

	count, err := store.CountJobsByRoot(ctx, "R")
	if err != nil {
		t.Fatalf("CountJobsByRoot(R) returned error: %v", err)
	}
	if count != 3 {
		t.Fatalf("CountJobsByRoot(R) = %d, want 3", count)
	}
	sum, err := store.SumJobTokensByRoot(ctx, "R")
	if err != nil {
		t.Fatalf("SumJobTokensByRoot(R) returned error: %v", err)
	}
	if want := 10 + 5 + 3 + 7; sum != want {
		t.Fatalf("SumJobTokensByRoot(R) = %d, want %d", sum, want)
	}
	// Empty tree (no rows) sums to 0, not an error.
	empty, err := store.SumJobTokensByRoot(ctx, "no-such-root")
	if err != nil {
		t.Fatalf("SumJobTokensByRoot(missing) returned error: %v", err)
	}
	if empty != 0 {
		t.Fatalf("SumJobTokensByRoot(missing) = %d, want 0", empty)
	}
}

// TestDelegateQueuedJobPreservesRootID pins #420's decision to leave root_id
// untouched on in-place re-delegation: a queued child re-assigned to a different
// agent stays in the same tree, so its root_id must not change.
func TestDelegateQueuedJobPreservesRootID(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	if err := store.CreateJob(ctx, Job{ID: "R/child", Agent: "from", Type: "ask", State: "queued", Payload: `{"root_job_id":"R"}`}); err != nil {
		t.Fatalf("CreateJob returned error: %v", err)
	}
	if rootID := rootIDOf(t, store, "R/child"); rootID != "R" {
		t.Fatalf("pre-delegation root_id = %q, want R", rootID)
	}
	ok, err := store.DelegateQueuedJob(ctx, "R/child", "from", "to", `{"root_job_id":"R","sender":"to"}`, JobEvent{Kind: "delegated", Message: "reassigned"})
	if err != nil {
		t.Fatalf("DelegateQueuedJob returned error: %v", err)
	}
	if !ok {
		t.Fatal("DelegateQueuedJob reported no rows affected")
	}
	if rootID := rootIDOf(t, store, "R/child"); rootID != "R" {
		t.Fatalf("post-delegation root_id = %q, want R (must be preserved)", rootID)
	}
}

// TestMigrateBackfillsRootID pins the forward-only backfill on a pre-#420 DB:
// apply every migration except the root_id one, insert legacy jobs (a self-root,
// a payload-rooted child, an empty-root child, and a malformed-payload row), then
// reopen and assert every row's root_id matches rootJobID()'s rule. The malformed
// row is the load-bearing case: an in-migration json_extract would abort here, so
// the Go-side backfill must self-root it instead.
func TestMigrateBackfillsRootID(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gitmoot.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	store := &Store{db: raw}
	// Apply every migration EXCEPT the last (the root_id ALTER+index), reproducing a
	// DB whose jobs table has no root_id column.
	for version, migration := range migrations[:len(migrations)-1] {
		if err := store.applyMigration(ctx, version+1, migration); err != nil {
			t.Fatalf("applyMigration(%d) returned error: %v", version+1, err)
		}
	}
	insert := func(id, payload string) {
		if _, err := raw.ExecContext(ctx, `INSERT INTO jobs(id, agent, type, state, payload) VALUES (?, 'a', 'ask', 'succeeded', ?)`, id, payload); err != nil {
			t.Fatalf("insert legacy %q returned error: %v", id, err)
		}
	}
	insert("R", `{"sender":"coord"}`)     // self-root (no root_job_id)
	insert("R/c1", `{"root_job_id":"R"}`) // child -> payload root
	insert("R/c2", `{"root_job_id":""}`)  // empty root -> self-root
	insert("bad", `definitely not json`)  // malformed -> self-root
	if err := raw.Close(); err != nil {
		t.Fatalf("raw Close returned error: %v", err)
	}

	upgraded, err := Open(path)
	if err != nil {
		t.Fatalf("Open upgraded DB returned error: %v", err)
	}
	defer upgraded.Close()

	for _, tc := range []struct{ id, want string }{
		{"R", "R"},
		{"R/c1", "R"},
		{"R/c2", "R/c2"},
		{"bad", "bad"},
	} {
		if got := rootIDOf(t, upgraded, tc.id); got != tc.want {
			t.Fatalf("backfilled root_id(%q) = %q, want %q", tc.id, got, tc.want)
		}
	}

	// The index must exist so the helpers hit it.
	var name string
	if err := upgraded.db.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type='index' AND name='idx_jobs_root_id'`).Scan(&name); err != nil {
		t.Fatalf("idx_jobs_root_id missing after migrate: %v", err)
	}

	// Backfill is idempotent: a second run (re-Migrate) changes nothing.
	if err := upgraded.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate returned error: %v", err)
	}
	for _, tc := range []struct{ id, want string }{{"R", "R"}, {"R/c1", "R"}, {"R/c2", "R/c2"}, {"bad", "bad"}} {
		if got := rootIDOf(t, upgraded, tc.id); got != tc.want {
			t.Fatalf("after idempotent re-Migrate root_id(%q) = %q, want %q", tc.id, got, tc.want)
		}
	}
}
