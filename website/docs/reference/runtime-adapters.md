# Runtime Adapters

Runtime adapters keep Gitmoot workflow logic independent from Codex, Claude
Code, Kimi Code, omp, shell commands, and future runtimes. Gitmoot snapshots the
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
- **omp** is the oh-my-pi CLI (v17.2.4), a multi-provider *routing harness*
  rather than one vendor's CLI. Select it with `gitmoot agent start <name>
  --runtime omp`. It advertises `review`, `implement`, and `ask` — **not**
  `produce` — and every job runs a fresh, stateless session. The full contract
  is below.
- **Shell** invokes a configured shell command and is mainly for smoke tests,
  demos, and adapter contract checks.

## The omp Runtime

`omp` (oh-my-pi) resolves its own provider per run from its profile auth storage
or from provider environment keys, so "which model answered" is a property of
the omp profile, not of the Gitmoot runtime name. Everything below is the v1
adapter contract.

### Command shape

Every delivery — and `agent start` — builds one argument vector:

```sh
omp -p --mode=json --approval-mode=yolo --no-session \
    [--add-dir <path>]… [--model <M>] [--thinking <level>] [--max-time <s>] \
    [--plan-yolo [--plan-yolo-into <M>]] \
    [@<staged>/prompt.md] -- '<single prompt token>'
```

Each fixed element is load-bearing:

- `-p --mode=json` selects print mode with the NDJSON envelope the adapter
  parses. Without it omp prints prose and every job fails result extraction.
- `--no-session` keeps the run in memory: no per-worktree session files
  accumulate, and there is nothing to accidentally resume.
- `--add-dir` grants the agent's writable paths first, then its readable paths
  (the same ordering Kimi uses). These are cooperative visibility hints for
  omp's own file layer, not an enforcement boundary — read-only paths are not
  made read-only by this flag.
- `--thinking` carries the resolved Gitmoot effort value (see below).
- `--max-time` is emitted only when the job's context carries a deadline, at
  90% of the time remaining. omp treats it as a hard deadline and still flushes
  a normal NDJSON envelope, whereas Gitmoot's own context deadline kills the
  process group mid-stream: the partial NDJSON is still captured as the job's
  raw output, but the terminal `agent_end` never arrives, so the run fails as
  truncated instead of reporting what it did. The `--max-time` trip is the
  quieter of the two, because the envelope it leaves behind is *complete* — see
  [Truncation](#truncation-is-not-an-answer) for how it is caught anyway.
- The prompt is exactly **one** argv token after `--`. Multiple positionals
  would become multiple separately billed turns, a prompt starting with `-`
  would be read as an unknown flag (exit 2), and one starting with `@` would be
  read as a file attachment. `--` disables all of that for the value after it.
- `--plan-yolo` appears **if and only if** the job asked for plan mode, and
  `--plan-yolo-into <M>` only alongside it. See
  [Plan mode](#plan-mode) below.

The adapter resolves `omp` on `PATH` *before* spawning anything, so a daemon
that cannot see the binary reports a PATH problem instead of a parse error.

`--thinking` accepts a closed allow-list: `off`, `minimal`, `low`, `medium`,
`high`, `xhigh`, `max`, `auto`. A resolved effort outside that set is **dropped**
(no flag, omp's own default) rather than forwarded — a typo must not silently
downgrade a seat, and inventing a level a future omp rejects at parse time would
exit 2 with no output at all. Codex is the only other runtime that consumes the
resolved effort value; Claude and Kimi ignore it entirely.

### Sessions: fresh every job, never resumed

v1 is stateless by construction, and that is a correctness requirement rather
than a simplification. omp's `--resume` runs `switchToResumedProject`, which
calls `setProjectDir()` with the *resumed session's* cwd and **overwrites** the
parsed `--cwd`. A resumed job would therefore edit the previous worktree while
the job's own worktree stayed clean — a green job with an empty diff, which
surfaces as an empty pull request that reads as success. The adapter never
passes `--resume`, `--continue`, or `--fork`, and `Deliver` ignores the stored
runtime reference entirely.

A registered omp agent's `RuntimeRef` may be a session UUID (what `agent start`
returns from the stream header), `fresh:<suffix>`, or — at the adapter's own
`Validate` — empty. `last` is rejected on purpose: it is the resume grammar of
the other runtimes, and accepting it would advertise a capability v1 does not
have.

### Exit code 0 is not success

Under `--mode=json` omp's `process.exit(1)` for a model error lives inside an
`if (mode === "text")` branch, so **a failed turn exits 0**. Success is
parse-derived from the NDJSON stream, and every rule fails loudly rather than
returning an empty success:

- an assistant `stopReason` of `error` or `aborted` fails the run even on exit 0
  **when it is still unrecovered at end of stream** (see [Retries and terminal
  state](#retries-and-terminal-state) — omp retries transient failures on the
  same stream, so the first errored turn is not the run's verdict);
- no `agent_end` event anywhere fails the run (the CLI died mid-stream, or the
  envelope drifted past recognition);
- a final `agent_end` tagged `isTerminal: false` fails the run (the stream
  stopped between scheduled continuations);
- the **final** assistant message must be the answer: empty text there fails the
  run — an empty review must never read as "nothing to flag" — and so does a
  final `stopReason` of `toolUse` or `length` (see
  [Truncation](#truncation-is-not-an-answer));
- forward compatibility covers the rows the parser does **not** read, and only
  those. Unknown event types stay skipped whether or not their payload decodes,
  so new omp event kinds keep working, and so does a `message_end` whose role
  puts it outside the assistant filter (omp emits one for every user, developer
  and custom message of a turn, and their content is legally a bare string). But
  a row the parser was going to read and then could not fails the run, naming the
  shape: a load-bearing event (`session`, `message_end`, `auto_retry_start`,
  `auto_retry_end`, `agent_end`) whose payload does not decode; a `message_end`
  that decodes carrying **no role at all** (role is the discriminator that
  decides whether the parser reads the row, so a zero-valued one is a guess, not
  a reading); and a syntactically malformed line that still identifies itself as
  an assistant `message_end` (a `"type"` of `message_end` **and** a `"role"` of
  `assistant`). Each of those shapes would otherwise delete a failed final turn
  from the stream and hand an *earlier* turn's sentence back as the run's answer.
  Garbage that identifies nothing — a stray log line, a half-written tail
  carrying only one of those two markers — is still skipped.

Stdout is preserved as the job's raw output on every failure path, and the
summary is the final assistant text, never the NDJSON envelope.

### Truncation is not an answer

A run can be **cut off** while every rule above still passes, and the adapter
creates the conditions for it itself: `--max-time` rides on every
daemon-dispatched job, and omp enforces that deadline *inside* its tool loop. It
stops scheduling tools, pairs each pending tool call with a synthetic `aborted`
tool result, and closes the run with a **terminal** `agent_end`. Nothing on that
wire carries an error `stopReason`. The provider's output cap does the same thing
via `stopReason: "length"`.

So the parser reads the **final** assistant message rather than the last one that
happened to carry text. A truncated run fails, and the error says `TRUNCATED`:

- a final message with no text — it was holding a tool call it never got to
  answer;
- a final `stopReason` of `toolUse` (the loop was still calling tools) or
  `length` (the provider cut the message mid-sentence), even when that message
  carries text.

The `toolUse` rule is deliberately **wider than omp's own reading**, and it is the
one place the adapter overrules the runtime: omp settles a `toolUse` stop as
complete as soon as the message carries a tool call *or* any non-whitespace text,
so a final `toolUse` message with real text is a run omp considers finished and
Gitmoot fails anyway. The tradeoff is chosen, not overlooked — a false red costs
one retry of a job, a false green costs a wrong merge verdict, and the two cannot
be told apart on the wire, because a work note ("Let me read the file first.") and
a finished review's verdict are the same bytes to the parser.

The rule this replaces would have answered such a job with whatever the run said
*before* the cut — that work note reported as a review's verdict. A partial answer
is a failure, never a success, and nothing is thrown away by that: the whole
stream, including the text the rule discarded, is kept as the job's raw output.

### Retries and terminal state

omp absorbs transient provider failures itself (retries are enabled by default,
up to 10 of them, covering 5xx/overload/429/stream-stall), and the failed
attempt and the successful one land on the **same** stream. So the first errored
assistant turn is not the run's verdict, and latching it would fail a completed
job and discard its answer. The rules the parser applies:

- A **recovered** retry is a success. What clears a pending failure is narrow: a
  later assistant message carrying real content — a non-whitespace text part or
  a tool call.
- A **content-less** turn can never clear a failure. omp's own classifier treats
  a stop with no text and no tool call as a failure shape, so letting one count
  as recovery would report a failed run as a success carrying a stale mid-run
  sentence as the job's answer.
- `auto_retry_end` with `success: false` is omp giving up, so it **sets** a
  failure in its own right; when nothing earlier in the saga carried a cause, its
  `finalError` becomes the cause (that is the entire failure signal for a saga
  whose every message carried a non-error stop reason). That failure is
  **latched**: a content-carrying message arriving afterwards does not clear it,
  because a verdict the runtime issued about itself is not something a later
  message gets to overturn.
- A retry saga still **open** at end of stream — an `auto_retry_start` with no
  `auto_retry_end` after it — fails closed. The state machine ended mid-retry;
  "a retry was pending" is never "the error was being handled".

### Token usage

Usage is per assistant message, not per run: the adapter sums `usage.input` and
`usage.output` across assistant `message_end` events. (Note the field names —
omp spells them `input`/`output`, not the `input_tokens`/`output_tokens` every
other adapter here uses.) The `agent_end.telemetry` rollup exists only when omp
runs under an OTEL telemetry config; it is preferred over the sum only when
every `agent_end` carried one **and** it is at least the per-message sum on both
axes, because the sum is a lower bound on what the run was billed. Usage from a
**failed** run is reported on every path — the exit-0 one (the dominant failure
here, since a failed omp turn exits 0) and the non-zero exit alike, including a
process group killed on Gitmoot's own deadline. It is whatever the stream proved
before the run died, so read it as a **floor**: a process killed mid-stream may
have been billed for a message whose `message_end` never reached stdout.

### Autonomy policies

| `--policy` | omp argument | Effect |
|---|---|---|
| `read-only` | `--approval-mode=yolo` | advisory only at the runtime layer |
| `workspace-write` | `--approval-mode=yolo` | same |
| `danger-full-access` | `--approval-mode=yolo` | same |
| `auto` (default) | `--approval-mode=yolo` | same |

Every policy passes the **same explicit** `--approval-mode=yolo`, and that is
deliberate. omp's default tool tier is `exec` and its read/grep/ls tools declare
no tier at all, so under `always-ask` every headless tool call throws ("requires
approval but no interactive UI available") — a policy-to-approval mapping would
brick the runtime rather than restrict it. Omitting the flag instead would
inherit whatever approval mode the host config carries, which is not
deterministic across machines.

Read-only therefore stays enforced **Gitmoot-side**, exactly as it is for Kimi:
the `implement` **capability** is refused at `agent start` and `agent subscribe`
when the agent carries `auto`/empty or `read-only`, and an implement **job** for
such an agent is refused again at dispatch — both at CLI enqueue and in the
daemon. Treat the runtime-level approval flag as advisory and the Gitmoot gate as
the boundary. (Landlock is not a second layer here: the sandbox wrapper selects
by runtime **name** and wraps only Claude and Kimi, so no omp process is ever
confined by it. omp separately does not advertise `produce`, which is why it
never reaches the produce-stage wrapper either.)

### Plan mode

omp ships a `--plan-yolo` mode and the adapter passes it — but only when the job
asks for it. A job payload carrying `plan: true` runs
`--plan-yolo`; adding `plan_into: "<model>"` also emits
`--plan-yolo-into <model>`, pinning the model the execution phase runs on (omp's
declared default target is the `smol` role). `plan_into` must be one non-flag
model selector without internal whitespace or control characters. An unpaired
or malformed target is rejected before dispatch.

Plan mode is **workflow shape**, not permission. On `omp/17.2.4`, the
reproducible check `omp --version && omp --help` declares that `--plan-yolo`
starts in read-only plan mode, auto-approves the model's plan on its first
resolve call, then executes it, and that `--plan-yolo-into` selects the execution
model. This is omp's versioned CLI declaration, not internal behavior Gitmoot
can observe. Gitmoot verifies that the installed binary lists the flags and
records the requested plan shape. `--approval-mode` is untouched and stays
`yolo` for plan and non-plan runs alike — mapping plan mode onto the approval
tier would brick the runtime rather than restrict it.

Only the omp runtime implements plan mode. A plan request routed to any other
runtime **fails loudly at dispatch** instead of quietly running as an ordinary
implementation, and the resolved shape is echoed into the job payload as
`plan_mode` (`plan` or `plan-into:<model>`) so a reader can tell a plan run from
a normal one without re-deriving it from the argv. Runtime preflight evaluates
the two plan flags only for a plan request, so an older omp CLI that lacks those
optional flags can still run ordinary non-plan jobs.

Note that this is omp's plan-first discipline *inside one run*; it is **not**
Gitmoot's plan gate, where approval remains an explicit human act.

### Capabilities

`review`, `implement`, `ask`. `produce` is deliberately excluded: the produce
path runs the runtime under Gitmoot's Landlock wrapper, and omp ships as a Bun
binary whose sandbox interaction is unprobed. Advertising the capability before
it is proven would turn an unknown into a silent stage failure.

### Cross-family review

**omp joins no model family, on purpose.** A routing harness's resolved provider
is opaque, so mapping omp to a family would manufacture diversity the merge gate
would then trust. The consequence is stated out loud rather than hidden: an omp
implement job's cross-family review is **refused loudly** — the engine records a
`cross_family_review_failed` job event naming the runtime and both real remedies
— instead of silently skipping, which used to look identical to "no reviewer was
authenticated". No review row is written either way.

The exclusion is **symmetric**. A registered omp seat that declares the `review`
capability is dropped from the reviewer candidate set too, so it is never
returned as another runtime's cross-family reviewer: refusing omp as an
implementer while selecting it as a reviewer would manufacture the same false
diversity from the other direction. Selection falls through to the family
rotation, and then to the same-family fallback (tagged so it weights below a
genuine cross-family review). Per-seat provider declaration is the sound fix and
is tracked as issue #1436.

### Transcripts

omp's raw NDJSON is retained verbatim by `[transcripts]` retention, and the
transcript translator is a deliberate **passthrough** for now: every row is
emitted undecoded and carries the transcript kind `raw`, so a JSONL export shows
`"kind":"raw"` on every omp line and omp's own event discriminator — the NDJSON
`type` field — survives only inside each row's raw JSON text. Do not write a
consumer that expects per-event kinds here. A decoding translator that lifts
tool calls and usage out of the stream is a named follow-up. The passthrough
exists because the alternative is not a graceful degradation — with no
translator case, `job watch --transcript` and
`job transcript <id>` would error even though the bytes are on disk, and
`job transcript --all` would abort the whole batch mid-stream, breaking the
export of every *other* runtime's jobs.

### Credentials and environment

omp is a routing harness, so an authentication failure never means one fixed
credential; the adapter's auth message names the profile auth storage, the
provider environment keys, and the binary's install path, because a daemon that
cannot see the binary and a daemon that cannot see a credential are the two
failure modes operators actually hit.

Child-environment curation is **off by default**, and with it off omp inherits
the daemon's whole environment exactly like every other runtime. When
`[credentials] env_curation = true` is enabled, omp's curated allowlist is
**routing plumbing only** — `OMP_PROFILE`, `PI_PROFILE`, `PI_CODING_AGENT_DIR`,
`PI_SMOL_MODEL`, `PI_SLOW_MODEL`, `PI_PLAN_MODEL`, `OMP_AUTH_BROKER_URL`,
`OMP_AUTH_BROKER_TOKEN`. No raw provider key is listed; under curation a
provider key reaches omp only through omp's own profile auth storage or an
explicit `env_passthrough` entry.

Two caveats worth knowing before enabling curation:

- The `OMP_AUTH_BROKER_URL` / `OMP_AUTH_BROKER_TOKEN` pair really is a
  credential-acquisition path (omp discovers a broker-backed auth store from it),
  and the pair is **indivisible**: omp throws when the URL is set and no token is
  available, so dropping only the token converts an inherited URL into a hard
  failure. Drop both — and pass them through explicitly — if you want omp
  strictly fail-closed.
- `--profile` selects omp's auth/state store; it does **not** isolate process
  environment. A passed-through key is visible to every omp profile this daemon
  runs.

### Heartbeats

Heartbeat runtime overrides are derived from the adapter registry, so `omp`
became a valid `--runtime` for heartbeats the moment it was registered — no
config change required. Operationally that means enabled heartbeats can dispatch
to omp, and therefore to whatever credential its profile resolves, as soon as
this ships.

### Oversize prompts

Any single process argument above the kernel's `MAX_ARG_STRLEN` (~128 KiB) fails
to launch, so a prompt at or above the 100 KiB safety threshold is not passed on
argv at all. Unlike Kimi, omp has a native file channel: the adapter
stages the prompt to `prompt.md` in a dedicated temporary directory (never the
job worktree, so an implement job's `git add` cannot pick it up) and passes
`@<abs-path>` before `--`. omp's own CLI reads that file and inlines it into the
same turn, so no workspace grant and no agent file-read is involved. The
trailing prompt token becomes a short pointer telling the model the attached
file is the task. Above omp's own 5 MiB attachment ceiling the job is **refused**
instead: omp would substitute a `(skipped: too large)` placeholder and answer a
task it never received, which the adapter would otherwise return as a success.

## Implement Jobs and the Commit Contract

Gitmoot owns the commit for implement jobs: it commits and delivers the
worktree's changes after the job finishes. Every rendered implement prompt
carries one deterministic sentence telling the worker not to run `git commit`
or `git push`. Ask and review prompts are unchanged.

For Codex, a workspace-write job whose checkout is a linked `git worktree`
gets one extra sandbox grant: the worktree's resolved git directory
(`<main-repo>/.git/worktrees/<name>`) is passed to the Codex CLI with
`--add-dir`, so routine git operations that write metadata (an index refresh
from `git status`, or `git add`) work inside the sandbox. The grant is
additive; it does not replace any `writable_roots` configured in the
operator's `~/.codex/config.toml`. Read-only and danger-full-access sandboxes
are unchanged, and a primary (non-worktree) checkout gets no extra grant.

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
receives the resolved value as `-c model_reasoning_effort=<value>`; omp receives
it as `--thinking <level>` when it matches omp's closed allow-list
(`off|minimal|low|medium|high|xhigh|max|auto`) and drops it otherwise; Claude and
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
- omp accepts a session UUID or `fresh:<suffix>`, never `last` — it never
  resumes, and the reference is not used at delivery.
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
[`docs/adapters.md`](https://github.com/gitmoot/gitmoot/blob/main/docs/adapters.md).
