# Troubleshooting

Start with the local doctor from the repository checkout:

```sh
gitmoot doctor --repo .
gitmoot status --repo owner/repo
gitmoot daemon status
```

Most Gitmoot failures come from one of four places: the installed binary, GitHub
CLI auth, runtime/plugin discovery, or a local daemon/job/lock state.

## Local Review Lead Is Refused

Symptom: `gitmoot agent review` or a review-resolved `gitmoot agent run` refuses
before creating a job because the lead is missing, cannot access the repo,
lacks `implement`, or has a non-write autonomy policy.

Likely cause: a review-only agent was dispatched without `--lead`, or the named
implementer is not dispatchable from its agents-database row.

Fix: keep the reviewer read-only and name a separate registered implementer:

```sh
gitmoot agent review reviewer --repo owner/repo --pr 12 --lead implementer "Review this PR."
gitmoot agent show implementer
gitmoot agent repos implementer
```

The implementer must be allowed on the repo, hold `implement`, and use
`workspace-write` or `danger-full-access`. Do not grant implementation access to
the reviewer merely to bypass the preflight.

## Install Script Failed

Symptom: `curl -fsSL https://gitmoot.io/install.sh | sh` exits before
installing `gitmoot`.

Likely cause: network failure, unsupported platform, missing shell tools, or a
release artifact that is not available for the current OS/architecture.

Check:

```sh
uname -s
uname -m
curl -fsSL https://gitmoot.io/install.sh -o /tmp/gitmoot-install.sh
sh -n /tmp/gitmoot-install.sh
```

Fix: retry the installer or use the direct binary fallback from the install
page. Verify the artifact checksum before running it.

## Binary Not On PATH

Symptom: `gitmoot: command not found` after install.

Likely cause: the install directory is not on `PATH`, or the shell has not been
restarted after `pipx ensurepath` or installer profile changes.

Check:

```sh
command -v gitmoot
echo "$PATH"
ls -l ~/.local/bin/gitmoot
```

Fix: add the install directory to `PATH`, restart the shell, or move the binary
to a directory already on `PATH`.

## Checksum Mismatch

Symptom: the local SHA256 does not match the release checksum.

Likely cause: partial download, wrong artifact, stale checksum, or a tampered
download.

Check:

```sh
sha256sum <artifact>
shasum -a 256 <artifact>
```

Fix: delete the file, download the artifact again from GitHub Releases, and
compare against the checksum for that exact release and platform.

## GitHub CLI Auth Fails

Symptom: PR comments, issue comments, review publication, status checks, or
merge actions fail.

Likely cause: `gh` is not installed, is authenticated as the wrong account, or
does not have access to the repo.

Check:

```sh
gh auth status
gh repo view owner/repo --json nameWithOwner
gh pr list --repo owner/repo --state open
```

Fix: authenticate `gh` for the account that can read and write the repository,
then retry the Gitmoot operation.

## Send A Gitmoot Bug Report

Symptom: a Gitmoot job failed, blocked, or was cancelled, and you want to send a
useful report without exposing raw runtime output.

Likely cause: the failing job has local context that matters for debugging:
repo, agent, runtime, action, task, selected error, result summary, and recent
events.

Check:

```sh
gitmoot job show <job-id>
gitmoot report bug --job <job-id> --preview
```

Fix: preview first. The report is redacted, omits raw runtime output by default,
adds the `gitmoot-dashboard-report` and `bug` labels, and includes a
fingerprint marker so open duplicates can be reused.

Create the GitHub issue only when you intend to file it:

```sh
gitmoot report bug --job <job-id> --create --yes
```

The command prints either `created issue: ...` or `existing issue: ...`; use that
URL when sharing status. In the interactive dashboard, select a failed, blocked,
or cancelled job and press `B report bug` to open the same preview, then `g` to
create or reuse the issue. If creation fails, the preview stays open and shows
the error inline.

## Plugin Doctor Fails

Symptom: Codex or Claude Code does not discover Gitmoot, or
`gitmoot plugin doctor` reports missing package or runtime state.

Likely cause: plugin package was not generated, runtime CLI is missing, runtime
uses a different home directory, or the package cache is stale.

Check:

```sh
gitmoot plugin doctor
gitmoot plugin doctor codex
gitmoot plugin doctor claude
gitmoot plugin path codex
gitmoot plugin path claude
```

Fix:

```sh
gitmoot plugin install codex --force
gitmoot plugin install claude --force
gitmoot plugin doctor
```

The plugin is discovery and guidance. It does not replace `gitmoot`, GitHub
CLI auth, or runtime/model credentials.

## Runtime Session Not Found

Symptom: `gitmoot agent doctor <name>` cannot validate a Codex, Claude, or Kimi
session, or a job resumes the wrong session.

Likely cause: a `last` reference changed, the runtime home changed, or the
session id is stale. For a Kimi agent, the runtime reference must be a Kimi
session id (`session_<uuid>`) or empty.

Check:

```sh
gitmoot agent list
gitmoot agent show <agent>
gitmoot agent doctor <agent>
codex exec resume --help
claude --help
kimi --help
```

Fix: prefer explicit session UUIDs or thread names over `last`, then
re-subscribe the agent with the correct session reference — or, for a
registered agent whose session is genuinely dead or stranded, rebind it in
place without re-registering:

```sh
gitmoot agent restart <agent>
```

`agent restart` abandons the old runtime session and starts a fresh one for the
same agent; it refuses while the session is live or the agent has in-flight
jobs (finish or cancel those first).

Note that some session failures self-heal without your intervention: a dead
Claude `--resume` target is retried on a fresh session and re-pinned (#443),
and a transient 401 ("socket connection closed unexpectedly") under sustained
concurrency is retried with backoff (#487/#509). A job whose events show one of
these errors followed by a success worked as designed.

## Daemon Not Running

Symptom: queued jobs do not move, PR comments are not consumed, or dashboard
shows the daemon as down.

Likely cause: daemon was never started, exited, or is running with the wrong
repo/home.

Check:

```sh
gitmoot daemon status
gitmoot daemon logs
gitmoot status --repo owner/repo
```

If `daemon status` warns that the advertised log is missing or was last written
before the running daemon started, the path is not receiving current output.
`gitmoot doctor` reports the same confirmed condition as a non-fatal
`daemon log` check. If the daemon runs under `systemd --user`, follow the likely
live stream instead:

```sh
journalctl --user -u gitmoot-daemon -f
```

The warning does not assert how the daemon was launched. It is omitted when the
daemon is stopped, the log is current, or the comparison cannot be made.

Fix:

```sh
gitmoot daemon start --poll 30s --workers 1
```

Use `gitmoot daemon run` only when you intentionally want a foreground process.

`--repo owner/repo` **scopes** the daemon to a single repo: it polls only that
repo's PRs and claims only that repo's queued jobs. Omit `--repo` to supervise
every enabled registered repo from one daemon (#581). If queued jobs for one
repo never move while another repo's jobs do, check whether the daemon was
started with `--repo` scoping it to a different repo, or with a `--session
<root-job-id>` (alias `--root`) filter: a daemon started with `--session` runs
only jobs whose `root_job_id` matches that orchestration run plus the root
coordinator job itself. Restart it without `--repo`/`--session` to drain
unrelated jobs.
Also check the repo is enabled in `gitmoot repo list`.

Claude auth is independent of daemon restarts. Rotate the owner-only
`runtime-auth.env` with `gitmoot auth set claude`; the next delivery observes
it. Inspect masked sources with `gitmoot auth status` and validate them with
`gitmoot auth probe claude` or `gitmoot doctor`.

## Daemon Already Running

Symptom: `gitmoot daemon start`/`run` refuses with `daemon already running with
pid …`.

Likely cause: a daemon is already supervising this Gitmoot home. One daemon per
home is enforced with a pidfile plus a flock backstop (#550/#556); starting a
second one is refused by design (a stale pidfile whose owner is dead is
liveness-checked and recovered automatically, so restarts after a crash work).

Fix: use the running daemon — it supervises all subscribed repos. To change its
settings, send `kill -HUP <pid>` for a live `[daemon]` config reload (#577), or
use `gitmoot daemon restart`. Scripts that start daemons should treat this
refusal as success, not an error.

## Job Stuck Or Failed

Symptom: a job is queued, blocked, failed, or no longer changing state.

Likely cause: runtime delivery failed, worker is read-only, GitHub auth failed,
or another lock is active.

Check — read the stuck reason first:

```sh
gitmoot job list --repo owner/repo   # WHY: column on queued/blocked jobs
gitmoot job show <job-id>            # why_stuck: / next_retry_at: lines
gitmoot job events <job-id>
gitmoot agent show <agent>
gitmoot lock list --repo owner/repo
```

`gitmoot job list` appends a `WHY:` column and `gitmoot job show` prints a
`why_stuck:` line for queued/blocked jobs (#552) — e.g. a runtime-session lock
wait (naming the holder), `blocked: awaiting human`, `auth failing: …`,
`throttled: …`, `retrying: …`, or a `blocked-operational: <class>` deferral with
the attempt schedule. A deferral that needs a human (dirty/wrong-head checkout)
also prints a `suggested_action` naming the fix.

Deferred jobs recover on their own (#532): a delivery failure classified as a
retryable operational blocker — `runtime_auth`, `runtime_quota`,
`network_outage`, or `checkout_contention` — is re-queued with a bounded retry
budget instead of failing terminally. `job show --json` carries the
`blocker_class`, attempt count, and `suggested_action`. A `runtime_auth` deferral
only re-dispatches once a live doctor-style credential probe passes (a failing
probe extends the hold without spending a retry), and over `[events]` the
deferral is a first-class `job.deferred` emitted instead of `job.failed`. A job
that "failed then reappeared as queued" is the deferral working; only act when
the retry budget is spent and the job stays failed.

A read-only seat (every `review`/`ask` job under the read-only autonomy policy)
does NOT authenticate with the ambient credential: it stages a SNAPSHOT of its
runtime config dir (`payload.runtime_config_dir`, else the daemon's
`CLAUDE_CONFIG_DIR`, else `~/.claude`) and carries the resolved
`runtime-auth.env` overlay. When that snapshot is already expired the job records
one `readonly_seat_credential_expired` event naming the expiry, the refresh-token
state and whether an overlay was available; read it with `gitmoot job events
<job-id>` before treating the runtime's own "OAuth session expired and could not
be refreshed" wording as an account problem. It never refuses the job.
`gitmoot doctor` and `gitmoot auth probe claude` both report that seat credential
beside the ambient one, and doctor FAILS (non-zero exit) when it is expired with
no refresh token, since every read-only seat job on that runtime will fail until
the account is re-logged in.

A Claude `produce` stage does NOT run against the operator profile. The daemon
copies `.credentials.json` and `settings.json` from the configured account
(`CLAUDE_CONFIG_DIR`, else `~/.claude`) into a job-private profile, points the
runtime's `CLAUDE_CONFIG_DIR` and `XDG_CACHE_HOME` at that job-private state root
under `<gitmoot home>/cache/produce-runtime/<hash>/run-*/` — one directory per
dispatch, so two runs of one job id cannot wipe each other — and removes it when
the job finishes. The operator profile is never granted writable and is never
named in any read grant, so a token the runtime refreshes mid-job lands in the
discarded copy: re-login on the host, not in the job. It may still be READABLE:
an agent that declares no readable paths falls back to a read-only grant over
`/`, which is a read, never a write.

Only those two files cross into a produce job. Everything else in the operator
profile — `agents/`, `commands/`, `plugins/`, `CLAUDE.md`, `settings.local.json`,
`~/.claude.json` — deliberately does not, so a produce job sees runtime defaults
rather than operator customisation. A profile that does not exist yet is valid
and starts the job with an empty one. A `settings.json` that is symlinked is
followed; one that is empty, holds a non-object, or is not a file at all is
SKIPPED rather than failing the job. An unusable `.credentials.json` does fail
the job, because it decides which account the work runs as.

`sandbox-exec: sandbox read path "…": no such file or directory` means a path
granted to the sandbox does not exist on disk. Explicit read grants are required,
not skipped — check `readable_paths` in `gitmoot job show <job-id> --json` and
create (or stop granting) the named path.

A job stuck in `running` is recovered automatically once it shows no lease
progress past the staleness window (default 30m; tune with the
`GITMOOT_STALE_RUNNING_AFTER` environment variable; the smallest honored value
is 1m — below-1m, malformed, or non-positive values are rejected in favor of the
30m default rather than clamped, #560). The window is a same-boot crash backstop,
not a timeout: a job holding a runtime session lock whose lease has not elapsed is
left running regardless of the window. After a **reboot** there is no wait at
all — the kernel boot id changes, so on its next startup and every tick the daemon
immediately requeues every job claimed on the previous boot and reclaims its
stranded runtime session lock, regardless of any unexpired lease (#651).
Boot-aware recovery is Linux only; elsewhere recovery falls back to the lease/age
window above.

Fix: resolve the underlying runtime/auth/lock issue, then retry when safe:

```sh
gitmoot job retry <job-id>
```

Cancel (abandon) only when the job should not continue. Cancel now dismisses a
`blocked` job — one paused awaiting a human — as well as a `queued`/`running` one
(#631); a dismissed job is not lost, since `gitmoot job retry` accepts a cancelled
job and resurrects it:

```sh
gitmoot job cancel <job-id>
```

A backlog of `blocked` jobs never clears on its own. Clear a stale batch with the
bulk form, which is a **dry-run by default** (it prints id/agent/repo/age and
cancels nothing) until you pass `--yes`:

```sh
gitmoot job cancel --state blocked --older-than 7d          # preview the selection
gitmoot job cancel --state blocked --older-than 7d --yes    # cancel it
```

Only `blocked` is accepted for `--state`; narrow the selection with `--older-than`
(a Go duration like `168h`, or a `<N>d` days suffix), `--repo owner/repo`, and
`--agent name`. `gitmoot doctor` warns when blocked jobs older than 30d have piled
up and prints the exact command to dismiss them.

To sweep the backlog automatically, set `[orchestrate].blocked_ttl` to a positive
Go duration (e.g. `blocked_ttl = "168h"`): the daemon then dismisses any blocked
job idle longer than the TTL through the same cancel path, recording a
`blocked_ttl_expired` job event. It is **off by default** (empty or `0s` disables
it; a negative value is rejected), because a blocked job is a human-awaiting
decision that is never auto-discarded unless you opt in. This is the single-job
counterpart of `[orchestrate].escalation_ttl`, which auto-finalizes a whole paused
delegation tree and is on by default (24h).

## Stale Lock

Symptom: implementation or merge work is blocked by a lock whose owner is gone.

Likely cause: a worker died or the daemon stopped before cleanup.

Check:

```sh
gitmoot lock list --repo owner/repo
gitmoot lock show owner/repo <branch>
```

Fix: let a running daemon reclaim stale resource locks automatically. Task lane
locks are released only after they have been unchanged for at least 24 hours and
no non-terminal task or job references the same repository and branch. Human-
resumable `blocked`, `awaiting_human_merge`, and `awaiting_human` tasks retain
their locks. Release a branch lock manually only after confirming the owner is
no longer working:

```sh
gitmoot lock release owner/repo <branch> --owner <agent>
```

## Merge Gate Deferred By An Active Branch Job

Symptom: a ready PR remains unmerged and the gate reason says an active job is
in flight on its branch.

Likely cause: an `ask`, `review`, or `implement` job targeting that PR branch is
still queued or running. Gitmoot treats this as a transient deferral so it cannot
squash-merge, delete the source branch, and strand an in-progress fix. The task
stays `ready_to_merge` rather than entering a blocked state or emitting a
merge-gate error.

Check:

```sh
gitmoot job list --repo owner/repo
gitmoot job show <job-id>
gitmoot job events <job-id>
```

Fix: let the job settle or cancel it deliberately. The daemon re-evaluates the
unchanged policy merge path on its next tick; do not release its branch lock
while the job is active.

## Gate Status Says A Head Was Never Cleared

Symptom: a pull request carries `gitmoot/merge-gate` = pending with the
description `Gitmoot merge gate has not cleared this head`.

Likely cause: this is the intended signal, not a failure. GitHub's
`mergeStateStatus` reports the absence of blockers, so a head Gitmoot has never
evaluated used to read exactly like one it approved. Gitmoot now marks an active
managed head as unjudged until its own gate produces a verdict.

Check:

```sh
gh pr view <number> --repo owner/repo --json statusCheckRollup,mergeStateStatus
gitmoot task list --repo owner/repo
```

Fix: nothing to do. A later gate evaluation replaces the marker with the
specific pending, failure, or success verdict for the same head. Gitmoot marks
heads only while it owns the merge decision: with `[merge_gate] auto_merge =
false`, with `GITMOOT_DISABLE_NATIVE_MERGE_GATE=1`, or once a task reaches
`awaiting_human_merge`, `dismissed`, `superseded`, `stranded` or `merged`, it
replaces only its own generic marker with `Gitmoot merge gate is not applied to
this head` and leaves any real gate verdict untouched. A `blocked` or
`awaiting_human` task keeps the marker, because that head genuinely has not been
cleared and Gitmoot can still resolve it when the task resumes. A draft pull
request keeps the marker until it is undrafted.

## Dashboard Blank Or Noninteractive

Symptom: `gitmoot dashboard` does not open the TUI, prints plain output, or
looks blank under a script/agent.

Likely cause: stdin/stdout is not a TTY, `TERM=dumb`, or TUI was disabled.

Check:

```sh
gitmoot dashboard --plain
gitmoot dashboard --json
gitmoot dashboard --watch
echo "$TERM"
```

Fix: run from a real terminal for the interactive TUI, or use `--plain` /
`--json` in agents, CI, pipes, and redirected output.

## Live Docs Or LLM Context Stale

Symptom: `gitmoot.io/docs` or `/llms.txt` does not show current source docs.

Likely cause: docs were changed but not rebuilt/deployed, or stale deployed
files were not deleted.

Check:

```sh
cd website
npm run build
curl -fsS https://gitmoot.io/docs/reference/cli | rg 'gitmoot dashboard'
curl -fsS https://gitmoot.io/llms.txt | rg 'Dashboard|Release Notes'
```

Fix: deploy the current static build with delete semantics:

```sh
cd /root/gitmoot/website
npm run build
rsync -a --delete build/ /var/www/gitmoot-docs/
```

## Worktrees consume too much disk

Every five minutes, the daemon checks task-owned worktrees whose task is
`merged`, `dismissed`, `superseded`, or `stranded`. Age alone never qualifies a
task worktree. Gitmoot retains it unless the recorded path is deterministic,
there is no non-final job or branch lock, the `/proc/<pid>/cwd` scan is
conclusive with no live process inside it, the worktree HEAD is reachable from
the recorded task branch, and `git status --porcelain --ignored` reports no
tracked, untracked, or ignored content. Removal is non-force and preserves the
branch.

A malformed payload on any non-final job also pins task worktrees because
Gitmoot cannot prove that the job owns some other path. The bounded retention
log names one malformed job responsible for this global safety pin.

A worktree registered to an older checkout root is removed through the owner in
its `.git` pointer. If that owner cannot inspect or remove it, Gitmoot records
`terminal_worktree_unremovable` once instead of retrying every daemon tick.
Retention decisions log three times per path and classification before
identical messages are suppressed.

`gitmoot doctor` reports delegation worktrees only; ordinary task worktrees
appear in the bounded `terminal task worktree retained` daemon lines instead.
Doctor separates reclaimable final delegation owners, pinned non-final owners,
and unproven directories. `/api/health` exposes the same delegation metric and
quarantined cleanup count in its top-level `worktrees` field.

`[workflow].delegation_worktree_ttl = "72h"` is default-on. After that grace
period the daemon force-removes dirty terminal-owned read-only and delegation
worktrees. Set the TTL to `"0"` to disable this pass. Blocked, queued, and
running owners remain pinned.

No gitmoot path deletes an independent fix clone. It is a standalone object
database, and Linux has no inode-conditional unlink that can make deletion match
a preceding proof. Commit and nested-repository checks can diagnose obvious
retention reasons, but they do not close over every blob, tree, annotated tag,
pack, or concurrent write. Passing them therefore records
`delegation_worktree_retained_unproved`, never a proved-disposable handoff.

Terminal cleanup leaves the managed clone in place. Allocation recovery and
enqueue failures rename it to a `.ttl-reclaiming-orphaned-*` sibling so retries
can allocate without destroying bytes. Cleanup obligations remain open.
Managed-path absence is not treated as removal, even when no sibling is found,
and dangling symlinks remain visible through `lstat`.

`gitmoot doctor` and `/api/health` discover the canonical `fixes/` directory
structurally, including set-asides created before a job row exists. Directory
discovery and logical-size accounting each stop after 4096 entries; the API sets
`truncated=true`, the summary marks counts and bytes as lower bounds, and doctor
warns. Removal remains a manual operator decision after inspecting the working
tree and object database.

The daemon queries at most 256 due pending or aged delegation owners host-wide.
Attempted candidates and selected rows skipped by repository, session,
lifecycle, or checkout-liveness filters persist a later next-attempt time, so
fairness survives restarts rather than depending on an in-memory cursor. The
aged pass also attempts at most eight candidates per repository per tick.
Candidate-local failures skip only that worktree, and later candidates continue.
The five-minute pass cadence advances after every attempt so a failed cleanup
cannot hot-loop. Cleanup obligations retry once per minute and stop in
`quarantined` after the third failure. Inspect them with `gitmoot job cleanup
list --state quarantined` and reopen a repaired target with
`gitmoot job cleanup reopen <resource-id>`.

Repeated terminal-task failures log three times per path before identical
messages are suppressed. The terminal-task pass proves at most eight candidates
per tick, because each proof takes the checkout mutation lock and walks the
ignored tree twice; the window rotates through the candidate list so a
permanently retained worktree cannot starve the ones behind it.

For immediate relief, list candidate directories and prove ownership before
removing anything:

```sh
find "$HOME/.gitmoot/worktrees" -type d -path '*/delegations/*/*' -prune -print
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

The `sqlite3` command is an optional operator aid, not a Gitmoot dependency.
Verify that `job show` still reports `succeeded`, `failed`, or `cancelled` and
the same `payload.worktree_path`. Never remove a worktree for a blocked, queued,
or running job; settle it first.

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

## Isolated worktrees duplicate gigabytes of tool cache

An isolated-worktree job re-materializing its own `uv`/`go`/`npm`/`pip` cache
inside its worktree (rather than reusing a shared one) is the largest driver of
`gitmoot doctor`'s worktree disk figure once a fleet has run for a while — one
worktree can carry several gigabytes of immutable, content-addressed packages
that duplicate what every other job already downloaded.

Gitmoot points `UV_CACHE_DIR`, `PIP_CACHE_DIR`, `npm_config_cache`, `GOCACHE`,
and `GOMODCACHE` at one shared, host-level directory (default
`<home>/cache/tools`) for isolated-worktree jobs, so package caches are reused
across jobs instead of duplicated per worktree. This is on by default; set
`[cache] enabled = false` in `config.toml` to opt out, or `[cache] dir =
"/absolute/path"` to relocate the shared directory (must be absolute).

A read-only/`auto` codex job does not get the redirect — codex never grants it
writable-path access, so pointing its tools at the shared directory would break
rather than help. Those jobs keep their pre-existing (in-worktree) cache
behavior.

If an existing worktree already carries a large in-worktree cache from before
this took effect, it is cleaned up the same way as any other worktree content:
by the delegation-worktree reclaim pass above, or manually once the owning job
is terminal.
