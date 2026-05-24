# Gitmoot Workflows

## First Repo Setup

1. Confirm the repo identity and GitHub auth.
2. Start the daemon for the repo.
3. Start or subscribe at least one agent.
4. Verify the agent is healthy before asking PR comments to route jobs.

```sh
gh repo view --json nameWithOwner
gh auth status
gitmoot daemon start --repo owner/repo
gitmoot agent start reviewer --runtime codex --repo owner/repo --role reviewer --capability ask --capability review --start-daemon
gitmoot agent doctor reviewer
```

## Review Agent From A PR Comment

1. Register a reviewer agent for the target repo.
2. Ensure the daemon watches the same repo.
3. Comment on the PR.
4. Inspect job status if no result appears.

```text
/gitmoot reviewer review
```

```sh
gitmoot job list --repo owner/repo
gitmoot job show <job-id>
gitmoot job events <job-id>
```

## Built-In Thermo Review Agent

Use the thermo preset for strict review-only work. It should not implement code
or request implementation capability.

```sh
gitmoot preset update thermo-nuclear-code-quality-review
gitmoot agent start thermo-review --runtime codex --repo owner/repo --preset thermo-nuclear-code-quality-review --start-daemon
```

PR comment:

```text
/gitmoot thermo-review review
```

## Custom Prompt Agent

Use custom prompt presets for project-specific reviewers or helpers.

```sh
mkdir -p agents
printf '%s\n' 'Review frontend changes for correctness and responsive behavior.' > agents/frontend-reviewer.md
gitmoot preset add frontend-reviewer --file agents/frontend-reviewer.md
gitmoot agent start frontend-reviewer \
  --runtime codex \
  --repo owner/repo \
  --preset frontend-reviewer \
  --role reviewer \
  --capability ask \
  --capability review
```

Custom preset content is snapshotted into local Gitmoot state. After editing the
source prompt file, run `gitmoot preset diff <id>` and `gitmoot preset update
<id>` before expecting new jobs to use the changed prompt.

## Multi-Repo Work

Agents are global identities with explicit per-repo access. When working across
multiple repos, always pass `--repo owner/repo` to status, daemon, job, and event
commands so jobs are routed in the correct repository context.

```sh
gitmoot agent allow reviewer --repo owner/project-a
gitmoot agent allow reviewer --repo owner/project-b
gitmoot status --repo owner/project-a
gitmoot status --repo owner/project-b
```
