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
- `failure_policy` (optional): one of `block_parent`, `continue`, `escalate`, or
  `escalate_human`. Defaults to `block_parent` when omitted.
  - `block_parent` — a failed child blocks the shared parent task (terminal).
  - `continue` — independent siblings keep running; only this branch's dependents
    are skipped.
  - `escalate` — hand every child outcome to the **coordinator** continuation to
    decide autonomously (no human).
  - `escalate_human` — a **durable human-in-the-loop pause** (#340). On a child
    failure the parent task enters the resumable `awaiting_human` state, **no
    continuation is enqueued, and the tree consumes zero tokens/compute** until a
    human resumes it. The daemon @-tags the human in a GitHub comment (default
    handle: the repo owner, or `[orchestrate].escalation_handle`) and the
    dashboard lists the tree under **Attention**. A human resumes with
    `/gitmoot resume <coordinatorJobID> retry|continue|abort [instructions]`:
    `retry` re-runs the failing leg with the instructions folded in, `continue`
    proceeds the coordinator continuation (now human-approved), and `abort`
    routes to the graceful finalize continuation. A never-answered escalation is
    auto-finalized after `[orchestrate].escalation_ttl` (default 24h); paused
    time is excluded from the per-root wall-clock budget, and a paused tree is
    never counted as a budget failure. The daemon routes `/gitmoot resume`
    comments on the tree's **open** PR or issue (it watches open PRs/issues); the
    dashboard **Attention** section and the `escalation_ttl` backstop cover a tree
    whose PR/issue is no longer open.
- `synthesis_rule` (optional): one of `summary`, `vote`, or `quorum`.
- `quorum` (optional): an integer `K` (`> 0`), required when `synthesis_rule`
  is `quorum`. The coordinator continuation proceeds only if at least `K`
  children reach an approving decision; otherwise the parent blocks, exactly as a
  failed `vote` does. `vote` is the special case where `K` equals the number of
  delegations (every child must approve). `K` is an integer count only — no
  fractions or percentages — and must not exceed the number of delegations (a
  larger `K` is unsatisfiable and is rejected).

  **Produce vs. independent check.** The `synthesis_rule`s above reconcile what
  the producers **self-report** — `summary` merges their summaries, `vote`/`quorum`
  count their *own* approving decisions. That is **self-evaluation**: it trusts the
  producer's "I approve / I implemented it" and inherits the producer's blind
  spots. To check the *combined* result against the original goal independently,
  add a separate **verify leg** — a read-only ephemeral `review` child that `deps`
  on the producer(s), runs on a **different runtime/model**, and re-runs the build
  and tests itself rather than trusting the producers' self-reported `tests_run`.
  It returns `decision: changes_requested` with structured findings on any
  objective failure, else `approved`. This is **cross-evaluation**, which the
  literature consistently finds beats self-evaluation: a capable verifier catches
  failures the solver does not (the generator-verifier gap), and LLM-as-judge
  graders show a self-preference bias toward their own outputs that a
  different-model judge does not share. It is the same separation as ROMA's
  Verifier (`VerifierSignature: (goal, candidate_output) -> verdict + feedback`,
  vendored at `repos/ROMA`), where a failed verdict drives a re-plan instead of
  trusting the producer. Route a failed verdict with `failure_policy: escalate`
  (autonomous correction in the coordinator continuation) or `escalate_human` (a
  human-in-the-loop pause); the merge gate independently blocks merge on the
  non-ready decision. The shipped `decompose-and-verify` recipe is one instance of
  this (parallel producers + one verify gate), and the `verifier` recipe is its
  minimal one-producer form — both are templates under
  `skills/gitmoot/agent-templates/`. No new primitive is involved: `EphemeralSpec`,
  `failure_policy`, and the merge gate already ship.
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

  **Model-tier routing (recommendation).** Picking a per-delegation `model` is
  the coordinator's call — Gitmoot does not choose or override it. As a costing
  heuristic, think of each leg as falling into one of four abstract **tiers**,
  then map the tier to a concrete model for whichever runtime the leg uses:

  - **mechanical** — rote, deterministic edits (a rename, a codemod-style
    find-and-replace, a version bump, regenerating a file). Use the smallest /
    cheapest model.
  - **cheap** — low-complexity legs (a short `ask`, a narrow single-file
    lookup). A small, fast model is enough.
  - **standard** — ordinary work with no strong hardness signal. **Leave
    `model` empty** so the leg runs on the delegated agent's runtime default.
  - **deep** — genuinely hard or quorum-critical legs. Reserve a strong,
    expensive model.

  Cheap **hardness signals** a coordinator can read off a leg before dispatch: a
  long, detailed prompt; hard-topic keywords (`architecture`, `oauth`, `schema`,
  `concurrency`, `migration`, `security`, `refactor`, …); a multi-file
  `implement`/`fix` action versus a read-only `ask`; broad scope (several
  `artifacts` or a dedicated `worktree`); and whether the leg is part of a
  `quorum`. More of these lean **deep**; their absence leans **cheap**. Gitmoot
  ships an uncalibrated helper for this — `workflow.ScoreComplexity` /
  `workflow.TierFor` (`internal/workflow/modeltier.go`) — that turns a
  delegation into a tier. It is a pure recommendation primitive: the engine
  never calls it and never overrides a coordinator's chosen `model`.

  **Cascade / escalate-in-continuation pattern.** Prefer to start a leg on the
  cheapest plausible tier and **escalate in a continuation** only if it falls
  short: run the first attempt on a cheap/standard model, and if the result is
  `blocked`/`failed` (or a `quorum` vote is not met), have the coordinator
  re-issue that leg in its next round on a deep model. This keeps the common
  case cheap while still reaching a strong model for the legs that genuinely
  need it.

  **Rule of thumb:** downshift `model` for cheap/mechanical legs; leave `model`
  empty for standard legs (so they take the runtime default); and reserve a deep
  model for genuinely hard or quorum-critical legs.
- `phase` (optional): a free-form per-delegation string. It is pass-through
  metadata — Gitmoot carries it through to the child job untouched and echoes it
  back in the coordinator continuation for each delegation that set a non-empty
  value, so the coordinator can group or label legs (for example `plan`,
  `implement`, `verify`). Like `model`, it is metadata only: it does **not**
  affect scheduling, loop detection, or termination, and it is not part of the
  delegation-set signature used for loop detection. There is no allow-list.
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
- Per-root wall-clock budget: `MaxDelegationWallClock = 2h`. The whole tree under
  one root is bounded in duration (measured from the root job's creation); a
  coordinator that tries to fan out after the tree has run this long is refused
  with a `delegation_walltime_exceeded` event. A generous runaway backstop, not a
  tight deadline.
- Per-root token budget (cost): `[orchestrate].max_delegation_token_budget`,
  **off by default** (`0` = unlimited). When set to a positive value, the whole
  tree under one root is bounded by cumulative token usage (input + output,
  summed across every job in the tree). A coordinator that tries to fan out after
  the tree has already used at least the budget is refused with a
  `delegation_cost_exceeded` event and routes through the finalize continuation.
  Token capture is **best-effort per runtime** — see the capture-status note
  below — so the budget can under-count a runtime that does not report usage; it
  never over-counts. Leaving the knob at `0` skips the check entirely (behavior is
  byte-identical to before the knob existed).
- Per-root **dollar-cost** budget (#380): `[orchestrate].max_delegation_cost_usd`,
  **off by default** (`0` = unlimited). The cost analogue of the token budget: it
  bounds the same tree by *measured spend*, derived from the same per-job token
  usage priced through a built-in per-model price table (Haiku/Sonnet/Opus list
  prices matched by substring; unknown/empty models priced at the mid-tier Sonnet
  default so they are never free). When the tree's accumulated cost reaches the
  budget, the next fan-out is refused with a `delegation_cost_usd_exceeded` event
  and routes through the finalize continuation — never hard-killed. Coarse
  runaway-cost backstop, not a precise spend meter; leaving the knob at `0` is
  byte-identical to before the knob existed.
- Per-coordinator width: `MaxDelegationWidth = 16`. A single coordinator result
  may not fan out more than this many delegations in one generation; an over-wide
  set is refused with a `delegation_width_exceeded` event and routes through the
  finalize continuation.
- Loop detection (two signals): a **structural** windowed signature over recent
  delegation sets halts a coordinator re-issuing the same set (e.g. oscillating
  A→B→A) well before the depth cap; a **result-aware non-progress streak** (#339)
  additionally catches a coordinator that perturbs the set each round to dodge the
  structural hash but whose children keep returning nothing new — it fingerprints
  the children's verifiable side effects (`decision`, `changes_made`, `tests_run`,
  PR/HeadSHA, `artifact_body`; self-reported summary/findings text is excluded) and
  trips after `MaxDelegationNonProgressStreak` consecutive generations with no new
  durable side effect (default `2`, configurable per-host via
  `[orchestrate].max_delegation_non_progress_streak`). Any new durable side effect
  resets the streak even if the summary repeats. Both share the same ladder
  (`delegation_loop_warning` → corrective continuation → `delegation_loop_detected`
  → finalize).
- Operator kill switch: `gitmoot job kill <root-job-id>` terminates a runaway
  tree by its root id from outside. It is the **first** backstop (operator action
  wins over every budget cap) and is graceful — in-flight jobs finish, the
  coordinator's next continuation routes through the finalize path below (a
  `delegation_killed` event is emitted), and the daemon stops new children.

When a bound trips (a budget cap or confirmed loop), the offending delegations
are not dispatched and the parent receives a typed lifecycle event explaining why
(for example, "delegation tree for root <id> reached the job budget of 64").
Rather than stopping silently, the engine then enqueues one **graceful finalize
continuation** back to the coordinator (`delegation_finalize_enqueued`): it is
told it cannot delegate further and asked to synthesize a best-effort final
result and return empty delegations. That continuation is terminal — any
delegations it returns are ignored (`delegation_finalized`) — so the chain always
stops with a clean synthesis instead of a dead end.

#### Token-capture status (per runtime)

The per-root **token budget** sums whatever token usage each job's runtime
reports at delivery time. Capture is **best-effort and uneven across runtimes**;
a job whose runtime reports no usage contributes `0` to the sum, so the budget
**under-counts** that runtime rather than failing. Current status:

| Runtime | Reports token usage? | How |
| --- | --- | --- |
| **Claude Code** | Yes | Parsed from the `usage.{input,output}_tokens` of the `--output-format json` envelope on delivery. |
| **Kimi Code** | Best-effort | Captured if the `--output-format stream-json` stream emits a `usage` object; otherwise `0`. |
| **Codex** | No (contributes `0`) | `codex exec resume … -- <prompt>` runs without `--json` (plain text), so delivery exposes no machine-readable usage. |

Because of this, a tree made up mostly of Codex jobs will accumulate little or no
counted usage — set the budget with that in mind, and prefer it as a coarse
runaway-cost backstop rather than a precise spend limit. A `$`-denominated price
table is intentionally **not** implemented yet; the budget is in raw tokens.

### Top-level fields

- `artifact_body` (optional): the artifact payload made available to delegated
  children. Required whenever any delegation sets `artifacts`. When the
  orchestrate policy enables it, a child's `artifact_body` can also be **inlined**
  into the coordinator continuation prompt — appended as a fenced block after each
  child's decision/summary/PR line, size-capped (per body and per continuation)
  and rune-safe truncated, with a marker pointing at the full on-disk brief at
  `<ArtifactRoot>/delegations/<parent>/brief.md`. Inlining is **off by default**;
  see `inline_artifact_bodies` in the orchestrate config docs
  (`docs/cockpit-orchestrate.md`). With it off, the continuation prompt is
  byte-identical to before.

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
