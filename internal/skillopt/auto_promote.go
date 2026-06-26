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
//   - min_score unset (nil) OR a nil candidate Summary.Score -> hard do-not-promote
//     ON THE MODE A (verifiable-outcome) PATH. A genuine Mode B (ask/research)
//     candidate has NO harvester score; its evidence is the pairwise bandit, so the
//     score/feedback-sample floors are satisfied by the confidence + bandit-pull
//     floor instead (see modeBConfidenceBacked below) and a nil score is allowed.
//   - require_external_ci=true with no real-CI positive in the run -> do not promote
//     (still hard, including on the Mode B path: a pure ask agent has no CI, so an
//     operator simply leaves require_external_ci off; if they DO set it we fail safe).
//   - min_confidence (#473 Mode B) is ADDITIVE and OPTIONAL: when
//     policy.AutoPromoteMinConfidence is nil (the default) the bandit confidence is
//     ignored entirely and behavior is byte-identical to #471 — `confidence` may be
//     nil. When the floor IS set, promote additionally requires a non-nil
//     confidence >= the floor AND at least AutoPromoteMinSamples bandit samples
//     behind it (small-sample over-confidence guard); a nil/low confidence or thin
//     evidence FAILS SAFE to notify-only.
//
// MODE B PATH (the ask/research loop this slice closes): when AutoPromoteMinConfidence
// is set AND a confidence backed by >= AutoPromoteMinSamples bandit pulls is present,
// the bandit pulls ARE the sample evidence and the pairwise preference IS the quality
// signal — so the bandit-pull floor stands in for len(feedbackEvents) (a Mode B
// candidate has zero harvester feedback rows) and the nil-score hard stop is skipped.
// This is what makes a real ask-agent candidate (empty feedbackEvents, nil score)
// reachable by the confidence gate; without it the zero-evidence / nil-score hard
// stops above would make Mode B auto-promotion structurally unreachable. A score, if
// present, must still clear min_score; require_external_ci, if set, must still hold.
//
// `confidence` is the Mode B P(challenger>champion) the runCandidateNotify seam
// supplies from the bandit (nil when there is no arm); `confidenceSamples` is the
// challenger arm's pull count behind it (0 when absent). Both are ignored unless
// AutoPromoteMinConfidence is set, so EXISTING callers/tests are unchanged in
// behavior.
//
// On a pass the reason names the guardrails that held (score, samples, external CI,
// confidence) so the candidate.auto_promoted event explains the decision.
func EvaluateAutoPromote(policy config.SkillOptPolicy, candidate CandidatePackage, feedbackEvents []db.FeedbackEvent, feedbackUnavailable bool, confidence *float64, confidenceSamples int) AutoPromoteDecision {
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

	// min_samples must always be set (the unset-is-hard-no discipline holds for both
	// paths: a user who flips auto_promote without a sample floor must never promote).
	if policy.AutoPromoteMinSamples == nil {
		return notifyOnly("auto_promote_min_samples is not set; refusing to promote without a sample floor (notify only)")
	}

	// modeBConfidenceBacked is true when this candidate's promotion rests on pairwise
	// bandit evidence rather than a harvested score/feedback run: a min_confidence
	// floor is configured AND a confidence is present. On this path the bandit pulls
	// are the samples and the preference is the quality signal, so the Mode A
	// feedback-sample and score floors below are deferred to the confidence block —
	// which is the single place the bandit-pull floor and confidence floor are
	// enforced (with precise, Mode-B-worded reasons). The pull-count adequacy is NOT
	// part of this predicate on purpose: a too-thin confidence must still reach the
	// confidence block and be rejected there with the "below auto_promote_min_samples"
	// reason, not silently fall back to the Mode A "zero evidence" stop.
	modeBConfidenceBacked := policy.AutoPromoteMinConfidence != nil && confidence != nil

	passed := make([]string, 0, 4)

	// Guardrail: min_samples (Mode A feedback rows). On the Mode B confidence path the
	// bandit pulls ARE the sample evidence (a real ask candidate has zero harvester
	// feedback rows), so the sample floor is checked against the bandit pulls in the
	// confidence block below instead of here.
	if !modeBConfidenceBacked {
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
	}

	// Guardrail: min_score. Unset is always a hard do-not-promote. A nil candidate
	// score is a hard stop ONLY on the Mode A path — a genuine Mode B candidate has no
	// harvester score, so the bandit confidence stands in for it; a score that IS
	// present must still clear the floor on either path.
	if policy.AutoPromoteMinScore == nil {
		return notifyOnly("auto_promote_min_score is not set; refusing to promote without a score floor (notify only)")
	}
	if candidate.Summary.Score == nil {
		if !modeBConfidenceBacked {
			return notifyOnly("candidate has no score; cannot evaluate auto_promote_min_score (notify only)")
		}
		// Mode B: no score is expected; the confidence gate below carries the burden.
	} else {
		score := *candidate.Summary.Score
		if score < *policy.AutoPromoteMinScore {
			return notifyOnly(fmt.Sprintf("score %.4g below auto_promote_min_score=%.4g; notify only", score, *policy.AutoPromoteMinScore))
		}
		passed = append(passed, fmt.Sprintf("score %.4g >= %.4g", score, *policy.AutoPromoteMinScore))
	}

	// Guardrail: require_external_ci. At least one feedback event in the eval_run
	// must be a real-CI positive (mirrors the harvester's vocabulary, never the
	// raw 0.5 band). Applies on both paths: a Mode B ask agent has no CI, so an
	// operator leaves this off; if they set it anyway we fail safe.
	if policy.AutoPromoteRequireExternalCI {
		if !hasRealExternalCIPositive(feedbackEvents) {
			return notifyOnly("auto_promote_require_external_ci is set but no eval_run feedback recorded a real external-CI pass; notify only")
		}
		passed = append(passed, "external CI confirmed")
	}

	// Guardrail: min_confidence (#473 Mode B). ADDITIVE and OPTIONAL — when unset
	// (the default) the bandit confidence is ignored and this whole block is a
	// no-op, so #471 behavior is byte-identical. When set, promote requires a
	// non-nil confidence at or above the floor, backed by at least
	// AutoPromoteMinSamples bandit pulls (a Beta posterior on a couple of pulls can
	// read 0.9 by luck — the tiering floor refuses thin evidence). nil/low/thin all
	// fail safe to notify-only. On the Mode B path this floor IS the sample floor
	// (the bandit pulls stand in for the absent harvester feedback rows).
	if policy.AutoPromoteMinConfidence != nil {
		floor := *policy.AutoPromoteMinConfidence
		if confidence == nil {
			return notifyOnly(fmt.Sprintf("auto_promote_min_confidence=%.4g is set but no bandit confidence is available; notify only", floor))
		}
		if confidenceSamples < *policy.AutoPromoteMinSamples {
			return notifyOnly(fmt.Sprintf("bandit confidence %.0f%% has only %d sample(s), below auto_promote_min_samples=%d; notify only", *confidence*100, confidenceSamples, *policy.AutoPromoteMinSamples))
		}
		if *confidence < floor {
			return notifyOnly(fmt.Sprintf("bandit confidence %.0f%% below auto_promote_min_confidence=%.4g; notify only", *confidence*100, floor))
		}
		passed = append(passed, fmt.Sprintf("confidence %.0f%% >= %.4g over %d samples", *confidence*100, floor, confidenceSamples))
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
