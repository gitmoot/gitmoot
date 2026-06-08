# Claude Runtime Validation

Use this checklist when validating Claude Code as a Gitmoot implementation
worker. It is intentionally operational: it proves the daemon can route Claude
jobs through task worktrees without storing Claude credentials or raw runtime
transcripts in the repository.

## Preconditions

- `gh auth status` succeeds for the target GitHub account.
- `claude --help` succeeds.
- For Claude background jobs, the daemon environment has non-interactive
  credentials. Prefer:

  ```sh
  claude setup-token
  export CLAUDE_CODE_OAUTH_TOKEN=<token>
  ```

  Then restart the Gitmoot daemon so it inherits the token. Do not commit or
  paste the token into issue comments, PR bodies, logs, or tracked files.
- `gitmoot plugin doctor claude` stays cheap and environment-only.
- `gitmoot plugin doctor claude --live` is the explicit token-consuming smoke
  check. It should report `runtime-live ok` or a classified auth setup error.

## Scenario Matrix

| Scenario | Required signal |
| --- | --- |
| Claude read-only implement worker | Job is blocked before runtime delivery with a `permission_blocked` event and the standard write-permission message. |
| Claude workspace-write implement worker | Job runs in `task.worktree_path` and produces the expected marker change there, not in the registered checkout. |
| Mixed Codex + Claude parallel implement | Two tasks have two distinct worktrees, daemon runs with `--workers 2`, Codex owns one runtime session, Claude owns another, and both jobs finish without checkout or runtime-session contention. |
| Local/no-PR implement advancement | Implementation job records `advance_skipped_no_pr`, then `advance_completed`; it should not keep retrying PR advancement when no PR is attached. |

## Smoke Flow

Create or reuse a disposable repository registered with Gitmoot:

```sh
gitmoot repo add owner/repo --path /path/to/repo
gitmoot goal import --file GOAL.md --repo owner/repo
gitmoot task list --repo owner/repo
```

Register or start workers with separate runtime sessions:

```sh
gitmoot agent subscribe codex-worker \
  --repo owner/repo \
  --runtime codex \
  --session <codex-session> \
  --role implementer \
  --capability implement \
  --policy workspace-write

gitmoot agent subscribe claude-worker \
  --repo owner/repo \
  --runtime claude \
  --session <claude-session-uuid> \
  --role implementer \
  --capability implement \
  --policy workspace-write
```

Start one task per worker. `task run` should allocate a dedicated worktree and
print its path:

```sh
gitmoot task run task-001 --repo owner/repo --owner codex-worker
gitmoot task run task-002 --repo owner/repo --owner claude-worker
```

Run the daemon with enough workers for both jobs:

```sh
gitmoot daemon run --repo owner/repo --workers 2
```

Inspect job and task state:

```sh
gitmoot job list --repo owner/repo
gitmoot task list --repo owner/repo
gitmoot job show <job-id>
gitmoot job events <job-id>
```

Expected evidence:

- Each implementation job uses its task worktree path.
- No job reuses the same `runtime:<runtime>:<runtime_ref>` lock concurrently.
- Local jobs without a PR number include `advance_skipped_no_pr` followed by
  `advance_completed`.
- Read-only implement attempts never start Claude or Codex; they stop with the
  standard `permission_blocked` event.

## Related Unit Coverage

The following focused tests cover the non-live invariants:

```sh
GOTOOLCHAIN=go1.26.0 go test ./internal/cli ./internal/runtime ./internal/workflow \
  -run 'Permission|RunTaskRun|SelectRunnableQueuedJobs|AdvanceImplement' \
  -v -timeout 180s
```
