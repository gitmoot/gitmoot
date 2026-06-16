# Implement Gitmoot Bug Report Flow

Implement GitHub issue #289 task by task. Each task must be developed,
reviewed, opened as its own pull request, merged, and verified before moving on,
unless tasks are explicitly safe to run in parallel.

Issue: https://github.com/jerryfane/gitmoot/issues/289

The goal is to add a CLI-first Gitmoot bug-reporting system that can create
high-quality GitHub issues from failed jobs, dashboard errors, daemon errors,
and SkillOpt/train errors. The same underlying report builder should power both
agent/CLI usage and the dashboard `B report bug` flow.

## Core Rules

- Target repository: `jerryfane/gitmoot`.
- Target base branch: `main`.
- Preserve existing unrelated uncommitted docs/logo changes. If those changes
  make a task commit ambiguous, stop and ask before continuing.
- Work one task at a time in the listed order by default.
- Task 1 must merge first because it defines the shared report-builder
  interface.
- After Task 1 is merged, Tasks 2 and 3 may run in parallel on separate
  branches/worktrees because they should touch mostly disjoint CLI/GitHub and
  dashboard/TUI surfaces.
- Task 4 must start only after Tasks 2 and 3 are merged.
- Do not commit generated reports, caches, build artifacts, secrets,
  credentials, session archives, cloned helper repos, local plugin build output,
  or large outputs unless a task explicitly says they are intended tracked
  fixtures/artifacts.
- Preserve existing behavior unless the current task explicitly changes it.
- Keep changes clean, scoped, and organized. Avoid broad rewrites.
- Avoid code duplication. When repeated logic appears, extract small reusable
  helpers that match existing repo patterns.
- When delegating to Gitmoot agents, use `gitmoot agent run`,
  `gitmoot agent implement`, `gitmoot agent review`, or `gitmoot task run`.
  Do not put branch creation, commit, push, PR creation, or merge instructions
  inside `gitmoot agent ask`; Gitmoot owns repository orchestration.
- If implementation depends on external APIs, docs, CLIs, data formats,
  generated scripts, installers, service launchers, subprocess calls, env vars,
  config formats, or third-party libraries, verify the real contract with local
  commands and/or official sources before editing.

## Before Starting

1. Inspect current repo state:
   - `git status --short`
   - `git branch --show-current`
   - `git remote -v`
2. Confirm the current checkout is `jerryfane/gitmoot` on `main`, or stop and
   ask before continuing.
3. If unrelated local changes overlap a task's files, stop and ask before
   committing.
4. Verify PR tooling before the first PR:
   - `gh auth status`
   - `gh repo view jerryfane/gitmoot`
5. Pull/update `main` before creating each task branch.

## Per-Task Branch Workflow

1. Confirm the current task's scope.
2. Create a task branch from latest `main`.
3. Implement only that task.
4. Add or update focused tests/checks appropriate to the task.
5. Run focused tests for touched modules.
6. Run broader checks when the task touches shared behavior, CLI/API surfaces,
   GitHub integration, TUI workflows, docs build systems, or user-facing
   behavior.
7. For CLI, subprocess, GitHub, or dashboard changes, include an operational
   smoke test or direct contract check. Syntax checks alone are not enough.
8. Run `git diff --check`.
9. Run `codex exec review --uncommitted` in every repo with uncommitted
   changes.
10. Preserve the exact raw final review output for the PR body.

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
6. Re-run focused tests/checks and `codex exec review --uncommitted`.
7. Repeat until the final raw review output contains no findings, or stop if
   blocked or if a finding is incorrect after verification.

## Commit Gate

1. Inspect the final diff.
2. Commit only the current task's intended tracked changes.
3. Use the suggested commit message unless there is a clearer task-specific
   alternative.
4. Push the task branch.
5. Verify the task branch worktree is clean after push, except for intentionally
   ignored generated files.

## Pull Request Gate

1. Create one PR for the current task.
2. The PR title must describe only the current task.
3. The PR body must include:
   - WHAT: what changed
   - WHY: why the task was needed
   - CHANGES: concrete implementation changes
   - RESULTS: tests/checks/review results
   - RISK: skipped checks, blockers, or residual risk
4. Include the exact raw final `codex exec review --uncommitted` output in the
   PR body.
5. If CI or required checks exist, wait for them and fix failures before merge.
6. Merge using the repository's configured/preferred merge method. If no
   preference is discoverable, use squash merge for a clean task-level history.
7. After merge, update local `main` and verify the worktree is clean except for
   unrelated pre-existing changes.
8. Record the PR number, PR URL, branch name, and merged commit hash.
9. Delete the task branch after merge only if the repository normally does so or
   the merge command supports safe branch deletion.

## Parallel Task Rules

- Parallelize only when tasks are independent, have disjoint file ownership, and
  can be reviewed and merged without order-dependent assumptions.
- Use one branch/worktree per task.
- Clearly assign each branch a task number and file ownership.
- Do not duplicate work across branches.
- For Gitmoot-owned task PRs, let the merge gate update stale branches and
  retry. If a real content conflict remains, resolve it in an explicit fix task
  and re-run checks/review.
- If Task 2 and Task 3 both need a shared interface adjustment after Task 1,
  stop parallel work, make the shared adjustment in a small follow-up PR, merge
  it, then rebase/update both task branches.

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

### Task 1: Core Report Builder And Redaction

Implement the shared report engine only. This task must merge before any CLI or
dashboard integration work starts.

Scope:

- Add an internal report builder package that returns a structured bug report:
  title, markdown body, labels, fingerprint, source metadata, and redaction
  summary.
- Support job-source reports first:
  - load job by id
  - parse payload
  - include repo, agent, runtime if available, action, state, task, PR, result
    summary, result decision, and recent events
  - include enough request context to debug without exposing unnecessary raw
    runtime output
- Use the marker:

  ```md
  <!-- gitmoot:dashboard-report fingerprint:<hash> -->
  ```

- Reuse existing redaction behavior where possible:
  - `workflow.RedactCommentText`
  - comment truncation limits/patterns
  - `/gitmoot` command neutralization
- Omit raw runtime output by default. If any raw output excerpt is included, it
  must be redacted and truncated.
- Return labels `gitmoot-dashboard-report` and `bug` from the report builder.
- Do not create GitHub issues in this task.

Acceptance criteria:

- A failed/blocked/cancelled job can produce a markdown report.
- The report includes environment, selected error, job context, recent events,
  and redaction notes.
- Secrets such as GitHub tokens, API keys, passwords, Claude/OAuth/auth tokens,
  and AWS credentials are redacted.
- Fingerprint is stable for the same source/error and changes when the source
  job/error changes materially.

Tests/checks:

- Focused tests for report builder formatting.
- Redaction tests with GitHub token, API key, password, AWS key, and auth token
  samples.
- Truncation/omission test for long raw output.
- `go test ./internal/...` or a narrower command if package boundaries make
  that cheaper and sufficient.
- `git diff --check`.
- `codex exec review --uncommitted`.

Suggested branch:

```text
issue-289-report-builder
```

Suggested commit:

```text
feat: add bug report builder
```

Dedicated task prompt:

```text
/goal Read /root/gitmoot/GOAL-issue-289-bug-report-flow.md. Execute Task 1 only: Core Report Builder And Redaction. Use a separate branch/worktree from latest main, preserve unrelated changes, implement only the shared builder/redaction layer, run focused tests, run codex exec review --uncommitted, open one PR, and stop after the PR is merged and verified.
```

### Task 2: CLI And GitHub Issue Creation

Add the `gitmoot report bug` CLI using Task 1's report builder.

Scope:

- Add a top-level `report` CLI command with `bug` subcommand.
- Implement:

  ```sh
  gitmoot report bug --job <job-id> --preview
  gitmoot report bug --job <job-id> --create --yes
  gitmoot report bug --source daemon --preview
  gitmoot report bug --source dashboard --preview
  gitmoot report bug --train <session-id> --create --yes
  ```

- For MVP, `--job` must be fully functional.
- If daemon/dashboard/train report sources cannot be implemented safely in this
  task, keep the CLI shape and return clear unsupported-source errors that name
  the future source.
- Default behavior is preview if neither `--preview` nor `--create` is given.
- Non-interactive issue creation requires `--create --yes`.
- Extend `github.CreateIssueInput` with `Labels []string`.
- Apply labels from the report builder. Generated reports should use:
  - `gitmoot-dashboard-report`
  - `bug`
- Best-effort label handling:
  - try to create/apply `gitmoot-dashboard-report` if missing
  - if label creation/application fails, still create the issue
  - always include the marker in the body so reports remain searchable
- Best-effort duplicate detection:
  - search open `jerryfane/gitmoot` issues for the fingerprint marker
  - if a match exists, print the existing issue URL instead of creating a
    duplicate

Acceptance criteria:

- Preview prints redacted markdown and does not call GitHub issue creation.
- Create creates an issue and prints the URL.
- Missing/generated label failures do not block issue creation.
- Duplicate fingerprint returns the existing issue URL.
- CLI usage/help is clear enough for agents to call correctly.

Tests/checks:

- CLI preview test.
- CLI create test using fake GitHub client/runner.
- `--create` without `--yes` failure test.
- label fallback test.
- duplicate fingerprint test.
- GitHub client label argument test.
- `go test ./internal/cli ./internal/github ./internal/report` or equivalent.
- `git diff --check`.
- `codex exec review --uncommitted`.

Suggested branch:

```text
issue-289-report-cli
```

Suggested commit:

```text
feat: add bug report CLI
```

Dedicated task prompt:

```text
/goal Read /root/gitmoot/GOAL-issue-289-bug-report-flow.md. Execute Task 2 only after Task 1 is merged: CLI And GitHub Issue Creation. Use a separate branch/worktree from latest main, implement the CLI and GitHub issue integration only, run focused and relevant broader tests, run codex exec review --uncommitted, open one PR, and stop after the PR is merged and verified.
```

### Task 3: Dashboard TUI Report Action

Add the dashboard `B report bug` preview/create flow using the shared report
builder.

Scope:

- Add a dashboard TUI mode such as `modeBugReportPreview`.
- Add TUI deps for:
  - building a bug report preview for a selected source
  - creating a bug report issue for a selected source
- Show `B report bug` only when the selected item is reportable:
  - failed job
  - blocked job
  - cancelled job
  - visible dashboard refresh/action error if already modeled as selectable
- For MVP, failed/blocked/cancelled jobs must be fully functional.
- Preview panel is read-only.
- `g` creates the GitHub issue.
- `esc` cancels and returns to the prior dashboard view.
- Successful creation shows the issue URL.
- Creation errors stay inline without closing the preview.
- Reuse existing job detail/event loading and modal action patterns.
- Do not duplicate report-building logic in the TUI layer.

Acceptance criteria:

- `B report bug` appears only for reportable selected items.
- Pressing `B` opens a redacted preview for the selected job.
- Pressing `g` creates an issue through the dep and shows the URL.
- Failed creation keeps preview open with an inline error.
- Existing retry/cancel/detail flows continue to work.

Tests/checks:

- TUI key hint visibility tests.
- TUI preview render test.
- TUI create-success test.
- TUI create-error test.
- Regression tests for retry/cancel overlays if touched.
- `go test ./internal/cli/tui ./internal/cli` or equivalent.
- `git diff --check`.
- `codex exec review --uncommitted`.

Suggested branch:

```text
issue-289-dashboard-report-action
```

Suggested commit:

```text
feat: add dashboard bug report action
```

Dedicated task prompt:

```text
/goal Read /root/gitmoot/GOAL-issue-289-bug-report-flow.md. Execute Task 3 only after Task 1 is merged: Dashboard TUI Report Action. Use a separate branch/worktree from latest main, avoid touching CLI code except shared interfaces, run TUI-focused tests, run codex exec review --uncommitted, open one PR, and stop after the PR is merged and verified.
```

### Task 4: Docs, Skill Guidance, Integration, Final Review

Document the feature and verify the end-to-end behavior after Tasks 1-3 merge.

Scope:

- Update CLI reference docs with `gitmoot report bug`.
- Update dashboard docs/release notes if appropriate.
- Update Gitmoot skill guidance:
  - agents preview by default
  - agents create reports only when the user explicitly asks or policy allows
  - agents report the issue URL back to the user after creation
- Add integration smoke coverage around CLI preview/create with fake GitHub.
- Verify the final behavior and issue #289 acceptance criteria.

Acceptance criteria:

- Docs clearly explain dashboard and CLI/agent flows.
- Agents have explicit guidance for preview vs create.
- Full expected command set is documented.
- End-to-end tests demonstrate report preview and create behavior.
- Issue #289 can be closed after final PR merge.

Tests/checks:

- Focused Go tests for report/CLI/TUI packages.
- `go test ./...`.
- `cd website && npm run build` if website docs changed.
- `git diff --check`.
- `codex exec review --uncommitted`.

Suggested branch:

```text
issue-289-report-docs-integration
```

Suggested commit:

```text
docs: document bug report workflow
```

Dedicated task prompt:

```text
/goal Read /root/gitmoot/GOAL-issue-289-bug-report-flow.md. Execute Task 4 only after Tasks 2 and 3 are merged: Docs, Skill Guidance, Integration, Final Review. Update docs/skill guidance, run full relevant checks, run codex exec review --uncommitted, open the final PR, merge when clean, and summarize all task PRs.
```

## Coordinator Prompt

Use this prompt to start the coordinator:

```text
/goal Read and execute /root/gitmoot/GOAL-issue-289-bug-report-flow.md for jerryfane/gitmoot issue #289. Coordinate the work task-by-task. Start Task 1 first. After Task 1 is merged, dispatch Tasks 2 and 3 concurrently on separate branches/worktrees. Start Task 4 only after Tasks 2 and 3 are merged. For every task, implement only that task, run focused tests and required broader checks, run codex exec review --uncommitted, fix until clean, open one PR, wait for checks, merge, update main, and record branch/PR/merge hash. Preserve existing unrelated uncommitted docs/logo changes and stop if they make task commits ambiguous.
```
