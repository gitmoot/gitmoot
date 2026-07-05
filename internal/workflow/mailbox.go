package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"strings"

	"github.com/jerryfane/gitmoot/internal/agenttemplate"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/prompts"
	"github.com/jerryfane/gitmoot/internal/runtime"
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
	TemplateOverride       *db.AgentTemplate
	ParentJobID            string
	DelegationID           string
	DelegationDepth        int
	DelegatedBy            string
	RootJobID              string
	Deps                   []string
	JobTimeout             string
	RetryCount             int
	Fingerprint            string
	FailurePolicy          string
	SynthesisRule          string
	DelegationArtifactDir  string
	WorktreePath           string
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
	// RuntimeOverride, when non-empty, runs THIS job through the named runtime
	// instead of the agent's registered default runtime (#531). The agent's
	// stored runtime/session are untouched: the job runs on RuntimeOverrideRef
	// (an explicit session on the override runtime, or a minted fresh ref), and
	// the engine never persists a refreshed session ref for an overridden job.
	RuntimeOverride        string
	RuntimeOverrideRef     string
	Phase                  string
	Cockpit                bool
	CockpitSession         string
	CockpitPaneKey         string
	SkipNativeReviewFanout bool
	Ephemeral              *EphemeralSpec
	// HumanAnswer carries the rendered ask-gate answer block (#445) into the
	// coordinator continuation enqueued by the `answer` resume verb. Empty for
	// every other job, so the stored payload is byte-identical by default.
	HumanAnswer string
}

type JobPayload struct {
	Repo                   string         `json:"repo"`
	Branch                 string         `json:"branch"`
	PullRequest            int            `json:"pull_request"`
	HeadSHA                string         `json:"head_sha,omitempty"`
	GoalID                 string         `json:"goal_id,omitempty"`
	TaskID                 string         `json:"task_id"`
	TaskTitle              string         `json:"task_title"`
	LeadAgent              string         `json:"lead_agent,omitempty"`
	Reviewers              []string       `json:"reviewers,omitempty"`
	ReviewRound            string         `json:"review_round,omitempty"`
	Sender                 string         `json:"sender"`
	Instructions           string         `json:"instructions"`
	Constraints            []string       `json:"constraints"`
	ParentJobID            string         `json:"parent_job_id,omitempty"`
	DelegationID           string         `json:"delegation_id,omitempty"`
	DelegationDepth        int            `json:"delegation_depth,omitempty"`
	DelegatedBy            string         `json:"delegated_by,omitempty"`
	RootJobID              string         `json:"root_job_id,omitempty"`
	Deps                   []string       `json:"deps,omitempty"`
	JobTimeout             string         `json:"job_timeout,omitempty"`
	RetryCount             int            `json:"retry_count,omitempty"`
	Fingerprint            string         `json:"fingerprint,omitempty"`
	FailurePolicy          string         `json:"failure_policy,omitempty"`
	SynthesisRule          string         `json:"synthesis_rule,omitempty"`
	DelegationArtifactDir  string         `json:"delegation_artifact_dir,omitempty"`
	WorktreePath           string         `json:"worktree_path,omitempty"`
	TemplateID             string         `json:"template_id,omitempty"`
	TemplateResolvedCommit string         `json:"template_resolved_commit,omitempty"`
	TemplateContent        string         `json:"template_content,omitempty"`
	OriginalAgent          string         `json:"original_agent,omitempty"`
	DelegatedAgent         string         `json:"delegated_agent,omitempty"`
	DelegationReason       string         `json:"delegation_reason,omitempty"`
	RecentDelegationHashes []string       `json:"recent_delegation_hashes,omitempty"`
	DelegationRepeatCount  int            `json:"delegation_repeat_count,omitempty"`
	NonProgressStreak      int            `json:"non_progress_streak,omitempty"`
	LastProgressDigest     string         `json:"last_progress_digest,omitempty"`
	VerifyAttempt          int            `json:"verify_attempt,omitempty"`
	DelegationFinalize     bool           `json:"delegation_finalize,omitempty"`
	Model                  string         `json:"model,omitempty"`
	RuntimeOverride        string         `json:"runtime_override,omitempty"`
	RuntimeOverrideRef     string         `json:"runtime_override_ref,omitempty"`
	Phase                  string         `json:"phase,omitempty"`
	Cockpit                bool           `json:"cockpit,omitempty"`
	CockpitSession         string         `json:"cockpit_session,omitempty"`
	CockpitPaneKey         string         `json:"cockpit_pane_key,omitempty"`
	SkipNativeReviewFanout bool           `json:"skip_native_review_fanout,omitempty"`
	Ephemeral              *EphemeralSpec `json:"ephemeral,omitempty"`
	HumanAnswer            string         `json:"human_answer,omitempty"`
	RawOutputs             []string       `json:"raw_outputs,omitempty"`
	Result                 *AgentResult   `json:"result,omitempty"`
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
		RuntimeOverride:        strings.TrimSpace(request.RuntimeOverride),
		RuntimeOverrideRef:     strings.TrimSpace(request.RuntimeOverrideRef),
		Phase:                  request.Phase,
		Cockpit:                request.Cockpit,
		CockpitSession:         strings.TrimSpace(request.CockpitSession),
		CockpitPaneKey:         strings.TrimSpace(request.CockpitPaneKey),
		SkipNativeReviewFanout: request.SkipNativeReviewFanout,
		Ephemeral:              request.Ephemeral,
		HumanAnswer:            request.HumanAnswer,
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

	if err := m.claim(ctx, job); err != nil {
		return AgentResult{}, err
	}

	registeredFreshRef := runtime.IsFreshRef(agent.RuntimeRef)
	prompt := prompts.RenderJob(payload.prompt(job.Type))
	// Append the off-by-default "Prior learnings" memory block (#626 READ path).
	// When the memory hook is unset (every non-enrolled path) or returns "" (no
	// enrolled agent, empty sanitized query, or no confirmed match), the prompt is
	// byte-identical.
	if m.injectMemory != nil {
		if block := m.injectMemory(ctx, agent, payload); block != "" {
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
	firstRaw, firstRefreshedRef, firstEphemeral, firstErr := m.deliver(ctx, adapter, agent, job, payload, prompt)
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
		_ = m.fail(ctx, job.ID, fmt.Sprintf("delivery failed: %v", firstErr))
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
			repairRaw, repairRefreshedRef, repairEphemeral, repairErr := m.deliver(ctx, adapter, agent, job, payload, repairPrompt)
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
				_ = m.fail(ctx, job.ID, fmt.Sprintf("repair delivery failed: %v", repairErr))
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

			result, parseErr = ExtractAgentResult(repairRaw)
			if parseErr == nil {
				break
			}
			if err := m.savePayload(ctx, job.ID, payload); err != nil {
				return AgentResult{}, err
			}
		}

		if parseErr != nil {
			_ = m.fail(ctx, job.ID, fmt.Sprintf("repair output malformed: %v", parseErr))
			return AgentResult{}, parseErr
		}
	}

	if err := m.ensureRunning(ctx, job.ID); err != nil {
		return AgentResult{}, err
	}
	payload.Result = &result
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
	return result, nil
}

func (m Mailbox) deliver(ctx context.Context, adapter DeliveryAdapter, agent runtime.Agent, job db.Job, payload JobPayload, prompt string) (string, string, bool, error) {
	result, err := adapter.Deliver(ctx, agent, runtime.Job{
		ID:          job.ID,
		AgentName:   agent.Name,
		Action:      job.Type,
		Prompt:      prompt,
		Repository:  payload.Repo,
		PullRequest: payload.PullRequest,
		Model:       payload.Model,
	})
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
		}
		// Only persist when a positive count remains so runtimes that report nothing
		// (e.g. the shell runtime) or a delta that resolved to 0 leave the columns at
		// their 0 default rather than taking a no-op write.
		if inTok > 0 || outTok > 0 {
			_ = m.Store.UpdateJobUsage(ctx, job.ID, inTok, outTok)
		}
	}
	if strings.TrimSpace(result.Summary) != "" {
		return result.Summary, result.RefreshedRuntimeRef, result.SessionEphemeral, err
	}
	return result.Raw, result.RefreshedRuntimeRef, result.SessionEphemeral, err
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
	switch {
	case strings.TrimSpace(request.ID) == "":
		return errors.New("job id is required")
	case strings.TrimSpace(request.Agent) == "":
		return errors.New("job agent is required")
	case strings.TrimSpace(request.Action) == "":
		return errors.New("job action is required")
	case strings.TrimSpace(request.Repo) == "":
		return errors.New("job repo is required")
	}
	return validateJobRuntimeOverrideRequest(request)
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
