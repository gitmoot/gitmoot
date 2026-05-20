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
gitmoot doctor --repo .
gitmoot daemon start --repo owner/repo --poll 30s
gitmoot agent subscribe <name> --runtime codex|claude|shell --session <id|name|last|command> --role <role> --repo owner/repo --capability <capability>
gitmoot agent list
gitmoot agent doctor <name>
gitmoot agent remove <name>
```

```text
gitmoot status
gitmoot goal import --file <path>
gitmoot task run <id> --repo owner/repo --owner <agent>
```

## Documentation

- [Local workflow walkthrough](docs/local-workflow.md)
- [Runtime adapter authoring](docs/adapters.md)
- [Troubleshooting](docs/troubleshooting.md)

## Development

```sh
go test ./...
go vet ./...
```
