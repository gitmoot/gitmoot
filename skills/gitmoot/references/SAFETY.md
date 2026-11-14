# Gitmoot Safety Reference

What gitmoot enforces today, and what a seat must not work around.

## Scope and ownership

- Pass `--repo owner/repo` whenever more than one repository is in play; a
  repository must be registered (`gitmoot repo list`) before gitmoot acts on it.
- A seat implements on its own branch. A review job only reads: it never
  commits, pushes or opens a pull request.
- Do not merge with `gh pr merge`, `git push` to the base, or an admin override
  to get around a held gate. The gate is the merge authority; see section 3 of
  [SKILL.md](../SKILL.md) for what to do when it holds.
- Do not start or stop daemons, register or remove reviewers, cancel other roles'
  jobs, or release locks unless the user asks.

## Locks

- **Repo checkout locks.** One lock per registered checkout. The daemon polls a
  repository and runs anything that touches its checkout while holding it, so
  two jobs never mutate the same checkout at once. Merges take the same lock.
- **Runtime session locks.** A job on a resumable runtime session locks
  `runtime:<runtime>:<session>` for its run, so two jobs never drive the same
  session at once. omp starts a fresh session per job, so omp jobs do not
  contend for it.
- **Merge serialization.** Merge attempts are serialized per base branch.
- The daemon runs one job at a time by default (`--workers 1`). Raise it only
  for independent jobs (distinct checkouts and sessions).

## Review isolation

- Every review runs in its own throwaway worktree checked out at the exact head
  under review, never in the registered checkout. The worktree is removed when
  the job ends.
- A review seat is read-only: on Linux the runtime runs under a Landlock
  sandbox that allows writes only to the job's scratch space and caches, and a
  separate runtime profile. A review requested with `--no-execution` may run no
  program but the runtime itself.
- The disk guard refuses new dispatches when the gitmoot home's filesystem is
  below its free-space floor (`[disk_guard]`, default 2 GiB and 5%).

## The merge gate

- It merges only with an approval bound to the pull request's current head and
  green CI at that head. An approval of an older head never carries forward.
- It never updates a reviewed branch: if the base has moved in a way that needs
  an update, it holds and asks for an update plus a new review.
- It never counts the implementer's own review. Who implemented comes from the
  implement job a seat records with `gitmoot job record`; record only the agent
  or role that really wrote the change.
- It holds a pull request with a dirty local checkout, a draft, or an open P0 to
  P2 finding at the head.
- `[repos."owner/repo".merge_gate] auto_merge = false` turns automatic merging
  off for a repository; `/gitmoot merge` still runs the gate on request.

## Unavailable roles

An org role can carry an unavailability incident after a provider quota
rejection, until the provider's reset time. `gitmoot org status` and
`gitmoot org chart` show it as `UNAVAILABLE`. An incident holds only dispatches
on the runtime recorded on it. Incidents recorded on a removed runtime (`codex`,
`claude`, `kimi`) are history: they never refuse an omp or shell dispatch. Only
an incident with no recorded runtime, or one whose stored runtime is neither
current nor a removed one (a corrupt row), holds the whole role. An incident
clears at its reset time or on the role's next successful job on that runtime.

## Files and secrets

- Do not commit generated data, caches, logs, secrets, session archives or
  large outputs unless asked.
- Redact secrets from summaries, findings, messages and quoted output.
- Remote (E2B) reviews never receive the host GitHub token; they read the
  repository through a short-lived, repository-scoped lease.

## When to stop

Stop and say so, in `needs` for a review or in a message to your parent role,
when the work needs credentials you do not have, a decision only a human can
make, or a change to a branch or repository you do not own.
