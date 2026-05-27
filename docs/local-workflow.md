# Local Workflow

Gitmoot V1 runs on one machine. The GitHub repository is the visible audit
trail, while local SQLite state is the workflow source of truth. The local
daemon polls GitHub PRs and comments, routes jobs to registered agents, resumes
their runtime sessions through adapters, records job output, updates PR
statuses, and merges only after the merge gate passes.

## What Exists Today

The current CLI supports local setup, multi-repo daemon management, agent
registration, plan import, task branch startup, status, recovery, and updates:

```sh
gitmoot setup --repo owner/repo --path . --agent lead --runtime codex --session <session-ref> --role lead
gitmoot doctor --repo .
gitmoot plugin install codex
gitmoot plugin install claude
gitmoot plugin doctor
gitmoot agent template list
gitmoot agent template add frontend-reviewer --file agents/frontend-reviewer.md
gitmoot agent template update thermo-nuclear-code-quality-review
gitmoot agent start <name> --runtime codex|claude --repo owner/repo --path . --template thermo-nuclear-code-quality-review --start-daemon
gitmoot agent subscribe <name> --runtime codex|claude|shell --session <id|name|last|command> --role <role> --repo owner/repo --capability <capability>
gitmoot agent ask <name> "message" --repo owner/repo
gitmoot agent ask <name> --background --repo owner/repo "message"
gitmoot agent type list
gitmoot agent type show planner
gitmoot agent gc
gitmoot agent start thermo-review --runtime codex --repo owner/repo --template thermo-nuclear-code-quality-review
gitmoot agent allow <name> --repo owner/repo
gitmoot agent repos <name>
gitmoot agent list
gitmoot agent doctor <name>
gitmoot goal import --file GOAL.md --repo owner/repo
gitmoot task run task-001 --repo owner/repo --owner lead --base main
gitmoot job list
gitmoot job show <job-id>
gitmoot job watch <job-id>
gitmoot job retry <job-id>
gitmoot job cancel <job-id>
gitmoot status --repo owner/repo
gitmoot version --json
gitmoot update --check
gitmoot daemon start --repo owner/repo --poll 30s --workers 1
gitmoot daemon start
gitmoot daemon status
```

Goal import turns Markdown headings shaped like `### Task N: Title` into local
planned tasks. `task run` starts one task branch and records its branch lock.

## Runtime Plugin Setup

Install the Codex or Claude plugin when you want the runtime to discover
Gitmoot's Agent Skill through its plugin system:

```sh
curl -fsSL https://gitmoot.io/install.sh | sh
gitmoot plugin install codex
gitmoot plugin install claude
gitmoot plugin doctor
```

The plugins are guidance and discovery surfaces. They do not replace
`gitmoot daemon start`, agent registration, GitHub CLI authentication, or the
local SQLite workflow state.

For fast planning in the current Codex or Claude chat, ask the runtime to use
the Gitmoot planner here. That applies the lightweight `planner-here`
instructions directly in the current conversation and avoids the startup cost of
a background planner job.

If a Codex or Claude chat wants to invoke a registered Gitmoot agent directly,
it should run `gitmoot agent ask <agent> --repo owner/repo "..."`. That uses the
same local agent registry and runtime adapter path as PR-comment ask jobs.

## Execution Model

Gitmoot has two execution paths:

- **Here**: the current Codex or Claude chat reads Gitmoot skill and template
  instructions directly. This does not create a Gitmoot job and is the fastest
  path for planning when the current chat already has context.
- **Background**: Gitmoot creates a job, records events in SQLite, and the
  daemon or `gitmoot job run <job-id>` delivers that job through the runtime
  adapter.

Background execution uses separate resource categories:

- **Repo checkout locks**: daemon-side mutexes that prevent two enabled repo
  ticks from mutating the same checkout at once.
- **Runtime session locks**: SQLite resource locks keyed as
  `runtime:<runtime>:<runtime_ref>`, shared by daemon jobs, `job run`, and
  synchronous `agent ask`, so one Codex or Claude session is never resumed by
  two jobs at the same time.
- **Branch locks**: workflow ownership records used for implementation and
  merge safety.

The daemon defaults to `--workers 1`. Raise `--workers` only when the Gitmoot
home has independent runtime sessions or managed agent types with
`max_background` greater than one. Jobs that target the same runtime session or
same checkout remain serialized even when the worker count is higher.

## End-To-End Demo Path

1. Create or choose the project repository.

   ```sh
   git clone git@github.com:owner/project.git
   cd project
   git status --short
   git remote -v
   gh auth status
   ```

2. Write and import the plan.

   Keep the implementation plan in a tracked file such as `GOAL.md`. The future
   command shape is:

   ```sh
   gitmoot goal import --file GOAL.md --repo owner/project
   ```

3. Initialize Gitmoot state and start Gitmoot-managed agents.

   `agent start` creates a new runtime session, stores the returned session
   reference, grants the repo, and can start the background daemon. For Codex it
   runs `codex exec --json -- <startup-prompt>` and records the emitted
   `thread.started.thread_id`. For Claude Code it creates a session id and uses
   the installed Claude CLI's non-interactive print mode.

   ```sh
   gitmoot init
   gitmoot doctor --repo .
   gitmoot plugin install codex
   gitmoot plugin doctor codex
   gitmoot agent start lead \
     --runtime codex \
     --repo owner/project \
     --path . \
     --role lead \
     --capability ask \
     --capability review \
     --capability implement
   gitmoot agent list
   gitmoot agent doctor lead
   ```

   To add the built-in strict review template, fetch and cache it explicitly.
   `--template` supplies the default reviewer role and `ask,review` capabilities
   when those flags are omitted.

   ```sh
   gitmoot agent template update thermo-nuclear-code-quality-review
   gitmoot agent start thermo-review \
     --runtime codex \
     --repo owner/project \
     --template thermo-nuclear-code-quality-review
   gitmoot agent doctor thermo-review
   ```

   If the template is not cached yet, `agent start --template ...` fails with the
   same explicit `gitmoot agent template update <template>` guidance as `agent subscribe`.
   Add `--update-template` when you want startup to refresh the cached template
   before creating the runtime session.

   Custom prompt agent templates are local files snapshotted into Gitmoot state. Use
   them when you want a repo- or team-specific agent profile without changing
   Codex, Claude, or repository agent files.

   ```sh
   mkdir -p agents
   printf '%s\n' 'Review frontend changes for correctness and responsive behavior.' > agents/frontend-reviewer.md
   gitmoot agent template add frontend-reviewer --file agents/frontend-reviewer.md
   gitmoot agent start frontend-reviewer \
     --runtime codex \
     --repo owner/project \
     --path . \
     --template frontend-reviewer \
     --role reviewer \
     --capability ask \
     --capability review
   ```

   Built-in agent templates can define default roles and capabilities. Custom agent templates
   do not in V1, so pass the role and capabilities you want. `agent start`
   still keeps the normal fallback defaults if omitted, while
   `agent subscribe --template <custom-id>` requires explicit values.

   After editing a custom prompt file, refresh the cached snapshot explicitly:

   ```sh
   gitmoot agent template diff frontend-reviewer
   gitmoot agent template update frontend-reviewer
   ```

   After startup, open a created Codex session later with the session id printed
   by Gitmoot:

   ```sh
   codex resume <session-id>
   ```

4. Subscribe existing sessions or shell adapters when needed.

   Use `agent subscribe` when a Codex or Claude session already exists, or when
   registering a shell command adapter. Using `last` works for quick demos, but
   explicit ids are safer because the newest session can change.

   ```sh
   gitmoot agent subscribe audit --runtime claude --session <claude-session-id> --role reviewer --repo owner/project --capability review --capability ask
   gitmoot agent subscribe shell-smoke --runtime shell --session "printf '%s\n' '{\"gitmoot_result\":{\"decision\":\"approved\",\"summary\":\"ok\",\"findings\":[],\"changes_made\":[],\"tests_run\":[\"shell\"],\"needs\":[],\"next_agents\":[]}}'" --role reviewer --repo owner/project --capability ask
   gitmoot agent list
   ```

   Template updates are explicit and auditable. Diff upstream content before
   refreshing the local cached copy. For custom agent templates, `diff` compares the
   cached content with the stored local file path.

   ```sh
   gitmoot agent template diff thermo-nuclear-code-quality-review
   gitmoot agent template update thermo-nuclear-code-quality-review
   ```

   To inspect the installed Gitmoot build or check for a beta release:

   ```sh
   gitmoot version
   gitmoot update --check
   ```

5. Start the daemon from the repository checkout.

   ```sh
   gitmoot daemon start --repo owner/project --poll 30s
   ```

   The daemon validates that the current checkout's `origin` remote matches
   `--repo`. `agent start --start-daemon` runs this same background start path
   for the selected checkout. Use `gitmoot daemon start` without `--repo` after
   registering all intended repos if one daemon should supervise the whole
   Gitmoot home.

6. Start and open the first task PR.

   ```sh
   gitmoot task run task-001 --repo owner/project --owner lead --base main
   ```

   The lead agent or the human creates the task branch, implements the task,
   pushes it, and opens a PR. The PR comments become the public audit trail.
   The local Gitmoot database tracks the task, jobs, branch locks, PR head SHA,
   and merge gate state.

7. Route other agents through PR comments.

   A repository writer can ask a subscribed agent to work from a PR comment:

   ```text
   /gitmoot help
   /gitmoot audit review focus on correctness and missed edge cases
   /gitmoot thermo-review review
   /gitmoot lead implement fix the review findings without broad refactors
   /gitmoot retry <job-id>
   /gitmoot cancel <job-id>
   ```

   Implement jobs require the agent to hold the branch lock. Review and ask jobs
   are routed through the runtime adapter and must return the `gitmoot_result`
   JSON contract.

   For a local chat ask that should not go through a PR comment, call the same
   registered agent directly:

   ```sh
   gitmoot agent ask project-planner --repo owner/project "Write a task-by-task implementation plan and goal file prompt."
   gitmoot job show <job-id>
   ```

   Stale branch locks can be inspected and released locally:

   ```sh
   gitmoot job list --repo owner/project
   gitmoot job show <job-id>
   gitmoot job events <job-id>
   gitmoot lock list --repo owner/project
   gitmoot lock show owner/project <branch>
   gitmoot lock release owner/project <branch> --owner <agent>
   ```

8. Let the PR converge.

   Agents review, request fixes, and rerun work through comments and job output.
   When required reviews are approved and the branch is ready, the merge gate
   checks the current PR head, local worktree cleanliness, branch freshness,
   Gitmoot statuses, external CI if present, and mergeability.

9. Merge and continue.

   By default Gitmoot merges with a squash merge guarded by the current head SHA.
   After merge it records the merged commit, releases the branch lock, updates
   the local base branch, and can enqueue the next task once the task queueing
   policy selects it.

## Local-Only Limits

- The machine running `gitmoot daemon start` must stay online.
- Polling is the V1 mechanism; there is no webhook receiver yet.
- GitHub Checks are best implemented later through GitHub App mode. V1 uses
  commit statuses and `gh pr checks`.
- Use explicit session ids for long workflows. `last` is convenient but can
  point at the wrong session after a new Codex or Claude session starts.
- GitHub comments are the public audit trail, not canonical state. Local SQLite
  remains the workflow source of truth.
- There is no hosted dashboard, cloud runner, billing, or remote control plane
  in V1.
- GitHub comments are authored by the authenticated user. Agent attribution is
  written in the comment body.
- Template content is not fetched at job runtime. Run `gitmoot agent template update`
  intentionally when you want to refresh a cached template.

## Multi-Repo Supervision

One daemon can supervise multiple enabled repos for the same Gitmoot home.
Register each checkout explicitly and grant agents repo access explicitly:

```sh
cd /path/to/project-a
gitmoot setup --repo owner/project-a --path . --agent lead --runtime codex --session <session-ref> --role lead

cd /path/to/project-b
gitmoot setup --repo owner/project-b --path . --agent lead --runtime codex --session <same-session-or-explicit-ref> --role lead

gitmoot agent repos lead
gitmoot daemon start
gitmoot status
```

The PR repository supplies routing context. `/gitmoot lead review` in
`owner/project-a` queues work for `owner/project-a`; the same command in
`owner/project-b` queues work for `owner/project-b` if `lead` is allowed there.

## Skill Usage

Agents should read the root [`SKILL.md`](../SKILL.md) before working through
Gitmoot. The skill documents PR commands, the required `gitmoot_result` JSON
contract, branch lock rules, repo access, and safe agent behavior. If an agent
is unsure how Gitmoot expects it to behave, it should reread `SKILL.md`, then
inspect `/gitmoot help`, `gitmoot status`, and relevant job events.

## Agent Output Contract

Agents must return exactly one JSON object containing `gitmoot_result`:

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

Valid decisions are `approved`, `changes_requested`, `blocked`, `implemented`,
and `failed`.
