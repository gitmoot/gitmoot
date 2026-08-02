package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/subprocess"
)

const (
	// OmpRuntime is the runtime id of the oh-my-pi CLI (`omp`) adapter. It lives
	// in this file rather than adapter.go's const block because the adapter ships
	// ahead of its registration: the Factory case, the metadata registry entry and
	// the CLI switches land together, and nothing outside this file needs the name
	// until they do.
	OmpRuntime = "omp"

	OmpLiveCheckPrompt = "Gitmoot omp live check. Return OK only."

	// OmpAuthSetupMessage is the operator fix for an omp authentication failure.
	// omp is a ROUTING harness: it resolves a provider per run from its profile's
	// auth storage or from provider env keys, so "auth failed" never means one
	// fixed credential — the message therefore names both channels plus the binary
	// location, because a daemon that cannot see the binary and a daemon that
	// cannot see a credential are the two failure modes operators actually hit.
	OmpAuthSetupMessage = "omp resolves a provider per run, so an auth failure means the profile it ran under has no usable credential. " +
		"Authenticate omp once interactively so its profile auth storage holds one (a dedicated per-seat store is `omp --profile <name>`), " +
		"or export the provider key the daemon should use (for example ANTHROPIC_OAUTH_TOKEN, ANTHROPIC_API_KEY or OPENAI_API_KEY) into the " +
		"daemon environment, then restart the Gitmoot daemon so it inherits the credential. The daemon must also see the binary itself: omp is " +
		"installed at /root/.local/bin/omp, and the daemon's PATH comes from its systemd EnvironmentFile, not from a login shell."

	// ompMaxArgvPromptBytes is the per-argument size ceiling for the omp prompt,
	// the same 100 KiB trip point kimi uses (#723): Linux caps any SINGLE execve
	// argument at MAX_ARG_STRLEN (128 KiB), and a prompt at or above that fails
	// fork/exec with E2BIG. Unlike kimi, omp HAS a native file channel — an argv
	// token of the form `@<path>` before `--` is read by omp's own CLI and inlined
	// into the SAME turn as `<file name="<abs>">…</file>` — so an oversize prompt
	// is delivered as a real attachment rather than an instruction to go read a
	// file (see ompPromptDelivery). We trip below the hard limit to leave headroom
	// for UTF-8 and kernel argv accounting.
	ompMaxArgvPromptBytes = 100 * 1024

	// ompMaxAttachmentBytes is omp's OWN ceiling on an `@file` text attachment:
	// MAX_CLI_TEXT_BYTES in packages/coding-agent/src/cli/file-processor.ts:16
	// (5 MiB). Above it omp does NOT fail — file-processor.ts:49-55 writes a yellow
	// warning to stderr and substitutes `<file name="…">(skipped: too large, …)</file>`
	// for the contents, then runs the turn anyway. Since the pointer message this
	// adapter sends alongside the attachment asserts that the file's full contents
	// are already in the message, a silently skipped attachment would make the model
	// answer a task it never received and the adapter would return that answer as a
	// success. So a prompt above this ceiling is a LOUD failure here instead: there
	// is no delivery channel left that keeps the prompt intact.
	ompMaxAttachmentBytes = 5 * 1024 * 1024

	// ompMaxTimeDeadlineFraction is the share of the context's remaining time
	// handed to omp's own `--max-time` budget. omp treats it as a hard deadline and
	// flushes a normal NDJSON envelope when it trips, whereas gitmoot's context
	// deadline SIGKILLs the process group MID-STREAM. The bytes already written are
	// not lost (subprocess buffers stdout/stderr and returns them alongside the
	// error, and omp emits every non-agent_end event eagerly), but the TERMINAL
	// agent_end never arrives, so parseOmpStreamJSON fails the run as truncated.
	// Leaving 10% of the budget to omp's own shutdown means the job reports what it
	// did instead of failing on a partial envelope.
	ompMaxTimeDeadlineFraction = 0.9
)

// ompThinkingLevels is the CLOSED allow-list of values omp's `--thinking` flag
// accepts. Gitmoot's effort vocabulary is per-runtime and operator-supplied, so an
// unrecognized effort is DROPPED (no flag, omp's own default) rather than passed
// through: omp logs a warning and ignores an invalid level, but a typo silently
// downgrading a seat is worse than the CLI default, and inventing a level that a
// future omp rejects at parse time would exit 2 with no output at all.
var ompThinkingLevels = map[string]struct{}{
	"off":     {},
	"minimal": {},
	"low":     {},
	"medium":  {},
	"high":    {},
	"xhigh":   {},
	"max":     {},
	"auto":    {},
}

// OmpAdapter delivers Gitmoot jobs to the local oh-my-pi CLI (`omp`).
//
// v1 is STATELESS BY CONSTRUCTION: every delivery is a fresh in-memory session
// (`--no-session`) and no delivery ever resumes. That is not a simplification, it
// is a correctness requirement — omp's `--resume` runs switchToResumedProject,
// which setProjectDir()s the resumed session's cwd and OVERWRITES the parsed
// `--cwd`, so a resumed job would edit the previous worktree while the job's own
// worktree stayed clean: a green job with an empty diff. Sessions, resume, the
// produce capability and per-seat `--profile` isolation are deliberately deferred.
type OmpAdapter struct {
	Runner subprocess.Runner
	Dir    string
}

func (a OmpAdapter) Name() string { return OmpRuntime }

func (a OmpAdapter) Start(ctx context.Context, request StartRequest) (StartResult, error) {
	if err := a.validateStart(request); err != nil {
		return StartResult{}, err
	}
	if err := a.preflight(); err != nil {
		return StartResult{}, err
	}
	promptArg, attachArgs, cleanup, err := ompPromptDelivery(request.Prompt)
	if err != nil {
		return StartResult{}, err
	}
	defer cleanup()
	args := ompArgs(request.Agent, request.Agent.Model, ompThinkingLevel(request.Agent.Effort), ompMaxTimeArg(ctx), attachArgs, promptArg)
	result, err := a.runner().Run(ctx, a.Dir, "omp", args...)
	if err != nil {
		return StartResult{Raw: result.Stdout + result.Stderr}, ompCommandError(result, err)
	}
	content, sessionID, _, parseErr := parseOmpStreamJSON(result.Stdout)
	if parseErr != nil {
		return StartResult{Raw: result.Stdout}, ompCommandError(result, parseErr)
	}
	// The session header is the ONLY channel that reports the id of a finished
	// print-mode run, so a missing one leaves the registration with no reference at
	// all. Fail loudly here rather than register an agent whose ref is empty for a
	// reason nobody can reconstruct later.
	if sessionID == "" {
		return StartResult{Raw: content}, errors.New("omp stream carried no session header id; cannot register a runtime reference")
	}
	return StartResult{RuntimeRef: sessionID, Raw: content}, nil
}

// Validate mirrors KimiAdapter.Validate's shape — pure, no subprocess — but runs
// the shared agent-field checks itself instead of delegating to validateRuntime,
// for two reasons that both outlive this slice:
//
//  1. validateRuntime -> ValidateAgent -> validateAgentFields resolves the agent's
//     runtime through Factory.Adapter, which would make an adapter's own Validate
//     depend on that adapter already being registered in the Factory.
//  2. ValidateAgent calls validateAgentFields with requireRuntimeRef=TRUE, so it
//     rejects an empty runtime ref. omp v1 is stateless — it mints an in-memory
//     session per job and Deliver ignores the ref entirely — so an empty ref is a
//     legitimate registration here, not an error. (This reason is specific to the
//     ValidateAgent path: the Start path, validateStartRequest, passes false.)
//
// Accepted refs: a session UUID (what Start returns from the stream header),
// fresh:<suffix>, or empty. "last" is rejected on purpose: it is the resume
// grammar of the other runtimes, and accepting it here would advertise a resume
// capability v1 deliberately does not have.
//
// The shared name/role/repo-scope shape checks run here too (ompAgentFieldChecks),
// so the Deliver path is exactly as strict as the Start path and as every sibling
// runtime, whose Validate reaches the same checks through validateRuntime ->
// ValidateAgent -> validateAgentFields.
func (a OmpAdapter) Validate(_ context.Context, agent Agent) error {
	if _, err := NormalizeAutonomyPolicy(agent.AutonomyPolicy); err != nil {
		return err
	}
	if agent.Runtime != a.Name() {
		return fmt.Errorf("agent runtime %q does not match adapter %q", agent.Runtime, a.Name())
	}
	if err := ompAgentFieldChecks(agent); err != nil {
		return err
	}
	if ref := strings.TrimSpace(agent.RuntimeRef); ref != "" && !isUUID(ref) && !IsFreshRef(ref) {
		return fmt.Errorf("omp runtime reference %q must be an omp session UUID, fresh:<suffix>, or empty", agent.RuntimeRef)
	}
	return nil
}

// ompAgentFieldChecks is everything validateAgentFields enforces except the
// requireRuntimeRef clause (omp v1 is stateless, so an empty ref is legitimate)
// and the Factory.Adapter lookup (which would make this adapter's own validation
// depend on its registration, a slice away). It is shared by Validate and
// validateStart so neither path can be stricter than the other.
func ompAgentFieldChecks(agent Agent) error {
	switch {
	case strings.TrimSpace(agent.Name) == "":
		return errors.New("agent name is required")
	case strings.ContainsAny(agent.Name, " \t\n/"):
		return fmt.Errorf("agent name %q cannot contain whitespace or slash", agent.Name)
	case strings.TrimSpace(agent.Role) == "":
		return errors.New("agent role is required")
	case strings.TrimSpace(agent.RepoScope) != "" && !validRepoScope(agent.RepoScope):
		return fmt.Errorf("agent repo scope %q must be owner/repo", agent.RepoScope)
	}
	return nil
}

func (a OmpAdapter) Deliver(ctx context.Context, agent Agent, job Job) (Result, error) {
	if err := a.Validate(ctx, agent); err != nil {
		return Result{}, err
	}
	// PATH preflight before anything else runs: the daemon's PATH comes from its
	// systemd EnvironmentFile, so "works in my shell" is not evidence the daemon
	// can spawn omp. Failing here means ZERO subprocesses ran and the operator gets
	// the install path instead of a garbled parse error.
	if err := a.preflight(); err != nil {
		return Result{}, err
	}
	// v1 NEVER resumes (see OmpAdapter). Gitmoot jobs are self-contained — the full
	// context is in job.Prompt — so the validated ref is deliberately unused and
	// every job starts its own in-memory session.
	_ = agent.RuntimeRef
	promptArg, attachArgs, cleanup, err := ompPromptDelivery(job.Prompt)
	if err != nil {
		return Result{}, err
	}
	defer cleanup()
	args := ompArgs(agent, EffectiveModel(agent, job), ompThinkingLevel(effectiveEffort(agent, job)), ompMaxTimeArg(ctx), attachArgs, promptArg)
	// STDIN INVARIANT: omp's CLI reads stdin to EOF for every non-protocol mode,
	// including --mode=json (main.ts:1209 calls readPipedInput, main.ts:191-207 does
	// `await Bun.stdin.text()` whenever `process.stdin.isTTY === false`). It is safe
	// here only because internal/subprocess never sets cmd.Stdin, so os/exec wires
	// /dev/null and readPipedInput returns immediately. Any future runner that
	// attaches a PIPE to a Gitmoot subprocess MUST attach /dev/null for omp: omp
	// would otherwise block before the model is ever called, get SIGKILLed on the ctx
	// deadline, and this parser would diagnose the empty stream as "ended without an
	// agent_end event" — the wrong cause for a wiring bug.
	result, err := runAgentCommand(ctx, a.runner(), a.Dir, job.AgentEnv, job.OnPID, "omp", args...)
	// Parse before branching on err: the header id is worth capturing for the
	// session diagnostics even when the process itself failed.
	content, sessionID, usage, parseErr := parseOmpStreamJSON(result.Stdout)
	if err != nil {
		return Result{Raw: result.Stdout + result.Stderr, SessionDiag: newSessionDiag(result, err, sessionID)}, ompCommandError(result, err)
	}
	// EXIT 0 IS NOT SUCCESS for omp under --mode=json: its process.exit(1) on a
	// model error lives inside an `if (mode === "text")` branch, so a failed turn
	// exits 0. Success is parse-derived (see parseOmpStreamJSON), and stdout is
	// preserved as Raw so the evidence outlives the failure.
	//
	// The USAGE the parser accumulated before the failure is booked too. A failed
	// omp turn is billed like any other: the dominant failure mode here is exit 0
	// with `stopReason error`, and omp retries that up to 10 times by default
	// (retry.maxRetries) with every attempt billed and every attempt's usage summed
	// into this value. Dropping it would report 0/0 tokens for the runs that spend
	// the most.
	if parseErr != nil {
		return Result{
			Raw:          result.Stdout,
			InputTokens:  usage.InputTokens,
			OutputTokens: usage.OutputTokens,
			SessionDiag:  newSessionDiag(result, nil, sessionID),
		}, ompCommandError(result, parseErr)
	}
	return Result{
		Raw:          content,
		Summary:      strings.TrimSpace(content),
		InputTokens:  usage.InputTokens,
		OutputTokens: usage.OutputTokens,
		SessionDiag:  newSessionDiag(result, nil, sessionID),
	}, nil
}

func (a OmpAdapter) Health(ctx context.Context, agent Agent) error {
	if err := a.Validate(ctx, agent); err != nil {
		return err
	}
	_, err := a.Deliver(ctx, agent, Job{Prompt: OmpLiveCheckPrompt})
	return err
}

// Capabilities excludes "produce": the produce path runs the runtime under
// Gitmoot's Landlock wrapper, and omp ships as a Bun binary whose sandbox
// interaction is unprobed. Advertising a capability before it is proven would turn
// an unknown into a silent stage failure.
func (a OmpAdapter) Capabilities(context.Context) ([]string, error) {
	return []string{"review", "implement", "ask"}, nil
}

func (a OmpAdapter) runner() subprocess.Runner {
	if a.Runner != nil {
		return a.Runner
	}
	return subprocess.GroupRunner{}
}

// validateStart applies the Start-time checks: the same runtime/ref grammar as
// Validate plus validateStartRequest's own field checks and a non-empty prompt.
//
// It re-implements rather than calls validateStartRequest for ONE reason — reason
// (1) on Validate: validateStartRequest -> validateAgentFields ends in a
// Factory.Adapter lookup, which fails until this runtime is registered there.
// Reason (2) on Validate does NOT apply here: the Start path passes
// requireRuntimeRef=false, so validateStartRequest would accept an empty ref
// anyway. Once OmpRuntime reaches Factory.Adapter (the registration slice), this
// whole function should collapse into `validateStartRequest(request.Agent,
// a.Name(), request.Prompt)` plus the omp ref-shape check.
//
// The name/role/repo-scope checks every other runtime enforces at Start live in
// Validate (ompAgentFieldChecks) rather than here, so Start and Deliver carry the
// SAME field guarantees: skipping them on either path would leave the CLI preflight
// (runtime.ValidateStartRequest / runtime.ValidateAgent) as the only guard, and
// defense in depth is the point of having two.
func (a OmpAdapter) validateStart(request StartRequest) error {
	if err := a.Validate(context.Background(), request.Agent); err != nil {
		return err
	}
	if strings.TrimSpace(request.Prompt) == "" {
		return errors.New("start prompt is required")
	}
	return nil
}

// preflight resolves the omp binary before any subprocess is spawned so a PATH
// problem surfaces as a PATH problem.
func (a OmpAdapter) preflight() error {
	if _, err := a.runner().LookPath("omp"); err != nil {
		return fmt.Errorf("omp CLI not found on PATH (%w). Install it or put its directory on the Gitmoot daemon's PATH: this fleet installs it at /root/.local/bin/omp, and the daemon's PATH comes from its systemd EnvironmentFile, not from a login shell", err)
	}
	return nil
}

// ompArgs builds the complete `omp` argument vector. It is the SINGLE argv builder
// for this runtime — Start and Deliver share it — so no flag can appear on one
// path and go missing on the other. Shape (asserted byte-for-byte in tests):
//
//	omp -p --mode=json --approval-mode=yolo --no-session [--add-dir <p>]…
//	    [--model <M>] [--thinking <lvl>] [--max-time <s>] [@<staged>/prompt.md]
//	    -- <single prompt token>
//
// Every fixed element is load-bearing:
//
//   - `-p --mode=json` selects print mode with the NDJSON envelope this adapter
//     parses; without --mode=json omp prints prose and every job fails extraction.
//   - `--approval-mode=yolo` is passed EXPLICITLY for every autonomy policy. omp's
//     default tool tier is "exec" and its read/grep/ls tools declare no tier at
//     all, so under `always-ask` every headless tool call THROWS ("requires
//     approval but no interactive UI available") — a policy-to-approval mapping
//     would brick the runtime rather than restrict it. Omitting the flag instead
//     would inherit whatever tools.approvalMode the host config carries, which is
//     not deterministic across machines. read-only stays enforced Gitmoot-side by
//     readOnlyImplementationBlocked (the kimi precedent) and by NOTHING ELSE: the
//     Landlock wrapper selects by runtime NAME and wraps only claude/kimi (both
//     call sites in cli/daemon_worker.go), so no omp process is ever confined.
//   - `--no-session` keeps the run in memory: no per-worktree session .jsonl
//     accretion, and nothing to accidentally resume (v1 never resumes).
//   - the prompt is exactly ONE token after `--`: multiple positionals become
//     multiple sequential (separately billed) turns, a prompt starting with `-` is
//     read as an unknown flag and exits 2, and one starting with `@` is read as a
//     file attachment. `--` disables all of that for the value that follows.
func ompArgs(agent Agent, model string, thinking string, maxTime string, attachArgs []string, prompt string) []string {
	args := []string{"-p", "--mode=json", "--approval-mode=yolo", "--no-session"}
	args = append(args, ompWorkspaceArgs(agent)...)
	if model != "" {
		args = append(args, "--model", model)
	}
	if thinking != "" {
		args = append(args, "--thinking", thinking)
	}
	if maxTime != "" {
		args = append(args, "--max-time", maxTime)
	}
	// Attachments must precede `--`; everything after it is message text.
	args = append(args, attachArgs...)
	return append(args, "--", prompt)
}

// ompWorkspaceArgs grants the agent's produce paths as additional workspace roots,
// writable first then readable — the same ordering kimi uses. These are
// cooperative visibility hints for omp's own file layer and NOT an enforcement
// boundary: --add-dir does not make the readable subset read-only, and omp gets no
// Landlock confinement underneath either (that wrapper selects by runtime name and
// wraps only claude/kimi). readOnlyImplementationBlocked is the ONLY gate that
// keeps a read-only omp agent from writing.
func ompWorkspaceArgs(agent Agent) []string {
	var args []string
	for _, path := range agent.WritablePaths {
		if path = strings.TrimSpace(path); path != "" {
			args = append(args, "--add-dir", path)
		}
	}
	for _, path := range agent.ReadablePaths {
		if path = strings.TrimSpace(path); path != "" {
			args = append(args, "--add-dir", path)
		}
	}
	return args
}

// ompThinkingLevel maps a Gitmoot effort value onto omp's `--thinking` level
// through the closed allow-list, returning "" (no flag) for anything unknown.
func ompThinkingLevel(effort string) string {
	level := strings.ToLower(strings.TrimSpace(effort))
	if _, ok := ompThinkingLevels[level]; !ok {
		return ""
	}
	return level
}

// ompMaxTimeArg returns the `--max-time` value in seconds for a context that
// carries a deadline, and "" for one that does not (no flag — omp runs until
// gitmoot's own ctx kills it, which is the pre-existing behavior for every other
// runtime). The value is floor(0.9 × remaining): omp exits cleanly on its own
// deadline and still writes the NDJSON envelope, so the last 10% is the margin in
// which the job's output survives instead of being SIGKILLed away. A context that
// is already at or past its deadline gets no flag: the run is about to be
// cancelled anyway, and `--max-time 0` would be a worse way to say so.
func ompMaxTimeArg(ctx context.Context) string {
	deadline, ok := ctx.Deadline()
	if !ok {
		return ""
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return ""
	}
	// int() truncates toward zero, which is floor for a positive remaining.
	seconds := int(remaining.Seconds() * ompMaxTimeDeadlineFraction)
	if seconds < 1 {
		return ""
	}
	return strconv.Itoa(seconds)
}

// ompPromptDelivery returns the single prompt token to place after `--`, any argv
// tokens the caller MUST add BEFORE `--`, and a cleanup func the caller MUST
// defer. A normal prompt (below ompMaxArgvPromptBytes) is returned VERBATIM with
// no extra args — byte-identical to the plain argv path. An oversize prompt is
// written to prompt.md in a dedicated temp DIRECTORY and delivered as an `@<abs>`
// attachment, which keeps the argv far below MAX_ARG_STRLEN (see
// ompMaxArgvPromptBytes).
//
// The attachment is read by omp's OWN CLI at parse time and inlined into the same
// turn as `<file name="<abs path>">…</file>`, so — unlike kimi's staged-file
// wrapper — no workspace grant is involved: the agent never has to open the file,
// and there is no tool-scoping failure mode where the real prompt is silently
// lost. The trailing prompt token still has to exist (a run with an attachment and
// no message would be a turn with no instruction), so it is a short pointer at the
// attached content; the real instructions travel inside the file.
//
// Staging into a dedicated temp dir (never the job worktree) keeps the file out of
// an implement job's `git add`, and RemoveAll on cleanup leaves nothing behind.
func ompPromptDelivery(prompt string) (promptArg string, attachArgs []string, cleanup func(), err error) {
	noop := func() {}
	if len(prompt) < ompMaxArgvPromptBytes {
		return prompt, nil, noop, nil
	}
	// Above omp's own attachment ceiling the file channel stops delivering the
	// prompt and starts delivering a placeholder (see ompMaxAttachmentBytes), so
	// there is nothing left to stage into. Refuse the job here rather than run one
	// whose model never saw the task.
	if len(prompt) > ompMaxAttachmentBytes {
		return "", nil, noop, fmt.Errorf(
			"omp prompt is %d bytes, above omp's %d-byte @file attachment ceiling: omp would replace the contents with a `(skipped: too large)` placeholder and answer a task it never received",
			len(prompt), ompMaxAttachmentBytes)
	}
	dir, err := os.MkdirTemp("", "gitmoot-omp-prompt-*")
	if err != nil {
		return "", nil, noop, fmt.Errorf("stage oversize omp prompt: %w", err)
	}
	remove := func() { _ = os.RemoveAll(dir) }
	path := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(path, []byte(prompt), 0o600); err != nil {
		remove()
		return "", nil, noop, fmt.Errorf("write oversize omp prompt: %w", err)
	}
	return ompAttachedPromptPointer(), []string{"@" + path}, remove, nil
}

// ompAttachedPromptPointer is the short message that accompanies an attached
// oversize prompt. omp has already inlined the file's full contents into this same
// turn, so the pointer only has to tell the model that the attachment IS the task,
// not ask it to go read anything.
func ompAttachedPromptPointer() string {
	return "The attached file prompt.md is your complete task: all of its context and the exact output format you must produce are inside it, " +
		"and its full contents are already included in this message. Carry out that task exactly as written and respond exactly as it instructs. " +
		"Do not ask for the file's contents and do not treat its name as the task."
}

// ompUsage holds the token counts extracted from an omp NDJSON stream. Counts
// default to 0 when the stream reports no usage at all.
type ompUsage = StreamUsage

// ompStreamEvent is the verified subset of one `omp -p --mode=json` stdout line.
// Unknown fields and unknown event types are ignored by design (see
// parseOmpStreamJSON).
type ompStreamEvent struct {
	Type string `json:"type"`
	// ID carries the session id, and is populated only on the header line.
	ID        string            `json:"id"`
	Message   *ompStreamMessage `json:"message"`
	Telemetry *ompRunTelemetry  `json:"telemetry"`
	// IsTerminal is set on agent_end and distinguishes a FINAL settle from a
	// scheduled continuation: agent-session.ts emits `{...event, isTerminal:
	// !options?.willContinue}` and eleven call sites pass willContinue:true
	// (retries, model fallbacks, unexpected-stop recovery, auto-compaction).
	//
	// Those non-terminal events are CONSTRUCTED per continuation but are usually not
	// WRITTEN: agent-session.ts:1966-1969 holds a wire-level agent_end while
	// `#promptInFlightCount > 0` in a SINGLE `#pendingAgentEndEmit` slot, and each
	// later one overwrites the previous ("A later agent_end … supersedes the pending
	// one … they only care about the final settle"); #flushPendingAgentEnd
	// (:850-855) then emits exactly one, once the prompt unwinds. Since print mode
	// issues one session.prompt per run, a real stdout stream normally carries ONE
	// agent_end — the final settle. This adapter still handles the multi-event shape
	// (a future build, or a mode that does not hold the event, may write them), but
	// never DEPENDS on seeing one per continuation; see the telemetry note below.
	//
	// It is a *bool so an absent field reads as terminal — an omp build that stops
	// tagging the event must not turn every run into a truncation error.
	IsTerminal *bool `json:"isTerminal"`
	// Success is omp's own verdict on a retry saga, carried by auto_retry_end
	// (agent-session-events.ts:42-47: `{type:"auto_retry_end"; success: boolean;
	// attempt: number; finalError?: string; …}`). It is a *bool so a build that
	// stops sending it leaves the verdict UNKNOWN rather than reading as false.
	Success *bool `json:"success"`
	// FinalError is the cause omp attaches to a giving-up auto_retry_end
	// (agent-session-events.ts:45). It is the ONLY failure text on the wire for a
	// saga whose every attempt ended in a content-less assistant stop: the
	// empty-stop cap closes with `finalError: "Assistant returned empty stop after
	// retry cap…"` (turn-recovery.ts:527-541) and no message on that stream ever
	// carries stopReason error.
	FinalError string `json:"finalError"`
}

// ompStreamMessage is the message payload of a message_end event. errorStatus is
// a number in omp today but is kept raw so a provider that reports it as a string
// degrades the DETAIL of the error rather than making the whole line unparseable.
type ompStreamMessage struct {
	Role         string           `json:"role"`
	Content      []ompContentPart `json:"content"`
	Usage        *ompMessageUsage `json:"usage"`
	StopReason   string           `json:"stopReason"`
	ErrorMessage string           `json:"errorMessage"`
	ErrorStatus  json.RawMessage  `json:"errorStatus"`
}

type ompContentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ompMessageUsage is the PER-ASSISTANT-MESSAGE usage block. Note the field names:
// omp's per-message Usage uses `input`/`output`, NOT the `input_tokens`/
// `output_tokens` spelling every other runtime in this package uses — decoding
// with the wrong tags yields a silent 0/0 for every omp job.
type ompMessageUsage struct {
	Input  int `json:"input"`
	Output int `json:"output"`
}

// ompRunTelemetry is the agent_end run rollup. It exists ONLY when omp was started
// with an OTEL telemetry config, and — unlike the per-message block — it does use
// the inputTokens/outputTokens spelling.
type ompRunTelemetry struct {
	Usage *ompTelemetryUsage `json:"usage"`
}

type ompTelemetryUsage struct {
	InputTokens  int `json:"inputTokens"`
	OutputTokens int `json:"outputTokens"`
}

// parseOmpStreamJSON reads omp's `-p --mode=json` NDJSON stdout and returns the
// final assistant text, the session id from the header line, and the run's token
// usage. It is where SUCCESS IS DECIDED, because omp's exit code cannot decide it:
// in json mode a failed turn exits 0 (its process.exit(1) is inside an
// `if (mode === "text")` branch).
//
// Rules, each of which fails LOUD rather than returning an empty success:
//
//  1. `{"type":"session",…}` supplies the session id (the only channel a finished
//     print-mode run has for it).
//  2. `message_end` with message.role=="assistant" accumulates usage (SUMMED
//     across events — usage is per assistant message, not per run) and keeps the
//     last non-empty text from the content parts of type "text". The role filter
//     is mandatory: message_end also fires for user and tool-result messages.
//  3. an assistant stopReason of "error" or "aborted" — omp's own definition of a
//     retryable failed tail, turn-recovery.ts:84 — marks the turn failed, even
//     on exit 0, and so does omp's own giving-up verdict (auto_retry_end with
//     success:false). The run FAILS whenever that failure is still standing at the
//     end of the stream — see the retry note below for the NARROW set of events
//     that can clear it — and the reported error is the FIRST failure of the
//     unrecovered saga (a later one in the same saga is usually a cascade of it).
//  4. no agent_end line anywhere is an ERROR: it means the CLI died mid-stream, or
//     the envelope drifted so far that this parser understood nothing. Either way
//     an "empty but successful" result would be a lie. A LAST agent_end tagged
//     isTerminal:false is the same class of failure — the stream stopped between
//     continuations — and errors too.
//  5. empty assistant text is an ERROR — an empty review must never read as
//     "nothing to flag".
//  6. unknown event types and unparseable lines are SKIPPED, so new omp event types
//     (auto_compaction_*, model_changed, notice, …) are forward-compatible.
//  7. a retry saga still OPEN at end of stream — an auto_retry_start with no
//     auto_retry_end after it — is an ERROR. The state machine ended in a
//     non-terminal retry state, which means the process died mid-retry; "a retry
//     was pending" is never "the error was being handled".
//
// RETRIES: omp absorbs transient provider failures itself (retry.enabled defaults
// true, retry.maxRetries defaults 10, covering 5xx/overload/429/stream-stall), and
// the failed attempt and the successful one land on the SAME stream: every event
// except agent_end is emitted eagerly (agent-session.ts:2354-2360, pushed onto the
// stream at agent-loop.ts:941/:1201), and the retry decision is taken AFTER that
// message_end. So the first errored assistant turn is NOT the run's verdict.
// Latching it would fail a completed job, discard its answer and its usage, and
// (for a recovered 429 whose text mentions the api key) send operators to fix a
// credential that works.
//
// What CLEARS a pending failure is deliberately narrow: a later assistant
// message_end carrying REAL CONTENT — a non-whitespace text part or a tool call.
// omp's own failure classifier is wider than stopReason error/aborted:
// #isEmptyAssistantStop (turn-recovery.ts:554-576) treats a "stop" with no
// text/toolCall and a "toolUse" with no toolCall as failures, agent-session.ts:
// 2685-2688 routes them into bounded empty-stop retries, and at the cap
// turn-recovery.ts:527-541 closes the saga with auto_retry_end{success:false,
// finalError:"Assistant returned empty stop after retry cap…"}. A content-less turn
// therefore proves nothing about recovery: letting one clear a real 529 would report
// a failed run as a SUCCESS carrying a stale mid-run sentence as the job's answer.
//
// The mirror holds too. auto_retry_end{success:false} is omp giving up —
// turn-recovery.ts:257-261 (error settled without retry), :527-541 (empty-stop cap),
// :1462-1470 (retry budget exhausted), :1490-1495 (classifier refusal) — so it SETS
// the failure, and when the stream carried no earlier cause its finalError IS the
// cause (that is the whole failure signal for an empty-stop saga, whose every
// message has a non-error stopReason).
//
// auto_retry_end{success:true} is NOT a recovery signal, and this parser does not
// treat it as one. It is emitted from exactly one place,
// onAssistantSettledSuccessfully (turn-recovery.ts:224-249), which returns early
// unless the message is a settled, non-empty-stop assistant message (:226-231) —
// and that message_end is already on the wire, because non-agent_end events are
// emitted eagerly (agent-session.ts:2354-2360) while the settle callback runs later
// (agent-session.ts:2477). The ordering the old "success:true clears the failure"
// clause existed for — a recovered saga with NO assistant message after the failure
// — is therefore one omp cannot emit, and honoring it would let a future build turn
// a stale pre-failure sentence into a success. What auto_retry_end does carry, on
// either verdict, is the CLOSE of the saga: turn-recovery.ts:1481-1487 is explicit
// that every dead end must emit one "so subscribers tracking retry-outstanding
// state … don't stay latched on an announcement that never resolves". That is what
// rule 7 checks, and it is why the success:true case is still handled here.
//
// Once a saga IS recovered, its error is no longer the run's cause: the first-error
// latch is dropped together with the flag, so a later, DIFFERENT failure reports
// itself instead of the blip omp already absorbed — and a recovered 429 whose text
// names the api key can no longer drive isOmpAuthFailure into telling operators to
// fix a credential that works. First-wins still holds WITHIN one unrecovered saga.
//
// agent_end.telemetry.usage (OTEL-only) can REPLACE the per-message sum, but only
// when it is demonstrably not an undercount. The rollup is per-agentLoop-invocation,
// not per-run (telemetry.ts:400-412: "Constructed once per `agentLoop` invocation",
// "Per-invocation event collector"), so a retry, model fallback or compaction
// continuation starts a fresh collector and each rollup covers one segment: genuine
// multi-segment rollups are disjoint and are summed.
//
// The trap is that a MISSING segment is undetectable from the stream. Intra-prompt
// agent_end events are superseded rather than written (agent-session.ts:1966-1969,
// single-slot #pendingAgentEndEmit), while message_end is always emitted eagerly
// (agent-session.ts:2354-2360) — so a run whose earlier invocation's agent_end was
// swallowed still shows agentEndCount == rollupCount == 1 and looks fully
// instrumented while a whole invocation's usage is gone. Counting events therefore
// cannot prove coverage, and it is used only as a cheap "partially instrumented"
// reject.
//
// What CAN be checked is the arithmetic: the per-message sum covers every assistant
// message the run produced, so it is a lower bound on what was billed. The rollup is
// preferred only when it is at least that lower bound on BOTH axes; otherwise it is
// covering less than the run did and the per-message sum is the honest source. This
// keeps the OTEL rollup's extra fidelity (it counts what the messages cannot) while
// never letting the reported usage fall BELOW what the assistant messages prove was
// billed. It is a floor, not a completeness proof: a rollup that sits above the
// per-message sum while still omitting a swallowed segment is accepted and
// under-reports that segment — nothing in the stream can detect it.
//
// LINE LENGTH IS UNBOUNDED, deliberately. The sibling adapters read their streams
// with a bufio.Scanner capped at 1 MiB per line, which is safe for them because
// their per-line payload is one message. omp's is not: agent_end re-serializes the
// whole run (`messages: AgentMessage[]`, print-mode.ts writes it as ONE line and
// strips only providerPayload), so the line this parser REQUIRES is structurally
// the largest in the stream and grows with every tool result — a few dozen tool
// calls put it past 1 MiB. A cap there would fail successful jobs, so the stream is
// walked by newline index over the (already in-memory) stdout string instead: no
// ceiling, no per-line copy, and no scanner error path that could discard text and
// usage already parsed.
func parseOmpStreamJSON(output string) (string, string, ompUsage, error) {
	var (
		sessionID     string
		text          string
		summed        ompUsage
		rollup        ompUsage
		rollupCount   int
		agentEndCount int
		lastTerminal  bool
		firstErr      error
		turnFailed    bool
		retryPending  bool
	)
	for rest := output; rest != ""; {
		var line string
		if idx := strings.IndexByte(rest, '\n'); idx >= 0 {
			line, rest = rest[:idx], rest[idx+1:]
		} else {
			line, rest = rest, ""
		}
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		var event ompStreamEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		switch event.Type {
		case "session":
			if event.ID != "" {
				sessionID = event.ID
			}
		case "message_end":
			message := event.Message
			if message == nil || message.Role != "assistant" {
				continue
			}
			if message.Usage != nil {
				summed.InputTokens += message.Usage.Input
				summed.OutputTokens += message.Usage.Output
			}
			if err := ompAssistantError(message); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				turnFailed = true
			} else if ompAssistantCarriesRealContent(message) {
				// The run moved past the failure under its own power, and PROVED it by
				// producing something: a content-less stop is one of omp's own failure
				// shapes, not a recovery (see the retry note on parseOmpStreamJSON).
				// The recovered saga's error goes with it — the next failure, if any,
				// is a different cause and reports itself.
				turnFailed = false
				firstErr = nil
			}
			if content := ompAssistantText(message); content != "" {
				text = content
			}
		case "auto_retry_start":
			// The saga is now OPEN and stays open until omp closes it. Nothing else
			// may clear this: a recovered assistant message is followed by the closing
			// auto_retry_end (turn-recovery.ts:224-249), so a stream that ends here was
			// cut mid-retry.
			retryPending = true
		case "auto_retry_end":
			// omp's own verdict, and the event that CLOSES the saga on EITHER verdict.
			retryPending = false
			// success:false is omp GIVING UP (turn-recovery.ts:257-261, :527-541,
			// :1462-1470, :1490-1495). It is a failure in its own right, and for an
			// empty-stop saga — every message a non-error stop — it is the only failure
			// signal on the wire, so its finalError becomes the cause when nothing
			// earlier in the saga already is one.
			//
			// success:true is NOT treated as a recovery: the successful assistant
			// message_end it necessarily follows has already cleared the failure, and
			// the ordering where it would be the sole recovery signal is one omp cannot
			// emit. An absent success field is an UNKNOWN verdict and changes neither.
			if event.Success != nil && !*event.Success {
				turnFailed = true
				if firstErr == nil {
					firstErr = ompRetryGaveUpError(event.FinalError)
				}
			}
		case "agent_end":
			agentEndCount++
			// Only the LAST agent_end decides. A scheduled continuation CONSTRUCTS a
			// non-terminal one, and on a wire that writes it (see IsTerminal) it lands
			// first; on omp's normal print-mode wire the superseding logic collapses
			// them and this is simply the final settle.
			lastTerminal = event.IsTerminal == nil || *event.IsTerminal
			if event.Telemetry != nil && event.Telemetry.Usage != nil {
				rollupCount++
				rollup.InputTokens += event.Telemetry.Usage.InputTokens
				rollup.OutputTokens += event.Telemetry.Usage.OutputTokens
			}
		}
	}
	usage := summed
	// Prefer the rollup only when every agent_end carried one AND it is at least the
	// per-message sum on both axes. The per-message sum is a lower bound on what the
	// run was billed (it covers every assistant message), so a rollup below it is
	// covering less than the run did — a segment omp never wrote — and taking it
	// would under-report the bill. See the telemetry note on parseOmpStreamJSON.
	if rollupCount > 0 && rollupCount == agentEndCount &&
		rollup.InputTokens >= summed.InputTokens && rollup.OutputTokens >= summed.OutputTokens {
		usage = rollup
	}
	if turnFailed {
		return "", sessionID, usage, firstErr
	}
	if agentEndCount == 0 {
		return "", sessionID, usage, errors.New("omp stream ended without an agent_end event: the CLI died mid-stream or its --mode=json envelope changed")
	}
	if !lastTerminal {
		return "", sessionID, usage, errors.New("omp stream ended on a non-terminal agent_end (isTerminal false): the run was scheduling a continuation (retry, model fallback or auto-compaction) and the stream stopped before it finished")
	}
	// Envelope integrity is settled; the retry STATE MACHINE has to be settled too.
	// omp closes every saga it opens, on both verdicts and from every dead end
	// (turn-recovery.ts:1481-1487 says so in as many words), so an auto_retry_start
	// with nothing after it means the process died mid-retry. Fail closed: a pending
	// retry is a failure, never "the error was being handled".
	if retryPending {
		return "", sessionID, usage, errors.New("omp stream ended with a retry saga still open (an auto_retry_start with no auto_retry_end after it): the CLI died mid-retry, so the run neither recovered nor reported a verdict")
	}
	if strings.TrimSpace(text) == "" {
		return "", sessionID, usage, errors.New("omp stream carried no assistant text")
	}
	return text, sessionID, usage, nil
}

// ompAssistantText concatenates the text parts of one assistant message, ignoring
// thinking, tool-call and image parts.
func ompAssistantText(message *ompStreamMessage) string {
	var builder strings.Builder
	for _, part := range message.Content {
		if part.Type == "text" {
			builder.WriteString(part.Text)
		}
	}
	return builder.String()
}

// ompAssistantCarriesRealContent reports whether an assistant message_end carries
// something the run actually PRODUCED: a non-whitespace text part or a tool call.
// It is the inverse of omp's own #isEmptyAssistantStop (turn-recovery.ts:554-576,
// where hasNonWhitespace is /\S/ — TrimSpace here) and it is what a message must
// pass before it may clear a pending failure.
//
// Two deliberate differences from omp's classifier, both fail-closed:
//
//   - omp only asks the question for stopReason "stop" and "toolUse"; any other
//     stopReason is never an empty stop for it. Here EVERY non-error assistant
//     message must carry content to count as recovery, so a content-less turn with
//     some other stopReason cannot clear a failure either.
//   - omp lets a signed thinking part (thinkingSignature) make a "stop" terminal.
//     Gitmoot cannot report a signature as a job's answer — the run's Summary would
//     have to come from a message written BEFORE the failure, which is exactly the
//     stale-text false green this rule exists to stop — so signed thinking alone
//     does not clear a failure here.
func ompAssistantCarriesRealContent(message *ompStreamMessage) bool {
	for _, part := range message.Content {
		switch part.Type {
		case "toolCall":
			return true
		case "text":
			if strings.TrimSpace(part.Text) != "" {
				return true
			}
		}
	}
	return false
}

// ompRetryGaveUpError renders omp's own giving-up verdict as the run's cause. It is
// used only when the saga carried no earlier failure text — the empty-stop cap is
// the shape that produces exactly that, since every message on such a stream has a
// non-error stopReason and the finalError on auto_retry_end is the only diagnosis
// omp ever writes (turn-recovery.ts:527-541).
func ompRetryGaveUpError(finalError string) error {
	if detail := strings.TrimSpace(finalError); detail != "" {
		return fmt.Errorf("omp gave up retrying (auto_retry_end success false): %s", detail)
	}
	return errors.New("omp gave up retrying (auto_retry_end success false) and reported no finalError")
}

// ompAssistantError converts a failed assistant turn into an error, preserving the
// provider's own message and HTTP status so operators see the real cause instead
// of "the job produced no result".
func ompAssistantError(message *ompStreamMessage) error {
	switch message.StopReason {
	case "error", "aborted":
	default:
		return nil
	}
	detail := strings.TrimSpace(message.ErrorMessage)
	if detail == "" {
		detail = "no errorMessage reported"
	}
	if status := ompErrorStatus(message.ErrorStatus); status != "" {
		return fmt.Errorf("omp turn failed (stopReason %s, errorStatus %s): %s", message.StopReason, status, detail)
	}
	return fmt.Errorf("omp turn failed (stopReason %s): %s", message.StopReason, detail)
}

// ompErrorStatus renders the raw errorStatus value (a number today) as text,
// returning "" when the field is absent or null.
func ompErrorStatus(raw json.RawMessage) string {
	status := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if status == "" || status == "null" {
		return ""
	}
	return status
}

// ompCommandError classifies a failed omp run — a non-zero exit as well as an
// exit-0 run whose stream said it failed — and attaches the auth fix when the
// failure looks like a credential problem.
//
// It deliberately does NOT use commandError's stdout fallback: omp's stdout is the
// whole NDJSON stream, and the real message has already been lifted out of it by
// parseOmpStreamJSON, so falling back to stdout would bury a one-line cause under
// the entire transcript.
func ompCommandError(result subprocess.Result, err error) error {
	base := err
	if detail := strings.TrimSpace(result.Stderr); detail != "" {
		base = fmt.Errorf("%s: %w", detail, err)
	}
	if !isOmpAuthFailure(result, err) {
		return base
	}
	return fmt.Errorf("omp authentication required. %s: %w", OmpAuthSetupMessage, base)
}

// isOmpAuthFailure reports whether an omp failure looks like a credential problem.
// It scans stderr and the FAILURE text — never the whole stdout transcript: the
// auth failure that matters most arrives on exit 0 inside the stream's
// errorMessage, which parseOmpStreamJSON has already lifted into the error, while
// stdout also carries the model's own prose, where a review that merely discusses
// API keys would masquerade as an auth failure.
func isOmpAuthFailure(result subprocess.Result, err error) bool {
	parts := []string{result.Stderr}
	if err != nil {
		parts = append(parts, err.Error())
	}
	text := strings.ToLower(strings.Join(parts, "\n"))
	return strings.Contains(text, "unauthorized") ||
		strings.Contains(text, "authentication") ||
		strings.Contains(text, "authenticate") ||
		strings.Contains(text, "api key") ||
		strings.Contains(text, "no model available") ||
		strings.Contains(text, "credential")
}
