package workflow

import (
	"context"
	"sort"
	"strings"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/runtime"
)

// ReviewLegDispatcher is the injected, best-effort, nil-by-default seam (#469)
// the engine calls after a merge to run a CROSS-FAMILY review leg. It owns
// reviewer selection (registered different-family agent, else an ephemeral
// different-family leg, else — REFINEMENT #1 — a SAME-family fallback with a
// warning), the read-only review-leg dispatch (reusing the runtime adapter +
// EphemeralSpec, like the #421 verifier.md verify-leg), and the rubric parse. It
// returns the OutcomeReviewed the engine then harvests into the auto-trace run.
//
// It NEVER blocks the merge path: the engine calls it best-effort off the
// blocking path and swallows its error. ok=false means NO review-capable runtime
// is authed at all (skip, no review row); ok=true returns the soft outcome
// (cross-family OR same-family-with-warning, distinguished by Outcome.SelfFamily).
type ReviewLegDispatcher interface {
	Review(ctx context.Context, implementJob db.Job, implementPayload JobPayload, mergedHead string) (Outcome, bool, error)
}

// crossFamilyRotation is the fixed, deterministic family rotation used when no
// registered different-family reviewer exists and the dispatcher materializes an
// ephemeral different-family review leg (#469): codex→claude, claude→codex,
// kimi→claude. Every target is provably a DIFFERENT family than the key, so the
// ephemeral fallback is always cross-family.
var crossFamilyRotation = map[string]string{
	runtime.CodexRuntime:  runtime.ClaudeRuntime,
	runtime.ClaudeRuntime: runtime.CodexRuntime,
	runtime.KimiRuntime:   runtime.ClaudeRuntime,
}

// reviewCapability is the capability a registered agent must declare to be picked
// as a cross-family reviewer.
const reviewCapability = "review"

// CrossFamilyReviewer is the resolved reviewer the dispatcher will run: either a
// pre-registered agent (RegisteredAgent set) or an ephemeral worker spec
// (Ephemeral set). Runtime is the resolved reviewer runtime family (always one of
// codex/claude/kimi). SelfFamily is true ONLY when no different family was
// available and selection fell back to a SAME-family reviewer (REFINEMENT #1) —
// the engine/harvester tag that row distinctly so it weights below a cross-family
// review.
type CrossFamilyReviewer struct {
	Runtime         string
	RegisteredAgent string
	Ephemeral       *EphemeralSpec
	SelfFamily      bool
}

// AgentLister is the minimal read the cross-family selector needs over the store
// (*db.Store satisfies it). It is its own narrow interface so PickCrossFamilyReviewer
// is unit-testable with a stub and so the cli dispatcher can pass the real store.
type AgentLister interface {
	ListAgents(ctx context.Context) ([]db.Agent, error)
}

// PickCrossFamilyReviewer is the exported entry point the cli dispatcher calls;
// see pickCrossFamilyReviewer for the full selection contract.
func PickCrossFamilyReviewer(ctx context.Context, store AgentLister, implementerRuntime string, repo string, authedRuntimes map[string]bool) (CrossFamilyReviewer, bool, error) {
	return pickCrossFamilyReviewer(ctx, store, implementerRuntime, repo, authedRuntimes)
}

// pickCrossFamilyReviewer selects the reviewer for an implement job whose runtime
// family is implementerRuntime, scoped to repo (#469 + REFINEMENT #1).
//
// Preference order:
//  1. A registered, review-capable agent of a DIFFERENT runtime family whose repo
//     scope covers repo — picked deterministically by name (cross-family).
//  2. An EPHEMERAL different-family review leg via the fixed crossFamilyRotation
//     (codex→claude, claude→codex, kimi→claude), read-only, verifier.md-style —
//     but ONLY when that target family is among the runtimes that are actually
//     authed/available (authedRuntimes), so the leg can really run (cross-family).
//  3. REFINEMENT #1: when NO different family is available at all, FALL BACK to a
//     SAME-family reviewer WITH A WARNING — prefer a registered same-family
//     review-capable agent, else an ephemeral same-family leg — and mark it
//     SelfFamily so it weights BELOW a cross-family review. The caller emits the
//     warning event/log; selection never silently returns same-family.
//
// ok=false is returned ONLY when NO review-capable runtime is authed at all (no
// registered reviewer and no authed runtime to run an ephemeral leg) — the caller
// then skips the review entirely (no review row). implementerRuntime that is not
// a known family (e.g. an unrecoverable/migrated agent) yields ok=false too, so a
// possibly-same-family review is never emitted by accident.
func pickCrossFamilyReviewer(ctx context.Context, store AgentLister, implementerRuntime string, repo string, authedRuntimes map[string]bool) (CrossFamilyReviewer, bool, error) {
	implementerRuntime = strings.TrimSpace(implementerRuntime)
	if _, known := crossFamilyRotation[implementerRuntime]; !known {
		// Unknown/unrecoverable implementer family: skip rather than guess and risk a
		// silent same-family review (#469 risk note: SKIP-not-guess).
		return CrossFamilyReviewer{}, false, nil
	}

	agents, err := listReviewAgents(ctx, store, repo)
	if err != nil {
		return CrossFamilyReviewer{}, false, err
	}

	// 1. A registered DIFFERENT-family review-capable agent (deterministic by name).
	for _, agent := range agents {
		if !strings.EqualFold(agent.Runtime, implementerRuntime) {
			return CrossFamilyReviewer{
				Runtime:         strings.TrimSpace(agent.Runtime),
				RegisteredAgent: agent.Name,
			}, true, nil
		}
	}

	// 2. An ephemeral DIFFERENT-family leg, but only if that family is authed.
	if target := crossFamilyRotation[implementerRuntime]; authedRuntimes[target] {
		return CrossFamilyReviewer{
			Runtime:   target,
			Ephemeral: ephemeralReviewerSpec(target),
		}, true, nil
	}

	// 3. REFINEMENT #1: same-family fallback WITH WARNING (caller emits it).
	//    Prefer a registered same-family review-capable agent, else an ephemeral
	//    same-family leg — but only if the implementer's own family is authed.
	for _, agent := range agents {
		if strings.EqualFold(agent.Runtime, implementerRuntime) {
			return CrossFamilyReviewer{
				Runtime:         strings.TrimSpace(agent.Runtime),
				RegisteredAgent: agent.Name,
				SelfFamily:      true,
			}, true, nil
		}
	}
	if authedRuntimes[implementerRuntime] {
		return CrossFamilyReviewer{
			Runtime:    implementerRuntime,
			Ephemeral:  ephemeralReviewerSpec(implementerRuntime),
			SelfFamily: true,
		}, true, nil
	}

	// No review-capable runtime authed at all.
	return CrossFamilyReviewer{}, false, nil
}

// listReviewAgents returns the registered review-capable agents whose repo scope
// covers repo, sorted deterministically by name (ListAgents already orders by
// name; this re-sorts defensively so the first-match selection is stable
// regardless of the store's ordering).
func listReviewAgents(ctx context.Context, store AgentLister, repo string) ([]db.Agent, error) {
	if store == nil {
		return nil, nil
	}
	all, err := store.ListAgents(ctx)
	if err != nil {
		return nil, err
	}
	repo = strings.TrimSpace(repo)
	var out []db.Agent
	for _, agent := range all {
		if !contains(agent.Capabilities, reviewCapability) {
			continue
		}
		if !agentRepoScopeCovers(agent.RepoScope, repo) {
			continue
		}
		out = append(out, agent)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// agentRepoScopeCovers reports whether a registered agent's repo scope covers repo.
// An empty scope is treated as "all repos" (the registry convention used by
// AgentCanAccessRepo for a global agent); otherwise the scope must equal the repo.
func agentRepoScopeCovers(scope string, repo string) bool {
	scope = strings.TrimSpace(scope)
	repo = strings.TrimSpace(repo)
	if scope == "" {
		return true
	}
	return strings.EqualFold(scope, repo)
}

// ephemeralReviewerSpec builds the read-only ephemeral review-leg spec for the
// given runtime, mirroring the verifier.md cross-model verify-leg (#421): a
// reviewer role with ask+review capabilities and a read-only autonomy policy so
// the leg can read the diff but never write.
func ephemeralReviewerSpec(rt string) *EphemeralSpec {
	return &EphemeralSpec{
		Runtime:        rt,
		Role:           "reviewer",
		Capabilities:   []string{"ask", reviewCapability},
		AutonomyPolicy: runtime.AutonomyPolicyReadOnly,
	}
}

// ReviewLegPrompt assembles the read-only cross-family reviewer prompt from the
// IMPLEMENT job's intended scope (Instructions + TaskTitle + resolved Goal.Title)
// vs the delivered work (the PR diff + AgentResult.ChangesMade as a secondary
// cross-check), per scope_fidelity_inputs (#469). The rubric instructions ask the
// reviewer to score coverage/containment/fidelity (scope) + architecture/
// readability/abstraction (subjective quality), each in [0,1], and return them in
// its gitmoot_result. The exact rubric text is built ONLY here at review time and
// is NEVER injected into the implementer's prompt (anti-gaming guardrail).
//
// goalTitle is the resolved Goal.Title (empty when the job carries no goal); diff
// is the read-only PR diff text (empty when the diff read failed — the reviewer
// then leans on ChangesMade only, a graceful degrade).
func ReviewLegPrompt(payload JobPayload, goalTitle string, diff string) string {
	var b strings.Builder
	b.WriteString("You are a cross-family code reviewer scoring a MERGED pull request on subjective quality AND scope-fidelity. ")
	b.WriteString("This is a SOFT, advisory signal — your score never blocks or reverts the merge.\n\n")

	b.WriteString("## Intended scope (what was asked)\n")
	if t := strings.TrimSpace(payload.TaskTitle); t != "" {
		b.WriteString("Task: " + t + "\n")
	}
	if g := strings.TrimSpace(goalTitle); g != "" {
		b.WriteString("Goal: " + g + "\n")
	}
	if instr := strings.TrimSpace(payload.Instructions); instr != "" {
		b.WriteString("Instructions:\n" + instr + "\n")
	}

	b.WriteString("\n## Delivered work (what was done)\n")
	if d := strings.TrimSpace(diff); d != "" {
		b.WriteString("PR diff:\n" + d + "\n")
	} else {
		b.WriteString("(PR diff unavailable; rely on the self-reported changes below.)\n")
	}
	if payload.Result != nil && len(payload.Result.ChangesMade) > 0 {
		b.WriteString("Self-reported changes (secondary cross-check):\n")
		for _, change := range payload.Result.ChangesMade {
			if c := strings.TrimSpace(change); c != "" {
				b.WriteString("- " + c + "\n")
			}
		}
	}

	b.WriteString("\n## Rubric\n")
	b.WriteString("Score EACH dimension in [0,1] (1 = excellent, 0 = poor):\n")
	b.WriteString("- coverage: did the change do ALL of the ask?\n")
	b.WriteString("- containment: no creep beyond the ask (no unrelated/over-broad changes)?\n")
	b.WriteString("- fidelity: did it do THE ask, not something adjacent or different?\n")
	b.WriteString("- architecture: does the design fit the codebase (no over-engineering)?\n")
	b.WriteString("- readability: is the code clear and maintainable?\n")
	b.WriteString("- abstraction: are abstractions appropriate (not duplicative, not premature)?\n\n")

	b.WriteString("Return ONLY a gitmoot_result whose `metadata.rubric` is an object mapping each dimension above to its [0,1] score, ")
	b.WriteString("with `summary`/`findings` carrying your reasoning. Do NOT modify any files (read-only).\n")
	return b.String()
}

// ReviewRubricResult is the parsed reviewer rubric: the [0,1] dimension scores and
// the free-text findings the dispatcher maps onto an Outcome{Kind:OutcomeReviewed}.
type ReviewRubricResult struct {
	Rubric   map[string]float64
	Findings string
}

// reviewRubricDimensions is the fixed set of rubric dimensions the reviewer is
// asked to score; the parser keeps only these keys (clamped to [0,1]) so a
// reviewer that hallucinates extra keys cannot skew the mean.
var reviewRubricDimensions = []string{
	"coverage", "containment", "fidelity", "architecture", "readability", "abstraction",
}

// ParseReviewRubric extracts the rubric dimension scores + findings from a
// reviewer's AgentResult (#469). The reviewer is asked to return the rubric under
// metadata.rubric; this also tolerates a top-level rubric in the raw output. Only
// the known dimensions are kept (each clamped to [0,1]); unknown keys are dropped.
// An empty/absent rubric yields an empty map so the harvester writes HasScore=false
// (no fabricated neutral 0.5) rather than a bogus score.
func ParseReviewRubric(result AgentResult, rawRubric map[string]float64) ReviewRubricResult {
	rubric := map[string]float64{}
	for _, dim := range reviewRubricDimensions {
		if v, ok := rawRubric[dim]; ok {
			rubric[dim] = clampUnit01(v)
		}
	}
	findings := strings.TrimSpace(result.Summary)
	if findings == "" {
		findings = "cross-family review"
	}
	return ReviewRubricResult{Rubric: rubric, Findings: findings}
}

// clampUnit01 clamps a rubric score to [0,1] so a reviewer that returns an
// out-of-range value cannot push the projected mean outside the contract range.
func clampUnit01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
