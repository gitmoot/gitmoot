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

## Locks

Gitmoot uses separate locks for separate resources:

- Repo checkout locks keep daemon ticks from using the same local checkout at
  the same time.
- Runtime session locks serialize Codex or Claude delivery for the same
  `runtime:<runtime>:<runtime_ref>` across daemon jobs, `job run`, and
  synchronous `agent ask`.
- Branch locks prevent multiple agents from racing on the same implementation
  branch.

Review and ask jobs usually inspect and report. Implementation jobs should only
mutate branches when Gitmoot assigned the job and the branch lock is held.

The daemon defaults to `--workers 1`. Raise workers only when jobs use
independent runtime sessions or managed agent types with `max_background`
greater than one.
