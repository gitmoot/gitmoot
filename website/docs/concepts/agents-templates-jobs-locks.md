# Agents, Agent Templates, Jobs, And Locks

Gitmoot uses runtime-neutral agent records. A named agent has a runtime, runtime
reference, repo access, role, capabilities, and optional template.

## Agents

Agents can be started by Gitmoot or subscribed from an existing runtime session.

```sh
gitmoot agent start project-planner --runtime codex --repo owner/repo --template planner
gitmoot agent subscribe reviewer --runtime codex --session <session-id> --repo owner/repo --capability review
```

## Agent Templates

Agent Templates are reusable prompt/profile bundles. Gitmoot snapshots template content
into each job so the job has reproducible instructions.

```sh
gitmoot agent template update planner
gitmoot agent template update thermo-nuclear-code-quality-review
gitmoot agent template draft release-planner
gitmoot agent template validate .gitmoot/templates/release-planner.md
gitmoot agent template add frontend-reviewer --file agents/frontend-reviewer.md
```

Template capture is the current-chat path for creating new custom templates
from a successful visible workflow. The current Codex or Claude chat fills a
draft from visible context; `agent template add` installs the reviewed snapshot.

## Jobs

Jobs are units of routed work. They can come from PR comments, local
`agent ask`, task runs, retries, or merge actions.

## Delegations

An agent's result can return a validated `delegations[]` DAG, and Gitmoot
dispatches each entry as a child job. Dependency-free delegations run in
parallel, and once every top-level delegation is terminal Gitmoot enqueues a
single coordinator continuation job back to the delegating agent to synthesize
the results. Delegation trees are bounded by a depth cap, a per-root job budget,
and loop detection, so offending delegations are dropped rather than retried
forever. See the [Result Contract](../reference/result-contract.md) for the full
field reference.

**Orchestra** is gitmoot's name for this structured multi-agent delegation: a
conductor (coordinator) returns a `delegations[]` score, the players (child
agents) run in parallel or in dependency order, and a finale (continuation)
reconvenes and synthesizes the results. It is a naming layer on top of the same
`delegations` mechanics described above.

## Choosing a worker: registered agent vs. ephemeral worker

There are only **two** kinds of worker you deliberately create — a **registered
agent** and an **ephemeral worker**. A third, the **temp worker**, is created
*automatically* by the daemon and is never something you make by hand.

| | Registered agent | Ephemeral worker | Temp worker |
|---|---|---|---|
| Who creates it | You (`gitmoot agent start` / `subscribe` / `type set`) | A coordinator (a `delegations[]` entry with an `ephemeral` spec) | The daemon, automatically |
| Persists | Yes — lives in the registry | No — auto-disposed after its job | No — auto-disposed after its job |
| Identity / name | Stable, addressable | Synthetic, hidden from the registry | Synthetic, hidden from the registry |
| Reusable | Across many jobs and PR subscriptions | One job, then gone | One job, then gone |
| Built from | Your config (runtime / model / template / capabilities) | An inline spec on the delegation | A **fork of an existing registered agent** |
| Purpose | A durable role | A one-off delegated task | **Concurrency** — run a registered agent's jobs in parallel |

A temp worker is just *"this registered agent is busy; clone it to run another
job in parallel, then drop the clone."* You influence it only through a managed
agent type's `max_background` — you never create one directly.

**Create a registered agent when the role is durable and addressable:**

- You will reuse it (a repo's `reviewer`, a `planner` you invoke repeatedly).
- It needs a stable name — PR-comment subscriptions, `gitmoot agent ask <name>`,
  the dashboard, job history and accountability.
- It carries managed state: a versioned, SkillOpt-trained template, a resumable
  runtime session, repo scope / capabilities / autonomy you tune.
- You want a pool that auto-scales under load — a managed *agent type* with
  `max_background` (the daemon forks temp workers from it).
- It needs to **itself delegate** — ephemeral workers are leaf-only and cannot
  return their own delegations.

**Spawn an ephemeral worker (via a coordinator's delegation) when the work is
one-off and disposable:**

- Dynamic fan-out you do not know ahead of time ("three workers each draft an
  option", "a cheap gate plus a strong verifier").
- No reuse, no identity, no subscription — it exists only for that delegation.
- The coordinator wants to choose the runtime and model *per task* (model
  tiering).
- You want zero setup and zero cleanup — no pre-registration, auto-disposed,
  never clutters the registry.

**Rule of thumb:** *durable and named → register an agent; one-off and
coordinator-spawned → ephemeral; concurrency of a registered agent → temp
workers happen for you.* The trade-off: registered agents have setup cost but are
reusable, addressable, trainable, and resume a warm session; ephemeral workers
are zero-setup and self-cleaning but cold-spawn each time and carry no
accumulated identity.

## Locks

Gitmoot uses separate locks for separate resources:

- Repo checkout locks keep daemon ticks from using the same local checkout at
  the same time.
- Runtime session locks serialize Codex, Claude, or Kimi delivery for the same
  `runtime:<runtime>:<runtime_ref>` across daemon jobs, `job run`, and
  synchronous `agent ask`.
- Branch locks prevent multiple agents from racing on the same implementation
  branch.

Review and ask jobs usually inspect and report. Implementation jobs should only
mutate branches when Gitmoot assigned the job and the branch lock is held.

The daemon defaults to `--workers 1`. Raise workers only when jobs use
independent runtime sessions or managed agent types with `max_background`
greater than one.
