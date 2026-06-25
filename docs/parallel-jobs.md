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
  not counted, because they serialize regardless of the scheduler.

## What actually runs in parallel (two serialization layers)

Parallelism under `pool` is bounded by **two** independent locks:

1. **Checkout lock (per worktree).** Jobs run concurrently only when they have
   distinct checkout keys. Delegation / orchestra `implement` children already get
   their own worktree from the workflow engine, so they parallelize; read-only
   `ask`/`review` jobs can be auto-isolated into an ephemeral detached worktree. A
   plain, top-level same-repo `implement` job with no worktree still shares the
   `repo:<repo>` key and serializes even under `pool`.
2. **Runtime session lock (`runtime:<runtime>:<ref>`).** Two jobs that use the
   **same** agent/runtime session serialize on the session lock even when their
   checkouts differ. So same-repo parallelism is bounded by **distinct runtime
   sessions** — spread the work across distinct sessions (or distinct agents). This
   applies to resumable runtimes (Codex `thread_id`, Claude, Kimi); the checkout
   lock is runtime-agnostic, the session lock is not.

The practical recipe for N-wide same-repo work today: **N delegation/orchestra
legs** (each engine-isolated into its own worktree) under **distinct runtime
sessions**, with `--parallel N`.

## Not yet automatic (follow-ups)

- **Top-level `implement` auto-isolation.** `pool` does not auto-isolate plain
  same-repo `implement` jobs into worktrees — only read-only `ask`/`review` jobs.
  Parallelizing independent top-level `implement` jobs via the daemon needs
  implement-eligible auto-isolation (a real branch worktree + branch-lock handling
  + a worktree cap and disposal sweep); intentionally deferred.
- **`daemon reconfigure`.** Changing workers/scheduler today still needs a restart;
  a drain-not-drop reconfigure that preserves daemon auth is a planned follow-up.

See [`local-workflow.md`](./local-workflow.md) for the broader daemon model.
