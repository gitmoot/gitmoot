package runtime

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jerryfane/gitmoot/internal/subprocess"
)

const (
	CodexRuntime   = "codex"
	ClaudeRuntime  = "claude"
	KimiRuntime    = "kimi"
	KimiCLIRuntime = "kimi-cli"
	ShellRuntime   = "shell"
	LastRef        = "last"

	healthPrompt = "Gitmoot health check. Reply OK only."

	AutonomyPolicyAuto             = "auto"
	AutonomyPolicyReadOnly         = "read-only"
	AutonomyPolicyWorkspaceWrite   = "workspace-write"
	AutonomyPolicyDangerFullAccess = "danger-full-access"
)

type Agent struct {
	Name           string
	Role           string
	Runtime        string
	RuntimeRef     string
	RepoScope      string
	TemplateID     string
	Capabilities   []string
	AutonomyPolicy string
	HealthStatus   string
	Model          string
	Effort         string
	// PresetDelivery is the agent's prompt preset delivery mode (#33): full
	// (default), referenced, or auto. Carried in-memory from the stored agents row
	// so the delivery seam can decide whether to inline the whole preset or send a
	// short reference. Empty (the default and every construction site that does not
	// set it) is treated as full, so delivery is byte-identical.
	PresetDelivery string
	// SingleUseSession marks an agent whose runtime session exists solely for
	// the current job (ephemeral delegation workers and per-job temp workers:
	// the session is started for the job and disposed after it). Adapters whose
	// CLIs report SESSION-CUMULATIVE usage on resumed sessions (codex, see
	// codexDeliverResult) may attribute that cumulative usage to the job when
	// this is set — the whole session belongs to the job, so cumulative IS the
	// per-job cost. Never set for long-lived registered agents, whose sessions
	// span many jobs. In-memory only; not persisted on the agents table.
	SingleUseSession bool
	// ChatSeat marks a moot/chat SEAT job (#732): its whole purpose is to converse
	// via `gitmoot chat send/wait`, which under the default read-only codex sandbox
	// cannot even reach the daemon relay unix socket (the read-only seccomp filter
	// blocks the connect() syscall itself, regardless of the socket's path). A seat
	// is therefore dispatched with codex workspace-write + network_access=true so it
	// can reach the socket; the gitmoot home stays OUTSIDE workspace-write's
	// writable roots ([workdir, /tmp, $TMPDIR]), so the read-only-home invariant is
	// preserved — the relay, not the seat, performs the DB write. The daemon sets
	// this in-memory for a `gitmoot moot` seat (payload.MootSeat) — and ONLY when it
	// also injects a working relay env, so a seat is never elevated without a relay;
	// never persisted.
	ChatSeat bool
	// WorkingDir is the resolved filesystem checkout directory the runtime
	// adapter should chdir into for a delivery. It is DISTINCT from RepoScope,
	// which stays in "owner/repo" form (and is validated as such). Callers that
	// need a real subprocess to run inside a repo checkout (e.g. the SkillOpt
	// synth loop) resolve the repo's registered db.Repo.CheckoutPath and set it
	// here; the delivery seam prefers this over RepoScope when picking the
	// adapter Dir. In-memory only; not persisted on the agents table and never
	// marshaled to the wire. Empty means "fall back to RepoScope" (unchanged
	// legacy behavior for callers that never set it).
	WorkingDir string
}

type Job struct {
	ID          string
	AgentName   string
	Action      string
	Prompt      string
	Repository  string
	PullRequest int
	Model       string
	Effort      string
	// RuntimeDefaultModel is the runtime's configured registry default_model
	// (#652), threaded in by the dispatch layer from the HOME-AWARE resolved runtime
	// registry (built-in defaults overlaid with [runtimes.<name>] config). It is the
	// FINAL model fallback: effectiveModel uses it ONLY when neither the job (Model)
	// nor the agent (Agent.Model) pins a model, so an agent/job --model always wins.
	// Empty — the built-in default for every runtime, and the value when no config
	// sets it — means "no registry default", so delivery defers to the runtime CLI's
	// own default exactly as before #652 (byte-identical).
	RuntimeDefaultModel string
	// RuntimeDefaultEffort is the runtime's configured registry default_effort,
	// threaded in by the dispatch layer from the HOME-AWARE resolved runtime
	// registry. It is the FINAL effort fallback: effectiveEffort uses it ONLY when
	// neither the job (Effort) nor the agent (Agent.Effort) pins an effort, so an
	// agent/job --effort always wins. Empty means no -c argument is emitted.
	RuntimeDefaultEffort string
}

type Result struct {
	Decision string
	Summary  string
	Raw      string
	// InputTokens and OutputTokens are best-effort runtime token usage captured
	// from the CLI's structured output where it exposes one (#338 Part B). They
	// default to 0. Per-runtime status today: claude reports usage via its
	// --output-format json envelope; kimi-code 0.19.2 emits no usage event so it
	// contributes 0; codex Deliver reads the last turn.completed usage from its
	// `exec --json` JSONL output (#658), falling back to 0 on an older CLI that
	// predates --json. For codex these counts are SESSION-CUMULATIVE on a resumed
	// thread — the session's whole running total, not this job's usage — so the
	// caller must delta them (see CumulativeUsage and codexDeliverResult); fresh
	// and single-use sessions already report a per-job count. A 0 here means "not
	// captured", and the per-root delegation token budget simply under-counts that
	// job rather than failing.
	InputTokens  int
	OutputTokens int
	// CumulativeUsage reports that InputTokens/OutputTokens are runtime-session
	// cumulative — the session's whole running total, not this job's usage (codex
	// resumed threads report turn.completed usage this way, #661). When true the
	// caller must subtract the last-seen counters for the runtime session (keyed by
	// runtime+ref) and record only the delta. False means the counts are already
	// per-job — fresh sessions and single-use ephemeral/temp workers, where the
	// session's cumulative == this job's usage — and are recorded verbatim (#664).
	// Only codex sets this today; other adapters leave it false.
	CumulativeUsage bool
	// RefreshedRuntimeRef is non-empty when the adapter delivered on a runtime
	// reference other than the agent's stored one: either a #443 dead-session
	// self-heal that re-pinned the agent to a fresh reference, or a by-design
	// per-job session minted for an isolated delivery (the #531 fresh-ref path and
	// the last+template coordinator path). The mailbox adopts it in-memory so a
	// same-job repair delivery resumes the same session; whether it is also
	// PERSISTED onto the agent's stored ref is gated by SessionEphemeral. Other
	// adapters and the happy path leave it empty.
	RefreshedRuntimeRef string
	// SessionEphemeral reports that RefreshedRuntimeRef names a per-job session the
	// adapter minted BY DESIGN — the isolated Claude sessions from deliverFresh (the
	// #531 fresh-ref path and the last+template coordinator path) — and NOT a #443
	// dead-session self-heal re-pin. The mailbox must adopt an ephemeral ref
	// in-memory (so same-job repair stays coherent) but must NEVER persist it onto
	// the agent's stored runtime_ref: persisting it would rewrite a "last"/fresh
	// registration to the minted UUID and end the per-job isolation after job 1. The
	// self-heal path (a genuinely dead pinned session, replaced permanently) leaves
	// this false so its re-pin IS persisted.
	SessionEphemeral bool
	// SessionDiag carries process-level diagnostics for the runtime CLI run
	// backing this delivery (#806). Adapters populate it best-effort on every
	// Deliver return that actually ran a CLI process — success and failure alike —
	// so the engine can persist bounded, redacted crash context when a job ends
	// WITHOUT producing a gitmoot_result envelope. nil means no CLI process ran
	// (e.g. a validation error), so there is nothing to diagnose.
	SessionDiag *SessionDiag
}

// SessionDiag is the raw process-level evidence of how a runtime CLI run ended
// (#806). It is UNREDACTED and UNBOUNDED here — the workflow engine redacts and
// bounds the stderr before anything is persisted or surfaced.
type SessionDiag struct {
	// Stderr is the CLI process's captured stderr, kept separate from Result.Raw
	// (which merges stdout+stderr on failure paths).
	Stderr string
	// StdoutSeen reports whether the CLI produced any stdout before it ended —
	// the engine uses it to distinguish a crash before output (launched) from a
	// crash mid-output (streaming).
	StdoutSeen bool
	// ExitCode is the process exit status when known; nil when the process was
	// terminated by a signal or the run error was not a process exit at all
	// (spawn failure, context cancellation).
	ExitCode *int
	// Signal is the signal name when the process was terminated by a signal.
	Signal string
	// SessionID is the concrete runtime session id in play when one was
	// created/known; empty for runtimes without session ids (shell, kimi's
	// unreported per-job sessions) and for non-concrete refs ("last", fresh:*).
	SessionID string
}

// newSessionDiag builds the SessionDiag for one runner invocation: the captured
// stderr, whether any stdout was produced, and the exit code/signal decoded from
// the runner error (nil error means the process exited 0).
func newSessionDiag(res subprocess.Result, runErr error, sessionID string) *SessionDiag {
	code, signal := decodeProcessExit(runErr)
	return &SessionDiag{
		Stderr:     res.Stderr,
		StdoutSeen: strings.TrimSpace(res.Stdout) != "",
		ExitCode:   code,
		Signal:     signal,
		SessionID:  strings.TrimSpace(sessionID),
	}
}

// decodeProcessExit extracts how a runner subprocess ended from its run error:
// (code, "") for a plain exit — a nil error means exit 0 — (nil, signal) when
// the process was terminated by a signal, and (nil, "") when the error is not a
// process exit at all (spawn failure, context cancellation).
func decodeProcessExit(err error) (*int, string) {
	if err == nil {
		code := 0
		return &code, ""
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return nil, ""
	}
	if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return nil, status.Signal().String()
	}
	if code := exitErr.ExitCode(); code >= 0 {
		return &code, ""
	}
	return nil, ""
}

// concreteSessionRef returns the agent's runtime ref when it names a concrete
// resumable session, and "" for the non-concrete forms ("last", fresh:*, empty)
// that identify no specific session.
func concreteSessionRef(agent Agent) string {
	ref := strings.TrimSpace(agent.RuntimeRef)
	if ref == "" || ref == LastRef || IsFreshRef(ref) {
		return ""
	}
	return ref
}

type StartRequest struct {
	Agent  Agent
	Prompt string
}

type StartResult struct {
	RuntimeRef string
	Raw        string
}

type Adapter interface {
	Name() string
	Start(ctx context.Context, request StartRequest) (StartResult, error)
	Validate(ctx context.Context, agent Agent) error
	Deliver(ctx context.Context, agent Agent, job Job) (Result, error)
	Health(ctx context.Context, agent Agent) error
	Capabilities(ctx context.Context) ([]string, error)
}

type Factory struct {
	Runner subprocess.Runner
}

func (f Factory) Adapter(name string) (Adapter, error) {
	switch name {
	case CodexRuntime:
		return CodexAdapter{Runner: f.Runner}, nil
	case ClaudeRuntime:
		return ClaudeAdapter{Runner: f.Runner}, nil
	case KimiRuntime:
		return KimiAdapter{Runner: f.Runner}, nil
	case KimiCLIRuntime:
		return KimiCLIAdapter{Runner: f.Runner}, nil
	case ShellRuntime:
		return ShellAdapter{Runner: f.Runner}, nil
	default:
		return nil, fmt.Errorf("unsupported runtime: %s (supported: %s)", name, strings.Join(SupportedRuntimes(), ", "))
	}
}

// SupportedRuntimes enumerates every runtime name the adapter Factory can
// construct — the single source of truth callers (e.g. the per-job --runtime
// override validation) use instead of hard-coding runtime names. It is derived
// from the built-in runtime metadata registry's dispatchable entries, so the
// enumeration, the registry, and Factory.Adapter share one source of truth; keep
// it in lockstep with Factory.Adapter (TestSupportedRuntimesMatchFactory proves
// the coupling both ways).
func SupportedRuntimes() []string {
	return builtinRegistry.dispatchableNames()
}

// FreshRefPrefix marks a runtime reference that names no existing session:
// "fresh:<suffix>". A delivery against a fresh ref must start a brand-new
// session on its runtime and must never resume (or write back to) any stored
// session. The suffix scopes runtime-session lock keys; per-job override refs
// are unique when minted, and registered fresh refs are rewritten to a job
// scoped ref before execution.
const FreshRefPrefix = "fresh:"

// NewFreshRef mints a unique fresh-session runtime reference (see
// FreshRefPrefix).
func NewFreshRef() (string, error) {
	id, err := newUUID()
	if err != nil {
		return "", err
	}
	return FreshRefPrefix + id, nil
}

// IsFreshRef reports whether ref is a fresh-session reference.
func IsFreshRef(ref string) bool {
	return strings.HasPrefix(ref, FreshRefPrefix)
}

// FreshRefForJob returns a deterministic fresh-session ref scoped to a job id.
// This lets a registered "fresh:<seat>" agent get a separate runtime lock and
// CLI session per job instead of sharing one lock across all jobs.
func FreshRefForJob(jobID string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(jobID)))
	return FreshRefPrefix + "job:" + hex.EncodeToString(sum[:8])
}

func ValidateAgent(agent Agent) error {
	if err := validateAgentFields(agent, true); err != nil {
		return err
	}
	if agent.Runtime == ClaudeRuntime && agent.RuntimeRef != LastRef && !isUUID(agent.RuntimeRef) && !IsFreshRef(agent.RuntimeRef) {
		return fmt.Errorf("claude runtime reference %q must be a UUID, last, or fresh:<suffix>", agent.RuntimeRef)
	}
	if isKimiRuntime(agent.Runtime) && agent.RuntimeRef != "" && !isKimiSessionID(agent.RuntimeRef) && !IsFreshRef(agent.RuntimeRef) {
		return fmt.Errorf("kimi runtime reference %q must be a Kimi session id, fresh:<suffix>, or empty", agent.RuntimeRef)
	}
	return nil
}

func ValidateStartRequest(request StartRequest) error {
	adapter, err := (Factory{}).Adapter(request.Agent.Runtime)
	if err != nil {
		return err
	}
	return validateStartRequest(request.Agent, adapter.Name(), request.Prompt)
}

func validateStartRequest(agent Agent, runtimeName string, prompt string) error {
	if err := validateAgentFields(agent, false); err != nil {
		return err
	}
	if agent.Runtime != runtimeName {
		return fmt.Errorf("agent runtime %q does not match adapter %q", agent.Runtime, runtimeName)
	}
	if strings.TrimSpace(prompt) == "" {
		return errors.New("start prompt is required")
	}
	return nil
}

func validateAgentFields(agent Agent, requireRuntimeRef bool) error {
	if _, err := NormalizeAutonomyPolicy(agent.AutonomyPolicy); err != nil {
		return err
	}
	switch {
	case strings.TrimSpace(agent.Name) == "":
		return errors.New("agent name is required")
	case strings.ContainsAny(agent.Name, " \t\n/"):
		return fmt.Errorf("agent name %q cannot contain whitespace or slash", agent.Name)
	case strings.TrimSpace(agent.Role) == "":
		return errors.New("agent role is required")
	case strings.TrimSpace(agent.Runtime) == "":
		return errors.New("agent runtime is required")
	case requireRuntimeRef && strings.TrimSpace(agent.RuntimeRef) == "":
		return errors.New("agent runtime reference is required")
	case strings.TrimSpace(agent.RepoScope) != "" && !validRepoScope(agent.RepoScope):
		return fmt.Errorf("agent repo scope %q must be owner/repo", agent.RepoScope)
	}
	if _, err := (Factory{}).Adapter(agent.Runtime); err != nil {
		return err
	}
	return nil
}

func NormalizeAutonomyPolicy(policy string) (string, error) {
	switch strings.TrimSpace(policy) {
	case "", AutonomyPolicyAuto:
		return AutonomyPolicyAuto, nil
	case AutonomyPolicyReadOnly:
		return AutonomyPolicyReadOnly, nil
	case AutonomyPolicyWorkspaceWrite:
		return AutonomyPolicyWorkspaceWrite, nil
	case AutonomyPolicyDangerFullAccess:
		return AutonomyPolicyDangerFullAccess, nil
	default:
		return "", fmt.Errorf("autonomy policy %q is not supported; use auto, read-only, workspace-write, or danger-full-access", strings.TrimSpace(policy))
	}
}

func NormalizeStoredAutonomyPolicy(policy string) string {
	normalized, err := NormalizeAutonomyPolicy(policy)
	if err != nil {
		return AutonomyPolicyAuto
	}
	return normalized
}

// ImplementWritePolicyGuidance is the single, actionable message emitted whenever
// an agent (or ephemeral spec) carrying the "implement" capability is paired with
// a non-write autonomy policy (auto/empty or read-only). It is shared verbatim by
// every fail-closed seam — agent start, agent subscribe, implement-job dispatch,
// and ephemeral-spec validation — so operators always see the same fix.
//
// The guidance names both writable policies and is explicit that workspace-write
// (acceptEdits) only auto-accepts file edits — it does NOT unblock the Bash an
// implement job needs (go/git/gh), so full headless implementation requires
// danger-full-access (bypassPermissions). The behavior is fail-closed: gitmoot
// refuses rather than silently delegating the write decision to ambient Claude
// config, whose headless write capability is non-deterministic across hosts.
const ImplementWritePolicyGuidance = "autonomy policy %q grants no write permission in headless runs, but this worker has the \"implement\" capability — the job would run and produce no files. Set --policy danger-full-access for full headless implementation (file writes plus go/git/gh via Bash), or --policy workspace-write for edits-only (note: workspace-write maps to acceptEdits and does NOT unblock Bash, so go/git/gh stay blocked)."

// PolicyGrantsImplementWrite reports whether the given autonomy policy permits
// headless file writes. Only workspace-write and danger-full-access qualify; auto
// (and the empty/unset policy, which normalizes to auto) and read-only do not.
func PolicyGrantsImplementWrite(policy string) bool {
	switch NormalizeStoredAutonomyPolicy(policy) {
	case AutonomyPolicyWorkspaceWrite, AutonomyPolicyDangerFullAccess:
		return true
	default:
		return false
	}
}

// HasImplementCapability reports whether the capability set contains "implement".
func HasImplementCapability(capabilities []string) bool {
	for _, capability := range capabilities {
		if strings.TrimSpace(capability) == "implement" {
			return true
		}
	}
	return false
}

// ImplementWritePolicyError returns a non-nil, actionable error when the given
// capability set contains "implement" but the autonomy policy grants no write
// (auto/empty or read-only); otherwise it returns nil. This is the shared
// fail-closed predicate behind every capability<->policy seam. read-only / ask /
// review agents (no implement capability) are never refused.
func ImplementWritePolicyError(capabilities []string, policy string) error {
	if !HasImplementCapability(capabilities) {
		return nil
	}
	if PolicyGrantsImplementWrite(policy) {
		return nil
	}
	return fmt.Errorf(ImplementWritePolicyGuidance, NormalizeStoredAutonomyPolicy(policy))
}

type CodexSessionResolver interface {
	Exists(ctx context.Context, ref string) (bool, error)
}

type CodexSessionIndex struct {
	Path string
	Home string
}

type CodexAdapter struct {
	Runner          subprocess.Runner
	Dir             string
	SessionResolver CodexSessionResolver
}

func (a CodexAdapter) Name() string { return CodexRuntime }

func (a CodexAdapter) Start(ctx context.Context, request StartRequest) (StartResult, error) {
	if err := validateStartRequest(request.Agent, a.Name(), request.Prompt); err != nil {
		return StartResult{}, err
	}
	args := append([]string{"exec"}, codexSandboxArgs(request.Agent, a.Dir)...)
	if request.Agent.Model != "" {
		args = append(args, "--model", request.Agent.Model)
	}
	if request.Agent.Effort != "" {
		args = append(args, "-c", "model_reasoning_effort="+request.Agent.Effort)
	}
	args = append(args, "--json", "--", request.Prompt)
	result, err := a.runner().Run(ctx, a.Dir, "codex", args...)
	if err != nil {
		return StartResult{Raw: result.Stdout + result.Stderr}, codexCommandError(result, err)
	}
	threadID, err := parseCodexStartedThreadID(result.Stdout)
	if err != nil {
		return StartResult{Raw: result.Stdout}, err
	}
	// Unwrap the `codex exec --json` JSONL stream so forked-session consumers
	// (skillopt synth/ab) that parse Raw as the assistant's answer see the
	// agent_message text, not the whole transcript (banner, thread.started,
	// turn events) — the codex flavor of #722. Reuses the same parser the
	// Deliver path uses to build Summary. Fail-open: fall back to the raw stdout
	// when the stream carries no agent_message (older CLI, plain-text fallback,
	// or an unexpected shape), so Raw always surfaces the underlying text and
	// unwrap never errors. RuntimeRef is untouched.
	raw := result.Stdout
	if msg, _, _, ok := parseCodexJSONResult(result.Stdout); ok {
		raw = msg
	}
	return StartResult{RuntimeRef: threadID, Raw: raw}, nil
}

func (a CodexAdapter) Validate(_ context.Context, agent Agent) error {
	return validateRuntime(agent, a.Name())
}

func (a CodexAdapter) Deliver(ctx context.Context, agent Agent, job Job) (Result, error) {
	if err := a.Validate(ctx, agent); err != nil {
		return Result{}, err
	}
	model := effectiveModel(agent, job)
	effort := effectiveEffort(agent, job)
	// A fresh ref (per-job --runtime override, #531) starts a brand-new exec
	// session — never `resume` — so an overridden job cannot read or pollute any
	// stored session's state. Every other ref resumes an existing session, which
	// we first verify still exists.
	if !IsFreshRef(agent.RuntimeRef) {
		if err := a.verifySession(ctx, agent); err != nil {
			return Result{}, err
		}
	}
	result, err := a.runCodex(ctx, agent, job.Prompt, model, effort)
	// The session id in play: the pinned concrete thread on a resume, or the
	// thread id codex printed for a fresh/`last` run (best-effort — the stream
	// may not carry one when the run died early).
	sessionID := concreteSessionRef(agent)
	if sessionID == "" {
		sessionID = parseCodexThreadID(result.Stdout)
	}
	if err != nil {
		return Result{Raw: result.Stdout + result.Stderr, SessionDiag: newSessionDiag(result, err, sessionID)}, codexCommandError(result, err)
	}
	// Usage is attributable to this job when the session belongs to it alone:
	// a fresh ref (brand-new session, no resume) or a single-use session
	// (ephemeral/temp workers — started for the job, disposed after it).
	freshSession := IsFreshRef(agent.RuntimeRef) || agent.SingleUseSession
	parsed := codexDeliverResult(result.Stdout, freshSession)
	if IsFreshRef(agent.RuntimeRef) {
		parsed.RefreshedRuntimeRef = parseCodexThreadID(result.Stdout)
	}
	parsed.SessionDiag = newSessionDiag(result, nil, sessionID)
	return parsed, nil
}

// codexDeliverArgs builds the `codex exec` argument vector for a Deliver call.
// dir is the adapter's working directory (the job checkout); when it is a linked
// git worktree and the sandbox is workspace-write, codexSandboxArgs grants the
// worktree's resolved gitdir as an extra writable root (#805).
// jsonOutput adds --json so codex streams JSONL events (thread/turn/item) on
// stdout, which is what lets a delivery capture token usage (#658). --json is an
// `exec` option that codex accepts before the `resume` subcommand, which is where
// we place it. jsonOutput=false is the older-CLI fallback (see runCodex): it
// reproduces the pre-#658 plain-text command exactly. A fresh ref (#531) always
// starts a brand-new exec session and never resumes.
//
// ARG_MAX exposure (#723): like kimi, codex passes the prompt as a trailing argv
// arg, so a prompt above the kernel's MAX_ARG_STRLEN (~128 KiB per single arg)
// would fork/exec with E2BIG. codex, unlike kimi, has a native escape hatch —
// `codex exec` reads the prompt from stdin when the positional is `-` or omitted
// ("instructions are read from stdin"). Routing oversize prompts through stdin is
// deferred to a follow-up; #723 fixes kimi, which offers no stdin/file channel.
func codexDeliverArgs(agent Agent, dir, prompt, model string, effort string, jsonOutput bool) []string {
	args := append([]string{"exec"}, codexSandboxArgs(agent, dir)...)
	if jsonOutput {
		args = append(args, "--json")
	}
	if IsFreshRef(agent.RuntimeRef) {
		if model != "" {
			args = append(args, "--model", model)
		}
		if effort != "" {
			args = append(args, "-c", "model_reasoning_effort="+effort)
		}
		return append(args, "--", prompt)
	}
	args = append(args, "resume")
	if model != "" {
		args = append(args, "--model", model)
	}
	if effort != "" {
		args = append(args, "-c", "model_reasoning_effort="+effort)
	}
	if agent.RuntimeRef == "last" {
		args = append(args, "--last")
	} else {
		args = append(args, agent.RuntimeRef)
	}
	return append(args, "--", prompt)
}

// runCodex performs a single codex Deliver run with --json (for token capture),
// re-running once WITHOUT --json when an older codex CLI rejects the flag
// (mirrors runClaude's --output-format fallback). The plain-text re-run keeps the
// pre-#658 semantics; codexDeliverResult then fails open on the non-JSONL stdout
// and contributes 0 usage. Only the JSON re-run is retried — a genuine delivery
// failure surfaces unchanged.
func (a CodexAdapter) runCodex(ctx context.Context, agent Agent, prompt, model string, effort string) (subprocess.Result, error) {
	result, err := a.runner().Run(ctx, a.Dir, "codex", codexDeliverArgs(agent, a.Dir, prompt, model, effort, true)...)
	if err != nil && isCodexJSONUnsupported(result) {
		result, err = a.runner().Run(ctx, a.Dir, "codex", codexDeliverArgs(agent, a.Dir, prompt, model, effort, false)...)
	}
	return result, err
}

// codexDeliverResult turns codex `exec --json` stdout into a Deliver Result: the
// joined agent_message text becomes Raw — the exact text the engine scans for the
// gitmoot_result blob, just as the plain-text stdout was before #658 — plus the
// last turn.completed usage as best-effort token counts. It fails open: stdout
// carrying no agent_message event (older CLI, the plain-text --json fallback, or
// an unexpected shape) is returned verbatim as Raw with 0 usage, so a delivery is
// never lost because usage parsing changed.
//
// codex's turn.completed usage is SESSION-CUMULATIVE on a resumed thread, not
// per-turn: probed live on codex-cli 0.142.4, three one-word turns on one thread
// reported input 16504 -> 85681 -> 103779 and output 20 -> 40 -> 45 (monotonically
// accumulating), and a single resumed ask on a long-lived agent reported 22.4M
// input tokens — orders of magnitude beyond any single turn. So the parsed counts
// are always returned, with CumulativeUsage set to !freshSession: a resumed
// (shared) session flags them cumulative so the caller records only the
// per-session delta (#661), while a fresh ref or single-use session — its
// cumulative == this job's cost, the #338 budget's primary target — flags them
// per-job so they are recorded verbatim (#664). The fail-open branch (no
// agent_message event) returns 0 usage with CumulativeUsage=false, unchanged.
func codexDeliverResult(stdout string, freshSession bool) Result {
	raw, inputTokens, outputTokens, ok := parseCodexJSONResult(stdout)
	if !ok {
		return Result{Raw: stdout, Summary: strings.TrimSpace(stdout)}
	}
	return Result{
		Raw:             raw,
		Summary:         strings.TrimSpace(raw),
		InputTokens:     inputTokens,
		OutputTokens:    outputTokens,
		CumulativeUsage: !freshSession,
	}
}

func parseCodexThreadID(output string) string {
	scanner := bufio.NewScanner(strings.NewReader(output))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event struct {
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
		}
		if json.Unmarshal([]byte(line), &event) != nil {
			continue
		}
		if event.Type == "thread.started" && strings.TrimSpace(event.ThreadID) != "" {
			return strings.TrimSpace(event.ThreadID)
		}
	}
	return ""
}

func (a CodexAdapter) Health(ctx context.Context, agent Agent) error {
	_, err := a.Deliver(ctx, agent, Job{Prompt: healthPrompt})
	return err
}

func (a CodexAdapter) Capabilities(context.Context) ([]string, error) {
	return []string{"review", "implement", "ask"}, nil
}

func (a CodexAdapter) runner() subprocess.Runner {
	if a.Runner != nil {
		return a.Runner
	}
	// Group semantics so cancelling a job kills the runtime CLI's whole
	// process tree, not just the immediate child.
	return subprocess.GroupRunner{}
}

func (a CodexAdapter) verifySession(ctx context.Context, agent Agent) error {
	if agent.RuntimeRef == "last" {
		return nil
	}
	exists, err := a.sessionResolver().Exists(ctx, agent.RuntimeRef)
	if err != nil {
		return fmt.Errorf("verify codex session: %w", err)
	}
	if !exists {
		if isUUID(agent.RuntimeRef) {
			return nil
		}
		return fmt.Errorf("codex session %q was not found", agent.RuntimeRef)
	}
	return nil
}

func (a CodexAdapter) sessionResolver() CodexSessionResolver {
	if a.SessionResolver != nil {
		return a.SessionResolver
	}
	return CodexSessionIndex{}
}

// effectiveModel resolves which model a delivered job runs on. Precedence, most
// specific first: the per-job/per-delegation override (job.Model) wins, then the
// agent's configured default (agent.Model), then — when NEITHER pins a model — the
// runtime's configured registry default_model (job.RuntimeDefaultModel, #652). An
// empty result means "no --model arg" (the runtime CLI's own default). Because the
// registry default is consulted last and defaults to empty, an agent/job pin
// always wins and an unset registry default is byte-identical to before #652.
func effectiveModel(agent Agent, job Job) string {
	if job.Model != "" {
		return job.Model
	}
	if agent.Model != "" {
		return agent.Model
	}
	return strings.TrimSpace(job.RuntimeDefaultModel)
}

// effectiveEffort resolves which reasoning effort a delivered job runs with.
// Precedence mirrors effectiveModel exactly: the per-job override wins, then the
// agent default, then the runtime registry default_effort. An empty result means
// no `-c model_reasoning_effort=...` argument is emitted.
func effectiveEffort(agent Agent, job Job) string {
	if job.Effort != "" {
		return job.Effort
	}
	if agent.Effort != "" {
		return agent.Effort
	}
	return strings.TrimSpace(job.RuntimeDefaultEffort)
}

func codexSandboxArgs(agent Agent, workdir string) []string {
	// #732: a moot/chat seat must reach the daemon chat-relay unix socket, but a
	// codex read-only sandbox blocks the connect() syscall itself (empirically
	// probed on codex-cli 0.142.4 bubblewrap+seccomp). Dispatch the seat with
	// workspace-write + network so it can connect; the gitmoot home is NOT in
	// workspace-write's writable roots (workdir + /tmp + $TMPDIR), so the home stays
	// read-only — the relay, not the seat, does the write. This is self-contained
	// (does not depend on the operator's global ~/.codex/config.toml). A seat's
	// whole job is chat, not git, so it takes no #805 gitdir grant.
	if agent.ChatSeat {
		return []string{"--sandbox", "workspace-write", "-c", "sandbox_workspace_write.network_access=true"}
	}
	switch NormalizeStoredAutonomyPolicy(agent.AutonomyPolicy) {
	case AutonomyPolicyReadOnly:
		return []string{"--sandbox", "read-only"}
	case AutonomyPolicyWorkspaceWrite:
		args := []string{"--sandbox", "workspace-write"}
		// #805: a linked-worktree checkout keeps its real git metadata under the
		// main repo's .git/worktrees/<name>, OUTSIDE workspace-write's writable
		// roots, so every metadata-writing git operation (status refresh, add,
		// commit) fails on the index lock inside the sandbox. Grant exactly that
		// resolved gitdir via --add-dir — additive, mirroring how `plugin
		// codex-launch` grants the gitmoot home, and unlike a
		// `-c sandbox_workspace_write.writable_roots=[...]` override it cannot
		// clobber roots the operator's config.toml already grants. A primary
		// checkout (or empty/unknown workdir) resolves to "" and the argv stays
		// byte-identical.
		if gitdir := linkedWorktreeGitDir(workdir); gitdir != "" {
			args = append(args, "--add-dir", gitdir)
		}
		return args
	case AutonomyPolicyDangerFullAccess:
		return []string{"--sandbox", "danger-full-access"}
	default:
		return nil
	}
}

// linkedWorktreeGitDir resolves the per-worktree git metadata directory of a
// linked `git worktree` checkout (#805). In a linked worktree, <dir>/.git is a
// FILE whose first line is "gitdir: <main-repo>/.git/worktrees/<name>"; that
// resolved directory holds the worktree's index/HEAD and lives outside the
// checkout. It returns "" — the fail-open "not applicable" answer — for a
// primary checkout (.git is a directory), a missing or empty dir, or a
// malformed .git file. A relative gitdir (git tolerates one) is resolved
// against dir so the sandbox grant is always an absolute root.
func linkedWorktreeGitDir(dir string) string {
	if strings.TrimSpace(dir) == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(dir, ".git"))
	if err != nil {
		// A primary checkout's .git is a directory (EISDIR) and a non-repo has
		// none (ENOENT); both mean there is no linked gitdir to grant.
		return ""
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(data)), "\n")
	rest, ok := strings.CutPrefix(strings.TrimSpace(line), "gitdir:")
	if !ok {
		return ""
	}
	gitdir := strings.TrimSpace(rest)
	if gitdir == "" {
		return ""
	}
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(dir, gitdir)
	}
	return filepath.Clean(gitdir)
}

// defaultClaudeRetryBackoff is the base pause between Claude delivery attempts
// when a transient socket-closed failure is retried. ClaudeAdapter.RetryBackoff
// overrides it; tests inject a near-zero value to stay fast.
const defaultClaudeRetryBackoff = 500 * time.Millisecond

// claudeDeliveryMaxAttempts bounds how many times a single Claude delivery is
// attempted when it keeps hitting the transient socket-closed 401 (#509). A
// fixed single retry (the old maxAttempts=2) empirically does not clear it, yet
// a fresh delivery minutes later does, so we retry more and spread the attempts
// across different transient windows via exponential backoff. Each failing
// attempt burns ~30-77s, so 4 attempts is ~few min worst case — comfortably
// inside the managed ask job-timeout (~10m). Only transient errors are retried.
const claudeDeliveryMaxAttempts = 4

// maxClaudeRetryBackoff caps the exponential per-attempt pause so the backoff
// never balloons past a sane ceiling on the later attempts.
const maxClaudeRetryBackoff = 30 * time.Second

type ClaudeAdapter struct {
	Runner        subprocess.Runner
	Dir           string
	NewRuntimeRef func() (string, error)
	// RetryBackoff is the pause before retrying a transient socket-closed
	// delivery failure. When zero it defaults to defaultClaudeRetryBackoff.
	RetryBackoff time.Duration
}

func (a ClaudeAdapter) Name() string { return ClaudeRuntime }

func (a ClaudeAdapter) Start(ctx context.Context, request StartRequest) (StartResult, error) {
	if err := validateStartRequest(request.Agent, a.Name(), request.Prompt); err != nil {
		return StartResult{}, err
	}
	runtimeRef, err := a.newRuntimeRef()
	if err != nil {
		return StartResult{}, err
	}
	args := claudePermissionArgs(request.Agent)
	if request.Agent.Model != "" {
		args = append(args, "--model", request.Agent.Model)
	}
	args = append(args, "--session-id", runtimeRef, "-p", "--output-format", "json", "--", request.Prompt)
	result, err := a.runner().Run(ctx, a.Dir, "claude", args...)
	if err != nil {
		return StartResult{Raw: result.Stdout + result.Stderr}, claudeCommandError(result, err)
	}
	// Unwrap the --output-format json result envelope so forked-session
	// consumers (skillopt synth/ab) that parse Raw as the assistant's answer see
	// the answer text, not the CLI envelope (#721). Reuses the same helper the
	// Deliver path uses to build Summary. Fail-open: fall back to the raw stdout
	// when stdout is not the envelope or carries no "result" field (e.g. an older
	// CLI's plain-text --output-format fallback), so Raw always surfaces the
	// underlying text and unwrap never errors.
	raw := result.Stdout
	if summary, _, _ := parseClaudeJSONResult(result.Stdout); summary != "" {
		raw = summary
	}
	return StartResult{RuntimeRef: runtimeRef, Raw: raw}, nil
}

func (a ClaudeAdapter) Validate(_ context.Context, agent Agent) error {
	return validateRuntime(agent, a.Name())
}

func (a ClaudeAdapter) Deliver(ctx context.Context, agent Agent, job Job) (Result, error) {
	if err := a.Validate(ctx, agent); err != nil {
		return Result{}, err
	}
	model := effectiveModel(agent, job)
	// A fresh ref (per-job --runtime override, #531) delivers on a brand-new
	// dedicated --session-id — never --resume/--continue — so an overridden job
	// cannot read or pollute any stored session's state.
	if IsFreshRef(agent.RuntimeRef) {
		return a.deliverFresh(ctx, agent, job, model)
	}
	// Template-backed Claude agents are coordinator-class jobs: they execute a
	// long-form prompt recipe and must never resume whichever interactive Claude
	// session happens to be "last". Treat a legacy --session last registration as
	// a per-job temp session instead of --continue.
	if agent.RuntimeRef == LastRef && strings.TrimSpace(agent.TemplateID) != "" {
		return a.deliverFresh(ctx, agent, job, model)
	}
	// The Claude CLI intermittently fails a delivery with a transient
	// "401 socket connection was closed unexpectedly" under sustained
	// concurrency; a byte-identical retry typically clears it. Retry on that
	// signature only, never on a permanent failure (e.g. an invalid token).
	// The retry is safe because the 401 fails before the model turn runs, so the
	// failed attempt mutates no partial session state. Use exponential backoff
	// (see waitRetryBackoff) so retries land in different transient windows.
	var result subprocess.Result
	var err error
	for attempt := 1; ; attempt++ {
		result, err = a.runClaude(ctx, agent, job, model)
		if attempt >= claudeDeliveryMaxAttempts || !isTransientClaudeDeliveryError(result, err) {
			break
		}
		if waitErr := a.waitRetryBackoff(ctx, attempt); waitErr != nil {
			break
		}
	}
	// Make ONE last-resort delivery on a fresh dedicated --session-id when a
	// pinned --resume delivery is still failing after the in-call retries, for
	// the two pre-execution failure classes a byte-identical --resume retry
	// cannot clear:
	//   - the pinned conversation no longer exists (#443), or
	//   - it keeps hitting the transient socket-closed 401 on EVERY attempt
	//     (#509). The in-call retries above already spread byte-identical
	//     --resume attempts across different transient windows; when they all
	//     fail together yet a fresh, later delivery succeeds, the wedged thing is
	//     the *current* resume attempt, not the prompt — so a one-off fresh
	//     session gets this delivery past the flake, mirroring why a separate
	//     later job clears it.
	//
	// The two classes differ in what they do AFTER that one delivery:
	//   - #443 (sessionMissing): the old session is genuinely GONE, so we thread
	//     the fresh ref out via Result.RefreshedRuntimeRef and mailbox.Run
	//     PERMANENTLY re-pins the agent to it. Nothing is lost — there was no
	//     conversation left to resume.
	//   - #509 (transient): the old session is still ALIVE (the issue's own
	//     repro shows the same `--resume <ref>` recovering on a later delivery),
	//     so we must NOT re-pin. Permanently switching a stateful pinned agent
	//     (e.g. the researcher/coordinator) to a brand-new empty session would
	//     silently discard all accumulated conversation context on a transport
	//     blip. We therefore leave refreshedRef empty: the fresh --session-id is
	//     used only to unstick THIS delivery, and the agent stays pinned to its
	//     original, context-bearing session so the next delivery resumes it.
	//
	// Bounded to a single pre-execution retry: never loops, never touches the
	// shared --continue ("last") path, is skipped once the context is cancelled
	// (so a killed job stops promptly instead of launching one more CLI), and
	// never masks an auth failure (a fresh start that fails for auth reasons
	// still surfaces as auth below).
	var refreshedRef string
	// The session id this delivery is actually running on, for crash
	// diagnostics (#806): the pinned concrete ref, replaced by the minted fresh
	// id when the self-heal below takes over the delivery.
	diagSessionRef := concreteSessionRef(agent)
	if err != nil && agent.RuntimeRef != LastRef && ctx.Err() == nil {
		sessionMissing := isClaudeSessionMissing(result)
		transient := isTransientClaudeDeliveryError(result, err)
		if sessionMissing || transient {
			newRef, refErr := a.newRuntimeRef()
			if refErr == nil {
				// Only re-pin permanently for the dead-session class (#443); the
				// transient class (#509) keeps its still-alive original session.
				if sessionMissing {
					refreshedRef = newRef
				}
				diagSessionRef = newRef
				result, err = a.runner().Run(ctx, a.Dir, "claude", claudeFreshSessionArgs(agent, job.Prompt, model, newRef)...)
				if err != nil {
					if sessionMissing {
						return Result{Raw: result.Stdout + result.Stderr, SessionDiag: newSessionDiag(result, err, diagSessionRef)}, a.claudeSessionMissingError(agent, result, err)
					}
					return Result{Raw: result.Stdout + result.Stderr, SessionDiag: newSessionDiag(result, err, diagSessionRef)}, claudeCommandError(result, err)
				}
			}
		}
	}
	if err != nil {
		return Result{Raw: result.Stdout + result.Stderr, SessionDiag: newSessionDiag(result, err, diagSessionRef)}, claudeCommandError(result, err)
	}
	parsed := Result{Raw: result.Stdout, RefreshedRuntimeRef: refreshedRef, SessionDiag: newSessionDiag(result, nil, diagSessionRef)}
	summary, inTok, outTok := parseClaudeJSONResult(result.Stdout)
	if summary != "" {
		parsed.Summary = summary
	} else {
		parsed.Summary = strings.TrimSpace(result.Stdout)
	}
	parsed.InputTokens = inTok
	parsed.OutputTokens = outTok
	return parsed, nil
}

// deliverFresh delivers one job on a brand-new dedicated Claude session (the
// fresh-ref path, #531). Each attempt mints a NEW session id, mirroring the
// #443 self-heal shape (claudeFreshSessionArgs — never --resume/--continue),
// so a transient-401 retry can never trip over an already-created session id.
// It returns the successful concrete session id as RefreshedRuntimeRef so
// malformed-output repair prompts in the same job resume that isolated job
// session. The mailbox guards registered fresh refs from being re-pinned.
func (a ClaudeAdapter) deliverFresh(ctx context.Context, agent Agent, job Job, model string) (Result, error) {
	var result subprocess.Result
	var err error
	var sessionID string
	for attempt := 1; ; attempt++ {
		sessionID, err = a.newRuntimeRef()
		if err != nil {
			return Result{}, err
		}
		result, err = a.runner().Run(ctx, a.Dir, "claude", claudeFreshSessionArgs(agent, job.Prompt, model, sessionID)...)
		if attempt >= claudeDeliveryMaxAttempts || !isTransientClaudeDeliveryError(result, err) {
			break
		}
		if waitErr := a.waitRetryBackoff(ctx, attempt); waitErr != nil {
			break
		}
	}
	if err != nil {
		return Result{Raw: result.Stdout + result.Stderr, SessionDiag: newSessionDiag(result, err, sessionID)}, claudeCommandError(result, err)
	}
	// SessionEphemeral marks this session as by-design per-job (fresh-ref #531 or
	// last+template coordinator): the mailbox adopts sessionID in-memory for
	// same-job repair but must NOT persist it onto the agent's stored ref, or the
	// isolation would end after job 1.
	parsed := Result{Raw: result.Stdout, RefreshedRuntimeRef: sessionID, SessionEphemeral: true, SessionDiag: newSessionDiag(result, nil, sessionID)}
	summary, inTok, outTok := parseClaudeJSONResult(result.Stdout)
	if summary != "" {
		parsed.Summary = summary
	} else {
		parsed.Summary = strings.TrimSpace(result.Stdout)
	}
	parsed.InputTokens = inTok
	parsed.OutputTokens = outTok
	return parsed, nil
}

// runClaude performs a single Claude delivery attempt, including the JSON
// output-format fallback re-run for older CLIs that reject --output-format.
func (a ClaudeAdapter) runClaude(ctx context.Context, agent Agent, job Job, model string) (subprocess.Result, error) {
	result, err := a.runner().Run(ctx, a.Dir, "claude", claudeArgs(agent, job.Prompt, true, model)...)
	if err != nil && isClaudeJSONUnsupported(result) {
		result, err = a.runner().Run(ctx, a.Dir, "claude", claudeArgs(agent, job.Prompt, false, model)...)
	}
	return result, err
}

// waitRetryBackoff pauses before the next delivery attempt (1-indexed), returning
// early if the context is cancelled so a killed job stops promptly. It uses a
// stopped timer (not time.After) so the timer is released when ctx wins the race.
func (a ClaudeAdapter) waitRetryBackoff(ctx context.Context, attempt int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	timer := time.NewTimer(a.retryBackoff(attempt))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// retryBackoff computes the exponential pause before the next delivery attempt:
// base * 2^(attempt-1), capped at maxClaudeRetryBackoff. base is RetryBackoff
// when set (tests inject a tiny value to stay fast) else defaultClaudeRetryBackoff.
func (a ClaudeAdapter) retryBackoff(attempt int) time.Duration {
	base := a.RetryBackoff
	if base <= 0 {
		base = defaultClaudeRetryBackoff
	}
	if attempt < 1 {
		attempt = 1
	}
	backoff := base
	for i := 1; i < attempt; i++ {
		backoff *= 2
		if backoff >= maxClaudeRetryBackoff {
			return maxClaudeRetryBackoff
		}
	}
	if backoff > maxClaudeRetryBackoff {
		return maxClaudeRetryBackoff
	}
	return backoff
}

// isTransientClaudeDeliveryError reports whether a failed Claude delivery is the
// known intermittent "401 socket connection was closed unexpectedly" transient,
// which a byte-identical retry typically clears. It returns false when the run
// succeeded (err == nil) and for permanent failures (e.g. an invalid token,
// which carries no socket-closed signature). The signature shows up in the JSON
// result on stdout in practice, so it matches both stdout and stderr; the
// err != nil gate keeps a successful run from ever being treated as transient.
//
// The match also requires a "401" status so the retry is confined to the
// provably-safe pre-turn handshake case the safety note in Deliver relies on: a
// 401 fails before the model turn runs, so the failed attempt mutates no partial
// session state and a byte-identical --resume retry cannot duplicate side
// effects. A bare "socket connection was closed unexpectedly" (the underlying
// Node/undici fetch error) can also be thrown on a MID-STREAM drop after the
// turn has already executed tool calls or file edits, where a retry would be a
// duplicate-execution hazard; such mid-stream drops carry no status code, so the
// "401" requirement correctly excludes them.
func isTransientClaudeDeliveryError(result subprocess.Result, err error) bool {
	return isClaude401SocketClosed(result, err)
}

// isClaude401SocketClosed reports whether a failed run carries the
// "401 socket connection was closed unexpectedly" signature. In the Deliver path
// this is the concurrency transient a byte-identical retry clears
// (isTransientClaudeDeliveryError); in the isolated doctor probe path (no
// concurrency) a signature that SURVIVES a retry is instead the documented #486
// invalid-token symptom — `Failed to authenticate. API Error: 401 The socket
// connection was closed unexpectedly` — which carries "401" and "socket connection
// was closed unexpectedly" but NOT "authentication", so isClaudeAuthFailure alone
// would miss it. The err != nil gate keeps a successful run from ever matching.
func isClaude401SocketClosed(result subprocess.Result, err error) bool {
	if err == nil {
		return false
	}
	// Never treat a genuine auth failure (an invalid/expired token, #486) as a
	// retryable transient, even if its message also happens to carry a
	// socket-closed phrase: retrying it claudeDeliveryMaxAttempts times and then
	// minting a fresh session would burn minutes and could mask the real "fix
	// your token" signal. The socket-closed transient (#509) carries a "Failed to
	// authenticate" wording but none of the auth-failure markers, so this guard
	// excludes only genuinely invalid credentials, not the transient.
	if isClaudeAuthFailure(result) {
		return false
	}
	text := strings.ToLower(result.Stdout + "\n" + result.Stderr)
	if !strings.Contains(text, "401") {
		return false
	}
	return strings.Contains(text, "socket connection was closed unexpectedly") ||
		strings.Contains(text, "socket connection closed unexpectedly")
}

// claudeFreshSessionArgs builds the args for a brand-new dedicated session,
// mirroring Start's --session-id shape (never --resume/--continue), so self-heal
// preserves per-agent isolation rather than collapsing onto the shared --continue
// path.
func claudeFreshSessionArgs(agent Agent, prompt string, model string, sessionID string) []string {
	args := claudePermissionArgs(agent)
	if model != "" {
		args = append(args, "--model", model)
	}
	return append(args, "--session-id", sessionID, "-p", "--output-format", "json", "--", prompt)
}

// claudeSessionMissingError wraps an unrecoverable dead-session failure (the
// pinned session was gone AND the fresh start also failed) in an actionable,
// agent-named gitmoot message instead of raw CLI stderr. If the fresh start
// failed for an auth reason, ClassifyClaudeCommandError still surfaces it as auth
// so a real auth failure is never masked as a stale session.
func (a ClaudeAdapter) claudeSessionMissingError(agent Agent, result subprocess.Result, err error) error {
	if isClaudeAuthFailure(result) {
		return claudeCommandError(result, err)
	}
	return fmt.Errorf("runtime session for agent %q no longer exists and a fresh session could not be started. %s: %w",
		agent.Name, ClaudeSessionMissingMessage, claudeCommandError(result, err))
}

// claudeJSONResult mirrors the relevant fields of the Claude Code
// --output-format json result envelope. The CLI emits a single JSON object whose
// "result" holds the assistant's final text and whose "usage" object carries the
// token counts. We read only what the budget and summary need and ignore the rest
// (cost, session id, tool stats, …) so unrelated schema additions are harmless.
type claudeJSONResult struct {
	Result string `json:"result"`
	Usage  struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// parseClaudeJSONResult extracts the assistant summary and best-effort token
// usage from a Claude Code --output-format json envelope. It returns ("", 0, 0)
// when stdout is not the JSON envelope (e.g. the --output-format json fallback to
// plain text), so the caller can fall back to the raw text and contribute 0 to
// the token budget. Usage capture is best-effort: a missing "usage" object leaves
// the counts at 0 rather than erroring.
func parseClaudeJSONResult(stdout string) (summary string, inputTokens int, outputTokens int) {
	var payload claudeJSONResult
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		return "", 0, 0
	}
	return payload.Result, payload.Usage.InputTokens, payload.Usage.OutputTokens
}

func (a ClaudeAdapter) Health(ctx context.Context, agent Agent) error {
	if err := a.Validate(ctx, agent); err != nil {
		return err
	}
	_, err := a.Deliver(ctx, agent, Job{Prompt: healthPrompt})
	return err
}

func (a ClaudeAdapter) Capabilities(context.Context) ([]string, error) {
	return []string{"review", "implement", "ask"}, nil
}

func (a ClaudeAdapter) runner() subprocess.Runner {
	if a.Runner != nil {
		return a.Runner
	}
	// Group semantics so cancelling a job kills the runtime CLI's whole
	// process tree, not just the immediate child.
	return subprocess.GroupRunner{}
}

func (a ClaudeAdapter) newRuntimeRef() (string, error) {
	if a.NewRuntimeRef != nil {
		return a.NewRuntimeRef()
	}
	return newUUID()
}

type ShellAdapter struct {
	Runner subprocess.Runner
	Dir    string
}

func (a ShellAdapter) Name() string { return ShellRuntime }

func (a ShellAdapter) Start(ctx context.Context, request StartRequest) (StartResult, error) {
	if err := validateStartRequest(request.Agent, a.Name(), request.Prompt); err != nil {
		return StartResult{}, err
	}
	return StartResult{}, errors.New("shell runtime does not support agent start; use gitmoot agent subscribe --runtime shell --session <command>")
}

func (a ShellAdapter) Validate(_ context.Context, agent Agent) error {
	return validateRuntime(agent, a.Name())
}

func (a ShellAdapter) Deliver(ctx context.Context, agent Agent, job Job) (Result, error) {
	if err := a.Validate(ctx, agent); err != nil {
		return Result{}, err
	}
	// Shell "sessions" are commands, so there is no such thing as minting a
	// fresh one; a fresh ref reaching this adapter means a caller forgot to
	// require an explicit session command for a shell override.
	if IsFreshRef(agent.RuntimeRef) {
		return Result{}, errors.New("shell runtime cannot mint a fresh session; provide an explicit session command")
	}
	result, err := a.runner().Run(ctx, a.Dir, "sh", "-c", agent.RuntimeRef, "gitmoot", job.Prompt)
	// A shell "session" is a command line, not a session id, so SessionID stays
	// empty; the exit/stderr diagnostics still apply.
	if err != nil {
		return Result{Raw: result.Stdout + result.Stderr, SessionDiag: newSessionDiag(result, err, "")}, commandError(result, err)
	}
	return Result{Raw: result.Stdout, Summary: strings.TrimSpace(result.Stdout), SessionDiag: newSessionDiag(result, nil, "")}, nil
}

func (a ShellAdapter) Health(ctx context.Context, agent Agent) error {
	if err := a.Validate(ctx, agent); err != nil {
		return err
	}
	result, err := a.runner().Run(ctx, a.Dir, "sh", "-c", agent.RuntimeRef, "gitmoot-health", healthPrompt)
	if err != nil {
		return commandError(result, err)
	}
	return nil
}

func (a ShellAdapter) Capabilities(context.Context) ([]string, error) {
	return []string{"review", "implement", "ask"}, nil
}

func (a ShellAdapter) runner() subprocess.Runner {
	if a.Runner != nil {
		return a.Runner
	}
	// Group semantics so cancelling a job kills the runtime CLI's whole
	// process tree, not just the immediate child.
	return subprocess.GroupRunner{}
}

func commandError(result subprocess.Result, err error) error {
	detail := strings.TrimSpace(result.Stderr)
	if detail == "" {
		detail = strings.TrimSpace(result.Stdout)
	}
	if detail == "" {
		return err
	}
	return fmt.Errorf("%s: %w", detail, err)
}

// codexCommandError prefers the real failure message from codex's --json events
// (on stdout) over the stderr-first commandError, which otherwise surfaces
// codex's harmless "Reading additional input from stdin..." line instead of the
// actual cause (usage limit, auth, etc.). Falls back to commandError when stdout
// carries no parseable error event (e.g. the non-json Deliver path).
func codexCommandError(result subprocess.Result, err error) error {
	if detail := parseCodexErrorMessage(result.Stdout); detail != "" {
		return fmt.Errorf("%s: %w", detail, err)
	}
	return commandError(result, err)
}

// parseCodexErrorMessage scans codex --json JSONL for the failure message in an
// "error" ({"message":...}) or "turn.failed" ({"error":{"message":...}}) event,
// returning the last non-empty one (turn.failed usually follows the error event).
func parseCodexErrorMessage(output string) string {
	scanner := bufio.NewScanner(strings.NewReader(output))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	message := ""
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			Error   struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(line), &event) != nil {
			continue
		}
		switch event.Type {
		case "error":
			if m := strings.TrimSpace(event.Message); m != "" {
				message = m
			}
		case "turn.failed":
			if m := strings.TrimSpace(event.Error.Message); m != "" {
				message = m
			}
		}
	}
	return message
}

// codexUsage mirrors the usage object on codex's turn.completed --json event.
// input_tokens already includes cached_input_tokens, so we read only the billed
// input/output totals and deliberately ignore cached_input_tokens and
// reasoning_output_tokens — the per-root delegation token budget sums those two.
type codexUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// parseCodexJSONResult extracts the deliverable text and best-effort token usage
// from codex `exec --json` JSONL output. It joins the .text of every
// item.completed agent_message event with a blank line (the agent's full reply,
// which carries the gitmoot_result blob the engine scans for) and reads the LAST
// turn.completed usage (the final turn is authoritative if codex emits several).
//
// ok reports whether at least one agent_message was seen; ok=false means the
// stream is not the expected JSONL shape (older CLI, the plain-text --json
// fallback, or an error-only turn), so the caller fails open on the raw stdout
// with 0 usage. Usage is best-effort: agent_messages with no turn.completed leave
// the counts at 0 rather than erroring. It mirrors parseCodexErrorMessage's
// narrow, unmarshal-per-line style so unrelated schema additions stay harmless.
func parseCodexJSONResult(output string) (raw string, inputTokens int, outputTokens int, ok bool) {
	scanner := bufio.NewScanner(strings.NewReader(output))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var messages []string
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event struct {
			Type string `json:"type"`
			Item struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"item"`
			Usage codexUsage `json:"usage"`
		}
		if json.Unmarshal([]byte(line), &event) != nil {
			continue
		}
		switch event.Type {
		case "item.completed":
			if event.Item.Type == "agent_message" {
				messages = append(messages, event.Item.Text)
			}
		case "turn.completed":
			inputTokens = event.Usage.InputTokens
			outputTokens = event.Usage.OutputTokens
		}
	}
	if len(messages) == 0 {
		return "", 0, 0, false
	}
	return strings.Join(messages, "\n\n"), inputTokens, outputTokens, true
}

// isCodexJSONUnsupported reports whether a failed codex run rejected --json, i.e.
// an older CLI that predates JSONL event output. codex (clap) prints e.g.
// "error: unexpected argument '--json' found" to stderr with a nonzero exit;
// runCodex re-runs plain-text on this signal (mirrors isClaudeJSONUnsupported).
func isCodexJSONUnsupported(result subprocess.Result) bool {
	text := strings.ToLower(result.Stdout + "\n" + result.Stderr)
	if !strings.Contains(text, "--json") {
		return false
	}
	return strings.Contains(text, "unexpected argument") ||
		strings.Contains(text, "unknown") ||
		strings.Contains(text, "unrecognized") ||
		strings.Contains(text, "unsupported") ||
		strings.Contains(text, "invalid")
}

func claudeCommandError(result subprocess.Result, err error) error {
	return ClassifyClaudeCommandError(result, err)
}

// classifyClaudeProbeError classifies the error from an ISOLATED doctor live
// probe (a single `claude -p`, no concurrency). It is ClassifyClaudeCommandError
// plus one extra Invalid signature: a 401 "socket connection was closed
// unexpectedly" that survived the probe's retry is the documented #486
// invalid-token symptom rather than a concurrency transient, so it must be tagged
// ErrClaudeAuthFailed (→ ClaudeClassifyProbe Invalid) instead of falling through
// to Unknown, which kept `gitmoot doctor` green for the exact bug it exists to
// catch. The Deliver path keeps treating the same signature as transient (it
// never calls this), so retry behavior there is unchanged.
func classifyClaudeProbeError(result subprocess.Result, err error) error {
	if !isClaudeAuthFailure(result) && isClaude401SocketClosed(result, err) {
		base := commandError(result, err)
		msg := fmt.Errorf("Claude Code authentication failed. %s: %w", ClaudeSessionAuthFailedMessage, base)
		return &claudeAuthFailedError{wrapped: msg}
	}
	return ClassifyClaudeCommandError(result, err)
}

func ClassifyClaudeCommandError(result subprocess.Result, err error) error {
	base := commandError(result, err)
	if !isClaudeAuthFailure(result) {
		return base
	}
	msg := fmt.Errorf("Claude Code authentication failed. %s: %w", ClaudeSessionAuthFailedMessage, base)
	// Tag the classified auth failure with ErrClaudeAuthFailed so callers can
	// distinguish a genuine credential rejection (401/invalid) from a transient
	// or network error via errors.Is, without re-matching stderr strings. The
	// wrapper's Error() is the message verbatim, so existing text assertions and
	// the wrapped exec error remain reachable.
	return &claudeAuthFailedError{wrapped: msg}
}

// ErrClaudeAuthFailed marks an error chain produced by ClassifyClaudeCommandError
// for a genuine Claude credential rejection. Use errors.Is(err, ErrClaudeAuthFailed)
// to tell "the token was rejected" (Invalid) apart from "the probe could not
// reach a verdict" (transient/network/binary-missing → Unknown).
var ErrClaudeAuthFailed = errors.New("Claude authentication failed")

// claudeAuthFailedError tags wrapped with ErrClaudeAuthFailed while preserving
// wrapped's message and chain. Go's errors.Is walks the slice returned by
// Unwrap() []error, so both wrapped (and through it the exec error) and the
// sentinel stay reachable.
type claudeAuthFailedError struct{ wrapped error }

func (e *claudeAuthFailedError) Error() string   { return e.wrapped.Error() }
func (e *claudeAuthFailedError) Unwrap() []error { return []error{e.wrapped, ErrClaudeAuthFailed} }

// claudeSessionMissingMarker is the exact stderr signature emitted by `claude
// --resume <uuid>` when the pinned conversation no longer exists (captured from
// claude v2.1.191). It is a pre-execution failure: exit 1, empty stdout, no model
// turn ran — so a retry on a fresh session duplicates no side effects.
const claudeSessionMissingMarker = "No conversation found with session ID:"

// isClaudeSessionMissing reports whether a failed Claude delivery is the
// pre-execution dead-session signal — distinct from auth failures and from
// generic non-zero exits. It matches the distinctive mixed-case marker without
// lowercasing (lowercasing would force lowercasing the marker too and weaken the
// match). The exit!=0 precondition is enforced structurally by the Deliver call
// site, which only consults this classifier when the runner returned err!=nil;
// subprocess.Result carries no exit-code field, so the marker is the signal.
func isClaudeSessionMissing(result subprocess.Result) bool {
	return strings.Contains(result.Stderr, claudeSessionMissingMarker) ||
		strings.Contains(result.Stdout, claudeSessionMissingMarker)
}

func isClaudeAuthFailure(result subprocess.Result) bool {
	text := strings.ToLower(strings.Join([]string{
		result.Stdout,
		result.Stderr,
	}, "\n"))
	if strings.Contains(text, "authentication_error") {
		return true
	}
	if strings.Contains(text, "invalid authentication credentials") {
		return true
	}
	if strings.Contains(text, "invalid x-api-key") {
		return true
	}
	return strings.Contains(text, "401") && strings.Contains(text, "authentication")
}

func parseCodexStartedThreadID(output string) (string, error) {
	scanner := bufio.NewScanner(strings.NewReader(output))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event struct {
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
		}
		if json.Unmarshal([]byte(line), &event) != nil {
			continue
		}
		if event.Type == "thread.started" && strings.TrimSpace(event.ThreadID) != "" {
			return strings.TrimSpace(event.ThreadID), nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("parse codex start output: %w", err)
	}
	return "", errors.New("codex start did not emit thread.started thread_id")
}

func newUUID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	encoded := make([]byte, 32)
	hex.Encode(encoded, bytes[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", encoded[0:8], encoded[8:12], encoded[12:16], encoded[16:20], encoded[20:32]), nil
}

// claudeArgs builds the `claude` argument vector; the prompt is the trailing
// positional after `--`.
//
// ARG_MAX exposure (#723): like kimi, claude passes the prompt as a trailing argv
// arg, so a prompt above the kernel's MAX_ARG_STRLEN (~128 KiB per single arg)
// would fork/exec with E2BIG. claude, unlike kimi, has a native escape hatch — its
// `-p/--print` non-interactive mode reads the prompt from stdin when no positional
// is supplied. Routing oversize prompts through stdin is deferred to a follow-up;
// #723 fixes kimi, which offers no stdin/file channel.
func claudeArgs(agent Agent, prompt string, jsonOutput bool, model string) []string {
	args := claudePermissionArgs(agent)
	if model != "" {
		args = append(args, "--model", model)
	}
	if agent.RuntimeRef == "last" {
		args = append(args, "--continue")
	} else {
		args = append(args, "--resume", agent.RuntimeRef)
	}
	args = append(args, "-p")
	if jsonOutput {
		args = append(args, "--output-format", "json")
	}
	return append(args, "--", prompt)
}

func claudePermissionArgs(agent Agent) []string {
	switch NormalizeStoredAutonomyPolicy(agent.AutonomyPolicy) {
	case AutonomyPolicyReadOnly:
		return []string{"--permission-mode", "plan"}
	case AutonomyPolicyWorkspaceWrite:
		return []string{"--permission-mode", "acceptEdits"}
	case AutonomyPolicyDangerFullAccess:
		return []string{"--permission-mode", "bypassPermissions"}
	default:
		return nil
	}
}

func isClaudeJSONUnsupported(result subprocess.Result) bool {
	text := strings.ToLower(result.Stdout + "\n" + result.Stderr)
	return strings.Contains(text, "output-format") &&
		(strings.Contains(text, "unknown") || strings.Contains(text, "unsupported") || strings.Contains(text, "invalid"))
}

func validateRuntime(agent Agent, runtimeName string) error {
	if err := ValidateAgent(agent); err != nil {
		return err
	}
	if agent.Runtime != runtimeName {
		return fmt.Errorf("agent runtime %q does not match adapter %q", agent.Runtime, runtimeName)
	}
	return nil
}

func validRepoScope(scope string) bool {
	parts := strings.Split(scope, "/")
	return len(parts) == 2 && strings.TrimSpace(parts[0]) != "" && strings.TrimSpace(parts[1]) != ""
}

func isUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index, char := range value {
		switch index {
		case 8, 13, 18, 23:
			if char != '-' {
				return false
			}
		default:
			if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f') || (char >= 'A' && char <= 'F')) {
				return false
			}
		}
	}
	return true
}

func (r CodexSessionIndex) Exists(ctx context.Context, ref string) (bool, error) {
	locations, err := r.locations()
	if err != nil {
		return false, err
	}
	for _, location := range locations {
		found, err := codexSessionIndexContains(ctx, location.IndexPath, ref)
		if err != nil {
			return false, err
		}
		if found {
			return true, nil
		}
		if isUUID(ref) {
			found, err = codexShellSnapshotsContain(ctx, location.SnapshotsDir, ref)
			if err != nil {
				return false, err
			}
			if found {
				return true, nil
			}
		}
	}
	return false, nil
}

type codexSessionLocation struct {
	IndexPath    string
	SnapshotsDir string
}

func codexSessionIndexContains(ctx context.Context, path string, ref string) (bool, error) {
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		var entry struct {
			ID         string `json:"id"`
			ThreadName string `json:"thread_name"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			return false, fmt.Errorf("parse codex session index: %w", err)
		}
		if entry.ID == ref || entry.ThreadName == ref {
			return true, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return false, err
	}
	return false, nil
}

func codexShellSnapshotsContain(ctx context.Context, dir string, ref string) (bool, error) {
	pattern := filepath.Join(dir, ref+".*.sh")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return len(matches) > 0, nil
}

func (r CodexSessionIndex) locations() ([]codexSessionLocation, error) {
	if r.Path != "" {
		return []codexSessionLocation{codexLocationForIndex(r.Path)}, nil
	}
	homes := []string{}
	if home := strings.TrimSpace(r.Home); home != "" {
		return []codexSessionLocation{codexLocationForHome(home)}, nil
	}
	if home := strings.TrimSpace(os.Getenv("CODEX_HOME")); home != "" {
		homes = append(homes, home)
	}
	userHome, err := os.UserHomeDir()
	if err != nil && len(homes) == 0 {
		return nil, err
	}
	if err == nil {
		homes = append(homes, filepath.Join(userHome, ".codex"))
	}
	locations := make([]codexSessionLocation, 0, len(homes))
	seen := map[string]bool{}
	for _, home := range homes {
		home = filepath.Clean(home)
		if seen[home] {
			continue
		}
		seen[home] = true
		locations = append(locations, codexLocationForHome(home))
	}
	return locations, nil
}

func (r CodexSessionIndex) path() (string, error) {
	locations, err := r.locations()
	if err != nil {
		return "", err
	}
	if len(locations) == 0 {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(userHome, ".codex", "session_index.jsonl"), nil
	}
	return locations[0].IndexPath, nil
}

func codexLocationForIndex(path string) codexSessionLocation {
	home := filepath.Dir(path)
	return codexSessionLocation{
		IndexPath:    path,
		SnapshotsDir: filepath.Join(home, "shell_snapshots"),
	}
}

func codexLocationForHome(home string) codexSessionLocation {
	return codexSessionLocation{
		IndexPath:    filepath.Join(home, "session_index.jsonl"),
		SnapshotsDir: filepath.Join(home, "shell_snapshots"),
	}
}
