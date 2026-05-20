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
```

Current V1 does not expose a lock-management CLI. Inspect local state only if
you understand the schema and have backed up the database. The safer path is to
finish or merge the owning task so the merge gate releases the lock.

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
