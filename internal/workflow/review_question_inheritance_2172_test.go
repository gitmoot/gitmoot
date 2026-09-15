package workflow

import (
	"context"
	"slices"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// #2171/#2172. The router's purpose, model pool and requester describe the
// QUESTION, not the job that happens to answer it. On a staged-review repo the
// answer arrives through a verdict CHILD, so a purpose that stops at the
// preflight leaves the purposed waiter unsatisfiable and the requester waits
// out its whole TTL for a verdict that was produced.
//
// Round 3 found this half had no coverage either way: deleting the inheritance
// passed the entire suite, and the inheritance was broader than the contract
// needs - a non-review leg carried a review purpose into the type-blind claim
// walk and the reviewer-pool fallback. Both directions are pinned here.
func TestReviewQuestionFieldsAreInheritedByReviewChildrenOnly(t *testing.T) {
	store := openEngineStore(t)
	engine := testEngine(store)
	parent := db.Job{ID: "preflight-job", Agent: "opus-reviewer", Type: "review"}
	payload := JobPayload{
		Repo:            "gitmoot/gitmoot",
		PullRequest:     2172,
		ReviewPurpose:   "security",
		ReviewModelPool: []string{"sentinel/router-a", "sentinel/router-b"},
		ReviewRequester: "joltra",
	}

	verdict := engine.delegationRequest(context.Background(), parent, payload, Delegation{
		ID: "verdict", Agent: "reviewer", Action: "review", Prompt: "judge the head",
	})
	if verdict.ReviewPurpose != "security" {
		t.Fatalf("verdict child purpose = %q, want security: a purposed waiter can never be satisfied by the child that answers it", verdict.ReviewPurpose)
	}
	if verdict.ReviewRequester != "joltra" {
		t.Fatalf("verdict child requester = %q, want joltra: the requester waits its whole TTL for a verdict that was produced", verdict.ReviewRequester)
	}
	// CONTENT, not length (#2188 round 2): a length check passes on a wrong pool
	// of the right size - the same family as equality-by-coincidence.
	if want := []string{"sentinel/router-a", "sentinel/router-b"}; !slices.Equal(verdict.ReviewModelPool, want) {
		t.Fatalf("verdict child pool = %v, want the question's own %v", verdict.ReviewModelPool, want)
	}

	// The other direction, and the round-3 P2/P3 pair: a non-review leg of the
	// SAME preflight must carry none of it. Its two harmful consumers are the
	// claim walk (which read a non-review child's approved decision as an answer
	// and pinned the claim forever) and the dispatch pool fallback (which
	// applied a reviewer pool to a leg that is not choosing a reviewer).
	implement := engine.delegationRequest(context.Background(), parent, payload, Delegation{
		ID: "impl", Agent: "builder", Action: "implement", Prompt: "do the work",
	})
	if implement.ReviewPurpose != "" || implement.ReviewRequester != "" || len(implement.ReviewModelPool) != 0 {
		t.Fatalf("non-review leg inherited the review question: purpose=%q requester=%q pool=%v", implement.ReviewPurpose, implement.ReviewRequester, implement.ReviewModelPool)
	}
}
