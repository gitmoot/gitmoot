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

  Then restart the Gitmoot daemon **from that same shell** so it inherits the
  token. Do not commit or paste the token into issue comments, PR bodies, logs,
  or tracked files.

### Where to persist the token (this trips people up)

`export` only affects the **current shell**. `gitmoot doctor` shows `claude
auth` based on whatever shell you ran it in — and now also reports the **running
daemon's** auth state on Linux (best-effort, read from the daemon process's
environment). The daemon is what actually runs Claude jobs, so the daemon line
is the signal that matters; a `current shell (not the daemon)` warn in some
terminal does not mean the daemon is broken.

To make the token survive new terminals and daemon restarts:

- Persist it in **`~/.bashrc`**, not `~/.profile`. Interactive non-login
  terminals (desktop tabs, most SSH sessions) read `~/.bashrc`; `~/.profile` is
  read **only by login shells**. On Debian / Raspberry Pi OS the default
  `~/.profile` itself sources `~/.bashrc`, so `~/.bashrc` covers both login and
  non-login shells:

  ```sh
  echo 'export CLAUDE_CODE_OAUTH_TOKEN=<token>' >> ~/.bashrc
  ```

  Open a new terminal (or `source ~/.bashrc`), then restart the daemon from it.

- Most robust: run the daemon under **`systemd --user`** with an
  `EnvironmentFile`, so the daemon's auth no longer depends on which shell
  launched it:

  ```ini
  # ~/.config/systemd/user/gitmoot-daemon.service
  [Service]
  EnvironmentFile=%h/.config/gitmoot/daemon.env   # contains CLAUDE_CODE_OAUTH_TOKEN=<token>
  ExecStart=%h/.local/bin/gitmoot daemon run --repo owner/repo
  ```

  Keep the env file readable only by you (`chmod 600`); never commit it.

Check the daemon's view of auth with:

```sh
gitmoot daemon status   # prints the running daemon's claude auth line on Linux
gitmoot doctor          # shows claude auth (daemon) and the shell-local check
```
- `gitmoot plugin doctor claude` stays cheap and environment-only.
- `gitmoot plugin doctor claude --live` is the explicit token-consuming smoke
  check. It should report `runtime-live ok` or a classified auth setup error.

## Scenario Matrix

| Scenario | Required signal |
| --- | --- |
| Claude read-only (or `auto`/default) implement worker | Job is blocked before runtime delivery with a `permission_blocked` event and the standard write-permission message — `auto` grants no deterministic headless write, so it fails closed like `read-only` (#452). |
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
