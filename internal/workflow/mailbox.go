package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jerryfane/gitmoot/internal/agenttemplate"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/prompts"
	"github.com/jerryfane/gitmoot/internal/runtime"
)

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
}

type JobRequest struct {
	ID                     string
	Agent                  string
	Action                 string
	Repo                   string
	Branch                 string
	PullRequest            int
	HeadSHA                string
	GoalID                 string
	TaskID                 string
	TaskTitle              string
	LeadAgent              string
	Reviewers              []string
	ReviewRound            string
	Sender                 string
	Instructions           string
	Constraints            []string
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
	Phase                  string         `json:"phase,omitempty"`
	Cockpit                bool           `json:"cockpit,omitempty"`
	CockpitSession         string         `json:"cockpit_session,omitempty"`
	CockpitPaneKey         string         `json:"cockpit_pane_key,omitempty"`
	SkipNativeReviewFanout bool           `json:"skip_native_review_fanout,omitempty"`
	Ephemeral              *EphemeralSpec `json:"ephemeral,omitempty"`
	HumanAnswer            string         `json:"human_answer,omitempty"`
	RawOutputs             []string       `json:"raw_outputs,omitempty"`
	Result                 *AgentResult   `json:"result,omitempty"`
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
	return template, nil
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

	prompt := prompts.RenderJob(payload.prompt(job.Type))
	firstRaw, firstErr := m.deliver(ctx, adapter, agent, job, payload, prompt)
	if firstErr != nil {
		_ = m.fail(ctx, job.ID, fmt.Sprintf("delivery failed: %v", firstErr))
		return AgentResult{}, firstErr
	}
	payload.RawOutputs = append(payload.RawOutputs, firstRaw)

	result, parseErr := ExtractAgentResult(firstRaw)
	if parseErr != nil {
		if err := m.savePayload(ctx, job.ID, payload); err != nil {
			return AgentResult{}, err
		}
		if err := m.addEvent(ctx, job.ID, "malformed_output", parseErr.Error()); err != nil {
			return AgentResult{}, err
		}
		if err := m.ensureRunning(ctx, job.ID); err != nil {
			return AgentResult{}, err
		}
		if err := m.addEvent(ctx, job.ID, "repair_retry", "retrying once with repair prompt"); err != nil {
			return AgentResult{}, err
		}

		repairPrompt := prompts.RenderRepairPrompt(firstRaw, parseErr)
		secondRaw, secondErr := m.deliver(ctx, adapter, agent, job, payload, repairPrompt)
		if secondErr != nil {
			_ = m.fail(ctx, job.ID, fmt.Sprintf("repair delivery failed: %v", secondErr))
			return AgentResult{}, secondErr
		}
		payload.RawOutputs = append(payload.RawOutputs, secondRaw)
		result, parseErr = ExtractAgentResult(secondRaw)
		if parseErr != nil {
			if err := m.savePayload(ctx, job.ID, payload); err != nil {
				return AgentResult{}, err
			}
			_ = m.fail(ctx, job.ID, fmt.Sprintf("repair output malformed: %v", parseErr))
			return AgentResult{}, parseErr
		}
	}

	if err := m.ensureRunning(ctx, job.ID); err != nil {
		return AgentResult{}, err
	}
	payload.Result = &result
	state := stateForDecision(result.Decision)
	if err := m.finishWithPayload(ctx, job.ID, state, fmt.Sprintf("job %s", state), payload); err != nil {
		return AgentResult{}, err
	}
	return result, nil
}

func (m Mailbox) deliver(ctx context.Context, adapter DeliveryAdapter, agent runtime.Agent, job db.Job, payload JobPayload, prompt string) (string, error) {
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
	// budget (#338 Part B) can sum a tree's cost. Only persist when the runtime
	// actually reported usage (a positive count) so runtimes that report nothing
	// (e.g. codex Deliver, which runs without --json) leave the columns at their 0
	// default rather than taking a no-op write. Usage accounting must never fail a
	// delivery, so a write error is swallowed.
	if err == nil && (result.InputTokens > 0 || result.OutputTokens > 0) {
		_ = m.Store.UpdateJobUsage(ctx, job.ID, result.InputTokens, result.OutputTokens)
	}
	if strings.TrimSpace(result.Summary) != "" {
		return result.Summary, err
	}
	return result.Raw, err
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
	claimed, err := m.Store.TransitionJobStateWithEvent(ctx, job.ID, string(JobQueued), string(JobRunning), db.JobEvent{
		JobID:   job.ID,
		Kind:    string(JobRunning),
		Message: "job started",
	})
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
