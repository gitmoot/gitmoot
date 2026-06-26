package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/subprocess"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// TestCrossFamilyReviewFullChain is the FULL-CHAIN integration test for #469. It
// drives a real workflow.Engine through AdvanceJob to a MERGE, where the engine
// dispatches a CROSS-FAMILY review leg to a SHELL-runtime stub reviewer (a family
// different from the codex implementer), the stub echoes a canned gitmoot_result
// rubric, the engine derives Outcome{Kind:OutcomeReviewed}, and the REAL
// skillopt.OutcomeHarvester writes a SECOND, judge-tagged, down-weighted
// FeedbackEvent into the SAME auto-trace:<versionID> run alongside the verifiable
// floor — with NO live LLM and NO live GitHub. Everything between AdvanceJob and
// the persisted rows is production code.
//
// It also pins the invariants: the review never blocks the job (the merge still
// happens), no candidate/promotion row is written, and the floor row coexists with
// the review row.

// reviewShellScript is the stub reviewer body run via `sh -c <script> gitmoot
// <prompt>`. It ignores its input and echoes a canned gitmoot_result whose
// metadata.rubric carries the [0,1] dimension scores, exactly the contract the
// dispatcher's parser reads.
const reviewShellScript = `printf '%s' '{"gitmoot_result":{"decision":"changes_requested","summary":"scope drift on PR","metadata":{"rubric":{"coverage":0.4,"containment":0.6,"fidelity":0.5,"architecture":0.7,"readability":0.8,"abstraction":0.6}}}}'`

// seedCodexImplementJob seeds a completed implement job on the codex runtime,
// attributed to the template version, so the dispatcher recovers the implementer
// family as codex and the review row lands on this version.
func seedCodexImplementJob(t *testing.T, store *db.Store, version db.AgentTemplateVersion) {
	t.Helper()
	// Register the implementer agent so resolveImplementerRuntime reads codex.
	if err := store.UpsertAgent(context.Background(), db.Agent{
		Name:         "lead",
		Runtime:      runtime.CodexRuntime,
		RepoScope:    "jerryfane/gitmoot",
		Capabilities: []string{"implement"},
		RuntimeRef:   "last",
	}); err != nil {
		t.Fatalf("UpsertAgent(lead) returned error: %v", err)
	}
	insertChainJob(t, store, db.Job{ID: "implement-job", Agent: "lead", Type: "implement"}, workflow.JobPayload{
		Repo: "jerryfane/gitmoot", Branch: "task-7", PullRequest: 7, HeadSHA: "head123",
		TaskID: "task-7", TaskTitle: "Workflow Engine", LeadAgent: "lead",
		TemplateID: version.TemplateID, TemplateResolvedCommit: version.ResolvedCommit,
		Instructions: "Implement the cross-family review",
		Result:       &workflow.AgentResult{Decision: "implemented", Summary: "did the work", ChangesMade: []string{"x.go"}},
	})
}

// registerShellReviewer registers a REVIEW-capable agent on the SHELL runtime (a
// family different from the codex implementer) whose RuntimeRef is the canned
// rubric-echoing script — a deterministic cross-family stub reviewer.
func registerShellReviewer(t *testing.T, store *db.Store) {
	t.Helper()
	if err := store.UpsertAgent(context.Background(), db.Agent{
		Name:         "shell-reviewer",
		Runtime:      runtime.ShellRuntime,
		RepoScope:    "jerryfane/gitmoot",
		Capabilities: []string{"ask", "review"},
		RuntimeRef:   reviewShellScript,
	}); err != nil {
		t.Fatalf("UpsertAgent(shell-reviewer) returned error: %v", err)
	}
}

// reviewDispatcherForTest builds the REAL crossFamilyReviewDispatcher wired to the
// real store + a real ShellAdapter builder, with a stub diff reader and an authed
// probe that reports the shell reviewer available.
func reviewDispatcherForTest(store *db.Store, diff reviewDiffFileReader) *crossFamilyReviewDispatcher {
	return &crossFamilyReviewDispatcher{
		store:        store,
		diff:         diff,
		buildAdapter: buildRuntimeAdapter,
		authed: func(context.Context) map[string]bool {
			return map[string]bool{runtime.ShellRuntime: true}
		},
	}
}

func TestCrossFamilyReviewFullChain(t *testing.T) {
	ctx := context.Background()
	store := openTraceChainStore(t)
	version := installChainTemplate(t, store, "planner")
	gate := &stubMergeGate{decision: workflow.MergeDecision{Ready: true, Merged: true, MergeCommitSHA: "merge123"}}
	engine := chainEngine(store, gate)

	// The real harvester (built through the daemon construction path) AND the real
	// cross-family review dispatcher with a shell stub reviewer.
	engine.OutcomeHarvester = enabledHarvesterFor(t, store, harvestGitHubStub{status: realCIChainStatus()})
	engine.ReviewLegDispatcher = reviewDispatcherForTest(store, harvestGitHubStub{})

	seedCodexImplementJob(t, store, version)
	registerShellReviewer(t, store)
	seedChainApprovingReview(t, store, "head123")

	if err := engine.AdvanceJob(ctx, "review-job"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}

	events := autoTraceFeedback(t, store, version.ID)
	// The verifiable floor (gitmoot-auto) AND the soft review (gitmoot-review:shell).
	if len(events) != 2 {
		t.Fatalf("expected the floor + review row (2), got %d: %+v", len(events), events)
	}
	var floor, review *db.FeedbackEvent
	for i := range events {
		switch {
		case events[i].Reviewer == "gitmoot-auto":
			floor = &events[i]
		case strings.HasPrefix(events[i].Reviewer, "gitmoot-review:"):
			review = &events[i]
		}
	}
	if floor == nil {
		t.Fatalf("verifiable floor row missing; got %+v", events)
	}
	if review == nil {
		t.Fatalf("cross-family review row missing; got %+v", events)
	}
	// Cross-family ENFORCED: reviewer family (shell) != implementer family (codex);
	// the reviewer id is the cross-family tag, NOT the self-family one.
	if review.Reviewer != "gitmoot-review:shell" {
		t.Fatalf("review reviewer = %q, want gitmoot-review:shell (cross-family)", review.Reviewer)
	}
	if strings.HasPrefix(review.Reviewer, "gitmoot-review-self:") {
		t.Fatal("a cross-family review must NOT be tagged self-family")
	}
	// Distinct item ids: the review row never overwrote the floor.
	if floor.ItemID == review.ItemID {
		t.Fatalf("floor and review share item id %q (review overwrote the floor)", floor.ItemID)
	}
	// The findings from the shell stub flowed through to the persisted reasoning.
	if !strings.Contains(review.Reasoning, "scope drift") {
		t.Fatalf("review reasoning = %q, want the stub's findings", review.Reasoning)
	}

	// Judge-tagged: the review item carries judge_derived=true.
	items, err := store.ListEvalReviewItems(ctx, "auto-trace:"+version.ID)
	if err != nil {
		t.Fatalf("ListEvalReviewItems returned error: %v", err)
	}
	var reviewMeta map[string]any
	for _, item := range items {
		if item.ItemID == review.ItemID {
			if err := json.Unmarshal([]byte(item.MetadataJSON), &reviewMeta); err != nil {
				t.Fatalf("review item metadata unmarshal: %v", err)
			}
		}
	}
	if reviewMeta == nil || reviewMeta["judge_derived"] != true {
		t.Fatalf("review item must be judge_derived=true, got %v", reviewMeta)
	}

	// The merge still happened (the review never blocked the job).
	if len(gate.requests) != 1 {
		t.Fatalf("merge gate requests = %d, want 1 (the merge runs regardless of the review)", len(gate.requests))
	}

	// Manual promotion preserved: the harvester writes ONLY eval/feedback rows, so
	// no NEW template version (candidate/promotion) was created — only the single
	// installed version still exists.
	versions, err := store.ListAgentTemplateVersions(ctx, version.TemplateID)
	if err != nil {
		t.Fatalf("ListAgentTemplateVersions returned error: %v", err)
	}
	if len(versions) != 1 {
		t.Fatalf("the review must create NO new template version (manual promotion), got %d", len(versions))
	}
}

// TestCrossFamilyReviewOffByDefault: with cross_family_review_enabled UNSET,
// daemonReviewLegDispatcher returns nil through the SAME gate, so the engine
// dispatches no review leg and writes ONLY the verifiable floor row (the review is
// byte-identically absent).
func TestCrossFamilyReviewOffByDefault(t *testing.T) {
	ctx := context.Background()
	store := openTraceChainStore(t)
	version := installChainTemplate(t, store, "planner")
	gate := &stubMergeGate{decision: workflow.MergeDecision{Ready: true, Merged: true, MergeCommitSHA: "merge123"}}
	engine := chainEngine(store, gate)
	engine.OutcomeHarvester = enabledHarvesterFor(t, store, harvestGitHubStub{status: realCIChainStatus()})

	// auto_trace on but cross_family_review OFF: the dispatcher must be nil.
	root := writeHarvestConfig(t, "[skillopt]\nauto_trace_enabled = true\n")
	if d := daemonReviewLegDispatcher(store, harvestGitHubStub{}, "", root); d != nil {
		t.Fatalf("off-by-default: review dispatcher must be nil, got %T", d)
	}
	// Leave engine.ReviewLegDispatcher nil (what the daemon would wire here).

	seedCodexImplementJob(t, store, version)
	registerShellReviewer(t, store)
	seedChainApprovingReview(t, store, "head123")

	if err := engine.AdvanceJob(ctx, "review-job"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}

	events := autoTraceFeedback(t, store, version.ID)
	if len(events) != 1 {
		t.Fatalf("off-by-default must write only the floor row, got %d: %+v", len(events), events)
	}
	if events[0].Reviewer != "gitmoot-auto" {
		t.Fatalf("off-by-default row reviewer = %q, want only the verifiable floor", events[0].Reviewer)
	}
}

// stubReviewAdapter is a DeliveryAdapter that echoes a canned rubric regardless of
// the agent's runtime, so the same-family-fallback path can be exercised without a
// live codex/claude runtime.
type stubReviewAdapter struct{}

func (stubReviewAdapter) Deliver(_ context.Context, _ runtime.Agent, _ runtime.Job) (runtime.Result, error) {
	return runtime.Result{Raw: `{"gitmoot_result":{"summary":"same-family review","metadata":{"rubric":{"coverage":0.5}}}}`}, nil
}

// TestCrossFamilyReviewSameFamilyFallbackWarns (REFINEMENT #1): when NO different
// family is available, the dispatcher falls back to a SAME-family reviewer, emits
// the cross_family_review_samefamily_fallback warning event, and tags the row
// gitmoot-review-self:<rt> + self_family=true so it weights below cross-family.
func TestCrossFamilyReviewSameFamilyFallbackWarns(t *testing.T) {
	ctx := context.Background()
	store := openTraceChainStore(t)
	version := installChainTemplate(t, store, "planner")
	gate := &stubMergeGate{decision: workflow.MergeDecision{Ready: true, Merged: true, MergeCommitSHA: "merge123"}}
	engine := chainEngine(store, gate)
	engine.OutcomeHarvester = enabledHarvesterFor(t, store, harvestGitHubStub{status: realCIChainStatus()})

	// A SAME-family (codex) registered reviewer; NO different family authed.
	if err := store.UpsertAgent(ctx, db.Agent{
		Name: "codex-reviewer", Role: "reviewer", Runtime: runtime.CodexRuntime,
		RepoScope: "jerryfane/gitmoot", Capabilities: []string{"ask", "review"}, RuntimeRef: "last",
	}); err != nil {
		t.Fatalf("UpsertAgent(codex-reviewer) returned error: %v", err)
	}
	engine.ReviewLegDispatcher = &crossFamilyReviewDispatcher{
		store: store,
		diff:  harvestGitHubStub{},
		buildAdapter: func(runtime.Agent, string, subprocess.Runner) (workflow.DeliveryAdapter, error) {
			return stubReviewAdapter{}, nil
		},
		authed: func(context.Context) map[string]bool { return map[string]bool{} },
	}

	seedCodexImplementJob(t, store, version)
	seedChainApprovingReview(t, store, "head123")

	if err := engine.AdvanceJob(ctx, "review-job"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}

	// The same-family warning event fired on the implement job.
	events, err := store.ListJobEvents(ctx, "implement-job")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	warned := false
	for _, ev := range events {
		if ev.Kind == "cross_family_review_samefamily_fallback" {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("expected a cross_family_review_samefamily_fallback warning event, got %+v", events)
	}

	// The review row is tagged self-family (gitmoot-review-self:codex).
	rows := autoTraceFeedback(t, store, version.ID)
	foundSelf := false
	for _, r := range rows {
		if r.Reviewer == "gitmoot-review-self:codex" {
			foundSelf = true
		}
	}
	if !foundSelf {
		t.Fatalf("expected a gitmoot-review-self:codex row, got %+v", rows)
	}
}

// TestDaemonReviewLegDispatcherGate proves the admission gate: the dispatcher is
// non-nil ONLY when BOTH knobs are set, and nil otherwise.
func TestDaemonReviewLegDispatcherGate(t *testing.T) {
	store := openHarvestStore(t)
	gh := harvestGitHubStub{}

	bothOff := writeHarvestConfig(t, "[skillopt]\n")
	if d := daemonReviewLegDispatcher(store, gh, "", bothOff); d != nil {
		t.Fatalf("both knobs off: want nil, got %T", d)
	}
	reviewOnly := writeHarvestConfig(t, "[skillopt]\ncross_family_review_enabled = true\n")
	if d := daemonReviewLegDispatcher(store, gh, "", reviewOnly); d != nil {
		t.Fatal("cross_family_review without auto_trace must be nil (requires BOTH)")
	}
	bothOn := writeHarvestConfig(t, "[skillopt]\nauto_trace_enabled = true\ncross_family_review_enabled = true\n")
	d := daemonReviewLegDispatcher(store, gh, "", bothOn)
	if d == nil {
		t.Fatal("both knobs on: want a non-nil review dispatcher")
	}
	if _, ok := d.(*crossFamilyReviewDispatcher); !ok {
		t.Fatalf("expected the real *crossFamilyReviewDispatcher, got %T", d)
	}
}
