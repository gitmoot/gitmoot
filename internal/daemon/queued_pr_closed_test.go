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
		// The forge is the authority: a number must BE a pull request and must not be
		// open right now. #7 is closed. #11 is a real closed PR the store never
		// recorded (a PR opened and closed between polls, a fresh home, a restored
		// database). Issue #12 is absent from here, which is what a 404 looks like.
		pullsByNumber: map[int64]github.PullRequest{
			7:  {Number: 7, State: "closed", HeadRef: "task-7", BaseRef: "main", HeadSHA: "head-seven"},
			11: {Number: 11, State: "closed", HeadRef: "task-11", BaseRef: "main", HeadSHA: "head-eleven"},
		},
	}
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: repo.FullName(), Number: 7, HeadBranch: "task-7", BaseBranch: "main",
		HeadSHA: "head-seven", State: "closed",
	}); err != nil {
		t.Fatalf("UpsertPullRequest 7: %v", err)
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
	seedQueuedJob(t, store, "issue-ask-child", "audit", "ask", workflow.JobPayload{
		// handleIssueAsk stores an ISSUE number in PullRequest, and delegationRequest
		// copies it onto children, which get no `routed` event of their own. Issue #12
		// can never appear in a PR listing, so "absent" says nothing about it.
		Repo: repo.FullName(), Branch: "task-12", PullRequest: 12, TaskID: "task-12", LeadAgent: "lead",
		ParentJobID: "issue-coordinator", DelegationID: "research",
	})
	seedQueuedJob(t, store, "cli-dispatched-review", "audit", "review", workflow.JobPayload{
		// `gitmoot agent review ... --pr 7` enqueues with Sender "local" and no
		// routed event: an operator asked for it by name.
		Repo: repo.FullName(), Branch: "task-7", PullRequest: 7, TaskID: "task-7", LeadAgent: "lead",
		Sender: "local",
	})
	seedQueuedJob(t, store, "unrecorded-pr-review", "audit", "review", workflow.JobPayload{
		// Closed on the forge, never recorded in the store: leaving it queued would
		// strand exactly the leg this sweep exists to clear, so the forge answers.
		Repo: repo.FullName(), Branch: "task-11", PullRequest: 11, TaskID: "task-11", LeadAgent: "lead",
	})
	seedQueuedJob(t, store, "pipeline-stage-unrecorded", "audit", "review", workflow.JobPayload{
		// Exempt AND its PR has no store row. If the evidence check ran before the
		// exemption check, this job would spend a forge call the sweep can never use.
		Repo: repo.FullName(), Branch: "task-11", PullRequest: 11, TaskID: "task-11", LeadAgent: "lead",
		Sender: workflow.PipelineJobSender,
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
		{"issue-ask-child", workflow.JobQueued, "issue #12 is not a pull request at all"},
		{"cli-dispatched-review", workflow.JobQueued, "an operator dispatched it by name"},
		{"unrecorded-pr-review", workflow.JobCancelled, "a closed PR the store never recorded is still a closed PR"},
		{"pipeline-stage-unrecorded", workflow.JobQueued, "exempt regardless of what the forge would say"},
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

	// Cheapest gate first: the exempt job for the same unrecorded PR must not have
	// spent a forge call, so #11 is asked about exactly once — for the one job the
	// sweep can actually act on. Two calls means the evidence check ran before the
	// exemption check.
	asked := 0
	for _, number := range client.getPullRequestCalls {
		if number == 11 {
			asked++
		}
	}
	if asked != 1 {
		t.Fatalf("forge asked about PR #11 %d times, want 1 (the exempt job must not consult the forge)", asked)
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
	// PR 7 is absent from the open listing (merged underneath its fan-out) but IS a
	// recorded pull request, which is what the sweep requires as positive evidence.
	client := &fakeGitHub{
		pulls:    nil,
		comments: map[int64][]github.IssueComment{},
		// reconcileExternallyMergedTasks looks these up for the seeded tasks.
		// #7 is CLOSED WITHOUT MERGING, so the external-merge reconciler leaves
		// task-7 alone and its coordinator is still legitimately advanceable. #8 is
		// merged, which is what drives task-8 to `merged` before the sweep runs.
		pullsByNumber: map[int64]github.PullRequest{
			7: {Number: 7, State: "closed", HeadRef: "task-7", BaseRef: "main", HeadSHA: "head-seven"},
			8: {Number: 8, State: "closed", HeadRef: "task-8", BaseRef: "main", HeadSHA: "head-eight", Merged: true},
		},
	}
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: repo.FullName(), Number: 7, HeadBranch: "task-7", BaseBranch: "main",
		HeadSHA: "head-seven", State: "closed",
	}); err != nil {
		t.Fatalf("UpsertPullRequest 7: %v", err)
	}
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: repo.FullName(), Number: 8, HeadBranch: "task-8", BaseBranch: "main",
		HeadSHA: "head-eight", State: "closed",
	}); err != nil {
		t.Fatalf("UpsertPullRequest 8: %v", err)
	}
	// task-7 is still live, so its coordinator can legitimately be advanced.
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-7", RepoFullName: repo.FullName(), GoalID: "goal-1", Title: "Task 7",
		State: string(workflow.TaskReviewing), Branch: "task-7",
	}); err != nil {
		t.Fatalf("UpsertTask task-7: %v", err)
	}
	// task-8 already MERGED. Advancing its coordinator would end in block_parent ->
	// setTaskState(blocked), rewriting the one state the sweep's own premise asserts.
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-8", RepoFullName: repo.FullName(), GoalID: "goal-1", Title: "Task 8",
		State: string(workflow.TaskMerged), Branch: "task-8",
	}); err != nil {
		t.Fatalf("UpsertTask task-8: %v", err)
	}

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
	// A child of a coordinator whose task already MERGED: terminate it, but never
	// drive an advance that would rewrite `merged` to `blocked`.
	mergedCoordinator := "review-coordinator/task-8/review-1"
	mergedCoordinatorPayload, err := json.Marshal(workflow.JobPayload{
		Repo: repo.FullName(), Branch: "task-8", PullRequest: 8, TaskID: "task-8", LeadAgent: "lead",
		Result: &workflow.AgentResult{
			Decision: "approved",
			Summary:  "fan out",
			Delegations: []workflow.Delegation{
				{ID: "correctness", Agent: "audit", Action: "review", Prompt: "review correctness"},
			},
		},
	})
	if err != nil {
		t.Fatalf("json.Marshal(mergedCoordinator): %v", err)
	}
	if err := store.CreateJob(ctx, db.Job{
		ID: mergedCoordinator, Agent: "coord", Type: "ask", State: string(workflow.JobSucceeded), Payload: string(mergedCoordinatorPayload),
	}); err != nil {
		t.Fatalf("CreateJob(mergedCoordinator): %v", err)
	}
	seedQueuedJob(t, store, mergedCoordinator+"/delegation/correctness", "audit", "review", workflow.JobPayload{
		Repo: repo.FullName(), Branch: "task-8", PullRequest: 8, TaskID: "task-8", LeadAgent: "lead",
		ParentJobID: mergedCoordinator, DelegationID: "correctness",
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
		// A merged task's coordinator must STILL be released. Refusing here traded one
		// strand for another: reconcileExternallyMergedTasks drives the task to `merged`
		// earlier in the SAME poll, so every child took the cancel path and no
		// coordinator ever advanced. Protecting `merged` is setTaskState's job.
		{mergedCoordinator + "/delegation/correctness", workflow.JobFailed, "a merged task's coordinator must still be released"},
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

	// The child must have been RELEASED into the DAG, not merely killed. The
	// load-bearing observable is the SYNTHETIC RESULT on the child: advanceDelegations
	// refuses a child whose Result is nil, which is precisely how a cancelled child
	// left its coordinator waiting forever. (advance_completed is NOT the instrument:
	// with the default block_parent policy AdvanceJob returns a BlockedError and that
	// event is never written, even though the DAG did act.)
	for _, child := range []string{coordinator + "/delegation/correctness", mergedCoordinator + "/delegation/correctness"} {
		job, err := store.GetJob(ctx, child)
		if err != nil {
			t.Fatalf("GetJob(%s): %v", child, err)
		}
		var payload workflow.JobPayload
		if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
			t.Fatalf("unmarshal %s: %v", child, err)
		}
		if payload.Result == nil {
			t.Fatalf("child %s has no result: its coordinator can never advance", child)
		}
	}

	// The live task records the DAG's decision (block_parent). That transition IS the
	// release: the coordinator is no longer waiting on a child that will never run.
	live, err := store.GetTask(ctx, "task-7")
	if err != nil {
		t.Fatalf("GetTask(task-7): %v", err)
	}
	if live.State != string(workflow.TaskBlocked) {
		t.Fatalf("task-7 state = %q, want blocked (the coordinator's failure_policy decision)", live.State)
	}

	// The merged task must still be merged: the sweep's premise is that the PR
	// merged, so undoing that state is the one outcome it can never be allowed.
	merged, err := store.GetTask(ctx, "task-8")
	if err != nil {
		t.Fatalf("GetTask(task-8): %v", err)
	}
	if merged.State != string(workflow.TaskMerged) {
		t.Fatalf("task-8 state = %q, want merged", merged.State)
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
