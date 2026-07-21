# Troubleshooting

Use `gitmoot doctor --repo .` first. It checks local prerequisites from the
repository checkout.

## The Daemon Is Running An Old Build

Symptoms:

- You upgraded gitmoot (or rebuilt it) and the fix did not take effect.
- Jobs still behave like the previous version; new flags are ignored.
- `gitmoot version` shows the new build, so everything *looks* current.

Cause: the daemon is a long-lived process. Replacing the binary on disk does not
change the code an already-running daemon executes — it keeps running the build
it started from until it is restarted. `gitmoot version` reports the binary
**you** invoked, not the one the daemon is running, so it cannot tell you this.

Check:

```sh
gitmoot daemon status   # prints "build: <version>" and warns on skew
gitmoot doctor          # non-fatal "build" check, same comparison
```

A skew reads:

```
WARNING: daemon running dev-cd43a49 (cd43a495); /usr/local/bin/gitmoot is dev-56ba1c7 (56ba1c74) — restart the daemon to pick it up
```

Fix: restart the daemon (when it is idle — confirm no running jobs first). If it
runs under systemd, use your service manager rather than `gitmoot daemon restart`.

Notes:

- The comparison is between the daemon **process** and the binary at the
  **daemon's own path** — the one a restart would load. It is not about the binary
  you happen to be invoking, which may be a different one entirely.
- The comparison is only made when both builds are identifiable, and an unknown is
  never reported as skew *or* as agreement — it reports "comparison skipped". A
  daemon started by an older gitmoot recorded no build; and two unstamped builds
  with no VCS commit are both just `dev`, so comparing them would prove nothing.
- If you run the web dashboard as a **separate service**, it is its own process
  with its own build. `/api/health` reports the serving process's build separately
  from the daemon's, so restart both after an upgrade. The daemon block includes
  `versionSource: "recorded"` when its version came from daemon startup metadata.
  A daemon started by a pre-build-recording gitmoot instead reports
  `versionSource: "unknown"` with an empty `version`; unknown is never a skew or
  healthy-agreement verdict and never falls back to the binary now on disk.

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

## Reporting A Gitmoot Failure

Symptoms:

- A job is failed, blocked, or cancelled and the user wants to send the details
  upstream.
- The dashboard shows `B report bug` for the selected job.
- An agent needs to file a report without copying raw runtime logs into chat.

Checks:

```sh
gitmoot job show <job-id>
gitmoot report bug --job <job-id> --preview
```

Fixes:

- Preview first. The report builder redacts secrets, omits raw runtime output by
  default, includes recent job events and selected error context, and adds the
  `gitmoot-dashboard-report` / `bug` labels.
- Create the issue only when the user explicitly asks or the active workflow
  policy allows it:

  ```sh
  gitmoot report bug --job <job-id> --create --yes
  ```

- Report the printed GitHub issue URL back to the user. If Gitmoot prints
  `existing issue: ...`, use that URL instead of creating or describing a new
  issue.
- In the dashboard TUI, press `B` on a failed, blocked, or cancelled job to open
  the same redacted preview. Press `g` from that preview to create or reuse the
  issue; errors stay inline so the preview is not lost.

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
- `agent start`/`subscribe` refuses an `implement` agent whose policy is
  `auto`/empty or `read-only` (these grant no deterministic headless write).

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
  implementation jobs. The default `auto` policy (and an unset policy) grants no
  deterministic headless write, so it is refused for `implement` just like
  `read-only`; set `--policy danger-full-access` for full implementation
  including `go`/`git`/`gh`, or `--policy workspace-write` for edits-only (note
  `acceptEdits` does not unblock Bash).
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

What Gitmoot already retries for you (no action needed when the events show a
failure followed by a success):

- A dead pinned session — the `--resume` target no longer exists — is retried
  on a fresh session and the agent is re-pinned to it (#443).
- A transient 401 ("socket connection closed unexpectedly") under sustained
  concurrency is retried with backoff without abandoning the session
  (#487/#509).

Claude auth is read from owner-only (0600) `runtime-auth.env` for every adapter
build. Rotate it with `gitmoot auth set claude`; no daemon restart is needed.
Use `gitmoot auth status` for masked source details and `gitmoot auth probe
claude` (or `gitmoot doctor`) for a live check. Clear it with `gitmoot auth
unset claude`, not by deleting the file.

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

Note that `--repo owner/repo` **scopes** the daemon to a single repo: it polls
only that repo's PRs and claims only that repo's queued jobs. Omit `--repo` to
supervise every enabled registered repo from one daemon (#581).

## Daemon Already Running

Symptoms:

- `gitmoot daemon start`/`run` refuses with `daemon already running with pid …`.

Fixes:

- One daemon per Gitmoot home is enforced with a pidfile plus a flock backstop
  (#550/#556); a second daemon is refused by design, and a stale pidfile whose
  owner is dead is liveness-checked and recovered automatically. Use the
  running daemon — it supervises all subscribed repos. To change its settings,
  send `kill -HUP <pid>` for a live `[daemon]` config reload (#577) or use
  `gitmoot daemon restart`. Scripts should treat the refusal as success.

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
- Increasing `--workers` does not start a temp worker for the job.

Checks:

```sh
gitmoot job show <job-id>
gitmoot job events <job-id>
gitmoot daemon status
gitmoot agent list
```

Fixes:

- Check `[parallel_sessions]`. The default is `same_session = "fork_temp_session"`,
  `merge_back = "summary"`, `max_temp_sessions_per_agent = 4`, and
  `eligible_actions = ["ask", "review", "implement"]`.
- If the job still waits, inspect the job events for the ineligibility reason:
  unsupported runtime, action not eligible, temp-worker cap reached, read-only
  implementation agent, missing task worktree, or summary merge-back waiting for
  the original session.
- Wait for the active job using the same runtime session to finish, or use a
  different registered agent or managed background instance when the work is
  independent.
- Use `gitmoot agent gc` to remove expired managed background instances.
- For a registered agent whose session is genuinely dead or stranded, rebind it
  in place with `gitmoot agent restart <agent>` (refused while the session is
  live or the agent has in-flight jobs).

## Stuck Or Deferred Jobs

Symptoms:

- A queued/blocked job is not moving, or a job "failed then reappeared as
  queued".
- A job sits in `running` long after its worker died.

Checks — read the stuck reason first:

```sh
gitmoot job list --repo owner/repo   # WHY: column on queued/blocked jobs
gitmoot job show <job-id>            # why_stuck: / next_retry_at: lines
gitmoot job events <job-id>
```

Fixes:

- `gitmoot job list` appends a `WHY:` column and `gitmoot job show` prints a
  `why_stuck:` line for queued/blocked jobs (#552) — a runtime-session lock
  wait (naming the holder), `blocked: awaiting human`, `auth failing: …`,
  `throttled: …`, `retrying: …`, or a `blocked-operational: <class>` deferral
  with the attempt schedule. A deferral that needs a human (dirty/wrong-head
  checkout) also prints a `suggested_action` naming the fix.
- Deferred jobs recover on their own (#532): a delivery failure classified as a
  retryable operational blocker — `runtime_auth`, `runtime_quota`,
  `network_outage`, or `checkout_contention` — is re-queued with a bounded
  retry budget instead of failing terminally. `job show --json` carries the
  `blocker_class`, attempt count, and `suggested_action`. A `runtime_auth`
  deferral only re-dispatches once a live doctor-style credential probe passes
  (a failing probe extends the hold without spending a retry). Over `[events]`
  the deferral is a first-class `job.deferred` emitted instead of `job.failed`.
  Only act when the retry budget is spent and the job stays failed.
- A job stuck in `running` is recovered automatically once it shows no lease
  progress past the staleness window (default 30m; tune with the
  `GITMOOT_STALE_RUNNING_AFTER` environment variable; the smallest honored value
  is 1m — below-1m, malformed, or non-positive values are rejected in favor of
  the 30m default rather than clamped, #560). This window is a same-boot crash
  backstop, not a timeout: a job holding a runtime session lock whose lease has
  not elapsed is left running regardless of the window (its real timeout has not
  passed). After a **reboot** you do not wait it out at all — the kernel boot id
  changes, so on its next startup and every tick the daemon immediately requeues
  every job claimed on the previous boot and reclaims its stranded runtime session
  lock, regardless of any unexpired lease (#651). Boot-aware recovery is Linux
  only; elsewhere recovery falls back to the lease/age window above.
- A backlog of `blocked` jobs (each paused awaiting a human) never clears on its
  own. Dismiss one with `gitmoot job cancel <job-id>` (cancel now abandons a
  `blocked` job as well as a `queued`/`running` one; #631), or clear a stale
  batch with the bulk form:

  ```sh
  gitmoot job cancel --state blocked --older-than 7d   # dry-run preview
  gitmoot job cancel --state blocked --older-than 7d --yes
  ```

  The bulk form is a dry-run by default (it prints id/agent/repo/age and cancels
  nothing) until you pass `--yes`; narrow it with `--older-than` (a Go duration
  like `168h`, or a `<N>d` days suffix), `--repo owner/repo`, and `--agent name`.
  `gitmoot doctor` warns when blocked jobs older than 30d have piled up and prints
  the exact command. A dismissed job is not lost — `gitmoot job retry` accepts a
  cancelled job and resurrects it.
- To sweep the backlog automatically, set `[orchestrate].blocked_ttl` to a
  positive Go duration (e.g. `blocked_ttl = "168h"`): the daemon then dismisses
  any blocked job idle longer than the TTL through the same cancel path, recording
  a `blocked_ttl_expired` job event. It is **off by default** (empty or `0s`
  disables it; a negative value is rejected), because a blocked job is a
  human-awaiting decision that is never auto-discarded unless you opt in. This is
  the single-job counterpart of `[orchestrate].escalation_ttl`, which
  auto-finalizes a whole paused delegation tree and is on by default (24h).

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
- If a removed task worktree was previously cached as the registered checkout,
  run `gitmoot repo doctor owner/repo`. Gitmoot verifies the recorded primary
  checkout and repairs the registration before the next job resolves its base.
- Use separate runtime sessions, managed background instances, or forkable temp
  workers for jobs that should truly run concurrently. Worktrees isolate files;
  temp workers isolate busy Codex/Claude runtime sessions when eligible.
- Forked implementation sessions remain gated on task worktree isolation.
  Forking sessions without checkout isolation only moves the contention from
  runtime memory to local git state.
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
    "delegations": []
  }
}
```

Fixes:

- Return exactly one JSON object.
- Use one of the supported decisions: `approved`, `changes_requested`,
  `blocked`, `implemented`, or `failed`.
- Keep `summary` non-empty.

Gitmoot already retries this for you: output missing the `gitmoot_result`
envelope records a `malformed_output` event and is re-asked with the repair
prompt a bounded number of times before failing terminally (#495). A job whose
events show `malformed_output` followed by a success worked as designed.

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
- If the reason says an active job is in flight on the PR branch, let that queued
  or running job settle (or cancel it deliberately). This is a transient safety
  deferral: the task stays `ready_to_merge` rather than becoming blocked, and the
  daemon re-evaluates on its next tick instead of squash-merging and deleting a
  branch beneath an `ask`, `review`, or `implement` job.
- If the PR branch is merely behind or diverged from base, keep the daemon
  running. Gitmoot serializes the base-branch merge gate, asks GitHub to update
  the PR branch safely, then retries on a later daemon poll tick. The default
  poll interval is `30s` unless `--poll` was configured differently.
- If GitHub reports a branch update conflict, Gitmoot stops retrying, posts a
  PR comment, marks `gitmoot/merge-gate` as failed, records `advance_blocked`
  when the block came from job advancement, and shows the reason in
  `gitmoot task list` / `gitmoot job events <job-id>`. Resolve the conflict
  manually or run an explicit implement/fix job, then rerun review/merge.
- Fix failing external CI or Gitmoot statuses.
- If the reason reads `waiting to confirm no external CI` (or `waiting … for CI
  to be created`), the gate saw **zero** external commit-statuses and check-runs
  at the head and is deferring rather than merging before GitHub Actions creates
  its run (#596). A genuinely CI-less repo merges on the next tick after
  `[merge_gate] min_ci_wait` (default `60s`) has elapsed with the head unchanged.
  A CI-configured repo (has `.github/workflows/`) stays pending until the real
  check appears, but only up to `[merge_gate] max_ci_wait` (default `10m`) — past
  that bound, with the head unchanged and still no check, the gate concludes no-CI
  and merges anyway, so a PR whose workflows never trigger for it (docs-only under
  paths filters, tag-only / `workflow_dispatch`-only workflows, a non-targeted
  branch) is not wedged forever. Set `[merge_gate] require_external_ci = true`
  (global or per-repo `[repos."owner/repo".merge_gate]`) to hard-block an empty
  gate instead of stamping `gitmoot/ci` once that window elapses.
- Rerun reviews after the PR head SHA changes.
- If a merged task reports a worktree cleanup warning, inspect the stored task
  worktree path, clean or remove that worktree manually, then clear stale local
  state only after confirming the path is no longer needed.
- When an **external** system owns the merge decision, set
  `GITMOOT_DISABLE_NATIVE_MERGE_GATE=1` (also `true`/`yes`/`on`; #545): Gitmoot
  then abstains from its native merge gate — fail-closed, it never merges
  gatelessly; the external gate makes the call.
