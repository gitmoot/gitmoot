package workflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// #1533: concurrent fix legs race on git push and the loser's completed work is
// destroyed.
//
// MECHANISM, all three parts measured rather than argued:
//
//  1. a blocking verdict dispatches a fix leg REGARDLESS of whether a leg is
//     already in flight on that branch, so a two-family panel that both object
//     produces two legs seconds apart;
//  2. the branch lock cannot stop it, because Store.AcquireLock returns true
//     when the existing owner equals the requester, so one lock admits N
//     concurrent same-owner legs. Every leg in the measured races ran under the
//     same lock;
//  3. the loser's push rejection is TERMINAL: a push failure maps to
//     blockedResultDelivery with no rebase, no retry and no salvage.
//
// LIVE POPULATION, re-derived from the job store: 34 implement jobs carry
// "failed to push some refs", from 2026-08-27 to 2026-09-07. Of the 20 whose own
// window is under 6h, 14 overlapped another short-window implement leg on the
// SAME branch. The 14 long-window rows are left unattributed on purpose, since a
// 226h window makes an overlap test meaningless.
//
// The refusal that would have stopped this already existed, but only on the
// OPERATOR path: findActiveImplementJobForTask in internal/cli, reached from
// agent_dispatch. The engine's automatic fix-leg path consulted nothing.
func TestChangesRequestedDoesNotDispatchASecondFixLegOnABranchAlreadyBeingWritten(t *testing.T) {
	ctx := context.Background()
	store, engine := reviewRefanoutFixture(t)
	enableAutoFix(t, store, 1678)

	// The first family's leg, already working. This is the shape the race needs:
	// dispatched against the head the reviewers saw, minutes from its push.
	insertActiveJob(t, store, db.Job{ID: "fix-leg-family-a", Agent: "lead", Type: "implement"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		Branch:      "task-1678",
		PullRequest: 1678,
		TaskID:      "task-1678",
		LeadAgent:   "lead",
	}, JobRunning)

	// The second family's verdict arrives while that leg is still running.
	insertPriorReviewResult(t, store, "sibling-review", "head-one", AgentResult{
		Decision: "changes_requested",
		Summary:  "The same property from a different angle.",
		Findings: []json.RawMessage{
			json.RawMessage(`{"id":"F-1","file":"internal/boundary.go","summary":"Also unguarded on the failure path."}`),
		},
	})
	if err := engine.AdvanceJob(ctx, "sibling-review"); err != nil {
		t.Fatalf("AdvanceJob sibling review: %v", err)
	}

	if _, err := store.GetJob(ctx, "implement-lead-task-1678-review-1"); err == nil {
		t.Fatal("a second fix leg was dispatched onto a branch already being written; whichever leg finishes second loses the push and its completed work is discarded")
	}

	// THE SKIP MUST BE ON THE RECORD. A silent skip is the same defect class this
	// campaign exists to remove: the reason a leg was not dispatched has to be
	// findable, or an operator reads "no fix leg" as "nothing to fix".
	events, err := store.ListJobEvents(ctx, "sibling-review")
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	var skip db.JobEvent
	for _, event := range events {
		if event.Kind == "auto_fix_skipped_active_leg" {
			skip = event
		}
	}
	if skip.Kind == "" {
		t.Fatalf("no auto_fix_skipped_active_leg event recorded; the skip is invisible. events = %+v", events)
	}
	if !strings.Contains(skip.Message, "fix-leg-family-a") {
		t.Fatalf("the skip record does not name the leg that owns the branch: %q", skip.Message)
	}

	// The objection still transitioned the task, so the PR is not left looking
	// approved. Only the duplicate WRITER is refused.
	assertTaskState(t, store, "task-1678", TaskChangesRequested)
}

// The control, and the one a blunt guard breaks: with no leg in flight, the
// verdict must still dispatch its fix. A guard that simply stopped dispatching
// would pass the case above and silently disable auto-fix.
func TestChangesRequestedStillDispatchesAFixLegWhenTheBranchIsIdle(t *testing.T) {
	ctx := context.Background()
	store, engine := reviewRefanoutFixture(t)
	enableAutoFix(t, store, 1678)

	// A SETTLED leg on the same branch must not be mistaken for a live writer:
	// every fix round after the first has one of these behind it, so counting it
	// would refuse every round but the first.
	insertCompletedJob(t, store, db.Job{ID: "earlier-leg", Agent: "lead", Type: "implement"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		Branch:      "task-1678",
		PullRequest: 1678,
		TaskID:      "task-1678",
		LeadAgent:   "lead",
	})
	// An ACTIVE job on a DIFFERENT branch is not a writer of this one.
	insertActiveJob(t, store, db.Job{ID: "other-branch-leg", Agent: "lead", Type: "implement"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		Branch:      "some-other-branch",
		PullRequest: 1679,
		TaskID:      "task-1679",
		LeadAgent:   "lead",
	}, JobRunning)
	// An ACTIVE REVIEW job on this branch is not a writer either: it pushes
	// nothing, and holding the fix for it would stall every round.
	insertActiveJob(t, store, db.Job{ID: "reviewer-in-flight", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		Branch:      "task-1678",
		PullRequest: 1678,
		TaskID:      "task-1678",
		LeadAgent:   "lead",
	}, JobRunning)

	insertPriorReviewResult(t, store, "lone-review", "head-one", AgentResult{
		Decision: "changes_requested",
		Summary:  "Fix the boundary check.",
		Findings: []json.RawMessage{
			json.RawMessage(`{"id":"F-1","file":"internal/boundary.go","summary":"Fix the boundary check."}`),
		},
	})
	if err := engine.AdvanceJob(ctx, "lone-review"); err != nil {
		t.Fatalf("AdvanceJob lone review: %v", err)
	}
	fix, err := store.GetJob(ctx, "implement-lead-task-1678-review-1")
	if err != nil {
		t.Fatalf("an idle branch did not receive its fix leg, so the guard disabled auto-fix instead of bounding it: %v", err)
	}
	payload, err := unmarshalPayload(fix.Payload)
	if err != nil {
		t.Fatalf("unmarshal fix payload: %v", err)
	}
	if !strings.Contains(payload.Instructions, `"id":"F-1"`) {
		t.Fatalf("the dispatched fix lost the finding it exists for: %q", payload.Instructions)
	}
}

func insertActiveJob(t *testing.T, store *db.Store, job db.Job, payload JobPayload, state JobState) {
	t.Helper()
	encoded, err := marshalPayload(payload)
	if err != nil {
		t.Fatalf("marshalPayload: %v", err)
	}
	job.State = string(state)
	job.Payload = encoded
	if err := store.CreateJobWithEvent(context.Background(), job, db.JobEvent{Kind: string(state), Message: "in flight"}); err != nil {
		t.Fatalf("CreateJobWithEvent: %v", err)
	}
}
