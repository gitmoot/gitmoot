# Run Jobs In Parallel On A Repo

By default the daemon runs queued jobs **one at a time per repo**: `--workers`
defaults to `1` and the scheduler defaults to `barrier` (a per-tick batch that
serializes same-repo jobs on one checkout lock). That is the safe, conservative
default — but it means a fan-out of independent jobs on one repo runs serially
unless you opt in to parallelism.

This guide shows the one-step on-ramp.

## The short version

```sh
# Intent-level: workers + the pool scheduler together, named for the goal.
gitmoot daemon start --parallel 5

# Equivalent, explicit form:
gitmoot daemon start --workers 5 --scheduler pool
```

`--parallel N` sets `--workers N` **and** `--scheduler pool` in one flag. It is
sugar for the explicit pair and cannot be combined with `--workers` or
`--scheduler` (that would be ambiguous).

If you raise `--workers` above 1 and do **not** pass `--scheduler`, the daemon now
**auto-selects `pool`** for you — requesting multiple workers under the serializing
`barrier` is almost never what anyone wants:

```sh
gitmoot daemon start --workers 5     # auto-selects --scheduler pool
```

To keep the old per-tick semantics with multiple workers, ask for them
explicitly — an explicit `--scheduler barrier` is always honored:

```sh
gitmoot daemon start --workers 5 --scheduler barrier
```

## Confirm the daemon is configured for it

`gitmoot daemon status` reports the scheduler mode and worker count, so you can
answer "is the daemon actually set up for the parallelism I asked for?" without
re-deriving it from launch flags:

```text
daemon running pid 12345
log: ~/.gitmoot/logs/daemon.log
scheduler: pool, workers: 5
claude auth: ok (...)
```

If you started under `barrier` with multiple workers, the line flags it and points
at the fix.

## Preflight warning

If a parallelizable workload is queued under a serializing config, the daemon
**logs an actionable warning** with the exact relaunch command instead of silently
serializing the work:

```text
warning: 3 parallelizable jobs queued for owner/repo will run serially under the
current scheduler config; relaunch with: gitmoot daemon restart --parallel 3
```

"Parallelizable" is counted conservatively: same repo, dependency-unblocked
(already true of queued jobs), and **distinct runtime sessions** (see the next
section). Two jobs on the same agent session are *not* counted as parallelizable,
because they serialize regardless of the scheduler.

The warning is rate-limited: it is re-logged only when the parallelizable set
changes, not on every poll, so a steady backlog does not spam the daemon log.

## What actually runs in parallel (two serialization layers)

Parallelism under `pool` is bounded by **two** independent locks, not one:

1. **Checkout lock (per worktree).** Jobs run concurrently only when they have
   **distinct checkout keys**. Delegation / orchestra `implement` children already
   get their own worktree from the workflow engine, so they parallelize. Read-only
   `ask`/`review` jobs can be auto-isolated into an ephemeral detached worktree.
   A plain, top-level same-repo `implement` job with **no** worktree still shares
   the `repo:<repo>` key and serializes even under `pool`.

   **Auto-isolated read-only worktrees are the committed tip.** An auto-isolated
   read-only worktree is a detached `git worktree add` at the **committed tip** of
   the base branch, so it does **not** contain gitignored paths (e.g. vendored
   clones under `repos/**`) or any uncommitted working-tree changes. Isolation only
   kicks in when a same-repo read-only job is **contended** — a delegation fan-out
   of **two or more** read-only siblings, or two-plus independently-fired top-level
   `ask`/`review` jobs on one repo; a **single**, uncontended read-only job stays in
   the shared base checkout and sees everything. Every auto-isolated job's prompt
   carries a note with the canonical base-checkout absolute path, so a worker whose
   sandbox can read it (e.g. codex) reaches the real tree instead of reporting a
   working-tree feature as missing. For whole-working-tree analysis that must see
   gitignored or uncommitted state, either keep the job uncontended (a lone
   read-only job stays in the base checkout) or pass an **absolute** path to the
   file/dir under analysis.
2. **Runtime session lock (`runtime:<runtime>:<ref>`).** Two jobs that use the
   **same agent/runtime session** serialize on the session lock even if their
   checkouts differ. So same-repo parallelism is bounded by **distinct runtime
   sessions** — give the work distinct sessions (or distinct agents) to spread it
   across the pool. This applies to resumable runtimes (Codex `thread_id`, Claude,
   Kimi); the checkout lock is runtime-agnostic, the session lock is not.

The practical recipe for N-wide same-repo work today is therefore: **N
delegation/orchestra legs** (each engine-isolated into its own worktree) running
under **distinct runtime sessions**, with `--parallel N`.

## Reconfigure without restarting (SIGHUP)

Changing workers/scheduler/poll/idle cadence no longer needs a daemon restart (#577):
`kill -HUP <daemon-pid>` re-reads the `[daemon]` config section (`poll`,
`workers`, `scheduler`, parallelism, `idle_grace_ticks`, and
`idle_max_multiplier`) live — no teardown, no dropped jobs, no
environment re-inheritance (so the daemon's runtime auth is untouched). Values
pinned by explicit launch flags win over the re-read config. When a full
restart is genuinely needed, prefer `gitmoot daemon restart`, which recovers
the persisted Claude token (#578).

## Cap one repo's parallelism from config

To cap a single repo on a shared daemon — without touching the global worker
count or relaunching anything — add a `[repos."owner/repo"]` section (#576):

```toml
[repos."owner/repo"]
max_parallel = 1          # cap this repo's in-flight jobs; 0/unset = global default
# scheduler = "barrier"   # optional per-repo scheduler override
```

`max_parallel = 0` (or an absent section) means "use the global default"; a
positive value caps that repo's concurrent jobs. The keys are re-read every
tick, so edits apply live.

## Host-wide admission budget

On a memory-constrained host, the opt-in `[admission]` section adds a second,
host-global gate the daemon applies **before** starting each agent session, on
top of `--workers`/pool and the per-repo locks (#365):

```toml
[admission]
max_concurrent_sessions = 0   # cap total in-flight sessions; 0 = off
max_memory_gb = 0             # cap summed per-runtime RAM estimate; 0 = off
# codex_memory_gb = 0.2       # operator-tunable per-runtime RAM priors
# claude_memory_gb = 0.85
# kimi_memory_gb = 0.5
# default_memory_gb = 0.5
```

With both caps `0` (the default) the budget is disabled and scheduling is
byte-identical to a config without the section. A job that does not fit BOTH
caps is left **queued** and retried next tick — never failed — so "jobs stay
queued for no visible reason" on a small host can mean the admission budget is
holding them. The budget is enforced per daemon process (host-global for the
normal single-daemon deployment).

## GitHub rate-limit-aware scheduling

The daemon's polling and every agent's `gh`/API calls share one GitHub account.
GitHub's **secondary** (abuse-detection) rate limit fires on **burstiness and
concurrency**, not total volume, so concurrent bursts can trip it (HTTP 403
"secondary rate limit") and freeze all GitHub ops even while the primary quota is
fine — the only manual workaround being to stop the daemon and wait out the
cooldown (#683).

The opt-in `[github]` section installs a **GitHub call budget + adaptive backoff**
that is **in-process to the daemon** — it covers the `gh`/API calls gitmoot itself
issues from the daemon process (polling, comments, merges, status). It is enforced
per daemon process (host-global for the normal single-daemon deployment), the same
scope as the admission budget above. It does **not** reach into separate foreground
processes (a foreground `gitmoot orchestrate`/`pool`/`review`/`pr comment`) or the
`gh` calls a codex/claude runtime subprocess makes on its own — those run outside the
daemon process and never touch the shared limiter.

```toml
[github]
max_concurrent = 0        # cap in-flight gh calls; 0 = unlimited (default)
min_interval = "0s"       # min spacing between call starts; 0 = off (Go duration or bare seconds)
secondary_backoff = true  # pause all GitHub calls on a secondary/abuse limit (default true)
backoff_base = "60s"      # exponential fallback base when no Retry-After (default 60s)
backoff_max = "5m"        # exponential fallback cap (default 5m)
conditional_requests = true # ETag conditional polling (default true)
calls_per_hour_warn = 0      # daemon-local sliding-hour warning; 0 = off
```

**Safe defaults:** the proactive caps (`max_concurrent`, `min_interval`) default
off, so single-call latency and steady-state throughput are unchanged; only the
reactive `secondary_backoff` is on, and it is invisible on the happy path — it
engages **only after** a `gh` call actually fails with a secondary/abuse limit.
On a hit the limiter pauses **all** GitHub calls process-wide (respecting the
response's `Retry-After`, else the exponential fallback) rather than retry-storming
the abuse detector, which only prolongs the block. Calls are never dropped — they
queue/delay until the window passes. On a busy host, set `max_concurrent` (e.g.
`6`) and/or a small `min_interval` (e.g. `250ms`) to also smooth bursts
proactively. `gitmoot daemon status` shows the configured budget.

The four per-tick repository list reads use an in-memory ETag cache. A `304 Not
Modified` replays the prior raw JSON and does not consume GitHub's REST quota.
After three consecutive successful all-304 ticks, a quiet repo moves to 2x base
cadence and then to 4x; configure the thresholds under `[daemon]`:

```toml
[daemon]
idle_grace_ticks = 3
idle_max_multiplier = 4  # 1 disables idle decay
```

Any response-body miss, poll error, queued repo job, or in-flight repo job resets
the streak and promotes the repo immediately. A repo with an open PR remains at
base cadence because its per-PR comment reads are deliberately non-conditional.
The decayed `NextPoll` gates GitHub calls only: heartbeat, pipeline, and chat
maintenance still wake at the resolved base interval. The local call count is
approximate and covers only this daemon process; foreground commands and
agent-owned `gh` processes are outside it.

## Not yet automatic (follow-ups)

- **Top-level `implement` auto-isolation.** `pool` does *not* auto-isolate plain
  same-repo `implement` jobs into worktrees — only read-only `ask`/`review` jobs.
  Parallelizing independent top-level `implement` jobs via the daemon needs
  implement-eligible auto-isolation (a real branch worktree + branch-lock handling
  + a worktree cap and disposal sweep); that is intentionally deferred.

## See also

- [CLI reference](../reference/cli.md) — `daemon start` / `daemon run` flags.
- [Coordinator recipes](./coordinator-recipes-workflow.md) — orchestra fan-outs
  whose legs are already worktree-isolated.
