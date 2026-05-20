# Runtime Adapter Authoring

Gitmoot treats Codex, Claude Code, and shell commands as runtime adapters behind
one interface. Workflow, daemon, and GitHub code should stay runtime-neutral.

## Adapter Contract

An adapter implements `runtime.Adapter`:

```go
type Adapter interface {
    Name() string
    Validate(ctx context.Context, agent Agent) error
    Deliver(ctx context.Context, agent Agent, job Job) (Result, error)
    Health(ctx context.Context, agent Agent) error
    Capabilities(ctx context.Context) ([]string, error)
}
```

Responsibilities:

- `Name` returns the runtime key used by `gitmoot agent subscribe`.
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
    Capabilities   []string
    AutonomyPolicy string
    HealthStatus   string
}
```

`RuntimeRef` is runtime-specific. Codex accepts a session UUID, thread name, or
`last`. Claude accepts a UUID or `last`. Shell uses the configured command.

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
requested action, constraints, and the required `gitmoot_result` JSON shape.
Adapters should pass the prompt through without rewriting workflow semantics.

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
5. Add tests for validation, command arguments, error handling, health checks,
   and capability reporting.
6. Add or update docs for the runtime-specific `--session` value.

Keep runtime-specific command names, flags, JSON modes, session lookup, and
fallback behavior inside the adapter package. Do not leak Codex or Claude
assumptions into workflow, daemon, GitHub, database, or merge-gate code.

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
