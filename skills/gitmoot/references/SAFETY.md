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

Delegation and coordinator-continuation chains are bounded so they cannot recurse
or fan out forever:

- Depth cap `MaxDelegationDepth = 8`: each delegation child and each coordinator
  continuation increments `delegation_depth`. A job at or beyond this depth may
  not delegate further.
- Per-root job budget `MaxDelegationTotalJobs = 64`: the whole delegation tree
  under one root (all children and continuations sharing that root) is capped.
  When a batch would exceed it, the delegations are not dispatched.
- Loop detection: a canonical windowed signature hash over recent delegation
  activity halts repeated or cyclic delegation chains well before the depth cap
  is reached, preventing oscillating A→B→A loops.

When a bound trips, the offending delegations are dropped (not dispatched) and
the parent receives a lifecycle/mailbox event explaining why (for example, the
delegation tree for a root reached the job budget of 64). Work is bounded, not
silently retried forever.

## When To Stop

Stop and report `blocked` when the target repo is unclear, GitHub auth is
missing, the daemon cannot access the repo, branch lock ownership is wrong, or
continuing would require credentials or destructive operations the user did not
approve.
