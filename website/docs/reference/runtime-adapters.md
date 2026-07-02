# Runtime Adapters

Runtime adapters keep Gitmoot workflow logic independent from Codex, Claude
Code, Kimi Code, shell commands, and future runtimes. Gitmoot snapshots the
agent template and rendered job prompt before handing work to an adapter.

## Current Runtimes

- **Codex** starts and resumes sessions through the Codex CLI noninteractive
  commands. Prefer explicit session ids for long-running agents.
- **Claude Code** uses Claude CLI print/resume style commands when available.
  Restart the daemon or runtime session after changing token environment.
- **Kimi Code** starts a session with `kimi -p '<prompt>' --output-format
  stream-json` and resumes or delivers follow-up work with `kimi -S <session-id>
  -p '<prompt>' --output-format stream-json`, parsing the session id from the
  stream-json output. Select it with `gitmoot agent start <name> --runtime
  kimi`. Authenticate once with `kimi login`, then restart the Gitmoot daemon so
  it inherits the session.
- **Kimi CLI (legacy)** is the opt-in `--runtime kimi-cli` adapter (#546) for
  the **older** Kimi CLI, which requires the `--print` command shape the
  current Kimi Code CLI does not support. It is intentionally separate from
  `kimi` so the default Kimi Code path is never probed or changed. Choose
  `kimi` unless you specifically run the legacy CLI; the two count as the same
  runtime *family* for cross-family review.
- **Shell** invokes a configured shell command and is mainly for smoke tests,
  demos, and adapter contract checks.

## Agent Session Values

`RuntimeRef` is runtime-specific:

- Codex accepts a session UUID, thread name, or `last`.
- Claude accepts a UUID or `last`.
- Kimi accepts a session id of the form `session_<uuid>` or an empty value.
- Kimi CLI (legacy) accepts a session UUID or an empty value.
- Shell uses the configured command.

Prefer explicit runtime session ids over `last` for durable agents. Use
`gitmoot agent doctor <name>` after subscribing or starting an agent.

## Runtime Safety

Adapters should pass the rendered Gitmoot prompt through without rewriting
workflow semantics. Gitmoot parses the returned `gitmoot_result` object after
delivery and keeps raw output for diagnostics.

Use the plugin docs for runtime discovery setup:
[Codex And Claude Plugins](../plugins/codex-claude.md). Use troubleshooting
when session validation or resume fails:
[Troubleshooting](../operations/troubleshooting.md).

The full adapter authoring reference lives in
[`docs/adapters.md`](https://github.com/jerryfane/gitmoot/blob/main/docs/adapters.md).
