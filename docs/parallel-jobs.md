# Run Jobs In Parallel On A Repo

By default the daemon runs queued jobs **one at a time per repo**: `--workers`
defaults to `1` and the scheduler defaults to `barrier` (a per-tick batch that
serializes same-repo jobs on one checkout lock). That is the safe, conservative
default — but a fan-out of independent jobs on one repo runs serially unless you
opt in to parallelism.

## The short version

```sh
# Intent-level: workers + the pool scheduler together, named for the goal.
gitmoot daemon start --parallel 5

# Equivalent, explicit form:
gitmoot daemon start --workers 5 --scheduler pool
```

`--parallel N` sets `--workers N` **and** `--scheduler pool` in one flag. It is
sugar for the explicit pair and cannot be combined with `--workers` or
`--scheduler`.

If you raise `--workers` above 1 and do **not** pass `--scheduler`, the daemon
**auto-selects `pool`** — requesting multiple workers under the serializing
`barrier` is almost never what anyone wants:

```sh
gitmoot daemon start --workers 5     # auto-selects --scheduler pool
```

An explicit `--scheduler barrier` is always honored, preserving the old per-tick
semantics:

```sh
gitmoot daemon start --workers 5 --scheduler barrier
```

## Confirm and discover

- `gitmoot daemon status` reports the scheduler mode and worker count (e.g.
  `scheduler: pool, workers: 5`), so you can confirm the daemon is configured for
  the parallelism you asked for without reading launch flags.
- When a parallelizable workload is queued under a serializing config, the daemon
  logs an actionable warning with the exact relaunch command instead of silently
  serializing the work:

  ```text
  warning: 3 parallelizable jobs queued for owner/repo will run serially under the
  current scheduler config; relaunch with: gitmoot daemon restart --parallel 3
  ```

  "Parallelizable" is counted conservatively: same repo, dependency-unblocked, and
  **distinct runtime sessions** (see below). Two jobs on the same agent session are
  not counted, because they serialize regardless of the scheduler. The warning is
  rate-limited — it is re-logged only when the parallelizable set changes, not on
  every poll — so a steady backlog does not spam the daemon log.

## What actually runs in parallel (two serialization layers)

Parallelism under `pool` is bounded by **two** independent locks:

1. **Checkout lock (per worktree).** Jobs run concurrently only when they have
   distinct checkout keys. Delegation / orchestra `implement` children already get
   their own worktree from the workflow engine, so they parallelize; read-only
   `ask`/`review` jobs can be auto-isolated into an ephemeral detached worktree. A
   plain, top-level same-repo `implement` job with no worktree still shares the
   `repo:<repo>` key and serializes even under `pool`.

   **Read-only fan-out worktrees are the committed tip.** An auto-isolated
   read-only worktree is a detached `git worktree add` at the **committed tip** of
   the base branch, so it does **not** contain gitignored paths (e.g. vendored
   clones under `repos/**`) or any uncommitted working-tree changes. This only
   kicks in for **two or more** read-only siblings (the fan-out that would
   otherwise serialize); a **single** read-only delegation stays in the shared
   base checkout and sees everything. Each fan-out child's prompt now carries a
   note with the canonical base-checkout absolute path, so a worker whose sandbox
   can read it (e.g. codex) reaches the real tree instead of reporting a
   working-tree feature as missing. For whole-working-tree analysis that must see
   gitignored or uncommitted state, either run a **single** read-only delegation
   (no fan-out → stays in the base checkout) or pass an **absolute** path to the
   file/dir under analysis.
2. **Runtime session lock (`runtime:<runtime>:<ref>`).** Two jobs that use the
   **same** agent/runtime session serialize on the session lock even when their
   checkouts differ. So same-repo parallelism is bounded by **distinct runtime
   sessions** — spread the work across distinct sessions (or distinct agents). This
   applies to resumable runtimes (Codex `thread_id`, Claude, Kimi); the checkout
   lock is runtime-agnostic, the session lock is not.

The practical recipe for N-wide same-repo work today: **N delegation/orchestra
legs** (each engine-isolated into its own worktree) under **distinct runtime
sessions**, with `--parallel N`.

## Reconfigure without restarting (SIGHUP)

Changing workers/scheduler/poll no longer needs a daemon restart (#577):
`kill -HUP <daemon-pid>` re-reads the `[daemon]` config section (`poll`,
`workers`, `scheduler`, parallelism) live — no teardown, no dropped jobs, no
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

## Not yet automatic (follow-ups)

- **Top-level `implement` auto-isolation.** `pool` does not auto-isolate plain
  same-repo `implement` jobs into worktrees — only read-only `ask`/`review` jobs.
  Parallelizing independent top-level `implement` jobs via the daemon needs
  implement-eligible auto-isolation (a real branch worktree + branch-lock handling
  + a worktree cap and disposal sweep); intentionally deferred.

See [`local-workflow.md`](./local-workflow.md) for the broader daemon model.
