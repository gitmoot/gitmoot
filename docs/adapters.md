# Runtime Adapter Authoring

Gitmoot treats Codex, Claude Code, and shell commands as runtime adapters behind
one interface. Workflow, daemon, and GitHub code should stay runtime-neutral.

## Adapter Contract

An adapter implements `runtime.Adapter`:

```go
type Adapter interface {
    Name() string
    Start(ctx context.Context, request StartRequest) (StartResult, error)
    Validate(ctx context.Context, agent Agent) error
    Deliver(ctx context.Context, agent Agent, job Job) (Result, error)
    Health(ctx context.Context, agent Agent) error
    Capabilities(ctx context.Context) ([]string, error)
}
```

Responsibilities:

- `Name` returns the runtime key used by `gitmoot agent start` and
  `gitmoot agent subscribe`.
- `Start` creates a new runtime session for `gitmoot agent start` and returns
  the runtime reference Gitmoot should store.
- `Validate` checks the agent record without doing unnecessary work.
- `Deliver` resumes or invokes the runtime with the rendered job prompt and
  returns raw output.
- `Health` performs a small operational check that proves the runtime can accept
  a job.
- `Capabilities` advertises actions such as `review`, `implement`, and `ask`.

## Agent Record

Adapters receive a normalized `runtime.Agent`:

```go
type Agent struct {
    Name           string
    Role           string
    Runtime        string
    RuntimeRef     string
    RepoScope      string
    PresetID       string
    Capabilities   []string
    AutonomyPolicy string
    HealthStatus   string
}
```

`RuntimeRef` is runtime-specific. Codex accepts a session UUID, thread name, or
`last`. Claude accepts a UUID or `last`. Shell uses the configured command.
`PresetID` is Gitmoot-owned metadata. Adapters do not fetch or interpret preset
content; Gitmoot snapshots cached preset instructions into the rendered prompt
before delivery.

## Session Startup

`gitmoot agent start` uses the adapter `Start` method to create a new session
without leaving an interactive terminal open. The startup prompt tells the
runtime to initialize only, make no file edits, and reply with a short readiness
acknowledgment.

Codex startup runs in the repo checkout path:

```sh
codex exec --json -- '<startup-prompt>'
```

The adapter parses JSONL stdout and stores the first
`thread.started.thread_id`. Future jobs resume that session with:

```sh
codex exec resume <session-id> -- '<job-prompt>'
```

Claude startup generates a UUID before invocation, then runs:

```sh
claude --session-id <uuid> -p --output-format json -- '<startup-prompt>'
```

The UUID is stored only after the command succeeds. Future jobs use the Claude
adapter's resume path. This depends on the installed Claude Code CLI supporting
the documented `--session-id`, `-p`, `--output-format json`, and `--resume`
contract.

Shell adapters do not support `agent start`; register shell commands with
`agent subscribe`.

## Job Input

Gitmoot sends adapters a `runtime.Job`:

```go
type Job struct {
    ID          string
    AgentName   string
    Action      string
    Prompt      string
    Repository  string
    PullRequest int
}
```

The prompt already includes repo, branch, PR number, task label, sender,
requested action, cached preset instructions when present, constraints, and the
required `gitmoot_result` JSON shape. Adapters should pass the prompt through
without rewriting workflow semantics.

## Result Handling

`Deliver` should return raw runtime output. Gitmoot parses the
`gitmoot_result` object after delivery. If the runtime returns structured JSON
with a nested text result, the adapter may also fill `Result.Summary`, but raw
output must be preserved for parsing and diagnostics.

## Adding A Runtime

1. Add a runtime constant in `internal/runtime/adapter.go`.
2. Implement an adapter type in `internal/runtime`.
3. Register it in `runtime.Factory.Adapter`.
4. Extend `ValidateAgent` only for runtime-specific reference rules.
5. Implement startup semantics or return a clear unsupported error from
   `Start`.
6. Add tests for startup command arguments, validation, delivery command
   arguments, error handling, health checks, and capability reporting.
7. Add or update docs for the runtime-specific `--session` and startup values.

Keep runtime-specific command names, flags, JSON modes, session lookup, and
fallback behavior inside the adapter package. Do not leak Codex or Claude
assumptions into workflow, daemon, GitHub, database, or merge-gate code.

## Presets

Presets are prompt/profile bundles layered above runtimes. They are not runtime
adapters and should not create adapter-specific behavior. Gitmoot snapshots
cached preset content into startup and job prompts before invoking an adapter.

The built-in `thermo-nuclear-code-quality-review` preset is fetched explicitly
with:

```sh
gitmoot preset update thermo-nuclear-code-quality-review
```

After it is cached, bind it to a normal runtime-backed agent:

```sh
gitmoot agent start thermo-review \
  --runtime codex \
  --repo owner/repo \
  --preset thermo-nuclear-code-quality-review
```

The thermo preset is non-mutating. It supplies reviewer defaults and allows
`ask,review`, but it cannot grant `implement`.

Local custom presets are installed from files:

```sh
gitmoot preset add frontend-reviewer --file agents/frontend-reviewer.md
```

They store `local@file:<absolute-path>` metadata and a `sha256:<hash>` resolved
identifier. Adapters should not read those files or decide how presets behave;
workflow code passes only the rendered prompt. After a prompt file changes, the
user must run `gitmoot preset update <custom-id>` before new jobs use the new
content.

## Shell Adapter

The shell adapter is useful for experiments and contract tests. It invokes:

```sh
sh -c '<configured command>' gitmoot '<job prompt>'
```

Health checks invoke:

```sh
sh -c '<configured command>' gitmoot-health 'Gitmoot health check. Reply OK only.'
```

The command must print a valid `gitmoot_result` object for normal jobs.
