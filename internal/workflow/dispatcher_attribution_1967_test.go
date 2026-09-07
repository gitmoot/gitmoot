package workflow

import (
	"context"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// #1967. review_finding_observations records observer_job (the reviewer that
// FOUND a finding) and source_job (the job whose claim it repeats). Nothing
// recorded who ASKED for the review, so the only identity a finding could be
// routed by was the reviewer agent name - the misrouting mechanism on #1890.
//
// Measured on this box's live store on 2026-09-07, joining every open finding
// to its observing job: 271 open findings, 271 matched a jobs row, 3 had any
// dispatcher field set, and 268 resolved to a job whose entire recorded
// identity was Sender: "local".
//
// READER 1 of 2 - the in-process PR-open trigger. The implement job that just
// opened the PR is the dispatcher, and it is the ADVANCING job's agent, not the
// reviewer that will answer.
func TestInProcessPROpenRecordsTheDispatchingSeatOnFanoutChildren(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	seedAgent(t, store, "reviewer", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	engine.RequiredReviewers = []string{"reviewer"}

	if acquired, err := store.AcquireLock(ctx, db.BranchLock{
		RepoFullName: "gitmoot/gitmoot",
		Branch:       "task-1967",
		Owner:        "lead",
	}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	insertCompletedJob(t, store, db.Job{
		ID:    "dispatching-implement",
		Agent: "lead",
		Type:  "implement",
	}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		Branch:      "task-1967",
		PullRequest: 1967,
		HeadSHA:     "head1967",
		TaskID:      "task-1967",
		LeadAgent:   "lead",
		Result:      &AgentResult{Decision: "implemented", Summary: "opened PR"},
	})

	if err := engine.AdvanceJob(ctx, "dispatching-implement"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}

	review := onlyReviewJob(t, ctx, store)
	if review.DispatchedBy != "lead" {
		t.Fatalf("fanout child dispatched_by = %q, want %q; an unattributed review finding can only be routed by the reviewer's own name (#1890)", review.DispatchedBy, "lead")
	}
	if review.DispatchedBy == review.Agent {
		t.Fatalf("fanout child dispatched_by = %q equals the reviewer agent; that is the attribution #1967 exists to replace", review.DispatchedBy)
	}
}

// The read-back half. #1250 finding 3 established the failure mode this guards:
// a column written but omitted from the SELECT projection returns a
// structurally valid but FALSELY BLANK field, which reads as "nobody dispatched
// this" rather than "not loaded". jobs.root_id already has exactly that defect
// (GetJob and jobColumns omit it), so a new attribution column that only the
// writer knows about would be worth nothing to the query in #1967's acceptance.
func TestDispatcherSurvivesEveryJobReadBack(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "reviewer", []string{"review"}, "gitmoot/gitmoot")
	mailbox := NewMailbox(store, UnavailableDeliveryWorktreeResolver("test"))

	if _, err := mailbox.Enqueue(ctx, JobRequest{
		ID:           "readback-review",
		Agent:        "reviewer",
		Action:       "review",
		Repo:         "gitmoot/gitmoot",
		PullRequest:  7,
		Sender:       "github",
		DispatchedBy: "impl-seat",
		Instructions: "review it",
	}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}

	got, err := store.GetJob(ctx, "readback-review")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if got.DispatchedBy != "impl-seat" {
		t.Fatalf("GetJob dispatched_by = %q, want %q", got.DispatchedBy, "impl-seat")
	}

	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("ListJobs returned %d jobs, want 1", len(jobs))
	}
	if jobs[0].DispatchedBy != "impl-seat" {
		t.Fatalf("ListJobs dispatched_by = %q, want %q", jobs[0].DispatchedBy, "impl-seat")
	}
}

// The chokepoint resolution. Six independent JobRequest literals create review
// jobs and none named a dispatcher field; fixing the six leaves the seventh
// unattributed, so the resolution lives at Enqueue - the same argument #1277
// made for inheriting the skip-fanout intent there.
//
// The two "never" rows are the point of the test, not padding: on a CLI review
// dispatch LeadAgent defaults to the reviewer agent itself
// (agent_dispatch.go: firstNonEmpty(request.LeadAgent, agent.Name)), so a
// fallback to either Agent or LeadAgent would make the column look answered
// while carrying exactly the identity #1890 misroutes on.
func TestEnqueueResolvesTheDispatcherFromTheMostSpecificSourceAvailable(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "reviewer", []string{"review"}, "gitmoot/gitmoot")
	mailbox := NewMailbox(store, UnavailableDeliveryWorktreeResolver("test"))

	for _, tc := range []struct {
		name    string
		request JobRequest
		want    string
	}{
		{
			name:    "explicit dispatcher wins",
			request: JobRequest{DispatchedBy: "impl-seat", DelegatedBy: "coordinator", ActingOrgRole: "role", Sender: "github"},
			want:    "impl-seat",
		},
		{
			name:    "delegation child is attributed to its delegating coordinator",
			request: JobRequest{DelegatedBy: "coordinator", ActingOrgRole: "role", Sender: "coordinator-agent"},
			want:    "coordinator",
		},
		{
			name:    "a CLI dispatch is attributed to its acting org role",
			request: JobRequest{ActingOrgRole: "gm-findings", Sender: "local"},
			want:    "gm-findings",
		},
		{
			name:    "with no identity at all the channel is recorded, never nothing",
			request: JobRequest{Sender: "local"},
			want:    "local",
		},
		{
			name:    "never the reviewer agent",
			request: JobRequest{Agent: "reviewer"},
			want:    "",
		},
		{
			name:    "never the lead agent, which is the reviewer on a CLI dispatch",
			request: JobRequest{Agent: "reviewer", LeadAgent: "reviewer"},
			want:    "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := tc.request
			request.ID = "dispatch-" + t.Name()
			request.Agent = "reviewer"
			request.Action = "review"
			request.Repo = "gitmoot/gitmoot"
			request.PullRequest = 7
			request.Instructions = "review it"
			if _, err := mailbox.Enqueue(ctx, request); err != nil {
				t.Fatalf("Enqueue returned error: %v", err)
			}
			got, err := store.GetJob(ctx, request.ID)
			if err != nil {
				t.Fatalf("GetJob returned error: %v", err)
			}
			if got.DispatchedBy != tc.want {
				t.Fatalf("dispatched_by = %q, want %q", got.DispatchedBy, tc.want)
			}
		})
	}
}

// Attribution must not rewire scheduling. parent_job_id carries delegation
// depth, the per-root job budget, loop detection, root-kill propagation and the
// #1277 skip-fanout inheritance; #1967's proposed change ("populate dispatcher
// identity") would have been satisfiable by back-filling it, and that would
// silently give every fan-out review a delegation parent it never had.
func TestRecordingTheDispatcherDoesNotSynthesizeADelegationParent(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "reviewer", []string{"review"}, "gitmoot/gitmoot")
	mailbox := NewMailbox(store, UnavailableDeliveryWorktreeResolver("test"))

	if _, err := mailbox.Enqueue(ctx, JobRequest{
		ID:           "attribution-only",
		Agent:        "reviewer",
		Action:       "review",
		Repo:         "gitmoot/gitmoot",
		PullRequest:  7,
		Sender:       "github",
		DispatchedBy: "impl-seat",
		Instructions: "review it",
	}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	got, err := store.GetJob(ctx, "attribution-only")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if got.ParentJobID != "" || got.DelegationID != "" || got.DelegationDepth != 0 || got.DelegatedBy != "" {
		t.Fatalf("attribution leaked into the delegation DAG: parent=%q delegation=%q depth=%d delegated_by=%q", got.ParentJobID, got.DelegationID, got.DelegationDepth, got.DelegatedBy)
	}
}

func onlyReviewJob(t *testing.T, ctx context.Context, store *db.Store) db.Job {
	t.Helper()
	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	var reviews []db.Job
	for _, job := range jobs {
		if job.Type == "review" {
			reviews = append(reviews, job)
		}
	}
	if len(reviews) != 1 {
		t.Fatalf("found %d review jobs, want exactly 1; cannot assert attribution", len(reviews))
	}
	return reviews[0]
}
