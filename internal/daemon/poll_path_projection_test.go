package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/github"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// seedLargeNonReviewJob inserts a non-review job carrying a ~1MB payload — a stand
// in for the real multi-megabyte implement payloads whose per-poll materialization
// #619's ListJobsByType swap avoids. The poll paths must reach the same decision
// they did under ListJobs, without this row's type ever qualifying. The payload is
// a raw JSON blob (never unmarshaled by the poll paths, which skip non-review types
// before decoding), so it does not depend on any JobPayload field.
func seedLargeNonReviewJob(t *testing.T, store *db.Store) {
	t.Helper()
	blob := `{"blob":"` + strings.Repeat("x", 1<<20) + `"}`
	if err := store.CreateJob(context.Background(), db.Job{
		ID:      "implement-huge",
		Agent:   "coder",
		Type:    "implement",
		State:   string(workflow.JobRunning),
		Payload: blob,
	}); err != nil {
		t.Fatalf("CreateJob implement returned error: %v", err)
	}
}

func seedReviewJob(t *testing.T, store *db.Store, repo, id, headSHA string, state workflow.JobState, result *workflow.AgentResult) {
	t.Helper()
	payload, err := json.Marshal(workflow.JobPayload{
		Repo:        repo,
		Branch:      "task-7",
		PullRequest: 7,
		HeadSHA:     headSHA,
		TaskID:      "task-007",
		TaskTitle:   "Task 7",
		LeadAgent:   "lead",
		Reviewers:   []string{"audit"},
		ReviewRound: id,
		Result:      result,
	})
	if err != nil {
		t.Fatalf("Marshal review payload returned error: %v", err)
	}
	if err := store.CreateJobWithEvent(context.Background(), db.Job{
		ID:      id,
		Agent:   "audit",
		Type:    "review",
		State:   string(state),
		Payload: string(payload),
	}, db.JobEvent{JobID: id, Kind: string(state), Message: "seeded review job"}); err != nil {
		t.Fatalf("CreateJobWithEvent review returned error: %v", err)
	}
}

func reviewPull(headSHA string) github.PullRequest {
	return github.PullRequest{
		Number:  7,
		Title:   "Task 7",
		State:   "open",
		URL:     "https://github.com/jerryfane/gitmoot/pull/7",
		HeadRef: "task-7",
		BaseRef: "main",
		HeadSHA: headSHA,
	}
}

// countingReviewLister wraps a real *db.Store and counts ListJobsByType calls,
// delegating so the query still returns the real rows. It backs
// TestPollOnceFetchesReviewJobsOncePerPoll's proof that PollOnce's reviewJobsMemo
// fetches the review-job list once per poll, not once (or ~2×) per open PR.
type countingReviewLister struct {
	inner *db.Store
	calls int32
}

func (c *countingReviewLister) ListJobsByType(ctx context.Context, jobType string) ([]db.Job, error) {
	atomic.AddInt32(&c.calls, 1)
	return c.inner.ListJobsByType(ctx, jobType)
}

// TestPollOnceFetchesReviewJobsOncePerPoll pins FIX-F (#620 review): PollOnce fetches
// the review-job list AT MOST ONCE per poll and shares that snapshot across every open
// PR's review-job consumers, instead of re-running ListJobsByType("review") per PR. A
// regression that dropped the per-poll memo (fetching per consumer/per PR) fires the
// counting lister once per PR and trips the assert.
func TestPollOnceFetchesReviewJobsOncePerPoll(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "jerryfane", Name: "gitmoot"}

	// Three open PRs, each already stored with a matching head, so pullRequestChanged
	// takes the unchanged-head path that inspects the review-job list for staleness on
	// EVERY PR — exactly the per-PR fetch the memo collapses to one.
	var pulls []github.PullRequest
	for i := int64(1); i <= 3; i++ {
		pull := github.PullRequest{
			Number: i, Title: fmt.Sprintf("Task %d", i), State: "open",
			URL:     fmt.Sprintf("https://github.com/jerryfane/gitmoot/pull/%d", i),
			HeadRef: fmt.Sprintf("task-%d", i), BaseRef: "main", HeadSHA: fmt.Sprintf("head-%d", i),
		}
		pulls = append(pulls, pull)
		if err := store.UpsertPullRequest(ctx, db.PullRequest{
			RepoFullName: repo.FullName(), Number: pull.Number, HeadBranch: pull.HeadRef,
			BaseBranch: pull.BaseRef, HeadSHA: pull.HeadSHA, State: "open",
		}); err != nil {
			t.Fatalf("UpsertPullRequest(%d) returned error: %v", i, err)
		}
	}
	// A large non-review job the memo must never materialize, plus one review job that
	// matches none of the PRs (keeps the list non-empty without changing any routing).
	seedLargeNonReviewJob(t, store)
	seedReviewJob(t, store, repo.FullName(), "review-unrelated", "other-head", workflow.JobQueued, nil)

	counter := &countingReviewLister{inner: store}
	realNewReviewJobsMemo := newReviewJobsMemo
	newReviewJobsMemo = func(reviewJobLister) *reviewJobsMemo { return realNewReviewJobsMemo(counter) }
	defer func() { newReviewJobsMemo = realNewReviewJobsMemo }()

	client := &fakeGitHub{pulls: pulls}
	if err := (Daemon{Repo: repo, Store: store, GitHub: client}).PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce returned error: %v", err)
	}
	if got := atomic.LoadInt32(&counter.calls); got != 1 {
		t.Fatalf("ListJobsByType(review) ran %d times across %d PRs, want 1 (memoized once per poll)", got, len(pulls))
	}
}

// TestSupersedeStaleReviewJobsSkipsLargeNonReviewPayload proves the #619 projection
// swap did not change supersession behavior: a stale-head review job is still
// superseded even with a large non-review payload present that ListJobsByType now
// skips reading.
func TestSupersedeStaleReviewJobsSkipsLargeNonReviewPayload(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "jerryfane", Name: "gitmoot"}
	seedLargeNonReviewJob(t, store)
	seedReviewJob(t, store, repo.FullName(), "review-stale", "old-head", workflow.JobQueued, nil)

	d := Daemon{Repo: repo, Store: store, Workflow: &workflow.Engine{Store: store}}
	if err := d.supersedeStaleReviewJobs(ctx, reviewPull("new-head"), nil); err != nil {
		t.Fatalf("supersedeStaleReviewJobs returned error: %v", err)
	}

	review, err := store.GetJob(ctx, "review-stale")
	if err != nil {
		t.Fatalf("GetJob review returned error: %v", err)
	}
	if review.State != string(workflow.JobCancelled) {
		t.Fatalf("stale review state = %q, want cancelled (superseded)", review.State)
	}
	events, err := store.ListJobEvents(ctx, "review-stale")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	found := false
	for _, e := range events {
		if e.Kind == workflow.JobEventSupersededStaleHead {
			found = true
		}
	}
	if !found {
		t.Fatalf("stale review missing superseded_stale_head event: %+v", events)
	}
	// The large non-review job must be untouched.
	implement, err := store.GetJob(ctx, "implement-huge")
	if err != nil {
		t.Fatalf("GetJob implement returned error: %v", err)
	}
	if implement.State != string(workflow.JobRunning) {
		t.Fatalf("implement job state = %q, want running (untouched)", implement.State)
	}
}

// TestPullRequestWorkflowRoutingStaleAmongLargeNonReviewPayload proves routing's
// stale detection is unchanged by the projection swap.
func TestPullRequestWorkflowRoutingStaleAmongLargeNonReviewPayload(t *testing.T) {
	ctx := context.Background()
	repo := github.Repository{Owner: "jerryfane", Name: "gitmoot"}

	t.Run("stale head routes stale", func(t *testing.T) {
		store := testStore(t)
		seedLargeNonReviewJob(t, store)
		seedReviewJob(t, store, repo.FullName(), "review-stale", "old-head", workflow.JobQueued, nil)
		d := Daemon{Repo: repo, Store: store, Workflow: &workflow.Engine{Store: store}}
		routing, err := d.pullRequestWorkflowRouting(ctx, reviewPull("new-head"), nil)
		if err != nil {
			t.Fatalf("pullRequestWorkflowRouting returned error: %v", err)
		}
		if !routing.stale {
			t.Fatalf("routing.stale = false, want true for a stale-head review")
		}
	})

	t.Run("current head routes not stale", func(t *testing.T) {
		store := testStore(t)
		seedLargeNonReviewJob(t, store)
		seedReviewJob(t, store, repo.FullName(), "review-current", "cur-head", workflow.JobQueued, nil)
		d := Daemon{Repo: repo, Store: store, Workflow: &workflow.Engine{Store: store}}
		routing, err := d.pullRequestWorkflowRouting(ctx, reviewPull("cur-head"), nil)
		if err != nil {
			t.Fatalf("pullRequestWorkflowRouting returned error: %v", err)
		}
		if routing.stale {
			t.Fatalf("routing.stale = true, want false when the review matches the current head")
		}
	})
}

// TestReconcileReviewingPullRequestAdvancesAmongLargeNonReviewPayload proves the
// reconcile advance decision is unchanged by the projection swap: an approved
// current-head review still advances the reviewing task to ready_to_merge with the
// large non-review payload present.
func TestReconcileReviewingPullRequestAdvancesAmongLargeNonReviewPayload(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "jerryfane", Name: "gitmoot"}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name: "lead", Role: "lead", Runtime: "codex", RuntimeRef: "last",
		RepoScope: repo.FullName(), Capabilities: []string{"implement"},
		AutonomyPolicy: "workspace-write", HealthStatus: "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent lead returned error: %v", err)
	}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name: "audit", Role: "reviewer", Runtime: "codex", RuntimeRef: "last",
		RepoScope: repo.FullName(), Capabilities: []string{"review"},
		AutonomyPolicy: "auto", HealthStatus: "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent audit returned error: %v", err)
	}
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: repo.FullName(), Branch: "task-7", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-007", RepoFullName: repo.FullName(), GoalID: "goal-1",
		Title: "Task 7", State: string(workflow.TaskReviewing), Branch: "task-7",
	}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	seedLargeNonReviewJob(t, store)
	seedReviewJob(t, store, repo.FullName(), "review-audit-task-007-review-1", "abc123", workflow.JobSucceeded, &workflow.AgentResult{Decision: "approved", Summary: "approved"})
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: repo.FullName(), Number: 7, HeadBranch: "task-7",
		BaseBranch: "main", HeadSHA: "abc123", State: "open",
	}); err != nil {
		t.Fatalf("UpsertPullRequest returned error: %v", err)
	}

	gate := &fakeWorkflowMergeGate{decision: workflow.MergeDecision{Ready: true}}
	engine := workflow.Engine{Store: store, MergeGate: gate}
	d := Daemon{Repo: repo, Store: store, Workflow: &engine}

	if err := d.reconcileReviewingPullRequest(ctx, reviewPull("abc123"), nil); err != nil {
		t.Fatalf("reconcileReviewingPullRequest returned error: %v", err)
	}
	task, err := store.GetTask(ctx, "task-007")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.State != string(workflow.TaskReadyToMerge) {
		t.Fatalf("task state = %q, want ready_to_merge (review advanced despite large non-review payload)", task.State)
	}
	if len(gate.requests) != 1 || gate.requests[0].HeadSHA != "abc123" {
		t.Fatalf("merge gate requests = %+v, want one for head abc123", gate.requests)
	}
}
