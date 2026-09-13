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

// #2176: the exported subject parser is what scopes the router's claim walk, so
// a malformed key must FAIL rather than yield a partial subject the walk would
// then match loosely.
func TestParseReviewVerdictSubjectKeyRejectsMalformedKeys(t *testing.T) {
	good, err := ReviewRequestSubjectKey("owner/repo", 12, "81ef6f8d994f6d1a94b65add5e0ada9b121d3eec", "security")
	if err != nil {
		t.Fatal(err)
	}
	repo, pr, head, err := ParseReviewVerdictSubjectKey(good)
	if err != nil || repo != "owner/repo" || pr != 12 || head != "81ef6f8d994f6d1a94b65add5e0ada9b121d3eec" {
		t.Fatalf("round trip = (%q,%d,%q,%v), want the subject it was built from", repo, pr, head, err)
	}
	if got := ReviewVerdictKeyPurpose(good); got != "security" {
		t.Fatalf("purpose = %q, want security", got)
	}
	// The BARE key is the historical `org await review` form with no purpose
	// suffix - the constructor refuses to mint one, so it is written literally
	// here exactly as a pre-router waiter would have stored it. It must report
	// the documented default rather than an empty purpose.
	bare := "owner/repo#12@81ef6f8d994f6d1a94b65add5e0ada9b121d3eec"
	if repo, pr, head, err := ParseReviewVerdictSubjectKey(bare); err != nil || repo != "owner/repo" || pr != 12 || head != "81ef6f8d994f6d1a94b65add5e0ada9b121d3eec" {
		t.Fatalf("bare key parse = (%q,%d,%q,%v), want the pre-router subject to keep parsing", repo, pr, head, err)
	}
	// The bare key is scoped to NO purpose, which means ANY purpose - it is not
	// a shorthand for "code". Consumers apply DefaultReviewPurpose themselves
	// when they need a concrete one; conflating the two here would let a bare
	// `org await review` wait be satisfied only by code verdicts.
	if got := ReviewVerdictKeyPurpose(bare); got != "" {
		t.Fatalf("bare key purpose = %q, want empty: the any-purpose key must not collapse to a single purpose", got)
	}
	for _, malformed := range []string{"", "owner/repo", "owner/repo#12", "owner/repo@head", "owner/repo#notanumber@head", "#12@head"} {
		if _, _, _, err := ParseReviewVerdictSubjectKey(malformed); err == nil {
			t.Fatalf("ParseReviewVerdictSubjectKey(%q) succeeded, want an error: a partial subject makes the claim walk match loosely", malformed)
		}
	}
}
