# Gitmoot Safety Reference

## Repo Scope

A PR repository is the routing context for `/gitmoot <agent> ...`. Always confirm
or pass `--repo owner/repo` when the user works across multiple repositories.

## Branch Locks

Implementation jobs must respect Gitmoot branch locks. Do not edit or push an
implementation branch unless Gitmoot assigned the job and the branch lock is held
by the assigned agent.

Review and ask jobs should inspect and report without mutating branches unless
the task explicitly instructs otherwise.

Do not ask child agents to run PR lifecycle commands such as `git pull`,
`git merge`, `git push`, or `gh pr merge` to make parallel task PRs mergeable.
Gitmoot owns the final merge gate. It serializes merge attempts per base branch,
updates stale PR branches through GitHub when possible, retries pending states
through the daemon, and blocks clearly when GitHub reports a real merge
conflict.

Useful commands:

```sh
gitmoot lock list --repo owner/repo
gitmoot lock show owner/repo <branch>
```

## Files And Secrets

Do not commit generated data, caches, logs, build outputs, session archives,
cloned helper repos, secrets, credentials, or large artifacts unless the user or
plan explicitly says they are intended tracked fixtures or release assets.

Redact secrets from GitHub comments, job summaries, raw examples, and copied
command output.

## External Contracts

If the work depends on external APIs, CLIs, env vars, generated scripts, service
launchers, installers, deployment behavior, or third-party libraries, verify the
real contract with local commands and/or official documentation before editing.

## Delegation Termination Bounds

These bounds keep an orchestra finite: when you orchestrate an orchestra of
agents, the conductor's score and the players it spawns cannot recurse or fan out
forever. Delegation and coordinator-continuation chains are bounded so they
cannot recurse or fan out forever:

- Depth cap `MaxDelegationDepth = 8`: each delegation child and each coordinator
  continuation increments `delegation_depth`. A job at or beyond this depth may
  not delegate further.
- Per-root job budget `MaxDelegationTotalJobs = 64`: the whole delegation tree
  under one root (all children and continuations sharing that root) is capped.
  When a batch would exceed it, the delegations are not dispatched.
- Per-root wall-clock budget `MaxDelegationWallClock = 2h`: the whole tree under
  one root is bounded in duration (measured from the root job's creation); a
  coordinator that tries to fan out after the tree has run this long is refused
  with a `delegation_walltime_exceeded` event. A generous runaway backstop, not a
  tight deadline. **Time a tree spends paused awaiting a human** (the
  `escalate_human` failure_policy, below) **is excluded** from this measurement, so
  a slow human response is never punished as a runaway.
- Per-root token budget (cost) `[orchestrate].max_delegation_token_budget`,
  **off by default** (`0` = unlimited): when set to a positive value, the whole
  tree under one root is bounded by cumulative token usage (input + output across
  every job in the tree). A coordinator that tries to fan out after the tree has
  already used at least the budget is refused with a `delegation_cost_exceeded`
  event. Token capture is **best-effort per runtime** (Claude reports usage; Kimi
  reports it if its stream emits it; Codex `Deliver` runs without `--json` and so
  contributes `0`), so the budget can under-count but never over-counts. Leaving
  the knob at `0` skips the check entirely.
- Per-root **dollar-cost** budget `[orchestrate].max_delegation_cost_usd`,
  **off by default** (`0` = unlimited): the cost analogue of the token budget
  (#380). It bounds the same tree by *measured spend* — the same per-job token
  usage priced through a built-in per-model price table (Haiku/Sonnet/Opus list
  prices, matched by substring; unknown models priced at the mid-tier Sonnet
  default so they are never free). When the tree's accumulated cost reaches the
  budget, the next fan-out is refused with a `delegation_cost_usd_exceeded` event
  and routed to the finalize continuation — never hard-killed. Coarse backstop,
  not a precise spend meter; leaving the knob at `0` skips the check entirely.
- Per-coordinator width `MaxDelegationWidth = 16`: a single coordinator result
  may not fan out more than this many delegations in one generation; an over-wide
  set is refused with a `delegation_width_exceeded` event.
- Loop detection (two signals): a cheap **structural** signature hash over recent
  delegation sets halts a coordinator that literally re-issues the same set,
  preventing oscillating A→B→A loops well before the depth cap. Layered on top, a
  **result-aware non-progress streak** (#339) catches a coordinator that perturbs
  the set each round to evade the structural hash but whose children keep
  returning nothing new: after every generation finishes, the engine fingerprints
  the children's *verifiable* side effects (decision, `changes_made`, `tests_run`,
  PR/HeadSHA, `artifact_body` — self-reported summary/findings text is excluded).
  When that digest repeats for `MaxDelegationNonProgressStreak` consecutive
  generations (default `2`, set per-host via
  `[orchestrate].max_delegation_non_progress_streak`), the tree trips the same loop
  ladder; any new durable side effect resets the streak even if the summary text
  repeats. Both signals share one ladder: `delegation_loop_warning` + a corrective
  continuation, then `delegation_loop_detected` + the graceful finalize below.
- Operator kill switch: `gitmoot job kill <root-job-id>` lets an operator
  terminate a runaway tree by its root id from outside. It is the **first**
  backstop, so operator action wins over every budget cap. The kill is graceful,
  not a hard stop — in-flight jobs finish normally, the coordinator's next
  continuation routes through the same finalize path below (a
  `delegation_killed` event is emitted), and the daemon stops starting queued
  children of the killed root.
- Human-in-the-loop pause (`escalate_human` failure_policy, #340): when a child
  fails under `escalate_human`, the parent task enters the resumable
  `awaiting_human` state, a one-shot `delegation_escalation_requested` event is
  recorded, the human is @-tagged in a GitHub comment, and **no continuation is
  enqueued** — the tree consumes zero tokens/compute while it waits. A paused tree
  is **not** a budget failure. A human resumes with
  `/gitmoot resume <coordinatorJobID> retry|continue|abort [instructions]`
  (authorize-commenter gated, exactly like `/gitmoot retry`/`cancel`); a
  never-answered escalation is auto-finalized through the graceful finalize path
  below after `[orchestrate].escalation_ttl` (default 24h). The daemon ingests
  `/gitmoot resume` comments on the tree's **open** PR or issue; the dashboard
  **Attention** section and the TTL backstop cover a tree whose PR/issue is no
  longer open.
- Non-failure ask-gate (`human_questions[]`, #445): the **healthy-result** sibling
  of `escalate_human`. A worker/coordinator that returns a healthy result carrying
  `human_questions[]` pauses the parent task at the **same** resumable
  `awaiting_human` state for a specific human answer — **no leg fails**, no
  continuation or delegation children are enqueued, and the tree consumes zero
  tokens/compute. It reuses the **same** pause plumbing as `escalate_human` (one
  `delegation_escalation_requested` event tagged `kind=ask` with the questions, the
  @-tag comment, the dashboard **Attention** section), so the **same**
  `[orchestrate].escalation_ttl` auto-finalizes an unanswered ask, the **same**
  wall-clock pause exclusion applies, and the pause is **budget-neutral** (it
  enqueues no job; only the eventual answer-driven continuation occupies the single
  continuation slot). A human answers with
  `/gitmoot resume <coordinatorJobID> answer "<id>: ..."` (authorize-commenter
  gated); the answer is injected into the coordinator continuation prompt. The
  `answer` verb is valid only on an ask round and `retry`/`continue`/`abort` only
  on a failure round — a mismatch is rejected with a clear message. Absence of
  `human_questions[]` is byte-identical to today's behavior.

When a bound trips, the offending delegations are not dispatched and the parent
receives a typed lifecycle event explaining why (for example, the delegation tree
for a root reached the job budget of 64). Rather than stopping silently, the
engine then enqueues one **graceful finalize continuation**
(`delegation_finalize_enqueued`) back to the coordinator — told it cannot
delegate further and asked to synthesize a best-effort final result and return
empty delegations. That continuation is terminal: any delegations it returns are
ignored (`delegation_finalized`), so work is bounded and always ends in a clean
synthesis, not a silent dead end.

## When To Stop

Stop and report `blocked` when the target repo is unclear, GitHub auth is
missing, the daemon cannot access the repo, branch lock ownership is wrong, or
continuing would require credentials or destructive operations the user did not
approve.
