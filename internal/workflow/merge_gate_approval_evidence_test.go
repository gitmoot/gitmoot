package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// TestMergeGateRecordsApprovalEvidence is #1839's consumer test.
//
// A review found that EvidenceWasExecuted had exactly ONE production caller -
// the result check that deliberately ignores it for static_only - so the
// executed/static-only difference was durable JSON and nothing more, while the
// commit message claimed the gate consumed it. This drives the gate itself and
// requires the distinction to reach the durable record of the approval.
func TestMergeGateRecordsApprovalEvidence(t *testing.T) {
	for _, tc := range []struct {
		name     string
		evidence string
		want     string
		wantNot  string
	}{
		{name: "static_only_is_named", evidence: EvidenceStaticOnly, want: "evidence=static_only", wantNot: "(executed)"},
		{name: "executed_is_named", evidence: EvidenceExecuted, want: "evidence=executed"},
		{name: "absent_reads_as_static_only", evidence: "", want: "evidence=static_only"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := openEngineStore(t)
			insertIndependentMergeGateReview(t, store, db.Job{ID: "review-1", Agent: "gm-review-opus", Type: "review"}, JobPayload{
				Repo: "mobile/app", Branch: "task-9", PullRequest: 9, HeadSHA: "head123",
				TaskID: "task-9", ReviewRound: "review-1",
				Result: &AgentResult{
					Decision: "approved",
					Summary:  "verified the head at head123 and found nothing blocking",
					TestsRun: []string{"go test ./... -> ok"},
					Evidence: tc.evidence,
				},
			})

			if err := (PolicyMergeGate{Store: store}).ensureFinalReviewCaptured(ctx, MergeRequest{
				Repo: "mobile/app", PullRequest: 9, TaskID: "task-9", Reviewer: "gm-review-opus",
			}, "head123"); err != nil {
				t.Fatalf("an approving verdict must clear the gate, got %v", err)
			}

			events, err := store.ListJobEvents(ctx, "review-1")
			if err != nil {
				t.Fatal(err)
			}
			var message string
			for _, event := range events {
				if event.Kind == mergeApprovalEvidenceEvent {
					message = event.Message
				}
			}
			if message == "" {
				t.Fatalf("the gate recorded no %s event: the executed/static-only distinction never reaches the merge decision", mergeApprovalEvidenceEvent)
			}
			if !strings.Contains(message, tc.want) {
				t.Errorf("event %q does not carry %q", message, tc.want)
			}
			if tc.wantNot != "" && strings.Contains(message, tc.wantNot) {
				t.Errorf("event %q claims execution for a %s verdict", message, tc.evidence)
			}
			if !strings.Contains(message, "gm-review-opus") || !strings.Contains(message, "head123") {
				t.Errorf("event %q does not name the reviewer and the head it approved", message)
			}
		})
	}
}

// TestMergeGateRecordsNothingForARefusedApproval pins the F2 defect: the
// annotation used to fire the moment a row LOOKED approved, before the gate
// could refuse it. Measured then: an approval the gate rejects still wrote
// "approval by selfy ... evidence=executed", so the durable record could not
// tell an approval that authorised a merge from one that was refused - the very
// ambiguity the field exists to remove.
func TestMergeGateRecordsNothingForARefusedApproval(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	// An approval with NO recorded reviewer author, which the gate refuses
	// because an independent reviewer cannot be verified. (A literal
	// self-approval cannot be built through this helper: it renames the
	// implementer whenever the reviewer would collide with it.)
	insertIndependentMergeGateReview(t, store, db.Job{ID: "review-self", Agent: "", Type: "review"}, JobPayload{
		Repo: "mobile/app", Branch: "task-9", PullRequest: 9, HeadSHA: "head123",
		TaskID: "task-9", ReviewRound: "review-1",
		Result: &AgentResult{
			Decision: "approved", Summary: "approved at head123 with no recorded author, which the gate must refuse",
			TestsRun: []string{"go test ./... -> ok"}, Evidence: EvidenceExecuted,
		},
	})

	err := (PolicyMergeGate{Store: store}).ensureFinalReviewCaptured(ctx, MergeRequest{
		Repo: "mobile/app", PullRequest: 9, TaskID: "task-9",
	}, "head123")
	if err == nil {
		t.Fatal("an unattributed approval must not clear the gate; this test's premise is broken")
	}

	events, listErr := store.ListJobEvents(ctx, "review-self")
	if listErr != nil {
		t.Fatal(listErr)
	}
	for _, event := range events {
		if event.Kind == mergeApprovalEvidenceEvent {
			t.Fatalf("the gate recorded %q for an approval it REFUSED (%v): nothing was authorised", event.Message, err)
		}
	}
}

// TestMergeGateApprovalEvidenceIsRecordedOnce pins F3. ensureFinalReviewCaptured
// runs on every merge-gate pass, so a PR pending on CI is re-evaluated on every
// poll; the first version wrote one row per evaluation. The earlier test could
// not see it, because it called the gate once and kept only the LAST matching
// event - it would have passed at any duplication factor.
func TestMergeGateApprovalEvidenceIsRecordedOnce(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	insertIndependentMergeGateReview(t, store, db.Job{ID: "review-1", Agent: "gm-review-opus", Type: "review"}, JobPayload{
		Repo: "mobile/app", Branch: "task-9", PullRequest: 9, HeadSHA: "head123",
		TaskID: "task-9", ReviewRound: "review-1",
		Result: &AgentResult{
			Decision: "approved", Summary: "verified the head at head123",
			TestsRun: []string{"go test ./... -> ok"}, Evidence: EvidenceExecuted,
		},
	})

	gate := PolicyMergeGate{Store: store}
	for i := 0; i < 5; i++ {
		if err := gate.ensureFinalReviewCaptured(ctx, MergeRequest{
			Repo: "mobile/app", PullRequest: 9, TaskID: "task-9", Reviewer: "gm-review-opus",
		}, "head123"); err != nil {
			t.Fatalf("evaluation %d: %v", i, err)
		}
	}

	events, err := store.ListJobEvents(ctx, "review-1")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if event.Kind == mergeApprovalEvidenceEvent {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("5 gate evaluations of ONE approval produced %d annotations, want exactly 1: the row count scales with poll count, not approvals", count)
	}
}
