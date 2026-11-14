---
name: gitmoot
description: Use Gitmoot to get an independent review of a seat's pull request at an exact head, read the verdict and findings, pass the merge gate, send and read durable messages between org roles, read the org chart and briefs, escalate questions, and unstick jobs with job show, job cancel, doctor and daemon drain. Use when the user mentions gitmoot, a review request or verdict, findings, the merge gate, org messages or escalations, design-notes issues, or the gitmoot dashboard.
license: Apache-2.0
compatibility: Requires the gitmoot CLI, git, an authenticated GitHub CLI (gh), network access to GitHub, and omp, the only runtime gitmoot runs reviews on.
metadata:
  source: gitmoot/gitmoot
---

# Gitmoot Agent Skill

## What gitmoot is for

Gitmoot coordinates a fleet of agent seats that work on GitHub pull requests.
Seats write the code themselves; gitmoot does five things around that work:

- **Reviews.** A seat asks for an independent review of its pull request at an
  exact commit. Gitmoot picks a reviewer, runs it, stores the verdict and its
  findings, and wakes the seat when the verdict is saved.
- **Durable messages** between org roles: saved first, delivered when the
  recipient can take them.
- **The org chart:** roles, their parents, panes and briefs.
- **The merge gate:** merges a pull request only with an approval bound to its
  current head and green CI.
- **The dashboard:** a read-only view of jobs, roles and messages.

Gitmoot does not implement. It never writes code for you and never dispatches
an implementation job.

## omp is required

Every review job runs on omp. `shell` is a runtime for tests that need no
model: its "session" is a command whose output is the verdict. Like every
review, it runs sandboxed in a read-only checkout of the reviewed head: it can
read that checkout, not other host paths such as `/tmp`, so print the verdict
inline (`echo '...'`). Agents still registered on a removed runtime are
refused; register them again with `--runtime omp`.

`gitmoot plugin install omp` installs the omp add-on that delivers messages
to omp sessions. The `codex` and `claude` plugins
(`gitmoot plugin install codex`, `gitmoot plugin install claude`) give seats
running in those apps this skill and a hook that collects their messages; they
are not job runtimes.

Check the install before anything else: `gitmoot version`, then `gitmoot doctor`
(exit 1 lists `failed required checks:`; on a throwaway home without GitHub,
Herdr or omp, the `omp`, `gh auth` and `repo remote` failures are expected,
plus `herdr` once an org role is declared and `base branch` when run outside
a git checkout). The binary is the authority. If this skill disagrees with
`gitmoot --help` or a command's own `--help`, trust the help.
[references/CLI.md](references/CLI.md) is that help, generated from the binary.

**Home and argument order.** `--home <dir>` keeps state in `<dir>/.gitmoot`
instead of `$HOME/.gitmoot`. It is a flag of each subcommand, never of
`gitmoot` itself: write `gitmoot doctor --home H`, because `--home` before the
subcommand fails with `unknown command "--home"`. `gitmoot version --home H`
prints the `config:` and `database:` paths of that home. Put a positional name
or id right after the subcommand words and flags after it
(`gitmoot job show <job-id> --home H`,
`gitmoot reviewer subscribe <name> --runtime ...`); the other order fails with
"requires exactly one id/name". Two exceptions take every flag, `--home`
included, before the positional:
`message escalate --to <role> "question"` and
`org await withdraw --reason "why" <await-id>`. Every example here uses the
order that works. Role-aware commands read your role from `GITMOOT_ORG_ROLE`;
pass `--role <role>` when it is unset.

## 1. Record your work and request a review

Record the implementation first, so the merge gate knows who wrote the change
and can check that the reviewer is someone else:

```sh
gitmoot job record --agent <seat> --repo owner/repo --type implement --decision implemented --pr <n> --branch <branch> --summary "what changed"
```

Use `--acting-role <role>` instead of `--agent` when the seat that wrote the
change is an org role, not a registered reviewer (every seat created with
`gitmoot org seat add` is a role; `--agent` with a role name fails `agent
"<name>" not found`). The record then prints an empty
`agent:` and keeps the role as the job's acting role, which is what the gate
reads. The record binds to the repository and pull request (and `--task`), not
to a head: one record covers every later head of that pull request, and
`--head-sha` is display only. Add `--task <task-id>` when the pull request
already has a review task (`gitmoot task list --repo owner/repo`). Never record
an agent that did not write the change.

Then request the review at the exact head you pushed:

```sh
gitmoot review request --repo owner/repo --pr <n> --head <40-hex-sha> --role <your-role>
```

- Gitmoot picks a review-capable reviewer and model; use `--reviewer <name>` only
  to name one. `--purpose code|security|ui|architecture` (default `code`)
  requests a different kind of review; each purpose is its own review.
- One review per head and purpose. Asking again for the same head attaches to
  a queued, running or blocked review (`state: attached`), or prints
  `state: verdict_exists` with `verdict: <decision> (already saved; no new
  review spent)`; neither starts a second reviewer. A review that ended
  `failed` or `cancelled` has no verdict, so asking again starts a new review
  of that head: that is how to retry one. To replace a saved verdict at the
  same head, pass `--supersede <job-id> --reason "why"`.
- The output names the run (`run:`), the reviewer, any `hold:` lines, and a
  `watch:` command. Add `--json` for `run_id`, `reviewer`, `awaited_fact_id`
  and `holds`. Every request, `verdict_exists` included, registers a wake-up
  (`notify: ... awaited fact N`) and reports the daemon hold.
- Your role is woken when the verdict is saved; you do not need to poll.
- The review runs when the daemon claims it. Without a daemon the output says
  ``hold: daemon not running: the review stays queued until `gitmoot daemon
  start` starts the daemon``: start it (`gitmoot daemon start`) or run the job
  once in the foreground with `gitmoot job run <job-id>`.

Pass review instructions with `--message "instructions"`. Ask a reviewer a
read-only question with `gitmoot ask <reviewer> "question" --repo owner/repo`.

**Review level.** `gitmoot review level --repo owner/repo --pr <n>` decides how
much review the head needs, posts it as the `gitmoot/review-level` commit
status, and prints `level:`, `head:` and `reason:`:

- `level1_no_review`: merge without review.
- `level2_background`: merge, then request a review of the merged head with
  `--post-merge`; the command prints that request for you.
- `level3_required`: review before merge.

Some paths always need review before merge (agent instructions such as
`AGENTS.md`, `SKILL.md` files under `skills/`, CI workflows, deploy config,
dependency manifests and locks, migrations), and so does every pull request on
`gitmoot/gitmoot`.

## 2. Read the verdict and re-review after a fix

```sh
gitmoot review status --repo owner/repo --pr <n>
gitmoot job show <job-id>
gitmoot findings --repo owner/repo --pr <n>
```

- `review status` prints the gate state and one line per review job: state,
  head, reviewer, `verdict=`, `evidence=` and the findings count. It and
  `job show` are the source of truth for a review.
- `job show` prints `decision:` and `summary:`, and the full result under
  `payload.result`: `decision`, `severity`, `findings`, `tests_run`,
  `evidence`. Failed result checks (a verdict that accounts for nothing) are
  under `payload.result_checks` and in `gitmoot job events <job-id>` as
  `result_checks_failed`. The result fields are in
  [references/RESULT_CONTRACT.md](references/RESULT_CONTRACT.md).
- `findings --repo --pr` lists each finding with its `STATE`; add
  `--at-head <sha>` to list only what the merge gate still demands at that
  head.

A verdict is `approved` or `changes_requested` (with a `severity` of `P0` to
`P3`); `blocked`, `failed` and `cancelled` mean the review did not finish.

- **`failed` or `cancelled`:** request the same head again; that starts a new
  review of it.
- **`blocked`:** requesting the same head again does not retry it. The blocked
  job keeps its claim on the head, so the request prints `state: attached`
  and the same job with `(blocked)`. Read why with `gitmoot job show <job-id>`
  (`why_stuck`) and what it needs with `gitmoot job gates <job-id>`. Then
  either clear its gates (`gitmoot job gates clear <job-id> --need "text"`;
  clearing the last one runs it again), re-queue the same job with
  `gitmoot job retry <job-id>`, or cancel it with `gitmoot job cancel <job-id>`
  and request the head again for a new review.

To act on `changes_requested`: fix, push, and request a review of the **new**
head. A new head is a new review; asking again for the same head only returns
the verdict it already has. A finding closes only when a later review cites
its UID as `continues_uid` with `"state": "answered"` and evidence (see
[references/RESULT_CONTRACT.md](references/RESULT_CONTRACT.md#the-findings-ledger)); an
approval that does not cite it leaves it open.

## 3. Merge rules

The gate merges a pull request only when all of these hold at its current head:

- an approval bound to that exact head, from a reviewer who is not the
  recorded implementer (an approval of an older head does not count);
- no review at that head still running, and none that requested changes,
  blocked or failed;
- no open finding of severity P0, P1 or P2 at that head (P3 never blocks);
- green CI, mergeable, not a draft, and a branch the base has not moved past
  in a way that needs an update (the gate never updates a reviewed branch).

When automatic merge is on, the daemon merges on its own once the gate passes.
`review status` prints whether it is on. To ask for a merge explicitly, comment
`/gitmoot merge` on the pull request. The comment runs the gate even when
automatic merge is off and always replies with the outcome. Your role's
`merge_rule` from the org chart applies to that comment: `self` may merge,
`owner` refuses a role that is not the root, `none` refuses.

**When a merge is held**, the reason names the remedy. The common ones:

- *No implement job recorded* (an attribution gap): run the `job record` from
  section 1, then `/gitmoot merge` again.
- *No approval bound to the current head*: request a review of that head.
- *The repository requires GitHub's merge queue*: gitmoot cannot merge
  through it. Add the pull request to the merge queue on GitHub.
- *The branch is behind its base*: the gate never updates a reviewed branch
  itself. Update it, push, and request a review of the new head.
- *A draft*: held open on purpose until it is marked ready.
- *Low-risk auto-merge left it open* (a repository with
  `auto_merge = "low_risk"` merges by itself only level 1 or 2 heads, and
  never one labelled `hold` or `do-not-merge`): a person merges it on GitHub.

Never merge around a held gate on your own authority.

## 4. Messages

```sh
gitmoot message send <role> "text"
gitmoot message inbox
gitmoot message show <id> --thread
gitmoot message reply <id> "text"
```

Flags go after the positional arguments. A message is saved before it is
delivered, so "sent" and "read" are separate facts. `send` and `show` print the
notification state:

- `queued`: saved, not yet delivered. The recipient may be busy or offline, or
  its pane is unbound. The message stays in its inbox and is delivered when it
  can take it.
- `submitting`, then `submitted`: being delivered, then delivered to the
  recipient's session.
- `uncertain`: delivery may or may not have happened.
- `stalled` or `failed`: delivery stopped trying; the message is still saved
  in the recipient's inbox.

So "delivered" is `submitted`, and "deferred" is a `queued` message whose
reason ends in "notification deferred": delivery is waiting until the recipient
can take it. If a message to a role stays undelivered for more than 15 minutes,
gitmoot sends you, the sender, one notice saying how long it has waited and why.
The message stays queued and is delivered when the recipient can take it. Do
not type it into the recipient's pane yourself.

Read your inbox when you start or resume, and keep message ids in handoffs.

## 5. Org: chart, brief, escalate, awaits

```sh
gitmoot org chart
gitmoot org brief --role <role>
gitmoot message escalate --to <role> "question"
gitmoot org await list
```

- `org chart` shows every role under its parent with its live session state
  (it reads Herdr); `--json` for scripts. `org brief --role <role>` prints that
  role's brief, including how to use messages.
- `message escalate` asks a question of a role above or below you on the
  chart: your parent by default, or the role given with `--to`. Questions to a
  role on another branch are refused. The asker or the addressed role closes it
  with `gitmoot message resolve <id> --answer "text"`.
- Awaits are the wake-ups gitmoot owes you. `review request` creates one for
  you. To wait for a review someone else requested, use
  `gitmoot org await review --repo owner/repo --pr <n> --head <sha> --ttl 12h`.
  End a wait you no longer need with
  `gitmoot org await withdraw --reason "why" <await-id>`.

## 6. When something is stuck

```sh
gitmoot job show <job-id>
gitmoot job cancel <job-id>
gitmoot doctor
gitmoot daemon status
gitmoot daemon drain --timeout 15m
```

- `job show` reports the state and, for a stuck job, `why_stuck` and a
  suggested action. `gitmoot job events <job-id>` lists what happened.
- `job cancel <job-id>` cancels one job.
  `gitmoot job cancel --state blocked --older-than 7d` only previews the old
  blocked jobs it would cancel and changes nothing; add `--yes` to cancel
  them: `gitmoot job cancel --state blocked --older-than 7d --yes`.
- A `blocked` job lists what it needs with `gitmoot job gates <job-id>`. Clear
  one with `gitmoot job gates clear <job-id> --need "text"`; clearing the last
  one runs the job again.
- `doctor` checks the install, the daemon, stuck jobs and the runtimes;
  `--json` for scripts.
- `daemon drain` stops claiming new jobs and waits for the running ones (up to
  `--timeout`), for example before a restart. `gitmoot daemon drain --clear`
  resumes claiming.

Do not start or stop daemons, cancel other roles' jobs, or release locks unless
the user asks for it.

## 7. Design-notes issues

P3 findings never block a merge. When a pull request merges with P3 findings
still open, the daemon files them on that repository's design-notes issue for
the week: one issue per repository per ISO week, titled
`[design-notes] owner/repo 2026-W46` and labelled `design-notes`, with one
comment per merged pull request. When the daemon opens the issue, it sends the
role that requested the review one message with the link ("Advisory; nothing
blocks").

The notes are advisory. Each may already be fixed on the default branch, so
check the current code first. Pick up what is worth doing, and close the issue
once every note is triaged.

## More

- [references/WORKFLOWS.md](references/WORKFLOWS.md): end-to-end walkthroughs,
  including a no-model review on a throwaway home.
- [references/RESULT_CONTRACT.md](references/RESULT_CONTRACT.md): the review
  result a reviewer returns.
- [references/SAFETY.md](references/SAFETY.md): locks, isolation and the gate's
  guarantees.
- [references/CLI.md](references/CLI.md): every command's `--help`.
- The dashboard: `gitmoot dashboard` prints a snapshot,
  `gitmoot dashboard --web --addr 127.0.0.1:8080` serves the read-only web view.
