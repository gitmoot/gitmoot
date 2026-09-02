package workflow

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	// escalationEffectsCompletedEvent is the RECEIPT: the claim's second half landed.
	//
	// The claim is committed BEFORE the verb's irreversible effects so only one
	// resolver runs them. A crash in between would otherwise strand the tree, so a
	// claim without a receipt is an UNFINISHED resolution that the recovery sweep
	// re-drives from the round's stored claim (#1673).
	escalationEffectsCompletedEvent = "delegation_escalation_effects_completed"
	// escalationNeedsRepairEvent is the ONE signal a parked round emits. Its
	// exactly-once-ness comes from the affected-row predicate on the state transition,
	// not from counting events.
	escalationNeedsRepairEvent = "delegation_escalation_needs_repair"
	// escalationRepairedEvent and escalationSupersededEvent are the two operator repair
	// arms' durable traces. Supersede is the ONLY path that discards a claimed human
	// decision, and it carries the operator and their reason.
	escalationRepairedEvent   = "delegation_escalation_repair_retried"
	escalationSupersededEvent = "delegation_escalation_repair_superseded"
	// escalationReleasedEvent records a Class I no-op release: the coordinator row is
	// gone, so the round can never be replayed by anyone.
	escalationReleasedEvent = "delegation_escalation_released"
)

// escalationRecoveryAttemptBound is how many replay attempts a claimed round gets
// before the sweep STOPS RETRYING AND ASKS A HUMAN. It is deliberately larger than
// one poll's worth of transient failure (a lock, a dependency outage) and it can
// never discard a claim: exhaustion routes to needs_repair, which PRESERVES the
// decision and holds the slot, so every value of this constant is safe.
const escalationRecoveryAttemptBound = 5

// escalationReleaseReasonCoordinatorGone is the named reason for the only settlement
// the engine may perform without applying effects.
const escalationReleaseReasonCoordinatorGone = "coordinator_row_absent"

// newEscalationRoundID mints a round's durable identity. It must be unique per
// coordinator over time; it is never parsed, only compared. Identity is for PAIRING
// a round's request, claim and receipt — exclusion is the job-level slot, never this.
func newEscalationRoundID() string {
	var tail [8]byte
	if _, err := rand.Read(tail[:]); err != nil {
		// A failed read must never fail a pause: the timestamp alone still separates
		// rounds on one coordinator, which is the only uniqueness required here.
		return strconv.FormatInt(time.Now().UTC().UnixNano(), 36)
	}
	return strconv.FormatInt(time.Now().UTC().UnixNano(), 36) + "-" + hex.EncodeToString(tail[:])
}

// escalationTTLPreClaimHook fires between the TTL sweep's candidate selection and
// its resolution claim — the exact window in which a human resume lands, and the
// only place a test can make that race deterministic. Nil in production.
var escalationTTLPreClaimHook func(ctx context.Context, jobID string)

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
	// RoundID is the round's DURABLE IDENTITY, minted by the opener and echoed by its
	// claim and receipt (#1673). It PAIRS a round's three phases so recovery can never
	// join one round's request to another round's resolution. It is NOT exclusion:
	// exclusion is the coordinator's unsettled-round slot, because two concurrent
	// openers mint different ids and an identity-scoped predicate would pass both.
	//
	// Absent on every event written before this protocol, which is why a legacy round
	// has no escalation_rounds row and can never enter recovery.
	RoundID string `json:"round_id,omitempty"`
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

	// The pre-check is a FAST PATH ONLY: the authoritative exclusion is the
	// coordinator's unsettled-round slot, taken inside the round-open transaction. A
	// slot is held from OPEN through SETTLEMENT, so it also covers a claimed round
	// whose effects have not landed - a stale replay must never be able to clear a
	// newer round's live pause (#1673).
	open, err := e.escalationOpen(ctx, parentJob.ID)
	if err != nil {
		return err
	}
	if open {
		return awaitErr
	}

	record := EscalationRecord{
		DelegationID: d.ID,
		ChildJobID:   child.ID,
		Reason:       reason,
		Question:     strings.TrimSpace(d.Prompt),
		PausedAt:     e.now().UTC().Format(time.RFC3339),
	}
	_, announce, err := e.openHumanRound(ctx, ref, parentJob.ID, "", record, func(rec EscalationRecord) string {
		if encoded, marshalErr := json.Marshal(rec); marshalErr == nil {
			return string(encoded)
		}
		return awaitErr.Reason
	})
	if err != nil {
		return err
	}
	if !announce {
		// A sibling holds the slot, or the pause was refused and rolled back with it.
		// The coordinator is still parked from this leg's point of view, but nothing
		// here may call a human about a round this caller did not open.
		return awaitErr
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

	// Open a FRESH ask round: pause the (coordinator's) task and record the requested
	// event with the questions AS ONE DURABLE OPERATION, then notify best-effort and
	// emit job.needs_attention once. All keyed on targetID/targetRef so a child's ask
	// routes to its coordinator.
	//
	// Same shape and same fix as pauseAwaitingHuman above (#1673): the two writes are
	// atomic, and announcements strictly follow their commit.
	questions := payload.Result.HumanQuestions
	record := EscalationRecord{
		Reason:    fmt.Sprintf("%d human question(s) awaiting an answer", len(questions)),
		Question:  renderHumanQuestions(questions),
		PausedAt:  e.now().UTC().Format(time.RFC3339),
		Kind:      escalationKindAsk,
		Questions: questions,
	}
	_, announce, err := e.openHumanRound(ctx, targetRef, targetID, escalationKindAsk, record, func(rec EscalationRecord) string {
		if encoded, marshalErr := json.Marshal(rec); marshalErr == nil {
			return string(encoded)
		}
		return rec.Reason
	})
	if err != nil {
		return false, err
	}
	if !announce {
		// A sibling ask opened this round, or the pause was refused and rolled back
		// with its event. The gate still reports "paused" so the caller does not
		// redispatch, but this caller announces nothing.
		return true, nil
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
	// ResumeTTL is the auto-finalize verb the TTL sweep claims a round with. It is
	// REPLAY-ONLY: validResumeDecision rejects it, so an operator cannot type it,
	// while the recovery sweep replays it through the SAME applyResolutionEffects
	// switch as the human verbs. Before this, a crashed TTL claim was unrecoverable
	// because "ttl" was not a decision anything could replay (#1673).
	ResumeTTL ResumeDecision = "ttl"
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

// validReplayableResumeDecision accepts every verb the recovery sweep may replay,
// which is the operator set PLUS ResumeTTL. Operator input goes through
// validResumeDecision, so "ttl" stays unusable as a resume verb.
func validReplayableResumeDecision(decision string) (ResumeDecision, bool) {
	if ResumeDecision(strings.ToLower(strings.TrimSpace(decision))) == ResumeTTL {
		return ResumeTTL, true
	}
	return validResumeDecision(decision)
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
	if verb == ResumeAnswer {
		answers = parseHumanAnswers(rec.Questions, instructions)
	}

	// CLAIM THE RESOLUTION FIRST, by ROUND IDENTITY, THEN act (#1673). The pre-checks
	// above are a fast path only: a human resume racing TTL auto-finalization, or two
	// concurrent resume callers, both read the round as open. Whoever loses the claim
	// UPDATE must not run the irreversible effects below.
	//
	// The claim does NOT release the coordinator's slot - the receipt does, after the
	// effects land - so a crash in between cannot let a newer round open and be
	// clobbered by this round's replay.
	round, hasRound, err := e.adoptOrLoadUnsettledRound(ctx, coordinatorJobID, rec)
	if err != nil {
		return err
	}
	if !hasRound || round.Claimed() {
		// Nothing open to claim (or already claimed): idempotent no-op, exactly as a
		// duplicate resume comment was always meant to be.
		return nil
	}
	if round.NeedsRepair() {
		return fmt.Errorf("job %s escalation round %s needs operator repair (%s); resolve it with `gitmoot escalation repair`",
			coordinatorJobID, round.RoundID, round.IntegrityCause)
	}
	resolution := EscalationRecord{
		DelegationID: rec.DelegationID,
		ChildJobID:   rec.ChildJobID,
		Reason:       string(verb),
		Question:     strings.TrimSpace(instructions),
		PausedAt:     e.now().UTC().Format(time.RFC3339), // reused as resolved_at
		Kind:         rec.Kind,
		Answers:      answers,
		RoundID:      round.RoundID,
	}
	message := string(verb)
	if encoded, marshalErr := json.Marshal(resolution); marshalErr == nil {
		message = string(encoded)
	}
	claimed, err := e.Store.CloseHumanRound(ctx, coordinatorJobID, round.RoundID, string(verb),
		parentJob.LifecycleGeneration, message, db.JobEvent{
			JobID:   coordinatorJobID,
			Kind:    escalationResolvedEvent,
			Message: message,
		}, time.Now().UTC())
	if err != nil {
		return err
	}
	if !claimed {
		return nil
	}

	// EXCLUSIVE OWNERSHIP THROUGH EFFECT COMMIT: take the fence, run the effects under
	// it, and commit them with the receipt in one transaction (#1673).
	owner := newEscalationRoundID()
	now := time.Now().UTC()
	held, err := e.Store.AcquireEscalationRecoveryLease(ctx, coordinatorJobID, round.RoundID, owner,
		now.Add(escalationRecoveryLeaseTTL), now)
	if err != nil {
		return err
	}
	if !held {
		// Another pass owns this replay; its effects are the same idempotent set.
		return nil
	}
	defer func() { // Explicitly rejected: see ReleaseEscalationRecoveryLease - zero rows means the
		// lease is already gone.
		_, _ = e.Store.ReleaseEscalationRecoveryLease(ctx, coordinatorJobID, round.RoundID, owner)
	}()
	return e.applyResolutionEffectsFenced(ctx, parentJob, parentPayload, ref, rec, verb, instructions, answers, round.RoundID, owner)
}

// adoptOrLoadUnsettledRound returns the coordinator's unsettled round, ADOPTING a
// legacy open round on first touch: escalations that predate escalation_rounds have
// no row, and adoption is what lets them be resolved by the one mechanism instead of
// a second legacy code path. The partial unique index makes adoption idempotent.
func (e Engine) adoptOrLoadUnsettledRound(ctx context.Context, coordinatorJobID string, rec EscalationRecord) (db.EscalationRound, bool, error) {
	round, ok, err := e.Store.UnsettledEscalationRound(ctx, coordinatorJobID)
	if err != nil || ok {
		return round, ok, err
	}
	// No row. That is the NORMAL state for a coordinator with nothing open, so
	// adoption must not fire on it: minting a round here would let a second resolver
	// claim a round nobody opened, which is how the full suite caught a duplicate
	// resolution. Adopt ONLY on positive evidence of a PRE-UPGRADE open round: its
	// request record carries no RoundID, because every post-upgrade request does.
	legacy, hasLegacy, err := e.legacyOpenEscalation(ctx, coordinatorJobID)
	if err != nil || !hasLegacy {
		return db.EscalationRound{}, false, err
	}
	if _, err := e.Store.AdoptLegacyEscalationRound(ctx, coordinatorJobID, newEscalationRoundID(), legacy.Kind, time.Now().UTC()); err != nil {
		return db.EscalationRound{}, false, err
	}
	return e.Store.UnsettledEscalationRound(ctx, coordinatorJobID)
}

// escalationRecoveryLeaseTTL bounds how long a recovery fence is held before another
// pass may reclaim it. Expiry transfers OWNERSHIP ONLY: it never settles, never
// discards, and never touches the preserved claim, so it cannot become the
// abandonment path rejected in v3.
// escalationRecoveryLeaseTTL bounds how long a recovery fence is held before another
// pass may reclaim it. A var, not a const, so a test can shrink it and drive the
// heartbeat that keeps unbounded git work fenced (#1673).
var escalationRecoveryLeaseTTL = 2 * time.Minute

// escalationPreRenewHook fires between the fence acquisition and the renewal that
// covers the pre-effects, which is the one window a test cannot otherwise reach
// deterministically. Nil in production (#1673).
var escalationPreRenewHook func(ctx context.Context, jobID string, roundID string)

// deadlineFor returns the instant after which a renewal call has nothing useful left to
// confirm: the expiry it is trying to establish. It is passed as a context deadline on a
// BEST-EFFORT basis - a driver that honours cancellation will release the goroutine
// there - but it is NOT a bound, because the SQLite driver does not interrupt an UPDATE
// waiting on another connection's write lock. Authority is bounded by the heartbeat's
// expiry timer and shutdown by its own timer; neither depends on this (#1673).
func deadlineFor(confirmedUntil time.Time) time.Time {
	if confirmedUntil.Before(time.Now().UTC().Add(50 * time.Millisecond)) {
		// Always leave a little room, so a deadline that has effectively arrived does not
		// cancel the call before it can even be issued.
		return time.Now().UTC().Add(50 * time.Millisecond)
	}
	return confirmedUntil
}

// escalationRenewFaultHook injects a renewal STORE FAILURE into the heartbeat, which is
// the only way to reach the expiry-bound cancellation through the production loop rather
// than by calling a helper. It returns the error the renewal should report; a nil return
// lets the real renewal run. Nil in production (#1673).
var escalationRenewFaultHook func(attempt int) error

// applyResolutionEffectsFenced is the authorized shape (#1673, design note v6 +
// A-NARROW): acquire the fence, run the pre-effects UNDER it, then commit every
// durable database write AND the receipt in ONE transaction that re-validates the
// fence, and only then announce.
//
// The order is load-bearing. The fence comes first so two recoverers can never both
// allocate a worktree or both take a branch lock - an idempotent worktree key does not
// protect a lock that has an owner. The receipt is inside the transaction so there is
// no "effect committed, receipt missing" state for a crash to land in, which is what a
// per-effect boundary could not provide.
func (e Engine) applyResolutionEffectsFenced(ctx context.Context, parentJob db.Job, parentPayload JobPayload, ref taskRef, rec EscalationRecord, verb ResumeDecision, instructions string, answers map[string]string, roundID string, owner string) error {
	sink := &resolutionEffectSink{}
	capturing := e
	capturing.resolutionSink = sink

	// PRE-EFFECTS AND PREPARATION run here, under the held fence. Everything durable
	// they would write is captured instead; the git/lock work is real and is recorded
	// on the round so a later supersede or release can hand it back.
	//
	// The lease is RENEWED first: git work has no bound, and a fixed lease that expires
	// mid-allocation would let a second recoverer take ownership while this pass is
	// still creating resources (#1673).
	// escalationPreRenewHook is the only place a test can land an ownership change in the
	// exact window between taking the fence and renewing it for the pre-effects. Nil in
	// production.
	if escalationPreRenewHook != nil {
		escalationPreRenewHook(ctx, parentJob.ID, roundID)
	}
	// THE PERSISTED EXPIRY IS THE ONE THAT COUNTS. This value is what the store WRITES,
	// so authority is bound to it rather than to a second clock reading taken after the
	// call returns - a slow database call or a scheduler pause would otherwise leave the
	// local deadline LATER than the row's, and another recoverer could legitimately
	// acquire at the row's expiry while this pass believed it still held the fence
	// (#1673).
	preEffectUntil := time.Now().UTC().Add(escalationRecoveryLeaseTTL)
	if renewed, err := e.Store.RenewEscalationRecoveryLease(ctx, parentJob.ID, roundID, owner,
		preEffectUntil, time.Now().UTC()); err != nil {
		return err
	} else if !renewed {
		// Lost between the fence and here: apply nothing.
		return nil
	} else if !time.Now().UTC().Before(preEffectUntil) {
		// The write landed, but so late that the expiry it persisted has already
		// elapsed. Accepting it as confirmed would claim authority the row no longer
		// grants.
		return nil
	}
	// A HEARTBEAT, NOT A ONE-SHOT RENEWAL. Git work has no bound, and a single fixed
	// extension is a bet on how long it takes: if allocation outlives the lease, another
	// recoverer takes ownership and runs the SAME pre-effects concurrently, and
	// RecordEscalationRoundPreEffects only notices after the external work finished -
	// too late to have preserved single-owner pre-effects at all (#1673).
	//
	// The heartbeat renews while the work runs and CANCELS the work's context the moment
	// ownership is genuinely lost, so a losing pass stops mid-flight instead of
	// completing effects it no longer owns.
	// THE LAST CONFIRMED EXPIRY. A renewal ERROR is not proof of loss, so the heartbeat
	// retries it - but retrying forever is a bet that the store recovers before the lease
	// lapses, and if it does not, another recoverer legitimately takes the fence while
	// this pass is still creating worktrees and locks. Commit-time validation would stop
	// the database write; it cannot undo an overlapping external effect.
	//
	// So authority is bounded by what this pass can PROVE it holds: the expiry it last
	// confirmed. A retry loop that ignores that boundary is not a retry policy, it is an
	// unbounded extension of authority the store never granted (#1673).
	confirmedUntil := preEffectUntil
	effectCtx, stopHeartbeat := context.WithCancel(ctx)
	// ownershipLost is set ONLY by the heartbeat observing a genuine loss. It is a
	// separate signal from effectCtx.Err() on purpose: this function cancels effectCtx
	// itself once the effects return, so reading the context's error as "lost" would
	// classify every successful run as a loss and silently discard its commit.
	var ownershipLost atomic.Bool
	// renewalsInFlight tracks the renewal goroutines so shutdown can TRY to await them.
	// It is a courtesy, not a guarantee, and the distinction is measured: the SQLite
	// driver does NOT interrupt an UPDATE waiting on another connection's write lock at
	// its context deadline - a renewal with a 120ms deadline was observed returning after
	// ~11.7s, bounded by the 15s busy timeout instead. So the wait below is bounded by
	// OUR timer and may ABANDON a renewal still in flight, which is safe only because
	// every value such a goroutine touches is captured below (#1673).
	var renewalsInFlight sync.WaitGroup
	// EVERY PACKAGE-LEVEL VALUE THE HEARTBEAT NEEDS IS CAPTURED HERE, on the caller's
	// goroutine, before anything starts. The heartbeat and its renewals then read no
	// package state at all - which is required because the shutdown wait below is bounded
	// and may give up on a goroutine that outlives this call (#1673).
	leaseTTL := escalationRecoveryLeaseTTL
	faultHook := escalationRenewFaultHook
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(leaseTTL / 3)
		defer ticker.Stop()
		// AUTHORITY EXPIRES BY THE CLOCK, NOT BY OBSERVATION. Checking the bound only
		// after a renewal RETURNS leaves the stalled case open: a hung store call keeps
		// the effect context live past the persisted expiry, another pass acquires the
		// reclaimable fence, and both run external pre-effects. The commit would reject
		// the stale pass's database write but cannot undo an overlapping worktree or
		// branch lock.
		//
		// So the deadline is armed as a TIMER and the renewal runs on its own goroutine.
		// Whatever the store does - return, error, or never come back - the pass stops at
		// the last expiry it could prove (#1673).
		expiry := time.NewTimer(time.Until(confirmedUntil))
		defer expiry.Stop()
		type renewOutcome struct {
			until time.Time
			held  bool
			err   error
		}
		renewals := make(chan renewOutcome, 1)
		attempt := 0
		inFlight := false
		for {
			select {
			case <-effectCtx.Done():
				return
			case <-expiry.C:
				// The lease this pass last confirmed has lapsed and nothing has renewed
				// it. Whether a renewal is still in flight is irrelevant: the row is
				// reclaimable now.
				ownershipLost.Store(true)
				stopHeartbeat()
				return
			case <-ticker.C:
				if inFlight {
					// A renewal is stalled. Do not pile on; the expiry timer owns the
					// deadline.
					continue
				}
				inFlight = true
				attempt++
				// The call carries a deadline as a BEST EFFORT, not as a bound. Authority is
				// enforced entirely by the expiry timer above, which is why a hung renewal
				// cannot extend it. The deadline's only job is to let a driver that DOES
				// honour cancellation release the goroutine early; measured on this
				// repository, an UPDATE blocked on another connection's write lock ignores
				// it, so nothing here may assume the call returns by that instant.
				renewCtx, cancelRenew := context.WithDeadline(context.WithoutCancel(ctx), deadlineFor(confirmedUntil))
				renewalsInFlight.Add(1)
				go func(attempt int, renewCtx context.Context, cancelRenew context.CancelFunc) {
					defer renewalsInFlight.Done()
					defer cancelRenew()
					until := time.Now().UTC().Add(leaseTTL)
					var held bool
					var err error
					if faultHook != nil {
						err = faultHook(attempt)
					}
					if err == nil {
						held, err = e.Store.RenewEscalationRecoveryLease(renewCtx, parentJob.ID, roundID, owner,
							until, time.Now().UTC())
					}
					renewals <- renewOutcome{until: until, held: held, err: err}
				}(attempt, renewCtx, cancelRenew)
			case outcome := <-renewals:
				inFlight = false
				if outcome.err != nil {
					// A transient failure is survivable; the expiry timer enforces the
					// bound if failures persist.
					continue
				}
				if !outcome.held {
					ownershipLost.Store(true)
					stopHeartbeat()
					return
				}
				if !time.Now().UTC().Before(outcome.until) {
					// A successful UPDATE that returned after the expiry it wrote grants
					// nothing: the row is already reclaimable.
					ownershipLost.Store(true)
					stopHeartbeat()
					return
				}
				confirmedUntil = outcome.until
				if !expiry.Stop() {
					select {
					case <-expiry.C:
					default:
					}
				}
				expiry.Reset(time.Until(confirmedUntil))
			}
		}
	}()
	verbErr := capturing.applyResolutionEffects(effectCtx, parentJob, parentPayload, ref, rec, verb, instructions, answers, roundID)
	stopHeartbeat()
	<-heartbeatDone
	// AWAIT THE RENEWAL, BUT BOUNDED - and by OUR clock, not by the store's goodwill.
	// A context deadline is not a real bound here: measured on this repository, an UPDATE
	// blocked behind another connection's write lock returned ~11.7s after a 120ms
	// deadline, because the SQLite driver does not interrupt a statement waiting on the
	// write lock (the busy timeout is 15s). Trusting the deadline would stall
	// ResolveEscalation and the daemon poll for that whole window.
	//
	// Ownership safety does not depend on this wait: the expiry timer has already
	// cancelled the pre-effects and marked ownership lost. The wait exists only to avoid
	// leaking a goroutine, so giving up on it is safe - the goroutine captured everything
	// it needs and writes only to a buffered channel nobody reads (#1673).
	renewalsSettled := make(chan struct{})
	go func() {
		renewalsInFlight.Wait()
		close(renewalsSettled)
	}()
	select {
	case <-renewalsSettled:
	case <-time.After(leaseTTL):
	}
	// Ownership lost while the pre-effects ran: the heartbeat cancelled them, so apply
	// nothing rather than committing a partially-run decision.
	if ownershipLost.Load() {
		return nil
	}
	// A RECORDED DECISION MUST REACH ITS TRANSACTION, even though it surfaces as an error.
	// Returning here on any non-nil error discarded a legitimate synthesis block: a
	// failed delegation under a vote/quorum rule blocks the parent task, blockTask
	// captured that transition and its event, and this branch then threw both away - the
	// task stayed awaiting_human, no block and no receipt landed, and recovery re-drove
	// the round until it parked. sink.blocked is deliberately nil for such a block,
	// because it is a decision the DAG recorded, not a refused allocation (#1673).
	//
	// So the early return is now for errors that produced NOTHING to commit. Anything
	// captured proceeds to the fenced commit, and the verb's error is returned after it.
	var blockedDecision BlockedError
	if verbErr != nil && sink.blocked == nil && !errors.As(verbErr, &blockedDecision) {
		// A GENUINE FAILURE - a crash, a transient store error - commits NOTHING. That
		// all-or-nothing property is the point of the fenced transaction, and an earlier
		// version of this guard committed captured writes on any error, which let an
		// injected crash land a job.
		return verbErr
	}
	if sink.preEffectWorktree != "" || sink.preEffectBranch != "" {
		recorded, err := e.Store.RecordEscalationRoundPreEffects(ctx, parentJob.ID, roundID, owner,
			sink.preEffectRepo, sink.preEffectBranch, sink.preEffectWorktree, sink.preEffectLockOwner,
			time.Now().UTC())
		if err != nil {
			return err
		}
		if !recorded {
			// OWNERSHIP LOST while the external work ran: apply nothing.
			//
			// AND RELEASE NOTHING. An earlier version handed the branch lock back here,
			// which was WORSE THAN LEAKING IT: the shared-checkout lock is owned by
			// request.Agent (engine_delegation.go), an identity that is STABLE ACROSS
			// PASSES rather than unique to the lease holder. So a replacement pass
			// acquires the same repo/branch/agent lock legitimately, and a stale pass
			// releasing "its" lock by that tuple deletes the lock the new pass believes
			// it holds - turning a bounded leak into a silent mutual-exclusion failure.
			//
			// Nothing is stranded by declining: the same-agent identity means the next
			// pass's AcquireLock succeeds, and the worktree key is idempotent so it is
			// re-used rather than duplicated. Handback would only be safe with a
			// pass-specific lock identity, which is a change to the lock's own contract
			// and not this PR's scope (#1673).
			return nil
		}
	}

	commit := db.ResolutionCommit{
		JobID:   parentJob.ID,
		RoundID: roundID,
		Owner:   owner,
		Jobs:    sink.jobs,
		Events:  sink.events,
	}
	if sink.taskSet {
		commit.Task = sink.task
		commit.TaskForbidden = sink.taskForbidden
		// The task_event travels with the transition it belongs to, captured by
		// blockTask, so the two can never land separately.
		commit.TaskEvent = sink.taskEvent
		commit.TaskEventValid = sink.taskEventValid && strings.TrimSpace(sink.task.ID) != ""
	}
	if sink.blocked != nil {
		// ALTERNATIVE OUTCOME: the decision could not be applied at all. The block and
		// its event land under the fence, the prepared work is DROPPED - a refused
		// decision must not dispatch anything - and NO receipt is written, so the claim
		// stays preserved and the next pass re-drives or parks rather than double-blocking.
		commit.Jobs = nil
	} else {
		commit.Receipt = db.JobEvent{
			JobID:   parentJob.ID,
			Kind:    escalationEffectsCompletedEvent,
			Message: roundID + " " + string(verb),
		}
		commit.ReceiptValid = true
	}
	committed, err := e.Store.CommitResolutionEffects(ctx, commit, time.Now().UTC())
	if err != nil {
		return err
	}
	if !committed {
		// The fence was lost - superseded, parked, or lapsed. Nothing landed.
		return nil
	}
	if sink.blocked != nil {
		return *sink.blocked
	}
	// The verb's own error is returned only AFTER its captured writes committed. A
	// legitimate block (a synthesis rule refusing the parent) arrives here: the block and
	// its event are durable, and the caller still learns the DAG's decision.
	return verbErr
}

// applyResolutionEffects runs the verb's irreversible half AFTER its resolution has
// been exclusively claimed. Under a capturing engine it WRITES NOTHING: its effects
// are collected for applyResolutionEffectsFenced to commit in one transaction.
//
// Every verb's effect is idempotent — deterministic resume ids, once-guarded
// continuations, and a task-state write that is a no-op when already planned — which
// is what makes re-driving safe.
func (e Engine) applyResolutionEffects(ctx context.Context, parentJob db.Job, parentPayload JobPayload, ref taskRef, rec EscalationRecord, verb ResumeDecision, instructions string, answers map[string]string, roundID string) error {
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
		if err := e.resumeAnswerLeg(ctx, parentJob, parentPayload, ref, rec, answers); err != nil {
			return err
		}
	case ResumeTTL:
		// The TTL verb's effect lives in the SAME switch as the human verbs rather than
		// inline in the sweep - that duplication is what made a crashed TTL claim
		// unrecoverable (#1673).
		if err := e.enqueueFinalizeContinuation(ctx, parentJob, parentPayload,
			"escalation TTL elapsed with no human response"); err != nil {
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
	if resolutionEffectsHook != nil {
		if err := resolutionEffectsHook(ctx, parentJob.ID); err != nil {
			return err
		}
	}
	if e.capturing() {
		// The receipt is committed by applyResolutionEffectsFenced, in the SAME
		// transaction as these effects. Writing it here would recreate the window v6
		// deletes.
		return nil
	}
	// The RECEIPT goes last and releases the coordinator's slot. Until it exists this
	// resolution is unfinished and the recovery sweep owns it; the affected-row
	// predicate makes it exactly-once, so concurrent recoverers cannot over-settle.
	settled, err := e.Store.SettleEscalationRound(ctx, parentJob.ID, roundID, "", "", time.Now().UTC())
	if err != nil {
		return err
	}
	if !settled {
		// Another recoverer settled it first: its effects were the same idempotent set.
		return nil
	}
	return e.Store.AddJobEvent(ctx, db.JobEvent{
		JobID:   parentJob.ID,
		Kind:    escalationEffectsCompletedEvent,
		Message: roundID + " " + string(verb),
	})
}

// resolutionEffectsHook fires after a resolution's effects and before its receipt —
// the only window in which a test can crash a claimed resolution and prove the
// recovery sweep re-drives it. Nil in production.
var resolutionEffectsHook func(ctx context.Context, jobID string) error

// RecoverUnfinishedEscalationResolutions re-drives resolutions that were CLAIMED but
// whose effects never finished. It is the counterpart of claim-before-act: without it
// one crash would strand a coordinator, because the slot stays held until settlement.
//
// Everything it reads is keyed by ROUND IDENTITY, so it can never pair one round's
// request with another round's resolution, and a round parked in needs_repair is
// skipped entirely — that state is terminal until an operator acts.
func (e Engine) RecoverUnfinishedEscalationResolutions(ctx context.Context) (int, error) {
	if err := e.validate(); err != nil {
		return 0, err
	}
	rounds, err := e.Store.UnfinishedEscalationRounds(ctx)
	if err != nil {
		return 0, err
	}
	recovered := 0
	for _, round := range rounds {
		done, err := e.recoverEscalationRound(ctx, round)
		if err != nil {
			return recovered, err
		}
		if done {
			recovered++
		}
	}
	return recovered, nil
}

func (e Engine) recoverEscalationRound(ctx context.Context, round db.EscalationRound) (bool, error) {
	// THE FENCE FIRST. Recovery must own the round through EFFECT COMMIT, and the
	// order is load-bearing: taking it before the pre-effects is what stops two
	// recoverers from both allocating a worktree or both taking a branch lock (#1673).
	owner := newEscalationRoundID()
	fenceNow := time.Now().UTC()
	held, err := e.Store.AcquireEscalationRecoveryLease(ctx, round.JobID, round.RoundID, owner,
		fenceNow.Add(escalationRecoveryLeaseTTL), fenceNow)
	if err != nil {
		return false, err
	}
	if !held {
		// Another pass owns it, or it was parked or settled since the candidate query.
		return false, nil
	}
	defer func() { // Explicitly rejected: a false here means the round already settled or moved on,
		// in which case the lease is gone anyway.
		_, _ = e.Store.ReleaseEscalationRecoveryLease(ctx, round.JobID, round.RoundID, owner)
	}()

	// CLASS I - STRUCTURALLY IMPOSSIBLE, and the only one: the coordinator row is
	// gone. With no coordinator there is no DAG to pause, no task to move and no
	// continuation to enqueue, so this claim can never be replayed by anyone, and
	// holding the slot would reserve exclusivity against rounds nothing can open.
	exists, err := e.Store.JobExists(ctx, round.JobID)
	if err != nil {
		return false, err
	}
	if !exists {
		return e.releaseAbsentCoordinatorRound(ctx, round)
	}

	// CLASS II - everything else PRESERVES the claim: effects_completed_at stays NULL,
	// so the slot stays held and no new round or ordinary advance may proceed until an
	// operator repairs or supersedes it.
	attempts, err := e.Store.RecordEscalationRoundAttempt(ctx, round.JobID, round.RoundID)
	if err != nil {
		return false, err
	}
	var resolution EscalationRecord
	if err := json.Unmarshal([]byte(round.ClaimPayload), &resolution); err != nil {
		return false, e.parkEscalationRound(ctx, round, owner, "claim_payload_unreadable")
	}
	verb, ok := validReplayableResumeDecision(round.ClaimVerb)
	if !ok {
		return false, e.parkEscalationRound(ctx, round, owner, "claim_verb_unreplayable")
	}
	job, payload, err := e.jobPayload(ctx, round.JobID)
	if err != nil {
		return false, err
	}
	if payload.Result == nil {
		return false, e.parkEscalationRound(ctx, round, owner, "coordinator_result_absent")
	}
	if round.ClaimGeneration != job.LifecycleGeneration {
		// The human decided about a run that has since been re-queued. Applying it
		// would be a stale effect; dropping it would lose intent. An operator decides.
		return false, e.parkEscalationRound(ctx, round, owner, "lifecycle_generation_moved")
	}
	rec, exists, err := e.loadEscalationForRound(ctx, round.JobID, round.RoundID)
	if err != nil {
		return false, err
	}
	if !exists {
		return false, e.parkEscalationRound(ctx, round, owner, "request_record_absent")
	}
	if err := e.applyResolutionEffectsFenced(ctx, job, payload, taskRefFromPayload(payload), rec, verb,
		resolution.Question, resolution.Answers, round.RoundID, owner); err != nil {
		if attempts >= escalationRecoveryAttemptBound {
			// The bound's ONLY role is to stop hammering and ask a human. It can never
			// discard the claim: this parks the round with the decision intact.
			return false, e.parkEscalationRound(ctx, round, owner, "retry_exhausted")
		}
		return false, err
	}
	return true, nil
}

// parkEscalationRound enters the terminal integrity state and emits the ONE repair
// signal, both under the affected-row predicate that makes the signal exactly-once.
func (e Engine) parkEscalationRound(ctx context.Context, round db.EscalationRound, owner string, cause string) error {
	// Parking requires the FENCE. That is what makes parking and an in-flight replay
	// mutually exclusive, and therefore what makes an operator supersede - which
	// requires a parked round - unable to race a replay.
	parked, err := e.Store.MarkEscalationRoundNeedsRepairAsOwner(ctx, round.JobID, round.RoundID, owner, cause, db.JobEvent{
		JobID:   round.JobID,
		Kind:    escalationNeedsRepairEvent,
		Message: fmt.Sprintf("round %s verb %s needs operator repair: %s", round.RoundID, round.ClaimVerb, cause),
	}, time.Now().UTC())
	if err != nil || !parked {
		return err
	}
	// A blocked coordinator must never be silent: the attention surface carries the
	// same fact the report does.
	e.emitEscalationRepairAttention(ctx, round, cause)
	return nil
}

// releaseAbsentCoordinatorRound is the Class I no-op release. It records the NAMED
// reason and a durable event, applies no effects, and frees the slot.
func (e Engine) releaseAbsentCoordinatorRound(ctx context.Context, round db.EscalationRound) (bool, error) {
	settled, err := e.Store.SettleEscalationRound(ctx, round.JobID, round.RoundID,
		escalationReleaseReasonCoordinatorGone, "engine", time.Now().UTC())
	if err != nil || !settled {
		return false, err
	}
	// The coordinator row is gone, so a job event keyed to it is the only trace
	// available; it is written best-effort for the audit and never gates the release.
	_ = e.Store.AddJobEvent(ctx, db.JobEvent{
		JobID:   round.JobID,
		Kind:    escalationReleasedEvent,
		Message: fmt.Sprintf("round %s released without effects: %s", round.RoundID, escalationReleaseReasonCoordinatorGone),
	})

	return false, nil
}

// emitEscalationRepairAttention makes a parked round OPERATOR-VISIBLE. A blocked
// coordinator must never be silent: this rides the same needs-attention seam the
// escalate/ask pauses use, so the dashboard's Attention section and any event
// consumer see the block and its cause without polling the table.
func (e Engine) emitEscalationRepairAttention(ctx context.Context, round db.EscalationRound, cause string) {
	job, payload, err := e.jobPayload(ctx, round.JobID)
	if err != nil {
		return
	}
	rootID := strings.TrimSpace(payload.RootJobID)
	if rootID == "" {
		rootID = job.ID
	}
	ev := events.NewEvent(
		events.EventJobNeedsAttention,
		job.ID,
		rootID,
		payload.Repo,
		EscalationRoundNeedsRepairState,
		fmt.Sprintf("escalation round %s (%s) needs operator repair: %s", round.RoundID, round.ClaimVerb, cause),
		e.now(),
		RedactCommentText,
	)
	ev.Cause = "escalation_needs_repair"
	events.EmitEvent(ctx, e.EventSink, ev)
}

// EscalationRoundNeedsRepairState is the state string the attention event carries for
// a parked round. It is deliberately NOT a task state: the task keeps whatever the
// round left it in, and this names the round's integrity state instead.
const EscalationRoundNeedsRepairState = "escalation_needs_repair"

// EscalationRepairReport is one row of the operator-facing blocked report.
type EscalationRepairReport struct {
	JobID   string
	RoundID string
	Verb    string
	Cause   string
}

// EscalationRoundsNeedingRepair is the blocked-with-cause report. It is what the
// operator surface prints, so a parked coordinator is discoverable without knowing
// the schema.
func (e Engine) EscalationRoundsNeedingRepair(ctx context.Context) ([]EscalationRepairReport, error) {
	if err := e.validate(); err != nil {
		return nil, err
	}
	rounds, err := e.Store.EscalationRoundsNeedingRepair(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]EscalationRepairReport, 0, len(rounds))
	for _, round := range rounds {
		out = append(out, EscalationRepairReport{
			JobID:   round.JobID,
			RoundID: round.RoundID,
			Verb:    round.ClaimVerb,
			Cause:   round.IntegrityCause,
		})
	}
	return out, nil
}

// RepairEscalationRound is the operator entry point's engine half. arm selects which
// repair the operator asked for; supersede is the ONLY path that discards a claimed
// decision, and it requires a reason.
func (e Engine) RepairEscalationRound(ctx context.Context, jobID string, roundID string, supersede bool, operator string, reason string) error {
	if err := e.validate(); err != nil {
		return err
	}
	if supersede {
		if strings.TrimSpace(reason) == "" {
			return errors.New("superseding a claimed escalation requires a reason: it discards a human decision")
		}

		_, err := e.Store.RepairSupersedeEscalationRound(ctx, jobID, roundID, reason, operator, time.Now().UTC(), db.JobEvent{
			JobID:   jobID,
			Kind:    escalationSupersededEvent,
			Message: fmt.Sprintf("round %s superseded by %s without applying effects: %s", roundID, operator, reason),
		})
		return err
	}
	_, err := e.Store.RepairRetryEscalationRound(ctx, jobID, roundID, db.JobEvent{
		JobID:   jobID,
		Kind:    escalationRepairedEvent,
		Message: fmt.Sprintf("round %s re-armed by %s; the stored claim is preserved and replays on the next sweep", roundID, operator),
	})
	return err
}

// loadEscalationForRound reads the REQUEST record for one round id. Recovery uses it
// instead of "the latest requested event", which could belong to a different round.
func (e Engine) loadEscalationForRound(ctx context.Context, jobID string, roundID string) (EscalationRecord, bool, error) {
	events, err := e.Store.ListJobEvents(ctx, jobID)
	if err != nil {
		return EscalationRecord{}, false, err
	}
	for _, event := range events {
		if event.Kind != escalationRequestedEvent {
			continue
		}
		var record EscalationRecord
		if err := json.Unmarshal([]byte(event.Message), &record); err != nil {
			continue
		}
		if record.RoundID == roundID {
			return record, true, nil
		}
	}
	return EscalationRecord{}, false, nil
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
	return e.recordEffectEvent(ctx, db.JobEvent{
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
	// Captured, not written: this event must land with the receipt, or a refused commit
	// leaves it behind while the continuation, task transition and settlement roll back -
	// and recovery appends it again on every attempt (#1673).
	return e.recordEffectEvent(ctx, db.JobEvent{
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
	// A resolution CLAIMED but never finished is repaired first, on every poll, and
	// independently of ttl: it is not a paused tree, it is a debt (#1673). Doing it
	// here rather than in a new sweep keeps it on the one path the daemon already
	// calls for escalation upkeep.
	if _, err := e.RecoverUnfinishedEscalationResolutions(ctx); err != nil {
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
		ref := taskRefFromPayload(payload)
		dismissedTaskID, dismissed, err := e.dismissedTaskForAdvancement(ctx, ref)
		if err != nil {
			return finalized, err
		}
		if dismissed {
			slog.Debug("workflow advancement skipped dismissed task",
				"task_id", dismissedTaskID,
				"job_id", jobID)
			continue
		}
		// CLAIM FIRST, by ROUND IDENTITY, through the SAME mechanism a human resume
		// uses: a TTL sweep racing a resume must not also finalize (#1673). The
		// candidate query above is a fast path; this claim UPDATE is the decision, and
		// it does not release the slot - the receipt does, after the effects land.
		round, hasRound, err := e.adoptOrLoadUnsettledRound(ctx, jobID, rec)
		if err != nil {
			return finalized, err
		}
		if !hasRound || round.Claimed() || round.NeedsRepair() {
			continue
		}
		resolution := EscalationRecord{
			DelegationID: rec.DelegationID,
			ChildJobID:   rec.ChildJobID,
			Reason:       string(ResumeTTL),
			PausedAt:     now.Format(time.RFC3339), // reused as resolved_at
			RoundID:      round.RoundID,
		}
		message := string(ResumeTTL)
		if encoded, marshalErr := json.Marshal(resolution); marshalErr == nil {
			message = string(encoded)
		}
		if escalationTTLPreClaimHook != nil {
			escalationTTLPreClaimHook(ctx, jobID)
		}
		claimed, err := e.Store.CloseHumanRound(ctx, jobID, round.RoundID, string(ResumeTTL),
			job.LifecycleGeneration, message, db.JobEvent{
				JobID:   jobID,
				Kind:    escalationResolvedEvent,
				Message: message,
			}, time.Now().UTC())
		if err != nil {
			return finalized, err
		}
		if !claimed {
			// A human claimed it between the candidate query and now.
			continue
		}
		// The verb's effects and its receipt commit in ONE fenced transaction, so a
		// crash here leaves either nothing or everything, and is replayed as ResumeTTL.
		ttlOwner := newEscalationRoundID()
		ttlNow := time.Now().UTC()
		ttlHeld, ttlErr := e.Store.AcquireEscalationRecoveryLease(ctx, jobID, round.RoundID, ttlOwner,
			ttlNow.Add(escalationRecoveryLeaseTTL), ttlNow)
		if ttlErr != nil {
			return finalized, ttlErr
		}
		if !ttlHeld {
			continue
		}
		if err := e.applyResolutionEffectsFenced(ctx, job, payload, ref, rec, ResumeTTL, "", nil, round.RoundID, ttlOwner); err != nil {
			_, _ = e.Store.ReleaseEscalationRecoveryLease(ctx, jobID, round.RoundID, ttlOwner)
			return finalized, err
		}
		_, _ = e.Store.ReleaseEscalationRecoveryLease(ctx, jobID, round.RoundID, ttlOwner)
		finalized++
	}
	return finalized, nil
}

// dismissedTaskForAdvancement checks both identities setTaskState can resolve:
// the payload task ID and the canonical task owning its repo branch. A dismissed
// task is terminal, so automatic advancement must skip it before enqueueing any
// continuation work.
func (e Engine) dismissedTaskForAdvancement(ctx context.Context, ref taskRef) (string, bool, error) {
	if strings.TrimSpace(ref.ID) != "" {
		task, err := e.Store.GetTask(ctx, ref.ID)
		if err == nil {
			if task.State == string(TaskDismissed) {
				return task.ID, true, nil
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return "", false, err
		}
	}
	if strings.TrimSpace(ref.Repo) == "" || strings.TrimSpace(ref.Branch) == "" {
		return "", false, nil
	}
	task, err := e.Store.GetTaskByRepoBranch(ctx, ref.Repo, ref.Branch)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return task.ID, task.State == string(TaskDismissed), nil
}

// escalationRepairBlock reports whether this advance must be refused because the
// coordinator it would settle carries a round parked in needs_repair.
//
// The parked round is on the COORDINATOR, so an advance of a delegation child checks
// its parent: that is the job whose continuation/task the stale claim would touch.
func (e Engine) escalationRepairBlock(ctx context.Context, job db.Job, ref taskRef) (error, bool, error) {
	coordinatorID := strings.TrimSpace(job.ParentJobID)
	if coordinatorID == "" {
		coordinatorID = job.ID
	}
	round, ok, err := e.Store.UnsettledEscalationRound(ctx, coordinatorID)
	if err != nil {
		return nil, false, err
	}
	if !ok || !round.NeedsRepair() {
		return nil, false, nil
	}
	reason := fmt.Sprintf("escalation round %s (%s) needs operator repair: %s; run `gitmoot escalation repair %s --round %s --retry` or `--supersede --reason ...`",
		round.RoundID, round.ClaimVerb, round.IntegrityCause, coordinatorID, round.RoundID)
	// EscalationRepairRequiredError, not a bare BlockedError: this refusal is CLEARABLE
	// by the operator, and a caller holding follow-up work must leave it outstanding
	// rather than settle it. It unwraps to BlockedError so the task block, the dashboard
	// and the daemon's classifier all keep matching (#1673).
	if strings.TrimSpace(ref.ID) == "" {
		return EscalationRepairRequiredError{Blocked: BlockedError{Reason: reason}}, true, nil
	}
	// Route it through the ordinary block choke point so the task carries the cause
	// and the dashboard shows it: a blocked coordinator must never be silent.
	blockErr := e.blockTask(ctx, ref, "escalation_needs_repair", reason, "escalation repair")
	var blocked BlockedError
	if errors.As(blockErr, &blocked) {
		return EscalationRepairRequiredError{Blocked: blocked}, true, nil
	}
	return blockErr, true, nil
}

// legacyOpenEscalation reports whether this coordinator has a PRE-UPGRADE open round:
// its latest requested event carries no RoundID, and the legacy requested/resolved
// counters say it is still open.
//
// Counting is acceptable here and ONLY here: it decides whether to ADOPT a historical
// round, never whether a claim, an effect or a receipt may proceed. Those are settled
// by round identity, which is what the counters could not express (#1673).
func (e Engine) legacyOpenEscalation(ctx context.Context, coordinatorJobID string) (EscalationRecord, bool, error) {
	events, err := e.Store.ListJobEvents(ctx, coordinatorJobID)
	if err != nil {
		return EscalationRecord{}, false, err
	}
	var latest EscalationRecord
	requested, resolved, found := 0, 0, false
	for _, event := range events {
		switch event.Kind {
		case escalationRequestedEvent:
			requested++
			var record EscalationRecord
			if err := json.Unmarshal([]byte(event.Message), &record); err == nil {
				latest = record
			} else {
				latest = EscalationRecord{}
			}
			found = true
		case escalationResolvedEvent:
			resolved++
		}
	}
	if !found || requested <= resolved {
		return EscalationRecord{}, false, nil
	}
	if strings.TrimSpace(latest.RoundID) != "" {
		// Post-upgrade round: it has a row, or it has none because it is already
		// settled. Either way there is nothing to adopt.
		return EscalationRecord{}, false, nil
	}
	return latest, true, nil
}
