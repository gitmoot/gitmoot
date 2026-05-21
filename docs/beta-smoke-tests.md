# Beta Smoke Tests

Use these smoke tests before cutting a beta release. They verify the local V1
loop without a hosted service or webhook receiver.

## Prerequisites

Run from each repository checkout that will be watched:

```sh
git status --short
git remote -v
gh auth status
gitmoot doctor --repo .
```

Use a test repository or a disposable branch. Keep generated logs, cloned
helper repos, session archives, and large outputs untracked.

## One-Repo Smoke Test

Goal: PR comment -> queued ask job -> adapter result -> attributed PR comment
-> local job status update. This intentionally uses `ask`, not `review`, so
the smoke test cannot approve or merge the PR.

1. Register the repo and a shell smoke agent.

   ```sh
   gitmoot setup --repo owner/project --path . --agent shell-smoke --runtime shell --session "printf '%s\n' '{\"gitmoot_result\":{\"decision\":\"approved\",\"summary\":\"shell ask smoke passed\",\"findings\":[],\"changes_made\":[],\"tests_run\":[\"shell smoke\"],\"needs\":[],\"next_agents\":[]}}'"
   gitmoot agent repos shell-smoke
   ```

2. Start the background daemon.

   ```sh
   gitmoot daemon start --repo owner/project --poll 30s
   gitmoot daemon status
   ```

3. Open a small test PR in `owner/project`, then comment:

   ```text
   /gitmoot help
   /gitmoot shell-smoke ask smoke test routing
   ```

4. Confirm the job was queued and completed.

   ```sh
   gitmoot job list --repo owner/project
   gitmoot events --repo owner/project
   gh pr view <number> --repo owner/project --comments
   ```

Expected signals:

- The PR receives a Gitmoot queued-job acknowledgement.
- `gitmoot job list --repo owner/project` shows the job as succeeded.
- The PR receives a result comment with:

  ```md
  > Agent: `shell-smoke`
  > Runtime: `shell`
  > Job: `...`
  ```

- `gitmoot events --repo owner/project` shows the queued/running/succeeded
  job events.

5. Stop the daemon when finished.

   ```sh
   gitmoot daemon stop
   gitmoot daemon status
   ```

## Two-Repo Smoke Test

Goal: one daemon -> two registered repos -> same allowed agent -> ask jobs in
each repo -> no cross-routing. This intentionally avoids approving reviews.

1. Register both repos with the same agent identity.

   ```sh
   cd /path/to/project-a
   gitmoot setup --repo owner/project-a --path . --agent shell-smoke --runtime shell --session "printf '%s\n' '{\"gitmoot_result\":{\"decision\":\"approved\",\"summary\":\"repo ask smoke passed\",\"findings\":[],\"changes_made\":[],\"tests_run\":[\"shell smoke\"],\"needs\":[],\"next_agents\":[]}}'"

   cd /path/to/project-b
   gitmoot setup --repo owner/project-b --path . --agent shell-smoke --runtime shell --session "printf '%s\n' '{\"gitmoot_result\":{\"decision\":\"approved\",\"summary\":\"repo ask smoke passed\",\"findings\":[],\"changes_made\":[],\"tests_run\":[\"shell smoke\"],\"needs\":[],\"next_agents\":[]}}'"

   gitmoot agent repos shell-smoke
   ```

2. Start one daemon for all enabled repos.

   ```sh
   gitmoot daemon start
   gitmoot daemon status
   gitmoot status
   ```

3. Open one test PR in each repo. Comment in each PR:

   ```text
   /gitmoot shell-smoke ask repo routing smoke
   ```

4. Verify each repo saw only its own job.

   ```sh
   gitmoot job list --repo owner/project-a
   gitmoot job list --repo owner/project-b
   gitmoot events --repo owner/project-a
   gitmoot events --repo owner/project-b
   gh pr view <project-a-pr> --repo owner/project-a --comments
   gh pr view <project-b-pr> --repo owner/project-b --comments
   ```

Expected signals:

- Each PR receives exactly the acknowledgement and result for its own comment.
- `gitmoot job list --repo owner/project-a` does not show project B jobs.
- `gitmoot job list --repo owner/project-b` does not show project A jobs.
- The same agent name is allowed on both repos:

  ```sh
  gitmoot agent repos shell-smoke
  ```

## Recovery Checks

Run these against one smoke job if you need to verify recovery UX:

```sh
gitmoot job show <job-id>
gitmoot job events <job-id>
gitmoot job retry <job-id>
gitmoot job cancel <job-id>
gitmoot lock list --repo owner/project
gitmoot lock show owner/project <branch>
```

Only retry failed, blocked, or cancelled jobs. Only cancel queued or running
jobs. Use `gitmoot lock release owner/project <branch> --owner <agent>` for an
exact-owner stale lock; use `--force` only when the stored owner is stale.

## Known V1 Limits

- Local-only: the machine running the daemon must stay online.
- Polling watches GitHub; there is no webhook receiver.
- GitHub comments are authored by the authenticated `gh` user, not a bot.
- Agent identity is shown in the comment body.
- There is no hosted dashboard, GitHub App bot identity, cloud runner, billing,
  or remote control plane.
