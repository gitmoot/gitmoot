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
gitmoot agent template update planner
gitmoot agent prompt planner
gitmoot agent ask project-planner --repo owner/project --background "Say OK."
gitmoot job watch <job-id>
gitmoot job events <job-id>
```

For concurrency checks, keep `--workers 1` by default and raise it only when
jobs use independent runtime sessions or a managed agent type with
`max_background` greater than one.

## Template Capture Smoke

This smoke does not need a live background agent:

```sh
export GITMOOT_SMOKE_HOME="$(mktemp -d)"
export GITMOOT_DRAFT_FILE="$GITMOOT_SMOKE_HOME/release-planner.md"
gitmoot init --home "$GITMOOT_SMOKE_HOME"
gitmoot agent template draft release-planner --home "$GITMOOT_SMOKE_HOME" --output "$GITMOOT_DRAFT_FILE"
```

Then ask Codex or Claude Code to fill the draft from visible current-chat
context:

```text
Use Gitmoot to capture this session as agent template release-planner. Draft only.
```

After reviewing the filled draft:

```sh
gitmoot agent template validate "$GITMOOT_DRAFT_FILE"
gitmoot agent template add release-planner --home "$GITMOOT_SMOKE_HOME" --file "$GITMOOT_DRAFT_FILE"
gitmoot agent template show --home "$GITMOOT_SMOKE_HOME" release-planner
gitmoot agent prompt release-planner --home "$GITMOOT_SMOKE_HOME"
```

Expected signal: the chat fills the draft and does not start a daemon, queue a
job, or install the template until explicitly asked.

For the detailed release smoke path, see
[`docs/beta-smoke-tests.md`](https://github.com/jerryfane/gitmoot/blob/main/docs/beta-smoke-tests.md).
