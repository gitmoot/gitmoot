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

## Thermo Template Smoke Test

Goal: PR comment -> queued review job -> Codex resume with cached thermo template
instructions -> attributed PR result comment. Run this with a Gitmoot build that
includes `gitmoot agent template` commands.

1. Cache the template and start a Gitmoot-managed Codex review agent.

   ```sh
   gitmoot agent template update thermo-nuclear-code-quality-review
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

5. Check or refresh the cached template only through explicit commands.

   ```sh
   gitmoot agent template diff thermo-nuclear-code-quality-review
   gitmoot agent template update thermo-nuclear-code-quality-review
   ```

## Planner Template Smoke Test

Goal: canonical goal template -> cached planner template -> Gitmoot-managed Codex
planner agent. This verifies the planning workflow is discoverable before using
it on a real PR.

1. Build a local test binary and use an isolated Gitmoot home.

   ```sh
   GOTOOLCHAIN=go1.26.0 go build -o /tmp/gitmoot-current ./cmd/gitmoot
   export GITMOOT_SMOKE_HOME=/tmp/gitmoot-planner-template-smoke
   rm -rf "$GITMOOT_SMOKE_HOME"
   /tmp/gitmoot-current init --home "$GITMOOT_SMOKE_HOME"
   ```

2. Confirm the canonical template and planner template are available.

   ```sh
   /tmp/gitmoot-current goal template | grep "codex exec review is clean; ready for manual /review."
   /tmp/gitmoot-current agent template list --home "$GITMOOT_SMOKE_HOME" | grep planner
   /tmp/gitmoot-current agent template update --home "$GITMOOT_SMOKE_HOME" planner
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
     "Write a task-by-task implementation plan for this feature, then create the goal file prompt."
   /tmp/gitmoot-current job list --home "$GITMOOT_SMOKE_HOME" --repo owner/project
   /tmp/gitmoot-current job show <local-ask-job-id> --home "$GITMOOT_SMOKE_HOME"
   ```

5. Open a disposable PR, then comment:

   ```text
   /gitmoot project-planner-smoke ask Write a task-by-task implementation plan for this feature, then create the goal file prompt.
   ```

6. Verify the queued PR job and PR result.

   ```sh
   /tmp/gitmoot-current job list --home "$GITMOOT_SMOKE_HOME" --repo owner/project
   /tmp/gitmoot-current job show <pr-ask-job-id> --home "$GITMOOT_SMOKE_HOME"
   /tmp/gitmoot-current events --home "$GITMOOT_SMOKE_HOME" --repo owner/project
   gh pr view <number> --repo owner/project --comments
   ```

Expected signals:

- `goal template` prints the canonical PR-per-task prompt.
- `agent template show` displays `default role: planner`, `default capabilities: ask`,
  and `mutation: true`.
- `agent doctor project-planner-smoke` succeeds.
- `agent ask project-planner-smoke` prints `state: succeeded`, `agent: project-planner-smoke`,
  `action: ask`, and a planner summary.
- `job show <local-ask-job-id>` includes `"sender": "local"`, the cached
  `planner` template metadata, and the planner result.
- The PR result comment includes `Template: planner`.
- The planner returns a structured plan and, when requested, a
  `GOAL-<short-slug>.md` path plus `/goal GOAL-<short-slug>.md`.

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

2. From the test repo checkout, cache the template and start the agent.

   ```sh
   cd /path/to/project
   /tmp/gitmoot-current agent template update --home "$GITMOOT_SMOKE_HOME" thermo-nuclear-code-quality-review
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

## Custom Prompt Template Smoke Test

Goal: local prompt file -> cached custom template -> template-backed Codex agent ->
queued PR comment job with custom template metadata.

Prerequisites: a safe test repository, authenticated `gh`, installed Codex, and
a Gitmoot build that includes `agent template add`.

1. Build a local test binary and use an isolated Gitmoot home.

   ```sh
   GOTOOLCHAIN=go1.26.0 go build -o /tmp/gitmoot-current ./cmd/gitmoot
   export GITMOOT_SMOKE_HOME=/tmp/gitmoot-custom-template-smoke
   rm -rf "$GITMOOT_SMOKE_HOME"
   /tmp/gitmoot-current init --home "$GITMOOT_SMOKE_HOME"
   ```

2. From the test repo checkout, create and install a local prompt template.

   ```sh
   cd /path/to/project
   mkdir -p agents
   printf '%s\n' 'Review only correctness, regressions, and missing tests.' > agents/local-reviewer.md
   /tmp/gitmoot-current agent template add local-reviewer \
     --home "$GITMOOT_SMOKE_HOME" \
     --file agents/local-reviewer.md \
     --name "Local Reviewer"
   /tmp/gitmoot-current agent template show --home "$GITMOOT_SMOKE_HOME" local-reviewer
   ```

3. Start or subscribe a Codex test agent with the custom template.

   ```sh
   /tmp/gitmoot-current agent start local-reviewer \
     --home "$GITMOOT_SMOKE_HOME" \
     --runtime codex \
     --repo owner/project \
     --path . \
     --template local-reviewer \
     --role reviewer \
     --capability ask \
     --capability review \
     --start-daemon
   /tmp/gitmoot-current agent doctor local-reviewer --home "$GITMOOT_SMOKE_HOME"
   ```

   To register an existing session instead, use:

   ```sh
   /tmp/gitmoot-current agent subscribe local-reviewer \
     --home "$GITMOOT_SMOKE_HOME" \
     --runtime codex \
     --session <session-id-or-last> \
     --repo owner/project \
     --template local-reviewer \
     --role reviewer \
     --capability ask \
     --capability review
   /tmp/gitmoot-current daemon start \
     --home "$GITMOOT_SMOKE_HOME" \
     --repo owner/project \
     --poll 10s
   ```

4. Open a disposable PR, then comment:

   ```text
   /gitmoot local-reviewer review
   ```

5. Verify the job and metadata.

   ```sh
   /tmp/gitmoot-current job list --home "$GITMOOT_SMOKE_HOME" --repo owner/project
   /tmp/gitmoot-current job show <job-id> --home "$GITMOOT_SMOKE_HOME"
   gh pr view <number> --repo owner/project --comments
   ```

Expected signals:

- `agent template show` displays `source: local@file:` and `resolved commit: sha256:...`.
- The PR receives a queued-job acknowledgement for `local-reviewer`.
- The result comment includes `Agent`, `Runtime`, `Template`, and `Job` metadata.
- `job show <job-id>` includes the custom template id and `sha256:` content hash.

6. Edit and refresh the prompt only through explicit template commands.

   ```sh
   printf '%s\n' 'Review correctness, regressions, missing tests, and edge cases.' > agents/local-reviewer.md
   /tmp/gitmoot-current agent template diff --home "$GITMOOT_SMOKE_HOME" local-reviewer
   /tmp/gitmoot-current agent template update --home "$GITMOOT_SMOKE_HOME" local-reviewer
   ```

7. Stop the isolated daemon.

   ```sh
   /tmp/gitmoot-current daemon stop --home "$GITMOOT_SMOKE_HOME"
   /tmp/gitmoot-current daemon status --home "$GITMOOT_SMOKE_HOME"
   ```

## Template Capture Smoke Test

Goal: current-chat template capture semantics -> draft scaffold -> structural
validation -> local template install -> prompt reuse, without requiring a live
background agent or PR comment.

Prerequisites: a Gitmoot build that includes `agent template draft` and
`agent template validate`.

1. Build a local test binary and use an isolated Gitmoot home.

   ```sh
   GOTOOLCHAIN=go1.26.0 go build -o /tmp/gitmoot-current ./cmd/gitmoot
   export GITMOOT_SMOKE_HOME="$(mktemp -d)"
   export GITMOOT_DRAFT_FILE="$GITMOOT_SMOKE_HOME/release-planner.md"
   /tmp/gitmoot-current init --home "$GITMOOT_SMOKE_HOME"
   ```

2. Scaffold a draft file.

   ```sh
   /tmp/gitmoot-current agent template draft release-planner \
     --home "$GITMOOT_SMOKE_HOME" \
     --output "$GITMOOT_DRAFT_FILE"
   ```

3. In a Codex or Claude Code chat with the Gitmoot plugin/skill installed, fill
   that draft from visible current-chat context:

   ```text
   Use Gitmoot to capture this session as agent template release-planner. Draft only.
   ```

4. After reviewing the filled draft, validate, install, and inspect the captured
   template.

   ```sh
   /tmp/gitmoot-current agent template validate "$GITMOOT_DRAFT_FILE"
   /tmp/gitmoot-current agent template add release-planner \
     --home "$GITMOOT_SMOKE_HOME" \
     --file "$GITMOOT_DRAFT_FILE"
   /tmp/gitmoot-current agent template show \
     --home "$GITMOOT_SMOKE_HOME" \
     release-planner
   /tmp/gitmoot-current agent prompt release-planner \
     --home "$GITMOOT_SMOKE_HOME"
   ```

Expected signals:

- `agent template draft` writes `$GITMOOT_DRAFT_FILE` with the standard
  title and required sections.
- The current-chat capture step fills the draft from visible context without
  starting a daemon, queueing a job, or installing the template.
- `agent template validate` succeeds for the draft and reports clear missing
  sections or placeholders if the file is edited into an invalid state.
- `agent template show` displays `source: local@file:` and
  `resolved commit: sha256:...`.
- `agent prompt release-planner` prints the installed template content for
  current-chat reuse.

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
