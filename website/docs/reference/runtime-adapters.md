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

## Metadata Registry

Each built-in runtime carries declarative metadata — advertised capabilities, a
declared default model, an advisory list of known-valid models, and a descriptor of
where token usage is read from — seeded from compiled defaults that reproduce
Gitmoot's historical behavior. This metadata is **inspection-only**: it is surfaced
by `gitmoot runtime list` (add `--json` for machine output) and is never consulted
at job delivery.

Operators can override a built-in runtime's recorded metadata **without
recompiling** via a `[runtimes.<name>]` section in `config.toml`:

```toml
[runtimes.codex]
default_model = "gpt-5.5-codex"
models = ["gpt-5.5-codex", "gpt-5.4-codex"]
capabilities = ["review", "implement", "ask"]
```

This is inspection-only metadata — adapter *behavior* (auth, sandbox, session
resume, stream parsing, **and which model a job actually runs on**) stays in Go and
is never consulted at delivery. Setting `default_model` does **not** retarget the
model a job uses (a job's model still comes from the agent/job `--model` or the
runtime CLI's own config); `models` is advisory (Gitmoot never rejects a `--model`
based on it); and `capabilities` gates nothing at dispatch. With no `[runtimes.*]`
section behavior is byte-identical. The section can only tweak a **built-in**
runtime; adding a new first-class runtime is a code change, and an unknown runtime
name is a config error.

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
