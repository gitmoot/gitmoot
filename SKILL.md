---
name: gitmoot
description: Use Gitmoot for local-first AI agent coordination across repositories, reviews, GitHub PR comments, daemon jobs, stuck jobs, custom prompt agents, orchestration, the web dashboard, runtime metadata, and Codex, Claude Code, Kimi Code, or omp runtime workflows.
version: 0.1.0
license: Apache-2.0
compatibility: Requires the gitmoot CLI, git, GitHub CLI authentication, network access to GitHub, and omp (the job runtime; shell runs no-LLM tests).
metadata:
  gitmoot-version: "0.8.8"
  source: "gitmoot/gitmoot"
  openclaw:
    requires:
      bins:
        - gitmoot
        - git
        - gh
    envVars:
      - name: GH_TOKEN
        required: false
        description: Optional GitHub token used by gh when GitHub CLI is not already authenticated.
---

# Gitmoot Agent Skill

This root `SKILL.md` is kept as a raw compatibility entrypoint for agents and
`gitmoot.io/SKILL.md`. The canonical Agent Skills package lives at
`skills/gitmoot/`, with deeper reference files under `skills/gitmoot/references/`.

Gitmoot is a local-first coordinator for AI agents working across repositories,
reviews, PR comments, and runtime workflows. Use this skill when the
user wants PR-comment agent workflows, repo-scoped agent subscriptions,
background daemon checks, omp agent startup, the Claude Code, Codex or omp plugins,
structured implementation plans,
custom prompt agents, job status,
agent memory,
or runtime metadata.

Gitmoot's CLI evolves quickly. If the user names a Gitmoot feature or command
that is not covered here, verify the live surface with `gitmoot --help` and the
relevant `gitmoot <command> --help` before answering. If this compatibility
entrypoint disagrees with the live binary, trust the live help and treat the
skill text as stale documentation that should be refreshed.

For "use <agent> here" requests, do the work directly in this current chat and,
when it should be tracked, clock in with `gitmoot job open --agent <agent>
--repo owner/repo --type ask|review|implement` and clock out with `gitmoot job
close <job-id>`. Do not route a "here" request through a background
`gitmoot agent ask` unless the user explicitly asks for background execution,
PR-comment routing, or job tracking by the daemon.

For fast planning, "use the Gitmoot planner here" means plan directly in this
current chat (optionally tracked as a session job via `gitmoot job open --type
ask`). Use `gitmoot agent ask <agent>` only when a separate agent should plan.

For background work, keep Gitmoot's resource model explicit: repo checkout
locks protect local checkouts, runtime session locks serialize delivery for the
same resumable runtime session (omp runs a fresh session per job).
The daemon default is `--workers 1`; raise it only for independent jobs
(distinct checkouts and runtime sessions; omp jobs each run a fresh session).

For runtime selection, `gitmoot agent start <name> --runtime <runtime>` accepts
only `omp`, the sole job runtime. Agents still registered on the removed
`codex`, `claude`, or `kimi` runtimes are reported as unsupported by `gitmoot
agent list` and `gitmoot doctor`, and their jobs are refused; re-register them
with `--runtime omp`. `omp` is the oh-my-pi CLI, a multi-provider routing harness:
authenticate it once (interactively, via a provider key in the daemon
environment, or through an auth broker) and restart the daemon so it inherits the
credential. omp holds `review`/`implement`/`ask`, runs a fresh
session per job and never resumes, and its implement jobs get a loud
cross-family-review refusal because an opaque router has no model family.
`agent subscribe` additionally accepts `--runtime shell`, whose
session is a command (for deterministic tests).

For Gitmoot health or status questions, run the relevant read-only Gitmoot CLI
checks and answer directly from the results. Mention `gitmoot dashboard` only
after that answer, as a live monitoring follow-up. Do not start daemons, create
agents, change subscriptions, or release locks unless the user
asks for that action.

## Before Acting

1. Check whether `gitmoot` is installed with `gitmoot version`.
2. Confirm GitHub CLI access with `gh auth status` before using PR workflows.
3. Detect or ask for the target repo before starting daemons, subscribing agents,
   or routing jobs.
4. Do not start daemons, create agents, change
   subscriptions, or release locks unless the user asks or the current task
   clearly requires it.
5. Prefer read-only status commands and answer directly before mutating Gitmoot
   state or pointing the user to live monitoring.
6. Use `gitmoot --help` and subcommand help for version-sensitive features such
   as `memory`, `runtime`, dashboard web,
   and session jobs.

## Install And Update

If `gitmoot` is not installed, install the latest beta:

```sh
curl -fsSL https://gitmoot.io/install.sh | sh
```

Verify the install and GitHub access before using Gitmoot:

```sh
gitmoot version
gitmoot update --check
gh auth status
```

Install runtime plugin guidance when the user wants Codex or Claude Code to
discover Gitmoot from its plugin system:

```sh
gitmoot plugin install codex
gitmoot plugin install claude
gitmoot plugin doctor
```

## Common Local Commands

```sh
gitmoot status --repo owner/repo
gitmoot daemon start --poll 30s
gitmoot daemon start --session <root-job-id>
gitmoot daemon start
gitmoot daemon status
gitmoot daemon restart
gitmoot daemon stop
gitmoot dashboard --json
gitmoot dashboard --web [--addr 127.0.0.1:8080]
gitmoot plugin doctor
gitmoot runtime list [--json]
gitmoot agent list
gitmoot agent doctor <agent>
gitmoot agent review <agent> --repo owner/repo --pr <number> "review request"
gitmoot agent ask <agent> --repo owner/repo "question or instructions"
gitmoot job list --repo owner/repo
gitmoot job show <job-id>
gitmoot job events <job-id>
gitmoot job gates <job-id>
gitmoot job gates clear <job-id> --need "<text>"|--all
gitmoot job open --agent <name> --repo owner/repo --type ask|review|implement [--title "..."]
gitmoot job close <job-id> --decision approved|changes_requested|blocked|implemented|failed [--severity P0|P1|P2|P3] [--summary "..."]
gitmoot job record (--agent <name> | --acting-role <role>) --repo owner/repo --type ask|review|implement --decision implemented [--severity P0|P1|P2|P3] [--summary "..."]
```

**Gitmoot does not dispatch implementation (#2203).** `agent implement`,
`--action implement`, `task run`/`task recover`/`task resume-work`, `repo
auto-fix`, and the implement heartbeat are gone; a `/gitmoot <agent> implement`
comment is refused. Seats implement in
their own session and record it — 568 of the last 570 implement rows were
already exactly that:

```sh
gitmoot job record --agent <seat> --repo owner/repo --type implement \
  --decision implemented [--pr <number> --head-sha <sha>] [--task <task-id>] \
  --summary "What changed and why."
```

Use `gitmoot daemon start` for the background daemon. Use `gitmoot daemon run`
only when the user explicitly wants a foreground process. `--repo owner/repo`
SCOPES the daemon to a single repo: it polls only that repo's PRs and claims
only that repo's queued jobs. Omit `--repo` to supervise every enabled
registered repo from one daemon (#581). Both `daemon run` and `daemon start` accept `--session <root-job-id>`
(alias `--root`) to pin the worker to one job tree: it then runs only
jobs whose `root_job_id` matches that value plus the root job
itself.

Use `gitmoot agent review` to dispatch
a review through a registered Gitmoot agent; Gitmoot owns read-only worktrees
and workflow advancement. Use `gitmoot agent ask` only for analysis, planning,
or questions. Add `--background` only when the user wants a queued background
job. This is the same agent registry and runtime adapter path used by PR-comment
jobs; the plugin only helps the runtime discover this skill and does not replace
the `gitmoot` CLI.

Use `gitmoot runtime list` to inspect runtime metadata.

Use `gitmoot workflow list`, `gitmoot workflow show`, and `gitmoot workflow note`
for external-coordinator workflow groups.
Attach `--workflow <label>` to agent ask/review or
`job open`.

## PR Comment Commands

Use GitHub PR comments as the public audit trail:

```text
/gitmoot help
/gitmoot status
/gitmoot <agent> review [instructions]
/gitmoot ask <agent> [question]
/gitmoot retry <job-id>
/gitmoot cancel <job-id>
/gitmoot merge
@<agent> ask|review [instructions]
```

## Agent Job Contract

Every Gitmoot job should return a concise and truthful `gitmoot_result` JSON
object:

```json
{
  "gitmoot_result": {
    "decision": "approved|changes_requested|blocked|implemented|failed|skipped",
    "summary": "Brief outcome.",
    "findings": [],
    "changes_made": [],
    "tests_run": [],
    "needs": []
  }
}
```

Use `blocked` when work cannot continue without human input or external state.
Use `failed` when the attempted action errored. Do not report tests, changes, or
approvals that were not actually verified.

## Safety Rules
A read-only review seat is handed an immutable, daemon-owned COPY of the
operator-pinned Go toolchain: run `go` normally, with `GOROOT`, `PATH` and
`GOTOOLCHAIN=local` already set. Do NOT invoke a toolchain path directly and do
not download Go into the seat cache; that workaround is retired. `CGO_ENABLED=0`
is still required and `-race` is still unavailable in a seat. If `go` returns exit
126 the copy was not staged, and the reason is on the daemon's stderr
(`gitmoot: read-only seat toolchain:`) rather than in the job's events.


Preserve existing behavior unless the job explicitly changes it. Keep work
scoped to the target repo. Do not commit generated data, caches, logs, secrets,
session archives, cloned helper repos, or large outputs unless explicitly
requested.

Implementation jobs must not edit or push an implementation branch unless
Gitmoot assigned the job.
Review and ask jobs should inspect and report without mutating branches unless
the task explicitly instructs otherwise.

Redact secrets from GitHub comments, job summaries, raw examples, and copied
command output.

## When Unsure

Reread this `SKILL.md`, then inspect `/gitmoot help`, `gitmoot status`, and the
relevant job events before acting. For CLI command questions, prefer
`gitmoot --help` and the relevant `gitmoot <command> --help` over remembered
skill text.
