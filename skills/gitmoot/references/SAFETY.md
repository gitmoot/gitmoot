# Gitmoot Safety Reference

## Repo Scope

A PR repository is the routing context for `/gitmoot <agent> ...`. Always confirm
or pass `--repo owner/repo` when the user works across multiple repositories.

## Branch Ownership

Do not edit or push an implementation branch unless Gitmoot assigned the job.

Review and ask jobs should inspect and report without mutating branches unless
the task explicitly instructs otherwise.

Do not ask child agents to run PR lifecycle commands such as `git pull`,
`git merge`, `git push`, or `gh pr merge` to make parallel task PRs mergeable.
Gitmoot owns the final merge gate. It serializes merge attempts per base branch,
updates stale PR branches through GitHub when possible, retries pending states
through the daemon, and blocks clearly when GitHub reports a real merge
conflict. Immediately before entering the policy gate, it also checks the PR
branch for any queued or running `ask`, `review`, or `implement` job. An active
job produces a transient deferral: the task remains `ready_to_merge` (not
blocked), and the daemon re-evaluates it on the next tick. The native policy
merge gate therefore never intentionally squash-merges and deletes a branch
beneath in-flight work. When an
**external** system owns the merge decision instead, set
`GITMOOT_DISABLE_NATIVE_MERGE_GATE=1` (also `true`/`yes`/`on`; #545): Gitmoot
then **abstains** from its native merge gate — fail-closed, meaning it never
merges gatelessly; the external gate makes the call.

### Native merge requires exact-head review and CI (#1114)

Gitmoot enables native task auto-merge by default, but only when an affirmative
review verdict matches the exact current head SHA and all SHA-scoped commit
statuses and check-runs are green. Missing or failing evidence parks the task in
`awaiting_human_merge` and journals an org escalation from the most-specific live
role whose scope matches the repo to its nearest live chart ancestor. With no
live scope match it addresses a live `owner`; a root match addresses itself.
With no live upward recipient, the gate fails closed without journaling an
addressed note. Archive filtering does not predict delivery; `org validate`
reports missing wake routes and pane bindings. Set `auto_merge = false` globally
or per repo as an explicit kill-switch:

```toml
[repos."owner/repo".merge_gate]
auto_merge = false
```

The kill-switch hold is not a quality block, is not stale-task reaped, and does
not escalate. A human GitHub merge is reconciled normally, while `@gitmoot merge`
is an explicit authorized merge request. Turning `auto_merge` back on re-arms
only tasks parked for this exact kill-switch reason.

A repository can instead opt into low-risk auto-merge while the global switch
stays off:

```toml
[repos."owner/repo".merge_gate]
auto_merge = "low_risk"
```

The gate then merges without a human only when the exact head has a clean
exact-head approval, real green external CI (no CI keeps it manual; the
synthetic `gitmoot/ci` success is not used), no `hold`/`do-not-merge` label, is
not a draft, and the review-level classifier puts that exact head at level 1 or
2. The level is the newest `~/.gitmoot/review-levels.jsonl` entry for that head,
or the daemon classifies the head like `gitmoot review level` and records it; a
classifier failure is reused for an hour before a retry. Level 3, a classifier
error or a head that moved keeps the pull request open with a reason starting
`low-risk auto-merge left the pull request open for a human merge:` that names
the failed condition. After a level 2 auto-merge the daemon requests the
post-merge review once for the acting role of the exact-head review job, or
records a `low_risk_post_merge_review_owed` task event when it cannot.
`gitmoot/gitmoot` always requires review and is refused as a config error.

When review independence cannot be verified, the merge gate names the evidence
it observed: no implement job for the task, implement jobs that do not match the
task identity, a matching implement job with no agent, or a malformed implement
payload. All four remain fail-closed. The no-job case is an **attribution gap,
not a failed independence check**: the gate cannot establish *who* implemented,
which is a different fact from "the reviewer implemented this", and it is the
expected state for in-session (pane-driven) implementation, where no engine
implement job exists. The approval is not disqualified. The remedy is runnable
by the implementing lane and needs no coordinator: record the durable
attribution row with `gitmoot job record --agent <implementing-agent> --repo
<owner/repo> --type implement --decision implemented --task <task-id> --pr
<number> --head-sha <sha>`, then re-evaluate. Session-recorded jobs (#657)
create rows with `Type == "implement"`, which is exactly what the gate reads. If the work was done in
session by an org role that is not a registered agent, pass `--acting-role
<role>` instead of `--agent`: attribution is then the role, validated against the
configured org registry, and the gate still enforces independence against it.
Never record an agent that did not implement, and never record the reviewer.

### No external CI: grace window, not instant pass (#596)

When a PR head reports **zero** external commit-statuses **and** zero check-runs,
Gitmoot does **not** immediately treat the repo as CI-less and stamp the
synthetic `gitmoot/ci` success. GitHub Actions creates a check-run a few seconds
*after* a head is pushed, so a single zero observation cannot distinguish "no CI
configured" from "CI not created yet". Instead the gate returns **pending**
(a non-escalating, automatically retried hold, not a policy miss, so it does
not park the task or enqueue an org escalation) and only concludes "no CI" after
a **second consecutive zero-external observation at the same head**, at least
`min_ci_wait`
(default `60s`) later. The gate is re-evaluated every daemon poll, so a
genuinely CI-less repo merges exactly one grace window later, provided the
exact-head review is also clean. Two extra guards layer on top:

- **Workflow-aware (bounded):** if `.github/workflows/` exists at the head tree,
  the gate treats a zero observation as an Actions creation lag and stays pending
  — but only up to `max_ci_wait` (default `10m`) with the head unchanged. Past
  that bound it concludes no-CI, so a PR whose workflows never produce a check for
  it (docs-only under paths filters, tag-only / `workflow_dispatch`-only
  workflows, a non-targeted branch) still merges instead of wedging forever, while
  the ~seconds creation lag is still covered.
- **`[merge_gate] require_external_ci`** (global, or per-repo under
  `[repos."owner/repo".merge_gate]`; default `false`): when `true`, an empty gate
  is left open with an escalation — *after* the wait window above, never during
  the creation-lag race — instead of ever stamping `gitmoot/ci`. Use it for repos
  you know always have CI. Optionally tune `min_ci_wait` / `max_ci_wait` in the
  same section.

A check or status that genuinely reports failure (not merely absent) always
leaves the PR open with an escalation, regardless of the grace window.

## Autonomy Policy And Headless Write Permission

An agent's autonomy policy maps to an omp `--approval-mode`. It governs what the
headless `omp -p` run is allowed to do:

| Policy | omp `--approval-mode` | Headless write capability |
|---|---|---|
| `read-only` | `always-ask` | no writes (inspect/report only) |
| `workspace-write` | `write` | in-directory file edits only — shell commands and writes outside the working directory are refused |
| `danger-full-access` | `yolo` | full: file writes plus `go`/`git`/`gh` via shell |
| `auto` (the default) / unset | `yolo` | widened to full access |

Two consequences matter for implementation jobs:

- **`auto` widens.** The flag is always emitted for determinism (omitting it
  would inherit the host's `tools.approvalMode`), and `auto` maps to `yolo`, so
  the instrumentation records it as `widened`. A kernel-enforced read-only seat
  also passes `yolo`, with Landlock as the write boundary.
- **`workspace-write` (`write`) permits file edits but does NOT unblock the
  shell.** An implement job needs `go build`/`go test`, `git`, and `gh`; use
  `danger-full-access` for full headless implementation. A git worktree is not
  a sandbox, so a shell there still reaches the whole machine.

### Runtime contract preflight

Engine jobs also pass a lazy installed-runtime contract preflight before launch.
Adapter metadata declares the exact CLI flags its argv uses, while environmental
refusals live beside the triggering argv. A parsed help document that omits a
required flag, or a definitively failed precondition, is `unsupported` and
blocks with the runtime, installed version, exact requirement, and remedy. A
missing binary, timeout, unparseable help, or undecidable precondition is
`unknown`: Gitmoot records that fact rather than treating it as a refusal. This
fail-open unknown state is deliberate; inability to inspect a CLI is not proof
that the CLI rejected the contract. A missing binary carries one added refusal,
and only under three explicit conditions together: the dispatcher declares that
this dispatch builds a real adapter which will exec that declared CLI, the
execution backend runs on the local host, and the executable does not resolve.
That refusal lands before any job row or worktree exists, and it leaves the
`unknown` classification intact; injected or fake adapters, remote or attached
backends, and present binaries stay dispatchable. Unknown binary probes use a
60-second cache TTL; successful probes cache by resolved path, size, and mtime
without a TTL.
Doctor uses the foreground process's `PATH`, so compare
its reported `resolved_path` with the daemon EnvironmentFile's `PATH` when they
may resolve different binaries.

#### Read-only seat runtime capability

A read-only seat resolves its runtime from a daemon-staged copy, never from the
host PATH. When the daemon cannot stage a runtime it publishes that name as an
engine-owned command that exits 126, so an inherited PATH entry cannot route the
seat back to an ungranted operator installation. If the runtime published that
way is the seat's OWN runtime, seat setup refuses the job: the job ends
`blocked`, a `seat_runtime_unavailable` event records the agent, the runtime and
the staging cause, and no adapter is composed, no agent instance is marked
running, and no model token is spent. A sibling runtime published unavailable is
a fact about the host, stays a daemon log line, and refuses nothing.

This check runs where the seat resolves its command, not on the dispatching
host's PATH. The two answers differ: a host can carry a working installation
while every seat receives the exit-126 shim, so a dispatch-time `LookPath`
reports the runtime present for a runtime no seat can run.

#### Read-only seat scratch space and checks

A read-only seat writes only its own cache root and a private temp dir,
`/tmp/gmr-<8 hex>` (mode `0700`, granted to that seat alone). `TMPDIR`, `TMP`
and `TEMP` point at it, and it is short enough for tests that bind Unix sockets
under `t.TempDir()`. The checkout, the gitmoot home, the rest of `/tmp` and
other seats' `gmr-*` directories stay read-only. The daemon removes the temp
dir when the job ends; after a crash, the next daemon start removes every one
whose owner process is gone. A short daemon `TMPDIR` (at most 16 bytes) replaces
`/tmp` as the parent.

Offline Go builds work: before the seat starts, the daemon downloads the
checkout's modules into a per-repository cache under
`<gitmoot-home>/cache/seat-go-mod/` and grants it to the seat read-only with
`GOPROXY=off`. Python (`pytest`, `ruff`, `uv`), Rust, Node and Flutter/Dart
checks do not run in a local seat: their toolchains live in operator-owned
trees a seat is not granted, and granting or copying those trees is not
bounded. They move to disposable VMs under #2315. A seat that cannot run its
checks says so with `evidence: static_only` and a `checks_blocked` reason (see
RESULT_CONTRACT.md).

#### Shell runtime risk acceptance (2026-08-05)

An operator-authored shell command is accepted as an explicit daemon-permission
risk: the `shell` adapter has no policy flag to apply, and the command runs with
the daemon's host permissions. Its warning records this dated acceptance. That
acceptance is provenance-specific, not a blanket exemption for the `shell`
label: a shell session that invokes a model CLI such as `omp -p` remains
`not-applied` without the acceptance reference.

### Fail-closed implement guard

Because of the above, Gitmoot **refuses** an agent (or ephemeral worker) that
carries the `implement` capability while its policy grants no headless write
(`auto`/empty or `read-only`), rather than letting the job run and produce no
files. The refusal fires at four seams with the same actionable message:

- `gitmoot agent start` and `gitmoot agent subscribe` (exit 2 before any session
  is spawned);
- implement-job **dispatch** (the job is BLOCKED — this catches pre-existing
  agents and later policy edits);
- ephemeral delegation specs at `validateEphemeralSpec` (an ephemeral implement
  worker must carry an explicit write policy).

To fix a refusal: set `--policy danger-full-access` for full headless
implementation, or `--policy workspace-write` for edits-only (knowing Bash stays
blocked). `read-only`/`ask`/`review` agents are never affected.

## Files And Secrets

Do not commit generated data, caches, logs, build outputs, session archives,
cloned helper repos, secrets, credentials, or large artifacts unless the user or
plan explicitly says they are intended tracked fixtures or release assets.

Redact secrets from GitHub comments, job summaries, raw examples, and copied
command output.

Read-only seats never receive recursive read access to an operator runtime or Go
installation. Gitmoot copies the selected omp artifact and a
Go installation selected from the daemon's `PATH` to satisfy the effective
`go.work` and checkout-root `go.mod`. Only daemon-owned copies are granted;
an external selected workfile receives a file-only read grant. Missing,
unsatisfying, or unstageable commands are shadowed by engine-owned exit-126
commands; inherited `PATH` remains available for ordinary system tools but
cannot fall through to an ungranted host runtime.

An opt-in `[credentials] model_gateway = true` makes broker material available
to remote jobs. For remote shell jobs it can issue an ephemeral mTLS client
identity and capability bound to the sandbox id, runtime, allowlist, and lease
expiry. Only a curl-config path enters the environment; the upstream key is
resolved on the host after authentication and never enters the sandbox.
The host transport decodes negotiated gzip before filtering. Initial residual
`Content-Encoding` and unexpected HTTP/2 responses fail before body release.
Credential-bearing response field names are dropped; remaining field values
and body bytes are filtered incrementally with bounded carry-over, so streaming
does not wait for EOF. ASCII-case variants and standard reversible URL/base
encodings remain best-effort defense in depth. The contract covers accidental
exact-byte reflection by a trusted, operator-selected upstream. Malicious
upstreams and transformed application payloads, including application-layer
compression without `Content-Encoding`, are out of scope. Model runtimes remain
refused remotely until they can present that identity.

### Runtime ambient credential hygiene

The optional `[credentials] env_curation = true` policy curates only runtime
agent subprocess environments. Its default `github = "deny"` omits ambient
`GH_*`/`GITHUB_*`, points `gh` at a fresh empty `0700` config directory, and
disables interactive prompts. `github = "inherit"` explicitly restores ambient
GitHub environment inheritance. See `CLI.md` for the exact base allowlist,
runtime exceptions, and `env_passthrough` syntax.

The gateway has one explicit limit: env-var routing is cooperative, not a hard
egress boundary — a malicious agent can unset it. This buys credential
custody/policy/attribution against a prompt-injected or misbehaving agent, not
enforcement against one that actively evades.

Landlock read rules, MITM CA support, corporate
proxy/`NO_PROXY` interoperability, and hard egress enforcement remain P3.

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
  The check is projected: the new jobs a batch would add (ready and deferred
  legs, minus already-enqueued/fingerprint-deduped ones) are counted before any
  child is enqueued, so a wide fan-out from just under the cap is refused whole
  rather than overshooting it.
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
  event. Token capture is **best-effort per runtime** (omp sums its NDJSON
  `message_end` usage across the run; `shell` reports none), so the budget can under-count
  but never over-counts. Leaving the knob at `0` skips the check entirely.
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
  children of the killed root. Kill is for a whole **tree**; to abandon a single
  job (including one blocked awaiting a human) use `gitmoot job cancel <job-id>`
  instead — or clear a stale backlog of blocked jobs with `gitmoot job cancel
  --state blocked --older-than 7d --yes` (dry-run without `--yes`). Cancel is a
  single-row transition that never propagates to siblings or the coordinator, so
  it must not be routed through the kill machinery (#631). Setting a positive
  `[orchestrate].blocked_ttl` (off by default) lets the daemon auto-dismiss a
  blocked job idle longer than the TTL through that same cancel path — the
  single-job counterpart to the tree-wide `escalation_ttl` below.
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
  `/gitmoot resume` comments on the tree's **open** PR or issue; the TTL backstop
  covers a tree whose PR/issue is no longer open.
- Non-failure ask-gate (`human_questions[]`, #445): the **healthy-result** sibling
  of `escalate_human`. A worker/coordinator that returns a healthy result carrying
  `human_questions[]` pauses the parent task at the **same** resumable
  `awaiting_human` state for a specific human answer — **no leg fails**, no
  continuation or delegation children are enqueued, and the tree consumes zero
  tokens/compute. It reuses the **same** pause plumbing as `escalate_human` (one
  `delegation_escalation_requested` event tagged `kind=ask` with the questions and
  the @-tag comment), so the **same** `[orchestrate].escalation_ttl` auto-finalizes
  an unanswered ask, the **same** wall-clock pause exclusion applies, and the pause
  is **budget-neutral** (it enqueues no job; only the eventual answer-driven
  continuation occupies the single continuation slot). A human answers with
  `/gitmoot resume <coordinatorJobID> answer "<id>: ..."` (authorize-commenter
  gated); the answer is injected into the coordinator continuation prompt. The
  `answer` verb is valid only on an ask round and `retry`/`continue`/`abort` only
  on a failure round — a mismatch is rejected with a clear message. Absence of
  `human_questions[]` is byte-identical to today's behavior.

- Dismissed task terminality (#913): `dismissed` may be entered only from
  `implementing` or `blocked`, and only by automatic reconciliation — no command
  dismisses a task. Automatic stale-task reconciliation proves there is no live
  matching job and requires its separate open-PR/remote-branch evidence, but
  does not inspect worktree processes. Ordinary task allocation and late review
  or continuation advancement fail closed instead of overwriting it; retrying
  one of its jobs performs the only explicit audited recovery, and `task
  recover` is gone with implementer dispatch (#2203). Dismissal never deletes
  the task branch or worktree.

- Dead implement worktree retries (#994): a queued top-level implement job may
  bypass only the dirty-checkout pre-flight when the dirty path resolves to that
  job's own recorded task worktree, its repo and branch still match, no worktree
  process or runtime-owner lease is live, and the bounded retry budget remains.
  Gitmoot re-delivers with an explicit uncommitted-work notice and leaves commit,
  push, and PR creation to the normal finalizer. Delegation children, dirty
  registered/other checkouts, wrong-head and lock failures, and any live or
  uncertain owner keep their existing blocking/failure paths.

When a bound trips, the offending delegations are not dispatched and the parent
receives a typed lifecycle event explaining why (for example, a delegation batch
of new jobs would exceed the per-root job budget of 64). Rather than stopping
silently, the engine then enqueues one **graceful finalize continuation**
(`delegation_finalize_enqueued`) back to the coordinator — told it cannot
delegate further and asked to synthesize a best-effort final result and return
empty delegations. That continuation is terminal: any delegations it returns are
ignored (`delegation_finalized`), so work is bounded and always ends in a clean
synthesis, not a silent dead end.

## When To Stop

Stop and report `blocked` when the target repo is unclear, GitHub auth is
missing, the daemon cannot access the repo, or
continuing would require credentials or destructive operations the user did not
approve.

## Delegation Worktree Reclaim

Local review dispatches use the same owned read-only-worktree lifecycle as
read-only delegations, but allocate at the requested PR head rather than
checkout `HEAD`. The dispatch keeps `head_sha` in the payload and fails closed
if allocation, the pull-ref retry, or the 5 GiB free-space preflight fails. The
stable review Task row does not own this checkout. Background taskless asks keep
their separate fail-open committed-tip isolation policy.

An engine-dispatched `changes_requested` fix uses a different owned shape: an
independent writable clone attached to the real task branch at its fetched
remote head. Dispatch fails closed if that clone cannot be allocated; it never
falls back to the registered checkout. No automatic path deletes the clone.
Terminal cleanup records it for an operator without deleting the task branch. Allocation recovery and enqueue failures atomically
rename it to a `.ttl-reclaiming-orphaned-*` sibling so retries can proceed
without destroying data.

Ordinary and aged cleanup of recognized owned delegation/read-only worktrees use
the same producer-owned admission gate. A terminal owner alone is not enough:
blocked/resumable consumers, declared but undispatched dependencies, unknown
ownership or process state, and preservation failures all retain the resource. Succeeded owners require a stored result. Failed or
cancelled owners additionally require a proven reconstruction path; currently
this is a native read-only PR review pinned to an exact head.
Failed/cancelled implement legs and every other unproven owner therefore remain
retained indefinitely while they lack that proof, including their owned branches.
TTL is a minimum age for eligible cleanup, not a maximum retention or disk bound.
`retry_disposition_unknown` cannot be overridden by `apply` or `reopen`; there is
no force-disposal command. This intentional safety tradeoff can accumulate disk
usage. Inspect the job and escalate capacity to its owner; retry only when the
work itself should resume. Do not retry merely to discard evidence or bypass the
gate with direct deletion.

A configured `[worktrees]` root (another filesystem, e.g. an SSHFS mount) is
used only after a mount preflight: mount point, source and filesystem type must
match `/proc/self/mountinfo` and the root must sit on that mount. A failed
preflight refuses allocation, reopen and cleanup under that root
(`worktree root unavailable`); nothing falls back to `<home>/worktrees`, and an
absent mount is never treated as an already-removed worktree. Cleanup there is
deferred with refusal code `worktree_root_unavailable` and never quarantined for
an outage. Mount checks need Linux 5.6+; otherwise a section with mount keys
refuses every allocation.

Janitor pressure admission (`[janitor_capacity]`, #2359) is off by default. When
enabled it only HOLDS new heavy work (queued jobs stay queued, foreground
dispatch refuses before it writes); it never cancels running work, never
deletes anything and never overrides `[disk_guard]`. Any doubt about janitor's
answer (stale, unknown, uncovered device, other policy hash, janitor failing)
holds.

Aged cleanup also requires the generation's terminal time to precede
`[workflow].delegation_worktree_ttl` (default `72h`; `"0"` disables aged cleanup).
Ordinary succeeded-owner cleanup keeps its existing retention policy. Neither
path overrides quarantine, checkout locks or live-process protection.
Successful aged removal and metadata reconciliation record
`delegation_worktree_reclaimed_ttl`.

No NEW fix clone is ever created: #2203 removed the fix-worktree lifecycle
along with the auto-fix leg that allocated one. The clones already on disk are
still discovered, sized, and reported, and an aged one is retained rather than
deleted. Deleting a standalone clone deletes its object
database, and Linux has no inode-conditional unlink that can make a deletion
match a preceding proof. Commit reachability and nested-repository checks can
diagnose obvious unpublished content, but they do not prove the absence of
every loose blob, tree, annotated tag, pack, or concurrent write. Dropping the
payload field that used to mark these clones only reclassified them from
pinned/reclaimable to unproven; it never made them invisible.

Cleanup obligations for fix clones stay OPEN. Managed-path absence is not
removal evidence, even when no sibling is found; a clone may have been set
aside, and only a durable operator action can close that work item. Dangling
symlinks are inspected with `lstat` and never treated as absent targets.
`gitmoot doctor` and `/api/health` discover the canonical `fixes/` directory
structurally, including set-asides created before any job row exists. Discovery
and logical-size accounting each stop after 4096 entries; `truncated=true` and
the summary identify lower-bound results, and doctor warns.

The daemon's pending and aged delegation-reclaim queries return at most 256 due
owners host-wide. Attempted candidates and selected rows skipped by repository,
session, lifecycle, or checkout-liveness filters persist a later next-attempt
time, so fairness survives daemon restarts instead of depending on an in-memory
cursor. The aged and terminal-task passes additionally attempt at most eight
candidates per repository per tick.
The task pass is driven by terminal task lifecycle state rather than age.
Candidate-local lookup, runner, and removal failures skip only that worktree so
later candidates continue; candidate-query and store-wide lookup failures still
abort. Repeated failures log three times per path before suppression. Cleanup
attempt state is also durable: every failure waits one minute before retry, and
the third failure moves the obligation to terminal `quarantined` state. A
quarantined target is excluded from automation until an operator verifies its
job and expected path, then runs `gitmoot job cleanup reopen <resource-id>`.

`gitmoot doctor` and dashboard `/api/health` surface stale count and logical
size, split into reclaimable final owners, pinned non-final owners, and unproven
directories, plus the quarantined cleanup count. Pinned, unproven, and
quarantined worktrees are observation-only.

For an explicitly authorized manual cleanup, use the producer contract:

```sh
gitmoot job cleanup inspect --json <job-id>
gitmoot job cleanup apply --resource <resource-id> --token <eligibility-token> --json <job-id>
```

Inspection is advisory, not a lock or deletion grant. Apply derives the path
from producer metadata and rechecks consumers, lifecycle generation, policy,
physical identity and liveness under the lifecycle fence, then the checkout
lock. Retry/resume and dependent enqueue share that fence; no SQLite write
transaction remains open during filesystem removal. Never substitute direct
`rm`, `git worktree remove`, or a Janitor path scan for this admission.

Immutable per-generation receipts survive removal in the producer database.
Replaying a completed token returns its historical receipt without touching a
new tree at the same path. An interrupted old episode superseded by retry is
reported as `superseded`, not as a successful removal. Accounting-only recovery
can complete after retention changes; remaining destructive effects still need
current admission. No replay automatically reopens quarantine.

`approved_size_budget: null` means unknown, not zero or unlimited. Since schema
v2 (#2360) the consumer path (`job cleanup inspect`/`apply`) is budget-gated: it
refuses unless a producer-owned `[cleanup_budget]` cap exists and a fresh, complete
allocated-bytes measurement is within it, measured again right before removal.
Unknown or incomplete measurements refuse; every refused apply leaves a durable
refusal receipt. Automatic ordinary/aged reclaim is not budget-gated and records
`budget_gate: "not_applicable"`. Shared checkouts, orphan paths and
standalone fix clones remain outside this cleanup API.

## Shared Tool Cache (isolated worktrees)

An isolated-worktree job (a per-job worktree under `$GITMOOT_HOME/worktrees/...`)
gets `UV_CACHE_DIR`, `PIP_CACHE_DIR`, `npm_config_cache`, `GOCACHE`, and
`GOMODCACHE` pointed at one shared, host-level directory (default
`$GITMOOT_HOME/cache/tools`) instead of each isolated job re-materializing its
own copy of immutable, content-addressed package caches inside its worktree —
which otherwise grows unbounded with fleet lifetime and is deleted only when
the reaper removes the worktree (#1113). Enabled by default; set `[cache]
enabled = false` to opt out, or `[cache] dir = "/absolute/path"` to relocate it
(must be absolute). The shared directory is created on first use and is never
swept by worktree teardown. Non-isolated jobs (the persistent registered
checkout) are unaffected — they do not duplicate caches per job.

Read-only seats run inside Landlock, so this also widens their writable-path
grant to a per-worktree cache root (an untrusted review or ask must not poison
state another job consumes). Non-seat omp and `shell` runtime jobs already run
unsandboxed, so only the env redirect applies to them.
