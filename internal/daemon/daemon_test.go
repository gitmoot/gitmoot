package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/github"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

func TestPollOnceCreatesJobAndAcknowledgement(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "jerryfane", Name: "gitmoot"}
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
			URL:     "https://github.com/jerryfane/gitmoot/pull/7",
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
	if payload.TemplateID != "thermo-nuclear-code-quality-review" || payload.TemplateResolvedCommit != "abc123" || payload.TemplateContent != "Review deeply." {
		t.Fatalf("payload template snapshot = %+v", payload)
	}
	events, err := store.ListJobEvents(ctx, jobID)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if len(events) != 2 || events[0].Kind != string(workflow.JobQueued) || events[1].Kind != "routed" {
		t.Fatalf("events = %+v", events)
	}
}

func TestPollOnceAcknowledgesAgentWithoutRepoAccess(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "jerryfane", Name: "gitmoot"}
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
			URL:     "https://github.com/jerryfane/gitmoot/pull/7",
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
	repo := github.Repository{Owner: "jerryfane", Name: "gitmoot"}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "lead",
		Role:           "lead",
		Runtime:        "codex",
		RuntimeRef:     "last",
		RepoScope:      repo.FullName(),
		Capabilities:   []string{"implement"},
		AutonomyPolicy: "auto",
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
			URL:     "https://github.com/jerryfane/gitmoot/pull/7",
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

func TestPollOnceRetriesPullRequestWorkflowAfterRoutingFailure(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "jerryfane", Name: "gitmoot"}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "lead",
		Role:           "lead",
		Runtime:        "codex",
		RuntimeRef:     "last",
		RepoScope:      repo.FullName(),
		Capabilities:   []string{"implement"},
		AutonomyPolicy: "auto",
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
			URL:     "https://github.com/jerryfane/gitmoot/pull/7",
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
	repo := github.Repository{Owner: "jerryfane", Name: "gitmoot"}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "lead",
		Role:           "lead",
		Runtime:        "codex",
		RuntimeRef:     "last",
		RepoScope:      repo.FullName(),
		Capabilities:   []string{"implement"},
		AutonomyPolicy: "auto",
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
			URL:     "https://github.com/jerryfane/gitmoot/pull/7",
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
	repo := github.Repository{Owner: "jerryfane", Name: "gitmoot"}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "lead",
		Role:           "lead",
		Runtime:        "codex",
		RuntimeRef:     "last",
		RepoScope:      repo.FullName(),
		Capabilities:   []string{"implement"},
		AutonomyPolicy: "auto",
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
			URL:     "https://github.com/jerryfane/gitmoot/pull/7",
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
	job, err := store.GetJob(ctx, "review-audit-task-7-review-2")
	if err != nil {
		t.Fatalf("GetJob rerouted review returned error: %v", err)
	}
	if !strings.Contains(job.Payload, `"head_sha":"abc123"`) {
		t.Fatalf("rerouted job payload missing head sha: %s", job.Payload)
	}
}

func TestPollOnceRetriesReadyToMergePullRequestWithoutHeadChange(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "jerryfane", Name: "gitmoot"}
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
		URL:          "https://github.com/jerryfane/gitmoot/pull/7",
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
			URL:     "https://github.com/jerryfane/gitmoot/pull/7",
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

func TestPollOnceRetriesClosedReadyToMergePullRequest(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "jerryfane", Name: "gitmoot"}
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
		URL:          "https://github.com/jerryfane/gitmoot/pull/7",
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
				URL:     "https://github.com/jerryfane/gitmoot/pull/6",
				HeadRef: "task-7",
				BaseRef: "main",
				HeadSHA: "old123",
			}, {
				Number:  7,
				Title:   "Task 7",
				State:   "closed",
				Merged:  true,
				URL:     "https://github.com/jerryfane/gitmoot/pull/7",
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
}

func TestPollOnceDoesNotOverwriteNoReviewerAutoMerge(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "jerryfane", Name: "gitmoot"}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "lead",
		Role:           "lead",
		Runtime:        "codex",
		RuntimeRef:     "last",
		RepoScope:      repo.FullName(),
		Capabilities:   []string{"implement"},
		AutonomyPolicy: "auto",
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
			URL:     "https://github.com/jerryfane/gitmoot/pull/7",
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
				URL:            "https://github.com/jerryfane/gitmoot/pull/7",
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
	repo := github.Repository{Owner: "jerryfane", Name: "gitmoot"}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "lead",
		Role:           "lead",
		Runtime:        "codex",
		RuntimeRef:     "last",
		RepoScope:      repo.FullName(),
		Capabilities:   []string{"implement"},
		AutonomyPolicy: "auto",
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
			URL:     "https://github.com/jerryfane/gitmoot/pull/7",
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
	repo := github.Repository{Owner: "jerryfane", Name: "gitmoot"}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "lead",
		Role:           "lead",
		Runtime:        "codex",
		RuntimeRef:     "last",
		RepoScope:      repo.FullName(),
		Capabilities:   []string{"implement"},
		AutonomyPolicy: "auto",
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
			URL:     "https://github.com/jerryfane/gitmoot/pull/7",
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
	repo := github.Repository{Owner: "jerryfane", Name: "gitmoot"}
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
	repo := github.Repository{Owner: "jerryfane", Name: "gitmoot"}
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
	repo := github.Repository{Owner: "jerryfane", Name: "gitmoot"}
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
	repo := github.Repository{Owner: "jerryfane", Name: "gitmoot"}
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
	repo := github.Repository{Owner: "jerryfane", Name: "gitmoot"}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "builder",
		Role:           "builder",
		Runtime:        "codex",
		RuntimeRef:     "last",
		RepoScope:      repo.FullName(),
		Capabilities:   []string{"implement"},
		AutonomyPolicy: "auto",
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
	repo := github.Repository{Owner: "jerryfane", Name: "gitmoot"}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "builder",
		Role:           "builder",
		Runtime:        "codex",
		RuntimeRef:     "last",
		RepoScope:      repo.FullName(),
		Capabilities:   []string{"implement"},
		AutonomyPolicy: "auto",
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
	repo := github.Repository{Owner: "jerryfane", Name: "gitmoot"}
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
	repo := github.Repository{Owner: "jerryfane", Name: "gitmoot"}
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
			URL:     "https://github.com/jerryfane/gitmoot/pull/10",
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
	for _, want := range []string{"Gitmoot help for `jerryfane/gitmoot` PR #10", "`audit`: review,ask", "/gitmoot <agent> <review|implement|ask>"} {
		if !strings.Contains(client.posted[0].body, want) {
			t.Fatalf("help output missing %q:\n%s", want, client.posted[0].body)
		}
	}
}

func TestPollOnceRetriesJobFromComment(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "jerryfane", Name: "gitmoot"}
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
	repo := github.Repository{Owner: "jerryfane", Name: "gitmoot"}
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
	repo := github.Repository{Owner: "jerryfane", Name: "gitmoot"}
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
	repo := github.Repository{Owner: "jerryfane", Name: "gitmoot"}
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
	repo := github.Repository{Owner: "jerryfane", Name: "gitmoot"}
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
	if len(client.posted) != 1 || !strings.Contains(client.posted[0].body, "belongs to jerryfane/gitmoot PR #11") {
		t.Fatalf("posted acknowledgements = %+v", client.posted)
	}
}

func TestPollOnceReportsStatusCommandCountsUnregisteredPRJobs(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "jerryfane", Name: "gitmoot"}
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
	repo := github.Repository{Owner: "jerryfane", Name: "gitmoot"}
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

func TestPollOnceMergeCommandRequiresReadyTask(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "jerryfane", Name: "gitmoot"}
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
	repo := github.Repository{Owner: "jerryfane", Name: "gitmoot"}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "builder",
		Role:           "builder",
		Runtime:        "codex",
		RuntimeRef:     "last",
		RepoScope:      repo.FullName(),
		Capabilities:   []string{"implement"},
		AutonomyPolicy: "auto",
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
	repo := github.Repository{Owner: "jerryfane", Name: "gitmoot"}
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
		Repo:         github.Repository{Owner: "jerryfane", Name: "gitmoot"},
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
		Repo:         github.Repository{Owner: "jerryfane", Name: "gitmoot"},
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

func testStore(t *testing.T) *db.Store {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
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
	pulls                 []github.PullRequest
	pullsByState          map[string][]github.PullRequest
	comments              map[int64][]github.IssueComment
	posted                []postedComment
	permissions           map[string]string
	postErrs              []error
	listPullRequestsCalls int
	listPullRequestsErrs  []error
}

type postedComment struct {
	issueNumber int64
	body        string
}

func (f *fakeGitHub) Ping(context.Context) error {
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

func (f *fakeGitHub) CreatePullRequest(context.Context, github.CreatePullRequestInput) (github.PullRequest, error) {
	return github.PullRequest{}, errors.New("not implemented")
}

func (f *fakeGitHub) ListIssueComments(_ context.Context, _ github.Repository, issueNumber int64) ([]github.IssueComment, error) {
	return append([]github.IssueComment(nil), f.comments[issueNumber]...), nil
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

func (f *fakeGitHub) GetPullRequest(context.Context, github.Repository, int64) (github.PullRequest, error) {
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

func (f *fakeGitHub) CreateCommitStatus(context.Context, github.CommitStatusInput) (github.CommitStatus, error) {
	return github.CommitStatus{}, errors.New("not implemented")
}

func (f *fakeGitHub) ListPullRequestFiles(context.Context, github.Repository, int64) ([]github.PullRequestFile, error) {
	return nil, errors.New("not implemented")
}

func (f *fakeGitHub) ListPullRequestCommits(context.Context, github.Repository, int64) ([]github.PullRequestCommit, error) {
	return nil, errors.New("not implemented")
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
