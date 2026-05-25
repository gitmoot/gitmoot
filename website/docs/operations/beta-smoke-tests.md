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

For the detailed release smoke path, see
[`docs/beta-smoke-tests.md`](https://github.com/jerryfane/gitmoot/blob/main/docs/beta-smoke-tests.md).

