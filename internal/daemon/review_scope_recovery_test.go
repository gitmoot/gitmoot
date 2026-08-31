package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// TestPollOnceRetriesTransientScopeErrorThenDegrades pins the retry behaviour that
// used to be entangled with the deleted scope-block hooks. PollOnce previously
// carried a branch that recognised a BlockedError carrying the scope marker and
// recorded the PR baseline anyway, so the same head never re-fired. With the block
// gone, that branch is gone too, and every lifecycle error must take the ordinary
// transient path: surfaced, baseline NOT advanced, retried on the next poll. The
// second poll then exercises the degrade path at the same head, proving the removal
// suppressed nothing.
func TestPollOnceRetriesTransientScopeErrorThenDegrades(t *testing.T) {
	ctx := context.Background()
	store, repo, client := reviewScopeRecoveryFixture(t, string(workflow.TaskPullRequestOpen))

	resolverCalls := 0
	engine := workflow.Engine{
		Store: store, RequiredReviewers: []string{"audit"},
		ReviewChangedFiles: func(context.Context, string, int, string, string) ([]string, error) {
			resolverCalls++
			if resolverCalls == 1 {
				return nil, errors.New("compare review scope: github api 502 bad gateway")
			}
			return nil, workflow.ReviewScopeUnavailableError{Reason: `review scope compare is "diverged", not a direct follow-up`}
		},
	}
	daemon := Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}

	err := daemon.PollOnce(ctx)
	if err == nil || !strings.Contains(err.Error(), "502 bad gateway") {
		t.Fatalf("PollOnce 1 error = %v, want the transient resolver failure surfaced", err)
	}
	if jobs := reviewScopeDispatchedJobs(t, store); len(jobs) != 0 {
		t.Fatalf("jobs after a transient failure = %+v, want none dispatched", jobs)
	}
	// The baseline staying at the OLD head is what makes the next poll re-fire.
	if stored, storeErr := store.GetPullRequest(ctx, repo.FullName(), 7); storeErr != nil || stored.HeadSHA != "head-one" {
		t.Fatalf("baseline after a transient failure = %+v err=%v, want head-one (unrecorded, so the poll retries)", stored, storeErr)
	}
	if task, taskErr := store.GetTask(ctx, "task-007"); taskErr != nil || task.State == string(workflow.TaskBlocked) {
		t.Fatalf("task after a transient failure = %+v err=%v, want not blocked", task, taskErr)
	}

	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 2: %v", err)
	}
	if resolverCalls != 2 {
		t.Fatalf("resolver calls = %d, want the transient poll retried exactly once", resolverCalls)
	}
	dispatched := reviewScopeDispatchedJobs(t, store)
	if len(dispatched) != 1 {
		t.Fatalf("jobs after the retry = %+v, want one degraded full review", dispatched)
	}
	if payload := reviewScopePayload(t, dispatched[0]); payload.HeadSHA != "head-two" {
		t.Fatalf("degraded review head = %q, want head-two", payload.HeadSHA)
	}
	if stored, storeErr := store.GetPullRequest(ctx, repo.FullName(), 7); storeErr != nil || stored.HeadSHA != "head-two" {
		t.Fatalf("baseline after the retry = %+v err=%v, want head-two", stored, storeErr)
	}
	assertReviewScopeEvents(t, store, 1)
}

// TestPollOnceRecoversTaskAlreadyBlockedAtUnscopableHead is the upgrade path, and it
// pins the removal of the routing suppression specifically. The OLD code recorded the
// PR baseline at the blocked head (its PollOnce branch did exactly that) and then had
// pullRequestWorkflowRouting return "not changed" for any head carrying a
// review_scope_unavailable event, so a task blocked that way could never be looked at
// again: baseline == pull head, routing suppressed, nothing ran. This reconstructs
// that exact state — blocked task, baseline ALREADY at the current head, scope event at
// that head — and proves one poll now heals it.
//
// Measured, and worth stating because it corrects the obvious guess: the healing
// mechanism is the LIFECYCLE re-firing on a stale prior review, not reconcilePROpenTasks
// promoting the blocked task. HandlePullRequestOpened sets the task straight from
// blocked to reviewing when it dispatches, so no task_pr_open_auto event is emitted.
func TestPollOnceRecoversTaskAlreadyBlockedAtUnscopableHead(t *testing.T) {
	ctx := context.Background()
	store, repo, client := reviewScopeRecoveryFixture(t, string(workflow.TaskBlocked))
	// The old block path recorded the baseline at the blocked head.
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: repo.FullName(), Number: 7, HeadBranch: "task-7", BaseBranch: "main",
		HeadSHA: "head-two", State: "open",
	}); err != nil {
		t.Fatalf("UpsertPullRequest blocked baseline: %v", err)
	}
	if err := store.AddTaskEvent(ctx, db.TaskEvent{
		TaskID: "task-007",
		Kind:   "review_scope_unavailable",
		Reason: fmt.Sprintf("repo=%s pull_request=7 head_sha=head-two: %s: legacy block written before the degrade path existed",
			repo.FullName(), workflow.ReviewScopeUnavailableMarker),
	}); err != nil {
		t.Fatalf("AddTaskEvent: %v", err)
	}

	engine := workflow.Engine{
		Store: store, RequiredReviewers: []string{"audit"},
		ReviewChangedFiles: func(context.Context, string, int, string, string) ([]string, error) {
			return []string{"internal/boundary.go"}, nil
		},
	}
	daemon := Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}

	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	task, err := store.GetTask(ctx, "task-007")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != string(workflow.TaskReviewing) {
		t.Fatalf("task after the upgrade poll = %q, want reviewing (the legacy wedge healed)", task.State)
	}
	dispatched := reviewScopeDispatchedJobs(t, store)
	if len(dispatched) != 1 {
		t.Fatalf("jobs after the upgrade poll = %+v, want one review", dispatched)
	}
	payload := reviewScopePayload(t, dispatched[0])
	if payload.HeadSHA != "head-two" {
		t.Fatalf("recovered review head = %q, want head-two", payload.HeadSHA)
	}
	// The range IS scopable now, so recovery must not silently re-review the whole PR.
	if payload.ReviewScope == nil || payload.ReviewScope.PreviousHeadSHA != "head-one" {
		t.Fatalf("recovered review scope = %+v, want a scoped follow-up from head-one", payload.ReviewScope)
	}
	// No NEW scope record: the range was resolvable this time.
	assertReviewScopeEvents(t, store, 1)
}

func reviewScopeRecoveryFixture(t *testing.T, taskState string) (*db.Store, github.Repository, *fakeGitHub) {
	t.Helper()
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	for _, agent := range []db.Agent{
		{Name: "lead", Role: "lead", Runtime: "codex", RuntimeRef: "last", RepoScope: repo.FullName(), Capabilities: []string{"implement"}, AutonomyPolicy: "workspace-write", HealthStatus: "ok"},
		{Name: "audit", Role: "reviewer", Runtime: "codex", RuntimeRef: "last", RepoScope: repo.FullName(), Capabilities: []string{"review"}, AutonomyPolicy: "auto", HealthStatus: "ok"},
	} {
		if err := store.UpsertAgent(ctx, agent); err != nil {
			t.Fatalf("UpsertAgent %s: %v", agent.Name, err)
		}
	}
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: repo.FullName(), Branch: "task-7", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock acquired=%v err=%v", acquired, err)
	}
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-007", RepoFullName: repo.FullName(), GoalID: "goal-1", Title: "Task 7",
		State: taskState, Branch: "task-7",
	}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	priorPayload, err := json.Marshal(workflow.JobPayload{
		Repo: repo.FullName(), Branch: "task-7", PullRequest: 7, HeadSHA: "head-one",
		TaskID: "task-007", TaskTitle: "Task 7", LeadAgent: "lead", Reviewers: []string{"audit"},
		ReviewRound: "review-1", Result: &workflow.AgentResult{Decision: "changes_requested", Summary: "fix it"},
	})
	if err != nil {
		t.Fatalf("json.Marshal prior review: %v", err)
	}
	if err := store.CreateJob(ctx, db.Job{
		ID: "prior-review", Agent: "audit", Type: "review", State: string(workflow.JobSucceeded), Payload: string(priorPayload),
	}); err != nil {
		t.Fatalf("CreateJob prior review: %v", err)
	}
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: repo.FullName(), Number: 7, HeadBranch: "task-7", BaseBranch: "main",
		HeadSHA: "head-one", State: "open",
	}); err != nil {
		t.Fatalf("UpsertPullRequest: %v", err)
	}
	client := &fakeGitHub{
		pulls: []github.PullRequest{{
			Number: 7, Title: "Task 7", State: "open", URL: "https://github.com/gitmoot/gitmoot/pull/7",
			HeadRef: "task-7", BaseRef: "main", HeadSHA: "head-two",
		}},
		comments: map[int64][]github.IssueComment{7: {}},
	}
	return store, repo, client
}

func reviewScopeDispatchedJobs(t *testing.T, store *db.Store) []db.Job {
	t.Helper()
	jobs, err := store.ListJobs(context.Background())
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	var dispatched []db.Job
	for _, job := range jobs {
		if job.ID != "prior-review" {
			dispatched = append(dispatched, job)
		}
	}
	return dispatched
}

func reviewScopePayload(t *testing.T, job db.Job) workflow.JobPayload {
	t.Helper()
	var payload workflow.JobPayload
	if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
		t.Fatalf("unmarshal payload of %s: %v", job.ID, err)
	}
	return payload
}

func assertReviewScopeEvents(t *testing.T, store *db.Store, want int) {
	t.Helper()
	events, err := store.ListTaskEvents(context.Background(), "task-007")
	if err != nil {
		t.Fatalf("ListTaskEvents: %v", err)
	}
	got := 0
	for _, event := range events {
		if event.Kind == "review_scope_unavailable" {
			got++
		}
	}
	if got != want {
		t.Fatalf("review_scope_unavailable events = %d, want %d", got, want)
	}
}
