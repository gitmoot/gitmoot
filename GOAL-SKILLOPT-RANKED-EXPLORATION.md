# SkillOpt Ranked Exploration Loop

Implement ranked N-way exploration for Gitmoot SkillOpt reviews so early
template learning explores broad output directions before narrowing into
refinement, distillation, and final validation.

The intended user-facing outcome is that Gitmoot can create review runs with
more than two options, collect ranked human feedback plus trait-level notes,
turn rankings into optimizer-ready preference signals, and preserve the current
A/B workflow for final validation. This goal touches SkillOpt review storage,
feedback parsing, review packet generation, export contracts, CLI help, tests,
and docs. It does not require changing `gitmoot-skillopt` optimizer internals
unless the export contract needs a small compatibility field.

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

- Parallelize only when tasks are independent, have disjoint file ownership, and
  can be reviewed and merged without order-dependent assumptions.
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

### Task 1: Add Ranked Review Data Model

Scope:
- Extend SkillOpt review data structures to support run mode and exploration
  settings:
  - `mode`: `explore`, `refine`, `distill`, or `validate`
  - `exploration_level`: `high`, `medium`, or `low`
  - `options`: expected option count for N-way review runs
- Add storage for review item options beyond baseline/candidate while keeping
  existing A/B fields working for validation runs.
- Add canonical ranked feedback structures:
  - ordered option ranking
  - optional winner
  - useful traits by option
  - rejected traits by option
  - free-form reasoning
  - derived pairwise preferences
- Preserve existing `a`, `b`, `tie`, `neither`, and `skip` feedback events for
  current A/B callers.

Acceptance criteria:
- Existing A/B review tests still pass unchanged.
- New model tests cover a 5-option ranked item and pairwise expansion.
- Invalid modes, duplicate option labels, missing ranked options, and unknown
  option references fail clearly.
- Existing local database migrations keep old installs readable.

Tests/checks:
- `go test ./internal/db ./internal/feedback ./internal/skillopt`
- `go test ./internal/cli -run SkillOpt`
- `git diff --check`

Suggested commit message:
- `feat(skillopt): add ranked review data model`

### Task 2: Add N-Way Review CLI And Packet Generation

Scope:
- Extend review creation and item-add commands so users can create exploration
  runs with multiple options.
- Keep the existing A/B command path simple and backward-compatible.
- Add CLI support for adding option artifacts to a review item, using a clean
  repeated flag or subcommand shape that avoids ad hoc positional parsing.
- Update Markdown and GitHub review packets to render N-way ranked review
  instructions.
- Include a copy-paste feedback format such as:

  ```text
  run_id: landing-page-trial-010
  item-011 ranking: C > A > D > B
  best traits:
  - C: clearest product explanation
  - A: best visual style
  - D: best motion
  reject:
  - B: too generic
  ```

Acceptance criteria:
- `gitmoot skillopt review create` can create both A/B validation runs and
  N-way exploration runs.
- Review packet output is clear enough for a human to rank options without
  knowing baseline/candidate identity.
- GitHub packet output includes table-style option links when artifacts contain
  preview URLs or output paths.
- Existing A/B publish output remains stable except for intentional wording
  improvements.

Tests/checks:
- `go test ./internal/cli -run SkillOpt`
- `go test ./internal/feedback`
- CLI smoke in a temp home:
  - create an explore run
  - add one item with four options
  - publish/export a Markdown packet
  - inspect generated feedback instructions

Suggested commit message:
- `feat(skillopt): generate ranked review packets`

### Task 3: Parse Ranked Feedback And Derive Preference Signals

Scope:
- Extend GitHub and Markdown feedback importers to parse ranked comments.
- Parse both YAML and short-form ranking syntax.
- Store trait-level feedback in canonical form.
- Derive pairwise preferences from rankings, for example `C > A > D > B`
  becomes:
  - `C beats A`
  - `C beats D`
  - `C beats B`
  - `A beats D`
  - `A beats B`
  - `D beats B`
- Keep deblinding/mapping correct when displayed option labels are randomized.
- Fix the class of mapping bug found in the manual PR #8 trial by adding tests
  that prove displayed choices map to internal option identities correctly.

Acceptance criteria:
- Ranked comments on GitHub import into canonical feedback events.
- Unknown option labels, partial rankings, duplicate labels, and malformed trait
  blocks produce actionable errors or documented skip behavior.
- A/B comment import remains correct and covered by regression tests.
- Pairwise-derived signals are visible in status/export output.

Tests/checks:
- `go test ./internal/feedback`
- `go test ./internal/cli -run SkillOptFeedback`
- Add regression tests for A/B deblinding and N-way deblinding.

Suggested commit message:
- `feat(skillopt): import ranked feedback`

### Task 4: Update SkillOpt Export Contract For Exploration Feedback

Scope:
- Extend `gitmoot skillopt export` so exported training packages include ranked
  feedback, trait notes, run mode, exploration level, and derived pairwise
  preferences.
- Keep the current A/B export schema consumable by `gitmoot-skillopt`.
- If `gitmoot-skillopt` cannot yet consume the richer fields, include them as
  additive metadata while preserving the existing baseline/candidate fields.
- Add contract docs that explain:
  - exploration data is for broad output-direction learning
  - refinement data combines winning traits
  - distillation updates template body
  - validation compares current template vs candidate on fresh prompts

Acceptance criteria:
- Exported package validates for both A/B validation runs and N-way exploration
  runs.
- Existing `gitmoot-skillopt` runs are not broken by the new fields.
- The export includes enough information for a future optimizer to combine
  traits across ranked options.

Tests/checks:
- `go test ./internal/cli -run SkillOptExport`
- `go test ./internal/skillopt`
- Manual JSON inspection of an N-way fixture export.

Suggested commit message:
- `feat(skillopt): export ranked exploration feedback`

### Task 5: Add Exploration-Level Recommendations

Scope:
- Add status logic that summarizes whether a run should stay in `explore`,
  move to `refine`, move to `distill`, or run `validate`.
- Keep this as a recommendation only; do not auto-promote or auto-change modes.
- Suggested heuristic:
  - `explore -> refine` when the same direction wins multiple rounds, top
    options share traits, or reviewer language says the direction is close.
  - `refine -> distill` when feedback becomes mostly specific improvements and
    broad new directions stop winning.
  - `distill -> validate` when enough ranked feedback exists to justify a
    template update.
  - `validate -> promote` only after candidate beats current template on fresh
    review items.
- Surface the recommendation in `review status`, Markdown packets, and GitHub
  packet summaries.

Acceptance criteria:
- Status output explains the current mode, feedback count, ranking stability,
  and recommended next mode.
- Heuristic is deterministic and covered with tests.
- The output does not overclaim; it should say "recommend" rather than forcing
  a transition.

Tests/checks:
- `go test ./internal/cli -run SkillOptReviewStatus`
- `go test ./internal/feedback ./internal/skillopt`

Suggested commit message:
- `feat(skillopt): recommend exploration phase transitions`

### Task 6: Document The Ranked Exploration Workflow

Scope:
- Update Gitmoot docs and skill files so users understand the new flow:
  1. Explore with 4-6 diverse options.
  2. Rank options and identify useful/rejected traits.
  3. Refine with 2-3 combined candidates.
  4. Distill the template from accumulated feedback.
  5. Validate old vs new on fresh prompts.
- Update `docs/skillopt-exchange-contract.md`.
- Update `skills/gitmoot/SKILL.md` and references if the CLI surface changes.
- Add examples for landing pages and a non-visual text task.
- Explicitly state that A/B remains the right mode for final validation.

Acceptance criteria:
- Docs explain when to use explore/refine/distill/validate.
- Docs include exact CLI examples for Markdown and GitHub review collectors.
- The Gitmoot skill tells agents not to run heavy SkillOpt after every tiny
  feedback round unless the user explicitly wants that.

Tests/checks:
- `go test ./...`
- Run any docs/example smoke checks present in the repo.
- `git diff --check`

Suggested commit message:
- `docs(skillopt): document ranked exploration workflow`

### Task 7: End-To-End Smoke Test And Release Notes

Scope:
- Add a focused smoke test or documented manual smoke under
  `docs/beta-smoke-tests.md` for:
  - N-way exploration review creation
  - option artifact registration
  - Markdown packet export/import
  - GitHub packet publish/sync
  - ranked feedback pairwise expansion
  - export package inspection
- Add release notes for the next beta.
- If needed, add a small fixture package rather than generated previews or large
  artifacts.

Acceptance criteria:
- A maintainer can run the smoke from a clean temp home without touching real
  user state.
- Release notes accurately describe ranked exploration as a beta feature.
- No large generated preview outputs are committed unless they are tiny
  intentional fixtures.

Tests/checks:
- `go test ./...`
- Smoke commands from docs using a temp home.
- `codex exec review --uncommitted`
- `git diff --check`

Suggested commit message:
- `test(skillopt): add ranked exploration smoke coverage`
