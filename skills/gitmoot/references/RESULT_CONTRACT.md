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

## Findings

`findings` is a free-form array — each entry may be a plain string or a JSON
object; there is no rigid schema. When you emit **object** findings, the posted
PR/issue comment renders them as readable markdown (a bold heading plus an
indented `key: value` sub-list) instead of an inline JSON blob, so a few
conventional keys make the result easier for humans to audit:

- **Heading**: the renderer uses the first present of `title`, `approach`,
  `name`, `summary`, or `finding` as the bold heading.
- **Qualifier**: the first present of `recommendation`, `severity`, or `status`
  is shown in parentheses next to the heading (e.g. `(PRIMARY)`, `(high)`).
- **Links**: a `source_url`, `url`, or `source` value is rendered as a clickable
  markdown link.
- **Nested values**: arrays become a nested bullet sub-list; nested objects
  render as `key: value` lines (depth-bounded).

None of these keys is mandatory. String findings still render exactly as the
plain text you provide, and any finding the renderer cannot map (for example a
top-level array) falls back to a pretty-printed fenced ```json``` block.

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
- `agent` (required): the **registered agent name** to run (e.g. `shipper`,
  `researcher`) — **not** a runtime. `codex`, `claude`, and `kimi` are *runtimes*,
  not agent names; naming one here is the common mistake that makes a fan-out
  unroutable. To run a worker that is **not** pre-registered, use the inline
  `ephemeral` spec below instead (it needs no registration). An unroutable `agent` (unknown / not allowed on the repo / lacking
  the action capability) does **not** silently dead-end: it emits a structured
  `delegation_preflight_failed` event listing the agents valid for the repo and
  routes the coordinator through a corrective continuation (see
  [Termination bounds](#termination-bounds) below).
- `action` (required): job action, e.g. `ask`, `review`, or `implement`.
- `prompt` (required): instructions for the delegated job.
- `deps` (optional): array of sibling delegation `id`s. This delegation runs
  only after every listed sibling succeeds. Each entry must reference a known
  sibling in the same result, may not be self-referential, and may not form a
  cycle — delegations form a DAG, and cycles are rejected. By default a
  dependent leg runs *blind* to its deps' outputs. The opt-in
  `[orchestrate].inject_upstream_dep_context = true` (#419) makes `deps[]` real
  dataflow: when a ready dependent leg is enqueued, each succeeded direct dep's
  result (decision, summary preview, PR link, `changes_made` count, short
  HeadSHA, then the fenced `artifact_body`) is appended to the dependent's
  prompt as a byte-budgeted "Upstream dependency results" block (same size caps
  as artifact-body inlining). Off by default; with the flag off the enqueued
  prompt is byte-identical.
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
- `synthesis_rule` (optional): one of `summary`, `vote`, `quorum`, or `verify`.
- `quorum` (optional): an integer `K` (`> 0`), required when `synthesis_rule`
  is `quorum`. The coordinator continuation proceeds only if at least `K`
  children reach an approving decision; otherwise the parent blocks, exactly as a
  failed `vote` does. `vote` is the special case where `K` equals the number of
  delegations (every child must approve). `K` is an integer count only — no
  fractions or percentages — and must not exceed the number of delegations (a
  larger `K` is unsatisfiable and is rejected).

  **`verify` — engine-enforced verify→replan (#439).** Where `vote`/`quorum`
  **block** the parent on failure (a terminal dead-end), `verify` does NOT block:
  when every child has resolved, the engine derives a pass/fail **verdict** from
  the `verify`-tagged leg(s) — a verify leg passes iff its decision approves
  (`approved`/`implemented`), and a `changes_requested`/`failed` or missing verify
  leg fails it — and on a **failed** verdict enqueues a single **bounded corrective
  "replan" continuation** (autonomous self-correction) instead of the normal
  synthesis continuation. The verify→replan loop is bounded by a dedicated per-root
  attempt cap (default **2**, configurable via `[orchestrate].max_verify_replan_attempts`);
  on exhaustion it routes to the graceful **finalize** continuation (#305) like
  every other backstop, never an unbounded loop. All existing structural bounds
  (depth/width/jobs/wall-clock/token/cost) still apply — each replan is just
  another continuation generation. The verdict is read mechanically from the
  already-completed verify leg (the convention below): the engine adds **no** new
  verify subprocess or second model call. `verify` requires no `quorum` field. A
  set that does not tag a `verify` leg behaves exactly as before.

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
  trusting the producer.

  There are **two ways** to drive the failed verdict back into a re-plan: a
  template pattern and an engine-enforced rule.

  - **Template pattern (#421).** Tag the verify leg with `failure_policy: escalate`
    (autonomous correction in the coordinator continuation) or `escalate_human` (a
    human-in-the-loop pause); the **coordinator** hand-rolls the verify→replan loop
    in its continuation. The merge gate independently blocks merge on the non-ready
    decision. The shipped `decompose-and-verify` recipe is one instance of this
    (parallel producers + one verify gate), and the `verifier` recipe is its
    minimal one-producer form — both are templates under
    `skills/gitmoot/agent-templates/`. No new primitive is involved: `EphemeralSpec`,
    `failure_policy`, and the merge gate already ship.
  - **Engine-enforced rule (#439).** Tag the verify leg with
    `synthesis_rule: verify` (see above). The **engine** then derives the verdict
    from the verify leg and, on a fail, enqueues the bounded replan continuation
    itself — so the verify→replan loop and its attempt cap are enforced engine-side
    rather than depending on the coordinator to drive them. Use this when you want
    the self-correction loop guaranteed and bounded by the engine; use the template
    pattern when you want full coordinator control over how the failure is routed.
- `timeout` (optional): a Go duration string and must be positive (e.g. `10m`).
  When omitted, the child job's timeout falls back to the `[orchestrate]`
  defaults (#548), with precedence **per-delegation `timeout` > phase default
  (`default_plan_timeout` / `default_implement_timeout` /
  `default_review_timeout` / `default_gate_timeout` / `default_repair_timeout`,
  keyed by the delegation's `phase`, falling back to its `action`) >
  `default_delegation_timeout` > unbounded** (the historical behavior; all keys
  are empty by default).
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
  **One exception (#548):** when the delegation omits `timeout`, a recognized
  phase value (`plan`, `implement`, `review`/`review-prep`/`review-dispatch`,
  `gate`, `repair`/`continue`) selects the matching `[orchestrate]`
  phase-default timeout — see the `timeout` bullet above.
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
    `read-only`. An **implement** ephemeral worker (`action: "implement"` or an
    explicit `"implement"` capability) **must** carry a write policy
    (`workspace-write` or `danger-full-access`); an empty/`auto`/`read-only`
    policy is rejected at validation with the same fail-closed guidance the CLI
    emits, because an unset policy normalizes to `auto`, which grants no
    deterministic headless write. Note `workspace-write` (`acceptEdits`) auto-
    accepts file edits but does NOT unblock Bash (`go`/`git`/`gh`), so full
    headless implementation needs `danger-full-access`. See
    `references/SAFETY.md` for the policy→permission-mode mapping.

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
  this depth may not delegate further. Override per host with the
  `GITMOOT_MAX_DELEGATION_DEPTH` env var (positive integer).
- Per-root job budget: `MaxDelegationTotalJobs = 64`. The whole delegation tree
  under one root is capped at this many jobs. Override per host with the
  `GITMOOT_MAX_DELEGATION_TOTAL_JOBS` env var (positive integer).
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
- Unroutable delegation set (preflight): every delegation (ready **and** deferred)
  is preflighted **atomically** before any child is enqueued — if even one names an
  agent that does not resolve to a routable registered agent (unknown / not allowed
  on the repo / lacking the action capability), **none** of the set dispatches (no
  partial fan-out). This is no longer a terminal block: the engine emits a
  structured `delegation_preflight_failed` event carrying an actionable reason (the
  agents valid for the repo, a runtime-name-mixup hint when the name is a runtime,
  and the `ephemeral` escape hatch) and routes the coordinator through the **same
  corrective continuation** as loop detection, so it can re-emit a corrected set. A
  coordinator that keeps naming bad agents is bounded by
  `MaxDelegationNonProgressStreak` → after a corrective nudge it routes to the
  graceful finalize instead of looping. The set is retryable once the agent names
  are corrected; no need to recreate the root job. Because the coordinator now ends
  `succeeded` (not blocked), the failure is surfaced from the
  `delegation_preflight_failed` event — not the job state — in `gitmoot job list`
  (a trailing `PREFLIGHT_FAILED:` column), the `gitmoot dashboard` **Attention**
  page (flagged with its reason regardless of state), and the `delegation preflight
  failures` count in `gitmoot daemon status`.
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
| **Codex** | Fresh sessions only | Read from the last `turn.completed` usage of the `codex exec --json` JSONL stream (#658) for fresh sessions (ephemeral delegation workers, per-job `--runtime` overrides). Resumed sessions contribute `0`: codex reports session-cumulative usage there, which would attribute the whole session history to each job. Older CLIs that predate `--json` fall back to plain text and contribute `0`. |

Capture is best-effort, so treat the budget as a coarse runaway-cost backstop
rather than a precise spend limit — a runtime that reports nothing (or an older
codex CLI that predates `--json`) contributes `0` and is silently under-counted.
The same capture also feeds the `$`-denominated
`[orchestrate].max_delegation_cost_usd` budget — see the dollar-cost bullet in
[Termination bounds](#termination-bounds) above.

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

- `human_questions` (optional): an **ask-gate** — the non-failure sibling of
  `escalate_human` (#445). A **healthy** result (any decision) may carry
  `human_questions[]` to **pause for a specific human answer instead of guessing**.
  Each entry is `{ "id": "...", "prompt": "...", "choices": ["...", ...] }` where
  `id` is required and unique within the result, `prompt` is required, and
  `choices` is optional. It is **fully additive and orthogonal to `decision`** — a
  result that omits it behaves byte-identically, and a coordinator can both fan out
  (`delegations[]`) **and** ask in the same result.

  When a result carries `human_questions[]`, the parent task enters the resumable
  `awaiting_human` state, **no continuation or delegation children are enqueued,
  and the tree consumes zero tokens/compute** until a human answers — exactly like
  `escalate_human`, but **no leg fails** (it is a healthy result that simply needs
  a decision). The daemon @-tags the human (default handle: the repo owner, or
  `[orchestrate].escalation_handle`) with the question(s) rendered, and the
  dashboard lists the tree under **Attention**. A human answers with
  `/gitmoot resume <coordinatorJobID> answer "<id>: ..."` — one `<id>: text` line
  per question (a single-question pause also accepts a bare answer body). The
  answer is delivered to the coordinator continuation as a clearly-labelled
  **"Human answers to your questions"** block injected at the top of its prompt;
  the coordinator then proceeds with the human's decision. An unmatched id (a typo)
  is surfaced as additional guidance, never silently dropped.

  The ask-gate reuses the **same** `[orchestrate].escalation_ttl` backstop (default
  24h): an unanswered ask auto-finalizes gracefully exactly like a failure
  escalation, paused time is excluded from the per-root wall-clock budget, and the
  pause is **budget-neutral** (it enqueues no job; only the eventual continuation
  occupies the single continuation slot). The `answer` verb is valid **only** on an
  ask round, and `retry`/`continue`/`abort` are valid **only** on a failure
  escalation round — a mismatch is rejected with a clear message. **Use the ask-gate
  sparingly**: ask only when you genuinely cannot proceed without a human decision,
  not on every result.

- `learnings` (optional): durable, keyed **facts** worth remembering for future
  jobs (agent persistent memory, #626). Each entry is
  `{ "key": "...", "scope": "repo"|"general", "content": "..." }` where `key` is a
  short stable handle (required), `content` is the fact itself (required), and
  `scope` is optional — `"repo"` (about this repository, the default) or
  `"general"` (true everywhere). It is **fully additive**: a result that omits it
  behaves byte-identically, and **most jobs should omit it**. Record a fact only
  when you learned something a future job would benefit from (e.g. "this repo's
  arm64 CI is flaky"), state it as a **fact, not a directive** ("always…"/"you
  must…" is rejected), and never put commands or secrets in it. Memory is **off by
  default** (opt in per agent with `[agents.<name>].memory = true`); when off,
  returning `learnings` is harmless and simply ignored. In the current observation
  phase, returned learnings are logged for measurement but are **not** injected
  into any prompt. See the *Agent Persistent Memory* concept doc.

## Decisions

- `approved`: review found no blocking issues.
- `changes_requested`: review found issues that should be fixed before merge.
- `blocked`: work cannot continue without human input or an external state change.
  A blocked job is *settled* (a delegation barrier will not wait on it and a
  `job watch` stops tailing it) but not *final*: it can be resumed via retry, so
  subsystems that stamp an end time or tear down live resources treat it as still
  live. See `internal/workflow` `IsSettledJobState` vs `IsFinalJobState` (#632).
- `implemented`: the requested implementation work was completed.
- `failed`: the attempted action errored or could not complete.

## Reporting Rules

- Do not claim tests were run unless they were actually run.
- Do not claim files were changed unless they were actually changed.
- Use `needs` for missing credentials, unclear scope, unavailable tools, failing
  external services, or required human decisions.
- Use `delegations` when another named Gitmoot agent should be invoked.
- Redact secrets from summaries, findings, raw command output, and examples.

## Automatic trace-harvested feedback (Mode A, off by default)

When `[skillopt].auto_trace_enabled = true` (default `false`), gitmoot derives
template-learning feedback from the **verifiable outcomes** an implement job
reaches — merged with passing CI vs. blocked at the merge gate, review
`changes_requested`, and a later **revert** (wired #467: the daemon detects a
merged GitHub Revert-button PR whose body is `Reverts owner/repo#NN`, maps it back
to the original PR's auto-trace row, and overwrites the prior positive with a
negative in place — gated additionally on the optional opt-out
`revert_detection_enabled`, unset = on whenever the harvester is on) — and writes
a synthetic `FeedbackEvent` (`source = auto-trace`, `reviewer = gitmoot-auto`, A/B
`choice`, `feedback_source = automatic_trace`) into a dedicated
per-template-version `auto-trace:<version>` eval run. This is **additive**
(`contract_version` stays `1`, no new result field), **best-effort** (a harvest
error never blocks or fails a job; it records an `auto_trace_harvest_failed` job
event), and **never promotes** — a human still promotes a candidate. With the knob
unset, no harvester runs and behavior is byte-identical. See
`docs/skillopt-exchange-contract.md` for the outcome→score mapping, the no-CI
guard, and the corrective-on-revert overwrite.

### Cross-family review soft signal (off by default)

When **both** `[skillopt].auto_trace_enabled = true` **and**
`[skillopt].cross_family_review_enabled = true` (both default `false`), a merged
implement job additionally runs a read-only **cross-family review leg**: a
reviewer of a *different runtime family* than the implementer
(codex→claude, claude→codex, kimi→claude — a registered review-capable agent of
another family if one is scoped to the repo, else an ephemeral different-family
read-only leg in the `verifier.md` style). The reviewer scores subjective quality
**and** scope-fidelity as ONE rubric — `coverage` / `containment` / `fidelity`
(intended scope = the implement job's instructions + task title + goal title vs.
the PR diff + `changes_made`) plus `architecture` / `readability` / `abstraction`
— each in `[0,1]`. The rubric is mapped to `dimension_scores`, fused by the
mean-of-dimensions path, and written as a **second** `FeedbackEvent` in the SAME
`auto-trace:<version>` run under a distinct item id (`review#<repo>#<pr>`) and the
fixed `gitmoot-review` reviewer sentinel, so it never overwrites the verifiable
floor row — and so a re-review by a *different* reviewer family overwrites the row
in place rather than accumulating a stale duplicate. The mapped mean **drives the
a/b choice** (a below-`0.5` mean registers as a non-baseline `b` vote, not a
baseline win) and an empty rubric writes **no row**.

This signal is **soft, judge-tagged, and weighted low**. The review row rides the
run's `feedback_source = automatic_trace` (no new contract field,
`contract_version` stays `1`) and its eval item carries `judge_derived = true`,
the `reviewer_runtime` family, and the projected `rubric_score`, so
the export/optimizer ranks it below the verifiable floor and below human gold —
**weight tiers: human gold > verifiable floor > cross-family judge > same-family
judge.** It is **best-effort and off the blocking merge path**: a review-leg
failure never blocks or fails a job (it records a `cross_family_review_failed` job
event). The rubric text is **never** injected into the implementer's prompt
(anti-gaming).

**Same-family fallback (with warning).** When *no* different-family reviewer is
available/authed, gitmoot falls back to a **same-family** reviewer — but never
silently: it logs and records a `cross_family_review_samefamily_fallback` job
event (self-preference bias applies), tags the review item `self_family = true`
(with `reviewer_runtime` carrying the family), and weights it *below* a
cross-family review. Only when **no** review-capable runtime is authed at all is
the review skipped (no review row).

The judge is uncalibrated in this slice — it is judge-tagged and weighted low (a
measure-the-judge calibration hook for #344/#345 is a follow-on). The review
signal feeds the optimizer like any other and, when the configurable
`[skillopt].auto_promote` policy (#463) lands, is subject to it (weighted-low +
judge-tagged — *not* barred from promotion); this slice simply preserves the
current **manual** promotion by writing only `eval_runs` / `eval_review_items` /
`feedback_events`.

### Deterministic checkers (objective, non-LLM signal, off by default)

When **both** `[skillopt].auto_trace_enabled = true` **and**
`[skillopt].deterministic_checkers_enabled = true` (both default `false`), a merged
implement job additionally runs an **objective, tool-measured** checker leg (#485)
— the complement to the subjective cross-family review. It runs plain external
**tools** (no LLM): `dupl`/`jscpd` for duplication, `golangci-lint` for lint,
`gocyclo` for cyclomatic complexity, plus a **pure-Go `diff_size`** metric parsed
from the PR patches (always available, no tool/checkout needed). Each is normalized
to `[0,1]` (fewer issues → higher), mapped to `dimension_scores`, fused by the
mean-of-dimensions path, and written as a **third** `FeedbackEvent` in the SAME
`auto-trace:<version>` run under the fixed `gitmoot-checker` reviewer sentinel + a
distinct item id (`checker#<repo>#<pr>`), so it **coexists with** both the
verifiable floor (`gitmoot-auto`) and the cross-family review (`gitmoot-review`)
instead of overwriting either. The optional `[skillopt].deterministic_checkers`
comma list selects which run (default `diff_size` only).

These dimensions are **objective and un-gameable** (tool-measured, not estimated),
tagged `objective = true` on the eval item (alongside the projected `checker_score`
and per-dimension `dimension_scores`), so the optimizer weights them distinctly
from the subjective judge row. The leg is **fail-safe throughout**: a missing tool
binary, no PR-head checkout, a tool error, or a timeout **SKIPS that one dimension**
(no row for it) and never errors the harvest, blocks the merge, or stalls the job;
an all-skipped run writes **no row**. It runs **off the blocking merge path**
(detached, timeout-bounded); a dispatch failure records a
`deterministic_checkers_failed` job event. It is additive (`contract_version` stays
`1`) and **never promotes** (writes only `eval_runs` / `eval_review_items` /
`feedback_events`). With the knob unset, no checker leg runs and behavior is
byte-identical.

### Candidate promotion policy + notifications (off by default)

When a candidate becomes **pending** (after `skillopt import` or `train
continue`), gitmoot can **notify** over the event stream and optionally
**auto-promote** it — both **off by default**, additive, and **importing never
promotes** (#471). The import write only ever creates the pending version; the
notify + auto-promote is a separate, config-gated step that runs *after* import
returns and, on a guardrails pass, calls the existing promote machinery.

- **`candidate.awaiting_promotion`** is emitted **once** for every newly-pending
  candidate over the `[events]` stream (`job_id` = version id, `root_id` = template
  id, a redacted score/samples/CI `detail`), **independent of the auto-promote
  policy**. The dashboard **Attention** page also lists pending candidates
  (read-only), so they are visible locally even with `[events]` off. Nil-safe:
  with `[events]` unset, nothing is emitted and behavior is byte-identical.
- **`[skillopt].auto_promote`** (default `false` = manual, byte-identical) gates
  auto-promotion on **checkable guardrails** read from the candidate's HARVESTER
  auto-trace run (`auto-trace:<version>`), not the human/markdown review run:
  `auto_promote_min_samples` (feedback-event count), `auto_promote_min_score`
  (`summary.score >=` threshold), and `auto_promote_require_external_ci` (≥1 real
  external-CI feedback event, keyed off the harvester's `auto-trace`/`gitmoot-auto`
  provenance so a cross-family review row cannot spoof it). **ALL configured
  guardrails must hold**, and **any** uncertainty — nil score, unset/garbled
  threshold, an unresolvable run or a store read error (treated as *feedback
  unavailable*), **zero feedback samples** (an absolute floor even when
  `min_samples = 0`), `auto_promote_require_measured_judge = true` (deferred, gated
  on #344), or `auto_promote_canary = true` with an unset/invalid
  `auto_promote_canary_sample` — **fails safe to notify, do not promote**. An unset
  `min_samples`/`min_score` is a **hard do-not-promote, not `0`**. On a pass it
  promotes via the existing path and emits **`candidate.auto_promoted`** so a human
  can review or roll back even in full-auto.
- **`[skillopt].auto_promote_canary`** (#484, default `false`) makes a
  guardrails-pass candidate a **canary** behind the live champion instead of
  promoting it straight to `current`: `auto_promote_canary_sample` (a fraction in
  `(0,1]`; `1.0` = all traffic) is the per-resolution probability a job routes to
  the canary version, and `candidate.canary_started` is emitted when it goes
  live. The daemon then watches a bounded regression window over the canary's
  harvested verifiable outcomes versus the prior champion: on parity-or-better it
  graduates the canary to `current` (`candidate.auto_promoted`); on a **material
  regression** it auto-rolls-back — champion stays current, canary rejected,
  **`candidate.rolled_back`** emitted. Fail-safe: too few outcomes, no champion
  baseline, or unreadable feedback **hold** (keep sampling); `auto_promote_canary
  = true` with `auto_promote_canary_sample` unset degrades to notify-only.

### Champion-challenger A/B preferences (Mode B, off by default)

For **ask / research** agents there is no verifiable terminal outcome, so Mode A's
harvester and the score/CI guardrails above cannot supply a promotion confidence.
**Mode B** (#473, scoped from RFC #463) closes the loop via **pairwise
preference**, and is **additive** (`contract_version` stays `1`) and **manual**:

- `gitmoot skillopt ab <agent> "<prompt>" [--challenger <versionId>] [--pick a|b]
  [--seed N] [--home path]` resolves the **champion** (the template's current
  promoted version) and a **challenger** (a pending candidate version, or
  `--challenger`), delivers **both** through the runtime adapter **serialized** (so
  the two one-shot asks never overlap and runtime-session locks release cleanly),
  presents them **label-shuffled** as Option A / Option B, and records the human
  pick mapped back to the correct role. **No pending challenger ⇒ a clean no-op**
  (`nothing to A/B …`), zero rows written.
- A pick writes a **2-option `eval_run`** (`OptionsCount = 2`,
  `metadata.feedback_source = preference_ab`, run id `skillopt-ab:<versionId>`),
  **two `eval_review_options`** (`champion` / `challenger`, answers stored as
  `eval_artifacts` via the blob path), and **one `RankedFeedbackEvent` per pick**
  (`ranking = [winner, loser]`, `reviewer = human`, `source = skillopt-ab`) that
  passes `validateRankedFeedbackEventOptions`. Repeated A/Bs of the same challenger
  each persist as a **distinct** row (a unique per-pick `source_url` keeps the
  `(run_id, item_id, reviewer, source, source_url)` conflict key distinct), so picks
  accumulate instead of overwriting one row. The `source = skillopt-ab` tag keeps
  Mode B rows separable from Mode A (`auto-trace`) and human gold.
- Each `(template_id, version_id)` variant is a **Beta-Bernoulli arm** (`Beta(1,1)`
  prior) persisted in `skillopt_bandit_arms`; a pick increments winner/loser, then
  the confidence **`P(challenger > champion)`** is recomputed by deterministic (seeded)
  Monte Carlo and surfaced as `NN% likely better over K samples`.
- That confidence is the scalar the **`[skillopt].auto_promote_min_confidence`** (nil
  default = ignored, byte-identical to #471) guardrail consumes: at the candidate
  notify seam it rides into `candidate.awaiting_promotion`'s detail and, when the
  floor is set, auto-promote additionally requires `confidence >= floor` backed by at
  least `auto_promote_min_samples` bandit pulls — a nil/low/thin confidence fails safe
  to notify-only. For a **genuine Mode B candidate** (an ask/research agent with no
  harvester score and no eval_run feedback rows), the bandit pulls stand in for the
  Mode A feedback-sample floor and the absent score, so the confidence gate alone can
  promote it — the loop actually closes for ask agents, not just when Mode A evidence
  is also present. A present score still has to clear `auto_promote_min_score`, and a
  set `require_external_ci` still applies (a pure ask agent simply leaves it off).
  **`bandit_min_samples`** (default 30) is the low-traffic floor for the DEFERRED auto
  loop; the manual `skillopt ab` CLI is always allowed.

See `docs/skillopt-exchange-contract.md` and `docs/events.md` for the full knob
reference and event shapes.
