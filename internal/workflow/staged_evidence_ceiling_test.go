package workflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
)

// #1821 commit two of two: the evidence ceiling that reads the marker.
//
// THE FIRST TEST HERE IS THE ONE THAT CLOSED PR #2024. That PR carried this
// mechanism scoped by inference - "the parent declared evidence" - and a
// reviewer broke it with an honest review coordinator, whose lens children
// genuinely execute. Everything else in this file is secondary to keeping that
// case red if the scoping ever regresses.

// TestCoordinatorFanOutIsNeverClamped is the P1 regression. It is written
// against stagedVerdictCeiling rather than the clamp, because the defect was in
// deciding WHO inherits, not in the clamp arithmetic.
func TestCoordinatorFanOutIsNeverClamped(t *testing.T) {
	coordinator := JobPayload{
		Result: &AgentResult{Decision: "approved", Evidence: EvidenceStaticOnly, EvidenceDeclared: true},
	}
	for _, lens := range []Delegation{
		{ID: "lens-a", Agent: "gm-review-opus", Action: "review"},
		{ID: "lens-b", Agent: "g7-review", Action: "review"},
	} {
		if got := stagedVerdictCeiling(coordinator, lens); got != "" {
			t.Fatalf("an unmarked coordinator imposed ceiling %q on lens child %q; "+
				"its children execute in their own worktrees and this downgrades the rows "+
				"ensureDelegatedReviewEvidence reads as a fan-out's only evidence", got, lens.ID)
		}
	}
}

// The marker alone is not enough: it must also NAME this delegation.
func TestOnlyTheNamedVerdictChildInherits(t *testing.T) {
	preflight := JobPayload{
		StagedReviewVerdictAgent: "gm-review-opus",
		Result:                   &AgentResult{Evidence: EvidenceStaticOnly, EvidenceDeclared: true},
	}
	verdict := Delegation{ID: "verdict", Agent: "gm-review-opus", Action: "review"}
	if got := stagedVerdictCeiling(preflight, verdict); got != EvidenceStaticOnly {
		t.Fatalf("the named verdict child inherited %q, want %q", got, EvidenceStaticOnly)
	}
	other := Delegation{ID: "side", Agent: "some-other-agent", Action: "ask"}
	if got := stagedVerdictCeiling(preflight, other); got != "" {
		t.Fatalf("a delegation the marker does not name inherited %q", got)
	}
}

// A preflight that declared nothing imposes nothing: silence is not a finding,
// and defaulting it would clamp on the engine's default rather than on an
// observation.
func TestUndeclaredPreflightImposesNothing(t *testing.T) {
	for _, parent := range []JobPayload{
		{StagedReviewVerdictAgent: "gm-review-opus"},
		{StagedReviewVerdictAgent: "gm-review-opus", Result: &AgentResult{Evidence: EvidenceStaticOnly}},
		{StagedReviewVerdictAgent: "gm-review-opus", Result: &AgentResult{}},
	} {
		if got := stagedVerdictCeiling(parent, Delegation{Agent: "gm-review-opus"}); got != "" {
			t.Fatalf("an undeclared preflight imposed %q", got)
		}
	}
}

// Direction: down only. An executed preflight must not license a claim.
func TestCeilingOnlyMovesEvidenceDownward(t *testing.T) {
	executed := AgentResult{Evidence: EvidenceExecuted, EvidenceDeclared: true}
	if ApplyInheritedEvidenceCeiling(&executed, EvidenceExecuted) {
		t.Fatal("an executed preflight clamped an executed verdict")
	}
	static := AgentResult{Evidence: EvidenceStaticOnly, EvidenceDeclared: true}
	if ApplyInheritedEvidenceCeiling(&static, EvidenceExecuted) || static.Evidence != EvidenceStaticOnly {
		t.Fatalf("an executed preflight upgraded its child to %q", static.Evidence)
	}
	// The case a mutation survived on #2024: static under static must stay static
	// rather than being rewritten in either direction.
	both := AgentResult{Evidence: EvidenceStaticOnly, EvidenceDeclared: true}
	if ApplyInheritedEvidenceCeiling(&both, EvidenceStaticOnly) || both.Evidence != EvidenceStaticOnly {
		t.Fatalf("static under static became %q; two admissions that nothing ran cannot add up to an execution claim", both.Evidence)
	}
}

func TestCeilingClampsAndKeepsTheProducerDeclared(t *testing.T) {
	r := AgentResult{Decision: "approved", Evidence: EvidenceExecuted, EvidenceDeclared: true}
	if !ApplyInheritedEvidenceCeiling(&r, EvidenceStaticOnly) {
		t.Fatal("an executed claim under a static_only preflight was not clamped")
	}
	if r.Evidence != EvidenceStaticOnly || EvidenceWasExecuted(r) {
		t.Fatalf("evidence is %q after clamping", r.Evidence)
	}
	if !EvidenceWasDeclared(r) {
		t.Fatal("clamping cleared evidence_declared; an overruled producer is not a silent one")
	}
}

func TestInheritedEvidenceAbsentFromAnOrdinaryPayload(t *testing.T) {
	raw, err := json.Marshal(JobPayload{Repo: "owner/repo"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "inherited_evidence") {
		t.Fatalf("an ordinary payload carries the ceiling: %s", raw)
	}
}

// TestEnqueueCarriesTheStagedMarkerAndCeilingIntoThePayload covers the seam the
// other tests here do not: JobRequest -> stored JobPayload.
//
// Every test above either calls stagedVerdictCeiling directly or constructs a
// JobPayload by hand, so a mutant deleting either field from Enqueue's payload
// literal would leave all of them green while the marker never reached the
// store. That is the same shape as the pool-seam gap on #2018 and the
// coordinator gap on #2024, so it gets a production-entry assertion.
func TestEnqueueCarriesTheStagedMarkerAndCeilingIntoThePayload(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{store: store, resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree")}
	if _, err := mailbox.Enqueue(ctx, JobRequest{
		ID: "preflight", Agent: "preflight", Action: "review", Repo: "owner/repo",
		StagedReviewVerdictAgent: "gm-review-opus",
		InheritedEvidence:        EvidenceStaticOnly,
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	job, err := store.GetJob(ctx, "preflight")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	stored, err := unmarshalPayload(job.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload: %v", err)
	}
	if stored.StagedReviewVerdictAgent != "gm-review-opus" {
		t.Fatalf("the staged marker did not reach the stored payload: %q", stored.StagedReviewVerdictAgent)
	}
	if stored.InheritedEvidence != EvidenceStaticOnly {
		t.Fatalf("the inherited ceiling did not reach the stored payload: %q", stored.InheritedEvidence)
	}
	// An unrecognised ceiling must be dropped at the boundary rather than stored
	// for a later reader to interpret.
	if _, err := mailbox.Enqueue(ctx, JobRequest{
		ID: "junk", Agent: "preflight", Action: "review", Repo: "owner/repo",
		InheritedEvidence: "partially",
	}); err != nil {
		t.Fatalf("Enqueue(junk): %v", err)
	}
	junk, err := store.GetJob(ctx, "junk")
	if err != nil {
		t.Fatalf("GetJob(junk): %v", err)
	}
	jp, err := unmarshalPayload(junk.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload(junk): %v", err)
	}
	if jp.InheritedEvidence != "" {
		t.Fatalf("an unrecognised ceiling %q was stored", jp.InheritedEvidence)
	}
}

// TestStagedCeilingStopsAtTheVerdictChild is the P3 from #2024's review: a real
// engine chain rather than two hops of hand-built payloads.
//
// The chain measured here is preflight -> verdict child -> coordinator
// continuation, which is what the engine actually produces: a delegation
// child's own delegations do not become grandchildren at this depth, they route
// into a continuation of the ORIGINAL parent. That is worth asserting rather
// than assuming, because it is where a leak would go.
//
// #2024 carried a fallback that passed a parent's own inherited ceiling further
// down, to stop a chain laundering the constraint. With an explicit marker that
// hole does not exist - only a marked preflight imposes anything - and the
// fallback was the mechanism that spread the contamination. So nothing beyond
// the named verdict child is constrained, and all three rows are checked.
func TestStagedCeilingStopsAtTheVerdictChild(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "preflight", []string{"review"}, "gitmoot/gitmoot")
	seedAgent(t, store, "gm-review-opus", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)

	insertCompletedJob(t, store, db.Job{ID: "preflight-job", Agent: "preflight", Type: "review"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "main", PullRequest: 7, TaskID: "task-9", Sender: "preflight",
		StagedReviewVerdictAgent: "gm-review-opus",
		Result: &AgentResult{
			Decision: "approved", Summary: "preflight", Evidence: EvidenceStaticOnly, EvidenceDeclared: true,
			Delegations: []Delegation{{ID: "verdict", Agent: "gm-review-opus", Action: "review", Prompt: "review it"}},
		},
	})
	if err := engine.AdvanceJob(ctx, "preflight-job"); err != nil {
		t.Fatalf("AdvanceJob(preflight): %v", err)
	}

	// HOP TWO: the named verdict child inherits the ceiling.
	verdict := mustJob(t, store, "preflight-job/delegation/verdict")
	vp, err := unmarshalPayload(verdict.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload(verdict): %v", err)
	}
	if vp.InheritedEvidence != EvidenceStaticOnly {
		t.Fatalf("the verdict child inherited %q, want %q", vp.InheritedEvidence, EvidenceStaticOnly)
	}
	// And it is NOT itself a marked preflight, which is what stops propagation at
	// the source rather than at each consumer.
	if vp.StagedReviewVerdictAgent != "" {
		t.Fatalf("the verdict child carries a staged marker %q; it would constrain its own children", vp.StagedReviewVerdictAgent)
	}

	// HOP THREE: the coordinator continuation carries NEITHER.
	completeDelegationChild(t, store, "preflight-job/delegation/verdict", JobSucceeded, AgentResult{
		Decision: "approved", Summary: "verdict", Evidence: EvidenceStaticOnly, EvidenceDeclared: true,
	})
	if err := engine.AdvanceJob(ctx, "preflight-job/delegation/verdict"); err != nil {
		t.Fatalf("AdvanceJob(verdict): %v", err)
	}
	cont := mustJob(t, store, "preflight-job/continuation")
	cp, err := unmarshalPayload(cont.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload(continuation): %v", err)
	}
	if cp.InheritedEvidence != "" {
		t.Fatalf("the continuation inherited ceiling %q; a chain of honest static_only stages would silently constrain work that can execute", cp.InheritedEvidence)
	}
	if cp.StagedReviewVerdictAgent != "" {
		t.Fatalf("the continuation carries the staged marker %q, so it would impose a ceiling on its own delegations", cp.StagedReviewVerdictAgent)
	}
}

// The production-entry test: a real Mailbox.Run storing a clamped result, with
// the STORED payload asserted rather than the returned value, because a clamp
// applied after the write would pass a return-value assertion.
func TestMailboxRunClampsAStagedVerdict(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{store: store, resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree")}
	agent := runtime.Agent{Name: "verdict", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "owner/repo", Role: "reviewer"}
	adapter := &fakeDelivery{outputs: []string{
		`{"gitmoot_result":{"decision":"approved","summary":"looks fine","evidence":"executed",` +
			`"tests_run":["go test ./..."],"findings":[],"changes_made":[],"needs":[],"delegations":[]}}`,
	}}
	if _, err := mailbox.Enqueue(ctx, JobRequest{
		ID: "verdict-job", Agent: "verdict", Action: "review", Repo: "owner/repo",
		InheritedEvidence: EvidenceStaticOnly,
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	result, err := mailbox.Run(ctx, "verdict-job", agent, adapter)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Evidence != EvidenceStaticOnly {
		t.Fatalf("Run returned %q", result.Evidence)
	}
	job, err := store.GetJob(ctx, "verdict-job")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	var stored JobPayload
	if err := json.Unmarshal([]byte(job.Payload), &stored); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if stored.Result == nil || stored.Result.Evidence != EvidenceStaticOnly {
		t.Fatalf("STORED evidence is not clamped: %+v", stored.Result)
	}
	events, err := store.ListJobEvents(ctx, "verdict-job")
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	var clamped bool
	for _, e := range events {
		if e.Kind == InheritedEvidenceClampedEvent {
			clamped = true
		}
	}
	if !clamped {
		t.Fatalf("no %s event recorded", InheritedEvidenceClampedEvent)
	}
}

// The should-SUCCEED arm: an ordinary review keeps its executed claim.
func TestMailboxRunLeavesAnUnstagedVerdictAlone(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{store: store, resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree")}
	agent := runtime.Agent{Name: "verdict", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "owner/repo", Role: "reviewer"}
	adapter := &fakeDelivery{outputs: []string{
		`{"gitmoot_result":{"decision":"approved","summary":"ran it","evidence":"executed",` +
			`"tests_run":["go test ./..."],"findings":[],"changes_made":[],"needs":[],"delegations":[]}}`,
	}}
	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "plain-job", Agent: "verdict", Action: "review", Repo: "owner/repo"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	result, err := mailbox.Run(ctx, "plain-job", agent, adapter)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Evidence != EvidenceExecuted {
		t.Fatalf("an unstaged verdict was downgraded to %q", result.Evidence)
	}
}
