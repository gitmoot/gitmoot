package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/agenttemplate"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/prompts"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/subprocess"
)

// maxRepairAttempts bounds how many times a malformed (missing gitmoot_result
// envelope) output is re-asked with the repair prompt before the job is failed
// terminally. The initial delivery plus up to this many repair re-asks salvages a
// recoverable missing envelope — where the agent already produced useful work but
// omitted the JSON contract — instead of driving the job straight to JobFailed,
// which would make automation treat delivered work as a failure and re-run it
// (causing duplicate side effects). It is intentionally small and bounded so the
// loop can never spin (#495).
const maxRepairAttempts = 2

type Mailbox struct {
	Store *db.Store
	// emitTerminal, when set, is called best-effort AFTER a genuine running->
	// terminal transition in BOTH finishWithPayload (success/advance + timeout-
	// finalize) and finish (the m.fail delivery/parse-failure path) (#446). Wiring
	// both is what makes the whole terminal set fan out exactly once: the
	// success/advance path emits job.finished, and the most common failure mode —
	// a runtime delivery error, timeout, or malformed-output-after-repair — emits
	// job.failed/job.blocked through finish rather than being silently dropped. It
	// is nil-safe: when unset (the default, no EventSink configured) no event is
	// constructed or emitted and behavior is byte-identical. The hook is
	// fire-and-forget — it must never block or fail the finish.
	emitTerminal func(ctx context.Context, jobID string, state JobState, payload JobPayload)
	// CanaryRand, when set, is the per-resolution random draw in [0,1) the canary
	// routing seam uses (#484) so tests can force a deterministic route (0.0 always
	// hits the canary, a value >= sample always resolves the champion). When nil
	// (the default, every production construction site) it falls back to the
	// concurrency-safe global rand.Float64, so concurrent Enqueues each draw
	// independently and safely. It is ONLY consulted when an active canary row
	// exists for the resolved template, so when the feature is off the rng is never
	// drawn and resolution is byte-identical.
	CanaryRand func() float64
	// deferBlocker, when set, is the injected PRE-TERMINAL operational-blocker
	// deferrer (#532 slice E). On a DELIVERY-seam failure (adapter.Deliver errored)
	// Run consults it BEFORE m.fail: if it re-queues the job (running->queued +
	// job.deferred), Run returns an ErrJobDeferred error and NEVER calls m.fail, so
	// a deferred job never emits job.failed to the [events] sink first (slice A
	// deferred AFTER the terminal transition, producing a failed→deferred flap). It
	// is nil-safe: when unset (the default, foreground/ask paths) Run is
	// byte-identical. The cause it receives is always a DeliveryError so the
	// classifier's #602 contract gate still refuses agent-authored text.
	deferBlocker func(ctx context.Context, jobID string, cause error) (bool, error)
	// CanaryEnabled gates the #484 canary ROUTING seam on the SAME policy the
	// daemon's regression comparator uses (config.SkillOptPolicy.CanaryEnabled()),
	// so the two seams turn on and off together. When false (the default, every
	// construction site that does not opt in) routeCanary returns immediately —
	// BEFORE the GetActiveCanaryVersion query — so no traffic is sampled, no rng is
	// drawn, and the hot Enqueue path is byte-identical to before the feature
	// existed. This is what stops a canary row from outliving the manager: turning
	// auto_promote_canary off (or unsetting the sample) and restarting the daemon
	// disables BOTH the comparator (no graduate/rollback) AND routing (no sampled
	// traffic), so a stranded canary can never keep serving traffic with no
	// auto-rollback. It is wired true only where the daemon-resolved policy reports
	// CanaryEnabled().
	CanaryEnabled bool
	// injectMemory, when set, returns the "Prior learnings" block (#626) to append
	// to the rendered job prompt. Nil (default, every path with no enrolled agent)
	// => byte-identical: no query runs and nothing is appended. It is wired from
	// Engine.Memory via mailbox(). Best-effort: it never errors up.
	injectMemory func(ctx context.Context, agent runtime.Agent, payload JobPayload) string
	// recordMemory, when set, shadow-logs the agent's returned learnings to
	// memory_observations and writes any gitmoot-authored mechanical facts to
	// confirmed_memories at job terminal (#626/#645). It is passed the job action
	// (job.Type) so the mechanical producers can key facts by (action, outcome).
	// Nil (default) => no-op, so the terminal path is byte-identical. Best-effort:
	// it never fails the job.
	recordMemory func(ctx context.Context, jobID string, agent runtime.Agent, action string, payload JobPayload, result AgentResult)
	// RuntimeDefaultModel, when set, resolves a runtime's configured default model
	// (the registry default_model, HOME-AWARE: built-in defaults overlaid with any
	// [runtimes.<name>] config) for the runtime named by the argument (#652). It is
	// consulted at delivery ONLY as the final model fallback — after the job --model
	// (payload.Model) and the agent --model (agent.Model) — so an agent/job pin
	// always wins. Wired from the CLI layer (which owns the home-aware resolver) via
	// Engine.RuntimeDefaultModel or directly on a CLI-constructed Mailbox. Nil (the
	// default, and any error/empty result) forces nothing, so delivery is
	// byte-identical to before #652.
	RuntimeDefaultModel func(runtimeName string) string
	// RuntimeDefaultEffort mirrors RuntimeDefaultModel for reasoning effort. It is
	// consulted only as the final fallback after payload.Effort and agent.Effort;
	// nil or empty forces no runtime argument.
	RuntimeDefaultEffort func(runtimeName string) string
	// routerContextEnabled gates the off-by-default #530 coordinator context block.
	// When true, Run appends a bounded (<=12 line) observed-performance table to a
	// TOP-LEVEL (coordinator) job's prompt. When false (the default, every
	// construction site that does not opt in) no telemetry query runs and the prompt
	// is byte-identical. Wired from Engine.RouterContextEnabled via mailbox().
	routerContextEnabled bool
	// resultCheckMode is the resolved [workflow] result_checks policy (#526). The
	// zero value ("") and "off" both disable the deterministic post-parse audit,
	// so every Mailbox built without explicitly setting it — every test, the ask/
	// foreground path — is byte-identical. The daemon resolves the real mode
	// (default warn) from config and wires it through Engine.ResultCheckMode.
	resultCheckMode ResultCheckMode
	// produceCheckDir is the resolved stage checkout used as cwd for trusted
	// operator checks when the payload has no explicit disposable worktree.
	produceCheckDir string
	// produceCheckTimeout bounds each trusted check command. Zero uses the
	// production default; tests may inject a short timeout.
	produceCheckTimeout time.Duration
}

// PipelineKeyAccess is the persisted, names-only authorization for one pipeline
// stage environment name. Source is pinned at enqueue so delivery never changes
// audit meaning by falling through to another source.
type PipelineKeyAccess struct {
	Stage  string `json:"stage"`
	Name   string `json:"name"`
	Source string `json:"source"`
	Mode   string `json:"mode"`
}

type JobRequest struct {
	ID           string
	Agent        string
	Action       string
	Repo         string
	Branch       string
	PullRequest  int
	HeadSHA      string
	GoalID       string
	TaskID       string
	TaskTitle    string
	LeadAgent    string
	Reviewers    []string
	ReviewRound  string
	Sender       string
	Instructions string
	Constraints  []string
	// TemplateOverride, when non-nil, replaces the agent's own template snapshot
	// for this job only (the agent's identity is unchanged). Used by the
	// orchestrate/run --recipe flag to route a coordinator to a built-in recipe
	// template's prompt without rebinding the agent.
	TemplateOverride      *db.AgentTemplate
	ParentJobID           string
	DelegationID          string
	DelegationDepth       int
	DelegatedBy           string
	RootJobID             string
	Deps                  []string
	JobTimeout            string
	RetryCount            int
	Fingerprint           string
	FailurePolicy         string
	SynthesisRule         string
	DelegationArtifactDir string
	WorktreePath          string
	// ReadOnlyWorktree marks a job whose WorktreePath is a throwaway detached
	// committed-tip worktree allocated for read-only (ask) isolation at DISPATCH
	// time (#739) — as opposed to a delegation child's fan-out worktree (which
	// carries a DelegationID) or an implement/task worktree (which carries a
	// Branch). It is the explicit signal the terminal cleanup uses to dispose a
	// TOP-LEVEL read-only worktree that has no DelegationID. Additive/omitempty:
	// false leaves the enqueued payload byte-identical.
	ReadOnlyWorktree       bool
	OriginalAgent          string
	DelegatedAgent         string
	DelegationReason       string
	RecentDelegationHashes []string
	DelegationRepeatCount  int
	NonProgressStreak      int
	LastProgressDigest     string
	VerifyAttempt          int
	DelegationFinalize     bool
	Model                  string
	Effort                 string
	// WorkflowID groups jobs started by an external coordinator. Empty preserves
	// the legacy payload byte-for-byte; non-empty values are inherited by every
	// delegation child and continuation in the coordination tree.
	WorkflowID string
	// RuntimeOverride, when non-empty, runs THIS job through the named runtime
	// instead of the agent's registered default runtime (#531). The agent's
	// stored runtime/session are untouched: the job runs on RuntimeOverrideRef
	// (an explicit session on the override runtime, or a minted fresh ref), and
	// the engine never persists a refreshed session ref for an overridden job.
	RuntimeOverride    string
	RuntimeOverrideRef string
	// ShellEnv carries pipeline trigger inputs and stage metadata as exact
	// KEY=value entries to a shell runtime job. It is empty for every non-pipeline
	// and non-shell job.
	ShellEnv []string
	// PipelineName and PipelineKeyAccess are the names-only keycard authority for
	// a pipeline stage. PipelineKeyAgent pins an agent stage to its registered seat
	// grant; empty means the existing pipeline/shell consumer. PipelineEnvFile and
	// PipelineEnvKeys remain for in-flight shell jobs enqueued before registry/grant
	// resolution shipped. PipelineEnv contains only selected inline NON-secret
	// defaults; secret values are loaded at delivery and never enter the payload.
	PipelineName      string
	PipelineKeyAgent  string
	PipelineKeyAccess []PipelineKeyAccess
	PipelineEnvFile   string
	PipelineEnvKeys   []string
	PipelineEnv       map[string]string
	// ShellUpstreamContext carries the persisted JSON CONTENT supplied to a
	// dependent pipeline shell stage. Delivery writes it to a fresh temporary
	// file; paths are deliberately never persisted. Additive/omitempty: empty
	// leaves ordinary and root shell jobs byte-identical.
	ShellUpstreamContext   string
	Phase                  string
	Cockpit                bool
	CockpitSession         string
	CockpitPaneKey         string
	SkipNativeReviewFanout bool
	ValidatedPullRequest   bool
	Ephemeral              *EphemeralSpec
	// HumanAnswer carries the rendered ask-gate answer block (#445) into the
	// coordinator continuation enqueued by the `answer` resume verb. Empty for
	// every other job, so the stored payload is byte-identical by default.
	HumanAnswer string
	// RiskTier is the resolved risk-tier of the change (#650), stamped on a
	// high-risk review coordinator and inherited by its lens children so reports
	// and the dashboard can explain an escalation. Empty (the default) for every
	// job outside the opt-in risk-tiered path.
	RiskTier string
	// ThreadID / ChatMessageID link a job back to the chat message it was
	// promoted from (#534). Populated only by `chat task` promotion; empty for
	// every non-chat job, so the stored payload is byte-identical by default. The
	// daemon terminal back-link posts the result into ThreadID when it is set.
	ThreadID      string
	ChatMessageID string
	// MootSeat marks the job as a `gitmoot moot` conversing SEAT (#732) — the ONLY
	// job class whose whole purpose is to run `gitmoot chat send/wait` mid-run and
	// therefore needs the daemon relay (and, for codex, the sandbox elevation that
	// reaches it). It is distinct from ThreadID, which every chat-linked job carries
	// (chat-task promotions, ask-gate coordinator continuations, delegation children
	// inheriting it for back-linking) but which must NOT trigger seat elevation. Set
	// only by the moot dispatch; never inherited by continuations/children.
	MootSeat bool
	// OrchestrateStage marks a #758 pipeline orchestrate stage job: the stage's agent
	// runs as a bounded sub-tree COORDINATOR, so its delegations[] are NOT stripped by
	// the pipeline-sender leaf strip (they fan out as children owned by this stage
	// job). It is set ONLY from the validated orchestrate spec by the pipeline
	// dispatch, never inferred by sender-sniffing, so every other pipeline-sender job
	// (shell, #757 agent leaf) keeps the delegations strip byte-identically.
	// Additive/omitempty: false leaves the enqueued payload byte-identical.
	OrchestrateStage bool
	WritablePaths    []string
	ReadablePaths    []string
	Network          bool
	Check            string
	CheckRetries     int
}

type JobPayload struct {
	Repo                  string   `json:"repo"`
	Branch                string   `json:"branch"`
	PullRequest           int      `json:"pull_request"`
	HeadSHA               string   `json:"head_sha,omitempty"`
	GoalID                string   `json:"goal_id,omitempty"`
	TaskID                string   `json:"task_id"`
	TaskTitle             string   `json:"task_title"`
	LeadAgent             string   `json:"lead_agent,omitempty"`
	Reviewers             []string `json:"reviewers,omitempty"`
	ReviewRound           string   `json:"review_round,omitempty"`
	Sender                string   `json:"sender"`
	Instructions          string   `json:"instructions"`
	Constraints           []string `json:"constraints"`
	ParentJobID           string   `json:"parent_job_id,omitempty"`
	DelegationID          string   `json:"delegation_id,omitempty"`
	DelegationDepth       int      `json:"delegation_depth,omitempty"`
	DelegatedBy           string   `json:"delegated_by,omitempty"`
	RootJobID             string   `json:"root_job_id,omitempty"`
	Deps                  []string `json:"deps,omitempty"`
	JobTimeout            string   `json:"job_timeout,omitempty"`
	RetryCount            int      `json:"retry_count,omitempty"`
	Fingerprint           string   `json:"fingerprint,omitempty"`
	FailurePolicy         string   `json:"failure_policy,omitempty"`
	SynthesisRule         string   `json:"synthesis_rule,omitempty"`
	DelegationArtifactDir string   `json:"delegation_artifact_dir,omitempty"`
	WorktreePath          string   `json:"worktree_path,omitempty"`
	// ReadOnlyWorktree marks a top-level read-only (ask) worktree allocated at
	// dispatch time (#739): its WorktreePath is a throwaway detached committed-tip
	// worktree with no DelegationID and no Branch. Additive/omitempty so a payload
	// without it serializes byte-identically. The terminal cleanup keys off it to
	// dispose top-level read-only worktrees that the DelegationID-gated read-only
	// delegation cleanup would otherwise orphan.
	ReadOnlyWorktree       bool                `json:"read_only_worktree,omitempty"`
	TemplateID             string              `json:"template_id,omitempty"`
	TemplateResolvedCommit string              `json:"template_resolved_commit,omitempty"`
	TemplateContent        string              `json:"template_content,omitempty"`
	OriginalAgent          string              `json:"original_agent,omitempty"`
	DelegatedAgent         string              `json:"delegated_agent,omitempty"`
	DelegationReason       string              `json:"delegation_reason,omitempty"`
	RecentDelegationHashes []string            `json:"recent_delegation_hashes,omitempty"`
	DelegationRepeatCount  int                 `json:"delegation_repeat_count,omitempty"`
	NonProgressStreak      int                 `json:"non_progress_streak,omitempty"`
	LastProgressDigest     string              `json:"last_progress_digest,omitempty"`
	VerifyAttempt          int                 `json:"verify_attempt,omitempty"`
	DelegationFinalize     bool                `json:"delegation_finalize,omitempty"`
	Model                  string              `json:"model,omitempty"`
	Effort                 string              `json:"effort,omitempty"`
	WorkflowID             string              `json:"workflow_id,omitempty"`
	RuntimeOverride        string              `json:"runtime_override,omitempty"`
	RuntimeOverrideRef     string              `json:"runtime_override_ref,omitempty"`
	ShellEnv               []string            `json:"shell_env,omitempty"`
	PipelineName           string              `json:"pipeline_name,omitempty"`
	PipelineKeyAgent       string              `json:"pipeline_key_agent,omitempty"`
	PipelineKeyAccess      []PipelineKeyAccess `json:"pipeline_key_access,omitempty"`
	PipelineEnvFile        string              `json:"pipeline_env_file,omitempty"`
	PipelineEnvKeys        []string            `json:"pipeline_env_keys,omitempty"`
	PipelineEnv            map[string]string   `json:"pipeline_env,omitempty"`
	ShellUpstreamContext   string              `json:"shell_upstream_context,omitempty"`
	Phase                  string              `json:"phase,omitempty"`
	Cockpit                bool                `json:"cockpit,omitempty"`
	CockpitSession         string              `json:"cockpit_session,omitempty"`
	CockpitPaneKey         string              `json:"cockpit_pane_key,omitempty"`
	SkipNativeReviewFanout bool                `json:"skip_native_review_fanout,omitempty"`
	ValidatedPullRequest   bool                `json:"validated_pull_request,omitempty"`
	Ephemeral              *EphemeralSpec      `json:"ephemeral,omitempty"`
	HumanAnswer            string              `json:"human_answer,omitempty"`
	// ThreadID / ChatMessageID back-link a chat-promoted job (#534) to its origin
	// message. Additive/omitempty: a non-chat job serializes byte-identically.
	ThreadID      string `json:"thread_id,omitempty"`
	ChatMessageID string `json:"chat_message_id,omitempty"`
	// MootSeat marks a `gitmoot moot` conversing seat (#732): the daemon elevates
	// ONLY these jobs (codex workspace-write+network to reach the relay socket) and
	// injects the relay env into them. Additive/omitempty so every non-seat job —
	// including chat-task promotions and continuations that carry ThreadID — is
	// byte-identical. Never inherited by delegation children or continuations.
	MootSeat bool `json:"moot_seat,omitempty"`
	// OrchestrateStage marks a #758 pipeline orchestrate stage job whose delegations[]
	// survive the pipeline-sender leaf strip (they fan out as children owned by this
	// stage job, whose own id is the sub-tree RootJobID). Set only from the validated
	// orchestrate spec, never by sender-sniffing. Additive/omitempty so every other
	// pipeline-sender payload — shell + #757 agent leaf — serializes byte-identically.
	OrchestrateStage bool         `json:"orchestrate_stage,omitempty"`
	WritablePaths    []string     `json:"writable_paths,omitempty"`
	ReadablePaths    []string     `json:"readable_paths,omitempty"`
	Network          bool         `json:"network,omitempty"`
	Check            string       `json:"check,omitempty"`
	CheckRetries     int          `json:"check_retries,omitempty"`
	RawOutputs       []string     `json:"raw_outputs,omitempty"`
	Result           *AgentResult `json:"result,omitempty"`
	// FailureDiagnostics captures process-level crash context when the runtime
	// session ended WITHOUT producing a gitmoot_result envelope (#806): a phase
	// marker (launched | streaming | result-parse), the exit code or signal, a
	// redacted stderr tail hard-capped at MaxStderrTailBytes, and the runtime
	// session id when one is known. Additive/omitempty — a job that produced an
	// envelope serializes byte-identically — and reset at the start of every run
	// so a retried job never carries a previous run's crash report.
	FailureDiagnostics *FailureDiagnostics `json:"failure_diagnostics,omitempty"`
	// ResultChecks, when non-empty, carries the deterministic binary-checklist
	// audit FAILURES for this job's parsed result (#526). It is the job-detail
	// surface the dashboard and `gitmoot job show --json` read. Fully additive
	// (omitempty): a job whose result passed every applicable check — and every
	// job when [workflow] result_checks = off — serializes byte-identically.
	ResultChecks []ResultCheck `json:"result_checks,omitempty"`
	// RiskTier is the resolved risk-tier of the change (#650), additive/omitempty
	// so a payload outside the opt-in risk-tiered review path serializes
	// byte-identically. It is stamped on a high-risk review coordinator and
	// inherited by its lens children for explainable escalation.
	RiskTier string `json:"risk_tier,omitempty"`
	// Operational-blocker deferral context (#532, additive — all omitempty, so a
	// job that never hit a classified blocker serializes byte-identically).
	// BlockerClass is the last classified blocker (e.g. "runtime_auth",
	// "runtime_quota"); BlockerAttempts counts classified deferrals over the
	// job's lifetime (hard-bounded by the daemon); BlockerRetryAt is the
	// RFC3339Nano earliest automatic re-dispatch time the queue gate honors.
	BlockerClass    string `json:"blocker_class,omitempty"`
	BlockerAttempts int    `json:"blocker_attempts,omitempty"`
	BlockerRetryAt  string `json:"blocker_retry_at,omitempty"`
	// BlockerSuggestedAction is a concrete human-facing remedy for a deferral that
	// usually needs manual intervention (a dirty/wrong-head checkout, #532 slice C).
	// It is surfaced through the #552 stuck surface (job list/show) so an operator
	// sees what condition must change; auth/quota/network deferrals leave it empty
	// because they clear on their own. Additive/omitempty (byte-identical when unset).
	BlockerSuggestedAction string `json:"blocker_suggested_action,omitempty"`
	// BlockerPreDelivery marks a deferral that happened BEFORE the agent was ever
	// delivered a prompt — currently only the daemon pre-flight checkout_contention
	// hold (#532 slice C), which trips at checkout validation before Mailbox.Run
	// claims the job and delivers. A pre-delivery deferral means the agent never
	// executed, so a re-dispatch is a FIRST run with NO prior side effects to
	// reconcile: the slice-F reconciliation notice must be suppressed for it. The
	// mid-delivery seam (deferOperationalBlockerPreTerminal) clears this flag so a
	// later runtime_auth/quota/network deferral still carries the at-least-once
	// side-effect warning. Additive/omitempty (byte-identical when unset).
	BlockerPreDelivery bool `json:"blocker_pre_delivery,omitempty"`
}

type DeliveryAdapter interface {
	Deliver(ctx context.Context, agent runtime.Agent, job runtime.Job) (runtime.Result, error)
}

func (m Mailbox) Enqueue(ctx context.Context, request JobRequest) (db.Job, error) {
	if m.Store == nil {
		return db.Job{}, errors.New("mailbox store is required")
	}
	if err := validateJobRequest(request); err != nil {
		return db.Job{}, err
	}

	snapshot, err := m.templateSnapshot(ctx, request.Agent)
	if err != nil {
		return db.Job{}, err
	}
	// A --recipe override swaps in the recipe template's content while leaving the
	// agent's identity untouched; the snapshot fields below then carry it.
	if request.TemplateOverride != nil {
		snapshot = *request.TemplateOverride
	}

	payload, err := marshalPayload(JobPayload{
		Repo:                   request.Repo,
		Branch:                 request.Branch,
		PullRequest:            request.PullRequest,
		HeadSHA:                request.HeadSHA,
		GoalID:                 request.GoalID,
		TaskID:                 request.TaskID,
		TaskTitle:              request.TaskTitle,
		LeadAgent:              request.LeadAgent,
		Reviewers:              compactStrings(request.Reviewers),
		ReviewRound:            request.ReviewRound,
		Sender:                 request.Sender,
		Instructions:           request.Instructions,
		Constraints:            compactStrings(request.Constraints),
		ParentJobID:            request.ParentJobID,
		DelegationID:           request.DelegationID,
		DelegationDepth:        request.DelegationDepth,
		DelegatedBy:            request.DelegatedBy,
		RootJobID:              request.RootJobID,
		Deps:                   compactStrings(request.Deps),
		JobTimeout:             strings.TrimSpace(request.JobTimeout),
		RetryCount:             request.RetryCount,
		Fingerprint:            strings.TrimSpace(request.Fingerprint),
		FailurePolicy:          strings.TrimSpace(request.FailurePolicy),
		SynthesisRule:          strings.TrimSpace(request.SynthesisRule),
		DelegationArtifactDir:  request.DelegationArtifactDir,
		WorktreePath:           request.WorktreePath,
		ReadOnlyWorktree:       request.ReadOnlyWorktree,
		TemplateID:             snapshot.ID,
		TemplateResolvedCommit: snapshot.ResolvedCommit,
		TemplateContent:        snapshot.Content,
		OriginalAgent:          request.OriginalAgent,
		DelegatedAgent:         request.DelegatedAgent,
		DelegationReason:       request.DelegationReason,
		RecentDelegationHashes: request.RecentDelegationHashes,
		DelegationRepeatCount:  request.DelegationRepeatCount,
		NonProgressStreak:      request.NonProgressStreak,
		LastProgressDigest:     request.LastProgressDigest,
		VerifyAttempt:          request.VerifyAttempt,
		DelegationFinalize:     request.DelegationFinalize,
		Model:                  request.Model,
		Effort:                 request.Effort,
		WorkflowID:             strings.TrimSpace(request.WorkflowID),
		RuntimeOverride:        strings.TrimSpace(request.RuntimeOverride),
		RuntimeOverrideRef:     strings.TrimSpace(request.RuntimeOverrideRef),
		ShellEnv:               append([]string(nil), request.ShellEnv...),
		PipelineName:           strings.TrimSpace(request.PipelineName),
		PipelineKeyAgent:       strings.TrimSpace(request.PipelineKeyAgent),
		PipelineKeyAccess:      compactPipelineKeyAccess(request.PipelineKeyAccess),
		PipelineEnvFile:        strings.TrimSpace(request.PipelineEnvFile),
		PipelineEnvKeys:        compactStrings(request.PipelineEnvKeys),
		PipelineEnv:            request.PipelineEnv,
		ShellUpstreamContext:   request.ShellUpstreamContext,
		Phase:                  request.Phase,
		Cockpit:                request.Cockpit,
		CockpitSession:         strings.TrimSpace(request.CockpitSession),
		CockpitPaneKey:         strings.TrimSpace(request.CockpitPaneKey),
		SkipNativeReviewFanout: request.SkipNativeReviewFanout,
		ValidatedPullRequest:   request.ValidatedPullRequest,
		Ephemeral:              request.Ephemeral,
		HumanAnswer:            request.HumanAnswer,
		RiskTier:               strings.TrimSpace(request.RiskTier),
		ThreadID:               strings.TrimSpace(request.ThreadID),
		ChatMessageID:          strings.TrimSpace(request.ChatMessageID),
		MootSeat:               request.MootSeat,
		OrchestrateStage:       request.OrchestrateStage,
		WritablePaths:          compactStrings(request.WritablePaths),
		ReadablePaths:          compactStrings(request.ReadablePaths),
		Network:                request.Network,
		Check:                  strings.TrimSpace(request.Check),
		CheckRetries:           request.CheckRetries,
	})
	if err != nil {
		return db.Job{}, err
	}

	job := db.Job{
		ID:              request.ID,
		Agent:           request.Agent,
		Type:            request.Action,
		State:           string(JobQueued),
		Payload:         payload,
		ParentJobID:     request.ParentJobID,
		DelegationID:    request.DelegationID,
		DelegationDepth: request.DelegationDepth,
		DelegatedBy:     request.DelegatedBy,
	}
	if err := m.Store.CreateJobWithEvent(ctx, job, db.JobEvent{JobID: job.ID, Kind: string(JobQueued), Message: "job queued"}); err != nil {
		return db.Job{}, err
	}
	return job, nil
}

func (m Mailbox) templateSnapshot(ctx context.Context, agentName string) (db.AgentTemplate, error) {
	agent, err := m.Store.GetAgent(ctx, agentName)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return db.AgentTemplate{}, nil
		}
		return db.AgentTemplate{}, err
	}
	if strings.TrimSpace(agent.TemplateID) == "" {
		return db.AgentTemplate{}, nil
	}
	template, err := m.Store.GetAgentTemplateReference(ctx, agent.TemplateID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return db.AgentTemplate{}, fmt.Errorf("agent %q references missing template %q", agent.Name, agent.TemplateID)
		}
		return db.AgentTemplate{}, err
	}
	// Canary routing (#484): a sampled fraction of resolutions that WOULD resolve the
	// champion (the default/current reference, NOT a pinned version) route to the
	// active canary version instead. The champion `template` resolved above is the
	// always-valid fallback: any miss, error, or no-canary case returns it unchanged,
	// so a mid-canary/half-promoted/missing canary or concurrent resolve can NEVER
	// return no-template or a broken version. Off-by-default: with no canary row the
	// lookup is one indexed miss, no rng is drawn, and the result is byte-identical.
	if canary, ok := m.routeCanary(ctx, agent.TemplateID, template); ok {
		return canary, nil
	}
	return template, nil
}

// routeCanary decides, for one resolution, whether to swap the resolved champion
// snapshot for the template's active canary version (#484). It returns ok=false
// (resolve the champion) in EVERY uncertain case — a pinned version reference, no
// active canary, an out-of-range sample, a draw at/above the sample, or any store
// error — so the champion (already resolved by the caller) is the guaranteed valid
// fallback and resolution is never broken. The random draw is taken ONLY when an
// active canary actually exists, keeping the no-canary path byte-identical.
func (m Mailbox) routeCanary(ctx context.Context, agentTemplateRef string, champion db.AgentTemplate) (db.AgentTemplate, bool) {
	// Config gate (#484): routing is gated on the SAME policy the daemon's
	// regression comparator uses, so the two seams are consistent. When the knob is
	// off this returns before the GetActiveCanaryVersion query, so no rng is drawn,
	// no traffic is sampled, and the path is byte-identical — and a canary row left
	// behind by a since-disabled run can never keep serving traffic with no
	// auto-rollback.
	if !m.CanaryEnabled {
		return db.AgentTemplate{}, false
	}
	templateID, versionRef := db.SplitAgentTemplateReference(agentTemplateRef)
	// Only the default/current resolution participates in the canary; an explicit
	// version pin (latest / @vN) is honored verbatim.
	if versionRef != "" && versionRef != "current" {
		return db.AgentTemplate{}, false
	}
	canary, found, err := m.Store.GetActiveCanaryVersion(ctx, templateID)
	if err != nil || !found {
		return db.AgentTemplate{}, false
	}
	sample := canary.CanarySample
	if sample <= 0 || sample > 1 {
		return db.AgentTemplate{}, false
	}
	if m.canaryDraw() >= sample {
		return db.AgentTemplate{}, false
	}
	// Resolve the canary version's snapshot through the EXISTING reference path
	// (canary.ID is "templateID@vN"), so its distinct ResolvedCommit/Content flow
	// into the payload unchanged and the #465 harvester attributes the outcome to
	// the canary version. A resolve error falls back to the champion (never break).
	snap, err := m.Store.GetAgentTemplateReference(ctx, canary.ID)
	if err != nil || strings.TrimSpace(snap.VersionID) == "" {
		return db.AgentTemplate{}, false
	}
	return snap, true
}

// canaryDraw returns a per-resolution random draw in [0,1) for canary routing,
// using the injected CanaryRand when set (deterministic tests) and the
// concurrency-safe global rand.Float64 otherwise.
func (m Mailbox) canaryDraw() float64 {
	if m.CanaryRand != nil {
		return m.CanaryRand()
	}
	return rand.Float64()
}

// DeliveryError marks an error that provably originated from the runtime
// DELIVERY seam — adapter.Deliver itself failed (runtime CLI exited nonzero,
// provider rejected the request) — as opposed to an error ABOUT the agent's
// delivered output (gitmoot_result contract/validation failures, which are
// product failures the agent authored). The daemon's operational-blocker
// classifier (#532) requires this marker before doing ANY string matching, so
// agent-authored text that merely mentions "quota"/"rate limit" (a summary, a
// delegation id such as "audit-quota-enforcement") can never be misclassified
// as a retryable operational blocker. Error() is transparent so every existing
// string-based consumer of a delivery failure stays byte-identical.
type DeliveryError struct{ Err error }

func (e DeliveryError) Error() string { return e.Err.Error() }
func (e DeliveryError) Unwrap() error { return e.Err }

// ErrJobDeferred is reported by Mailbox.Run (and Engine.RunJob) when a
// delivery-seam failure was classified as an operational blocker and the injected
// deferBlocker re-queued the job PRE-terminally (#532 slice E): the job is now
// JobQueued with a hold and a job.deferred event/emit, and NO job.failed was
// emitted. The daemon recognizes it via errors.Is to short-circuit the terminal
// failure path (no handleRunJobError, no failure comment) — the run already
// resolved to a first-class deferral. The wrapped cause stays inspectable
// (errors.As DeliveryError) for logging.
var ErrJobDeferred = errors.New("operational blocker: job deferred pre-terminally")

type deferredError struct{ cause error }

func (e deferredError) Error() string { return e.cause.Error() }

func (e deferredError) Unwrap() []error { return []error{e.cause, ErrJobDeferred} }

// tryDeferBlocker consults the injected pre-terminal deferrer, if any, for a
// delivery-seam failure. It returns (true, nil) exactly when the job was
// re-queued behind an operational blocker (so Run must skip m.fail); every other
// case — no deferrer wired, not classifiable, ineligible, budget spent, or a
// deferral error — returns false so Run takes its existing terminal path.
func (m Mailbox) tryDeferBlocker(ctx context.Context, jobID string, cause error) bool {
	if m.deferBlocker == nil {
		return false
	}
	deferred, err := m.deferBlocker(ctx, jobID, cause)
	return err == nil && deferred
}

// blockerRetryReconciliationNotice is prepended to the prompt of a job the
// daemon re-dispatches after an operational-blocker deferral (#532). It composes
// two concerns onto the retry prompt (the InjectUpstreamDepContext prompt-prefix
// pattern):
//
//   - WARN level (#532 slice F): tell the agent the previous attempt died
//     OPERATIONALLY — a runtime auth/quota/network/checkout condition in the
//     environment — and NOT on the merits of its own output, so it does not
//     second-guess an approach that was actually sound and get pulled off course.
//   - AT-LEAST-ONCE side effects (#602's side-effect gate): a blocker that hit
//     mid-turn may have interrupted an attempt that already pushed branches,
//     opened PRs, or posted comments, and (for ephemeral/fresh-session workers)
//     the retried run has no memory of it — so tell the agent to reconcile
//     instead of duplicating.
//
// attempt is the deferral count (payload.BlockerAttempts) so the agent sees this
// is a bounded operational retry, not an open-ended loop.
func blockerRetryReconciliationNotice(class string, attempt int) string {
	return fmt.Sprintf("NOTE (operational retry, attempt %d): the PREVIOUS attempt of this exact job did NOT fail on the merits of your work — "+
		"it was interrupted by an operational blocker (%s) in the environment (a runtime auth/quota/network/checkout condition), "+
		"not by anything wrong in your own output. The daemon automatically re-dispatched it now that the condition cleared, so treat "+
		"your prior approach as still valid rather than a product failure. That interrupted attempt may already have performed side "+
		"effects — pushed branches, opened pull requests, posted comments, or made commits — so before doing any work, check for those "+
		"artifacts and reuse/reconcile them instead of redoing or duplicating the work.", attempt, class)
}

func (m Mailbox) Run(ctx context.Context, jobID string, agent runtime.Agent, adapter DeliveryAdapter) (AgentResult, error) {
	if m.Store == nil {
		return AgentResult{}, errors.New("mailbox store is required")
	}
	if adapter == nil {
		return AgentResult{}, errors.New("delivery adapter is required")
	}

	job, err := m.Store.GetJob(ctx, jobID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return AgentResult{}, fmt.Errorf("job %q not found", jobID)
		}
		return AgentResult{}, err
	}
	if job.Agent != agent.Name {
		return AgentResult{}, fmt.Errorf("job %q is assigned to %q, not %q", job.ID, job.Agent, agent.Name)
	}
	payload, err := unmarshalPayload(job.Payload)
	if err != nil {
		return AgentResult{}, err
	}
	// A retried job must never carry a previous run's crash report (#806):
	// reset before this run's deliveries so every payload persist below — a
	// success terminal included — writes fresh diagnostics state.
	payload.FailureDiagnostics = nil

	if err := m.claim(ctx, job); err != nil {
		return AgentResult{}, err
	}
	// Execution clock for #530 routing telemetry: measured from the claim (the real
	// start of this run) to the terminal transition below. Advisory only.
	runStart := time.Now()

	registeredFreshRef := runtime.IsFreshRef(agent.RuntimeRef)
	// #33 preset delivery mode: decide whether the resumed session already holds
	// this exact preset and may receive the short reference instead of the full
	// body. For a `full` agent (the default) this returns false without touching
	// the store, so the assembled prompt is byte-identical. The snapshot in the
	// payload (id/commit/content) is unchanged regardless — auditability/retry are
	// preserved.
	referenceUsed := m.usePresetReference(ctx, agent, payload)
	jobPrompt := payload.prompt(job.Type)
	jobPrompt.TemplateReferenceOnly = referenceUsed
	prompt := prompts.RenderJob(jobPrompt)
	// Append the off-by-default "Prior learnings" memory block (#626 READ path).
	// When the memory hook is unset (every non-enrolled path) or returns "" (no
	// enrolled agent, empty sanitized query, or no confirmed match), the prompt is
	// byte-identical.
	if m.injectMemory != nil {
		if block := m.injectMemory(ctx, agent, payload); block != "" {
			prompt = prompt + "\n\n" + block
		}
	}
	// Append the off-by-default #530 coordinator routing-context block. Gated on the
	// [router] context_enabled knob (routerContextEnabled) AND on the job being a
	// top-level coordinator (no parent) — a delegation child inherits its
	// coordinator's routing decision, so it needs no table. When the knob is off no
	// telemetry query runs and the prompt is byte-identical; when on-but-empty the
	// builder returns "" and the prompt is still byte-identical.
	if m.routerContextEnabled && strings.TrimSpace(payload.ParentJobID) == "" {
		if block := m.buildRouterContextBlock(ctx, payload); block != "" {
			prompt = prompt + "\n\n" + block
		}
	}
	if payload.BlockerAttempts > 0 && strings.TrimSpace(payload.BlockerClass) != "" && !payload.BlockerPreDelivery {
		// This run is an automatic operational-blocker retry (#532): the previous
		// attempt died on an environment condition, not on its own output, and may
		// have executed side effects before the blocker hit — so warn the agent its
		// prior approach was sound and ask it to reconcile prior artifacts rather than
		// duplicate them (slice F). Suppressed for a PRE-DELIVERY deferral
		// (BlockerPreDelivery, e.g. a checkout_contention hold): that deferral tripped
		// at the daemon pre-flight before any delivery, so the agent never executed and
		// there are zero prior artifacts to reconcile — this is its first real run.
		prompt = blockerRetryReconciliationNotice(payload.BlockerClass, payload.BlockerAttempts) + "\n\n" + prompt
	}
	firstRaw, firstRefreshedRef, firstEphemeral, firstDiag, firstErr := m.deliver(ctx, adapter, agent, job, payload, prompt)
	if firstErr != nil {
		deliveryErr := DeliveryError{Err: firstErr}
		// PRE-TERMINAL operational-blocker classification (#532 slice E): consult the
		// injected deferrer BEFORE failing. If it re-queues the job (running->queued +
		// job.deferred), do NOT call m.fail — no job.failed reaches the [events] sink
		// first. At the FIRST delivery no RawOutputs are persisted yet, so an eligible
		// job (no result, not a child, budget left) defers here; the deferrer's own
		// guards refuse everything else and this falls through to the terminal path.
		if m.tryDeferBlocker(ctx, job.ID, deliveryErr) {
			return AgentResult{}, deferredError{cause: deliveryErr}
		}
		// Persist crash diagnostics (#806) before the terminal fail so `job show`
		// and `report bug` can explain a session that died without an envelope.
		// Best-effort: diagnostics must never change the failure path.
		m.storeFailureDiagnostics(ctx, job.ID, &payload, firstDiag)
		_ = m.fail(ctx, job.ID, fmt.Sprintf("delivery failed: %v", firstErr))
		// Record an advisory failure observation (#530) so a runtime/model that
		// repeatedly crashes at the ADAPTER level (never reaching a parsed decision)
		// still counts against its success rate — otherwise it contributes zero rows
		// and its recorded SuccessRate stays artificially high. Only reached on the
		// TERMINAL m.fail path; the tryDeferBlocker re-queue above returns first.
		m.recordRoutingTelemetry(ctx, job, agent, payload, AgentResult{}, JobFailed, time.Since(runStart))
		// Typed DeliveryError: this error is FROM the delivery seam (not about the
		// agent's output), the precondition for #532 operational classification.
		return AgentResult{}, deliveryErr
	}
	// SESSION SAFETY (#531): a job running under a per-job runtime override must
	// never write back to the agent's stored resume state — the stored ref
	// belongs to the DEFAULT runtime, and persisting an override-runtime ref
	// there would corrupt the agent's session identity. An ephemeral by-design
	// session (deliverFresh: the #531 fresh-ref path and the last+template
	// coordinator path, #665) is likewise never persisted — persisting the minted
	// UUID would rewrite a "last"/fresh registration and end the per-job isolation
	// after job 1. Both refs are still adopted in-memory below so repair retries
	// stay coherent. Only a genuine #443 dead-session self-heal re-pin persists.
	if payload.RuntimeOverride == "" && !registeredFreshRef && !firstEphemeral {
		m.persistRefreshedRuntimeRef(ctx, job.ID, agent, firstRefreshedRef)
	}
	// If the first delivery self-healed a dead session (#443), adopt the freshly
	// minted ref in-memory so a subsequent repair retry resumes the new session
	// rather than re-resuming the dead UUID (which would self-heal a second time
	// and orphan the first healed session).
	if firstRefreshedRef != "" {
		agent.RuntimeRef = firstRefreshedRef
	}
	// #33: after a successful FULL preset delivery, record that the resumed session
	// now holds this preset so a later referenced/auto delivery can send the short
	// reference. agent.RuntimeRef here is the effective session the next job will
	// resume (the refreshed ref was adopted above). No-op for a `full` agent, when
	// a reference was already used, or for a non-concrete session — so the default
	// path writes nothing.
	m.recordPresetSessionState(ctx, agent, payload, agent.RuntimeRef, referenceUsed)
	payload.RawOutputs = append(payload.RawOutputs, firstRaw)

	result, parseErr := ExtractAgentResult(firstRaw)
	if parseErr != nil {
		// The agent may have delivered useful work but omitted the required
		// gitmoot_result envelope. Re-ask with the repair prompt up to
		// maxRepairAttempts times to salvage a recoverable missing envelope before
		// failing terminally (#495). Persist the malformed output and emit the
		// malformed_output event once, preserving the existing event semantics, then
		// loop the (previously single) repair re-ask.
		if err := m.savePayload(ctx, job.ID, payload); err != nil {
			return AgentResult{}, err
		}
		if err := m.addEvent(ctx, job.ID, "malformed_output", parseErr.Error()); err != nil {
			return AgentResult{}, err
		}

		lastRaw := firstRaw
		lastDiag := firstDiag
		for attempt := 1; attempt <= maxRepairAttempts; attempt++ {
			// Re-check cancellation before each repair delivery so a job cancelled
			// during the repair window is not re-asked (preserves the cancellation
			// guards).
			if err := m.ensureRunning(ctx, job.ID); err != nil {
				return AgentResult{}, err
			}
			if err := m.addEvent(ctx, job.ID, "repair_retry", fmt.Sprintf("retrying with repair prompt (attempt %d of %d)", attempt, maxRepairAttempts)); err != nil {
				return AgentResult{}, err
			}

			repairPrompt := prompts.RenderRepairPrompt(lastRaw, parseErr)
			repairRaw, repairRefreshedRef, repairEphemeral, repairDiag, repairErr := m.deliver(ctx, adapter, agent, job, payload, repairPrompt)
			if repairErr != nil {
				deliveryErr := DeliveryError{Err: repairErr}
				// Same pre-terminal seam as the first delivery, but the deferrer's
				// duplicate-side-effect guard REFUSES this job: the persisted RawOutputs
				// from the completed first delivery prove side-effectful execution, so it
				// returns false and this takes the terminal path (no flap to avoid — the
				// job genuinely fails). Consulting it here keeps a single seam.
				if m.tryDeferBlocker(ctx, job.ID, deliveryErr) {
					return AgentResult{}, deferredError{cause: deliveryErr}
				}
				// Crash diagnostics (#806) for the repair delivery that died, mirroring
				// the first-delivery terminal above. Best-effort.
				m.storeFailureDiagnostics(ctx, job.ID, &payload, repairDiag)
				_ = m.fail(ctx, job.ID, fmt.Sprintf("repair delivery failed: %v", repairErr))
				// Advisory failure observation (#530): a hard adapter error during repair
				// is still a runtime-level failure — count it so flaky runtimes are not
				// over-recommended by the coordinator context block.
				m.recordRoutingTelemetry(ctx, job, agent, payload, AgentResult{}, JobFailed, time.Since(runStart))
				return AgentResult{}, deliveryErr
			}
			// Same #531 override + #665 ephemeral guard as the first delivery: never
			// persist an override-runtime ref or a by-design per-job session onto the
			// agent's default-runtime resume state.
			if payload.RuntimeOverride == "" && !registeredFreshRef && !repairEphemeral {
				m.persistRefreshedRuntimeRef(ctx, job.ID, agent, repairRefreshedRef)
			}
			// Adopt a freshly minted session ref so the next repair re-ask resumes the
			// new session rather than re-resuming a dead UUID (mirrors the first
			// delivery's self-heal adoption above).
			if repairRefreshedRef != "" {
				agent.RuntimeRef = repairRefreshedRef
			}
			payload.RawOutputs = append(payload.RawOutputs, repairRaw)
			lastRaw = repairRaw
			lastDiag = repairDiag

			result, parseErr = ExtractAgentResult(repairRaw)
			if parseErr == nil {
				break
			}
			if err := m.savePayload(ctx, job.ID, payload); err != nil {
				return AgentResult{}, err
			}
		}

		if parseErr != nil {
			// The session completed every delivery but never produced a valid
			// envelope: record result-parse phase diagnostics (#806). The last
			// delivery's process evidence (exit code 0, its stderr) is what
			// explains the miss; synthesize a phase-only record when the adapter
			// carried none (a delivery that succeeded proves a session ran).
			if lastDiag == nil {
				lastDiag = &FailureDiagnostics{}
			}
			lastDiag.Phase = FailurePhaseResultParse
			m.storeFailureDiagnostics(ctx, job.ID, &payload, lastDiag)
			_ = m.fail(ctx, job.ID, fmt.Sprintf("repair output malformed: %v", parseErr))
			// Advisory failure observation (#530): output that stays malformed after all
			// repair attempts is a runtime/model failure to honor the contract — count it
			// so it lowers the recorded success rate instead of being invisible.
			m.recordRoutingTelemetry(ctx, job, agent, payload, AgentResult{}, JobFailed, time.Since(runStart))
			return AgentResult{}, parseErr
		}
	}

	// A produce stage may declare a trusted deterministic check. Run it only after
	// a valid envelope and before terminal persistence. Failures re-deliver to the
	// SAME in-memory runtime session; m.deliver records every correction turn's
	// token usage on the same job row.
	if job.Type == "produce" && strings.TrimSpace(payload.Check) != "" {
		for correction := 0; ; correction++ {
			checkOutput, checkErr := runProduceCheck(ctx, payload, m.produceCheckDir, m.produceCheckTimeout)
			if checkErr == nil {
				break
			}
			if correction >= payload.CheckRetries {
				message := fmt.Sprintf("produce check failed after %d correction attempt(s): %v", correction, checkErr)
				if strings.TrimSpace(checkOutput) != "" {
					message += ": " + checkOutput
				}
				payload.Result = &AgentResult{Decision: "failed", Summary: message}
				if err := m.savePayload(ctx, job.ID, payload); err != nil {
					return AgentResult{}, err
				}
				_ = m.addEvent(ctx, job.ID, "produce_check_failed", message)
				if err := m.fail(ctx, job.ID, message); err != nil {
					return AgentResult{}, err
				}
				return AgentResult{}, errors.New(message)
			}
			if err := m.ensureRunning(ctx, job.ID); err != nil {
				return AgentResult{}, err
			}
			if err := m.addEvent(ctx, job.ID, "produce_check_retry", fmt.Sprintf("retrying after deterministic check failure (attempt %d of %d)", correction+1, payload.CheckRetries)); err != nil {
				return AgentResult{}, err
			}
			correctionRaw, refreshedRef, ephemeral, diag, deliveryErr := m.deliver(ctx, adapter, agent, job, payload, produceCheckCorrectionPrompt(checkOutput))
			if deliveryErr != nil {
				m.storeFailureDiagnostics(ctx, job.ID, &payload, diag)
				_ = m.fail(ctx, job.ID, fmt.Sprintf("produce correction delivery failed: %v", deliveryErr))
				return AgentResult{}, DeliveryError{Err: deliveryErr}
			}
			if payload.RuntimeOverride == "" && !registeredFreshRef && !ephemeral {
				m.persistRefreshedRuntimeRef(ctx, job.ID, agent, refreshedRef)
			}
			if refreshedRef != "" {
				agent.RuntimeRef = refreshedRef
			}
			payload.RawOutputs = append(payload.RawOutputs, correctionRaw)
			result, parseErr = ExtractAgentResult(correctionRaw)
			if parseErr != nil {
				message := fmt.Sprintf("produce correction output malformed: %v", parseErr)
				payload.Result = &AgentResult{Decision: "failed", Summary: message}
				if err := m.savePayload(ctx, job.ID, payload); err != nil {
					return AgentResult{}, err
				}
				_ = m.fail(ctx, job.ID, message)
				return AgentResult{}, parseErr
			}
			if err := m.savePayload(ctx, job.ID, payload); err != nil {
				return AgentResult{}, err
			}
		}
	}

	if err := m.ensureRunning(ctx, job.ID); err != nil {
		return AgentResult{}, err
	}
	// A #681 pipeline stage is a LEAF: it runs a shell command and returns a
	// decision. Its ParentJobID is empty, so any delegations[] the stage command
	// emitted would otherwise dispatch as TOP-LEVEL jobs in AdvanceJob. Strip them
	// for a pipeline-sender job so a stage can never spawn phantom children; the
	// advancer already ignores stage delegations, and this makes stages-as-leaves
	// hold at the engine seam too. Cheap and byte-identical for every other sender.
	if payload.Sender == PipelineJobSender {
		// #758: an orchestrate stage IS a bounded sub-tree ROOT — it is explicitly
		// AUTHORIZED to fan out, so its delegations[] survive here and the engine's
		// dispatchDelegations gives every child ParentJobID = this stage job (owned,
		// not orphaned). The relaxation is gated STRICTLY on the OrchestrateStage
		// payload flag, which the pipeline dispatch sets only from the validated
		// orchestrate:true spec — NEVER by sender-sniffing — so every other
		// pipeline-sender job (shell + #757 agent leaf) keeps the delegations strip
		// byte-identically and can never spawn phantom children.
		if !payload.OrchestrateStage {
			result.Delegations = nil
		}
		// Same leaf enforcement for human_questions[] (#757): a healthy agent-stage
		// result with an empty ParentJobID would otherwise drive the TOP-LEVEL
		// ask-gate (AdvanceJob, engine.go), opening an escalation / needs-attention
		// round on a stage job that the pipeline can never resolve (job_recovery
		// refuses to gate-resume a pipeline-sender job) while the advancer silently
		// folds the stage on its decision and proceeds — the pause intent is
		// dropped and a dangling round is left behind. A leaf stage can no more
		// pause a human than it can spawn a child; strip it so the stage folds
		// purely on its decision (a stage that must halt returns decision
		// "blocked", which the advancer parks the whole run on with needs).
		//
		// This strip stays unconditional for EVERY pipeline-sender stage job — the
		// #758 orchestrate coordinator included: the coordinator's own ParentJobID is
		// empty, so its human_questions would hit the same unresolvable top-level
		// ask-gate. Per the #758 design a CHILD's ask-gate is what pauses the tree,
		// and a child is a non-pipeline-sender job (Sender = the coordinator agent,
		// ParentJobID = this stage job), so this strip never touches it.
		result.HumanQuestions = nil
	}
	payload.Result = &result
	// #526 deterministic binary-checklist audit of the parsed result. Off (the
	// zero value and "off") => no checks run, no event, no payload field, no feed-
	// forward row, so the terminal path is byte-identical. warn => failures are
	// recorded (job event + job-detail field + feed-forward row) but the job still
	// finishes on its own decision. block => a failure additionally routes the job
	// down the same terminal path a malformed result takes (m.fail), reusing the
	// contract-violation machinery. The audit itself is pure and LLM-free.
	if mode := normalizeResultCheckMode(m.resultCheckMode); mode != ResultChecksOff {
		if failed := FailedResultChecks(ResultCheckInput{Action: job.Type, IsFinalize: payload.DelegationFinalize, Result: result}); len(failed) > 0 {
			payload.ResultChecks = failed
			summary := SummarizeResultChecks(failed)
			// Surface in `gitmoot job events`/`job show`. Best-effort in block mode
			// (the fail path below is authoritative); required in warn mode so the
			// failure is visible, hence the error is returned there.
			if err := m.addEvent(ctx, job.ID, ResultChecksFailedEventKind, summary); err != nil && mode != ResultChecksBlock {
				return AgentResult{}, err
			}
			// Feed-forward stub for SkillOpt (#526): persist the failures so the
			// optimizer can later consume them as structured feedback. Best-effort —
			// it must never fail the job, and it does nothing tonight beyond storing.
			rootID := strings.TrimSpace(payload.RootJobID)
			if rootID == "" {
				rootID = job.ID
			}
			_ = m.Store.RecordResultCheckFailures(ctx, job.ID, rootID, job.Type, toDBResultCheckFailures(failed))
			if mode == ResultChecksBlock {
				// Persist the payload (carrying ResultChecks + Result) BEFORE failing so
				// the job-detail surface shows exactly which checks failed, then map the
				// audit failure onto the terminal contract-violation path via m.fail.
				if err := m.savePayload(ctx, job.ID, payload); err != nil {
					return AgentResult{}, err
				}
				if err := m.fail(ctx, job.ID, "result checks failed: "+summary); err != nil {
					return AgentResult{}, err
				}
				return AgentResult{}, &ResultChecksError{Failed: failed}
			}
		}
	}
	// Shadow-log returned learnings + write any mechanical fact at job terminal
	// (#626 WRITE path). No-op when the hook is unset or the agent is not enrolled,
	// so the terminal path is byte-identical. Best-effort — it never fails the job.
	if m.recordMemory != nil {
		m.recordMemory(ctx, job.ID, agent, job.Type, payload, result)
	}
	state := stateForDecision(result.Decision)
	if err := m.finishWithPayload(ctx, job.ID, state, fmt.Sprintf("job %s", state), payload); err != nil {
		return AgentResult{}, err
	}
	// Record advisory routing telemetry (#530) at the settled terminal state.
	// Best-effort and fail-safe: it swallows every error internally, so a telemetry
	// write can never turn a successfully-finished job into a failure.
	m.recordRoutingTelemetry(ctx, job, agent, payload, result, state, time.Since(runStart))
	return result, nil
}

const maxProduceCheckOutputBytes = 8 * 1024
const defaultProduceCheckTimeout = 2 * time.Minute

func runProduceCheck(ctx context.Context, payload JobPayload, fallbackDir string, timeout time.Duration) (string, error) {
	dir := strings.TrimSpace(payload.WorktreePath)
	if dir == "" {
		dir = strings.TrimSpace(fallbackDir)
	}
	if timeout <= 0 {
		timeout = defaultProduceCheckTimeout
	}
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	result, err := (subprocess.GroupRunner{MaxOutputBytes: 2 * maxProduceCheckOutputBytes}).Run(checkCtx, dir, "sh", "-c", payload.Check)
	return sanitizeProduceCheckOutput(result.Stdout + result.Stderr), err
}

func sanitizeProduceCheckOutput(output string) string {
	return strings.TrimSpace(tailBytes(RedactCommentText(output), maxProduceCheckOutputBytes))
}

func produceCheckCorrectionPrompt(output string) string {
	if strings.TrimSpace(output) == "" {
		output = "(check exited non-zero with no output)"
	}
	return "Your produce output failed the deterministic check. Fix and return a fresh gitmoot_result. Check output:\n\n~~~text\n" + output + "\n~~~"
}

// deliver returns the delivery's raw text, refreshed runtime ref, ephemeral
// flag, redacted crash-diagnostics snapshot (#806, nil when no CLI process
// ran), and error.
func (m Mailbox) deliver(ctx context.Context, adapter DeliveryAdapter, agent runtime.Agent, job db.Job, payload JobPayload, prompt string) (string, string, bool, *FailureDiagnostics, error) {
	delivery := runtime.Job{
		ID:                   job.ID,
		AgentName:            agent.Name,
		Action:               job.Type,
		Prompt:               prompt,
		Repository:           payload.Repo,
		PullRequest:          payload.PullRequest,
		Model:                payload.Model,
		Effort:               payload.Effort,
		ShellEnv:             append([]string(nil), payload.ShellEnv...),
		ShellUpstreamContext: payload.ShellUpstreamContext,
	}
	// #652: thread in the runtime's configured registry default_model as the FINAL
	// model fallback. effectiveModel applies the precedence (job.Model > agent.Model
	// > this default), so an agent/job --model always wins; a nil hook or empty
	// default forces nothing and is byte-identical. Resolved for the agent's
	// EFFECTIVE runtime, so a #531 per-job runtime override picks up the override
	// runtime's default.
	if m.RuntimeDefaultModel != nil {
		delivery.RuntimeDefaultModel = m.RuntimeDefaultModel(agent.Runtime)
	}
	if m.RuntimeDefaultEffort != nil {
		delivery.RuntimeDefaultEffort = m.RuntimeDefaultEffort(agent.Runtime)
	}
	result, err := adapter.Deliver(ctx, agent, delivery)
	// Record best-effort runtime token usage so the per-root delegation token
	// budget (#338 Part B) can sum a tree's cost. Usage accounting must never fail
	// a delivery, so every write error here is swallowed.
	if err == nil {
		inTok, outTok := result.InputTokens, result.OutputTokens
		// codex reports SESSION-CUMULATIVE counts on a resumed thread (#661): the
		// session's whole running total, not this job's usage. When the adapter flags
		// them cumulative, convert to this job's per-session delta keyed by the
		// runtime session (runtime+ref) before persisting. Only a concrete, stable
		// ref names a session we can key on; an empty or "last" ref does not, so —
		// as today — it contributes 0 rather than being mis-attributed. A false flag
		// (fresh/single-use, and every non-codex runtime) skips the delta table
		// entirely and records the count verbatim (#664).
		if result.CumulativeUsage && agent.RuntimeRef != "" && agent.RuntimeRef != runtime.LastRef {
			inTok, outTok, _ = m.Store.RecordRuntimeSessionUsageDelta(ctx, agent.Runtime+":"+agent.RuntimeRef, result.InputTokens, result.OutputTokens)
		} else if result.CumulativeUsage {
			inTok, outTok = 0, 0
		} else if agent.Runtime == runtime.CodexRuntime && result.RefreshedRuntimeRef != "" && result.RefreshedRuntimeRef != runtime.LastRef {
			// A fresh codex delivery reports PER-JOB (non-cumulative) usage and skips the
			// delta table by design (#664), so the runtime_session_usage baseline for its
			// thread is never seeded. But a fresh codex delivery ALSO adopts its concrete
			// thread in-memory for same-job repair (#665): if turn 1 is malformed, the
			// repair delivery resumes that thread, reports SESSION-CUMULATIVE usage, and
			// (turn1+turn2) would delta against a ZERO baseline — re-counting turn 1 on
			// top of the verbatim record below (#669 interaction). SEED the baseline with
			// turn 1's counts, keyed byte-identically to the key the repair path computes
			// from the adopted ref (runtime+RefreshedRuntimeRef), so a repair delta =
			// (turn1+turn2)-turn1 = turn2. The returned delta is discarded; the verbatim
			// UpdateJobUsage below records this turn. Errors are swallowed like the delta
			// call — usage accounting never fails a delivery.
			_, _, _ = m.Store.RecordRuntimeSessionUsageDelta(ctx, agent.Runtime+":"+result.RefreshedRuntimeRef, result.InputTokens, result.OutputTokens)
		}
		// Only persist when a positive count remains so runtimes that report nothing
		// (e.g. the shell runtime) or a delta that resolved to 0 leave the columns at
		// their 0 default rather than taking a no-op write.
		if inTok > 0 || outTok > 0 {
			_ = m.Store.UpdateJobUsage(ctx, job.ID, inTok, outTok)
		}
	}
	diag := failureDiagnosticsFromSession(result.SessionDiag)
	if strings.TrimSpace(result.Summary) != "" {
		return result.Summary, result.RefreshedRuntimeRef, result.SessionEphemeral, diag, err
	}
	return result.Raw, result.RefreshedRuntimeRef, result.SessionEphemeral, diag, err
}

// storeFailureDiagnostics persists crash diagnostics (#806) onto the job
// payload just before a no-envelope terminal fail. Best-effort by design: a
// nil diag (no CLI process ran) stores nothing, and a persist error never
// changes the failure path.
func (m Mailbox) storeFailureDiagnostics(ctx context.Context, jobID string, payload *JobPayload, diag *FailureDiagnostics) {
	if diag == nil {
		return
	}
	payload.FailureDiagnostics = diag
	_ = m.savePayload(ctx, jobID, *payload)
}

// persistRefreshedRuntimeRef re-pins an agent that self-healed a dead session
// (#443). It is best-effort and fail-open: a ref-write failure must never fail an
// otherwise-successful job, mirroring the usage-write swallow in deliver. It
// emits a session_refresh_retry event alongside the repair_retry pattern so the
// self-heal is observable.
func (m Mailbox) persistRefreshedRuntimeRef(ctx context.Context, jobID string, agent runtime.Agent, refreshedRef string) {
	if refreshedRef == "" || refreshedRef == agent.RuntimeRef {
		return
	}
	_ = m.Store.UpdateAgentRuntimeRef(ctx, agent.Name, refreshedRef)
	_ = m.addEvent(ctx, jobID, "session_refresh_retry", fmt.Sprintf("re-pinned agent %q to fresh runtime session after dead-session self-heal", agent.Name))
}

func (m Mailbox) finish(ctx context.Context, jobID string, state JobState, message string) error {
	transitioned, err := m.Store.TransitionJobStateWithEvent(ctx, jobID, string(JobRunning), string(state), db.JobEvent{
		JobID:   jobID,
		Kind:    string(state),
		Message: message,
	})
	if err != nil {
		return err
	}
	if !transitioned {
		latest, getErr := m.Store.GetJob(ctx, jobID)
		if getErr != nil {
			return getErr
		}
		return fmt.Errorf("job %q is %s, not running", jobID, latest.State)
	}
	// Best-effort outbound emit on the genuine running->terminal transition, wired
	// symmetrically with finishWithPayload (#446). m.fail (delivery failure,
	// malformed-output-after-repair) reaches a terminal state through THIS path,
	// not finishWithPayload, so without this the most common runtime failure mode
	// silently never emits job.failed/job.blocked. finish has no payload arg, so
	// load the stored one for full root_id/repo; when it carries no Result (the
	// usual delivery-failure case) synthesize a transient one from the transition
	// message so detail is a meaningful, redacted failure summary. Gated on
	// transitioned==true (fires exactly once) and nil-safe (no EventSink => no-op,
	// byte-identical). The load failure degrades gracefully to an id-rooted emit
	// rather than dropping the event or failing the finish.
	if m.emitTerminal != nil {
		payload := m.loadTerminalEmitPayload(ctx, jobID, message)
		m.emitTerminal(ctx, jobID, state, payload)
	}
	return nil
}

// loadTerminalEmitPayload loads the stored payload for a finish-path terminal
// emit, ensuring a non-nil Result so the emit detail carries a failure summary.
// A delivery/parse failure transitions via finish without a stored Result; the
// transition message (e.g. "delivery failed: ...") is the only failure context,
// so synthesize a transient Result from it (in-memory only — never persisted).
// On any load error it returns a minimal payload so the emit still fires.
func (m Mailbox) loadTerminalEmitPayload(ctx context.Context, jobID, message string) JobPayload {
	job, err := m.Store.GetJob(ctx, jobID)
	if err != nil {
		return JobPayload{Result: &AgentResult{Summary: strings.TrimSpace(message)}}
	}
	payload, err := unmarshalPayload(job.Payload)
	if err != nil {
		return JobPayload{Result: &AgentResult{Summary: strings.TrimSpace(message)}}
	}
	if payload.Result == nil {
		payload.Result = &AgentResult{Summary: strings.TrimSpace(message)}
	}
	return payload
}

func (m Mailbox) finishWithPayload(ctx context.Context, jobID string, state JobState, message string, payload JobPayload) error {
	encoded, err := marshalPayload(payload)
	if err != nil {
		return err
	}
	transitioned, err := m.Store.TransitionJobStatePayloadWithEvent(ctx, jobID, string(JobRunning), string(state), encoded, db.JobEvent{
		JobID:   jobID,
		Kind:    string(state),
		Message: message,
	}, db.JobEvent{
		JobID:   jobID,
		Kind:    "advance_started",
		Message: "workflow advancement started",
	})
	if err != nil {
		return err
	}
	if !transitioned {
		latest, getErr := m.Store.GetJob(ctx, jobID)
		if getErr != nil {
			return getErr
		}
		return fmt.Errorf("job %q is %s, not running", jobID, latest.State)
	}
	// Best-effort outbound emit on the genuine running->terminal transition only
	// (#446). Gated on transitioned==true so a re-run never double-emits; nil-safe
	// so the default (no EventSink) path is byte-identical.
	if m.emitTerminal != nil {
		m.emitTerminal(ctx, jobID, state, payload)
	}
	// Record resumable gates for a blocked stage that carries a `needs` list (#682)
	// so the blocker becomes actionable: `gitmoot job gates` lists them and clearing
	// them all auto-re-runs the stage. This is the single result-bearing terminal
	// chokepoint (an agent-returned blocked decision routes here via
	// stateForDecision), so the daemon-owned permission/pre-flight blocked paths
	// (Result nil, no needs) never reach it. Gated on state==JobBlocked and a
	// non-empty needs list, so every non-blocked terminal — and a blocked result
	// with no needs — writes nothing and is byte-identical. Best-effort: a gate
	// write must never turn a successfully-recorded blocked transition into an error.
	// Pipeline stage jobs (#681) are excluded: their needs are persisted on the
	// pipeline run/stage rows and resumed at the RUN level via ResumePipelineRun
	// (attempt+1, new job id). Recording job gates here would let `job gates
	// clear` RetryJob the OLD stage job id — an orphaned re-execution the
	// pipeline advancer never folds, and a double execution once the run is
	// properly resumed.
	if state == JobBlocked && payload.Result != nil && len(payload.Result.Needs) > 0 && payload.Sender != PipelineJobSender {
		if _, gateErr := m.Store.RecordJobGates(ctx, jobID, payload.Result.Needs); gateErr == nil {
			_ = m.addEvent(ctx, jobID, "gates_recorded", fmt.Sprintf("recorded %d resumable gate(s) from needs", len(payload.Result.Needs)))
		}
	}
	return nil
}

func (m Mailbox) ensureRunning(ctx context.Context, jobID string) error {
	latest, err := m.Store.GetJob(ctx, jobID)
	if err != nil {
		return err
	}
	if latest.State != string(JobRunning) {
		return fmt.Errorf("job %q is %s, not running", jobID, latest.State)
	}
	return nil
}

func (m Mailbox) claim(ctx context.Context, job db.Job) error {
	if job.State != string(JobQueued) {
		return fmt.Errorf("job %q is %s, not queued", job.ID, job.State)
	}
	// Stamp the claiming process's identity in the SAME atomic queued->running
	// UPDATE (#651): runner_boot_id (db.BootID) is the cross-boot liveness signal
	// that lets the daemon recover this job immediately after a reboot, and
	// runner_pid is recorded for observability only. Off Linux BootID() is "", so
	// the row is identity-less and only the existing age/lease recovery applies.
	claimed, err := m.Store.ClaimRunningJob(ctx, job.ID, string(JobQueued), string(JobRunning), db.JobEvent{
		JobID:   job.ID,
		Kind:    string(JobRunning),
		Message: "job started",
	}, os.Getpid(), db.BootID())
	if err != nil {
		return err
	}
	if !claimed {
		latest, getErr := m.Store.GetJob(ctx, job.ID)
		if getErr != nil {
			return getErr
		}
		return fmt.Errorf("job %q is %s, not queued", job.ID, latest.State)
	}
	return nil
}

func (m Mailbox) fail(ctx context.Context, jobID string, message string) error {
	return m.finish(ctx, jobID, JobFailed, message)
}

func (m Mailbox) addEvent(ctx context.Context, jobID string, kind string, message string) error {
	return m.Store.AddJobEvent(ctx, db.JobEvent{JobID: jobID, Kind: kind, Message: message})
}

func (m Mailbox) savePayload(ctx context.Context, jobID string, payload JobPayload) error {
	encoded, err := marshalPayload(payload)
	if err != nil {
		return err
	}
	return m.Store.UpdateJobPayload(ctx, jobID, encoded)
}

func (p JobPayload) prompt(action string) prompts.JobPrompt {
	return prompts.JobPrompt{
		Repo:                   p.Repo,
		Branch:                 p.Branch,
		PullRequest:            p.PullRequest,
		Task:                   taskLabel(p.TaskID, p.TaskTitle),
		Sender:                 p.Sender,
		Action:                 action,
		Instructions:           p.Instructions,
		Constraints:            p.Constraints,
		DelegationArtifactDir:  p.DelegationArtifactDir,
		TemplateID:             p.TemplateID,
		TemplateResolvedCommit: p.TemplateResolvedCommit,
		TemplateInstructions:   agenttemplate.InstructionsForContent(p.TemplateContent),
	}
}

func validateJobRequest(request JobRequest) error {
	action := strings.TrimSpace(request.Action)
	switch {
	case strings.TrimSpace(request.ID) == "":
		return errors.New("job id is required")
	case strings.TrimSpace(request.Agent) == "":
		return errors.New("job agent is required")
	case strings.TrimSpace(request.Action) == "":
		return errors.New("job action is required")
	case strings.TrimSpace(request.Repo) == "":
		return errors.New("job repo is required")
	case action == "produce" && request.Sender != PipelineJobSender:
		return errors.New("job action produce is reserved for pipeline stages")
	case action == "produce" && len(compactStrings(request.WritablePaths)) == 0:
		return errors.New("job action produce requires at least one writable path")
	case action != "produce" && len(compactStrings(request.WritablePaths)) > 0:
		return errors.New("job writable_paths are only valid for action produce")
	case action != "produce" && len(compactStrings(request.ReadablePaths)) > 0:
		return errors.New("job readable_paths are only valid for action produce")
	case action != "produce" && request.Network:
		return errors.New("job network access is only valid for action produce")
	case action != "produce" && strings.TrimSpace(request.Check) != "":
		return errors.New("job check is only valid for action produce")
	case action != "produce" && request.CheckRetries > 0:
		return errors.New("job check_retries are only valid for action produce")
	case request.CheckRetries < 0:
		return errors.New("job check_retries must be >= 0")
	}
	if err := ValidateWorkflowID(request.WorkflowID); err != nil {
		return err
	}
	return validateJobRuntimeOverrideRequest(request)
}

const workflowIDSegmentPattern = `[a-z0-9]+(-[a-z0-9]+)*`

var workflowIDRe = regexp.MustCompile(`^` + workflowIDSegmentPattern + `(/` + workflowIDSegmentPattern + `)?$`)

// ValidateWorkflowID applies the global external-workflow label contract.
// Empty means the job is ungrouped and is always accepted.
func ValidateWorkflowID(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if len(value) > 64 {
		return errors.New("workflow id must be at most 64 characters")
	}
	if !workflowIDRe.MatchString(value) {
		return fmt.Errorf("invalid workflow id %q: use lowercase letters, digits, and single hyphens, with at most one slash between namespace and campaign", value)
	}
	return nil
}

// validateJobRuntimeOverrideRequest rejects an invalid per-job runtime
// override (#531) at the enqueue chokepoint, BEFORE a job row exists, so every
// producer (CLI dispatch, daemon re-enqueues, future delegation runtime) fails
// fast with an actionable error instead of enqueuing a job the worker cannot
// run. Empty override = no override = always valid.
func validateJobRuntimeOverrideRequest(request JobRequest) error {
	override := strings.TrimSpace(request.RuntimeOverride)
	ref := strings.TrimSpace(request.RuntimeOverrideRef)
	if override == "" {
		if ref != "" {
			return errors.New("job runtime override session requires a runtime override")
		}
		return nil
	}
	if _, err := (runtime.Factory{}).Adapter(override); err != nil {
		return err
	}
	if ref == "" {
		return fmt.Errorf("job runtime override %q requires a session reference", override)
	}
	if override == runtime.ShellRuntime && runtime.IsFreshRef(ref) {
		return errors.New("shell runtime override requires an explicit session command (shell sessions are commands, not resumable sessions)")
	}
	// SESSION SAFETY (#531): "last" names no concrete session — the delivery
	// would resume whichever session in the checkout is most recent (possibly an
	// agent's default-runtime session, mid-flight), while the lock key would be
	// the literal "runtime:<rt>:last" and so could never serialize with that
	// concrete session's lock. Shell refs are commands, not resumable sessions,
	// so they are exempt.
	if override != runtime.ShellRuntime && ref == runtime.LastRef {
		return errors.New("job runtime override session \"last\" is not allowed; use an explicit session id")
	}
	return nil
}

func marshalPayload(payload JobPayload) (string, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// ParseJobPayload decodes a stored job payload (the same form the mailbox
// writes), tolerating the legacy preset_* keys. Read-only callers — e.g. the
// dashboard's job detail — use this to surface the request and result.
func ParseJobPayload(value string) (JobPayload, error) {
	return unmarshalPayload(value)
}

func unmarshalPayload(value string) (JobPayload, error) {
	var payload JobPayload
	if err := json.Unmarshal([]byte(value), &payload); err != nil {
		return JobPayload{}, fmt.Errorf("parse job payload: %w", err)
	}
	var legacy struct {
		PresetID             string `json:"preset_id"`
		PresetResolvedCommit string `json:"preset_resolved_commit"`
		PresetContent        string `json:"preset_content"`
	}
	if err := json.Unmarshal([]byte(value), &legacy); err != nil {
		return JobPayload{}, fmt.Errorf("parse legacy job payload: %w", err)
	}
	if payload.TemplateID == "" {
		payload.TemplateID = legacy.PresetID
	}
	if payload.TemplateResolvedCommit == "" {
		payload.TemplateResolvedCommit = legacy.PresetResolvedCommit
	}
	if payload.TemplateContent == "" {
		payload.TemplateContent = legacy.PresetContent
	}
	return payload, nil
}

func taskLabel(id string, title string) string {
	id = strings.TrimSpace(id)
	title = strings.TrimSpace(title)
	switch {
	case id == "":
		return title
	case title == "":
		return id
	default:
		return id + ": " + title
	}
}

func compactStrings(values []string) []string {
	compacted := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			compacted = append(compacted, value)
		}
	}
	return compacted
}

func compactPipelineKeyAccess(values []PipelineKeyAccess) []PipelineKeyAccess {
	if len(values) == 0 {
		return nil
	}
	out := make([]PipelineKeyAccess, 0, len(values))
	for _, value := range values {
		value.Stage = strings.TrimSpace(value.Stage)
		value.Name = strings.TrimSpace(value.Name)
		value.Source = strings.TrimSpace(value.Source)
		value.Mode = strings.TrimSpace(value.Mode)
		out = append(out, value)
	}
	return out
}
