package workflow

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// referenceMergeGateReviewRequired is mergeGateReviewRequired before #2687:
// every job in the store, every payload decoded.
func referenceMergeGateReviewRequired(ctx context.Context, store *db.Store, payload JobPayload) (bool, error) {
	jobs, err := store.ListJobs(ctx)
	if err != nil {
		return false, err
	}
	for _, job := range jobs {
		if job.Type != "review" {
			continue
		}
		jobPayload, err := DecodeJobPayload(job)
		if err != nil {
			return false, err
		}
		if sameTask(payload, jobPayload) {
			return true, nil
		}
	}
	return false, nil
}

// #2687: the review-required check reads only the task's candidate review
// rows. On a fixture spanning several repos and PRs it answers exactly as the
// full scan did, including a legacy row whose PR lives only in its payload and
// a repo-less row that matches by task id alone.
func TestMergeGateReviewRequiredMatchesFullScan(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	jobs := []db.Job{
		{ID: "a1-review", Type: "review", Payload: `{"repo":"o/a","pull_request":1,"task_id":"t-a1"}`},
		{ID: "a2-review", Type: "review", Payload: `{"repo":"o/a","pull_request":2}`},
		{ID: "a2-implement", Type: "implement", Payload: `{"repo":"o/a","pull_request":2,"task_id":"t-a2"}`},
		{ID: "b1-review", Type: "review", Payload: `{"repo":"o/b","pull_request":1,"task_id":"t-b1"}`},
		{ID: "b2-ask", Type: "ask", Payload: `{"repo":"o/b","pull_request":2,"task_id":"t-b2"}`},
		{ID: "legacy-review", Type: "review", Payload: `{"repo":"o/b","pull_request":3,"task_id":"t-legacy"}`},
		{ID: "repoless-review", Type: "review", Payload: `{"task_id":"t-repoless"}`},
		{ID: "c-review-no-pr", Type: "review", Payload: `{"repo":"o/c","task_id":"t-c"}`},
	}
	for _, job := range jobs {
		job.Agent, job.State = "audit", string(JobSucceeded)
		if err := store.CreateJob(ctx, job); err != nil {
			t.Fatalf("CreateJob %s: %v", job.ID, err)
		}
	}
	raw, err := sql.Open("sqlite", store.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	// A row written before the pull_request projection existed.
	if _, err := raw.ExecContext(ctx, `UPDATE jobs SET pull_request = 0 WHERE id = 'legacy-review'`); err != nil {
		t.Fatal(err)
	}

	engine := testEngine(store)
	matched := 0
	for _, repo := range []string{"o/a", "o/b", "o/c", "o/x", ""} {
		for _, pr := range []int{0, 1, 2, 3} {
			for _, task := range []string{"", "t-a1", "t-a2", "t-b1", "t-b2", "t-legacy", "t-repoless", "t-c", "t-missing"} {
				payload := JobPayload{Repo: repo, PullRequest: pr, TaskID: task}
				want, err := referenceMergeGateReviewRequired(ctx, store, payload)
				if err != nil {
					t.Fatal(err)
				}
				got, err := engine.mergeGateReviewRequired(ctx, payload)
				if err != nil {
					t.Fatalf("mergeGateReviewRequired(%+v): %v", payload, err)
				}
				if got != want {
					t.Errorf("mergeGateReviewRequired(repo=%q pr=%d task=%q) = %v, full scan = %v", repo, pr, task, got, want)
				}
				if want {
					matched++
				}
			}
		}
	}
	if matched == 0 {
		t.Fatal("fixture matched nothing; the equivalence is vacuous")
	}
	for _, payload := range []JobPayload{
		{Repo: "o/b", PullRequest: 3, TaskID: "t-legacy"},
		{Repo: "o/a", PullRequest: 1, TaskID: "t-repoless"},
		{Repo: "o/a", PullRequest: 2},
	} {
		if got, err := engine.mergeGateReviewRequired(ctx, payload); err != nil || !got {
			t.Errorf("mergeGateReviewRequired(%+v) = %v, %v; want true", payload, got, err)
		}
	}
}

// The scoped read never decodes another repository's or PR's review payload:
// one the full scan choked on no longer fails an unrelated task's merge gate.
func TestMergeGateReviewRequiredSkipsOtherPullRequestsPayloads(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	for i, job := range []db.Job{
		// The projection reads repo/pull_request only; the full decode rejects
		// a numeric task_id.
		{ID: "b1-bad", Type: "review", Payload: `{"repo":"o/b","pull_request":1,"task_id":5}`},
		{ID: "a2-bad", Type: "review", Payload: `{"repo":"o/a","pull_request":2,"task_id":5}`},
		// Inserted and named last, so the full scan meets the bad rows first.
		{ID: "z-a1-review", Type: "review", Payload: `{"repo":"o/a","pull_request":1,"task_id":"t-a1"}`},
	} {
		job.Agent, job.State = fmt.Sprintf("audit-%d", i), string(JobSucceeded)
		if err := store.CreateJob(ctx, job); err != nil {
			t.Fatalf("CreateJob %s: %v", job.ID, err)
		}
	}
	payload := JobPayload{Repo: "o/a", PullRequest: 1, TaskID: "t-a1"}
	if _, err := referenceMergeGateReviewRequired(ctx, store, payload); err == nil {
		t.Fatal("fixture: the full scan should fail on the malformed payloads")
	}
	got, err := testEngine(store).mergeGateReviewRequired(ctx, payload)
	if err != nil || !got {
		t.Fatalf("mergeGateReviewRequired = %v, %v; want true without decoding o/b#1 or o/a#2", got, err)
	}
}
