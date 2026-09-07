package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// #1557: review fanout lens legs fail silently. The dominant cause, measured
// rather than inferred, is that a delegated REVIEW child is created with no head
// SHA and is then correctly refused by the worker with "review job for PR #N has
// no head SHA" (internal/cli/daemon_checkout.go:469). A review that cannot name
// the commit it read is not a review, so the refusal is right and the defect is
// upstream, in creating the child at all.
//
// LIVE STORE, 2026-07-28 to 2026-09-07: 156 failed `lens-*` delegation legs, 145
// with no head, and the recorded failure reason on 135 of them is literally that
// sentence. Of the 145, 141 carry a PR number, and for ALL 141 the
// `pull_requests` mirror already held a head SHA. The head was in the store the
// whole time.
//
// The issue also reports these legs as carrying "no recorded reason at all".
// That is true of `payload.error` and `payload.failure_diagnostics`, and false of
// the record: every one of the 156 has a `failed` job event carrying its reason.
// Corrected on the issue rather than fixed here.
func TestReviewDelegationChildInheritsTheHeadFromTheStoredPullRequest(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	const head = "203263dccd20fa155a88dcd973ede08de3c2fe3d"
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: "gitmoot/gitmoot",
		Number:       1903,
		HeadBranch:   "feat/lens",
		BaseBranch:   "main",
		State:        "open",
		HeadSHA:      head,
	}); err != nil {
		t.Fatalf("UpsertPullRequest: %v", err)
	}

	// The coordinator as observed: it knows its PR but its payload carries no
	// head, which is exactly the shape that produced the silent legs.
	parent := db.Job{ID: "local-review-g6-review-sol-18d283b232f739f7", Agent: "g6-review-sol", Type: "review"}
	payload := JobPayload{
		Repo:        "gitmoot/gitmoot",
		PullRequest: 1903,
		TaskID:      "review-pr-1903",
		LeadAgent:   "lead",
	}

	child := engine.delegationRequest(ctx, parent, payload, Delegation{
		ID:     "lens-citations-durability",
		Action: "review",
		Agent:  "auditor",
		Prompt: "one lens",
	})
	if child.HeadSHA != head {
		t.Fatalf("review delegation child head = %q, want the stored pull-request head %q; without it the worker refuses the leg with \"review job for PR #%d has no head SHA\" and the lens never runs", child.HeadSHA, head, payload.PullRequest)
	}
	if !strings.HasSuffix(child.ID, "/delegation/lens-citations-durability") {
		t.Fatalf("child id = %q, want the delegation suffix preserved", child.ID)
	}
}

// Three bounds, because a head is not free to invent.
func TestDelegationHeadResolutionIsBounded(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	const mirrorHead = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: "gitmoot/gitmoot", Number: 7, HeadBranch: "b", BaseBranch: "main",
		State: "open", HeadSHA: mirrorHead,
	}); err != nil {
		t.Fatalf("UpsertPullRequest: %v", err)
	}
	parent := db.Job{ID: "parent", Agent: "coordinator", Type: "review"}

	// 1. THE PARENT'S OWN HEAD WINS. The mirror is a fallback, never an override:
	//    a coordinator pinned to an exact head must not have its children
	//    silently re-targeted at a newer one, which would be the #1522 staleness
	//    defect arriving from the other direction.
	const pinned = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	pinnedChild := engine.delegationRequest(ctx, parent, JobPayload{
		Repo: "gitmoot/gitmoot", PullRequest: 7, HeadSHA: pinned,
	}, Delegation{ID: "lens", Action: "review", Agent: "auditor"})
	if pinnedChild.HeadSHA != pinned {
		t.Fatalf("child head = %q, want the parent's pinned %q; the mirror must not override an exact head", pinnedChild.HeadSHA, pinned)
	}

	// 2. REVIEW ONLY. An implement or ask child legitimately runs without a
	//    pinned head, and pinning one changes where it works.
	for _, action := range []string{"implement", "ask", "produce"} {
		child := engine.delegationRequest(ctx, parent, JobPayload{
			Repo: "gitmoot/gitmoot", PullRequest: 7,
		}, Delegation{ID: "leg", Action: action, Agent: "worker"})
		if child.HeadSHA != "" {
			t.Fatalf("%s delegation child head = %q, want empty; only a review child is pinned", action, child.HeadSHA)
		}
	}

	// 3. NO PR, NO HEAD. A PR-less review delegation has nothing to resolve
	//    against and must stay exactly as it was rather than borrowing a head
	//    from somewhere else.
	prLess := engine.delegationRequest(ctx, parent, JobPayload{
		Repo: "gitmoot/gitmoot",
	}, Delegation{ID: "lens", Action: "review", Agent: "auditor"})
	if prLess.HeadSHA != "" {
		t.Fatalf("PR-less review delegation child head = %q, want empty", prLess.HeadSHA)
	}
}
