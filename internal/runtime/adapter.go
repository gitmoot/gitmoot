package runtime

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"

	"github.com/gitmoot/gitmoot/internal/subprocess"
)

const (
	ShellRuntime = "shell"
	LastRef      = "last"

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
	// SingleUseSession marks an agent whose runtime session exists solely for
	// the current job (ephemeral delegation workers and per-job temp workers:
	// the session is started for the job and disposed after it). Adapters whose
	// CLIs report SESSION-CUMULATIVE usage on resumed sessions (codex, see
	// codexDeliverResult) may attribute that cumulative usage to the job when
	// this is set — the whole session belongs to the job, so cumulative IS the
	// per-job cost. Never set for long-lived registered agents, whose sessions
	// span many jobs. In-memory only; not persisted on the agents table.
	SingleUseSession bool
	// ReadOnlySeat marks a delivery running in a detached read-only worktree.
	// The CLI wraps these jobs in a hard filesystem sandbox before adapters
	// receive the widened command and network permissions below.
	// In-memory only; never persisted on the agents table.
	ReadOnlySeat bool
	// NoExecution narrows a ReadOnlySeat to a seat that may run nothing (#2364).
	// Adapters that support it restrict the runtime to read-only tools; the
	// daemon also launches the runtime under a sandbox that denies executing any
	// other program. In-memory only.
	NoExecution bool
	// NoExecutionConfig is the omp settings overlay a no-execution seat runs
	// with (it disables every config-discovery provider, MCP and extensions).
	// Set with NoExecution; the daemon grants the sandbox read access to it.
	// In-memory only.
	NoExecutionConfig string
	// noExecutionProjectDir is the checkout a no-execution omp run works on,
	// passed as --cwd while the process starts elsewhere. Set by OmpAdapter
	// on its own copy of the agent at delivery; never set by callers.
	noExecutionProjectDir string
	// RuntimeConfigDir is the source profile selected by the dispatching process.
	// Read-only seats copy only the provider authentication needed by that
	// runtime into per-job state; the source directory is never sandbox-visible.
	RuntimeConfigDir string
	// ExecBackend is the execution backend resolved for THIS job at dispatch
	// (#1536 P1) from [remote_exec].backend plus the payload's exec_backend
	// override. Dispatch stamps it in-memory before adapter construction so the
	// same selection reaches runtime preflight, lifecycle, workflow-owned checks,
	// and result observation; "local" (the only implemented backend) leaves those
	// paths byte-for-byte unchanged.
	// Empty — every construction site that does not stamp it (direct adapter
	// builds, tests, `agent start`) — selects the same local default. In-memory
	// only; never persisted on the agents table.
	ExecBackend string
	// WorkingDir is the resolved filesystem checkout directory the runtime
	// adapter should chdir into for a delivery. It is DISTINCT from RepoScope,
	// which stays in "owner/repo" form (and is validated as such). Callers that
	// need a real subprocess to run inside a repo checkout (e.g. an
	// synth loop) resolve the repo's registered db.Repo.CheckoutPath and set it
	// here; the delivery seam prefers this over RepoScope when picking the
	// adapter Dir. In-memory only; not persisted on the agents table and never
	// marshaled to the wire. Empty means "fall back to RepoScope" (unchanged
	// legacy behavior for callers that never set it).
	WorkingDir string
	// ConfigHome is the raw --home value used only when constructing a runtime
	// adapter for one-shot deliveries. It is in-memory only.
	ConfigHome string
	// WritablePaths are additive runtime workspace grants (a read-only seat's
	// isolated tool cache). Codex enforces them itself; Claude/Kimi/omp receive
	// matching --add-dir hints while Gitmoot's Landlock wrapper provides hard
	// enforcement.
	WritablePaths []string
	// ReadablePaths are external read-only inputs. Claude/Kimi receive
	// cooperative --add-dir visibility while Landlock keeps these roots read-only;
	// Codex's native workspace sandbox already permits filesystem reads and has no
	// read-only --add-dir equivalent.
	ReadablePaths []string
	// ReadableFiles are runtime-owned configuration files admitted to the
	// Landlock ruleset without exposing their parent directory. They are not
	// passed as --add-dir hints and remain read-only.
	ReadableFiles []string
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
	// Plan and PlanInto request PLAN MODE for this delivery: the runtime plans the
	// work first and then AUTO-EXECUTES that plan, instead of implementing straight
	// from the prompt. They are PRIMITIVES on purpose — the workflow layer resolves
	// them from JobPayload exactly the way it resolves Model/Effort, so
	// internal/runtime never learns the payload type. PlanInto names the model the
	// execution phase runs on and REQUIRES Plan; the pair is rejected before any
	// subprocess otherwise. Only the runtimes SupportsPlanMode reports can honour
	// them — a plan request routed anywhere else fails loudly at dispatch rather
	// than silently degrading to a normal run.
	Plan     bool
	PlanInto string
	// ShellEnv is an exact list of KEY=value entries injected into the shell
	// runtime's subprocess (for example the managed-worktree GOFLAGS). Other
	// runtimes ignore it.
	ShellEnv []string
	// AgentEnv is an in-memory-only list of KEY=value entries injected into a
	// managed agent runtime's subprocess (for example the managed-worktree
	// GOFLAGS). It is never represented in workflow.JobPayload. Empty preserves
	// the historical plain Runner path.
	AgentEnv []string
	// RuntimeDefaultModel is the runtime's configured registry default_model
	// (#652), threaded in by the dispatch layer from the HOME-AWARE resolved runtime
	// registry (built-in defaults overlaid with [runtimes.<name>] config). It is the
	// FINAL model fallback: EffectiveModel uses it ONLY when neither the job (Model)
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
	// OnPID, when non-nil and supported by the runner, receives the exact
	// subprocess PID after start and before Deliver blocks for completion.
	// Runners without the optional PID capability ignore it and retain the
	// historical delivery path.
	OnPID func(int)
}

type Result struct {
	Decision string
	Summary  string
	Raw      string
	// InputTokens and OutputTokens are best-effort per-job runtime token usage
	// captured from the CLI's structured output where it exposes one (#338 Part
	// B). They default to 0; a 0 means "not captured", and the per-root
	// delegation token budget simply under-counts that job rather than failing.
	InputTokens  int
	OutputTokens int
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
	// PlanMode is the adapter's own evidence of the plan shape it DISPATCHED, so a
	// reader can tell a plan run from a normal one without re-deriving it from the
	// argv: "" for a normal run, "plan-into:<model>" when Gitmoot resolved the
	// execution target, and "plan-into:<runtime-default>" when it could not. There
	// is deliberately no bare "plan" value — the execution phase always runs on some
	// model, and a value that declined to say which is the ambiguity this field
	// exists to remove. Adapters that cannot honour plan mode never set it (the
	// dispatch layer rejects the request before they are reached), and it is
	// populated on failure returns too — a plan run that died is still a plan run.
	PlanMode string
	// UpstreamProvider is execution evidence emitted only after a routed runtime
	// completed successfully. OMP sets it from provider/model metadata on the
	// final successful runtime message; callers must not infer it from a requested
	// model string or an agent default. Native runtimes leave it empty.
	UpstreamProvider string
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
	case OmpRuntime:
		return OmpAdapter{Runner: f.Runner}, nil
	case ShellRuntime:
		return ShellAdapter{Runner: f.Runner}, nil
	default:
		if IsRemovedRuntime(name) {
			return nil, RemovedRuntimeError(name)
		}
		return nil, fmt.Errorf("unsupported runtime: %s (supported: %s)", name, strings.Join(SupportedRuntimes(), ", "))
	}
}

// removedRuntimes are the job runtimes gitmoot no longer drives (#2587): omp is
// the job runtime and shell runs the no-LLM tests. An agent row or [agents.*]
// block still naming one keeps loading so it can be listed and re-registered,
// but every path that needs an adapter refuses it with RemovedRuntimeError.
var removedRuntimes = []string{"codex", "claude", "kimi"}

// ErrRemovedRuntime marks an error chain refusing an agent on a removed runtime.
var ErrRemovedRuntime = errors.New("removed job runtime")

// IsRemovedRuntime reports whether name is a job runtime gitmoot removed.
func IsRemovedRuntime(name string) bool {
	name = strings.TrimSpace(name)
	for _, removed := range removedRuntimes {
		if name == removed {
			return true
		}
	}
	return false
}

// RemovedRuntimeError is the refusal for a removed runtime; it says omp is
// required so the operator knows how to re-register the agent.
func RemovedRuntimeError(name string) error {
	return fmt.Errorf("%w: runtime %q is no longer supported; gitmoot requires omp for jobs (shell only for no-LLM tests): re-register the agent with --runtime omp", ErrRemovedRuntime, strings.TrimSpace(name))
}

// UnsupportedAgentRuntimeError names the agent whose registration still points
// at a removed runtime, for agent list, doctor, the daemon log and job refusal.
func UnsupportedAgentRuntimeError(agentName, runtimeName string) error {
	return fmt.Errorf("agent %s: %w", agentName, RemovedRuntimeError(runtimeName))
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
	// omp is STATELESS (OmpAdapter never resumes), so "last" — the resume grammar
	// the other runtimes accept — is rejected here rather than silently stored on a
	// registration it can never honor. The grammar mirrors OmpAdapter.Validate EXCEPT
	// in two ways, and this door is the STRICTER one on both:
	//
	//  1. It additionally requires a NON-EMPTY ref. validateAgentFields(agent, true)
	//     runs above, BEFORE this clause, and has already rejected an empty ref, so
	//     the clause is never reached with one and must not promise "or empty". The
	//     accepts-empty contract lives on OmpAdapter.Validate, which guards both the
	//     Start and the Deliver path: it never calls validateAgentFields at all,
	//     running ompAgentFieldChecks — which carries no requireRuntimeRef clause —
	//     instead, so a stateless omp job that legitimately carries no ref is still
	//     accepted there.
	//  2. It matches the RAW field, while OmpAdapter.Validate TRIMS the ref first
	//     (`ref := strings.TrimSpace(agent.RuntimeRef)`) and matches that. So a ref
	//     with surrounding whitespace — " <uuid> " — is REJECTED here and ACCEPTED
	//     there; the two are not byte-for-byte mirrors. validateAgentFields' own
	//     TrimSpace does not close that gap: it trims only for the EMPTINESS test and
	//     never rewrites the field. That is deliberate rather than an oversight: the
	//     CLI stores the flag value untrimmed (runAgentSubscribe assigns
	//     `RuntimeRef: *session`), so the raw string is what actually lands in the
	//     agent record — refusing to register a ref whose stored form is not a ref is
	//     the honest behavior for a registration door.
	if agent.Runtime == OmpRuntime && agent.RuntimeRef != "" && !isUUID(agent.RuntimeRef) && !IsFreshRef(agent.RuntimeRef) {
		return fmt.Errorf("omp runtime reference %q must be an omp session UUID or fresh:<suffix>", agent.RuntimeRef)
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

// SupportsPlanMode reports whether a runtime's CLI can honour a plan-mode job
// (Job.Plan / Job.PlanInto). Plan mode is WORKFLOW SHAPE — plan first, then
// auto-execute the plan — and is a SEPARATE FIELD from the autonomy policy so the
// two are not conflated into one overloaded knob (#1420's defect).
//
// SEPARATE IS NOT INDEPENDENT, and an earlier version of this doc got that wrong
// twice. It called plan mode "orthogonal to write permission" and justified that
// by claiming an autonomy-policy-to-approval-mode mapping "would brick the
// runtime". Both are false: measured against omp 17.2.4, always-ask restricts omp
// to read-only tools and exits cleanly (see the ompRuntimeContract comment), and
// upstream plan mode ADDS the write tool to the active set rather than removing
// it. Plan mode ends in an implementation phase, so a plan request is a WRITE
// REQUEST and the dispatch gate requires the same write-granting policy an
// implement job requires.
//
// omp is the only runtime with the flag today (`--plan-yolo`). The set is closed
// on purpose: the dispatch layer refuses a plan request aimed anywhere else, so a
// runtime that grows the capability must be added HERE and taught to emit its own
// flag in the same change. Failing loudly beats a plan-gated brief silently
// running as an ordinary implementation.
func SupportsPlanMode(runtimeName string) bool {
	return strings.TrimSpace(runtimeName) == OmpRuntime
}

// PlanModeDescriptor renders the resolved plan shape as the durable evidence
// string carried by Result.PlanMode: "" when plan mode is off, "plan-into:<model>"
// when the execution phase is pinned to a model, and "plan-into:<runtime-default>"
// when it is not — because the execution phase ALWAYS runs on some model, and the
// evidence has to say whether Gitmoot chose it. Callers pass the RESOLVED target,
// not the raw request field: a bare "plan" would let a run whose diff was written
// by the runtime's cheap default look identical to one written by the pinned
// model, which is the ambiguity this string exists to remove. A planInto without
// plan is not representable — it is rejected before dispatch — so an unset plan
// always renders "".
func PlanModeDescriptor(plan bool, planInto string) string {
	if !plan {
		return ""
	}
	if into := strings.TrimSpace(planInto); into != "" {
		return "plan-into:" + into
	}
	return "plan-into:<runtime-default>"
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

// EffectiveModel resolves which model a delivered job runs on. Precedence, most
// specific first: the per-job/per-delegation override (job.Model) wins, then the
// agent's configured default (agent.Model), then — when NEITHER pins a model — the
// runtime's configured registry default_model (job.RuntimeDefaultModel, #652). An
// empty result means "no --model arg" (the runtime CLI's own default). Because the
// registry default is consulted last and defaults to empty, an agent/job pin
// always wins and an unset registry default is byte-identical to before #652.
func EffectiveModel(agent Agent, job Job) string {
	if job.Model != "" {
		return job.Model
	}
	if agent.Model != "" {
		return agent.Model
	}
	return strings.TrimSpace(job.RuntimeDefaultModel)
}

// ApplyJobRuntimeOverride returns the effective agent identity for a job-level
// runtime override. Model and effort defaults belong to the registered runtime,
// so both are cleared when switching runtimes; the caller's per-job Model and
// Effort continue to travel on Job.
func ApplyJobRuntimeOverride(agent Agent, runtimeName, runtimeRef string) Agent {
	runtimeName = strings.TrimSpace(runtimeName)
	if runtimeName == "" {
		return agent
	}
	agent.Runtime = runtimeName
	agent.RuntimeRef = strings.TrimSpace(runtimeRef)
	agent.Model = ""
	agent.Effort = ""
	return agent
}

// effectiveEffort resolves which reasoning effort a delivered job runs with.
// Precedence mirrors EffectiveModel exactly: the per-job override wins, then the
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

func runAgentCommand(ctx context.Context, runner subprocess.Runner, dir string, env []string, onPID func(int), command string, args ...string) (subprocess.Result, error) {
	if onPID == nil {
		if len(env) == 0 {
			return runner.Run(ctx, dir, command, args...)
		}
		envRunner, ok := runner.(subprocess.EnvRunner)
		if !ok {
			return subprocess.Result{}, errors.New("runtime runner does not support agent environment injection")
		}
		return envRunner.RunEnv(ctx, dir, env, command, args...)
	}
	if len(env) == 0 {
		if pidRunner, ok := runner.(subprocess.PIDRunner); ok {
			return pidRunner.RunWithPID(ctx, dir, onPID, command, args...)
		}
		return runner.Run(ctx, dir, command, args...)
	}
	if envPIDRunner, ok := runner.(subprocess.EnvPIDRunner); ok {
		return envPIDRunner.RunEnvWithPID(ctx, dir, env, onPID, command, args...)
	}
	envRunner, ok := runner.(subprocess.EnvRunner)
	if !ok {
		return subprocess.Result{}, errors.New("runtime runner does not support agent environment injection")
	}
	return envRunner.RunEnv(ctx, dir, env, command, args...)
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
	runner := a.runner()
	env := append([]string(nil), job.ShellEnv...)
	var result subprocess.Result
	var err error
	if len(env) > 0 {
		envRunner, ok := runner.(subprocess.EnvRunner)
		if !ok {
			return Result{}, errors.New("shell runtime runner does not support environment injection")
		}
		result, err = envRunner.RunEnv(ctx, a.Dir, env, "sh", "-c", agent.RuntimeRef, "gitmoot", job.Prompt)
	} else {
		result, err = runner.Run(ctx, a.Dir, "sh", "-c", agent.RuntimeRef, "gitmoot", job.Prompt)
	}
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
