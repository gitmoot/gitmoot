# Runtime Adapter Authoring

Gitmoot treats Codex, Claude Code, Kimi Code, omp, and shell commands as runtime
adapters behind one interface. Workflow, daemon, and GitHub code should stay
runtime-neutral.

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
    TemplateID       string
    Capabilities   []string
    AutonomyPolicy string
    HealthStatus   string
}
```

`RuntimeRef` is runtime-specific. Codex accepts a session UUID, thread name, or
`last`. Claude accepts a UUID or `last`. Kimi accepts a Kimi session id of the
form `session_<uuid>` or empty. Kimi CLI (legacy, `kimi-cli`) accepts a session
UUID or empty. omp accepts a session UUID or `fresh:<suffix>` — never `last`,
because it never resumes — and its adapter also accepts an empty reference,
since delivery ignores the value entirely. Shell uses the configured command.
`TemplateID` is Gitmoot-owned metadata. Adapters do not fetch or interpret template
content; Gitmoot snapshots cached template instructions into the rendered prompt
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

Kimi startup runs in the repo checkout path:

```sh
kimi -p '<startup-prompt>' --output-format stream-json
```

The adapter parses the stream-json output and stores the reported session id.
Future jobs resume that session with:

```sh
kimi -S <session-id> -p '<job-prompt>' --output-format stream-json
```

Kimi runs against a logged-in Kimi CLI. Run `kimi login`, then restart the
Gitmoot daemon so it inherits the session.

The opt-in `kimi-cli` runtime (#546) targets the **legacy** Kimi CLI, which
requires the older `--print` command shape the current Kimi Code CLI does not
support:

```sh
kimi --print -p '<prompt>' --output-format stream-json
```

It is intentionally a separate runtime name so the default `kimi` (Kimi Code)
path is never probed or changed; choose `kimi` unless you specifically run the
legacy CLI. The two count as the same runtime family for cross-family review.

omp (oh-my-pi) startup and every later job run the SAME argument vector — one
builder, so no flag can appear on one path and go missing on the other:

```sh
omp -p --mode=json --approval-mode=yolo --no-session \
    [--add-dir <path>]… [--model <M>] [--thinking <level>] [--max-time <s>] \
    [--plan-yolo [--plan-yolo-into <M>]] \
    [@<staged>/prompt.md] -- '<single prompt token>'
```

`--plan-yolo` appears if and only if the job asked for plan mode (`plan` /
`plan_into` on the job payload), with `--plan-yolo-into` pinning the model the
execution phase runs on. `plan_into` must be one non-flag model selector with no
internal whitespace or control characters. It is orthogonal to
`--approval-mode`, which stays `yolo` for every job, plan or not. `plan_into`
without `plan`, malformed targets, and a plan request routed to any non-omp
runtime are refused before dispatch — a plan-gated brief never silently
degrades into an ordinary implementation. Runtime preflight requires the two
plan flags only for plan requests; an ordinary omp job does not require them.

This behavior description is grounded in a versioned CLI declaration, not an
internal transition Gitmoot can observe. On `omp/17.2.4`, run
`omp --version && omp --help`: the help declares that `--plan-yolo` starts in
read-only plan mode, auto-approves the model's plan on its first resolve call,
then executes it, and that `--plan-yolo-into` selects the execution model with
the `smol` role as the default. Gitmoot verifies the installed flags and records
the requested plan shape; it cannot verify omp's internal phase transitions.

Startup stores the session id from the NDJSON header line, but delivery never
uses it: omp is stateless in v1 and never passes `--resume`, `--continue`, or
`--fork`. That is a correctness requirement, not a simplification —
`switchToResumedProject` sets the project directory to the *resumed* session's
cwd and overwrites the parsed `--cwd`, so a resumed job would edit the previous
worktree while the job's own worktree stayed clean: a green job with an empty
diff. Two more properties an adapter author has to keep in mind here:

- **Exit code 0 is not success.** omp's `process.exit(1)` for a model error is
  inside an `if (mode === "text")` branch, so a failed turn exits 0 under
  `--mode=json`. Success is derived from the stream: an unrecovered assistant
  `stopReason` at end of stream, a terminal `agent_end`, a closed retry saga, and
  a **final** assistant message that actually answered — not merely some earlier
  message that carried text. A run cut off by `--max-time` or by the provider's
  output cap ends with a complete envelope and no answer, and fails as truncated.
  The `toolUse` half of that rule is deliberately wider than omp's own reading —
  omp settles a text-carrying `toolUse` stop as complete, and Gitmoot still fails
  it, because narration and an answer are the same bytes on the wire and a needless
  retry is cheaper than a wrong merge verdict. The discarded text is preserved in
  the job's raw output.
- **Usage is per assistant message**, spelled `input`/`output` (not
  `input_tokens`/`output_tokens`), and is summed across the run.
- **Skipping a row you were going to read is a false green.** Unknown event types
  stay skipped for forward compatibility, but a row the parser reads and then
  cannot read fails the run instead: a load-bearing event whose payload does not
  decode, a `message_end` that decodes with no role at all, and a malformed line
  that still identifies itself as an assistant `message_end`. Each of those would
  otherwise delete a failed final turn from the stream and report an *earlier*
  turn's sentence as the job's answer.

omp advertises `review`, `implement`, and `ask` — not `produce`, whose Landlock
wrapper interaction with omp's Bun binary is unprobed. The behavioral contract
in full (policies, retries, credentials, transcripts, cross-family) is in the
[Runtime Adapters reference](https://gitmoot.io/docs/reference/runtime-adapters).

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
requested action, cached template instructions when present, constraints, and the
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

## Agent Templates

Agent Templates are prompt/profile bundles layered above runtimes. They are not runtime
adapters and should not create adapter-specific behavior. Gitmoot snapshots
cached template content into startup and job prompts before invoking an adapter.

The built-in `thermo-nuclear-code-quality-review` template is fetched explicitly
with:

```sh
gitmoot agent template update thermo-nuclear-code-quality-review
```

After it is cached, bind it to a normal runtime-backed agent:

```sh
gitmoot agent start thermo-review \
  --runtime codex \
  --repo owner/repo \
  --template thermo-nuclear-code-quality-review
```

The thermo template is non-mutating. It supplies reviewer defaults and allows
`ask,review`, but it cannot grant `implement`.

Local custom agent templates are installed from files:

```sh
gitmoot agent template validate agents/frontend-reviewer.md
gitmoot agent template add frontend-reviewer --file agents/frontend-reviewer.md
```

They store `local@file:<absolute-path>` metadata and a `sha256:<hash>` resolved
identifier. Adapters should not read those files or decide how agent templates behave;
workflow code passes only the rendered prompt. After a template file changes, the
user must run `gitmoot agent template update <custom-id>` before new jobs use the new
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
