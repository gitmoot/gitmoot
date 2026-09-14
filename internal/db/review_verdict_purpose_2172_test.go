package db

import (
	"context"
	"encoding/json"
	"strings"
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

// seedVerdictWithFindings writes a succeeded review whose findings array is
// exactly the given JSON, so a test can pin one decode shape at a time.
func seedVerdictWithFindings(t *testing.T, store *Store, id, head, findingsJSON string) {
	t.Helper()
	payload := `{"repo":"owner/repo","pull_request":12,"head_sha":"` + head + `",` +
		`"review_purpose":"code","result":{"decision":"approved","evidence":"executed","findings":` + findingsJSON + `}}`
	if err := store.CreateJob(context.Background(), Job{ID: id, Agent: "reviewer", Type: "review",
		State: "succeeded", Payload: payload, Repo: "owner/repo"}); err != nil {
		t.Fatal(err)
	}
}

// #2179. Findings are written as OBJECTS or as bare STRINGS - both are valid to
// every producer - but this decoder accepted only objects, so a string-findings
// verdict was dropped from verdict history entirely: 930 of 3,816 terminal
// verdicts with a real pull request, store-wide. A genuinely malformed shape
// must still fail, because the delta router distinguishes "no verdict here"
// from "a verdict I could not read".
func TestSucceededReviewVerdictsAcceptsStringFindings(t *testing.T) {
	store := openAwaitedFactTestStore(t)
	ctx := context.Background()
	head := "0bd967c5ba8e506607bd3a9999a94a4db5b881b4"

	seedVerdictWithFindings(t, store, "job-string", head, `["F1: bare string"]`)
	seedVerdictWithFindings(t, store, "job-object", head, `[{"severity":"P2","title":"F2"}]`)
	seedVerdictWithFindings(t, store, "job-malformed", head, `[42]`)

	verdicts, err := store.SucceededReviewVerdicts(ctx, "owner/repo", 12)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, v := range verdicts {
		seen[v.JobID] = true
	}
	if !seen["job-string"] {
		t.Fatal("a verdict whose findings are bare strings is still invisible to verdict history")
	}
	if !seen["job-object"] {
		t.Fatal("a verdict whose findings are objects became invisible")
	}
	if seen["job-malformed"] {
		t.Fatal("a numeric finding decoded; a genuinely malformed payload must stay undecodable, or the delta router cannot tell absent from unreadable")
	}

	undecodable, err := store.UndecodableReviewVerdicts(ctx, "owner/repo", 12)
	if err != nil {
		t.Fatal(err)
	}
	if len(undecodable) != 1 || undecodable[0].JobID != "job-malformed" {
		t.Fatalf("undecodable = %+v, want only job-malformed", undecodable)
	}
}

// The count that feeds the requester's wake must include string findings, or a
// woken seat is told a changes_requested verdict rests on nothing.
func TestReviewVerdictFactCountsStringFindings(t *testing.T) {
	payload := `{"repo":"owner/repo","pull_request":12,"head_sha":"0bd967c5ba8e506607bd3a9999a94a4db5b881b4",` +
		`"result":{"decision":"approved","evidence":"executed","tests_run":["go test ./..."],` +
		`"findings":["F1","F2",{"severity":"P3","title":"F3"}]}}`
	observation, ok := reviewVerdictFact("job-1", "reviewer", "succeeded", payload, func(string) string { return "P1" })
	if !ok {
		t.Fatal("a verdict with string findings produced no observation")
	}
	if !strings.Contains(observation.detail, "findings=3") {
		t.Fatalf("detail = %q, want findings=3", observation.detail)
	}
}
