package skillopt

import (
	"fmt"
	"strings"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
)

// AutoPromoteDecision is the pure, total result of EvaluateAutoPromote (#471): a
// promote/notify-only verdict plus a human-readable reason that travels into the
// candidate.* event Detail. Promote is true ONLY when EVERY configured, checkable
// guardrail held; any uncertainty (off, nil score, unset threshold, deferred
// knob, …) returns Promote=false with a reason explaining why — the fail-safe
// "notify, don't promote" path.
type AutoPromoteDecision struct {
	// Promote is true only when the policy is on AND every configured guardrail
	// passed; false is the always-safe default.
	Promote bool
	// Reason is a short, human-facing explanation (named guardrails on a pass; the
	// first blocking reason on a fail). It is redacted/path-scrubbed at the event
	// seam before it leaves the box.
	Reason string
}

// notifyOnly is the canonical fail-safe decision: never promote, carry the reason.
func notifyOnly(reason string) AutoPromoteDecision {
	return AutoPromoteDecision{Promote: false, Reason: reason}
}

// EvaluateAutoPromote is the pure, deterministic, total auto-promote guardrail
// evaluator (#471). It takes the host [skillopt] policy, the just-imported
// candidate package, and the feedback events of the candidate's eval_run, and
// returns a promote/notify-only decision + reason. It performs NO I/O and NEVER
// mutates state: the CLI helper resolves the eval_run, calls this, and only on a
// Promote=true result invokes the EXISTING PromoteAgentTemplateVersion.
//
// FAIL-SAFE DISCIPLINE (every uncertainty returns Promote=false):
//   - policy.AutoPromote off (the default) -> notify only (manual, byte-identical).
//   - feedbackUnavailable=true (the caller could not resolve/read the eval_run, e.g.
//     a ListFeedbackEvents error or a missing run id) -> hard do-not-promote: we can
//     never promote on evidence we failed to read, regardless of the configured
//     min_samples floor.
//   - require_measured_judge=true -> DEFERRED (#344): no judge<->human calibration
//     source exists, so honoring it now would lie; fail safe.
//   - canary=true -> DEFERRED (canary follow-on): the sampled-traffic + regression
//     infrastructure does not exist; fail safe.
//   - ZERO feedback samples -> a HARD do-not-promote (an absolute floor of at least
//     one real sample), even when min_samples is explicitly 0: we never promote on
//     zero verifiable evidence.
//   - min_samples unset (nil) -> a HARD do-not-promote, NOT 0 (a user who flips
//     auto_promote without a sample floor must never promote a sparse candidate).
//   - min_score unset (nil) OR a nil candidate Summary.Score -> hard do-not-promote.
//   - require_external_ci=true with no real-CI positive in the run -> do not promote.
//
// On a pass the reason names the guardrails that held (score, samples, external CI)
// so the candidate.auto_promoted event explains the decision.
func EvaluateAutoPromote(policy config.SkillOptPolicy, candidate CandidatePackage, feedbackEvents []db.FeedbackEvent, feedbackUnavailable bool) AutoPromoteDecision {
	if !policy.AutoPromote {
		return notifyOnly("auto_promote is off; notify only (manual promotion)")
	}

	// A read error or an unresolvable eval_run is uncertainty, not "zero samples":
	// promoting on evidence we could not read would defeat every downstream
	// guardrail, so fail safe outright.
	if feedbackUnavailable {
		return notifyOnly("candidate eval_run feedback is unavailable (unresolved run or read error); notify only")
	}

	// Deferred knobs (#471): parsed for forward-compat, but they have no honest
	// implementation yet, so when set they force the safe path.
	if policy.AutoPromoteRequireMeasuredJudge {
		return notifyOnly("auto_promote_require_measured_judge is set but judge calibration is unavailable (deferred, #344); notify only")
	}
	if policy.AutoPromoteCanary {
		return notifyOnly("auto_promote_canary is set but canary promotion is deferred; notify only")
	}

	passed := make([]string, 0, 3)

	// Guardrail: min_samples. Unset is a hard do-not-promote, never 0.
	if policy.AutoPromoteMinSamples == nil {
		return notifyOnly("auto_promote_min_samples is not set; refusing to promote without a sample floor (notify only)")
	}
	samples := len(feedbackEvents)
	// Absolute floor: a configured auto_promote_min_samples=0 is a LEGAL value, but
	// `0 < 0` is false, so without this an explicit 0 would let a zero-evidence
	// candidate promote. We never auto-promote on zero verifiable samples regardless
	// of the configured minimum.
	if samples == 0 {
		return notifyOnly("no eval_run feedback samples; refusing to auto-promote on zero evidence (notify only)")
	}
	if samples < *policy.AutoPromoteMinSamples {
		return notifyOnly(fmt.Sprintf("only %d feedback sample(s), below auto_promote_min_samples=%d; notify only", samples, *policy.AutoPromoteMinSamples))
	}
	passed = append(passed, fmt.Sprintf("%d samples >= %d", samples, *policy.AutoPromoteMinSamples))

	// Guardrail: min_score. Unset, or a nil candidate score, is a hard do-not-promote.
	if policy.AutoPromoteMinScore == nil {
		return notifyOnly("auto_promote_min_score is not set; refusing to promote without a score floor (notify only)")
	}
	if candidate.Summary.Score == nil {
		return notifyOnly("candidate has no score; cannot evaluate auto_promote_min_score (notify only)")
	}
	score := *candidate.Summary.Score
	if score < *policy.AutoPromoteMinScore {
		return notifyOnly(fmt.Sprintf("score %.4g below auto_promote_min_score=%.4g; notify only", score, *policy.AutoPromoteMinScore))
	}
	passed = append(passed, fmt.Sprintf("score %.4g >= %.4g", score, *policy.AutoPromoteMinScore))

	// Guardrail: require_external_ci. At least one feedback event in the eval_run
	// must be a real-CI positive (mirrors the harvester's vocabulary, never the
	// raw 0.5 band).
	if policy.AutoPromoteRequireExternalCI {
		if !hasRealExternalCIPositive(feedbackEvents) {
			return notifyOnly("auto_promote_require_external_ci is set but no eval_run feedback recorded a real external-CI pass; notify only")
		}
		passed = append(passed, "external CI confirmed")
	}

	return AutoPromoteDecision{
		Promote: true,
		Reason:  "auto-promoted: " + strings.Join(passed, ", "),
	}
}

// hasRealExternalCIPositive reports whether any feedback event in the run records
// a merge that passed genuine external CI, via the shared harvester predicate.
func hasRealExternalCIPositive(events []db.FeedbackEvent) bool {
	for _, event := range events {
		if FeedbackEventIsRealExternalCIPositive(event) {
			return true
		}
	}
	return false
}
