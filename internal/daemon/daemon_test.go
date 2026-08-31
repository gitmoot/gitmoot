package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestPollOnceCreatesJobAndAcknowledgement(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	if err := store.UpsertAgentTemplate(ctx, db.AgentTemplate{
		ID:             "thermo-nuclear-code-quality-review",
		Name:           "Thermo-Nuclear Code Quality Review",
		SourceRepo:     "cursor/plugins",
		SourceRef:      "main",
		SourcePath:     "cursor-team-kit/skills/thermo-nuclear-code-quality-review/SKILL.md",
		ResolvedCommit: "abc123",
		Content:        "Review deeply.",
	}); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "audit",
		Role:           "reviewer",
		Runtime:        "codex",
		RuntimeRef:     "last",
		RepoScope:      repo.FullName(),
		TemplateID:     "thermo-nuclear-code-quality-review",
		Capabilities:   []string{"review"},
		AutonomyPolicy: "auto",
		HealthStatus:   "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent returned error: %v", err)
	}
	client := &fakeGitHub{
		pulls: []github.PullRequest{{
			Number:  7,
			Title:   "Task 7",
			State:   "open",
			URL:     "https://github.com/gitmoot/gitmoot/pull/7",
			HeadRef: "task-7",
			BaseRef: "main",
			HeadSHA: "abc123",
		}},
		comments: map[int64][]github.IssueComment{
			7: {{ID: 101, Body: "/gitmoot audit review focus on tests", Author: "alice"}},
		},
	}

	err := (Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &workflow.Engine{Store: store, RequireWorkflowPolicy: func(string) workflow.RequireWorkflowPolicy {
		return workflow.RequireWorkflowPolicy{Enabled: true, Mode: "strict"}
	}}}).PollOnce(ctx)

	if err != nil {
		t.Fatalf("PollOnce returned error: %v", err)
	}
	if len(client.posted) != 1 {
		t.Fatalf("posted acknowledgements = %+v, want 1", client.posted)
	}
	if !strings.Contains(client.posted[0].body, "queued `review` job") || !strings.Contains(client.posted[0].body, "`audit`") {
		t.Fatalf("ack body = %q", client.posted[0].body)
	}

	jobID := jobID(repo, 7, 101, 0, "audit", "review")
	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.Agent != "audit" || job.Type != "review" || job.State != string(workflow.JobQueued) {
		t.Fatalf("job = %+v", job)
	}
	var payload workflow.JobPayload
	if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.Repo != repo.FullName() || payload.Branch != "task-7" || payload.PullRequest != 7 || payload.Sender != "alice" || payload.Instructions != "focus on tests" {
		t.Fatalf("payload = %+v", payload)
	}
	if !strings.HasPrefix(payload.WorkflowID, "adhoc/") {
		t.Fatalf("strict comment dispatch workflow=%q, want auto label", payload.WorkflowID)
	}
	if payload.TemplateID != "thermo-nuclear-code-quality-review" || payload.TemplateResolvedCommit != "abc123" || payload.TemplateContent != "Review deeply." {
		t.Fatalf("payload template snapshot = %+v", payload)
	}
	events, err := store.ListJobEvents(ctx, jobID)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if len(events) != 3 || events[0].Kind != string(workflow.JobQueued) || events[1].Kind != "workflow_autolabeled" || events[2].Kind != "routed" {
		t.Fatalf("events = %+v", events)
	}
}

func TestPollOnceAcknowledgesAgentWithoutRepoAccess(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "audit",
		Role:           "reviewer",
		Runtime:        "codex",
		RuntimeRef:     "last",
		Capabilities:   []string{"review"},
		AutonomyPolicy: "auto",
		HealthStatus:   "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent returned error: %v", err)
	}
	client := &fakeGitHub{
		pulls: []github.PullRequest{{
			Number:  7,
			Title:   "Task 7",
			State:   "open",
			URL:     "https://github.com/gitmoot/gitmoot/pull/7",
			HeadRef: "task-7",
			BaseRef: "main",
			HeadSHA: "abc123",
		}},
		comments: map[int64][]github.IssueComment{
			7: {{ID: 101, Body: "/gitmoot audit review focus on tests", Author: "alice"}},
		},
	}

	err := (Daemon{Repo: repo, Store: store, GitHub: client}).PollOnce(ctx)

	if err != nil {
		t.Fatalf("PollOnce returned error: %v", err)
	}
	if len(client.posted) != 1 || !strings.Contains(client.posted[0].body, "not allowed") {
		t.Fatalf("posted acknowledgements = %+v, want not-allowed ack", client.posted)
	}
	jobID := jobID(repo, 7, 101, 0, "audit", "review")
	if _, err := store.GetJob(ctx, jobID); err == nil {
		t.Fatal("job was queued for agent without repo access")
	}
}

func TestPollOnceRoutesPullRequestUpdatesToWorkflow(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "lead",
		Role:           "lead",
		Runtime:        "codex",
		RuntimeRef:     "last",
		RepoScope:      repo.FullName(),
		Capabilities:   []string{"implement"},
		AutonomyPolicy: "workspace-write",
		HealthStatus:   "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent lead returned error: %v", err)
	}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "audit",
		Role:           "reviewer",
		Runtime:        "codex",
		RuntimeRef:     "last",
		RepoScope:      repo.FullName(),
		Capabilities:   []string{"review"},
		AutonomyPolicy: "auto",
		HealthStatus:   "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent audit returned error: %v", err)
	}
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: repo.FullName(), Branch: "task-7", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	if err := store.UpsertTask(ctx, db.Task{ID: "task-007", GoalID: "goal-1", Title: "Task 7", State: string(workflow.TaskPlanned), Branch: "task-7"}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	client := &fakeGitHub{
		pulls: []github.PullRequest{{
			Number:  7,
			Title:   "Task 7",
			State:   "open",
			URL:     "https://github.com/gitmoot/gitmoot/pull/7",
			HeadRef: "task-7",
			BaseRef: "main",
			HeadSHA: "abc123",
		}},
		comments: map[int64][]github.IssueComment{7: {}},
	}
	engine := workflow.Engine{
		Store: store,
		JobID: func(request workflow.JobRequest) string {
			parts := []string{request.Action, request.Agent, request.TaskID}
			if request.ReviewRound != "" {
				parts = append(parts, request.ReviewRound)
			}
			return strings.Join(parts, "-")
		},
	}
	daemon := Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}

	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("first PollOnce returned error: %v", err)
	}
	if _, err := store.GetJob(ctx, "review-audit-task-007-review-1"); err != nil {
		t.Fatalf("GetJob first review round returned error: %v", err)
	}
	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("second PollOnce returned error: %v", err)
	}
	if _, err := store.GetJob(ctx, "review-audit-task-007-review-2"); err == nil {
		t.Fatal("unchanged pull request head created a second review round")
	}

	client.pulls[0].HeadSHA = "def456"
	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("third PollOnce returned error: %v", err)
	}
	if _, err := store.GetJob(ctx, "review-audit-task-007-review-2"); err != nil {
		t.Fatalf("GetJob second review round returned error: %v", err)
	}
}

func TestPollOnceDegradesUnscopableHeadToRecordedFullReview(t *testing.T) {
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
		State: string(workflow.TaskPullRequestOpen), Branch: "task-7",
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
	resolverCalls := 0
	engine := workflow.Engine{
		Store: store, RequiredReviewers: []string{"audit"},
		ReviewChangedFiles: func(context.Context, string, int, string, string) ([]string, error) {
			resolverCalls++
			return nil, workflow.ReviewScopeUnavailableError{Reason: `review scope compare is "diverged", not a direct follow-up`}
		},
	}
	daemon := Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}
	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 1: %v", err)
	}
	// One derivation for this head, not one per consumer: the dispatch invalidates
	// the poll's review-job snapshot, so reconcileReviewingPullRequest sees the
	// review that was just enqueued instead of re-entering the lifecycle.
	if resolverCalls != 1 {
		t.Fatalf("changed-files resolver calls in poll 1 = %d, want 1", resolverCalls)
	}
	for poll := range 4 {
		if err := daemon.PollOnce(ctx); err != nil {
			t.Fatalf("PollOnce %d: %v", poll+2, err)
		}
	}
	// The degraded review is dispatched AT this head, so routing stops seeing the
	// PR as changed. A range that keeps re-resolving every poll is the wedge this
	// replaced.
	if resolverCalls != 1 {
		t.Fatalf("changed-files resolver calls = %d after 5 polls, want no re-fire beyond the first", resolverCalls)
	}
	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	var dispatched []db.Job
	for _, job := range jobs {
		if job.ID != "prior-review" {
			dispatched = append(dispatched, job)
		}
	}
	if len(dispatched) != 1 {
		t.Fatalf("jobs dispatched after unscopable head = %+v, want exactly one full review", dispatched)
	}
	var payload workflow.JobPayload
	if err := json.Unmarshal([]byte(dispatched[0].Payload), &payload); err != nil {
		t.Fatalf("unmarshal dispatched payload: %v", err)
	}
	if payload.HeadSHA != "head-two" {
		t.Fatalf("dispatched review head = %q, want head-two (re-anchors the prior head)", payload.HeadSHA)
	}
	if strings.Contains(payload.Instructions, "scoped follow-up") {
		t.Fatalf("dispatched instructions = %q, want an unscoped full review", payload.Instructions)
	}
	task, err := store.GetTask(ctx, "task-007")
	if err != nil || task.State != string(workflow.TaskReviewing) {
		t.Fatalf("task after unscopable head = %+v err=%v, want reviewing, not a permanent block", task, err)
	}
	events, err := store.ListTaskEvents(ctx, "task-007")
	if err != nil {
		t.Fatalf("ListTaskEvents: %v", err)
	}
	scopeEvents := 0
	for _, event := range events {
		if event.Kind == "review_scope_unavailable" {
			scopeEvents++
		}
	}
	if scopeEvents != 1 {
		t.Fatalf("review_scope_unavailable events = %d, want 1", scopeEvents)
	}
	stored, err := store.GetPullRequest(ctx, repo.FullName(), 7)
	if err != nil || stored.HeadSHA != "head-two" {
		t.Fatalf("stored baseline = %+v err=%v, want head-two", stored, err)
	}
}

func TestHandlePullRequestWorkflowSkipsReviewFanoutWhenLockSet(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "lead",
		Role:           "lead",
		Runtime:        "codex",
		RuntimeRef:     "last",
		RepoScope:      repo.FullName(),
		Capabilities:   []string{"implement"},
		AutonomyPolicy: "workspace-write",
		HealthStatus:   "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent lead returned error: %v", err)
	}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "audit",
		Role:           "reviewer",
		Runtime:        "codex",
		RuntimeRef:     "last",
		RepoScope:      repo.FullName(),
		Capabilities:   []string{"review"},
		AutonomyPolicy: "auto",
		HealthStatus:   "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent audit returned error: %v", err)
	}
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: repo.FullName(), Branch: "task-7", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	if err := store.UpsertTask(ctx, db.Task{ID: "task-007", GoalID: "goal-1", Title: "Task 7", State: string(workflow.TaskPlanned), Branch: "task-7"}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	// The implement-job advancement would persist this flag onto the lock; set it
	// directly here to exercise the daemon's trigger-2 read.
	if err := store.SetBranchLockReviewFanout(ctx, repo.FullName(), "task-7", true); err != nil {
		t.Fatalf("SetBranchLockReviewFanout returned error: %v", err)
	}
	client := &fakeGitHub{
		pulls:    []github.PullRequest{},
		comments: map[int64][]github.IssueComment{7: {}},
	}
	gate := &fakeWorkflowMergeGate{decision: workflow.MergeDecision{Ready: true}}
	engine := workflow.Engine{
		Store:     store,
		MergeGate: gate,
		JobID: func(request workflow.JobRequest) string {
			parts := []string{request.Action, request.Agent, request.TaskID}
			if request.ReviewRound != "" {
				parts = append(parts, request.ReviewRound)
			}
			return strings.Join(parts, "-")
		},
	}
	daemon := Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}

	pull := github.PullRequest{
		Number:  7,
		Title:   "Task 7",
		State:   "open",
		URL:     "https://github.com/gitmoot/gitmoot/pull/7",
		HeadRef: "task-7",
		BaseRef: "main",
		HeadSHA: "abc123",
	}
	if err := daemon.handlePullRequestWorkflow(ctx, pull, nil); err != nil {
		t.Fatalf("handlePullRequestWorkflow returned error: %v", err)
	}

	// Zero review jobs were enqueued even though a review-capable agent exists.
	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	for _, job := range jobs {
		if job.Type == "review" {
			t.Fatalf("expected no review jobs with skip set, found %+v", job)
		}
	}
	// The baseline is recorded, but native merge authority is skipped entirely.
	if len(gate.requests) != 0 {
		t.Fatalf("merge gate requests = %+v, want none for skip-native-review-fanout", gate.requests)
	}
	if _, err := store.GetPullRequest(ctx, repo.FullName(), 7); err != nil {
		t.Fatalf("GetPullRequest returned error: %v", err)
	}
}

func TestHandlePullRequestWorkflowFansOutWhenLockUnset(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "lead",
		Role:           "lead",
		Runtime:        "codex",
		RuntimeRef:     "last",
		RepoScope:      repo.FullName(),
		Capabilities:   []string{"implement"},
		AutonomyPolicy: "workspace-write",
		HealthStatus:   "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent lead returned error: %v", err)
	}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "audit",
		Role:           "reviewer",
		Runtime:        "codex",
		RuntimeRef:     "last",
		RepoScope:      repo.FullName(),
		Capabilities:   []string{"review"},
		AutonomyPolicy: "auto",
		HealthStatus:   "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent audit returned error: %v", err)
	}
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: repo.FullName(), Branch: "task-7", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	if err := store.UpsertTask(ctx, db.Task{ID: "task-007", GoalID: "goal-1", Title: "Task 7", State: string(workflow.TaskPlanned), Branch: "task-7"}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	client := &fakeGitHub{
		pulls:    []github.PullRequest{},
		comments: map[int64][]github.IssueComment{7: {}},
	}
	engine := workflow.Engine{
		Store: store,
		JobID: func(request workflow.JobRequest) string {
			parts := []string{request.Action, request.Agent, request.TaskID}
			if request.ReviewRound != "" {
				parts = append(parts, request.ReviewRound)
			}
			return strings.Join(parts, "-")
		},
	}
	daemon := Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}

	pull := github.PullRequest{
		Number:  7,
		Title:   "Task 7",
		State:   "open",
		URL:     "https://github.com/gitmoot/gitmoot/pull/7",
		HeadRef: "task-7",
		BaseRef: "main",
		HeadSHA: "abc123",
	}
	if err := daemon.handlePullRequestWorkflow(ctx, pull, nil); err != nil {
		t.Fatalf("handlePullRequestWorkflow returned error: %v", err)
	}
	if _, err := store.GetJob(ctx, "review-audit-task-007-review-1"); err != nil {
		t.Fatalf("expected review job to be enqueued (default fanout): %v", err)
	}
}

func TestPollOnceRetriesPullRequestWorkflowAfterRoutingFailure(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "lead",
		Role:           "lead",
		Runtime:        "codex",
		RuntimeRef:     "last",
		RepoScope:      repo.FullName(),
		Capabilities:   []string{"implement"},
		AutonomyPolicy: "workspace-write",
		HealthStatus:   "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent lead returned error: %v", err)
	}
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: repo.FullName(), Branch: "task-7", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	client := &fakeGitHub{
		pulls: []github.PullRequest{{
			Number:  7,
			Title:   "Task 7",
			State:   "open",
			URL:     "https://github.com/gitmoot/gitmoot/pull/7",
			HeadRef: "task-7",
			BaseRef: "main",
			HeadSHA: "abc123",
		}},
		comments: map[int64][]github.IssueComment{
			7: {{ID: 707, Body: "/gitmoot lead implement handle manual fallback", Author: "alice"}},
		},
	}
	engine := workflow.Engine{
		Store:             store,
		RequiredReviewers: []string{"audit"},
		JobID: func(request workflow.JobRequest) string {
			parts := []string{request.Action, request.Agent, request.TaskID}
			if request.ReviewRound != "" {
				parts = append(parts, request.ReviewRound)
			}
			return strings.Join(parts, "-")
		},
	}
	daemon := Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}

	if err := daemon.PollOnce(ctx); err == nil {
		t.Fatal("PollOnce succeeded despite missing required reviewer")
	}
	if _, err := store.GetPullRequest(ctx, repo.FullName(), 7); err == nil {
		t.Fatal("pull request head was recorded before workflow routing succeeded")
	}
	if _, err := store.GetJob(ctx, jobID(repo, 7, 707, 0, "lead", "implement")); err != nil {
		t.Fatalf("manual comment job was not routed after workflow failure: %v", err)
	}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "audit",
		Role:           "reviewer",
		Runtime:        "codex",
		RuntimeRef:     "last",
		RepoScope:      repo.FullName(),
		Capabilities:   []string{"review"},
		AutonomyPolicy: "auto",
		HealthStatus:   "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent audit returned error: %v", err)
	}

	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("retry PollOnce returned error: %v", err)
	}
	if _, err := store.GetJob(ctx, "review-audit-task-7-review-1"); err != nil {
		t.Fatalf("GetJob retry review round returned error: %v", err)
	}
	if pr, err := store.GetPullRequest(ctx, repo.FullName(), 7); err != nil || pr.HeadSHA != "abc123" {
		t.Fatalf("stored pull request after retry = %+v err=%v", pr, err)
	}
}

func TestPollOnceRecordsAlreadyRoutedPullRequestWithoutDuplicateReviewRound(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "lead",
		Role:           "lead",
		Runtime:        "codex",
		RuntimeRef:     "last",
		RepoScope:      repo.FullName(),
		Capabilities:   []string{"implement"},
		AutonomyPolicy: "workspace-write",
		HealthStatus:   "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent lead returned error: %v", err)
	}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "audit",
		Role:           "reviewer",
		Runtime:        "codex",
		RuntimeRef:     "last",
		RepoScope:      repo.FullName(),
		Capabilities:   []string{"review"},
		AutonomyPolicy: "auto",
		HealthStatus:   "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent audit returned error: %v", err)
	}
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: repo.FullName(), Branch: "task-7", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	stalePayload, err := json.Marshal(workflow.JobPayload{
		Repo:        repo.FullName(),
		Branch:      "task-7",
		PullRequest: 7,
		HeadSHA:     "old123",
		TaskID:      "task-7",
		LeadAgent:   "lead",
		Reviewers:   []string{"audit"},
		ReviewRound: "review-1",
	})
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{
		ID:      "review-audit-task-7-review-1",
		Agent:   "audit",
		Type:    "review",
		State:   string(workflow.JobQueued),
		Payload: string(stalePayload),
	}, db.JobEvent{Kind: string(workflow.JobQueued), Message: "old routed review"}); err != nil {
		t.Fatalf("CreateJobWithEvent stale returned error: %v", err)
	}
	currentPayload, err := json.Marshal(workflow.JobPayload{
		Repo:        repo.FullName(),
		Branch:      "task-7",
		PullRequest: 7,
		HeadSHA:     "abc123",
		TaskID:      "task-7",
		LeadAgent:   "lead",
		Reviewers:   []string{"audit"},
		ReviewRound: "review-2",
	})
	if err != nil {
		t.Fatalf("Marshal current returned error: %v", err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{
		ID:      "review-audit-task-7-review-2",
		Agent:   "audit",
		Type:    "review",
		State:   string(workflow.JobQueued),
		Payload: string(currentPayload),
	}, db.JobEvent{Kind: string(workflow.JobQueued), Message: "already routed by engine"}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: repo.FullName(),
		Number:       7,
		HeadBranch:   "task-7",
		HeadSHA:      "abc123",
		State:        "open",
	}); err != nil {
		t.Fatalf("UpsertPullRequest returned error: %v", err)
	}
	client := &fakeGitHub{
		pulls: []github.PullRequest{{
			Number:  7,
			Title:   "Task 7",
			State:   "open",
			URL:     "https://github.com/gitmoot/gitmoot/pull/7",
			HeadRef: "task-7",
			BaseRef: "main",
			HeadSHA: "abc123",
		}},
		comments: map[int64][]github.IssueComment{7: {}},
	}
	engine := workflow.Engine{
		Store: store,
		JobID: func(request workflow.JobRequest) string {
			parts := []string{request.Action, request.Agent, request.TaskID}
			if request.ReviewRound != "" {
				parts = append(parts, request.ReviewRound)
			}
			return strings.Join(parts, "-")
		},
	}
	daemon := Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}

	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce returned error: %v", err)
	}
	if _, err := store.GetJob(ctx, "review-audit-task-7-review-3"); err == nil {
		t.Fatal("already routed pull request created a duplicate review round")
	}
	if pr, err := store.GetPullRequest(ctx, repo.FullName(), 7); err != nil || pr.HeadSHA != "abc123" {
		t.Fatalf("stored pull request = %+v err=%v", pr, err)
	}
}

func TestPollOnceReroutesLegacyReviewWithoutHeadSHA(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "lead",
		Role:           "lead",
		Runtime:        "codex",
		RuntimeRef:     "last",
		RepoScope:      repo.FullName(),
		Capabilities:   []string{"implement"},
		AutonomyPolicy: "workspace-write",
		HealthStatus:   "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent lead returned error: %v", err)
	}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "audit",
		Role:           "reviewer",
		Runtime:        "codex",
		RuntimeRef:     "last",
		RepoScope:      repo.FullName(),
		Capabilities:   []string{"review"},
		AutonomyPolicy: "auto",
		HealthStatus:   "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent audit returned error: %v", err)
	}
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: repo.FullName(), Branch: "task-7", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	payload, err := json.Marshal(workflow.JobPayload{
		Repo:        repo.FullName(),
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		LeadAgent:   "lead",
		Reviewers:   []string{"audit"},
		ReviewRound: "review-1",
	})
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{
		ID:      "review-audit-task-7-review-1",
		Agent:   "audit",
		Type:    "review",
		State:   string(workflow.JobQueued),
		Payload: string(payload),
	}, db.JobEvent{Kind: string(workflow.JobQueued), Message: "legacy review"}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: repo.FullName(),
		Number:       7,
		HeadBranch:   "task-7",
		HeadSHA:      "abc123",
		State:        "open",
	}); err != nil {
		t.Fatalf("UpsertPullRequest returned error: %v", err)
	}
	client := &fakeGitHub{
		pulls: []github.PullRequest{{
			Number:  7,
			Title:   "Task 7",
			State:   "open",
			URL:     "https://github.com/gitmoot/gitmoot/pull/7",
			HeadRef: "task-7",
			BaseRef: "main",
			HeadSHA: "abc123",
		}},
		comments: map[int64][]github.IssueComment{7: {}},
	}
	engine := workflow.Engine{
		Store: store,
		JobID: func(request workflow.JobRequest) string {
			parts := []string{request.Action, request.Agent, request.TaskID}
			if request.ReviewRound != "" {
				parts = append(parts, request.ReviewRound)
			}
			return strings.Join(parts, "-")
		},
	}
	daemon := Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}

	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce returned error: %v", err)
	}
	oldJob, err := store.GetJob(ctx, "review-audit-task-7-review-1")
	if err != nil {
		t.Fatalf("GetJob legacy review returned error: %v", err)
	}
	if oldJob.State != string(workflow.JobCancelled) {
		t.Fatalf("legacy review state = %q, want cancelled", oldJob.State)
	}
	events, err := store.ListJobEvents(ctx, oldJob.ID)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !hasDaemonJobEvent(events, workflow.JobEventSupersededStaleHead) {
		t.Fatalf("legacy review events = %+v, want superseded stale head", events)
	}
	job, err := store.GetJob(ctx, "review-audit-task-7-review-2")
	if err != nil {
		t.Fatalf("GetJob rerouted review returned error: %v", err)
	}
	if !strings.Contains(job.Payload, `"head_sha":"abc123"`) {
		t.Fatalf("rerouted job payload missing head sha: %s", job.Payload)
	}
}

func TestPollOnceReconcilesReviewingPullRequestWithApprovedCurrentReview(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "lead",
		Role:           "lead",
		Runtime:        "codex",
		RuntimeRef:     "last",
		RepoScope:      repo.FullName(),
		Capabilities:   []string{"implement"},
		AutonomyPolicy: "workspace-write",
		HealthStatus:   "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent lead returned error: %v", err)
	}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "audit",
		Role:           "reviewer",
		Runtime:        "codex",
		RuntimeRef:     "last",
		RepoScope:      repo.FullName(),
		Capabilities:   []string{"review"},
		AutonomyPolicy: "auto",
		HealthStatus:   "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent audit returned error: %v", err)
	}
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: repo.FullName(), Branch: "task-7", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	if err := store.UpsertTask(ctx, db.Task{
		ID:           "task-007",
		RepoFullName: repo.FullName(),
		GoalID:       "goal-1",
		Title:        "Task 7",
		State:        string(workflow.TaskReviewing),
		Branch:       "task-7",
	}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	payload, err := json.Marshal(workflow.JobPayload{
		Repo:        repo.FullName(),
		Branch:      "task-7",
		PullRequest: 7,
		HeadSHA:     "abc123",
		TaskID:      "task-007",
		TaskTitle:   "Task 7",
		LeadAgent:   "lead",
		Reviewers:   []string{"audit"},
		ReviewRound: "review-1",
		Result:      &workflow.AgentResult{Decision: "approved", Summary: "approved"},
	})
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{
		ID:      "review-audit-task-007-review-1",
		Agent:   "audit",
		Type:    "review",
		State:   string(workflow.JobSucceeded),
		Payload: string(payload),
	}, db.JobEvent{Kind: string(workflow.JobSucceeded), Message: "review completed before daemon reconciliation"}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: repo.FullName(),
		Number:       7,
		HeadBranch:   "task-7",
		BaseBranch:   "main",
		HeadSHA:      "abc123",
		State:        "open",
	}); err != nil {
		t.Fatalf("UpsertPullRequest returned error: %v", err)
	}
	client := &fakeGitHub{
		pulls: []github.PullRequest{{
			Number:  7,
			Title:   "Task 7",
			State:   "open",
			URL:     "https://github.com/gitmoot/gitmoot/pull/7",
			HeadRef: "task-7",
			BaseRef: "main",
			HeadSHA: "abc123",
		}},
		comments: map[int64][]github.IssueComment{7: {}},
	}
	gate := &fakeWorkflowMergeGate{decision: workflow.MergeDecision{Ready: true}}
	engine := workflow.Engine{Store: store, MergeGate: gate}
	daemon := Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}

	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce returned error: %v", err)
	}
	task, err := store.GetTask(ctx, "task-007")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.State != string(workflow.TaskReadyToMerge) {
		t.Fatalf("task state = %q, want ready_to_merge", task.State)
	}
	if len(gate.requests) != 1 || gate.requests[0].HeadSHA != "abc123" {
		t.Fatalf("merge gate requests = %+v", gate.requests)
	}
}

func TestPollOnceRetriesReadyToMergePullRequestWithoutHeadChange(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	if err := store.UpsertTask(ctx, db.Task{
		ID:           "task-7",
		RepoFullName: repo.FullName(),
		GoalID:       "goal-1",
		Title:        "Task 7",
		State:        string(workflow.TaskReadyToMerge),
		Branch:       "task-7",
	}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "builder",
		Role:           "builder",
		Runtime:        "codex",
		RuntimeRef:     "last",
		RepoScope:      repo.FullName(),
		Capabilities:   []string{"implement"},
		AutonomyPolicy: "workspace-write",
		HealthStatus:   "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent returned error: %v", err)
	}
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: repo.FullName(), Branch: "task-7", Owner: "builder"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: repo.FullName(),
		Number:       7,
		URL:          "https://github.com/gitmoot/gitmoot/pull/7",
		HeadBranch:   "task-7",
		BaseBranch:   "main",
		HeadSHA:      "abc123",
		State:        "open",
	}); err != nil {
		t.Fatalf("UpsertPullRequest returned error: %v", err)
	}
	client := &fakeGitHub{
		pulls: []github.PullRequest{{
			Number:  7,
			Title:   "Task 7",
			State:   "open",
			URL:     "https://github.com/gitmoot/gitmoot/pull/7",
			HeadRef: "task-7",
			BaseRef: "main",
			HeadSHA: "abc123",
		}},
		comments: map[int64][]github.IssueComment{7: {}},
	}
	gate := &fakeWorkflowMergeGate{decision: workflow.MergeDecision{Ready: true, Merged: true}}
	engine := workflow.Engine{Store: store, MergeGate: gate}
	daemon := Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}

	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce returned error: %v", err)
	}
	if len(gate.requests) != 1 || gate.requests[0].PullRequest != 7 || gate.requests[0].HeadSHA != "abc123" {
		t.Fatalf("merge gate requests = %+v", gate.requests)
	}
}

func TestPollOnceRetriesReadyToMergePullRequestDespiteConsecutiveChangedPolls(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	if err := store.UpsertTask(ctx, db.Task{
		ID:           "task-7",
		RepoFullName: repo.FullName(),
		GoalID:       "goal-1",
		Title:        "Task 7",
		State:        string(workflow.TaskReadyToMerge),
		Branch:       "task-7",
	}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: repo.FullName(),
		Number:       7,
		URL:          "https://github.com/gitmoot/gitmoot/pull/7",
		HeadBranch:   "task-7",
		BaseBranch:   "main",
		HeadSHA:      "current-head",
		State:        "open",
	}); err != nil {
		t.Fatalf("UpsertPullRequest returned error: %v", err)
	}
	payload, err := json.Marshal(workflow.JobPayload{
		Repo:        repo.FullName(),
		Branch:      "task-7",
		PullRequest: 7,
		HeadSHA:     "stale-head",
		TaskID:      "task-7",
		LeadAgent:   "lead",
		Reviewers:   []string{"reviewer"},
		ReviewRound: "review-1",
		Result:      &workflow.AgentResult{Decision: "approved"},
	})
	if err != nil {
		t.Fatalf("Marshal review payload returned error: %v", err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{
		ID:      "review-reviewer-task-7-review-1",
		Agent:   "reviewer",
		Type:    "review",
		State:   string(workflow.JobSucceeded),
		Payload: string(payload),
	}, db.JobEvent{Kind: string(workflow.JobSucceeded), Message: "review completed on stale head"}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
	pull := github.PullRequest{
		Number:  7,
		Title:   "Task 7",
		State:   "open",
		URL:     "https://github.com/gitmoot/gitmoot/pull/7",
		HeadRef: "task-7",
		BaseRef: "main",
		HeadSHA: "current-head",
	}
	client := &fakeGitHub{
		pulls:    []github.PullRequest{pull},
		comments: map[int64][]github.IssueComment{7: {}},
	}
	gate := &fakeWorkflowMergeGate{decision: workflow.MergeDecision{Ready: true}}
	engine := workflow.Engine{Store: store, MergeGate: gate}
	daemon := Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}

	for poll := 1; poll <= 2; poll++ {
		changed, err := daemon.pullRequestChanged(ctx, pull, newReviewJobsMemo(store))
		if err != nil {
			t.Fatalf("poll %d pullRequestChanged returned error: %v", poll, err)
		}
		if !changed {
			t.Fatalf("poll %d pullRequestChanged = false, want true from retained stale-head review", poll)
		}
		ready, err := daemon.pullRequestReadyToMerge(ctx, pull)
		if err != nil {
			t.Fatalf("poll %d pullRequestReadyToMerge returned error: %v", poll, err)
		}
		if !ready {
			t.Fatalf("poll %d pullRequestReadyToMerge = false, want true", poll)
		}
		if err := daemon.PollOnce(ctx); err != nil {
			t.Fatalf("poll %d PollOnce returned error: %v", poll, err)
		}
	}

	if len(gate.requests) != 2 {
		t.Fatalf("merge gate requests = %+v, want one for each changed poll", gate.requests)
	}
}

func TestPollOnceDismissedEscalationDoesNotBlockEligibleMerge(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	if err := store.UpsertTask(ctx, db.Task{
		ID:           "task-ready",
		RepoFullName: repo.FullName(),
		State:        string(workflow.TaskReadyToMerge),
		Branch:       "ready-branch",
	}); err != nil {
		t.Fatalf("UpsertTask ready: %v", err)
	}
	if err := store.UpsertTask(ctx, db.Task{
		ID:           "task-dismissed",
		RepoFullName: repo.FullName(),
		State:        string(workflow.TaskDismissed),
		Branch:       "dismissed-branch",
	}); err != nil {
		t.Fatalf("UpsertTask dismissed: %v", err)
	}
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{
		RepoFullName: repo.FullName(),
		Branch:       "ready-branch",
		Owner:        "lead",
	}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "coordinator",
		Role:           "coordinator",
		Runtime:        "codex",
		RuntimeRef:     "last",
		RepoScope:      repo.FullName(),
		Capabilities:   []string{"ask"},
		AutonomyPolicy: "auto",
		HealthStatus:   "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent coordinator: %v", err)
	}
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: repo.FullName(),
		Number:       7,
		HeadBranch:   "ready-branch",
		BaseBranch:   "main",
		HeadSHA:      "abc123",
		State:        "open",
	}); err != nil {
		t.Fatalf("UpsertPullRequest: %v", err)
	}
	payload, err := json.Marshal(workflow.JobPayload{
		Repo:      repo.FullName(),
		Branch:    "dismissed-branch",
		TaskID:    "task-dismissed",
		Result:    &workflow.AgentResult{Decision: "approved", Summary: "stale escalation"},
		LeadAgent: "coordinator",
	})
	if err != nil {
		t.Fatalf("Marshal payload: %v", err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{
		ID:      "dismissed-coordinator",
		Agent:   "coordinator",
		Type:    "ask",
		State:   string(workflow.JobSucceeded),
		Payload: string(payload),
	}, db.JobEvent{
		JobID:   "dismissed-coordinator",
		Kind:    string(workflow.JobSucceeded),
		Message: "completed",
	}); err != nil {
		t.Fatalf("CreateJobWithEvent: %v", err)
	}
	escalation, err := json.Marshal(workflow.EscalationRecord{
		DelegationID: "stale-leg",
		PausedAt:     time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339),
	})
	if err != nil {
		t.Fatalf("Marshal escalation: %v", err)
	}
	if err := store.AddJobEvent(ctx, db.JobEvent{
		JobID:   "dismissed-coordinator",
		Kind:    "delegation_escalation_requested",
		Message: string(escalation),
	}); err != nil {
		t.Fatalf("AddJobEvent escalation: %v", err)
	}

	client := &fakeGitHub{
		pulls: []github.PullRequest{{
			Number:  7,
			Title:   "Ready",
			State:   "open",
			HeadRef: "ready-branch",
			BaseRef: "main",
			HeadSHA: "abc123",
		}},
		comments: map[int64][]github.IssueComment{7: {}},
	}
	gate := &fakeWorkflowMergeGate{decision: workflow.MergeDecision{Ready: true, Merged: true}}
	engine := workflow.Engine{Store: store, MergeGate: gate}
	d := Daemon{
		Repo:          repo,
		Store:         store,
		GitHub:        client,
		Workflow:      &engine,
		EscalationTTL: time.Hour,
	}

	if err := d.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce returned error: %v", err)
	}
	if len(gate.requests) != 1 || gate.requests[0].PullRequest != 7 {
		t.Fatalf("merge gate requests = %+v, want eligible PR #7", gate.requests)
	}
	ready, err := store.GetTask(ctx, "task-ready")
	if err != nil || ready.State != string(workflow.TaskMerged) {
		t.Fatalf("ready task = %+v, err=%v; want merged", ready, err)
	}
	dismissed, err := store.GetTask(ctx, "task-dismissed")
	if err != nil || dismissed.State != string(workflow.TaskDismissed) {
		t.Fatalf("dismissed task = %+v, err=%v; want dismissed", dismissed, err)
	}
	if _, err := store.GetJob(ctx, workflow.DelegationContinuationID("dismissed-coordinator")); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("dismissed task continuation error = %v, want no row", err)
	}
}

func TestMergeGateSkippedFanoutStaysPendingWhileNamedCheckRuns(t *testing.T) {
	ctx := context.Background()
	store, client, daemon, gate := newSkippedFanoutPendingGateDaemon(t, workflow.TaskReadyToMerge)
	request := workflow.MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 7, TaskID: "task-7"}

	decision, err := gate.Evaluate(ctx, request)
	if err != nil {
		t.Fatalf("initial Evaluate returned error: %v", err)
	}
	if !decision.Ready || decision.Merged {
		t.Fatalf("initial decision = %+v", decision)
	}
	if len(client.statuses) != 1 || client.statuses[0].Context != "gitmoot/merge-gate" || client.statuses[0].State != "pending" {
		t.Fatalf("initial statuses = %+v", client.statuses)
	}

	for poll := 1; poll <= 2; poll++ {
		if err := daemon.PollOnce(ctx); err != nil {
			t.Fatalf("pending poll %d returned error: %v", poll, err)
		}
		status := client.statuses[len(client.statuses)-1]
		if status.Context != "gitmoot/merge-gate" || status.State != "pending" {
			t.Fatalf("pending poll %d status = %+v", poll, status)
		}
		if len(client.merges) != 0 {
			t.Fatalf("pending poll %d merge inputs = %+v", poll, client.merges)
		}
		task, err := store.GetTask(ctx, "task-7")
		if err != nil {
			t.Fatalf("pending poll %d GetTask returned error: %v", poll, err)
		}
		if task.State != string(workflow.TaskReadyToMerge) {
			t.Fatalf("pending poll %d task state = %q, want %q", poll, task.State, workflow.TaskReadyToMerge)
		}
	}
}

func TestPollOnceSkippedFanoutReevaluatesCompletedNamedCheck(t *testing.T) {
	ctx := context.Background()
	store, client, daemon, gate := newSkippedFanoutPendingGateDaemon(t, workflow.TaskReadyToMerge)
	request := workflow.MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 7, TaskID: "task-7"}

	decision, err := gate.Evaluate(ctx, request)
	if err != nil {
		t.Fatalf("initial Evaluate returned error: %v", err)
	}
	if !decision.Ready || decision.Merged {
		t.Fatalf("initial decision = %+v", decision)
	}
	if len(client.statuses) != 1 || client.statuses[0].State != "pending" {
		t.Fatalf("initial statuses = %+v", client.statuses)
	}

	client.checks[0].Bucket = "pass"
	client.checks[0].State = "COMPLETED"
	client.checks[0].CompletedAt = "2026-08-31T03:20:15Z"
	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("completed poll returned error: %v", err)
	}
	if len(client.statuses) != 2 {
		t.Fatalf("completed poll statuses = %+v", client.statuses)
	}
	status := client.statuses[len(client.statuses)-1]
	if status.Context != "gitmoot/merge-gate" || status.State != "success" {
		t.Fatalf("completed poll status = %+v", status)
	}
	if len(client.merges) != 1 {
		t.Fatalf("completed poll merge inputs = %+v", client.merges)
	}
	task, err := store.GetTask(ctx, "task-7")
	if err != nil {
		t.Fatalf("completed poll GetTask returned error: %v", err)
	}
	if task.State != string(workflow.TaskMerged) {
		t.Fatalf("completed poll task state = %q, want %q", task.State, workflow.TaskMerged)
	}
}

func TestPollOnceSkippedFanoutRetriesAfterPendingStatusWriteFailure(t *testing.T) {
	ctx := context.Background()
	store, client, daemon, _ := newSkippedFanoutPendingGateDaemon(t, workflow.TaskPullRequestOpen)
	client.statusErrs = []error{errors.New("status bookkeeping unavailable")}

	if err := daemon.Workflow.AdvanceJob(ctx, "review-job"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	if len(client.statuses) != 1 || client.statuses[0].State != "pending" {
		t.Fatalf("AdvanceJob statuses = %+v", client.statuses)
	}
	task, err := store.GetTask(ctx, "task-7")
	if err != nil {
		t.Fatalf("AdvanceJob GetTask returned error: %v", err)
	}
	if task.State != string(workflow.TaskReadyToMerge) {
		t.Fatalf("AdvanceJob task state = %q, want %q", task.State, workflow.TaskReadyToMerge)
	}

	client.checks[0].Bucket = "pass"
	client.checks[0].State = "COMPLETED"
	client.checks[0].CompletedAt = "2026-08-31T03:20:15Z"
	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("retry PollOnce returned error: %v", err)
	}
	if len(client.statuses) != 2 || client.statuses[1].State != "success" {
		t.Fatalf("retry PollOnce statuses = %+v", client.statuses)
	}
	if len(client.merges) != 1 {
		t.Fatalf("retry PollOnce merge inputs = %+v", client.merges)
	}
}

func newSkippedFanoutPendingGateDaemon(t *testing.T, initialState workflow.TaskState) (*db.Store, *mergeGateRaceGitHub, Daemon, *workflow.PolicyMergeGate) {
	t.Helper()
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	if err := store.UpsertTask(ctx, db.Task{
		ID:           "task-7",
		RepoFullName: repo.FullName(),
		GoalID:       "goal-1",
		Title:        "Task 7",
		State:        string(initialState),
		Branch:       "task-7",
	}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "lead",
		Role:           "implementer",
		Runtime:        "codex",
		RuntimeRef:     "last",
		RepoScope:      repo.FullName(),
		Capabilities:   []string{"implement"},
		AutonomyPolicy: "auto",
		HealthStatus:   "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent returned error: %v", err)
	}
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{
		RepoFullName: repo.FullName(),
		Branch:       "task-7",
		Owner:        "lead",
	}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	if err := store.SetBranchLockReviewFanout(ctx, repo.FullName(), "task-7", true); err != nil {
		t.Fatalf("SetBranchLockReviewFanout returned error: %v", err)
	}
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: repo.FullName(),
		Number:       7,
		URL:          "https://github.com/gitmoot/gitmoot/pull/7",
		HeadBranch:   "task-7",
		BaseBranch:   "main",
		HeadSHA:      "abc123",
		State:        "open",
	}); err != nil {
		t.Fatalf("UpsertPullRequest returned error: %v", err)
	}
	insertCompleted := func(job db.Job, payload workflow.JobPayload) {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("json.Marshal returned error: %v", err)
		}
		job.State = string(workflow.JobSucceeded)
		job.Payload = string(encoded)
		if err := store.CreateJobWithEvent(ctx, job, db.JobEvent{
			Kind:    string(workflow.JobSucceeded),
			Message: "done",
		}); err != nil {
			t.Fatalf("CreateJobWithEvent returned error: %v", err)
		}
	}
	insertCompleted(db.Job{ID: "implement-job", Agent: "lead", Type: "implement"}, workflow.JobPayload{
		Repo:        repo.FullName(),
		PullRequest: 7,
		HeadSHA:     "abc123",
		TaskID:      "task-7",
		Result:      &workflow.AgentResult{Decision: "implemented", Summary: "implemented"},
	})
	insertCompleted(db.Job{ID: "review-job", Agent: "audit", Type: "review"}, workflow.JobPayload{
		Repo:        repo.FullName(),
		PullRequest: 7,
		HeadSHA:     "abc123",
		TaskID:      "task-7",
		ReviewRound: "review-1",
		Result:      &workflow.AgentResult{Decision: "approved", Summary: "ready"},
	})
	mergeable := true
	baseClient := &fakeGitHub{
		pulls: []github.PullRequest{{
			Number:    7,
			Title:     "Task 7",
			State:     "open",
			URL:       "https://github.com/gitmoot/gitmoot/pull/7",
			HeadRef:   "task-7",
			BaseRef:   "main",
			HeadSHA:   "abc123",
			Mergeable: &mergeable,
		}},
		comments: map[int64][]github.IssueComment{7: {}},
	}
	client := &mergeGateRaceGitHub{
		fakeGitHub: baseClient,
		checks: []github.PullRequestCheck{{
			Name:   "shard 4 / test",
			Bucket: "pending",
			State:  "IN_PROGRESS",
		}},
	}
	gate := &workflow.PolicyMergeGate{AutoMerge: true, Store: store, GitHub: client}
	engine := workflow.Engine{Store: store, MergeGate: gate}
	daemon := Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}
	return store, client, daemon, gate
}

type mergeGateRaceGitHub struct {
	*fakeGitHub
	checks     []github.PullRequestCheck
	statuses   []github.CommitStatusInput
	statusErrs []error
	merges     []github.MergePullRequestInput
}

func (f *mergeGateRaceGitHub) GetCombinedStatus(context.Context, github.Repository, string) (github.CombinedStatus, error) {
	return github.CombinedStatus{State: "success"}, nil
}

func (f *mergeGateRaceGitHub) ListCheckRunsForRef(context.Context, github.Repository, string) ([]github.PullRequestCheck, error) {
	return append([]github.PullRequestCheck(nil), f.checks...), nil
}

func (f *mergeGateRaceGitHub) CreateCommitStatus(_ context.Context, input github.CommitStatusInput) (github.CommitStatus, error) {
	f.statuses = append(f.statuses, input)
	var err error
	if len(f.statusErrs) > 0 {
		err = f.statusErrs[0]
		f.statusErrs = f.statusErrs[1:]
	}
	return github.CommitStatus{Context: input.Context, State: input.State}, err
}

func (f *mergeGateRaceGitHub) CompareCommits(context.Context, github.Repository, string, string) (github.CompareResult, error) {
	return github.CompareResult{Status: "ahead"}, nil
}

func (f *mergeGateRaceGitHub) MergePullRequest(_ context.Context, input github.MergePullRequestInput) (github.MergeResult, error) {
	f.merges = append(f.merges, input)
	return github.MergeResult{Merged: true, SHA: "merge123"}, nil
}

func TestPollOnceRetriesReadyToMergePullRequestAfterBranchUpdateHeadChange(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	if err := store.UpsertTask(ctx, db.Task{
		ID:           "task-7",
		RepoFullName: repo.FullName(),
		GoalID:       "goal-1",
		Title:        "Task 7",
		State:        string(workflow.TaskReadyToMerge),
		Branch:       "task-7",
	}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "builder",
		Role:           "builder",
		Runtime:        "codex",
		RuntimeRef:     "last",
		RepoScope:      repo.FullName(),
		Capabilities:   []string{"implement"},
		AutonomyPolicy: "workspace-write",
		HealthStatus:   "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent returned error: %v", err)
	}
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: repo.FullName(), Branch: "task-7", Owner: "builder"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: repo.FullName(),
		Number:       7,
		URL:          "https://github.com/gitmoot/gitmoot/pull/7",
		HeadBranch:   "task-7",
		BaseBranch:   "main",
		HeadSHA:      "old123",
		State:        "open",
	}); err != nil {
		t.Fatalf("UpsertPullRequest returned error: %v", err)
	}
	client := &fakeGitHub{
		pulls: []github.PullRequest{{
			Number:  7,
			Title:   "Task 7",
			State:   "open",
			URL:     "https://github.com/gitmoot/gitmoot/pull/7",
			HeadRef: "task-7",
			BaseRef: "main",
			HeadSHA: "old123",
		}},
		comments: map[int64][]github.IssueComment{7: {}},
	}
	gate := &fakeWorkflowMergeGate{decision: workflow.MergeDecision{Ready: true, Reason: workflow.PlainReason("branch update requested")}}
	engine := workflow.Engine{Store: store, MergeGate: gate}
	daemon := Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}

	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("first PollOnce returned error: %v", err)
	}
	client.pulls[0].HeadSHA = "new456"
	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("second PollOnce returned error: %v", err)
	}

	if len(gate.requests) != 2 {
		t.Fatalf("merge gate requests = %+v, want 2", gate.requests)
	}
	if gate.requests[0].HeadSHA != "old123" || gate.requests[1].HeadSHA != "new456" {
		t.Fatalf("merge gate request heads = %q, %q; want old123 then new456", gate.requests[0].HeadSHA, gate.requests[1].HeadSHA)
	}
}

func TestPollOnceRetriesClosedReadyToMergePullRequest(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	if err := store.UpsertTask(ctx, db.Task{
		ID:           "task-7",
		RepoFullName: repo.FullName(),
		GoalID:       "goal-1",
		Title:        "Task 7",
		State:        string(workflow.TaskReadyToMerge),
		Branch:       "task-7",
	}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: repo.FullName(), Branch: "task-7", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: repo.FullName(),
		Number:       7,
		URL:          "https://github.com/gitmoot/gitmoot/pull/7",
		HeadBranch:   "task-7",
		BaseBranch:   "main",
		HeadSHA:      "abc123",
		State:        "open",
	}); err != nil {
		t.Fatalf("UpsertPullRequest returned error: %v", err)
	}
	client := &fakeGitHub{
		pullsByState: map[string][]github.PullRequest{
			"open": {},
			"closed": {{
				Number:  6,
				Title:   "Task 7 old",
				State:   "closed",
				Merged:  true,
				URL:     "https://github.com/gitmoot/gitmoot/pull/6",
				HeadRef: "task-7",
				BaseRef: "main",
				HeadSHA: "old123",
			}, {
				Number:  7,
				Title:   "Task 7",
				State:   "closed",
				Merged:  true,
				URL:     "https://github.com/gitmoot/gitmoot/pull/7",
				HeadRef: "task-7",
				BaseRef: "main",
				HeadSHA: "abc123",
			}},
		},
		comments: map[int64][]github.IssueComment{},
	}
	gate := &fakeWorkflowMergeGate{decision: workflow.MergeDecision{Ready: true, Merged: true}}
	engine := workflow.Engine{Store: store, MergeGate: gate}
	daemon := Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}

	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce returned error: %v", err)
	}
	if len(gate.requests) != 1 || gate.requests[0].PullRequest != 7 || gate.requests[0].HeadSHA != "abc123" {
		t.Fatalf("merge gate requests = %+v", gate.requests)
	}
	stored, err := store.GetPullRequest(ctx, repo.FullName(), 7)
	if err != nil || stored.State != "merged" {
		t.Fatalf("stored pull request = %+v, err=%v; want merged", stored, err)
	}
}

func TestPollOnceDoesNotOverwriteNoReviewerAutoMerge(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "lead",
		Role:           "lead",
		Runtime:        "codex",
		RuntimeRef:     "last",
		RepoScope:      repo.FullName(),
		Capabilities:   []string{"implement"},
		AutonomyPolicy: "workspace-write",
		HealthStatus:   "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent lead returned error: %v", err)
	}
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: repo.FullName(), Branch: "task-7", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	client := &fakeGitHub{
		pulls: []github.PullRequest{{
			Number:  7,
			Title:   "Task 7",
			State:   "open",
			URL:     "https://github.com/gitmoot/gitmoot/pull/7",
			HeadRef: "task-7",
			BaseRef: "main",
			HeadSHA: "abc123",
		}},
		comments: map[int64][]github.IssueComment{7: {}},
	}
	gate := &fakeWorkflowMergeGate{
		decision: workflow.MergeDecision{Ready: true, Merged: true, MergeCommitSHA: "merge123"},
		onEvaluate: func(request workflow.MergeRequest) {
			if err := store.UpsertPullRequest(ctx, db.PullRequest{
				RepoFullName:   request.Repo,
				Number:         int64(request.PullRequest),
				URL:            "https://github.com/gitmoot/gitmoot/pull/7",
				HeadBranch:     request.Branch,
				BaseBranch:     "main",
				HeadSHA:        request.HeadSHA,
				MergeCommitSHA: "merge123",
				State:          "merged",
			}); err != nil {
				t.Fatalf("UpsertPullRequest returned error: %v", err)
			}
		},
	}
	engine := workflow.Engine{Store: store, MergeGate: gate}
	daemon := Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}

	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce returned error: %v", err)
	}
	pr, err := store.GetPullRequest(ctx, repo.FullName(), 7)
	if err != nil {
		t.Fatalf("GetPullRequest returned error: %v", err)
	}
	if pr.State != "merged" || pr.MergeCommitSHA != "merge123" {
		t.Fatalf("stored pull request = %+v", pr)
	}
}

func TestPollOnceRoutesPullRequestWithEmptyStoredHeadSHA(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "lead",
		Role:           "lead",
		Runtime:        "codex",
		RuntimeRef:     "last",
		RepoScope:      repo.FullName(),
		Capabilities:   []string{"implement"},
		AutonomyPolicy: "workspace-write",
		HealthStatus:   "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent lead returned error: %v", err)
	}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "audit",
		Role:           "reviewer",
		Runtime:        "codex",
		RuntimeRef:     "last",
		RepoScope:      repo.FullName(),
		Capabilities:   []string{"review"},
		AutonomyPolicy: "auto",
		HealthStatus:   "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent audit returned error: %v", err)
	}
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: repo.FullName(), Branch: "task-7", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: repo.FullName(),
		Number:       7,
		HeadBranch:   "task-7",
		State:        "open",
	}); err != nil {
		t.Fatalf("UpsertPullRequest returned error: %v", err)
	}
	client := &fakeGitHub{
		pulls: []github.PullRequest{{
			Number:  7,
			Title:   "Task 7",
			State:   "open",
			URL:     "https://github.com/gitmoot/gitmoot/pull/7",
			HeadRef: "task-7",
			BaseRef: "main",
			HeadSHA: "abc123",
		}},
		comments: map[int64][]github.IssueComment{7: {}},
	}
	engine := workflow.Engine{
		Store: store,
		JobID: func(request workflow.JobRequest) string {
			parts := []string{request.Action, request.Agent, request.TaskID}
			if request.ReviewRound != "" {
				parts = append(parts, request.ReviewRound)
			}
			return strings.Join(parts, "-")
		},
	}
	daemon := Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}

	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce returned error: %v", err)
	}
	if _, err := store.GetJob(ctx, "review-audit-task-7-review-1"); err != nil {
		t.Fatalf("GetJob review round returned error: %v", err)
	}
	if pr, err := store.GetPullRequest(ctx, repo.FullName(), 7); err != nil || pr.HeadSHA != "abc123" {
		t.Fatalf("stored pull request = %+v err=%v", pr, err)
	}
}

func TestPollOnceDoesNotTreatManualReviewJobAsWorkflowRoute(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "lead",
		Role:           "lead",
		Runtime:        "codex",
		RuntimeRef:     "last",
		RepoScope:      repo.FullName(),
		Capabilities:   []string{"implement"},
		AutonomyPolicy: "workspace-write",
		HealthStatus:   "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent lead returned error: %v", err)
	}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "audit",
		Role:           "reviewer",
		Runtime:        "codex",
		RuntimeRef:     "last",
		RepoScope:      repo.FullName(),
		Capabilities:   []string{"review"},
		AutonomyPolicy: "auto",
		HealthStatus:   "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent audit returned error: %v", err)
	}
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: repo.FullName(), Branch: "task-7", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	manualPayload, err := json.Marshal(workflow.JobPayload{
		Repo:        repo.FullName(),
		Branch:      "task-7",
		PullRequest: 7,
		TaskID:      "task-7",
		TaskTitle:   "Task 7",
		Sender:      "alice",
	})
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{
		ID:      "manual-review-job",
		Agent:   "audit",
		Type:    "review",
		State:   string(workflow.JobQueued),
		Payload: string(manualPayload),
	}, db.JobEvent{Kind: string(workflow.JobQueued), Message: "manual review"}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: repo.FullName(),
		Number:       7,
		HeadBranch:   "task-7",
		State:        "open",
	}); err != nil {
		t.Fatalf("UpsertPullRequest returned error: %v", err)
	}
	client := &fakeGitHub{
		pulls: []github.PullRequest{{
			Number:  7,
			Title:   "Task 7",
			State:   "open",
			URL:     "https://github.com/gitmoot/gitmoot/pull/7",
			HeadRef: "task-7",
			BaseRef: "main",
			HeadSHA: "abc123",
		}},
		comments: map[int64][]github.IssueComment{7: {}},
	}
	engine := workflow.Engine{
		Store: store,
		JobID: func(request workflow.JobRequest) string {
			parts := []string{request.Action, request.Agent, request.TaskID}
			if request.ReviewRound != "" {
				parts = append(parts, request.ReviewRound)
			}
			return strings.Join(parts, "-")
		},
	}
	daemon := Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}

	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce returned error: %v", err)
	}
	if _, err := store.GetJob(ctx, "review-audit-task-7-review-2"); err != nil {
		t.Fatalf("GetJob workflow review round returned error: %v", err)
	}
	if pr, err := store.GetPullRequest(ctx, repo.FullName(), 7); err != nil || pr.HeadSHA != "abc123" {
		t.Fatalf("stored pull request = %+v err=%v", pr, err)
	}
}

func TestPollOnceDedupesSeenComments(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "audit",
		Role:           "reviewer",
		Runtime:        "codex",
		RuntimeRef:     "last",
		RepoScope:      repo.FullName(),
		Capabilities:   []string{"review"},
		AutonomyPolicy: "auto",
		HealthStatus:   "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent returned error: %v", err)
	}
	client := &fakeGitHub{
		pulls: []github.PullRequest{{Number: 3, Title: "Task 3", State: "open", HeadRef: "task-3", BaseRef: "main"}},
		comments: map[int64][]github.IssueComment{
			3: {{ID: 202, Body: "/gitmoot audit review", Author: "bob"}},
		},
	}
	daemon := Daemon{Repo: repo, Store: store, GitHub: client}

	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("first PollOnce returned error: %v", err)
	}
	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("second PollOnce returned error: %v", err)
	}
	if len(client.posted) != 1 {
		t.Fatalf("posted acknowledgements = %+v, want one after duplicate poll", client.posted)
	}
}

func TestPollOnceQueuesRepeatedCommandsInOneComment(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "audit",
		Role:           "reviewer",
		Runtime:        "codex",
		RuntimeRef:     "last",
		RepoScope:      repo.FullName(),
		Capabilities:   []string{"review"},
		AutonomyPolicy: "auto",
		HealthStatus:   "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent returned error: %v", err)
	}
	client := &fakeGitHub{
		pulls: []github.PullRequest{{Number: 6, Title: "Task 6", State: "open", HeadRef: "task-6", BaseRef: "main"}},
		comments: map[int64][]github.IssueComment{
			6: {{ID: 505, Body: "/gitmoot audit review first\n/gitmoot audit review second", Author: "erin"}},
		},
	}

	err := (Daemon{Repo: repo, Store: store, GitHub: client}).PollOnce(ctx)

	if err != nil {
		t.Fatalf("PollOnce returned error: %v", err)
	}
	if len(client.posted) != 2 {
		t.Fatalf("posted acknowledgements = %+v, want 2", client.posted)
	}
	for sequence := 0; sequence < 2; sequence++ {
		if _, err := store.GetJob(ctx, jobID(repo, 6, 505, sequence, "audit", "review")); err != nil {
			t.Fatalf("GetJob for sequence %d returned error: %v", sequence, err)
		}
	}
}

func TestPollOnceAcknowledgesUnknownAgentWithoutJob(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	client := &fakeGitHub{
		pulls: []github.PullRequest{{Number: 4, Title: "Task 4", State: "open", HeadRef: "task-4", BaseRef: "main"}},
		comments: map[int64][]github.IssueComment{
			4: {{ID: 303, Body: "/gitmoot missing review", Author: "carol"}},
		},
	}

	err := (Daemon{Repo: repo, Store: store, GitHub: client}).PollOnce(ctx)

	if err != nil {
		t.Fatalf("PollOnce returned error: %v", err)
	}
	if len(client.posted) != 1 || !strings.Contains(client.posted[0].body, "could not find subscribed agent `missing`") {
		t.Fatalf("posted acknowledgements = %+v", client.posted)
	}
	if _, err := store.GetJob(ctx, jobID(repo, 4, 303, 0, "missing", "review")); err == nil {
		t.Fatal("unknown agent created a job")
	}
}

func TestPollOnceRejectsUnauthorizedCommenter(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "audit",
		Role:           "reviewer",
		Runtime:        "codex",
		RuntimeRef:     "last",
		RepoScope:      repo.FullName(),
		Capabilities:   []string{"review"},
		AutonomyPolicy: "auto",
		HealthStatus:   "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent returned error: %v", err)
	}
	client := &fakeGitHub{
		permissions: map[string]string{"mallory": "read"},
		pulls:       []github.PullRequest{{Number: 8, Title: "Task 8", State: "open", HeadRef: "task-8", BaseRef: "main"}},
		comments: map[int64][]github.IssueComment{
			8: {{ID: 606, Body: "/gitmoot audit review", Author: "mallory"}},
		},
	}

	err := (Daemon{Repo: repo, Store: store, GitHub: client}).PollOnce(ctx)

	if err != nil {
		t.Fatalf("PollOnce returned error: %v", err)
	}
	if len(client.posted) != 1 || !strings.Contains(client.posted[0].body, "ignored comment 606") {
		t.Fatalf("posted acknowledgements = %+v", client.posted)
	}
	if _, err := store.GetJob(ctx, jobID(repo, 8, 606, 0, "audit", "review")); err == nil {
		t.Fatal("unauthorized commenter created a job")
	}
	seen, err := store.HasCommentSeen(ctx, repo.FullName(), 606)
	if err != nil {
		t.Fatalf("HasCommentSeen returned error: %v", err)
	}
	if !seen {
		t.Fatal("unauthorized command was not marked seen after acknowledgement")
	}
}

func TestPollOnceAcknowledgesMissingCapabilityWithoutJob(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "builder",
		Role:           "builder",
		Runtime:        "codex",
		RuntimeRef:     "last",
		RepoScope:      repo.FullName(),
		Capabilities:   []string{"implement"},
		AutonomyPolicy: "workspace-write",
		HealthStatus:   "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent returned error: %v", err)
	}
	client := &fakeGitHub{
		pulls: []github.PullRequest{{Number: 5, Title: "Task 5", State: "open", HeadRef: "task-5", BaseRef: "main"}},
		comments: map[int64][]github.IssueComment{
			5: {{ID: 404, Body: "/gitmoot builder review", Author: "dana"}},
		},
	}

	err := (Daemon{Repo: repo, Store: store, GitHub: client}).PollOnce(ctx)

	if err != nil {
		t.Fatalf("PollOnce returned error: %v", err)
	}
	if len(client.posted) != 1 || !strings.Contains(client.posted[0].body, "does not advertise `review` capability") {
		t.Fatalf("posted acknowledgements = %+v", client.posted)
	}
}

func TestPollOnceRejectsImplementWithoutBranchLock(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "builder",
		Role:           "builder",
		Runtime:        "codex",
		RuntimeRef:     "last",
		RepoScope:      repo.FullName(),
		Capabilities:   []string{"implement"},
		AutonomyPolicy: "workspace-write",
		HealthStatus:   "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent returned error: %v", err)
	}
	client := &fakeGitHub{
		pulls: []github.PullRequest{{Number: 10, Title: "Task 10", State: "open", HeadRef: "task-10", BaseRef: "main"}},
		comments: map[int64][]github.IssueComment{
			10: {{ID: 808, Body: "/gitmoot builder implement", Author: "dana"}},
		},
	}

	err := (Daemon{Repo: repo, Store: store, GitHub: client}).PollOnce(ctx)

	if err != nil {
		t.Fatalf("PollOnce returned error: %v", err)
	}
	if len(client.posted) != 1 || !strings.Contains(client.posted[0].body, "without holding the branch lock") {
		t.Fatalf("posted acknowledgements = %+v", client.posted)
	}
	if _, err := store.GetJob(ctx, jobID(repo, 10, 808, 0, "builder", "implement")); err == nil {
		t.Fatal("implement job was created without a branch lock")
	}
}

func TestPollOnceReportsStatusCommand(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	if err := store.UpsertTask(ctx, db.Task{
		ID:           "task-010",
		RepoFullName: repo.FullName(),
		GoalID:       "goal-1",
		Title:        "Task 10",
		State:        string(workflow.TaskReviewing),
		Branch:       "task-10",
	}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: repo.FullName(), Branch: "task-10", Owner: "builder"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	payload, err := json.Marshal(workflow.JobPayload{
		Repo:        repo.FullName(),
		Branch:      "task-10",
		PullRequest: 10,
		TaskID:      "task-010",
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if err := store.CreateJob(ctx, db.Job{ID: "job-1", Agent: "audit", Type: "review", State: string(workflow.JobQueued), Payload: string(payload)}); err != nil {
		t.Fatalf("CreateJob returned error: %v", err)
	}
	if err := store.UpsertMergeGate(ctx, db.MergeGate{RepoFullName: repo.FullName(), PullRequest: 10, State: "pending", Reason: "ci pending"}); err != nil {
		t.Fatalf("UpsertMergeGate returned error: %v", err)
	}
	client := &fakeGitHub{
		pulls: []github.PullRequest{{Number: 10, Title: "Task 10", State: "open", HeadRef: "task-10", BaseRef: "main", HeadSHA: "abc123"}},
		comments: map[int64][]github.IssueComment{
			10: {{ID: 909, Body: "/gitmoot status", Author: "dana"}},
		},
	}

	err = (Daemon{Repo: repo, Store: store, GitHub: client}).PollOnce(ctx)

	if err != nil {
		t.Fatalf("PollOnce returned error: %v", err)
	}
	if len(client.posted) != 1 {
		t.Fatalf("posted acknowledgements = %+v, want 1", client.posted)
	}
	body := client.posted[0].body
	for _, want := range []string{"Gitmoot status for PR #10", "task: `task-010` `reviewing`", "branch_lock: `builder`", "queued=1", "merge_gate: `pending` ci pending"} {
		if !strings.Contains(body, want) {
			t.Fatalf("status body missing %q:\n%s", want, body)
		}
	}
}

func TestPollOnceReportsHelpCommand(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "audit",
		Role:           "reviewer",
		Runtime:        "codex",
		RuntimeRef:     "last",
		RepoScope:      repo.FullName(),
		Capabilities:   []string{"review", "ask"},
		AutonomyPolicy: "auto",
		HealthStatus:   "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent returned error: %v", err)
	}
	client := &fakeGitHub{
		pulls: []github.PullRequest{{
			Number:  10,
			Title:   "Task 10",
			State:   "open",
			URL:     "https://github.com/gitmoot/gitmoot/pull/10",
			HeadRef: "task-10",
			BaseRef: "main",
			HeadSHA: "abc123",
		}},
		comments: map[int64][]github.IssueComment{
			10: {{ID: 709, Body: "/gitmoot help", Author: "alice"}},
		},
	}

	if err := (Daemon{Repo: repo, Store: store, GitHub: client}).PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce returned error: %v", err)
	}
	if len(client.posted) != 1 {
		t.Fatalf("posted = %+v, want one help comment", client.posted)
	}
	for _, want := range []string{"Gitmoot help for `gitmoot/gitmoot` PR #10", "`audit`: review,ask", "/gitmoot <agent> <review|implement|ask>"} {
		if !strings.Contains(client.posted[0].body, want) {
			t.Fatalf("help output missing %q:\n%s", want, client.posted[0].body)
		}
	}
}

func TestPollOnceRetriesJobFromComment(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	payload, err := json.Marshal(workflow.JobPayload{
		Repo:        repo.FullName(),
		Branch:      "task-10",
		PullRequest: 10,
		TaskID:      "task-010",
		RawOutputs:  []string{"raw"},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{ID: "job-retry", Agent: "audit", Type: "review", State: string(workflow.JobFailed), Payload: string(payload)}, db.JobEvent{
		Kind:    string(workflow.JobFailed),
		Message: "failed",
	}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
	client := &fakeGitHub{
		pulls: []github.PullRequest{{Number: 10, Title: "Task 10", State: "open", HeadRef: "task-10", BaseRef: "main", HeadSHA: "abc123"}},
		comments: map[int64][]github.IssueComment{
			10: {{ID: 920, Body: "/gitmoot retry job-retry", Author: "dana"}},
		},
	}

	err = (Daemon{Repo: repo, Store: store, GitHub: client}).PollOnce(ctx)

	if err != nil {
		t.Fatalf("PollOnce returned error: %v", err)
	}
	job, err := store.GetJob(ctx, "job-retry")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobQueued) || !strings.Contains(job.Payload, `"raw_outputs":["raw"]`) {
		t.Fatalf("job after retry = %+v", job)
	}
	if len(client.posted) != 1 || !strings.Contains(client.posted[0].body, "queued retry for job `job-retry`") {
		t.Fatalf("posted acknowledgements = %+v", client.posted)
	}
}

func TestPollOnceCancelsJobFromComment(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	payload, err := json.Marshal(workflow.JobPayload{
		Repo:        repo.FullName(),
		Branch:      "task-10",
		PullRequest: 10,
		TaskID:      "task-010",
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{ID: "job-cancel", Agent: "audit", Type: "review", State: string(workflow.JobRunning), Payload: string(payload)}, db.JobEvent{
		Kind:    string(workflow.JobRunning),
		Message: "running",
	}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
	client := &fakeGitHub{
		pulls: []github.PullRequest{{Number: 10, Title: "Task 10", State: "open", HeadRef: "task-10", BaseRef: "main", HeadSHA: "abc123"}},
		comments: map[int64][]github.IssueComment{
			10: {{ID: 921, Body: "/gitmoot cancel job-cancel", Author: "dana"}},
		},
	}

	err = (Daemon{Repo: repo, Store: store, GitHub: client}).PollOnce(ctx)

	if err != nil {
		t.Fatalf("PollOnce returned error: %v", err)
	}
	job, err := store.GetJob(ctx, "job-cancel")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobCancelled) {
		t.Fatalf("job state = %q, want cancelled", job.State)
	}
	if len(client.posted) != 1 || !strings.Contains(client.posted[0].body, "cancelled job `job-cancel`") {
		t.Fatalf("posted acknowledgements = %+v", client.posted)
	}
}

func TestPollRecoveryCommandsOnceOnlyHandlesJobRecovery(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	payload, err := json.Marshal(workflow.JobPayload{
		Repo:        repo.FullName(),
		Branch:      "task-10",
		PullRequest: 10,
		TaskID:      "task-010",
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{ID: "job-cancel", Agent: "audit", Type: "review", State: string(workflow.JobRunning), Payload: string(payload)}, db.JobEvent{
		Kind:    string(workflow.JobRunning),
		Message: "running",
	}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "audit",
		Role:           "reviewer",
		Runtime:        "codex",
		RuntimeRef:     "last",
		RepoScope:      repo.FullName(),
		Capabilities:   []string{"review"},
		AutonomyPolicy: "auto",
		HealthStatus:   "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent returned error: %v", err)
	}
	client := &fakeGitHub{
		pulls: []github.PullRequest{{Number: 10, Title: "Task 10", State: "open", HeadRef: "task-10", BaseRef: "main", HeadSHA: "abc123"}},
		comments: map[int64][]github.IssueComment{
			10: {
				{ID: 921, Body: "/gitmoot cancel job-cancel", Author: "dana"},
				{ID: 922, Body: "/gitmoot audit review later", Author: "dana"},
			},
		},
	}

	err = (Daemon{Repo: repo, Store: store, GitHub: client}).PollRecoveryCommandsOnce(ctx)

	if err != nil {
		t.Fatalf("PollRecoveryCommandsOnce returned error: %v", err)
	}
	job, err := store.GetJob(ctx, "job-cancel")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobCancelled) {
		t.Fatalf("job state = %q, want cancelled", job.State)
	}
	if len(client.posted) != 1 || !strings.Contains(client.posted[0].body, "cancelled job `job-cancel`") {
		t.Fatalf("posted acknowledgements = %+v", client.posted)
	}
	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("jobs = %+v, want only recovery target", jobs)
	}
}

func TestPollOnceDoesNotRetryRunningJobCancelledInSameComment(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	payload, err := json.Marshal(workflow.JobPayload{
		Repo:        repo.FullName(),
		Branch:      "task-10",
		PullRequest: 10,
		TaskID:      "task-010",
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{ID: "job-cancel-retry", Agent: "audit", Type: "review", State: string(workflow.JobRunning), Payload: string(payload)}, db.JobEvent{
		Kind:    string(workflow.JobRunning),
		Message: "running",
	}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
	client := &fakeGitHub{
		pulls: []github.PullRequest{{Number: 10, Title: "Task 10", State: "open", HeadRef: "task-10", BaseRef: "main", HeadSHA: "abc123"}},
		comments: map[int64][]github.IssueComment{
			10: {{ID: 923, Body: "/gitmoot cancel job-cancel-retry\n/gitmoot retry job-cancel-retry", Author: "dana"}},
		},
	}

	err = (Daemon{Repo: repo, Store: store, GitHub: client}).PollOnce(ctx)

	if err != nil {
		t.Fatalf("PollOnce returned error: %v", err)
	}
	job, err := store.GetJob(ctx, "job-cancel-retry")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobCancelled) {
		t.Fatalf("job state = %q, want cancelled", job.State)
	}
	if len(client.posted) != 2 || !strings.Contains(client.posted[1].body, "wait for the active worker to settle") {
		t.Fatalf("posted acknowledgements = %+v", client.posted)
	}
}

func TestPollOnceRejectsJobRecoveryForDifferentPullRequest(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	payload, err := json.Marshal(workflow.JobPayload{
		Repo:        repo.FullName(),
		Branch:      "task-11",
		PullRequest: 11,
		TaskID:      "task-011",
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{ID: "job-other", Agent: "audit", Type: "review", State: string(workflow.JobFailed), Payload: string(payload)}, db.JobEvent{
		Kind:    string(workflow.JobFailed),
		Message: "failed",
	}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
	client := &fakeGitHub{
		pulls: []github.PullRequest{{Number: 10, Title: "Task 10", State: "open", HeadRef: "task-10", BaseRef: "main", HeadSHA: "abc123"}},
		comments: map[int64][]github.IssueComment{
			10: {{ID: 922, Body: "/gitmoot retry job-other", Author: "dana"}},
		},
	}

	err = (Daemon{Repo: repo, Store: store, GitHub: client}).PollOnce(ctx)

	if err != nil {
		t.Fatalf("PollOnce returned error: %v", err)
	}
	job, err := store.GetJob(ctx, "job-other")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobFailed) {
		t.Fatalf("job state = %q, want failed", job.State)
	}
	if len(client.posted) != 1 || !strings.Contains(client.posted[0].body, "belongs to gitmoot/gitmoot PR #11") {
		t.Fatalf("posted acknowledgements = %+v", client.posted)
	}
}

func TestPollOnceReportsStatusCommandCountsUnregisteredPRJobs(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	payload, err := json.Marshal(workflow.JobPayload{
		Repo:        repo.FullName(),
		Branch:      "task-10",
		PullRequest: 10,
		TaskID:      "pr-10-comment-111",
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if err := store.CreateJob(ctx, db.Job{ID: "job-1", Agent: "audit", Type: "review", State: string(workflow.JobQueued), Payload: string(payload)}); err != nil {
		t.Fatalf("CreateJob returned error: %v", err)
	}
	client := &fakeGitHub{
		pulls: []github.PullRequest{{Number: 10, Title: "Task 10", State: "open", HeadRef: "task-10", BaseRef: "main", HeadSHA: "abc123"}},
		comments: map[int64][]github.IssueComment{
			10: {{ID: 909, Body: "/gitmoot status", Author: "dana"}},
		},
	}

	err = (Daemon{Repo: repo, Store: store, GitHub: client}).PollOnce(ctx)

	if err != nil {
		t.Fatalf("PollOnce returned error: %v", err)
	}
	if len(client.posted) != 1 {
		t.Fatalf("posted acknowledgements = %+v, want 1", client.posted)
	}
	body := client.posted[0].body
	for _, want := range []string{"task: `pr-10-comment-909` not registered", "queued=1"} {
		if !strings.Contains(body, want) {
			t.Fatalf("status body missing %q:\n%s", want, body)
		}
	}
}

func TestPollOnceMergeCommandRunsMergeGate(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	if err := store.UpsertTask(ctx, db.Task{
		ID:           "task-010",
		RepoFullName: repo.FullName(),
		GoalID:       "goal-1",
		Title:        "Task 10",
		State:        string(workflow.TaskReadyToMerge),
		Branch:       "task-10",
	}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: repo.FullName(), Branch: "task-10", Owner: "builder"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "audit",
		Role:           "reviewer",
		Runtime:        "codex",
		RuntimeRef:     "last",
		RepoScope:      repo.FullName(),
		Capabilities:   []string{"review"},
		AutonomyPolicy: "auto",
		HealthStatus:   "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent audit returned error: %v", err)
	}
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: repo.FullName(),
		Number:       10,
		HeadBranch:   "task-10",
		BaseBranch:   "main",
		HeadSHA:      "abc123",
		State:        "open",
	}); err != nil {
		t.Fatalf("UpsertPullRequest returned error: %v", err)
	}
	gate := &fakeWorkflowMergeGate{decision: workflow.MergeDecision{Ready: true, Merged: true, MergeCommitSHA: "merge123"}}
	engine := workflow.Engine{Store: store, MergeGate: gate}
	client := &fakeGitHub{}
	err := (Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}).handleMergeCommand(
		ctx,
		github.PullRequest{Number: 10, Title: "Task 10", State: "open", HeadRef: "task-10", BaseRef: "main", HeadSHA: "abc123"},
		github.IssueComment{ID: 910, Body: "/gitmoot merge", Author: "dana"},
	)

	if err != nil {
		t.Fatalf("PollOnce returned error: %v", err)
	}
	if len(gate.requests) != 1 {
		t.Fatalf("merge gate requests = %+v, want 1", gate.requests)
	}
	request := gate.requests[0]
	if request.Repo != repo.FullName() || request.Branch != "task-10" || request.PullRequest != 10 || request.TaskID != "task-010" {
		t.Fatalf("merge request = %+v", request)
	}
	if request.ReviewOptional {
		t.Fatalf("merge request ReviewOptional = true, want false when repo review agents are configured")
	}
	if len(client.posted) != 1 || !strings.Contains(client.posted[0].body, "Gitmoot merged PR #10") {
		t.Fatalf("posted acknowledgements = %+v", client.posted)
	}
	task, err := store.GetTask(ctx, "task-010")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.State != string(workflow.TaskMerged) {
		t.Fatalf("task state = %q, want merged", task.State)
	}
}

func TestPollOnceMergeCommandAcceptsAwaitingHumanMergeTask(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	if err := store.UpsertTask(ctx, db.Task{ID: "task-awaiting", RepoFullName: repo.FullName(), Title: "Awaiting human merge", State: string(workflow.TaskAwaitingHumanMerge), Branch: "task-awaiting"}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: repo.FullName(), Branch: "task-awaiting", Owner: "builder"}); err != nil || !acquired {
		t.Fatalf("AcquireLock = %v, %v", acquired, err)
	}
	gate := &fakeWorkflowMergeGate{decision: workflow.MergeDecision{Ready: true, Merged: true, MergeCommitSHA: "merge-awaiting"}}
	engine := workflow.Engine{Store: store, MergeGate: gate}
	client := &fakeGitHub{}
	if err := (Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}).handleMergeCommand(ctx,
		github.PullRequest{Number: 27, State: "open", HeadRef: "task-awaiting", BaseRef: "main", HeadSHA: "abc"},
		github.IssueComment{ID: 27, Body: "/gitmoot merge", Author: "operator"}); err != nil {
		t.Fatalf("handleMergeCommand: %v", err)
	}
	if len(gate.requests) != 1 || !gate.requests[0].HumanMergeRequested {
		t.Fatalf("merge requests = %+v; want one explicit human request", gate.requests)
	}
	task, err := store.GetTask(ctx, "task-awaiting")
	if err != nil || task.State != string(workflow.TaskMerged) {
		t.Fatalf("task = %+v, err=%v; want merged", task, err)
	}
}

func TestPollOnceMergeCommandRefusesSkipNativeFanoutBranch(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	if err := store.UpsertTask(ctx, db.Task{
		ID:           "task-010",
		RepoFullName: repo.FullName(),
		GoalID:       "goal-1",
		Title:        "Task 10",
		State:        string(workflow.TaskReadyToMerge),
		Branch:       "task-10",
	}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: repo.FullName(), Branch: "task-10", Owner: "builder"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	if err := store.SetBranchLockReviewFanout(ctx, repo.FullName(), "task-10", true); err != nil {
		t.Fatalf("SetBranchLockReviewFanout returned error: %v", err)
	}
	gate := &fakeWorkflowMergeGate{decision: workflow.MergeDecision{Ready: true, Merged: true, MergeCommitSHA: "merge123"}}
	engine := workflow.Engine{Store: store, MergeGate: gate}
	client := &fakeGitHub{}
	err := (Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}).handleMergeCommand(
		ctx,
		github.PullRequest{Number: 10, Title: "Task 10", State: "open", HeadRef: "task-10", BaseRef: "main", HeadSHA: "abc123"},
		github.IssueComment{ID: 910, Body: "/gitmoot merge", Author: "dana"},
	)

	if err != nil {
		t.Fatalf("handleMergeCommand returned error: %v", err)
	}
	if len(gate.requests) != 0 {
		t.Fatalf("merge gate requests = %+v, want none", gate.requests)
	}
	if len(client.posted) != 1 || !strings.Contains(client.posted[0].body, "external council gate") {
		t.Fatalf("posted acknowledgements = %+v", client.posted)
	}
}

func TestPollOnceMergeCommandRequiresReadyTask(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	if err := store.UpsertTask(ctx, db.Task{
		ID:           "task-010",
		RepoFullName: repo.FullName(),
		GoalID:       "goal-1",
		Title:        "Task 10",
		State:        string(workflow.TaskReviewing),
		Branch:       "task-10",
	}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: repo.FullName(), Branch: "task-10", Owner: "builder"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	gate := &fakeWorkflowMergeGate{decision: workflow.MergeDecision{Ready: true, Merged: true, MergeCommitSHA: "merge123"}}
	engine := workflow.Engine{Store: store, MergeGate: gate}
	client := &fakeGitHub{}
	err := (Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}).handleMergeCommand(
		ctx,
		github.PullRequest{Number: 10, Title: "Task 10", State: "open", HeadRef: "task-10", BaseRef: "main", HeadSHA: "abc123"},
		github.IssueComment{ID: 911, Body: "/gitmoot merge", Author: "dana"},
	)

	if err != nil {
		t.Fatalf("PollOnce returned error: %v", err)
	}
	if len(gate.requests) != 0 {
		t.Fatalf("merge gate requests = %+v, want none", gate.requests)
	}
	if len(client.posted) != 1 || !strings.Contains(client.posted[0].body, "not `ready_to_merge`") {
		t.Fatalf("posted acknowledgements = %+v", client.posted)
	}
}

func TestPollOnceQueuesImplementWithBranchLock(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "builder",
		Role:           "builder",
		Runtime:        "codex",
		RuntimeRef:     "last",
		RepoScope:      repo.FullName(),
		Capabilities:   []string{"implement"},
		AutonomyPolicy: "workspace-write",
		HealthStatus:   "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent returned error: %v", err)
	}
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: repo.FullName(), Branch: "task-10", Owner: "builder"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	if err := store.UpsertTask(ctx, db.Task{ID: "task-010", GoalID: "goal-1", Title: "Task 10", State: string(workflow.TaskImplementing), Branch: "task-10"}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	client := &fakeGitHub{
		pulls: []github.PullRequest{{Number: 10, Title: "Task 10", State: "open", HeadRef: "task-10", BaseRef: "main"}},
		comments: map[int64][]github.IssueComment{
			10: {{ID: 808, Body: "/gitmoot builder implement", Author: "dana"}},
		},
	}

	err := (Daemon{Repo: repo, Store: store, GitHub: client}).PollOnce(ctx)

	if err != nil {
		t.Fatalf("PollOnce returned error: %v", err)
	}
	job, err := store.GetJob(ctx, jobID(repo, 10, 808, 0, "builder", "implement"))
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	var payload workflow.JobPayload
	if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
		t.Fatalf("Unmarshal payload returned error: %v", err)
	}
	if payload.TaskID != "task-010" || payload.GoalID != "goal-1" {
		t.Fatalf("payload task context = task %q goal %q, want existing branch task context", payload.TaskID, payload.GoalID)
	}
}

func TestPollOnceRetriesUnseenCommentAfterAckFailure(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "audit",
		Role:           "reviewer",
		Runtime:        "codex",
		RuntimeRef:     "last",
		RepoScope:      repo.FullName(),
		Capabilities:   []string{"review"},
		AutonomyPolicy: "auto",
		HealthStatus:   "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent returned error: %v", err)
	}
	client := &fakeGitHub{
		postErrs: []error{errors.New("temporary ack failure")},
		pulls:    []github.PullRequest{{Number: 9, Title: "Task 9", State: "open", HeadRef: "task-9", BaseRef: "main"}},
		comments: map[int64][]github.IssueComment{
			9: {{ID: 707, Body: "/gitmoot audit review", Author: "frank"}},
		},
	}
	daemon := Daemon{Repo: repo, Store: store, GitHub: client}

	if err := daemon.PollOnce(ctx); err == nil {
		t.Fatal("first PollOnce succeeded despite acknowledgement failure")
	}
	seen, err := store.HasCommentSeen(ctx, repo.FullName(), 707)
	if err != nil {
		t.Fatalf("HasCommentSeen returned error: %v", err)
	}
	if seen {
		t.Fatal("comment was marked seen before acknowledgement succeeded")
	}

	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("second PollOnce returned error: %v", err)
	}
	if len(client.posted) != 2 {
		t.Fatalf("posted acknowledgements = %+v, want 2 attempts", client.posted)
	}
	events, err := store.ListJobEvents(ctx, jobID(repo, 9, 707, 0, "audit", "review"))
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("events = %+v, want original queue+routed only", events)
	}
}

func TestRunReturnsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := testStore(t)
	client := &fakeGitHub{}
	daemon := Daemon{
		Repo:         github.Repository{Owner: "gitmoot", Name: "gitmoot"},
		Store:        store,
		GitHub:       client,
		PollInterval: time.Hour,
		Sleep: func(ctx context.Context, _ time.Duration) error {
			cancel()
			<-ctx.Done()
			return ctx.Err()
		},
	}

	err := daemon.Run(ctx)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}
	if client.listPullRequestsCalls != 1 {
		t.Fatalf("ListPullRequests calls = %d, want 1", client.listPullRequestsCalls)
	}
}

func TestRunContinuesAfterPollError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := testStore(t)
	client := &fakeGitHub{listPullRequestsErrs: []error{errors.New("rate limited"), nil}}
	var sleeps int
	daemon := Daemon{
		Repo:         github.Repository{Owner: "gitmoot", Name: "gitmoot"},
		Store:        store,
		GitHub:       client,
		PollInterval: time.Second,
		Sleep: func(ctx context.Context, _ time.Duration) error {
			sleeps++
			if sleeps == 1 {
				return nil
			}
			cancel()
			<-ctx.Done()
			return ctx.Err()
		},
	}

	err := daemon.Run(ctx)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}
	if client.listPullRequestsCalls != 2 {
		t.Fatalf("ListPullRequests calls = %d, want 2", client.listPullRequestsCalls)
	}
}

func TestPollOnceWithoutWatchIssuesIgnoresIssues(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "researcher",
		Role:           "researcher",
		Runtime:        "claude",
		RuntimeRef:     "last",
		RepoScope:      repo.FullName(),
		Capabilities:   []string{"ask"},
		AutonomyPolicy: "auto",
		HealthStatus:   "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent returned error: %v", err)
	}
	client := &fakeGitHub{
		issues: []github.Issue{{Number: 42, Title: "Question", State: "open"}},
		comments: map[int64][]github.IssueComment{
			42: {{ID: 900, Body: "/gitmoot researcher ask what is best", Author: "alice"}},
		},
	}

	// WatchIssues defaults to false: the issue loop must not run at all.
	err := (Daemon{Repo: repo, Store: store, GitHub: client}).PollOnce(ctx)

	if err != nil {
		t.Fatalf("PollOnce returned error: %v", err)
	}
	if client.listIssuesCalls != 0 {
		t.Fatalf("ListIssues calls = %d, want 0 when --watch-issues is off", client.listIssuesCalls)
	}
	if len(client.posted) != 0 {
		t.Fatalf("posted = %+v, want none when --watch-issues is off", client.posted)
	}
	if _, err := store.GetJob(ctx, issueJobID(repo, 42, 900, 0, "researcher", "ask")); err == nil {
		t.Fatal("issue ask created a job while --watch-issues was off")
	}
}

func TestPollIssuesOnceRoutesAskAndDedupes(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "researcher",
		Role:           "researcher",
		Runtime:        "claude",
		RuntimeRef:     "last",
		RepoScope:      repo.FullName(),
		Capabilities:   []string{"ask"},
		AutonomyPolicy: "auto",
		HealthStatus:   "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent returned error: %v", err)
	}
	client := &fakeGitHub{
		issues: []github.Issue{
			{Number: 42, Title: "Question", State: "open"},
			// A PR slipped into the listing (defense in depth): it must be skipped.
			{Number: 43, Title: "A PR", State: "open", IsPullRequest: true},
		},
		comments: map[int64][]github.IssueComment{
			42: {{ID: 900, Body: "/gitmoot researcher ask what is the best approach", Author: "alice"}},
			43: {{ID: 901, Body: "/gitmoot researcher ask should never run", Author: "alice"}},
		},
	}

	d := Daemon{Repo: repo, Store: store, GitHub: client, WatchIssues: true}
	if err := d.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce returned error: %v", err)
	}

	if client.listIssuesCalls != 1 {
		t.Fatalf("ListIssues calls = %d, want 1", client.listIssuesCalls)
	}
	if len(client.posted) != 1 || client.posted[0].issueNumber != 42 {
		t.Fatalf("posted acknowledgements = %+v, want 1 on issue 42", client.posted)
	}
	if !strings.Contains(client.posted[0].body, "queued `ask` job") || !strings.Contains(client.posted[0].body, "`researcher`") {
		t.Fatalf("ack body = %q", client.posted[0].body)
	}

	wantID := issueJobID(repo, 42, 900, 0, "researcher", "ask")
	job, err := store.GetJob(ctx, wantID)
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if !strings.HasPrefix(job.ID, "issue-comment-") {
		t.Fatalf("issue job id = %q, want issue-comment- prefix", job.ID)
	}
	if job.Agent != "researcher" || job.Type != "ask" || job.State != string(workflow.JobQueued) {
		t.Fatalf("job = %+v", job)
	}
	var payload workflow.JobPayload
	if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.Repo != repo.FullName() || payload.Branch != "" || payload.HeadSHA != "" {
		t.Fatalf("ask payload should carry empty branch/headSHA: %+v", payload)
	}
	if payload.PullRequest != 42 || payload.Sender != "alice" || payload.Instructions != "what is the best approach" {
		t.Fatalf("payload = %+v", payload)
	}
	if _, err := store.GetJob(ctx, issueJobID(repo, 43, 901, 0, "researcher", "ask")); err == nil {
		t.Fatal("ask on a PR row was routed; PRs must be skipped")
	}
	events, err := store.ListJobEvents(ctx, wantID)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if len(events) != 2 || events[1].Kind != "routed" || !strings.Contains(events[1].Message, "issue #42") {
		t.Fatalf("events = %+v", events)
	}

	// Second poll: the comment is already seen, so no duplicate job/ack.
	if err := d.PollOnce(ctx); err != nil {
		t.Fatalf("second PollOnce returned error: %v", err)
	}
	if len(client.posted) != 1 {
		t.Fatalf("posted acknowledgements after re-poll = %+v, want 1 (deduped)", client.posted)
	}
}

// TestPollIssuesOnceCollapsesToSingleRepoWideCommentCall is the #566 regression
// guard: PollIssuesOnce must make ONE repo-wide ListRepoIssueComments call per
// tick (not N per-issue ListIssueComments calls), group the flat result back by
// issue number, route each open non-PR issue's comments through the unchanged
// handleIssueComment path, advance a persisted since cursor, and dedup an
// already-seen/edited comment on the next tick.
func TestPollIssuesOnceCollapsesToSingleRepoWideCommentCall(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "researcher",
		Role:           "researcher",
		Runtime:        "claude",
		RuntimeRef:     "last",
		RepoScope:      repo.FullName(),
		Capabilities:   []string{"ask"},
		AutonomyPolicy: "auto",
		HealthStatus:   "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent returned error: %v", err)
	}

	now := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)
	client := &fakeGitHub{
		issues: []github.Issue{
			{Number: 42, Title: "Question A", State: "open"},
			{Number: 7, Title: "Question B", State: "open"},
			// A PR in the issue listing: its comments must be skipped here (owned by
			// the PR loop), even though the repo-wide endpoint returns them.
			{Number: 43, Title: "A PR", State: "open", IsPullRequest: true},
		},
		repoComments: []github.IssueComment{
			{ID: 900, IssueNumber: 42, Body: "/gitmoot researcher ask question one", Author: "alice", UpdatedAt: "2026-06-27T12:00:30Z"},
			{ID: 901, IssueNumber: 7, Body: "/gitmoot researcher ask question two", Author: "alice", UpdatedAt: "2026-06-27T12:00:45Z"},
			// PR comment (issue 43) and a comment on an unknown/closed issue (99):
			// both must be skipped by the grouping.
			{ID: 902, IssueNumber: 43, Body: "/gitmoot researcher ask on a PR", Author: "alice", UpdatedAt: "2026-06-27T12:00:20Z"},
			{ID: 903, IssueNumber: 99, Body: "/gitmoot researcher ask on unknown", Author: "alice", UpdatedAt: "2026-06-27T12:00:20Z"},
		},
	}
	d := Daemon{Repo: repo, Store: store, GitHub: client, WatchIssues: true, Now: func() time.Time { return now }}

	if err := d.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce returned error: %v", err)
	}

	// ONE repo-wide comment call, ZERO per-issue comment calls (no open PRs, so the
	// PR loop makes none either).
	if client.listRepoCommentsCalls != 1 {
		t.Fatalf("ListRepoIssueComments calls = %d, want 1 per tick", client.listRepoCommentsCalls)
	}
	if client.listIssueCommentsCalls != 0 {
		t.Fatalf("per-issue ListIssueComments calls = %d, want 0 (collapsed into the repo-wide call)", client.listIssueCommentsCalls)
	}
	// since seeded from now minus the skew overlap on the first poll.
	if len(client.repoCommentsSince) != 1 || !client.repoCommentsSince[0].Equal(now.Add(-issueCommentPollOverlap)) {
		t.Fatalf("first since = %v, want %v", client.repoCommentsSince, now.Add(-issueCommentPollOverlap))
	}

	// Grouping/routing: jobs for issue 42 and issue 7; none for the PR or unknown.
	if _, err := store.GetJob(ctx, issueJobID(repo, 42, 900, 0, "researcher", "ask")); err != nil {
		t.Fatalf("issue 42 comment was not routed: %v", err)
	}
	if _, err := store.GetJob(ctx, issueJobID(repo, 7, 901, 0, "researcher", "ask")); err != nil {
		t.Fatalf("issue 7 comment was not routed: %v", err)
	}
	if _, err := store.GetJob(ctx, issueJobID(repo, 43, 902, 0, "researcher", "ask")); err == nil {
		t.Fatal("PR comment was routed by the issue watcher; must be skipped")
	}
	if _, err := store.GetJob(ctx, issueJobID(repo, 99, 903, 0, "researcher", "ask")); err == nil {
		t.Fatal("comment on an unknown issue was routed; must be skipped")
	}
	if len(client.posted) != 2 {
		t.Fatalf("acks = %d, want 2 (issues 42 and 7)", len(client.posted))
	}

	// Cursor advanced to the newest comment updated_at seen (12:00:45).
	cursor, ok, err := store.GetIssueCommentPollCursor(ctx, repo.FullName())
	if err != nil || !ok {
		t.Fatalf("GetIssueCommentPollCursor ok=%v err=%v", ok, err)
	}
	wantCursor := time.Date(2026, 6, 27, 12, 0, 45, 0, time.UTC)
	if !cursor.Equal(wantCursor) {
		t.Fatalf("cursor = %v, want %v", cursor, wantCursor)
	}

	// Second poll: an EDITED replay of comment 900 (newer updated_at, so it passes
	// the since filter) plus a genuinely new comment 904. The edit is already seen
	// and must be deduped; only 904 routes.
	client.repoComments = []github.IssueComment{
		{ID: 900, IssueNumber: 42, Body: "/gitmoot researcher ask question one edited", Author: "alice", UpdatedAt: "2026-06-27T12:01:00Z"},
		{ID: 904, IssueNumber: 7, Body: "/gitmoot researcher ask question three", Author: "alice", UpdatedAt: "2026-06-27T12:01:10Z"},
	}
	if err := d.PollOnce(ctx); err != nil {
		t.Fatalf("second PollOnce returned error: %v", err)
	}
	if client.listRepoCommentsCalls != 2 {
		t.Fatalf("ListRepoIssueComments calls = %d after 2 ticks, want 2", client.listRepoCommentsCalls)
	}
	// Second since = advanced cursor (12:00:45) minus overlap.
	if !client.repoCommentsSince[1].Equal(wantCursor.Add(-issueCommentPollOverlap)) {
		t.Fatalf("second since = %v, want %v", client.repoCommentsSince[1], wantCursor.Add(-issueCommentPollOverlap))
	}
	// Edited already-seen comment 900 produced no second ack; only 904 did.
	if len(client.posted) != 3 {
		t.Fatalf("acks after 2nd poll = %d, want 3 (edited 900 deduped, only 904 new)", len(client.posted))
	}
	if _, err := store.GetJob(ctx, issueJobID(repo, 7, 904, 0, "researcher", "ask")); err != nil {
		t.Fatalf("new comment 904 was not routed: %v", err)
	}
	cursor2, _, err := store.GetIssueCommentPollCursor(ctx, repo.FullName())
	if err != nil {
		t.Fatalf("GetIssueCommentPollCursor: %v", err)
	}
	if !cursor2.Equal(time.Date(2026, 6, 27, 12, 1, 10, 0, time.UTC)) {
		t.Fatalf("cursor after 2nd poll = %v, want 12:01:10", cursor2)
	}
}

func TestPollIssuesOnceIgnoresNonAskAndUnknownAgent(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	client := &fakeGitHub{
		issues: []github.Issue{{Number: 7, Title: "Issue 7", State: "open"}},
		comments: map[int64][]github.IssueComment{
			// review/implement/status are PR-only: ignored on a plain issue, and the
			// comment must NOT be marked seen so a later real ask is still picked up.
			7: {{ID: 11, Body: "/gitmoot someone review please", Author: "alice"}},
		},
	}

	d := Daemon{Repo: repo, Store: store, GitHub: client, WatchIssues: true}
	if err := d.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce returned error: %v", err)
	}
	if len(client.posted) != 0 {
		t.Fatalf("posted = %+v, want none for a non-ask issue comment", client.posted)
	}
	seen, err := store.HasCommentSeen(ctx, repo.FullName(), 11)
	if err != nil {
		t.Fatalf("HasCommentSeen returned error: %v", err)
	}
	if seen {
		t.Fatal("non-ask issue comment was marked seen; a later ask would be lost")
	}
}

// TestHandleIssueCommentRoutesMentionForm is the daemon-level regression guard
// for the #389 live bug. The existing PollIssuesOnce test fed `/gitmoot <agent>
// ask …` and passed, so it never exercised the form a real user types: a bare
// `@<agent> ask …` mention. handleIssueComment ran ParseCommands on that mention,
// got zero commands, and silently returned nil — no job, no reply. This drives
// the real handleIssueComment with the exact live mention body and asserts it
// enqueues the deduped issue ask job and posts the acknowledgement.
func TestHandleIssueCommentRoutesMentionForm(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "helper",
		Role:           "helper",
		Runtime:        "codex",
		RuntimeRef:     "last",
		RepoScope:      repo.FullName(),
		Capabilities:   []string{"ask"},
		AutonomyPolicy: "auto",
		HealthStatus:   "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent returned error: %v", err)
	}
	client := &fakeGitHub{}
	d := Daemon{Repo: repo, Store: store, GitHub: client, WatchIssues: true}

	issue := github.Issue{Number: 1, Title: "Question", State: "open"}
	comment := github.IssueComment{ID: 900, Body: "@helper ask Reply with exactly: ok", Author: "alice"}
	if err := d.handleIssueComment(ctx, issue, comment); err != nil {
		t.Fatalf("handleIssueComment returned error: %v", err)
	}

	wantID := issueJobID(repo, 1, 900, 0, "helper", "ask")
	job, err := store.GetJob(ctx, wantID)
	if err != nil {
		t.Fatalf("GetJob returned error (mention was not routed): %v", err)
	}
	if job.Agent != "helper" || job.Type != "ask" || job.State != string(workflow.JobQueued) {
		t.Fatalf("job = %+v", job)
	}
	var payload workflow.JobPayload
	if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.Instructions != "Reply with exactly: ok" {
		t.Fatalf("payload instructions = %q", payload.Instructions)
	}
	if len(client.posted) != 1 || client.posted[0].issueNumber != 1 {
		t.Fatalf("posted = %+v, want 1 ack on issue 1", client.posted)
	}
	if !strings.Contains(client.posted[0].body, "queued `ask` job") || !strings.Contains(client.posted[0].body, "`helper`") {
		t.Fatalf("ack body = %q", client.posted[0].body)
	}

	// Re-running the same mention must dedupe: the comment is now seen, so no
	// duplicate job or ack.
	if err := d.handleIssueComment(ctx, issue, comment); err != nil {
		t.Fatalf("second handleIssueComment returned error: %v", err)
	}
	if len(client.posted) != 1 {
		t.Fatalf("posted after re-run = %+v, want 1 (deduped)", client.posted)
	}
}

func testStore(t *testing.T) *db.Store {
	t.Helper()
	store, err := dbtest.Open(t, filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	})
	return store
}

type fakeGitHub struct {
	pulls                  []github.PullRequest
	pullsByState           map[string][]github.PullRequest
	pullsByNumber          map[int64]github.PullRequest
	issues                 []github.Issue
	comments               map[int64][]github.IssueComment
	repoComments           []github.IssueComment
	repoCommentsSince      []time.Time
	listRepoCommentsCalls  int
	listIssueCommentsCalls int
	posted                 []postedComment
	permissions            map[string]string
	postErrs               []error
	listPullRequestsCalls  int
	listPullRequestsErrs   []error
	listIssuesCalls        int
	recentClosedCalls      int
	getPullRequestCalls    []int64
}

type postedComment struct {
	issueNumber int64
	body        string
}

func (f *fakeGitHub) Ping(context.Context) error {
	return nil
}

func (f *fakeGitHub) Preflight(context.Context, github.Repository) error {
	return nil
}

func (f *fakeGitHub) RepositoryExists(context.Context, github.Repository) (bool, error) {
	return true, nil
}

func (f *fakeGitHub) CreateRepository(context.Context, github.Repository, bool) error {
	return nil
}

func (f *fakeGitHub) CloneRepository(context.Context, github.Repository, string) error {
	return nil
}

func (f *fakeGitHub) ListUserRepositories(context.Context, int) ([]github.RepoSummary, error) {
	return nil, nil
}

func (f *fakeGitHub) DeleteRepository(context.Context, github.Repository) error {
	return nil
}

func (f *fakeGitHub) ListPullRequests(_ context.Context, _ github.Repository, state string) ([]github.PullRequest, error) {
	f.listPullRequestsCalls++
	if len(f.listPullRequestsErrs) > 0 {
		err := f.listPullRequestsErrs[0]
		f.listPullRequestsErrs = f.listPullRequestsErrs[1:]
		if err != nil {
			return nil, err
		}
	}
	if f.pullsByState != nil {
		return append([]github.PullRequest(nil), f.pullsByState[state]...), nil
	}
	return append([]github.PullRequest(nil), f.pulls...), nil
}

// ListRecentClosedPullRequests models the bounded closed-PR scan (#467): the fake
// simply returns the "closed"-state fixtures (the test fixtures are already small,
// so a single page covers them). It counts calls so the off-path byte-identical
// test can prove no closed read happens when revert detection is disabled.
func (f *fakeGitHub) ListRecentClosedPullRequests(_ context.Context, _ github.Repository) ([]github.PullRequest, error) {
	f.recentClosedCalls++
	if f.pullsByState != nil {
		return append([]github.PullRequest(nil), f.pullsByState["closed"]...), nil
	}
	return append([]github.PullRequest(nil), f.pulls...), nil
}

func (f *fakeGitHub) ListIssues(_ context.Context, _ github.Repository, _ string) ([]github.Issue, error) {
	f.listIssuesCalls++
	return append([]github.Issue(nil), f.issues...), nil
}

func (f *fakeGitHub) CreatePullRequest(context.Context, github.CreatePullRequestInput) (github.PullRequest, error) {
	return github.PullRequest{}, errors.New("not implemented")
}

func (f *fakeGitHub) GetOpenPullRequestByHead(context.Context, github.Repository, string, string) (github.PullRequest, bool, error) {
	return github.PullRequest{}, false, errors.New("not implemented")
}

func (f *fakeGitHub) EnsurePullRequest(context.Context, github.CreatePullRequestInput) (github.PullRequest, error) {
	return github.PullRequest{}, errors.New("not implemented")
}

func (f *fakeGitHub) CreateIssue(context.Context, github.CreateIssueInput) (github.Issue, error) {
	return github.Issue{}, errors.New("not implemented")
}

func (f *fakeGitHub) CloseIssue(context.Context, github.Repository, int64) (github.Issue, error) {
	return github.Issue{}, errors.New("not implemented")
}

func (f *fakeGitHub) ListIssueComments(_ context.Context, _ github.Repository, issueNumber int64) ([]github.IssueComment, error) {
	f.listIssueCommentsCalls++
	return append([]github.IssueComment(nil), f.comments[issueNumber]...), nil
}

// ListRepoIssueComments models the repo-wide comment endpoint (#566). It counts
// calls (so a test can prove ONE call per repo per tick, not N per-issue) and
// records the `since` cursor it was handed. Fixtures can be supplied two ways:
//   - repoComments: an explicit flat list (each carries its own IssueNumber and
//     UpdatedAt); comments with a parseable UpdatedAt strictly before `since` are
//     filtered out, mirroring the real endpoint's since= behavior.
//   - comments map (issue-number keyed): flattened with IssueNumber set to the
//     key. Map fixtures carry no timestamps, so they are always returned (the
//     since filter only applies to comments that declare an UpdatedAt).
func (f *fakeGitHub) ListRepoIssueComments(_ context.Context, _ github.Repository, since time.Time) ([]github.IssueComment, error) {
	f.listRepoCommentsCalls++
	f.repoCommentsSince = append(f.repoCommentsSince, since)
	var out []github.IssueComment
	appendIfAfter := func(c github.IssueComment) {
		if !since.IsZero() && strings.TrimSpace(c.UpdatedAt) != "" {
			if t, err := time.Parse(time.RFC3339, c.UpdatedAt); err == nil && t.Before(since) {
				return
			}
		}
		out = append(out, c)
	}
	if f.repoComments != nil {
		for _, c := range f.repoComments {
			appendIfAfter(c)
		}
		return out, nil
	}
	for number, list := range f.comments {
		for _, c := range list {
			c.IssueNumber = number
			appendIfAfter(c)
		}
	}
	return out, nil
}

func (f *fakeGitHub) PostIssueComment(_ context.Context, _ github.Repository, issueNumber int64, body string) (github.IssueComment, error) {
	f.posted = append(f.posted, postedComment{issueNumber: issueNumber, body: body})
	if len(f.postErrs) > 0 {
		err := f.postErrs[0]
		f.postErrs = f.postErrs[1:]
		if err != nil {
			return github.IssueComment{}, err
		}
	}
	return github.IssueComment{ID: int64(len(f.posted)), Body: body}, nil
}

func (f *fakeGitHub) GetUserPermission(_ context.Context, _ github.Repository, username string) (github.UserPermission, error) {
	permission := "write"
	if f.permissions != nil {
		permission = f.permissions[username]
	}
	return github.UserPermission{Permission: permission, RoleName: permission}, nil
}

func (f *fakeGitHub) MergePullRequest(context.Context, github.MergePullRequestInput) (github.MergeResult, error) {
	return github.MergeResult{}, errors.New("not implemented")
}

func (f *fakeGitHub) UpdatePullRequestBranch(context.Context, github.UpdatePullRequestBranchInput) (github.UpdatePullRequestBranchResult, error) {
	return github.UpdatePullRequestBranchResult{}, errors.New("not implemented")
}

func (f *fakeGitHub) GetPullRequest(_ context.Context, _ github.Repository, number int64) (github.PullRequest, error) {
	f.getPullRequestCalls = append(f.getPullRequestCalls, number)
	if f.pullsByNumber != nil {
		if pull, ok := f.pullsByNumber[number]; ok {
			return pull, nil
		}
	}
	for _, state := range []string{"open", "closed"} {
		pulls := f.pullsByState[state]
		for _, pull := range pulls {
			if pull.Number == number {
				return pull, nil
			}
		}
	}
	for _, pull := range f.pulls {
		if pull.Number == number {
			return pull, nil
		}
	}
	return github.PullRequest{}, errors.New("not implemented")
}

func (f *fakeGitHub) GetCombinedStatus(context.Context, github.Repository, string) (github.CombinedStatus, error) {
	return github.CombinedStatus{}, errors.New("not implemented")
}

func (f *fakeGitHub) CompareCommits(context.Context, github.Repository, string, string) (github.CompareResult, error) {
	return github.CompareResult{}, errors.New("not implemented")
}

func (f *fakeGitHub) ListPullRequestChecks(context.Context, github.Repository, int64) ([]github.PullRequestCheck, error) {
	return nil, errors.New("not implemented")
}

func (f *fakeGitHub) ListCheckRunsForRef(context.Context, github.Repository, string) ([]github.PullRequestCheck, error) {
	return nil, errors.New("not implemented")
}

func (f *fakeGitHub) CreateCommitStatus(context.Context, github.CommitStatusInput) (github.CommitStatus, error) {
	return github.CommitStatus{}, errors.New("not implemented")
}

func (f *fakeGitHub) ListPullRequestFiles(context.Context, github.Repository, int64) ([]github.PullRequestFile, error) {
	return nil, errors.New("not implemented")
}

func (f *fakeGitHub) ListPullRequestCommits(context.Context, github.Repository, int64) ([]github.PullRequestCommit, error) {
	return nil, errors.New("not implemented")
}

func (f *fakeGitHub) UpsertFile(context.Context, github.UpsertFileInput) (github.RepositoryFile, error) {
	return github.RepositoryFile{}, errors.New("not implemented")
}

type fakeWorkflowMergeGate struct {
	decision   workflow.MergeDecision
	onEvaluate func(workflow.MergeRequest)
	requests   []workflow.MergeRequest
}

func (f *fakeWorkflowMergeGate) Evaluate(_ context.Context, request workflow.MergeRequest) (workflow.MergeDecision, error) {
	f.requests = append(f.requests, request)
	if f.onEvaluate != nil {
		f.onEvaluate(request)
	}
	return f.decision, nil
}

func hasDaemonJobEvent(events []db.JobEvent, kind string) bool {
	for _, event := range events {
		if event.Kind == kind {
			return true
		}
	}
	return false
}

// #1250, READER 2 of 2: the daemon PR-watcher. It takes the acting org role from
// the SAME branch lock row it already fetches for the skip flag — one durable
// writer, two readers — so this trigger and the in-process advance attribute
// native fanout children identically and cannot drift.
//
// Fanout children were previously enqueued with NO attribution (0 of 99 on the
// live store), and attribution is the wake target role: an unattributed job's
// blocked event has no owner to wake (#1347).
func TestHandlePullRequestWorkflowAttributesFanoutChildrenFromBranchLock(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
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
	// The branch was taken by an attributed dispatch; the lock carries the role.
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{
		RepoFullName:  repo.FullName(),
		Branch:        "task-7",
		Owner:         "lead",
		ActingOrgRole: "gmc-fanout",
	}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	if err := store.UpsertTask(ctx, db.Task{ID: "task-007", GoalID: "goal-1", Title: "Task 7", State: string(workflow.TaskPlanned), Branch: "task-7"}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	client := &fakeGitHub{pulls: []github.PullRequest{}, comments: map[int64][]github.IssueComment{7: {}}}
	engine := workflow.Engine{
		Store: store,
		JobID: func(request workflow.JobRequest) string {
			parts := []string{request.Action, request.Agent, request.TaskID}
			if request.ReviewRound != "" {
				parts = append(parts, request.ReviewRound)
			}
			return strings.Join(parts, "-")
		},
	}
	daemon := Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}

	pull := github.PullRequest{
		Number: 7, Title: "Task 7", State: "open",
		URL:     "https://github.com/gitmoot/gitmoot/pull/7",
		HeadRef: "task-7", BaseRef: "main", HeadSHA: "abc123",
	}
	if err := daemon.handlePullRequestWorkflow(ctx, pull, nil); err != nil {
		t.Fatalf("handlePullRequestWorkflow returned error: %v", err)
	}

	job, err := store.GetJob(ctx, "review-audit-task-007-review-1")
	if err != nil {
		t.Fatalf("expected review job to be enqueued: %v", err)
	}
	payload, err := workflow.ParseJobPayload(job.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload returned error: %v", err)
	}
	if payload.ActingOrgRole != "gmc-fanout" {
		t.Fatalf("daemon-trigger fanout child acting_org_role = %q, want %q; an unattributed fanout child has no owner to wake (#1347)", payload.ActingOrgRole, "gmc-fanout")
	}
}
