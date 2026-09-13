package db

import (
	"context"
	"encoding/json"
	"testing"
)

// ROUND-4 P2, and the SECOND guard in this change shipped without coverage.
// SucceededReviewVerdicts is the canonical same-head verdict history every
// purpose-aware consumer reads: the review-loop guard and the router's reviewer
// selection both decide "has this question already been answered" from it.
// Deleting the ReviewPurpose decode passed the entire suite, which silently
// re-opens same-purpose repeat dispatch for every non-default purpose - the
// verdict arrives with an empty purpose, defaults to "code", and a repeated
// security review is dispatched as if nothing had answered it.
func TestSucceededReviewVerdictsCarriesTheReviewPurpose(t *testing.T) {
	store := openAwaitedFactTestStore(t)
	ctx := context.Background()
	head := "0bd967c5ba8e506607bd3a9999a94a4db5b881b4"

	for _, seed := range []struct {
		id      string
		agent   string
		purpose string
	}{
		{"job-security", "reviewer-a", "security"},
		{"job-code", "reviewer-b", "code"},
		{"job-legacy", "reviewer-c", ""},
	} {
		payload, err := json.Marshal(map[string]any{
			"repo": "owner/repo", "pull_request": 12, "head_sha": head,
			"review_purpose": seed.purpose,
			"result":         map[string]any{"decision": "approved", "evidence": "executed"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.CreateJob(ctx, Job{ID: seed.id, Agent: seed.agent, Type: "review", State: "succeeded", Payload: string(payload), Repo: "owner/repo"}); err != nil {
			t.Fatal(err)
		}
	}

	verdicts, err := store.SucceededReviewVerdicts(ctx, "owner/repo", 12)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, verdict := range verdicts {
		got[verdict.JobID] = verdict.ReviewPurpose
	}
	if got["job-security"] != "security" {
		t.Fatalf("security verdict purpose = %q, want security: a repeated security review dispatches as if nothing answered it", got["job-security"])
	}
	if got["job-code"] != "code" {
		t.Fatalf("code verdict purpose = %q, want code", got["job-code"])
	}
	// A verdict predating the router carries no purpose; consumers default it
	// to DefaultReviewPurpose, so the decode must preserve empty rather than
	// inventing a value here.
	if got["job-legacy"] != "" {
		t.Fatalf("legacy verdict purpose = %q, want empty so consumers apply the documented default", got["job-legacy"])
	}
}
