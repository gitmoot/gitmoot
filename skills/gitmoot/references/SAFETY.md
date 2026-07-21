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
conflict. Immediately before entering the policy gate, it also checks the PR
branch for any queued or running `ask`, `review`, or `implement` job. An active
job produces a transient deferral: the task remains `ready_to_merge` (not
blocked), and the daemon re-evaluates it on the next tick. The native policy
merge gate therefore never intentionally squash-merges and deletes a branch
beneath in-flight work. (The separate pipeline auto-merge gate does not yet run
this branch-activity check; it squash-merges *without* deleting the branch, so it
cannot strand commits by branch deletion — closing that path is a tracked
follow-up.) When an
**external** system owns the merge decision instead, set
`GITMOOT_DISABLE_NATIVE_MERGE_GATE=1` (also `true`/`yes`/`on`; #545): Gitmoot
then **abstains** from its native merge gate — fail-closed, meaning it never
merges gatelessly; the external gate makes the call.

### No external CI: grace window, not instant pass (#596)

When a PR head reports **zero** external commit-statuses **and** zero check-runs,
Gitmoot does **not** immediately treat the repo as CI-less and stamp the
synthetic `gitmoot/ci` success. GitHub Actions creates a check-run a few seconds
*after* a head is pushed, so a single zero observation cannot distinguish "no CI
configured" from "CI not created yet" — and a fast approve path could otherwise
merge inside that window before CI exists. Instead the gate returns **pending**
and only concludes "no CI" after a **second consecutive zero-external observation
at the same head**, at least `min_ci_wait` (default `60s`) later. The gate is
re-evaluated every daemon poll, so a genuinely CI-less repo merges exactly one
grace window later. Two extra guards layer on top:

- **Workflow-aware (bounded):** if `.github/workflows/` exists at the head tree,
  the gate treats a zero observation as an Actions creation lag and stays pending
  — but only up to `max_ci_wait` (default `10m`) with the head unchanged. Past
  that bound it concludes no-CI, so a PR whose workflows never produce a check for
  it (docs-only under paths filters, tag-only / `workflow_dispatch`-only
  workflows, a non-targeted branch) still merges instead of wedging forever, while
  the ~seconds creation lag is still covered.
- **`[merge_gate] require_external_ci`** (global, or per-repo under
  `[repos."owner/repo".merge_gate]`; default `false`): when `true`, an empty gate
  **hard-blocks** with an actionable reason — *after* the wait window above, never
  during the creation-lag race — instead of ever stamping `gitmoot/ci`. Use it for
  repos you know always have CI. The block is classified transient (it is a
  repo-config/operator-policy condition, not a template defect), so it is not
  scored against the implement template. Optionally tune `min_ci_wait` /
  `max_ci_wait` in the same section.

Useful commands:

```sh
gitmoot lock list --repo owner/repo
gitmoot lock show owner/repo <branch>
```

## Autonomy Policy And Headless Write Permission

An agent's autonomy policy maps to a Claude Code `--permission-mode` (Codex uses
the equivalent `--sandbox`). It governs what the headless `claude -p` / `codex
exec` run is allowed to do:

| Policy | Claude `--permission-mode` | Headless write capability |
|---|---|---|
| `read-only` | `plan` | no writes (inspect/report only) |
| `workspace-write` | `acceptEdits` | file edits only — does NOT unblock Bash |
| `danger-full-access` | `bypassPermissions` | full: file writes plus `go`/`git`/`gh` via Bash |
| `auto` (the default) / unset | *(no flag emitted)* | non-deterministic — inherited from ambient Claude config |

Two consequences matter for implementation jobs:

- **`auto` is non-deterministic, not "always denied".** With `auto` (or an unset
  policy) Gitmoot emits no `--permission-mode`, so the headless run inherits its
  write capability from whatever ambient Claude config exists on the host
  (settings.json allow rules / `defaultMode` / prior directory trust). It writes
  where permissive settings exist and silently denies where they don't — a
  hidden, environment-dependent dependency on a job-critical decision.
- **`workspace-write` (`acceptEdits`) auto-accepts file edits but does NOT unblock
  Bash.** An implement job needs `go build`/`go test`, `git`, and `gh`; those run
  through Bash, which `acceptEdits` still gates. Use `danger-full-access`
  (`bypassPermissions`) for full headless implementation. `bypassPermissions`
  stays gated strictly behind explicit `danger-full-access`; a git worktree is not
  a sandbox, so Bash there still reaches the whole machine.

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

Claude runtime credentials live only in the owner-readable (mode `0600`)
`~/.gitmoot/runtime-auth.env`. Write them with `gitmoot auth set claude`, which
reads stdin and replaces the file atomically; rotate without restarting the
daemon. `gitmoot auth status` prints masked fingerprints only. To clear auth,
use `gitmoot auth unset claude`: it writes an explicit empty file so bootstrap
cannot resurrect a legacy or ambient token. When one managed variable is
selected, Gitmoot injects all three Claude auth names and blanks the absent
ones, preventing ambient API-key precedence from changing the selection.

An opt-in `[credentials] model_gateway = true` routes Claude through a
daemon-owned `127.0.0.1` reverse model gateway. Each delivery gets a random,
job-scoped placeholder; the daemon snapshots the real `runtime-auth.env`
credential, attaches it only on the allowlisted upstream request, and revokes
the placeholder when delivery returns. Unknown/revoked placeholders fail with
`401`, and an unavailable gateway or unallowlisted upstream fails the delivery
without direct-auth fallback. The child is also pointed at a credential-free
`CLAUDE_CONFIG_DIR` (a per-home mirror of the real Claude config minus
`.credentials.json`), because Claude Code otherwise prefers its cached
credential over the placeholder and the gateway would `401` every job (#936).
It is off by default and does not cover Codex or Kimi.

### Runtime ambient credential hygiene

The optional `[credentials] env_curation = true` policy curates only runtime
agent subprocess environments. Its default `github = "deny"` omits ambient
`GH_*`/`GITHUB_*`, points `gh` at a fresh empty `0700` config directory, and
disables interactive prompts. `github = "inherit"` explicitly restores ambient
GitHub environment inheritance. See `CLI.md` for the exact base allowlist,
runtime exceptions, and `env_passthrough` syntax.

The gateway has two explicit limits:

1. env-var routing is cooperative, not a hard egress boundary — a malicious
   agent can unset `ANTHROPIC_BASE_URL`, or point `CLAUDE_CONFIG_DIR` back at
   the real `~/.claude` to read the cached credential. This buys credential
   custody/policy/attribution against a prompt-injected or misbehaving agent,
   not enforcement against one that actively evades.
2. The strong "agents never hold real credentials" claim also requires
   Landlock read-rules for `runtime-auth.env` and `~/.claude/.credentials.json`
   (same-UID read is currently possible) — that is P3.

Codex/Kimi custody, Landlock read rules, MITM CA support, corporate
proxy/`NO_PROXY` interoperability, and hard egress enforcement remain P3.

## Pipeline Stages Run With Daemon Permissions

`gitmoot pipeline add` (#681) is an **operator-trust action**. A pipeline stage's
`cmd` is run **verbatim** via `sh -c` with the daemon's own permissions — there is
no sandbox, allowlist, or policy gate around a stage command the way there is around
an `implement` job. Registering a spec is exactly as privileged as pasting its stage
commands into the daemon host's shell, so only add specs you would run yourself, and
treat `pipeline add` from an untrusted source as arbitrary code execution.

The spec is stored **verbatim** (the raw YAML bytes plus a content hash), so a spec
that embeds private hostnames, filesystem paths, tokens-by-reference, or repo names
is retained in the local store as-is — the same private-repo caveat as a captured
agent template or a published template. Do not register a spec carrying a secret
in a stage command or inline `env`. Use an operator-owned `0600` pipeline
`env_file`, or register a name from the separate `gitmoot key path` keychain and
grant it to the pipeline, then select it with the shell stage's `env_keys`
allowlist.

Produce agent stages have a separate filesystem allowlist. `writes:` grants
read/write output directories. Optional `reads:` grants external input directories
read-only: Claude/Kimi receive a cooperative runtime visibility hint, but Landlock
is the enforcement boundary and installs those paths with read rights only. Gitmoot
refuses read roots that expose its home, the configured keychain, or the pipeline
`env_file`, and refuses a read root that contains a write root. Unsupported
Claude/Kimi hosts fail closed instead of falling back to advisory confinement;
Codex uses its native sandbox, and read paths are never passed as writable
`--add-dir` grants.

When Claude produce declares `reads:`, Gitmoot derives additional read-only
runtime resources from the operator's user settings: the Claude config directory,
`~/.claude.json`, and parent directories of absolute command-hook scripts. The same
Gitmoot-home/keychain/`env_file` exclusions override auto-discovery. An excluded,
missing, or unreadable script is refused before runtime launch with its path;
relative or malformed hook commands emit a `produce_runtime_resource_warning`
job event. No such discovery runs when `reads:` is absent.

Pipeline key access is deny-by-default. An `injected` selection gives only a
shell stage the real value, which it can print or transmit. Agent stages require
both a configured proxied key granted to their registered seat and an explicit
stage selector; injected agent grants are refused. A configured `proxied`
selection gives the selected shell or agent a per-job placeholder and loopback URL; Gitmoot
rereads the value and rechecks the grant on every request, pins forwarding to
the configured HTTPS origin/base path, and revokes the lease when delivery
ends. Gitmoot stores only `{stage,name,source,mode}` audit rows, never values or
hashes. Keychain and pipeline files are revalidated as owner-only `0600` files
outside Gitmoot state and managed checkouts.

For agent stages, the real value never enters the agent process. Proxied mode
hides key bytes; it does **not** prevent an authorized child from
exercising the credential on the pinned upstream. Curated upstreams and base
paths are part of the model, and only trusted upstreams should be configured.
Gate stages remain ineligible. Ordinary agent jobs receive nothing, and
delegation children do not inherit a stage grant. This generic pipeline proxy
is distinct from the Claude model gateway.

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
  event. Token capture is **best-effort per runtime** (Claude reports usage; Kimi
  reports it if its stream emits it; Codex reads usage from its `codex exec --json`
  JSONL stream — a resumed session's usage is session-cumulative, so it records only
  the per-session delta (#661) — falling back to `0` on an older CLI), so the budget can under-count
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

- Dismissed task terminality (#913): `dismissed` may be entered only from
  `implementing` or `blocked`. Manual `task dismiss` proves there is no live
  matching job and no process in the task worktree. Automatic stale-task
  reconciliation proves there is no live matching job and requires its separate
  open-PR/remote-branch evidence, but does not inspect worktree processes.
  Ordinary task run/allocation and late review or continuation advancement fail
  closed instead of overwriting it. Only `task recover` or retrying a job performs
  an explicit audited recovery. Dismissal never deletes the task branch or
  worktree.

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

## Pipeline service exposures

Service access is opt-in per pipeline and bounded schema. V1 exposes only
shell-only, template-free pipelines; submitted values are validated as typed
fields and delivered only through reserved `GITMOOT_INPUT_*` environment names,
never prompt text. Stages declaring `env_keys`, network access, or extra
read/write authority are ineligible for exposure. Every accepted shell stage
requires a detached worktree and fails closed if isolation cannot be allocated.
The listener binds to loopback by default; do not expose it to production
networks without explicit owner action, TLS, and a trusted firewall.

Successful service shell stages can publish up to 64 MiB total from `out/`.
Collection happens before detached-worktree disposal, rejects absolute/parent
paths, symlinks, and non-regular files, and binds each accepted file's name,
size, and SHA-256 digest into the store-verified proof. The authenticated service
bundle contains those bytes under `artifacts/<stage-id>/`; the public receipt
lists only their metadata and its sanitized bundle omits artifact bytes.

Both bundles contain the frozen #941 pipeline spec, including full shell command
bodies and referenced environment-variable names. Never inline secret literals
in `cmd`. Public receipt handlers only read an already completed archive.
Disabling an exposure blocks new runs but does not revoke authenticated reads or
polling for accepted runs; rotate the token to revoke the old bearer credential
(the public receipt URL remains public once finalized).

## When To Stop

Stop and report `blocked` when the target repo is unclear, GitHub auth is
missing, the daemon cannot access the repo, branch lock ownership is wrong, or
continuing would require credentials or destructive operations the user did not
approve.
