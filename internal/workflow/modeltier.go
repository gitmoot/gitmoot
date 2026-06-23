package workflow

import "strings"

// Model-tier complexity scoring (issue #379).
//
// This file is a PURE, deterministic, side-effect-free primitive. It scores a
// delegation's likely difficulty and maps that score to an abstract model Tier
// so a future coordinator-policy step (or a CLI command, or a test) can decide
// whether to downshift the per-delegation `model` to a cheaper runtime model,
// leave it empty for the runtime default, or reserve a deep model for genuinely
// hard work.
//
// CRITICAL: nothing in the engine, dispatcher, or scheduler calls into this
// file. It is intentionally NOT wired into job dispatch — invoking it has zero
// behavioral effect on how delegations run. Tiers are deliberately abstract:
// there is no tier -> concrete vendor-model (codex/claude/kimi) map here, and
// the `standard` tier maps to an empty `model` (the runtime default) by design.
//
// ALL of the weights, thresholds, and keyword lists below are HEURISTIC and
// UNCALIBRATED. They encode a rough intuition ("longer + harder keywords +
// multi-file implement == more complex"), not a measured model. Treat the exact
// numbers as a starting point to be tuned, not as ground truth.

// Tier is an abstract difficulty band for a delegation. It is intentionally not
// bound to any concrete vendor model; mapping a Tier to a runtime model string
// is a separate, deferred product decision.
type Tier string

const (
	// TierMechanical is reserved for trivial, rote, deterministic edits (e.g. a
	// rename or a codemod-style change). It is selected ONLY by an explicit intent
	// match, never from the numeric complexity score.
	TierMechanical Tier = "mechanical"
	// TierCheap is for low-complexity work that a small/fast model can handle.
	TierCheap Tier = "cheap"
	// TierStandard is the default band; it maps to an empty `model` (runtime
	// default) rather than to a specific model.
	TierStandard Tier = "standard"
	// TierDeep is for genuinely hard or quorum-critical work that warrants a
	// strong, expensive model.
	TierDeep Tier = "deep"
)

// Heuristic scoring weights and thresholds. Uncalibrated — see the file-level
// doc comment. Each contribution is clamped so the total stays within [0, 1].
const (
	// promptLenFullScoreChars is the prompt length (in characters) at which the
	// length signal reaches its full weight. Shorter prompts scale linearly below
	// it.
	promptLenFullScoreChars = 1200.0
	// weightPromptLen is the maximum contribution of the prompt-length signal.
	weightPromptLen = 0.30
	// weightHardKeyword is the contribution of the FIRST matched hard keyword.
	weightHardKeyword = 0.35
	// weightHardKeywordEach is the additional contribution of each further hard
	// keyword beyond the first, so several hard keywords push harder than one.
	weightHardKeywordEach = 0.10
	// maxHardKeywordScore caps the total hard-keyword contribution.
	maxHardKeywordScore = 0.55
	// weightImplementAction is the contribution of an implement-style action; an
	// `ask` contributes nothing here (an ask is cheaper than a multi-file
	// implement).
	weightImplementAction = 0.20
	// weightReviewAction is the (smaller) contribution of a review-style action.
	weightReviewAction = 0.10
	// weightScopeSignal is the contribution of the scope signal (a delegation
	// that fans across many artifacts or carries a worktree is wider in scope).
	weightScopeSignal = 0.15
	// scopeArtifactThreshold is the artifact count at or above which the scope
	// signal fires on artifact breadth alone.
	scopeArtifactThreshold = 2
	// weightQuorumCritical is the contribution of a quorum-critical leg; quorum
	// legs gate the coordinator, so they lean toward a deeper model.
	weightQuorumCritical = 0.20
)

// Tier thresholds over the complexity score. Fixed by design (issue #379):
// `<0.3 -> cheap`, `<0.6 -> standard`, `>=0.6 -> deep`. `mechanical` is never
// produced from the score — only from an explicit intent match.
const (
	tierCheapBelow    = 0.30
	tierStandardBelow = 0.60
)

// hardKeywords are tokens whose presence in a delegation's prompt strongly
// suggests genuinely hard work (architecture, security, data-shape, or
// concurrency reasoning). Heuristic and non-exhaustive. Matched
// case-insensitively as substrings.
var hardKeywords = []string{
	"architecture",
	"oauth",
	"schema",
	"concurrency",
	"migration",
	"security",
	"refactor",
	"distributed",
	"consensus",
	"cryptograph",
	"race condition",
	"deadlock",
	"transaction",
	"performance",
}

// mechanicalKeywords are explicit intent tokens that mark a delegation as rote,
// deterministic, mechanical work. A match selects TierMechanical directly,
// independent of the numeric complexity score. Heuristic and non-exhaustive.
var mechanicalKeywords = []string{
	"rename",
	"codemod",
	"find and replace",
	"find-and-replace",
	"bump version",
	"format the",
	"run gofmt",
	"fix typo",
	"fix the typo",
	"regenerate",
}

// implementActions are the action verbs that imply writing code (and so tend to
// be more complex than a read-only ask/review). Matched against the
// delegation's Action, case-insensitively.
var implementActions = map[string]struct{}{
	"implement": {},
	"fix":       {},
	"build":     {},
	"write":     {},
	"refactor":  {},
	"migrate":   {},
}

// reviewActions are read-style action verbs that carry a small complexity
// contribution (more than a bare ask, less than an implement).
var reviewActions = map[string]struct{}{
	"review": {},
	"audit":  {},
	"verify": {},
}

// ScoreComplexity returns a deterministic complexity estimate in [0, 1] for a
// delegation, combining a prompt-length signal, a hard-keyword signal, the
// action type (an ask is cheaper than a multi-file implement), a scope signal
// (artifact breadth / worktree), and whether the leg is quorum-critical.
//
// It is pure and side-effect-free; it reads only the passed delegation and is
// not called by the engine. All weights are heuristic — see the file-level doc
// comment.
func ScoreComplexity(d Delegation) float64 {
	var score float64

	// Length signal: longer prompts tend to describe more involved work. Scales
	// linearly up to promptLenFullScoreChars, then saturates at weightPromptLen.
	promptLen := float64(len(strings.TrimSpace(d.Prompt)))
	lenFraction := promptLen / promptLenFullScoreChars
	if lenFraction > 1 {
		lenFraction = 1
	}
	score += lenFraction * weightPromptLen

	// Hard-keyword signal: the first match contributes weightHardKeyword, each
	// further distinct match adds weightHardKeywordEach, capped at
	// maxHardKeywordScore.
	if matches := countHardKeywords(d.Prompt); matches > 0 {
		kw := weightHardKeyword + float64(matches-1)*weightHardKeywordEach
		if kw > maxHardKeywordScore {
			kw = maxHardKeywordScore
		}
		score += kw
	}

	// Action signal: implement-style actions imply writing (multi-file) code and
	// score highest; review-style actions score a little; a bare ask scores zero.
	action := strings.ToLower(strings.TrimSpace(d.Action))
	if _, ok := implementActions[action]; ok {
		score += weightImplementAction
	} else if _, ok := reviewActions[action]; ok {
		score += weightReviewAction
	}

	// Scope signal: a delegation that fans across multiple artifacts or carries a
	// dedicated worktree is wider in scope than a single-target one.
	if len(d.Artifacts) >= scopeArtifactThreshold || strings.TrimSpace(d.Worktree) != "" {
		score += weightScopeSignal
	}

	// Quorum-critical legs gate the coordinator continuation, so bias them upward.
	if d.Quorum > 0 {
		score += weightQuorumCritical
	}

	if score > 1 {
		score = 1
	}
	if score < 0 {
		score = 0
	}
	return score
}

// countHardKeywords returns the number of distinct hard keywords present in the
// prompt (case-insensitive substring match).
func countHardKeywords(prompt string) int {
	lower := strings.ToLower(prompt)
	count := 0
	for _, kw := range hardKeywords {
		if strings.Contains(lower, kw) {
			count++
		}
	}
	return count
}

// TierFor maps a complexity score in [0, 1] to an abstract Tier using the fixed
// thresholds from issue #379: `<0.3 -> cheap`, `<0.6 -> standard`,
// `>=0.6 -> deep`. It never returns TierMechanical — that band is reachable only
// through TierForDelegation's explicit intent match, not from the score.
func TierFor(score float64) Tier {
	switch {
	case score < tierCheapBelow:
		return TierCheap
	case score < tierStandardBelow:
		return TierStandard
	default:
		return TierDeep
	}
}

// TierForDelegation is the convenience entry point: it returns TierMechanical
// when the delegation's prompt carries an explicit mechanical-intent keyword,
// and otherwise falls back to TierFor(ScoreComplexity(d)). Like everything in
// this file, it is a pure helper and is not invoked by the engine.
func TierForDelegation(d Delegation) Tier {
	if hasMechanicalIntent(d.Prompt) {
		return TierMechanical
	}
	return TierFor(ScoreComplexity(d))
}

// hasMechanicalIntent reports whether the prompt explicitly signals rote,
// mechanical work via a mechanicalKeywords match (case-insensitive substring).
func hasMechanicalIntent(prompt string) bool {
	lower := strings.ToLower(prompt)
	for _, kw := range mechanicalKeywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}
