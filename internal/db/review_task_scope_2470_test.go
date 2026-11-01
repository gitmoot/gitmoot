package db

import (
	"context"
	"slices"
	"testing"
)

// #2470. Review-round lookups read only the task's candidate rows. The scope
// must still include legacy rows whose pull_request projection is unset (the PR
// lives only in their payload), and must exclude other repos and other PRs.
func TestListReviewJobsInTaskScopeKeepsLegacyRows(t *testing.T) {
	ctx := context.Background()
	store := openHeadlessTestStore(t)
	for _, job := range []Job{
		{ID: "same-pr", Agent: "r", Type: "review", State: "succeeded", Payload: `{"repo":"o/r","pull_request":7,"review_round":"review-2"}`},
		{ID: "legacy", Agent: "r", Type: "review", State: "succeeded", Payload: `{"repo":"o/r","pull_request":7,"review_round":"review-3"}`},
		{ID: "other-pr", Agent: "r", Type: "review", State: "succeeded", Payload: `{"repo":"o/r","pull_request":8}`},
		{ID: "other-repo", Agent: "r", Type: "review", State: "succeeded", Payload: `{"repo":"o/x","pull_request":7}`},
		{ID: "not-review", Agent: "r", Type: "ask", State: "succeeded", Payload: `{"repo":"o/r","pull_request":7}`},
	} {
		if err := store.CreateJob(ctx, job); err != nil {
			t.Fatalf("CreateJob %s: %v", job.ID, err)
		}
	}
	// Simulate a row written before the pull_request projection existed.
	if _, err := store.db.ExecContext(ctx, `UPDATE jobs SET pull_request = 0 WHERE id = 'legacy'`); err != nil {
		t.Fatal(err)
	}
	ids := func(repo string, pr int) []string {
		t.Helper()
		jobs, err := store.ListReviewJobsInTaskScope(ctx, repo, pr)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, job := range jobs {
			out = append(out, job.ID)
		}
		slices.Sort(out)
		return out
	}
	if got, want := ids("o/r", 7), []string{"legacy", "same-pr"}; !slices.Equal(got, want) {
		t.Fatalf("scope(o/r, 7) = %v, want %v", got, want)
	}
	if got, want := ids("o/r", 0), []string{"legacy", "other-pr", "same-pr"}; !slices.Equal(got, want) {
		t.Fatalf("scope(o/r, 0) = %v, want %v", got, want)
	}
	if got, want := ids("", 7), []string{"legacy", "other-pr", "other-repo", "same-pr"}; !slices.Equal(got, want) {
		t.Fatalf("scope(empty repo) = %v, want every review job %v", got, want)
	}
}
