# Orchestrate vs. workflow: who is the manager?

Gitmoot has two ways to run multi-job efforts, and they answer one question
differently: **who decides the next step?**

- **`gitmoot orchestrate`** — *gitmoot is the manager.* An agent **job inside
  gitmoot** plans the work by returning `delegations[]`, and gitmoot's engine
  executes the plan.
- **`--workflow <label>`** — *someone outside gitmoot is the manager*: a Claude
  or Codex session, a script, a cron job, or a human at the CLI. Gitmoot runs
  the individual jobs; the label and its journal make the outside manager's
  project **visible and remembered**.

## What each gives you

|  | `orchestrate` | `--workflow` + `workflow note` |
| --- | --- | --- |
| Who decides the next step | an LLM coordinator **job** returning `delegations[]` | the external coordinator, live, between steps |
| Execution machinery | **gitmoot's engine**: dependency ordering, retries, timeouts, failure policies, quorum/vote synthesis, tree token budgets, wall-clock limits, kill scope, human-question pauses | **none by design** — grouping and journaling only; the external coordinator owns retries, ordering, and judgment |
| Job relationships | real parent → child edges (delegation tree) | a shared label on otherwise independent jobs |
| Where you see it | the run graph (`/?run=<id>`) as a delegation tree | `gitmoot workflow list/show`, Galaxy workflow hubs, and `/workflows/<label>` in the web dashboard |
| Reasoning trail | the coordinator job's own result | the **journal** (`gitmoot workflow note`), optionally distilled into shared memory with `--remember` |
| Coordinator cost | the coordinator is itself an LLM job (billed and visible) | free to gitmoot — the brain lives elsewhere |

## When to use `orchestrate`

Reach for `orchestrate` when the plan fits a tree you can **declare up front**
and you want gitmoot's guardrails while nobody watches:

- parallel fan-out — review a PR from several lenses, research N topics at once;
- bounded autonomous work, where budgets, timeouts, and failure policies do the
  babysitting;
- inside pipelines, as an `orchestrate` stage.

## When to use `--workflow`

Use an external coordinator — and label its jobs — when **judgment between
steps is the point**. The canonical case is a coordinated build: run the tests
*between* jobs, read what a reviewer found, decide the scope of a fix round,
own git and PRs, choose merge timing. No declarative tree can express "run the
suite, read the failure, decide whether it is a real bug or a timeout, bisect
against main." The label makes that externally-managed project visible; the
notes keep the *why*; `--remember` turns cross-job insight into durable shared
memory that future agents recall.

`--workflow` is deliberately **visibility-only**: it never adds scheduling,
locking, budgets, or lifecycle behavior to the labeled jobs.

## They compose

`orchestrate --workflow release-42` labels the **whole delegation tree** —
children and continuations inherit the label — so a workflow can *contain*
orchestrations. The realistic shape of a large effort is exactly that: an
external coordinator drives the project as a workflow, and fires an
`orchestrate` inside it for the sub-tasks that do fit a declarative tree. One
label, mixed management styles, one story in the dashboard.

## Rule of thumb

If you would trust a written plan executed unattended, use `orchestrate`. If
the work needs a thinking manager reacting to results, coordinate externally
and pass `--workflow` so the effort is not invisible. When in doubt on
quality-critical code changes, prefer the external pattern — an adversarial
review-and-fix loop with a judging coordinator catches defects a one-shot
declarative plan does not.
