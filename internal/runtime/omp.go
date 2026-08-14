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

func (a OmpAdapter) PermissionPolicyApplication(Agent) PermissionPolicyApplication {
	return PermissionPolicyNotApplied
}

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
	// Start never carries a plan request: it registers a seat, it does not run a
	// brief. The primitives are passed explicitly so the single argv builder has no
	// implicit default to drift from.
	args := ompArgs(request.Agent, request.Agent.Model, ompThinkingLevel(request.Agent.Effort), ompMaxTimeArg(ctx), false, "", attachArgs, promptArg)
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
	//
	// Raw is the RAW STDOUT on this path, matching the two failure returns above and
	// the Deliver contract: the failure is about the ENVELOPE, so the envelope is the
	// evidence. Handing back the parsed assistant text instead would throw away the
	// only thing that can explain a missing header — which line omp actually wrote
	// first — and leave the operator a sentence from a run whose id is gone.
	if sessionID == "" {
		return StartResult{Raw: result.Stdout}, errors.New("omp stream carried no session header id; cannot register a runtime reference")
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
	// Plan shape is validated before the PATH preflight: it is a property of the
	// REQUEST, so it is wrong for its diagnosis to depend on whether omp happens to
	// be installed.
	if err := ompValidatePlan(job.Plan, job.PlanInto); err != nil {
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
	planMode := PlanModeDescriptor(job.Plan, job.PlanInto)
	args := ompArgs(agent, EffectiveModel(agent, job), ompThinkingLevel(effectiveEffort(agent, job)), ompMaxTimeArg(ctx), job.Plan, job.PlanInto, attachArgs, promptArg)
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
	// A FAILED RUN IS STILL BILLED, on this path as much as on the parse-error path
	// below: a run that spent 400k tokens and then died on a non-zero exit (an OOM
	// kill, a crashed Bun binary, a SIGKILLed process group) spent them for real, and
	// reporting 0/0 would hide the most expensive failures from every spend audit.
	// The usage is whatever the partial stream proved before the process went down.
	if err != nil {
		return Result{
			Raw:          result.Stdout + result.Stderr,
			InputTokens:  usage.InputTokens,
			OutputTokens: usage.OutputTokens,
			PlanMode:     planMode,
			SessionDiag:  newSessionDiag(result, err, sessionID),
		}, ompCommandError(result, err)
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
			PlanMode:     planMode,
			SessionDiag:  newSessionDiag(result, nil, sessionID),
		}, ompCommandError(result, parseErr)
	}
	return Result{
		Raw:          content,
		Summary:      strings.TrimSpace(content),
		InputTokens:  usage.InputTokens,
		OutputTokens: usage.OutputTokens,
		PlanMode:     planMode,
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
//	    [--model <M>] [--thinking <lvl>] [--max-time <s>]
//	    [--plan-yolo [--plan-yolo-into <M>]] [@<staged>/prompt.md]
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
//   - `--plan-yolo` is emitted IF AND ONLY IF the job asked for plan mode. It is
//     ORTHOGONAL to --approval-mode: plan mode is workflow shape (plan first, then
//     AUTO-EXECUTE the plan), the approval mode is write permission, and --plan-yolo
//     starts read-only and auto-approves the model's own plan on its first resolve
//     call. `--plan-yolo-into <M>` pins the model the execution phase runs on and is
//     only accepted alongside --plan-yolo (omp's own default target is the "smol"
//     role); ompValidatePlan rejects the unpaired form BEFORE any subprocess, and a
//     plan request aimed at a runtime SupportsPlanMode rejects never reaches an argv
//     at all. A silent downgrade to a normal run is the defect this ordering exists
//     to prevent.
var ompRuntimeContract = RuntimeContract{
	Binary: "omp",
	Requirements: []RuntimeRequirement{
		{Kind: RuntimeRequirementFlag, Name: "flag -p", Flag: "-p", Source: "internal/runtime/omp.go::ompArgs", Remedy: "install an omp CLI that lists -p, or run the job on a runtime whose installed CLI satisfies its declared contract"},
		{Kind: RuntimeRequirementFlag, Name: "flag --mode", Flag: "--mode", Source: "internal/runtime/omp.go::ompArgs", Remedy: "install an omp CLI that lists --mode, or run the job on a runtime whose installed CLI satisfies its declared contract"},
		{Kind: RuntimeRequirementFlag, Name: "flag --approval-mode", Flag: "--approval-mode", Source: "internal/runtime/omp.go::ompArgs", Remedy: "install an omp CLI that lists --approval-mode, or run the job on a runtime whose installed CLI satisfies its declared contract"},
		{Kind: RuntimeRequirementFlag, Name: "flag --no-session", Flag: "--no-session", Source: "internal/runtime/omp.go::ompArgs", Remedy: "install an omp CLI that lists --no-session, or run the job on a runtime whose installed CLI satisfies its declared contract"},
		{Kind: RuntimeRequirementFlag, Name: "flag --plan-yolo", Flag: "--plan-yolo", Source: "internal/runtime/omp.go::ompArgs", Remedy: "install an omp CLI that lists --plan-yolo, or run the job on a runtime whose installed CLI satisfies its declared contract"},
		{Kind: RuntimeRequirementFlag, Name: "flag --plan-yolo-into", Flag: "--plan-yolo-into", Source: "internal/runtime/omp.go::ompArgs", Remedy: "install an omp CLI that lists --plan-yolo-into, or run the job on a runtime whose installed CLI satisfies its declared contract"},
	},
}

// ompValidatePlan rejects a plan request omp's CLI cannot honour BEFORE any
// subprocess runs. `--plan-yolo-into` without `--plan-yolo` makes omp exit
// non-zero with no NDJSON envelope at all, which this adapter would diagnose as a
// truncated stream — the wrong cause for a request shape we can reject here with
// the actual fix.
func ompValidatePlan(plan bool, planInto string) error {
	if into := strings.TrimSpace(planInto); into != "" && !plan {
		return fmt.Errorf("omp plan target %q requires plan mode: --plan-yolo-into is only accepted alongside --plan-yolo (set plan on the job, or drop plan_into)", into)
	}
	return nil
}

func ompArgs(agent Agent, model string, thinking string, maxTime string, plan bool, planInto string, attachArgs []string, prompt string) []string {
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
	// Plan mode is opt-in and never inferred: no plan request, no flag, so a normal
	// run's argv stays byte-identical. --plan-yolo-into is meaningless on its own
	// (ompValidatePlan already refused that shape) and so is nested under it here.
	if plan {
		args = append(args, "--plan-yolo")
		if into := strings.TrimSpace(planInto); into != "" {
			args = append(args, "--plan-yolo-into", into)
		}
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

// ompLoadBearingEventTypes is the CLOSED set of event types parseOmpStreamJSON
// actually READS. It exists so rule 6 can tell a row this parser depends on from a
// row it merely tolerates: an unknown type is forward-compat and stays skipped even
// when its payload will not decode, while an undecodable row of one of THESE types
// fails the run (see ompUndecodableEventType).
//
// This map GATES the dispatch in parseOmpStreamJSON instead of merely describing it:
// the loop consults it before the switch, so the two cannot drift in the dangerous
// direction. A case added to that switch without an entry here is UNREACHABLE — its
// behaviour is simply absent, which the first test exercising it reports — rather
// than working on the happy path and going silently missing on envelope drift, which
// is the defect this map exists to close. That is a structural property of the
// dispatch, not a behavioural one, so no test is claimed for it.
//
// The opposite direction is NOT structural and is not claimed to be: an entry here
// with no case in the switch would fail runs over a row nobody reads, and nothing
// detects it. What IS enforced is fixture parity —
// TestParseOmpStreamJSONUndecodableKnownEventFailsRun enumerates this map and demands
// an undecodable fixture per member — so a new entry cannot land unexercised.
var ompLoadBearingEventTypes = map[string]struct{}{
	"session":          {},
	"message_end":      {},
	"auto_retry_start": {},
	"auto_retry_end":   {},
	"agent_end":        {},
}

// ompEventTypeProbe re-reads ONLY the `type` field of a line whose full decode into
// ompStreamEvent failed. Declaring one field is the point: encoding/json skips every
// other field WITHOUT type-checking it, so this probe cannot fail for the reason the
// full decode did, and it succeeds for any valid JSON object carrying a string type.
type ompEventTypeProbe struct {
	Type string `json:"type"`
}

// ompMessageRoleProbe re-reads ONLY message.role, by the same one-field trick. The
// role is message_end's SECOND discriminator: it decides whether this parser would
// have read the row at all (rule 2). Message is a pointer, and its role a plain
// string, so "no message object at all" and "an object with no role" are both
// distinguishable from a role that is actually there.
type ompMessageRoleProbe struct {
	Message *struct {
		Role string `json:"role"`
	} `json:"message"`
}

// ompUndecodableEventType classifies a line whose decode into ompStreamEvent FAILED.
// It returns the line's event type and true only when that type is one this parser
// reads AND the row is one this parser would have read; ("", false) means the line is
// not valid JSON at all, or carries a type this parser never looks at, or is a
// message_end whose role puts it outside what the parser reads — all SKIPPED under
// rule 6, which covers exactly the rows this parser does not read.
//
// message_end is the one load-bearing type with that second discriminator, and the
// role check is not an optimization — without it rule 6 fails HEALTHY runs. omp emits
// message_end for every input, aside, steer and custom message of a turn as well as
// for assistant ones (agent-loop.ts:938-943 emitInputMessages, written verbatim to
// stdout in json mode by print-mode.ts:173-176), and those payloads legitimately do
// not decode here: UserMessage, DeveloperMessage and CustomMessageContent all type
// content as `string | (TextContent | ImageContent)[]` (packages/ai/src/types.ts:
// 803-805 and 817-819, session/messages.ts:314) while this adapter declares
// []ompContentPart, so a plain-string content is an UnmarshalTypeError on a row
// NOTHING here reads. Such rows are routine — plan/goal/vibe-mode preludes, todo
// reminders, late LSP diagnostics, thinking-loop redirects — so failing them would
// report complete runs as envelope drift and discard their answers: the exact mirror
// of the false green rule 6 exists to close.
//
// Both discriminators are re-read through their own probe rather than taken from the
// partially filled event struct. Against encoding/json today that is a distinction
// with no behavioural difference — the decoder records an UnmarshalTypeError and keeps
// going, so the struct a failed decode leaves behind normally carries these fields
// anyway, and no test in this package can tell the two readings apart. The probes are
// still preferred because their contract is legible from their declarations (one
// field; nothing else is even type-checked) whereas the struct's depends on decoder
// internals. No guard is claimed for that preference.
func ompUndecodableEventType(line string) (string, bool) {
	var probe ompEventTypeProbe
	if err := json.Unmarshal([]byte(line), &probe); err != nil {
		return "", false
	}
	if _, ok := ompLoadBearingEventTypes[probe.Type]; !ok {
		return "", false
	}
	if probe.Type == "message_end" && !ompUndecodableMessageEndWasRead(line) {
		return "", false
	}
	return probe.Type, true
}

// ompUndecodableMessageEndWasRead reports whether an undecodable message_end row is
// one parseOmpStreamJSON would have READ — that is, an ASSISTANT message.
//
// It fails CLOSED on an unreadable discriminator: a `message` that is not an object,
// or an object carrying no role at all, counts as read. Rule 6 is the admission that
// a row could not be decoded, and answering "some other role, skip it" out of a
// payload that would not decode is exactly the guess the rule refuses — a drifted
// assistant row that also lost its role must not be able to buy silence with the
// second defect.
//
// This used to be documented here as deliberately STRICTER than the decodable path,
// "which skips a roleless message_end under rule 2's filter: there the row WAS read
// and its role really is absent". That claim is OVERRULED — the g7 delta review at
// cb34f666 disproved it by probe. A decodable row's absent role is not read either,
// it is FILLED IN by encoding/json's zero value, and a failed assistant message_end
// with role:null (or no role member, or no message member) took exactly the silence
// this function refuses, through the decoder rather than around it. The two paths now
// agree: a role that is there and is not "assistant" skips, a role that is not there
// fails (parseOmpStreamJSON's message_end case, ompRolelessMessageEndError).
func ompUndecodableMessageEndWasRead(line string) bool {
	var probe ompMessageRoleProbe
	if err := json.Unmarshal([]byte(line), &probe); err != nil {
		return true
	}
	if probe.Message == nil || probe.Message.Role == "" {
		return true
	}
	return probe.Message.Role == "assistant"
}

// ompUnreadableRowError classifies a line whose decode into ompStreamEvent FAILED and
// returns the error to REPORT, or nil when rule 6 skips the line. It is the whole of
// rule 6's failing half for lines the decoder rejected, and it splits on one question
// the decoder already answered: is the line valid JSON at all?
//
//   - VALID JSON that failed to decode has FIELDS, so it is classified from them
//     (ompUndecodableEventType): the type this parser reads, and for message_end the
//     role that decides whether it would have read the row.
//   - NOT VALID JSON has no fields to read — the line was cut, interleaved with
//     another writer, or is not a row at all — so the only reading left is the text
//     itself (ompMalformedAssistantMessageEnd), and that reading is deliberately
//     narrow: it fails ONLY on a fragment that still identifies an assistant
//     message_end.
//
// The split is load-bearing, not tidiness, and the reason is PRECEDENCE: where the
// structure survived, the row's own `type` field is present and authoritative, and a
// substring scan cannot improve on it — it can only overrule it. A marker matched in
// raw text says nothing about WHOSE field it is (a value, a key, a nested object),
// while the row's `type` already answers the only question rule 6 asks: would this
// parser have read the row. So structure wins wherever structure is left, and the
// raw-text reading applies only where there is none.
//
// The rationale previously stated here was different and is WITHDRAWN: it claimed an
// unknown-type row "may legitimately CARRY message rows inside it — an auto-compaction
// handoff re-serializes what it summarized". Checked against omp at 06343fef4: the
// session-level arms of the AgentSessionEvent union (agent-session-events.ts:12-64,
// ending at goal_updated) plus the core AgentEvent members the union pulls in via
// its Exclude arm (packages/agent/src/types.ts) were read. auto_compaction_end's
// CompactionResult names only string/number scalars — though its `details?: T`
// (T = unknown) and `preserveData` members are unconstrained, so the claim that
// carries this branch is the PRECEDENCE rule above, not any payload-shape guarantee.
// agent_end carries `messages: AgentMessage[]`, and a message is keyed by `role` —
// the custom kinds add `customType`, none of them a `type` (messages.ts:852-908) —
// so no verified omp event nests a serialized `{"type":"message_end",...}` row. The
// fixture pinning this branch is therefore a CONSTRUCTED row, not an observed one,
// and it pins the precedence rule above rather than a shape omp emits.
//
// What the split does NOT buy, stated plainly: on the invalid-JSON side there is no
// structure to appeal to, so a cut row of ANY type that carries both markers fails
// the run — including, if such a row ever existed, the cut variant of the nested
// shape just withdrawn. That asymmetry is the price of a last-resort reading, and it
// is why identification demands BOTH markers rather than either one (see
// ompMalformedAssistantMessageEnd).
func ompUnreadableRowError(line string, decodeErr error) error {
	if !json.Valid([]byte(line)) {
		if !ompMalformedAssistantMessageEnd(line) {
			return nil
		}
		return fmt.Errorf(
			"omp stream carried a MALFORMED assistant message_end (envelope drift): the line is not valid JSON (%w) yet still identifies itself as one, carrying a \"type\" of message_end and a \"role\" of assistant — so an assistant turn's verdict, answer and usage are all unknown and the stream cannot be trusted to report success",
			decodeErr)
	}
	eventType, loadBearing := ompUndecodableEventType(line)
	if !loadBearing {
		return nil
	}
	return fmt.Errorf(
		"omp stream carried a %s event this parser could not decode (envelope drift): %w — %s is load-bearing here, so the verdict, answer and usage that row carried are all unknown and the stream cannot be trusted to report success",
		eventType, decodeErr, eventType)
}

// ompMalformedAssistantMessageEnd reports whether a line that is NOT valid JSON still
// identifies itself as the one row whose loss is invisible to every other rule: an
// ASSISTANT message_end.
//
// The g7 delta review at cb34f666 found the gap by probe. A row cut mid-write is not
// garbage in the sense rule 6 means: when it is the FINAL assistant message_end of an
// otherwise complete run, the terminal agent_end still arrives (the cut is upstream of
// it), so rule 4 sees an agent_end, rule 7 sees no open saga, and rule 5 reads the
// PREVIOUS turn's sentence as the run's answer. Nothing else catches it, which is why
// the earlier deliberate seam — "a line that is not valid JSON is always skipped" —
// could not stand.
//
// Both markers are required, and both are matched on the raw text, so WHERE the line
// was cut does not matter: `"type"` must identify message_end and `"role"` must
// identify assistant. One marker alone is not identification — a truncated agent_end
// re-serializes the whole transcript and carries assistant roles inside it, a
// message_end cut before its role could be any role, and a stray log line is neither —
// and failing on those would turn omp's own stdout warnings into failed jobs.
//
// That leaves ONE residual, stated rather than hidden: a line cut before its role
// reaches the wire is still skipped, so an assistant message_end truncated that early
// can still take an earlier turn's sentence with it. Closing it would mean failing on
// every fragment that merely LOOKS like a message_end, and omp writes a message_end
// for every tool result and every input message of a turn — the cure would fail
// healthy runs far more often than the disease corrupts them. The line is drawn at
// identification, not suspicion.
func ompMalformedAssistantMessageEnd(line string) bool {
	return ompRawFieldIs(line, "type", "message_end") &&
		ompRawFieldIs(line, "role", "assistant")
}

// ompRawFieldIs reports whether the raw text of a line shows `"<field>": "<value>"`
// anywhere in it. It is a LAST RESORT reading used only on lines encoding/json
// rejected outright (see ompUnreadableRowError), so it cannot lean on structure: it
// scans every occurrence of the key, since a cut line may repeat it or carry it
// nested, and a match anywhere is what "identifiable" means here.
//
// A JSON string cannot contain a raw `"` — the encoder escapes it as `\"` — so a
// value quoted inside some other string's TEXT cannot produce a match: the bytes
// preceding the key would be `\"` rather than `"`. That is what keeps a tool result
// whose output happens to quote a message_end from being mistaken for one.
//
// Three precision claims are made above and all three are now pinned by fixtures
// (TestOmpRawFieldIsPrecision) rather than asserted: scan-all — a key occurrence that
// is not a field does not end the search, because a cut line may carry the key as
// some other field's value before the real one; colon-required — a key merely
// ADJACENT to the value is not a field, so a fragment whose colons did not survive
// stays skipped; and value-at-head — the value must begin right after the colon, so a
// row that merely NAMES message_end somewhere later (a notice, a diagnostic) is not
// one. Each was an unpinned claim until the g7 verification at cb34f666 mutated it
// and nothing failed; the loose readings move lines between "skipped" and "failed
// job" in both directions, which is why they are pinned at the helper AND at the
// stream.
func ompRawFieldIs(line, field, value string) bool {
	key := `"` + field + `"`
	want := `"` + value + `"`
	for rest := line; ; {
		idx := strings.Index(rest, key)
		if idx < 0 {
			return false
		}
		rest = rest[idx+len(key):]
		after := strings.TrimLeft(rest, " \t")
		if !strings.HasPrefix(after, ":") {
			continue
		}
		if strings.HasPrefix(strings.TrimLeft(after[1:], " \t"), want) {
			return true
		}
	}
}

// ompRolelessMessageEndError is the verdict on a message_end that DECODED and still
// cannot be attributed to a role: role null, role absent, or no message member at all.
//
// Rule 2 filters on role, and encoding/json fills a missing string with "" — so
// without this the filter would read a zero value as "some other role" and skip the
// row. That is the same guess ompUndecodableMessageEndWasRead refuses on the
// undecodable path, arriving through the decoder instead of around it: a FAILED
// assistant turn whose role also went missing would vanish from the stream, every
// other envelope rule would still pass, and the run would report SUCCESS carrying the
// sentence an EARLIER turn wrote. Fail closed instead — the role is a load-bearing
// discriminator, and a load-bearing field that is gone is envelope drift.
func ompRolelessMessageEndError() error {
	return errors.New("omp stream carried a message_end with no role (envelope drift): message.role is the discriminator that decides whether this parser reads the row at all, and a row without one cannot be shown to be a non-assistant message — a FAILED assistant turn that also lost its role would otherwise be filtered out as \"some other role\" and hand an earlier turn's sentence back as the run's answer")
}

// ompStreamEvent is the verified subset of one `omp -p --mode=json` stdout line.
// Unknown FIELDS are ignored by design, and so are whole lines of an unknown event
// type; a line of a KNOWN type that fails to decode is not (see rule 6 on
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
//     FINAL assistant message's text from its content parts of type "text" — the
//     final one, NOT the last non-empty one (rule 5). The role filter is
//     mandatory: message_end also fires for user and tool-result messages. It
//     filters on a role that is THERE: a message_end that decoded but carries NO
//     role — role null, role absent, or no message object at all — is drift on a
//     load-bearing type and fails the run under rule 6, because "not assistant" out
//     of a field Go zero-valued is a guess, not a reading (see
//     ompRolelessMessageEndError).
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
//  5. the FINAL assistant message must BE the answer. Empty text there is an
//     ERROR — an empty review must never read as "nothing to flag" — and so is a
//     final stopReason of "toolUse" or "length", which mean the run was CUT
//     rather than finished (see the truncation note below). Neither one may fall
//     back to an earlier turn's sentence.
//  6. FORWARD COMPATIBILITY COVERS THE ROWS THIS PARSER DOES NOT READ, and only
//     those. A line that is not valid JSON at all is SKIPPED (garbage on the wire
//     — a stray log line, a half-written tail — is not evidence about the run)
//     UNLESS the fragment still IDENTIFIES itself as a row this parser reads: a
//     `"type"` of message_end together with a `"role"` of assistant, matched at
//     substring level so the point where the line was cut does not matter (see
//     ompMalformedAssistantMessageEnd). That row fails the run — nothing else on
//     the stream covers it, because a mid-stream cut leaves the terminal agent_end
//     intact and rules 4 and 7 never fire. Skipped too is a valid-JSON line whose
//     type this parser never looks at
//     (auto_compaction_*, model_changed, notice, …), whether or not its payload
//     would decode — as is a message_end this parser would have filtered out anyway
//     ON A ROLE IT COULD READ, since role, not just type, decides what rule 2 reads
//     (omp emits message_end for user/developer/custom messages whose content is a
//     bare string, which cannot decode here and never could be read here either; see
//     ompUndecodableEventType).
//     But a line whose type IS load-bearing here — session, message_end,
//     auto_retry_start, auto_retry_end, agent_end, the closed set in
//     ompLoadBearingEventTypes — and whose payload FAILS to decode is an ERROR
//     naming that type, and so is a message_end that DECODED while carrying no role
//     at all (rule 2). The parser was supposed to read that row, so its verdict,
//     its text and its usage are all unknown, and a stream with an unreadable
//     load-bearing row cannot be trusted to report SUCCESS. Skipping one is what
//     makes a failed final message_end whose usage block drifted (a provider that
//     starts reporting `usage.input` as a string) disappear from the stream and
//     hands the PREVIOUS turn's sentence back as the run's answer — the same
//     stale-text false green rule 5 and the TRUNCATION note exist to refuse. Each
//     of the three shapes this rule fails on is a row this parser was GOING to read
//     and then could not: an undecodable payload, an unattributable role, a line cut
//     mid-write. None of them is forward compatibility.
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
// ONE EXCEPTION to the clear: a failure SET by auto_retry_end{success:false} —
// omp's own giving-up verdict — is latched (gaveUp) and no later content can clear
// it. A run omp itself declared lost cannot be talked back into success by a
// message that arrives afterwards, and the sentence such a message would hand back
// is the same stale text this whole section exists to refuse. This guard is LATENT
// against omp v17.2.4: every success:false emitter in turn-recovery.ts (:257-261,
// :527-541, :1462-1470, :1490-1495, :1512-1520, :1541) is immediately followed by
// `return false` and a settled turn, so no post-give-up assistant message_end can
// reach this parser today. It is here because the SET is a binding rule, and a rule
// that can be silently un-set is not one.
//
// TRUNCATION: a run can be cut off while every envelope rule above still passes,
// and this adapter MANUFACTURES the shape that does it. ompMaxTimeArg puts
// --max-time on every daemon-dispatched job (the daemon wraps each job in a
// context deadline), and when omp's own deadline trips inside the tool loop it
// clears hasMoreToolCalls (agent-loop.ts:1283-1286), pairs each pending tool call
// with a synthetic `aborted` TOOL RESULT (:1349-1366, filtered out here by the role
// check) and calls endAgentStream, which writes a TERMINAL agent_end (:1398-1400).
// The provider's output cap does the same via stopReason "length" (:1352). Nothing
// on that wire carries an error stopReason: the run is envelope-perfect and simply
// never answered. That is why rule 5 reads the FINAL assistant message rather than
// the last one that happened to carry text — keeping a mid-run "Let me read the
// file first." and reporting it as the job's Summary is precisely the stale-text
// false green ompAssistantCarriesRealContent refuses on the failure-clearing path.
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
		sessionID string
		// finalText/finalStop describe the LAST assistant message_end on the wire —
		// the only one that can be the run's answer. sawText remembers whether ANY
		// assistant message carried text, purely so a truncated run can be told apart
		// from a run that never spoke at all when the failure is reported.
		finalText     string
		finalStop     string
		sawText       bool
		summed        ompUsage
		rollup        ompUsage
		rollupCount   int
		agentEndCount int
		lastTerminal  bool
		firstErr      error
		turnFailed    bool
		retryPending  bool
		gaveUp        bool
		// unreadable is the FIRST load-bearing row this parser could not read
		// (rule 6) in ANY of its three shapes — an undecodable payload, a
		// message_end with no attributable role, a line cut mid-write that still
		// identifies an assistant message_end. First, not last, because a later
		// drift is usually the same drift again and the earliest one is where an
		// operator has to look.
		//
		// Two guards enforce that, one per arm, and each needs its own fixture:
		// "two undecodable rows report the FIRST" covers only the undecodable arm
		// (two rows of the SAME shape produce identical text, so they cannot tell
		// first-wins from last-wins on the OTHER arm at all). The roleless arm's
		// guard is pinned by "the FIRST unreadable row wins ACROSS shapes", which
		// mixes a malformed row with a roleless one in both orders — the only
		// stream shape that can distinguish them.
		// It is a latch of its own rather than a firstErr/turnFailed pair on
		// purpose: the content-carrying recovery clause below may clear a failure
		// the stream REPORTED, but it may not clear the parser's own admission that
		// it could not read a row — the recovery evidence and the unread row are not
		// even about the same event.
		unreadable error
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
			// Rule 6, the narrow half: a row this parser READS that it cannot read is
			// a failure, not forward compatibility. Everything else — unidentifiable
			// garbage, any unknown type, and a message_end whose role puts it outside
			// rule 2's filter — is still skipped (ompUnreadableRowError).
			if rowErr := ompUnreadableRowError(line, err); rowErr != nil && unreadable == nil {
				unreadable = rowErr
			}
			// Keep scanning rather than bailing: the run is already lost, but the
			// session id and the usage the decodable messages prove were billed are
			// still worth reporting (see the FAILED RUN IS STILL BILLED note on
			// Deliver) — including the ones that come AFTER the bad row, which is
			// what "diagnostics survive a bad row anywhere in the stream" pins.
			continue
		}
		// The classifier set gates the dispatch, so the switch below cannot acquire a
		// case that rule 6 does not know about: an unlisted case is unreachable rather
		// than silently-skipped-when-undecodable (see ompLoadBearingEventTypes). This
		// is a structural constraint and behaviour-preserving by construction — an
		// event type with no case is a no-op either way — so there is no mutant to
		// kill here, and none is claimed.
		if _, loadBearing := ompLoadBearingEventTypes[event.Type]; !loadBearing {
			continue
		}
		switch event.Type {
		case "session":
			if event.ID != "" {
				sessionID = event.ID
			}
		case "message_end":
			message := event.Message
			// The role has to be READ before it can filter. A message_end that
			// decoded while carrying no role — role null, role absent, or no message
			// member at all — is drift on a load-bearing type, and treating the zero
			// value encoding/json left behind as "some other role" is what lets a
			// FAILED assistant turn disappear and an earlier turn's sentence stand as
			// the run's answer (see ompRolelessMessageEndError).
			if message == nil || message.Role == "" {
				if unreadable == nil {
					unreadable = ompRolelessMessageEndError()
				}
				continue
			}
			if message.Role != "assistant" {
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
			} else if !gaveUp && ompAssistantCarriesRealContent(message) {
				// The run moved past the failure under its own power, and PROVED it by
				// producing something: a content-less stop is one of omp's own failure
				// shapes, not a recovery (see the retry note on parseOmpStreamJSON).
				// The recovered saga's error goes with it — the next failure, if any,
				// is a different cause and reports itself.
				//
				// gaveUp is the one failure this cannot reach: omp already announced it
				// was giving up, and a verdict the RUNTIME ITSELF issued is not something
				// a later message gets to overturn (see the exception note above — latent
				// against v17.2.4, deliberate all the same).
				turnFailed = false
				firstErr = nil
			}
			// The FINAL assistant message REPLACES whatever an earlier one said, even
			// with nothing — carrying the last non-empty text forward is what turns a
			// truncated run into a false green (see the TRUNCATION note above).
			content := ompAssistantText(message)
			if strings.TrimSpace(content) != "" {
				sawText = true
			}
			finalText = content
			finalStop = message.StopReason
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
				// LATCHED, not merely set: this verdict is omp's own, and the
				// content-carrying clause above may not clear it.
				gaveUp = true
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
	// An unreadable load-bearing row is reported BEFORE any verdict derived from the
	// readable ones — the ordering matters and is pinned ("an unreadable row outranks
	// a failure the stream reported"). Every rule below reasons about what the stream
	// said; this one says a piece of the stream could not be read at all, which is the
	// newer and more actionable fact (envelope drift an operator must fix) and the one
	// that makes the other verdicts provisional — the unread row may itself have been
	// the failure, the answer, or the terminal settle.
	if unreadable != nil {
		return "", sessionID, usage, unreadable
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
	// The envelope and the retry state machine are settled; what is left is whether
	// the run actually ANSWERED. Both checks below read the FINAL assistant message
	// only: an earlier turn's sentence was written before the cut, so handing it back
	// as the job's Summary is the stale-text false green (see TRUNCATION above).
	if ompStopReasonIsTruncation(finalStop) {
		return "", sessionID, usage, fmt.Errorf("omp run was TRUNCATED: its final assistant message stopped on %q instead of \"stop\", so the run was cut mid-work (Gitmoot's --max-time deadline for the job, or the provider's output cap) and never wrote a final answer", finalStop)
	}
	if strings.TrimSpace(finalText) == "" {
		if sawText {
			return "", sessionID, usage, errors.New("omp run was TRUNCATED: its final assistant message carried no text (a tool call it never got to answer, or a content-less stop), so the only text on the stream was written BEFORE the run was cut — and that is not the run's answer")
		}
		return "", sessionID, usage, errors.New("omp stream carried no assistant text")
	}
	return finalText, sessionID, usage, nil
}

// ompStopReasonIsTruncation reports whether a FINAL assistant stopReason means the
// run was CUT rather than finished. omp's stopReason union is closed — "stop" |
// "length" | "toolUse" | "error" | "aborted" (packages/ai/src/types.ts:792) — and
// error/aborted have already failed the run by the time this is asked, so the two
// truncating values are the whole remainder that is not "stop":
//
//   - "toolUse" — the agent loop was still calling tools. This is the shape
//     --max-time produces: agent-loop.ts:1281-1286 computes hasMoreToolCalls =
//     runnableStop && toolCalls.length > 0 and then clears it once the deadline has
//     passed, so the loop exits on a toolUse tail and settles terminally.
//   - "length" — the provider hit its output cap mid-message. omp never continues
//     past it: continuation is gated on runnableStop, which agent-loop.ts:1280
//     computes as stopReason "toolUse" or "stop" — "length" is neither, so
//     hasMoreToolCalls is false (:1281), the loop ends here, and whatever text the
//     message carries is half a sentence, not a verdict.
//
// The "toolUse" arm is deliberately WIDER than omp's own reading, and this is the
// one place this parser knowingly overrules the runtime rather than following it.
// omp settles a toolUse stop as COMPLETE — no retry, no recovery — as soon as the
// message carries a toolCall block OR any non-whitespace text: turn-recovery.ts's
// #isEmptyAssistantStop (:554), case "toolUse", returns false in exactly those
// cases, and the unexpected-stop classifier never sees the row either (it returns
// false unless stopReason === "stop", unexpected-stop-classifier.ts:35-36 — a
// toolUse tail is outside its whole domain). The shape is reachable, not
// theoretical: the Cursor exec channel stamps kCursorExecResolved on the toolCall
// blocks it already executed and agent-loop.ts:1276-1279 filters those out of
// `toolCalls`, so hasMoreToolCalls goes false while the message still carries text
// and the run ends with a terminal agent_end. A final toolUse message with real
// text is therefore a run omp itself considers finished, and this parser fails it.
//
// That is a ruled tradeoff, not an oversight (#1428 delta review). A false RED
// costs one retry of a job; a false GREEN costs a wrong merge verdict — and the two
// cannot be told apart from the wire, because "Let me read the file first." and a
// finished review's verdict are the same bytes to this parser: narration and an
// answer are indistinguishable once the stopReason says the loop was still calling
// tools. Nothing is destroyed by the choice: Deliver returns the whole stream as the
// job's Raw output on every failure path, so the discarded text survives as evidence
// and an operator can read it by hand.
//
// An EMPTY stopReason is deliberately NOT truncation. The field is required on
// omp's AssistantMessage, so an empty one means envelope drift, and drift must not
// fail every run: the terminal-text rule still has to be satisfied, which is the
// check that matters for an answer. This mirrors the absent-isTerminal handling.
func ompStopReasonIsTruncation(stopReason string) bool {
	switch stopReason {
	case "length", "toolUse":
		return true
	}
	return false
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
