# Runtime Adapters

Runtime adapters keep Gitmoot's workflow logic independent from Codex, Claude
Code, shell, and future runtime details.

An adapter starts sessions when supported, validates agent records, delivers
job prompts, runs health checks, and returns raw output for result parsing.

Current runtime shape:

- **Codex**: starts and resumes sessions through Codex CLI non-interactive
  commands.
- **Claude Code**: uses Claude CLI print/resume style commands when available.
- **Shell**: invokes a configured shell command and is useful for smoke tests.

The full adapter authoring reference lives in
[`docs/adapters.md`](https://github.com/jerryfane/gitmoot/blob/main/docs/adapters.md).

