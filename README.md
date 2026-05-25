# Gitmoot

Gitmoot is a local-first multi-agent orchestration tool for GitHub pull request
workflows. It coordinates persistent AI agent sessions running on a user's
machine and uses GitHub PRs as the public audit trail.

V1 is intentionally local-only:

```text
GitHub PR comments/state
  -> local gitmoot daemon
  -> local SQLite state machine and job mailbox
  -> registered runtime adapter
  -> Codex, Claude Code, or another agent runtime
  -> GitHub PR comments, statuses, branches, PRs, and merges
```

The core primitive is a runtime-neutral Gitmoot agent, not a Codex-specific
session. Codex and Claude Code are adapters behind the same internal runtime
contract.

## Current Command Surface

```text
gitmoot init
gitmoot setup --repo owner/repo --path . --agent <name> --runtime codex|claude|shell --session <ref>
gitmoot doctor --repo .
gitmoot config path|show
gitmoot version [--json]
gitmoot update --check
gitmoot update [--restart-daemon]
gitmoot plugin build codex|claude
gitmoot plugin install codex|claude
gitmoot plugin doctor [codex|claude]
gitmoot plugin path codex|claude
gitmoot daemon start [--repo owner/repo] [--poll 30s]
gitmoot daemon run [--repo owner/repo] [--poll 30s]
gitmoot daemon stop|restart|status|logs
gitmoot preset list
gitmoot preset add <preset-id> --file ./agents/<preset-id>.md
gitmoot preset show|update|diff <preset-id>
gitmoot goal template
gitmoot goal import --file <path> [--repo owner/repo]
gitmoot agent start <name> --runtime codex|claude --repo owner/repo [--path .] [--preset <preset-id>] [--start-daemon]
gitmoot agent subscribe <name> --runtime codex|claude|shell --session <id|name|last|command> --role <role> --repo owner/repo --capability <capability>
gitmoot agent start planner --runtime codex --repo owner/repo --preset gitmoot-plan-and-goal
gitmoot agent start thermo-review --runtime codex --repo owner/repo --preset thermo-nuclear-code-quality-review
gitmoot agent allow|deny|repos
gitmoot agent list
gitmoot agent doctor <name>
gitmoot agent remove <name>
```

```text
gitmoot status
gitmoot events --repo owner/repo
gitmoot task run <id> --repo owner/repo --owner <agent>
gitmoot job list|show|events|run|retry|cancel
gitmoot lock list|show|release
```

Agents should read the canonical Agent Skills package at
[skills/gitmoot/SKILL.md](skills/gitmoot/SKILL.md) for the Gitmoot job contract,
branch lock expectations, and safe behavior rules. The root [SKILL.md](SKILL.md)
remains as a raw compatibility entrypoint for agents and `gitmoot.io/SKILL.md`.

## Runtime Plugins

Gitmoot can package the canonical Agent Skill for Codex and Claude Code so the
runtime can discover Gitmoot guidance from its plugin system.

```sh
curl -fsSL https://gitmoot.io/install.sh | sh
gitmoot plugin install codex
gitmoot plugin install claude
gitmoot plugin doctor
```

The plugins do not run a hosted service or replace the Gitmoot daemon. They
install the local skill package and point agents back to the `gitmoot` CLI for
repo setup, daemon status, jobs, locks, and PR-comment workflows. See
[docs/plugins.md](docs/plugins.md) for install details and troubleshooting.

## Planner Preset

Gitmoot includes `gitmoot-plan-and-goal` for structured implementation planning
and standard goal-file writing. It uses the canonical goal template from
`gitmoot goal template`, then writes plan tasks as `### Task N: ...` headings so
`gitmoot goal import` can track them.

```sh
gitmoot preset update gitmoot-plan-and-goal
gitmoot agent start planner \
  --runtime codex \
  --repo owner/repo \
  --path . \
  --preset gitmoot-plan-and-goal \
  --start-daemon
```

Ask it from a PR comment:

```text
/gitmoot planner ask Write a task-by-task implementation plan for this feature, then create the goal file prompt.
```

## Thermo Review Preset

Gitmoot includes one built-in preset agent profile,
`thermo-nuclear-code-quality-review`, for strict review-only work. Preset
content is fetched explicitly and cached locally, then snapshotted into each
queued job so retries remain reproducible.

```sh
gitmoot preset update thermo-nuclear-code-quality-review
gitmoot agent start thermo-review \
  --runtime codex \
  --repo owner/repo \
  --preset thermo-nuclear-code-quality-review \
  --start-daemon
gitmoot agent doctor thermo-review
```

Use `agent start` for a new Gitmoot-managed Codex or Claude session. Use
`agent subscribe` when the runtime session already exists, or for shell command
adapters.

Ask it to review from a PR comment:

```text
/gitmoot thermo-review review
```

Check upstream changes before refreshing the cached preset:

```sh
gitmoot preset diff thermo-nuclear-code-quality-review
gitmoot preset update thermo-nuclear-code-quality-review
```

## Custom Prompt Presets

Custom presets let you keep a local prompt file and bind its snapshotted
instructions to any Gitmoot agent.

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

Gitmoot copies the file content into local SQLite when you run `preset add` or
`preset update`. Jobs use that cached snapshot. After editing the prompt file,
run:

```sh
gitmoot preset diff frontend-reviewer
gitmoot preset update frontend-reviewer
```

## Documentation

- [Agent Skills package](skills/gitmoot/SKILL.md)
- [Codex and Claude plugins](docs/plugins.md)
- [Local workflow walkthrough](docs/local-workflow.md)
- [Beta smoke tests](docs/beta-smoke-tests.md)
- [Runtime adapter authoring](docs/adapters.md)
- [Troubleshooting](docs/troubleshooting.md)

## V1 Limits

- Local-only: no hosted dashboard, GitHub App bot identity, cloud runner, or
  remote control plane.
- Polling watches GitHub PRs; there is no webhook receiver in V1.
- GitHub comments are authored by the authenticated `gh` user. Agent identity
  appears in the comment body.

## Development

```sh
go test ./...
go vet ./...
```
