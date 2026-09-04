# Troubleshooting

Use `gitmoot doctor --repo .` first. It checks local prerequisites from the
repository checkout.

## The Advertised Daemon Log Is Stale

`gitmoot daemon status` prints the configured log path. When the daemon is
running, it warns if that file is missing or its last write predates the
daemon's recorded start time; `gitmoot doctor` reports the same confirmed
condition as a non-fatal `daemon log` check.

This commonly means the daemon runs under `systemd --user`, which captures
stdout and stderr in the journal instead of the file. Follow the likely live
stream with:

```sh
journalctl --user -u gitmoot-daemon -f
```

The warning is mechanism-agnostic: it reports the timestamp mismatch and phrases
systemd as a possibility. It is omitted when the daemon is stopped, the log is
current, or the comparison cannot be made.

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
  local review fix passes to a separate implementation-capable agent with
  `gitmoot agent review thermo-review --repo owner/repo --pr <number> --lead <implementer> "Review this PR."`
- If local review dispatch says the lead is missing, repo-ineligible, lacks
  `implement`, or has a non-write policy, correct the named implementer's agents
  database row or choose another lead. Gitmoot refuses before creating the
  review job; do not loosen the reviewer's capability or sandbox.

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
- A read-only seat (every `review`/`ask` job under the read-only autonomy
  policy) does NOT authenticate with the ambient credential: it stages a
  SNAPSHOT of its runtime config dir (`payload.runtime_config_dir`, else the
  daemon's `CLAUDE_CONFIG_DIR`, else `~/.claude`) and carries the resolved
  `runtime-auth.env` overlay. When that snapshot is already expired the job
  records one `readonly_seat_credential_expired` event naming the expiry, the
  refresh-token state and whether an overlay was available — read it with
  `gitmoot job events <job-id>` before treating the runtime's own "OAuth session
  expired and could not be refreshed" wording as an account problem. It never
  refuses the job. `gitmoot doctor` and `gitmoot auth probe claude` both report
  that seat credential beside the ambient one, and doctor FAILS (non-zero exit)
  when it is expired with no refresh token, since every read-only seat job on
  that runtime will fail until the account is re-logged in.
- A Claude `produce` stage does NOT run against the operator profile. The daemon
  copies `.credentials.json` and `settings.json` from the configured account
  (`CLAUDE_CONFIG_DIR`, else `~/.claude`) into a job-private profile, points the
  runtime's `CLAUDE_CONFIG_DIR` and `XDG_CACHE_HOME` at that job-private state
  root under `<gitmoot home>/cache/produce-runtime/<hash>/run-*/` — one
  directory per dispatch, so two runs of one job id cannot wipe each other — and
  removes it when the job finishes. The operator profile is never granted
  writable and is never named in any read grant, so a token the runtime
  refreshes mid-job lands in the discarded copy: re-login on the host, not in
  the job. It may still be READABLE: an agent that declares no readable paths
  falls back to a read-only grant over `/`, which is a read, never a write.
- Only those two files cross into a produce job. Everything else in the operator
  profile — `agents/`, `commands/`, `plugins/`, `CLAUDE.md`,
  `settings.local.json`, `~/.claude.json` — deliberately does not, so a produce
  job sees runtime defaults rather than operator customisation. A profile that
  does not exist yet is valid and starts the job with an empty one. A
  `settings.json` that is symlinked is followed; one that is empty, holds a
  non-object, or is not a file at all is SKIPPED rather than failing the job. An
  unusable `.credentials.json` does fail the job, because it decides which
  account the work runs as.
- `sandbox-exec: sandbox read path "…": no such file or directory` means a path
  granted to the sandbox does not exist on disk. Explicit read grants are
  required, not skipped — check `readable_paths` in `gitmoot job show <job-id>
  --json` and create (or stop granting) the named path.
- A `stale_worktree_dirty_blocked` task event is not an auto-retrying
  `checkout_contention` deferral. It means the existing task worktree is off the
  resolved base lineage and has uncommitted changes, so Gitmoot preserves it and
  moves the task to `blocked`. Confirm the event with `gitmoot task events
  <task-id> --json`; `gitmoot task list --repo owner/repo --state blocked
  --json` exposes its `worktree_path`. Manually salvage, commit, stash, or clean
  the changes before retrying `task run`/`agent implement`; an off-lineage
  worktree is re-cut automatically only when it is clean. For a
  delegated/Orchestra implement worktree, the same event is stored as a
  JobEvent on the parent coordinator job; the delegation-worktree allocator
  writes nothing to the tasks table. Inspect it with `gitmoot job events
  <parent-job-id>` (`job events` has no `--json` flag). `gitmoot job show
  <parent-job-id> --json` is valid but does not include event history.
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
- `gitmoot/merge-gate` is pending with `Gitmoot merge gate has not cleared
  this head` before the policy gate has produced a more specific verdict.

Checks:

```sh
gh pr checks <number> --repo owner/repo
gh pr view <number> --repo owner/repo --json mergeable,statusCheckRollup,reviewDecision
git status --short
```

Fixes:

- The generic `has not cleared this head` status makes an active managed head
  visibly unjudged, so an unevaluated head stops reading as an approved one. A
  later gate evaluation replaces it with the specific pending, failure, or
  success verdict for that same head.
- Gitmoot publishes it only while it owns the merge decision. With
  `[merge_gate] auto_merge = false`, with `GITMOOT_DISABLE_NATIVE_MERGE_GATE=1`,
  or once a task reaches `awaiting_human_merge`, `dismissed`, `superseded`,
  `stranded` or `merged`, Gitmoot replaces only its own generic marker with
  `Gitmoot merge gate is not applied to this head` and preserves a real gate
  failure or a specific pending verdict.
- A `blocked` or `awaiting_human` task KEEPS the generic marker, because that
  head genuinely has not been cleared and Gitmoot can still resolve it when the
  task resumes.
- A draft pull request keeps the generic marker until it is undrafted, because
  the gate deliberately withholds a verdict on a draft head.
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
- If the task is `awaiting_human_merge`, inspect its reason. Either the mandatory
  exact-head review/CI gate missed (and the daemon journaled its chart-derived org
  escalation) or the repository has the explicit
  `[merge_gate] auto_merge = false` kill-switch. Merge it in
  GitHub or use an authorized `@gitmoot merge` comment. If more implementation
  is required instead, a coordinator can explicitly run `gitmoot task
  resume-work <id> --reason "..." --override-pending-human-decision`; this
  preserves the branch lock and records the override before returning the task
  to `implementing`.
- If the reason reads `waiting to confirm no external CI` (or `waiting … for CI
  to be created`), the gate saw **zero** external commit-statuses and check-runs
  at the head and is deferring rather than merging before GitHub Actions creates
  its run (#596) — this is not an escalation, just wait for the next poll. A
  genuinely CI-less repo qualifies on the next tick after `[merge_gate]
  min_ci_wait` (default `60s`) has elapsed with the head unchanged. A
  CI-configured repo (has `.github/workflows/`) stays pending until the real
  check appears, but only up to `[merge_gate] max_ci_wait` (default `10m`) — past
  that bound, with the head unchanged and still no check, the gate concludes
  no-CI rather than wedging the task forever. Set `[merge_gate]
  require_external_ci = true` (global or per-repo `[repos."owner/repo".merge_gate]`)
  to leave an empty gate open with an escalation instead of stamping `gitmoot/ci`
  once that window elapses.
- If the reason says an external CI check or status is not successful (not
  merely absent), the exact head genuinely failed CI: fix it and push, or merge
  manually / use an authorized `@gitmoot merge` comment.
- Rerun reviews after the PR head SHA changes.
- If a merged task reports a worktree cleanup warning, inspect the stored task
  worktree path, clean or remove that worktree manually, then clear stale local
  state only after confirming the path is no longer needed.
- When an **external** system owns the merge decision, set
  `GITMOOT_DISABLE_NATIVE_MERGE_GATE=1` (also `true`/`yes`/`on`; #545): Gitmoot
  then abstains from its native merge gate — fail-closed, it never merges
  gatelessly; the external gate makes the call.

## Read-Only Review Seat Cannot Run Go Or Read Prior Verdicts

A read-only review seat is Landlock-confined. Two symptoms have the same root
cause - it is reaching for something outside its grants - and neither needs a
wider grant to fix.

### `Permission denied`, exit 126, running Go

NEVER INVOKE AN UNGRANTED TOOLCHAIN PATH FROM A SEAT. A pinned install such as
`/root/.local/toolchains/go1.26.4/bin/go` is not readable there and returns exit
126 whatever `GOTOOLCHAIN` says. Measured: three verdicts on one box in one
hour, and the discriminator was not the flag - the two that failed both invoked
the pinned path and the one that executed the full gate never referenced it.

Use the distro bootstrap and let it fetch the release into the seat's OWN
writable cache:

```sh
export GOTOOLCHAIN=go1.26.4          # an explicit RELEASE name
export CGO_ENABLED=0                 # cgo dies on /usr/include EACCES; -race is unavailable
export TMPDIR=<seat cache>/tmp GOCACHE=<seat cache>/gocache GOMODCACHE=<seat cache>/gomodcache
/usr/lib/go-1.22/bin/go version      # -> go version go1.26.4 linux/amd64
```

- `TMPDIR` must be a concrete path under the seat's own cache root. Both `/tmp`
  and the workspace return EACCES on `mkdir` there, and "a writable dir" reads
  as satisfied by `/tmp` when it is not.
- Never pair `GOTOOLCHAIN=local` with the distro bootstrap: `local` forbids the
  fetch, so you silently get 1.22.2, which cannot satisfy a `go 1.26` directive
  and looks like a broken environment rather than a wrong flag. On a binary that
  is ALREADY the required release, `local` is correct and more hermetic - the
  hazard is the pairing, not the flag.
- Never use a bare `go1.26`: that is the `go.mod` directive, not a released
  toolchain name, and it fails with "toolchain not available".
- Quote `go version` output in any note making a baseline claim. It is the check
  that works without knowing which binary you got.

### Codex reviews report zero executed checks (`bwrap: setting up uid map`)

A codex read-only seat used to fail EVERY command before running it, so its
reviews came back static-only with an executed-check count of zero. Codex's own
sandbox is bwrap, which needs a user namespace that gitmoot's Landlock domain
denies, so two sandboxes were fighting.

Measured on this box, one command with only the domain differing:

```
bwrap --dev-bind / / --unshare-user -- /bin/true
  outside the Landlock domain        -> rc=0
  inside the domain                  -> rc=1  bwrap: setting up uid map: Permission denied
  inside + /proc writable            -> rc=1  bwrap: Failed to make / slave
  inside + /proc + /dev writable     -> rc=1  bwrap: Failed to make / slave
plain /bin/true inside the domain    -> rc=0
```

So the domain executes fine and NESTING was the cause; granting more only moved
the error from the uid map to mount propagation, which is why widening grants
could not settle it.

A codex seat therefore now runs with its own sandbox OFF and relies on the
Landlock domain, which is the boundary gitmoot builds and tests. End to end in
that seat, same grants either way:

- before: `bwrap: setting up uid map: Permission denied`, nothing executed
- after: `/bin/bash -lc 'echo PROBE_RAN' … succeeded in 0ms: PROBE_RAN`
- writing outside the granted roots is still refused - `sh: cannot create
  /root/…: Permission denied`, and no file appears on the host

If a codex seat still reports zero executed checks, the seat's own state
directory is the usual cause: `CODEX_HOME` must be WRITABLE (it is staged
inside the seat's cache root). A read-only `CODEX_HOME` fails earlier and
differently, with `failed to initialize in-process app-server client:
Permission denied`.

### A review reporting it could not read prior verdicts

The seat is given a RENDERED, REPO-SCOPED list of prior verdicts inside its own
cache root, and `GITMOOT_PRIOR_VERDICTS` names the file. It states its own
`as_of` time and scope, so a frozen list cannot be mistaken for a live query.

Rendered rather than a copy of the database, for two measured reasons. Granting
the live store as read-only files does not work at all: SQLite opens the `-wal`
and `-shm` sidecars `O_RDWR|O_CREAT`, which a read-only grant refuses, so the
open fails before any verdict is read - and `journal_mode=wal` persists in the
header, so a quiescent database behaves the same. And a whole-database snapshot
is far too wide: it would hand every repo's jobs, prompts and results, plus
lock and fencing tokens, to a runtime that forwards what it reads to a
third-party model API. The rendered list carries only verdict fields, so
everything else is structurally absent rather than merely unmentioned.

If a seat has no single repo scope, no list is staged and the reason is recorded
in the job's `dropped` diagnostics rather than passing silently.

## Read-Only Reviewer Seat Refuses To Start

A read-only seat runs the runtime against an ISOLATED home rather than yours, so
anything the runtime reads at startup must be staged into that home first. When
a startup input is missing, Gitmoot now fails BY NAME instead of letting the
runtime report something unrelated.

Symptoms:

- `read-only seat requires runtime input "config.toml", and <path> does not
  exist` - the host profile has no such file. This is a HARD PREREQUISITE for
  kimi: `~/.kimi-code/config.toml` must exist on the host, or the seat cannot
  start. Previously this surfaced as "No model configured" behind an auth
  message that pointed at `kimi login`, which cannot fix it.
- `runtime input "<path>" must be a regular file` - the path is a directory,
  socket, device or fifo. A SYMLINK is fine and is followed, so a
  stow/chezmoi-managed profile works; a symlink whose target is missing counts
  as missing, and a symlink to a directory is refused.
- `read-only seat credential <path> is unusable: claudeAiOauth expired at
  <time> and carries no refreshToken` - the profile stages and parses but cannot
  authenticate. Re-authenticate the host claude profile. An expired token WITH a
  refresh token is accepted, because refreshing it is the runtime's job.
- `narrow runtime input "<path>": codex config.toml has a section header that is
  not readable` - the file has a section header that never closes, so Gitmoot
  cannot locate the credentials it must strip. It refuses rather than stage a
  file it cannot classify. Fix the TOML or remove it from the host state dir.

What the seat stages, per runtime:

| runtime | staged from | inputs |
| --- | --- | --- |
| claude | `~/.claude` | `.credentials.json`, narrowed to `claudeAiOauth` and checked for usability. Nothing is staged in gateway mode, where the gateway supplies the credential. |
| codex | `~/.codex` | `auth.json`, plus `config.toml` when present - NARROWED, see below |
| kimi | `~/.kimi-code` | `config.toml` (REQUIRED), `credentials/kimi-code.json` |

### Why the staged codex config.toml is not a copy

The seat's staged state lives inside the one writable path granted to the
sandbox, so a reviewer can read anything placed there. A codex `config.toml`
routinely carries credentials that have nothing to do with running the model, so
Gitmoot narrows it before staging:

- `[mcp_servers.*]` is dropped ENTIRELY. Its `env` table holds tokens, `args`
  can hold them just as easily, and a read-only reviewer seat has no business
  spawning third-party servers. MCP servers are optional to codex, so dropping
  them cannot stop the seat starting.
- `[model_providers.*]` KEEPS its structure and loses only `api_key` and
  `http_headers`. Dropping the section would look stricter and would break a
  custom `model`, because removing `base_url` and `wire_api` makes the provider
  unresolvable. `env_key` and `env_http_headers` name environment variables
  rather than hold values, so they stay.
- Everything else is kept: those are the model and sandbox settings the file is
  staged for.

Dotted and quoted forms are classified the same way, so
`mcp_servers.github.env.TOKEN = "..."` at the top level and
`[mcp_servers."my server".env]` are both dropped.

### The staged kimi config.toml is narrowed too

kimi's `config.toml` is REQUIRED, and it carries more than codex's does:
measured on a live host, `api_key` under `[services.*]` and a key under
`[services.*.oauth]` alongside `[providers.*]`. For kimi the staged copy lands
directly inside the seat's writable root, so it is narrowed on the same rule:

- `[services.*]` is dropped ENTIRELY. It configures optional tooling (moonshot
  search and fetch) and carries those tools' credentials.
- `[providers.*]` keeps its structure and keeps `api_key` and its `env`
  sub-table ONLY for the provider `default_model` resolves to - the model's
  segment before the first `/` matched against the provider name's segment
  after the last `:`, so `default_model = "kimi-code/k3"` selects
  `[providers."managed:kimi-code"]`. Dropping every provider credential is not
  an option: kimi refuses to start when both `api_key` and the `env` sub-table
  are absent, so the seat legitimately needs exactly one.
- If NO provider can be resolved, or two match ambiguously, every provider
  credential is withheld. A credential nobody can prove is needed is not
  staged.

The same rule now applies to codex: `[model_providers.*]` keeps `api_key` and
`http_headers` for the provider `model_provider` selects and strips every
other. Stripping the selected provider's key too left the seat unable to
authenticate at all, because `env_key` names a variable the sandbox's
environment allowlist does not pass through.

### Finding out what was withheld

Narrowing is not silent. When anything is withheld the job records a
`read_only_seat_config_narrowed` event naming it, so a reviewer whose MCP tool
is missing - or a seat that cannot reach a provider - can find out why:

```sh
gitmoot job events <job-id> | grep read_only_seat_config_narrowed
```

### Two more refusals, and what gateway mode withholds

Narrowing reads the file with ONE scanner shared by narrowing and provider
selection, so escapes, comments, multi-line strings and unbalanced brackets are
interpreted identically everywhere. Two shapes are refused rather than staged,
because in both cases part of the file could not be classified, and staging an
unclassified tail is the leak the narrowing exists to prevent:

- `config has an array or inline table that never closes` - an unbalanced `[`
  or `{` reached the end of the file.
- `config has a multi-line string that never closes` - as before.

A triple delimiter inside a COMMENT or inside a single-line string is not a
multi-line string, and a backslash-escaped quote is valid TOML that stages
unchanged.

One provider shape is refused for a different reason:

- `codex model_provider "<name>" authenticates only through env_key, and a
  read-only seat's environment allowlist does not pass that variable through` -
  set an inline `api_key` for that provider, or point `model_provider` at one
  the seat can reach. Previously this staged cleanly and the seat failed later
  with the runtime's own message.

In GATEWAY mode the gateway supplies the credential, so a gateway seat stages
NO credential file for any runtime - claude's `.credentials.json`, codex's
`auth.json` and kimi's `credentials/kimi-code.json` are all withheld. Model
settings are still staged, narrowed as above.

### Comments, and what a seat does when a config cannot be narrowed

Narrowing classifies the COMMENT-FREE part of each line and stages the line
unchanged, so an ordinary TOML comment cannot change what is withheld. Both
directions were defects: `[services] # see [docs]` used to have the comment's
`]` swallowed into the section name, staging that section's secrets verbatim,
and `default_model = "kimi-code/k3" # pinned` used to make the comment part of
the provider name, withholding the one credential the seat needs. Your comments
survive into the staged file.

A key/value pair is split on the first `=` OUTSIDE quotes, so a quoted key
containing `=` is kept rather than dropped.

**A config that cannot be narrowed is treated by whether the runtime needs it:**

- codex's `config.toml` is OPTIONAL. If it cannot be narrowed - an unbalanced
  `[` or `{` at end of file, an unreadable section header, or a selected
  provider that can only authenticate through `env_key` - the seat starts
  WITHOUT it and falls back to the runtime default. The file is not staged and
  the reason is named in the `read_only_seat_config_narrowed` job event.
- kimi's `config.toml` is REQUIRED, so the same refusal fails the seat by name.
  kimi cannot start without it, and a silent fallback would surface later as
  "No model configured".

In GATEWAY mode no runtime stages a credential file, and the policy the seat
computes says so rather than naming files it then withholds.

## Worktrees consume too much disk

The daemon checks task-owned worktrees every five minutes. A task is eligible
only in `merged`, `dismissed`, `superseded`, or `stranded`. Age is not an
eligibility signal. Before removal, Gitmoot re-reads the task under the checkout
mutation lock and requires all of these checks to pass:

- no non-final job names the task or path
- no branch lock owns the task branch
- the `/proc/<pid>/cwd` scan was conclusive and no process has a cwd in the worktree
- the recorded path is the deterministic task path
- `git status --porcelain --ignored` reports no tracked, untracked, or ignored content
- the worktree HEAD is reachable from the recorded task branch, so removing a
  clean detached worktree cannot orphan a local commit

A malformed payload on any non-final job also counts as an active owner because
Gitmoot cannot prove that it refers to another worktree. The bounded retention
log names one malformed job responsible for this global safety pin.

Removal uses `git worktree remove` without `--force` and preserves the branch.
Gitmoot then clears the task's stored path and records
`terminal_worktree_reclaimed`. A live process, unreadable process table, dirty
tree, unreachable HEAD, active owner, or failed proof retains the path.
Retention decisions are logged three times per path and classification before
identical messages are suppressed.

If the worktree belongs to an older checkout root, Gitmoot retries removal
through the owner named by the worktree's `.git` pointer. An owner mismatch that
cannot be inspected or removed is recorded as `terminal_worktree_unremovable`
and excluded from later passes. A later task update makes it eligible again.

`gitmoot doctor` reports delegation-worktree usage as
`N stale worktrees / X GB under <home>/worktrees`. It does not include ordinary
task worktrees; use the bounded `terminal task worktree retained` daemon lines
for those paths. Doctor detail separates aged final delegation owners that are
**reclaimable**, resumable/non-final owners that are **pinned**, and directories
whose owner is **unproven**. Pinned includes `blocked`, `queued`, and `running`;
Gitmoot never force-removes those.

`[workflow].delegation_worktree_ttl = "72h"` is default-on because a final
delegation owner cannot resume, while the grace period preserves short-term
debugging access. Set it to `"0"` to disable the TTL pass. Aged read-only and
delegation worktrees are force-removed even when dirty. An independent fix
clone is different: removing it deletes its standalone object database.

### Fix clones are never deleted automatically

Linux has no inode-conditional unlink that can guarantee a delete removes
exactly the bytes a preceding proof examined. Gitmoot therefore never deletes a
fix clone and never labels one proved disposable. Commit reachability and
nested-repository checks can explain obvious retention cases, but they do not
close over every loose blob, tree, annotated tag, pack, or concurrent write.

| path | behaviour |
| --- | --- |
| terminal cleanup after a job ends | records `delegation_worktree_retained_unproved` and leaves the clone |
| aged TTL pass | records a specific dirty/live/unpublished/nested reason when established; otherwise records `delegation_worktree_retained_unproved` because complete object closure was not proved |
| allocation, interrupted pre-enqueue recovery, enqueue failure | renames the clone to a `.ttl-reclaiming-orphaned-*` sibling so a retry can allocate without destroying data |

Cleanup obligations stay OPEN in every case. Absence of the managed pathname is
not removal evidence: the clone may have been moved aside, and even an empty
sibling scan is not durable operator confirmation. Dangling symlinks remain
visible through `lstat` rather than being mistaken for absent targets.

`gitmoot doctor` and `/api/health` discover the canonical `fixes/` layout
structurally, so a `.ttl-reclaiming-*` clone remains visible even when the crash
happened before any job row was written. Managed clones are counted too. Both
directory discovery and logical-size accounting stop after 4096 filesystem
entries; the JSON `truncated` field and summary then state that counts and bytes
are lower bounds, and doctor warns. Manual cleanup requires inspecting the
directory and deciding independently whether its object database and working
tree can be discarded.

Every retention records **why**, once per reason per job, so an inert deployment
is visible instead of silent: `delegation_worktree_retained_unpublished`,
`delegation_worktree_retained_dirty`, `delegation_worktree_retained_live`,
`delegation_worktree_liveness_unknown`, nested-repository retention, or
`delegation_worktree_retained_unproved`.

The daemon's pending and aged delegation-reclaim queries return at most 256 due
owners host-wide. Attempted candidates and selected rows skipped by repository,
session, lifecycle, or checkout-liveness filters all persist a later
`next_attempt_at`, so later candidates advance across daemon restarts instead of
depending on an in-memory cursor. The aged pass additionally attempts at most
eight candidates per repository per tick.

Terminal-task reclaim failures use the same three-message path limit before
suppressing identical repeats. The pass proves at most eight candidates per
tick — each proof takes the checkout mutation lock and walks the ignored tree
twice — and rotates its window through the candidate list so a permanently
retained worktree cannot starve the ones behind it.

One unreclaimable delegation candidate does not stop the pass. Candidate-local
lookup, runner, and removal failures are skipped while later paths continue.
The daemon logs the first three failures for a path and suppresses repeats.
Each failure advances a durable, restart-safe cleanup obligation. Retries wait
one minute and stop after the third failure in `quarantined`; quarantined paths
are not selected again until an operator inspects `gitmoot job cleanup list
--state quarantined` and runs `gitmoot job cleanup reopen <resource-id>`.
The five-minute worktree cadence advances after every attempted pass, including
passes with candidate failures, so a failed cleanup cannot hot-loop.

For immediate manual relief, list paths and then prove the owner is final. Do
not infer safety from directory age alone:

```sh
find "$HOME/.gitmoot/worktrees" -type d -path '*/delegations/*/*' -prune -print

# sqlite3 is an optional operator aid, not a Gitmoot runtime dependency.
sqlite3 "$HOME/.gitmoot/gitmoot.db" -header -column '
SELECT id, state, updated_at, json_extract(payload, "$.worktree_path") AS path
FROM jobs
WHERE state IN ("succeeded", "failed", "cancelled")
  AND datetime(updated_at) <= datetime("now", "-72 hours")
  AND json_extract(payload, "$.worktree_path") <> ""
  AND (json_extract(payload, "$.delegation_id") <> ""
       OR json_extract(payload, "$.read_only_worktree") = 1);
'

gitmoot job show <job-id> --json
git -C <registered-checkout> worktree remove --force <verified-worktree-path>
git -C <registered-checkout> worktree prune
```

Before removal, verify that `job show` still reports `succeeded`, `failed`, or
`cancelled` and the exact same `payload.worktree_path`. Never manually remove a
worktree owned by a blocked, queued, or running job; settle that job first.
