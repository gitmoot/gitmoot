package db

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// #2755. The merge gate's shared-commit check reads the succeeded review rows
// of a repo, on any pull request, whose payload head is one of the merging pull
// request's commits. Repo and head match case-insensitively and a row with no
// repo projection is kept for the caller to match by payload; other repos,
// other heads, unsettled rows, non-review jobs and payloads that are not JSON
// stay out. Rows come back oldest first.
func TestListReviewJobsAtHeadsMatchesRepoAndHeadsCaseInsensitively(t *testing.T) {
	ctx := context.Background()
	store := openHeadlessTestStore(t)
	for _, job := range []Job{
		{ID: "other-pr-at-commit", Agent: "r", Type: "review", State: "succeeded", Payload: `{"repo":"O/R","pull_request":6,"head_sha":"AAAA"}`},
		{ID: "own-pr-at-head", Agent: "r", Type: "review", State: "succeeded", Payload: `{"repo":"o/r","pull_request":7,"head_sha":"bbbb"}`},
		{ID: "legacy-no-projection", Agent: "r", Type: "review", State: "succeeded", Payload: `{"repo":"o/r","pull_request":6,"head_sha":"aaaa"}`},
		{ID: "failed-at-commit", Agent: "r", Type: "review", State: "failed", Payload: `{"repo":"o/r","pull_request":6,"head_sha":"aaaa"}`},
		{ID: "other-head", Agent: "r", Type: "review", State: "succeeded", Payload: `{"repo":"o/r","pull_request":6,"head_sha":"cccc"}`},
		{ID: "other-repo", Agent: "r", Type: "review", State: "succeeded", Payload: `{"repo":"o/x","pull_request":6,"head_sha":"aaaa"}`},
		{ID: "not-review", Agent: "r", Type: "implement", State: "succeeded", Payload: `{"repo":"o/r","pull_request":6,"head_sha":"aaaa"}`},
		{ID: "headless", Agent: "r", Type: "review", State: "succeeded", Payload: `{"repo":"o/r","pull_request":6}`},
		{ID: "malformed", Agent: "r", Type: "review", State: "succeeded", Payload: `{not-json`},
	} {
		if err := store.CreateJob(ctx, job); err != nil {
			t.Fatalf("CreateJob %s: %v", job.ID, err)
		}
	}
	for id, created := range map[string]string{
		"other-pr-at-commit": "2026-11-25 10:00:02", "own-pr-at-head": "2026-11-25 10:00:01", "legacy-no-projection": "2026-11-25 10:00:03",
	} {
		if _, err := store.db.ExecContext(ctx, `UPDATE jobs SET created_at = ? WHERE id = ?`, created, id); err != nil {
			t.Fatal(err)
		}
	}
	// A row written before the repo projection existed.
	if _, err := store.db.ExecContext(ctx, `UPDATE jobs SET repo = '' WHERE id = 'legacy-no-projection'`); err != nil {
		t.Fatal(err)
	}
	jobs, err := store.ListReviewJobsAtHeads(ctx, "o/r", []string{"aaaa", " BBBB ", "aaaa", ""})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, job := range jobs {
		got = append(got, job.ID)
	}
	if want := []string{"own-pr-at-head", "other-pr-at-commit", "legacy-no-projection"}; !slices.Equal(got, want) {
		t.Fatalf("ListReviewJobsAtHeads = %v, want %v (oldest first)", got, want)
	}
	if jobs, err := store.ListReviewJobsAtHeads(ctx, "o/r", nil); err != nil || len(jobs) != 0 {
		t.Fatalf("no heads = %v, %v; want nothing", jobs, err)
	}
}

// The read seeks idx_jobs_review_lower_repo_state (case-insensitive repo and
// the succeeded state) instead of scanning jobs, and needs no SQL sort; the
// only scan is json_each over the bound array of heads.
func TestListReviewJobsAtHeadsSeeksCaseInsensitiveRepoIndex(t *testing.T) {
	ctx := context.Background()
	store := openHeadlessTestStore(t)
	rows, err := store.db.QueryContext(ctx, `EXPLAIN QUERY PLAN `+listReviewJobsAtHeadsSQL, "o/r", `["aaaa","bbbb"]`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan, "; ")
	if !strings.Contains(joined, "SEARCH jobs USING INDEX idx_jobs_review_lower_repo_state") ||
		strings.Contains(joined, "SCAN jobs") || strings.Contains(joined, "TEMP B-TREE") {
		t.Fatalf("query plan = %q, want a seek of idx_jobs_review_lower_repo_state without a jobs scan or sort", joined)
	}
}

// A pull request may carry more commits than SQLite binds variables (32,766):
// the heads are one JSON array parameter, so a 40,000-commit read succeeds.
func TestListReviewJobsAtHeadsReadsMoreHeadsThanSQLiteVariables(t *testing.T) {
	ctx := context.Background()
	store := openHeadlessTestStore(t)
	heads := make([]string, 0, 40000)
	for i := range 40000 {
		heads = append(heads, fmt.Sprintf("%040x", i))
	}
	last := heads[len(heads)-1]
	if err := store.CreateJob(ctx, Job{ID: "at-last-commit", Agent: "r", Type: "review", State: "succeeded",
		Payload: `{"repo":"o/r","pull_request":6,"head_sha":"` + last + `"}`}); err != nil {
		t.Fatal(err)
	}
	jobs, err := store.ListReviewJobsAtHeads(ctx, "o/r", heads)
	if err != nil {
		t.Fatalf("ListReviewJobsAtHeads(40000 heads): %v", err)
	}
	if len(jobs) != 1 || jobs[0].ID != "at-last-commit" {
		t.Fatalf("ListReviewJobsAtHeads(40000 heads) = %+v, want at-last-commit", jobs)
	}
}
