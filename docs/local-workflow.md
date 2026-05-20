# Local Workflow

Gitmoot V1 runs on one machine. The GitHub repository is the visible audit
trail, while local SQLite state is the workflow source of truth. The local
daemon polls GitHub PRs and comments, routes jobs to registered agents, resumes
their runtime sessions through adapters, records job output, updates PR
statuses, and merges only after the merge gate passes.

## What Exists Today

The current CLI supports local state initialization, prerequisite checks, agent
registration, agent health checks, plan import, task branch startup, status,
and the daemon:

```sh
gitmoot init
gitmoot doctor --repo .
gitmoot agent subscribe <name> --runtime codex|claude|shell --session <id|name|last|command> --role <role> --repo owner/repo --capability <capability>
gitmoot agent list
gitmoot agent doctor <name>
gitmoot goal import --file GOAL.md --repo owner/repo
gitmoot task run task-001 --repo owner/repo --owner lead --base main
gitmoot status --repo owner/repo
gitmoot daemon start --repo owner/repo --poll 30s
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
   gitmoot init
   gitmoot doctor --repo .
   gitmoot agent subscribe lead --runtime codex --session <codex-session-id> --role lead --repo owner/project --capability implement --capability review --capability ask
   gitmoot agent subscribe audit --runtime claude --session <claude-session-id> --role reviewer --repo owner/project --capability review --capability ask
   gitmoot agent list
   gitmoot agent doctor lead
   gitmoot agent doctor audit
   ```

5. Start the daemon from the repository checkout.

   ```sh
   gitmoot daemon start --repo owner/project --poll 30s
   ```

   The daemon validates that the current checkout's `origin` remote matches
   `--repo`. Keep it running while the workflow is active.

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
   /gitmoot audit review focus on correctness and missed edge cases
   /gitmoot lead implement fix the review findings without broad refactors
   ```

   Implement jobs require the agent to hold the branch lock. Review and ask jobs
   are routed through the runtime adapter and must return the `gitmoot_result`
   JSON contract.

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
