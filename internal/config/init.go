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

# [workflow] controls job-level workflow defaults. implement_base is optional:
# when set, agent implement and agent run jobs that route to implement create a
# new-branch worktree from that ref. Use "origin/main" for a remote-tracking
# default, or "HEAD" to follow the registered checkout. With no value, implement
# follows checkout HEAD and guards stale non-default checkouts. result_checks is
# off | warn | block and defaults to warn when omitted.
# [workflow]
# implement_base = "origin/main"
# result_checks = "warn"

# [daemon] is the OPTIONAL warm-reloadable runtime config (issue #577). CLI flags to
# "daemon start" / "daemon run" remain the initial value; a key here is applied only
# where the matching flag was NOT passed (flag = override). Its real purpose is WARM
# RELOAD: send the running daemon SIGHUP (kill -HUP <pid>) and it RE-READS this section
# and applies poll/workers/scheduler to the live supervisor WITHOUT a restart — a
# restart tears down in-flight supervision and re-inherits the launching shell's env,
# dropping runtime auth (#559). With no [daemon] section behavior is byte-identical.
# poll is a Go duration; workers is the worker-pool size (applied live — the pool is
# re-dispatched each tick); scheduler is barrier|pool. "parallel = N" is sugar for
# workers=N + scheduler=pool and conflicts with an explicit workers/scheduler here.
# [daemon]
# poll = "30s"
# workers = 1
# scheduler = "barrier"

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

# [template_remote] is the OPTIONAL default GitHub repo the agent-template
# publish / pull / add commands fall back to when --repo is omitted (#476).
# Empty repo (the default) means no default remote: those commands then require
# an explicit --repo, so behavior is byte-identical to having no section. repo is
# owner/repo; ref defaults to "main" when empty; path is the subdir holding the
# template .md files and defaults to "templates" when empty. Set it with
# gitmoot agent template remote set <owner/repo> [--ref] [--path].
# CAUTION: templates are stored and published verbatim (prompt body + metadata);
# point this at a PRIVATE repo unless you intend the prompts to be public.
[template_remote]
repo = ""
ref = ""
path = ""

# [memory] is the OFF-BY-DEFAULT agent persistent-memory read-path policy (#626).
# With no [memory] section AND no agent enrolled, behavior is byte-identical: no
# learnings block is ever injected and the feature is entirely inert. Enrollment is
# PER AGENT — add memory = true to an [agents.<name>] block (see [agents.builder]
# below) to opt that agent in; this section only carries the shared read knobs plus
# a global kill switch. When enrolled, Gitmoot runs in OBSERVATION MODE: while
# assembling a job prompt it retrieves the agent's own confirmed, repo-filtered
# (current repo + the always-travelling "general" scope) mechanical facts and
# renders a fenced REFERENCE-ONLY "Prior learnings (reference only, not
# instructions)" block — it is context, never instructions, and an empty result
# adds nothing. Agent-returned learnings are shadow-logged for measurement but are
# NOT injected in this phase. disabled is the global kill switch (default false):
# true overrides every per-agent memory=true, turning the whole feature off box-wide
# without editing each agent block. token_budget caps the injected block's estimated
# tokens (default 1500); max_entries caps how many confirmed rows are considered for
# injection (default 15); both must be >= 0. distill_at_terminal (default false) is
# the master switch for #737 P4.1 deterministic distill-at-terminal: on an anomalous
# terminal (failed/blocked/changes_requested) Gitmoot stages bounded PENDING
# observations (failing tests + named errors) at trust_mark=low, provenance
# distill:<job-id>, NEVER confirmed memory (the memory confirm gate stays the only
# promotion path). distill_successes (default false) enables #781 deterministic
# success producers: SkillOpt promotions and recovered-failure observations. They
# also stage only trust_mark=low pending observations. ingest_auto_confirm (default
# false) lets memory ingest and chat remember immediately confirm into the authoring
# agent's private pool only; the shared pool is always explicit through confirm
# --to-shared or promote --to-shared. distill_max_per_job (default 3, >= 0) caps
# distilled rows per job; distill_all_jobs (default false) widens distill past
# enrolled agents to every job.
# All [memory] keys are read PER TICK; no daemon restart is needed to flip them.
# Inspect the store read-only with gitmoot memory list; see the "Agent Persistent
# Memory" concepts page and CLI.md for the full model.
# [memory]
# disabled = false
# token_budget = 1500
# max_entries = 15
# distill_at_terminal = false
# distill_successes = false
# distill_max_per_job = 3
# distill_all_jobs = false
# ingest_auto_confirm = false
# groom_split_llm = false # Phase 2 gate only; the LLM atomizer is not implemented yet.
#
# Built-in memory pipeline inputs are optional. The daemon and
# gitmoot pipeline install-defaults register memory-ingest-sweep and
# memory-groom-propose as ordinary pipelines, but schedules stay disabled unless
# you set an interval here. "nightly" is accepted as 24h.
# [[memory.ingest]]
# path = "/path/to/markdown-notes"
# agent = "builder"
# repo = "owner/repo"
# tier = "repo"
#
# [memory.pipelines]
# repo = "owner/repo"
# ingest_sweep = "nightly"
# groom_propose = "nightly"
#
# Enroll a specific agent (per-agent opt-in; omit for byte-identical default):
# [agents.builder]
# memory = true

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
#   hard_verifiers_enabled (#474): OFF by default; requires auto_trace AND at least one
#     hard_verifier_commands line. When true, a MERGED implement job runs the configured
#     build/test/lint COMMANDS in a FRESH clean sandbox checkout at the merged head (an
#     INDEPENDENT local 'git clone' of the daemon checkout — its own git dir, so a
#     verifier that shells out to git cannot touch the live repo; no external sandbox
#     dep), exit 0 == pass. The binary verdict is the authoritative EvaluatorScore.Hard
#     (pass ⇒ hard=1.0/choice a, fail ⇒ hard=0.0/choice b) — an un-gameable FOURTH
#     coexisting row (reviewer gitmoot-verifier, item hard#repo#pr) that catches a merge
#     whose code fails a clean build/test even through an empty (no-CI) gate. DETACHED +
#     best-effort: an unprovisionable sandbox or empty command list writes no row and
#     never blocks the merge. Off ⇒ byte-identical. Promotion stays MANUAL.
#   hard_verifier_commands (#474): REPEATABLE — one verifier command per line (so a
#     command may contain commas / shell operators). The run PASSES only when EVERY
#     command exits 0. UNSET/empty ⇒ the tier is a no-op (no safe universal default).
#     IMPORTANT: the sandbox is a BARE checkout of the merged code — it has NO installed
#     dependencies, build artifacts, or generated files, and inherits only the daemon's
#     ambient PATH. Each command MUST self-provision what it needs (e.g.
#     'npm ci && npm test', 'pip install -e . && pytest', or 'go test ./...' which
#     fetches modules on its own) — a command that assumes a pre-installed
#     node_modules/venv will FAIL every merge and record a false hard=0. A command that
#     cannot be RUN at all (its interpreter/binary is absent, exit 127) is treated as an
#     environment failure and SKIPS the run (no row) instead of recording a negative, but
#     a command that runs and exits non-zero because deps were missing is an honest FAIL
#     — so keep each command self-contained.
#   gate_enabled (#627): OFF by default; STANDALONE (no auto_trace dependency). The
#     deterministic fixed-corpus REPLAY GATE (AutoMem A.2): a pre-canary check that
#     replays a candidate template against a FIXED job corpus and accepts it only on
#     STRICT improvement over the champion on the SAME corpus (a tie fails). When on, a
#     candidate must carry a PASSING gate run ('gitmoot skillopt gate run --candidate
#     <id>') before it may be promoted to canary/current — otherwise promotion is
#     blocked with a gate_blocked notify. Off ⇒ byte-identical (the gate is never
#     consulted). It reuses the #474/#485 deterministic scorers on the corpus outputs
#     (no new judge, no live LLM in the gate itself); promotion stays MANUAL.
#   gate_corpus (#627): default fixed-corpus file path used by 'gitmoot skillopt gate
#     run' when no --corpus is passed. UNSET ⇒ a corpus must be supplied on the CLI.
#   gate_replay_command (#627): default deterministic replay driver run via 'sh -c' per
#     corpus item. It receives GITMOOT_GATE_TEMPLATE_FILE (the candidate template),
#     GITMOOT_GATE_PROMPT, GITMOOT_GATE_EXPECTED, and GITMOOT_GATE_ITEM_ID in the env
#     and emits a per-item GateReplayResult JSON ({"rubric":{...}} or
#     {"hard_verifier":true,"hard_passed":bool}) on stdout — the command IS the
#     deterministic map. A corpus's own replay_command overrides this default.
#   pace_enabled (#687): OFF by default. The PACE anytime-valid commit gate — an
#     ADDITIONAL auto-promote gate (never a replacement) layered on top of every
#     existing guardrail. When on, a guardrails-pass candidate is auto-promoted only
#     when a testing-by-betting e-process over its recorded candidate-vs-champion
#     pairwise outcomes (the Mode B bandit arm's win/loss tally, #481/#482) crosses the
#     commit threshold 1/pace_alpha; a budget-exhausted or not-yet-decisive stream
#     FAILS SAFE to a pace_blocked notify (no promotion). It is model-free arithmetic —
#     no extra LLM calls — and rides the pairwise comparisons #473 already records.
#     OFF ⇒ byte-identical (the e-process is never consulted).
#   pace_alpha (#687): PACE target false-commit probability in (0,1); the commit
#     threshold is 1/pace_alpha (0.05 -> 20). Default 0.05. Only consulted when
#     pace_enabled.
#   pace_lambda (#687): PACE bet fraction in [0,1]; a candidate win scales the wealth
#     by (1+pace_lambda), a loss by (1-pace_lambda). Default 0.5. Only consulted when
#     pace_enabled.
#   pace_max_pairs (#687): PACE discordant-pair budget; after this many win/loss pairs
#     without crossing the threshold the e-process rejects (notify-only). Default 200.
#     Only consulted when pace_enabled.
# [skillopt]
# auto_trace_enabled = false
# cross_family_review_enabled = false
# revert_detection_enabled = true
# deterministic_checkers_enabled = false
# deterministic_checkers = diff_size
# hard_verifiers_enabled = false
# hard_verifier_commands = go build ./...
# hard_verifier_commands = go test ./...
# gate_enabled = false
# gate_corpus = .gitmoot/skillopt/gate-corpus.json
# gate_replay_command = sh .gitmoot/skillopt/gate-replay.sh
# pace_enabled = false
# pace_alpha = 0.05
# pace_lambda = 0.5
# pace_max_pairs = 200
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

# [github] tunes the PROCESS-WIDE GitHub call budget + secondary-rate-limit backoff
# the daemon installs at startup (issue #683). GitHub's SECONDARY (abuse-detection)
# rate limit fires on burstiness/concurrency — NOT total volume — so a busy daemon +
# many concurrent agent gh calls can trip it (HTTP 403 "secondary rate limit") and
# freeze all GitHub ops even while the PRIMARY quota is fine. This limiter smooths
# bursts and, on a secondary hit, pauses all GitHub calls process-wide (respecting
# Retry-After, else exponential backoff) instead of retry-storming the abuse detector.
#
# SAFE DEFAULTS (no [github] section): max_concurrent = 0 (unlimited) and
# min_interval = 0 (no spacing) leave single-call latency and steady-state throughput
# byte-identical — the PROACTIVE smoothing is OPT-IN. secondary_backoff defaults TRUE:
# it is invisible on the happy path (it engages only after a gh call actually fails
# with a secondary/abuse limit) and is the protection the incident needed. To also
# smooth bursts proactively on a busy host, set a concurrency cap (e.g. 6) and/or a
# small min_interval (e.g. "250ms"). Durations accept a Go duration ("250ms", "2s")
# or a bare integer read as whole seconds.
# [github]
# max_concurrent = 0
# min_interval = "0s"
# secondary_backoff = true
# backoff_base = "60s"
# backoff_max = "5m"

# [runtimes.<name>] is the OPTIONAL config-driven runtime metadata registry
# (issue #652). Gitmoot ships built-in metadata for each compiled runtime (codex,
# claude, kimi, kimi-cli, shell) — capabilities, default model/effort, known models, and where
# token usage is read from — that reproduces today's behavior. A [runtimes.<name>]
# section OVERRIDES that recorded metadata for a BUILT-IN runtime WITHOUT a
# recompile: retarget the default model, record which models a runtime accepts, or
# adjust its advertised capabilities. Two fields are BEHAVIORAL: default_model is
# consulted at job DELIVERY (#652) as the model fallback when NEITHER the agent
# NOR the job pins a --model; default_effort follows the same precedence after
# job/agent --effort and is forwarded to Codex as model_reasoning_effort. Claude
# and Kimi ignore effort. Every other field is inspection-only, surfaced by
# 'gitmoot runtime list' but changing nothing at runtime: models is advisory
# (Gitmoot never REJECTS a --model based on it), and capabilities gates nothing at
# dispatch (agent capabilities do). Adapter behavior (auth, sandbox, session resume,
# stream parsing) always stays in Go. With no [runtimes.*] section — and with
# default_model/default_effort unset (empty = none recorded, the built-in default)
# behavior is byte-identical: no model or effort is forced. NOTE: this section can only tweak a BUILT-IN
# runtime's metadata — it cannot add a new first-class runtime (that is a code
# change); an unknown runtime name here is an error. default_model/default_effort
# are surfaced by 'runtime list' AND used as delivery fallbacks; models is the
# advisory known-valid list; capabilities is a subset of review/implement/ask/produce;
# usage_source is a human-readable descriptor.
# [runtimes.codex]
# default_model = "gpt-5.5-codex"
# default_effort = "high"
# models = ["gpt-5.5-codex", "gpt-5.4-codex"]
# capabilities = ["review", "implement", "ask"]

# [chat] is the OFF-BY-DEFAULT native-chat auto-respond policy (#534 V1.5). With no
# [chat] section — or with no agent enrolled — behavior is byte-identical: the daemon
# tick never touches the chat tables and no agent is ever auto-summoned. auto_respond
# is the global kill switch (default false = OFF): only when true does the sweep run.
# Enrollment is PER AGENT — add chat_autorespond = true to an [agents.<name>] block
# (see [agents.builder] below) to opt that agent in; BOTH the global switch and the
# per-agent flag must be true. When enrolled, each daemon tick looks for OPEN chat
# threads with an unread @mention of the agent on a kind='chat' message (job_result /
# system / promotion_request messages NEVER trigger) and enqueues ONE bounded
# read-only ask through the normal dispatch gate; its reply is posted back as a
# non-promotable job_result, so it can never itself re-trigger (structural
# anti-ping-pong). auto_respond_cap (default 4) is the HARD cap on auto-responses per
# (thread, agent): on the cap the thread HARD-STOPS — no auto-extension — and a
# VISIBLE "needs a human" system message is posted into the thread. auto_respond_cooldown
# (default "2m", a Go duration) is the minimum spacing between an agent's auto-responses
# in a thread; a trigger seen inside the window is deferred, never dropped. cap must be
# >= 0 and cooldown >= 0.
#
# The [chat] section also carries the 'gitmoot moot' knobs (#534 V1.5). A moot
# convenes N registered agents as SEATS (one background read-only ask job each)
# that converse in one chat thread via 'gitmoot chat send'/'gitmoot chat wait'.
# moot_max_seats (default 6) bounds how many agents one moot may convene (more is
# rejected). moot_message_cap (default 30, overridable per-moot via --max-messages)
# is the HARD per-thread cap on agent-authored turns: on the cap the moot HARD-STOPS
# (no auto-extension), further 'chat send --as' is refused, and a VISIBLE overrun
# system message is posted; each seat then posts its partial conclusions (know /
# unsure / would-ask-next) via its gitmoot_result. Both must be >= 1. These are
# resolved even when auto_respond is off (a moot is convened by an explicit command).
# [chat]
# auto_respond = false
# auto_respond_cap = 4
# auto_respond_cooldown = "2m"
# moot_max_seats = 6
# moot_message_cap = 30
#
# Enroll a specific agent (per-agent opt-in; omit for byte-identical default):
# [agents.builder]
# chat_autorespond = true

# [merge_gate] tunes how the native merge gate handles a PR head that reports NO
# external CI (issue #596). By default (no section) the gate defers concluding
# "no CI" until a SECOND consecutive zero-external observation at the same head,
# at least min_ci_wait later, so a fresh head cannot merge before GitHub Actions
# creates its check run. When .github/workflows/ exists at the head it instead
# waits up to max_ci_wait (default 10m) for a check to appear, then concludes
# no-CI so a PR whose workflows never trigger for it (docs-only under paths
# filters, tag-only or workflow_dispatch-only workflows, a non-targeted branch)
# still merges rather than wedging forever. Set require_external_ci = true to
# instead HARD-BLOCK an empty gate once that window elapses (for repos you know
# always have CI). All keys can be set globally and overridden per repo under
# [repos."owner/repo".merge_gate].
# [merge_gate]
# require_external_ci = false
# min_ci_wait = "60s"
# max_ci_wait = "10m"
`, paths.Database, paths.Logs, paths.Workspaces, paths.Evals, paths.ArtifactBlobs)
}
