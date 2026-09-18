# Claude Runtime Validation

Use this checklist when validating Claude Code as a Gitmoot runtime. It is
intentionally operational: it proves the daemon can route Claude jobs through
their own worktrees without storing Claude credentials or raw runtime
transcripts in the repository.

Gitmoot no longer dispatches implementation (#2203), so the scenarios below use
the dispatchable actions — `ask` and `review`. The invariants this checklist
exists for are unchanged by that: credential custody, per-job worktree
isolation, runtime-session locks, and fail-closed permission policy are all
exercised by read-only dispatch. What it can no longer prove end-to-end is the
implement finalizer (commit/push/PR), because nothing enqueues an implement job.

## Preconditions

- `gh auth status` succeeds for the target GitHub account.
- `claude --help` succeeds.
- For Claude background jobs, configure the authoritative non-interactive
  credential:

  ```sh
  claude setup-token
  gitmoot auth set claude
  ```

  The next delivery observes the rotation without a daemon restart. Do not
  commit or paste the token into issue comments, PR bodies, logs, or tracked
  files.

### Where the token lives

Gitmoot stores managed Claude auth in `~/.gitmoot/runtime-auth.env` with mode
`0600`. The file is read for every adapter build, for both foreground and daemon
deliveries. Inspect the selected source and masked fingerprints locally with:

```sh
gitmoot auth status
gitmoot auth probe claude
gitmoot doctor
```

`auth status` is free and masked. The probe and doctor use a fresh one-shot
session. Clear credentials with `gitmoot auth unset claude`, which writes an
explicit-empty file rather than deleting it. A systemd `daemon.env` should keep
operational values such as `PATH` only.
- `gitmoot plugin doctor claude` stays cheap and environment-only.
- `gitmoot plugin doctor claude --live` is the explicit token-consuming smoke
  check. It should report `runtime-live ok` or a classified auth setup error.

## Scenario Matrix

| Scenario | Required signal |
| --- | --- |
| Claude read-only (or `auto`/default) worker carrying `--capability implement` | `agent start`/`agent subscribe` refuses the registration outright — `auto` grants no deterministic headless write, so it fails closed like `read-only` (#452). |
| Claude review worker | Job runs in its own detached read-only worktree at the requested exact head, not in the registered checkout. |
| Mixed Codex + Claude parallel review | Two reviews have two distinct worktrees, daemon runs with `--workers 2`, Codex owns one runtime session, Claude owns another, and both jobs finish without checkout or runtime-session contention. |
| Recorded implementation | `gitmoot job record --type implement` writes a succeeded row the merge gate can attribute; no dispatch, no worktree, no finalizer is involved. |

## Smoke Flow

Create or reuse a disposable repository registered with Gitmoot:

```sh
gitmoot repo add owner/repo --path /path/to/repo
gitmoot task list --repo owner/repo
```

Register or start workers with separate runtime sessions:

```sh
gitmoot agent subscribe codex-reviewer \
  --repo owner/repo \
  --runtime codex \
  --session <codex-session> \
  --role reviewer \
  --capability review \
  --policy read-only

gitmoot agent subscribe claude-reviewer \
  --repo owner/repo \
  --runtime claude \
  --session <claude-session-uuid> \
  --role reviewer \
  --capability review \
  --policy read-only
```

Dispatch one review per worker. Each should allocate its own detached
read-only worktree at the requested exact head and print its path:

```sh
gitmoot agent review codex-reviewer --repo owner/repo --pr <n> --head-sha <40-char-head> --background "Review this PR."
gitmoot agent review claude-reviewer --repo owner/repo --pr <n> --head-sha <40-char-head> --background "Review this PR."
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

- Each review job uses its own detached worktree path at the requested head.
- No job reuses the same `runtime:<runtime>:<runtime_ref>` lock concurrently.
- A registration that pairs `--capability implement` with `read-only`/`auto` is
  refused at `agent start`/`agent subscribe`, before any job exists.

## Related Unit Coverage

The following focused tests cover the non-live invariants:

```sh
GOTOOLCHAIN=go1.26.0 go test ./internal/cli ./internal/runtime ./internal/workflow \
  -run 'Permission|SelectRunnableQueuedJobs|ReviewWorktree' \
  -v -timeout 180s
```
