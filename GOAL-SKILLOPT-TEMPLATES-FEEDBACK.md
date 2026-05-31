# Skill-Like Agent Templates And SkillOpt Feedback Loop

Implement the plan task by task. Each task must be developed, reviewed, opened
as its own pull request, merged, and verified before moving on, unless tasks are
explicitly safe to run in parallel.

Evolve Gitmoot agent templates from prompt snapshots into versioned,
skill-like templates with YAML metadata, discoverable capabilities, human
feedback collectors, artifact-backed eval storage, and an external
`gitmoot-skillopt` optimization contract. This goal does not implement a hosted
web UI, live pairwise evaluation, or full XLSX diff rendering beyond the
foundational driver interfaces explicitly listed below.

External context checked before writing this goal:

- Agent skill formats use YAML frontmatter plus Markdown bodies for routing,
  discovery, and progressive disclosure.
- SkillOpt treats compact skill documents as trainable artifacts and improves
  them through rollout, reflection, bounded edits, and validation gates.
- GitHub issues/comments are a suitable first collaborative feedback collector
  because Gitmoot already uses GitHub and GitHub supports issue/comment APIs.

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

### Task 1: Remove Active Preset Terminology

Scope:

- Remove active user-facing `preset` aliases from config parsing, CLI help,
  docs, website docs, generated LLM docs, and tests.
- Keep only narrowly scoped DB migrations or legacy payload readers if removing
  them would corrupt existing local Gitmoot state. If retained, isolate them as
  migration-only compatibility and hide them from public behavior.
- Rename test cases and comments that still describe current behavior as
  presets.

Acceptance criteria:

- `rg -n "\bpreset\b|Preset|--preset|preset_id" README.md SKILL.md skills website internal`
  returns only explicitly justified legacy migration or legacy payload handling.
- `gitmoot agent type` no longer accepts `preset =`.
- Public docs and help use only `agent template`.

Tests/checks:

- Focused config and agent-template tests.
- `go test ./internal/config ./internal/cli ./internal/db ./internal/workflow`
- `go test ./...` if migrations or job payload handling changes.

Suggested commit message:

- `refactor: remove active preset terminology`

### Task 2: Add Skill-Like Template Frontmatter

Scope:

- Add an `internal/agenttemplate` parser for Markdown templates with YAML
  frontmatter plus body.
- Define a v1 metadata schema with at least:
  - `id`
  - `name`
  - `description`
  - `kind: agent-template`
  - `version`
  - `capabilities`
  - `runtime_compatibility`
  - `tags`
  - `inputs`
  - `outputs`
  - optional `evaluation`
- Update built-in templates, including `planner`, to use frontmatter.
- Update `agent template draft` to emit the v1 frontmatter format.
- Update `agent template validate` and `agent template add` to validate
  frontmatter and body together.

Acceptance criteria:

- Existing built-in templates validate as v1 agent templates.
- Local custom templates without valid frontmatter fail with actionable errors,
  unless a deliberate migration command is added in the same task.
- Template IDs in frontmatter and CLI arguments must match when adding a local
  template.

Tests/checks:

- Parser/validator unit tests for valid frontmatter, missing required fields,
  wrong `kind`, mismatched IDs, invalid capabilities, and empty body.
- CLI tests for `draft`, `validate`, and `add`.
- `go test ./internal/agenttemplate ./internal/cli`

Suggested commit message:

- `feat: add skill-like agent template frontmatter`

### Task 3: Index Template Metadata And Discovery Filters

Scope:

- Extend SQLite storage to persist parsed template metadata alongside the
  content snapshot.
- Keep raw template content available for job snapshots.
- Add discovery filters to `gitmoot agent template list`, such as:
  - `--capability plan`
  - `--runtime codex`
  - `--tag planning`
  - `--output goal_file`
- Update `show` output to include parsed metadata in a readable format.

Acceptance criteria:

- Users can discover installed and built-in templates by metadata fields.
- Metadata is parsed once on add/update and reused for listing.
- Invalid metadata cannot be stored through normal template commands.

Tests/checks:

- DB migration tests.
- CLI list/show filter tests.
- `go test ./internal/db ./internal/agenttemplate ./internal/cli`

Suggested commit message:

- `feat: index agent template metadata`

### Task 4: Add Versioned Template Storage

Scope:

- Split logical template identity from versioned template content.
- Add tables or equivalent storage for:
  - logical template records
  - template versions
  - current/promoted version pointer
  - pending candidate versions
- Preserve job snapshot semantics: queued jobs must use the exact template
  version content captured at enqueue time.
- Support latest and pinned references in agent configuration where appropriate.
- Add rollback-safe migrations from the current `agent_templates` table.

Acceptance criteria:

- `agent template update` creates a new version when content changes.
- `agent template show` displays current version, content hash, source, and
  promotion state.
- Existing agents continue to enqueue jobs with a stable template snapshot.
- A pending version can exist without becoming current.

Tests/checks:

- Migration tests from existing template storage.
- Mailbox snapshot tests for pinned/current versions.
- CLI update/show tests.
- `go test ./internal/db ./internal/agenttemplate ./internal/workflow ./internal/cli`
- `go test ./...` because storage changes are shared.

Suggested commit message:

- `feat: version agent templates`

### Task 5: Add Artifact-Backed Eval Storage

Scope:

- Add a local content-addressed artifact store for eval outputs, previews,
  diffs, reports, and source artifacts.
- Store blobs by SHA256 under Gitmoot home, not in the repository by default.
- Add SQLite manifests/references for artifacts, runs, and review items.
- Add a text/Markdown preview and diff driver as the first supported driver.
- Keep `.gitmoot/evals/...` review packets local/ignored unless the user
  explicitly commits fixtures.

Acceptance criteria:

- Identical artifact content is stored once and referenced many times.
- Text/Markdown artifacts can produce human-readable previews and diffs.
- Artifact paths are deterministic and do not expose secrets in filenames.
- No generated run artifacts are accidentally tracked by default.

Tests/checks:

- Artifact store unit tests for write, dedupe, read, missing blob, and manifest
  references.
- Text diff driver tests.
- `go test ./internal/...`

Suggested commit message:

- `feat: add artifact-backed eval storage`

### Task 6: Define The Gitmoot-SkillOpt Exchange Contract

Scope:

- Add Gitmoot-side export/import commands and data structures for an external
  `gitmoot-skillopt` optimizer.
- Export training packages containing:
  - template identity and current promoted version
  - eval items/tasks
  - saved baseline outputs
  - artifact references
  - feedback events when present
  - evaluator config
- Import candidate packages containing:
  - candidate template content
  - parsed metadata
  - eval report
  - diff/score/preference summary
- Imported candidates become pending template versions, not promoted versions.

Acceptance criteria:

- Exported packages are deterministic enough for review and testing.
- Import validates template frontmatter, version compatibility, and artifact
  references.
- Candidate import never changes the current promoted version.
- The contract is documented so `gitmoot-skillopt` can be built as a separate
  repo later.

Tests/checks:

- Contract serialization tests.
- CLI export/import tests using temp Gitmoot homes.
- Docs test or snapshot for the contract examples if local patterns support it.
- `go test ./internal/...`

Suggested commit message:

- `feat: add skillopt exchange contract`

### Task 7: Add Canonical Feedback Events And Markdown Collector

Scope:

- Add a canonical feedback event schema:
  - `run_id`
  - `item_id`
  - `choice`
  - optional `reasoning`
  - `reviewer`
  - `source`
  - optional `source_url`
  - `created_at`
- Allowed choices are exactly:
  - `a`
  - `b`
  - `tie`
  - `neither`
  - `skip`
- Add blind A/B assignment metadata so baseline/candidate mapping is hidden from
  reviewers but recoverable by Gitmoot.
- Implement the Markdown collector:
  - generate `index.md`
  - generate item Markdown files
  - generate editable `feedback.yml`
  - import and validate `feedback.yml`

Acceptance criteria:

- Review packets are clear enough for a human to fill without reading docs.
- Feedback import rejects unknown item IDs and invalid choices.
- Reasoning is optional.
- Imported feedback normalizes into canonical events.

Tests/checks:

- Feedback schema validation tests.
- Markdown packet snapshot tests.
- Feedback import tests for valid YAML, invalid item IDs, invalid choices, and
  missing optional reasoning.
- `go test ./internal/...`

Suggested commit message:

- `feat: add markdown skillopt feedback collector`

### Task 8: Add GitHub Issue And Comment Feedback Collector

Scope:

- Add a GitHub collector that publishes A/B review packets to an issue by
  default, with an option to comment on an existing PR.
- Implement repository resolution:
  1. use explicit `--repo owner/name`
  2. else use the eval run target repo
  3. else use the template source/owner repo
  4. else use configured default feedback repo
  5. else stop and ask for `--repo`
- Make the human response format copy-pasteable in the issue body.
- Accept full YAML replies and short-form replies:
  - `item-001: b - More concrete and easier to execute.`
  - `item-002: tie`
- Sync GitHub comments into canonical feedback events with reviewer and
  `source_url`.

Acceptance criteria:

- Published GitHub review issues clearly explain allowed choices and include a
  copy-paste feedback block.
- Sync imports valid GitHub comments and ignores unrelated comments safely.
- Duplicate sync does not create duplicate feedback events.
- PR comment mode works when `--pr <number>` is provided.

Tests/checks:

- GitHub collector unit tests using mocked GitHub command/API runner.
- Parser tests for YAML replies and short-form replies.
- Operational smoke with a test repo if GitHub auth is available.
- `go test ./internal/...`

Suggested commit message:

- `feat: add github feedback collector`

### Task 9: Add Candidate Review, Promotion, And Rejection Workflow

Scope:

- Add commands to inspect pending candidate versions, view diffs/eval summaries,
  promote a candidate, or reject a candidate.
- Promotion updates the template's current promoted version.
- Rejection keeps audit history but prevents the candidate from being selected
  as current/latest.
- Show A/B feedback summary without exposing mapping during active blind
  reviews; expose mapping only after review closes or in admin/debug output.
- Update docs, skill references, website docs, and smoke tests for the full
  workflow.

Acceptance criteria:

- Candidate templates never auto-promote.
- Human can inspect template diff, eval report, feedback summary, and artifact
  review packet before promotion.
- Promotion and rejection are auditable.
- Docs show a complete local Markdown path and GitHub collector path.

Tests/checks:

- CLI tests for candidate list/show/promote/reject.
- DB tests for promotion state transitions.
- End-to-end smoke using a temp Gitmoot home and text artifacts.
- `go test ./...`
- `git diff --check`

Suggested commit message:

- `feat: add template candidate promotion workflow`

### Task 10: Create Follow-Up Issue For Live Pairwise Evaluation

Scope:

- Create a GitHub issue documenting future live pairwise evaluation.
- The issue should explain that MVP uses saved baseline outputs, while future
  live pairwise mode runs current promoted template and candidate live for every
  validation item.
- Include tradeoffs:
  - more faithful comparisons
  - higher latency and token cost
  - more runtime/session complexity
  - better protection against stale baseline outputs

Acceptance criteria:

- A GitHub issue exists and is linked from docs or roadmap notes.
- No live pairwise implementation is included in this task.

Tests/checks:

- No code tests required unless docs are edited.
- If docs are edited, run the repo's normal docs/static generation checks.

Suggested commit message:

- `docs: track live pairwise evaluation follow-up`
