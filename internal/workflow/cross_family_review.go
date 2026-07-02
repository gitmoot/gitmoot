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

// DeterministicCheckerDispatcher is the injected, best-effort, nil-by-default seam
// (#485) the engine calls after a merge to run an OBJECTIVE, non-LLM
// deterministic-checker leg ALONGSIDE the subjective cross-family review leg. It
// runs plain external TOOLS (code duplication, lint, cyclomatic complexity) plus a
// pure-Go diff-size metric, normalizes each to a [0,1] dimension, and returns an
// Outcome{Kind: OutcomeReviewed, Objective: true, Rubric: <tool dims>} the engine
// harvests into the SAME auto-trace run as a THIRD coexisting signal — distinct
// from the LLM rubric via the Objective tag.
//
// It mirrors ReviewLegDispatcher EXACTLY: nil-safe (when nil — the default, no
// deterministic_checkers_enabled — NO checker leg runs and NO checker row is
// written, so behavior is byte-identical), DETACHED off the blocking AdvanceJob
// path (a wedged tool can never stall job advancement), and best-effort (a dispatch
// error is swallowed and recorded as a deterministic_checkers_failed job event,
// never returned up). ok=false means NO dimension was producible at all (every
// checker skipped) so the engine writes no checker row; ok=true returns the
// objective outcome with whatever dimensions survived. The concrete impl is wired
// only in cli (gated by deterministic_checkers_enabled AND auto_trace_enabled),
// keeping the engine free of subprocess/skillopt coupling.
type DeterministicCheckerDispatcher interface {
	Check(ctx context.Context, implementJob db.Job, implementPayload JobPayload, mergedHead string) (Outcome, bool, error)
}

// HardVerifierDispatcher is the injected, best-effort, nil-by-default seam (#474)
// the engine calls after a merge to run the deterministic HARD-verifier tier: the
// operator's configured build/test/lint COMMANDS run in a FRESH, clean sandbox
// checkout at the merged head — exit 0 == pass. It returns an
// Outcome{Kind: OutcomeReviewed, HardVerifier: true, HardPassed: <all-passed>} the
// engine harvests into the SAME auto-trace run, mapping the binary verdict onto the
// authoritative EvaluatorScore.Hard (1.0 pass / 0.0 fail) — an un-gameable gate the
// LLM judge's prose can never move.
//
// It mirrors DeterministicCheckerDispatcher EXACTLY: nil-safe (when nil — the
// default, no hard_verifiers_enabled — NO verifier leg runs and NO hard row is
// written, byte-identical), DETACHED off the blocking AdvanceJob path (a slow test
// suite can never stall job advancement or the daemon checkoutLock), and
// best-effort (a dispatch error is swallowed and recorded as a hard_verifiers_failed
// job event, never returned up). ok=false means NO verdict was producible at all
// (the sandbox could not be provisioned, or the command list was empty) so the
// engine writes no hard row; ok=true returns the binary verdict. The concrete impl
// is wired only in cli (gated by hard_verifiers_enabled AND auto_trace_enabled AND a
// non-empty command list), keeping the engine free of subprocess/skillopt/git
// coupling.
type HardVerifierDispatcher interface {
	Verify(ctx context.Context, implementJob db.Job, implementPayload JobPayload, mergedHead string) (Outcome, bool, error)
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

// reviewerFamily collapses a runtime to its model FAMILY for cross-family
// comparisons. The opt-in legacy kimi-cli runtime (#546) is the SAME model family
// as kimi, so it must normalize to kimi here: otherwise a kimi-cli IMPLEMENTER
// would miss the rotation lookup and silently skip cross-family review/jury, and a
// kimi reviewer would be wrongly treated as a DIFFERENT family from a kimi-cli
// implementer. All other runtimes are their own family.
func reviewerFamily(runtimeName string) string {
	r := strings.TrimSpace(runtimeName)
	if strings.EqualFold(r, runtime.KimiCLIRuntime) {
		return runtime.KimiRuntime
	}
	return r
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
//
// AgentCanAccessRepo is the AUTHORITATIVE repo-access check (the agent_repos /
// agent_instances join the rest of the engine uses at preflightDelegation /
// availableAgentsForRepo), NOT the single agents.repo_scope string — so a reviewer
// granted multi-repo access via agent_repos is correctly included and an
// empty-scope agent is not silently treated as global. Its signature matches
// *db.Store.AgentCanAccessRepo so the real store satisfies this interface directly.
type AgentLister interface {
	ListAgents(ctx context.Context) ([]db.Agent, error)
	AgentCanAccessRepo(ctx context.Context, agentName string, repoFullName string) (bool, error)
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
	family := reviewerFamily(implementerRuntime)
	if _, known := crossFamilyRotation[family]; !known {
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
		if !strings.EqualFold(reviewerFamily(agent.Runtime), family) {
			return CrossFamilyReviewer{
				Runtime:         strings.TrimSpace(agent.Runtime),
				RegisteredAgent: agent.Name,
			}, true, nil
		}
	}

	// 2. An ephemeral DIFFERENT-family leg, but only if that family is authed.
	if target := crossFamilyRotation[family]; authedRuntimes[target] {
		return CrossFamilyReviewer{
			Runtime:   target,
			Ephemeral: ephemeralReviewerSpec(target),
		}, true, nil
	}

	// 3. REFINEMENT #1: same-family fallback WITH WARNING (caller emits it).
	//    Prefer a registered same-family review-capable agent, else an ephemeral
	//    same-family leg — but only if the implementer's own family is authed.
	for _, agent := range agents {
		if strings.EqualFold(reviewerFamily(agent.Runtime), family) {
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

// crossFamilyJuryUniverse is the fixed, deterministic family universe the
// N-diverse-family jury picker iterates (#349): the three known runtime families
// in a stable order. PickCrossFamilyJury draws DISTINCT families from this list
// (skipping the implementer's own family), so the resulting jury is cross-family
// AND family-deduped by construction — diversity over headcount, never two
// near-identical judges of the same family.
var crossFamilyJuryUniverse = []string{
	runtime.ClaudeRuntime,
	runtime.CodexRuntime,
	runtime.KimiRuntime,
}

// PickCrossFamilyJury selects up to `size` cross-family reviewers from DISTINCT
// model families for the judge-jury (#349), generalizing PickCrossFamilyReviewer
// from one reviewer to N diverse ones. It is the diversity-first picker the issue
// mandates: every returned reviewer is a DIFFERENT family from the implementer
// AND from every other returned reviewer (deduped by family), because correlated
// same-family judges undermine a panel — so it NEVER pads with a near-identical
// family to reach `size`.
//
// Selection per candidate family (iterating crossFamilyJuryUniverse minus the
// implementer's family, deterministically):
//  1. a registered, review-capable agent of that family whose repo scope covers
//     repo (deterministic by name), else
//  2. an EPHEMERAL read-only review leg on that family — but ONLY when that family
//     is actually authed/available (so the leg can really run).
//
// A family with neither is skipped. The result is the available distinct families
// (0..size). GRACEFUL DEGRADATION: when fewer than `size` distinct families are
// available it returns as many as exist; the CALLER falls back to the single
// PickCrossFamilyReviewer path when fewer than 2 are returned (a jury of one is
// just the single judge). An unknown/unrecoverable implementer family yields nil
// (skip — never a possibly-same-family jury), and size < 2 yields nil (the jury
// is off; the caller takes the single-judge path) so the off-by-default behavior
// is byte-identical to today's single cross-family judge.
func PickCrossFamilyJury(ctx context.Context, store AgentLister, implementerRuntime string, repo string, authedRuntimes map[string]bool, size int) ([]CrossFamilyReviewer, error) {
	implementerRuntime = strings.TrimSpace(implementerRuntime)
	implementerFamily := reviewerFamily(implementerRuntime)
	if _, known := crossFamilyRotation[implementerFamily]; !known {
		// Unknown implementer family: skip rather than risk a same-family jury.
		return nil, nil
	}
	if size < 2 {
		// A jury needs at least two distinct families; below that the caller uses the
		// existing single cross-family judge path (byte-identical off-by-default).
		return nil, nil
	}

	agents, err := listReviewAgents(ctx, store, repo)
	if err != nil {
		return nil, err
	}

	var jury []CrossFamilyReviewer
	for _, family := range crossFamilyJuryUniverse {
		if strings.EqualFold(family, implementerFamily) {
			// Cross-family ONLY: never include the agent's own family (preference-leakage
			// guard), which also keeps the panel's families distinct.
			continue
		}

		// 1. A registered DIFFERENT-family review-capable agent of this family.
		var picked *CrossFamilyReviewer
		for _, agent := range agents {
			if strings.EqualFold(agent.Runtime, family) {
				picked = &CrossFamilyReviewer{
					Runtime:         family,
					RegisteredAgent: agent.Name,
				}
				break
			}
		}
		// 2. Else an ephemeral DIFFERENT-family leg, but only if that family is authed.
		if picked == nil && authedRuntimes[family] {
			picked = &CrossFamilyReviewer{
				Runtime:   family,
				Ephemeral: ephemeralReviewerSpec(family),
			}
		}
		if picked == nil {
			continue
		}
		jury = append(jury, *picked)
		if len(jury) >= size {
			break
		}
	}
	return jury, nil
}

// listReviewAgents returns the registered review-capable agents that can access
// repo, sorted deterministically by name (ListAgents already orders by name; this
// re-sorts defensively so the first-match selection is stable regardless of the
// store's ordering).
//
// Repo access is resolved through the AUTHORITATIVE AgentCanAccessRepo seam (the
// agent_repos / agent_instances join the rest of the engine uses), NOT the single
// agents.repo_scope string — so a reviewer granted access to repo via an
// agent_repos row is correctly included even when its repo_scope column names a
// different repo, and an empty-scope agent the convention would deny is not
// silently treated as global. A per-agent access error fails soft (the agent is
// dropped from the candidate set) rather than failing the whole selection.
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
		allowed, err := store.AgentCanAccessRepo(ctx, agent.Name, repo)
		if err != nil || !allowed {
			continue
		}
		out = append(out, agent)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
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
