package workflow

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
)

func (e Engine) dispatchDelegations(ctx context.Context, job db.Job, payload JobPayload, ref taskRef) error {
	if payload.Result == nil || len(payload.Result.Delegations) == 0 {
		return nil
	}

	// Ephemeral workers are leaf executors: they are auto-disposed when their job
	// completes, so a continuation enqueued to their (now-deleted) synthetic agent
	// would strand. Do not dispatch delegations returned by an ephemeral worker.
	if payload.Ephemeral != nil {
		_ = e.Store.AddJobEvent(ctx, db.JobEvent{
			JobID:   job.ID,
			Kind:    "delegation_ignored_ephemeral",
			Message: fmt.Sprintf("ephemeral workers cannot delegate; ignoring %d delegation(s)", len(payload.Result.Delegations)),
		})
		return nil
	}

	// A finalize continuation (enqueued when a backstop tripped, #305 "Later") is
	// terminal: the coordinator was asked to synthesize a best-effort final result,
	// so ignore any delegations it returned and stop the chain here. This must
	// precede the budget checks so an over-budget finalize continuation is not
	// itself re-tripped.
	if payload.DelegationFinalize {
		_ = e.Store.AddJobEvent(ctx, db.JobEvent{
			JobID:   job.ID,
			Kind:    "delegation_finalized",
			Message: fmt.Sprintf("finalize continuation is terminal; ignoring %d delegation(s)", len(payload.Result.Delegations)),
		})
		return nil
	}

	rootID := e.rootJobID(job, payload)

	// Operator kill switch (#341): an operator can terminate a runaway tree by
	// root id (gitmoot job kill). This is the FIRST backstop so operator action
	// wins over every budget cap: rather than dispatching the next generation,
	// route through the same #305 graceful finalize continuation (synthesize what
	// completed → stop). Fails open: a lookup error never blocks dispatch.
	if killed, _ := e.Store.IsRootJobKilled(ctx, rootID); killed {
		_ = e.Store.AddJobEvent(ctx, db.JobEvent{
			JobID:   job.ID,
			Kind:    "delegation_killed",
			Message: fmt.Sprintf("root delegation tree %s killed by operator; not dispatching %d delegation(s)", rootID, len(payload.Result.Delegations)),
		})
		return e.enqueueFinalizeContinuation(ctx, job, payload, "delegation tree killed by operator")
	}

	maxDepth := effectiveMaxDelegationDepth()
	if payload.DelegationDepth >= maxDepth {
		_ = e.Store.AddJobEvent(ctx, db.JobEvent{
			JobID:   job.ID,
			Kind:    "delegation_depth_exceeded",
			Message: fmt.Sprintf("delegation depth %d reached the limit of %d; not dispatching %d delegation(s)", payload.DelegationDepth, maxDepth, len(payload.Result.Delegations)),
		})
		return e.enqueueFinalizeContinuation(ctx, job, payload, fmt.Sprintf("delegation depth limit of %d reached", maxDepth))
	}

	total, err := e.countRootDelegationJobs(ctx, rootID)
	if err != nil {
		return err
	}
	// #649: PROJECTED, all-or-nothing job-budget admission. The old guard was
	// reactive (`total >= maxJobs`) and only tripped once the tree was ALREADY at
	// the cap, so a wide fan-out launched from just under the limit enqueued its
	// whole batch and overshot MaxDelegationTotalJobs by up to (batch_width - 1).
	// The job count is the one backstop that is exactly projectable before
	// enqueue, so count the NEW jobs this batch would add (ready AND deferred legs
	// — deferred legs escape every later budget check — minus already-enqueued /
	// fingerprint-deduped legs) and refuse the WHOLE batch when the tree would
	// cross the cap. total+projected == maxJobs ADMITS (may reach but not exceed);
	// > maxJobs refuses. When projected == 0 (an idempotent re-advance whose legs
	// all already have children) this admits and the enqueue loop below adds
	// nothing new — the correct idempotent outcome, where the old guard finalized.
	projected, err := e.projectedNewDelegationJobs(ctx, job.ID, payload.Result.Delegations)
	if err != nil {
		return err
	}
	maxJobs := effectiveMaxDelegationTotalJobs()
	if total+projected > maxJobs {
		_ = e.Store.AddJobEvent(ctx, db.JobEvent{
			JobID:   job.ID,
			Kind:    "delegation_budget_exceeded",
			Message: fmt.Sprintf("delegation batch of %d new job(s) would exceed the per-root job budget of %d (tree at %d); not dispatching %d delegation(s)", projected, maxJobs, total, len(payload.Result.Delegations)),
		})
		return e.enqueueFinalizeContinuation(ctx, job, payload, fmt.Sprintf("per-root job budget of %d reached", maxJobs))
	}

	if exceeded, elapsed := e.rootWallClockExceeded(ctx, rootID); exceeded {
		_ = e.Store.AddJobEvent(ctx, db.JobEvent{
			JobID:   job.ID,
			Kind:    "delegation_walltime_exceeded",
			Message: fmt.Sprintf("delegation tree for root %s ran %s, exceeding the wall-clock limit of %s; not dispatching %d delegation(s)", rootID, elapsed.Round(time.Second), MaxDelegationWallClock, len(payload.Result.Delegations)),
		})
		return e.enqueueFinalizeContinuation(ctx, job, payload, fmt.Sprintf("wall-clock limit of %s reached", MaxDelegationWallClock))
	}

	// Per-root token budget (#338 Part B). Where the depth/total-jobs/wall-clock
	// backstops bound the shape and duration of a tree, this bounds its cost: a
	// coordination tree that has already used at least MaxDelegationTokenBudget
	// tokens (input + output, summed across the whole tree) is refused further
	// fan-out and routed to the #305 finalize continuation. It is opt-in: when the
	// budget is 0 (the default) the check is skipped entirely, so default behavior
	// is byte-identical. The sum fails open — a lookup/parse hiccup yields 0 and
	// does not block dispatch — and is best-effort per runtime (a runtime that
	// reports no usage contributes 0, so the budget under-counts that runtime).
	if e.MaxDelegationTokenBudget > 0 {
		if used, _ := e.sumRootDelegationTokens(ctx, rootID); used >= e.MaxDelegationTokenBudget {
			_ = e.Store.AddJobEvent(ctx, db.JobEvent{
				JobID:   job.ID,
				Kind:    "delegation_cost_exceeded",
				Message: fmt.Sprintf("delegation tree %s reached token budget %d (used %d); not dispatching %d delegation(s)", rootID, e.MaxDelegationTokenBudget, used, len(payload.Result.Delegations)),
			})
			return e.enqueueFinalizeContinuation(ctx, job, payload, fmt.Sprintf("token budget %d reached", e.MaxDelegationTokenBudget))
		}
	}

	// Per-root dollar-cost budget (#380). This is the cost analogue of the token
	// budget above: where the token budget bounds raw token count, this bounds the
	// measured spend derived from those same tokens × a per-model price table (see
	// cost.go). A tree that has already spent at least MaxDelegationCostUSD dollars
	// is refused further fan-out and routed to the same #305 finalize continuation,
	// exactly like every backstop above — never hard-killed. It is opt-in: when the
	// budget is 0 (the default) the check is skipped entirely, so default behavior
	// is byte-identical. The sum fails open (a lookup/parse hiccup yields 0 and does
	// not block dispatch) and is best-effort per runtime: a runtime that reports no
	// usage contributes $0, so the budget under-counts that runtime.
	if e.MaxDelegationCostUSD > 0 {
		if spent, _ := e.sumRootDelegationCost(ctx, rootID); spent >= e.MaxDelegationCostUSD {
			_ = e.Store.AddJobEvent(ctx, db.JobEvent{
				JobID:   job.ID,
				Kind:    "delegation_cost_usd_exceeded",
				Message: fmt.Sprintf("delegation tree %s reached cost budget $%.4f (spent $%.4f); not dispatching %d delegation(s)", rootID, e.MaxDelegationCostUSD, spent, len(payload.Result.Delegations)),
			})
			return e.enqueueFinalizeContinuation(ctx, job, payload, fmt.Sprintf("cost budget $%.4f reached", e.MaxDelegationCostUSD))
		}
	}

	if width := len(payload.Result.Delegations); width > MaxDelegationWidth {
		_ = e.Store.AddJobEvent(ctx, db.JobEvent{
			JobID:   job.ID,
			Kind:    "delegation_width_exceeded",
			Message: fmt.Sprintf("delegation set width %d exceeds the per-coordinator limit of %d; not dispatching", width, MaxDelegationWidth),
		})
		return e.enqueueFinalizeContinuation(ctx, job, payload, fmt.Sprintf("delegation set width %d exceeds the per-coordinator limit of %d", width, MaxDelegationWidth))
	}

	// Windowed non-progress detection. The depth/budget caps above are blunt
	// safety nets; this catches the common loop sooner: a coordinator whose
	// continuation re-issues a delegation set it already issued within the last
	// few generations (the window threaded through the payload). A real,
	// progressing coordinator emits a different set each round, so its hash is
	// never in the window and dispatch proceeds untouched.
	if stopped, err := e.handleDelegationLoop(ctx, job, payload, ref); err != nil || stopped {
		return err
	}

	delegations := payload.Result.Delegations

	// Preflight every delegation (ready and deferred) before enqueueing any
	// child job so a partial failure does not leave the parent half-dispatched
	// and an unknown/uncapable agent on a deferred delegation blocks the parent
	// immediately rather than only when its deps clear. Use a lightweight check
	// that does not acquire branch locks or other execution side effects.
	for _, d := range delegations {
		request := e.delegationRequest(job, payload, d)
		if err := e.preflightDelegation(ctx, request); err != nil {
			// An unroutable delegation set (an unknown / not-allowed / uncapable
			// agent — usually a runtime name where an agent NAME was required) is no
			// longer a terminal block: that dead-ends the coordinator before any
			// child or continuation enqueues. Instead emit a structured
			// delegation_preflight_failed event and route the actionable error back
			// through the corrective continuation so the coordinator can re-emit a
			// corrected set. The all-or-nothing preflight is preserved: we return on
			// the FIRST failure before the enqueue loop below, so no partial dispatch,
			// and deferred legs are covered because this loop sees every delegation.
			reason := fmt.Sprintf("delegation %q preflight failed: %v", request.DelegationID, err)
			return e.handleDelegationPreflightFailure(ctx, job, payload, reason)
		}
	}

	// Write the shared coordinator brief and context manifest once, fully,
	// before enqueueing any child job so a child that starts immediately reads a
	// complete directory. Every child of a parent that produced artifacts is
	// pointed at the same directory so its prompt can reference brief.md and
	// context-manifest.json. Writing is skipped when the engine has no artifact
	// root or no delegation requested artifacts.
	artifactDir, err := writeDelegationArtifacts(e.ArtifactRoot, job.ID, payload.Result)
	if err != nil {
		// #758 dispatch-path edge: an orchestrate root has no task, so e.block would
		// strand the chain — route to a foldable finalize tail (byte-identical e.block
		// for every other tree).
		return e.finalizeOrBlockDispatch(ctx, job, payload, e.block(ctx, ref, fmt.Sprintf("write delegation artifacts: %v", err)))
	}
	if artifactDir != "" {
		_ = e.Store.AddJobEvent(ctx, db.JobEvent{
			JobID:   job.ID,
			Kind:    "delegation_artifacts_written",
			Message: fmt.Sprintf("delegation artifacts written to %s", artifactDir),
		})
	}

	// Only delegations with no unmet deps are enqueued now; deferred ones are
	// enqueued by advanceDelegations once every dep has succeeded. Allocate
	// worktrees/branch locks only for the ready implement delegations so a
	// deferred delegation does not hold a lock before it can run.
	for _, d := range delegations {
		if len(compactStrings(d.Deps)) > 0 {
			continue
		}
		// Ready (dep-free) delegations dispatch here with no upstream context: a
		// dep-free leg has no upstream deps to inject, and a deps-bearing leg is
		// deferred to advanceDelegations (which injects #419 upstream results).
		if err := e.enqueueDelegation(ctx, job, payload, d, artifactDir, "", ref); err != nil {
			// A worktree/branch-lock allocation block on an orchestrate root routes to a
			// foldable finalize tail (and stops the loop) instead of stranding the chain.
			return e.finalizeOrBlockDispatch(ctx, job, payload, err)
		}
	}
	return nil
}

// handleDelegationLoop implements the windowed non-progress check. It compares
// the current delegation set's canonical hash against the sliding window
// threaded through the payload and decides whether dispatch should be halted:
//
//   - the hash is NOT in the window: not a repeat. Returns (false, nil); the
//     caller dispatches normally and maybeEnqueueContinuation records this hash
//     in the window for the next generation.
//   - the hash IS in the window and the coordinator was already nudged once
//     (DelegationRepeatCount >= 1): a confirmed loop. Records a
//     delegation_loop_detected event and returns (true, nil) so dispatch is
//     skipped — the coordinator got a corrective continuation and repeated anyway.
//   - the hash IS in the window for the first time: records a
//     delegation_loop_warning and, instead of dispatching, enqueues one
//     corrective continuation that tells the coordinator to change its approach
//     or finish. Returns (true, nil) so the repeat is not dispatched.
//
// Returning (true, ...) means "do not dispatch"; (false, nil) means "proceed".
func (e Engine) handleDelegationLoop(ctx context.Context, job db.Job, payload JobPayload, ref taskRef) (bool, error) {
	currentHash := canonicalDelegationSetHash(payload.Result.Delegations)
	if !windowContainsHash(payload.RecentDelegationHashes, currentHash) {
		return false, nil
	}

	// Idempotent on re-advance: if this job already emitted a loop event it has
	// already enqueued its corrective continuation (deterministic id), so do not
	// re-emit the event or double-count it.
	events, err := e.Store.ListJobEvents(ctx, job.ID)
	if err != nil {
		return true, err
	}
	for _, ev := range events {
		if ev.Kind == "delegation_loop_warning" || ev.Kind == "delegation_loop_detected" {
			return true, nil
		}
	}

	if payload.DelegationRepeatCount >= 1 {
		_ = e.Store.AddJobEvent(ctx, db.JobEvent{
			JobID:   job.ID,
			Kind:    "delegation_loop_detected",
			Message: fmt.Sprintf("delegation set %s repeated after a corrective nudge (repeat count %d); not dispatching %d delegation(s)", currentHash, payload.DelegationRepeatCount, len(payload.Result.Delegations)),
		})
		// Graceful finalize (#305 "Later"): rather than stopping silently, give the
		// coordinator one terminal continuation to synthesize a best-effort result.
		if err := e.enqueueFinalizeContinuation(ctx, job, payload, "delegation set repeated after a corrective nudge"); err != nil {
			return true, err
		}
		return true, nil
	}

	_ = e.Store.AddJobEvent(ctx, db.JobEvent{
		JobID:   job.ID,
		Kind:    "delegation_loop_warning",
		Message: fmt.Sprintf("delegation set %s repeats a recent round; sending a corrective continuation instead of dispatching %d delegation(s)", currentHash, len(payload.Result.Delegations)),
	})

	request := JobRequest{
		ID:     delegationContinuationID(job.ID),
		Agent:  job.Agent,
		Action: "ask",
		Model:  payload.Model,
		Effort: payload.Effort,
		// Per-job runtime override (#531): a same-coordinator continuation stays on
		// the override runtime/ref, like Model (see maybeEnqueueContinuation).
		RuntimeOverride:    payload.RuntimeOverride,
		RuntimeOverrideRef: payload.RuntimeOverrideRef,
		Phase:              payload.Phase,
		Repo:               payload.Repo,
		Branch:             payload.Branch,
		PullRequest:        payload.PullRequest,
		HeadSHA:            payload.HeadSHA,
		GoalID:             payload.GoalID,
		TaskID:             payload.TaskID,
		TaskTitle:          payload.TaskTitle,
		LeadAgent:          payload.LeadAgent,
		Reviewers:          payload.Reviewers,
		Sender:             job.Agent,
		Instructions:       buildCorrectiveContinuationPrompt(e.originalGoal(ctx, e.rootJobID(job, payload), payload.Instructions), payload.Result),
		Constraints:        payload.Constraints,
		ParentJobID:        job.ID,
		DelegationDepth:    payload.DelegationDepth + 1,
		DelegatedBy:        job.Agent,
		RootJobID:          e.rootJobID(job, payload),
		WorkflowID:         payload.WorkflowID,
		// Carry the window forward (now including this repeat) and mark that a
		// corrective nudge has fired, so if the next generation repeats again the
		// detector escalates to delegation_loop_detected.
		RecentDelegationHashes: appendDelegationHashWindow(payload.RecentDelegationHashes, currentHash),
		DelegationRepeatCount:  payload.DelegationRepeatCount + 1,
		// Inherit the coordinator's cockpit settings so the continuation renders
		// its pane under the same workspace/session as the rest of the tree.
		Cockpit:        payload.Cockpit,
		CockpitSession: payload.CockpitSession,
		CockpitPaneKey: payload.CockpitPaneKey,
	}
	if err := e.enqueue(ctx, request); err != nil {
		return true, fmt.Errorf("enqueue corrective continuation for %q: %w", job.ID, err)
	}
	return true, nil
}

// handleDelegationPreflightFailure is the disposition of an unroutable delegation
// set (#451). It mirrors the delegation-loop seam (handleDelegationLoop) rather
// than terminal-blocking: the coordinator named an agent that does not resolve to
// a routable registered agent (unknown / not-allowed / uncapable — most often a
// runtime name where an agent NAME was required), so instead of dead-ending it is
// re-invoked once with the actionable error as a corrective continuation so it can
// re-emit a corrected set. The non-progress bound (MaxDelegationNonProgressStreak)
// guarantees a graceful finalize if it keeps naming bad agents: a repeat after a
// corrective nudge (DelegationRepeatCount >= 1) at the streak threshold routes to
// the #305 finalize continuation rather than looping forever.
//
// It always emits a structured delegation_preflight_failed event carrying the
// reason (the observability surface for `gitmoot job list`), then enqueues the
// corrective (or finalize) continuation and returns nil — NOT a BlockedError — so
// the coordinator can self-correct. Idempotent on re-advance: a deterministic
// continuation id makes e.enqueue a no-op, and a once-guard on the
// delegation_preflight_failed event avoids double-emitting/double-enqueueing.
func (e Engine) handleDelegationPreflightFailure(ctx context.Context, job db.Job, payload JobPayload, reason string) error {
	// Once-guard: if this generation already recorded a preflight failure it has
	// already enqueued its single continuation (deterministic id), so a re-advance
	// must not re-emit the event or re-run the streak logic.
	events, err := e.Store.ListJobEvents(ctx, job.ID)
	if err != nil {
		return err
	}
	for _, ev := range events {
		if ev.Kind == "delegation_preflight_failed" {
			return nil
		}
	}

	_ = e.Store.AddJobEvent(ctx, db.JobEvent{
		JobID:   job.ID,
		Kind:    "delegation_preflight_failed",
		Message: reason,
	})

	// Bound the corrective loop with the SAME streak ladder the loop/non-progress
	// detectors use: a coordinator that keeps re-emitting an unroutable set climbs
	// the streak and, once a corrective nudge has already fired, finalizes
	// gracefully instead of looping forever.
	nonProgressStreak := payload.NonProgressStreak + 1
	if nonProgressStreak >= e.nonProgressStreakThreshold() && payload.DelegationRepeatCount >= 1 {
		return e.enqueueFinalizeContinuation(ctx, job, payload, "delegation set named an unroutable agent after a corrective nudge")
	}

	goal := e.originalGoal(ctx, e.rootJobID(job, payload), payload.Instructions)
	request := JobRequest{
		ID:     delegationContinuationID(job.ID),
		Agent:  job.Agent,
		Action: "ask",
		Model:  payload.Model,
		Effort: payload.Effort,
		// Per-job runtime override (#531): a same-coordinator continuation stays on
		// the override runtime/ref, like Model (see maybeEnqueueContinuation).
		RuntimeOverride:    payload.RuntimeOverride,
		RuntimeOverrideRef: payload.RuntimeOverrideRef,
		Phase:              payload.Phase,
		Repo:               payload.Repo,
		Branch:             payload.Branch,
		PullRequest:        payload.PullRequest,
		HeadSHA:            payload.HeadSHA,
		GoalID:             payload.GoalID,
		TaskID:             payload.TaskID,
		TaskTitle:          payload.TaskTitle,
		LeadAgent:          payload.LeadAgent,
		Reviewers:          payload.Reviewers,
		Sender:             job.Agent,
		Instructions:       buildPreflightCorrectiveContinuationPrompt(goal, payload.Result, reason),
		Constraints:        payload.Constraints,
		ParentJobID:        job.ID,
		DelegationDepth:    payload.DelegationDepth + 1,
		DelegatedBy:        job.Agent,
		RootJobID:          e.rootJobID(job, payload),
		WorkflowID:         payload.WorkflowID,
		// Carry the window forward and mark that a corrective nudge has fired, and
		// thread the streak forward, so a coordinator that keeps naming bad agents
		// escalates to a graceful finalize.
		RecentDelegationHashes: appendDelegationHashWindow(payload.RecentDelegationHashes, canonicalDelegationSetHash(payload.Result.Delegations)),
		DelegationRepeatCount:  payload.DelegationRepeatCount + 1,
		NonProgressStreak:      nonProgressStreak,
		LastProgressDigest:     payload.LastProgressDigest,
		// Inherit the coordinator's cockpit settings so the continuation renders its
		// pane under the same workspace/session as the rest of the tree.
		Cockpit:        payload.Cockpit,
		CockpitSession: payload.CockpitSession,
		CockpitPaneKey: payload.CockpitPaneKey,
	}
	if err := e.enqueue(ctx, request); err != nil {
		return fmt.Errorf("enqueue preflight corrective continuation for %q: %w", job.ID, err)
	}
	// The corrective continuation IS the coordinator's single continuation, so it
	// occupies the continuation slot: emit delegation_continuation_enqueued so a
	// re-advance hits the continuationEnqueued top-guard.
	_ = e.Store.AddJobEvent(ctx, db.JobEvent{
		JobID:   job.ID,
		Kind:    "delegation_continuation_enqueued",
		Message: fmt.Sprintf("preflight corrective continuation occupies the continuation slot for job %s", request.ID),
	})
	return nil
}

// enqueueFinalizeContinuation enqueues a single best-effort "finalize"
// continuation back to the coordinator after a termination backstop trips (loop
// detected, or a per-root depth/job/width budget exceeded). Instead of dropping
// the offending delegations silently, the coordinator is re-invoked once with the
// completed results and told to synthesize a final result and return empty
// delegations. The continuation carries DelegationFinalize, so dispatchDelegations
// treats its result as terminal (any delegations it returns are ignored),
// guaranteeing the chain stops. Idempotent: the deterministic continuation id makes
// e.enqueue a no-op on re-advance, and the once-guard avoids duplicate events.
func (e Engine) enqueueFinalizeContinuation(ctx context.Context, job db.Job, payload JobPayload, reason string) error {
	events, err := e.Store.ListJobEvents(ctx, job.ID)
	if err != nil {
		return err
	}
	for _, ev := range events {
		if ev.Kind == "delegation_finalize_enqueued" {
			return nil
		}
	}
	request := JobRequest{
		ID:     delegationContinuationID(job.ID),
		Agent:  job.Agent,
		Action: "ask",
		Model:  payload.Model,
		Effort: payload.Effort,
		// Per-job runtime override (#531): the finalize continuation stays on the
		// override runtime/ref, like Model (see maybeEnqueueContinuation).
		RuntimeOverride:    payload.RuntimeOverride,
		RuntimeOverrideRef: payload.RuntimeOverrideRef,
		Phase:              payload.Phase,
		Repo:               payload.Repo,
		Branch:             payload.Branch,
		PullRequest:        payload.PullRequest,
		HeadSHA:            payload.HeadSHA,
		GoalID:             payload.GoalID,
		TaskID:             payload.TaskID,
		TaskTitle:          payload.TaskTitle,
		LeadAgent:          payload.LeadAgent,
		Reviewers:          payload.Reviewers,
		Sender:             job.Agent,
		Instructions:       buildFinalizeContinuationPrompt(e.originalGoal(ctx, e.rootJobID(job, payload), payload.Instructions), payload.Result, reason),
		Constraints:        payload.Constraints,
		ParentJobID:        job.ID,
		DelegationDepth:    payload.DelegationDepth + 1,
		DelegatedBy:        job.Agent,
		RootJobID:          e.rootJobID(job, payload),
		WorkflowID:         payload.WorkflowID,
		DelegationFinalize: true,
		ThreadID:           payload.ThreadID,
		ChatMessageID:      payload.ChatMessageID,
		// Inherit the coordinator's cockpit settings so the finalize continuation
		// renders its pane under the same workspace/session as the rest of the tree.
		Cockpit:        payload.Cockpit,
		CockpitSession: payload.CockpitSession,
		CockpitPaneKey: payload.CockpitPaneKey,
	}
	if err := e.enqueue(ctx, request); err != nil {
		return fmt.Errorf("enqueue finalize continuation for %q: %w", job.ID, err)
	}
	// The finalize continuation IS the coordinator's single continuation, so it
	// occupies the continuation slot: emit delegation_continuation_enqueued too.
	// This makes continuationEnqueued() true, so if the tripped coordinator's
	// advanceDelegations later runs (when this finalize child completes) it can
	// never enqueue a *normal* continuation that would collide with the finalize
	// job's deterministic id. The finalize-specific event below drives the
	// once-guard above and keeps the backstop observable.
	_ = e.Store.AddJobEvent(ctx, db.JobEvent{
		JobID:   job.ID,
		Kind:    "delegation_continuation_enqueued",
		Message: fmt.Sprintf("finalize continuation occupies the continuation slot for job %s", request.ID),
	})
	_ = e.Store.AddJobEvent(ctx, db.JobEvent{
		JobID:   job.ID,
		Kind:    "delegation_finalize_enqueued",
		Message: fmt.Sprintf("termination backstop tripped (%s); enqueued a best-effort finalize continuation as job %s", reason, request.ID),
	})
	return nil
}

// enqueueDelegation allocates the per-delegation worktree (or branch lock) for
// implement delegations, then enqueues the child job and records a
// delegation_enqueued event on the parent. It is idempotent: a duplicate
// deterministic-ID insert is swallowed by e.enqueue when the existing job
// matches the request, so it is safe to call from both dispatchDelegations and
// advanceDelegations.
func (e Engine) enqueueDelegation(ctx context.Context, job db.Job, payload JobPayload, d Delegation, artifactDir string, upstreamContext string, ref taskRef) error {
	request := e.delegationRequest(job, payload, d)
	request.DelegationArtifactDir = artifactDir
	// Append the #419 "Upstream dependency results" block (built by the caller
	// from this dependent's succeeded direct deps) to the child's instructions.
	// upstreamContext is "" unless Engine.InjectUpstreamDepContext is set, so the
	// flag-off enqueued prompt is byte-identical to before this field existed.
	if upstreamContext != "" {
		request.Instructions = request.Instructions + upstreamContext
	}

	// Fingerprint dedup: skip enqueueing a child whose fingerprint already
	// appears among a sibling under the same parent. Scoped to this parent via
	// the (parentJobID, fingerprint) key so identical fingerprints under
	// different parents never collide. The delegation's own child is excluded so
	// an idempotent re-enqueue of the same delegation is not treated as a dup.
	if fingerprint := strings.TrimSpace(d.Fingerprint); fingerprint != "" {
		seen, err := e.delegationFingerprintSeen(ctx, job.ID, d.ID, fingerprint)
		if err != nil {
			return err
		}
		if seen {
			_ = e.Store.AddJobEvent(ctx, db.JobEvent{
				JobID:   job.ID,
				Kind:    "delegation_deduped",
				Message: fmt.Sprintf("delegation %q skipped: fingerprint %q already enqueued (key %s)", d.ID, fingerprint, delegationFingerprintKey(job.ID, fingerprint)),
			})
			return nil
		}
	}

	return e.allocateAndEnqueueDelegation(ctx, job, payload, d, request, ref)
}

type integrationDepResolution struct {
	branches       []string
	alreadyOnBase  []string
	unresolvedDeps []string
}

// resolveIntegrationDeps classifies delegation d's implement-leg dependencies.
// Succeeded implement legs on their own branches must be merged into an
// integration worktree; succeeded implement legs already on the parent base are
// safe to read from the base checkout; missing/not-succeeded/invalid legs are
// unresolved and must fail closed so reviewers do not inspect stale code.
func (e Engine) resolveIntegrationDeps(ctx context.Context, job db.Job, payload JobPayload, d Delegation) (integrationDepResolution, error) {
	deps := compactStrings(d.Deps)
	if len(deps) == 0 || payload.Result == nil {
		return integrationDepResolution{}, nil
	}
	byID := make(map[string]Delegation, len(payload.Result.Delegations))
	for _, sib := range payload.Result.Delegations {
		byID[strings.TrimSpace(sib.ID)] = sib
	}
	hasImplementDep := false
	for _, dep := range deps {
		if sib, ok := byID[dep]; ok && !readOnlyDelegationAction(sib.Action) {
			hasImplementDep = true
			break
		}
	}
	if !hasImplementDep {
		return integrationDepResolution{}, nil
	}

	// Resolve each dep to its winning child job the same way advanceDelegations
	// does (latest attempt per delegation id), so a leg that succeeded on a retry
	// contributes its retry branch rather than the failed original.
	children, err := e.childDelegationJobs(ctx, job.ID)
	if err != nil {
		return integrationDepResolution{}, err
	}
	base := strings.TrimSpace(payload.Branch)
	var result integrationDepResolution
	for _, dep := range deps {
		sib, ok := byID[dep]
		if !ok || readOnlyDelegationAction(sib.Action) {
			continue
		}
		legJob, ok := children[dep]
		if !ok || legJob.State != string(JobSucceeded) {
			result.unresolvedDeps = append(result.unresolvedDeps, dep)
			continue
		}
		legPayload, err := unmarshalPayload(legJob.Payload)
		if err != nil {
			result.unresolvedDeps = append(result.unresolvedDeps, dep)
			continue
		}
		legBranch := strings.TrimSpace(legPayload.Branch)
		switch {
		case legBranch == "":
			result.unresolvedDeps = append(result.unresolvedDeps, dep)
		case legBranch == base:
			result.alreadyOnBase = append(result.alreadyOnBase, dep)
		default:
			result.branches = append(result.branches, legBranch)
		}
	}
	return result, nil
}

// commitDelegationLeg commits an implement delegation leg's worktree changes to
// its own branch when the leg has its own per-delegation worktree but no task/PR
// finalizer (a PR-less local orchestrate, where the finalizer never runs and the
// edits would otherwise stay uncommitted). This makes the leg's work available on
// its branch for a dependent integration step (#332). It is a no-op for jobs with
// no delegation worktree, a clean worktree, or a manager that cannot commit.
func (e Engine) commitDelegationLeg(ctx context.Context, job db.Job, payload JobPayload) error {
	if strings.TrimSpace(payload.DelegationID) == "" || strings.TrimSpace(payload.WorktreePath) == "" {
		return nil
	}
	committer, ok := e.DelegationWorktrees.(WorktreeCommitter)
	if !ok {
		return nil
	}
	committed, err := committer.CommitWorktree(ctx, payload.WorktreePath, fmt.Sprintf("Gitmoot delegation %s implementation", payload.DelegationID))
	if err != nil {
		return err
	}
	if committed {
		_ = e.Store.AddJobEvent(ctx, db.JobEvent{
			JobID:   job.ID,
			Kind:    "delegation_committed",
			Message: fmt.Sprintf("delegation %q committed its implementation to branch %s", payload.DelegationID, payload.Branch),
		})
	}
	return nil
}

// allocateAndEnqueueDelegation allocates the per-delegation worktree (or branch
// lock) for implement delegations and enqueues the prepared request, recording a
// delegation_enqueued event. It is shared by enqueueDelegation (initial/deferred
// dispatch) and requeueDelegation (retry) so both go through identical worktree
// allocation and idempotent enqueue.
func (e Engine) allocateAndEnqueueDelegation(ctx context.Context, job db.Job, payload JobPayload, d Delegation, request JobRequest, ref taskRef) error {
	if request.Action == "implement" {
		if e.DelegationWorktrees == nil || strings.TrimSpace(e.Home) == "" {
			// No per-delegation worktree isolation is available (the engine lacks a
			// Home/DelegationWorktrees manager), so the child falls back to a
			// shared-checkout branch lock. Emit a parent event so the loss of
			// isolation is observable rather than silent.
			_ = e.Store.AddJobEvent(ctx, db.JobEvent{
				JobID:   job.ID,
				Kind:    "delegation_worktree_skipped",
				Message: fmt.Sprintf("delegation %q implement runs in the shared checkout on branch %s: per-delegation worktree isolation unavailable", request.DelegationID, request.Branch),
			})
			if err := e.ensureBranchLock(ctx, request.Repo, request.Branch, request.Agent, ref); err != nil {
				return err
			}
		} else {
			result, err := e.AllocateDelegationWorktree(ctx, DelegationWorktreeRequest{
				Home:         e.Home,
				Repo:         request.Repo,
				ParentJobID:  job.ID,
				DelegationID: request.DelegationID,
				Delegation:   d,
				BaseBranch:   payload.Branch,
				Owner:        request.Agent,
				Checkout:     e.DelegationCheckout,
				RetryAttempt: request.RetryCount,
			}, e.DelegationWorktrees)
			if err != nil {
				var blocked BlockedError
				if errors.As(err, &blocked) {
					return e.block(ctx, ref, blocked.Reason)
				}
				return err
			}
			request.Branch = result.Branch
			request.WorktreePath = result.Path
			// The freshly-allocated worktree is created off the parent's base
			// branch, whose tip may have advanced past the HeadSHA the child
			// inherited from the parent payload. validateTargetCheckout (daemon)
			// compares the worktree HEAD against payload.HeadSHA and would
			// spuriously reject the child on a moving parent branch. Clear the
			// inherited HeadSHA so the child validates against its own fresh
			// worktree HEAD instead of a stale parent SHA.
			request.HeadSHA = ""
		}
	} else if integration, err := e.resolveIntegrationDeps(ctx, job, payload, d); err != nil {
		return err
	} else if len(integration.unresolvedDeps) > 0 {
		// Fail closed (#19): this read-only delegation depends on implement legs that
		// are not safely readable from either the parent base checkout or branch-backed
		// integration. Falling through to BASE here would make the reviewer judge code
		// WITHOUT the implemented change. A zero-branch resolution is allowed only when
		// every implement dep is already on the parent base branch.
		unresolved := strings.Join(integration.unresolvedDeps, ", ")
		_ = e.Store.AddJobEvent(ctx, db.JobEvent{
			JobID:   job.ID,
			Kind:    "delegation_integration_unresolved",
			Message: fmt.Sprintf("delegation %q depends on unresolved implement leg(s) %s; refusing to review the base checkout", request.DelegationID, unresolved),
		})
		return e.block(ctx, ref, fmt.Sprintf("delegation %q depends on unresolved implement leg(s) %s; refusing to review the base checkout", request.DelegationID, unresolved))
	} else if len(integration.branches) > 0 {
		// This read-only delegation (e.g. a decompose-and-verify verify gate) depends
		// on succeeded implement legs that each live on their own branch. Merge them
		// into one detached worktree so the dependent sees the combined work instead
		// of the base checkout (#332).
		if manager, ok := e.DelegationWorktrees.(IntegrationWorktreeManager); !ok || strings.TrimSpace(e.Home) == "" {
			_ = e.Store.AddJobEvent(ctx, db.JobEvent{
				JobID:   job.ID,
				Kind:    "delegation_worktree_skipped",
				Message: fmt.Sprintf("delegation %q runs against the base checkout: integration worktree unavailable", request.DelegationID),
			})
		} else {
			path, err := e.AllocateIntegrationWorktree(ctx, DelegationWorktreeRequest{
				Home:         e.Home,
				Repo:         request.Repo,
				ParentJobID:  job.ID,
				DelegationID: request.DelegationID,
				BaseBranch:   payload.Branch,
				Checkout:     e.DelegationCheckout,
				RetryAttempt: request.RetryCount,
			}, integration.branches, manager)
			if err != nil {
				var blocked BlockedError
				if errors.As(err, &blocked) {
					return e.block(ctx, ref, blocked.Reason)
				}
				return err
			}
			request.WorktreePath = path
			// Validate against the integration worktree's own HEAD, not the inherited
			// parent HeadSHA (see isDelegationWorktreeChild).
			request.HeadSHA = ""
			_ = e.Store.AddJobEvent(ctx, db.JobEvent{
				JobID:   job.ID,
				Kind:    "delegation_integrated",
				Message: fmt.Sprintf("delegation %q runs in an integration worktree merging %d implement leg(s)", request.DelegationID, len(integration.branches)),
			})
		}
	} else if readOnlyFanoutNeedsWorktree(payload, d) {
		// Read-only fan-out: >=2 read-only siblings share the parent repo and would
		// otherwise serialize on the repo:<repo> checkout key (only one runs per
		// daemon tick). Give this child its own detached, branch-lock-free worktree
		// so its checkout key is worktree:<path> and the siblings run concurrently.
		if manager, ok := e.DelegationWorktrees.(ReadOnlyWorktreeManager); !ok || strings.TrimSpace(e.Home) == "" {
			// Isolation is unavailable (no Home/worktree manager, or the manager
			// cannot create detached worktrees): the siblings fall back to the shared
			// checkout and serialize. Emit a parent event so the loss of parallelism
			// is observable rather than silent.
			_ = e.Store.AddJobEvent(ctx, db.JobEvent{
				JobID:   job.ID,
				Kind:    "delegation_worktree_skipped",
				Message: fmt.Sprintf("delegation %q read-only fan-out runs serialized in the shared checkout: detached worktree isolation unavailable", request.DelegationID),
			})
		} else {
			path, err := e.AllocateReadOnlyDelegationWorktree(ctx, DelegationWorktreeRequest{
				Home:         e.Home,
				Repo:         request.Repo,
				ParentJobID:  job.ID,
				DelegationID: request.DelegationID,
				Delegation:   d,
				BaseBranch:   payload.Branch,
				Checkout:     e.DelegationCheckout,
				RetryAttempt: request.RetryCount,
			}, manager)
			if err != nil {
				var blocked BlockedError
				if errors.As(err, &blocked) {
					return e.block(ctx, ref, blocked.Reason)
				}
				return err
			}
			request.WorktreePath = path
			// The detached worktree is created at the parent base-branch tip, which
			// may have advanced past the inherited HeadSHA. Clear it so the child
			// validates against its own fresh worktree HEAD (see isDelegationWorktreeChild).
			request.HeadSHA = ""
			// The worktree is the committed tip: it omits gitignored (repos/**) and
			// uncommitted working-tree files. Point the child at the canonical base
			// checkout so an analysis task does not silently report working-tree state
			// as missing (#654). Appending to Instructions keeps the payload
			// deterministic for the idempotent-enqueue equality check, matching the
			// #419 upstream-context pattern. Scoped to this branch only: the
			// single-read-only, no-manager-fallback, and integration-worktree cases
			// must NOT be pointed at the base checkout.
			if note := readOnlyWorktreeContextNote(e.DelegationCheckout); note != "" {
				request.Instructions += note
			}
		}
	}

	if err := e.enqueue(ctx, request); err != nil {
		return fmt.Errorf("dispatch delegation %q: %w", request.DelegationID, err)
	}
	_ = e.Store.AddJobEvent(ctx, db.JobEvent{
		JobID:   job.ID,
		Kind:    "delegation_enqueued",
		Message: fmt.Sprintf("delegation %q enqueued as job %s", request.DelegationID, request.ID),
	})
	return nil
}

// requeueDelegation re-enqueues a failed delegation child as a fresh job when
// the delegation has retry budget left (failedChild's RetryCount < d.Retry). The
// retry uses a distinct .../retry/<n> id so the enqueue idempotency path does not
// mistake it for the already-failed original, carries RetryCount+1 in its
// payload, and inherits the same DelegationID so it represents the same node in
// the delegation graph. upstreamContext is the #419 "Upstream dependency results"
// block (or "" when InjectUpstreamDepContext is off); the caller computes it from
// the current children/dedupWinners so a retried dependent re-receives the same
// upstream block its first attempt got rather than running blind. It returns
// whether a retry was enqueued.
func (e Engine) requeueDelegation(ctx context.Context, parentJob db.Job, parentPayload JobPayload, d Delegation, failedChild db.Job, artifactDir string, upstreamContext string, ref taskRef) (bool, error) {
	if d.Retry <= 0 {
		return false, nil
	}
	attempt := delegationJobRetryCount(failedChild)
	if attempt >= d.Retry {
		return false, nil
	}
	next := attempt + 1

	request := e.delegationRequest(parentJob, parentPayload, d)
	request.ID = parentJob.ID + "/delegation/" + d.ID + "/retry/" + strconv.Itoa(next)
	request.RetryCount = next
	request.DelegationArtifactDir = artifactDir
	// Re-inject the #419 "Upstream dependency results" block so a retried dependent
	// runs WITH its upstream deps' results, exactly as its first attempt did.
	// upstreamContext is "" unless Engine.InjectUpstreamDepContext is set, so the
	// flag-off retry prompt is byte-identical to before this field existed.
	if upstreamContext != "" {
		request.Instructions = request.Instructions + upstreamContext
	}

	if err := e.allocateAndEnqueueDelegation(ctx, parentJob, parentPayload, d, request, ref); err != nil {
		return false, err
	}
	_ = e.Store.AddJobEvent(ctx, db.JobEvent{
		JobID:   parentJob.ID,
		Kind:    "delegation_retry",
		Message: fmt.Sprintf("delegation %q retry %d/%d enqueued as job %s after %s failed", d.ID, next, d.Retry, request.ID, failedChild.ID),
	})
	return true, nil
}

// advanceDelegations runs on a parent coordinator job each time one of its
// delegated children finishes. It enqueues now-ready dependent siblings,
// applies the failed delegation's failure_policy, and enqueues a single
// coordinator continuation job once every top-level sibling has reached a
// terminal state. It is idempotent: deferred dependents and the continuation
// job use deterministic ids, so concurrent child completions cannot double
// enqueue.
func (e Engine) advanceDelegations(ctx context.Context, parentJob db.Job, parentPayload JobPayload, parentResult *AgentResult, ref taskRef) error {
	if parentResult == nil || len(parentResult.Delegations) == 0 {
		return nil
	}

	children, err := e.childDelegationJobs(ctx, parentJob.ID)
	if err != nil {
		return err
	}

	// Parent job events drive two decisions below: which delegations dispatch
	// skipped via fingerprint dedup (so they resolve against their winning
	// sibling rather than stalling the continuation forever), and whether a
	// continuation has already been enqueued (so a late block_parent failure does
	// not contradict it).
	parentEvents, err := e.Store.ListJobEvents(ctx, parentJob.ID)
	if err != nil {
		return err
	}
	dedupWinners := dedupedDelegationWinners(parentResult.Delegations, children, parentEvents)

	// Resolve the shared artifact directory the same way dispatchDelegations did
	// so late-running dependents reference the same brief.md/context-manifest.
	artifactDir, err := delegationArtifactDir(e.ArtifactRoot, parentJob.ID, parentResult)
	if err != nil {
		return err
	}

	// #438: structured sibling of the #419 prose block. Now that a child has
	// finished, augment context-manifest.json with the result-reference fields for
	// every SUCCEEDED delegation so a downstream reader can reference an upstream
	// output by structured JSON rather than re-parsing the prose block. The
	// just-finished child is already in the children map read above, so the enriched
	// view reflects the current succeeded set (later re-reads in this pass only add
	// newly-enqueued, not-yet-succeeded dependents). It is gated on
	// InjectUpstreamDepContext so the flag-off path never re-writes the manifest and
	// stays byte-identical to today, and the write is idempotent (deterministic,
	// sorted JSON) so repeated passes over a stable succeeded set produce no churn.
	// Best-effort like the dispatch artifact write: a manifest write failure must
	// not block draining ready dependents.
	if e.InjectUpstreamDepContext {
		if augmentedDir, augErr := augmentDelegationManifest(e.ArtifactRoot, parentJob.ID, parentResult, children, dedupWinners, e); augErr != nil {
			_ = e.Store.AddJobEvent(ctx, db.JobEvent{
				JobID:   parentJob.ID,
				Kind:    "delegation_manifest_augment_failed",
				Message: fmt.Sprintf("augment delegation manifest: %v", augErr),
			})
		} else if augmentedDir != "" {
			_ = e.Store.AddJobEvent(ctx, db.JobEvent{
				JobID:   parentJob.ID,
				Kind:    "delegation_manifest_augmented",
				Message: fmt.Sprintf("delegation manifest augmented at %s", augmentedDir),
			})
		}
	}

	// Retry pass: a delegation that failed but has retry budget left is
	// re-enqueued as a fresh child before any failure_policy is applied, so its
	// failure is absorbed by the retry rather than blocking/escalating. A
	// successful retry replaces the failed attempt in the children map (the retry
	// id sorts after the original), so the delegation looks active again this
	// pass and the failure switch below skips it.
	retried := false
	for _, d := range parentResult.Delegations {
		child, ok := children[d.ID]
		if !ok || !IsSettledJobState(child.State) || child.State == string(JobSucceeded) {
			continue
		}
		// #419: a retried dependent must not run blind to its upstream deps. The
		// first attempt got the "Upstream dependency results" block injected at its
		// enqueue point in advanceDelegations, but a retry re-enqueues from
		// requeueDelegation, so re-inject the same block here (resolved against the
		// current children/dedupWinners, which still hold the succeeded deps).
		// Gated on InjectUpstreamDepContext so the flag-off retry prompt is
		// byte-identical to before this change.
		upstreamContext := ""
		if e.InjectUpstreamDepContext {
			upstreamContext = e.buildUpstreamDepBlock(d, children, dedupWinners)
		}
		didRetry, err := e.requeueDelegation(ctx, parentJob, parentPayload, d, child, artifactDir, upstreamContext, ref)
		if err != nil {
			return err
		}
		if didRetry {
			retried = true
		}
	}
	if retried {
		children, err = e.childDelegationJobs(ctx, parentJob.ID)
		if err != nil {
			return err
		}
		// Re-read events too: a delegation deduped during this pass emits a new
		// delegation_deduped event, and dedupedDelegationWinners derives the
		// deduped set from events. Reusing the stale snapshot would hide it.
		parentEvents, err = e.Store.ListJobEvents(ctx, parentJob.ID)
		if err != nil {
			return err
		}
		dedupWinners = dedupedDelegationWinners(parentResult.Delegations, children, parentEvents)
	}

	// Apply failure policies first so a failed dependency stops its dependents
	// (or escalates/blocks) before we try to enqueue anything new. Once a
	// continuation has already been enqueued (e.g. an earlier escalate fired it),
	// a later block_parent sibling failure must not block the shared parent task:
	// that would leave a blocked task AND an in-flight continuation, a
	// contradictory end state the continuation prompt does not reflect. The
	// continuation already carries every child outcome, so we fold the block into
	// it by skipping the block here.
	continuationAlreadyEnqueued := continuationEnqueued(parentEvents)
	escalate := false
	for _, d := range parentResult.Delegations {
		child, ok := children[d.ID]
		if !ok || !IsSettledJobState(child.State) || child.State == string(JobSucceeded) {
			continue
		}
		switch delegationFailurePolicy(d) {
		case "continue":
			// Independent ready siblings still run; only this branch's
			// dependents are skipped (handled by depsSatisfied below).
			continue
		case "escalate":
			escalate = true
		case "escalate_human":
			// Durable human-in-the-loop pause (#340): set TaskAwaitingHuman,
			// record the one-shot escalation event, notify the human best-effort,
			// and return an AwaitingHumanError so NO continuation is enqueued and
			// the tree consumes zero compute until an operator resumes it. As with
			// block_parent, once a continuation is already in flight (an earlier
			// escalate sibling fired it) the tree is already proceeding
			// autonomously, so fold this leg in rather than contradict it.
			if continuationAlreadyEnqueued {
				continue
			}
			return e.pauseAwaitingHuman(ctx, parentJob, parentPayload, ref, d, child)
		default: // block_parent (also the empty default)
			if continuationAlreadyEnqueued {
				continue
			}
			reason := fmt.Sprintf("delegation %q failed (failure_policy block_parent): %s", d.ID, childFailureReason(child))
			// #758 engine edge: a pipeline-orchestrate root has no task, so e.block
			// would set no task state and return a BlockedError that mints NO
			// continuation — stranding the stage's chain with no foldable tail. Route
			// it through the #305 graceful finalize continuation so the chain always
			// ends in a settled, delegation-less tail the pipeline advancer folds.
			if e.isPipelineOrchestrateRoot(ctx, parentJob, parentPayload) {
				return e.enqueueFinalizeContinuation(ctx, parentJob, parentPayload, reason)
			}
			return e.block(ctx, ref, reason)
		}
	}

	// Enqueue deferred dependents whose deps have all succeeded. Skip any whose
	// dependency failed under a continue policy (depsSatisfied returns false
	// unless every dep is succeeded). Already-enqueued children are skipped via
	// the children map and e.enqueue's idempotency.
	if !escalate {
		for _, d := range parentResult.Delegations {
			if len(compactStrings(d.Deps)) == 0 {
				continue
			}
			if _, exists := children[d.ID]; exists {
				continue
			}
			// A deduped delegation folds into its winning sibling and must never
			// get its own child, even if its deps are now satisfied.
			if _, deduped := dedupWinners[d.ID]; deduped {
				continue
			}
			if !depsSatisfied(d.Deps, children, dedupWinners) {
				continue
			}
			// #419: make deps[] real dataflow. This dependent is enqueued exactly
			// here, after every dep succeeded and while children/dedupWinners hold
			// the dep payloads, so inject the succeeded direct deps' results into
			// its prompt. Gated behind InjectUpstreamDepContext so the flag-off
			// enqueued instructions are byte-identical to before this change.
			upstreamContext := ""
			if e.InjectUpstreamDepContext {
				upstreamContext = e.buildUpstreamDepBlock(d, children, dedupWinners)
			}
			// No per-root job-budget re-check here: the whole batch (ready +
			// deferred) was pre-authorized as one unit by the #649 projected
			// admission in dispatchDelegations (projectedNewDelegationJobs counts
			// deferred legs), so this deferred enqueue is intentionally uncapped —
			// a late refusal here would strand a deferred leg whose refused
			// sibling deps can never satisfy.
			if err := e.enqueueDelegation(ctx, parentJob, parentPayload, d, artifactDir, upstreamContext, ref); err != nil {
				// A deferred-dependent dispatch block on an orchestrate root routes to a
				// foldable finalize tail (and stops the loop) instead of stranding the chain.
				return e.finalizeOrBlockDispatch(ctx, parentJob, parentPayload, err)
			}
			// Re-read children AND events so a second satisfied dependent in the
			// same pass is not mistaken for still-pending, and a delegation
			// deduped by the enqueueDelegation call above (which emits a fresh
			// delegation_deduped event) is observed by the final
			// allDelegationsResolved check below rather than stalling forever.
			children, err = e.childDelegationJobs(ctx, parentJob.ID)
			if err != nil {
				return err
			}
			parentEvents, err = e.Store.ListJobEvents(ctx, parentJob.ID)
			if err != nil {
				return err
			}
			dedupWinners = dedupedDelegationWinners(parentResult.Delegations, children, parentEvents)
		}
	}

	// Enqueue the coordinator continuation job once every top-level delegation
	// is resolved (terminal child, or permanently gated by a failed dependency
	// under a continue policy), or immediately when a failure escalates.
	if escalate || allDelegationsResolved(parentResult.Delegations, children, dedupWinners) {
		if err := e.maybeEnqueueContinuation(ctx, parentJob, parentPayload, parentResult, children, ref); err != nil {
			return err
		}
	}
	return nil
}
