# Gitmoot Plugins And Install Helper Goal

Implement Gitmoot Codex and Claude Code plugin packaging plus a local
`gitmoot plugin` install helper task by task. Each task must be developed,
reviewed, opened as its own pull request, merged, and verified before moving on,
unless tasks are explicitly safe to run in parallel.

Gitmoot is a local-first Go binary that coordinates AI agent sessions through
GitHub pull requests. This goal makes Gitmoot easier for agents to discover and
use from Codex and Claude Code by packaging the existing Gitmoot Agent Skill as
runtime plugins and by adding CLI helpers to build, install, validate, and
diagnose those plugin packages.

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
- Runtime-specific behavior must live behind clear plugin/runtime boundaries.
  Do not hardcode Codex or Claude Code plugin details into workflow, GitHub,
  daemon, database, or merge-gate packages.
- GitHub PR comments remain the public audit trail. Local SQLite state remains
  the workflow source of truth.
- Do not add MCP, webhook, hosted service, or tmux support in this goal.
- Do not change daemon job routing, runtime resume behavior, agent presets,
  branch locks, merge gates, or result-comment semantics in this goal.
- Do not duplicate the tracked Gitmoot skill content. Generate plugin packages
  from the canonical `skills/gitmoot` directory.

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
   - root command dispatch in `internal/cli/root.go`
   - config path helpers in `internal/config`
   - subprocess runner abstractions in `internal/subprocess`
   - existing CLI tests in `internal/cli/*_test.go`
   - canonical skill package in `skills/gitmoot`
5. Verify PR tooling is available before the first PR:
   - `gh auth status`
   - repo remote resolves to the expected GitHub repository
6. Verify local plugin/runtime tooling before plugin implementation or smoke
   tests:
   - `codex --version`
   - `codex plugin --help`
   - `codex plugin add --help`
   - `codex plugin marketplace add --help`
   - `claude --version`
   - `claude plugin --help`
   - `claude plugin install --help`
   - `claude plugin marketplace add --help`
   - `claude plugin validate --help`
7. Verify official plugin contracts before editing:
   - Codex plugin build and marketplace docs:
     `https://developers.openai.com/codex/plugins/build`
   - Codex plugin docs:
     `https://developers.openai.com/codex/plugins`
   - Codex slash command docs:
     `https://developers.openai.com/codex/cli/slash-commands`
   - Claude Code plugin docs:
     `https://code.claude.com/docs/en/plugins`
   - Claude Code plugin reference:
     `https://code.claude.com/docs/en/plugins-reference`
   - Claude Code plugin marketplace docs:
     `https://code.claude.com/docs/en/plugin-marketplaces`
   - Claude Code slash command docs:
     `https://code.claude.com/docs/en/slash-commands`

## Per-Task Branch Workflow

1. Confirm the current task's scope.
2. Create a task branch from the latest target base branch.
3. Implement only that task.
4. Add or update focused tests/checks appropriate to the task.
5. Run focused tests for touched modules.
6. Run broader checks when the task touches shared behavior, CLI/API surfaces,
   config paths, subprocess calls, installer behavior, docs, or user-facing
   workflows.
7. For CLI, subprocess, installer, generated-package, external-tool, or plugin
   validation changes, include an operational smoke test or direct contract
   check. Syntax checks alone are not enough.
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

## Product Decisions

- Gitmoot CLI remains the engine. Plugins are discovery, packaging, and agent
  UX surfaces only.
- The canonical skill source is `skills/gitmoot`. Plugin packages must copy
  that directory at build/install time instead of maintaining duplicate tracked
  skill files.
- Generated plugin packages live under Gitmoot home by default:
  `<gitmoot-home>/plugins/build/<runtime>/gitmoot`.
- Generated marketplace roots live under Gitmoot home by default:
  `<gitmoot-home>/plugins/marketplaces/<runtime>`.
- The stable plugin name is `gitmoot`.
- The local marketplace name is `gitmoot-local`.
- Codex plugin packages use `.codex-plugin/plugin.json`.
- Claude Code plugin packages use `.claude-plugin/plugin.json`.
- Codex should expose Gitmoot through the bundled skill. Do not invent custom
  Codex slash commands in this goal.
- Claude should expose Gitmoot through the bundled namespaced skill. Do not add
  extra Claude command files unless official validation proves they are required
  for plugin usability.
- Agents should not silently install Gitmoot. The plugin skill should check
  `gitmoot version` and give the human the exact install command if missing.
- Missing Codex or Claude runtime CLIs should produce clear diagnostic output,
  not partial silent success.

## Public Interface

Add:

```text
gitmoot plugin build codex|claude [--out <dir>] [--force] [--home <path>]
gitmoot plugin install codex|claude [--scope user|project|local] [--force] [--home <path>]
gitmoot plugin doctor [codex|claude] [--json] [--home <path>]
gitmoot plugin path codex|claude [--home <path>]
```

Behavior:
- `build` renders a plugin package for one runtime and refuses to overwrite an
  existing destination unless `--force` is present.
- `install codex` builds the Codex package, writes or updates a local Codex
  marketplace manifest, then invokes Codex plugin marketplace/install commands
  when `codex` is available.
- `install claude` builds the Claude package, writes or updates a local Claude
  marketplace manifest, then invokes Claude marketplace/install commands when
  `claude` is available.
- `doctor` reports package path, package existence, manifest validity, copied
  skill presence, runtime CLI availability, marketplace path, and validation or
  next-step hints.
- `path` prints the default generated plugin package path for the selected
  runtime.
- `--scope` is accepted for both runtimes for a consistent user interface.
  Claude passes it to Claude plugin commands. Codex should ignore it with a
  clear note if Codex does not support scope at install time.
- All commands must support `--home` consistently with existing Gitmoot command
  patterns.

## Implementation Tasks

### Task 1: Add Plugin Package Builder

Create a reusable package builder that renders runtime-specific plugin packages
from the canonical Gitmoot skill.

Requirements:
- Add `internal/pluginpack` or a similarly focused package.
- Add a provider enum or equivalent for `codex` and `claude`.
- Add build options for:
  - runtime/provider
  - Gitmoot home paths
  - output directory
  - overwrite behavior
  - version/build metadata
- Copy `skills/gitmoot` into generated package output as
  `skills/gitmoot`.
- Generate Codex package metadata at `.codex-plugin/plugin.json`.
- Generate Claude package metadata at `.claude-plugin/plugin.json`.
- Manifest content must be valid JSON, stable, and deterministic.
- Manifests must include:
  - name: `gitmoot`
  - display name or title: `Gitmoot`
  - description: local-first GitHub PR agent coordination
  - version from `buildinfo.Current()` when available
  - repository URL: `https://github.com/jerryfane/gitmoot`
  - license: `MIT`
- Refuse to build when the canonical skill directory is missing, empty, or does
  not include `SKILL.md`.
- Keep recursive copy, directory cleanup, JSON write, and path normalization
  logic in one helper path.
- Do not write into tracked repo plugin output by default.

Tests/checks:
- Unit test building Codex and Claude packages into temp directories.
- Assert both packages include the copied canonical `skills/gitmoot/SKILL.md`
  and reference files.
- Assert manifests are valid JSON and include stable metadata.
- Assert existing destinations are not overwritten without force.
- Assert force rebuild replaces stale generated content.
- Run focused package tests.

Commit message:

```text
feat: add plugin package builder
```

### Task 2: Add Plugin Build, Path, And Doctor CLI

Expose package rendering and diagnostics through the Gitmoot CLI.

Requirements:
- Add `plugin` to the root command list.
- Implement:
  - `gitmoot plugin build codex|claude`
  - `gitmoot plugin path codex|claude`
  - `gitmoot plugin doctor [codex|claude]`
- Follow existing CLI parsing, output, and exit-code style.
- `plugin build` prints the generated package path.
- `plugin path` prints only the package path, suitable for shell scripts.
- `plugin doctor` should be readable by humans and support `--json`.
- Doctor checks should include:
  - Gitmoot home paths resolve
  - canonical skill exists
  - generated package exists
  - manifest JSON is readable
  - copied skill exists in generated package
  - `codex` or `claude` CLI is available when that runtime is requested
  - package validation command is available when the runtime supports it
- Doctor should return non-zero only when the selected runtime is explicitly
  unhealthy. General all-runtime doctor can report warnings for missing runtimes
  without failing if at least one supported runtime is usable.

Tests/checks:
- CLI tests for usage, bad runtime, path output, build output, and JSON doctor
  output.
- Tests must use temp homes and temp output paths.
- Do not mutate the real user Codex or Claude config in tests.
- Run focused CLI tests.

Commit message:

```text
feat: add plugin build and doctor commands
```

### Task 3: Add Plugin Install Helper

Install generated packages into local Codex and Claude plugin marketplaces.

Requirements:
- Implement:
  - `gitmoot plugin install codex`
  - `gitmoot plugin install claude`
- Generate or update a local marketplace root under Gitmoot home for the
  selected runtime.
- Codex install flow:
  - build package under Gitmoot home
  - write/update a Codex-compatible marketplace manifest named `gitmoot-local`
  - call `codex plugin marketplace add <marketplace-root>`
  - call `codex plugin add gitmoot@gitmoot-local`
  - if Codex is missing, keep generated files and print exact manual next
    steps instead of failing after destructive partial work
- Claude install flow:
  - build package under Gitmoot home
  - write/update a Claude-compatible marketplace manifest named `gitmoot-local`
  - call `claude plugin marketplace add <marketplace-root> --scope <scope>`
  - call `claude plugin install gitmoot@gitmoot-local --scope <scope>`
  - run `claude plugin validate <package-path>` before install when available
  - if Claude is missing, keep generated files and print exact manual next
    steps instead of failing after destructive partial work
- Use an injectable subprocess runner for install logic so tests can assert
  commands without invoking real runtimes.
- Preserve idempotency. Re-running install should update generated package and
  marketplace metadata without duplicating entries.
- Respect `--force` for package overwrite.

Tests/checks:
- Fake-runner tests for Codex marketplace/install command order.
- Fake-runner tests for Claude validation/marketplace/install command order.
- Marketplace manifest idempotency tests.
- Missing-runtime tests with clear next-step output.
- Focused install tests must not write to real Codex or Claude config.

Commit message:

```text
feat: add plugin install helper
```

### Task 4: Update Skill And Documentation

Document how humans and agents install and use Gitmoot plugins.

Requirements:
- Update `README.md` command surface with:

  ```text
  gitmoot plugin build codex|claude
  gitmoot plugin install codex|claude
  gitmoot plugin doctor [codex|claude]
  gitmoot plugin path codex|claude
  ```

- Add or update `docs/plugins.md` with:
  - what the plugins do and do not do
  - install Gitmoot:
    `curl -fsSL https://gitmoot.io/install.sh | sh`
  - install Codex plugin:
    `gitmoot plugin install codex`
  - install Claude plugin:
    `gitmoot plugin install claude`
  - verify:
    `gitmoot plugin doctor`
  - use from Codex through plugin/skill discovery
  - use from Claude through the namespaced Gitmoot skill
  - troubleshooting for missing runtime CLI, stale package, and validation
    failures
- Update `docs/local-workflow.md` only where plugin setup improves the existing
  user flow.
- Update `docs/beta-smoke-tests.md` with plugin build/install smoke tests.
- Update `skills/gitmoot/references/CLI.md` with plugin commands and the short
  install-helper flow.
- Keep the root `SKILL.md` compatible with `gitmoot.io/SKILL.md`. Do not remove
  its existing OpenClaw metadata.

Tests/checks:
- Run skill/docs related tests if present.
- Verify docs commands match implemented CLI flags.
- If Go tests are unavailable, at least run `git diff --check` and inspect docs
  links/commands manually.

Commit message:

```text
docs: document gitmoot plugin installation
```

### Task 5: Live Smoke, Polish, And Release Readiness

Run the end-to-end validation path and fix any integration issues found.

Requirements:
- Build Codex plugin into a temp output directory.
- Build Claude plugin into a temp output directory.
- Run Claude validation when available:

  ```sh
  claude plugin validate <temp-claude-plugin-path>
  ```

- Run non-mutating Codex plugin checks where available:

  ```sh
  codex plugin marketplace list
  codex plugin list
  ```

- Run Gitmoot plugin diagnostics:

  ```sh
  gitmoot plugin doctor
  gitmoot plugin doctor codex
  gitmoot plugin doctor claude
  ```

- If safe in the current environment, install into an isolated Gitmoot home and
  avoid mutating the real user plugin state unless explicitly approved.
- Confirm generated output is not tracked by Git unless intentionally committed
  as docs or tests.
- Fix integration issues found by smoke tests before opening the PR.

Tests/checks:
- `git diff --check`
- focused Go tests for touched packages
- `go test ./...` and `go vet ./...` when the local Go toolchain satisfies
  `go.mod`
- if Go is blocked by the local toolchain, record the exact blocker in the PR
  body and run all available non-Go validation
- `codex exec review --uncommitted`

Commit message:

```text
test: add plugin smoke coverage
```

## Final Response After All Tasks

- List completed tasks.
- For each task, list branch, PR URL, merge status, and merged commit hash.
- List tests/checks run.
- Include exact final raw `codex exec review --uncommitted` output for the last
  task/repo.
- Mention skipped checks, blockers, or residual risk.
- Do not claim interactive `/review` is clean. Say:
  `codex exec review is clean; ready for manual /review.`
