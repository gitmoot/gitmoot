# Gitmoot CLI Reference

Use these commands from an agent session only when the user asks for Gitmoot
setup, status, agent coordination, or PR-comment workflow help.

## Install And Update

```sh
curl -fsSL https://gitmoot.io/install.sh | sh
gitmoot version
gitmoot update --check
gitmoot update --restart-daemon
gitmoot doctor [--repo <path>]
```

Verify GitHub access before PR workflows:

```sh
gh auth status
```

`gitmoot doctor` is the environment preflight: it validates `gh auth` (with an
actionable remediation hint) and the runtime credentials — including a
daemon-aware live probe of the Claude token — so a bad credential is caught
before jobs stall. Run it after install and before starting the daemon.

One-shot onboarding: `gitmoot setup` registers the repo and an agent in one
command (`--repo owner/repo --agent <name> --runtime codex|claude|shell
--session <ref> [--role <role>] [--path .] [--start-daemon]`). `--repo`,
`--agent`, `--runtime`, and `--session` are all **required** — setup errors out
if any is missing; `--session` takes a runtime session reference, `last`, or a
shell command.
`--watch-issues` is **on by default** in setup, so the daemon comes up
tagging-ready for `@<agent>` issue mentions.

Home and config: local state lives in the Gitmoot home (default `~/.gitmoot`) —
the SQLite store, `logs/`, `workspaces/`, `evals/`, `artifact_blobs/`, and
`config.toml`. `gitmoot init` creates it, `gitmoot config path` prints the
config file location, and `gitmoot config show` prints the effective config.
Set `GITMOOT_HOME` (or pass the global `--home <path>` flag, accepted by nearly
every command) to relocate everything — useful for isolated test homes.

## Runtime Plugins

Install Gitmoot's Agent Skill into Codex or Claude Code when the user wants the
runtime to discover Gitmoot workflow guidance from its plugin system:

```sh
gitmoot plugin install codex
gitmoot plugin install claude
gitmoot plugin doctor
```

Inspect or build packages without installing:

```sh
gitmoot plugin build codex
gitmoot plugin build claude
gitmoot plugin path codex
gitmoot plugin path claude
gitmoot plugin doctor codex
gitmoot plugin doctor claude
gitmoot plugin codex-launch --repo .
gitmoot plugin codex-launch --config-snippet
```

Claude scopes are supported with `--scope user|project|local`. Codex ignores
`--scope` because the current Codex plugin install command does not use it.
Use `plugin codex-launch` when Codex needs sandbox access to the resolved
Gitmoot home on Linux, macOS, or Windows. It prints a `codex-face --cd ...
--add-dir ... -s workspace-write` launch command, or a persistent config
snippet with `--config-snippet`.

## Runtime Metadata Registry

Gitmoot drives four built-in runtimes (`codex`, `claude`, `kimi`, plus the
subscribe-only `shell`; the legacy `kimi-cli` is also compiled in). Each carries
declarative metadata — advertised capabilities, a default model, an advisory list
of known-valid models, and a descriptor of where token usage is read from. Inspect
the resolved registry:

```sh
gitmoot runtime list
gitmoot runtime list --json
```

The values come from the compiled built-in defaults, overlaid with any
`[runtimes.<name>]` overrides in the config file. Override a built-in runtime's
recorded metadata **without recompiling** — for example to retarget its default
model or record its known models:

```toml
[runtimes.codex]
default_model = "gpt-5.5-codex"
models = ["gpt-5.5-codex", "gpt-5.4-codex"]
capabilities = ["review", "implement", "ask"]
usage_source = "codex exec --json turn.completed usage"
```

Exactly one field is **behavioral**: `default_model` is consulted at job **delivery**
as the model fallback when **neither the agent nor the job pins a `--model`** — so
setting it **does** retarget the model those jobs run on. The resolution order is:
the agent/job `--model` win, then this `default_model`, then the runtime CLI's own
default. Every other field is **inspection-only**, surfaced by `gitmoot runtime list`
but changing nothing at runtime: `models` is **advisory** (Gitmoot never rejects a
`--model` based on it), and `capabilities` gates nothing at dispatch. Adapter
behavior (auth, sandbox policy, session resume, stream parsing) always stays in Go.
With no `[runtimes.*]` section — and with `default_model` unset — behavior is
byte-identical: no model is forced.

A `[runtimes.<name>]` section can only tweak a **built-in** runtime's metadata; it
cannot add a new first-class runtime (that requires a code change). An unknown
runtime name is a config error surfaced by `gitmoot runtime list`.

## Repo And Daemon Status

```sh
gitmoot status --repo owner/repo
gitmoot events --repo owner/repo
gitmoot repo add owner/repo --path <path> [--poll 30s]
gitmoot repo list
gitmoot repo remove owner/repo
gitmoot repo doctor owner/repo
gitmoot daemon start --poll 30s --workers 1
gitmoot daemon start --session <root-job-id>
gitmoot daemon start
gitmoot daemon status
gitmoot daemon logs
gitmoot daemon restart
gitmoot daemon stop [--forget-runtime-auth]
```

For structured local state, use `gitmoot dashboard --json` or
`gitmoot task list --repo owner/repo --json`. `gitmoot status --json` and
`gitmoot task show` are not valid commands.

The `gitmoot repo` commands manage the **watched-repo registry**: one daemon
per Gitmoot home supervises every **enabled** registered repo (each with its
own `--poll` interval). `repo doctor owner/repo` checks a single repo's
checkout/config health.

Use `daemon start` for the background daemon. Use `daemon run` only when the
user explicitly wants a foreground process. Keep the default `--workers 1`
unless the Gitmoot home has multiple independent runtime sessions or managed
agent types with `max_background` greater than one.

`daemon start --repo owner/repo` **scopes** the daemon to a single repo: it
polls only that repo's PRs and claims only that repo's queued jobs. Omit
`--repo` to supervise every enabled registered repo from one daemon (#581). Do
not start one daemon per repo on the same home expecting parallel isolation: a
second daemon on the same home is **refused** (`daemon already running with pid
…`; a stale pidfile from a dead owner is liveness-checked, so restarts work
cleanly). To cap one repo's parallelism on a shared (no-`--repo`) daemon, use
the per-repo config keys below instead.

Both `daemon run` and `daemon start` accept `--session <root-job-id>` (alias
`--root`) to pin the worker to one orchestration run. With `--session` set, the
worker runs only jobs whose `root_job_id` matches that value plus the root
coordinator job itself, and ignores every other queued job. Leaving it empty
keeps the default behavior of matching all jobs.

Both `daemon run` and `daemon start` also accept three opt-in flags (all off by
default, so leaving them unset is byte-identical to before): `--watch-issues`
watches open **issues** for `@<agent> ask …` mentions and routes them to jobs,
mirroring the PR-comment watcher; `--scheduler pool` selects the continuous
worker-pool scheduler that re-queries the queue as workers free and auto-isolates
a contended same-repo read job into an ephemeral worktree (fixing a same-repo
dependent-job deadlock), versus the default `--scheduler barrier`;
`--watch-skillopt-reviews` polls watched SkillOpt review issue comments and
imports valid feedback automatically (see the SkillOpt Exchange section).

To run a repo's queued jobs N-wide, use `--parallel N` (sugar for `--workers N
--scheduler pool`; it cannot be combined with `--workers` or `--scheduler`).
Raising `--workers` above 1 without an explicit `--scheduler` now **auto-selects
`pool`** (multiple workers under `barrier` serialize same-repo jobs anyway); an
explicit `--scheduler barrier` is still honored. `gitmoot daemon status` reports
the live scheduler mode and worker count (e.g. `scheduler: pool, workers: 5`), and
the daemon logs a preflight warning — with the exact relaunch command — when ≥2
parallelizable jobs are queued under a serializing config. Same-repo parallelism
is bounded by **distinct runtime sessions** as well as distinct checkouts.

One repo's concurrency can also be capped **from config, without any relaunch**
(#576): a `[repos."owner/repo"]` section with `max_parallel = N` caps that
repo's in-flight jobs (`0` or unset = use the global worker count), and an
optional `scheduler = "pool"|"barrier"` overrides that repo's scheduler. The
keys are re-read every tick, so edits apply live.

Reconfigure the running daemon without a restart: `kill -HUP <daemon-pid>`
re-reads the `[daemon]` config section (`poll`, `workers`, `scheduler`,
parallelism) live (#577) — no teardown, no dropped jobs, no environment
re-inheritance. Values pinned by explicit launch flags win over the re-read
config. Prefer SIGHUP over a restart when only tuning throughput.

Runtime auth across restarts (#578/#588): the daemon persists its Claude token
into an owner-only (0600) `daemon-runtime.env` file in the Gitmoot home.
`gitmoot daemon restart` recovers that token even when the invoking shell lacks
`CLAUDE_CODE_OAUTH_TOKEN`; a plain `daemon stop` + `daemon start` does **not**
recover it (start re-inherits the launching shell's environment). A recovered
token may be stale — verify with `gitmoot doctor`. A (re)start that would come
up without Claude auth warns loudly on stderr (non-fatal). `gitmoot daemon stop
--forget-runtime-auth` deletes the persisted file so a later restart cannot
recover the token.

An opt-in, off-by-default `[admission]` config section adds a host-global
concurrency budget the daemon applies **before** starting each agent session,
on top of `--workers`/pool and the per-repo locks (#365):
`max_concurrent_sessions` caps total in-flight sessions across all repos, and
`max_memory_gb` caps the summed per-runtime RAM estimate (tunable priors:
`codex_memory_gb`, `claude_memory_gb`, `kimi_memory_gb`, `default_memory_gb`).
With both caps `0` (the default) it is disabled. A job that does not fit is
left **queued** and retried next tick — never failed — so on a small host
"jobs stay queued" can mean the admission budget is holding them. The budget is
enforced per daemon process.

A `[github]` config section installs a **GitHub call budget + secondary-rate-limit
backoff** over the `gh`/API calls gitmoot issues **from the daemon process** —
polling, comments, merges, status (#683). It is enforced **per daemon process**
(like the admission budget), so it does not reach separate foreground processes (a
foreground `gitmoot orchestrate`/`pool`/`review`/`pr comment`) or the `gh` calls a
codex/claude runtime subprocess makes on its own. GitHub's **secondary**
(abuse-detection) rate limit fires on burstiness/concurrency — not total volume —
so a busy daemon plus concurrent in-process calls can trip it (HTTP 403 "secondary
rate limit") and freeze all GitHub ops even while the primary quota is fine. The limiter smooths bursts and, on a
secondary hit, **pauses all GitHub calls process-wide** (respecting `Retry-After`,
else exponential backoff) instead of retry-storming the abuse detector. Knobs:
`max_concurrent` caps in-flight `gh` calls (0 = unlimited, the default),
`min_interval` spaces successive call starts (0 = off; accepts a Go duration or a
bare integer of seconds), `secondary_backoff` toggles the reactive pause (default
`true`), and `backoff_base`/`backoff_max` bound the exponential fallback
(defaults `60s`/`5m`). **Safe defaults:** the proactive caps are off (single-call
latency and steady-state throughput unchanged) and only the invisible reactive
backoff is on. Set `max_concurrent` (e.g. `6`) and/or a small `min_interval`
(e.g. `250ms`) to also smooth bursts proactively on a busy host. Calls are never
dropped — they queue/delay. `gitmoot daemon status` shows the configured budget
(`github limiter: max_concurrent=… min_interval=… secondary_backoff=…`).

`gitmoot dashboard` shows local state — daemon health, repos, agents and runtime
sessions, jobs by state, worktrees, branch locks, SkillOpt train phase/candidate,
and pending interactive prompts.

On a real terminal (stdin and stdout both a TTY) and with no other output/mutation
flag, `gitmoot dashboard` launches an **interactive TUI**: a sidebar of pages
(Attention, Activity, Trains, Agents, Workers, Jobs, Locks, Health, Config)
that auto-refreshes.
Navigate with `tab`/`shift+tab` or `←/→`; `↑/↓` selects a row; `?` opens a
per-page key reference; `r` refreshes, `q` quits. The TUI is the cockpit — every
page can act, and each action runs the same store/workflow code as its CLI
equivalent:

- **Attention** lists pending prompts (`a` answer inline, `d` dismiss) and the
  actual blocked/failed/cancelled jobs with their latest event message
  (`enter` detail, `R` retry, `B` report bug). A red banner appears when the
  daemon is stopped; `s` restarts it with its previously persisted flags.
- **Activity** shows the **live orchestras**: each active delegation root with
  its children, so "what are my agents working on right now" is answered at a
  glance; `enter` opens the request/result detail of a root or a specific
  delegate.
- **Trains** lists every train session: `enter` opens ANY session's live phase
  view (not just the newest), `s` stops a live session (a reason is required;
  same path as `skillopt train stop`), `d` deletes a finished session and its
  history — and, if gitmoot created GitHub repos for that session, a second
  confirm offers to delete those too (never offered for repos gitmoot did not
  create; a missing `delete_repo` token scope shows its `gh auth refresh`
  remedy and can be retried in place).
- **Agents** lists registered agents: `enter` opens a detail with the template,
  recent jobs, and the template's version history — in the detail `↑/↓` selects a
  recent job (then a version) and `enter` opens it: a recent job opens that job's
  detail (`esc` returns), and the list scrolls when an agent has many jobs; `n`
  registers a new agent
  (name, codex/claude/kimi runtime, installed template); `o` starts a training
  session for the agent's template via a pre-filled form (review/workspace
  repos, request, codex/claude backend, optional model — the backend/model are
  stored in the session's optimizer defaults so `train continue` inherits
  them), then drops into the live phase view; `D` deletes the agent (refused
  while jobs reference it); `v` in the detail reverts the template to a
  previous version (same as `gitmoot agent template revert`).
- **Jobs** lists every job with a state summary: `enter` shows the event
  history, `R` retries failed/blocked/cancelled jobs (same path as
  `gitmoot job retry`), `c` cancels queued, running, AND blocked jobs (same as
  `gitmoot job cancel`; running ones show `cancelling…` until the daemon
  settles them), and `B` opens a redacted bug-report preview for
  failed/blocked/cancelled jobs. In the preview, `g` creates or reuses the
  GitHub issue and keeps the issue URL visible.
- **Locks** explains and lists locks, stale resource locks first in red (the
  owning process died; a running daemon reclaims them automatically); active
  locks collapse to a count. Branch locks are released with
  `gitmoot lock release owner/repo <branch> --owner <agent>`.
- **Workers** lists runtime workers (agent sessions). **Health** shows the
  daemon block (running state, persisted flags, log error tail) plus
  environment checks. **Config** renders the effective config with inline
  edits for scalar fields (or `$EDITOR` for the full file).

Form questions are also published as interactive prompt records, so an agent can
answer them with `gitmoot interactive answer` while the form is open. Inline
answers/dismissals use the same store APIs as `gitmoot interactive answer` /
`clear`.

Everywhere else — pipes, redirects, CI, `--plain`, `--json`, `--all`, `--watch`,
`--answer`, `--dismiss` — it prints the one-shot snapshot instead, unchanged. Set
`GITMOOT_NO_TUI=1` or `TERM=dumb` to force the non-interactive path globally.

```sh
gitmoot dashboard                  # interactive TUI on a terminal
gitmoot dashboard --plain          # one-shot snapshot on a terminal
gitmoot dashboard --json
gitmoot dashboard --all
gitmoot dashboard --answer <prompt-id> --value <value>
gitmoot dashboard --dismiss <prompt-id>
gitmoot dashboard --watch          # plain redraw until Ctrl-C (terminal only)
gitmoot dashboard --watch --interval 2s
gitmoot dashboard --web [--addr 127.0.0.1:8080]
```

`gitmoot dashboard --web` serves the **read-only web dashboard** (a live
orchestration/delegation graph with run summaries and prompt/output inspection)
until interrupted; `--addr` sets the listen address (default
`127.0.0.1:8080`). Use it when the user wants a browser view of a running
orchestration.

In the one-shot styled output the dashboard leads with a "needs attention" block,
colors and truncates long lists, and groups near-identical runtime sessions;
`--all` shows everything. `--watch` redraws on an interval (default 5s) and cannot
be combined with `--json`, `--answer`, or `--dismiss`.

## Event Stream (Webhooks)

To notify an external system when jobs finish, configure the off-by-default
`[events]` section in the Gitmoot config — it is **not** in the generated
default config, so this documentation is its discovery surface:

```toml
[events]
webhook_url = "https://example.com/gitmoot-events"  # empty (default) = OFF
timeout = "2s"                                       # per-POST timeout
# socket_path = ""                                   # reserved, unused today
```

With `webhook_url` set, Gitmoot POSTs a small, versioned (`schema_version = 1`),
redacted JSON event to that endpoint for: `job.finished`, `job.failed`,
`job.blocked`, `job.needs_attention` (an `escalate_human` pause),
`job.deferred`, and the SkillOpt candidate events
(`candidate.awaiting_promotion`, `candidate.auto_promoted`,
`candidate.canary_started`, `candidate.rolled_back`). Delivery is best-effort
(bounded buffer + timeout; drops are recorded as a local `event_sink_drop` job
event, never blocking the job). Consumer rule: **treat a `job.failed` as final
only when it is NOT immediately followed by a `job.deferred` for the same job
id.** Since #532 slice E a deferred run emits `job.deferred` as a first-class
transition **instead of** `job.failed` (no preceding `job.failed`); the rule
still holds and is forward-compatible with the older `job.failed`→`job.deferred`
flap. See `docs/events.md` for the full contract.

## Risk-Tiered Adaptive Review

Scale review depth to a change's blast radius via the off-by-default `[review]`
config section — it is **not** in the generated default config, so this is its
discovery surface:

```toml
[review]
risk_tiers_enabled = true                     # empty/false (default) = OFF
# high_risk_paths matched against the PR's changed files (** = any path depth):
high_risk_paths = ["**/auth/**", "**/security/**", "**/payment/**", "**/migration/**", "go.mod"]
risk_label_high = "risk:high"                 # PR label that forces the high tier
risk_label_routine = "risk:routine"           # PR label that forces the routine tier
```

With `risk_tiers_enabled = true`, each opened PR is classified — **explicit PR
label > changed-path glob match > default routine** (a `risk:high`/`risk:routine`
label wins over paths; a high label wins a label tie). A `routine` PR keeps the
unchanged single-reviewer fan-out. A `high` PR instead fans out a delegation
batch of **refutation-framed lens reviewers** (correctness, security, and — with
≥3 configured reviewers — regression), each prompted to *disprove* the change and
return structured findings `{lens, refuted, severity, confidence, evidence}` in
`gitmoot_result.findings`. The lenses are synthesized by the existing delegation
`synthesis_rule = quorum` engine: **any critical-severity refutation (a `blocked`
lens decision) fails the quorum and blocks the merge**; unanimous approval
satisfies it. The resolved tier is recorded as a `risk_tier_resolved` job event so
an escalation is explainable in the report/dashboard. With the section absent or
`risk_tiers_enabled` off, PR review is byte-identical to the single-reviewer path.
The competition tier (two implementations + a judge) is a planned follow-up.

## Bug Reports

Use `gitmoot report bug` to build a redacted GitHub-ready issue from local
Gitmoot error state. Job reports are fully supported; daemon, dashboard, and
train selectors are reserved and return clear unsupported-source errors until
their source collectors are implemented.

```sh
gitmoot report bug --job <job-id> [--preview]
gitmoot report bug --job <job-id> --create --yes
gitmoot report bug --source daemon --preview
gitmoot report bug --source dashboard --preview
gitmoot report bug --train <session-id> --create --yes
```

Default behavior is preview. Agents should run preview first, show or summarize
the redacted draft, and create an issue only when the user explicitly asks or
the active workflow policy already permits filing reports. Non-interactive
creation requires `--create --yes`.

Created reports target `jerryfane/gitmoot`, include the labels
`gitmoot-dashboard-report` and `bug`, and carry a fingerprint marker in the
body so duplicate open issues can be reused instead of creating another report.
If duplicate search fails in the CLI path, Gitmoot prints a warning and still
creates the issue; dashboard creates fail closed and keep the preview open so
the user can retry.

After creation, report the printed issue URL back to the user. If Gitmoot says
an existing issue was found, report that URL instead of presenting it as a new
issue.

## Agent Setup

Start a new runtime session managed by Gitmoot:

```sh
gitmoot agent start reviewer \
  --runtime codex \
  --repo owner/repo \
  --path . \
  --role reviewer \
  --capability ask \
  --capability review \
  --model gpt-5-codex \
  --start-daemon
```

`--runtime` accepts `codex`, `claude`, `kimi`, or `kimi-cli`. `kimi` is the
current Kimi Code CLI (the default choice); `kimi-cli` is the opt-in legacy
Kimi CLI adapter (#546) — the two are the same runtime *family* for
cross-family review purposes. For either, run `kimi login` first and restart
the Gitmoot daemon so it inherits the session. `agent subscribe` additionally
accepts `--runtime shell`, the deterministic no-LLM adapter whose `--session`
is a **command** (the job prompt arrives as `$1`; stdout must carry the
`gitmoot_result` envelope) — the workhorse for deterministic E2E tests.

`agent start`, `agent subscribe`, and `agent type set` accept an optional
`--model <name>` flag that sets the agent's default runtime model. It is a
free-form, runtime-scoped string (a Codex, Claude Code, or Kimi Code model name)
with no allow-list; both `--model X` and `--model=X` are accepted. A per-job
`--model` (or a delegation's `model` field) overrides this default, and an
omitted model preserves the runtime's own default. The same default can be set
in config under `[agents.<type>].model`.

`agent start` and `agent subscribe` accept `--policy` (default `auto`). The policy
maps to the runtime permission mode and decides what a headless job may do:

| `--policy` | Claude `--permission-mode` | Headless capability |
|---|---|---|
| `read-only` | `plan` | inspect/report only, no writes |
| `workspace-write` | `acceptEdits` | file edits only — does NOT unblock Bash (`go`/`git`/`gh`) |
| `danger-full-access` | `bypassPermissions` | full implementation: file writes plus Bash |
| `auto` (default) | *(no flag)* | non-deterministic — inherited from ambient Claude config |

Because of this, an agent that carries the `implement` capability **must** be
started/subscribed with a write policy. Gitmoot fails closed: `--capability
implement` with `auto`/empty or `read-only` is refused at `agent start`, `agent
subscribe`, and at implement-job dispatch with an actionable message. Set
`--policy danger-full-access` for full headless implementation (file writes plus
`go`/`git`/`gh`), or `--policy workspace-write` for edits-only (Bash stays
blocked). See `references/SAFETY.md` for the full mapping and rationale.

`agent subscribe` accepts `--preset-delivery full|referenced|auto` (default
`full`) and `agent update <name> --preset-delivery <mode>` flips it in place on
an already-registered agent. The mode is a sticky per-agent preference:
re-running `agent subscribe` on an existing agent WITHOUT `--preset-delivery`
(e.g. to refresh its session/repo) preserves the stored mode; only brand-new
agents default to `full`. It controls how the agent's installed preset
(template) prompt is delivered on each job:

| mode | behavior |
|---|---|
| `full` (default) | always inline the full preset prompt every job — the pre-existing behavior, byte-identical |
| `referenced` | send a short "use your installed `<preset>` preset (commit `<c>`)" reference INSTEAD of the whole body, but only when Gitmoot has recorded that the exact resumed session already loaded the same preset at the same commit; any doubt (new/`last`/fresh session, unknown session, changed commit) falls back to full |
| `auto` | like `referenced`, and ADDITIONALLY only when the runtime persists sessions (`codex`/`claude`); `shell`/`kimi`/custom always send full |

The optimization is correctness-first and additive: the job payload **always**
snapshots the exact preset id, resolved commit, and content regardless of mode
(so auditability and retry determinism are unchanged), and a preset commit change
invalidates the recorded loaded-state so the next job re-sends the full preset.
Leave it at `full` unless you are resuming a stable persisted session repeatedly
and want to save preset tokens.

Subscribe an existing runtime session:

```sh
gitmoot agent subscribe reviewer \
  --runtime codex \
  --session <session-id-or-last> \
  --repo owner/repo \
  --role reviewer \
  --capability ask \
  --capability review \
  --model gpt-5-codex

# Deterministic shell runtime: the session is a command, not a session id.
gitmoot agent subscribe stub-agent \
  --runtime shell \
  --session '/path/to/answer.sh' \
  --repo owner/repo \
  --role agent \
  --capability ask
```

Inspect and manage agents:

```sh
gitmoot agent list
gitmoot agent show reviewer
gitmoot agent show reviewer --json
gitmoot agent repos reviewer
gitmoot agent doctor reviewer
gitmoot agent restart reviewer
gitmoot agent remove reviewer
```

`gitmoot agent restart <name>` abandons the agent's runtime session and binds a
fresh one **in place** — the fix for a dead or stranded session that would
otherwise tempt a re-register. It refuses while the session is live or the
agent has in-flight jobs (finish or cancel those first). `gitmoot agent remove
<name>` unregisters the agent.

Delegate to a registered agent from the current local chat:

```sh
gitmoot agent run project-planner --repo owner/repo "Return the plan status."
gitmoot agent run lead --repo owner/repo --task task-001 --background "Implement this task."
gitmoot agent run reviewer --repo owner/repo --pr 12 --background "Review this PR."
gitmoot agent review reviewer --repo owner/repo --pr 12 "Review this PR."
gitmoot agent implement lead --repo owner/repo --task task-001 "Implement this task."
gitmoot agent ask project-planner --repo owner/repo "Return the plan status."
gitmoot agent ask project-planner --repo owner/repo --background "Write the implementation plan and goal file."
gitmoot agent run lead --repo owner/repo --model gpt-5-codex "Implement this task."
gitmoot job watch <job-id>
```

`gitmoot agent run`, `ask`, `implement`, and `review` (and `orchestrate`) accept
an optional `--model <name>` flag that pins the runtime model for that one job,
overriding the agent's configured default. It is a free-form, runtime-scoped
string (a Codex, Claude Code, or Kimi Code model name) with no allow-list; an
omitted `--model` leaves the agent's default model in effect. Both `--model X`
and `--model=X` are accepted.

The same commands accept an optional per-job `--runtime
codex|claude|kimi|kimi-cli|shell` override: that ONE job runs through the named
runtime while the agent's registered default runtime stays untouched (`agent
show` is unchanged afterwards). An overridden job never resumes — and never
writes back to — the agent's default-runtime session: it runs on a fresh
session of the override runtime, or on an explicit `--session <ref>` (a
Codex/Claude session id, a Kimi session id, or — required for `shell` — a
command; `last` is rejected because it resumes whichever session is most
recent rather than a concrete one), and its runtime-session lock names the
override runtime so it cannot collide with the default session's lock. Model
rule: `--model` combined with `--runtime` is interpreted for the OVERRIDE
runtime; an override without `--model` uses the override runtime's default
model — the agent's configured default model is never applied to a different
runtime. An unknown
`--runtime` fails before any job is enqueued, background (daemon) jobs honor
the override identically to foreground, and a coordinator's delegation-tree
continuations (synthesis, corrective, replan, finalize) inherit the override,
so an `orchestrate --runtime` tree stays on the override runtime across
generations:

```sh
# Retry a hard review through Claude without re-registering the reviewer:
gitmoot agent review reviewer --repo owner/repo --pr 123 "Re-review this PR." --runtime claude
gitmoot agent ask reviewer "Compare the approaches." --repo owner/repo --runtime kimi --model kimi-k2
```

`gitmoot orchestrate`, `agent run`, and `agent implement` also accept an optional
`--skip-native-review-fanout` flag. By default an `implement` job that opens a
pull request fans the PR out to Gitmoot's native reviewers (the configured
required reviewers, or the ones passed for the task). With
`--skip-native-review-fanout` set, the coordinator owns review orchestration
instead: the implement→PR step still records the PR baseline, runs the merge
gate, and records the `implemented` decision, but it enqueues **no** native
review jobs. The skip is honored on both PR-open paths — the engine's
implement-advance and the daemon's GitHub PR-watcher — so a PR opened either way
stays free of native review fan-out. The flag defaults off; leave it off for the
full native review fan-out, which is byte-identical to prior behavior.

When a synchronous `agent implement`/`run`/`ask`/`review`/`orchestrate` job
delivers and **succeeds terminally** but a benign *post-success* advancement step
errors — for example a merge-gate block on the freshly-opened PR, or a 422
"a pull request already exists" race — the command no longer discards the result.
It **exits 0** with the agent result on stdout (in JSON mode this includes an
additive `advance_error` field carrying the advance warning, omitted when there
is none), prints `advance warning: …` to stderr, and shows an `advance_error:`
line in human output. Genuine non-terminal failures (the job did not reach
`succeeded`) still exit non-zero as before. A normal success with no advance error
is byte-identical to prior behavior — no `advance_error` field is emitted.

**Review resilience under branch churn.** A review job is pinned to the PR head
SHA it was queued against; in an active dev loop the branch often advances (a new
commit is pushed) before the queued review runs, leaving the registered checkout
on a newer head. Rather than failing the review on that head-SHA mismatch, Gitmoot
**re-syncs** it: when the PR is still **open**, the review is re-targeted to the
checkout's current head — reviewing the newest commit is exactly what a human
reviewer does — and a `review_head_resynced` job event records the old→new head.
The mismatch is only allowed to fail cleanly when the PR is **closed/merged** (a
stale review of a dead PR is not useful) or when the checkout is dirty. Relatedly,
when a foreground `agent review` finds the agent's serialized runtime session
**busy**, the review is now **left queued** for the daemon to run when the session
frees (a `requeued_runtime_busy` event is recorded) instead of being cancelled and
dropped; `agent ask`/`implement` stay synchronous and keep their existing
busy-session cancel behavior.

Start an orchestra of agents with `gitmoot orchestrate`:

```sh
gitmoot orchestrate project-planner "Plan and split this work across agents." --repo owner/repo
gitmoot orchestrate project-planner "Plan and split this work." --repo owner/repo --model gpt-5-codex
```

The built-in coordinator recipes `review-panel`, `decompose-and-verify`, and
`verifier` run a coordinator that fans work out to ephemeral workers and
reconvenes them in a continuation, with no agent pre-registration. The primary
invocation is the `--recipe` flag (also accepted on `agent run`), which routes
**any existing coordinator agent** through the named built-in recipe prompt
without changing the agent's identity or registration:

```sh
gitmoot orchestrate project-planner "Review PR #123 in this repo." --repo owner/repo --recipe review-panel
gitmoot orchestrate project-planner "Implement the export feature described in the task." --repo owner/repo --recipe decompose-and-verify
gitmoot orchestrate project-planner "Implement the rate limiter and prove it works." --repo owner/repo --recipe verifier
```

The bare `gitmoot orchestrate <recipe-id> "..."` form also works, but the
positional argument must resolve to a **registered agent** (or configured
managed type) — so it requires an agent registered under the recipe name
(e.g. after `agent template update review-panel` + `agent start review-panel
--template review-panel …`). On a fresh install without that registration it
fails with "agent not found"; prefer `--recipe`.

`gitmoot orchestrate <agent> "..." [--repo R]` is sugar for
`gitmoot agent run <agent> --background "..."`. It starts a conductor
(coordinator) that returns a `delegations[]` score; the players (child agents)
then run in parallel or in dependency order, and a finale (continuation)
reconvenes and synthesizes the results.

This uses the same agent registry, repo access grants, cached template snapshot,
runtime adapter, and local job history as PR-comment jobs. `agent run` is the
default coordinator-safe entrypoint because it routes to `ask`, `review`, or
`implement` and keeps branch, worktree, commit, push, PR, and workflow lifecycle
inside Gitmoot. `agent ask` is for analysis, planning, and questions only; it is
read-only, so when the message reads like branch/commit/push/PR orchestration it
prints a non-fatal note and still runs (pass `--force` to suppress the note).
The runtime
plugin helps Codex or Claude Code discover Gitmoot guidance, but it does not
replace the Gitmoot CLI. Synchronous jobs and queued jobs both use the same
runtime session locks.

Configure managed background agent types:

```sh
gitmoot agent type list
gitmoot agent type show planner
gitmoot agent type set planner --runtime codex --template planner --max-background 2 --idle-timeout 20m
gitmoot agent type set planner --model gpt-5-codex
gitmoot agent gc
```

`agent type set --model <name>` (or `[agents.<type>].model` in config) sets the
default runtime model for that managed agent type.

Schedule recurring agent work (heartbeats, off by default):

```sh
gitmoot agent heartbeat add repo-maintainer daily-status \
  --repo owner/repo --interval 24h --prompt "Daily status report." --enabled
# implement is policy-gated; --runtime pins a runtime for this schedule.
gitmoot agent heartbeat add builder nightly-tidy \
  --repo owner/repo --interval 24h --action implement --runtime codex \
  --prompt "Fix the top lint error and open a small PR."
gitmoot agent heartbeat list
gitmoot agent heartbeat show repo-maintainer daily-status
gitmoot agent heartbeat enable|disable repo-maintainer daily-status
gitmoot agent heartbeat remove repo-maintainer daily-status
```

A heartbeat enqueues a normal background job on its `interval`. Actions: read-only
`ask` (default) or `review` (`review` needs the agent's `review` capability), plus
the **policy-gated** write action `implement` — it only runs for an agent that
holds the `implement` capability AND a write-granting policy (`--policy
workspace-write` or `danger-full-access`); otherwise it is refused at `add` and
no-op'd (`last_status = policy_readonly`) by the daemon scan. An optional
`--runtime codex|claude|kimi` runs the scheduled job on that runtime (fresh
session) instead of the agent default. `gitmoot daemon status` surfaces each
schedule's last-run/next-due/last-status. See `docs/heartbeats.md` for the full
reference.

A registered single instance **shadows** a managed type of the same name:
dispatch resolves `gitmoot agent <name>` to a registered single instance before a
type, so force the type with `--type <name>` (or do not register a single
instance of that name). Since **v0.5.1** a foreground `gitmoot agent ask <type>`
(the `ask` action) dispatches to the managed type synchronously; background
`run`/`review`/`implement` to a type and `[parallel_sessions]` temp-session
forking use the **background** path. See WORKFLOWS.md → "Running one agent's jobs in parallel".

## Agent Templates

Install or refresh the built-in thermo review template:

```sh
gitmoot agent template update thermo-nuclear-code-quality-review
gitmoot agent start thermo-review \
  --runtime codex \
  --repo owner/repo \
  --template thermo-nuclear-code-quality-review \
  --start-daemon
```

Install or refresh the built-in full planner/goal template:

```sh
gitmoot agent template update planner
gitmoot agent start project-planner \
  --runtime codex \
  --repo owner/repo \
  --path . \
  --template planner \
  --start-daemon
```

Install or refresh the built-in coordinator recipe templates. These are
coordinator prompts for the Orchestra pattern, run with `gitmoot orchestrate
<agent> "..." --recipe <id>` (no re-registration needed) rather than started as
long-lived agents:

```sh
gitmoot agent template update review-panel
gitmoot agent template update decompose-and-verify
gitmoot agent template show review-panel
gitmoot orchestrate project-planner "Review PR #123 in this repo." --repo owner/repo --recipe review-panel
gitmoot orchestrate project-planner "Implement the export feature described in the task." --repo owner/repo --recipe decompose-and-verify
```

For fast current-chat planning, use the Gitmoot skill with the same packaged
`agent-templates/planner.md` instructions instead of starting a background job:

```text
Use the Gitmoot planner here. Write the implementation plan.
```

The current chat can also import any cached custom agent or template prompt:

```sh
gitmoot agent prompt frontend-reviewer
gitmoot agent prompt frontend-reviewer --json
```

This prints the prompt content for the current chat to apply locally. It does
not create a job, start a daemon, resume a runtime session, or post a PR
comment — a free read-only peek.

To track the here-method work by default, add `--record`: it opens a session
job on import (see "Session jobs" below) and returns the prompt with a header
line naming the job id, so the imported work shows in `job list` / the dashboard
once you clock out:

```sh
gitmoot agent prompt frontend-reviewer --record [--repo owner/repo] [--type ask|review|implement] [--json]
# prints:  [gitmoot session job <id> — when this work is complete, run:
#           gitmoot job close <id> --decision <approved|changes_requested|implemented|blocked|failed> --summary "..."]
# followed by the prompt body.
```

`--record` accepts either a **registered agent** or a bare **template id**:

- Registered agent: the repo comes from `--repo`, else the agent's `repo_scope`
  (error if neither is set); the session job records the agent name.
- Bare template (no agent of that name registered, e.g. the packaged `planner`):
  `--repo owner/repo` is **required** — a template has no `repo_scope` to fall
  back on — and the session job records the **template id** as its agent identity
  (#673). The repo must be tracked (`gitmoot repo add owner/repo` first).

`--type` defaults to `implement`. When the imported work is done, close the job
with `gitmoot job close <id> --decision …`. `--json` includes the opened
`job_id`. Without `--record`, behavior is unchanged (no job).

Draft and validate a captured template before installing it:

```sh
gitmoot agent template draft release-planner
gitmoot agent template validate .gitmoot/templates/release-planner.md
gitmoot agent template add release-planner --file .gitmoot/templates/release-planner.md
```

`agent template draft` only creates the standard markdown structure. For
current-chat capture, the active Codex or Claude chat reads
`references/TEMPLATE_CAPTURE.md` and fills that structure from visible
conversation context. Gitmoot does not extract hidden runtime memory.

Create a local custom prompt template:

```sh
mkdir -p agents
gitmoot agent template draft frontend-reviewer --output agents/frontend-reviewer.md
$EDITOR agents/frontend-reviewer.md
gitmoot agent template validate agents/frontend-reviewer.md
gitmoot agent template add frontend-reviewer --file agents/frontend-reviewer.md
gitmoot agent start frontend-reviewer \
  --runtime codex \
  --repo owner/repo \
  --template frontend-reviewer \
  --role reviewer \
  --capability ask \
  --capability review
```

After editing a local template file, refresh Gitmoot's cached snapshot:

```sh
gitmoot agent template diff frontend-reviewer
gitmoot agent template update frontend-reviewer
```

Template updates are versioned locally. `gitmoot agent template show <id>`
prints the current version, content hash, source commit, and promotion state.
Agents use the current promoted version by default, or a pinned version when
configured with a reference such as `--template frontend-reviewer@v1`.

A bad promotion can be undone: `gitmoot agent template revert <template-id>
--version <version-id>` makes a superseded version current again (the dashboard's
Agents page does the same with `v` in the agent detail).
Queued jobs keep the exact template content snapshot they were created with.

Discover templates by metadata:

```sh
gitmoot agent template list --runtime codex --output goal_file
gitmoot agent template list --tag review --capability ask
gitmoot agent template show frontend-reviewer
```

### Back Up And Share Templates Via GitHub

Templates can be backed up to and pulled from a GitHub repo (#476):

```sh
gitmoot agent template export [<id>...] [--all] [--to <dir>] [--dry-run]
gitmoot agent template publish [<id>...] [--all] [--repo <owner/repo>] [--path <subdir>] [--ref <branch>] [--message <msg>] [--create] [--dry-run]
gitmoot agent template pull [<id>...] [--all] [--repo <owner/repo>] [--ref <ref>] [--path <subdir>] [--dry-run]
gitmoot agent template add <id> --from-repo <owner/repo> [--ref <ref>] [--path <file>]
gitmoot agent template remote set <owner/repo> [--ref <ref>] [--path <subdir>]
gitmoot agent template remote show
```

`export` writes template `.md` files to a local directory; `publish` commits
them to a GitHub repo (`--create` creates a missing **private** repo); `pull`
installs or refreshes templates from that repo; `add --from-repo` installs a
single template file directly from a repo. `--all` on `export`/`publish` covers
only your custom templates — built-ins are skipped. `remote set` stores a default remote in the
`[template_remote]` config section (`repo`; `ref` defaults to `main`; `path`
defaults to `templates`) so publish/pull/add can omit `--repo`; with no remote
configured, those commands require an explicit `--repo`. **Caution: templates
are stored and published VERBATIM (prompt body + metadata) — point the remote
at a PRIVATE repo unless the prompts are meant to be public.**

## Goals

Print the standard Gitmoot goal prompt template:

```sh
gitmoot goal template
```

Import a goal file into local Gitmoot state:

```sh
gitmoot goal import --file GOAL-feature.md --repo owner/repo
```

Start a task in its dedicated branch worktree and inspect task state:

```sh
gitmoot task run task-001 --repo owner/repo --owner lead --base main
gitmoot task list --repo owner/repo
gitmoot task list --repo owner/repo --state implementing --json
```

`task run` stores the deterministic task worktree path under
`$GITMOOT_HOME/worktrees/<owner>--<repo>/<task-id>/` and leaves the registered
checkout on its current branch.

### Recover A Dead Implement

When an implementer dies mid-work — its process exits after editing the task
worktree but before it commits, pushes, and opens a PR — the edits are left
uncommitted in the worktree. `task run` (and `agent implement`) refuse to
restart over that state so nothing is silently discarded, and point you at
`task recover`:

```sh
gitmoot task recover task-001 --owner lead
gitmoot task recover task-001 --owner lead --repo owner/repo --skip-native-review-fanout --json
```

`--owner <agent>` is required (a registered implement-capable agent, attributed
as the recovery lead). `--repo owner/repo` is optional — it falls back to the
task's stored repo and is only required when the task carries none.
`--skip-native-review-fanout` persists that flag before the PR is opened;
`--json` prints the machine-readable recovery result.

`task recover` commits the full worktree state (`git add -A`, including
untracked non-ignored files), pushes the task branch, and opens or adopts the
task's PR — the exact finalize steps the dead implementer never reached. If the
worktree is already clean it recovers the commit already ahead of the base
(and refuses when there is nothing ahead to recover).

Two refusals guard `task recover` (and the `task run` / `agent implement`
restart that points to it):

- **Dirty worktree without an active job** — `task run`/`agent implement` refuse
  to restart a task whose worktree has uncommitted changes and no in-flight job;
  inspect it, then `task recover` to commit/push/open the PR, or clean/stash the
  worktree before retrying.
- **Live process still in the worktree** — `task recover` refuses while a live
  process is still inside the task worktree; wait for it to exit or stop the
  orphaned implementer before recovering.

## PR Comments

Use GitHub PR comments as the public audit trail:

```text
/gitmoot help
/gitmoot status
/gitmoot <agent> review [instructions]
/gitmoot <agent> implement [instructions]
/gitmoot ask <agent> [question]
/gitmoot retry <job-id>
/gitmoot cancel <job-id>
/gitmoot merge
/gitmoot resume <job-id> retry|continue|abort|answer [instructions]
@<agent> ask|review|implement [instructions]
```

A bare `@<agent> <action> …` mention on a PR comment (or, with the daemon's
`--watch-issues` flag, an issue comment) is treated as the same command as the
`/gitmoot <agent> <action>` form (#389). `/gitmoot resume <jobID>
retry|continue|abort|answer` resumes a delegation tree paused by
`escalate_human` or an ask-gate `human_questions` pause — see
`references/RESULT_CONTRACT.md` for the pause/resume semantics (`answer` is
PR-comment-only).

## Jobs And Locks

```sh
gitmoot job list --repo owner/repo   # add --json for machine-readable rows
gitmoot job show <job-id>            # add --json for the full job + why-stuck detail
gitmoot job watch <job-id>
gitmoot job events <job-id>
gitmoot job retry <job-id>
gitmoot job gates <job-id>                                         # list resumable gates; add --json
gitmoot job gates clear <job-id> --need "<text>"|--all             # satisfy gate(s); auto-resume on last
gitmoot job cancel <job-id>                                        # one queued|running|blocked job
gitmoot job cancel --state blocked [--older-than 7d] [--repo owner/repo] [--agent name] [--yes]
gitmoot job kill <root-job-id>
gitmoot lock list --repo owner/repo
gitmoot lock show owner/repo <branch>
```

### Resumable gates (make `blocked` + `needs` actionable)

When a stage returns `blocked` with a `needs` list (e.g. `needs: ["Maps API key"]`),
gitmoot persists each need as a **gate** attached to the blocked job. `gitmoot job
gates <job-id>` lists them (open / satisfied). Clearing a gate marks the blocker
resolved:

```sh
gitmoot job gates clear <job-id> --need "Maps API key"   # satisfy one need
gitmoot job gates clear <job-id> --all                   # satisfy every open gate
```

When the **last** gate is cleared, the blocked stage **auto-re-runs** via the same
`RetryJob` machinery `gitmoot job retry` uses (re-queued, then dispatched by the
daemon; downstream stages follow the normal delegation DAG) — no polling, resume
happens on clear. Two cases are deliberately **never** auto-resumed even with all
gates cleared: a **session job** (externally driven, #657) and a stage whose tree is
**paused awaiting a human** (`escalate_human` / ask-gate, #305/#340/#445) — a
resource gate must not bypass the human's `gitmoot resume` decision; the command
reports `not resumed: …` with the reason. A blocked job with **no** `needs` records
no gates and is byte-identical to before this feature.

### Session jobs (record "here"-method work)

The "here" method — importing an agent's prompt into your calling session with
`gitmoot agent prompt <agent>` — does the real work in your session but creates
**no gitmoot job**, so the dashboard / `job list` / event stream never reflect it.
Session jobs record that work as a first-class tracked job **without gitmoot
spawning a runtime** — a clock-in / clock-out pair (plus a one-shot recorder):

```sh
# Clock in: create a RUNNING, externally-driven job (no dispatch); prints its id.
gitmoot job open --agent <name> --repo owner/repo --type ask|review|implement \
                 [--title "..."] [--task <id>] [--pr <n>] [--json]

# Clock out: apply the result and move the job to its terminal state.
gitmoot job close <id> --decision approved|changes_requested|blocked|implemented|failed \
                 [--summary "..."] [--pr <n>] [--branch <name>] [--json]

# One-shot post-hoc: create an already-terminal job (open + close in one).
gitmoot job record --agent <name> --repo owner/repo --type ask|review|implement \
                 --decision <decision> [--title "..."] [--summary "..."] \
                 [--task <id>] [--pr <n>] [--branch <name>] [--json]
```

An `externally_driven` job is created directly in `running` (it never queues, so
the daemon never claims or Delivers it — no runtime subprocess, no runtime-session
or checkout lock) and the stuck-`running` reaper **skips** it, so a session may
hold it open for as long as the work takes. `close` reuses the exact result path an
engine-run job uses: `--decision` maps to the same terminal state
(`approved`/`changes_requested`/`implemented` → succeeded, `blocked` → blocked,
`failed` → failed) and emits the same finished/failed/blocked event, so a recorded
job is indistinguishable from an engine-run one in the dashboard and events. A job
can be closed **once** (it must be a running session job); an orphaned open job
stays `running` (reaper-exempt) until you `job close --decision failed` or `job
cancel <id>` it. A session job is never engine-executed, so `job retry` **refuses**
it (retrying would re-queue it for a real runtime with an empty payload) — recover
one by opening a fresh session job instead. The agent and repo must exist.

**Make in-chat / "here" work show on the dashboard.** The one-step default is
`gitmoot agent prompt <agent-or-template> --record`: it opens the session job *as
you import the prompt* and prints a header naming the job id (see the `agent
prompt` section above). It works for a registered agent (repo defaults to its
scope) and for a bare template id with an explicit `--repo` (the template id is
recorded as the identity, #673). Apply the prompt, do the work, then clock out with
`gitmoot job close <id> --decision …`. That is all it takes for otherwise-invisible
current-chat ("here") work to appear in `job list`, the dashboard, and the event
stream — no daemon, no runtime, no PR comment. Use plain `agent prompt` (no
`--record`) only when you just want to read a prompt without tracking it.

`gitmoot job kill <root-job-id>` is the operator kill switch for a runaway
delegation tree: it terminates the tree identified by its **root** job id
gracefully. In-flight jobs finish normally; the coordinator's next continuation
is routed through the graceful finalize path (synthesize what completed → stop)
and the daemon stops starting queued children of that root. See
`references/SAFETY.md` for how it relates to the other termination bounds.

For a `queued` or `blocked` job, `gitmoot job list` appends a `WHY:` column and
`gitmoot job show` prints a `why_stuck:` (and, when a lease applies, a
`next_retry_at:`) line explaining what the job is waiting on — e.g. `waiting on
runtime session lock runtime:codex:<ref> (held by job <id>)`, `blocked: awaiting
human`, `auth failing: …`, `throttled: …`, `retrying: …`, or a
`blocked-operational: <class>` deferral. The reason is derived from the most
authoritative existing signal (the latest reason-bearing `job events` entry plus
the owning resource lock's lease); a healthy job's output is unchanged. When a
deferral usually needs a human (a dirty or wrong-head checkout), the row/`job
show` also carries a `suggested_action` naming the concrete fix. `gitmoot doctor`
proactively validates `gh auth` (with an actionable remediation hint) and the
Claude runtime token so a bad credential is caught before a job stalls on it.

Operational blockers auto-retry (#532): a delivery failure classified as an
operational blocker — `runtime_auth`, `runtime_quota`, `network_outage`
(transient network/GitHub outage), or `checkout_contention` (a self-healing
branch-lock conflict, or a dirty/wrong-head checkout that carries a
`suggested_action`) — does **not** fail the job terminally. The daemon re-queues
it as **deferred** with a bounded retry budget and a hold until the earliest
retry time, shown as `blocked-operational: <class>: attempt n/3`. `gitmoot job
show --json` carries the `blocker_class`, attempt count, and `suggested_action`.
Over the `[events]` stream the deferral is now a **first-class** `job.deferred`
transition emitted **instead of** `job.failed` (no preceding `job.failed` for
that run). A `runtime_auth` deferral only re-dispatches once a live doctor-style
credential probe passes; the probe failing just extends the hold without spending
a retry. So a job that "failed then reappeared as queued" is the deferral
working, not a bug. Product failures (the agent answered with a `gitmoot_result`,
including `decision=failed`) are never auto-retried.

Jobs stuck in `running` are also backstopped: a running job with no lease
progress past the staleness window (default 30m) is assumed orphaned by a dead
worker and recovered/re-queued. The window is tunable via the
`GITMOOT_STALE_RUNNING_AFTER` env var; the smallest honored value is 1m —
below-1m, malformed, or non-positive values are rejected (with a one-time
warning) in favor of the 30m default rather than clamped (#560). That window is a
same-boot crash backstop, not a timeout: a running job whose runtime-session lease
has not elapsed is left held regardless of the window. After a **reboot** there is
no wait at all — the daemon detects the changed kernel boot id and, on its next
startup and every tick, immediately requeues every job claimed on a previous boot
and reclaims its stranded `runtime:<rt>:<session>` lock, regardless of any
unexpired lease (Linux only; elsewhere recovery falls back to the lease/age window;
#651).

`gitmoot job cancel <job-id>` is the single-job **abandon** verb. It dismisses a
`queued`, `running`, **or `blocked`** job (a blocked job is one paused awaiting a
human — an operator permission gate or an unrecoverable blocker — so dismissing it
is the same abandon intent as cancelling a queued/running one; #631). Cancel is a
single-row transition: it does **not** propagate to a delegation tree, touch task
state, or set the killed flag — abandoning a whole tree is `gitmoot job kill`.
Cancelling also releases any resource locks the cancelled job still owned —
including a stranded `runtime:<rt>:<session>` lock left behind when a foreground
`gitmoot agent ask` was killed — so the next ask on that agent does not wait out
the lock TTL before it can run. Dismissal is reversible: `gitmoot job retry`
accepts a cancelled job (its accepted source states are failed/blocked/cancelled),
so a mistakenly dismissed job can be resurrected.

`gitmoot job cancel --state blocked` is the **bulk** form for clearing a backlog
of blocked jobs. Only `blocked` is accepted for `--state` (queued/running jobs use
single-job cancel; terminal jobs use retry). Narrow the selection with
`--older-than` (a Go duration like `168h`, or a convenience `<N>d` days suffix like
`7d`; age is measured from when each job became blocked), `--repo owner/repo`
(matches the job's payload repo), and `--agent name`. The bulk form is a **dry-run
by default** — it prints the matching jobs (id, agent, repo, age) and exits without
cancelling anything; pass `--yes` to actually cancel the selection. Each selected
job is dismissed through the same per-job cancel path, so its locks are released
too. `<id>` and `--state` are mutually exclusive, and `--older-than`/`--repo`/
`--agent` require `--state`. `gitmoot doctor` warns when blocked jobs older than
30d have piled up and prints this exact command as the remediation.

To automate the sweep, set `[orchestrate].blocked_ttl` to a positive Go duration
(e.g. `blocked_ttl = "168h"`): the daemon's housekeeping tick then dismisses any
blocked job whose blocked-transition timestamp (updated_at, created_at fallback)
is older than the TTL, through the same `CancelJob` abandon path, recording a
distinct `blocked_ttl_expired` job event so a TTL auto-expiry is distinguishable
from a manual `job cancel`. It is **off by default** — an empty or `0s` value
disables it (a negative value is rejected). Unlike `[orchestrate].escalation_ttl`
(which auto-finalizes a whole paused delegation *tree* and is on by default, 24h),
`blocked_ttl` dismisses a *single* blocked job and is off by default.

Merge-gate retries are automatic while the daemon is running. Retryable states,
such as a busy base-branch merge queue or a GitHub branch update in progress,
are retried on the next daemon poll tick. The default poll interval is `30s`
unless the daemon was started with a different `--poll`.

## Result Checks

After a daemon-run job's `gitmoot_result` is parsed, Gitmoot runs a set of
**deterministic, LLM-free binary checks** over the parsed result (#526) — a
contract-hygiene audit that catches results that are technically valid but vague
or missing evidence. Each check is a yes/no question with an explanation, e.g.:

- **implement** — a result whose decision is `implemented` must list its
  `changes_made` and its `tests_run`.
- **review** — a `changes_requested` review must carry `findings` (evidence).
- **ask** — the answer (`summary`/`artifact_body`) must be non-empty and
  actionable.
- **blocked** (any action) — a `blocked` result must list actionable `needs`.
- **coordinator finalize** — a finalize continuation must produce a substantive
  reconciliation summary.

The mode is set in `config.toml` and is **warn by default**:

```toml
[workflow]
result_checks = "warn"   # off | warn | block (default: warn)
```

- `warn` (default) — failing checks are recorded as a `result_checks_failed`
  job event (visible in `gitmoot job events <id>` and `gitmoot job show <id>`)
  and attached to the job detail (`job show --json` `payload.result_checks`, and
  the web dashboard), but the job still finishes on its own decision.
- `block` — a failing check additionally fails the job through the same terminal
  path a malformed result takes (opt-in, for strict workflows).
- `off` — the audit is disabled entirely, restoring byte-identical pre-feature
  behavior (no event, no payload field, no stored record).

A result that passes every applicable check records nothing, so the audit is
quiet on healthy jobs. Failed checks are also stored durably so a later SkillOpt
pass can consume them as structured feedback; there is no SkillOpt behavior
change today.

## SkillOpt Exchange

```sh
gitmoot skillopt review create --template <id> --repo owner/repo --run <run-id>
gitmoot skillopt review item add --run <run-id> --item <item-id> --baseline baseline.md --candidate candidate.md [--title text]
gitmoot skillopt review create --template <id> --repo owner/repo --run <run-id> --mode explore --exploration-level high --options 4
gitmoot skillopt review item add --run <run-id> --item <item-id> --option a=option-a.md --option b=option-b.md [...]
gitmoot skillopt review status --run <run-id>
gitmoot skillopt export --run <run-id> [--output training.json]
gitmoot skillopt import --file candidate.json [--artifact-dir artifacts]
gitmoot skillopt candidate list [--template id]
gitmoot skillopt candidate show <version-id>
gitmoot skillopt candidate promote <version-id>
gitmoot skillopt candidate reject <version-id> [--reason text]
gitmoot skillopt gate run --candidate <version-id> [--corpus path] [--replay-command cmd] [--config path] [--json]
gitmoot skillopt gate history --candidate <version-id> [--json]
gitmoot skillopt binary run --set <file> --run <run-id> --source <file> [--deterministic] [--reviewer runtime] [--home path] [--json]
gitmoot skillopt binary show --run <run-id> [--home path] [--json]
gitmoot skillopt synth --template <id> --repo owner/repo --weak <agent> --strong <agent> [--judge <agent>] [--challenger <agent>] [--max-items N] [--max-rounds-per-item M] [--gap F] [--out dir] [--json]
gitmoot skillopt synth list [--status pending_human_approval|approved|rejected] [--json]
gitmoot skillopt synth approve <item-id>
gitmoot skillopt synth reject <item-id>
gitmoot skillopt ab <agent> "<prompt>" [--challenger <versionId>] [--pick a|b] [--seed N] [--judge] [--judge-only] [--home path]
gitmoot skillopt pairwise import <packet-dir> [--packet path] [--secret-map path] [--picks path] [--reviewer name] [--json]
gitmoot skillopt rubric induce --template <id> [--out <dir>] [--holdout 0.2] [--min-events N] [--home path] [--json]
gitmoot skillopt feedback markdown export --run <run-id> --output .gitmoot/evals/<run-id>
gitmoot skillopt feedback markdown import --packet .gitmoot/evals/<run-id> [--reviewer name]
gitmoot skillopt feedback github publish --run <run-id> [--repo owner/repo] [--pr <number>]
gitmoot skillopt feedback github sync --run <run-id> [--repo owner/repo] (--issue <number>|--pr <number>)
gitmoot skillopt train start --template <id> --repo owner/repo --request <text> --items-file items.yml [--workspace-repo owner/workspace] [--preview-repo owner/previews] [--preview-mode none|optional|required] [--preview-renderer none|vue-vite] [--preview-publisher none|github-pages] [--preview-route-template template] [--create-repos] [--yes]
gitmoot skillopt train status --session <id>
gitmoot skillopt train run [--config path | --session <id>] [--plain]
gitmoot skillopt train continue --session <id> [--generator-type skillopt-generator | --generator-agent name] [--skillopt-bin path] [--dry-run] [--promote version|--reject version --reason text] [--start-next]
gitmoot skillopt train recover --session <id> [--out-root path] [--generation [--abort | --advance-state]] [--json]
gitmoot skillopt train stop --session <id> --reason <text>
gitmoot skillopt judge-report [--template <id>] [--home <path>]
gitmoot skillopt judge agreement [--template <id>] [--home <path>] [--json]
gitmoot skillopt judge promote --template <id> --task-kind <kind> --file <pkg.json> [--home <path>] [--yes] [--json]
```

On a real terminal, `skillopt train run` opens an interactive view of one session
(resolved from `--session` or the newest session of a `--config`): a phase bar
plus a single keypress per step — `enter` advances the current phase (the long
generate/optimizer steps run in a detached background process so `q` leaves the
run going), `p`/`x` promote or reject a candidate, `n` starts the next iteration.
Review-blocked phases show the GitHub issue link to continue from the browser.
`--plain`, a piped stdin, or `GITMOOT_NO_TUI`/`TERM=dumb` print a one-shot status
snapshot instead. `train status`/`continue` print a `continue_from_github:` line
at review-blocked phases. `train start --create-repos` (or the prompt in the
`train init` form) creates a missing target/workspace/review repo on GitHub.

Use `skillopt train` for the product workflow. It pins the template version,
tracks sessions and iterations, validates item diversity, generates temporary
agent options, publishes review packets, syncs feedback, hands off to the
external optimizer, imports pending candidates, publishes candidate review
context, and starts follow-up iterations only after a promoted/rejected/abandoned
decision. Use `skillopt review`, `feedback`, `export`, `import`, and
`candidate` directly only for advanced debugging, custom research runs, or
recovering one step of a train session.

`skillopt train continue` generation is durable and idempotent on resume. Each
review item's artifacts, item row, and options commit in one transaction the
moment that item finishes, so an interrupted generate phase loses only the item
that was in flight. Re-running `train continue` regenerates ONLY the items that
are not yet complete; fully-generated items are skipped, so no duplicate options
are produced and completed work is never rewritten. If a single item has some
but not all of its options/artifacts persisted, resume returns a hard error
(`item <id> has partial generated options; inspect or clear review options
before continuing`) rather than guessing.

`skillopt train recover --session <id>` recovers the OPTIMIZER phase by default.
It re-imports or repairs the optimizer candidate package and classifies the
iteration (for example `already_completed_candidate`,
`already_completed_no_candidate`, `optimizer_active`, or
`corrupted_unrecoverable`).

`skillopt train recover --session <id> --generation` recovers the GENERATION
phase: it reclaims a generation lock stranded by a crashed/killed `train
continue` (whose deferred lock release never ran) and salvages the already
persisted per-item options. Reclamation is liveness-gated — the stranded lock is
released ONLY when its owner PID is provably dead AND it was held on this same
host; a live owner is refused (`skillopt train generation is already running`) so
you stop the running process first, and a cross-host owner requires the lock's
TTL to have expired. The recover process re-acquires the lock for itself so the
salvage is also crash-safe. Salvage is import-only: completed items are already
durable, so recovery classifies them as `expected_items`/`recovered_items`/
`missing_items` and reports `recovery_state: generation_complete`,
`generation_incomplete`, or `generation_active`. The iteration advances to
`options_generated` only with `--advance-state` and only when every expected item
is recovered (regenerating missing items remains `train continue`'s job). Use
`--abort` to reclaim the lock and leave the iteration at `items_ready` (persisted
items are kept). A stale generation lock is also surfaced in `train status` (as a
`stale` active lock, separate from the true current phase).

`skillopt review create` starts a review run for a template and target repo.
Use the default A/B shape for validation, or pass `--mode explore|refine|distill`
with `--options N` for ranked exploration. `skillopt review item add` stores
saved baseline/candidate outputs as artifact-backed A/B review items, or
repeated `--option label=path` artifacts for ranked N-way items. `skillopt
review status` reports whether the run has items, complete artifacts, imported
feedback for every item, ranking stability, pairwise preference count, and a
recommended next mode. Recommendations are advisory; Gitmoot never changes mode,
imports a candidate, or promotes a template automatically.

`skillopt export` writes a JSON training package with the template snapshot,
eval run, review items, artifact manifests, feedback events when present, and
evaluator config. Use `gitmoot-skillopt optimize --training-package
training.json --artifact-root ~/.gitmoot/evals/blobs --out-root
.gitmoot/skillopt/<run-id> --candidate-output candidate.json --dry-run` first
to validate the contract without model calls.
Before real model-backed optimization, check `gitmoot-skillopt --version` and
`gitmoot-skillopt optimize --help`, or install it with
`pipx install https://github.com/jerryfane/gitmoot-skillopt/releases/download/v0.4.2/gitmoot_skillopt-0.4.2-py3-none-any.whl`.
Verify required model/backend environment variables for the installed optimizer
version. `skillopt import` validates a candidate package and stores the
candidate template as a pending version; it never promotes the candidate
automatically. If the candidate package includes new artifact manifest entries,
pass `--artifact-dir` so Gitmoot can verify relative paths and SHA256 hashes
before storing blobs. `skillopt candidate show` displays candidate metadata, eval
report JSON, preference summary, and a content diff against the base/current
version. `skillopt candidate promote` makes a pending candidate current, while
`skillopt candidate reject` records an auditable rejection and prevents that
version from being selected by `@latest`.

`skillopt gate run --candidate <version-id>` runs the off-by-default,
deterministic **pre-canary replay gate** (`#627`, AutoMem A.2): it replays the
candidate against a **fixed, versioned job corpus** and accepts it only on
**strict improvement** over the current champion on the same corpus (a tie
fails). It reuses the `#474`/`#485` deterministic scorers on the corpus outputs —
no new judge, no live LLM in the gate itself; the replay driver is a deterministic
`sh -c` command that reads `GITMOOT_GATE_TEMPLATE_FILE`/`GITMOOT_GATE_PROMPT`/
`GITMOOT_GATE_EXPECTED`/`GITMOOT_GATE_ITEM_ID` and emits a per-item result JSON.
The command prints pass/fail plus per-item deltas, exits non-zero on a rejected
gate, and **persists** the run for audit (`skillopt gate history --candidate
<version-id>`). When `[skillopt].gate_enabled` is on, a candidate must carry a
passing gate run before it may be promoted to canary or current; otherwise the
promotion seam blocks. On a gate failure the protocol allows exactly one retry
feeding the failing replay log back to the optimizer, then rejects.

When `[skillopt].pace_enabled` is on, an **additional** off-by-default
**PACE anytime-valid commit gate** (`#687`) sits on the auto-promote path: a
guardrails-pass candidate is auto-promoted only when a model-free
testing-by-betting e-process over its recorded candidate-vs-champion pairwise
outcomes (the Mode B bandit arm's win/loss tally, `#481`/`#482`) crosses the
commit threshold `1/pace_alpha`. Each discordant pair bets `pace_lambda` of the
wealth (`E ← E·(1+λ(2w−1))`, ties discarded); the gate **stops early** the moment
it is decisive and **rejects** once `pace_max_pairs` discordant pairs are spent
without crossing — so peeking-until-you-win cannot p-hack a promotion (Ville's
inequality bounds the false-commit probability by `pace_alpha`). It is a strictly
additional gate: **every** existing guardrail (`auto_promote_min_samples`/
`_min_score`/`require_external_ci`/`_min_confidence`/canary/replay gate) still
applies, and a non-decisive or budget-exhausted stream **fails safe** to a
`pace_blocked` notify (no promotion). Off (the default) it is never consulted —
byte-identical. Knobs: `pace_alpha` (default `0.05` → threshold 20), `pace_lambda`
(default `0.5`), `pace_max_pairs` (default `200`).

`skillopt binary run --set <file> --run <run-id> --source <file>` runs the
off-by-default, additive **BINEVAL-style binary evaluation** (`#525`): instead of
one opaque scalar judge score, a rubric is decomposed into small, independent
**yes/no questions**, each answered on its own with a verdict plus explanation,
and the per-question verdicts aggregate into a per-dimension **weighted
yes-fraction** and a weighted-mean **overall** score. The **question set** is a
YAML or JSON file (`{version: 1, template_or_task_kind, dimensions: [{name,
weight, questions: [{id, text, violation_example, weight}]}]}`); ids must be
unique and weights default to `1`. `--deterministic` uses a **rule-based runner**
(no LLM) that answers each question purely from optional
`contains`/`not_contains`/`regex`/`not_regex` assertions on the question — the
reproducible/test mode. Without `--deterministic`, `--reviewer <runtime>` selects
an **opt-in LLM-backed runner** wired through the same cross-family judge plumbing
as `skillopt ab --judge`, so each question is answered by a *different* family
(never a self-preference), read-only. Verdicts persist to the additive
`skillopt_binary_verdicts` table keyed by `(run_id, question_id)` and are re-read
by `skillopt binary show --run <run-id>`. Each row also stores the
`question_weight`/`dimension_weight` the run used (default `1`), so `show`
re-aggregates with the same weighting and reports the **identical** per-dimension
and overall scores `run` emitted (even for non-uniform weights). They also ride the training-package
export as an **optional** `binary_verdicts` section (omitempty — verdict-less
packets are byte-identical). The per-dimension scores map onto the existing
`EvaluatorScore.DimensionScores` shape with **no contract change**. Nothing here
runs unless a `skillopt binary` command is invoked — every existing SkillOpt
review/optimize flow is byte-identical when it is unused.

`skillopt pairwise import <packet-dir>` ingests a **blinded paired-review
packet** produced by the gitmoot-skillopt fork (the `pairwise-review.json`
packet plus its secret map and the reviewer's picks), de-blinds it, and stores
the pairwise-preference feedback events — the import path for Mode B's
paired-review evidence. The daemon can also import review-issue feedback
automatically when started with `--watch-skillopt-reviews`.

`skillopt synth --template <id> --repo owner/repo --weak <agent> --strong
<agent>` is the off-by-default, **explicit opt-in** Autodata-style synthetic
review-item generator (`#535`). There is NO daemon/auto integration — nothing
runs it but you. For each of up to `--max-items` (default 3) items it runs a
loop through the SAME runtime-adapter path the A/B path uses: a **Challenger**
(the `--challenger` agent, default the strong agent) writes a `{context,
question, rubric}`; the `--weak` and `--strong` agents each attempt it; a
`--judge` (default the strong agent) scores both answers against the rubric and
flags well-formedness. An item is **accepted** only when the strong agent
meaningfully beats the weak agent (score gap ≥ `--gap`, default 0.20) AND the
judge confirms the item is well-formed; otherwise the round records a
diagnostic (`too_easy`, `too_hard`, `strong_failed`, `bad_rubric`, or
`context_leak`) and the Challenger regenerates with targeted feedback until
accepted or `--max-rounds-per-item` (default 3) is exhausted. Every
challenger/weak/strong/judge delivery is **sandboxed** into a fresh per-item temp
scratch dir (never a registered repo checkout) and framed with an answer-only
preamble, so an agentic-CLI attempt can never write files, start servers, or
modify a live checkout while "answering" the exercise (`#725`); the scratch dir
is deleted when the item finishes. Accepted items are
written to `--out` (default `<home>/evals/synth`) and stored in the DB with
status `pending_human_approval`. They are **structurally isolated**: nothing in
the promotion/training path reads the synth table, so a pending item can never
affect a promotion. `skillopt synth list [--status ...]` shows them; `skillopt
synth approve <item-id>` / `skillopt synth reject <item-id>` is the load-bearing
human gate — only an approved item is cleared for later manual inclusion in a
review pool. Skipped/rejected candidates are logged with their diagnostic and
never persisted.

At the manual promote/reject gate, Gitmoot records every judge↔human outcome
into a local store — all four directions: `agree_accept`, `agree_reject`,
`judge_accept_human_reject` (judge accepts, human rejects — a false positive),
and `judge_reject_human_accept` (judge rejects, human accepts — a false
negative). Each outcome is tagged with the judge prompt version, evaluator id,
and prompt hash that produced the score, so later analysis can compare judges by
prompt revision. This capture is measurement only: it never changes the judge,
overrides a decision, or bumps the result contract.

`skillopt judge-report` reads those captured outcomes and reports how well the
LLM judge is calibrated against human verdicts. It prints a confusion matrix
(the four `direction` buckets), the agreement rate and Cohen's κ, calibration
buckets (judge soft-score versus the human decision), and per-dimension
disagreement. Pass `--template <id>` to scope the report to one template, and
`--home <path>` to read from a non-default Gitmoot home. It is read-only.

`skillopt judge agreement` is the judge↔human agreement measurement harness
(#344). It joins the stored A/B judge verdicts (`skillopt ab --judge` /
jury rows) against the human ranked/pairwise feedback on the same
**comparison**: each `skillopt ab` invocation stamps a shared per-comparison
token on all of its rows, so repeated A/Bs of one challenger stay separate
observations (older tokenless rows are excluded and counted as unmeasurable,
never pooled; internal ties within one comparison are skipped and counted).
It reports Cohen's κ as the **headline** metric (raw agreement overstates
judge quality because it does not correct for chance), the raw agreement
rate, per-human-source and per-juror-family breakdowns, an
assignment-corrected position-bias audit over judge rows that carry the
recorded raw a/b pick (`P(pick=a)` stratified by the champion's presented
position and reported alongside `P(option A = champion)`; undefined when a
fixed `--seed` pinned the champion to one position), and a summary of the
candidate-level judge outcomes above. Small samples get a loud warning —
sample size is the limiter. `--json` emits the machine-readable report. It is
read-only.

`skillopt rubric induce` is the **offline, deterministic rubric-induction**
tool (#344/#347, AutoLibra-style — 2505.02820). It reads the human feedback
already captured for a template (the `useful_traits` / `rejected_traits` /
`required_improvements` on ranked feedback events, across all of that
template's runs) and **induces a criterion-separated rubric** from it, then
freezes it as reviewed static JSON. The pipeline is fully offline (no LLM
calls, so it is reproducible and testable): (1) **ground** each trait string
into an aspect `{text, sign +/-, source_event_id}`; (2) **cluster** aspects by
normalized token-overlap (Jaccard, greedy single-linkage, stable ordering)
into up to six metrics, each `{name, definition, positive_examples,
negative_examples, source_event_ids}`; (3) **meta-evaluate** on a held-out
split — `coverage` (fraction of held-out aspects a metric matches) and
`redundancy` (max inter-metric similarity, lower is better); (4) **write**
`rubric.json` (the frozen rubric, `{version, template, metrics[...]}`),
`report.json`, and a human-readable `report.txt` under `--out` (default
`<home>/skillopt/rubrics/<template>`). Flags: `--template <id>` (required),
`--holdout 0.2` (fraction reserved for coverage; `0` keeps in-sample),
`--min-events N` (minimum usable feedback events, hard floor 3),
`--home <path>`, and `--json` (emits the report plus the artifact paths). It
errors with an actionable message and a non-zero exit when there are fewer than
three usable feedback events or fewer than two separable metric clusters. The
token-overlap clusterer is the deterministic v1; swapping in AutoLibra's LLM
thematic clustering is a clearly-marked extension point that changes only the
clustering step, leaving grounding, the held-out meta-eval, and the emitted
contract identical.

The tool is **read-only over the store and human-gated**: it only writes files
and never injects anywhere. To adopt an induced rubric, a human reviews
`rubric.json` and maps its metrics onto gitmoot-skillopt's
`evaluator_config['rubric']` — one dimension per metric, with the metric
`name` as the dimension key, its `definition` (and `positive_examples` /
`negative_examples`) as the description, and a weight the reviewer chooses.
Because `_compose_evaluator_rubric` already merges arbitrary
`evaluator_config['rubric']` dimensions and `_normalize_dimension_list`
accepts arbitrary names, this needs **zero judge-code change and no result
contract bump**. For example a metric named `clarity / headline / copy` becomes
`evaluator_config['rubric'] = {"clarity / headline / copy": {"description":
"<definition + examples>", "weight": 1.0}}`.

`skillopt judge promote` closes the judge-prompt optimization loop: it applies an
**accepted** judge-prompt variant (from the judge-prompt optimizer's
`gitmoot-skillopt-judge-candidate` package) into a template, so the next
skill-opt run judges with the improved prompt. Select the variant with
`--task-kind <kind>` (use `_global` for the all-items pass). It **previews by
default** — printing the template id, task kind, the `baseline→best` agreement
delta, and a truncated prompt preview, and writing nothing — and requires
`--yes` to apply. It refuses (hard error) any variant whose `accepted` is not
true or whose `best_prompt` is empty, and any task kind missing from the package.
On apply it writes the prompt into the template's `evaluation` metadata
(`judge_prompt_templates` keyed by task kind, **merging** so other task kinds are
preserved, plus a bumped `judge_prompt_version`) and records a
`skillopt_judge_outcomes` audit row (`human_decision=promoted`, the old→new
version, and the agreement delta in `reason`). `--json` emits the machine-readable
preview/apply summary.

The Markdown feedback collector writes blind A/B review packets with `index.md`,
per-item Markdown files, editable `feedback.yml`, and hidden assignment metadata
that Gitmoot uses to validate the full response and import de-blinded canonical
feedback events. Open `index.md`, review every file in `items/*.md`, set
`reviewer`, edit `feedback.yml` with exactly one of `a`, `b`, `tie`, `neither`,
or `skip` for every item, and leave `.assignments.json` untouched.
Ranked packets use the same files, but `feedback.yml` contains ordered rankings
plus optional `useful_traits`, `rejected_traits`, and reasoning. After feedback
exists, packet summaries hide outcome-bearing phase details so later blind
reviewers do not see the current winner before responding.

The GitHub feedback collector publishes the same blind A/B review packet to a
new issue by default, or to an existing PR when `--pr <number>` is provided.
Repository resolution uses `--repo`, then the eval run target repo, then the
template source repo, then optional `[feedback].repo = "owner/reviews"` in
Gitmoot config. Reviewers can reply with full YAML or run-scoped short-form
lines such as `run_id: run-1` followed by `item-001: b - More concrete.`.
`github sync` imports valid comments into canonical feedback events and ignores
unrelated comments safely.
Ranked GitHub comments can use `item-001 ranking: C > A > D > B` plus trait
notes. Use the ranked workflow for exploration/refinement and return to A/B
validation for final promotion decisions on fresh items.

## Agent Memory

Agent persistent memory (#626) gives an enrolled agent a repo-filtered pool of
durable *facts* ("this repo's arm64 CI is flaky") that Gitmoot injects into the
job prompt as a reference-only block. It is **off by default** and, in the
current phase, runs in **observation mode**: the injectable ("confirmed") tier
is populated only by Gitmoot's own deterministic mechanical facts, while
agent-returned learnings are logged for measurement but never injected.

Gitmoot writes a mechanical fact only when a terminal job carries a genuine,
bounded signal — never one fact per job (#645):

- **Fix-round facts** — when a job reached its decision only after one or more
  corrective verify/retry rounds ("recent implement jobs here needed up to N fix
  rounds"), keyed by decision.
- **Terminal-outcome facts** — when an *ordinary* job (the shape `agent
  ask`/`agent run`/`review`/`implement` enqueue, no verify/retry loop) ends on the
  **notable**, non-anomalous decision `changes_requested` ("some review jobs here
  concluded with changes requested") — keyed by `(action, outcome)`. A routine
  first-try success (`approved`/`implemented`) writes nothing, and the *anomalous*
  one-off terminals (`failed`, `blocked`) are **not** auto-promoted: without a
  recurrence threshold, a single flaky failure must not become a durable, injected
  repo fact.

Facts are keyed by low-cardinality **closed** categories, never free-form
content: the outcome is a validated decision value and the action is collapsed to
a small fixed allowlist (a delegation's free-form action buckets to a generic
token). So repeated jobs UPSERT the same row rather than growing the pool, and
every fact passes the same deterministic write filters as agent learnings.

Enrollment is per agent, plus optional global knobs:

```toml
[agents.builder]
runtime = "codex"
memory = true          # enroll this agent (default off)

[memory]
disabled = false       # global kill switch (overrides every enrollment)
token_budget = 1500    # cap on injected block size (estimated tokens)
max_entries = 15       # cap on confirmed rows considered for injection
```

An agent returns durable facts via the optional top-level `learnings` field in
`gitmoot_result` — each entry is `{key, scope ("repo"|"general"), content}`.
Most jobs return none. Returned learnings are shadow-logged only (never injected
in this phase) and pass deterministic pre-filters that reject directive-phrased,
executable, secret-shaped, or non-repo-agnostic content.

Inspect and measure the store (all read-only):

```sh
gitmoot memory list [--pending|--confirmed] [--agent NAME] [--repo owner/repo] [--json]
gitmoot memory replay [--agent NAME] [--repo owner/repo] [--limit N] [--json]
gitmoot memory eval --fixtures fixtures.json [--k N] [--json]
```

`memory list` shows confirmed memories and/or pending observations. `memory
replay` is an offline A/B: it re-renders recent real jobs' prompts with and
without the learnings block and reports the injection delta (added tokens,
entries injected) — it measures injection *mechanics*, not outcome quality
(running real agents twice is a later-phase gate). `memory eval` computes
recall/precision@K of retrieval over a labeled fixtures file whose cases are
`{agent, repo, instructions, expected_keys}`.

## Pipelines

A pipeline (#681) runs a **declared DAG of shell stages** — a fixed, repeatable
multi-step flow — on demand or on an interval schedule. Each stage is an ordinary
queued job run through the **shell runtime**: the normal worker tick claims and
runs it, and a scan-based advancer folds each stage's `gitmoot_result` **decision**
and enqueues the stages whose `needs` have all succeeded. Pipelines reuse the job
queue, the result contract, and the heartbeat scheduling idiom (durable `next_due`,
overlap guard, missed-ticks-coalesce). They are **off by default** (no pipelines ⇒
the daemon's pipeline scan returns before touching state).

Define a pipeline in a YAML file and register it:

```yaml
name: nightly-sync          # required, name-safe token (letters, digits, - _)
repo: owner/repo            # optional to register; REQUIRED to run
schedule:                   # optional interval schedule (no cron in v1)
  interval: 24h             #   positive Go duration (required with a schedule block)
  jitter: 15m               #   optional random [0, jitter] added to next_due
stages:                     # the DAG, keyed by unique id and wired by needs
  - id: source
    cmd: "curl -sf https://example.com/data > data.json"
  - id: score
    cmd: "python score.py data.json"
    needs: [source]         # runs only after every listed stage SUCCEEDS
  - id: deploy
    cmd: "rclone copy out/ r2:bucket"
    needs: [score]
    timeout: 30m            # optional per-stage job timeout
    retry: 2                # optional; re-attempt a FAILED stage up to N times
```

```sh
gitmoot pipeline add nightly-sync.yaml --enable   # validate + store; omit --enable to add disabled
gitmoot pipeline list [--json]
gitmoot pipeline show nightly-sync [--json]        # registry view for a name
gitmoot pipeline run nightly-sync                  # start a manual run; prints the run id
gitmoot pipeline show <run-id> [--json]            # run funnel for a "prun-…" id
gitmoot pipeline resume <run-id> [--from <stage>]
gitmoot pipeline cancel <run-id>
gitmoot pipeline enable|disable nightly-sync
gitmoot pipeline remove nightly-sync
```

`pipeline add` validates the whole spec at add time (unknown keys, duplicate/self/
cyclic `needs`, missing `cmd`, invalid durations, a `success_decisions` outside
`approved`/`implemented`/`changes_requested`) so a mistake is a clear error, not a
stuck run. It stores the raw YAML **verbatim** plus a content hash; each run
snapshots that hash and executes its snapshot, so editing the file later never
mutates an in-flight run. `pipeline add` also auto-creates one hidden shell runner
agent (`pipeline-<name>-runner`) that owns the stage jobs — filtered out of `agent
list` and disposed by `pipeline remove`.

A stage signals its outcome by printing a `gitmoot_result` blob to stdout; the
advancer folds by the **decision**, never the job's exit state (`changes_requested`
is a succeeded job but folds as a stage **failure** by default — a stage folds on the
decision, not the job state):

```sh
printf '%s' '{"gitmoot_result":{"decision":"approved","summary":"synced"}}'
printf '%s' '{"gitmoot_result":{"decision":"blocked","summary":"secret missing","needs":["R2 token"]}}'
```

- a decision in the stage's `success_decisions` (default `approved`/`implemented`) →
  **succeeded**, dependents enqueue;
- `blocked` → the stage blocks, its `needs` persist at the stage **and** run level,
  the run **parks blocked** (downstream never enqueues, zero compute while parked);
- `failed` / any other decision / a cancelled job / no `gitmoot_result` → the stage
  **fails** (retried if budget remains), else the run **parks failed**.

`pipeline run` prints only the run id (script-stable: `RUN=$(gitmoot pipeline run
nightly-sync)`); a manual run ignores `enabled` but still needs a `repo` and refuses
to start while a run is already active. `pipeline show <run-id>` renders the **text
funnel** (`source OK -> score BLOCKED (needs: R2 token) -> deploy SKIPPED`) under a
run header; a **failed** run also prints the exact `gitmoot report bug --job
<stage-job>` command (gitmoot never auto-files it).

`pipeline resume` re-runs a **parked** (blocked/failed) run from its halted stage
(or `--from <stage>`) plus its transitive dependents — bumping their attempt — while
**never re-running a succeeded stage**; it refuses a non-parked run and a run whose
spec drifted. `ResumePipelineRun` is the #682 approval-gate seam. `pipeline cancel`
abandons a run through the shared `job cancel` path.

A pipeline stage is a **leaf**: a stage result carrying `delegations[]` does not
spawn children — the advancer ignores them and the engine strips them for a pipeline
stage job. Use an orchestra for dynamic fan-out, a pipeline for a fixed shell DAG.
See `docs/pipelines.md` for the full reference and `WORKFLOWS.md → Pipelines` for the
end-to-end story.

## Native Chat (agent threads)

`gitmoot chat` (#534, V1 local-only) is a durable, repo-aware **conversation
ledger** where registered agents and the human talk in threads, `@`-tag each
other, and (explicitly) promote a message into a real job. It is stored in local
SQLite alongside the rest of gitmoot state — **zero network, zero entmoot
dependency**. The core rule: **a message is a row (free); a job is compute
(explicit)**. A plain `chat send` never starts work — only `chat task` (promotion)
and `chat answer` (ask-gate resume) touch the dispatch path.

```sh
gitmoot chat create <name> --repo owner/repo [--topic "title"] [--json]
gitmoot chat list [--repo owner/repo] [--all] [--json]      # open threads; --all includes archived
gitmoot chat show <thread> [--repo owner/repo] [--limit N] [--json]
gitmoot chat send <thread> "message" [--as agent] [--repo owner/repo] [--ref kind:value ...] [--json]
gitmoot chat inbox <agent> [--unread] [--json]
gitmoot chat task <thread> "@agent message" [--action ask|review|implement] [--repo owner/repo] [--json]
gitmoot chat answer <thread> "<question-id>: answer text" [--repo owner/repo] [--json]
gitmoot chat close|reopen <thread> [--repo owner/repo] [--json]
gitmoot chat rename <thread> "new name" [--repo owner/repo] [--json]
```

- **`create`** — `<name>` is slugified to a topic-path-safe handle (`[a-z0-9-]`, no
  `+`/`#`/`/`; unique per repo); a name that slugifies to nothing is rejected.
  `--repo` is required. `--topic` sets the human display title (defaults to the
  slug). The slug is the stable handle; a later `rename` changes only the title.
- **`send`** — appends a `chat` message. `@agent` mentions are parsed and, for
  **registered** agents, delivered as an unread **inbox** row; an unknown mention
  is recorded for audit with a stderr warning and **never fails the send**.
  `--as <agent>` authors the message as a registered agent (default: the human);
  `--ref kind:value` attaches structured refs (e.g. `--ref pr:42`). Sending to an
  archived thread is refused until you `reopen` it.
- **`inbox`** — an agent's mentions, newest first; `--unread` restricts to unread.
- **`task`** — the one promotion verb. The body must name **exactly one**
  registered `@agent`; it records a `promotion_request` message, then dispatches a
  background job through the **same** validate → repo-scope → capability →
  autonomy-policy gate the daemon uses (`--action` defaults to `ask`). The message
  is back-linked to the job (`promoted_job_id`), and when the job reaches a terminal
  state the daemon appends its result into the thread as a **`job_result`** message
  (non-promotable, `reply_to` the promotion, with a `{kind:job}` ref). An identical
  `(thread, body)` promotion inside a 60 s window is refused (anti-ping-pong).
- **`answer`** — the local answer channel for the **#445 ask-gate**. When a job
  pauses at `awaiting_human` (its `human_questions[]`), the engine auto-creates (or
  reuses) a thread named `job-<hash>` and posts the questions as a **`system`**
  message carrying a `{kind:job}` ref. `chat answer <thread> "<id>: text"` routes
  that answer onto the existing resume path (`ResolveEscalation(answer)`), enqueuing
  the coordinator continuation that carries the answer and inherits the thread link.
- **`close`/`reopen`** — archive (audit-preserving) / restore a thread.

**Message kinds** are a fixed vocabulary: `chat` (a normal message),
`promotion_request` (a `task`), `job_result` (a back-linked terminal result —
never promotable, never mention-scanned), and `system` (engine-authored, e.g. the
ask-gate questions). Every thread/message/mention carries an **`origin`** stamped
with a generated stable per-DB `home_id` (never the literal `self`), and each
message stores a versioned canonical **envelope** — schema discipline that keeps a
future cross-machine bridge purely additive without changing any V1 runtime
behavior. See `WORKFLOWS.md → Chat` for the human↔agent thread story.
## Routing Telemetry (Advisory)

Gitmoot records lightweight **execution-grounded routing telemetry** (#530): one
additive row per job at its terminal transition, capturing which combination
actually ran and how it turned out — `repo`, `action` (ask/review/implement/
continuation/…), `phase`, `runtime`, `model`, `agent`, resolved `template_id` +
commit, terminal `job_state` (succeeded/failed/blocked), result `decision` +
approval flag, a coarse tests-run count, `duration_ms`, and `input`/`output`
tokens (best-effort; a runtime that reports no usage contributes 0). Capture is
**always on, additive, and fail-safe**: it writes only to the new
`routing_telemetry` table and a telemetry error can never fail a job, so wire
output is unchanged.

**v1 is advisory only.** Nothing reads this back to change routing — no automatic
model/runtime override happens anywhere. It is a local feedback loop you inspect,
not a global benchmark. (This capture slice subsumes the phase-aware capability
telemetry proposed in #522.)

Inspect observed performance (read-only), grouped by `(action, runtime, model,
template)`:

```sh
gitmoot router summary [--repo owner/repo] [--action ask|review|implement] [--since 30d] [--json]
```

It reports per-group count, success rate, approval rate, median duration, and
summed tokens, always labeled **"local observed performance, not a benchmark"**.
`--since` accepts a Go duration or an `<N>d` days suffix.

Optionally feed a **bounded** (≤12-line) observed-performance table into a
**coordinator's** prompt so it can weigh which runtime/model/template has done
well on the repo. It is **off by default**; with it off, coordinator prompt
assembly is byte-identical and no telemetry query runs during a job:

```toml
[router]
context_enabled = true   # inject the advisory table into top-level coordinator prompts (default false)
```

The injected block carries the same "not a benchmark" disclaimer and is only added
to top-level (coordinator) jobs — a delegation child inherits its coordinator's
routing decision. Routing stays advisory: the block never forces a route.
