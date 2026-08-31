package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// TestPollOnceSupersedesQueuedLegsWhosePullRequestClosed drives #1673 through the
// real entry point: legs queued for a PR that has since merged are terminated with a
// legible reason, and every class that must outlive the PR is left alone. The
// negative cases are the load-bearing half — a sweep that is too wide silently
// discards work somebody asked for.
func TestPollOnceSupersedesQueuedLegsWhosePullRequestClosed(t *testing.T) {
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
	// PR 7 is merged (absent from the open listing); PR 9 is still open.
	client := &fakeGitHub{
		pulls: []github.PullRequest{{
			Number: 9, Title: "Open work", State: "open", URL: "https://github.com/gitmoot/gitmoot/pull/9",
			HeadRef: "task-9", BaseRef: "main", HeadSHA: "head-nine",
		}},
		comments: map[int64][]github.IssueComment{9: {}},
	}

	seedQueuedJob(t, store, "stranded-review", "audit", "review", workflow.JobPayload{
		Repo: repo.FullName(), Branch: "task-7", PullRequest: 7, TaskID: "task-7", LeadAgent: "lead",
	})
	seedQueuedJob(t, store, "stranded-implement", "lead", "implement", workflow.JobPayload{
		Repo: repo.FullName(), Branch: "task-7", PullRequest: 7, TaskID: "task-7", LeadAgent: "lead",
	})
	seedQueuedJob(t, store, "open-pr-review", "audit", "review", workflow.JobPayload{
		// HeadSHA matches the open PR's head so the pre-existing stale-head
		// supersession has nothing to act on: this row must survive on the merits.
		Repo: repo.FullName(), Branch: "task-9", PullRequest: 9, HeadSHA: "head-nine",
		TaskID: "task-9", LeadAgent: "lead",
	})
	seedQueuedJob(t, store, "other-repo-review", "audit", "review", workflow.JobPayload{
		// PR numbers are not repo-qualified: #7 exists in every repo.
		Repo: "gitmoot/other", Branch: "task-7", PullRequest: 7, TaskID: "task-7", LeadAgent: "lead",
	})
	seedQueuedJob(t, store, "no-pr-recorded", "lead", "implement", workflow.JobPayload{
		// jobs.pull_request was never backfilled, so 0 means "no PR recorded".
		Repo: repo.FullName(), Branch: "task-7", PullRequest: 0, TaskID: "task-7", LeadAgent: "lead",
	})
	seedQueuedJob(t, store, "pipeline-stage", "audit", "review", workflow.JobPayload{
		Repo: repo.FullName(), Branch: "task-7", PullRequest: 7, TaskID: "task-7", LeadAgent: "lead",
		Sender: workflow.PipelineJobSender,
	})
	seedQueuedJob(t, store, "merge-back-summary", "audit", "ask", workflow.JobPayload{
		Repo: repo.FullName(), Branch: "task-7", PullRequest: 7, TaskID: "task-7", LeadAgent: "lead",
		DelegationReason: "temp_worker_merge_back",
	})
	seedQueuedJob(t, store, "human-comment-ask", "audit", "ask", workflow.JobPayload{
		Repo: repo.FullName(), Branch: "task-7", PullRequest: 7, TaskID: "task-7", LeadAgent: "lead",
		Sender: "jerryfane",
	})
	if err := store.AddJobEvent(ctx, db.JobEvent{
		JobID:   "human-comment-ask",
		Kind:    "routed",
		Message: "routed from PR #7 comment 42 by jerryfane",
	}); err != nil {
		t.Fatalf("AddJobEvent routed: %v", err)
	}
	// A coordinator continuation synthesizes work that already happened; its id is
	// deterministically parent + "/continuation" and it carries the parent as
	// ParentJobID, which is what the exemption keys on. Without the exemption it
	// would take the delegation-child path and be failed instead.
	seedQueuedJob(t, store, "coordinator-job/continuation", "lead", "ask", workflow.JobPayload{
		Repo: repo.FullName(), Branch: "task-7", PullRequest: 7, TaskID: "task-7", LeadAgent: "lead",
		ParentJobID: "coordinator-job",
	})

	engine := workflow.Engine{Store: store}
	daemon := Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}
	for poll := range 2 {
		if err := daemon.PollOnce(ctx); err != nil {
			t.Fatalf("PollOnce %d: %v", poll+1, err)
		}
	}

	for _, tc := range []struct {
		id    string
		state workflow.JobState
		why   string
	}{
		{"stranded-review", workflow.JobCancelled, "queued review for a merged PR"},
		{"stranded-implement", workflow.JobCancelled, "queued implement for a merged PR"},
		{"open-pr-review", workflow.JobQueued, "its PR is still open"},
		{"other-repo-review", workflow.JobQueued, "PR #7 of another repo"},
		{"no-pr-recorded", workflow.JobQueued, "no PR recorded on the payload"},
		{"pipeline-stage", workflow.JobQueued, "pipeline stage jobs own run rows"},
		{"merge-back-summary", workflow.JobQueued, "a merge-back describes work that already ran"},
		{"human-comment-ask", workflow.JobQueued, "an operator asked for it explicitly"},
		{"coordinator-job/continuation", workflow.JobQueued, "a continuation synthesizes work that already happened"},
	} {
		job, err := store.GetJob(ctx, tc.id)
		if err != nil {
			t.Fatalf("GetJob(%s): %v", tc.id, err)
		}
		if job.State != string(tc.state) {
			t.Fatalf("%s state = %q, want %q (%s)", tc.id, job.State, tc.state, tc.why)
		}
	}

	// The terminal rows must be readable, and exactly once per job.
	for _, id := range []string{"stranded-review", "stranded-implement"} {
		events, err := store.ListJobEvents(ctx, id)
		if err != nil {
			t.Fatalf("ListJobEvents(%s): %v", id, err)
		}
		superseded := 0
		for _, event := range events {
			if event.Kind == workflow.JobEventSupersededPullRequestClosed {
				superseded++
				if !containsAll(event.Message, "pull request #7", "no longer open") {
					t.Fatalf("%s event message = %q, want the closed PR named", id, event.Message)
				}
			}
		}
		if superseded != 1 {
			t.Fatalf("%s %s events = %d, want 1 across two polls", id, workflow.JobEventSupersededPullRequestClosed, superseded)
		}
	}
}

// TestPollOnceRoutesQueuedDelegationChildToTheChildPath pins the daemon-side wiring
// of the two terminal paths. A child with a LIVE coordinator must not be cancelled:
// finalizeTimedOutJob's state gate rejects `cancelled`, so a cancelled child would
// leave its coordinator waiting and the strand would move up one level. An ORPHANED
// child takes the top-level path instead, because walking to a parent that does not
// exist fails every poll — a permanent error is the camouflage this sweep removes.
func TestPollOnceRoutesQueuedDelegationChildToTheChildPath(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	for _, agent := range []db.Agent{
		{Name: "coord", Role: "coordinator", Runtime: "codex", RuntimeRef: "last", RepoScope: repo.FullName(), Capabilities: []string{"ask"}, AutonomyPolicy: "auto", HealthStatus: "ok"},
		{Name: "audit", Role: "reviewer", Runtime: "codex", RuntimeRef: "last", RepoScope: repo.FullName(), Capabilities: []string{"review"}, AutonomyPolicy: "auto", HealthStatus: "ok"},
	} {
		if err := store.UpsertAgent(ctx, agent); err != nil {
			t.Fatalf("UpsertAgent %s: %v", agent.Name, err)
		}
	}
	// PR 7 is absent from the open listing: merged underneath its fan-out.
	client := &fakeGitHub{pulls: nil, comments: map[int64][]github.IssueComment{}}

	coordinator := "review-coordinator/task-7/review-1"
	coordinatorPayload, err := json.Marshal(workflow.JobPayload{
		Repo: repo.FullName(), Branch: "task-7", PullRequest: 7, TaskID: "task-7", LeadAgent: "lead",
		Result: &workflow.AgentResult{
			Decision: "approved",
			Summary:  "fan out",
			Delegations: []workflow.Delegation{
				{ID: "correctness", Agent: "audit", Action: "review", Prompt: "review correctness"},
			},
		},
	})
	if err != nil {
		t.Fatalf("json.Marshal(coordinator): %v", err)
	}
	if err := store.CreateJob(ctx, db.Job{
		ID: coordinator, Agent: "coord", Type: "ask", State: string(workflow.JobSucceeded), Payload: string(coordinatorPayload),
	}); err != nil {
		t.Fatalf("CreateJob(coordinator): %v", err)
	}
	seedQueuedJob(t, store, coordinator+"/delegation/correctness", "audit", "review", workflow.JobPayload{
		Repo: repo.FullName(), Branch: "task-7", PullRequest: 7, TaskID: "task-7", LeadAgent: "lead",
		ParentJobID: coordinator, DelegationID: "correctness",
	})
	// Same shape, but its coordinator row does not exist.
	seedQueuedJob(t, store, "missing-coordinator/delegation/security", "audit", "review", workflow.JobPayload{
		Repo: repo.FullName(), Branch: "task-7", PullRequest: 7, TaskID: "task-7", LeadAgent: "lead",
		ParentJobID: "missing-coordinator", DelegationID: "security",
	})

	engine := workflow.Engine{Store: store}
	daemon := Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}
	for poll := range 2 {
		if err := daemon.PollOnce(ctx); err != nil {
			t.Fatalf("PollOnce %d: %v", poll+1, err)
		}
	}

	for _, tc := range []struct {
		id    string
		state workflow.JobState
		why   string
	}{
		{coordinator + "/delegation/correctness", workflow.JobFailed, "a cancelled child cannot advance its coordinator"},
		{"missing-coordinator/delegation/security", workflow.JobCancelled, "an orphaned child has no coordinator to release"},
	} {
		job, err := store.GetJob(ctx, tc.id)
		if err != nil {
			t.Fatalf("GetJob(%s): %v", tc.id, err)
		}
		if job.State != string(tc.state) {
			t.Fatalf("%s state = %q, want %q (%s)", tc.id, job.State, tc.state, tc.why)
		}
		events, err := store.ListJobEvents(ctx, tc.id)
		if err != nil {
			t.Fatalf("ListJobEvents(%s): %v", tc.id, err)
		}
		legible := 0
		for _, event := range events {
			if event.Kind == workflow.JobEventSupersededPullRequestClosed {
				legible++
			}
		}
		if legible != 1 {
			t.Fatalf("%s %s events = %d, want 1", tc.id, workflow.JobEventSupersededPullRequestClosed, legible)
		}
	}
}

// TestPollOnceLeavesQueuedLegsAloneWhenTheForgeListingFails pins the fail-closed
// property the sweep depends on: an empty open set must never be inferred from a
// failed read, because everything PR-bound would then look closed.
func TestPollOnceLeavesQueuedLegsAloneWhenTheForgeListingFails(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name: "audit", Role: "reviewer", Runtime: "codex", RuntimeRef: "last",
		RepoScope: repo.FullName(), Capabilities: []string{"review"}, AutonomyPolicy: "auto", HealthStatus: "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent: %v", err)
	}
	seedQueuedJob(t, store, "queued-review", "audit", "review", workflow.JobPayload{
		Repo: repo.FullName(), Branch: "task-7", PullRequest: 7, TaskID: "task-7", LeadAgent: "lead",
	})
	client := &fakeGitHub{listPullRequestsErrs: []error{context.DeadlineExceeded}}
	engine := workflow.Engine{Store: store}
	daemon := Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}

	if err := daemon.PollOnce(ctx); err == nil {
		t.Fatal("PollOnce returned nil, want the forge listing failure surfaced")
	}
	job, err := store.GetJob(ctx, "queued-review")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.State != string(workflow.JobQueued) {
		t.Fatalf("state = %q, want queued: a failed listing is not evidence a PR closed", job.State)
	}
}

func seedQueuedJob(t *testing.T, store *db.Store, id, agent, jobType string, payload workflow.JobPayload) {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("json.Marshal(%s): %v", id, err)
	}
	if err := store.CreateJob(context.Background(), db.Job{
		ID: id, Agent: agent, Type: jobType, State: string(workflow.JobQueued), Payload: string(encoded),
	}); err != nil {
		t.Fatalf("CreateJob(%s): %v", id, err)
	}
}

func containsAll(text string, parts ...string) bool {
	for _, part := range parts {
		if !strings.Contains(text, part) {
			return false
		}
	}
	return true
}
