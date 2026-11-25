package db

import (
	"context"
	"slices"
	"testing"
)

// #2755. The merge gate's shared-commit check reads the review rows of a repo,
// on any pull request, whose payload head is one of the merging pull request's
// commits. Repo and head match case-insensitively; other repos, other heads,
// non-review jobs and payloads that are not JSON stay out.
func TestListReviewJobsAtHeadsMatchesRepoAndHeadsCaseInsensitively(t *testing.T) {
	ctx := context.Background()
	store := openHeadlessTestStore(t)
	for _, job := range []Job{
		{ID: "other-pr-at-commit", Agent: "r", Type: "review", State: "succeeded", Payload: `{"repo":"O/R","pull_request":6,"head_sha":"AAAA"}`},
		{ID: "own-pr-at-head", Agent: "r", Type: "review", State: "succeeded", Payload: `{"repo":"o/r","pull_request":7,"head_sha":"bbbb"}`},
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
	jobs, err := store.ListReviewJobsAtHeads(ctx, "o/r", []string{"aaaa", " BBBB ", "aaaa", ""})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, job := range jobs {
		got = append(got, job.ID)
	}
	slices.Sort(got)
	if want := []string{"other-pr-at-commit", "own-pr-at-head"}; !slices.Equal(got, want) {
		t.Fatalf("ListReviewJobsAtHeads = %v, want %v", got, want)
	}
	if jobs, err := store.ListReviewJobsAtHeads(ctx, "o/r", nil); err != nil || len(jobs) != 0 {
		t.Fatalf("no heads = %v, %v; want nothing", jobs, err)
	}
}
