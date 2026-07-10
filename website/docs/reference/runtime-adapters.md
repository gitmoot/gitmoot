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
  it inherits the session. **Very large prompts (≥100 KiB):** the Kimi CLI takes
  the prompt only as a single `-p` argument, and any single process argument above
  the kernel's `MAX_ARG_STRLEN` (~128 KiB) fails to launch with `fork/exec:
  argument list too long`. When a rendered prompt reaches the 100 KiB safety
  threshold, Gitmoot stages it to a file in a dedicated temporary directory,
  grants that directory to the Kimi session with `--add-dir` (so Kimi's
  workspace-scoped file-read tool can open it), and passes a short instruction
  telling the agent to read that file as its full task, keeping the launch under
  the limit. Normal-size prompts are passed verbatim as before, unchanged. (The
  Claude and Codex adapters pass the prompt as an argv argument too, but their
  CLIs can also read it from stdin, so they have a native escape hatch.)
- **Kimi CLI (legacy)** is the opt-in `--runtime kimi-cli` adapter (#546) for
  the **older** Kimi CLI, which requires the `--print` command shape the
  current Kimi Code CLI does not support. It is intentionally separate from
  `kimi` so the default Kimi Code path is never probed or changed. Choose
  `kimi` unless you specifically run the legacy CLI; the two count as the same
  runtime *family* for cross-family review.
- **Shell** invokes a configured shell command and is mainly for smoke tests,
  demos, and adapter contract checks.

## Metadata Registry

Each built-in runtime carries declarative metadata — advertised capabilities,
default model and effort values, an advisory list of known-valid models, and a
descriptor of where token usage is read from — seeded from compiled defaults
that reproduce Gitmoot's historical behavior. All of it is surfaced by
`gitmoot runtime list` (add `--json` for machine output). Two fields are
**behavioral**: `default_model` and `default_effort`. Every other field is
inspection-only.

Operators can override a built-in runtime's recorded metadata **without
recompiling** via a `[runtimes.<name>]` section in `config.toml`:

```toml
[runtimes.codex]
default_model = "gpt-5.5-codex"
default_effort = "high"
models = ["gpt-5.5-codex", "gpt-5.4-codex"]
capabilities = ["review", "implement", "ask"]
```

`default_model` is the fallback when neither the job nor agent pins `--model`:
job/agent model, then `default_model`, then the runtime CLI's own default.
`default_effort` follows the same precedence after job/agent `--effort`. Codex
receives the resolved value as `-c model_reasoning_effort=<value>`; Claude and
Kimi do not expose a reasoning-effort surface, so it is a no-op for those
adapters. With both defaults unset, no model or effort is forced.

Every other field is inspection-only: `models` is advisory (Gitmoot never
rejects a `--model` based on it); `capabilities` gates nothing at dispatch; and
adapter *behavior* (auth, sandbox, session resume, stream parsing) always stays
in Go. With no `[runtimes.*]` section behavior is byte-identical. The section can
only tweak a **built-in** runtime; adding a new first-class runtime is a code
change, and an unknown runtime name is a config error.

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
