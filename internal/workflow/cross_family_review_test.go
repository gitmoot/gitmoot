package workflow

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/runtime"
)

// stubAgentLister is an in-memory AgentLister for the cross-family selector tests.
type stubAgentLister struct {
	agents []db.Agent
}

func (s stubAgentLister) ListAgents(context.Context) ([]db.Agent, error) {
	return s.agents, nil
}

func reviewAgent(name, rt, repoScope string) db.Agent {
	return db.Agent{Name: name, Runtime: rt, RepoScope: repoScope, Capabilities: []string{"review"}}
}

// TestPickCrossFamilyReviewerPrefersDifferentFamilyRegistered: a registered
// review-capable agent of a DIFFERENT runtime family wins, deterministically by
// name, and is never tagged self-family.
func TestPickCrossFamilyReviewerPrefersDifferentFamilyRegistered(t *testing.T) {
	store := stubAgentLister{agents: []db.Agent{
		// Same-family agent sorts first by name but must be skipped in favor of cross.
		reviewAgent("aaa-codex-reviewer", runtime.CodexRuntime, "owner/repo"),
		reviewAgent("zzz-claude-reviewer", runtime.ClaudeRuntime, "owner/repo"),
	}}
	reviewer, ok, err := PickCrossFamilyReviewer(context.Background(), store, runtime.CodexRuntime, "owner/repo", nil)
	if err != nil {
		t.Fatalf("PickCrossFamilyReviewer error: %v", err)
	}
	if !ok {
		t.Fatal("expected a reviewer, got none")
	}
	if reviewer.SelfFamily {
		t.Fatal("a different-family reviewer must not be tagged self-family")
	}
	if reviewer.Runtime != runtime.ClaudeRuntime || reviewer.RegisteredAgent != "zzz-claude-reviewer" {
		t.Fatalf("picked %+v, want the claude registered reviewer", reviewer)
	}
}

// TestPickCrossFamilyReviewerEphemeralFallback: with no registered different-family
// agent but the rotation target authed, an ephemeral DIFFERENT-family read-only
// leg is materialized (codex -> claude).
func TestPickCrossFamilyReviewerEphemeralFallback(t *testing.T) {
	store := stubAgentLister{}
	authed := map[string]bool{runtime.ClaudeRuntime: true}
	reviewer, ok, err := PickCrossFamilyReviewer(context.Background(), store, runtime.CodexRuntime, "owner/repo", authed)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if !ok || reviewer.SelfFamily {
		t.Fatalf("expected a cross-family ephemeral reviewer, got ok=%v self=%v", ok, reviewer.SelfFamily)
	}
	if reviewer.Runtime != runtime.ClaudeRuntime || reviewer.Ephemeral == nil {
		t.Fatalf("picked %+v, want an ephemeral claude reviewer", reviewer)
	}
	if reviewer.Ephemeral.AutonomyPolicy != runtime.AutonomyPolicyReadOnly {
		t.Fatalf("ephemeral reviewer must be read-only, got %q", reviewer.Ephemeral.AutonomyPolicy)
	}
	if !contains(reviewer.Ephemeral.Capabilities, "review") {
		t.Fatalf("ephemeral reviewer must declare review capability, got %v", reviewer.Ephemeral.Capabilities)
	}
}

// TestPickCrossFamilyReviewerSameFamilyFallbackWithWarning (REFINEMENT #1): when
// NO different family is available (no different-family agent, rotation target NOT
// authed) but the implementer's own family IS authed, fall back to a SAME-family
// reviewer tagged SelfFamily (the caller emits the warning). It is never silent.
func TestPickCrossFamilyReviewerSameFamilyFallbackWithWarning(t *testing.T) {
	// (a) registered same-family agent path.
	store := stubAgentLister{agents: []db.Agent{
		reviewAgent("codex-reviewer", runtime.CodexRuntime, "owner/repo"),
	}}
	reviewer, ok, err := PickCrossFamilyReviewer(context.Background(), store, runtime.CodexRuntime, "owner/repo", nil)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if !ok {
		t.Fatal("expected a same-family fallback reviewer, got none")
	}
	if !reviewer.SelfFamily {
		t.Fatal("same-family fallback MUST be tagged self-family so it weights below cross-family")
	}
	if reviewer.Runtime != runtime.CodexRuntime || reviewer.RegisteredAgent != "codex-reviewer" {
		t.Fatalf("picked %+v, want the same-family registered reviewer", reviewer)
	}

	// (b) ephemeral same-family path: no registered agent, only the implementer's
	// own family authed (rotation target NOT authed).
	reviewer2, ok2, err2 := PickCrossFamilyReviewer(context.Background(), stubAgentLister{}, runtime.CodexRuntime, "owner/repo", map[string]bool{runtime.CodexRuntime: true})
	if err2 != nil {
		t.Fatalf("error: %v", err2)
	}
	if !ok2 || !reviewer2.SelfFamily {
		t.Fatalf("expected an ephemeral same-family fallback, got ok=%v self=%v", ok2, reviewer2.SelfFamily)
	}
	if reviewer2.Runtime != runtime.CodexRuntime || reviewer2.Ephemeral == nil {
		t.Fatalf("picked %+v, want an ephemeral same-family codex reviewer", reviewer2)
	}
}

// TestPickCrossFamilyReviewerSkipsWhenNoRuntimeAuthed: ok=false ONLY when no
// review-capable runtime is authed at all (no registered reviewer + nothing
// authed) — the caller then writes no review row.
func TestPickCrossFamilyReviewerSkipsWhenNoRuntimeAuthed(t *testing.T) {
	_, ok, err := PickCrossFamilyReviewer(context.Background(), stubAgentLister{}, runtime.CodexRuntime, "owner/repo", nil)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if ok {
		t.Fatal("expected SKIP (ok=false) when no review-capable runtime is authed")
	}
}

// TestPickCrossFamilyReviewerUnknownImplementerSkips: an unrecoverable implementer
// family yields SKIP rather than risk a silent same-family review.
func TestPickCrossFamilyReviewerUnknownImplementerSkips(t *testing.T) {
	store := stubAgentLister{agents: []db.Agent{reviewAgent("claude-reviewer", runtime.ClaudeRuntime, "owner/repo")}}
	_, ok, err := PickCrossFamilyReviewer(context.Background(), store, "mystery-runtime", "owner/repo", map[string]bool{runtime.ClaudeRuntime: true})
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if ok {
		t.Fatal("an unknown implementer family must SKIP, never guess a reviewer")
	}
}

// TestPickCrossFamilyReviewerRepoScope: a registered reviewer whose scope does not
// cover the repo is excluded; a global (empty-scope) reviewer is included.
func TestPickCrossFamilyReviewerRepoScope(t *testing.T) {
	store := stubAgentLister{agents: []db.Agent{
		reviewAgent("scoped-elsewhere", runtime.ClaudeRuntime, "other/repo"),
		reviewAgent("global", runtime.ClaudeRuntime, ""),
	}}
	reviewer, ok, err := PickCrossFamilyReviewer(context.Background(), store, runtime.CodexRuntime, "owner/repo", nil)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if !ok || reviewer.RegisteredAgent != "global" {
		t.Fatalf("picked %+v, want the global (empty-scope) reviewer", reviewer)
	}
}

// TestReviewLegPromptAssembly: the prompt is built from Instructions + TaskTitle +
// resolved Goal.Title vs the diff + ChangesMade, asks for the rubric dimensions,
// and never leaks back into the implementer payload (it is a fresh string).
func TestReviewLegPromptAssembly(t *testing.T) {
	payload := JobPayload{
		TaskTitle:    "Add cross-family review",
		Instructions: "Implement the soft review signal",
		Result:       &AgentResult{ChangesMade: []string{"added cross_family_review.go"}},
	}
	prompt := ReviewLegPrompt(payload, "Mode A review agent", "diff --git a/x b/x")
	for _, want := range []string{
		"Add cross-family review", "Implement the soft review signal", "Mode A review agent",
		"diff --git a/x b/x", "added cross_family_review.go",
		"coverage", "containment", "fidelity", "architecture", "readability", "abstraction",
		"metadata.rubric",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
	// Anti-gaming: assembling the prompt must not mutate the implementer payload.
	if payload.Instructions != "Implement the soft review signal" {
		t.Fatal("ReviewLegPrompt must not mutate the implementer payload")
	}
}

// TestReviewLegPromptDegradesWithoutDiff: when the diff read failed (empty diff)
// the prompt still assembles, telling the reviewer to lean on ChangesMade.
func TestReviewLegPromptDegradesWithoutDiff(t *testing.T) {
	payload := JobPayload{TaskTitle: "T", Result: &AgentResult{ChangesMade: []string{"c1"}}}
	prompt := ReviewLegPrompt(payload, "", "")
	if !strings.Contains(prompt, "PR diff unavailable") {
		t.Fatalf("expected the degrade marker, got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "c1") {
		t.Fatal("degrade path must still surface ChangesMade")
	}
}

// TestParseReviewRubricKeepsKnownDimensionsClamped: only the known dimensions are
// kept, each clamped to [0,1]; unknown keys are dropped.
func TestParseReviewRubricKeepsKnownDimensionsClamped(t *testing.T) {
	raw := map[string]float64{
		"coverage": 0.8, "fidelity": 1.5, "containment": -0.2, "hallucinated": 0.9,
	}
	out := ParseReviewRubric(AgentResult{Summary: "scope drift"}, raw)
	if out.Findings != "scope drift" {
		t.Fatalf("findings = %q, want the summary", out.Findings)
	}
	if got := out.Rubric["coverage"]; got != 0.8 {
		t.Fatalf("coverage = %v, want 0.8", got)
	}
	if got := out.Rubric["fidelity"]; got != 1.0 {
		t.Fatalf("fidelity = %v, want clamped 1.0", got)
	}
	if got := out.Rubric["containment"]; got != 0.0 {
		t.Fatalf("containment = %v, want clamped 0.0", got)
	}
	if _, present := out.Rubric["hallucinated"]; present {
		t.Fatal("unknown rubric keys must be dropped")
	}
}

// TestParseReviewRubricEmpty: an empty rubric yields an empty map (no fabricated
// scores) and a default findings string.
func TestParseReviewRubricEmpty(t *testing.T) {
	out := ParseReviewRubric(AgentResult{}, nil)
	if len(out.Rubric) != 0 {
		t.Fatalf("empty rubric must yield no dimensions, got %v", out.Rubric)
	}
	if strings.TrimSpace(out.Findings) == "" {
		t.Fatal("findings should default to a non-empty marker")
	}
}

// recordingReviewDispatcher is a stub ReviewLegDispatcher that records the call
// and returns a canned OutcomeReviewed (or err/skip), so the engine trigger can be
// tested without a live runtime.
type recordingReviewDispatcher struct {
	called  int
	jobIDs  []string
	heads   []string
	outcome Outcome
	ok      bool
	err     error
}

func (r *recordingReviewDispatcher) Review(_ context.Context, job db.Job, _ JobPayload, mergedHead string) (Outcome, bool, error) {
	r.called++
	r.jobIDs = append(r.jobIDs, job.ID)
	r.heads = append(r.heads, mergedHead)
	return r.outcome, r.ok, r.err
}

func seedMergeReviewJobs(t *testing.T, store *db.Store) {
	t.Helper()
	seedImplementJobForHarvest(t, store)
	insertCompletedJob(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
		Repo: "jerryfane/gitmoot", Branch: "task-7", PullRequest: 7, HeadSHA: "head123",
		TaskID: "task-7", TaskTitle: "Workflow Engine", LeadAgent: "lead",
		Result: &AgentResult{Decision: "approved", Summary: "looks good"},
	})
}

// TestEngineDispatchesReviewLegOnMerge proves the cross-family review leg fires on
// a merge, attributed to the implement job, and its OutcomeReviewed is harvested.
func TestEngineDispatchesReviewLegOnMerge(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	engine.MergeGate = &fakeMergeGate{decision: MergeDecision{Ready: true, Merged: true, MergeCommitSHA: "merge123"}}
	harvester := &recordingHarvester{}
	engine.OutcomeHarvester = harvester
	dispatcher := &recordingReviewDispatcher{
		ok: true,
		outcome: Outcome{
			Kind: OutcomeReviewed, Repo: "jerryfane/gitmoot", PullRequest: 7,
			Reviewer: "claude", Rubric: map[string]float64{"coverage": 0.8},
		},
	}
	engine.ReviewLegDispatcher = dispatcher
	seedMergeReviewJobs(t, store)

	if err := engine.AdvanceJob(ctx, "review-job"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	if dispatcher.called != 1 {
		t.Fatalf("review dispatcher called %d times, want 1", dispatcher.called)
	}
	if dispatcher.jobIDs[0] != "implement-job" {
		t.Fatalf("review leg attributed to %q, want implement-job", dispatcher.jobIDs[0])
	}
	if dispatcher.heads[0] != "head123" {
		t.Fatalf("review leg head sha = %q, want the PR head head123", dispatcher.heads[0])
	}
	// Both the merge floor AND the reviewed outcome are harvested.
	kinds := map[OutcomeKind]bool{}
	for _, o := range harvester.snapshot() {
		kinds[o.Kind] = true
	}
	if !kinds[OutcomeMerged] || !kinds[OutcomeReviewed] {
		t.Fatalf("harvested kinds = %v, want both merged and reviewed", kinds)
	}
}

// TestEngineNilReviewDispatcherIsByteIdentical proves off-by-default: with no
// ReviewLegDispatcher the merge advances exactly as before and no review leg runs.
func TestEngineNilReviewDispatcherIsByteIdentical(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	engine.MergeGate = &fakeMergeGate{decision: MergeDecision{Ready: true, Merged: true, MergeCommitSHA: "merge123"}}
	engine.OutcomeHarvester = &recordingHarvester{}
	// No ReviewLegDispatcher.
	seedMergeReviewJobs(t, store)

	if err := engine.AdvanceJob(ctx, "review-job"); err != nil {
		t.Fatalf("AdvanceJob with nil review dispatcher returned error: %v", err)
	}
	assertTaskState(t, store, "task-7", TaskMerged)
	for _, o := range engine.OutcomeHarvester.(*recordingHarvester).snapshot() {
		if o.Kind == OutcomeReviewed {
			t.Fatal("nil review dispatcher must not produce a reviewed outcome")
		}
	}
}

// TestEngineReviewDispatchErrorNeverFailsMerge proves a review-leg failure is
// best-effort: the merge still completes and a cross_family_review_failed event is
// recorded on the implement job (the review never blocks/fails the job).
func TestEngineReviewDispatchErrorNeverFailsMerge(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	engine.MergeGate = &fakeMergeGate{decision: MergeDecision{Ready: true, Merged: true, MergeCommitSHA: "merge123"}}
	engine.OutcomeHarvester = &recordingHarvester{}
	engine.ReviewLegDispatcher = &recordingReviewDispatcher{err: errors.New("review boom")}
	seedMergeReviewJobs(t, store)

	if err := engine.AdvanceJob(ctx, "review-job"); err != nil {
		t.Fatalf("AdvanceJob must not fail on a review error, got: %v", err)
	}
	assertTaskState(t, store, "task-7", TaskMerged)

	events, err := store.ListJobEvents(ctx, "implement-job")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	found := false
	for _, ev := range events {
		if ev.Kind == "cross_family_review_failed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a cross_family_review_failed job event, got %+v", events)
	}
}

// TestEngineReviewDispatchSkipWritesNothing proves a SKIP (ok=false, no
// review-capable runtime authed) writes no review row and never errors.
func TestEngineReviewDispatchSkipWritesNothing(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	engine.MergeGate = &fakeMergeGate{decision: MergeDecision{Ready: true, Merged: true, MergeCommitSHA: "merge123"}}
	harvester := &recordingHarvester{}
	engine.OutcomeHarvester = harvester
	engine.ReviewLegDispatcher = &recordingReviewDispatcher{ok: false}
	seedMergeReviewJobs(t, store)

	if err := engine.AdvanceJob(ctx, "review-job"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	for _, o := range harvester.snapshot() {
		if o.Kind == OutcomeReviewed {
			t.Fatal("a SKIP must not harvest a reviewed outcome")
		}
	}
}
