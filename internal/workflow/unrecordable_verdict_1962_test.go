package workflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// #1962: an `agent ask` dispatched as "review this PR at this head" returns a
// real review verdict, and the ledger writer never sees it - both call sites are
// inside `job.Type == "review"`, so for an ask the writer is not CALLED and its
// own recordLedgerSkip cannot fire. The verdict vanished with no row and no
// event.
//
// Measured on 2026-09-08: 15 succeeded ask verdicts, 11 carrying findings, zero
// ledger rows, zero explaining events. Positive control on that zero: the same
// join over review-TYPE jobs the same day returns 28 jobs and 80 rows.
//
// The arms below are one property in three parts. The middle one is the reason
// this change writes no rows: an unbound verdict must produce a RECORDED skip,
// never a ledger row keyed to nothing.
func unboundVerdictEvents(t *testing.T, store *db.Store, jobID string) []db.JobEvent {
	t.Helper()
	events, err := store.ListJobEvents(context.Background(), jobID)
	if err != nil {
		t.Fatalf("ListJobEvents(%s): %v", jobID, err)
	}
	var found []db.JobEvent
	for _, event := range events {
		if event.Kind == unboundReviewVerdictEventKind {
			found = append(found, event)
		}
	}
	return found
}

// ledgerRowCount counts observations the ledger holds for a repo/PR pair. The
// unbound arms use the pair the verdict WOULD have claimed, so a fabricated key
// would be caught rather than missed by looking somewhere it did not write.
func ledgerRowCount(t *testing.T, store *db.Store, repo string, pullRequest int64) int {
	t.Helper()
	rows, err := store.ListReviewFindingObservations(context.Background(), repo, pullRequest)
	if err != nil {
		t.Fatalf("ListReviewFindingObservations(%s#%d): %v", repo, pullRequest, err)
	}
	return len(rows)
}

// rawFindings builds the wire form the engine actually carries.
func rawFindings(t *testing.T, entries ...string) []json.RawMessage {
	t.Helper()
	out := make([]json.RawMessage, 0, len(entries))
	for _, entry := range entries {
		out = append(out, json.RawMessage(entry))
	}
	return out
}

// Arm 1: the loss becomes findable, and names what was missing.
func TestUnboundAskVerdictRecordsTheLoss(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)

	payload := JobPayload{
		Result: &AgentResult{
			Decision: "changes_requested",
			Summary:  "objection at the current head",
			Findings: rawFindings(t, `{"uid":"#2029-f1","severity":"P1","disposition":"open","evidence":"x.go:1 - why"}`),
		},
	}
	job := db.Job{ID: "ask-unbound", Type: "ask", Agent: "gm-review-opus"}
	engine.recordUnboundReviewVerdict(ctx, job, payload)

	events := unboundVerdictEvents(t, store, "ask-unbound")
	if len(events) != 1 {
		t.Fatalf("review_verdict_unrecordable events = %d, want 1; a lost verdict must not be indistinguishable from a review that found nothing", len(events))
	}
	for _, want := range []string{"pull_request", "head_sha", "changes_requested", "--pr"} {
		if !strings.Contains(events[0].Message, want) {
			t.Fatalf("event message = %q, want it to name %q", events[0].Message, want)
		}
	}
}

// Arm 2. THE ARM THAT CONSTRAINS THE FIX. An unbound verdict must write NO
// ledger row: review_finding_observations is keyed by repo + pull_request +
// head_sha, so a row missing all three answers no query anyone can write, and a
// synthesised key would assert a binding nobody established. Without this arm,
// arm 1 is satisfied by a version that also fabricates rows - which is the
// defect the issue exists to prevent, in the opposite direction.
func TestUnboundAskVerdictWritesNoLedgerRow(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)

	payload := JobPayload{
		Result: &AgentResult{
			Decision: "changes_requested",
			Findings: rawFindings(t,
				`{"uid":"#2029-f1","severity":"P1","evidence":"a.go:1 - why"}`,
				`{"uid":"#2029-f2","severity":"P2","evidence":"b.go:2 - why"}`),
		},
	}
	engine.recordUnboundReviewVerdict(ctx, db.Job{ID: "ask-norow", Type: "ask"}, payload)

	if n := ledgerRowCount(t, store, "gitmoot/gitmoot", 2029); n != 0 {
		t.Fatalf("ledger rows for an unbound verdict = %d, want 0; a row keyed to no PR and no head is worse than the gap", n)
	}
}

// Arm 3: a BOUND verdict that still misses the writer is a different defect and
// says so, rather than being reported as an unbound dispatch. This is the case
// that must not be made writable until #2059's field-name mismatch is fixed.
func TestBoundAskVerdictNamesTheTypeGateInstead(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)

	payload := JobPayload{
		Repo: "gitmoot/gitmoot", PullRequest: 2029, HeadSHA: "8bdf8f54",
		Result: &AgentResult{Decision: "changes_requested"},
	}
	engine.recordUnboundReviewVerdict(ctx, db.Job{ID: "ask-bound", Type: "ask"}, payload)

	events := unboundVerdictEvents(t, store, "ask-bound")
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if !strings.Contains(events[0].Message, "job type") || strings.Contains(events[0].Message, "--pr") {
		t.Fatalf("message = %q, want the TYPE gate named and the re-dispatch remedy absent; a bound verdict does not need re-dispatching", events[0].Message)
	}
	if n := ledgerRowCount(t, store, "gitmoot/gitmoot", 2029); n != 0 {
		t.Fatalf("ledger rows = %d, want 0 until #2059 lands", n)
	}
}

// The control. Without it every arm above is satisfied by a version that fires
// on everything, which would bury the real losses in noise from ordinary asks.
func TestOrdinaryAskRecordsNoUnrecordableVerdict(t *testing.T) {
	if reviewShapedResult(&AgentResult{Decision: "implemented", Summary: "did the thing"}) {
		t.Fatal("an implement-shaped result was treated as a review verdict")
	}
	// #2061 review, P3: findings ALONE used to be sufficient, so an implement job
	// that populated Findings - which the result shape permits - emitted an event
	// about work that was never a review. Found by reading the predicate; every
	// fixture here used a review-shaped decision, so no test could have caught it.
	if reviewShapedResult(&AgentResult{
		Decision: "implemented",
		Findings: rawFindings(t, `{"uid":"#1-f1","severity":"P2","evidence":"a.go:1 - note"}`),
	}) {
		t.Fatal("an implement result carrying findings was treated as a review verdict")
	}
	// A verdict with findings and NO decision is still a review: findings are the
	// only signal there is.
	if !reviewShapedResult(&AgentResult{Findings: rawFindings(t, `{"severity":"P1"}`)}) {
		t.Fatal("findings with no decision were not treated as a review verdict")
	}
	if reviewShapedResult(&AgentResult{Decision: "", Summary: "answered a question"}) {
		t.Fatal("a plain ask answer was treated as a review verdict")
	}
	if reviewShapedResult(nil) {
		t.Fatal("a nil result was treated as a review verdict")
	}
	// Normalisation still matters, but it is tested WITH findings now: this arm
	// used to assert that a bare "APPROVED " was a verdict, which encoded the very
	// behaviour CI later refuted (#2061, three race shards plus the tagged e2e).
	if !reviewShapedResult(&AgentResult{
		Decision: "APPROVED ",
		Findings: rawFindings(t, `{"uid":"#1-f1","severity":"P3","title":"t"}`),
	}) {
		t.Fatal("a review decision was missed on case and whitespace")
	}
	// AN ORDINARY ASK APPROVES NOTHING. This is the unit-level statement of what
	// TestLocalExecutionBackendAllowsNonImplement measured end to end: the shipped
	// result contract puts {"decision":"approved","findings":[]} in front of every
	// agent, so that shape is the ORDINARY ask answer, not a review verdict.
	if reviewShapedResult(&AgentResult{Decision: "approved", Summary: "ran on shell override"}) {
		t.Fatal("an ordinary ask that approved nothing was treated as a review verdict")
	}
}

// THE PRODUCTION-PATH ARM, and the only one that proves the call site is wired.
// The three arms above call recordUnboundReviewVerdict directly, so they would
// all pass against a branch where the helper exists and nothing ever invokes it -
// which is precisely the defect being fixed, one level up: a writer that is
// never called.
//
// This drives AdvanceJob with the measured shape: an `ask` job carrying a review
// verdict and findings, dispatched with no PR and no head. It is also the base
// control, because it compiles on main - it names no new symbol - and there it
// records nothing at all.
func TestAdvanceJobRecordsAnUnrecordableAskVerdict(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "gm-review-opus", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)

	insertCompletedJob(t, store, db.Job{ID: "ask-e2e", Agent: "gm-review-opus", Type: "ask"}, JobPayload{
		Repo: "gitmoot/gitmoot", TaskID: "task-ask",
		Result: &AgentResult{
			Decision: "changes_requested", Severity: "P1", Summary: "objection at the current head",
			Evidence: EvidenceExecuted,
			TestsRun: []string{"go test ./internal/workflow/ -> ok"},
			Findings: rawFindings(t, `{"uid":"#2029-f1","severity":"P1","disposition":"open","evidence":"internal/x.go:1 - why"}`),
		},
	})

	if err := engine.AdvanceJob(ctx, "ask-e2e"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}

	events, err := store.ListJobEvents(ctx, "ask-e2e")
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	found := 0
	for _, event := range events {
		if event.Kind == "review_verdict_unrecordable" {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("review_verdict_unrecordable events after AdvanceJob = %d, want 1; on main this is 0 and the verdict disappears with no row and no event. events=%+v", found, events)
	}
	if n := ledgerRowCount(t, store, "gitmoot/gitmoot", 0); n != 0 {
		t.Fatalf("ledger rows = %d, want 0; an unbound verdict must never be keyed", n)
	}
}

// #2061 review, P2: A BLOCKED REVIEWER THAT NAMED DEFECTS STILL REVIEWED.
//
// review_loop.go's followUpReviewScopes treats decision "blocked" WITH findings
// as a real verdict and refuses it without them. reviewShapedResult did not,
// so an ask dispatched as a review that blocked after finding defects was lost
// silently - the exact class #1962 closes. The reviewer found it by READING the
// predicate; no fixture here used a non-success decision, in either direction.
func TestBlockedWithFindingsIsAReviewVerdict(t *testing.T) {
	if !reviewShapedResult(&AgentResult{
		Decision: "blocked",
		Findings: []json.RawMessage{json.RawMessage(`{"id":"F1","severity":"P1","title":"t"}`)},
	}) {
		t.Fatal("a blocked reviewer that named defects was not treated as a review verdict")
	}
}

// THE OTHER DIRECTION, which is what makes the arm above non-vacuous: blocked
// with NO findings is not a verdict, matching followUpReviewScopes exactly.
func TestBlockedWithoutFindingsIsNotAReviewVerdict(t *testing.T) {
	if reviewShapedResult(&AgentResult{Decision: "blocked"}) {
		t.Fatal("a blocked job that named nothing was treated as a review verdict")
	}
}

// "failed" WITH FINDINGS IS A VERDICT, and this arm replaces the one that pinned
// my exclusion. The reviewer overturned it with the evidence I asked for: the
// result contract delivered to every agent offers "failed" with no reviewer
// exclusion, so a reviewer-authored "failed" is expressible and documented.
func TestFailedWithFindingsIsAReviewVerdict(t *testing.T) {
	if !reviewShapedResult(&AgentResult{
		Decision: "failed",
		Findings: []json.RawMessage{json.RawMessage(`{"id":"F1","severity":"P1","title":"t"}`)},
	}) {
		t.Fatal("a failed reviewer that named defects was not treated as a review verdict")
	}
}

// THE GUARD THAT KEEPS IT SAFE: an ENGINE-generated "failed" carries no
// findings, so it is still not a verdict. Without this, admitting "failed"
// would promote every engine terminal state to a review.
func TestFailedWithoutFindingsIsNotAReviewVerdict(t *testing.T) {
	if reviewShapedResult(&AgentResult{Decision: "failed", Summary: "worker crashed"}) {
		t.Fatal("an engine-authored \"failed\" with no findings was treated as a review verdict")
	}
}
