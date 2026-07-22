package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/events"
)

// Escalation event kinds (#340). Escalation is round-based: a coordinator/leg can
// pause more than once over its lifetime (a retried leg that fails again opens a
// NEW round). The pause is OPEN while requested > resolved, and CLOSED (resolved
// or finalizable) while requested == resolved. So a second escalate_human pass
// during an OPEN round re-notifies nothing and re-records nothing (idempotent),
// but the first failure of a round whose every prior escalation was resolved
// opens a fresh round: a new requested event, a re-notify, and a re-pause.
const (
	escalationRequestedEvent = "delegation_escalation_requested"
	escalationResolvedEvent  = "delegation_escalation_resolved"
)

// EscalationRecord is the structured payload stored in a
// delegation_escalation_requested event message, so the resume path can resolve
// the failing leg and child job, and the wall-clock backstop can exclude the
// paused duration. It is JSON-encoded into the event message (job_events has no
// dedicated columns); PausedAt is RFC3339-UTC.
type EscalationRecord struct {
	DelegationID string `json:"delegation_id"`
	ChildJobID   string `json:"child_job_id,omitempty"`
	Reason       string `json:"reason,omitempty"`
	Question     string `json:"question,omitempty"`
	PausedAt     string `json:"paused_at,omitempty"`
	// Kind discriminates the pause flavor (#445): "" (or "failure") is the
	// escalate_human failure pause; "ask" is the non-failure ask-gate pause opened
	// by a healthy result's human_questions[]. It rides the SAME
	// delegation_escalation_requested/_resolved event kinds so TTL, wall-clock
	// pause exclusion, and round-idempotency all apply unchanged; only the
	// human-facing rendering and the resume verb gating branch on it.
	Kind string `json:"kind,omitempty"`
	// Questions carries the ask-gate's human_questions[] on the requested event so
	// the notifier can render them and the resume can map answers by id. Empty for
	// a failure escalation.
	Questions []HumanQuestion `json:"questions,omitempty"`
	// Answers carries the human's parsed id->answer map on the resolved event of an
	// ask round. Empty for a failure escalation or an unanswered (TTL) resolution.
	Answers map[string]string `json:"answers,omitempty"`
}

// escalationKindAsk is the EscalationRecord.Kind discriminator for an ask-gate
// pause (#445); the failure-escalation pause leaves Kind empty.
const escalationKindAsk = "ask"

// pauseAwaitingHuman is the escalate_human analogue of block (#340): it sets the
// shared parent task to TaskAwaitingHuman, records a per-round
// delegation_escalation_requested event (idempotent WITHIN an open round via the
// requested>resolved guard, but a retried leg that fails again opens a fresh
// round and re-pauses), notifies the human best-effort through the injected
// EscalationNotifier, and returns an AwaitingHumanError so the caller enqueues NO
// continuation. The tree therefore consumes zero compute until an operator
// resumes it with `/gitmoot resume <coordinator> retry|continue|abort`.
func (e Engine) pauseAwaitingHuman(ctx context.Context, parentJob db.Job, parentPayload JobPayload, ref taskRef, d Delegation, child db.Job) error {
	reason := childFailureReason(child)
	awaitErr := AwaitingHumanError{Reason: fmt.Sprintf("delegation %q failed (failure_policy escalate_human): %s", d.ID, reason)}

	// While an escalation round is OPEN (requested > resolved), this is an
	// idempotent re-advance (a concurrent child completion, or the same child's
	// AdvanceJob re-running): keep the task in awaiting_human and return the same
	// pause error, but re-record nothing and re-notify nobody. When the round is
	// CLOSED (requested == resolved: a prior escalation was resolved, e.g. a retry
	// that has now failed AGAIN) we fall through to open a FRESH round below — a
	// new requested event, a re-pause, and a re-notify — so the tree never strands
	// permanently in planned with nothing in flight (#340).
	open, err := e.escalationOpen(ctx, parentJob.ID)
	if err != nil {
		return err
	}
	if open {
		return awaitErr
	}

	if err := e.setTaskState(ctx, ref, TaskAwaitingHuman); err != nil {
		return err
	}

	record := EscalationRecord{
		DelegationID: d.ID,
		ChildJobID:   child.ID,
		Reason:       reason,
		Question:     strings.TrimSpace(d.Prompt),
		PausedAt:     e.now().UTC().Format(time.RFC3339),
	}
	encoded, marshalErr := json.Marshal(record)
	message := awaitErr.Reason
	if marshalErr == nil {
		message = string(encoded)
	}
	if err := e.Store.AddJobEvent(ctx, db.JobEvent{
		JobID:   parentJob.ID,
		Kind:    escalationRequestedEvent,
		Message: message,
	}); err != nil {
		return err
	}

	// Notify the human best-effort: a nil notifier (ask-path/tests) or a notifier
	// error never fails the pause — the dashboard "Attention" section and the
	// recorded event are the durable source of truth; the comment is a courtesy.
	if e.EscalationNotifier != nil {
		_ = e.EscalationNotifier.NotifyEscalation(ctx, EscalationRequest{
			CoordinatorJobID: parentJob.ID,
			DelegationID:     d.ID,
			ChildJobID:       child.ID,
			Reason:           reason,
			Question:         strings.TrimSpace(d.Prompt),
			Repo:             firstNonEmptyString(ref.Repo, parentPayload.Repo),
			PullRequest:      parentPayload.PullRequest,
			Branch:           firstNonEmptyString(ref.Branch, parentPayload.Branch),
			TaskID:           ref.ID,
			TaskTitle:        ref.Title,
		})
	}

	// Emit a best-effort job.needs_attention on the FRESH escalation round (#446).
	// It is gated on the same one-shot path as the escalationRequested event above
	// (we only reach here when the round was CLOSED), so a re-advance does NOT
	// re-emit. nil-safe: no EventSink => no event. detail carries the redacted
	// question. This is the seam #445's ask-gate rides to emit its own
	// job.needs_attention. The coordinator job id is the subject so a consumer can
	// resume the right tree; root_id groups the run.
	rootID := strings.TrimSpace(parentPayload.RootJobID)
	if rootID == "" {
		rootID = parentJob.ID
	}
	ev := events.NewEvent(
		events.EventJobNeedsAttention,
		parentJob.ID,
		rootID,
		firstNonEmptyString(ref.Repo, parentPayload.Repo),
		string(TaskAwaitingHuman),
		strings.TrimSpace(d.Prompt),
		e.now(),
		RedactCommentText,
	)
	ev.Cause = "escalation"
	events.EmitEvent(ctx, e.EventSink, ev)

	// Auto-link a local chat thread as the answer channel (#534): best-effort and
	// swallow-all, so a chat failure never affects the pause. Participant is the
	// coordinator agent (whose resume the human drives).
	e.linkAskGateChatThread(ctx, parentJob.ID, firstNonEmptyString(ref.Repo, parentPayload.Repo), parentJob.Agent, awaitErr.Reason)

	return awaitErr
}

// pauseAwaitingHumanAnswer is the non-failure ask-gate sibling of
// pauseAwaitingHuman (#445): a HEALTHY job that returned human_questions[] pauses
// its task at awaiting_human for a specific human answer. It reuses the exact
// escalate_human pause plumbing — the same delegation_escalation_requested event
// kind (tagged Kind="ask" + the questions), the same escalationOpen round guard,
// and the same EscalationNotifier / job.needs_attention (#446) seam — so TTL
// auto-finalize, wall-clock pause exclusion, and round-idempotency all apply with
// no extra plumbing. It enqueues NO continuation and dispatches NO delegations
// (the caller short-circuits on a true return), so the pause is budget-neutral
// and consumes zero compute until the human resumes with `answer`.
//
// It returns whether the ask round is currently OPEN. true => the caller returns
// AwaitingHumanError (freshly opened, or an idempotent re-advance while awaiting).
// false => the round is CLOSED (the human already answered and the answer-driven
// continuation is in flight, or the ask was TTL-finalized): the caller
// short-circuits AdvanceJob without redispatching.
//
// When the asking job is a delegation CHILD (#445), the round is keyed/recorded/
// routed on its COORDINATOR (payload.ParentJobID) exactly like the escalate_human
// failure pause (which keys on parentJob.ID), NOT on the child's own id. This
// preserves the frozen single-round-per-parent invariant: the FIRST asking sibling
// opens the one shared round and subsequent siblings hit the open-round guard, the
// shared parent task flips once, and the human resumes the COORDINATOR (whose
// continuation carries the answer) — not a child that the parent DAG would advance
// past independently.
func (e Engine) pauseAwaitingHumanAnswer(ctx context.Context, job db.Job, payload JobPayload, ref taskRef) (bool, error) {
	// Route the pause to the coordinator when the asking job is a delegation child:
	// the escalation event, round guard, notifier target, and task ref all key on
	// the coordinator so siblings share one round (mirrors pauseAwaitingHuman).
	targetID := job.ID
	targetRef := ref
	if parentID := strings.TrimSpace(payload.ParentJobID); parentID != "" {
		targetID = parentID
		if _, parentPayload, perr := e.jobPayload(ctx, parentID); perr == nil {
			if pref := taskRefFromPayload(parentPayload); pref.ID != "" {
				targetRef = pref
			}
		}
	}

	// While ANY escalation round is OPEN (requested > resolved) on the target
	// (coordinator for a child, else self), this is an idempotent re-advance: keep
	// the pause, re-record nothing, re-notify nobody.
	open, err := e.escalationOpen(ctx, targetID)
	if err != nil {
		return false, err
	}
	if open {
		return true, nil
	}

	// No OPEN round. If an ask round was already opened AND resolved for the target,
	// the human has answered (or TTL-finalized) and the answer-driven continuation
	// is the asking job's sole continuation: report CLOSED so the caller
	// short-circuits without re-pausing or redispatching. Mirrors the
	// escalate_human round model, but an asking job is never re-run to open a
	// second ask round (only a retried failing leg re-pauses), so a resolved ask
	// round stays closed.
	if _, exists, lerr := e.loadEscalation(ctx, targetID); lerr != nil {
		return false, lerr
	} else if exists {
		// A child that asks while the coordinator's round was already opened+resolved
		// by a sibling is CLOSED (the shared answer-continuation is in flight); a
		// resolved/non-ask round on the coordinator is also not ours to reopen.
		return false, nil
	}

	// Open a FRESH ask round: pause the (coordinator's) task, record the requested
	// event with the questions on the target, notify best-effort, and emit
	// job.needs_attention once. All keyed on targetID/targetRef so a child's ask
	// routes to its coordinator.
	if err := e.setTaskState(ctx, targetRef, TaskAwaitingHuman); err != nil {
		return false, err
	}

	questions := payload.Result.HumanQuestions
	record := EscalationRecord{
		Reason:    fmt.Sprintf("%d human question(s) awaiting an answer", len(questions)),
		Question:  renderHumanQuestions(questions),
		PausedAt:  e.now().UTC().Format(time.RFC3339),
		Kind:      escalationKindAsk,
		Questions: questions,
	}
	encoded, marshalErr := json.Marshal(record)
	message := record.Reason
	if marshalErr == nil {
		message = string(encoded)
	}
	if err := e.Store.AddJobEvent(ctx, db.JobEvent{
		JobID:   targetID,
		Kind:    escalationRequestedEvent,
		Message: message,
	}); err != nil {
		return false, err
	}

	// Notify the human best-effort (nil notifier / notifier error never fails the
	// pause): the recorded event + dashboard Attention are the durable truth. The
	// CoordinatorJobID is the resume target (the coordinator for a child's ask) so
	// the human resumes the job whose continuation actually carries the answer.
	if e.EscalationNotifier != nil {
		_ = e.EscalationNotifier.NotifyEscalation(ctx, EscalationRequest{
			CoordinatorJobID: targetID,
			Question:         renderHumanQuestions(questions),
			Ask:              true,
			Questions:        questions,
			Repo:             firstNonEmptyString(targetRef.Repo, payload.Repo),
			PullRequest:      payload.PullRequest,
			Branch:           firstNonEmptyString(targetRef.Branch, payload.Branch),
			TaskID:           targetRef.ID,
			TaskTitle:        targetRef.Title,
		})
	}

	// Emit a best-effort job.needs_attention on the FRESH ask round via the SAME
	// #446 EventSink the failure escalation rides (NOT a parallel notify path).
	// One-shot (we only reach here when no round was open) and nil-safe. The subject
	// is the resume target (coordinator for a child) so a consumer resumes the right
	// tree; root_id groups the run.
	rootID := strings.TrimSpace(payload.RootJobID)
	if rootID == "" {
		rootID = targetID
	}
	ev := events.NewEvent(
		events.EventJobNeedsAttention,
		targetID,
		rootID,
		firstNonEmptyString(targetRef.Repo, payload.Repo),
		string(TaskAwaitingHuman),
		renderHumanQuestions(questions),
		e.now(),
		RedactCommentText,
	)
	ev.Cause = "ask_gate"
	events.EmitEvent(ctx, e.EventSink, ev)

	// Auto-link a local chat thread carrying the questions as the answer channel
	// (#534 keystone): best-effort and swallow-all. Keyed on the resume target
	// (coordinator for a child ask); participant is the asking job's agent.
	e.linkAskGateChatThread(ctx, targetID, firstNonEmptyString(targetRef.Repo, payload.Repo), job.Agent, renderHumanQuestions(questions))

	return true, nil
}

// renderHumanQuestions renders the ask-gate questions as a compact, human-facing
// multi-line block ("- <id>: <prompt> (choices: ...)") used both as the recorded
// Question text and the needs_attention detail. Empty for no questions.
func renderHumanQuestions(questions []HumanQuestion) string {
	if len(questions) == 0 {
		return ""
	}
	var b strings.Builder
	for i, q := range questions {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "- %s: %s", strings.TrimSpace(q.ID), strings.TrimSpace(q.Prompt))
		if len(q.Choices) > 0 {
			fmt.Fprintf(&b, " (choices: %s)", strings.Join(q.Choices, ", "))
		}
	}
	return b.String()
}

// firstNonEmptyString returns the first non-blank value, or "".
func firstNonEmptyString(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// loadEscalation returns the structured escalation record recorded on a paused
// coordinator job, and whether one exists. It tolerates a legacy/plain-text
// message (pre-JSON) by returning a record with only the raw reason, so a resume
// still routes.
func (e Engine) loadEscalation(ctx context.Context, coordinatorJobID string) (EscalationRecord, bool, error) {
	events, err := e.Store.ListJobEvents(ctx, coordinatorJobID)
	if err != nil {
		return EscalationRecord{}, false, err
	}
	var rec EscalationRecord
	found := false
	for _, ev := range events {
		if ev.Kind != escalationRequestedEvent {
			continue
		}
		found = true
		if json.Unmarshal([]byte(ev.Message), &rec) != nil {
			rec = EscalationRecord{Reason: ev.Message}
		}
	}
	if !found {
		return EscalationRecord{}, false, nil
	}
	return rec, true, nil
}

// escalationRoundCounts returns how many escalation rounds were requested vs
// resolved for a coordinator (#340). Escalation is round-based: a retried leg
// that fails again opens a fresh round, so a coordinator can accumulate several
// requested/resolved pairs over its lifetime. The pause is OPEN (resolvable /
// finalizable) while requested > resolved, and CLOSED while requested ==
// resolved.
func (e Engine) escalationRoundCounts(ctx context.Context, coordinatorJobID string) (requested, resolved int, err error) {
	events, err := e.Store.ListJobEvents(ctx, coordinatorJobID)
	if err != nil {
		return 0, 0, err
	}
	for _, ev := range events {
		switch ev.Kind {
		case escalationRequestedEvent:
			requested++
		case escalationResolvedEvent:
			resolved++
		}
	}
	return requested, resolved, nil
}

// escalationOpen reports whether a coordinator currently has an UNRESOLVED
// escalation round (requested > resolved): the tree is paused awaiting a human
// right now. When false, either no escalation ever opened, or every round that
// opened has been resolved (the leg may then fail AGAIN to open a new round).
func (e Engine) escalationOpen(ctx context.Context, coordinatorJobID string) (bool, error) {
	requested, resolved, err := e.escalationRoundCounts(ctx, coordinatorJobID)
	if err != nil {
		return false, err
	}
	return requested > resolved, nil
}

// escalationResolved reports whether the coordinator has no OPEN escalation round
// to act on — i.e. every requested escalation has been resolved (requested ==
// resolved). The resume path and the TTL backstop are idempotency-guarded by
// this check: an OPEN round (requested > resolved) is resolvable/finalizable, a
// CLOSED one is a no-op. Round-based so a re-pause after a failed retry is again
// resolvable, never permanently stranded (#340).
func (e Engine) escalationResolved(ctx context.Context, coordinatorJobID string) (bool, error) {
	open, err := e.escalationOpen(ctx, coordinatorJobID)
	if err != nil {
		return false, err
	}
	return !open, nil
}

// EscalationPending reports whether coordinatorJobID has an UNRESOLVED human
// escalation round right now: a round was requested and not yet
// answered/aborted/TTL-finalized. It is the read-side companion to
// ResolveEscalation, whose already-resolved branch is a silent idempotent no-op.
// A caller like `chat answer` checks this first so it does not report a false
// success (and record a duplicate answer message) when the round was already
// resolved. Returns false when the job never had an escalation at all.
func (e Engine) EscalationPending(ctx context.Context, coordinatorJobID string) (bool, error) {
	if err := e.validate(); err != nil {
		return false, err
	}
	_, exists, err := e.loadEscalation(ctx, coordinatorJobID)
	if err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}
	resolved, err := e.escalationResolved(ctx, coordinatorJobID)
	if err != nil {
		return false, err
	}
	return !resolved, nil
}

// ResumeDecision is one of the three human resume verbs for a paused tree (#340).
type ResumeDecision string

const (
	// ResumeRetry re-enqueues the failing delegation leg with the human's
	// instructions folded into its prompt.
	ResumeRetry ResumeDecision = "retry"
	// ResumeContinue proceeds the coordinator continuation (today's escalate path,
	// now human-approved): the coordinator synthesizes every child outcome.
	ResumeContinue ResumeDecision = "continue"
	// ResumeAbort routes to the #305 graceful finalize continuation for a terminal
	// best-effort synthesis of whatever completed.
	ResumeAbort ResumeDecision = "abort"
	// ResumeAnswer delivers a human's answer(s) to an ask-gate pause (#445): it
	// enqueues the coordinator continuation carrying the answer text. It is valid
	// only on an ask round (Kind="ask"); the retry/continue/abort verbs are valid
	// only on a failure-escalation round.
	ResumeAnswer ResumeDecision = "answer"
)

// validResumeDecision normalizes and validates a resume verb.
func validResumeDecision(decision string) (ResumeDecision, bool) {
	switch ResumeDecision(strings.ToLower(strings.TrimSpace(decision))) {
	case ResumeRetry:
		return ResumeRetry, true
	case ResumeContinue:
		return ResumeContinue, true
	case ResumeAbort:
		return ResumeAbort, true
	case ResumeAnswer:
		return ResumeAnswer, true
	default:
		return "", false
	}
}

// ParseResumeDecision is the exported normalizer the daemon uses to validate the
// human's resume verb before calling ResolveEscalation (#340).
func ParseResumeDecision(decision string) (ResumeDecision, bool) {
	return validResumeDecision(decision)
}

// ResolveEscalation resumes a tree paused at TaskAwaitingHuman (#340). The
// coordinatorJobID is the job that recorded the escalation (the resume target the
// notification quoted). decision selects the verb; instructions is the human's
// optional guidance, folded into the retried leg's prompt (retry) and recorded on
// the resolution event (all verbs). It is idempotent: a second resume on an
// already-resolved coordinator is a no-op. It clears TaskAwaitingHuman and records
// a delegation_escalation_resolved event carrying resolved_at so the wall-clock
// backstop can close the pause window. The caller (daemon handleResumeCommand) is
// authorize-commenter gated.
func (e Engine) ResolveEscalation(ctx context.Context, coordinatorJobID string, decision ResumeDecision, instructions string) error {
	if err := e.validate(); err != nil {
		return err
	}
	verb, ok := validResumeDecision(string(decision))
	if !ok {
		return fmt.Errorf("invalid resume decision %q; want retry|continue|abort|answer", decision)
	}

	rec, exists, err := e.loadEscalation(ctx, coordinatorJobID)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("job %s has no pending human escalation", coordinatorJobID)
	}
	resolved, err := e.escalationResolved(ctx, coordinatorJobID)
	if err != nil {
		return err
	}
	if resolved {
		// Already answered: idempotent no-op so a duplicate comment poll cannot
		// re-run the verb (which could double-enqueue a leg or continuation).
		return nil
	}

	// Verb/round-kind gating (#445): the ask-gate's `answer` verb is valid ONLY on
	// an ask round, and the failure verbs (retry/continue/abort) ONLY on a failure
	// escalation round. A mismatch is a clear, side-effect-free error so a human who
	// sends the wrong verb gets a precise ack and the pause stays intact.
	isAskRound := rec.Kind == escalationKindAsk
	if verb == ResumeAnswer && !isAskRound {
		return fmt.Errorf("job %s is paused on a failure escalation, not an ask; resume it with retry|continue|abort, not answer", coordinatorJobID)
	}
	if verb != ResumeAnswer && isAskRound {
		return fmt.Errorf("job %s is awaiting a human answer; resume it with `answer \"<id>: ...\"`, not %s", coordinatorJobID, verb)
	}

	parentJob, parentPayload, err := e.jobPayload(ctx, coordinatorJobID)
	if err != nil {
		return err
	}
	if parentPayload.Result == nil {
		return fmt.Errorf("job %s has no result to resume", coordinatorJobID)
	}
	ref := taskRefFromPayload(parentPayload)

	// answers is populated only on the ask-gate `answer` verb; it is recorded on the
	// resolution event (parsed + any unmatched ids) and threaded into the continuation.
	var answers map[string]string

	switch verb {
	case ResumeRetry:
		if err := e.resumeRetryLeg(ctx, parentJob, parentPayload, rec, instructions); err != nil {
			return err
		}
	case ResumeContinue:
		children, err := e.childDelegationJobs(ctx, parentJob.ID)
		if err != nil {
			return err
		}
		if err := e.maybeEnqueueContinuation(ctx, parentJob, parentPayload, parentPayload.Result, children, ref); err != nil {
			return err
		}
	case ResumeAbort:
		reason := "human aborted the escalation"
		if strings.TrimSpace(instructions) != "" {
			reason = "human aborted the escalation: " + strings.TrimSpace(instructions)
		}
		if err := e.enqueueFinalizeContinuation(ctx, parentJob, parentPayload, reason); err != nil {
			return err
		}
	case ResumeAnswer:
		answers = parseHumanAnswers(rec.Questions, instructions)
		if err := e.resumeAnswerLeg(ctx, parentJob, parentPayload, ref, rec, answers); err != nil {
			return err
		}
	}

	// Clear the pause: move the task out of awaiting_human. retry/continue re-arm
	// the delegation machinery (the next child completion advances the DAG, or the
	// continuation runs), so move to reviewing-ish "implementing" intent; abort's
	// finalize continuation will settle the task itself. We use planned as a
	// neutral non-terminal state so the dashboard stops listing it under Attention.
	if err := e.setTaskState(ctx, ref, TaskPlanned); err != nil {
		return err
	}

	resolution := EscalationRecord{
		DelegationID: rec.DelegationID,
		ChildJobID:   rec.ChildJobID,
		Reason:       string(verb),
		Question:     strings.TrimSpace(instructions),
		PausedAt:     e.now().UTC().Format(time.RFC3339), // reused as resolved_at
		Kind:         rec.Kind,
		Answers:      answers,
	}
	message := string(verb)
	if encoded, marshalErr := json.Marshal(resolution); marshalErr == nil {
		message = string(encoded)
	}
	return e.Store.AddJobEvent(ctx, db.JobEvent{
		JobID:   coordinatorJobID,
		Kind:    escalationResolvedEvent,
		Message: message,
	})
}

// resumeRetryLeg re-enqueues the failing delegation leg of a paused tree with the
// human's instructions folded into its prompt, under a deterministic resume id so
// a duplicate resume cannot double-enqueue it. It is the retry verb's worker.
func (e Engine) resumeRetryLeg(ctx context.Context, parentJob db.Job, parentPayload JobPayload, rec EscalationRecord, instructions string) error {
	d, ok := findDelegation(parentPayload.Result.Delegations, rec.DelegationID)
	if !ok {
		return fmt.Errorf("escalated delegation %q not found on job %s", rec.DelegationID, parentJob.ID)
	}
	if instr := strings.TrimSpace(instructions); instr != "" {
		d.Prompt = strings.TrimSpace(d.Prompt) + "\n\nHuman guidance on resume: " + instr
	}
	artifactDir, err := delegationArtifactDir(e.ArtifactRoot, parentJob.ID, parentPayload.Result)
	if err != nil {
		return err
	}
	request := e.delegationRequest(parentJob, parentPayload, d)
	request.ID = parentJob.ID + "/delegation/" + d.ID + "/resume"
	request.Instructions = strings.TrimSpace(d.Prompt)
	request.DelegationArtifactDir = artifactDir
	if err := e.allocateAndEnqueueDelegation(ctx, parentJob, parentPayload, d, request, taskRefFromPayload(parentPayload)); err != nil {
		return err
	}
	return e.Store.AddJobEvent(ctx, db.JobEvent{
		JobID:   parentJob.ID,
		Kind:    "delegation_escalation_retry",
		Message: fmt.Sprintf("human resume retry re-enqueued delegation %q as job %s", d.ID, request.ID),
	})
}

// parseHumanAnswers parses the human's free-form `answer` instructions into an
// id->answer map (#445). The instruction body is multi-line; each line of the
// form "<id>: <text>" maps the answer to the matching question id. An id that
// does not match any open question is recorded under its literal key so it is
// surfaced (never silently dropped) rather than failing the resume. When the body
// has no recognizable "<id>:" prefix at all AND there is exactly one open
// question, the whole body is taken as that question's answer (the common
// single-question convenience). Returns nil when nothing parses, so the
// resolution event omits the answers map.
func parseHumanAnswers(questions []HumanQuestion, instructions string) map[string]string {
	known := make(map[string]struct{}, len(questions))
	for _, q := range questions {
		known[strings.TrimSpace(q.ID)] = struct{}{}
	}
	answers := make(map[string]string)
	matchedAny := false
	lastID := ""
	for _, line := range strings.Split(instructions, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if idx := strings.Index(line, ":"); idx > 0 {
			id := strings.TrimSpace(line[:idx])
			text := strings.TrimSpace(line[idx+1:])
			if id != "" {
				answers[id] = text
				lastID = id
				if _, ok := known[id]; ok {
					matchedAny = true
				}
				continue
			}
		}
		// A line with no "<id>:" prefix continues the most recently parsed answer
		// (a multi-line answer body); with no answer yet it is dropped (the
		// single-question convenience below covers a prefix-less single answer).
		if lastID != "" {
			answers[lastID] = strings.TrimSpace(answers[lastID] + "\n" + line)
		}
	}
	// Single-question convenience: if nothing matched a known id and there is exactly
	// one question, treat the whole body as that question's answer.
	if !matchedAny && len(questions) == 1 {
		if body := strings.TrimSpace(instructions); body != "" {
			return map[string]string{strings.TrimSpace(questions[0].ID): body}
		}
	}
	if len(answers) == 0 {
		return nil
	}
	return answers
}

// resumeAnswerLeg enqueues the coordinator continuation carrying the human's
// answer(s) to an ask-gate pause (#445). It mirrors the ResumeContinue path
// (maybeEnqueueContinuation under the deterministic continuation id, idempotent
// via the continuationEnqueued guard) but threads the parsed answers into the
// continuation request's HumanAnswer field so buildContinuationPrompt renders a
// clearly-labelled "Human answers to your questions" block at the top of the
// coordinator's continuation prompt. It is the `answer` verb's worker.
func (e Engine) resumeAnswerLeg(ctx context.Context, parentJob db.Job, parentPayload JobPayload, ref taskRef, rec EscalationRecord, answers map[string]string) error {
	children, err := e.childDelegationJobs(ctx, parentJob.ID)
	if err != nil {
		return err
	}
	answerBlock := renderHumanAnswerBlock(rec.Questions, answers)
	// The ask-gate short-circuits AdvanceJob BEFORE dispatchDelegations (engine.go
	// ask-gate block), so an asking result's delegations[] were NEVER dispatched —
	// no children exist for them (#445). Feeding the un-dispatched delegations into
	// maybeEnqueueContinuation would (a) make the vote/quorum synthesis gates fail
	// (every delegation id is missing from the empty children map) and block the
	// parent — silently losing the human's answer — and (b) make a verify
	// synthesis_rule emit a misleading replan continuation. It would also render
	// each delegation as "not enqueued (dependencies unmet)" in the continuation
	// prompt, which is wrong: they were never attempted. Resume the coordinator from
	// a copy of its result with Delegations cleared, so the answer-driven
	// continuation always enqueues and the coordinator decides fresh (it may
	// re-issue the same delegations now that it has the answer).
	resumeResult := *parentPayload.Result
	resumeResult.Delegations = nil
	if err := e.maybeEnqueueContinuation(ctx, parentJob, parentPayload, &resumeResult, children, ref, withHumanAnswer(answerBlock)); err != nil {
		return err
	}
	return e.Store.AddJobEvent(ctx, db.JobEvent{
		JobID:   parentJob.ID,
		Kind:    "delegation_ask_answered",
		Message: fmt.Sprintf("human answered %d ask-gate question(s) for job %s", len(rec.Questions), parentJob.ID),
	})
}

// renderHumanAnswerBlock renders the human's answers (#445) as a stable,
// id-ordered block keyed by each original question, so the coordinator
// continuation reads exactly which question got which answer. Questions with no
// answer are shown as "(no answer provided)"; any answer keyed to an unknown id
// (a typo the human made) is appended under an "unmatched" section so it is
// surfaced, never silently dropped.
func renderHumanAnswerBlock(questions []HumanQuestion, answers map[string]string) string {
	if len(questions) == 0 && len(answers) == 0 {
		return ""
	}
	var b strings.Builder
	matched := make(map[string]struct{}, len(answers))
	for _, q := range questions {
		id := strings.TrimSpace(q.ID)
		fmt.Fprintf(&b, "- %s — %s\n", id, strings.TrimSpace(q.Prompt))
		if ans, ok := answers[id]; ok {
			matched[id] = struct{}{}
			fmt.Fprintf(&b, "  answer: %s\n", strings.TrimSpace(ans))
		} else {
			b.WriteString("  answer: (no answer provided)\n")
		}
	}
	var unmatched []string
	for id := range answers {
		if _, ok := matched[id]; !ok {
			unmatched = append(unmatched, id)
		}
	}
	if len(unmatched) > 0 {
		sort.Strings(unmatched)
		b.WriteString("Unmatched answer ids (no such question; treat as additional human guidance):\n")
		for _, id := range unmatched {
			fmt.Fprintf(&b, "- %s: %s\n", id, strings.TrimSpace(answers[id]))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// findDelegation returns the delegation with the given id from a result's set.
func findDelegation(delegations []Delegation, id string) (Delegation, bool) {
	for _, d := range delegations {
		if d.ID == id {
			return d, true
		}
	}
	return Delegation{}, false
}

// AutoFinalizeExpiredEscalations scans for trees paused awaiting a human past the
// TTL and gracefully finalizes them (#340): a never-answered escalation must not
// strand a tree forever. For each coordinator with an unresolved
// delegation_escalation_requested whose paused_at is older than ttl, it routes to
// the #305 finalize continuation (synthesize what completed), clears
// TaskAwaitingHuman, and records a delegation_escalation_resolved event tagged
// "ttl" (so wall-clock pause accounting closes too). ttl <= 0 disables the scan.
// It returns the number of trees finalized. Idempotent: an already-resolved
// escalation is skipped, and the finalize continuation has a deterministic id.
func (e Engine) AutoFinalizeExpiredEscalations(ctx context.Context, ttl time.Duration) (int, error) {
	if err := e.validate(); err != nil {
		return 0, err
	}
	if ttl <= 0 {
		return 0, nil
	}
	// BOUNDED (#598): iterate ONLY the coordinators with an open escalation round
	// (requested > resolved) instead of listing EVERY job and re-reading each one's
	// full event history twice. Zero candidates => the loop body never runs, so this
	// is an immediate return with no per-job GetJob/ListJobEvents on the overwhelming
	// common case (no tree paused awaiting a human). The retained per-candidate
	// exists/resolved gates below are always-pass for a candidate but kept verbatim,
	// both to defend against an event added between this query and the walk and to
	// keep the finalization logic byte-identical.
	jobIDs, err := e.Store.JobIDsWithOpenEscalation(ctx)
	if err != nil {
		return 0, err
	}
	now := e.now().UTC()
	finalized := 0
	for _, jobID := range jobIDs {
		// A candidate escalation event whose job row was pruned would make the
		// payload load below fail; skip it here on sql.ErrNoRows, mirroring the
		// reclaim/retry bounded passes (#598). Unreachable today (nothing prunes
		// jobs), but keeps the pattern and the PollOnce caller robust to a future
		// job-pruning feature. The fetched job is reused for the payload below, so a
		// finalized candidate costs a single GetJob rather than the old
		// GetJob-then-jobPayload double read (#615 review).
		job, err := e.Store.GetJob(ctx, jobID)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return finalized, err
		}
		rec, exists, err := e.loadEscalation(ctx, jobID)
		if err != nil {
			return finalized, err
		}
		if !exists {
			continue
		}
		resolved, err := e.escalationResolved(ctx, jobID)
		if err != nil {
			return finalized, err
		}
		if resolved {
			continue
		}
		pausedAt, perr := parseJobTimestamp(rec.PausedAt)
		if perr != nil {
			// Without a parseable paused_at we cannot age the pause; skip rather than
			// finalize prematurely.
			continue
		}
		if now.Sub(pausedAt) < ttl {
			continue
		}
		payload, err := unmarshalPayload(job.Payload)
		if err != nil {
			return finalized, err
		}
		if payload.Result == nil {
			continue
		}
		reason := fmt.Sprintf("escalation TTL of %s elapsed with no human response", ttl)
		if err := e.enqueueFinalizeContinuation(ctx, job, payload, reason); err != nil {
			return finalized, err
		}
		if err := e.setTaskState(ctx, taskRefFromPayload(payload), TaskPlanned); err != nil {
			return finalized, err
		}
		resolution := EscalationRecord{
			DelegationID: rec.DelegationID,
			ChildJobID:   rec.ChildJobID,
			Reason:       "ttl",
			PausedAt:     now.Format(time.RFC3339), // reused as resolved_at
		}
		message := "ttl"
		if encoded, marshalErr := json.Marshal(resolution); marshalErr == nil {
			message = string(encoded)
		}
		if err := e.Store.AddJobEvent(ctx, db.JobEvent{
			JobID:   jobID,
			Kind:    escalationResolvedEvent,
			Message: message,
		}); err != nil {
			return finalized, err
		}
		finalized++
	}
	return finalized, nil
}
