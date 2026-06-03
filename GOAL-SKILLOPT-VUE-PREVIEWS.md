# SkillOpt Vue Preview Publishing

Implement the plan task by task. Each task must be developed, reviewed, opened
as its own pull request, merged, and verified before moving on, unless tasks are
explicitly safe to run in parallel.

Build the missing preview layer for `gitmoot skillopt train` so landing-page
training runs produce real Vue/Vite preview links before GitHub review packets
are published. The immediate implementation only needs two preview modes:
`none` for text-only review packets and `vue-vite` for landing-page previews
published through the preview repository. The design must stay composable so
future renderers such as LaTeX/PDF can be added without rewriting the train
state machine.

This goal touches SkillOpt train metadata, option generation prompts, preview
rendering/publishing, GitHub feedback publishing, review-repo enforcement,
tests, docs, and the Gitmoot skill references. Do not implement LaTeX, image,
Storybook, notebook, or other preview types in this goal.

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

## Final Response After All Tasks

- List completed tasks.
- For each task, list branch, PR URL, merge status, and merged commit hash.
- List tests/checks run.
- Include exact final raw `codex exec review --uncommitted` output for the last
  task/repo.
- Mention skipped checks, blockers, or residual risk.
- Do not claim interactive `/review` is clean. Say:
  `codex exec review is clean; ready for manual /review.`

## Design Direction

Use two composable layers:

```text
generated option
  -> renderer adapter
  -> preview bundle
  -> publisher adapter
  -> review packet
```

Renderer adapters convert generated artifacts into reviewable output. Implement
only:

- `none`: no rendered preview; GitHub issue content is enough.
- `vue-vite`: generated option must provide a machine-readable Vue/Vite file
  bundle that can be built and published.

Publisher adapters decide where reviewable output lives. Implement only:

- `none`: no preview publication.
- `github-pages`: copy built static output into the configured preview repo
  under a deterministic route, commit, push, and store public URLs.

For a train run with `preview.mode: required`, Gitmoot must not publish a human
review packet until every generated option has a preview URL. Inline artifact
fallback is allowed only for `preview.mode: none` or `preview.mode: optional`.

## Implementation Tasks

### Task 1: Add Preview Policy And Expected Review Repo

Scope:

- Add a small preview-policy model in the SkillOpt train layer, using session
  metadata for backward compatibility unless explicit columns are clearly
  simpler after inspection.
- Supported fields:
  - `preview.mode`: `none`, `optional`, or `required`
  - `preview.renderer`: `none` or `vue-vite`
  - `preview.publisher`: `none` or `github-pages`
  - `preview.repo`
  - `preview.route_template`
  - `review.expected_repo`
- `train start` should derive safe defaults:
  - no `--preview-repo` and no preview flags: `mode=none`,
    `renderer=none`, `publisher=none`, `expected_review_repo=<target repo>`.
  - `--preview-repo` present: `mode=required`, `renderer=vue-vite`,
    `publisher=github-pages`, `preview.repo=<preview repo>`,
    `expected_review_repo=<preview repo>`.
- Add explicit flags if needed, but keep the initial public surface small:
  `--preview-mode`, `--preview-renderer`, `--preview-publisher`,
  `--preview-route-template`.
- Reject invalid combinations, including `mode=required` with
  `renderer=none`, `publisher=none`, or missing preview repo.
- Show the resolved preview policy and expected review repo in
  `train start --dry-run`, `train start`, and `train status`.

Acceptance criteria:

- Existing train sessions without preview metadata load as `mode=none`.
- Starting with `--preview-repo owner/previews` resolves to required Vue/Vite
  previews and expected review repo `owner/previews`.
- Starting without a preview repo keeps text-only behavior and expected review
  repo equal to the target repo.
- Invalid preview-policy combinations fail before writing state.

Tests/checks:

- `GOTOOLCHAIN=go1.26.2 go test ./internal/skillopt`
- `GOTOOLCHAIN=go1.26.2 go test ./internal/cli -run 'SkillOptTrain.*Start|SkillOptTrain.*Status'`
- `git diff --check`
- `codex exec review --uncommitted`

Suggested branch:

- `task/skillopt-preview-policy`

Suggested commit message:

- `feat(skillopt): add train preview policy`

### Task 2: Enforce Review Repository Selection

Scope:

- Update low-level `skillopt feedback github publish` and `sync` so runs that
  belong to a train iteration resolve the expected review repo from the train
  session policy.
- If the user supplies `--repo` and it differs from `review.expected_repo`,
  fail with a clear error instead of publishing.
- If `--repo` is omitted for a train run, use `review.expected_repo`.
- Preserve existing low-level behavior for non-train eval runs.
- Add tests for both publish and sync to prevent review packets for preview
  runs from being posted to the target repo by mistake.

Acceptance criteria:

- A train run with preview repo `owner/previews` cannot be published or synced
  against `owner/product`.
- The same train run publishes to `owner/previews` when `--repo` is omitted or
  matches explicitly.
- A text-only train run defaults to the target repo unless an explicit review
  repo is configured.
- Non-train low-level review runs keep the documented repo-resolution order.

Tests/checks:

- `GOTOOLCHAIN=go1.26.2 go test ./internal/cli -run 'SkillOptFeedbackGitHub|SkillOptTrain'`
- `GOTOOLCHAIN=go1.26.2 go test ./internal/feedback`
- `git diff --check`
- `codex exec review --uncommitted`

Suggested branch:

- `task/skillopt-review-repo-guard`

Suggested commit message:

- `fix(skillopt): enforce train review repository`

### Task 3: Add Vue/Vite Preview Bundle Contract

Scope:

- Add a machine-readable preview bundle contract for generated options when
  `preview.renderer=vue-vite`.
- Update the option-generation prompt so temporary agents must return a
  structured Vue/Vite file bundle, not prose-only markdown, when previews are
  required.
- Keep the contract small and parseable. Suggested shape:

  ```json
  {
    "renderer": "vue-vite",
    "files": [
      {"path": "package.json", "content": "..."},
      {"path": "index.html", "content": "..."},
      {"path": "src/main.js", "content": "..."},
      {"path": "src/App.vue", "content": "..."}
    ],
    "build_command": "npm run build",
    "dist_dir": "dist"
  }
  ```

- Add parser/validator helpers that reject unsafe paths, missing required files,
  empty content, unsupported renderer names, and bundles without a build output
  contract.
- Store parsed preview-bundle metadata with the generated option. Do not store
  secrets, local absolute paths, dependency caches, or built outputs in the
  Gitmoot database.
- For `preview.mode=required`, generation should fail if any option does not
  return a valid bundle.

Acceptance criteria:

- Vue/Vite preview-required generation prompts explicitly request the bundle
  contract.
- Valid bundles parse and attach to the generated option metadata.
- Invalid or prose-only option output fails generation for required previews
  instead of creating an unreviewable packet.
- Text-only runs are not required to return preview bundles.

Tests/checks:

- `GOTOOLCHAIN=go1.26.2 go test ./internal/skillopt`
- `GOTOOLCHAIN=go1.26.2 go test ./internal/cli -run 'SkillOptTrain.*Generate|SkillOptTrain.*Preview'`
- `git diff --check`
- `codex exec review --uncommitted`

Suggested branch:

- `task/skillopt-vue-preview-contract`

Suggested commit message:

- `feat(skillopt): add vue preview bundle contract`

### Task 4: Build And Publish Vue/Vite Previews

Scope:

- Add a `vue-vite` renderer that writes each parsed bundle to a temp work
  directory, installs dependencies using the repository's existing safe command
  pattern, runs the configured build command, and verifies the configured
  `dist_dir` exists with an `index.html`.
- Add a `github-pages` publisher that writes built output into the configured
  preview repo checkout using a deterministic route:

  ```text
  runs/{run_id}/{item_id}/{option_label}/
  ```

- Resolve the preview repo checkout from Gitmoot repo registration. If the
  preview repo has no checkout path, fail with an actionable message such as:
  `run gitmoot repo add owner/previews --path /path/to/checkout`.
- Commit and push only the generated preview output in the preview repo. Do not
  touch unrelated files. Do not commit temp work directories, logs, caches, or
  `node_modules`.
- Store the resulting public URL in each option's metadata as `preview_url`.
- Preserve a route-template hook for future publishers, but do not overbuild
  non-GitHub-Pages publishers in this task.

Acceptance criteria:

- For a generated Vue/Vite bundle, Gitmoot publishes a static page under the
  preview repo and stores a usable `preview_url`.
- Missing preview repo checkout, dirty preview repo, build failure, missing
  `index.html`, or push failure stops with a clear error and does not publish a
  misleading review packet.
- Re-running after a successful publish is idempotent or updates the same route
  deterministically.
- The target product repo is not modified by preview publication.

Tests/checks:

- Unit tests for route construction, checkout validation, safe path handling,
  and metadata updates.
- Focused integration-style test with a fake preview repo and a tiny Vue/Vite
  fixture where practical.
- Manual smoke with a local temp Git repo if network/push is not appropriate in
  tests.
- `GOTOOLCHAIN=go1.26.2 go test ./internal/cli -run 'SkillOptTrain.*Preview|SkillOptTrain.*Continue'`
- `GOTOOLCHAIN=go1.26.2 go test ./...`
- `git diff --check`
- `codex exec review --uncommitted`

Suggested branch:

- `task/skillopt-vue-preview-publish`

Suggested commit message:

- `feat(skillopt): publish vue train previews`

### Task 5: Wire Preview Publishing Into Train Continue

Scope:

- Update `gitmoot skillopt train continue` so `options_generated` is no longer
  a dead end.
- For `preview.mode=required`, `continue` should:
  1. render and publish missing previews,
  2. verify every option has a `preview_url`,
  3. publish the human review packet to `review.expected_repo`,
  4. persist issue/PR metadata on the train iteration,
  5. transition session and iteration to `review_published`.
- For `preview.mode=none`, `continue` should publish the existing GitHub review
  packet to the expected review repo and allow inline/text content.
- For `preview.mode=optional`, use preview URLs when present, but allow inline
  fallback when preview rendering is unavailable.
- If `preview.mode=required`, disable inline fallback and fail before GitHub
  publication when preview URLs are missing.
- Add recovery metadata for in-progress preview publishing and GitHub packet
  publication so interrupted runs can be retried without duplicating issues or
  corrupting preview routes.

Acceptance criteria:

- A landing-page train run with `--preview-repo owner/previews` can progress:
  `items_ready -> options_generated -> review_published`.
- The review issue links to preview URLs instead of embedding large code
  blocks.
- The review issue is created in the expected preview/review repo without
  manually passing `--repo`.
- Interrupted runs can be retried without stale resource locks or duplicate
  issue creation.
- Text-only runs still publish review packets without preview URLs.

Tests/checks:

- `GOTOOLCHAIN=go1.26.2 go test ./internal/cli -run 'SkillOptTrain.*Continue|SkillOptFeedbackGitHub'`
- `GOTOOLCHAIN=go1.26.2 go test ./internal/feedback`
- `GOTOOLCHAIN=go1.26.2 go test ./...`
- `scripts/skillopt-train-smoke.sh`
- `git diff --check`
- `codex exec review --uncommitted`

Suggested branch:

- `task/skillopt-train-preview-continue`

Suggested commit message:

- `feat(skillopt): publish train review previews`

### Task 6: Update Docs, Skill References, And Smoke Coverage

Scope:

- Update docs and skill references to explain:
  - preview modes: `none`, `optional`, `required`
  - currently implemented renderer/publisher pairs: `none/none` and
    `vue-vite/github-pages`
  - required preview repo registration/checkouts
  - review-repo enforcement
  - why required previews block inline fallback
  - the landing-page training command sequence
- Update `scripts/skillopt-train-smoke.sh` or add a focused smoke script so the
  basic flow covers expected review repo enforcement and required-preview
  blocking.
- Add a documented manual smoke for a real `jerryfane/gitmoot-previews` style
  repo:

  ```sh
  gitmoot repo add owner/previews --path /path/to/previews
  gitmoot skillopt train start --preview-repo owner/previews ...
  gitmoot skillopt train continue --session landing-page-train
  gitmoot skillopt train continue --session landing-page-train
  ```

- Document that LaTeX/PDF and other preview types are intentionally future
  adapters, not part of this goal.

Acceptance criteria:

- A new user can understand when previews are required and how to configure the
  preview repo.
- Docs no longer imply `preview_repo` alone publishes previews.
- The smoke test catches the wrong-repo class of bug.
- The smoke test catches required-preview runs that would otherwise publish
  inline code blocks.

Tests/checks:

- `scripts/skillopt-train-smoke.sh`
- `GOTOOLCHAIN=go1.26.2 go test ./...`
- `(cd website && npm run build)`
- `git diff --check`
- `codex exec review --uncommitted`

Suggested branch:

- `task/skillopt-preview-docs-smoke`

Suggested commit message:

- `docs(skillopt): document train preview publishing`
