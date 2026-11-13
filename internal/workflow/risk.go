package workflow

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Risk classification (#650). ClassifyRisk labels a change `routine` or `high`
// from PR labels and high-risk path globs. Its remaining consumer is remote
// review routing (cli/agent_dispatch.go); the high-risk multi-lens review
// fan-out it once drove was retired with the delegation engine (#2588).

// RiskTier is the resolved risk level of a change: `routine` (the default) or
// `high`.
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
