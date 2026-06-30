package config

import (
	"fmt"
	"os"
)

func Initialize(paths Paths) error {
	for _, dir := range []string{paths.Home, paths.Logs, paths.Workspaces, paths.Evals, paths.ArtifactBlobs} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("chmod %s: %w", dir, err)
		}
	}

	if _, err := os.Stat(paths.ConfigFile); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat config file: %w", err)
	}

	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)), 0o600); err != nil {
		return fmt.Errorf("write config file: %w", err)
	}
	return nil
}

func DefaultConfig(paths Paths) string {
	return fmt.Sprintf(`# Gitmoot local configuration.

[paths]
database = %q
logs = %q
workspaces = %q
evals = %q
artifact_blobs = %q

[parallel_sessions]
same_session = "fork_temp_session"
merge_back = "summary"
max_temp_sessions_per_agent = 4
eligible_actions = ["ask", "review", "implement"]

[orchestrate]
# Render one live herdr pane per delegation subagent when a job opts in with
# --cockpit. cockpit_mode: on | off | auto (auto gates on herdr reachability).
# cockpit_max_panes caps concurrent panes (constrained hosts ~4); beyond the cap
# a job runs status-only with no pane. cockpit_pane_key: job (one pane per job)
# or seat (reuse one pane per seat). cockpit_session is an optional named session.
cockpit_mode = "auto"
cockpit_session = ""
cockpit_max_panes = 4
cockpit_pane_key = "job"
# escalate_human failure_policy (#340): when a delegation pauses awaiting a human,
# the daemon @-tags escalation_handle (default: the repo owner) in a comment with
# the resume instructions. escalation_ttl auto-finalizes a never-answered pause
# (Go duration; default 24h). Both optional.
escalation_handle = ""
escalation_ttl = ""
# Optional default timeouts for child delegation jobs. Empty means unbounded,
# preserving historical behavior. Per-delegation timeout always wins; otherwise
# phase-specific defaults apply, then default_delegation_timeout, then unbounded.
default_delegation_timeout = ""
default_plan_timeout = ""
default_implement_timeout = ""
default_review_timeout = ""
default_gate_timeout = ""
default_repair_timeout = ""

# [skillopt] is the OFF-BY-DEFAULT template-learning policy (#465 Mode A, #471).
# With no [skillopt] section behavior is byte-identical: no trace harvesting, no
# event emission, and promotion stays fully MANUAL. auto_trace_enabled opts the
# daemon into harvesting an implement job's verifiable terminal outcome into a
# synthetic feedback event; cross_family_review_enabled (requires auto_trace) adds
# a down-weighted cross-family review soft signal. revert_detection_enabled (#467,
# requires auto_trace; UNSET = on) lets the daemon detect a merged GitHub
# Revert-button PR (body "Reverts owner/repo#NN") and CORRECT the original PR's
# auto-trace positive to a negative in place; set it false to keep the harvester on
# but turn the (delayed, corrective) revert overwrites OFF. auto_promote (#471) opts into
# AUTO-PROMOTING a newly-pending candidate, but ONLY when every configured
# guardrail below holds — any uncertainty fails safe to notify-only (no promote).
# A pending candidate ALWAYS emits candidate.awaiting_promotion when [events] is
# configured, independent of auto_promote; a successful auto-promote additionally
# emits candidate.auto_promoted so a human can review or roll back.
# The guardrails read the candidate's HARVESTER auto-trace run
# (auto-trace:<version_id>), NOT the human/markdown review run. A feedback read
# error or unresolvable run fails safe to notify-only, and ZERO samples is always a
# hard do-not-promote regardless of the min below.
#   auto_promote_min_samples: minimum feedback-event count in the candidate's
#     auto-trace run. UNSET is a HARD "do not promote" (never 0) — flipping
#     auto_promote on without this never promotes. Even an explicit 0 cannot promote
#     a zero-evidence candidate (absolute floor of at least one sample).
#   auto_promote_min_score: minimum candidate score. UNSET, or a candidate with no
#     score, is a HARD "do not promote".
#   auto_promote_require_external_ci: require at least one auto-trace feedback event
#     to record a merge that passed GENUINE external CI (not the no-CI band). Keys
#     off the harvester's provenance so only Mode A (auto-trace) evidence counts and
#     a cross-family review row cannot spoof it.
#   auto_promote_require_measured_judge: PARSED but DEFERRED (gated on #344) — there
#     is no judge<->human calibration source yet, so when true it FAILS SAFE to
#     notify-only.
#   auto_promote_canary (#484): OFF by default. When true AND auto_promote_canary_sample
#     is a valid fraction, a guardrails-pass candidate is promoted to a CANARY version
#     (routed a sampled fraction of resolutions while the prior champion stays the live
#     current version) instead of directly to current; a bounded regression window then
#     graduates it (-> current, candidate.auto_promoted) or auto-rolls-back on a real
#     regression vs the prior champion (champion stays current, canary rejected,
#     candidate.rolled_back). Off ⇒ byte-identical to #471's direct promote. When true
#     but auto_promote_canary_sample is unset/invalid it FAILS SAFE to notify-only.
#   auto_promote_canary_sample (#484): the canary's sampled-traffic fraction in (0,1] —
#     the per-resolution probability a job routes to the active canary version instead of
#     the champion. UNSET (the default) disables the canary path (notify-only fail-safe
#     when auto_promote_canary is on). 1.0 routes ALL traffic to the canary (useful for
#     a deterministic test); a small value (e.g. 0.1) routes about a tenth of traffic.
#   auto_promote_min_confidence (#473 Mode B): minimum bandit confidence
#     P(challenger>champion) — supplied by the manual 'skillopt ab' champion-
#     challenger A/B — required to auto-promote. UNSET ignores the guardrail
#     entirely (byte-identical to #471). When SET, auto-promote additionally
#     requires a non-nil confidence >= this floor; a nil/low confidence FAILS SAFE
#     to notify-only.
#   bandit_min_samples (#473 Mode B): per-agent low-traffic floor. Below it the
#     bandit still records preferences and updates its posterior but live-traffic
#     A/B never auto-runs and the confidence is never trusted to auto-promote off
#     thin evidence. The manual 'skillopt ab' CLI is always allowed regardless.
#     (default 30)
#   live_ab_sample_rate (#482 Mode B live A/B): probability in [0,1] that a single
#     foreground 'agent ask' (on a MANAGED agent whose champion arm is already at
#     or above bandit_min_samples) is intercepted into a champion-vs-challenger
#     A/B — running both variants serially, presenting both answers, and recording
#     the human pick through the SAME bandit + RankedFeedbackEvent path as the
#     manual 'skillopt ab'. UNSET / 0.0 (the default) NEVER intercepts — the ask
#     path is byte-identical. It only writes feedback + updates the posterior;
#     promotion stays MANUAL. Each intercepted ask runs the runtime twice (cost),
#     which is why it is sampled and floored.
#   mode_b_judge_enabled (#483 Mode B): OFF by default. When true (or with the
#     per-invocation 'skillopt ab --judge' flag), in addition to the human pick a
#     CROSS-FAMILY LLM judge (a DIFFERENT runtime family than the agent under test)
#     also picks the better of the two shuffled A/B answers and records a SEPARATE
#     skillopt-ab-judge feedback row that COEXISTS with (and weights BELOW) the human
#     row. The judge is cross-family ONLY (skipped — never same-family — when no other
#     family is available), NEVER touches the promotion bandit, and is never the sole
#     gate; its trust is DEFERRED to MEASURE-THE-JUDGE (#344). Off ⇒ byte-identical.
#   mode_b_jury_size (#349 Mode B): turns the single cross-family judge above into a
#     cross-family judge JURY — up to this many judges from DISTINCT model families
#     judge the same blind A/B, and their picks are aggregated by MAJORITY vote with
#     a DISAGREEMENT flag (non-unanimous vote, or per-dimension std > tau) that routes
#     to a human and feeds #345. 0/1 (the default) ⇒ jury OFF, byte-identical to the
#     single judge. Families are DEDUPED (diversity over headcount): a host with only
#     2 families caps the jury at 2; with < 2 distinct families it falls back to the
#     single judge (never fails the eval). Like the single judge, the jury is EVIDENCE
#     only — it NEVER promotes and NEVER touches the bandit.
#   mode_b_jury_veto_dimensions (#349): optional comma list of safety / hard-correctness
#     rubric dimensions subject to the jury's MINORITY-VETO — one judge below
#     mode_b_jury_veto_floor on any of these BLOCKS (fail-closed). UNSET/empty ⇒ no
#     veto. Inert on the pairwise A/B path (no rubric); applies to a rubric jury.
#   mode_b_jury_veto_floor (#349): the [0,1] floor for the veto dimensions. 0.0 (the
#     default) makes the veto inert (a clamped score is never < 0).
#   mode_b_jury_disagreement_tau (#349): per-dimension population-std threshold above
#     which the jury flags disagreement. 0.0 (the default) disables the std check,
#     leaving only the vote-split check (a non-unanimous vote always flags).
#   deterministic_checkers_enabled (#485): OFF by default; requires auto_trace. When
#     true, a MERGED implement job additionally runs a best-effort, DETACHED leg of
#     plain external TOOLS (code duplication, lint, cyclomatic complexity) plus a
#     pure-Go diff-size metric, normalizes each to a [0,1] dimension, and records a
#     THIRD coexisting OBJECTIVE feedback row (reviewer gitmoot-checker, item
#     checker#repo#pr) in the SAME auto-trace run as the verifiable floor and the
#     cross-family review. These dimensions are TOOL-MEASURED (no LLM) and
#     un-gameable. DEGRADE-GRACEFULLY: a missing tool binary, no PR-head checkout, a
#     tool error, or a timeout SKIPS that ONE dimension (no row for it) and NEVER
#     fails the harvest or blocks the merge; an all-skipped run writes no row.
#     diff_size is pure-Go and always available; tool dims appear only when their
#     binary AND a checkout are present. Off ⇒ byte-identical. Promotion stays MANUAL.
#   deterministic_checkers (#485): optional comma list selecting which checkers run
#     when enabled (diff_size,duplication,lint,complexity). UNSET/empty ⇒ the safe
#     default (diff_size only) so a tool-less host runs the always-available metric
#     and never a heavy tool. Narrow it to run only the cheap dims, or widen it to
#     opt heavy tools (jscpd/golangci-lint) in. An unknown name is ignored.
# [skillopt]
# auto_trace_enabled = false
# cross_family_review_enabled = false
# revert_detection_enabled = true
# deterministic_checkers_enabled = false
# deterministic_checkers = diff_size
# auto_promote = false
# auto_promote_min_samples = 0
# auto_promote_min_score = 0.0
# auto_promote_require_external_ci = false
# auto_promote_require_measured_judge = false
# auto_promote_canary = false
# auto_promote_canary_sample = 0.1
# auto_promote_min_confidence = 0.95
# bandit_min_samples = 30
# live_ab_sample_rate = 0.0
# mode_b_judge_enabled = false
# mode_b_jury_size = 1
# mode_b_jury_veto_dimensions =
# mode_b_jury_veto_floor = 0.0
# mode_b_jury_disagreement_tau = 0.0

# [admission] is an OPT-IN, off-by-default host-global concurrency budget the
# daemon applies BEFORE starting each agent session, on top of --workers/pool
# and the per-repo checkout / runtime-session locks (issue #365). With both caps
# 0 (the default, below) it is DISABLED and scheduling is byte-identical to a
# config with no [admission] section. Set max_concurrent_sessions to cap total
# in-flight sessions across all repos in the daemon process; set max_memory_gb to
# cap the summed per-runtime RAM estimate of in-flight sessions (a job is admitted
# only if it fits BOTH). A job that does not fit is left queued and retried next
# tick — never failed. The per-runtime *_memory_gb values are operator-tunable
# RAM priors; a non-session runtime contributes 0. Note: the budget is enforced
# per daemon process (host-global for the normal single-daemon deployment).
# [admission]
# max_concurrent_sessions = 0
# max_memory_gb = 0
# codex_memory_gb = 0.2
# claude_memory_gb = 0.85
# kimi_memory_gb = 0.5
# default_memory_gb = 0.5
`, paths.Database, paths.Logs, paths.Workspaces, paths.Evals, paths.ArtifactBlobs)
}
