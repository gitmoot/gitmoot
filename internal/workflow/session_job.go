package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/gitmoot/gitmoot/internal/db"
)

// SessionJobDisplayEventKind is a display-plane event. Merge policy must not
// read it: its head SHA comes from the CLI caller, not system-observed evidence.
const SessionJobDisplayEventKind = "session_job_display"

type SessionJobDisplayEvent struct {
	HeadSHA string `json:"head_sha"`
}

func sessionJobDisplayEvent(jobID, headSHA string) (db.JobEvent, bool) {
	headSHA = strings.TrimSpace(headSHA)
	if headSHA == "" {
		return db.JobEvent{}, false
	}
	message, err := json.Marshal(SessionJobDisplayEvent{HeadSHA: headSHA})
	if err != nil {
		return db.JobEvent{}, false
	}
	return db.JobEvent{
		JobID:   jobID,
		Kind:    SessionJobDisplayEventKind,
		Message: string(message),
	}, true
}

// ParseSessionJobDisplayEvent decodes caller-reported display metadata. The
// value is not merge evidence and must remain outside JobPayload.
func ParseSessionJobDisplayEvent(event db.JobEvent) (SessionJobDisplayEvent, bool) {
	if event.Kind != SessionJobDisplayEventKind {
		return SessionJobDisplayEvent{}, false
	}
	var display SessionJobDisplayEvent
	if err := json.Unmarshal([]byte(event.Message), &display); err != nil || strings.TrimSpace(display.HeadSHA) == "" {
		return SessionJobDisplayEvent{}, false
	}
	display.HeadSHA = strings.TrimSpace(display.HeadSHA)
	return display, true
}

// OpenExternalJob records the "clock in" of a session-driven ("here"/prompt-import)
// unit of work as a first-class tracked job WITHOUT the engine spawning a runtime
// (#657). The job is created directly in RUNNING state and flagged
// externally_driven, so the daemon's queued selector never claims it (no Deliver,
// no runtime subprocess, no runtime-session/checkout lock) and the engine lease
// reaper skips it. It emits the same running-state ("job started") event a normal
// job emits on claim, so the job list, events, and dashboard reflect it. The
// calling session does the real work and later calls CloseExternalJob to apply the
// result and move the job to its terminal state; lifecycle maintenance separately
// cancels a genuinely orphaned session row.
func (m Mailbox) OpenExternalJob(ctx context.Context, request JobRequest) (db.Job, error) {
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
		GoalID:                 request.GoalID,
		TaskID:                 request.TaskID,
		TaskTitle:              request.TaskTitle,
		Sender:                 firstNonEmptyString(request.Sender, "session"),
		Instructions:           request.Instructions,
		WorkflowID:             strings.TrimSpace(request.WorkflowID),
		TemplateID:             snapshot.ID,
		TemplateResolvedCommit: snapshot.ResolvedCommit,
		TemplateContent:        snapshot.Content,
	})
	if err != nil {
		return db.Job{}, err
	}

	job := db.Job{
		ID:               request.ID,
		Agent:            request.Agent,
		Type:             request.Action,
		State:            string(JobRunning),
		Payload:          payload,
		ExternallyDriven: true,
	}
	events := []db.JobEvent{{
		JobID:   job.ID,
		Kind:    string(JobRunning),
		Message: "job started (externally driven session)",
	}}
	if displayEvent, ok := sessionJobDisplayEvent(job.ID, request.HeadSHA); ok {
		events = append(events, displayEvent)
	}
	if err := m.Store.CreateExternallyDrivenJobWithEvent(ctx, job, events[0], events[1:]...); err != nil {
		return db.Job{}, err
	}
	return job, nil
}

// CloseExternalJob records the "clock out" of a session job (#657): it applies the
// session's result through the SAME result path an engine-run job uses —
// result.Decision maps to a terminal JobState via stateForDecision, the result is
// stored on the payload, and the terminal-state event + best-effort outbound event
// (job.finished/failed/blocked via the wired EventSink) fire exactly as they do for
// a runtime-returned result. Unlike the engine's finishWithPayload it emits NO
// "advance_started" event, because a session job has no downstream engine
// advancement to run (the session already did the work); emitting it would make the
// daemon try to advance a job the engine never owned.
//
// A job can be closed exactly once: it must currently be running AND
// externally_driven, else a clear error is returned (double-close, closing an
// engine job, or an unknown id all fail cleanly).
func (m Mailbox) CloseExternalJob(ctx context.Context, jobID string, result AgentResult, prOverride int, headSHAOverride, branchOverride string) (db.Job, error) {
	if m.Store == nil {
		return db.Job{}, errors.New("mailbox store is required")
	}
	if _, ok := allowedSet(ResultDecisions)[result.Decision]; !ok {
		return db.Job{}, fmt.Errorf("unsupported decision %q; want one of %s", result.Decision, strings.Join(ResultDecisions, ", "))
	}

	job, err := m.Store.GetJob(ctx, jobID)
	if err != nil {
		return db.Job{}, err
	}
	if !job.ExternallyDriven {
		return db.Job{}, fmt.Errorf("job %q is not a session job (not externally driven); only a job opened with `job open` can be closed", jobID)
	}
	if job.State != string(JobRunning) {
		return db.Job{}, fmt.Errorf("job %q is %s, not running; it has already been closed", jobID, job.State)
	}

	payload, err := unmarshalPayload(job.Payload)
	if err != nil {
		return db.Job{}, err
	}
	resultCopy := result
	payload.Result = &resultCopy
	if prOverride > 0 {
		payload.PullRequest = prOverride
	}
	if strings.TrimSpace(branchOverride) != "" {
		payload.Branch = strings.TrimSpace(branchOverride)
	}
	encoded, err := marshalPayload(payload)
	if err != nil {
		return db.Job{}, err
	}

	state := stateForDecision(result.Decision)
	extraEvents := []db.JobEvent{}
	if displayEvent, ok := sessionJobDisplayEvent(jobID, headSHAOverride); ok {
		extraEvents = append(extraEvents, displayEvent)
	}
	transitioned, err := m.Store.TransitionJobStatePayloadWithEvent(ctx, jobID, string(JobRunning), string(state), encoded, db.JobEvent{
		JobID:   jobID,
		Kind:    string(state),
		Message: fmt.Sprintf("job %s", state),
	}, extraEvents...)
	if err != nil {
		return db.Job{}, err
	}
	if !transitioned {
		latest, getErr := m.Store.GetJob(ctx, jobID)
		if getErr != nil {
			return db.Job{}, getErr
		}
		return db.Job{}, fmt.Errorf("job %q is %s, not running; it has already been closed", jobID, latest.State)
	}
	// Best-effort outbound emit on the genuine running->terminal transition (#446),
	// wired the same way the engine wires finishWithPayload's terminal emit. nil-safe:
	// with no EventSink configured no event is constructed and behavior is unchanged.
	if m.emitTerminal != nil {
		m.emitTerminal(ctx, jobID, state, payload)
	}
	return m.Store.GetJob(ctx, jobID)
}

// OpenExternalJob records a session job's clock-in through the engine's Mailbox so
// the terminal-event emit seam (e.mailbox()) is wired for the matching
// CloseExternalJob. See Mailbox.OpenExternalJob.
func (e Engine) OpenExternalJob(ctx context.Context, request JobRequest) (db.Job, error) {
	return e.mailbox().OpenExternalJob(ctx, request)
}

// CloseExternalJob applies a session job's result and moves it to its terminal
// state, emitting the outbound terminal event through the engine's wired EventSink.
// See Mailbox.CloseExternalJob.
func (e Engine) CloseExternalJob(ctx context.Context, jobID string, result AgentResult, prOverride int, headSHAOverride, branchOverride string) (db.Job, error) {
	return e.mailbox().CloseExternalJob(ctx, jobID, result, prOverride, headSHAOverride, branchOverride)
}
