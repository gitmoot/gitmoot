# SkillOpt Human Feedback Trial Workflow

Implement the plan task by task. Each task must be developed, reviewed, opened
as its own pull request, merged, and verified before moving on, unless tasks are
explicitly safe to run in parallel.

Build the missing operator-facing workflow needed to run a real Gitmoot
SkillOpt human A/B feedback trial without manual database seeding. The outcome
should let a user create a review run, attach baseline/candidate outputs from
files, export a Markdown feedback packet, import completed feedback, export a
training package, run Gitmoot-SkillOpt, import the candidate, and promote or
reject it. Preserve the existing SkillOpt storage/export/import contracts and
keep Gitmoot as the product/control layer; do not embed the external
`gitmoot-skillopt` optimizer into the Go CLI.

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

### Task 1: Add SkillOpt Review Run Creation And Status

Scope:

- Add a `gitmoot skillopt review` command group to the existing SkillOpt CLI
  surface.
- Add `gitmoot skillopt review create` with this intended shape:

  ```sh
  gitmoot skillopt review create \
    --template planner \
    --repo owner/repo \
    --run planner-ab-1
  ```

- Resolve and store the selected template's current version id at creation time.
- Create or update the `eval_runs` row with state suitable for human review.
- Add `gitmoot skillopt review status --run planner-ab-1`.
- Status should report template id/version, target repo, state, review item
  count, imported feedback count, and readiness for packet/training export.
- Keep the implementation in the existing CLI/store style; extract small
  helpers where status/count logic would otherwise duplicate export or feedback
  code.

Acceptance criteria:

- `gitmoot skillopt --help` includes the review commands.
- Creating a review run with an installed template succeeds.
- Creating a review run for an unknown template returns a clear actionable
  error.
- Status works for runs with zero items and for missing runs.
- Existing `skillopt export`, feedback, candidate, and import commands keep
  their behavior.

Tests/checks:

- Focused Go tests for `internal/cli` covering review create and status.
- Focused Go tests for any new `internal/skillopt` or helper package logic.
- `go test ./internal/cli ./internal/skillopt ./internal/db`.
- Broader `go test ./...` because this touches a CLI/API surface.
- `git diff --check`.
- `codex exec review --uncommitted`.

Suggested branch:

- `task/skillopt-review-create-status`

Suggested commit message:

- `feat: add skillopt review create and status`

### Task 2: Add Artifact-Backed Review Item Creation

Scope:

- Add `gitmoot skillopt review item add` with this intended shape:

  ```sh
  gitmoot skillopt review item add \
    --run planner-ab-1 \
    --item item-001 \
    --title "README planning task" \
    --baseline baseline.md \
    --candidate candidate.md
  ```

- Read baseline and candidate files from disk, store their bytes in Gitmoot's
  content-addressed artifact blob store, register `eval_artifacts`, and create
  the corresponding `eval_review_items` row.
- Use deterministic, collision-resistant artifact ids scoped to the run/item and
  artifact role, for example `planner-ab-1/item-001/baseline` and
  `planner-ab-1/item-001/candidate`, unless existing repo patterns suggest a
  better local convention.
- Infer `media_type` conservatively from file extension/content where practical,
  defaulting to `text/markdown` or `text/plain` for Markdown/text files.
- Add optional flags only if they stay simple and aligned with existing storage:
  `--source`, `--preview`, `--diff`, `--metadata-json`, or `--driver`.
- Reject missing files, identical baseline/candidate artifact ids, unsupported
  binary inputs without a clear media type, invalid JSON metadata, and missing
  runs with clear errors.
- Update `review status` so item and feedback counts reflect items added by the
  new command.

Acceptance criteria:

- Users can add multiple review items to one run.
- Markdown feedback export works immediately after adding at least one complete
  baseline/candidate item.
- The command does not duplicate file blobs unnecessarily; the blob store remains
  content-addressed.
- The generated review item records satisfy existing Markdown and GitHub
  feedback collector validation.

Tests/checks:

- Go CLI tests for successful item add with Markdown files.
- Go CLI tests for missing files, missing run, invalid metadata, and incomplete
  arguments.
- Go tests that export a Markdown packet after adding an item through the CLI.
- `go test ./internal/cli ./internal/feedback ./internal/db`.
- Broader `go test ./...`.
- `git diff --check`.
- `codex exec review --uncommitted`.

Suggested branch:

- `task/skillopt-review-item-add`

Suggested commit message:

- `feat: add skillopt review item creation`

### Task 3: Polish Human Feedback Packet And Trial Documentation

Scope:

- Improve Markdown packet guidance so a first-time reviewer knows exactly what
  to open and edit:
  - open `index.md`
  - review `items/*.md`
  - edit `feedback.yml`
  - use only `a`, `b`, `tie`, `neither`, or `skip`
  - import the completed packet with the exact command
- Keep `.assignments.json` hidden/internal and preserve the blind A/B mapping.
- Add a concise happy-path trial section to `docs/skillopt-exchange-contract.md`
  and any relevant skill/reference docs:

  ```text
  review create
  review item add
  feedback markdown export
  edit feedback.yml
  feedback markdown import
  skillopt export
  gitmoot-skillopt optimize
  skillopt import
  candidate show
  candidate promote/reject
  ```

- Add model/backend configuration notes for the non-`--dry-run`
  `gitmoot-skillopt optimize` step. State the exact local commands/env checks to
  run and clearly separate dry-run contract validation from real model-backed
  optimization.
- If a lightweight `review open` command is straightforward and follows repo
  style, add it only as a convenience that prints or opens the packet path; do
  not let it distract from the required trial flow.

Acceptance criteria:

- A user can follow docs from no review run to imported feedback without manual
  DB seeding.
- The packet instructions are clear enough to complete `feedback.yml` without
  reading source code.
- Docs distinguish saved-output A/B review from future live pairwise evaluation.
- Docs do not imply candidates auto-promote.

Tests/checks:

- Existing feedback tests continue to pass.
- Add focused tests only if packet output text changes are validated in tests.
- `go test ./internal/feedback ./internal/cli`.
- `go test ./...`.
- If docs site/build tooling exists and is lightweight, run the relevant docs
  check/build; otherwise record that no docs build is configured.
- `git diff --check`.
- `codex exec review --uncommitted`.

Suggested branch:

- `task/skillopt-feedback-trial-docs`

Suggested commit message:

- `docs: add skillopt human feedback trial workflow`

### Task 4: Add End-To-End Human Feedback Trial Smoke

Scope:

- Add a deterministic local smoke test or script that exercises the new
  user-facing commands without touching the user's real `~/.gitmoot`.
- The smoke should use a temp Gitmoot home and small tracked or temp Markdown
  files.
- It should cover:
  1. `gitmoot skillopt review create`
  2. `gitmoot skillopt review item add`
  3. `gitmoot skillopt feedback markdown export`
  4. writing a completed `feedback.yml`
  5. `gitmoot skillopt feedback markdown import`
  6. `gitmoot skillopt export --output training.json`
  7. `gitmoot-skillopt optimize --dry-run` when the external repo is available,
     or a documented local-only boundary substitute if cross-repo execution is
     too brittle for CI
  8. `gitmoot skillopt import --file candidate.json --artifact-dir artifacts`
  9. `gitmoot skillopt candidate show`
  10. `gitmoot skillopt candidate reject --reason "trial smoke"`
- Prefer testing Gitmoot-owned commands inside `/root/gitmoot`; only add
  cross-repo smoke references when they are stable and do not require generated
  outputs to be committed.

Acceptance criteria:

- The smoke proves the human-feedback-trial path no longer needs manual DB
  seeding.
- The smoke proves imported feedback reaches `training.json`.
- The smoke proves the imported candidate remains pending until explicit
  promote/reject.
- The smoke output or documented expected output is included in the PR body and
  final task summary.

Tests/checks:

- Focused smoke command or Go integration test.
- `go test ./internal/cli ./internal/feedback ./internal/skillopt ./internal/db`.
- Broader `go test ./...`.
- If cross-repo `gitmoot-skillopt` smoke is run, record exact command and
  output.
- `git diff --check`.
- `codex exec review --uncommitted`.

Suggested branch:

- `task/skillopt-human-feedback-smoke`

Suggested commit message:

- `test: add skillopt human feedback trial smoke`
