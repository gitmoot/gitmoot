# Goal: Implement Gitmoot SkillOpt Train Mode

Implement the plan task by task. Each task must be developed, reviewed, opened
as its own pull request, merged, and verified before moving on, unless tasks are
explicitly safe to run in parallel.

This goal implements the normal product workflow for SkillOpt training:

```text
gitmoot skillopt train start
gitmoot skillopt train status
gitmoot skillopt train continue
gitmoot skillopt train stop
```

The core product rule is that Gitmoot owns the guided training state machine,
review surfaces, candidate import, and promotion decisions. `gitmoot-skillopt`
proposes optimized candidate templates; Gitmoot imports, verifies, reviews, and
promotes or rejects them. The default user flow must not depend on manual
append-style template edits.

## Core Rules

- Work one task at a time in the listed order by default.
- If tasks are independent, have disjoint file ownership, and do not depend on
  each other's results, they may be done in parallel on separate branches.
- Do not start dependent work until the prerequisite task has passed checks,
  passed `codex exec review --uncommitted`, been pushed, opened as a PR, merged,
  and verified on the target branch.
- Do not commit generated data, reports, caches, build artifacts, secrets,
  credentials, or large outputs unless the plan explicitly says they are
  intended tracked fixtures/artifacts.
- Preserve existing behavior unless the current task explicitly changes it.
- Keep changes clean, scoped, and organized. Avoid broad rewrites.
- Avoid code duplication. When repeated logic appears, extract small reusable
  helpers that match existing repo patterns.
- If implementation depends on external APIs, docs, CLIs, data formats,
  generated scripts, installers, service launchers, subprocess calls, env vars,
  config formats, or third-party libraries, verify the real contract with local
  commands and/or official sources before editing.
- Keep `gitmoot` as the control/review layer. Do not embed optimizer internals
  from `gitmoot-skillopt` into the Go CLI.
- Use deterministic behavior for MVP normalization and state transitions unless
  the task explicitly asks for model-assisted behavior.
- For this repository, use the repo-required Go toolchain. If local tests need a
  toolchain override, use `GOTOOLCHAIN=go1.26.2`.

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
4. Inspect relevant existing patterns before editing.
5. Verify PR tooling is available before the first PR:
   - `gh auth status`
   - repo remote resolves to the expected GitHub repository
6. Re-check issue #67 for accepted product details before Task 1.

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
   "Review found these issues: <<PASTE RAW REVIEW RESULTS BY REPO>>.
   For each issue, identify the underlying invariant/class of bug, audit sibling
   paths for the same issue, and plan the smallest safe fix. Verify external
   assumptions with local commands and/or official sources. Preserve repo
   patterns, avoid unnecessary refactors, and list tests/checks per repo."
5. Execute the fix plan.
6. Re-run focused tests/checks and `codex exec review --uncommitted` in every
   repo with uncommitted changes.
7. Repeat until the final raw review output contains no findings, or stop if
   blocked or if a finding is incorrect after verification.

## Commit Gate

1. Before committing, run `git diff --check` and inspect the final diff.
2. Commit only the current task's intended tracked changes.
3. Use the commit message specified by the plan. If the plan does not specify
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

## Implementation Plan

### Task 1: Add Train Session State Model And CLI Skeleton

Scope:

- Add persistent train-session and train-iteration records for the guided
  SkillOpt workflow.
- Add CLI commands:
  - `gitmoot skillopt train start`
  - `gitmoot skillopt train status`
  - `gitmoot skillopt train continue`
  - `gitmoot skillopt train stop`
- Model explicit states:
  - `request_confirmed`
  - `workspace_ready`
  - `items_ready`
  - `options_generated`
  - `review_published`
  - `feedback_synced`
  - `training_package_created`
  - `optimizer_completed`
  - `candidate_created`
  - `candidate_review_published`
  - `candidate_promoted`
  - `candidate_rejected`
  - `run_abandoned`
- Add transition validation so invalid `continue` calls explain the next
  required action instead of silently doing the wrong thing.
- Keep low-level existing SkillOpt commands available as debug/manual tools.
- Do not run optimizer work yet; this task establishes the orchestration model.

Acceptance:

- `train start` creates a durable session and first iteration.
- `train status` prints current state, completed steps, blocked step, next
  command/action, latest links when present, candidate ID when present, and
  feedback count when present.
- `train continue` advances only through implemented safe transitions and gives
  clear errors for unimplemented future transitions.
- `train stop` marks the session abandoned with an optional reason.
- Existing SkillOpt commands keep working.

Tests/checks:

- Focused CLI tests for start/status/continue/stop.
- Store migration tests for train sessions and iterations.
- Transition validation tests.
- `go test ./internal/cli ./internal/db ./internal/skillopt`
- `go test ./...`

Suggested branch:

- `task/skillopt-train-state`

Suggested commit message:

- `feat(skillopt): add train session state machine`

### Task 2: Add Training Request Summary, Workspace Resolution, And Item Validation

Scope:

- Extend `train start` to capture and persist the human request summary:
  - template being trained
  - expected output type
  - comparison target
  - review surface
  - preview needs
  - what counts as better
  - task category: correctness, UX/preference, writing, design, data processing,
    or other
- Add workspace repo resolution:
  - explicit `--repo owner/name`
  - target repo from eval/template context
  - template source repo
  - configured default feedback repo
  - otherwise stop and ask for `--repo`
- Add optional preview repo configuration for clickable static previews.
- Require more than one training item by default.
- Warn when training items appear too homogeneous.
- Persist item metadata so later steps can explain what the human is comparing.

Acceptance:

- Starting a train run without enough item diversity warns or blocks according to
  CLI flags.
- Repo resolution follows the documented order and reports the selected repo.
- Preview repo is recorded when provided and omitted cleanly otherwise.
- The stored request summary appears in `train status`.

Tests/checks:

- CLI tests for repo resolution order.
- CLI tests for item-count validation and homogeneous-item warnings.
- Store tests for request summary persistence.
- `go test ./internal/cli ./internal/skillopt ./internal/db`
- `go test ./...`

Suggested branch:

- `task/skillopt-train-request-workspace`

Suggested commit message:

- `feat(skillopt): capture train requests and workspace repos`

### Task 3: Generate Review Options With Temporary Gitmoot Agents

Scope:

- Add train-mode generation step that creates temporary Gitmoot agents for
  review option generation.
- Support K-way options, not only A/B, with configurable option count and max
  concurrent temporary agents.
- Record generation prompts, runtime, template version, item ID, output artifact
  refs, and option metadata.
- Ensure generated options are diverse enough to make human feedback meaningful.
- Use locks/timeouts so parallel train runs do not reuse the same temporary
  agent incorrectly.
- Temporary agents must exit or be released after the generation job.

Acceptance:

- `train continue` can move from `items_ready` to `options_generated` by
  creating options for each item.
- Output artifacts and metadata are recorded reproducibly.
- K-way option assignment supports at least two options and defaults to a small
  practical count.
- Generation fails clearly if no compatible runtime/agent can be created.

Tests/checks:

- CLI tests for K-way generation state transitions.
- Runtime/dispatch tests for temporary agent reservation/release.
- Artifact metadata tests.
- Operational smoke with dry-run/fake runtime if available.
- `go test ./internal/cli ./internal/runtime ./internal/workflow ./internal/db`
- `go test ./...`

Suggested branch:

- `task/skillopt-train-generation`

Suggested commit message:

- `feat(skillopt): generate train options with temporary agents`

### Task 4: Improve GitHub Review Packets And Feedback Normalization

Scope:

- Make train-created GitHub review issues concise and table-first.
- Include:
  - run ID and session ID
  - what the human is comparing
  - item/option table
  - preview links when artifact metadata includes URLs
  - short copyable YAML feedback format
  - minimal instructions
- Avoid parseable sample comments outside the intended feedback block.
- Extend ranked feedback parsing with optional fields:
  - `quality: poor | acceptable | strong`
  - `continue_mode: explore | refine | distill | validate`
  - `promote: yes | no`
- Store/export these fields in canonical feedback events.
- Add deterministic normalization for common malformed feedback:
  - ranking whitespace
  - natural-language ranking summaries
  - long reasoning with colons
  - duplicated item-level notes
- Improve parse errors to point to exact comment/item and suggest block-scalar
  YAML for long reasoning.
- Update phase recommendation logic so stable relative rankings do not force
  `refine` when feedback says `quality: poor` or `continue_mode: explore`.

Acceptance:

- GitHub train review issues are short, table-first, and clear.
- Feedback with rankings plus optional quality/continue/promote imports
  successfully.
- Existing A/B and ranked feedback without optional fields remain valid.
- Stable winner no longer forces `refine` when reviewed items say absolute
  quality is poor or request exploration.

Tests/checks:

- `go test ./internal/feedback ./internal/skillopt`
- `go test ./internal/cli -run SkillOpt`
- Golden/body tests for concise GitHub issue output.
- Parser tests for optional fields and normalizable malformed feedback.
- Phase recommendation tests for quality/continue overrides.
- `go test ./...`

Suggested branch:

- `task/skillopt-train-feedback-ux`

Suggested commit message:

- `feat(skillopt): add train feedback quality signals`

### Task 5: Integrate External gitmoot-skillopt Optimizer Invocation

Scope:

- Add train-mode handoff to the external `gitmoot-skillopt` executable.
- Keep the optimizer as an external process and verify its CLI contract before
  integration.
- Export a training package containing:
  - current promoted/best template version
  - training items
  - generated options/artifact refs
  - imported feedback events
  - evaluator config
  - model selection
  - preferred gate
- Support evaluator gates:
  - `hard` for correctness tasks
  - `soft` for UX/preference/design/writing tasks
  - `hard_then_soft` as the default for human preference tasks
- Invoke `gitmoot-skillopt optimize` with explicit input/output paths.
- Import the resulting candidate package as a pending template version.
- Ensure optimizer output can include candidate template, diff, summary, and
  artifacts without mutating Gitmoot state directly.

Acceptance:

- `train continue` can move from `feedback_synced` to `candidate_created` by
  exporting, invoking the optimizer, and importing a pending candidate.
- Missing `gitmoot-skillopt` binary gives a clear setup error.
- Optimizer failures preserve logs/output path and do not corrupt Gitmoot state.
- Candidate templates are rewritten cleanly by the optimizer package, not
  append-only manual edits by Gitmoot.

Tests/checks:

- CLI tests using a fake `gitmoot-skillopt` executable.
- Contract tests for export package fields, preferred gate, and model selection.
- Candidate import tests for artifact-dir containment and pending state.
- Operational smoke with a fake optimizer command.
- `go test ./internal/cli ./internal/skillopt ./internal/db`
- `go test ./...`

Suggested branch:

- `task/skillopt-train-optimizer`

Suggested commit message:

- `feat(skillopt): run optimizer from train mode`

### Task 6: Publish Candidate Diffs, PRs, And Preview Comparisons

Scope:

- Add train-mode candidate review publishing after candidate import.
- For text/templates, publish candidate diff and summary.
- For visual/product tasks, support old-template-vs-candidate preview comparison
  artifacts and links.
- Support GitHub PR organization when useful:
  - issue is the canonical training conversation
  - PR contains candidate template diff and preview artifacts
  - Pages or preview repo hosts clickable demos when configured
- Link issue, PR, candidate version, and preview URLs in status output.

Acceptance:

- `train continue` can publish a candidate review packet after candidate import.
- Candidate review includes template diff, output previews when available,
  candidate summary, and a recommendation.
- Existing issue is updated instead of scattering a run across unrelated issues.
- PR/preview repo behavior is opt-in/configured and fails clearly if missing.

Tests/checks:

- GitHub body tests for candidate review updates.
- CLI tests for issue/PR link recording.
- Preview metadata tests.
- `go test ./internal/cli ./internal/feedback ./internal/skillopt ./internal/db`
- `go test ./...`

Suggested branch:

- `task/skillopt-train-candidate-review`

Suggested commit message:

- `feat(skillopt): publish candidate review packets`

### Task 7: Add Promotion, Rejection, And Iteration Loop Controls

Scope:

- Add train-mode promotion/rejection actions on top of existing candidate
  promote/reject primitives.
- Ensure candidate promotion is explicit and auditable.
- After promotion, ask whether to stop or continue with another iteration.
- If continuing, the next iteration must start from the promoted template.
- If rejected, require a reason or captured feedback before continuing
  exploration.
- Block new iterations from manual template edits unless the user explicitly uses
  an advanced/debug escape hatch.

Acceptance:

- Promoting a candidate marks it current and closes the iteration cleanly.
- Rejecting a candidate keeps it out of `@latest` and records the reason.
- Continuing after promotion starts from the promoted template version.
- Continuing after rejection starts a new exploration iteration only when the
  state has enough feedback/reasoning.
- Status clearly shows whether the run is complete, waiting for human decision,
  or ready for another iteration.

Tests/checks:

- State transition tests for promote/reject/continue.
- Store tests for audit records and current-version updates.
- CLI tests for invalid transitions.
- `go test ./internal/cli ./internal/skillopt ./internal/db`
- `go test ./...`

Suggested branch:

- `task/skillopt-train-iteration-controls`

Suggested commit message:

- `feat(skillopt): control train iteration promotion`

### Task 8: Update Docs, Skill, Install Notes, And End-To-End Smoke Tests

Scope:

- Update README, `SKILL.md`, generated LLM docs, and website/docs content for
  the guided train workflow.
- Document:
  - quick start
  - train start/status/continue/stop
  - GitHub issue feedback format
  - optional `quality`, `continue_mode`, and `promote`
  - workspace repo and preview repo behavior
  - external `gitmoot-skillopt` setup
  - troubleshooting and status outputs
- Add an end-to-end fake-optimizer smoke test that exercises:
  - start
  - option generation dry-run/fake runtime
  - review publish dry-run/fake GitHub if available
  - feedback sync/import
  - optimizer fake invocation
  - candidate import
  - candidate review publish dry-run
  - promote/reject
- Keep smoke fixtures small and tracked only when intentional.

Acceptance:

- A new user can understand the normal train workflow from docs and skill text.
- Existing plugin/skill guidance points users to `gitmoot skillopt train ...`
  instead of manual low-level loops.
- End-to-end smoke covers the core state machine without requiring live paid API
  calls.
- Release notes are ready for the next beta.

Tests/checks:

- `go test ./...`
- docs/build checks if applicable.
- plugin/skill install smoke if available.
- `git diff --check`
- `codex exec review --uncommitted`

Suggested branch:

- `task/skillopt-train-docs-smoke`

Suggested commit message:

- `docs(skillopt): document guided train mode`

## Final Response After All Tasks

- List completed tasks.
- For each task, list branch, PR URL, merge status, and merged commit hash.
- List tests/checks run.
- Include exact final raw `codex exec review --uncommitted` output for the last
  task/repo.
- Mention skipped checks, blockers, or residual risk.
- Do not claim interactive `/review` is clean. Say:
  "codex exec review is clean; ready for manual /review."
