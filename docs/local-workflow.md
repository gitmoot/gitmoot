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
gitmoot preset list
gitmoot preset update thermo-nuclear-code-quality-review
gitmoot agent subscribe <name> --runtime codex|claude|shell --session <id|name|last|command> --role <role> --repo owner/repo --capability <capability>
gitmoot agent subscribe thermo-review --runtime codex --session <session-id-or-last> --repo owner/repo --preset thermo-nuclear-code-quality-review
gitmoot agent allow <name> --repo owner/repo
gitmoot agent repos <name>
gitmoot agent list
gitmoot agent doctor <name>
gitmoot goal import --file GOAL.md --repo owner/repo
gitmoot task run task-001 --repo owner/repo --owner lead --base main
gitmoot job list
gitmoot job show <job-id>
gitmoot job retry <job-id>
gitmoot job cancel <job-id>
gitmoot status --repo owner/repo
gitmoot version --json
gitmoot update --check
gitmoot daemon start --repo owner/repo --poll 30s
gitmoot daemon start
gitmoot daemon status
```

Goal import turns Markdown headings shaped like `### Task N: Title` into local
planned tasks. `task run` starts one task branch and records its branch lock.

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

3. Start persistent agent sessions on the same machine.

   For Codex, start a normal session and note its session id or thread name.
   Using `last` works for quick demos, but explicit ids are safer because the
   newest session can change.

   For Claude Code, start the session and note its UUID. Claude sessions may use
   a UUID or `last`.

4. Initialize Gitmoot state and subscribe the agents.

   ```sh
   gitmoot setup --repo owner/project --path . --agent lead --runtime codex --session <codex-session-id> --role lead
   gitmoot doctor --repo .
   gitmoot agent subscribe audit --runtime claude --session <claude-session-id> --role reviewer --repo owner/project --capability review --capability ask
   gitmoot agent list
   gitmoot agent doctor lead
   gitmoot agent doctor audit
   ```

   To add the built-in strict review preset, fetch and cache it explicitly,
   then subscribe a Codex-backed review agent. `--preset` supplies the default
   reviewer role and `ask,review` capabilities when those flags are omitted.

   ```sh
   gitmoot preset update thermo-nuclear-code-quality-review
   gitmoot agent subscribe thermo-review \
     --runtime codex \
     --session <session-id-or-last> \
     --repo owner/project \
     --preset thermo-nuclear-code-quality-review
   gitmoot agent doctor thermo-review
   ```

   Preset updates are explicit and auditable. Diff upstream content before
   refreshing the local cached copy:

   ```sh
   gitmoot preset diff thermo-nuclear-code-quality-review
   gitmoot preset update thermo-nuclear-code-quality-review
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
   `--repo`. Use `gitmoot daemon start` without `--repo` after registering all
   intended repos if one daemon should supervise the whole Gitmoot home.

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
- Preset content is not fetched at job runtime. Run `gitmoot preset update`
  intentionally when you want to refresh a cached preset.

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
