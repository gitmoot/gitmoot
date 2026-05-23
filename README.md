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
gitmoot daemon start [--repo owner/repo] [--poll 30s]
gitmoot daemon run [--repo owner/repo] [--poll 30s]
gitmoot daemon stop|restart|status|logs
gitmoot preset list
gitmoot preset show|update|diff thermo-nuclear-code-quality-review
gitmoot agent subscribe <name> --runtime codex|claude|shell --session <id|name|last|command> --role <role> --repo owner/repo --capability <capability>
gitmoot agent subscribe thermo-review --runtime codex --session <session-id-or-last> --repo owner/repo --preset thermo-nuclear-code-quality-review
gitmoot agent allow|deny|repos
gitmoot agent list
gitmoot agent doctor <name>
gitmoot agent remove <name>
```

```text
gitmoot status
gitmoot events --repo owner/repo
gitmoot goal import --file <path>
gitmoot task run <id> --repo owner/repo --owner <agent>
gitmoot job list|show|events|run|retry|cancel
gitmoot lock list|show|release
```

Agents should read [SKILL.md](SKILL.md) for the Gitmoot job contract, branch
lock expectations, and safe behavior rules.

## Thermo Review Preset

Gitmoot includes one built-in preset agent profile,
`thermo-nuclear-code-quality-review`, for strict review-only work. Preset
content is fetched explicitly and cached locally, then snapshotted into each
queued job so retries remain reproducible.

```sh
gitmoot preset update thermo-nuclear-code-quality-review
gitmoot agent subscribe thermo-review \
  --runtime codex \
  --session <session-id-or-last> \
  --repo owner/repo \
  --preset thermo-nuclear-code-quality-review
gitmoot agent doctor thermo-review
```

Ask it to review from a PR comment:

```text
/gitmoot thermo-review review
```

Check upstream changes before refreshing the cached preset:

```sh
gitmoot preset diff thermo-nuclear-code-quality-review
gitmoot preset update thermo-nuclear-code-quality-review
```

## Documentation

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
