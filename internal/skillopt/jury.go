package skillopt

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// JuryJudgeVerdict is ONE judge's contribution to the pure jury aggregation
// (#349): its per-dimension [0,1] scores (empty on a pairwise A/B pick that has
// no rubric) and its boolean Decision (promote / pairwise "challenger wins").
// The aggregator is agnostic to the boolean's semantics — it only counts votes —
// so the same EvaluateJury serves both a promotion-boundary rubric jury and a
// pairwise A/B jury.
type JuryJudgeVerdict struct {
	// DimensionScores is this judge's per-dimension [0,1] rubric. It MAY be empty
	// (a pairwise A/B pick carries no rubric); the median/veto outputs are then
	// simply empty/inert and only the majority/disagreement outputs are meaningful.
	DimensionScores map[string]float64
	// Decision is this judge's boolean verdict: true = promote / "challenger wins",
	// false = reject / "champion wins". The aggregator never interprets it beyond
	// the majority count.
	Decision bool
}

// JuryConfig parameterizes the pure aggregation (#349). All fields are optional;
// the zero value is a total, safe configuration (no veto can fire, std-based
// disagreement is disabled).
type JuryConfig struct {
	// VetoDimensions is the configured set of safety / hard-correctness dimensions
	// subject to the minority-veto: a SINGLE judge scoring below VetoFloor on any
	// of these BLOCKS (fail-closed), even when the majority would promote. Empty
	// (the default) disables the veto entirely.
	VetoDimensions []string
	// VetoFloor is the [0,1] floor for the veto dimensions. The default 0 makes the
	// veto inert (a clamped [0,1] score is never < 0), so a veto only ever fires
	// when an operator sets BOTH a floor AND the dimensions.
	VetoFloor float64
	// DisagreementTau is the per-dimension population-std threshold above which the
	// jury is flagged as disagreeing (routes to a human, feeds #345). <= 0 (the
	// default) DISABLES the std-based check, leaving only the vote-split check.
	DisagreementTau float64
}

// JuryDecision is the pure, deterministic, total result of EvaluateJury (#349):
// the per-dimension medians, the majority boolean, the fail-closed veto flag, and
// the disagreement flag (+ human-readable reasons). It performs NO I/O and never
// mutates state — like EvaluateAutoPromote / EvaluateCanaryRegression — so the
// caller records it as EVIDENCE only and NEVER promotes or touches the bandit on it.
type JuryDecision struct {
	// MedianScores is the per-dimension MEDIAN across the judges that scored that
	// dimension (robust to one outlier judge; even N averages the two middle
	// values). Empty when no judge supplied any dimension (the pairwise A/B path).
	MedianScores map[string]float64
	// Decision is the MAJORITY vote of the judges' booleans. A tie (including the
	// even-N 1:1 / 2:2 case) resolves to false — the fail-safe "do not promote /
	// keep the baseline" verdict — and is additionally flagged as disagreement. A
	// fired veto forces Decision=false regardless of the majority.
	Decision bool
	// Vetoed is true when the minority-veto fired: at least one judge scored below
	// VetoFloor on a configured safety dimension (fail-closed). It is independent of
	// Disagreement (a unanimous below-floor reject is a veto but not a disagreement).
	Vetoed bool
	// VetoReason names the dimension/score that tripped the veto (empty when none).
	VetoReason string
	// Disagreement is true when the judges materially disagree: a non-unanimous
	// boolean vote (the 2:1 split generalized to "both sides voted") OR any
	// dimension whose population std exceeds DisagreementTau. It routes to a human
	// and is recorded to feed the judge<->human capture (#345).
	Disagreement bool
	// DisagreementReason explains the flag (vote split and/or the offending
	// dimension std); empty when there is no disagreement.
	DisagreementReason string
	// JudgeCount is the number of judges aggregated; Approve/Reject are the boolean
	// tallies (Approve+Reject == JudgeCount).
	JudgeCount int
	Approve    int
	Reject     int
}

// EvaluateJury is the pure, deterministic, total jury aggregator (#349): given N
// judges' per-dimension scores + boolean verdicts and a JuryConfig, it computes
// the per-dimension MEDIAN (robust to one outlier), the MAJORITY boolean vote
// (tie => false, fail-safe), the fail-closed MINORITY-VETO over the configured
// safety dimensions, and the DISAGREEMENT flag (vote split or std > tau). It is
// the unambiguous, load-bearing core; the CLI wiring records its result as
// EVIDENCE only (never promotes, never touches the bandit).
//
// It is total: zero judges yields the zero decision (JudgeCount 0, Decision
// false, no veto, no disagreement) and never panics; a judge may omit any
// dimension (the median is taken over the judges that scored it).
func EvaluateJury(cfg JuryConfig, verdicts []JuryJudgeVerdict) JuryDecision {
	decision := JuryDecision{MedianScores: map[string]float64{}, JudgeCount: len(verdicts)}
	if len(verdicts) == 0 {
		return decision
	}

	// Collect each dimension's scores across the judges that provided it, in a
	// deterministic key order so the reasons (and median map iteration in tests)
	// are stable.
	dimValues := map[string][]float64{}
	var dims []string
	for _, v := range verdicts {
		for k, val := range v.DimensionScores {
			if _, seen := dimValues[k]; !seen {
				dims = append(dims, k)
			}
			dimValues[k] = append(dimValues[k], val)
		}
	}
	sort.Strings(dims)
	for _, k := range dims {
		decision.MedianScores[k] = medianFloat(dimValues[k])
	}

	// MAJORITY vote. A tie (approve == reject, including even-N 1:1/2:2) is the
	// fail-safe reject: Decision=false, and it is flagged as disagreement below.
	for _, v := range verdicts {
		if v.Decision {
			decision.Approve++
		} else {
			decision.Reject++
		}
	}
	decision.Decision = decision.Approve > decision.Reject

	// DISAGREEMENT: a non-unanimous boolean vote (the 2:1 split, generalized to
	// "both sides cast a vote") OR any dimension whose population std exceeds tau.
	voteSplit := decision.Approve > 0 && decision.Reject > 0
	stdReason := ""
	if cfg.DisagreementTau > 0 {
		for _, k := range dims {
			if s := populationStd(dimValues[k]); s > cfg.DisagreementTau {
				stdReason = fmt.Sprintf("dimension %q std %.4g > tau %.4g", k, s, cfg.DisagreementTau)
				break
			}
		}
	}
	if voteSplit || stdReason != "" {
		decision.Disagreement = true
		switch {
		case voteSplit && stdReason != "":
			decision.DisagreementReason = fmt.Sprintf("vote split %d:%d; %s", decision.Approve, decision.Reject, stdReason)
		case voteSplit:
			decision.DisagreementReason = fmt.Sprintf("vote split %d:%d", decision.Approve, decision.Reject)
		default:
			decision.DisagreementReason = stdReason
		}
	}

	// MINORITY-VETO (fail-closed): one judge below the floor on a configured safety
	// dimension BLOCKS, forcing Decision=false regardless of the majority. It is
	// independent of the disagreement flag (a unanimous below-floor reject vetoes
	// without being a disagreement).
	// Build a case/space-insensitive lookup so a veto dimension configured as
	// "Safety" still matches a judge dimension key "safety": the minority-veto is a
	// fail-CLOSED safety control and must never silently fail OPEN on a mere
	// name-casing/whitespace mismatch between the config and the rubric keys. Merge
	// over the already-sorted dims for a deterministic VetoReason.
	normValues := map[string][]float64{}
	for _, k := range dims {
		nk := normalizeDimKey(k)
		normValues[nk] = append(normValues[nk], dimValues[k]...)
	}
	for _, dim := range cfg.VetoDimensions {
		vals, ok := normValues[normalizeDimKey(dim)]
		if !ok {
			continue
		}
		for _, v := range vals {
			if v < cfg.VetoFloor {
				decision.Vetoed = true
				decision.VetoReason = fmt.Sprintf("safety dimension %q scored %.4g below veto floor %.4g (minority veto, fail-closed)", dim, v, cfg.VetoFloor)
				break
			}
		}
		if decision.Vetoed {
			break
		}
	}
	if decision.Vetoed {
		decision.Decision = false
	}

	return decision
}

// normalizeDimKey canonicalizes a dimension name for veto matching so the
// configured veto set and the judges' dimension keys compare case- and
// whitespace-insensitively (a fail-closed safety control must not depend on the
// operator matching the rubric's exact casing).
func normalizeDimKey(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// medianFloat returns the median of vals (the caller guarantees len>0). Odd N is
// the middle element; even N averages the two middle elements so a single outlier
// judge cannot drag the central estimate. It copies before sorting so the caller's
// slice is untouched.
func medianFloat(vals []float64) float64 {
	n := len(vals)
	sorted := append([]float64(nil), vals...)
	sort.Float64s(sorted)
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

// populationStd returns the population standard deviation of vals (0 for an
// empty slice or a single value), used for the std-based disagreement check.
func populationStd(vals []float64) float64 {
	n := len(vals)
	if n == 0 {
		return 0
	}
	var sum float64
	for _, v := range vals {
		sum += v
	}
	mean := sum / float64(n)
	var sq float64
	for _, v := range vals {
		d := v - mean
		sq += d * d
	}
	return math.Sqrt(sq / float64(n))
}
