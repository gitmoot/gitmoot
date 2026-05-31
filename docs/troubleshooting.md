# Troubleshooting

Use `gitmoot doctor --repo .` first. It checks local prerequisites from the
repository checkout.

## `gh`

Symptoms:

- `gh auth status` fails.
- PR comments, PR reads, status creation, or merges fail.
- The daemon reports GitHub API or permission errors.

Checks:

```sh
gh auth status
gh repo view owner/repo --json nameWithOwner
gh pr list --repo owner/repo --state open
```

Fixes:

- Authenticate `gh` for the account that can read and write the repository.
- Confirm the `--repo owner/repo` value matches the checkout remote.
- Retry after GitHub rate limits clear.

## Codex

Symptoms:

- `gitmoot agent doctor <name>` cannot validate a Codex agent.
- A job cannot resume the intended session.
- A `last` reference resumes the wrong session.

Checks:

```sh
codex exec resume --help
gitmoot agent list
gitmoot agent doctor <name>
```

Fixes:

- Prefer an explicit Codex session UUID or thread name over `last`.
- Confirm `CODEX_HOME` if sessions are stored outside `~/.codex`.
- Re-subscribe the agent with the correct session reference.

## Agent Templates

Symptoms:

- `gitmoot agent subscribe ... --template thermo-nuclear-code-quality-review`
  fails with an install hint.
- `gitmoot agent start ... --template <custom-id>` fails with an `agent template add`
  hint.
- A custom prompt edit is not reflected in new jobs.
- A template-backed job does not include the expected review instructions.
- You want to know whether the cached template differs from upstream.

Checks:

```sh
gitmoot agent template list
gitmoot agent template show thermo-nuclear-code-quality-review
gitmoot agent template show <custom-id>
gitmoot agent template diff thermo-nuclear-code-quality-review
gitmoot agent template diff <custom-id>
gitmoot agent list
```

Fixes:

- Install or refresh the template explicitly:

  ```sh
  gitmoot agent template update thermo-nuclear-code-quality-review
  ```

  For a custom local template file:

  ```sh
  gitmoot agent template validate agents/<custom-id>.md
  gitmoot agent template add <custom-id> --file agents/<custom-id>.md
  gitmoot agent template update <custom-id>
  ```

- Re-subscribe the agent after the template is installed:

  ```sh
  gitmoot agent subscribe thermo-review \
    --runtime codex \
    --session <session-id-or-last> \
    --repo owner/repo \
    --template thermo-nuclear-code-quality-review
  gitmoot agent doctor thermo-review
  ```

- Template content is snapshotted when a job is queued. Retry an existing job to
  reuse its original snapshot; comment again after `agent template update` to queue a
  job with refreshed content.
- Custom template files are not read at job runtime. Run
  `gitmoot agent template diff <custom-id>` and `gitmoot agent template update <custom-id>`
  after editing the file.
- The thermo template is review-only. Remove `--capability implement` and route
  implementation work to a separate implementation-capable agent.

## Claude Code

Symptoms:

- Claude jobs fail to resume.
- JSON output mode is unsupported by the installed Claude CLI.
- `last` points at an unexpected session.

Checks:

```sh
claude --help
gitmoot agent doctor <name>
```

Fixes:

- Use a Claude session UUID for long workflows.
- Upgrade Claude Code if JSON output mode is needed.
- If JSON mode is unsupported, the adapter falls back to plain output, but the
  output still must contain the `gitmoot_result` object.

## Repo Remotes

Symptoms:

- `gitmoot daemon start` reports that the checkout origin is not the requested
  repo.
- The daemon reads the wrong repository's PRs.

Checks:

```sh
git rev-parse --show-toplevel
git remote get-url origin
gitmoot daemon start --repo owner/repo --poll 30s
```

Fixes:

- Start the daemon from the intended checkout.
- Correct the `origin` remote or pass the matching `--repo`.
- Avoid running one daemon from a parent folder that contains multiple repos.

## Permissions

Symptoms:

- `/gitmoot ...` comments are ignored.
- A commenter cannot route jobs.
- Merge attempts fail.

Checks:

```sh
gh api repos/owner/repo/collaborators/<user>/permission
gh pr view <number> --repo owner/repo --json reviewDecision,mergeable
```

Fixes:

- Comment routing requires write, maintain, or admin permission.
- Merge requires the authenticated `gh` user to have repository merge rights.
- Required reviews and branch protection still apply.

## Stale Locks

Symptoms:

- Implement jobs are rejected because another agent owns the branch lock.
- A branch remains locked after a failed or interrupted run.

Checks:

```sh
gitmoot agent list
gitmoot lock list --repo owner/repo
gitmoot lock show owner/repo <branch>
```

The safest path is still to finish or merge the owning task so the merge gate
releases the lock and records the release event. If the task is abandoned, use
an exact-owner release:

```sh
gitmoot lock release owner/repo <branch> --owner <agent>
```

Use `--force` only when the stored owner is stale or the owning session is no
longer recoverable:

```sh
gitmoot lock release owner/repo <branch> --force
```

## Runtime Session Lock Waits

Symptoms:

- `gitmoot agent ask` fails with `runtime session ... is busy`.
- A background job remains queued and its events include `runtime_lock_wait`.
- Increasing `--workers` does not make two jobs run against the same Codex or
  Claude session.

Checks:

```sh
gitmoot job show <job-id>
gitmoot job events <job-id>
gitmoot daemon status
gitmoot agent list
```

Fixes:

- Wait for the active job using the same runtime session to finish.
- Use a different registered agent or managed background instance when the work
  is independent.
- Keep `gitmoot daemon start --workers 1` unless you have multiple independent
  runtime sessions or an agent type with `max_background` greater than one.
- Use `gitmoot agent gc` to remove expired managed background instances.

## Malformed Agent Output

Symptoms:

- A job fails because output is missing `gitmoot_result`.
- The repair prompt keeps asking for JSON.

Required shape:

```json
{
  "gitmoot_result": {
    "decision": "approved",
    "summary": "ready",
    "findings": [],
    "changes_made": [],
    "tests_run": [],
    "needs": [],
    "next_agents": []
  }
}
```

Fixes:

- Return exactly one JSON object.
- Use one of the supported decisions: `approved`, `changes_requested`,
  `blocked`, `implemented`, or `failed`.
- Keep `summary` non-empty.

## Rate Limits

Symptoms:

- GitHub API calls fail with 429, `retry-after`, or rate-limit messages.
- Polling works briefly and then stalls.

Fixes:

- Increase `--poll`, for example `--poll 60s`.
- Reduce the number of active PRs watched by one daemon.
- Wait for the GitHub rate-limit window to reset.

## Merge Gate

Symptoms:

- The PR remains `ready_to_merge`.
- `gitmoot/merge-gate` is pending or failing.
- The daemon retries a queued merge.

Checks:

```sh
gh pr checks <number> --repo owner/repo
gh pr view <number> --repo owner/repo --json mergeable,statusCheckRollup,reviewDecision
git status --short
```

Fixes:

- Clean the local worktree before the daemon attempts the merge.
- Update the PR branch if it is behind or diverged from base.
- Fix failing external CI or Gitmoot statuses.
- Rerun reviews after the PR head SHA changes.
