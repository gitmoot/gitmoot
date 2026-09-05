package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
)

// attributionIdentityGate builds the same all-green gate the attribution-cause
// table uses, so these tests differ from it in exactly one variable: WHO is
// recorded as the implementer. Everything else - CI, mergeability, the approving
// review - is held identical, which is what makes a changed verdict attributable
// to the identity rather than to the environment.
func attributionIdentityGate(t *testing.T, store *db.Store) (PolicyMergeGate, *fakeMergeGateGitHub) {
	t.Helper()
	mergeable := true
	gh := &fakeMergeGateGitHub{
		pr: github.PullRequest{
			Number: 9, State: "open", HeadRef: "task-9", BaseRef: "main",
			HeadSHA: "head123", Mergeable: &mergeable,
		},
		status:      github.CombinedStatus{State: "success", Statuses: []github.CommitStatus{{Context: "ci", State: "success"}}},
		checks:      []github.PullRequestCheck{{Name: "ci", Bucket: "pass", State: "SUCCESS"}},
		mergeResult: github.MergeResult{Merged: true, SHA: "merge123"},
	}
	return PolicyMergeGate{AutoMerge: true, Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}, gh
}

// seedRoleImplementedTask records the shape #1916 actually produced: an implement
// job for this task whose work was done IN SESSION by an org role, so there is no
// registered agent to name. The job row's agent column is empty BY CONSTRUCTION -
// naming an agent here would be the lie the whole issue is about - and the acting
// role travels in the payload, where every local ingress already puts it.
func seedRoleImplementedTask(t *testing.T, store *db.Store, base JobPayload, role string) {
	t.Helper()
	payload := base
	payload.ActingOrgRole = role
	insertCompletedJob(t, store, db.Job{ID: "implement-in-session", Type: "implement"}, payload)
}

func attributionIdentityReviewPayload(base JobPayload) JobPayload {
	review := base
	review.HeadSHA = "head123"
	review.ReviewRound = "review-1"
	review.Result = &AgentResult{Decision: "approved", Summary: "approved"}
	return review
}

// TestRoleImplementedTaskIsAttributable is the #1718 regression. Before the fix
// the gate reports "matches this task but has no recorded agent" and fails closed,
// because collectImplementerAttribution reads only job.Agent and an in-session
// role leaves it empty. The remedy the gate itself printed was unrunnable: `job
// record --agent gitmoot` is refused, gitmoot being an org role and not one of the
// registered agents.
func TestRoleImplementedTaskIsAttributable(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	base := JobPayload{
		Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9",
		Result: &AgentResult{Decision: "implemented", Summary: "implemented"},
	}
	seedRoleImplementedTask(t, store, base, "gitmoot")
	insertCompletedJob(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"},
		attributionIdentityReviewPayload(base))

	gate, _ := attributionIdentityGate(t, store)
	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9"})
	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	rendered := decision.Reason.Render()
	for _, forbidden := range []string{
		emptyImplementAgentAttributionReason,
		noImplementJobAttributionReason,
	} {
		if strings.Contains(rendered, forbidden) {
			t.Errorf("gate reported an attribution gap for role-implemented work: %q", rendered)
		}
	}
	if decision.LeaveOpen {
		t.Errorf("decision = %+v, want the role-attributed implementer to satisfy attribution; reason %q", decision, rendered)
	}
}

// TestRoleImplementerStillRefusesSelfApproval is the half that must NOT get easier.
// Making role work attributable would be worthless - worse than the gap - if it let
// the implementer approve itself. A reviewer is always a dispatched agent, so the
// comparison is by NAME ACROSS KINDS: one name is treated as one actor.
//
// IT ASSERTS THE CAUSE, NOT MERELY THE REFUSAL, and that is not a style preference.
// Written against LeaveOpen alone it PASSED BEFORE THE FIX - refused because
// attribution was EMPTY, not because self-approval was detected - so it would have
// reported a working guard while the guard did not exist. Pinning the self-approval
// text is what makes it fail before and pass after.
func TestRoleImplementerStillRefusesSelfApproval(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	base := JobPayload{
		Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9",
		Result: &AgentResult{Decision: "implemented", Summary: "implemented"},
	}
	// the acting role and the reviewing agent share a name
	seedRoleImplementedTask(t, store, base, "audit")
	insertCompletedJob(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"},
		attributionIdentityReviewPayload(base))

	gate, gh := attributionIdentityGate(t, store)
	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9"})
	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	rendered := decision.Reason.Render()
	if !decision.LeaveOpen || decision.Merged {
		t.Fatalf("decision = %+v, want a self-approval refusal when the reviewer's name is the implementing role", decision)
	}
	if len(gh.merges) != 0 {
		t.Fatalf("self-approved role work was merged: %+v", gh.merges)
	}
	if !strings.Contains(rendered, "the implementing agent; an independent reviewer is required") {
		t.Errorf("reason = %q, want the SELF-APPROVAL cause; refusing for a mere attribution gap would leave this guard vacuous", rendered)
	}
	if strings.Contains(rendered, emptyImplementAgentAttributionReason) {
		t.Errorf("reason = %q, want self-approval detected rather than the role read as no attribution at all", rendered)
	}
}

// TestEmptyAttributionStillNamesTheAnomaly is the control for the first test: with
// NEITHER an agent nor an acting role, the pre-existing anomaly must still be
// reported. Without this, widening attribution to accept a role would be
// indistinguishable from accepting anything.
func TestEmptyAttributionStillNamesTheAnomaly(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	base := JobPayload{
		Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9",
		Result: &AgentResult{Decision: "implemented", Summary: "implemented"},
	}
	insertCompletedJob(t, store, db.Job{ID: "implement-nobody", Type: "implement"}, base)
	insertCompletedJob(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"},
		attributionIdentityReviewPayload(base))

	gate, _ := attributionIdentityGate(t, store)
	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9"})
	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.LeaveOpen {
		t.Fatalf("decision = %+v, want the unattributable implement row still refused", decision)
	}
	if !strings.Contains(decision.Reason.Render(), emptyImplementAgentAttributionReason) {
		t.Errorf("reason = %q, want the empty-agent anomaly still named", decision.Reason.Render())
	}
}
