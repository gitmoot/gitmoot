package workflow

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jerryfane/gitmoot/internal/db"
)

// childJobWith builds a succeeded delegation child job whose stored payload
// carries result for use in buildUpstreamDepBlock unit tests. It mirrors how the
// engine stores a finished delegation child (agent + action via Type + the
// JobPayload its result/PR/HeadSHA live in).
func childJobWith(t *testing.T, id, agent, action string, payload JobPayload) db.Job {
	t.Helper()
	encoded, err := marshalPayload(payload)
	if err != nil {
		t.Fatalf("marshalPayload(%s) returned error: %v", id, err)
	}
	return db.Job{ID: id, Agent: agent, Type: action, State: string(JobSucceeded), Payload: encoded}
}

// TestUpstreamDepBlockInjectsSucceededDepResult pins the #419 core: a ready
// dependent's upstream block carries each succeeded direct dep's decision,
// summary preview, PR link, changes_made count, short HeadSHA, and the dep's
// fenced artifact_body.
func TestUpstreamDepBlockInjectsSucceededDepResult(t *testing.T) {
	children := map[string]db.Job{
		"research": childJobWith(t, "parent/delegation/research", "researcher", "review", JobPayload{
			Repo:        "jerryfane/gitmoot",
			PullRequest: 42,
			HeadSHA:     "abcdef0123456789",
			Result: &AgentResult{
				Decision:     "approved",
				Summary:      "found three relevant prior arts",
				ChangesMade:  []string{"noted A", "noted B"},
				ArtifactBody: "RESEARCH BRIEF BODY",
			},
		}),
	}
	dep := Delegation{ID: "write", Agent: "writer", Action: "implement", Deps: []string{"research"}}
	block := (Engine{InjectUpstreamDepContext: true}).buildUpstreamDepBlock(dep, children, nil)

	for _, want := range []string{
		"Upstream dependency results:",
		`- dep "research" (agent researcher, action review): approved`,
		"found three relevant prior arts",
		"https://github.com/jerryfane/gitmoot/pull/42",
		"[changes_made: 2]",
		"[head abcdef0]", // 7-char short SHA
		"artifact_body:",
		"RESEARCH BRIEF BODY",
		"```",
	} {
		if !strings.Contains(block, want) {
			t.Fatalf("upstream block missing %q\n%s", want, block)
		}
	}
}

// TestUpstreamDepBlockMultiDepStableOrder pins that multiple deps render sorted
// by id regardless of the order they were declared, so the injected prompt is
// deterministic.
func TestUpstreamDepBlockMultiDepStableOrder(t *testing.T) {
	children := map[string]db.Job{
		"zeta": childJobWith(t, "parent/delegation/zeta", "z", "review", JobPayload{
			Result: &AgentResult{Decision: "approved", Summary: "zeta done"},
		}),
		"alpha": childJobWith(t, "parent/delegation/alpha", "a", "review", JobPayload{
			Result: &AgentResult{Decision: "approved", Summary: "alpha done"},
		}),
	}
	// Declared zeta-before-alpha; output must still be alpha-before-zeta.
	dep := Delegation{ID: "sink", Agent: "s", Action: "implement", Deps: []string{"zeta", "alpha"}}
	block := (Engine{InjectUpstreamDepContext: true}).buildUpstreamDepBlock(dep, children, nil)
	ai := strings.Index(block, `dep "alpha"`)
	zi := strings.Index(block, `dep "zeta"`)
	if ai < 0 || zi < 0 {
		t.Fatalf("expected both deps in block\n%s", block)
	}
	if ai > zi {
		t.Fatalf("deps not sorted by id: alpha at %d, zeta at %d\n%s", ai, zi, block)
	}
}

// TestUpstreamDepBlockNilResultDepDecisionOnly pins the defensive nil-Result
// path: a succeeded dep whose payload has no Result contributes a decision/state
// line only (no summary, no fenced body, no panic).
func TestUpstreamDepBlockNilResultDepDecisionOnly(t *testing.T) {
	children := map[string]db.Job{
		"dep1": childJobWith(t, "parent/delegation/dep1", "d", "review", JobPayload{}), // Result == nil
	}
	dep := Delegation{ID: "sink", Agent: "s", Action: "implement", Deps: []string{"dep1"}}
	block := (Engine{InjectUpstreamDepContext: true}).buildUpstreamDepBlock(dep, children, nil)
	if !strings.Contains(block, `- dep "dep1" (agent d, action review): succeeded`) {
		t.Fatalf("nil-Result dep should fall back to job state\n%s", block)
	}
	if strings.Contains(block, "artifact_body") || strings.Contains(block, " — ") {
		t.Fatalf("nil-Result dep must emit no summary/body\n%s", block)
	}
}

// TestUpstreamDepBlockOnlySucceededDeps pins that a non-succeeded (defensively
// reached) dep contributes nothing; with no succeeded deps the block is empty.
func TestUpstreamDepBlockOnlySucceededDeps(t *testing.T) {
	children := map[string]db.Job{
		"failed": {ID: "parent/delegation/failed", Agent: "f", Type: "review", State: string(JobFailed)},
	}
	dep := Delegation{ID: "sink", Agent: "s", Action: "implement", Deps: []string{"failed"}}
	if got := (Engine{InjectUpstreamDepContext: true}).buildUpstreamDepBlock(dep, children, nil); got != "" {
		t.Fatalf("expected empty block for non-succeeded dep, got:\n%s", got)
	}
	// A dep with no deps at all also yields an empty block.
	if got := (Engine{InjectUpstreamDepContext: true}).buildUpstreamDepBlock(Delegation{ID: "x"}, children, nil); got != "" {
		t.Fatalf("expected empty block for no-deps delegation, got:\n%s", got)
	}
}

// TestUpstreamDepBlockResolvesDedupWinner pins that a dep pointing at a
// fingerprint-deduped delegation is followed to its winning sibling.
func TestUpstreamDepBlockResolvesDedupWinner(t *testing.T) {
	winner := childJobWith(t, "parent/delegation/winner", "w", "review", JobPayload{
		Result: &AgentResult{Decision: "approved", Summary: "winner summary"},
	})
	dedupWinners := map[string]db.Job{"dup": winner}
	dep := Delegation{ID: "sink", Agent: "s", Action: "implement", Deps: []string{"dup"}}
	block := (Engine{InjectUpstreamDepContext: true}).buildUpstreamDepBlock(dep, map[string]db.Job{}, dedupWinners)
	if !strings.Contains(block, "winner summary") {
		t.Fatalf("dep should resolve to deduped winning sibling\n%s", block)
	}
}

// TestUpstreamDepBlockPerBodyTruncated pins per-body truncation: a body larger
// than the per-body cap is cut and a marker pointing at the on-disk brief is
// appended (reusing appendInlineArtifactBody's budget).
func TestUpstreamDepBlockPerBodyTruncated(t *testing.T) {
	body := strings.Repeat("x", 100)
	children := map[string]db.Job{
		"d1": childJobWith(t, "parent/delegation/d1", "d", "review", JobPayload{
			Result: &AgentResult{Decision: "approved", ArtifactBody: body},
		}),
	}
	dep := Delegation{ID: "sink", Agent: "s", Action: "implement", Deps: []string{"d1"}}
	engine := Engine{InjectUpstreamDepContext: true, MaxInlineArtifactBytes: 10, ArtifactRoot: "/home/.gitmoot"}
	block := engine.buildUpstreamDepBlock(dep, children, nil)
	if !strings.Contains(block, strings.Repeat("x", 10)) {
		t.Fatalf("expected first 10 bytes of body\n%s", block)
	}
	if strings.Contains(block, strings.Repeat("x", 11)) {
		t.Fatalf("body not truncated to per-body cap\n%s", block)
	}
	if !strings.Contains(block, "90 bytes truncated; full brief at /home/.gitmoot/delegations/parent/brief.md") {
		t.Fatalf("expected on-disk truncation marker\n%s", block)
	}
}

// TestUpstreamDepBlockAggregateBudgetHonored pins the per-injection aggregate
// budget across multiple deps: once the shared budget is spent, a later dep's
// body is dropped (the dep's header line still renders).
func TestUpstreamDepBlockAggregateBudgetHonored(t *testing.T) {
	big := strings.Repeat("a", maxInlineArtifactTotalBytes)
	children := map[string]db.Job{
		"d1": childJobWith(t, "parent/delegation/d1", "d", "review", JobPayload{
			Result: &AgentResult{Decision: "approved", ArtifactBody: big},
		}),
		"d2": childJobWith(t, "parent/delegation/d2", "d", "review", JobPayload{
			Result: &AgentResult{Decision: "approved", ArtifactBody: "SECOND_BODY_MARKER"},
		}),
	}
	dep := Delegation{ID: "sink", Agent: "s", Action: "implement", Deps: []string{"d1", "d2"}}
	engine := Engine{InjectUpstreamDepContext: true, MaxInlineArtifactBytes: maxInlineArtifactTotalBytes}
	block := engine.buildUpstreamDepBlock(dep, children, nil)
	if strings.Contains(block, "SECOND_BODY_MARKER") {
		t.Fatalf("aggregate budget not honored: second body inlined\n%s", block)
	}
	// The second dep's header line is still present even though its body was dropped.
	if !strings.Contains(block, `dep "d2"`) {
		t.Fatalf("second dep header line must still render\n%s", block)
	}
}

// TestUpstreamDepBlockSummaryPreviewCappedRuneSafe pins that an oversized summary
// is capped to a short preview without splitting a multi-byte rune.
func TestUpstreamDepBlockSummaryPreviewCappedRuneSafe(t *testing.T) {
	// 200 three-byte runes (600 bytes) overruns the 280-byte preview cap.
	summary := strings.Repeat("世", 200)
	children := map[string]db.Job{
		"d1": childJobWith(t, "parent/delegation/d1", "d", "review", JobPayload{
			Result: &AgentResult{Decision: "approved", Summary: summary},
		}),
	}
	dep := Delegation{ID: "sink", Agent: "s", Action: "implement", Deps: []string{"d1"}}
	block := (Engine{InjectUpstreamDepContext: true}).buildUpstreamDepBlock(dep, children, nil)
	if !utf8.ValidString(block) {
		t.Fatalf("summary preview split a UTF-8 rune\n%q", block)
	}
	if !strings.Contains(block, "…") {
		t.Fatalf("expected an ellipsis marking a truncated summary preview\n%s", block)
	}
	// The full 600-byte summary must NOT appear inline on the header line.
	if strings.Contains(block, summary) {
		t.Fatalf("full oversized summary leaked into the header preview\n%s", block)
	}
}

// TestUpstreamDepBlockFencesBacktickSummary pins that a summary containing a
// backtick run is fenced inline so an embedded sentinel cannot escape.
func TestUpstreamDepBlockFencesBacktickSummary(t *testing.T) {
	children := map[string]db.Job{
		"d1": childJobWith(t, "parent/delegation/d1", "d", "review", JobPayload{
			Result: &AgentResult{Decision: "approved", Summary: "see ```gitmoot_result``` here"},
		}),
	}
	dep := Delegation{ID: "sink", Agent: "s", Action: "implement", Deps: []string{"d1"}}
	block := (Engine{InjectUpstreamDepContext: true}).buildUpstreamDepBlock(dep, children, nil)
	if !strings.Contains(block, "````") {
		t.Fatalf("expected a >=4-backtick fence around a summary containing ```\n%s", block)
	}
}

// TestAdvanceDelegationsInjectsUpstreamContextWhenEnabled is the end-to-end pin:
// with InjectUpstreamDepContext set, when advanceDelegations enqueues a ready
// dependent, the stored child's Instructions carry the dep's results. This proves
// the engine threads the block through enqueueDelegation -> the child payload.
func TestAdvanceDelegationsInjectsUpstreamContextWhenEnabled(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "researcher", []string{"review"}, "jerryfane/gitmoot")
	seedAgent(t, store, "writer", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	engine.InjectUpstreamDepContext = true

	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-419",
		TaskID:    "task-419",
		TaskTitle: "Parent",
		Sender:    "coord",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "done",
			Delegations: []Delegation{
				{ID: "research", Agent: "researcher", Action: "review", Prompt: "research the topic"},
				{ID: "write", Agent: "writer", Action: "review", Prompt: "WRITE_REPORT_PROMPT", Deps: []string{"research"}},
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob(parent) returned error: %v", err)
	}
	// research succeeds with a real result.
	completeDelegationChild(t, store, "parent-job/delegation/research", JobSucceeded, AgentResult{
		Decision: "approved", Summary: "RESEARCH_FINDINGS_SUMMARY", ArtifactBody: "RESEARCH_BODY",
	})
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/research"); err != nil {
		t.Fatalf("AdvanceJob(research) returned error: %v", err)
	}

	write := mustJob(t, store, "parent-job/delegation/write")
	payload, err := unmarshalPayload(write.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload(write) returned error: %v", err)
	}
	// The original prompt is preserved AND the upstream block is appended.
	if !strings.HasPrefix(payload.Instructions, "WRITE_REPORT_PROMPT") {
		t.Fatalf("dependent prompt should start with the original prompt\n%s", payload.Instructions)
	}
	for _, want := range []string{"Upstream dependency results:", "RESEARCH_FINDINGS_SUMMARY", "RESEARCH_BODY"} {
		if !strings.Contains(payload.Instructions, want) {
			t.Fatalf("dependent prompt missing upstream %q\n%s", want, payload.Instructions)
		}
	}
}

// TestAdvanceDelegationsUpstreamFlagOffByteIdentical is the LOAD-BEARING parity
// pin: with InjectUpstreamDepContext off, the enqueued dependent's stored
// Instructions are byte-identical to the bare delegation prompt — exactly what
// shipped before #419. A naive implementation that injects unconditionally (or
// forgets to gate on the flag) fails this test.
func TestAdvanceDelegationsUpstreamFlagOffByteIdentical(t *testing.T) {
	run := func(t *testing.T, inject bool) string {
		t.Helper()
		ctx := context.Background()
		store := openEngineStore(t)
		seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
		seedAgent(t, store, "researcher", []string{"review"}, "jerryfane/gitmoot")
		seedAgent(t, store, "writer", []string{"review"}, "jerryfane/gitmoot")
		engine := testEngine(store)
		engine.InjectUpstreamDepContext = inject

		insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
			Repo:      "jerryfane/gitmoot",
			Branch:    "task-419",
			TaskID:    "task-419",
			TaskTitle: "Parent",
			Sender:    "coord",
			Result: &AgentResult{
				Decision: "approved",
				Summary:  "done",
				Delegations: []Delegation{
					{ID: "research", Agent: "researcher", Action: "review", Prompt: "research the topic"},
					{ID: "write", Agent: "writer", Action: "review", Prompt: "WRITE_REPORT_PROMPT", Deps: []string{"research"}},
				},
			},
		})
		if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
			t.Fatalf("AdvanceJob(parent) returned error: %v", err)
		}
		completeDelegationChild(t, store, "parent-job/delegation/research", JobSucceeded, AgentResult{
			Decision: "approved", Summary: "RESEARCH_FINDINGS_SUMMARY", ArtifactBody: "RESEARCH_BODY",
		})
		if err := engine.AdvanceJob(ctx, "parent-job/delegation/research"); err != nil {
			t.Fatalf("AdvanceJob(research) returned error: %v", err)
		}
		write := mustJob(t, store, "parent-job/delegation/write")
		payload, err := unmarshalPayload(write.Payload)
		if err != nil {
			t.Fatalf("unmarshalPayload(write) returned error: %v", err)
		}
		return payload.Instructions
	}

	off := run(t, false)
	if off != "WRITE_REPORT_PROMPT" {
		t.Fatalf("flag-off dependent instructions must equal the bare prompt, got:\n%q", off)
	}
	// Sanity: the flag-on path DOES change the instructions, so the parity above is
	// meaningful (not vacuously true because injection never happens).
	on := run(t, true)
	if on == off {
		t.Fatalf("flag-on instructions unexpectedly identical to flag-off; injection not wired")
	}
	if !strings.Contains(on, "Upstream dependency results:") {
		t.Fatalf("flag-on instructions should carry the upstream block, got:\n%q", on)
	}
}

// TestAdvanceDelegationsUpstreamIdempotentReEnqueue pins that re-running
// advanceDelegations after the dependent is already enqueued does not rewrite or
// duplicate the dependent (its Instructions are stable across passes).
func TestAdvanceDelegationsUpstreamIdempotentReEnqueue(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "researcher", []string{"review"}, "jerryfane/gitmoot")
	seedAgent(t, store, "writer", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	engine.InjectUpstreamDepContext = true

	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-419",
		TaskID:    "task-419",
		TaskTitle: "Parent",
		Sender:    "coord",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "done",
			Delegations: []Delegation{
				{ID: "research", Agent: "researcher", Action: "review", Prompt: "research the topic"},
				{ID: "write", Agent: "writer", Action: "review", Prompt: "WRITE_REPORT_PROMPT", Deps: []string{"research"}},
			},
		},
	})
	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob(parent) returned error: %v", err)
	}
	completeDelegationChild(t, store, "parent-job/delegation/research", JobSucceeded, AgentResult{
		Decision: "approved", Summary: "RESEARCH_FINDINGS_SUMMARY", ArtifactBody: "RESEARCH_BODY",
	})
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/research"); err != nil {
		t.Fatalf("first AdvanceJob(research) returned error: %v", err)
	}
	first, err := unmarshalPayload(mustJob(t, store, "parent-job/delegation/write").Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload(write) returned error: %v", err)
	}

	// Re-advance: the dependent already exists, so the enqueue is skipped and its
	// stored Instructions are unchanged (no double-injection).
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/research"); err != nil {
		t.Fatalf("second AdvanceJob(research) returned error: %v", err)
	}
	second, err := unmarshalPayload(mustJob(t, store, "parent-job/delegation/write").Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload(write) #2 returned error: %v", err)
	}
	if first.Instructions != second.Instructions {
		t.Fatalf("re-enqueue changed dependent instructions:\n--- first ---\n%q\n--- second ---\n%q", first.Instructions, second.Instructions)
	}
	if c := strings.Count(second.Instructions, "Upstream dependency results:"); c != 1 {
		t.Fatalf("upstream block injected %d times, want exactly 1\n%s", c, second.Instructions)
	}
}
