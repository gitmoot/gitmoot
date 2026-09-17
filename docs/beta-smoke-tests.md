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

## Plugin Package Smoke Test

Goal: prove Gitmoot can build runtime plugin packages, register local
marketplaces in isolated homes, and diagnose the generated packages without
writing into the real user runtime state.

For the scripted version, run:

```sh
GO_BIN=/path/to/go1.26 scripts/plugin-smoke.sh
```

1. Build a local test binary and use an isolated Gitmoot home.

   ```sh
   GOTOOLCHAIN=go1.26.0 go build -o /tmp/gitmoot-current ./cmd/gitmoot
   export GITMOOT_SMOKE_HOME=/tmp/gitmoot-plugin-smoke
   export GITMOOT_RUNTIME_HOME=/tmp/gitmoot-plugin-runtime-smoke
   rm -rf "$GITMOOT_SMOKE_HOME"
   rm -rf "$GITMOOT_RUNTIME_HOME"
   mkdir -p "$GITMOOT_RUNTIME_HOME"
   /tmp/gitmoot-current init --home "$GITMOOT_SMOKE_HOME"
   ```

2. Build both plugin packages.

   ```sh
   /tmp/gitmoot-current plugin build codex --home "$GITMOOT_SMOKE_HOME"
   /tmp/gitmoot-current plugin build claude --home "$GITMOOT_SMOKE_HOME"
   /tmp/gitmoot-current plugin path codex --home "$GITMOOT_SMOKE_HOME"
   /tmp/gitmoot-current plugin path claude --home "$GITMOOT_SMOKE_HOME"
   ```

3. Diagnose the built packages.

   ```sh
   /tmp/gitmoot-current plugin doctor --home "$GITMOOT_SMOKE_HOME" || true
   /tmp/gitmoot-current plugin doctor codex --home "$GITMOOT_SMOKE_HOME" || true
   /tmp/gitmoot-current plugin doctor claude --home "$GITMOOT_SMOKE_HOME" || true
   ```

   Missing runtime CLIs are valid in this smoke path. Continue when doctor
   reports `runtime-cli` failures for missing `codex` or `claude`.

4. Install with an isolated runtime home.

   ```sh
   HOME="$GITMOOT_RUNTIME_HOME" /tmp/gitmoot-current plugin install codex --home "$GITMOOT_SMOKE_HOME" --force
   HOME="$GITMOOT_RUNTIME_HOME" /tmp/gitmoot-current plugin install claude --home "$GITMOOT_SMOKE_HOME" --scope user --force
   ```

   If `codex` or `claude` is not installed, the command should keep generated
   files and print manual install commands instead of failing after partial
   destructive work.

5. Validate diagnostics again after install.

   ```sh
   /tmp/gitmoot-current plugin doctor --home "$GITMOOT_SMOKE_HOME" || true
   /tmp/gitmoot-current plugin doctor codex --home "$GITMOOT_SMOKE_HOME" || true
   /tmp/gitmoot-current plugin doctor claude --home "$GITMOOT_SMOKE_HOME" || true
   ```

Expected signals:

- `plugin path codex` and `plugin path claude` point under
  `$GITMOOT_SMOKE_HOME/.gitmoot/plugins/build`.
- Each generated package contains `skills/gitmoot/SKILL.md`.
- Doctor reports readable manifests and copied skill files.
- Runtime marketplace or install state is written under `$GITMOOT_RUNTIME_HOME`,
  not the real user home.
- Missing runtime CLIs are reported as diagnostics with next steps, not as
  corrupt generated packages.

## One-Repo Smoke Test

Goal: PR comment -> queued ask job -> adapter result -> attributed PR comment
-> local job status update. This intentionally uses `ask`, not `review`, so
the smoke test cannot approve or merge the PR.

1. Register the repo and a shell smoke agent.

   ```sh
   gitmoot setup --repo owner/project --path . --agent shell-smoke --runtime shell --session "printf '%s\n' '{\"gitmoot_result\":{\"decision\":\"approved\",\"summary\":\"shell ask smoke passed\",\"findings\":[],\"changes_made\":[],\"tests_run\":[\"shell smoke\"],\"needs\":[],\"delegations\":[]}}'"
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

## Delegation (Orchestra) Smoke Test

Orchestra is Gitmoot's name for structured multi-agent delegation: a conductor
(coordinator) returns a `delegations[]` score, the players (child agents) run in
parallel or in dependency order, and a finale (continuation) reconvenes and
synthesizes the results.

Goal: background coordinator job -> `delegations` in the coordinator result ->
two child jobs fanned out to worker agents -> both children succeed -> coordinator
continuation job enqueued. This exercises the structured delegation path end to
end with local shell agents, so it needs no external runtime CLIs.

The coordinator must run as background work so the daemon executes it through the
engine and dispatches the delegations. A synchronous `gitmoot agent ask` without
`--background` only returns the coordinator's own result and does not fan out, so
the test would silently pass with zero child jobs.

1. Register a coordinator shell agent whose result returns two delegations, and
   two worker shell agents whose results return empty `delegations` (so the
   fan-out terminates and does not recurse).

   ```sh
   gitmoot setup --repo owner/project --path . --agent coordinator --runtime shell --session "printf '%s\n' '{\"gitmoot_result\":{\"decision\":\"approved\",\"summary\":\"coordinator delegating ui and api\",\"findings\":[],\"changes_made\":[],\"tests_run\":[],\"needs\":[],\"delegations\":[{\"id\":\"ui\",\"agent\":\"ui-worker\",\"action\":\"ask\",\"prompt\":\"propose the ui changes\"},{\"id\":\"api\",\"agent\":\"api-worker\",\"action\":\"ask\",\"prompt\":\"review the api contract\"}]}}'"
   gitmoot agent subscribe ui-worker --runtime shell --session "printf '%s\n' '{\"gitmoot_result\":{\"decision\":\"approved\",\"summary\":\"ui worker done\",\"findings\":[],\"changes_made\":[],\"tests_run\":[\"shell smoke\"],\"needs\":[],\"delegations\":[]}}'" --role reviewer --repo owner/project --capability ask --capability review
   gitmoot agent subscribe api-worker --runtime shell --session "printf '%s\n' '{\"gitmoot_result\":{\"decision\":\"approved\",\"summary\":\"api worker done\",\"findings\":[],\"changes_made\":[],\"tests_run\":[\"shell smoke\"],\"needs\":[],\"delegations\":[]}}'" --role reviewer --repo owner/project --capability ask --capability review
   gitmoot agent repos coordinator
   ```

2. Start the background daemon so it executes the queued coordinator job and
   fans out the delegations.

   ```sh
   gitmoot daemon start --repo owner/project --poll 30s
   gitmoot daemon status
   ```

3. Queue the coordinator as background work.

   ```sh
   gitmoot agent ask coordinator --repo owner/project --background "coordinate the ui and api work"
   ```

4. Confirm the coordinator job, the two child jobs, and the continuation job.

   ```sh
   gitmoot job list --repo owner/project
   gitmoot events --repo owner/project
   ```

Expected signals:

- `gitmoot events --repo owner/project` shows two `delegation_enqueued` events
  on the coordinator job, one for the `ui` delegation and one for the `api`
  delegation.
- `gitmoot job list --repo owner/project` shows two child jobs with composite
  ids of the form `<coordinator-job-id>/delegation/ui` and
  `<coordinator-job-id>/delegation/api`, each reaching `succeeded`.
- After both children succeed, `gitmoot events --repo owner/project` shows a
  `delegation_continuation_enqueued` event and `gitmoot job list` shows a
  coordinator continuation job `<coordinator-job-id>/continuation` queued for
  `coordinator`.

> Note: the continuation job runs the **same** coordinator agent. A real LLM
> coordinator ends the loop by returning no `delegations` on the continuation,
> but a *static* shell agent returns the same delegations every time, so the
> continuation re-delegates each generation. This is bounded by
> `MaxDelegationDepth` (8): once a coordinator/continuation at that depth would
> dispatch, Gitmoot refuses, records a `delegation_depth_exceeded` event, and
> enqueues one terminal graceful-finalize continuation
> (`delegation_finalize_enqueued`) instead of spawning jobs forever. To keep this
> smoke test fast, run
> `gitmoot daemon stop` once you have observed the first continuation rather than
> waiting for the depth cap to halt the chain. Also note delegated `action:
> review` requires a pull request / head SHA; use `action: ask` for PR-less
> smoke agents (a no-PR review fails with "job for <branch> has no head SHA").

5. (Optional) Verify dependency gating. Re-run with a coordinator whose result
   adds a third delegation that depends on the first two; it must stay queued
   until both deps succeed, then run.

   ```json
   {
     "id": "integrate",
     "agent": "api-worker",
     "action": "ask",
     "prompt": "integrate the ui and api work",
     "deps": ["ui", "api"]
   }
   ```

   Expected: `gitmoot events --repo owner/project` shows the
   `<coordinator-job-id>/delegation/integrate` job enqueued only after the `ui`
   and `api` children reach `succeeded`, and the continuation job is enqueued
   once `integrate` also succeeds.

6. Stop the daemon when finished.

   ```sh
   gitmoot daemon stop
   gitmoot daemon status
   ```

## Thermo Template Smoke Test

Goal: PR comment -> queued review job -> Codex resume with the installed thermo
template instructions -> attributed PR result comment. Run this with a Gitmoot
build that includes `gitmoot agent template` commands.

1. Confirm the thermo template is installed, then start a Gitmoot-managed Codex
   review agent. If `agent template show` reports it missing, seed its
   `agent_templates` store row by hand before continuing.

   ```sh
   gitmoot agent template show thermo-nuclear-code-quality-review
   gitmoot agent start thermo-review \
     --runtime codex \
     --repo owner/project \
     --path . \
     --template thermo-nuclear-code-quality-review
   gitmoot agent doctor thermo-review
   ```

   Gitmoot prints the created session id. To inspect that Codex thread later:

   ```sh
   codex resume <session-id>
   ```

   If you prefer registering an already-open Codex session, use
   `gitmoot agent subscribe ... --session <session-id-or-last>` instead.

2. Start the daemon for the test repo, or pass `--start-daemon` to
   `agent start`.

   ```sh
   gitmoot daemon start --repo owner/project --poll 10s
   gitmoot daemon status
   ```

3. Open a disposable PR, then comment:

   ```text
   /gitmoot thermo-review review
   ```

4. Verify the queued job and PR result.

   ```sh
   gitmoot job list --repo owner/project
   gh pr view <number> --repo owner/project --comments
   ```

Expected signals:

- The PR receives a queued-job acknowledgement for `thermo-review`.
- `gitmoot job list --repo owner/project` shows the review job.
- The result comment includes template attribution:

  ```md
  > Agent: `thermo-review`
  > Runtime: `codex`
  > Template: `thermo-nuclear-code-quality-review`
  > Job: `...`
  ```

## Planner Template Smoke Test

Goal: installed planner template -> Gitmoot-managed Codex planner agent. This
verifies the planning workflow is discoverable before using it on a real PR.
The goal-file step is gone (#2205) and templates are installed data rather than
authored in gitmoot (#2204), so the smoke test starts from an installed row.

1. Build a local test binary and use an isolated Gitmoot home.

   ```sh
   GOTOOLCHAIN=go1.26.0 go build -o /tmp/gitmoot-current ./cmd/gitmoot
   export GITMOOT_SMOKE_HOME=/tmp/gitmoot-planner-template-smoke
   rm -rf "$GITMOOT_SMOKE_HOME"
   /tmp/gitmoot-current init --home "$GITMOOT_SMOKE_HOME"
   ```

2. Confirm the planner template is available.

   ```sh
   /tmp/gitmoot-current agent template list --home "$GITMOOT_SMOKE_HOME" | grep planner
   /tmp/gitmoot-current agent template show --home "$GITMOOT_SMOKE_HOME" planner
   ```

3. From the test repo checkout, start the planner agent.

   ```sh
   cd /path/to/project
   /tmp/gitmoot-current agent start project-planner-smoke \
     --home "$GITMOOT_SMOKE_HOME" \
     --runtime codex \
     --repo owner/project \
     --path . \
     --template planner \
     --start-daemon
   /tmp/gitmoot-current agent doctor project-planner-smoke --home "$GITMOOT_SMOKE_HOME"
   /tmp/gitmoot-current daemon status --home "$GITMOOT_SMOKE_HOME"
   ```

4. Ask the planner directly through the local agent path.

   ```sh
   /tmp/gitmoot-current agent ask project-planner-smoke \
     --home "$GITMOOT_SMOKE_HOME" \
     --repo owner/project \
     "Write a task-by-task implementation plan for this feature."
   /tmp/gitmoot-current job list --home "$GITMOOT_SMOKE_HOME" --repo owner/project
   /tmp/gitmoot-current job show <local-ask-job-id> --home "$GITMOOT_SMOKE_HOME"
   ```

5. Open a disposable PR, then comment:

   ```text
   /gitmoot project-planner-smoke ask Write a task-by-task implementation plan for this feature.
   ```

6. Verify the queued PR job and PR result.

   ```sh
   /tmp/gitmoot-current job list --home "$GITMOOT_SMOKE_HOME" --repo owner/project
   /tmp/gitmoot-current job show <pr-ask-job-id> --home "$GITMOOT_SMOKE_HOME"
   /tmp/gitmoot-current events --home "$GITMOOT_SMOKE_HOME" --repo owner/project
   gh pr view <number> --repo owner/project --comments
   ```

Expected signals:

- `agent template show` displays `default role: planner`, `default capabilities: ask`,
  and `mutation: true`.
- `agent doctor project-planner-smoke` succeeds.
- `agent ask project-planner-smoke` prints `state: succeeded`, `agent: project-planner-smoke`,
  `action: ask`, and a planner summary.
- `job show <local-ask-job-id>` includes `"sender": "local"`, the installed
  `planner` template metadata, and the planner result.
- The PR result comment includes `Template: planner`.
- The planner returns a decision-complete plan split into tasks, each with its
  scope, PR boundary, acceptance criteria, and suggested commit message.

7. Stop the isolated daemon.

   ```sh
   /tmp/gitmoot-current daemon stop --home "$GITMOOT_SMOKE_HOME"
   /tmp/gitmoot-current daemon status --home "$GITMOOT_SMOKE_HOME"
   ```

## Agent Start Smoke Test

Goal: prove `gitmoot agent start` can create a Codex session, store the session
reference, start the daemon, and route a PR comment job through that new
session.

1. Build a local test binary and use an isolated Gitmoot home.

   ```sh
   GOTOOLCHAIN=go1.26.0 go build -o /tmp/gitmoot-current ./cmd/gitmoot
   export GITMOOT_SMOKE_HOME=/tmp/gitmoot-agent-start-smoke
   rm -rf "$GITMOOT_SMOKE_HOME"
   /tmp/gitmoot-current init --home "$GITMOOT_SMOKE_HOME"
   ```

2. From the test repo checkout, start the agent against the installed thermo
   template. Seed its `agent_templates` store row by hand first if
   `agent template show` reports it missing.

   ```sh
   cd /path/to/project
   /tmp/gitmoot-current agent start thermo-start-smoke \
     --home "$GITMOOT_SMOKE_HOME" \
     --runtime codex \
     --repo owner/project \
     --path . \
     --template thermo-nuclear-code-quality-review \
     --start-daemon
   /tmp/gitmoot-current agent list --home "$GITMOOT_SMOKE_HOME"
   /tmp/gitmoot-current daemon status --home "$GITMOOT_SMOKE_HOME"
   ```

3. Open a disposable PR, then comment:

   ```text
   /gitmoot thermo-start-smoke review
   ```

4. Verify the job and PR comments.

   ```sh
   /tmp/gitmoot-current job list --home "$GITMOOT_SMOKE_HOME" --repo owner/project
   /tmp/gitmoot-current events --home "$GITMOOT_SMOKE_HOME" --repo owner/project
   gh pr view <number> --repo owner/project --comments
   ```

Expected signals:

- `agent list` shows `thermo-start-smoke` with a generated Codex session id.
- The PR receives a queued-job acknowledgement.
- The job succeeds and the result comment includes agent, runtime, template, and
  job metadata.
- `codex resume <session-id>` opens the created session if manual inspection is
  needed.

5. Stop the isolated daemon.

   ```sh
   /tmp/gitmoot-current daemon stop --home "$GITMOOT_SMOKE_HOME"
   /tmp/gitmoot-current daemon status --home "$GITMOOT_SMOKE_HOME"
   ```

## Two-Repo Smoke Test

Goal: one daemon -> two registered repos -> same allowed agent -> ask jobs in
each repo -> no cross-routing. This intentionally avoids approving reviews.

1. Register both repos with the same agent identity.

   ```sh
   cd /path/to/project-a
   gitmoot setup --repo owner/project-a --path . --agent shell-smoke --runtime shell --session "printf '%s\n' '{\"gitmoot_result\":{\"decision\":\"approved\",\"summary\":\"repo ask smoke passed\",\"findings\":[],\"changes_made\":[],\"tests_run\":[\"shell smoke\"],\"needs\":[],\"delegations\":[]}}'"

   cd /path/to/project-b
   gitmoot setup --repo owner/project-b --path . --agent shell-smoke --runtime shell --session "printf '%s\n' '{\"gitmoot_result\":{\"decision\":\"approved\",\"summary\":\"repo ask smoke passed\",\"findings\":[],\"changes_made\":[],\"tests_run\":[\"shell smoke\"],\"needs\":[],\"delegations\":[]}}'"

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

## Execution Model Smoke Test

Goal: verify the final `here` versus `background` execution model and the
resource scheduling rules.

1. Confirm fast planner guidance does not start a background runtime job.

   In a Codex or Claude chat with the Gitmoot skill installed, ask:

   ```text
   Use the Gitmoot planner here. Write a task-by-task implementation plan for a README wording update.
   ```

   Expected signal: the answer appears directly in the current chat, and
   `gitmoot job list --repo owner/project` does not gain a new planner job.

2. Queue two background asks to the same registered Codex or Claude agent.

   ```sh
   gitmoot agent ask project-planner --repo owner/project --background "Say first OK."
   gitmoot agent ask project-planner --repo owner/project --background "Say second OK."
   gitmoot job watch <first-job-id>
   gitmoot job watch <second-job-id>
   ```

   Expected signal: both jobs finish, but their `job events` do not show
   overlapping runtime delivery for the same `runtime:<runtime>:<runtime_ref>`.
   If the session is already busy, the later job records `runtime_lock_wait` and
   remains queued until a worker can retry it.

3. Queue background asks that can use independent managed instances.

   ```sh
   gitmoot agent type set project-planner --runtime codex --template planner --max-background 2 --idle-timeout 20m
   gitmoot daemon start --repo owner/project --workers 2
   gitmoot agent ask project-planner --repo owner/project --background "Say planner A OK."
   gitmoot agent ask project-planner --repo owner/project --background "Say planner B OK."
   gitmoot job list --repo owner/project
   ```

   Expected signal: Gitmoot may create or reuse up to two managed planner
   instances, different runtime references can run concurrently, and
   `gitmoot agent gc` later removes expired idle instances.

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

Only retry failed, blocked, or cancelled jobs. Only cancel queued, running, or
blocked jobs. Use `gitmoot lock release owner/project <branch> --owner <agent>`
for an exact-owner stale lock; use `--force` only when the stored owner is stale.

## Known V1 Limits

- Local-only: the machine running the daemon must stay online.
- Polling watches GitHub; there is no webhook receiver.
- GitHub comments are authored by the authenticated `gh` user, not a bot.
- Agent identity is shown in the comment body.
- There is no hosted dashboard, GitHub App bot identity, cloud runner, billing,
  or remote control plane.
