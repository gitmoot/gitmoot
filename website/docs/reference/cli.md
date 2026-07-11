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
before jobs stall on it. Run it after install and before starting the daemon.

One-shot onboarding: `gitmoot setup` registers the repo and an agent in one
command (`--repo owner/repo --agent <name> --runtime codex|claude|shell
--session <ref> [--role <role>] [--path .] [--start-daemon]`). `--repo`,
`--agent`, `--runtime`, and `--session` are all **required** — setup errors out
if any is missing; `--session` takes a runtime session reference, `last`, or a
shell command.
`--watch-issues` is **on by default** in setup, so the daemon comes up
tagging-ready for `@<agent>` issue mentions.

## Home And Config

Local state lives in the Gitmoot home (default `~/.gitmoot`): the SQLite store,
`logs/`, `workspaces/`, `evals/`, `artifact_blobs/`, and `config.toml`.

```sh
gitmoot init            # create the Gitmoot home
gitmoot config path     # print the config file location
gitmoot config show     # print the effective config
```

Set the `GITMOOT_HOME` environment variable (or pass the global `--home <path>`
flag, accepted by nearly every command) to relocate everything — useful for
isolated test homes.

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
declarative metadata — advertised capabilities, default model and effort values,
an advisory list of known-valid models, and a descriptor of where token usage is read from. Inspect
the resolved registry:

```sh
gitmoot runtime list
gitmoot runtime list --json
```

The values come from the compiled built-in defaults, overlaid with any
`[runtimes.<name>]` overrides in `config.toml`. Override a built-in runtime's
recorded metadata **without recompiling** — for example to retarget its default
model/effort or record its known models:

```toml
[runtimes.codex]
default_model = "gpt-5.5-codex"
default_effort = "high"
models = ["gpt-5.5-codex", "gpt-5.4-codex"]
capabilities = ["review", "implement", "ask"]
usage_source = "codex exec --json turn.completed usage"
```

Two fields are **behavioral**. `default_model` is the model fallback when neither
the agent nor the job pins `--model`: agent/job `--model`, then `default_model`,
then the runtime CLI's own default. `default_effort` follows the same precedence
after job/agent `--effort`; for Codex, Gitmoot emits
`-c model_reasoning_effort=<value>`. Claude and Kimi do not expose a reasoning
effort argument, so the resolved value is a no-op for those adapters. Every other
field is **inspection-only**, surfaced by `gitmoot runtime list` but changing
nothing at runtime: `models` is **advisory** (Gitmoot never rejects a `--model`
based on it), and `capabilities` gates nothing at dispatch. Adapter behavior
(auth, sandbox policy, session resume, stream parsing) always stays in Go. With
no `[runtimes.*]` section, and with both defaults unset, no model or effort is
forced.

A `[runtimes.<name>]` section can only tweak a **built-in** runtime's metadata; it
cannot add a new first-class runtime (that requires a code change). An unknown
runtime name is a config error surfaced by `gitmoot runtime list`.

## Runtime Launch Sandbox

```sh
gitmoot sandbox probe
```

`sandbox probe` prints whether this Linux host can enforce Gitmoot's strict
Landlock launch sandbox and includes the detected ABI. It runs the real hidden
re-exec shim and verifies both an allowed write and a denied outside write;
unsupported kernels return non-zero. Claude/Kimi `produce` pipeline stages require
this probe to pass and otherwise retain the explicit Codex-only refusal. Codex
produce remains on its native sandbox. Landlock confines filesystem writes but does
not govern network access; network policy remains the runtime CLI's. Wrapped Claude
may write its runtime-owned `$HOME/.claude` state and
`$XDG_CACHE_HOME/claude-cli-nodejs` cache; wrapped Kimi may write its runtime-owned
`$HOME/.kimi-code` state. Apart from runtime state/cache and standard device nodes,
only declared data paths, the disposable workdir, and temp roots are writable.

## Repo And Daemon Status

```sh
gitmoot status --repo owner/repo
gitmoot events --repo owner/repo
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

### Watched Repos

```sh
gitmoot repo add owner/repo --path <path> [--poll 30s]
gitmoot repo list
gitmoot repo remove owner/repo
gitmoot repo doctor owner/repo
```

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
imports valid feedback automatically.

To run a repo's queued jobs N-wide, use `--parallel N` (sugar for `--workers N
--scheduler pool`; it cannot be combined with `--workers` or `--scheduler`).
Raising `--workers` above 1 without an explicit `--scheduler` now **auto-selects
`pool`**, since multiple workers under `barrier` serialize same-repo jobs anyway;
an explicit `--scheduler barrier` is still honored. `gitmoot daemon status`
reports the live scheduler mode and worker count (e.g. `scheduler: pool, workers:
5`), and the daemon logs a preflight warning — with the exact relaunch command —
when ≥2 parallelizable jobs are queued under a serializing config. Same-repo
parallelism is bounded by **distinct runtime sessions** as well as distinct
checkouts; see [Run Jobs In Parallel On A Repo](../workflows/parallel-jobs-workflow.md).
One repo's concurrency can also be capped **from config, without any relaunch**,
via a `[repos."owner/repo"]` section with `max_parallel = N` (#576) — see
[Cap one repo's parallelism from config](../workflows/parallel-jobs-workflow.md#cap-one-repos-parallelism-from-config).

### Reconfigure Without Restarting

`kill -HUP <daemon-pid>` re-reads the `[daemon]` config section (`poll`,
`workers`, `scheduler`, parallelism) live (#577) — no teardown, no dropped
jobs, no environment re-inheritance. Values pinned by explicit launch flags win
over the re-read config. Prefer SIGHUP over a restart when only tuning
throughput.

### Runtime Auth Across Restarts

The daemon persists its Claude token into an owner-only (0600)
`daemon-runtime.env` file in the Gitmoot home (#578/#588). `gitmoot daemon
restart` recovers that token even when the invoking shell lacks
`CLAUDE_CODE_OAUTH_TOKEN`; a plain `daemon stop` + `daemon start` does **not**
recover it (start re-inherits the launching shell's environment). A recovered
token may be stale — verify with `gitmoot doctor`. A (re)start that would come
up without Claude auth warns loudly on stderr (non-fatal). `gitmoot daemon stop
--forget-runtime-auth` deletes the persisted file so a later restart cannot
recover the token.

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

In the one-shot styled output the dashboard leads with a "needs attention" block,
colors and truncates long lists, and groups near-identical runtime sessions;
`--all` shows everything. `--watch` redraws on an interval (default 5s) and cannot
be combined with `--json`, `--answer`, or `--dismiss`.

`gitmoot dashboard --web` serves the **read-only web dashboard** until
interrupted: a live orchestration/delegation graph plus whole-history Galaxy,
Jobs, Agents, Charts, and Health views with run summaries and prompt/output
inspection — the browser view of a running orchestration.
`--addr` sets the listen address (default `127.0.0.1:8080`). To expose it
beyond localhost, bind to an internal address and put an authenticating
reverse proxy in front — the dashboard itself has no authentication. See the
[Dashboard section](../dashboard/overview.md) for the views, routes, refresh
cadences, and mobile support.

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
  --effort high \
  --start-daemon
```

`agent start`, `agent subscribe`, and `agent type set` accept an optional
`--model <name>` flag that sets the agent's default runtime model. It is a
free-form, runtime-scoped string (a Codex, Claude Code, or Kimi Code model name)
with no allow-list; both `--model X` and `--model=X` are accepted. A per-job
`--model` (or a delegation's `model` field) overrides this default, and an
omitted model preserves the runtime's own default. The same default can be set
in config under `[agents.<type>].model`.

The same commands accept `--effort <value>` as the agent's default reasoning
effort, and `agent run`, `ask`, `implement`, `review`, and `orchestrate` accept it
as a per-job override. A delegation's `effort` field is the child-job override,
while an ephemeral worker spec's `effort` is that worker's default. The
resolution order mirrors model selection: job/delegation effort, agent/worker
effort, `[runtimes.<runtime>].default_effort`, then no explicit override. Values
are free-form pass-through strings. Codex receives
`-c model_reasoning_effort=<value>`; Claude and Kimi ignore the setting.

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
blocked). `read-only`/`ask`/`review` agents are unaffected.

`agent subscribe` accepts `--preset-delivery full|referenced|auto` (default
`full`), and `agent update <name> --preset-delivery <mode>` flips it in place on
an already-registered agent. The mode is a sticky per-agent preference:
re-running `agent subscribe` on an existing agent WITHOUT `--preset-delivery`
(e.g. to refresh its session/repo) preserves the stored mode; only brand-new
agents default to `full`. It controls how the agent's installed preset
(template) prompt is delivered on each job:

| mode | behavior |
|---|---|
| `full` (default) | always inline the full preset prompt every job — the pre-existing behavior, byte-identical |
| `referenced` | send a short "use your installed `<preset>` preset (commit `<c>`)" reference instead of the whole body, but only when Gitmoot has recorded that the exact resumed session already loaded the same preset at the same commit; any doubt (a new / `last` / fresh session, an unknown session, or a changed commit) falls back to full |
| `auto` | like `referenced`, and additionally only when the runtime persists sessions (`codex`/`claude`); `shell`/`kimi`/custom always send full |

The optimization is correctness-first and additive: the job payload **always**
snapshots the exact preset id, resolved commit, and content regardless of mode,
so auditability and retry determinism are unchanged, and a preset commit change
invalidates the recorded loaded-state so the next job re-sends the full preset.
Leave it at `full` unless you repeatedly resume a stable persisted session and
want to save preset tokens.

`--runtime` accepts `codex`, `claude`, `kimi`, or `kimi-cli`. Kimi Code is a
first-class runtime adapter alongside Codex and Claude Code; `kimi` targets the
current Kimi Code CLI (stream-json output) while `kimi-cli` is the opt-in
legacy Kimi CLI adapter (#546) — choose `kimi` unless you specifically run the
legacy CLI. The two count as the same runtime *family* for cross-family review.
Before starting a Kimi-backed agent, authenticate the Kimi CLI with
`kimi login`, then restart the Gitmoot daemon so it inherits the logged-in
session:

```sh
kimi login
gitmoot daemon restart
gitmoot agent start reviewer \
  --runtime kimi \
  --repo owner/repo \
  --path . \
  --role reviewer \
  --capability ask \
  --capability review
```

A Kimi agent's runtime reference must be a Kimi session id (`session_<uuid>`)
or empty; Gitmoot parses the session id from the Kimi CLI's stream-json output.

Subscribe an existing runtime session:

```sh
gitmoot agent subscribe reviewer \
  --runtime codex \
  --session <session-id-or-last> \
  --repo owner/repo \
  --role reviewer \
  --capability ask \
  --capability review \
  --model gpt-5-codex \
  --effort high
```

`agent subscribe` additionally accepts `--runtime shell`, the deterministic
no-LLM adapter whose `--session` is a **command** (the job prompt arrives as
`$1`; stdout must carry the `gitmoot_result` envelope) — useful for
deterministic end-to-end tests.

Inspect and manage agents:

```sh
gitmoot agent list
gitmoot agent show reviewer
gitmoot agent show reviewer --json
gitmoot agent repos reviewer
gitmoot agent allow reviewer --repo owner/other-repo
gitmoot agent deny reviewer --repo owner/other-repo
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
gitmoot agent implement lead --repo owner/repo --task task-002 --base origin/main "Implement from current origin/main."
gitmoot agent ask project-planner --repo owner/repo "Return the plan status."
gitmoot agent ask project-planner --repo owner/repo --background "Write the implementation plan and goal file."
gitmoot agent run lead --repo owner/repo --model gpt-5-codex "Implement this task."
gitmoot agent run lead --repo owner/repo --effort xhigh "Implement this task."
gitmoot job watch <job-id>
```

For `agent implement`, `--base <ref>` selects the commit used to create a new
branch worktree. `agent run` accepts the same flag when it routes to implement.
An `origin/*` ref is fetched before it is resolved, and an unknown ref fails
before a job is enqueued. `--base HEAD` explicitly follows the registered
checkout's current commit. On implement, `--head-sha <sha>` is a compatibility
alias for `--base <sha>`; passing both with different values is an error.

Set a default for implement dispatches in `config.toml`:

```toml
[workflow]
implement_base = "origin/main"
```

The flag wins over the config value. The config value `"HEAD"` keeps
checkout-following behavior. With no flag and no config value, Gitmoot still
uses checkout HEAD, but refuses when the checkout is on a non-default branch
that is behind `origin/<default>`. The error reports the branch and behind
count and offers both explicit choices: `--base origin/<default>` or
`--base HEAD`.

`gitmoot agent run`, `ask`, `implement`, and `review` (and `orchestrate`) accept
an optional `--model <name>` flag that pins the runtime model for that one job,
overriding the agent's configured default. It is a free-form, runtime-scoped
string (a Codex, Claude Code, or Kimi Code model name) with no allow-list; an
omitted `--model` leaves the agent's default model in effect. Both `--model X`
and `--model=X` are accepted.

`--effort <value>` and `--effort=<value>` select reasoning effort for one job
with the same job-over-agent-over-registry precedence. Gitmoot does not validate
an allow-list; Codex validates the forwarded value.

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
runtime. The same rule applies to `--effort`: an explicit job value belongs to
the override runtime, while the agent's default effort does not cross runtimes.
An unknown
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
gitmoot orchestrate project-planner "Plan and split this work." --repo owner/repo --effort high
gitmoot orchestrate project-planner "Review PR #123 in this repo." --repo owner/repo --recipe review-panel
```

`gitmoot orchestrate <agent> "..." [--repo R] [--recipe id]` is sugar for
`gitmoot agent run <agent> --background "..."`. It starts a conductor
(coordinator) that returns a `delegations[]` score; the players (child agents)
then run in parallel or in dependency order, and a finale (continuation)
reconvenes and synthesizes the results.
`--recipe review-panel|decompose-and-verify|verifier` (#477, also accepted on
`agent run`) routes the coordinator through a named built-in recipe prompt
without changing the agent's identity — see
[Coordinator Recipes](../workflows/coordinator-recipes-workflow.md).

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
gitmoot agent type set planner --effort high
gitmoot agent gc
```

`agent type set --model <name>` (or `[agents.<type>].model` in config) sets the
default runtime model for that managed agent type.

`agent type set --effort <value>` (or `[agents.<type>].effort`) sets its default
reasoning effort.

Schedule recurring agent work (heartbeats, off by default):

```sh
gitmoot agent heartbeat add repo-maintainer daily-status \
  --repo owner/repo --interval 24h --prompt "Daily status report." --enabled
# implement is policy-gated; --runtime pins a runtime for this schedule.
gitmoot agent heartbeat add builder nightly-tidy \
  --repo owner/repo --interval 24h --action implement --runtime codex \
  --prompt "Fix the top lint error and open a small PR."
gitmoot agent heartbeat list [--agent <agent>]
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
schedule's last-run/next-due/last-status. See
[Heartbeat Schedules](../workflows/heartbeat-schedules-workflow.md) for the
full reference.

A registered single instance **shadows** a managed type of the same name:
dispatch resolves `gitmoot agent <name>` to a registered single instance before a
type, so force the type with `--type <name>` (or do not register a single
instance of that name). Since **v0.5.1** a foreground `gitmoot agent ask <type>`
(the `ask` action) dispatches to the managed type synchronously; background
`run`/`review`/`implement` to a type and `[parallel_sessions]` temp-session
forking use the **background** path. See [Running one agent's jobs
concurrently](../concepts/agents-templates-jobs-locks.md#running-one-agents-jobs-concurrently).

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
#           gitmoot job close <id> --decision <approved|changes_requested|implemented|blocked|failed|skipped> --summary "..."]
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
at a PRIVATE repo unless the prompts are meant to be public.** See the
[template capture workflow](../workflows/template-capture-workflow.md#back-up-and-share-templates-via-github)
for the full flow.

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

### Recover a dead implement

If an implementer dies mid-work — its process exits after editing the task
worktree but before it commits, pushes, and opens a PR — the changes are left
uncommitted in the worktree. `task run` (and `agent implement`) refuse to
restart over that state rather than silently discard the work, and point you at
`task recover`:

```sh
gitmoot task recover task-001 --owner lead
gitmoot task recover task-001 --owner lead --repo owner/repo --skip-native-review-fanout --json
```

`--owner <agent>` is required and names a registered implement-capable agent,
attributed as the recovery lead. `--repo owner/repo` is optional and falls back
to the task's stored repo, so it is only needed when the task carries none.
`--skip-native-review-fanout` persists that flag before the PR is opened, and
`--json` prints the machine-readable recovery result.

`task recover` commits the full worktree state (`git add -A`, including
untracked non-ignored files), pushes the task branch, and opens or adopts the
task's PR — the finalize steps the dead implementer never reached. When the
worktree is already clean it recovers the commit already ahead of the base, and
refuses when there is nothing ahead to recover.

Two refusals guard recovery (and the `task run` / `agent implement` restart that
points to it):

- **Dirty worktree without an active job** — a restart refuses when the task
  worktree has uncommitted changes and no in-flight job. Inspect it, then run
  `task recover` to commit/push/open the PR, or clean/stash the worktree before
  retrying.
- **Live process still in the worktree** — `task recover` refuses while a live
  process is still inside the task worktree. Wait for it to exit, or stop the
  orphaned implementer, before recovering.

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
`escalate_human` or an ask-gate `human_questions` pause — see the
[result contract](result-contract.md) for the pause/resume semantics. See also
the [PR comment workflow](../workflows/pr-comment-workflow.md).

## Jobs And Locks

```sh
gitmoot job list --repo owner/repo   # add --json for machine-readable rows
gitmoot job show <job-id>            # add --json for the full job + why-stuck detail
gitmoot job watch <job-id>
gitmoot job events <job-id>
gitmoot job run <job-id>
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
gates <job-id>` lists them (open / satisfied); clearing a gate marks the blocker
resolved:

```sh
gitmoot job gates clear <job-id> --need "Maps API key"   # satisfy one need
gitmoot job gates clear <job-id> --all                   # satisfy every open gate
```

When the **last** gate is cleared, the blocked stage **auto-re-runs** via the same
`RetryJob` machinery `gitmoot job retry` uses (re-queued, then dispatched by the
daemon; downstream stages follow the normal delegation DAG) — resume happens on
clear, no polling. A **session job** (externally driven) and a stage whose tree is
**paused awaiting a human** (`escalate_human` / ask-gate) are never auto-resumed
even with all gates cleared — a resource gate must not bypass the human's `gitmoot
resume` decision; the command reports `not resumed: …` with the reason. A blocked
job with **no** `needs` records no gates and is byte-identical to before.

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
gitmoot job close <id> --decision approved|changes_requested|blocked|implemented|failed|skipped \
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
(`approved`/`changes_requested`/`implemented`/`skipped` -> succeeded, `blocked` -> blocked,
`failed` -> failed) and emits the same finished/failed/blocked event, so a recorded
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

For a `queued` or `blocked` job, `gitmoot job list` appends a `WHY:` column and
`gitmoot job show` prints a `why_stuck:` (and, when a lease applies, a
`next_retry_at:`) line explaining what the job is waiting on — e.g. `waiting on
runtime session lock runtime:codex:<ref> (held by job <id>)`, `blocked: awaiting
human`, `auth failing: …`, `throttled: …`, or `retrying: …` (#552). The reason
is derived from the most authoritative existing signal (the latest
reason-bearing `job events` entry plus the owning resource lock's lease); a
healthy job's output is unchanged.

Operational blockers auto-retry (#532): a delivery failure classified as
`runtime_auth` or `runtime_quota` does **not** fail the job terminally — the
daemon re-queues it as **deferred** with a bounded retry budget and a hold
until the earliest retry time. `gitmoot job show --json` carries the
`blocker_class` and attempt count, and over the `[events]` stream a
`job.deferred` follows the `job.failed` (making it non-terminal; see the
[event stream](event-stream.md)). A job that "failed then reappeared as queued"
is the deferral working, not a bug.

When a runtime session ends **without** producing a `gitmoot_result` envelope —
the CLI process crashed, exited non-zero, was signal-killed, or completed but
never emitted a valid envelope even after repair attempts — the job records
**failure diagnostics** (#806): a `phase` marker (`launched` = died before any stdout,
`streaming` = died mid-output, `result-parse` = every delivery completed but no
valid envelope was found), the process `exit_code` **or** terminating `signal`,
a **redacted** stderr tail (hard-capped at 2 KB; redaction runs over the full
text with the same token-redaction rules as job comments *before* the tail is
cut, so a secret can never leak partially), and the runtime session id when one
is known. `gitmoot job show` prints a `failure_diagnostics:` block,
`job show --json` carries `payload.failure_diagnostics`, and `gitmoot report
bug` includes a "Failure diagnostics" section. Successful jobs never store one,
and a retried job clears the previous run's crash report.

Jobs stuck in `running` are backstopped too: a running job with no lease
progress past the staleness window (default 30m) is assumed orphaned by a dead
worker and recovered/re-queued. The window is tunable via the
`GITMOOT_STALE_RUNNING_AFTER` environment variable; the smallest honored value
is 1m — below-1m, malformed, or non-positive values are rejected (with a
one-time warning) in favor of the 30m default rather than clamped (#560).

`gitmoot job kill <root-job-id>` is the operator kill switch for a runaway
delegation tree: it terminates the tree identified by its **root** job id
gracefully. In-flight jobs finish normally; the coordinator's next continuation
is routed through the graceful finalize path (synthesize what completed → stop)
and the daemon stops starting queued children of that root. See the
[termination bounds](result-contract.md#termination-bounds) for how it relates
to the other delegation backstops.

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
accepts a cancelled job, so a mistakenly dismissed one can be resurrected.

`gitmoot job cancel --state blocked` is the **bulk** form for clearing a backlog
of blocked jobs. Only `blocked` is accepted for `--state` (queued/running jobs
have single-job cancel; terminal jobs have retry). Narrow the selection with
`--older-than` (a Go duration like `168h`, or a convenience `<N>d` days suffix
like `7d`; age is measured from when each job became blocked), `--repo owner/repo`
(matches the job's payload repo), and `--agent name`. The bulk form is a
**dry-run by default** — it prints the matching jobs (id, agent, repo, age) and
exits without cancelling anything; pass `--yes` to actually cancel the selection.
Each selected job is dismissed through the same per-job `job cancel` path, so its
locks are released too. `<id>` and `--state` are mutually exclusive, and
`--older-than`/`--repo`/`--agent` require `--state`. `gitmoot doctor` warns when
blocked jobs older than 30d have piled up and prints this exact command.

To automate the sweep, set `[orchestrate].blocked_ttl` to a positive Go duration
(e.g. `blocked_ttl = "168h"`): the daemon's housekeeping tick then dismisses any
blocked job whose blocked-transition timestamp is older than the TTL, through the
same `CancelJob` abandon path (recording a distinct `blocked_ttl_expired` job
event so a TTL auto-expiry is distinguishable from a manual cancel). It is **off
by default** — an empty or `0s` value disables it (a negative value is rejected),
because a blocked job is a human-awaiting decision that is never auto-discarded
unless you opt in. This is distinct from `[orchestrate].escalation_ttl`, which
auto-finalizes a whole paused delegation *tree* and is on by default (24h);
`blocked_ttl` dismisses a *single* blocked job and is off by default.

Merge-gate retries are automatic while the daemon is running. Retryable states,
such as a busy base-branch merge queue or a GitHub branch update in progress,
are retried on the next daemon poll tick. The default poll interval is `30s`
unless the daemon was started with a different `--poll`. When an **external**
system owns the merge decision, set `GITMOOT_DISABLE_NATIVE_MERGE_GATE=1`
(also `true`/`yes`/`on`; #545): Gitmoot then **abstains** from its native merge
gate — fail-closed, it never merges gatelessly; the external gate makes the
call.

## Interactive Prompts

Pending interactive prompts (dashboard form questions, ask-gate questions) can
be answered from the CLI:

```sh
gitmoot interactive list [--state pending|resolved|all] [--json]
gitmoot interactive show <id> --json
gitmoot interactive answer <id> <value> [--source source]
gitmoot interactive clear <id> [<id>...] | --resolved | --all
```

## Result Checks

After a daemon-run job's `gitmoot_result` is parsed, Gitmoot runs a set of
**deterministic, LLM-free binary checks** over the parsed result — a
contract-hygiene audit that catches results that are technically valid but vague
or missing evidence. Each check is a yes/no question with an explanation:

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
  and attached to the job detail (`job show --json` `payload.result_checks` and
  the web dashboard), but the job still finishes on its own decision.
- `block` — a failing check additionally fails the job through the same terminal
  path a malformed result takes (opt-in, for strict workflows).
- `off` — the audit is disabled entirely, restoring byte-identical pre-feature
  behavior (no event, no payload field, no stored record).

A result that passes every applicable check records nothing, so the audit is
quiet on healthy jobs. Failed checks are also stored durably for later SkillOpt
consumption as structured feedback; there is no SkillOpt behavior change today.

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

Option generation in `skillopt train continue` is durable and idempotent on
resume. Each review item's artifacts, item row, and options commit in a single
transaction the moment that item finishes, so an interrupted generation phase
keeps every item that already completed instead of losing the whole batch.
Re-running `skillopt train continue` regenerates only the incomplete items:
completed items are skipped, no duplicate options are written, and finished work
is never rewritten. If an item has some but not all of its options persisted,
resume hard-errors with `item <id> has partial generated options; inspect or
clear review options before continuing` so you can inspect or clear that item
before retrying.

`skillopt train recover` repairs the optimizer phase of a session by default — it
re-imports or repairs the optimizer candidate package and classifies the
iteration as `already_completed_candidate`, `already_completed_no_candidate`,
`optimizer_active`, or `corrupted_unrecoverable`:

```sh
gitmoot skillopt train recover --session <id>
gitmoot skillopt train recover --session <id> --out-root .gitmoot/skillopt/<run-id>
gitmoot skillopt train recover --session <id> --json
```

`--out-root` overrides the optimizer output directory (it defaults to the
session's persisted optimizer path), and `--json` prints the recovery result as
JSON.

Pass `--generation` to recover the generation phase instead:

```sh
gitmoot skillopt train recover --session <id> --generation
gitmoot skillopt train recover --session <id> --generation --advance-state
gitmoot skillopt train recover --session <id> --generation --abort
```

This reclaims a generation lock stranded by a crashed/killed `train continue`
(whose deferred lock release never ran) and salvages the persisted per-item
options. Reclamation is liveness-gated: the lock is released only when its owner
PID is provably dead AND it was held on this same host. A live owner is refused
(`skillopt train generation is already running`) so you stop the running process
first; a cross-host owner requires the lock TTL to expire. The recover process
re-acquires the lock for itself so the salvage is crash-safe. Salvage is
import-only — it reports `expected_items`, `recovered_items`, and `missing_items`
and classifies the run as `generation_complete`, `generation_incomplete`, or
`generation_active`. The iteration advances to `options_generated` only with
`--advance-state` and only when every expected item is recovered (regenerating
missing items remains `train continue`'s job). `--abort` reclaims the lock and
leaves the phase at `items_ready`, keeping persisted items. A stale generation
lock also surfaces in `train status` as a `stale` active lock, separate from the
true current phase.

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

`skillopt pairwise import <packet-dir>` ingests a **blinded paired-review
packet** produced by the gitmoot-skillopt fork (the `pairwise-review.json`
packet plus its secret map and the reviewer's picks), de-blinds it, and stores
the pairwise-preference feedback events — the import path for Mode B's
paired-review evidence. The daemon can also import review-issue feedback
automatically when started with `--watch-skillopt-reviews`.

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
freezes it as reviewed static JSON. The pipeline is fully offline — no LLM
calls, so it is reproducible and testable: (1) **ground** each trait string
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
`evaluator_config['rubric']` — one dimension per metric, using the metric
`name` as the dimension key and its `definition` (plus the positive/negative
examples) as the description, with a weight the reviewer chooses. Because the
judge's `_compose_evaluator_rubric` already merges arbitrary
`evaluator_config['rubric']` dimensions and `_normalize_dimension_list`
accepts arbitrary names, adopting an induced rubric needs **zero judge-code
change and no result-contract bump**.

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

Agent persistent memory is **off by default** and enrolled per agent
(`[agents.<name>].memory = true`), with optional `[memory]` knobs (`disabled`,
`token_budget`, `max_entries`, and the distill-at-terminal knobs
`distill_at_terminal`, `distill_successes`, `distill_max_per_job`,
`distill_all_jobs`. All are read per tick, with no restart. See
[Agent Persistent Memory](../concepts/agent-memory.md) for the full model. The
inspection commands are read-only; `ingest` and `confirm` write behind a human
gate:

```sh
gitmoot memory list [--pending|--confirmed] [--agent NAME] [--repo owner/repo] [--json]
gitmoot memory recall "<query>" [--repo owner/repo] [--agent NAME|--shared] [--limit N] [--expand] [--json]
gitmoot memory replay [--agent NAME] [--repo owner/repo] [--limit N] [--json]
gitmoot memory eval --fixtures fixtures.json [--k N] [--json]
gitmoot memory vault export [--out DIR] [--agent NAME] [--force] [--json]
gitmoot memory vault import <DIR> [--dry-run|--yes] [--json]
gitmoot memory ingest <path|dir> --agent NAME [--shared] [--repo owner/repo] [--tier repo|general] [--dry-run] [--json]
gitmoot memory ingest sweep [--json]
gitmoot memory observations [--agent NAME] [--provenance-prefix P] [--json]
gitmoot memory confirm <obs-id>... | --provenance-prefix P [--agent NAME] [--to-shared] [--yes] [--json]
gitmoot memory retire --provenance-prefix P [--agent NAME] [--dry-run] [--yes] [--json]
gitmoot memory promote --to-shared <id>... [--json]
gitmoot memory links backfill [--dry-run] [--json]
gitmoot memory links list <id> [--json]
gitmoot memory groom --propose [--out PLAN.json] [--json]
gitmoot memory groom --yes --plan PLAN.json [--json]
gitmoot memory groom --split [--dry-run] [--json]
gitmoot memory groom --split-revert [--dry-run] [--parent N]... [--since RFC3339] [--json]
gitmoot memory clusters [--json]
gitmoot memory clusters recompute --propose [--out PLAN.json] [--json]
gitmoot memory clusters recompute --apply [--plan PLAN.json] [--json]
gitmoot memory cluster rename <cluster-id> <label>
```

`memory list` shows confirmed memories and/or pending observations. `memory
recall` runs the same FTS5/BM25 confirmed-memory retrieval used for prompt
injection and prints the matching facts in injection bullet format. Without
`--agent`, recall searches all agent owner pools plus the shared pool; pass
`--agent NAME` to inspect that agent's private pool plus shared, or `--shared` to
inspect only shared facts. Private matches outrank shared matches on equal BM25
scores, and a floor guard keeps a private match visible when shared rows would
otherwise fill the limit. Without `--repo`, recall searches every repo and
general-scope facts. `--repo owner/repo` narrows repo-scoped facts to that repo while still including
general-scope facts. `--expand` follows one hop of persisted memory links from
direct matches, appending visible linked facts after all direct matches and
marking their bullets with `[linked]`. `--json` returns raw rows for scripts,
including `author_ref` for shared facts that preserve a different author and
`linked_from` when a row came from link expansion. Prompt injection applies the
same link expansion automatically for enrolled agents, within the entry limit
and token budget, and non-empty memory blocks include a footer pointing the
agent at `gitmoot memory recall "<query>" --agent <agent-name>` for on-demand
search. Semantic or embedding search is future work; current retrieval stays
SQLite FTS5 plus persisted links. `memory replay`
re-renders recent real jobs' prompts with and without the injected
learnings block and reports the token/entry delta. `memory eval` computes
recall/precision@K of retrieval over a labeled `{agent, repo, instructions,
expected_keys}` fixtures file.

`memory vault export` renders confirmed memory as a **disposable, Obsidian-compatible
vault view**: one Markdown note per confirmed memory (sorted-key YAML frontmatter,
the content verbatim, and a `## Links` section of FTS co-occurrence plus persisted
`[[wikilinks]]`), a per-owner index note, and a `manifest.json` staleness anchor.
The vault is a **view, not a replica**: the SQLite store stays the only source of truth, so it is
regenerated from scratch on every export, safe to delete, and **deterministic**: the
same store yields byte-identical files (no `exported_at`; stable id-derived
filenames). The export is read-only and atomic (temp dir then rename over `--out`,
default a `vault/` directory under the home's evals area); `--agent` narrows it to a
single agent owner plus shared facts authored by that agent. Shared notes include
an `author:` frontmatter line when `author_ref` is set, so graph views still
attribute moved facts to the real author. Because the export **replaces `--out` wholesale**, it refuses to
overwrite a non-empty directory that is not itself a prior gitmoot vault (one with a
`manifest.json`), so an accidental `--out ~/my-obsidian-vault` can never delete your
own notes; pass `--force` to override.

`memory vault import <DIR>` is the **human curation gate**: export a vault, edit it in
any editor, then `import` **diffs the folder against a fresh export** and applies only
on confirmation. It regenerates a fresh export first and **aborts as stale** if the
store moved since the vault was written (manifest `snapshot_hash` mismatch). An
**edited** note updates its source memory's content via an optimistic **CAS on
`updated_at`** (exact-row, never key-based; resyncs FTS); a **deleted** note
**retires** its memory (additive `retired_at`/`retired_reason` + FTS removal — kept
for audit, never hard-deleted, and excluded from injection and future exports); a
**new** `.md` file (no `memory_id`) stages a **pending observation**
(`provenance=vault-import:<file>`, trust `normal`) behind the confirmation gate.
Frontmatter identity edits (key/scope/owner) are out of scope — detected, warned, and
skipped. `--dry-run` is the **default** (prints the diff, writes nothing); `--yes`
applies edits, retirements, and new observations in **one transaction**. If any note
fails to parse (e.g. broken YAML frontmatter), `--yes` **refuses to apply** so a
malformed note is never misread as a deletion. A vault produced by `export --agent
NAME` stays importable even when other owners have memories. The `<DIR>` positional
may sit before or after the flags.

`memory ingest` stages arbitrary Markdown as observations: it walks
`*.md`, strips leading YAML frontmatter, chunks a file only when its body exceeds
~512 estimated tokens (on `## ` headings, sub-splitting any still-oversized
section on paragraph/line boundaries so no chunk exceeds the budget), PreFilters
every chunk (per-reason rejection counts in the summary), dedups by exact content
**within the same scope+repo visibility domain** (identical text under a second
repo still stages), and inserts survivors with
`provenance = ingest:<relpath>` and **`trust_mark = low`**. `--tier` defaults to
`repo`; `general` is only chosen explicitly. `--shared` stages observations in the
shared pool while recording `--agent NAME` as the authoring identity.
Chunk keys are **stable**: `slug(file)-slug(heading)`, with an ordinal suffix
(`-2`, `-3`) only when a file/heading pair repeats within one sweep; the content
hash participates only in dedup, never in the key, so an edited note re-sweeps
onto the same key and updates its confirmed fact in place instead of spawning a
hash-suffixed sibling.
By default observations stay pending. If `[memory].ingest_auto_confirm = true`,
`memory ingest`, `memory ingest sweep`, and `chat remember` immediately confirm
the staged observation into the authoring agent's private pool only. They never
auto-confirm into shared; shared stays explicit through `confirm --to-shared` or
`promote --to-shared`. Auto-confirmed key-matched updates are
**supersede-preserving**: the prior edition is archived as a `superseded_by` row
(out of FTS, out of the vault, links unchanged on the live row) before the live
row is overwritten; manual paths (vault import CAS edits, `memory confirm
--yes`) keep plain overwrite semantics.
`memory ingest sweep` reads every configured `[[memory.ingest]]` source from the
current config at run time and runs the same ingest logic in-process for each one.
`--json` reports per-source `path`, `agent`, `repo`, `tier`, `inserted`,
`confirmed`, `skipped_retired`, `deduped`, `rejected`, and `error`, plus aggregate
totals. One bad source does not stop the rest; it exits non-zero only when the
config is invalid or every source fails. With no sources it exits zero with a
skipped note.
`memory observations` lists pending observations, flagging which keys are already
confirmed. `memory confirm` is the **human-gated promotion**: by id or
`--provenance-prefix`, it copies observations
into confirmed memory (idempotently), and without `--yes` only prints the plan.
`--to-shared` confirms selected observations into the shared pool while preserving
the observation author. `memory promote --to-shared <id>...` moves active
confirmed facts into shared, refuses retired or superseded rows, preserves
existing links, and stamps `author_ref` from the previous owner when needed.
`memory retire --provenance-prefix P` is the blast-radius undo for a collector
batch. It selects active confirmed rows whose provenance starts with `P`, scoped
optionally by `--agent NAME`, and is a dry run unless `--yes` is passed. Applying
the plan sets `retired_at` and `retired_reason` and removes the rows from FTS in
the same transaction. Retired keys are not resurrected by ingest or collectors on
re-ingest; only explicit human-controlled confirmation paths may revive a retired
key.
Ingested Markdown is an indirect-prompt-injection vector, so default installs keep
it inert at `trust_mark = low` until a human confirms it; nothing reads
`trust_mark` for a decision yet.

Confirming a fact also records up to three deterministic persisted links from that
confirmed row to active related confirmed memories. Links live in `memory_links`
with BM25-derived scores and do not rewrite fact content. Link candidates use the
same private-plus-shared visibility as prompt injection, so private facts can link
to shared facts and shared facts can link back through their author pool.
`memory links backfill` runs the same pass over the active confirmed pool in id
order; `--dry-run` reports what would be created, and repeat runs create nothing
new. `memory links list <id>` shows one fact's outgoing persisted links. Vault
export merges these persisted links with content-derived links and dedupes by
target in each note's `## Links` section.

`memory groom --split [--dry-run]` automatically partitions qualifying bricks at
deterministic byte-offset story seams into exact-substring children. List items,
`Why`, and `How to apply` sub-fields are not seams; length alone never cuts,
status/changelog content is excluded, and segments below 200 trimmed bytes merge
into a neighbor. The split supersedes and de-indexes the parent, carries its
cluster membership to the children, and gives each rendered child `(split from:
<parent-key>)` context in one CAS-guarded transaction.
`memory groom --split-revert [--dry-run] [--parent N]... [--since RFC3339]`
restores all active split parents by default. It retires, never deletes, children
only when their id-ordered content still reconstructs the original parent, then
restores parent FTS and the lowest-id child's current cluster. Changed groups skip
whole and repeat runs are no-ops. `memory groom` keeps all other curation as a
**propose → review → apply** round-trip. `--propose` reads active confirmed memory, computes the current
vault `snapshot_hash`, runs deterministic detectors
(status/changelog/ToC snapshots — short notes need a strong `STATUS:`/`… & deployed`
marker; bare to-do lists; exact duplicates scoped to the same owner/repo/scope;
over-long or strong-seam multi-story bricks are flagged when not already split;
seam-poor long prose remains flag-only; **legacy-key rekeys**
that migrate pre-stable-key rows ending in an 8-hex hash suffix, keeping the
newest edition under the stable key and retiring older siblings with reason
`rekey: superseded edition`; **cross-pool stale shared editions**, where a
strictly newer private fact matches a shared fact in the same repo and scope by
stable-key equality, or by a strong BM25 top-match that also shares a
`memory_links` edge, proposing promote-the-private-and-retire-the-shared with
reason `cross-pool: superseded by promoted edition`), and writes a reviewable
plan artifact — it touches nothing in the store. `--yes --plan` recomputes the
`snapshot_hash`, **aborts as stale** if the store changed since the proposal,
then applies the whole plan in one transaction: retirements (reason
`groom:<detector>`), rekey groups (FTS key column re-synced in the same
transaction), and cross-pool promote-and-retire pairs. Content is never edited,
and applying is idempotent (already-retired ids skip; a group whose rows changed
state skips whole). A ready-to-register
nightly proposal pipeline lives under
[`docs/examples/memory-groom-nightly`](https://github.com/jerryfane/gitmoot/tree/main/docs/examples/memory-groom-nightly).

`memory clusters` groups confirmed facts into **emergent communities** over the
fact-similarity graph (the same bm25 + id-tiebreak signal the vault `[[links]]` use),
retiring the dashboard's old fixed key-prefix "category" hubs. The community detection
is **id-ordered label propagation with lowest-label tie-breaks**, a pure function of
the graph, so the **same store yields byte-identical clusters, labels, medoids, and
ids**. A top-level cluster splits automatically at 20 facts when a second pass over
its internal graph yields at least two children of four or more facts. An existing
split remains above 12 parent facts while every child stays at least four; otherwise
it dissolves. Depth is capped at two levels. Labels are up to three distinctive terms (cluster term frequency weighted
against corpus document frequency), anchored to the cluster **medoid**; facts with no
neighbors fall into the reserved cluster **0 `unclustered`**. `recompute` is a
human-gated **propose → apply** round-trip: `--propose` writes a plan with a staleness
**anchor** over each active fact's `(id, updated_at)` and explicit planned splits or
dissolves; `--apply --plan` re-checks the
anchor, **aborts as stale** on drift, then rewrites the whole clustering in one
transaction (a bare `--apply` is allowed only on first run, when nothing exists to
protect). Confirming a new fact best-effort attaches it to the nearest neighbor's leaf
cluster; `memory cluster rename` sets an owner label override that wins over the
computed label and survives while that parent or child identity persists. The
Knowledge payload adds optional child `parent_id` values and renders a **repo →
cluster → subcluster → fact** hierarchy. Parent hubs are view-only aggregates.

## Pipelines

A pipeline (#681) runs a **declared DAG of shell stages** — a fixed, repeatable
multi-step flow — on demand or on an interval schedule. Each stage is an ordinary
queued job run through the **shell runtime**; a scan-based advancer folds each
stage's `gitmoot_result` **decision** and enqueues the stages whose `needs` have all
succeeded. Pipelines reuse the job queue, the result contract, and the heartbeat
scheduling idiom (durable `next_due`, overlap guard, missed-ticks-coalesce), and are
**off by default**.

Define a pipeline in a YAML file, then register and run it:

```yaml
name: nightly-sync          # required, name-safe token (letters, digits, - _)
repo: owner/repo            # optional to register; REQUIRED to run
schedule:                   # optional interval schedule (no cron in v1)
  interval: 24h
  jitter: 15m
stages:                     # the DAG, keyed by unique id and wired by needs
  - id: source
    cmd: "curl -sf https://example.com/data > data.json"
  - id: score
    cmd: "python score.py data.json"
    needs: [source]         # runs only after source SUCCEEDS
  - id: triage              # #757: an AGENT stage (exactly one of cmd|agent)
    agent: reply-triager    #   an existing managed agent, run as a read-only leaf
    action: ask             #   ask (default) | review — no implement
    prompt: "Triage the scored data; block if a human is needed."
    needs: [score]          #   upstream results are prepended to the prompt
  - id: deploy
    cmd: "rclone copy out/ r2:bucket"
    needs: [triage]
    timeout: 30m            # optional per-stage job timeout
    retry: 2                # optional; re-attempt a FAILED stage up to N times
```

```sh
gitmoot pipeline add nightly-sync.yaml --enable   # validate + store; omit --enable to add disabled
gitmoot pipeline install-defaults                 # install built-in memory pipelines, skipping existing names
gitmoot pipeline list [--json]
gitmoot pipeline show nightly-sync [--json]        # registry view for a name
gitmoot pipeline run nightly-sync                  # start a manual run; prints the run id
gitmoot pipeline show <run-id> [--json]            # run funnel for a "prun-…" id
gitmoot pipeline resume <run-id> [--from <stage>]
gitmoot pipeline cancel <run-id>
gitmoot pipeline enable|disable nightly-sync
gitmoot pipeline remove nightly-sync
```

`pipeline add` validates the whole spec at add time and stores the raw YAML
**verbatim** plus a content hash; each run snapshots the hash and executes its
snapshot, so editing the file later never mutates an in-flight run. It also
auto-creates one hidden shell runner agent (`pipeline-<name>-runner`) that owns the
**shell** stage jobs; it is hidden from `agent list` and disposed by `pipeline remove`. A
stage may instead set `agent` + `prompt` to run a named managed agent on its own
runtime. Four agent-stage kinds: **ask**/**review** (#757, read-only leaves);
**implement** (#768, `action: implement` + `write: true`; mutates the repo, with only
`implemented` promising a PR and waiting for its stamp; other configured successes
settle immediately; never auto-merges); **orchestrate** (#758, `orchestrate: true`; a sub-tree
coordinator that fans out owned children and folds the synthesis); and **gate** (#768,
`gate: pr_merged` + `source:`, no `agent`; a jobless waiter that folds when the source
implement stage's PR merges). A read-only stage's `needs` result summaries are prepended
to its prompt, and a repo-bound read-only agent stage runs in its own detached
read-only worktree so same-repo stages parallelize without touching the live checkout.

`pipeline install-defaults` installs the built-in `memory-ingest-sweep` and
`memory-groom-propose` pipelines. The daemon also runs this installer at startup.
It is idempotent: an existing pipeline with either name is skipped without
overwriting user-edited YAML, enabled state, or schedule. Empty memory pipeline
config installs manual-only definitions. Configure sources with `[[memory.ingest]]`
and intervals with `[memory.pipelines]`, or run them on demand with
`gitmoot pipeline run memory-ingest-sweep` and
`gitmoot pipeline run memory-groom-propose`. The installed ingest sweep has a fixed
two-stage shape that calls `gitmoot memory ingest sweep --json`, then summarizes the
totals. It reads `[[memory.ingest]]` at run time, so config edits apply on the next
manual or scheduled run without reinstalling defaults.
The installed groom pipeline has a fixed `split -> propose -> summarize` shape:
only the lossless split auto-applies, while the generated retirement/rekey/
cross-pool plan remains owner-gated.

A stage signals its outcome by printing a `gitmoot_result` blob to stdout; the
advancer folds by the **decision**, never the job's exit state (`changes_requested`
is a succeeded job but folds as a stage **failure** by default):

- a decision in the stage's `success_decisions` (default `approved`/`implemented`/`skipped`) ->
  **succeeded**, dependents enqueue;
- `blocked` → the stage blocks, its `needs` persist, the run **parks blocked**
  (downstream never enqueues, zero compute while parked);
- `failed` / any other decision / a cancelled job / no `gitmoot_result` → the stage
  **fails** (retried if budget remains), else the run **parks failed**.

`skipped` means the stage itself had no work and advances by default with a
`[skipped: no work]` summary marker. An explicit `success_decisions` list is
strict: omitting `skipped` makes it fail. A `pr_merged` gate whose terminal succeeded
source opened no PR parks blocked because there is nothing to wait for.

`pipeline run` prints only the run id (script-stable), ignores the `enabled` flag but
still needs a `repo` and refuses to start while a run is active. `pipeline show
<run-id>` renders the **text funnel** (`source OK -> score BLOCKED (needs: R2 token)
-> deploy SKIPPED`); a **failed** run also prints the exact `gitmoot report bug --job
<stage-job>` command (never auto-filed). `pipeline resume` re-runs a **parked** run
from its halted stage (or `--from`) plus its transitive dependents while **never**
re-running a succeeded stage. A pipeline stage is a **leaf**: a stage result carrying
`delegations[]` never spawns children. See
[Pipelines](../workflows/pipelines-workflow.md) for the full workflow.

## Native Chat (agent threads)

`gitmoot chat` (#534, V1 local-only) is a durable, repo-aware **conversation
ledger** where registered agents and the human talk in threads, `@`-tag each
other, and (explicitly) promote a message into a real job. It lives in local
SQLite — **zero network, zero entmoot dependency**. The core rule: **a message is
a row (free); a job is compute (explicit)**. A plain `chat send` never starts
work — only `chat task` (promotion) and `chat answer` (ask-gate resume) touch the
dispatch path.

```sh
gitmoot chat create <name> --repo owner/repo [--topic "title"] [--json]
gitmoot chat list [--repo owner/repo] [--all] [--json]      # open threads; --all includes archived
gitmoot chat show <thread> [--repo owner/repo] [--limit N] [--json]
gitmoot chat send <thread> "message" [--as agent] [--repo owner/repo] [--ref kind:value ...] [--json]
gitmoot chat remember <thread> <message-seq> [--repo owner/repo] [--tier repo|general] [--agent NAME] [--json]
gitmoot chat inbox <agent> [--unread] [--json]
gitmoot chat task <thread> "@agent message" [--action ask|review|implement] [--repo owner/repo] [--json]
gitmoot chat answer <thread> "<question-id>: answer text" [--repo owner/repo] [--json]
gitmoot chat close|reopen <thread> [--repo owner/repo] [--json]
gitmoot chat rename <thread> "new name" [--repo owner/repo] [--json]
```

- **`create`** — `<name>` is slugified to a topic-path-safe handle (`[a-z0-9-]`,
  no `+`/`#`/`/`; unique per repo). `--repo` is required; `--topic` sets the human
  display title. The slug is the stable handle — a later `rename` changes only the
  title.
- **`send`** — appends a `chat` message. `@agent` mentions land in a **registered**
  agent's unread **inbox**; an unknown mention is recorded for audit with a stderr
  warning and **never fails the send**. `--as <agent>` authors as a registered
  agent (default: the human); `--ref kind:value` attaches structured refs.
- **`remember`**: captures exactly one existing message by sequence as a memory
  observation with deterministic provenance `chat:<thread-id>#<seq>`. It stores
  the message body verbatim, applies the memory PreFilter, and dedups by content
  hash in the target scope/repo. It does not scan for natural-language prefixes
  or bulk-mine a thread. `--agent` defaults to `lead`; if
  `[memory].ingest_auto_confirm = true`, the observation is confirmed into that
  agent's private pool only.
- **`inbox`** — an agent's mentions, newest first; `--unread` restricts to unread.
- **`task`** — the one promotion verb. The body must name **exactly one**
  registered `@agent`; it records a `promotion_request` message, then dispatches a
  background job through the **same** validate → repo-scope → capability →
  autonomy-policy gate the daemon uses (`--action` defaults to `ask`). The message
  is back-linked (`promoted_job_id`), and the terminal result is appended into the
  thread as a **`job_result`** message (non-promotable, `reply_to` the promotion).
  An identical `(thread, body)` promotion within 60 s is refused (anti-ping-pong).
- **`answer`** — the local answer channel for the **ask-gate** (#445). When a job
  pauses at `awaiting_human`, the engine auto-links a `job-<hash>` thread and posts
  the questions as a **`system`** message with a `{kind:job}` ref; `chat answer`
  routes the answer onto the existing resume path and enqueues the coordinator
  continuation that carries it.
- **`close`/`reopen`** — archive (audit-preserving) / restore a thread.

**Message kinds** are a fixed vocabulary: `chat`, `promotion_request`,
`job_result` (never promotable), and `system`. Every row carries an `origin`
stamped with a generated stable per-DB `home_id` (never the literal `self`) and a
versioned canonical envelope — schema discipline that keeps a future cross-machine
bridge additive without changing any V1 behavior. See
[Chat](../workflows/chat-workflow.md) for the full workflow.
## Routing Telemetry (Advisory)

Gitmoot records lightweight **execution-grounded routing telemetry**: one additive
row per job at its terminal transition capturing which combination actually ran and
how it turned out — `repo`, `action`, `phase`, `runtime`, `model`, `agent`, resolved
`template_id` + commit, terminal `job_state`, result `decision` + approval flag, a
coarse tests-run count, `duration_ms`, and input/output tokens (best-effort). Capture
is **always on, additive, and fail-safe**: it writes only to the new
`routing_telemetry` table and a telemetry error can never fail a job.

**v1 is advisory only** — nothing reads this back to change routing, and no automatic
model/runtime override happens anywhere. It is a local feedback loop you inspect, not
a global benchmark.

Inspect observed performance (read-only), grouped by `(action, runtime, model,
template)`:

```sh
gitmoot router summary [--repo owner/repo] [--action ask|review|implement] [--since 30d] [--json]
```

It reports per-group count, success rate, approval rate, median duration, and summed
tokens, always labeled **"local observed performance, not a benchmark"**. `--since`
accepts a Go duration or an `<N>d` days suffix.

Optionally inject a **bounded** (≤12-line) observed-performance table into a
**coordinator's** prompt. It is **off by default**; with it off, coordinator prompt
assembly is byte-identical and no telemetry query runs during a job:

```toml
[router]
context_enabled = true   # inject the advisory table into top-level coordinator prompts (default false)
```

The injected block carries the same "not a benchmark" disclaimer, is added only to
top-level (coordinator) jobs, and never forces a route — routing stays advisory.

### V1.5 — auto-respond, `chat wait`, and `moot`

V1.5 (#534) adds the **agent-to-agent** layer. Both additions are **off by
default** and keep anti-ping-pong **structural**: only a `kind=chat` message with a
resolved mention triggers work, and every back-linked reply is a non-triggering
`kind=job_result`.

**Auto-respond sweep** — an opt-in daemon-tick sweep that lets an enrolled agent
answer an `@mention` without a human running `chat task`, enqueueing **one** bounded
read-only `ask` per unread mention through the same dispatch gate as `chat task`. It
is a no-op (zero chat-table queries per tick) unless **both** the global switch and a
per-agent opt-in are set: `[chat] auto_respond = true` and
`[agents.<name>] chat_autorespond = true`.

| `[chat]` knob | Default | Meaning |
|---|---|---|
| `auto_respond` | `false` | Global kill switch; `false` overrides every per-agent opt-in. |
| `auto_respond_cap` | `4` | HARD cap on auto-responses per (thread, agent). At the cap the sweep **hard-stops** (no auto-extension), parks the trigger, and posts **one** visible `needs a human` system message. |
| `auto_respond_cooldown` | `2m` | Minimum spacing per (thread, agent); a trigger inside the window is deferred (left unread to re-fire), never dropped. |

**`chat wait`** — a blocking read verb for moot turn-taking: it polls until the
thread has a message with `seq > --since-seq`, then prints the new messages plus a
`last-seq: N` line (feed `N` back as the next `--since-seq`). On a capped moot thread
it returns immediately with the wrap-up line instead of spinning to the timeout.

```sh
gitmoot chat wait <thread> [--since-seq N] [--timeout 90s] [--repo owner/repo] [--json]
```

**`gitmoot moot`** — convene N registered agents as **seats** in one bounded
brainstorm. Each seat is **one** background read-only `ask` job through the same
validate → repo-scope → capability → policy gate as `chat task`; seats converse via
`chat send` / `chat wait`, so the cost is exactly one job per seat regardless of
message count. Messages are rows (free).

```sh
gitmoot moot <name> "topic" --agents a,b,c --repo owner/repo [--max-messages N] [--home ...] [--json]
```

Every seat must be **registered**, **repo-scoped**, and carry the **`ask`**
capability, or the moot is rejected before any thread or seat is created. The moot
**HARD-STOPS** at its agent-message cap (no auto-extension): at the cap
`chat send --as` is refused, **one** visible `MOOT CAP REACHED` overrun system
message is posted, and each seat wraps up by returning its **partial conclusions**
(what it knows / is unsure of / would ask next) as its `gitmoot_result`, which arrive
via the `job_result` back-link path (the cap never blocks those). Human sends and
seat conclusions are never gated by the cap.

| `[chat]` knob | Default | Meaning |
|---|---|---|
| `moot_max_seats` | `6` | Max agents one moot may convene; a larger roster is rejected. |
| `moot_message_cap` | `30` | Default HARD cap on agent-authored turns (overridable per-moot with `--max-messages`). |

`[chat]` is optional: with no `[chat]` section every knob resolves to its default,
`auto_respond` stays off, and the daemon tick is byte-identical.
