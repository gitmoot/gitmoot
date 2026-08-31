package workflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// TestRoutineRoundUnionsBareAndLensScopes pins the verdict finding that an OLDER
// routine verdict suppressed the immediately preceding high-risk lens round. The
// sequence is routine round 1 -> lens round 2 -> routine round 3: reading the bare
// reviewer key first returned round 1 and dropped every round-2 lens finding. The
// routine round now reads a union of all of that reviewer's live scopes, so no round's
// findings are lost and the baseline is the oldest of them.
func TestRoutineRoundUnionsBareAndLensScopes(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "gitmoot/gitmoot")
	// Round 1: a plain routine verdict at head-one.
	insertCompletedJob(t, store, db.Job{ID: "routine-round-1", Agent: "audit", Type: "review"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-7", PullRequest: 7, HeadSHA: "head-one",
		TaskID: "task-7", ReviewRound: "review-1",
		Result: &AgentResult{
			Decision: "changes_requested",
			Summary:  "routine finding",
			Findings: []json.RawMessage{json.RawMessage(`{"id":"R-1","summary":"routine finding"}`)},
		},
	})
	// Round 2: the same reviewer ran two lenses at head-two.
	for _, lens := range []struct{ id, finding string }{
		{LensCorrectness, `{"lens":"correctness","id":"C-2","summary":"correctness finding"}`},
		{LensSecurity, `{"lens":"security","id":"S-2","summary":"security finding"}`},
	} {
		insertCompletedJob(t, store, db.Job{ID: "lens-round-2-" + lens.id, Agent: "audit", Type: "review"}, JobPayload{
			Repo: "gitmoot/gitmoot", Branch: "task-7", PullRequest: 7, HeadSHA: "head-two",
			TaskID: "task-7", ReviewRound: "review-2", DelegationID: lens.id,
			Result: &AgentResult{
				Decision: "changes_requested",
				Summary:  "lens finding",
				Findings: []json.RawMessage{json.RawMessage(lens.finding)},
			},
		})
	}

	engine := testEngine(store)
	engine.RiskTiersEnabled = false
	engine.ReviewChangedFiles = func(_ context.Context, _ string, _ int, previousHead, _ string) ([]string, error) {
		switch previousHead {
		case "head-one":
			return []string{"internal/from_head_one.go"}, nil
		case "head-two":
			return []string{"internal/from_head_two.go"}, nil
		}
		t.Fatalf("unexpected compare baseline %q", previousHead)
		return nil, nil
	}
	event := highRiskEvent()
	event.HeadSHA = "head-three"
	event.RequiredReviewers = []string{"audit"}

	if err := engine.HandlePullRequestOpened(ctx, event); err != nil {
		t.Fatalf("HandlePullRequestOpened: %v", err)
	}
	payload := reviewPayloadFor(t, store, "review-audit-task-7-review-3")
	if payload.ReviewScope == nil {
		t.Fatal("routine round 3 lost the scope")
	}
	joined := strings.Join(payload.ReviewScope.Findings, " ")
	for _, want := range []string{`"id":"R-1"`, `"id":"C-2"`, `"id":"S-2"`} {
		if !strings.Contains(joined, want) {
			t.Fatalf("routine scope findings = %#v, want %s carried forward", payload.ReviewScope.Findings, want)
		}
	}
	// The OLDEST baseline is named, because its file range covers the newer one.
	if payload.ReviewScope.PreviousHeadSHA != "head-one" {
		t.Fatalf("scope baseline = %q, want head-one (the oldest merged baseline)", payload.ReviewScope.PreviousHeadSHA)
	}
	files := strings.Join(payload.ReviewScope.ChangedFiles, ",")
	if files != "internal/from_head_one.go,internal/from_head_two.go" {
		t.Fatalf("scope files = %#v, want the union of both baselines", payload.ReviewScope.ChangedFiles)
	}
}

// TestLensScopeSurvivesReservedDelegationID pins the reserved-key collision the
// verdict raised as P3. Nothing validates delegation ids against control bytes, so a
// lens id equal to the old reserved routine suffix used to produce the same map key as
// the reviewer's routine aggregate and one overwrote the other. The key is a struct
// now, so the lens id cannot name the routine slot at all.
func TestLensScopeSurvivesReservedDelegationID(t *testing.T) {
	hostile := "\x00routine"
	scopes := map[reviewScopeKey]*ReviewScope{
		lensScopeKey("audit", hostile): {PreviousHeadSHA: "head-one", Findings: []string{"lens finding"}},
		routineScopeKey("audit"):       {PreviousHeadSHA: "head-one", Findings: []string{"routine aggregate"}},
	}
	if len(scopes) != 2 {
		t.Fatalf("hostile delegation id collapsed the key space: %d entries, want 2", len(scopes))
	}
	lens := reviewScopeFor(scopes, "audit", hostile)
	if lens == nil || len(lens.Findings) != 1 || lens.Findings[0] != "lens finding" {
		t.Fatalf("lens scope = %+v, want its own findings", lens)
	}
	routine := reviewScopeForRoutine(scopes, "audit")
	if routine == nil || len(routine.Findings) != 1 || routine.Findings[0] != "routine aggregate" {
		t.Fatalf("routine scope = %+v, want the aggregate", routine)
	}
}

// TestUnscopableHeadRecordsOneEventAcrossDifferentResolverErrors pins the dedupe
// identity. The event reason embeds the resolver's error text, so matching the whole
// reason recorded a second row for the same head whenever the transport error was
// worded differently. The claim is on task + PR + head instead.
func TestUnscopableHeadRecordsOneEventAcrossDifferentResolverErrors(t *testing.T) {
	ctx := context.Background()
	store, engine := reviewRefanoutFixture(t)
	insertPriorReviewResult(t, store, "prior-review", "head-one", AgentResult{
		Decision: "changes_requested",
		Summary:  "Fix the boundary check.",
	})
	call := 0
	engine.ReviewChangedFiles = func(context.Context, string, int, string, string) ([]string, error) {
		call++
		return nil, ReviewScopeUnavailableError{
			Reason: "review scope compare is \"diverged\", not a direct follow-up (attempt " + strings.Repeat("x", call) + ")",
		}
	}

	for poll := range 2 {
		if err := engine.HandlePullRequestOpened(ctx, reviewRefanoutEvent("head-two")); err != nil {
			t.Fatalf("HandlePullRequestOpened %d: %v", poll+1, err)
		}
	}
	if call != 2 {
		t.Fatalf("resolver calls = %d, want both observations to reach the resolver", call)
	}
	events, err := store.ListTaskEvents(ctx, "task-1678")
	if err != nil {
		t.Fatalf("ListTaskEvents: %v", err)
	}
	recorded := 0
	for _, event := range events {
		if event.Kind == "review_scope_unavailable" {
			recorded++
		}
	}
	if recorded != 1 {
		t.Fatalf("review_scope_unavailable events = %d, want 1 per head regardless of the error text", recorded)
	}
}

// TestExistingReviewLegBlocksDuplicateAtSameHeadAndRound pins the duplicate-leg guard.
// The idempotent-enqueue collision check compares DERIVED content, so a round that
// scopes on one derivation and degrades to an unscoped review on another surfaced a raw
// `UNIQUE constraint failed: jobs.id` out of the lifecycle. A reviewer that already has
// a leg at this head and round is skipped on identity instead.
func TestExistingReviewLegBlocksDuplicateAtSameHeadAndRound(t *testing.T) {
	ctx := context.Background()
	store, engine := reviewRefanoutFixture(t)
	insertPriorReviewResult(t, store, "prior-review", "head-one", AgentResult{
		Decision: "changes_requested",
		Summary:  "Fix the boundary check.",
	})
	scopable := true
	engine.ReviewChangedFiles = func(context.Context, string, int, string, string) ([]string, error) {
		if scopable {
			return []string{"internal/boundary.go"}, nil
		}
		return nil, ReviewScopeUnavailableError{Reason: `review scope compare is "diverged", not a direct follow-up`}
	}

	// First derivation: a SCOPED leg is enqueued and left queued.
	if err := engine.HandlePullRequestOpened(ctx, reviewRefanoutEvent("head-two")); err != nil {
		t.Fatalf("HandlePullRequestOpened scoped: %v", err)
	}
	first := mustJob(t, store, "review-audit-task-1678-review-2")
	if JobState(first.State) != JobQueued {
		t.Fatalf("first leg state = %q, want queued", first.State)
	}

	// Second derivation of the SAME round, now unscopable: different instructions,
	// therefore a different deterministic id if it were allowed to enqueue.
	scopable = false
	if err := engine.HandlePullRequestOpened(ctx, reviewRefanoutEvent("head-two")); err != nil {
		t.Fatalf("HandlePullRequestOpened degraded: %v", err)
	}
	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	legs := 0
	for _, job := range jobs {
		if job.Type == "review" && job.ID != "prior-review" {
			legs++
		}
	}
	if legs != 1 {
		t.Fatalf("review legs at one head/round = %d, want 1", legs)
	}
	// The surviving leg is the FIRST one, unmodified.
	still := mustJob(t, store, "review-audit-task-1678-review-2")
	if still.Payload != first.Payload {
		t.Fatal("the queued leg's payload was rewritten by the second derivation")
	}
}

// TestExistingTerminalReviewLegIsNotRederived covers the same class on the other side
// of the state machine: a TERMINAL leg cannot be revived by re-enqueue either, so a
// re-derivation whose instructions differ used to wedge the lifecycle with the raw
// UNIQUE error on every poll. Re-attempting a held leg is the worker's path — the row
// stays queued — and this guard never touches that.
func TestExistingTerminalReviewLegIsNotRederived(t *testing.T) {
	ctx := context.Background()
	store, engine := reviewRefanoutFixture(t)
	insertPriorReviewResult(t, store, "prior-review", "head-one", AgentResult{
		Decision: "changes_requested",
		Summary:  "Fix the boundary check.",
	})
	scopable := true
	engine.ReviewChangedFiles = func(context.Context, string, int, string, string) ([]string, error) {
		if scopable {
			return []string{"internal/boundary.go"}, nil
		}
		return nil, ReviewScopeUnavailableError{Reason: `review scope compare is "diverged", not a direct follow-up`}
	}
	if err := engine.HandlePullRequestOpened(ctx, reviewRefanoutEvent("head-two")); err != nil {
		t.Fatalf("HandlePullRequestOpened scoped: %v", err)
	}
	leg := mustJob(t, store, "review-audit-task-1678-review-2")
	if err := store.UpdateJobState(ctx, leg.ID, string(JobFailed)); err != nil {
		t.Fatalf("UpdateJobState failed: %v", err)
	}

	// Same head, same round, DIFFERENT derived instructions. Before the guard this
	// returned `UNIQUE constraint failed: jobs.id` and did so on every poll.
	scopable = false
	for poll := range 3 {
		if err := engine.HandlePullRequestOpened(ctx, reviewRefanoutEvent("head-two")); err != nil {
			t.Fatalf("HandlePullRequestOpened poll %d after a failed leg: %v", poll+1, err)
		}
	}
	after, err := store.GetJob(ctx, leg.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if after.State != string(JobFailed) || after.Payload != leg.Payload {
		t.Fatalf("terminal leg was rewritten: state %q payload changed=%t", after.State, after.Payload != leg.Payload)
	}
	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	legs := 0
	for _, job := range jobs {
		if job.Type == "review" && job.ID != "prior-review" {
			legs++
		}
	}
	if legs != 1 {
		t.Fatalf("review legs after re-derivation = %d, want the single terminal leg", legs)
	}
}

// TestReviewLegsAtHeadKeysOnIdentity pins what the guard treats as the same work.
func TestReviewLegsAtHeadKeysOnIdentity(t *testing.T) {
	event := reviewRefanoutEvent("head-two")
	payload, err := json.Marshal(JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-1678", PullRequest: 1678, HeadSHA: "head-two",
		TaskID: "task-1678", Reviewers: []string{"audit"}, ReviewRound: "review-2",
	})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	for _, state := range []JobState{JobQueued, JobRunning, JobFailed, JobSucceeded, JobCancelled, JobBlocked} {
		jobs := []db.Job{{ID: "leg", Agent: "audit", Type: "review", State: string(state), Payload: string(payload)}}
		if _, held := reviewLegsAtHead(jobs, event, "review-2")["audit"]; !held {
			t.Fatalf("state %s: leg not recognised at this head and round", state)
		}
	}
	jobs := []db.Job{{ID: "leg", Agent: "audit", Type: "review", State: string(JobQueued), Payload: string(payload)}}
	// A different round at the same head is different work.
	if _, held := reviewLegsAtHead(jobs, event, "review-3")["audit"]; held {
		t.Fatal("a leg from another round was treated as this round's work")
	}
	// A different head in the same round is different work.
	otherHead := reviewRefanoutEvent("head-three")
	if _, held := reviewLegsAtHead(jobs, otherHead, "review-2")["audit"]; held {
		t.Fatal("a leg at another head was treated as this head's work")
	}
}
func reviewPayloadFor(t *testing.T, store *db.Store, jobID string) JobPayload {
	t.Helper()
	job := mustJob(t, store, jobID)
	payload, err := unmarshalPayload(job.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload %s: %v", jobID, err)
	}
	return payload
}
