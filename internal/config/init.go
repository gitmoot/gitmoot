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
# off | warn | block and defaults to warn when omitted. stale_task_ttl is the
# conservative updated_at age after which abandoned implementing tasks are
# branch-cleaned and blocked/awaiting_human_merge tasks are evidence-disposed
# to a terminal state; it defaults to 168h and "0" disables those reconcilers.
# delegation_worktree_ttl defaults to 72h and bounds terminal-job worktree
# retention; terminal owners cannot resume, so this safe cleanup is default-on.
# Set it to "0" to disable the TTL reclaim pass.
# planned_ttl is a separate destructive retirement policy for never-started
# plans. It is OPT-IN and disabled by default because dismissal can discard human
# planning context that a later goal-file import cannot reconstruct.
# require_workflow is true by default. In the default auto mode, fresh unlabeled
# agent dispatches are filed under adhoc/<agent>-<yyyy-mm-dd> and never
# rejected. Set require_workflow = false to opt out. To reject unlabeled
# dispatches, explicitly set require_workflow and set require_workflow_mode =
# "strict"; a mode-only override remains inert for pre-#1053 compatibility.
# Both can be overridden in a flat [repos."owner/repo"] section.
# [workflow]
# implement_base = "origin/main"
# result_checks = "warn"
# stale_task_ttl = "168h"
# auto_settle_after = "24h" # default 24h; 0 disables workflow auto-settle
# delegation_worktree_ttl = "72h"
# planned_ttl = "720h" # opt-in; unset/empty/0/invalid disables
# require_workflow = false # opt out (default is true)
# require_workflow_mode = "auto" # auto | strict

# [cache] gives isolated-worktree jobs (codex is always sandboxed; claude/kimi
# "produce" jobs are Landlock-sandboxed) a shared, host-level tool-cache
# directory instead of each job re-materializing its own uv/go/npm/pip caches
# inside its worktree. Enabled by default; the shared dir is created on first
# use and is never swept by worktree teardown.
# [cache]
# enabled = true
# dir = "/root/.gitmoot/cache/tools" # default: "<home>/cache/tools"

# [disk_guard] prevents normal agent dispatch when the filesystem holding the
# Gitmoot home/worktrees is below either configured free-space floor. It is
# enabled by default. When both floors are non-zero, both must pass (the more
# conservative floor wins). Set enabled=false only for an explicit opt-out.
# [disk_guard]
# enabled = true
# min_free_bytes = 2147483648 # 2 GiB
# min_free_percent = 5

# [daemon] is the OPTIONAL warm-reloadable runtime config (issue #577). CLI flags to
# "daemon start" / "daemon run" remain the initial value; a key here is applied only
# where the matching flag was NOT passed (flag = override). Its real purpose is WARM
# RELOAD: send the running daemon SIGHUP (kill -HUP <pid>) and it RE-READS this section
# and applies poll/workers/scheduler/idle cadence to the live supervisor WITHOUT a restart — a
# restart tears down in-flight supervision and re-inherits the launching shell's env,
# dropping runtime auth (#559). With no [daemon] section behavior is byte-identical.
# poll is a Go duration; workers is the worker-pool size (applied live — the pool is
# re-dispatched each tick); scheduler is barrier|pool. "parallel = N" is sugar for
# workers=N + scheduler=pool and conflicts with an explicit workers/scheduler here.
# [daemon]
# poll = "30s"
# workers = 1
# scheduler = "barrier"
# idle_grace_ticks = 3       # consecutive all-304 polls before decay starts
# idle_max_multiplier = 4   # 1 disables idle cadence decay
# job_timeout_default = "4h" # kill deadline when payload and agent type omit one
# job_timeout_max = "8h"     # hard ceiling; larger payload requests are clamped
# quiet_kill_after = "45m"   # liveness transcript-silence threshold; floor 5m
# permission_policy_observation_enabled = false # live-store #1484 ratchet; off by default

# [credentials] is OFF by default. When env_curation=true, only a pinned base
# environment plus runtime-specific auth/state variables reaches runtime-agent
# subprocesses. env_passthrough accepts exact names or one trailing-* glob.
# github=deny omits ambient GH_*/GITHUB_* values and gives gh a fresh empty config;
# github=inherit explicitly restores ambient GitHub environment inheritance.
# model_gateway is an opt-in daemon-owned loopback gateway for Claude credentials;
# its allowlist contains exact upstream hostnames and defaults to Anthropic only.
# [credentials]
# env_curation = false
# env_passthrough = [] # e.g. ["GOCACHE", "NPM_*"]
# github = "deny"
# model_gateway = false
# model_gateway_allow_hosts = ["api.anthropic.com"]
# keychain_path = "" # default: <base-home>/.config/gitmoot/keychain.env

# [remote_exec] selects the execution backend — WHERE a job's runtime
# subprocess executes (#1535 contract, #1536). "local" is the default and a
# byte-for-byte passthrough to the existing runner composition (subprocess
# GroupRunner innermost; credential-gateway and Landlock produce wrappers
# unchanged). "remote" runs shell implementation jobs in an E2B sandbox; all
# unsupported routes fail loudly instead of falling back to the host. With no
# [remote_exec] section behavior is byte-identical. Any other value FAILS THE
# JOB LOUDLY at dispatch naming the value and the allowed set. A job payload's
# exec_backend field overrides this per job. This is NOT the Landlock local
# confinement surface (internal/sandbox, the "sandbox" CLI, agent path
# grants), which is unrelated.
# [remote_exec]
# backend = "local"
# local_uid = 1000 # optional; configure together with non-root local_gid
# local_gid = 1000 # use a dedicated group when unrelated local users must be excluded
# local_root = "/var/tmp/gitmoot-local" # absolute; parents must be traversable by local_uid
# e2b_api_key_file = "/run/secrets/e2b-api-key" # required for remote; owner-only regular file or symlink to one
# e2b_template = "gitmoot-shell" # required for remote
# e2b_base_url = "https://api.e2b.app" # optional control-plane override
# e2b_domain = "e2b.app" # optional sandbox-domain override
# credential_gateway_listen = "0.0.0.0:8443" # pair: daemon bind address
# credential_gateway_url = "https://broker.example.com:8443" # pair: sandbox-reachable origin

# [transcripts] is ON by default. Raw unredacted runtime output
# is retained in 0600 per-job append logs for deterministic trajectory export.
# retain and max_total_bytes bound home-scoped garbage collection.
# [transcripts]
# enabled = true
# retain = "168h"
# max_total_bytes = 2147483648

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
# Emit one synthetic blocked event per continuously blocked task or Herdr org
# role after this Go duration. 0s keeps both evaluators disabled.
blocked_role_wake_after = "0s"
# Flag an org role in chart/status after this many consecutive stalled wakes.
# 0 keeps flagging disabled while the best-effort counter remains available.
max_consecutive_missed_wakes = 0
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

# [pipeline_remote] is the OPTIONAL default GitHub repo used by pipeline
# publish / pull when --remote is omitted (#941). repo is owner/repo; ref
# defaults to "main"; path defaults to "pipelines". Pipeline bundles include
# agent-template prompts verbatim, so keep the remote private unless those
# prompts are intentionally public.
[pipeline_remote]
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
# promotion path). distill_successes (default false) enables the #781 deterministic
# success producer: recovered-failure observations. It also stages only
# trust_mark=low pending observations. ingest_auto_confirm (default
# false) lets memory ingest immediately confirm into the authoring
# agent's private pool only; the shared pool is always explicit through confirm
# --to-shared or promote --to-shared. distill_max_per_job (default 3, >= 0) caps
# distilled rows per job; distill_all_jobs (default false) widens distill past
# enrolled agents to every job. default_enroll (default false) makes manual
# agent start enroll new config-safe agents unless --memory=false is explicit.
# harvest_enabled (default false) runs a durable one-minute post-terminal sweep:
# it stages bounded low-trust shared/repo observations for human review, never
# confirmed memory, and never honors ingest_auto_confirm.
# Daemon-consumed [memory] keys are hot-read without restart; default_enroll is
# read on each manual agent start.
# Inspect the store read-only with gitmoot memory list; see the "Agent Persistent
# Memory" concepts page and CLI.md for the full model.
# [memory]
# disabled = false
# default_enroll = false
# token_budget = 1500
# max_entries = 15
# distill_at_terminal = false
# distill_successes = false
# distill_max_per_job = 3
# distill_all_jobs = false
# ingest_auto_confirm = false
# harvest_enabled = false
# harvest_runtime = "codex"
# harvest_model = "" # empty uses the runtime default
# harvest_effort = "low"
# harvest_max_per_job = 2
# harvest_max_jobs_per_sweep = 5
# groom_split_llm = false
# groom_split_llm_runtime = "codex"
# groom_split_llm_model = "" # empty uses the runtime default
# groom_split_llm_max_per_run = 5
# groom_quality = false # shadow audit still runs; true permits corroborated retirements
# groom_quality_max_per_run = 8
# groom_quality_min_age = "24h"
# groom_llm_total_max_per_run = 10 # shared across quality, stale, and split calls
# groom_stale = true
# groom_stale_age = "336h" # 14d; Go duration syntax
# cluster_fanout = 12
# cluster_fanout_keep = 9
# cluster_depth_cap = 4
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
# or a bare integer read as whole seconds. conditional_requests uses in-memory ETags
# for the four daemon polling reads (default true). calls_per_hour_warn is a
# daemon-local approximate sliding-hour warning threshold; 0 disables it.
# [github]
# max_concurrent = 0
# min_interval = "0s"
# secondary_backoff = true
# backoff_base = "60s"
# backoff_max = "5m"
# conditional_requests = true # ETag polling; in-memory cache is cold after restart
# calls_per_hour_warn = 0      # off; daemon-local approximate count when enabled

# [runtimes.<name>] is the OPTIONAL config-driven runtime metadata registry
# (issue #652). Gitmoot ships built-in metadata for each compiled runtime (codex,
# claude, kimi, omp, shell) — capabilities, default model/effort, known models, and where
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

# [review] controls native PR review scheduling. Native fanout is disabled by
# default; request deliberate reviews with 'gitmoot agent review <agent>'.
# Set native_fanout_enabled = true to opt in globally. blocking_severity is the
# least severe changes-requested verdict that restarts the fix loop; P3 preserves
# the default where every valid changes-requested review blocks. P1 lets P2/P3
# findings remain posted while resolving the round as approved with notes.
# Repository overrides under [repos."owner/repo".review] win. Enabled fanout
# selects the runtime family of the first configured reviewer whose family resolves,
# then dispatches only reviewers in that family. Risk tiers remain independently
# opt-in and global.
# [review]
# native_fanout_enabled = false
# blocking_severity = "P3"
# risk_tiers_enabled = false
# high_risk_paths = ["**/auth/**", "cmd/**"]
# risk_label_high = "risk:high"
# risk_label_routine = "risk:routine"
#
# [repos."owner/repo".review]
# native_fanout_enabled = true
# blocking_severity = "P1"

# [merge_gate] controls native task merges. Native auto-merge is enabled by
# default, but only when an approved review verdict matches the exact current
# head SHA and all SHA-scoped commit statuses and check-runs are green. A missing
# review or CI signal leaves the PR open and escalates the specific miss from the
# most-specific live role whose scope matches the repo to its nearest live
# ancestor. With no live scope match it addresses a live owner; a root match
# addresses itself. With no live upward recipient, the gate fails closed without
# journaling an addressed note. Archive filtering does not predict delivery; org
# validate reports missing wake routes and pane bindings. Set auto_merge = false
# globally or per repo as an explicit
# kill-switch; that deliberate operator choice leaves PRs open without escalation.
# All keys can be set globally and overridden per repo under
# [repos."owner/repo".merge_gate].
# [merge_gate]
# auto_merge = true
# Legacy compatibility fields below no longer permit an empty CI gate to merge.
# require_external_ci = false
# min_ci_wait = "60s"
# max_ci_wait = "10m"

# [org] optionally registers organization roles for scoped local dispatch. Any
# [org.roles.*] entry turns enforcement on. One parent-less role is the org
# owner; child scopes must be subsets of their parent's scope. scope accepts
# "*" (all repos), "owner/*", or exact "owner/name". enforce is "block"
# (default) or "warn". Scope is checked at dispatch; merge_rule is advisory in
# this phase (owner | self | none).
# [org]
# enforce = "block"
# directive_ack_ttl = "10m"
# directive_done_ttl = "0s" # disabled; a per-directive override takes precedence
# directive_max_nudges = 3
# [org.roles."owner"]
# scope = ["*"]
# merge_rule = "owner"
# model = "gpt-5.6-sol"  # optional: pin the runtime model honored at org recycle
# [org.roles."maintainer"]
# parent = "owner"
# scope = ["owner/*"]
# merge_rule = "self"
`, paths.Database, paths.Logs, paths.Workspaces, paths.Evals, paths.ArtifactBlobs)
}
