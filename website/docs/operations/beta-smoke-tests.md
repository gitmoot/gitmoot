# Beta Smoke Tests

Use smoke tests before cutting a beta release or after changing daemon, plugin,
runtime, or job routing behavior.

## Baseline

```sh
git status --short
git remote -v
gh auth status
gitmoot doctor --repo .
```

## Plugin Smoke

```sh
gitmoot plugin build codex
gitmoot plugin build claude
gitmoot plugin doctor
```

## One-Repo Routing Smoke

Register a shell agent, start the daemon, comment on a test PR, then inspect
jobs and PR comments:

```sh
gitmoot job list --repo owner/project
gitmoot events --repo owner/project
gh pr view <number> --repo owner/project --comments
```

## Execution Model Smoke

Use the Gitmoot planner here from the current chat for fast planning, then use
background asks when you need tracked jobs:

```sh
gitmoot agent ask project-planner --repo owner/project --background "Say OK."
gitmoot job watch <job-id>
gitmoot job events <job-id>
```

For concurrency checks, keep `--workers 1` by default and raise it only when
jobs use independent runtime sessions or a managed agent type with
`max_background` greater than one.

For the detailed release smoke path, see
[`docs/beta-smoke-tests.md`](https://github.com/jerryfane/gitmoot/blob/main/docs/beta-smoke-tests.md).
