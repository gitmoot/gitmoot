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
	events, err := store.ListJobEvents(ctx, jobID)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if len(events) != 2 || events[0].Kind != string(workflow.JobQueued) || events[1].Kind != "routed" {
		t.Fatalf("events = %+v", events)
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
	}, db.JobEvent{Kind: string(workflow.JobQueued), Message: "already routed by engine"}); err != nil {
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
	if _, err := store.GetJob(ctx, "review-audit-task-7-review-2"); err == nil {
		t.Fatal("already routed pull request created a duplicate review round")
	}
	if pr, err := store.GetPullRequest(ctx, repo.FullName(), 7); err != nil || pr.HeadSHA != "abc123" {
		t.Fatalf("stored pull request = %+v err=%v", pr, err)
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

func (f *fakeGitHub) ListPullRequests(context.Context, github.Repository, string) ([]github.PullRequest, error) {
	f.listPullRequestsCalls++
	if len(f.listPullRequestsErrs) > 0 {
		err := f.listPullRequestsErrs[0]
		f.listPullRequestsErrs = f.listPullRequestsErrs[1:]
		if err != nil {
			return nil, err
		}
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

func (f *fakeGitHub) GetCombinedStatus(context.Context, github.Repository, string) (github.CombinedStatus, error) {
	return github.CombinedStatus{}, errors.New("not implemented")
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
