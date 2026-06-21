# Gitmoot Workflows

## First Repo Setup

1. Confirm the repo identity and GitHub auth.
2. Start the daemon for the repo.
3. Start or subscribe at least one agent.
4. Verify the agent is healthy before asking PR comments to route jobs.

```sh
gh repo view --json nameWithOwner
gh auth status
gitmoot daemon start --repo owner/repo
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

## Execution Model

Use `here` when the current chat should answer directly from the Gitmoot skill.
Use `background` when Gitmoot should create a tracked job, store events, and run
through a runtime adapter.

Background jobs are scheduled against three distinct resources:

- repo checkout mutexes for daemon ticks that use the same local checkout;
- runtime session locks keyed as `runtime:<runtime>:<runtime_ref>` for Codex,
  Claude, and Kimi delivery;
- branch locks for implementation ownership and merge safety.

The daemon default is `--workers 1`. Users can raise it when jobs target
different runtime sessions, managed agent types with `max_background` greater
than one, or forkable temporary workers. By default `[parallel_sessions]` uses
`same_session = "fork_temp_session"`, `merge_back = "summary"`,
`max_temp_sessions_per_agent = 4`, and
`eligible_actions = ["ask", "review", "implement"]`. Same-checkout work remains
serialized; same-runtime Codex/Claude jobs can fork only when the action is
eligible and implementation jobs have a safe task worktree.

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

## Coordinator Recipes

Coordinator recipes are built-in agent templates that turn the Orchestra pattern
into one-command workflows. Each recipe is a coordinator prompt that emits a
`delegations[]` of **ephemeral** workers (no pre-registration), runs them in the
daemon, then reconvenes their results in a single continuation. Start one with
`gitmoot orchestrate <recipe-id> "..." --repo owner/repo`; the daemon runs the
coordinator in the background and dispatches its delegations.

Two recipes ship built in:

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

```sh
gitmoot orchestrate review-panel "Review PR #123 in this repo." --repo owner/repo
gitmoot orchestrate decompose-and-verify "Implement the export feature described in the task." --repo owner/repo
```

The panelists in `review-panel` and every implementation and verify leg in
`decompose-and-verify` are **ephemeral** workers: Gitmoot creates each from the
delegation's `ephemeral` spec, runs it, and disposes of it once the child job
finishes. Ephemeral workers are leaf-only — they return findings, never their own
delegations — so a recipe's fan-out is exactly one level deep. In both recipes the
delegations never set `agent`, because `agent` and `ephemeral` are mutually
exclusive. Once every leg is terminal, Gitmoot enqueues one continuation back to
the coordinator to merge the results (the panel verdict, or the verify gate plus
the merged changes). Inspect the run under `gitmoot job list --repo owner/repo`
and the `delegation_enqueued` events in `gitmoot events --repo owner/repo`. See
`RESULT_CONTRACT.md` for the `ephemeral` field reference and the termination
bounds these recipes run inside.
