package daemon

import (
	"context"
	"encoding/json"
	"errors"
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
		// The sweep revalidates each candidate immediately before terminalizing it
		// (#1673), so the fixture must state PR 7's CURRENT state rather than only
		// leaving it out of the open listing. That is the test's own premise made
		// explicit: #7 is merged.
		pullsByNumber: map[int64]github.PullRequest{
			7: {Number: 7, State: "closed", HeadRef: "task-7", BaseRef: "main", Merged: true},
			9: {Number: 9, State: "open", HeadRef: "task-9", BaseRef: "main", HeadSHA: "head-nine"},
		},
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

// TestPollOnceLeavesWorkAloneWhenThePullRequestReopenedAfterTheListing is Finding A of
// the #1763 exact-head review. The open-PR snapshot is taken at the top of PollOnce and
// the cancellation sweep runs arbitrarily later in the same tick, so a PR REOPENED in
// that window - or created with its queued job in that window - is genuinely open at
// mutation time while absent from the older map.
//
// Complete-or-error pagination does not cover this: completeness is not CURRENCY. The
// sweep therefore revalidates each candidate immediately before the irreversible
// transition.
//
// SEMANTIC REVERSION THIS KILLS: trust the snapshot (drop the revalidation, or let a
// revalidation error fall through to cancellation) and this valid queued work is
// silently terminalized.
func TestPollOnceLeavesWorkAloneWhenThePullRequestReopenedAfterTheListing(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name: "audit", Role: "reviewer", Runtime: "codex", RuntimeRef: "last",
		RepoScope: repo.FullName(), Capabilities: []string{"review"},
		AutonomyPolicy: "auto", HealthStatus: "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent: %v", err)
	}

	// THE RACE, expressed exactly: PR 11 is ABSENT from the open listing the poll reads
	// at the top, and OPEN when the sweep revalidates it a moment later.
	client := &fakeGitHub{
		pulls:         []github.PullRequest{},
		comments:      map[int64][]github.IssueComment{},
		pullsByNumber: map[int64]github.PullRequest{11: {Number: 11, State: "open", HeadRef: "task-11", BaseRef: "main"}},
	}

	seedQueuedJob(t, store, "reopened-review", "audit", "review", workflow.JobPayload{
		Repo:        repo.FullName(),
		Branch:      "task-11",
		PullRequest: 11,
		TaskID:      "task-11",
		Sender:      "audit",
	})

	engine := workflow.Engine{Store: store}
	daemon := Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}
	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}

	job, err := store.GetJob(ctx, "reopened-review")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.State != string(workflow.JobQueued) {
		t.Fatalf("job state = %q, want queued: the sweep terminalized work for a PR that is OPEN at mutation time", job.State)
	}
	// The revalidation actually happened - a passing assertion above with zero targeted
	// reads would mean the candidate never reached the guard.
	if len(client.getPullRequestCalls) == 0 {
		t.Fatal("no targeted revalidation read: this test cannot observe the guard")
	}
}

// TestPollOnceLeavesWorkQueuedWhenRevalidationFails is the FAIL-CLOSED half, and it is
// what stops the new forge call from becoming a new way to lose work: if the
// revalidation itself errors, the sweep must leave the job queued and surface the
// error, never fall through to cancellation.
func TestPollOnceLeavesWorkQueuedWhenRevalidationFails(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name: "audit", Role: "reviewer", Runtime: "codex", RuntimeRef: "last",
		RepoScope: repo.FullName(), Capabilities: []string{"review"},
		AutonomyPolicy: "auto", HealthStatus: "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent: %v", err)
	}
	client := &fakeGitHub{
		pulls:    []github.PullRequest{},
		comments: map[int64][]github.IssueComment{},
		// A TRANSIENT signature, deliberately, because that is what this test names. The
		// generic "forge unavailable" it used before is indistinguishable from the normal
		// not-found a 404 produces for an issue number, and reporting THAT on every poll
		// reds repos.last_error forever and first-wins-masks later reconcilers. The two
		// arms are now separated: this one must surface, the permanent one is recorded
		// once and stays out of the poll's error (see
		// TestPollOnceAsksTheForgeOncePerNumberPerPoll).
		getPullRequestErr: errors.New("dial tcp: connection refused"),
	}
	seedQueuedJob(t, store, "unproven-review", "audit", "review", workflow.JobPayload{
		Repo:        repo.FullName(),
		Branch:      "task-12",
		PullRequest: 12,
		TaskID:      "task-12",
		Sender:      "audit",
	})

	engine := workflow.Engine{Store: store}
	daemon := Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}
	if err := daemon.PollOnce(ctx); err == nil {
		t.Fatal("PollOnce returned nil: a revalidation failure must be surfaced, not swallowed")
	}
	job, err := store.GetJob(ctx, "unproven-review")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.State != string(workflow.JobQueued) {
		t.Fatalf("job state = %q, want queued: unproven closure must never terminalize work", job.State)
	}
}
