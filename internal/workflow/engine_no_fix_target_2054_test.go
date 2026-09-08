package workflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// TestChangesRequestedHonoursNoFixTarget is review finding F1 on #2064, and it
// is the half that made the flag a TRAP rather than merely incomplete.
//
// --no-fix-target declares that a review has no implementer and the dispatching
// operator owns the follow-up. Clearing the lead at dispatch was not enough:
// enqueue restored the reviewer through firstNonEmpty, and advancement had no
// field to read the declaration from, so a changes_requested verdict still
// dispatched a fix leg the operator had declined to authorize.
//
// CORRECTED SEVERITY, and the correction is against the original claim: this is
// NOT an independence violation via routing. dispatchFix resolves its owner with
// autoFixOwner(payload), which prefers a REGISTERED ActingOrgRole and otherwise
// derives the implementer from prior implement jobs; it never reads
// payload.LeadAgent. Measured by another seat with lead and prior implementer as
// different agents: the fix went to the prior implementer, not the lead. So the
// restored lead was a FALSE ATTRIBUTION RECORD in a field auditors read, and the
// defect this test pins is the unauthorized leg itself.
//
// The declaration therefore has to live on the PAYLOAD and be honoured HERE,
// where the leg is dispatched, not only at the CLI seam.
func TestChangesRequestedHonoursNoFixTarget(t *testing.T) {
	ctx := context.Background()
	store, engine := reviewRefanoutFixture(t)
	enableAutoFix(t, store, 1678)

	// A review dispatched --no-fix-target: no lead, and the declaration carried
	// on its own payload.
	// Mirrors insertPriorReviewResult exactly, EXCEPT no LeadAgent and
	// NoFixTarget set: the difference is the subject.
	result := AgentResult{
		Decision: "changes_requested",
		Summary:  "A finding with nobody assigned to fix it.",
		Findings: []json.RawMessage{
			json.RawMessage(`{"id":"F-1","file":"internal/boundary.go","summary":"Unguarded on the failure path."}`),
		},
	}
	insertCompletedJob(t, store, db.Job{ID: "review-only-verdict", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		Branch:      "task-1678",
		PullRequest: 1678,
		HeadSHA:     "head-one",
		TaskID:      "task-1678",
		TaskTitle:   "Bound native review fanout",
		Reviewers:   []string{"audit"},
		ReviewRound: "review-1",
		NoFixTarget: true,
		Result:      &result,
	})
	if err := engine.AdvanceJob(ctx, "review-only-verdict"); err != nil {
		t.Fatalf("AdvanceJob: %v", err)
	}

	if _, err := store.GetJob(ctx, "implement-lead-task-1678-review-1"); err == nil {
		t.Fatal("a fix leg was dispatched for a review declared --no-fix-target; " +
			"the operator declined to name an implementer and the fallback is the reviewer itself")
	}

	// THE SKIP MUST BE ON THE RECORD, for the same reason the active-leg skip is:
	// an operator reads "no fix leg" as "nothing to fix" unless the reason is
	// findable.
	events, err := store.ListJobEvents(ctx, "review-only-verdict")
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	var skip db.JobEvent
	for _, event := range events {
		if event.Kind == "auto_fix_skipped_no_fix_target" {
			skip = event
		}
	}
	if skip.Kind == "" {
		t.Fatalf("no auto_fix_skipped_no_fix_target event; the skip is invisible. events = %+v", events)
	}
	for _, want := range []string{"no-fix-target", "findings stay open"} {
		if !strings.Contains(skip.Message, want) {
			t.Fatalf("skip message %q does not name %q", skip.Message, want)
		}
	}
}

// TestDelegatedReviewChildInheritsNoFixTarget is review finding F1 on #2064's
// second cycle, and it is the escape the leaf test above cannot see.
//
// delegationRequest copies the review context - LeadAgent, Reviewers,
// ReviewRound, ActingOrgRole - and did not copy NoFixTarget. So a review-only
// ROOT could fan out to a delegated review child whose payload said
// no_fix_target=false, and that child's changes_requested advanced through its
// own native review path with dispatchFix taking no skip. With auto-fix enabled
// the declaration held at the root and leaked ONE LEVEL DOWN, which is worse
// than not having the flag: the operator is told there is no fix target and a
// leg is dispatched anyway from a job they never dispatched.
//
// The inheritance belongs in prepareEnqueue and not in delegationRequest,
// because that is the seam every review-creating request in the tree already
// passes through - the same argument the skip-native-review-fanout intent makes
// beside it. Asserting the STORED payload is therefore the contract: a test
// against delegationRequest's returned struct would pass with the bug present.
func TestDelegatedReviewChildInheritsNoFixTarget(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{store: store, resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree")}

	parentPayload, err := json.Marshal(JobPayload{
		Repo:        "gitmoot/gitmoot",
		Branch:      "task-2054",
		PullRequest: 2054,
		TaskID:      "task-2054",
		NoFixTarget: true,
	})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if err := store.CreateJob(ctx, db.Job{
		ID:      "review-only-root",
		Agent:   "audit",
		Type:    "review",
		State:   string(JobSucceeded),
		Payload: string(parentPayload),
	}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	// The child as delegationRequest builds it: review context copied, and the
	// declaration absent because the caller never carried it.
	if _, err := mailbox.Enqueue(ctx, JobRequest{
		ID:           "review-only-root/delegation/leg-1",
		Agent:        "audit",
		Action:       "review",
		Repo:         "gitmoot/gitmoot",
		Branch:       "task-2054",
		PullRequest:  2054,
		TaskID:       "task-2054",
		ParentJobID:  "review-only-root",
		DelegationID: "leg-1",
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	child, err := store.GetJob(ctx, "review-only-root/delegation/leg-1")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	childPayload, err := unmarshalPayload(child.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload: %v", err)
	}
	if !childPayload.NoFixTarget {
		t.Fatalf("delegated review child dropped the no-fix-target declaration; "+
			"its changes_requested can enqueue an implement leg the operator declined to authorize. payload = %s", child.Payload)
	}
}
