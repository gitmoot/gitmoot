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

## SkillOpt Review Operations

Symptoms:

- `gitmoot skillopt train continue` refuses to publish or sync GitHub review
  feedback.
- Review issue links show `pending deployment`, `failed deployment`, or
  `stale deployment`.
- A candidate review keeps waiting for a promote/reject decision.
- Required Vue/Vite review items fail during generation.

Checks:

```sh
gh auth status --hostname github.com
gh repo view owner/reviews --json nameWithOwner
gitmoot skillopt train status --session <session-id> --verbose
gitmoot repo list
```

Fixes:

- GitHub review operations use `gh`; authenticate it for the expected review
  repo before publishing, syncing, candidate review publication, or review
  watching. Preview publication can push Pages files before a later review issue
  preflight fails, so run the `gh` checks before starting review publication.
- Confirm `review.expected_repo` in train status. Preview review runs must
  publish and sync against the preview/review repo, not the target product repo.
- `pending deployment` means GitHub Pages had not finished for the pushed
  preview commit during Gitmoot's bounded wait. The stored review label is not
  refreshed automatically after it is written; inspect the link or the Pages
  build directly.
- `failed deployment` includes the Pages error when GitHub reports one. Fix the
  preview repo Pages configuration or generated output. Existing review links
  keep their recorded label; generate a new review item or clear/recreate the
  affected preview metadata if reviewers need an updated label.
- `stale deployment` means the latest Pages build still points at a different
  commit after the wait. Confirm the preview repo push and Pages build manually;
  `train continue` skips options that already have a preview URL, so it does not
  re-observe status for the old review option.
- Candidate review decisions are explicit: promote, reject with a reason, wait,
  or reject and `--start-next` to keep improving.
- Required Vue/Vite options retry once when preview-bundle validation fails with
  an actionable error. If the retry also fails, inspect the structured error for
  the item id, option label, validation class, and retry count.

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

## Read-Only Or Permission-Blocked Workers

Symptoms:

- An implementation job is blocked before the agent starts.
- A job comment says the worker is read-only or cannot make changes.
- Runtime output asks for permission or reports that writes are blocked.

Checks:

```sh
gitmoot agent list
gitmoot agent show <agent>
gitmoot job show <job-id>
gitmoot job events <job-id>
```

Fixes:

- If read-only was intentional, do not rerun the implementation job with that
  worker. Restart the agent in write mode or subscribe a writable worker, then
  rerun the task.
- For Codex agents, use an autonomy policy that permits writes for implementation
  jobs. For Claude Code agents, use a permission mode that accepts edits for
  implementation jobs.
- Review and ask jobs can still run with read-only workers when they do not need
  to modify files.

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

## Parallel Implementation And Worktrees

Symptoms:

- Parallel tasks contend on one checkout.
- A job reports that the checkout is already being mutated.
- Two jobs using different branches still block each other because they share one
  registered checkout.

Checks:

```sh
gitmoot task list --repo owner/repo
gitmoot job list --repo owner/repo
gitmoot job events <job-id>
gitmoot lock list --repo owner/repo
```

Fixes:

- Use task worktrees for parallel implementation. Gitmoot stores each task
  worktree path on the task and routes task-tied jobs there.
- Keep the registered checkout clean. Gitmoot still uses it for base branch
  updates and merge-gate cleanup.
- Use separate runtime sessions or managed background instances for jobs that
  should truly run concurrently. Worktrees isolate files; runtime session locks
  still serialize reuse of the same Codex or Claude session.
- Temporary forkable workers, such as the future #177 flow, should remain gated
  on task worktree isolation. Forking sessions without checkout isolation only
  moves the contention from runtime memory to local git state.
- For the full Claude implementation-worker smoke checklist, see
  [Claude Runtime Validation](claude-runtime-validation.md).

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
- If a merged task reports a worktree cleanup warning, inspect the stored task
  worktree path, clean or remove that worktree manually, then clear stale local
  state only after confirming the path is no longer needed.
