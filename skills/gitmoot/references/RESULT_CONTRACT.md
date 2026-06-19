# Gitmoot Result Contract

Every agent job must return a `gitmoot_result` JSON object. Keep it concise,
truthful, and tied to work that actually happened.

```json
{
  "gitmoot_result": {
    "decision": "approved|changes_requested|blocked|implemented|failed",
    "summary": "Brief outcome.",
    "findings": [],
    "changes_made": [],
    "tests_run": [],
    "needs": [],
    "delegations": []
  }
}
```

## Delegations

Orchestra is gitmoot's name for structured multi-agent delegation: a conductor
(coordinator) returns a `delegations[]` score, the players (child agents) run in
parallel or in dependency order, and a finale (continuation) reconvenes and
synthesizes the results.

Vocabulary: the conductor is the coordinator agent; the players are the delegated
child agents; the score is the `delegations[]` DAG (its `deps` are the cues); the
finale is the continuation job that reconvenes and synthesizes.

Use `delegations` to request follow-up work by named Gitmoot agents. Each
delegation describes a child job:

```json
{
  "gitmoot_result": {
    "decision": "approved",
    "summary": "Plan ready for review.",
    "findings": [],
    "changes_made": [],
    "tests_run": [],
    "needs": [],
    "delegations": [
      {
        "id": "review-plan",
        "agent": "thermo-review",
        "action": "review",
        "prompt": "Review the implementation plan for correctness."
      }
    ]
  }
}
```

Delegation fields:

- `id` (required): stable identifier for this delegation, unique within the
  result. Sibling delegations reference it through `deps`.
- `agent` (required): name of the Gitmoot agent to run.
- `action` (required): job action, e.g. `ask`, `review`, or `implement`.
- `prompt` (required): instructions for the delegated job.
- `deps` (optional): array of sibling delegation `id`s. This delegation runs
  only after every listed sibling succeeds. Each entry must reference a known
  sibling in the same result, may not be self-referential, and may not form a
  cycle — delegations form a DAG, and cycles are rejected.
- `failure_policy` (optional): one of `block_parent`, `continue`, or
  `escalate`. Defaults to `block_parent` when omitted.
- `synthesis_rule` (optional): one of `summary` or `vote`.
- `timeout` (optional): a Go duration string and must be positive (e.g. `10m`).
- `retry` (optional): integer `>= 0`.
- `worktree` (optional): worktree path for the child job.
- `artifacts` (optional): named artifact handles passed to the child. When any
  delegation requests artifacts, the parent result must also set the top-level
  `artifact_body` field; validation rejects the result otherwise.
- `fingerprint` (optional): dedup key. Identical fingerprints are
  de-duplicated, so the same delegation is not dispatched twice.
- `model` (optional): a free-form, runtime-scoped model string for the child
  job (for example a Codex, Claude Code, or Kimi Code model name). When omitted,
  the delegated agent's configured default model is used. There is no allow-list;
  Gitmoot passes the value through to the runtime as-is.
- `ephemeral` (optional): an inline worker spec that spawns a throwaway child
  agent on demand instead of routing to a pre-registered one. It is **mutually
  exclusive** with `agent`: a delegation must set exactly one of `agent` or
  `ephemeral`. When `ephemeral` is set, no agent needs to be registered first —
  Gitmoot materializes a worker from the spec, runs the child job, and disposes
  of the worker once the job finishes. The ephemeral child inherits the
  coordinator's allowed repo scope. Fields:
  - `runtime` (required): the runtime that backs the worker, one of `codex`,
    `claude`, or `kimi`. It is never `shell`.
  - `model` (optional): a runtime-scoped model string, as for the delegation
    `model` field above.
  - `template` (optional): an agent-template id to seed the worker's prompt.
  - `role` (optional): a human-readable role label for the worker.
  - `capabilities` (optional): an array of capability strings advertised by the
    worker.
  - `autonomy_policy` (optional): the worker's sandbox autonomy. Defaults to
    `read-only`.

  Ephemeral delegations are bounded by the same delegation limits as any other
  delegation (see [Termination bounds](#termination-bounds)); they do not relax
  the depth cap, per-root job budget, or loop detection.

  **`agent` vs `ephemeral` — which to use:** delegate to a registered `agent`
  when the work needs a specific, durable, addressable worker (a tuned/trained
  template, a resumable session, accountable history) or when the worker must
  itself delegate — **ephemeral workers are leaf-only and cannot return their own
  delegations**. Use `ephemeral` for one-off, disposable, dynamically-sized
  fan-out where you just need "a runtime + model + prompt" with no
  pre-registration and no cleanup (e.g. N workers each producing one result, or a
  cheap gate plus a strong verifier with per-worker models).

### Validation errors

Each required-field failure is reported per entry as
`delegations[<index>] (id "<id>"): <field> is required`, where `<index>` is the
0-based position in `delegations[]` and `<id>` is the delegation's id (or
`<missing>` when blank). All offending fields across the batch are reported
together — not just the first — and the coordinator gets one repair retry to fix
them all in a single round.

A delegation with no `deps` dispatches immediately and runs in parallel with
other dep-free siblings. Once every top-level delegation reaches a terminal
state, Gitmoot enqueues exactly one coordinator "continuation" job — back to
the delegating agent — to synthesize the children's results.

Sibling children that share the repo run in isolated git worktrees so they do
not serialize on the shared checkout: `implement` children each get their own
branch worktree, and when a coordinator fans out **two or more read-only**
(`ask`/`review`) children, each gets a throwaway detached worktree (no branch).
A read-only child that **`deps` on `implement` legs** (e.g. a decompose-and-verify
verify gate) runs in a detached worktree with those legs' branches **merged in**,
so it sees their combined work rather than the base checkout; if the legs are not
file-disjoint the merge conflicts and the parent is blocked. The worktrees are
disposed automatically when each child finishes. This is internal scheduling —
coordinators do not request it.

Each child job carries `parent_job_id`, `delegation_id`, `root_job_id`,
`delegation_depth`, and `task_id`, so a child can be traced to its parent, its
originating delegation, and the root of the job tree.

### Termination bounds

Delegation trees are bounded so they cannot run forever:

- Depth cap: `MaxDelegationDepth = 8`. Each delegation child and each
  coordinator continuation increments `delegation_depth`; a job at or beyond
  this depth may not delegate further.
- Per-root job budget: `MaxDelegationTotalJobs = 64`. The whole delegation tree
  under one root is capped at this many jobs.
- Loop detection: a windowed signature over recent delegation activity halts
  repeated or cyclic delegation chains (e.g. oscillating A→B→A) well before the
  depth cap is reached.

When a bound trips, the offending delegations are dropped rather than
dispatched, and the parent receives a lifecycle/mailbox event explaining why
(for example, "delegation tree for root <id> reached the job budget of 64").

### Top-level fields

- `artifact_body` (optional): the artifact payload made available to delegated
  children. Required whenever any delegation sets `artifacts`.

## Decisions

- `approved`: review found no blocking issues.
- `changes_requested`: review found issues that should be fixed before merge.
- `blocked`: work cannot continue without human input or an external state change.
- `implemented`: the requested implementation work was completed.
- `failed`: the attempted action errored or could not complete.

## Reporting Rules

- Do not claim tests were run unless they were actually run.
- Do not claim files were changed unless they were actually changed.
- Use `needs` for missing credentials, unclear scope, unavailable tools, failing
  external services, or required human decisions.
- Use `delegations` when another named Gitmoot agent should be invoked.
- Redact secrets from summaries, findings, raw command output, and examples.
