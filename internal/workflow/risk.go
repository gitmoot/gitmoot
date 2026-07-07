package workflow

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// Risk-tiered adaptive review (#650). This file is the OPT-IN classifier + lens
// vocabulary that scales review depth to the blast radius of a change. It is
// fully additive: with [review].risk_tiers_enabled off (the default) the engine
// never calls ClassifyRisk, so HandlePullRequestOpened's single-review fan-out is
// byte-identical. When enabled, a PR classified `high` fans out a delegation
// batch of refutation-framed lens reviewers whose outcomes are synthesized by the
// EXISTING delegation synthesis_rule engine (quorum) rather than any bespoke
// synthesis. Competition (two implementations + a judge) is intentionally OUT OF
// SCOPE here and tracked as a follow-up.

// RiskTier is the resolved risk level of a change. Only two tiers are wired in
// this slice: `routine` (the default, single-review path) and `high` (the
// adversarial multi-lens path). The intermediate/competition tiers from the
// ultraswarm eval are deliberately not modeled yet.
const (
	RiskTierRoutine = "routine"
	RiskTierHigh    = "high"
)

// DefaultHighRiskPaths is the built-in glob list a change is matched against when
// [review].high_risk_paths is not configured. These are the blast-radius-heavy
// areas (auth, security, payments, DB migrations, and the module manifest) that
// warrant adversarial review. `**` matches any number of path segments; `*`
// matches within a single segment.
var DefaultHighRiskPaths = []string{
	"**/auth/**",
	"**/security/**",
	"**/payment/**",
	"**/migration/**",
	"go.mod",
}

// Risk-signal label defaults. An explicit PR label wins over path heuristics so a
// human can force a tier either way (escalate a subtle change, or de-escalate a
// noisy path match). These are the label NAMES matched case-insensitively against
// the PR's labels.
const (
	DefaultRiskLabelHigh    = "risk:high"
	DefaultRiskLabelRoutine = "risk:routine"
)

// RiskClassification is the resolved tier plus a short, human-readable reason and
// the signal source that decided it ("label" | "path" | "default"). The reason is
// recorded as a job event so an escalation is explainable in the report/dashboard.
type RiskClassification struct {
	Tier   string
	Source string
	Reason string
}

// ClassifyRisk resolves a change's risk tier from, in priority order: an explicit
// PR label (high/routine label wins over paths), then a changed-path glob match
// against highRiskPaths, then the default `routine`. labelHigh/labelRoutine and
// highRiskPaths fall back to the built-in defaults when empty. It is a pure
// function of its inputs so it is exhaustively unit-testable.
func ClassifyRisk(highRiskPaths []string, labelHigh, labelRoutine string, labels, changedPaths []string) RiskClassification {
	labelHigh = strings.TrimSpace(labelHigh)
	if labelHigh == "" {
		labelHigh = DefaultRiskLabelHigh
	}
	labelRoutine = strings.TrimSpace(labelRoutine)
	if labelRoutine == "" {
		labelRoutine = DefaultRiskLabelRoutine
	}
	if len(highRiskPaths) == 0 {
		highRiskPaths = DefaultHighRiskPaths
	}

	// 1) Explicit label wins over path heuristics. A high label escalates; a
	// routine label de-escalates. If (pathologically) both are present, high wins
	// — a safety-biased tie-break so a mislabeled change is never under-reviewed.
	hasHigh, hasRoutine := false, false
	for _, l := range labels {
		switch strings.ToLower(strings.TrimSpace(l)) {
		case strings.ToLower(labelHigh):
			hasHigh = true
		case strings.ToLower(labelRoutine):
			hasRoutine = true
		}
	}
	if hasHigh {
		return RiskClassification{Tier: RiskTierHigh, Source: "label", Reason: fmt.Sprintf("PR carries the %q label", labelHigh)}
	}
	if hasRoutine {
		return RiskClassification{Tier: RiskTierRoutine, Source: "label", Reason: fmt.Sprintf("PR carries the %q label", labelRoutine)}
	}

	// 2) Changed-path glob match.
	for _, path := range changedPaths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		if pattern, ok := matchAnyGlob(path, highRiskPaths); ok {
			return RiskClassification{
				Tier:   RiskTierHigh,
				Source: "path",
				Reason: fmt.Sprintf("changed path %q matches high-risk glob %q", path, pattern),
			}
		}
	}

	// 3) Default.
	return RiskClassification{Tier: RiskTierRoutine, Source: "default", Reason: "no high-risk label or path matched"}
}

// matchAnyGlob returns the first glob in globs that matches path, if any.
func matchAnyGlob(path string, globs []string) (string, bool) {
	for _, g := range globs {
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
		if globMatch(g, path) {
			return g, true
		}
	}
	return "", false
}

// globMatch reports whether the `**`-aware glob pattern matches the /-separated
// path. `**` matches zero or more whole path segments; within a single segment,
// `*` and `?` behave as filepath.Match. Paths are normalized to forward slashes.
func globMatch(pattern, path string) bool {
	pattern = strings.TrimPrefix(filepath.ToSlash(pattern), "./")
	path = strings.TrimPrefix(filepath.ToSlash(path), "./")
	return matchSegments(strings.Split(pattern, "/"), strings.Split(path, "/"))
}

func matchSegments(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			// `**` matches zero segments (advance the pattern) …
			if matchSegments(pat[1:], name) {
				return true
			}
			// … or one-or-more segments (consume a name segment, keep `**`).
			if len(name) > 0 {
				return matchSegments(pat, name[1:])
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		ok, err := filepath.Match(pat[0], name[0])
		if err != nil || !ok {
			return false
		}
		pat = pat[1:]
		name = name[1:]
	}
	return len(name) == 0
}

// Lens vocabulary. Each lens is a refutation-framed reviewer that is prompted to
// actively DISPROVE the change along one axis and report structured findings. The
// lens names double as the delegation ids of the high-risk fan-out.
const (
	LensCorrectness = "correctness"
	LensSecurity    = "security"
	LensRegression  = "regression"
)

// Severity values for a LensFinding. Only `critical` is load-bearing for
// synthesis: a critical refutation blocks the merge (fails the quorum); the
// lower severities are advisory and recorded for explainability.
const (
	SeverityCritical = "critical"
	SeverityHigh     = "high"
	SeverityMedium   = "medium"
	SeverityLow      = "low"
)

// LensFinding is the documented convention a lens reviewer returns in
// AgentResult.Findings ([]json.RawMessage). It is NOT a contract change: findings
// remains a free-form raw-message list, and this struct is only the agreed shape
// the synthesizer parses. A reviewer that returns findings in another shape is
// simply treated as having reported no critical refutation.
type LensFinding struct {
	// Lens is which axis produced the finding (correctness/security/regression).
	Lens string `json:"lens"`
	// Refuted is true when the reviewer believes it DISPROVED the change on this
	// axis (found a real defect), false when the change survived the lens.
	Refuted bool `json:"refuted"`
	// Severity grades a refutation; `critical` is the only blocking level.
	Severity string `json:"severity"`
	// Confidence is the reviewer's [0,1] self-reported confidence in the finding.
	Confidence float64 `json:"confidence"`
	// Evidence is a short human-readable justification (file:line, a failing case).
	Evidence string `json:"evidence"`
}

// ParseLensFindings best-effort decodes the {lens, refuted, severity, confidence,
// evidence} convention out of a result's raw findings. Malformed entries are
// skipped rather than erroring, so a reviewer that mixes shapes still contributes
// its well-formed findings.
func ParseLensFindings(raw []json.RawMessage) []LensFinding {
	out := make([]LensFinding, 0, len(raw))
	for _, r := range raw {
		var f LensFinding
		if err := json.Unmarshal(r, &f); err != nil {
			continue
		}
		out = append(out, f)
	}
	return out
}

// SynthesizeLensDecision maps a single lens reviewer's findings to the decision it
// should return so the EXISTING quorum synthesis engine can act on it mechanically:
// any refuted, critical-severity finding blocks the merge (decision `blocked`,
// which is a NON-approving quorum outcome); otherwise the lens `approved`. This is
// the per-reviewer finding→decision convention, NOT a synthesis engine — the
// cross-lens synthesis is the delegation synthesis_rule quorum gate.
func SynthesizeLensDecision(findings []LensFinding) string {
	for _, f := range findings {
		if f.Refuted && strings.EqualFold(strings.TrimSpace(f.Severity), SeverityCritical) {
			return "blocked"
		}
	}
	return "approved"
}

// lensPrompt returns the refutation-framed reviewer prompt for a lens on a given
// pull request. Each lens is told to actively DISPROVE the change on its axis and
// to return the structured {lens, refuted, severity, confidence, evidence}
// findings the synthesizer reads. A critical refutation must be reported as a
// `blocked` decision so it fails the quorum and blocks the merge.
func lensPrompt(lens string, event PullRequestEvent) string {
	var axis string
	switch lens {
	case LensCorrectness:
		axis = "CORRECTNESS: hunt for logic errors, unhandled edge cases, race conditions, " +
			"broken invariants, and incorrect error handling. Try to construct an input or state that makes the change produce a wrong result."
	case LensSecurity:
		axis = "SECURITY: hunt for injection, authn/authz gaps, secret/credential exposure, unsafe deserialization, " +
			"missing input validation, and privilege escalation. Try to construct an exploit against the change."
	case LensRegression:
		axis = "REGRESSION: hunt for behavior the change silently alters — removed/weakened tests, changed public contracts, " +
			"altered defaults, and side effects on unrelated call sites. Try to name an existing behavior the change breaks."
	default:
		axis = strings.ToUpper(lens)
	}
	return fmt.Sprintf(
		"You are the %s LENS on a HIGH-RISK review of pull request #%d for task %s. "+
			"Adopt a refutation stance: assume the change is WRONG and try to prove it along one axis.\n\n%s\n\n"+
			"Return gitmoot_result with a refutation-framed decision and structured findings. "+
			"Each finding is an object {\"lens\":%q,\"refuted\":true|false,\"severity\":\"critical|high|medium|low\",\"confidence\":0..1,\"evidence\":\"file:line — why\"}. "+
			"If you find a CRITICAL refutation, set decision to \"blocked\" (this blocks the merge); "+
			"otherwise set decision to \"approved\" and record any lower-severity findings as advisory.",
		strings.ToUpper(lens), event.PullRequest, taskLabel(event.TaskID, event.TaskTitle), axis, lens,
	)
}

// highRiskLensDelegations builds the refutation lens delegation batch for a
// high-risk PR. It always produces at least 2 lenses (correctness, security) and
// adds regression when there are >= 3 reviewers to run it (cheap). Each lens is a
// read-only review delegation tagged synthesis_rule "quorum" with the quorum
// threshold set to the full lens count, so ANY non-approving lens (a critical
// refutation, reported as `blocked`) fails the quorum and blocks the merge, while
// unanimous approval satisfies it. Reviewers are assigned round-robin; with a
// single configured reviewer every lens runs as a distinct job on that reviewer.
func highRiskLensDelegations(reviewers []string, event PullRequestEvent) []Delegation {
	reviewers = compactStrings(reviewers)
	if len(reviewers) == 0 {
		return nil
	}
	lenses := []string{LensCorrectness, LensSecurity}
	if len(reviewers) >= 3 {
		lenses = append(lenses, LensRegression)
	}
	quorum := len(lenses)
	dels := make([]Delegation, 0, len(lenses))
	for i, lens := range lenses {
		dels = append(dels, Delegation{
			ID:            lens,
			Agent:         reviewers[i%len(reviewers)],
			Action:        "review",
			Prompt:        lensPrompt(lens, event),
			SynthesisRule: "quorum",
			Quorum:        quorum,
			// A refuting lens must not fail the whole tree — its `blocked` decision is a
			// quorum SIGNAL, not an infra failure — so let siblings finish and let the
			// quorum gate render the verdict.
			FailurePolicy: "continue",
		})
	}
	return dels
}
