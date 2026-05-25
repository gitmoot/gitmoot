# Gitmoot Planner Preset Agent Goal

Implement the Gitmoot planner preset agent task by task. Each task must be
developed, reviewed, opened as its own pull request, merged, and verified before
moving on, unless tasks are explicitly safe to run in parallel.

This goal adds a built-in Gitmoot preset agent named `gitmoot-plan-and-goal`
that turns vague feature requests into structured implementation plans and,
when asked, writes a standard PR-oriented goal file. Reuse Gitmoot's existing
preset system, plugin skill packaging, and goal workflow. Do not add MCP or a
separate planner runtime in this goal.

## Core Rules

- Work one task at a time in the listed order by default.
- If tasks are independent, have disjoint file ownership, and do not depend on
  each other's results, they may be done in parallel on separate branches.
- Do not start dependent work until the prerequisite task has passed checks,
  passed `codex exec review --uncommitted`, been pushed, opened as a PR, merged,
  and verified on the target branch.
- Do not commit generated data, reports, caches, build artifacts, secrets,
  credentials, session archives, cloned helper repos, local plugin build output,
  or large outputs unless the plan explicitly says they are intended tracked
  fixtures/artifacts.
- Preserve existing behavior unless the current task explicitly changes it.
- Keep changes clean, scoped, and organized. Avoid broad rewrites.
- Avoid code duplication. When repeated logic appears, extract small reusable
  helpers that match existing repo patterns.
- If implementation depends on external APIs, docs, CLIs, data formats,
  generated scripts, installers, service launchers, subprocess calls, env vars,
  config formats, or third-party libraries, verify the real contract with local
  commands and/or official sources before editing.
- Prefer official/primary sources for technical contracts.
- Keep the long standard goal template in one canonical source. Do not duplicate
  that text across CLI code, README, preset instructions, and docs.
- Runtime-specific behavior must remain behind clear runtime/plugin/preset
  boundaries. Do not hardcode planner behavior into GitHub, daemon, database, or
  merge-gate packages beyond the existing preset/job prompt integration.
- GitHub PR comments remain the public audit trail. Local SQLite state remains
  the workflow source of truth.

## Before Starting

1. Inspect current repo state with:
   - `git status --short`
   - current branch
   - current remote
2. If the target branch is unclear, the remote looks wrong, or the worktree has
   unrelated existing changes that make task commits ambiguous, stop and ask
   before continuing.
3. Confirm the target base branch from the current repo. If unspecified, use the
   current branch as the base.
4. Inspect relevant existing patterns before editing:
   - built-in preset definitions in `internal/preset`
   - preset CLI behavior in `internal/cli/preset.go`
   - goal CLI behavior in `internal/cli/workflow.go`
   - job/startup prompt rendering in `internal/prompts` and `internal/cli/agent.go`
   - canonical skill package in `skills/gitmoot`
   - plugin packaging in `internal/pluginpack`
5. Verify PR tooling is available before the first PR:
   - `gh auth status`
   - repo remote resolves to the expected GitHub repository
6. Verify relevant external contracts before editing:
   - Codex plugin/skill docs:
     `https://developers.openai.com/codex/plugins`
   - Codex plugin build docs:
     `https://developers.openai.com/codex/plugins/build`
   - Claude Code skill/plugin docs:
     `https://code.claude.com/docs/en/skills`
     `https://code.claude.com/docs/en/plugins`
   - Agent Skills specification:
     `https://agentskills.io/specification`
   - Cursor Team Kit marketplace pattern:
     `https://cursor.com/marketplace/cursor/cursor-team-kit`

## Per-Task Branch Workflow

1. Confirm the current task's scope.
2. Create a task branch from the latest target base branch.
3. Implement only that task.
4. Add or update focused tests/checks appropriate to the task.
5. Run focused tests for touched modules.
6. Run broader checks when the task touches shared behavior, CLI/API surfaces,
   config paths, subprocess calls, installer behavior, docs, plugins, presets,
   or user-facing workflows.
7. For CLI, generated-package, external-tool, or plugin validation changes,
   include an operational smoke test or direct contract check. Syntax checks
   alone are not enough.
8. Identify every repository where files changed. In each changed repo, run:
   `codex exec review --uncommitted`
9. Preserve the exact raw review output per repo.

## Review-Fix Loop

1. If review finds issues, do not only patch the literal line.
2. Identify the underlying invariant/class of bug.
3. Audit nearby and sibling paths for the same issue.
4. Write a concise fix plan using:

   ```text
   Review found these issues: <<PASTE RAW REVIEW RESULTS BY REPO>>.
   For each issue, identify the underlying invariant/class of bug, audit sibling
   paths for the same issue, and plan the smallest safe fix. Verify external
   assumptions with local commands and/or official sources. Preserve repo
   patterns, avoid unnecessary refactors, and list tests/checks per repo.
   ```

5. Execute the fix plan.
6. Re-run focused tests/checks and `codex exec review --uncommitted` in every
   repo with uncommitted changes.
7. Repeat until the final raw review output contains no findings, or stop if
   blocked or if a finding is incorrect after verification.

## Commit Gate

1. Before committing, run `git diff --check` and inspect the final diff.
2. Commit only the current task's intended tracked changes.
3. Use the commit message specified by the task. If the task does not specify
   one, use a concise conventional message that describes only the current task.
4. Push the task branch.
5. Verify the task branch worktree is clean after push, except for intentionally
   ignored generated files.

## Pull Request Gate

1. Create one PR for the current task.
2. The PR title must describe only the current task.
3. The PR body must include:
   - WHAT: what was changed
   - WHY: why the task was needed
   - CHANGES: concrete implementation changes
   - RESULTS: tests/checks/review results
   - RISK: skipped checks, blockers, or residual risk
4. Include the exact raw final `codex exec review --uncommitted` output for each
   changed repo in the PR body.
5. If CI or required checks exist, wait for them and fix failures before merge.
6. Merge the PR using the repository's configured/preferred merge method. If no
   preference is discoverable, use squash merge for a clean task-level history.
7. After merge, update the local target base branch and verify the worktree is
   clean.
8. Record the PR number, PR URL, branch name, and merged commit hash.
9. Delete the task branch after merge only if the repository normally does so or
   the merge command supports safe branch deletion.

## Parallel Task Rules

- Parallelize only when tasks are independent, have disjoint write sets, and can
  be reviewed and merged without order-dependent assumptions.
- Use a separate branch per task.
- Clearly assign each branch a task number and file ownership.
- Do not duplicate work across branches.
- If parallel branches conflict after one PR merges, rebase or update the
  remaining branch on the latest target base and re-run its checks/review.
- If a task becomes dependent on another task, stop treating it as parallel and
  merge the dependency first.

## Final Response After All Tasks

- List completed tasks.
- For each task, list branch, PR URL, merge status, and merged commit hash.
- List tests/checks run.
- Include exact final raw `codex exec review --uncommitted` output for the last
  task/repo.
- Mention skipped checks, blockers, or residual risk.
- Do not claim interactive `/review` is clean. Say:
  `codex exec review is clean; ready for manual /review.`

## Implementation Tasks

### Task 1: Add Canonical Goal Template

Add one canonical standard goal template under the Gitmoot skill package, then
expose it through the CLI.

Details:
- Add `skills/gitmoot/references/GOAL_TEMPLATE.md`.
- Put the full standard goal best-practices prompt there, including PR-per-task
  workflow, review loop, commit gate, PR gate, parallel task rules, and final
  response contract.
- Add `gitmoot goal template`, which prints that exact embedded template from
  `skills.FS`.
- Update `gitmoot goal` usage to show:
  - `gitmoot goal template`
  - `gitmoot goal import --file <path> [--repo owner/repo]`
- Keep the template as the single source of truth. Do not duplicate the long
  text in CLI code, README, or preset instructions.
- Preserve existing `gitmoot goal import` behavior.

Acceptance:
- `gitmoot goal template` prints the canonical template.
- Template contains the phrase:
  `codex exec review is clean; ready for manual /review.`
- Existing `goal import` tests continue to pass.

Suggested commit message:
- `feat: add canonical goal template command`

### Task 2: Add Built-In Planner Preset

Add a built-in planner preset definition and prompt file.

Details:
- Add preset ID: `gitmoot-plan-and-goal`.
- Display name: `Gitmoot Plan and Goal Writer`.
- Default role: `planner`.
- Default capabilities: `ask`.
- Set `Mutation: true` so users may explicitly add `implement` when they want
  the planner agent to write goal files or make repo changes.
- Built-in source:
  - repo: `jerryfane/gitmoot`
  - ref: `main`
  - path: `skills/gitmoot/presets/gitmoot-plan-and-goal.md`
- Add the preset prompt file at that path.

Preset prompt requirements:
- Inspect repo state before planning.
- Use web search for current external contracts, APIs, CLIs, framework
  behavior, package docs, or best-practice claims.
- Prefer official/primary sources when researching technical contracts.
- Produce a clean plan split into tasks with clear PR boundaries.
- Make the plan decision-complete enough for another engineer or agent to
  implement without guessing.
- Avoid broad rewrites and duplication; explicitly call out reusable helpers
  where appropriate.
- Ask clarifying questions only for high-impact product decisions that cannot
  be discovered.
- When asked for a goal file:
  - use `gitmoot goal template` as the standard template source;
  - write `GOAL-<short-slug>.md`;
  - include the plan tasks in `### Task N: ...` headings so
    `gitmoot goal import` can parse them;
  - return the exact prompt: `/goal GOAL-<short-slug>.md`.
- Do not implement the planned feature unless explicitly asked after the goal is
  created.

Acceptance:
- `gitmoot preset list` shows `gitmoot-plan-and-goal`.
- `gitmoot preset show gitmoot-plan-and-goal` displays role, capabilities,
  mutation flag, and source.
- `gitmoot preset update gitmoot-plan-and-goal` caches the prompt.
- `gitmoot agent start planner --runtime codex --repo owner/repo --preset gitmoot-plan-and-goal`
  uses role `planner` and capability `ask`.
- `gitmoot agent start ... --preset gitmoot-plan-and-goal --capability implement`
  is allowed because the preset is mutating-capable.

Suggested commit message:
- `feat: add planner preset agent`

### Task 3: Wire Planner Into Agent And Skill Docs

Update agent-facing docs so humans and agents know when to use the planner.

Details:
- Update `skills/gitmoot/SKILL.md` to mention structured planning and goal-file
  workflows.
- Update `skills/gitmoot/references/CLI.md` with:
  - `gitmoot goal template`
  - `gitmoot preset update gitmoot-plan-and-goal`
  - planner agent startup examples.
- Update `skills/gitmoot/references/WORKFLOWS.md` with a concise planner
  workflow:
  - install/update preset;
  - start or subscribe planner;
  - ask for plan;
  - ask for goal file;
  - import goal if desired with
    `gitmoot goal import --file GOAL-... --repo owner/repo`.
- Keep docs short and command-focused.

Usage examples to include:

```sh
gitmoot preset update gitmoot-plan-and-goal

gitmoot agent start planner \
  --runtime codex \
  --repo owner/repo \
  --path . \
  --preset gitmoot-plan-and-goal \
  --start-daemon
```

PR comment example:

```text
/gitmoot planner ask Write a task-by-task implementation plan for this feature, then create the goal file prompt.
```

Direct plugin example:

```text
$gitmoot:gitmoot Use the planner workflow to create a plan and goal file for this feature: ...
```

Acceptance:
- Agent-facing skill docs mention when to use the planner.
- CLI docs include exact commands.
- Workflow docs include PR-comment and plugin invocation examples.

Suggested commit message:
- `docs: document planner preset workflow`

### Task 4: Update README And Smoke Docs

Expose the feature in user-facing docs without making the README too long.

Details:
- Add a short README section named `Planner Preset`.
- Add the canonical command sequence.
- Add a beta smoke-test section that verifies:
  - `gitmoot goal template`;
  - `gitmoot preset update gitmoot-plan-and-goal`;
  - `gitmoot preset show gitmoot-plan-and-goal`;
  - starting a planner agent with default capabilities;
  - optional prompt-render smoke that confirms preset content appears in
    startup/job prompts.
- Do not duplicate the full goal template in README or smoke docs.

Acceptance:
- A new user can discover the planner from README.
- Smoke docs give exact commands and expected success signals.
- No long goal template text is duplicated in README.

Suggested commit message:
- `docs: add planner preset smoke guidance`

### Task 5: Add Tests And Verification Coverage

Add focused tests around the new preset and goal template.

Details:
- `internal/cli`:
  - `goal template` prints the embedded template.
  - `goal import` still parses `### Task N:` headings unchanged.
  - `preset list/show/update` handle `gitmoot-plan-and-goal`.
  - `agent start` resolves planner defaults.
  - `agent start --preset gitmoot-plan-and-goal --capability implement` is
    allowed.
- `internal/preset`:
  - built-in lookup includes both `thermo-nuclear-code-quality-review` and
    `gitmoot-plan-and-goal`.
  - fake fetcher can update the planner preset from the configured source path.
- `internal/skill` and/or `internal/pluginpack`:
  - canonical skill references `GOAL_TEMPLATE.md`.
  - generated Codex/Claude plugin packages include the new reference and
    planner preset files.

Required checks:

```sh
/root/temp/ts/tool/go test ./internal/cli ./internal/preset ./internal/skill ./internal/pluginpack
/root/temp/ts/tool/go test ./...
/root/temp/ts/tool/go vet ./...
git diff --check
```

Acceptance:
- All focused and broad checks pass.
- Final `codex exec review --uncommitted` output contains no findings.

Suggested commit message:
- `test: cover planner preset workflow`
