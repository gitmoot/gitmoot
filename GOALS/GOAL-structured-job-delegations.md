# Structured Job Delegations for Internal Multi-Agent Coordination

Implement GitHub issue #301: a first-class job delegation primitive so one
Gitmoot agent can dispatch parallel tasks to other agents, share focused context
through durable artifacts, and continue coordination through explicit
continuation jobs. This keeps multi-agent coordination entirely inside Gitmoot
(no external tools) and uses the existing job system as the coordination
substrate.

This goal also removes the old agent-list result field cleanly, as agreed in
https://github.com/jerryfane/gitmoot/issues/301#issuecomment-4717451503.

## Core Rules

- Work one task at a time in the listed order by default.
- If tasks are independent, have disjoint file ownership, and do not depend on
  each other's results, they may be done in parallel on separate branches and
  worktrees.
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
- When delegating to Gitmoot agents, use `gitmoot agent run`,
  `gitmoot agent implement`, `gitmoot agent review`, or `gitmoot task run`.
  Do not put branch creation, commit, push, PR creation, or merge instructions
  inside `gitmoot agent ask`; Gitmoot owns repository orchestration.
- If implementation depends on external APIs, docs, CLIs, data formats,
  generated scripts, installers, service launchers, subprocess calls, env vars,
  config formats, or third-party libraries, verify the real contract with local
  commands and/or official sources before editing.
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
3. Confirm the target base branch from the current repo. If unspecified, use
   `main`.
4. Inspect relevant existing patterns before editing:
   - `internal/workflow/result.go`
   - `internal/workflow/engine.go`
   - `internal/workflow/mailbox.go`
   - `internal/workflow/worktree.go`
   - `internal/db/store.go`
   - `internal/prompts/prompts.go`
   - `internal/cli/agent_dispatch.go`
   - `internal/cli/dashboard.go`
   - `skills/gitmoot/agent-templates/planner.md`
   - `skills/gitmoot/references/RESULT_CONTRACT.md`
   - `docs/beta-smoke-tests.md`
5. Verify PR tooling is available before the first PR:
   - `gh auth status`
   - repo remote resolves to the expected GitHub repository
6. Re-read issue #301 and the removal comment before implementation:
   - `gh issue view 301 --comments`

## Per-Task Branch Workflow

1. Confirm the current task's scope.
2. Create a task branch from the latest target base branch.
3. Implement only that task.
4. Add or update focused tests/checks appropriate to the task.
5. Run focused tests for touched modules.
6. Run broader checks when the task touches shared behavior, CLI/API surfaces,
   data/model/evaluation logic, generated scripts, installers, service
   launchers, docs build systems, or user-facing workflows.
7. For wrapper, installer, CLI, subprocess, generated-script, env propagation,
   service-launcher, or deployment changes, include an operational smoke test or
   direct contract check. Syntax checks alone are not enough.
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
- For Gitmoot-owned task PRs, let the merge gate update stale branches and
  retry. If a real content conflict remains, resolve it in an explicit fix task
  and re-run checks/review.
- If a task becomes dependent on another task, stop treating it as parallel and
  merge the dependency first.

## Execution Order

- **Task 3051** must merge first; every other task depends on the new schema.
- **Tasks 3052 and 3053** must merge before Tasks 3054-3058 because they define how
  delegations are parsed and dispatched.
- **Tasks 3054-3058** are the parallel block and can run concurrently on separate
  branches/worktrees.
- **Task 3059** runs after the core primitive is merged and stable.

For each task in the parallel block, assign a dedicated agent with the prompt:

```text
/goal GOALS/GOAL-structured-job-delegations.md
```

and tell the agent which task it owns. Each agent creates its own branch and
worktree, runs tests/review, pushes, opens a PR, and reports back.

## Final Response After All Tasks

- List completed tasks.
- For each task, list branch, PR URL, merge status, and merged commit hash.
- List tests/checks run.
- Include exact final raw `codex exec review --uncommitted` output for the last
  task/repo.
- Mention skipped checks, blockers, or residual risk.
- Do not claim interactive `/review` is clean. Say:
  `codex exec review is clean; ready for manual /review.`
- Update issue #301 with a summary and PR links.

## Implementation Tasks

### Task 3051: Schema migration — introduce structured delegations

**Scope:**

- Define a new `Delegation` struct in `internal/workflow/result.go`:

  ```go
  type Delegation struct {
      ID            string   `json:"id"`
      Agent         string   `json:"agent"`
      Action        string   `json:"action"`
      Worktree      string   `json:"worktree,omitempty"`
      Prompt        string   `json:"prompt"`
      Artifacts     []string `json:"artifacts,omitempty"`
      Deps          []string `json:"deps,omitempty"`
      Timeout       string   `json:"timeout,omitempty"`
      Retry         int      `json:"retry,omitempty"`
      FailurePolicy string   `json:"failure_policy,omitempty"`
      Fingerprint   string   `json:"fingerprint,omitempty"`
      SynthesisRule string   `json:"synthesis_rule,omitempty"`
  }
  ```

- Replace the old string-list handoff field in `AgentResult` with `Delegations []Delegation`.
- Update `normalizeAgentResult` to default `Delegations` to `[]Delegation{}`.
- Add job DAG fields to `JobRequest` and `JobPayload` in
  `internal/workflow/mailbox.go`:
  - `ParentJobID string`
  - `DelegationID string`
  - `DelegationDepth int`
  - `DelegatedBy string`
- Add SQLite migration in `internal/db/store.go`:
  - `parent_job_id TEXT`
  - `delegation_id TEXT`
  - `delegation_depth INTEGER DEFAULT 0`
  - indexes on `(parent_job_id)` and `(delegation_id)`
- Update `db.Job` struct and `Store.CreateJobWithEvent` / `GetJob` paths to
  persist the new fields.
- Remove the old agent-list field from the schema and data model.

**Acceptance criteria:**

- `go test ./internal/workflow ./internal/db` passes.
- a tracked-file grep for the old agent-list field returns nothing.

**Suggested commit message:**

```text
feat(workflow): add structured delegations schema
```

### Task 3052: Parse delegations and update the result contract

**Scope:**

- Update `ExtractAgentResult` in `internal/workflow/result.go` to parse
  `delegations`.
- Update `validateAgentResult` and `normalizeAgentResult` for the new field.
- Update `internal/prompts/prompts.go` `RenderJob` and `RenderRepairPrompt` to
  show a `delegations` example:

  ```json
  {"gitmoot_result":{"decision":"approved|changes_requested|blocked|implemented|failed","summary":"...","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}
  ```

- Update `skills/gitmoot/references/RESULT_CONTRACT.md` with the new field and
  delegation example.
- Update `internal/cli/agent_dispatch.go` `printLocalAgentJobOutput` to print
  delegation agent names.
- Update tests in `internal/workflow/result_test.go` and
  `internal/cli/agent_test.go` that assert on result fields.

**Acceptance criteria:**

- `go test ./internal/workflow ./internal/cli ./internal/prompts` passes.
- a tracked-file grep for the old result field returns no matches.

**Suggested commit message:**

```text
feat(workflow): parse delegations and update result contract
```

### Task 3053: Core delegation dispatcher

**Scope:**

- Delete the old string-list delegation dispatcher in `internal/workflow/engine.go`.
- Add `dispatchDelegations(ctx, payload, result, ref) error` that:
  - Iterates over `result.Delegations`.
  - Builds a `JobRequest` for each delegation with inherited repo/branch/PR
    context, `ParentJobID`, `DelegationID`, `DelegationDepth`, and the
    delegation-specific `Agent`, `Action`, `Instructions`.
  - Runs `ensureAgentAllowed` for each request.
  - Enqueues ready delegations (those with no `deps`) in parallel using
    goroutines and returns a combined error.
- Store a `delegation_enqueued` job event per child job.
- Update `Engine.AdvanceJob` to call `dispatchDelegations`.
- Add engine tests for single delegation, multiple parallel delegations,
  capability checks, branch-lock acquisition, and unknown-agent blocking.

**Acceptance criteria:**

- `go test ./internal/workflow -run Delegation` passes.
- `codex exec review --uncommitted` is clean.

**Suggested commit message:**

```text
feat(workflow): dispatch delegations from agent results
```

### Task 3054: Worktree allocation for delegated implement jobs

**Scope:**

- Extend `internal/workflow/worktree.go` with
  `AllocateDelegationWorktree(ctx, home, repo, parentJobID, delegationID, branchHint, baseBranch, checkout, owner)`.
- Worktree path:
  `$GITMOOT_HOME/worktrees/<owner>--<repo>/delegations/<parent-job-id>/<delegation-id>/`
- Branch naming:
  - Use `delegation.Worktree` if provided, slugged.
  - Otherwise `gitmoot-delegation-<parent-short>-<delegation-id>`.
- Use the existing checkout mutation lock.
- Store `WorktreePath` and `Branch` in the child `JobPayload`.
- Wire the allocation helper into the dispatcher for `action: implement`.
- Add tests for command construction, deterministic paths, and lock usage.

**Acceptance criteria:**

- Two delegated implement jobs from the same parent get separate worktrees.
- `go test ./internal/workflow` passes.

**Suggested commit message:**

```text
feat(workflow): allocate per-delegation worktrees
```

### Task 3055: Artifact writer and child prompt reader

**Scope:**

- When a coordinator job returns `delegations` with non-empty `artifacts`,
  write an artifact directory for the parent job:
  - `.gitmoot/delegations/<parent-job-id>/brief.md`
  - `.gitmoot/delegations/<parent-job-id>/context-manifest.json`
- Add an `ArtifactBody string` field to `AgentResult` so the coordinator can
  return the brief inline; Gitmoot writes it to disk.
- `context-manifest.json` contains `parent_job_id` and the delegations array
  with `id`, `agent`, `action`, `worktree`, `deps`.
- Update `internal/prompts/prompts.go` to include a `Delegation artifacts:`
  section and the child-specific delegation prompt.
- Add tests for artifact creation and child prompt content.

**Acceptance criteria:**

- A coordinator returning `delegations` produces a readable brief file.
- Child job prompts reference the artifacts.
- `go test ./internal/workflow ./internal/prompts` passes.

**Suggested commit message:**

```text
feat(workflow): write delegation artifacts and read them into child prompts
```

### Task 3056: Dependency graph and continuation jobs

**Scope:**

- Track `deps` per child job in `JobPayload`.
- Add `Engine.advanceDelegations(ctx, parentPayload, parentResult, ref)` that:
  - Records pending deps after dispatching ready delegations.
  - On child completion, checks whether all sibling deps for a dependent
    `delegation_id` are satisfied and enqueues it when ready.
- Enqueue a coordinator continuation job when all top-level delegations of a
  parent finish. Inline child results (job id, agent, decision, summary, PR
  links) in the continuation prompt.
- Failure handling:
  - `block_parent` (default): mark parent task blocked.
  - `continue`: still enqueue deps that do not depend on the failed job.
  - `escalate`: enqueue a continuation job immediately with failure details.
- Add tests for deps ordering, continuation enqueue, and failure policies.

**Acceptance criteria:**

- A delegation with `deps: ["api","ui"]` runs only after both deps succeed.
- A coordinator continuation job is created automatically.
- `go test ./internal/workflow` passes.

**Suggested commit message:**

```text
feat(workflow): delegation deps and coordinator continuation jobs
```

### Task 3057: Dashboard DAG rendering

**Scope:**

- Update `internal/cli/dashboard.go` (or dashboard TUI model) to show job
  parent/child relationships.
- Add a delegations view/job-detail pane rendering:
  - parent job id
  - child jobs with state, agent, action
  - deps satisfied/pending
  - continuation job if any
- Use existing `ListJobs` filtered by `ParentJobID`.
- Add tests for tree rendering.

**Acceptance criteria:**

- `gitmoot status --repo owner/repo` or dashboard shows a delegation tree.
- `go test ./internal/cli` passes.

**Suggested commit message:**

```text
feat(cli): render delegation DAG in dashboard
```

### Task 3058: Docs, templates, and E2E smoke

**Scope:**

- Update `skills/gitmoot/agent-templates/planner.md` to document `delegations`
  and provide a coordinator example.
- Update `skills/gitmoot/references/WORKFLOWS.md` with a multi-model delegation
  example.
- Update `docs/beta-smoke-tests.md` with a smoke test using local shell agents:
  - coordinator returns two delegations
  - workers return results
  - continuation job is enqueued
- Update any remaining docs that mention the old result field.

**Acceptance criteria:**

- Docs are consistent and link checks pass.
- Smoke test runs manually and passes.
- a tracked-file grep for the old result field returns no matches except release
  notes if deliberately retained.

**Suggested commit message:**

```text
docs: document delegations workflow and add smoke test
```

### Task 3059: Phase 2 — delegation lifecycle controls

**Scope:**

- Implement `timeout` per delegation by plumbing `JobTimeout` into delegated
  jobs.
- Implement `retry` by tracking `RetryCount` in `JobPayload` and re-enqueueing
  failed jobs up to `delegation.Retry`.
- Implement `fingerprint` deduplication by hashing
  `(parentJobID, delegation.Fingerprint)` and skipping duplicate jobs.
- Implement `failure_policy` values: `block_parent`, `continue`, `escalate`.
- Implement `synthesis_rule` for continuation jobs:
  - `summary` (default): concatenate child summaries.
  - `vote`: require all children `approved`/`succeeded`.
- Add focused tests for each control.

**Acceptance criteria:**

- Each lifecycle control has focused unit tests.
- `go test ./internal/workflow ./internal/cli` passes.

**Suggested commit message:**

```text
feat(workflow): add delegation lifecycle controls
```
