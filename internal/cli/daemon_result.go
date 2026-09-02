package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/daemon"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// finishQueuedJob closes the admitted lifecycle when it is still queued. Every
// pre-flight caller passes the db.Job it admitted so the state transition is
// atomic in both the job id and that row's lifecycle generation.
func (w jobWorker) finishQueuedJob(ctx context.Context, job db.Job, state workflow.JobState, cause error) error {
	return w.finishQueuedJobAtGeneration(ctx, job.ID, job.LifecycleGeneration, state, cause)
}

// finishQueuedJobAtGeneration is the ANCHORED form: atGeneration >= 0 makes the transition
// atomic in (state, generation), so a verdict formed about an earlier run cannot close a newer
// one that merely happens to be queued again.
//
// handleRunJobError passes the lifecycle it observed before execution. Pre-flight
// callers reach this through finishQueuedJob, which derives the generation from
// their admitted db.Job without a second, non-atomic read.
func (w jobWorker) finishQueuedJobAtGeneration(ctx context.Context, jobID string, atGeneration int64, state workflow.JobState, cause error) error {
	event := db.JobEvent{
		JobID:   jobID,
		Kind:    string(state),
		Message: cause.Error(),
	}
	var transitioned bool
	var err error
	if atGeneration >= 0 {
		transitioned, err = w.Store.TransitionJobStateWithEventAtGeneration(ctx, jobID, string(workflow.JobQueued), atGeneration, string(state), event)
	} else {
		transitioned, err = w.Store.TransitionJobStateWithEvent(ctx, jobID, string(workflow.JobQueued), string(state), event)
	}
	if err != nil {
		return err
	}
	return w.afterQueuedJobTransition(ctx, jobID, state, cause, transitioned)
}

func (w jobWorker) afterQueuedJobTransition(ctx context.Context, jobID string, state workflow.JobState, cause error, transitioned bool) error {
	if transitioned {
		writeLine(w.Stdout, "job %s %s: %v", jobID, state, cause)
		// Best-effort outbound emit (#446) for a DAEMON-owned pre-flight terminal
		// transition (queued->failed|blocked) — this never reaches the engine's
		// Mailbox.finishWithPayload chokepoint, so the daemon owns its emit. Gated
		// on transitioned==true so it fires exactly once per genuine transition;
		// nil-safe when [events] is OFF. The subsequent finalizePreflightDelegationChild
		// only attaches a synthetic result via savePayload (no further transition),
		// so it does not double-emit.
		if eventKind, ok := daemonTerminalEventKind(state); ok {
			emitDaemonTerminalEvent(ctx, w.eventSink(), w.Store, jobID, eventKind, string(state), cause.Error())
		}
	}
	// A delegation child that fails in ANY pre-flight step (checkout/branch-lock
	// validation, adapter factory, managed config, runtime-session lock/busy,
	// ephemeral bring-up, delegated dispatch) is closed here straight from
	// JobQueued — it never reached queued→running, so handleRunJobError's
	// ParentJobID finalize branch (which fires only for non-queued children) is
	// bypassed and the child strands `failed` with Result == nil. Without this,
	// advanceDelegations never runs for the child and its failure_policy
	// (escalate_human / block_parent / continue / escalate) never fires (#409).
	//
	// finishQueuedJobAtGeneration is the shared implementation all 19 pre-flight
	// finishQueuedJob sites and handleRunJobError's queued branch funnel through,
	// so finalizing here covers every pre-flight failure exactly once. Gate on a
	// genuine queued→(failed|blocked) transition + a delegation
	// child with no stored result, so non-delegation jobs (PR/issue asks) are
	// byte-identical and an already-terminal/cancelled child is never
	// force-finalized.
	//
	// JobBlocked is included alongside JobFailed: a queued delegation child that
	// fails an executor pre-flight check returning a BlockedError (a same-branch
	// sibling branch-lock conflict, an empty implement branch, a missing
	// action/repo capability, an unsubscribed agent — all from
	// ensureJobExecutorAllowed/e.block) is routed by handleRunJobError to
	// finishQueuedJob(..., JobBlocked, ...). Both failed and blocked are genuine
	// terminal failures the engine already finalizes (FinalizeTimedOutDelegationChild
	// accepts JobRunning/JobFailed/JobBlocked), so both must advance the parent DAG
	// or the blocked class strands the parent — the exact #409 bug. JobCancelled is
	// deliberately excluded: the engine switch rejects it and a cancelled child
	// must follow the cancelled path.
	if transitioned && (state == workflow.JobFailed || state == workflow.JobBlocked) {
		return w.finalizePreflightDelegationChild(ctx, jobID, cause)
	}
	return nil
}

// finalizePreflightDelegationChild drives the parent delegation DAG for a child
// that was just transitioned queued→failed in a daemon pre-flight step, so the
// delegation's failure_policy fires exactly as it would for a runtime failure.
// It is a no-op for a non-delegation job or a child that already stored a result
// (finalizeTimedOutDelegationChild / Engine.FinalizeTimedOutDelegationChild are
// idempotent), so a re-run (retry / stale-running recovery) re-enters cleanly. It
// mirrors handleRunJobError (~4169-4189): an AwaitingHumanError (escalate_human
// paused the tree awaiting a human, #340) and a BlockedError (block_parent blocked
// the shared parent task) are EXPECTED terminal outcomes of advancing the DAG, not
// errors to propagate.
func (w jobWorker) finalizePreflightDelegationChild(ctx context.Context, jobID string, cause error) error {
	job, err := w.Store.GetJob(ctx, jobID)
	if err != nil {
		return err
	}
	payload, payloadErr := daemonJobPayload(job)
	if payloadErr != nil || strings.TrimSpace(payload.ParentJobID) == "" || payload.Result != nil {
		return nil
	}
	if _, finalizeErr := w.finalizeTimedOutDelegationChild(ctx, job, cause); finalizeErr != nil {
		var awaiting workflow.AwaitingHumanError
		if errors.As(finalizeErr, &awaiting) {
			return nil
		}
		var blocked workflow.BlockedError
		if errors.As(finalizeErr, &blocked) {
			return nil
		}
		return finalizeErr
	}
	return nil
}

type jobTimeoutEvidence struct {
	Deadline time.Time
	Started  time.Time
}

// handleRunJobError settles a failed RUN. observed is the lifecycle of the run that produced
// cause, so a verdict from a finished run cannot be applied to a newer one.
//
// The generation is checked ONCE, before every state fast path, because each of those paths
// re-reads the row and acts on whatever it finds. Review reproduced the reachable interleaving:
// RunJob produces an old run's ResultDeliveryFailed BlockedError, an operator retries the
// terminal job (clearing its result and moving it to queued at a NEWER generation), and the
// queued fast path then flips that fresh row to blocked -- state=blocked at generation 1 where
// queued at generation 1 was correct.
//
// That is the same ABA this PR exists to close, in a path the first fix did not anchor. Anchoring
// only the settlement was anchoring one door of a room with several.
func (w jobWorker) handleRunJobError(ctx context.Context, jobID string, observed jobLifecycle, cause error, timeoutEvidence ...jobTimeoutEvidence) error {
	latest, err := w.Store.GetJob(ctx, jobID)
	if err != nil {
		return err
	}
	if latest.LifecycleGeneration != observed.generation {
		// A NEW run owns this job now. This error describes the previous one, so record it
		// without steering the live lifecycle -- the same treatment settleBlockedAdvancement
		// gives a superseded verdict, and for the same reason.
		return w.recordSupersededSettlement(ctx, jobID, cause.Error())
	}
	if latest.Type == "implement" && runtimePermissionFailure(cause) {
		payload, payloadErr := daemonJobPayload(latest)
		if payloadErr != nil || payload.Result == nil {
			transitioned, err := markJobPermissionBlockedAtGeneration(ctx, w.Store, jobID, observed.generation)
			if err != nil {
				return err
			}
			if transitioned {
				if err := blockTaskForPermissionBlockedJob(ctx, w.Store, latest); err != nil {
					return err
				}
				// Best-effort outbound emit (#446): this running->blocked permission
				// transition is daemon-owned (it does not pass through the engine's
				// Mailbox.finishWithPayload chokepoint), so emit job.blocked exactly
				// once here. The following finalizePreflightDelegationChild only attaches
				// a synthetic result (savePayload, no transition), so it never re-emits.
				emitDaemonTerminalEvent(ctx, w.eventSink(), w.Store, jobID, daemonTerminalPermissionGuard, string(workflow.JobBlocked), agentPermissionBlockedMessage)
				// A WRITABLE implement DELEGATION child whose runtime fails MID-RUN
				// with a permission error (read-only FS / sandbox denies write) is
				// transitioned JobRunning->JobBlocked here and returns early — it never
				// reaches the ParentJobID finalize branch below, so the parent DAG
				// strands exactly like #409 (the mid-run sibling of the pre-flight
				// read-only-implement case fixed at ~2127). Route it through the SAME
				// finalize helper so its failure_policy fires. The helper no-ops for a
				// non-delegation job (ParentJobID empty) or one that already stored a
				// result, so the solo-implement case stays byte-identical.
				if err := w.finalizePreflightDelegationChild(ctx, jobID, errors.New(agentPermissionBlockedMessage)); err != nil {
					return err
				}
				return nil
			}
		}
	}
	if latest.State == string(workflow.JobQueued) {
		state := workflow.JobFailed
		var blocked workflow.BlockedError
		if errors.As(cause, &blocked) {
			state = workflow.JobBlocked
		}
		// ANCHORED. The generation check above is a FAST PATH that avoids pointless work; it
		// guarantees nothing, because a retry can be claimed between that read and this
		// write. The guarantee lives here, in the atomic (state, generation) CAS.
		return w.finishQueuedJobAtGeneration(ctx, jobID, observed.generation, state, cause)
	}
	if latest.State == string(workflow.JobCancelled) {
		_, err := workflow.SettleCancelledRunningJob(ctx, w.Store, latest.ID, "cancelled job worker settled")
		return err
	}
	payload, payloadErr := daemonJobPayload(latest)
	if errors.Is(cause, context.DeadlineExceeded) || len(timeoutEvidence) > 0 {
		evidence := jobTimeoutEvidence{}
		if len(timeoutEvidence) > 0 {
			evidence = timeoutEvidence[0]
		}
		finalized, finalizeErr := w.finalizeTimedOutJob(ctx, latest, payload, cause, evidence)
		if finalizeErr != nil {
			var awaiting workflow.AwaitingHumanError
			if errors.As(finalizeErr, &awaiting) {
				return nil
			}
			var blocked workflow.BlockedError
			if errors.As(finalizeErr, &blocked) {
				return nil
			}
			return finalizeErr
		}
		if finalized {
			return nil
		}
	}
	if payloadErr == nil && payload.Result != nil {
		var awaiting workflow.AwaitingHumanError
		if errors.As(cause, &awaiting) {
			// escalate_human (#340): the parent tree paused durably awaiting a human;
			// the child delivered a result and the pause (task state + event +
			// notification) is already recorded. Treat this as the expected terminal
			// outcome, not a failure to propagate.
			return w.Store.AddJobEvent(ctx, db.JobEvent{JobID: latest.ID, Kind: "advance_awaiting_human", Message: cause.Error()})
		}
		var blocked workflow.BlockedError
		if errors.As(cause, &blocked) {
			return w.recordBlockedAdvancement(ctx, latest.ID, observedJobLifecycle(latest), cause, blocked)
		}
		if err := w.recordPostDeliveryWorkflowError(ctx, latest, cause); err != nil {
			return err
		}
		return nil
	}
	if payloadErr == nil && strings.TrimSpace(payload.ParentJobID) != "" {
		// A delegation child killed by its per-delegation timeout (or any runtime
		// failure that yields no parseable gitmoot_result) lands here still
		// JobRunning, or JobFailed/JobBlocked WITHOUT a stored result: Mailbox.Run
		// errored, so RunJob returned before AdvanceJob and the parent's
		// advanceDelegations never ran. Finalize it as a terminal failed child and
		// drive the parent DAG so the delegation's retry/failure_policy/continuation
		// actually fire instead of the child stranding until the 30m stale-running
		// recovery blindly re-queues it.
		finalized, finalizeErr := w.finalizeTimedOutDelegationChild(ctx, latest, cause)
		if finalizeErr != nil {
			var awaiting workflow.AwaitingHumanError
			if errors.As(finalizeErr, &awaiting) {
				// escalate_human failure_policy paused the shared parent task awaiting
				// a human (#340); the child is finalized and the DAG advanced, so this
				// is the expected durable-pause outcome, not an error to propagate.
				return nil
			}
			var blocked workflow.BlockedError
			if errors.As(finalizeErr, &blocked) {
				// block_parent failure_policy blocked the shared parent task; the
				// child is finalized and the DAG advanced, so this is the expected
				// terminal outcome rather than an error to propagate.
				return nil
			}
			return finalizeErr
		}
		if finalized {
			return nil
		}
	}
	if latest.State == string(workflow.JobFailed) || latest.State == string(workflow.JobBlocked) {
		return nil
	}
	return cause
}

func (w jobWorker) finalizeTimedOutJob(ctx context.Context, job db.Job, payload workflow.JobPayload, cause error, evidence jobTimeoutEvidence) (bool, error) {
	deadline := evidence.Deadline.UTC()
	elapsed := time.Duration(0)
	if !evidence.Started.IsZero() {
		elapsed = time.Since(evidence.Started).Round(time.Millisecond)
	}
	stderrTail := ""
	deliveryError := ""
	if payload.FailureDiagnostics != nil {
		stderrTail = payload.FailureDiagnostics.StderrTail
		deliveryError = payload.FailureDiagnostics.DeliveryError
	}
	detailBytes, _ := json.Marshal(struct {
		Deadline      string `json:"deadline"`
		Elapsed       string `json:"elapsed"`
		StderrTail    string `json:"stderr_tail,omitempty"`
		DeliveryError string `json:"delivery_error,omitempty"`
	}{
		Deadline:      deadline.Format(time.RFC3339Nano),
		Elapsed:       elapsed.String(),
		StderrTail:    stderrTail,
		DeliveryError: deliveryError,
	})
	reason := fmt.Sprintf("job %s exceeded its run deadline: %v", job.ID, cause)
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	runner, err := w.subprocessRunnerForJob(job)
	if err != nil {
		return false, err
	}
	engine := w.workflowForJob(w.delegationParentCheckout(writeCtx, job), runner)
	return engine.FinalizeTimedOutJob(writeCtx, job.ID, reason, string(detailBytes))
}

// finalizeTimedOutDelegationChild bridges the daemon run-error path into the
// engine's delegation DAG: it converts a timed-out/runtime-failed delegation
// child with no stored result into a terminal failed child and advances the
// parent. Advancing the parent can trigger a retry of an *implement* delegation,
// which must allocate a fresh per-delegation worktree; so it resolves the repo's
// main checkout and builds a fully-wired engine instead of a checkout-less one.
// A missing checkout degrades gracefully (the engine emits
// delegation_worktree_skipped and falls back to a shared-checkout branch lock).
// Returns whether the child was finalized.
func (w jobWorker) finalizeTimedOutDelegationChild(ctx context.Context, job db.Job, cause error) (bool, error) {
	reason := fmt.Sprintf("delegation child %s ended without a result: %v", job.ID, cause)
	runner, err := w.subprocessRunnerForJob(job)
	if err != nil {
		return false, err
	}
	engine := w.workflowForJob(w.delegationParentCheckout(ctx, job), runner)
	return engine.FinalizeTimedOutDelegationChild(ctx, job.ID, reason)
}

// delegationParentCheckout returns the repo's main registered checkout for a
// delegation child job (NOT the child's own worktree), so the engine can
// `git worktree add` a retry's per-delegation worktree against it. It returns
// "" on any lookup failure, leaving the engine to advance the DAG without
// worktree isolation rather than blocking finalization.
func (w jobWorker) delegationParentCheckout(ctx context.Context, job db.Job) string {
	payload, err := daemonJobPayload(job)
	if err != nil {
		return ""
	}
	repoRecord, err := w.Store.GetRepo(ctx, payload.Repo)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(repoRecord.CheckoutPath)
}

func (w jobWorker) recordPostDeliveryWorkflowError(ctx context.Context, job db.Job, cause error) error {
	return w.recordAdvanceRetryOnce(ctx, job.ID,
		"post-delivery workflow error; advancement will retry from stored result: "+cause.Error())
}

// jobLifecycle identifies the ONE RUN of a job that an advancement observed, as the pair
// (state, generation) read from the same row at the same instant.
//
// The state alone cannot do this. A state is a VALUE that recurs, so an operator retry
// running failed -> queued -> running -> failed returns the string to exactly what a slow
// advancement observed, and that advancement's compare-and-swap then succeeds against a
// lifecycle it never saw. That is ABA, and it is not fixable by comparing more carefully:
// the two values are genuinely equal. Only a monotonic token distinguishes them, so the
// generation travels WITH the state everywhere the state is used as an anchor.
type jobLifecycle struct {
	state      string
	generation int64
}

func observedJobLifecycle(job db.Job) jobLifecycle {
	return jobLifecycle{state: job.State, generation: job.LifecycleGeneration}
}

// recordDeliveryFailureDiagnostics carries the engine's terminal delivery error
// onto the job row's failure diagnostics (#1620), so `job show` shows the same
// line the daemon writes to its journal as "job <id> failed: <err>" instead of
// leaving the runtime's real error ("400 invalid_request_error: ... requires a
// newer version of Codex", a compaction-endpoint 404) journal-only. It is purely
// additive: the terminal state, its events, and the journal line are all
// unchanged, and an existing StderrTail/phase/exit_code is preserved.
//
// Best-effort by design, exactly like Mailbox.storeFailureDiagnostics — the row
// is already terminally settled by the time this runs, so a load or persist
// failure must never change the failure path.
//
// atGeneration is the lifecycle the run OWNED. A retry claimed between the run
// error and this write bumps the generation and clears diagnostics for its own
// run, so writing then would stamp a live run's payload with a dead run's error;
// the guard drops the write instead. It mirrors handleRunJobError's superseded
// check, and for the same reason.
//
// The generation is checked TWICE, and the second check is the load-bearing one.
// The read below only establishes what to write; a retry can still claim the row
// between that read and the write, and an observer that then wrote
// unconditionally would clobber the live generation's payload — a diagnostic
// given authority over the lifecycle it observes, which is the exact inversion the
// generation anchor exists to prevent. The write is therefore
// UpdateJobPayloadAtGeneration, whose compare-and-swap makes losing that race a
// no-op instead of an overwrite. The early check is kept anyway: it is free and
// skips the read-modify-write entirely in the common superseded case.
func (w jobWorker) recordDeliveryFailureDiagnostics(ctx context.Context, jobID string, atGeneration int64, cause error) {
	if cause == nil || strings.TrimSpace(cause.Error()) == "" {
		return
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	latest, err := w.Store.GetJob(writeCtx, jobID)
	if err != nil {
		w.reportDeliveryDiagnosticsLoss(jobID, cause, err)
		return
	}
	if latest.LifecycleGeneration != atGeneration {
		return
	}
	// ParseJobPayload, not daemonJobPayload: this is a read-MODIFY-write, and the
	// legacy-tolerant decoder is what keeps a payload still carrying the legacy
	// preset_* keys from losing them on the way back out.
	payload, err := workflow.ParseJobPayload(latest.Payload)
	if err != nil {
		w.reportDeliveryDiagnosticsLoss(jobID, cause, err)
		return
	}
	updated := workflow.WithDeliveryError(payload.FailureDiagnostics, cause.Error())
	if updated == nil {
		return
	}
	payload.FailureDiagnostics = updated
	encoded, err := json.Marshal(payload)
	if err != nil {
		w.reportDeliveryDiagnosticsLoss(jobID, cause, err)
		return
	}
	if w.beforeDeliveryDiagnosticsWrite != nil {
		w.beforeDeliveryDiagnosticsWrite()
	}
	// A LOST CAS is the superseded case arriving at the write instead of at the
	// check, so it stays exactly as quiet — the discarded bool is the whole point,
	// not an oversight. Only a genuine persistence FAULT is reported.
	if _, err := w.Store.UpdateJobPayloadAtGeneration(writeCtx, jobID, string(encoded), atGeneration); err != nil {
		w.reportDeliveryDiagnosticsLoss(jobID, cause, err)
	}
}

// reportDeliveryDiagnosticsLoss says on the daemon journal that the delivery
// error never reached the job row.
//
// The distinction it draws is the whole reason it exists. A SUPERSEDED generation
// is an expected drop — the write was supposed to be discarded — and stays silent;
// saying so on every retry would be noise that trains operators to ignore the
// line. A FAULT (the row would not load, its payload would not parse or re-encode,
// the write itself failed) is not expected: the row then carries no delivery_error
// and nothing anywhere says why, which re-creates one level up the exact
// journal-versus-row blindness #1620 exists to end.
//
// Both the fault and the delivery error are redacted through the same
// RedactCommentText path the stored field uses, because either can quote a
// provider URL, a request id, or a token, and this text is written to the journal.
// It remains best-effort: reporting is all it does, and the job's outcome is
// unchanged.
func (w jobWorker) reportDeliveryDiagnosticsLoss(jobID string, cause error, fault error) {
	if w.Stdout == nil {
		return
	}
	writeLine(w.Stdout, "job %s delivery-error diagnostics not recorded: %s", jobID,
		workflow.RedactCommentText(fmt.Sprintf("%v (delivery error: %v)", fault, cause)))
}

// matches reports whether the row read back is still the run this lifecycle identified.
// Both components must agree: an equal state at a LATER generation is the ABA case, and an
// equal generation is what makes the state comparison meaningful at all.
func (l jobLifecycle) matches(job db.Job) bool {
	return job.State == l.state && job.LifecycleGeneration == l.generation
}

func (w jobWorker) recordBlockedAdvancement(ctx context.Context, jobID string, observed jobLifecycle, cause error, blocked workflow.BlockedError) error {
	if blocked.ResultDeliveryFailed {
		return w.settleBlockedAdvancement(ctx, jobID, observed, cause)
	}
	return w.Store.AddJobEvent(ctx, db.JobEvent{JobID: jobID, Kind: "advance_blocked", Message: cause.Error()})
}

// settleBlockedAdvancement records a delivery-failed advancement as the job's terminal
// blocked outcome.
//
// observed is the lifecycle this advancement SAW when it began. Every decision below is
// anchored to it rather than to whatever the row holds now, because an operator's retry can
// move the job failed -> queued underneath a slow advancement. RetryJob has already cleared
// Result by then, so overwriting that fresh lifecycle with blocked strands it: no result,
// plus an advance_blocked event that clears needsRetry and prevents any further advancement.
//
// All THREE outcomes are anchored, not just the CAS, because a returning state fools each of
// them the same way:
//
//	blocked     - a NEW lifecycle that itself ended blocked is not "the same terminal
//	              outcome, concurrently reached"; writing advance_blocked onto it attributes
//	              this run's verdict to a different one.
//	succeeded   - a NEW lifecycle that succeeded is not a contradiction to report as an
//	              error; this advancement simply lost its race and has nothing to say.
//	anything    - the CAS itself, which is where the stale write would actually land.
//
// So the generation is checked FIRST, once, and a mismatch short-circuits every arm: if the
// run moved on, this verdict describes the previous one and the only correct action is to
// record that fact without steering the live lifecycle.
func (w jobWorker) settleBlockedAdvancement(ctx context.Context, jobID string, observed jobLifecycle, cause error) error {
	message := cause.Error()
	latest, err := w.Store.GetJob(ctx, jobID)
	if err != nil {
		return err
	}
	if latest.LifecycleGeneration != observed.generation {
		return w.recordSupersededSettlement(ctx, jobID, message)
	}
	if latest.State == string(workflow.JobBlocked) {
		return w.Store.AddJobEventIfAbsent(ctx, db.JobEvent{JobID: jobID, Kind: "advance_blocked", Message: message})
	}
	encoded, err := payloadWithBlockedResultDecision(latest.Payload)
	if err != nil {
		return fmt.Errorf("encode blocked advancement result: %w", err)
	}
	transitioned, err := w.Store.TransitionJobStatePayloadWithEventAtGeneration(ctx, jobID, observed.state, observed.generation, string(workflow.JobBlocked), encoded, db.JobEvent{
		JobID:   jobID,
		Kind:    "advance_blocked",
		Message: message,
	})
	if err != nil {
		return err
	}
	if transitioned {
		emitDaemonTerminalEvent(ctx, w.eventSink(), w.Store, jobID, daemonTerminalAdvanceBlocked, string(workflow.JobBlocked), message)
		return nil
	}
	latest, err = w.Store.GetJob(ctx, jobID)
	if err != nil {
		return err
	}
	if latest.LifecycleGeneration != observed.generation {
		// The job is on a NEW lifecycle: something re-queued it while this advancement was
		// in flight, so this verdict describes the PREVIOUS one.
		return w.recordSupersededSettlement(ctx, jobID, message)
	}
	if latest.State == string(workflow.JobSucceeded) {
		return fmt.Errorf("record blocked advancement for job %s: succeeded state did not transition", jobID)
	}
	if latest.State == string(workflow.JobBlocked) {
		// A concurrent settlement of the SAME run already reached the same terminal
		// outcome -- same generation, so this is genuinely a duplicate and not a
		// later run that happens to have ended blocked too.
		return w.Store.AddJobEventIfAbsent(ctx, db.JobEvent{JobID: jobID, Kind: "advance_blocked", Message: message})
	}
	// Same generation but a different state: the run moved on WITHIN its own lifecycle
	// while this advancement was in flight. Record it the same way -- this verdict is no
	// longer the live one and must not steer retry.
	return w.recordSupersededSettlement(ctx, jobID, message)
}

// payloadWithBlockedResultDecision patches the one claim settlement owns while
// retaining additive and legacy payload members it does not understand. Stored
// job payloads are an evolving envelope; decoding through JobPayload and
// marshaling it back would erase unknown evidence during an unrelated state
// transition.
func payloadWithBlockedResultDecision(payload string) (string, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(payload), &envelope); err != nil {
		return "", err
	}
	resultRaw, ok := envelope["result"]
	if !ok || len(resultRaw) == 0 || strings.TrimSpace(string(resultRaw)) == "null" {
		return payload, nil
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(resultRaw, &result); err != nil {
		return "", fmt.Errorf("parse result: %w", err)
	}
	decision, err := json.Marshal(string(workflow.JobBlocked))
	if err != nil {
		return "", err
	}
	result["decision"] = decision
	encodedResult, err := json.Marshal(result)
	if err != nil {
		return "", err
	}
	envelope["result"] = encodedResult
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// recordSupersededSettlement records a settlement whose lifecycle has moved on. The kind is
// deliberately one jobNeedsAdvanceRetry does not classify, so the record stays queryable
// without clearing needsRetry on a lifecycle it does not describe.
func (w jobWorker) recordSupersededSettlement(ctx context.Context, jobID string, message string) error {
	return w.Store.AddJobEventIfAbsent(ctx, db.JobEvent{JobID: jobID, Kind: "advance_blocked_superseded", Message: message})
}

func (w jobWorker) reviewBlockingSeverityForComment(job db.Job, payload workflow.JobPayload) string {
	if !strings.EqualFold(strings.TrimSpace(job.Type), "review") ||
		strings.EqualFold(strings.TrimSpace(payload.Sender), workflow.PipelineJobSender) {
		return ""
	}
	return loadReviewConfig(w.ConfigHome).For(payload.Repo).BlockingSeverity
}

func (w jobWorker) postJobResultComment(ctx context.Context, jobID string, agent runtime.Agent, _ string, cause error) error {
	job, payload, err := daemonWorkerJobPayload(ctx, w.Store, jobID)
	if err != nil {
		return err
	}
	if job.State == string(workflow.JobCancelled) {
		return nil
	}
	if payload.PullRequest <= 0 || strings.TrimSpace(payload.Repo) == "" {
		return nil
	}
	if w.CommenterFactory == nil {
		return nil
	}
	posted, err := w.jobResultCommentPosted(ctx, jobID)
	if err != nil {
		return err
	}
	if posted {
		return nil
	}
	repo, err := daemon.ParseRepository(payload.Repo)
	if err != nil {
		return err
	}
	advanceBlocked, err := w.jobAdvanceBlocked(ctx, job.ID)
	if err != nil {
		return err
	}
	resultDeliveryBlocked := resultDeliveryFailed(cause) || job.State == string(workflow.JobBlocked) && advanceBlocked
	diagnostic := jobResultDiagnostic(cause)
	if diagnostic == "" && (payload.Result == nil || resultDeliveryBlocked) {
		diagnostic = w.storedJobFailureDiagnostic(ctx, job)
	}
	body := workflow.RenderJobResultComment(workflow.JobResultComment{
		AgentName:              firstNonEmpty(agent.Name, job.Agent),
		Runtime:                agent.Runtime,
		JobID:                  job.ID,
		JobType:                job.Type,
		JobState:               job.State,
		Payload:                payload,
		Result:                 outwardJobResult(payload.Result, resultDeliveryBlocked),
		ReviewBlockingSeverity: w.reviewBlockingSeverityForComment(job, payload),
		Diagnostic:             diagnostic,
	})
	if _, err := w.CommenterFactory("").PostIssueComment(ctx, repo, int64(payload.PullRequest), body); err != nil {
		_ = w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "comment_post_failed", Message: err.Error()})
		return nil
	}
	return w.Store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "comment_posted", Message: "posted attributed PR result comment"})
}

func (w jobWorker) jobResultCommentPosted(ctx context.Context, jobID string) (bool, error) {
	events, err := w.Store.ListJobEvents(ctx, jobID)
	if err != nil {
		return false, err
	}
	posted := false
	for _, event := range events {
		switch event.Kind {
		case "retry_queued":
			posted = false
		case "comment_posted":
			posted = true
		}
	}
	return posted, nil
}

func (w jobWorker) jobNeedsCommentRetry(ctx context.Context, jobID string) (bool, error) {
	events, err := w.Store.ListJobEvents(ctx, jobID)
	if err != nil {
		return false, err
	}
	needsRetry := false
	for _, event := range events {
		switch event.Kind {
		case "comment_post_failed":
			needsRetry = true
		case "comment_posted":
			needsRetry = false
		case "retry_queued":
			needsRetry = false
		}
	}
	return needsRetry, nil
}

func (w jobWorker) storedJobFailureDiagnostic(ctx context.Context, job db.Job) string {
	if job.State != string(workflow.JobFailed) && job.State != string(workflow.JobBlocked) {
		return ""
	}
	events, err := w.Store.ListJobEvents(ctx, job.ID)
	if err != nil {
		return ""
	}
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if (event.Kind == job.State || job.State == string(workflow.JobBlocked) && event.Kind == "advance_blocked") && strings.TrimSpace(event.Message) != "" {
			return event.Message
		}
	}
	return ""
}

func outwardJobResult(result *workflow.AgentResult, advanceBlocked bool) *workflow.AgentResult {
	if result == nil || !advanceBlocked {
		return result
	}
	blocked := *result
	blocked.Decision = string(workflow.JobBlocked)
	return &blocked
}

func resultDeliveryFailed(cause error) bool {
	var blocked workflow.BlockedError
	return errors.As(cause, &blocked) && blocked.ResultDeliveryFailed
}

func (w jobWorker) jobAdvanceBlocked(ctx context.Context, jobID string) (bool, error) {
	events, err := w.Store.ListJobEvents(ctx, jobID)
	if err != nil {
		return false, err
	}
	latest, ok := latestDeliveryStatusEvent(events)
	return ok && latest.Kind == "advance_blocked", nil
}

func daemonWorkerJobPayload(ctx context.Context, store *db.Store, jobID string) (db.Job, workflow.JobPayload, error) {
	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		return db.Job{}, workflow.JobPayload{}, err
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		return db.Job{}, workflow.JobPayload{}, err
	}
	return job, payload, nil
}

func jobResultDiagnostic(cause error) string {
	if cause == nil {
		return ""
	}
	return cause.Error()
}

func daemonJobPayload(job db.Job) (workflow.JobPayload, error) {
	var payload workflow.JobPayload
	if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
		return workflow.JobPayload{}, fmt.Errorf("parse job payload %q: %w", job.ID, err)
	}
	return payload, nil
}
