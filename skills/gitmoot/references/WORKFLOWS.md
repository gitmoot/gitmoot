# Gitmoot Workflows

## First Repo Setup

The supported one-liner is `gitmoot setup`, which registers the repo and an
agent in one command (`--watch-issues` defaults on, so the daemon comes up
tagging-ready). `--repo`, `--agent`, `--runtime`, and `--session` are all
**required** (setup exits with an error if any is missing); `--session` takes a
runtime session reference, `last`, or a shell command:

```sh
gitmoot setup --repo owner/repo --agent reviewer --runtime codex --session last --start-daemon
```

Or the manual path:

1. Confirm the repo identity and GitHub auth.
2. Run `gitmoot doctor` to validate `gh auth` and runtime credentials before
   anything can stall on them.
3. Start the daemon.
4. Start or subscribe at least one agent.
5. Verify the agent is healthy before asking PR comments to route jobs.

```sh
gh repo view --json nameWithOwner
gh auth status
gitmoot doctor
gitmoot daemon start
gitmoot agent start reviewer --runtime codex --repo owner/repo --role reviewer --capability ask --capability review --start-daemon
gitmoot agent doctor reviewer
```

## Review Agent From A PR Comment

1. Register a reviewer agent for the target repo.
2. Ensure the daemon watches the same repo.
3. Comment on the PR.
4. Inspect job status if no result appears.

```text
/gitmoot reviewer review
```

```sh
gitmoot job list --repo owner/repo
gitmoot job show <job-id>
gitmoot job events <job-id>
```

## Built-In Thermo Review Agent

Use the thermo template for strict review-only work. It should not implement code
or request implementation capability.

```sh
gitmoot agent template update thermo-nuclear-code-quality-review
gitmoot agent start thermo-review --runtime codex --repo owner/repo --template thermo-nuclear-code-quality-review --start-daemon
```

PR comment:

```text
/gitmoot thermo-review review
```

## Custom Prompt Agent

Use custom prompt agent templates for project-specific reviewers or helpers.

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

Custom template content is snapshotted into local Gitmoot state. After editing the
source template file, run `gitmoot agent template diff <id>` and `gitmoot agent template update
<id>` before expecting new jobs to use the changed prompt.

## Current-Chat Planner

Use the same `planner` template in the current Codex or Claude chat when the
user wants a fast implementation plan and the current session already has the
repo context. Load the prompt with `gitmoot agent prompt planner` when it is
cached, or read the packaged `agent-templates/planner.md` instructions from the
Gitmoot skill package. Inspect only the relevant files, use web search only for
current external contracts or best-practice claims, and return the plan directly
in chat.

```text
Use the Gitmoot planner here. Write a task-by-task implementation plan for this feature.
```

If the user asks for a standard goal file, read the canonical goal template and
write the goal file. Do not create a goal file unless explicitly requested.

## Current-Chat Custom Agent Prompt

Use a registered agent or custom template in the current chat when the user says
something like:

```text
Use frontend-reviewer here. Review this diff.
```

Resolve and load the prompt with:

```sh
gitmoot agent prompt frontend-reviewer
```

Treat the returned content as instructions for the current chat. This is prompt
import, not true system-prompt injection, and it does not create a Gitmoot job,
start a daemon, resume a runtime session, or post a PR comment. If the user
wants tracked background execution, use `gitmoot agent ask <agent> --background`
instead.

## Current-Chat Template Capture

Use template capture when the user wants to turn a successful visible Codex or
Claude Code conversation into a reusable agent template.

```text
Use Gitmoot to capture this session as agent template release-planner. Draft only.
```

The current chat reads `references/TEMPLATE_CAPTURE.md`, extracts durable
workflow rules from visible conversation context and inspected files, and writes
or returns a draft. It must not route the request through `gitmoot agent ask`,
start a daemon, queue a job, or install/replace a template without explicit user
approval.

For a blank starting point, scaffold the required sections:

```sh
gitmoot agent template draft release-planner
```

After the user reviews the draft:

```sh
gitmoot agent template validate .gitmoot/templates/release-planner.md
gitmoot agent template add release-planner --file .gitmoot/templates/release-planner.md
gitmoot agent prompt release-planner
```

The capture pieces are distinct:

- `agent template draft`: scaffold a blank structure.
- "capture here": current chat fills that structure from visible context.
- `agent template validate`: structural check.
- `agent template add`: install a snapshot.
- `agent prompt`: reuse the installed prompt in the current chat.
- `agent start --template`: create a runnable background agent instance.

## Background Planner Agent

Use the planner template when the user wants a structured implementation plan or a
standard Gitmoot goal file to run as a tracked Gitmoot background agent job.

```sh
gitmoot agent template update planner
gitmoot agent start project-planner \
  --runtime codex \
  --repo owner/repo \
  --path . \
  --template planner \
  --start-daemon
```

Ask from a PR comment:

```text
/gitmoot project-planner ask Write a task-by-task implementation plan for this feature, then create the goal file prompt.
```

Ask directly from a local Codex or Claude Code chat by having the runtime call
the Gitmoot CLI when the user explicitly wants a registered background-capable
agent path:

```sh
gitmoot agent ask project-planner --repo owner/repo --background "Write a task-by-task implementation plan for this feature, then create the goal file prompt."
gitmoot job watch <job-id>
```

If the Codex plugin exposes a Gitmoot command bridge in chat, the equivalent
form is `$gitmoot:gitmoot agent ask project-planner --repo owner/repo --background "..."`. The
important part is that background planner work goes through `gitmoot agent ask`;
fast "here" planning stays in the current chat and uses `gitmoot agent prompt`
only to read prompt content.

If the planner writes a goal file and the user wants Gitmoot to track it, import
it explicitly:

```sh
gitmoot goal import --file GOAL-feature.md --repo owner/repo
```

## SkillOpt Train Mode

Use `gitmoot skillopt train` when a user wants Gitmoot to enforce the complete
template-learning loop. Use low-level `skillopt review`, `feedback`, `export`,
`import`, and `candidate` commands only for advanced/debug work, custom
research runs, or one-step recovery.

To scaffold a reusable train config, run `gitmoot skillopt train init`. On an
interactive terminal it runs a line-oriented wizard (numbered template choices
with a "Custom file" option, and the preview style); each question is also a
prompt record an agent can answer with `gitmoot interactive answer`, or pass
`--prompts` to emit them all at once. It prints the next `train start --config
... --workspace-repo <owner/repo>` command. See
[../../../docs/skillopt-train-workflow.md](../../../docs/skillopt-train-workflow.md)
and the optional Herdr-pane flow in
[../../../docs/herdr-composable-train-init.md](../../../docs/herdr-composable-train-init.md).
Use `gitmoot dashboard` (add `--json`) to watch train phase, pending prompts,
jobs, and daemon health.

```sh
gitmoot skillopt train start \
  --template planner \
  --session planner-train \
  --repo owner/product \
  --workspace-repo owner/product-workspace \
  --preview-repo owner/product-previews \
  --request "Improve release planning answers from reviewer feedback" \
  --items-file train-items.yml \
  --mode explore \
  --exploration-level high \
  --options 4 \
  --preferred-gate hard_then_soft \
  --yes

gitmoot skillopt train status --session planner-train
gitmoot skillopt train continue --session planner-train
```

Train sessions contain one or more iterations. Each iteration pins a base
template version, owns one eval review run, stores workspace/preview metadata,
collects ranked or A/B feedback, exports a training package, imports one
pending optimizer candidate, and records an explicit promote/reject/abandon
decision. The next iteration starts only through:

```sh
gitmoot skillopt train continue --session planner-train --start-next
```

If the previous candidate was promoted, the promoted candidate version becomes
the next base. Rejections require a reason. Manual append-style next iterations
are not part of the train workflow.

Preview modes are explicit. Text-only sessions use `preview.mode=none`,
`preview.renderer=none`, and `preview.publisher=none`; GitHub issues contain
inline review content. Landing-page preview sessions use
`--preview-repo owner/previews`, which defaults to `preview.mode=required`,
`preview.renderer=vue-vite`, `preview.publisher=github-pages`, and
`review.expected_repo=owner/previews`. Register the preview checkout first:

```sh
gitmoot repo add owner/previews --path /path/to/previews
```

Run `train continue` once to generate Vue/Vite bundles and a second time to
publish GitHub Pages previews and create the human review issue. Required
previews block inline fallback until every option has `preview_url`; optional
previews use URLs when present and fall back to inline Markdown only when
preview publication is unavailable.

Check `gh auth status --hostname github.com` and
`gh repo view owner/previews --json nameWithOwner` before GitHub-backed review
publication or watching. Gitmoot preflights GitHub issue/comment operations,
but preview publication can push Pages files before a later review issue
preflight fails.

Required Vue/Vite options are validated immediately. An actionable preview
bundle validation failure retries that option once while keeping valid sibling
options. GitHub Pages publication records `preview_commit`, `preview_status`,
and `preview_status_reason`; review links can show `open`,
`pending deployment`, `failed deployment`, or `stale deployment`. Existing
review links keep their recorded status label; `train continue` skips options
that already have a preview URL.

Low-level GitHub feedback publish/sync commands enforce the train run's expected
review repo. Candidate decisions are explicit: promote, reject with a reason,
wait, or reject and `--start-next` to keep improving. LaTeX/PDF, Storybook,
notebook, image, and other preview types are future adapters, not current
renderer/publisher pairs.

At each manual promote/reject decision, Gitmoot records the judge↔human outcome
into a local store, tagged with the judge prompt version/id/hash that produced
the score. All four directions are captured — `agree_accept`, `agree_reject`,
`judge_accept_human_reject` (false positive), `judge_reject_human_accept` (false
negative) — so you can measure agreements as well as disagreements. Capture is
measurement only: it never alters the judge, overrides the decision, or bumps
the result contract, and a capture error never fails the decision.

Inspect calibration with `gitmoot skillopt judge-report [--template <id>]
[--home <path>]`. It is read-only and prints a confusion matrix over the four
directions, the agreement rate and Cohen's κ, calibration buckets (judge
soft-score versus the human decision), and per-dimension disagreement — useful
to tell whether the LLM judge is well-calibrated against human verdicts before
you trust it to gate candidates.

`gitmoot skillopt judge agreement [--template <id>] [--json]` extends the
measurement to the pairwise slice: it joins the A/B judge rows (`skillopt ab
--judge` / jury) against human ranked/pairwise picks on the same
**comparison** (each `skillopt ab` invocation stamps a shared per-comparison
token on all of its rows; older tokenless rows are excluded and counted as
unmeasurable, never pooled by challenger) and reports Cohen's κ as the
headline, raw agreement, per-source/per-juror breakdowns, and an
assignment-corrected position-bias audit (stratified by the champion's
presented position, reported alongside `P(pick=a)` and
`P(option A = champion)`; undefined when a fixed `--seed` pinned the champion
to one position), with a loud small-sample warning. Read-only.

Those captured outcomes feed judge-prompt optimization. The contract carries a
per-`task_kind` judge prompt
(`evaluator_profile.judge.config.judge_prompt_templates[task_kind]` +
`judge_prompt_version`), and gitmoot-skillopt v0.3.0 can tune it offline against
held-out human verdicts with `gitmoot-skillopt optimize
--judge-prompt-optimization --judge-human-labeled-path <labels.json>` — the
freeze-and-alternate counterpart to skill optimization, accepting a candidate
judge prompt only when it raises held-out human agreement.

Run the deterministic smoke before changing train behavior:

```sh
scripts/skillopt-train-smoke.sh
```

## SkillOpt Ranked Exploration

Use ranked exploration when a template needs broad search before final
validation. Keep final promotion decisions on fresh A/B validation items unless
the user explicitly asks for a different evaluation design.

1. Explore with four to six diverse options per item.
2. Rank every option and capture useful/rejected traits.
3. Refine with two to three combined candidates once a direction stabilizes.
4. Distill the accumulated feedback into a candidate template.
5. Validate current template vs candidate on fresh A/B items.

Landing-page example:

```sh
gitmoot skillopt review create \
  --template landing-page-designer \
  --repo owner/gitmoot-web \
  --run landing-page-explore-001 \
  --mode explore \
  --exploration-level high \
  --options 4

gitmoot skillopt review item add \
  --run landing-page-explore-001 \
  --item hero-001 \
  --title "Gitmoot landing page hero" \
  --option a=previews/hero-a.md \
  --option b=previews/hero-b.md \
  --option c=previews/hero-c.md \
  --option d=previews/hero-d.md

gitmoot skillopt feedback markdown export \
  --run landing-page-explore-001 \
  --output .gitmoot/evals/landing-page-explore-001
```

Non-visual writing tasks use the same shape with text artifacts:

```sh
gitmoot skillopt review create \
  --template x-post-writer \
  --repo owner/content-workflows \
  --run x-post-style-explore-001 \
  --mode explore \
  --options 5

gitmoot skillopt review item add \
  --run x-post-style-explore-001 \
  --item thread-hook-001 \
  --option a=posts/hook-a.txt \
  --option b=posts/hook-b.txt \
  --option c=posts/hook-c.txt \
  --option d=posts/hook-d.txt \
  --option e=posts/hook-e.txt
```

After importing feedback, inspect:

```sh
gitmoot skillopt review status --run landing-page-explore-001
```

Only run the external optimizer when the user wants a candidate update and the
status recommendation is stable enough for the current phase. Do not run heavy
SkillOpt optimization after every tiny feedback round by default.
Before launching it, verify `gitmoot-skillopt --version` and
`gitmoot-skillopt optimize --help`; missing or broken installs should be
handled as configuration blockers.

## Automatic Trace-Harvested Feedback (Mode A, off by default)

For hands-off template learning, the daemon can derive feedback from the
**verifiable outcomes** an implement job reaches instead of waiting for a human
ranking. Enable it per host in `[skillopt]`:

```toml
[skillopt]
auto_trace_enabled = true              # off by default
cross_family_review_enabled = false    # off by default; also needs auto_trace_enabled
revert_detection_enabled = true        # unset = on when auto_trace_enabled; set false to opt out (#467)
deterministic_checkers_enabled = false # off by default; also needs auto_trace_enabled (#485)
deterministic_checkers = diff_size     # optional comma list; default = diff_size only
```

With `auto_trace_enabled = true`, a merge (passing CI vs. empty-gate), a
merge-gate block, a review `changes_requested`, or a later **revert** (the daemon
detects a merged GitHub Revert-button PR — body `Reverts owner/repo#NN` — and
overwrites the original PR's positive with a negative in place; gated additionally
on `revert_detection_enabled`, unset = on, set `false` to keep the harvester on
but turn revert overwrites off) is projected into a synthetic
`FeedbackEvent` (`reviewer = gitmoot-auto`, `feedback_source = automatic_trace`)
in a per-template-version `auto-trace:<version>` eval run. It is additive
(`contract_version` stays `1`), best-effort (a failure records an
`auto_trace_harvest_failed` job event, never blocks a job), and **never
promotes** — a human still promotes a candidate. With the knob unset, no
harvester runs and behavior is byte-identical.

### Cross-family review agent (soft quality + scope-fidelity signal)

Turning on **both** `auto_trace_enabled` and `cross_family_review_enabled` adds a
read-only **cross-family review leg** on every merge: a reviewer of a *different
runtime family* than the implementer (codex→claude, claude→codex, kimi→claude —
preferring a registered review-capable agent of another family scoped to the
repo, else an ephemeral different-family read-only leg in the `verifier.md`
style) scores subjective quality + scope-fidelity as one rubric (coverage /
containment / fidelity + architecture / readability / abstraction, each in
`[0,1]`). The rubric becomes a **second**, judge-tagged, down-weighted
`FeedbackEvent` in the SAME `auto-trace:<version>` run under a distinct item id
(`review#<repo>#<pr>`) and the fixed `gitmoot-review` reviewer sentinel, so it
never overwrites the verifiable floor — and so a re-review by a different reviewer
family overwrites in place rather than accumulating a stale duplicate. The mapped
mean **drives the a/b choice** (a below-`0.5` mean is a non-baseline `b` vote, not
a baseline win) and an empty rubric writes no row. Weight tiers: **human gold >
verifiable floor > cross-family judge > same-family judge.**

The review leg runs **off the blocking merge path** and is best-effort — a
failure records a `cross_family_review_failed` job event and never blocks the
merge. When no different-family reviewer is available it falls back to a
**same-family** reviewer *with a warning* (a `cross_family_review_samefamily_fallback`
job event, a review item tagged `self_family = true` with `reviewer_runtime`
carrying the family so it weights below a cross-family review); only when no
review-capable runtime is authed at all is the review skipped. The rubric text is never shown to the
implementer (anti-gaming). Promotion stays manual (the harvester writes only
eval/feedback rows); the signal is weighted-low + judge-tagged and is subject to
the configurable `[skillopt].auto_promote` policy (#463) when that lands — it is
not barred from promotion. See
`skills/gitmoot/references/RESULT_CONTRACT.md` for the full contract.

### Deterministic checkers (objective, non-LLM signal)

Turning on **both** `auto_trace_enabled` and `deterministic_checkers_enabled`
(#485) adds an **objective, tool-measured** checker leg on every merge — the
complement to the subjective cross-family review. It runs plain external tools
(`dupl`/`jscpd` for duplication, `golangci-lint` for lint, `gocyclo` for
cyclomatic complexity) plus a **pure-Go `diff_size`** metric (parsed from the PR
patches; always available, no tool/checkout needed), normalizes each to `[0,1]`,
and writes a **third** `FeedbackEvent` in the SAME `auto-trace:<version>` run under
the fixed `gitmoot-checker` reviewer sentinel + a distinct item id
(`checker#<repo>#<pr>`), so it coexists with both the verifiable floor and the
cross-family review. The `deterministic_checkers` comma list selects which run
(default `diff_size` only). It is **fail-safe**: a missing tool, no checkout, a
tool error, or a timeout SKIPS that one dimension (never the harvest, never the
merge); an all-skipped run writes no row. The dimensions are objective and
un-gameable, tagged `objective = true`, additive (`contract_version` stays `1`),
and never promote. With the knob unset, no checker leg runs and behavior is
byte-identical.

## Champion-Challenger A/B (Mode B, off by default)

For **ask / research** agents — which have no verifiable PR/CI/merge outcome —
use the manual champion-challenger A/B to capture a **pairwise preference**
(#473):

```sh
gitmoot skillopt ab planner-bot "Plan the data migration." --seed 42
```

It resolves the **champion** (the agent template's current promoted version) and
a **challenger** (the sole pending candidate, or `--challenger <versionId>`),
delivers **both** through the runtime adapter **serialized**, shows the two
answers **label-shuffled** as Option A / Option B, and records the human pick
(`--pick a|b`, or interactively). With **no pending challenger** it is a clean
no-op. The pick writes a 2-option `eval_run` + two `eval_review_options`
(`champion`/`challenger`) + one `RankedFeedbackEvent` **per pick** (`source =
skillopt-ab`, `contract_version` stays `1`; a unique per-pick `source_url` makes
repeated A/Bs of the same challenger each persist as a distinct row instead of
overwriting one), updates the per-variant **Beta-Bernoulli bandit**
(`skillopt_bandit_arms`), and prints `P(challenger > champion)` as
`NN% likely better over K samples`.

That confidence feeds the **`[skillopt].auto_promote_min_confidence`** guardrail
(nil default = ignored; set = require `confidence >= floor` over enough samples,
else fail safe to notify-only) — supplying the promotion confidence Mode A could
not provide for ask agents. For a genuine ask candidate (no harvester score or
feedback rows) the bandit pulls stand in for the Mode A sample/score floors, so the
confidence gate alone can auto-promote it. `bandit_min_samples` (default 30) gates
both the manual-floor read and live interception; the manual A/B is always allowed.

### Live-traffic A/B (off by default, #482)

The same champion-vs-challenger comparison can fire **automatically** on a
**sampled fraction** of real foreground `gitmoot agent ask` traffic, so templates
self-improve as you use them — no operator has to remember to run `skillopt ab`:

```toml
[skillopt]
live_ab_sample_rate = 0.1   # 10% of qualifying asks are intercepted; 0.0 (default) = never
bandit_min_samples  = 30    # only agents whose champion arm has >= 30 bandit pulls
```

`live_ab_sample_rate` is **0.0 by default** (and unset with no `[skillopt]`
section), which makes the foreground ask path **byte-identical**: no extra
`Deliver`, no A/B record, no bandit update — the hot path is untouched when off.
When set `> 0`, a foreground ask on a **managed** agent whose champion bandit arm
is **at or above `bandit_min_samples`** is intercepted with probability
`live_ab_sample_rate`: it runs the champion (the canonical answer you receive) and
then a single pending challenger **serialized under the one runtime-session lock
already held** (no second lock, no `session is busy`), presents both answers, and
routes the human pick through the **exact same** `recordSkillOptABPick` path as the
manual A/B (same `source = skillopt-ab` `RankedFeedbackEvent`, same bandit update).

Low-traffic / bespoke agents (e.g. `researcher`) below `bandit_min_samples` are
**never** auto-A/B'd. It is **fail-safe**: a challenger error, no pending
challenger, or no pick captured degrades to the normal single champion answer and
logs a `live_ab_skipped` job event — the primary ask is never blocked or
degraded. Each intercepted ask runs the runtime **twice** (the sampled cost), and
`contract_version` stays `1`. **Promotion stays MANUAL by default**: live A/B
only writes feedback + updates the posterior; it never auto-promotes or rolls
back a version on its own.

Canary auto-promotion is a separate, off-by-default policy (#484): with
`[skillopt].auto_promote_canary = true` **and** `auto_promote_canary_sample`
set to a fraction in `(0,1]`, a guardrails-pass candidate is promoted to a
**canary** behind the live champion — the sample fraction is the per-resolution
probability a job routes to the canary version — and the daemon watches a
bounded regression window over the canary's harvested verifiable outcomes: on
parity-or-better it graduates the canary to `current`
(`candidate.auto_promoted` event); on a **material regression** it
**auto-rolls-back** (champion stays current, canary rejected,
`candidate.rolled_back` event). `candidate.canary_started` fires when the
canary goes live. It fails safe: `auto_promote_canary = true` with an
unset/invalid sample degrades to notify-only.

Pass **`--judge`** (or set `[skillopt].mode_b_judge_enabled = true`, both **off by
default**, #483) to ALSO have a **cross-family LLM judge** (a different runtime
family than the agent under test) pick A/B from the **same shuffled** options and
record a **separate** `RankedFeedbackEvent` (`reviewer`/`source =
skillopt-ab-judge`) that **coexists with** and **weights below** the human row.
The judge is **cross-family only** (skipped — never same-family — when no other
family is available), **never** touches the promotion bandit, drops fail-safe on
unparseable output, and its trust is **deferred to measure-the-judge (#344)**.
`--judge-only` records only the judge row (skips the human prompt). Off ⇒
byte-identical.

## Execution Model

Use `here` when the current chat should answer directly from the Gitmoot skill.
Use `background` when Gitmoot should create a tracked job, store events, and run
through a runtime adapter.

Background jobs are scheduled against three distinct resources:

- repo checkout mutexes for daemon ticks that use the same local checkout;
- runtime session locks keyed as `runtime:<runtime>:<runtime_ref>` for Codex,
  Claude, and Kimi delivery;
- branch locks for implementation ownership and merge safety.

Delivery is self-healing — a job whose events show one of these errors and then
a success is working as designed, not flaky:

- **Dead pinned session (#443):** when a Claude `--resume` target no longer
  exists, delivery retries on a fresh session and re-pins the agent to it.
- **Transient auth errors (#487/#509):** a transient 401 ("socket connection
  closed unexpectedly") under concurrency is retried with backoff; the old
  session is not abandoned.
- **Malformed output (#495):** an agent reply missing the `gitmoot_result`
  envelope records a `malformed_output` event and is re-asked with a repair
  prompt a bounded number of times before failing terminally.

The daemon default is `--workers 1`. Users can raise it when jobs target
different runtime sessions, managed agent types with `max_background` greater
than one, or forkable temporary workers. By default `[parallel_sessions]` uses
`same_session = "fork_temp_session"`, `merge_back = "summary"`,
`max_temp_sessions_per_agent = 4`, and
`eligible_actions = ["ask", "review", "implement"]`. Same-checkout work remains
serialized; same-runtime Codex/Claude jobs can fork only when the action is
eligible and implementation jobs have a safe task worktree.

### Running one agent's jobs in parallel

One registered agent serves one **foreground** ask at a time: `gitmoot agent ask
<name>` pins a single resumable runtime session, serialized by the
`runtime:<runtime>:<runtime_ref>` lock. Foreground asks do not auto-fork — to run
the *same* agent on several questions at once, dispatch them as **background**
jobs, where two mechanisms spin extra sessions for you:

1. **Temp-session forking (default, zero-config).** `[parallel_sessions]` ships
   with `same_session = "fork_temp_session"` and `max_temp_sessions_per_agent =
   4`: when a registered agent's session is busy and another **eligible**
   background job (`ask`/`review`/`implement`) is queued for it, the daemon forks
   a throwaway temp worker from that agent so the jobs run in parallel. Same
   runtime only (Codex/Claude/Kimi); same-checkout work stays serialized; an
   `implement` fork needs a safe task worktree. Nothing to configure.
2. **Managed agent types (`max_background`).** `gitmoot agent type set <type>
   --max-background N` defines a *pool* of named, reusable managed instances.
   Dispatch to the type with `gitmoot agent run <type> --type <type>
   --background …` and the daemon reuses an idle instance or spins a new one, up
   to `N`.

Both only deliver real parallelism when the daemon has job slots: raise
`--workers` above the default `1` (e.g. `--workers 6`) so `max_background`
instances / temp sessions actually run concurrently.

**Precedence — a single instance shadows a same-named type.** Dispatch resolves a
registered agent by name **first**, so if you `gitmoot agent start researcher`
*and* `gitmoot agent type set researcher`, plain `researcher` always uses the
single instance. Force the managed type with `--type researcher` (or don't
register a single instance of that name). Since **v0.5.1** a **foreground**
`gitmoot agent ask <type>` (the `ask` action) dispatches to the managed type
synchronously — it spins or reuses a managed instance up to `max_background`.
`review`/`implement` to a type still use `--background`.

## Multi-Repo Work

Agents are global identities with explicit per-repo access. When working across
multiple repos, always pass `--repo owner/repo` to status, daemon, job, and event
commands so jobs are routed in the correct repository context.

```sh
gitmoot agent allow reviewer --repo owner/project-a
gitmoot agent allow reviewer --repo owner/project-b
gitmoot status --repo owner/project-a
gitmoot status --repo owner/project-b
```

## Multi-Model Delegation (Orchestra)

Orchestra is gitmoot's name for structured multi-agent delegation: a conductor
(coordinator) returns a `delegations[]` score, the players (child agents) run in
parallel or in dependency order, and a finale (continuation) reconvenes and
synthesizes the results. This is how you orchestrate an orchestra of agents
across different runtimes.

A coordinator agent can fan work out to other agents running on different
runtimes by returning a `delegations` array in its `gitmoot_result`. Gitmoot
enqueues one child job per delegation, records a `delegation_enqueued` event on
the coordinator job, and runs the children in the daemon. Start a coordinator
and two workers on different runtimes so each delegation lands on the best model
for the job:

```sh
gitmoot agent start coordinator --runtime codex --repo owner/repo --role planner --capability ask --start-daemon
gitmoot agent start ui-worker --runtime claude --repo owner/repo --role reviewer --capability ask --capability review
gitmoot agent start api-worker --runtime kimi --repo owner/repo --role reviewer --capability ask --capability review
```

Queue the coordinator as background work so the daemon runs it and dispatches
its delegations (a synchronous `gitmoot agent ask` without `--background` only
returns the coordinator's own answer and does not fan out):

```sh
gitmoot agent ask coordinator --repo owner/repo --background "Coordinate the redesign across the API and UI teams."
```

The coordinator returns two delegations to the workers on different runtimes:

```json
{
  "gitmoot_result": {
    "decision": "approved",
    "summary": "Delegating UI review and API review to the workers.",
    "findings": [],
    "changes_made": [],
    "tests_run": [],
    "needs": [],
    "delegations": [
      {
        "id": "ui",
        "agent": "ui-worker",
        "action": "ask",
        "prompt": "Propose the component changes for the new dashboard layout."
      },
      {
        "id": "api",
        "agent": "api-worker",
        "action": "review",
        "prompt": "Review the API contract for the dashboard endpoints."
      }
    ]
  }
}
```

Gitmoot enqueues each delegation as a flat parallel child job
(`<parent-job-id>/delegation/<id>`) with a `delegation_enqueued` event on the
parent. Delegations that declare `deps` run only after the named siblings
succeed, and once every top-level delegation is terminal Gitmoot enqueues one
coordinator continuation job so the coordinator can synthesize the results.
Inspect the fan-out with `gitmoot job list --repo owner/repo` (one row per child
job) and `gitmoot events --repo owner/repo` (the `delegation_enqueued` events).
Each child job carries job-tree linkage fields — `parent_job_id`,
`delegation_id`, `root_job_id`, `delegation_depth`, and `task_id` — so a child
can be traced back to its parent, its originating delegation, and the root of the
tree. See `RESULT_CONTRACT.md` for the full delegation field reference.

**Read-only fan-out runs at the committed tip.** When a coordinator emits **two
or more** read-only (`ask`/`review`) siblings, Gitmoot auto-isolates each into a
detached `git worktree` at the **committed tip** of the base branch so they run
in parallel instead of serializing on the shared checkout. That worktree does
**not** contain gitignored paths (e.g. vendored clones under `repos/**`) or any
uncommitted working-tree changes, so an analysis/research leg cannot see the
operator's live working tree there. Each such child's prompt now carries a note
with the canonical base-checkout absolute path so a worker whose sandbox can read
it (e.g. codex) reaches the real tree instead of reporting a working-tree feature
as absent. A **single** read-only delegation stays in the shared base checkout
and sees everything, so for whole-working-tree analysis (auditing gitignored
vendored repos, or in-flight uncommitted WIP) either fan out to exactly **one**
read-only leg or pass an **absolute** path to the file/dir under analysis.

## Coordinator-Owned Review

By default an `implement` job that opens a pull request fans the PR out to
Gitmoot's native reviewers — the configured required reviewers, or the ones
passed for the task — so each reviewer runs as its own review job before the
merge gate. When a coordinator already plans review itself (for example a
`review-panel` leg, or a custom continuation that reconvenes its own reviewers),
that native fan-out duplicates work. Pass `--skip-native-review-fanout` on
`gitmoot orchestrate`, `gitmoot agent run`, or `gitmoot agent implement` to hand
review orchestration to the coordinator:

```sh
gitmoot agent implement lead --repo owner/repo --task task-001 --skip-native-review-fanout "Implement this task."
gitmoot orchestrate decompose-and-verify "Implement the export feature described in the task." --repo owner/repo --skip-native-review-fanout
```

With the flag set, the implement→PR step still records the PR baseline, runs the
merge gate, and records the `implemented` decision — it simply enqueues **no**
native review jobs. The skip is honored on both PR-open paths: the engine's
implement-advance and the daemon's GitHub PR-watcher, so a PR opened either way
stays free of native review fan-out. The flag defaults off; leaving it off keeps
the full native review fan-out, byte-identical to prior behavior.

## Coordinator Recipes

Coordinator recipes are built-in agent templates that turn the Orchestra pattern
into one-command workflows. Each recipe is a coordinator prompt that emits a
`delegations[]` of **ephemeral** workers (no pre-registration), runs them in the
daemon, then reconvenes their results in a single continuation. Start one with
`gitmoot orchestrate <agent> "..." --repo owner/repo --recipe <recipe-id>`
(#477): the `--recipe` flag routes any existing coordinator agent through the
named recipe prompt without changing the agent's identity. The bare
`gitmoot orchestrate <recipe-id> "..."` form requires an agent **registered
under the recipe name** first; on a fresh install it fails with "agent not
found", so prefer `--recipe`.

Three recipes ship built in:

- **`review-panel`** — fans a PR or change out to a panel of ephemeral reviewers,
  each with a different lens (correctness and security; performance and
  maintainability; tests and edge cases), then synthesizes their findings into
  one verdict. The panelists are dep-free, so they review in parallel, each with
  a self-contained lens prompt across mixed runtimes so the panel does not share
  one model's blind spots (point a panelist at an installed review template such
  as `thermo-nuclear-code-quality-review` only if you want).
- **`decompose-and-verify`** — decomposes one implementation task into
  file-disjoint subtasks, fans them out to ephemeral implementation workers that
  build in parallel in their own branch worktrees, then runs a single `review`
  verify step that `deps` on every implementation leg before reporting back.
- **`verifier`** — the minimal **produce vs. independent check** recipe: one
  producer leg plus one independent verify leg. The verify leg is a read-only
  ephemeral `review` worker that `deps` on the producer, runs on a **different
  runtime/model**, and checks the producer's combined result against the original
  goal — re-running the build and tests itself rather than trusting the producer's
  self-report. It returns `changes_requested` with structured findings on any
  objective failure (else `approved`), with `failure_policy: escalate` routing a
  failed verdict back to the coordinator continuation for autonomous correction
  (or `escalate_human` for a human pause).

**Produce vs. independent check.** A `synthesis_rule` (`summary`/`vote`/`quorum`)
reconciles what the producers **self-report** — self-evaluation, which inherits
the producer's blind spots. A `verifier`/`decompose-and-verify` verify leg is a
*separate* worker on a different runtime/model that checks the combined result
against the goal — cross-evaluation, which the literature finds beats
self-evaluation (the generator-verifier gap; LLM-as-judge self-preference bias).
This generalizes ROMA's Verifier (`(goal, candidate_output) -> verdict +
feedback`, vendored at `repos/ROMA`); it uses only shipped primitives
(`ephemeral`, `failure_policy`, the merge gate) — no new engine code. See the
**produce vs. independent check** note in `RESULT_CONTRACT.md`.

```sh
gitmoot orchestrate project-planner "Review PR #123 in this repo." --repo owner/repo --recipe review-panel
gitmoot orchestrate project-planner "Implement the export feature described in the task." --repo owner/repo --recipe decompose-and-verify
gitmoot orchestrate project-planner "Implement the rate limiter described in the task and prove it works." --repo owner/repo --recipe verifier
```

The panelists in `review-panel` and every producer and verify leg in
`decompose-and-verify` and `verifier` are **ephemeral** workers: Gitmoot creates
each from the delegation's `ephemeral` spec, runs it, and disposes of it once the
child job finishes. Ephemeral workers are leaf-only — they return findings, never their own
delegations — so a recipe's fan-out is exactly one level deep. In all three recipes the
delegations never set `agent`, because `agent` and `ephemeral` are mutually
exclusive. Once every leg is terminal, Gitmoot enqueues one continuation back to
the coordinator to merge the results (the panel verdict, or the verify gate plus
the merged changes). Inspect the run under `gitmoot job list --repo owner/repo`
and the `delegation_enqueued` events in `gitmoot events --repo owner/repo`. See
`RESULT_CONTRACT.md` for the `ephemeral` field reference and the termination
bounds these recipes run inside.

## Cockpit: Live Panes For An Orchestra

Add `--cockpit` (alias `--herdr`) to `gitmoot orchestrate` to render one live
[Herdr](https://github.com/jerryfane/herdr) pane per delegation subagent so a
fan-out is watchable as it runs — in the terminal, and (through herdres) mirrored
to Telegram. The cockpit is **opt-in and fail-open**: with `--cockpit` off, or
when Herdr is absent or `herdr status` is not ok, orchestration is byte-identical
to today. Gitmoot imports no Herdr code; it drives Herdr over its CLI only, and a
herdr call never fails or stalls a job.

```sh
gitmoot orchestrate project-planner "Review PR #123 in this repo." --repo owner/repo --recipe review-panel --cockpit
gitmoot orchestrate project-planner "Implement the export feature." \
  --repo owner/repo --recipe decompose-and-verify --cockpit --cockpit-session feature-export
```

Each child job opens a pane labeled `<agent> · d<depth> · <branch>` and streams
its progress; children inherit the cockpit setting from their parent, so one
`--cockpit` on the root lights up the whole orchestra. The same panes are visible
in the terminal (open the Herdr workspace) and on Telegram via the herdres bridge.

A cockpit pane is a **view, not the job**: closing a pane (in the terminal or
from Telegram) tears down the visible surface but does NOT cancel the underlying
job — the child keeps running and its result is still synthesized. To stop work,
cancel through the record path, not by closing the pane:

```sh
gitmoot job list --repo owner/repo     # find the child job id
gitmoot job cancel <job-id>            # cancel via the record; pane teardown follows
```

Defaults live in `~/.gitmoot/config.toml` under `[orchestrate]` and are
overridable per run by the flags above:

```toml
[orchestrate]
cockpit_mode = "auto"      # on | off | auto (auto = on iff herdr is reachable)
cockpit_session = ""       # default Herdr session/workspace label ("" = per-run)
cockpit_max_panes = 4      # cap on simultaneous panes per run
cockpit_pane_key = "job"   # job (one pane per job) | seat (reuse a pane per role)
```

On a constrained host the pane count is what bites, not the jobs. Lower
`cockpit_max_panes` (the default is `4`; use `2` or `1` on a small box) — beyond
the cap, extra subagents run **status-only**: they still report state to Herdr
but do not split a new pane, so the work fans out exactly as without the cockpit
while the visible surface stays small. Prefer `cockpit_pane_key = "seat"` for
long multi-phase runs so panes are reused per role instead of accumulating one
per job. The cockpit never changes the engine — the DAG, the result contract,
the runtime-session locks, and the checkout keys are unchanged whether it is on,
off, or unavailable — so capping panes only changes what you see, never how the
orchestra runs. If `--cockpit` is asked for but Herdr is unreachable, gitmoot
emits one `cockpit_unavailable` job event on the root and runs unwrapped.

The optional `scripts/cockpit-smoke.sh` confirms the cockpit is opt-in and
fail-open: it runs against an isolated `--home`, exercises the `--cockpit` wrap
path when `herdr` is reachable, and skips cleanly when `herdr` or `gitmoot` is
unavailable. See `../../../docs/cockpit-orchestrate.md` for the full reference.
